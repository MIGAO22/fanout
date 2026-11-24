package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WARP 中转的执行层：注册账号、在出口 netns 里架 WireGuard 设备、读实况、
// 以及开/关这个开关时该干什么。判断和报文构造都在 warp.go，这里只动手。

// warpService 管着"每条出口对应的 Cloudflare 注册记录"。
//
// 记录必须落盘复用：每次重连都重新注册，等于在 Cloudflare 那边给同一条出口
// 开出一串账号。免费 WARP 对注册频率是有脾气的一方，被判定滥用之后
// 拿不到接入点，这个功能就永久坏了，而用户只会看到"WARP 连不上"。
type warpService struct {
	dir string

	mu     sync.Mutex
	accs   map[int]*WarpAccount
	failAt map[int]time.Time // 上次注册失败的时间，用来挡住重试风暴
}

var warpSvc *warpService

// warpRetryAfter 是单条出口注册失败后的冷却时间。
const warpRetryAfter = time.Minute

func warpStorePath(dir string) string { return filepath.Join(dir, "warp.json") }

// newWarpService 读回上次的注册记录。
// 记录读坏了不当致命错误：大不了重新注册，代价只是每条出口换一个新的
// Cloudflare 出口 IP，比 fanout 起不来好得多。
func newWarpService(dir string) (*warpService, error) {
	s := &warpService{dir: dir, accs: map[int]*WarpAccount{}, failAt: map[int]time.Time{}}
	blob, err := os.ReadFile(warpStorePath(dir))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := parseWarpStore(blob)
	if err != nil {
		log.Printf("fanout: %v，WARP 注册记录按空处理", err)
		return s, nil
	}
	for i := range list {
		s.accs[list[i].Slot] = &list[i]
	}
	return s, nil
}

// saveLocked 原子写盘。调用方必须持有 s.mu。
func (s *warpService) saveLocked() error {
	list := make([]WarpAccount, 0, len(s.accs))
	for _, a := range s.accs {
		list = append(list, *a)
	}
	blob, err := renderWarpStore(list)
	if err != nil {
		return err
	}
	path := warpStorePath(s.dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *warpService) get(slot int) *WarpAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accs[slot]
}

// all 返回按槽位排好序的注册记录，界面按这个顺序列。
func (s *warpService) all() []WarpAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WarpAccount, 0, len(s.accs))
	for _, a := range s.accs {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// ensure 保证这个槽位有一条能用的注册记录，必要时向 Cloudflare 新注册一个。
// version 和记录里的不一致时重新注册（内置客户端那张卡的"更新"就是这个动作）。
func (s *warpService) ensure(slot int, version string) (*WarpAccount, error) {
	if version == "" {
		version = warpClientVersion
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if cur := s.accs[slot]; cur != nil && cur.Version == version {
		return cur, nil
	}
	if at, ok := s.failAt[slot]; ok && time.Since(at) < warpRetryAfter {
		return nil, fmt.Errorf("这条出口的 WARP 注册刚刚失败过，%.0f 秒后再试",
			(warpRetryAfter - time.Since(at)).Seconds())
	}

	resp, priv, pub, err := warpRegister(version)
	if err != nil {
		s.failAt[slot] = time.Now()
		return nil, err
	}
	delete(s.failAt, slot)

	old := s.accs[slot]
	acc, err := warpAccountOf(slot, resp, priv, pub, version, time.Now())
	if err != nil {
		// 号注册上了却用不起来：当场退掉，别在 Cloudflare 那边留个没人认领的账号
		go warpUnregister(resp.ID, resp.Token)
		s.failAt[slot] = time.Now()
		return nil, err
	}
	s.accs[slot] = acc
	if err := s.saveLocked(); err != nil {
		log.Printf("fanout: 保存 WARP 注册记录失败（内存里仍然可用）: %v", err)
	}
	if old != nil {
		// 换版本之前的那条注册已经没人用了，退掉它
		go warpUnregister(old.ID, old.Token)
	}
	return acc, nil
}

// drop 丢掉某个槽位的注册并退掉 Cloudflare 那边的账号。
func (s *warpService) drop(slot int) {
	s.mu.Lock()
	a := s.accs[slot]
	delete(s.accs, slot)
	delete(s.failAt, slot)
	err := s.saveLocked()
	s.mu.Unlock()

	if err != nil {
		log.Printf("fanout: 保存 WARP 注册记录失败: %v", err)
	}
	if a == nil {
		return
	}
	s.removeKeyFile(a.devName())
	warpUnregister(a.ID, a.Token)
}

// dropAll 是关掉开关时的收尾：把 fanout 名下的注册全部退掉。
// 中转关了还留着注册没有意义，留着只会变成对端眼里的僵尸设备。
func (s *warpService) dropAll() {
	s.mu.Lock()
	list := make([]*WarpAccount, 0, len(s.accs))
	for _, a := range s.accs {
		list = append(list, a)
	}
	s.accs = map[int]*WarpAccount{}
	s.failAt = map[int]time.Time{}
	err := s.saveLocked()
	s.mu.Unlock()

	if err != nil {
		log.Printf("fanout: 保存 WARP 注册记录失败: %v", err)
	}
	for _, a := range list {
		s.removeKeyFile(a.devName())
		warpUnregister(a.ID, a.Token)
	}
}

// removeKeyFile 扫掉 v2.0.11 及之前留在磁盘上的私钥。现在私钥经管道递给 wg，
// 不再落盘，但升级过来的机器上那些 0600 文件还在，撤销注册/换密钥时顺手删掉。
// 删失败只记日志：一次 unlink 失败不该把出口拆掉。
func (s *warpService) removeKeyFile(dev string) {
	if err := os.Remove(filepath.Join(s.dir, warpKeyFileName(dev))); err != nil && !os.IsNotExist(err) {
		log.Printf("fanout: 删除 WireGuard 私钥 %s 失败: %v", dev, err)
	}
}

// warpRegister 向 Cloudflare 注册一个免费 WireGuard 接入，同时返回本机密钥对。
//
// 这一步必须包在 inMainNetns 里。原因不是"要直连才注册得到"，而是
// 注册请求本身会从哪条线路出去就决定了这账号归属谁——万一调度到已经被
// dialerInNetns 带进隧道的线程上，就等于从某条出口去注册，
// 拿到的接入配置和出口 IP 全是那条隧道的（见 netnsguard.go 的同类事故）。
func warpRegister(version string) (resp *warpRegResp, priv, pub string, err error) {
	priv, pub, err = newWarpKeys()
	if err != nil {
		return nil, "", "", err
	}
	req, err := newWarpRegReq(pub, version)
	if err != nil {
		return nil, "", "", err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", "", err
	}
	url := fmt.Sprintf("https://%s/%s/reg", "api.cloudflareclient.com", warpAPIVer)

	var blob []byte
	if err := inMainNetns(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		r.Header.Set("User-Agent", "warp-cli/"+req.Version+"(linux)")
		out, err := doWarpCall(r)
		blob = out
		return err
	}); err != nil {
		return nil, "", "", fmt.Errorf("向 Cloudflare 注册 WARP 失败: %w", err)
	}
	parsed, err := parseWarpReg(blob)
	if err != nil {
		return nil, "", "", err
	}
	return parsed, priv, pub, nil
}

// doWarpCall 发一次请求并把响应读出来，非 2xx 连同响应体一起报错——
// Cloudflare 的错误信息就在 body 里，只报状态码等于把线索扔了。
func doWarpCall(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 1MB 上限：这两个接口正常返回都是几百字节到几 KB，读到超量的东西
	// 说明对端已经不是我们要找的那个服务了（被门户/中间设备劫持常见）。
	blob, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return blob, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(blob)))
	}
	return blob, nil
}

// warpUnregister 退掉一条注册。失败只记日志：fanout 这边已经把记录丢了,
// 反复重试只会把用户等开关的耐心磨没。
func warpUnregister(id, token string) {
	if id == "" || token == "" {
		return
	}
	url := fmt.Sprintf("https://%s/%s/reg/%s", "api.cloudflareclient.com", warpAPIVer, id)
	_ = inMainNetns(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "CloudServices/"+warpClientVersion+"-linux")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			log.Printf("fanout: 撤销 WARP 注册 %s 返回 HTTP %d（记录已丢弃，不影响其它出口）", id, resp.StatusCode)
		}
		return nil
	})
}

// warpLatestVersion 问官方 apt 源 cloudflare-warp 的最新稳定号。
//
// 这是唯一能直接读到、又不依赖 GitHub 或签名密钥的官方版本源：
// cloudflare 没有发 WARP 的 GitHub release（几个常见仓库名全是 404），
// 而 apt 索引里的 Version 就是官方发布的那个号。
// codename 依次试：老发行版（bullseye/focal）的号常年落后，拿它当基准
// 会变成"明明有新版却说你是最新"。
func warpLatestVersion() (string, error) {
	// 整件事有一个总时限：这张卡上还要问 GitHub 两个仓库，用户点一次「检查更新」
	// 等上一分钟是不可接受的。四个 codename 的号本来就相同，问到第一个就返回，
	// 只有够不着官方源时才会走到超时——那更该早点停下说"问不到"。
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for _, cn := range parseWarpCodenames(warpCodenames) {
		if time.Now().After(deadline) {
			break
		}
		url := fmt.Sprintf(warpPkgIndex, cn)
		var blob []byte
		err := inMainNetns(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			req.Header.Set("User-Agent", "fanout-warp-check")
			req.Header.Set("Accept", "text/plain")
			out, err := doWarpCall(req)
			blob = out
			return err
		})
		if err != nil {
			lastErr = err
			continue
		}
		if v := parseWarpPkgVersion(string(blob)); v != "" {
			return v, nil
		}
		lastErr = fmt.Errorf("%s 的索引里没有 cloudflare-warp 的版本号", cn)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("官方 apt 源没有返回版本号")
	}
	return "", lastErr
}

// warpDeps 是"这台机器现在离能跑 WARP 还差什么"的完整答案。
type warpDeps struct {
	Ready  bool       // 现在能跑，或者点一下开关就能跑起来
	Reason string     // 完全跑不了时的原话，界面直接印
	Steps  [][]string // 还缺 wireguard-tools 时要跑的装包命令；nil 表示不缺
	Cmd    string     // Steps 的人话形式，界面把"将执行什么命令"摊开给用户看
}

// warpProbeDeps 查中转的前置条件。
//
// 缺 wireguard-tools 不再算"这台机器跑不了"：装了 fanout 就有责任把它要用的
// 东西配上，让人自己 SSH 上去 apt install 是半成品。所以只要包管理器认得出来，
// 这里就报 Ready，把 Steps 交出去，由开启流程的第一步把它装上。
//
// 不探测内核模块：ip link add type wireguard 能不能成，单独探测要么碰 modprobe
// 要么在母机上建个真设备，代价比收益大——真不行的话建设备那条命令的报错写得很清楚。
func warpProbeDeps() warpDeps {
	if warpSvc == nil {
		return warpDeps{Reason: "WARP 注册记录还没就绪（工作目录不可写？）"}
	}
	if hasCmd("wg") {
		return warpDeps{Ready: true}
	}
	steps, _, err := wireguardInstallPlan(readWarpOSRelease(), hasCmd)
	if err != nil {
		return warpDeps{Reason: fmt.Sprintf("%s：本机没有 wg 命令，%v", wireguardPkg, err)}
	}
	return warpDeps{Ready: true, Steps: steps, Cmd: describeSteps(steps)}
}

// readWarpOSRelease 读发行版身份。读不到时返回空串，
// wireguardInstallPlan 会把它归成"认不出这台机器"，那条路是有意的。
func readWarpOSRelease() string {
	blob, err := os.ReadFile(osReleasePath)
	if err != nil {
		return ""
	}
	return string(blob)
}

// warpUnderMTU 读母机默认出口网卡的 MTU。读不到返回 0，
// 由 warpPathMTU 按 1500 处理——宁可回到以前的行为，也不要因为读盘失败就配不出设备。
//
// 拿默认路由那块网卡而不是 netns 里的 veth：veth 永远是 1500，真正会卡住外层包的是
// 母机往外走的那一块（隧道、overlay、PPPoE 的 1492 都在这）。
func warpUnderMTU() int {
	route, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return 0
	}
	dev := warpDefaultDev(string(route))
	if dev == "" {
		return 0
	}
	blob, err := os.ReadFile("/sys/class/net/" + dev + "/mtu")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// warpMTUs 是给执行层用的一行入口：读一次母机网卡，算出三层 MTU。
func warpMTUs(on bool) (wgMTU, tunMTU, mssfix int) {
	return warpPathMTU(on, warpUnderMTU())
}

const (
	// warpInstallWait 是"翻开开关"那条路上装包的时限：用户在界面前台等，
	// 但作业进度在跑，给足到能让一次慢镜像源走完。
	warpInstallWait = 5 * time.Minute
	// warpInstallQuick 是建出口时顺手补装的时限。这里不能久等：
	// 出口卡在 starting 不动，用户看不出是在装包还是节点坏了。
	warpInstallQuick = 90 * time.Second
	// warpInstallCooloff 是装失败后的冷却。源不通时每条出口的每次重连
	// 都去撞一遍 apt，会把整台机器拖成一片红，而多撞几次并不会好。
	warpInstallCooloff = 3 * time.Minute
)

var (
	warpToolsMu    sync.Mutex
	warpToolsTried time.Time
	warpToolsWhy   error
)

// warpEnsureTools 缺 wg 时把 wireguard-tools 装上，装上之后才算成。
//
// 三个入口共用：开启中转时作业里的第一步、按新版本重新注册时同样的第一步、
// 以及建出口之前的顺手补装（机器重启、或者有人把包卸了，开关还开着，
// 只能在那里补）。装完不重启服务——wg 是每条命令现找的可执行文件。
//
// 同一时刻只装一次，失败后冷却一段时间再试。和 openvpn 的升级共用那把锁：
// dpkg 锁只有一把，两个 apt-get 同时跑只会互相撞死。
func warpEnsureTools(budget time.Duration) error {
	if hasCmd("wg") {
		return nil
	}
	warpToolsMu.Lock()
	defer warpToolsMu.Unlock()
	if hasCmd("wg") { // 等锁期间别人可能刚装完
		return nil
	}
	if time.Now().Before(warpToolsTried.Add(warpInstallCooloff)) {
		return warpToolsWhy
	}
	warpToolsTried = time.Now()
	steps, family, err := wireguardInstallPlan(readWarpOSRelease(), hasCmd)
	if err == nil {
		openvpnApplyMu.Lock()
		err = runUpgradeStepsWithin(steps, budget)
		openvpnApplyMu.Unlock()
		if err == nil && !hasCmd("wg") {
			err = fmt.Errorf("%s 说装完了，却还是找不到 wg 命令", family)
		}
		if err != nil {
			if h := wireguardInstallHint(family, err.Error()); h != "" {
				err = fmt.Errorf("%v\n%s", err, h)
			}
		} else {
			log.Printf("fanout: WARP 中转需要 %s，已用包管理器装好", wireguardPkg)
		}
	}
	warpToolsWhy = err
	return err
}

// warpRun 跑一条架桥命令，失败时把内核那句原话顶到报错最前面。
//
// 为什么不用 run()：它的报错是 `<完整命令>: exit status 1: <stderr>`，而
// `wg set fow1 private-key … peer <44 字符> endpoint … allowed-ips …` 这条命令
// 自己就快 200 字符，界面和日志的截断上限会先吃掉命令、把 stderr 留在外面——
// 于是能看到的永远只有"连不上节点"，真原因一个字都不剩（v2.0.9 就是这么空转了一轮）。
// 这里只回显前三条参数（够认出是哪一步、哪台设备），stderr 放最前，
// 再按 wireguardErrorHint 补一句下一步。
//
// stdin 非空时接到子进程的标准输入上：wg 的私钥就是这么递进去的（见 warpKeyStdin），
// 于是命令行里没有密钥、磁盘上也不留密钥。
func warpRun(ns, stdin string, c []string) error {
	name, argv := warpArgv(ns, c)
	cmd := exec.Command(name, argv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmdCombined(cmd)
	if err == nil {
		return nil
	}
	why := strings.TrimSpace(string(out))
	if why == "" {
		why = err.Error() // 程序压根没输出：没装、被信号杀掉，这种也只能报 exit status
	}
	// 先把内核那句压到 120 字，再拼提示——反过来的话长 stderr 会把"下一步"挤掉，
	// 而那半句才是让人不用再回来问的部分。内核在这几步上的原话基本不超过 60 字。
	why = oneLine(why, 120)
	// 只对真正在碰 WireGuard 的那几条补提示：同一批命令里还有 ip route / ip addr,
	// 它们报 "not supported" 跟内核有没有 WireGuard 无关，跟着说只会把人带偏。
	isWG := c[0] == "wg"
	for _, a := range c {
		if a == "wireguard" {
			isWG = true
		}
	}
	if isWG {
		if hint := wireguardErrorHint(why); hint != "" {
			why += " —— " + hint
		}
	}
	return fmt.Errorf("%s: %s", warpCmdTag(ns, c), oneLine(why, 200))
}

// warpSelfTest 拿一条真实注册记录在母机上把 WARP 设备架一遍，然后立刻拆掉。
//
// 它存在的理由是"开关一翻，18 条出口一起掉线去撞同一堵墙"：这套链路里任何一步
// 不成立（命令语法、内核没模块、wg 版本不认、密钥不对），原先都要等到逐条重连
// 才暴露，而暴露出来的句子还会被截断——用户只能看到"连不上节点"。
// 自检把它挪到还没动任何出口之前，并且因为命令短，内核那句原话能完整留在界面上。
//
// 它不验证"能不能通"：握手要等真实流量，那由每条出口自己的报错里那句
// WARP 已握手 / 未握手 来说。
func warpSelfTest(acc *WarpAccount) error {
	if acc == nil {
		return nil // 没有注册记录就没东西可试，交给后面的注册步骤报错
	}
	mtu, _, _ := warpMTUs(true)
	setup, cleanup := warpProbeCmds(acc.PeerKey, acc.Endpoint, acc.Port, mtu)
	defer runQuiet(cleanup[0], cleanup[1:]...)
	runQuiet(cleanup[0], cleanup[1:]...) // 清残骸：不存在是常态，不该因此说自检失败
	for _, c := range setup {
		if err := warpRun("", warpKeyFor(c, acc.PrivKey), c); err != nil {
			step := "建 WireGuard 设备"
			if c[0] == "wg" {
				step = "配置 WireGuard 密钥与接入点"
			}
			return fmt.Errorf("%s：%v", step, err)
		}
	}
	return nil
}

// warpLastErr 留着上一次"翻开开关却没成功"的原话。
//
// 为什么要单独留：这种失败只会出现在作业那一步的悬浮提示里，而作业几分钟后就淡出——
// 用户重新截图、重新描述，又是"连不上节点"。把它钉在开关那一行下面，
// 开关是关着的就说明它还没解决，界面就该一直说这件事。
var warpLastErr = struct {
	mu  sync.Mutex
	msg string
}{msg: ""}

func warpFailNote(msg string) {
	warpLastErr.mu.Lock()
	warpLastErr.msg = msg
	warpLastErr.mu.Unlock()
}

func warpFailText() string {
	warpLastErr.mu.Lock()
	defer warpLastErr.mu.Unlock()
	return warpLastErr.msg
}

// warpForTunnel 在拉起 openvpn 之前把 WARP 架进这条出口的 netns。
// 返回 nil 表示这个开关没开，调用方据此决定要不要改 MTU 和路由。
func warpForTunnel(t *Tunnel) (*WarpAccount, error) {
	if !warpOn() {
		return nil, nil
	}
	if warpSvc == nil {
		// 开关是开的却拿不到注册记录，这时候把出口按直连拉起来最坏：
		// 界面显示"中转已开启"，实际一条流量都没进 Cloudflare。宁可连不上。
		return nil, fmt.Errorf("WARP 注册记录没能加载（工作目录不可写？），中转开着时不建出口")
	}
	// 开关开着、包却被卸了（或这台机器只跑过 f 更新、从没重装过依赖）：在这里补一次。
	// 不补的话每条出口都会以 "exec: wg: file not found" 失败，
	// 而那句话在界面上看起来就像 fanout 自己的 bug。
	if err := warpEnsureTools(warpInstallQuick); err != nil {
		return nil, fmt.Errorf("WARP 中转需要 %s，装它没成功: %w", wireguardPkg, err)
	}
	acc, err := warpSvc.ensure(t.Slot, warpReportVersion())
	if err != nil {
		return nil, err
	}
	if err := t.setupWarp(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// setupWarp 按 warpSetupCmds 给出的顺序在 netns 里执行命令。
// 任何一条没成都算整个架设失败：半套路由（比如默认路由换过去了但例外没加上）
// 比完全不转更糟，它会让这条出口什么也连不上，而界面上看着是"节点坏了"。
func (t *Tunnel) setupWarp(acc *WarpAccount) error {
	ns, sub := t.nsName(), t.subnet()
	_, peer := t.vethNames()
	dev := acc.devName()

	mtu, _, _ := warpMTUs(true)
	in := warpSetupInput{
		Dev: dev, PeerKey: acc.PeerKey,
		Endpoint: acc.Endpoint, Port: acc.Port, TunIP: acc.TunIP, MTU: mtu,
		VethGw: sub + ".1", Veth: peer,
		DirectIPs: t.netnsNameservers(),
	}
	for _, c := range warpSetupCmds(in) {
		if err := warpRun(ns, warpKeyFor(c, acc.PrivKey), c); err != nil {
			return fmt.Errorf("%v（WARP 接入点 %s:%d）", err, acc.Endpoint, acc.Port)
		}
	}
	// 节点地址要在 openvpn 出发之前就钉好：隧道没连着时健康检查会把默认路由撤掉
	// （不让用户流量只走到 Cloudflare 就出去），这条 /32 是 openvpn 剩下的唯一一条路。
	// 留到 tun0 起来之后再装的话，换节点时新地址没人认识，openvpn 会当场
	// "Network is unreachable" 退出，隧道再也起不来——rc1 在真机上就是这么死的。
	for _, c := range warpGatePinCmds(dev, warpGateIPsOf(t)) {
		if err := warpRun(ns, "", c); err != nil {
			return fmt.Errorf("钉住节点地址失败: %v", err)
		}
	}
	return nil
}

// netnsNameservers 读这条出口 netns 用的 resolv.conf。
// 那几个地址要留在直连侧：openvpn 连的节点写成主机名时，
// 解析发生在隧道起来之前，除了直连没有别的路可走。
func (t *Tunnel) netnsNameservers() []string {
	blob, err := os.ReadFile(filepath.Join("/etc/netns", t.nsName(), "resolv.conf"))
	if err != nil {
		return []string{"8.8.8.8"} // setupNetns 写死的那份，读不到就照它算
	}
	return warpNameservers(string(blob))
}

// warpGateIPsOf 汇总这条隧道要钉在 WARP 上的节点地址。
func warpGateIPsOf(t *Tunnel) []string {
	n := t.node()
	ips := warpGateIPs(n.Config, n.IP)
	for _, name := range warpGateNames(n.Config) {
		ips = append(ips, warpResolveGate(name)...)
	}
	return ips
}

// warpSplit 在 tun0 起来之后把用户流量接进 Gate。
// 失败必须往上传：漏掉这一步的话流量停在 Cloudflare，
// 出口 IP 变成 WARP 的而不是节点的，用户以为中转生效了、实际根本没走 Gate。
func (t *Tunnel) warpSplit(acc *WarpAccount) error {
	for _, c := range warpSplitCmds(acc.devName(), "tun0", warpGateIPsOf(t)) {
		if err := run("ip", append([]string{"netns", "exec", t.nsName()}, c...)...); err != nil {
			return fmt.Errorf("WARP 分流失败: %w", err)
		}
	}
	return nil
}

// warpGuardTunnel 是健康检查里那一遍对齐：openvpn 自己重连会把装在 tun0 上的
// 分流路由一起带走，而 tunnel 状态却还挂着上一次连上的那个出口 IP。
// 返回做了什么（空串表示不用动），调用方据此记一行日志。
func (t *Tunnel) warpGuardTunnel(acc *WarpAccount) string {
	ns, dev := t.nsName(), acc.devName()
	out, err := cmdCombined(exec.Command("ip", "netns", "exec", ns, "ip", "-o", "route", "show"))
	if err != nil {
		// netns 没了/读不到：这条隧道正在别处被重建，这里什么都不做。
		return ""
	}
	st := parseWarpRoutes(string(out), dev)
	if !warpGuardNeeded(st) {
		return ""
	}
	why := "补装 WARP 分流"
	if !st.TunUp {
		why = "切断 WARP 转发（隧道没连着）"
	}
	for _, c := range warpGuardCmds(dev, "tun0", warpGateIPsOf(t), st.TunUp) {
		if err := warpRun(ns, "", c); err != nil {
			log.Printf("fanout: 出口 %d %s 没做成: %v", t.Slot, why, err)
			return ""
		}
	}
	return why
}

// warpResolveGate 在母机侧把一个主机名解析成 IPv4。
//
// 必须包 inMainNetns 并限制结果数量：出口 netns 里默认路由已经进了 WARP,
// 在那儿解析会把 DNS 查询送进隧道（隧道还没通），拿不到答案。
func warpResolveGate(host string) []string {
	var ips []string
	_ = inMainNetns(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		ips = nil
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip != nil && ip.To4() != nil && len(ips) < 4 {
				ips = append(ips, ip.String())
			}
		}
		return err
	})
	return ips
}

// warpLiveStatus 是一条出口此刻的 WARP 实况。
type warpLiveStatus struct {
	Endpoint string `json:"endpoint"`
	Live     bool   `json:"live"`
	Rx       int64  `json:"rx"`
	Tx       int64  `json:"tx"`
}

// warpStatusCache 给实况读盘做短缓存。主界面每几秒轮询一次，
// 而每次读都要 fork 一个 ip netns exec + wg，缓存能省掉绝大部分。
var warpStatusCache = struct {
	mu    sync.Mutex
	at    map[string]time.Time
	value map[string]warpLiveStatus
}{at: map[string]time.Time{}, value: map[string]warpLiveStatus{}}

// warpHandshakeNow 读这条 netns 里 WireGuard 设备的握手实况，不走缓存。
// 诊断要的是"此刻"，而界面那套 5 秒缓存是为了省 fork——目的不同，不能共用。
func warpHandshakeNow(ns, dev string) (age int64, seen bool) {
	out, err := cmdOutput(exec.Command("ip", "netns", "exec", ns, "wg", "show", dev, "dump"))
	if err != nil {
		return 0, false
	}
	peers := parseWgDump(string(out))
	if len(peers) == 0 || !peers[0].up() {
		return 0, false
	}
	return warpHandshakeAge(peers[0].Handshake, procUptime(), time.Now().Unix())
}

// warpHopNote 在"这条出口没连上"的报错里补一句 WARP 那一跳的通不通。
//
// 少了这句，两种完全不同的故障长得一模一样：节点死了，和 Cloudflare 那跳没握手。
// 前者该换节点（程序本来就在换），后者换多少节点都不会好——
// 而 netns 里的默认路由是 WireGuard，没握手就是黑洞，openvpn 只会等到超时。
func warpHopNote(ns string, acc *WarpAccount) string {
	age, seen := warpHandshakeNow(ns, acc.devName())
	if seen {
		return fmt.Sprintf("WARP 已握手（接入点 %s:%d，%d 秒前）", acc.Endpoint, acc.Port, age)
	}
	return fmt.Sprintf("WARP 未握手（接入点 %s:%d）——是 Cloudflare 这一跳不通，换节点不会有用", acc.Endpoint, acc.Port)
}

// warpStatusTTL 是界面那套实况的缓存时长。
const warpStatusTTL = 5 * time.Second

// warpStatus 读 netns 里 WireGuard 设备的实况：当前真正在用的 Cloudflare
// 接入点、有没有握手、以及经它转发多少字节。
//
// 接入点取 `wg show dump` 而不是注册记录里存的那个：响应给的是注册当时的
// 分配，实际连上的是哪个 anycast 地址以内核说的为准——这个字段是要给用户
// 当"我的流量现在到了哪个 Cloudflare IP"看的，说注册值就是说旧账。
func warpStatus(ns, dev string) warpLiveStatus {
	key := ns + "/" + dev
	warpStatusCache.mu.Lock()
	if at, ok := warpStatusCache.at[key]; ok && time.Since(at) < warpStatusTTL {
		v := warpStatusCache.value[key]
		warpStatusCache.mu.Unlock()
		return v
	}
	warpStatusCache.mu.Unlock()

	st := warpLiveStatus{}
	out, err := cmdOutput(exec.Command("ip", "netns", "exec", ns, "wg", "show", dev, "dump"))
	if err == nil {
		// 两把尺子都递过去：内核给的那列可能是开机秒也可能是 epoch 秒，
		// 由 warpHandshakeAge 按数值落在哪一段判断（见 warp.go）
		ep, live := warpLiveEndpoint(parseWgDump(string(out)), procUptime(), time.Now().Unix())
		st.Endpoint, st.Live = ep, live
		for _, p := range parseWgDump(string(out)) {
			st.Rx, st.Tx = p.Rx, p.Tx
		}
	}

	warpStatusCache.mu.Lock()
	warpStatusCache.at[key] = time.Now()
	warpStatusCache.value[key] = st
	warpStatusCache.mu.Unlock()
	return st
}

// procUptime 取母机开机以来的秒数。wg 打印的"最近握手时间"就是拿这把尺子量的，
// 不换算就没法判断握手是新是旧。读不到时返回 0，调用方会把状态判成不新鲜——
// 宁可少报一个"在线"，也不要把已经断了的链路说成还连着。
func procUptime() int64 {
	blob, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(blob))
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(n)
}

// warpSettle 是单条出口在新链路下该给的时间：连接预算 + 一次出口探测 + 余量。
func warpSettle(on bool) time.Duration {
	cfg := connConf()
	return warpBudget(time.Duration(cfg.ConnectBudget)*time.Second, on) +
		2*time.Duration(cfg.HealthTimeout)*time.Second + 10*time.Second
}

// warpWait 等一条出口稳定下来。
//
// 只等 up，不等 failed：reconnect 走的是 bringUpPersist，一轮候选全失败它会退避重试
// 而不是把状态停成 failed。所以超时那句要说清"还在自动重试"，不能说成"失败了"——
// 用户看到失败会去点停止，其实再等半分钟就起来了。
//
// 状态栏里那句真实报错（openvpn 日志里挑的 + WARP 握手实况）要一起带上：
// 光说"还没连上"，18 条出口就是 18 个猜谜，而节点死了、认证被拒、
// Cloudflare 那跳不通，三种的下一步完全不同。
func warpWait(t *Tunnel, timeout time.Duration) (bool, string) {
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			raw := t.state().Err
			// WARP 那一跳不通时这句要排在最前面：它决定作业要不要继续逐条撞，
			// 而排在后面会被截断吃掉，界面和 fail-fast 就都认不出来了。
			if i := strings.Index(raw, "WARP 未握手"); i >= 0 {
				return false, "还没连上：" + oneLine(raw[i:], 300)
			}
			if e := oneLine(raw, 420); e != "" {
				return false, "还没连上：" + e + "（出口会继续自动重试）"
			}
			return false, "还没连上，出口会继续自动重试（WARP 那一跳不通时所有节点都会连不上）"
		case <-tick.C:
			switch t.status() {
			case "up":
				return true, ""
			case "stopped":
				return false, "这条出口已经停了"
			}
		}
	}
}

// warpSet 开/关中转，并把已经存在的出口逐条重连。
//
// 为什么非得重连：开关只作用于以后新建的出口的话，界面上它就是句假话——
// 用户打开开关，看到出口照旧是绿的、出口 IP 也没变，以为流量已经过 Cloudflare 了，
// 实际一条都没进。开和关走同一条路：逐条 reconnect，一条一条等它稳定。
// 串行而不是并发，是为了不让二十条出口同时和 Cloudflare 握手、同时抢 VPN Gate。
func warpSet(m *Manager, on bool) (*Job, error) {
	cur := warpConf()
	if cur.Enabled == on {
		if on {
			return nil, fmt.Errorf("中转已经是开启状态")
		}
		return nil, fmt.Errorf("中转已经是关闭状态")
	}
	d := warpProbeDeps()
	if !d.Ready {
		return nil, fmt.Errorf("%s", d.Reason)
	}
	// 装包只在开启时要做：关闭只是拆设备、退注册，用不上 wg。
	needTools := on && d.Steps != nil
	list := m.Tunnels()
	// 注册预检得排在装工具之后、动任何一条出口之前，所以只能跟着进作业里。
	// 开关先翻上去、预检没过再退回原样，为的是让失败有地方看：
	// 一句 409 的原话在界面上闪一下就没了，作业里那一步红着一直在。
	preflight := on && len(list) > 0
	if err := setWarp(WarpSettings{Enabled: on, ClientVersion: cur.ClientVersion}); err != nil {
		return nil, err
	}

	labels := make([]string, 0, len(list)+3)
	if needTools {
		labels = append(labels, "装 "+wireguardPkg)
	}
	if preflight {
		labels = append(labels, "注册第一条出口的 WARP 账号")
		// 自检排在任何一条出口被动之前：内核/语法/密钥这一类问题，
		// 在这里报一次就够了，不必让 18 条出口各自掉线再各自撞一遍。
		labels = append(labels, "自检：在母机上试架 WireGuard 设备")
	}
	for _, t := range list {
		labels = append(labels, fmt.Sprintf("出口 %d · %s", t.Slot, t.host()))
	}
	if len(labels) == 0 {
		return nil, nil // 没出口要重连、也没东西要装，开关已经改完
	}
	verb := "关闭"
	if on {
		verb = "开启"
	}
	sum := verb + " WARP 中转"
	switch {
	case len(list) > 0:
		sum += "（" + strconv.Itoa(len(list)) + " 条出口逐条重连）"
	case needTools:
		sum += "（先装 " + wireguardPkg + "）"
	}
	job := m.jobs.New(sum, labels)
	timeout := warpSettle(on)
	revert := func(why string) {
		if err := setWarp(WarpSettings{Enabled: cur.Enabled, ClientVersion: cur.ClientVersion}); err != nil {
			log.Printf("fanout: 回滚 WARP 开关失败: %v", err)
		}
		warpFailNote(oneLine(why, 200)) // 这句要常驻在开关下面，压到六行以内才不像在念经
		log.Printf("fanout: WARP 中转没能开启：%s", why)
	}

	go func() {
		at := 0
		if needTools {
			job.Set(at, "running", "正在用包管理器装 "+wireguardPkg)
			if err := warpEnsureTools(warpInstallWait); err != nil {
				job.Set(at, "failed", err.Error())
				revert(err.Error())
				job.Finish()
				return
			}
			job.Set(at, "ok", "wg 已就位")
			at++
		}
		if preflight {
			i := at
			job.Set(i, "running", "正在向 Cloudflare 注册")
			// 注册不通（够不着 Cloudflare、被判定滥用）时一条出口都别动，
			// 开关退回原样。翻开了才发现中转架不起来，等于把所有出口推进黑洞。
			acc, err := warpSvc.ensure(list[0].Slot, warpReportVersion())
			if err != nil {
				job.Set(i, "failed", err.Error())
				revert(err.Error())
				job.Finish()
				return
			}
			job.Set(i, "ok", "接入点 "+acc.Endpoint)
			i++
			job.Set(i, "running", "正在母机上试建设备并下发密钥")
			if err := warpSelfTest(acc); err != nil {
				// 这句是整个功能里最该被看见的一行：它说的不是"节点连不上"，
				// 而是"这台机器的 WireGuard 配不起来"。命令短，所以内核原话
				// 不会被截断吃掉。
				job.Set(i, "failed", err.Error())
				revert(err.Error())
				job.Finish()
				return
			}
			job.Set(i, "ok", "内核接受这套配置")
			at = i + 1
		}
		failed := 0
		aborted := ""
		for i, t := range list {
			job.Set(at+i, "running", "正在重连")
			m.reconnect(t, t.host())
			ok, why := warpWait(t, timeout)
			if ok {
				job.Set(at+i, "ok", "出口 IP "+t.exitIP())
				continue
			}
			failed++
			job.Set(at+i, "failed", why)
			// WARP 那一跳没握手时，剩下每条出口都会撞同一堵墙，而且每条都要
			// 等满 settle 时间——18 条就是二十多分钟的"看着在忙"。
			// 但只有"到此刻为止没有一条成功过"才停下并退回开关：前面已经有
			// 出口连着 WARP 的话把开关退回，界面就成了"中转已关闭"而那条出口
			// 还在走 Cloudflare，那种不一致比多等几条更难解释。
			if strings.Contains(why, "WARP 未握手") && failed == i+1 {
				aborted = why
				for k := at + i + 1; k < len(labels); k++ {
					job.Set(k, "failed", "没再重连：WARP 那一跳不通，换节点不会有用")
				}
				break
			}
		}
		if !on {
			// 关到底把注册一起退掉：留着就是 Cloudflare 那边一堆没人用的设备。
			warpSvc.dropAll()
		}
		if on && len(list) > 0 && aborted != "" {
			revert(aborted)
		} else if on && len(list) > 0 && failed == len(list) {
			// 全都回不来，说明这台机器现在跑不了中转。把开关原样退回，
			// 比留着一堆"显示已连通、其实一步都走不出去"的出口好。
			revert("所有出口都没能连上")
		} else if on && (len(list) == 0 || failed < len(list)) {
			// 有一条起来了（或本来就没有出口），上次那句失败就不该继续挂在开关下面
			warpFailNote("")
		}
		job.Finish()
		log.Printf("fanout: WARP 中转已%s，%d 条出口中有 %d 条没连上", verb, len(list), failed)
	}()
	return job, nil
}

// warpReregister 按当前上报版本重新注册各条出口的 WARP 账号。
//
// 这就是内置客户端那张卡上「更新 WARP」的全部含义：换一对密钥、重新注册，
// 于是每条出口拿到一个新的 Cloudflare 接入。fanout 不跑官方 warp-cli，
// 机器上也没有属于 fanout 的 WARP 二进制可以替换——把这件事写成"升级客户端"
// 是骗人的，按钮和说明都得照这个说法来。
func warpReregister(m *Manager) (*Job, error) {
	d := warpProbeDeps()
	if !d.Ready {
		return nil, fmt.Errorf("%s", d.Reason)
	}
	if !warpOn() {
		return nil, fmt.Errorf("中转还没打开，开了才谈得上重新注册")
	}
	list := m.Tunnels()
	if len(list) == 0 {
		return nil, fmt.Errorf("现在没有出口，没有可重新注册的线路")
	}
	// 开关开着却缺 wg（只跑过 f 更新的机器就这样）：先把它装上，
	// 否则下面每条出口都会以 "exec: wg: file not found" 红一遍。
	needTools := d.Steps != nil
	labels := make([]string, 0, len(list)+1)
	if needTools {
		labels = append(labels, "装 "+wireguardPkg)
	}
	for _, t := range list {
		labels = append(labels, fmt.Sprintf("出口 %d · %s", t.Slot, t.host()))
	}
	job := m.jobs.New("按最新客户端版本重新注册 WARP（"+strconv.Itoa(len(list))+" 条出口）", labels)
	timeout := warpSettle(true)

	go func() {
		at := 0
		if needTools {
			job.Set(at, "running", "正在用包管理器装 "+wireguardPkg)
			if err := warpEnsureTools(warpInstallWait); err != nil {
				job.Set(at, "failed", err.Error())
				job.Finish()
				return
			}
			job.Set(at, "ok", "wg 已就位")
			at++
		}
		for i, t := range list {
			job.Set(at+i, "running", "正在重新注册")
			warpSvc.drop(t.Slot)
			acc, err := warpSvc.ensure(t.Slot, warpReportVersion())
			if err != nil {
				job.Set(at+i, "failed", err.Error())
				continue
			}
			m.reconnect(t, t.host())
			if ok, why := warpWait(t, timeout); ok {
				job.Set(at+i, "ok", "Cloudflare 接入 "+acc.Endpoint)
			} else {
				job.Set(at+i, "failed", why)
			}
		}
		job.Finish()
	}()
	return job, nil
}

// warpOverview 是主界面那个开关旁边要说的全部话。
func warpOverview() WarpOverview {
	v := WarpOverview{Enabled: warpOn(), Version: warpReportVersion()}
	d := warpProbeDeps()
	v.Ready, v.Reason = d.Ready, d.Reason
	v.NeedTools, v.PkgCmd = d.Steps != nil, d.Cmd
	v.LastErr = warpFailText()
	if warpSvc != nil {
		want := warpReportVersion()
		for _, a := range warpSvc.all() {
			v.Count++
			if a.Version != want {
				// 手上还有按旧号注册的线路：界面上得说明"更新"其实没做完
				v.Stale++
			}
		}
	}
	return v
}
