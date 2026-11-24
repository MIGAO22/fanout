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

// removeKeyFile 删掉写过盘的私钥。删失败只记日志：文件是 0600，
// 而且下次架设时会被覆盖，不至于因为一次 unlink 失败就把出口拆掉。
func (s *warpService) removeKeyFile(dev string) {
	if err := os.Remove(filepath.Join(s.dir, warpKeyFileName(dev))); err != nil && !os.IsNotExist(err) {
		log.Printf("fanout: 删除 WireGuard 私钥 %s 失败: %v", dev, err)
	}
}

// keyFile 返回某条出口的私钥文件路径。
func (s *warpService) keyFile(dev string) string { return filepath.Join(s.dir, warpKeyFileName(dev)) }

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

// warpReady 判断这台机器有没有条件跑 WARP。
//
// 只查 wireguard-tools：`ip link add type wireguard` 能不能成取决于内核带没带
// 那个模块，而单独探测模块要么碰 modprobe 要么在母机上建个真设备，
// 代价比收益大——真不行的话建设备那条命令的报错本来就写得很清楚。
func warpReady() (bool, string) {
	switch {
	case warpSvc == nil:
		return false, "WARP 注册记录还没就绪（工作目录不可写？）"
	case !hasCmd("wg"):
		return false, "本机没有 wireguard-tools，装一个再来：apt install wireguard-tools（Alpine 是 apk add wireguard-tools）"
	default:
		return true, ""
	}
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

	keyFile := warpSvc.keyFile(dev)
	if err := os.WriteFile(keyFile, []byte(acc.PrivKey+"\n"), 0600); err != nil {
		return fmt.Errorf("写 WireGuard 私钥失败: %w", err)
	}
	mtu, _, _ := warpPathMTU(true)
	in := warpSetupInput{
		Dev: dev, KeyFile: keyFile, PeerKey: acc.PeerKey,
		Endpoint: acc.Endpoint, Port: acc.Port, TunIP: acc.TunIP, MTU: mtu,
		VethGw: sub + ".1", Veth: peer,
		DirectIPs: t.netnsNameservers(),
	}
	for _, c := range warpSetupCmds(in) {
		if err := run("ip", append([]string{"netns", "exec", ns}, c...)...); err != nil {
			return fmt.Errorf("%v（WARP 接入点 %s:%d）", err, acc.Endpoint, acc.Port)
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

// warpSplit 在 tun0 起来之后把用户流量接进 Gate。
// 失败必须往上传：漏掉这一步的话流量停在 Cloudflare，
// 出口 IP 变成 WARP 的而不是节点的，用户以为中转生效了、实际根本没走 Gate。
func (t *Tunnel) warpSplit(acc *WarpAccount) error {
	ns := t.nsName()
	n := t.node()
	ips := warpGateIPs(n.Config, n.IP)
	for _, name := range warpGateNames(n.Config) {
		ips = append(ips, warpResolveGate(name)...)
	}
	for _, c := range warpSplitCmds(acc.devName(), "tun0", ips) {
		if err := run("ip", append([]string{"netns", "exec", ns}, c...)...); err != nil {
			return fmt.Errorf("WARP 分流失败: %w", err)
		}
	}
	return nil
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
func warpWait(t *Tunnel, timeout time.Duration) (bool, string) {
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
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
	list := m.Tunnels()
	if on {
		if ok, why := warpReady(); !ok {
			return nil, fmt.Errorf("%s", why)
		}
		// 先把第一个账号注册出来再动开关：注册不通（没装 wireguard-tools、
		// 内核不带模块、Cloudflare 够不着）时直接拒绝，开关保持原样。
		// 翻开了才发现中转架不起来，等于把所有出口一起推进黑洞。
		if len(list) > 0 {
			if _, err := warpSvc.ensure(list[0].Slot, warpReportVersion()); err != nil {
				return nil, err
			}
		}
	}
	if err := setWarp(WarpSettings{Enabled: on, ClientVersion: cur.ClientVersion}); err != nil {
		return nil, err
	}

	labels := make([]string, 0, len(list))
	for _, t := range list {
		labels = append(labels, fmt.Sprintf("出口 %d · %s", t.Slot, t.host()))
	}
	if len(labels) == 0 {
		return nil, nil // 没有出口要重连，开关已经改完
	}
	verb := "关闭"
	if on {
		verb = "开启"
	}
	job := m.jobs.New(verb+" WARP 中转（"+strconv.Itoa(len(labels))+" 条出口逐条重连）", labels)
	timeout := warpSettle(on)

	go func() {
		failed := 0
		for i, t := range list {
			job.Set(i, "running", "正在重连")
			m.reconnect(t, t.host())
			if ok, why := warpWait(t, timeout); ok {
				job.Set(i, "ok", "出口 IP "+t.exitIP())
			} else {
				failed++
				job.Set(i, "failed", why)
			}
		}
		if !on {
			// 关到底把注册一起退掉：留着就是 Cloudflare 那边一堆没人用的设备。
			warpSvc.dropAll()
		}
		if failed == len(list) && on {
			// 全都回不来，说明这台机器现在跑不了中转。把开关原样退回，
			// 比留着一堆"显示已连通、其实一步都走不出去"的出口好。
			if err := setWarp(WarpSettings{Enabled: false, ClientVersion: cur.ClientVersion}); err != nil {
				log.Printf("fanout: 回滚 WARP 开关失败: %v", err)
			}
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
	if ok, why := warpReady(); !ok {
		return nil, fmt.Errorf("%s", why)
	}
	if !warpOn() {
		return nil, fmt.Errorf("中转还没打开，开了才谈得上重新注册")
	}
	list := m.Tunnels()
	labels := make([]string, 0, len(list))
	for _, t := range list {
		labels = append(labels, fmt.Sprintf("出口 %d · %s", t.Slot, t.host()))
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("现在没有出口，没有可重新注册的线路")
	}
	job := m.jobs.New("按最新客户端版本重新注册 WARP（"+strconv.Itoa(len(labels))+" 条出口）", labels)
	timeout := warpSettle(true)

	go func() {
		for i, t := range list {
			job.Set(i, "running", "正在重新注册")
			warpSvc.drop(t.Slot)
			acc, err := warpSvc.ensure(t.Slot, warpReportVersion())
			if err != nil {
				job.Set(i, "failed", err.Error())
				continue
			}
			m.reconnect(t, t.host())
			if ok, why := warpWait(t, timeout); ok {
				job.Set(i, "ok", "Cloudflare 接入 "+acc.Endpoint)
			} else {
				job.Set(i, "failed", why)
			}
		}
		job.Finish()
	}()
	return job, nil
}

// warpOverview 是主界面那个开关旁边要说的全部话。
func warpOverview() WarpOverview {
	v := WarpOverview{Enabled: warpOn(), Version: warpReportVersion()}
	if ok, why := warpReady(); ok {
		v.Ready = true
	} else {
		v.Reason = why
	}
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
