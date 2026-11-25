package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SocksCred 是一条隧道的 SOCKS5 访问凭据。
//
// 每条隧道一套独立凭据：泄露一条不会连累其他出口，
// 换节点时也能只重置这一条而不影响已分发的其他配置。
type SocksCred struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

// Tunnel 是一条运行中的隧道：一个 netns + 一个 openvpn 进程 + 一个本地 SOCKS5 端口。
//
// Status/Err/ExitIP/Node/Port/Since 这几项会被三类人同时碰：拉起它的后台 goroutine、
// 健康检查、以及界面每几秒一次的轮询。写一律走下面的 setter，读走 state()，
// 否则 -race 下就是数据竞争，实际表现是界面读到半更新的状态。
type Tunnel struct {
	Slot   int       `json:"slot"`
	Port   int       `json:"port"`
	Node   Node      `json:"node"`
	Status string    `json:"status"` // starting | up | failed | stopped
	ExitIP string    `json:"exit_ip"`
	Err    string    `json:"err,omitempty"`
	Since  time.Time `json:"since"`
	Cred   SocksCred `json:"cred"`

	ns       string
	listener net.Listener
	ovpn     *ovpnProc
	mu       sync.Mutex
	// swapped 是这条出口换节点时用过的 hostname，按时间先后排。
	// 手动换节点要避开它们：只排除"当前这个"的话，连点两次就会在
	// 两个节点之间来回跳（A 换成 B，B 再换回 A）。
	swapped []string
	// prevHost 是"换节点动作还没收尾"的标记：记着换之前绑的是谁。
	//
	// 换节点分两步——先把隧道连到新节点，再把入站从旧节点改绑过来。
	// 两步之间崩溃或重启的话，存盘的隧道已经是新节点、而入站还指着旧节点，
	// 两边对不上，那个入站就掉成了没人认领的孤儿。
	// 这个字段跟着状态一起落盘，重启后照着它把入站接回来。
	prevHost string
}

// ovpnProc 包住 openvpn 子进程，顺带把"进程什么时候退出"变成可等的 channel。
//
// 原来代码是 `go cmd.Wait()` 丢弃结果，于是 cmd.ProcessState 永远不为 nil 之前
// 主流程已经开始轮询，退出判断形同虚设——openvpn 秒退也要空等满 40 秒才发现。
type ovpnProc struct {
	cmd  *exec.Cmd
	done chan error // 进程退出后由 wait goroutine 写入

	// stopped 由 kill 关闭，用来告诉"等退出"的那位这次是我们自己动的手，
	// 不必再把这一下当成掉线记进界面。
	stopped  chan struct{}
	stopOnce sync.Once

	// dead 关闭表示进程已经退出，deadWhy 在那之前写好（靠 close 建立先后关系）。
	// 健康检查拿它当**确定判据**：进程没了就不存在"再观察两轮"的意义，
	// 原来要等满 HealthFailures × HealthInterval（默认 30 秒）才判死，
	// 那 30 秒里界面是绿的、客户端却在等一条已经不存在的路。
	dead    chan struct{}
	deadWhy string
}

// newOvpnProc 造一个带完整 channel 的进程句柄。
func newOvpnProc(cmd *exec.Cmd, done chan error) *ovpnProc {
	return &ovpnProc{
		cmd:     cmd,
		done:    done,
		stopped: make(chan struct{}),
		dead:    make(chan struct{}),
	}
}

// markDead 记下进程退出的原因；重复调用只有第一次有效。
func (o *ovpnProc) markDead(why string) {
	select {
	case <-o.dead:
	default:
		o.deadWhy = why
		close(o.dead)
	}
}

// deadReason 报"这个进程是不是已经退出了"以及原因。
func (o *ovpnProc) deadReason() (string, bool) {
	if o == nil {
		return "", false
	}
	select {
	case <-o.dead:
		return o.deadWhy, true
	default:
		return "", false
	}
}

// kill 结束进程。幂等：进程已退或从没起来都不报错。
func (o *ovpnProc) kill() {
	if o == nil {
		return
	}
	// 先记下"这是我主动收的手"，再去杀进程。放在 cmd 判空之前：进程早就自己退了
	// 或者从没起来过时，盯着它的那个 goroutine 也得有人替它收工，否则每次换节点
	// 都要漏一条挂在 select 上的 goroutine。
	if o.stopped != nil {
		o.stopOnce.Do(func() { close(o.stopped) })
	}
	if o.cmd == nil || o.cmd.Process == nil {
		return
	}
	_ = o.cmd.Process.Kill()
	// 把 done 排空一次，避免 Wait 的结果没人收
	select {
	case <-o.done:
	default:
	}
}

// watchOvpnExit 在隧道已经起来之后继续盯着那个进程，把退出变成一条界面看得见的理由。
//
// waitTunUp 只在建立阶段等 done；就绪之后就没人收这个 channel 了，于是 openvpn 秒退
// （节点端口不该答、证书过期、被 OOM 杀掉）在界面上仍然是一句"绿着"，
// 真正的死因留在 logPath 里没人读。这里把它接上：谁先发生算谁。
func watchOvpnExit(o *ovpnProc, logPath string) {
	select {
	case err := <-o.done:
		why := "openvpn 进程已退出"
		if err != nil {
			why += "：" + err.Error()
		}
		if h := ovpnLogHint(logPath); h != "" {
			why += "（" + h + "）"
		}
		o.markDead(oneLine(why, 220))
	case <-o.stopped:
		// 主动收手：换节点/停出口时调用方已经改过状态，这里不再记第二次原因。
	}
}

// TunnelState 是运行态的一次一致性快照。
type TunnelState struct {
	Status string
	Err    string
	ExitIP string
	Node   Node
	Port   int
	Since  time.Time
}

func (t *Tunnel) state() TunnelState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TunnelState{
		Status: t.Status, Err: t.Err, ExitIP: t.ExitIP,
		Node: t.Node, Port: t.Port, Since: t.Since,
	}
}

func (t *Tunnel) status() string { return t.state().Status }
func (t *Tunnel) exitIP() string { return t.state().ExitIP }
func (t *Tunnel) host() string   { return t.state().Node.HostName }
func (t *Tunnel) port() int      { return t.state().Port }
func (t *Tunnel) node() Node     { return t.state().Node }

// setStatus 改状态与附带的说明。给 Err 传空串表示清掉上一次的失败原因。
func (t *Tunnel) setStatus(status, errMsg string) {
	t.mu.Lock()
	t.Status = status
	t.Err = errMsg
	t.mu.Unlock()
}

// setUp 是连上之后的收尾：状态、出口 IP、清错误、重置计时。
// 三件事必须一次做完，界面才不会读到 status=up 但 exit_ip 还是空的中间态。
func (t *Tunnel) setUp(ip string) {
	t.mu.Lock()
	t.Status = "up"
	t.ExitIP = ip
	t.Err = ""
	t.Since = time.Now()
	t.mu.Unlock()
}

func (t *Tunnel) setNode(n Node) {
	t.mu.Lock()
	t.Node = n
	t.mu.Unlock()
}

func (t *Tunnel) setExitIP(ip string) {
	t.mu.Lock()
	t.ExitIP = ip
	t.mu.Unlock()
}

// ovpnProc 取当前 openvpn 进程句柄（可能为 nil）。
func (t *Tunnel) proc() *ovpnProc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ovpn
}

// procDead 报"这条出口的 openvpn 进程是不是已经退了"。
//
// 这是确定证据，不是"探测失败第 N 次"那种需要防抖的信号：进程不会退了又活着。
// 健康检查据此跳过 HealthFailures 直接重连，把界面上"绿着但全不通"的时间
// 从 3 个检查周期压到 1 个。
func (t *Tunnel) procDead() (string, bool) {
	return t.proc().deadReason()
}

// netnsHasDefault 看这条 netns 此刻有没有默认路由。
//
// 它是"隧道到底有没有把流量接走"的直接证据：netns 建架时故意不装默认路由
// （见 netnsDirectCmds），唯一的默认路由应该来自节点推送。没有就说明这条节点
// 连上了却不接流量——表现是隧道状态是 up、客户端却一个包都出不去。
func netnsHasDefault(ns string) bool {
	out, err := cmdCombined(exec.Command("ip", "netns", "exec", ns,
		"ip", "-o", "route", "show", "default"))
	if err != nil {
		return false
	}
	return routeHasDefault(string(out))
}

func (t *Tunnel) setProc(o *ovpnProc) {
	t.mu.Lock()
	t.ovpn = o
	t.mu.Unlock()
}

// listenerRef 取当前 SOCKS5 监听，取不到返回 nil。
func (t *Tunnel) getListener() net.Listener {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.listener
}

// MarshalJSON 让落盘也走锁。
//
// saveState 会把整个隧道数组 json.Marshal 一遍，那是在别的 goroutine 里读的，
// 不加锁就与 setter 冲突。type plain Tunnel 没有继承本方法，不会递归。
func (t *Tunnel) MarshalJSON() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	type plain Tunnel
	return json.Marshal((*plain)(t))
}

// prevHostOf 读"换节点未收尾"标记。
func (t *Tunnel) prevHostOf() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prevHost
}

// setPrevHost 记下或清掉"换节点未收尾"标记。空串表示已经收尾。
func (t *Tunnel) setPrevHost(host string) {
	t.mu.Lock()
	t.prevHost = host
	t.mu.Unlock()
}

// swapHistoryMax 是换节点历史的上限。
//
// 留太多会把同地区的候选节点排干，反而让自动重连没得选；
// 留太少又挡不住来回跳。一个地区的可用节点通常个位数到十几个，
// 16 条足够覆盖"一轮都换过"的情况。
const swapHistoryMax = 16

// swapAvoid 返回换节点时要跳过的 hostname：历史用过的，加上当前这个。
func (t *Tunnel) swapAvoid() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]bool, len(t.swapped)+1)
	for _, h := range t.swapped {
		out[h] = true
	}
	if t.Node.HostName != "" {
		out[t.Node.HostName] = true
	}
	return out
}

// rememberSwap 把一个换掉的节点记进历史，超出上限就丢最早的。
func (t *Tunnel) rememberSwap(host string) {
	if host == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range t.swapped {
		if h == host {
			return
		}
	}
	t.swapped = append(t.swapped, host)
	if len(t.swapped) > swapHistoryMax {
		t.swapped = t.swapped[len(t.swapped)-swapHistoryMax:]
	}
}

// forgetSwaps 清空换节点历史。同地区的节点都换过一轮之后要清一次，
// 否则用户再点就只能收到"没有可用节点"。
func (t *Tunnel) forgetSwaps() {
	t.mu.Lock()
	t.swapped = nil
	t.mu.Unlock()
}

// 名字里都带上实例标识，否则同机第二个 fanout 会把这条隧道拆掉（见 instance.go）。
// 默认工作目录下 instTag 是空串、instBase 是 99，名字与老版本完全一致。
func (t *Tunnel) nsName() string { return fmt.Sprintf("fo%s%d", instTag, t.Slot) }
func (t *Tunnel) subnet() string { return fmt.Sprintf("10.%d.%d", instBase, t.Slot) }

// vethNames 返回母机侧与 netns 侧的网卡名。
// 网卡名上限 15 个字符，"fov" + 4 位标识 + 槽位最多 9 个，留足余量。
func (t *Tunnel) vethNames() (string, string) {
	return fmt.Sprintf("fov%s%d", instTag, t.Slot), fmt.Sprintf("fop%s%d", instTag, t.Slot)
}

// run 执行一条网络配置命令。
//
// 走 cmdCombined 而不是直接 exec：建 veth、改 iptables 都是"在当前网络
// 命名空间里生效"的操作，线程可能已经被 dialerInNetns 带进某条隧道，
// 那样规则会加到隧道内部，母机上什么也没有（见 netnsguard.go）。
func run(name string, args ...string) error {
	out, err := cmdCombined(exec.Command(name, args...))
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runQuiet 执行清理类命令，忽略"本来就不存在"这类错误。
func runQuiet(name string, args ...string) {
	_ = cmdRun(exec.Command(name, args...))
}

// setupNetns 建立 netns 与 veth 链路，并配好 NAT 与转发放行。
func (t *Tunnel) setupNetns() error {
	ns, sub := t.nsName(), t.subnet()
	veth, peer := t.vethNames()

	t.teardownNetns()

	if err := run("ip", "netns", "add", ns); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"); err != nil {
		return err
	}
	if err := run("ip", "link", "add", veth, "type", "veth", "peer", "name", peer); err != nil {
		return err
	}
	if err := run("ip", "link", "set", peer, "netns", ns); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", sub+".1/30", "dev", veth); err != nil {
		return err
	}
	if err := run("ip", "link", "set", veth, "up"); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "addr", "add", sub+".2/30", "dev", peer); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", peer, "up"); err != nil {
		return err
	}

	// netns 内的 DNS，仅用于 openvpn 解析远端主机名
	nsDir := filepath.Join("/etc/netns", ns)
	if err := os.MkdirAll(nsDir, 0755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", nsDir, err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0644); err != nil {
		return fmt.Errorf("写 resolv.conf 失败: %w", err)
	}

	// 这里刻意**不装默认路由**。openvpn 要连的地址和 DNS 逐条 /32 放行，
	// 剩下的路一条都不给——隧道没连着时"出不去"是正确结果，
	// "从母机出去"才是事故（原来那条 default via 母机就是漏口，见 netnsDirectCmds）。
	// 隧道的默认路由由节点推送；开 WARP 时由 warpSetupCmds 换成 WireGuard 设备。
	pins := netnsDirectCmds(netnsDirect{
		Gw:  sub + ".1",
		Dev: peer,
		IPs: append(nodeEgressIPs(t), t.netnsNameservers()...),
	})
	for _, c := range pins {
		if err := run("ip", append([]string{"netns", "exec", ns}, c...)...); err != nil {
			return err
		}
	}

	cidr := sub + ".0/30"
	ensureRule("nat", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	ensureRuleInsert("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	ensureRuleInsert("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
	return nil
}

// ensureRule 幂等追加一条 iptables 规则。
func ensureRule(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if cmdRun(exec.Command("iptables", check...)) == nil {
		return
	}
	add := append([]string{"-w", "5", "-t", table, "-A", chain}, spec...)
	runQuiet("iptables", add...)
}

// ensureRuleInsert 幂等插入规则到链首。
// FORWARD 链末尾常有兜底 REJECT，必须插到最前面才生效。
func ensureRuleInsert(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if cmdRun(exec.Command("iptables", check...)) == nil {
		return
	}
	ins := append([]string{"-w", "5", "-t", table, "-I", chain, "1"}, spec...)
	runQuiet("iptables", ins...)
}

func (t *Tunnel) teardownNetns() {
	ns, sub := t.nsName(), t.subnet()
	cidr := sub + ".0/30"
	veth, _ := t.vethNames()
	runQuiet("ip", "netns", "del", ns)
	runQuiet("ip", "link", "del", veth)
	runQuiet("iptables", "-w", "5", "-t", "nat", "-D", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	runQuiet("iptables", "-w", "5", "-D", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	runQuiet("iptables", "-w", "5", "-D", "FORWARD", "-d", cidr, "-j", "ACCEPT")
}

// ovpnTuning 是给 openvpn 加的一串保活与快速失败参数。
//
// 原版只给了 --connect-retry-max/--connect-timeout，其它全用 openvpn 默认值，
// 有两个默认值在 VPN Gate 这种志愿者节点上是有害的：
//
//   - --resolv-retry 默认 infinite：节点主机名解析不了时 openvpn 会永远重试 DNS，
//     进程既不退出也不报错，fanout 只能干等满就绪超时才换下一个候选。
//   - 没有 --keepalive：志愿者节点最常见的失效方式是"TCP 还连着但不再转发"，
//     没有心跳就没人发现，这条出口在界面上一直是绿的，实际全在超时。
func ovpnTuning(budget time.Duration, mssfix int) []string {
	// 握手窗口必须显著小于总预算，否则一个卡在半程的节点会把整轮候选都拖住。
	hand := int((budget / 3).Seconds())
	if hand < 5 {
		hand = 5
	}
	if hand > 20 {
		hand = 20
	}
	return []string{
		// 每 15 秒发一次控制通道心跳，45 秒没回应就重启数据通道。
		// 45 秒这个值要比重连判定快，让 openvpn 自己能恢复的它自己恢复，
		// 别动不动就惊动 fanout 换节点。
		"--keepalive", "15", "45",
		"--hand-window", strconv.Itoa(hand),
		"--tls-timeout", "1",
		"--connect-timeout", "15",
		"--connect-retry", "3", "30",
		// 三次连不上就退出。留着进程在 netns 里无限重连没有意义：
		// fanout 手上还有别的候选节点，换人的成功率比它继续磨高得多。
		"--connect-retry-max", "3",
		// 解析失败别无限重试（见上）
		"--resolv-retry", "3",
		// 很多 Gate 配置里写了不止一个 remote，默认按顺序试，
		// 第一个往往是先挂的那个；随机化让多次尝试落在不同地址上。
		"--remote-random",
		// 隧道 MTU 比物理链路小，不夹 MSS 的话大包静默丢失，
		// 表现是"能连上但网页打不开"。开 WARP 时外层又多 encapsulate 了一层，
		// 这个数由 warpPathMTU 一起给（见 warp.go）。
		"--mssfix", strconv.Itoa(mssfix),
	}
}

// startOpenVPN 在 netns 内拉起 openvpn，并等待 tun0 拿到地址。
// 就绪与否以"tun0 上有了 IPv4"为准——这是 SOCKS5 能出网的真正前提。
//
// acc 非空表示这条出口的默认路由已经进了 WARP：连接预算要加上那一跳的开销，
// 否则好好的节点会因为多走一跳被判成超时而白白换掉；MTU 和路由参数也要跟着变。
func (t *Tunnel) startOpenVPN(dir string, acc *WarpAccount) error {
	ns := t.nsName()
	cfgPath := filepath.Join(dir, ns+".ovpn")
	if err := os.WriteFile(cfgPath, []byte(t.node().Config), 0600); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	authPath := filepath.Join(dir, "auth.txt")
	if err := os.WriteFile(authPath, []byte("vpn\nvpn\n"), 0600); err != nil {
		return fmt.Errorf("写凭据失败: %w", err)
	}

	on := acc != nil
	budget := warpBudget(time.Duration(connConf().ConnectBudget)*time.Second, on)
	// 三层 MTU 一次读出来一起用：外层是 WireGuard，再外面是母机那块真网卡，
	// 按它而不是按 1500 算（见 warpPathMTU）。
	_, tunMTU, mssfix := warpMTUs(on)

	logPath := filepath.Join(dir, ns+".log")
	args := []string{
		"--config", cfgPath,
		"--auth-user-pass", authPath,
		"--auth-nocache",
		"--dev", "tun0",
		"--verb", "3",
		"--log", logPath,
	}
	args = append(args, warpOpenVPNArgs(on, tunMTU)...)
	// ovpnBinary 和版本探测必须同一个入口：喂给 openvpn 的参数名是按版本挑的
	// （2.4 叫 --ncp-ciphers、2.5+ 叫 --data-ciphers，--pull-filter 也是 2.4 才有），
	// 一旦"探的是系统那份、跑的是编出来的那份"，openvpn 会当场 Unrecognized option 退出。
	ovpnBin := openvpnBinary()
	ovpnVer := parseOpenVPNVersion(probeVersion(ovpnBin, "--version"))
	// AEAD-only 那一档是设置里的开关，但只对"节点自己声明支持 AEAD"的出口生效：
	// VPN Gate 的配置大多写死 data-ciphers AES-128-CBC，那种节点上硬上 AEAD
	// 是一个都连不上，而不是慢一点。开压缩的节点同理（--allow-compression no 会冲突）。
	aeadOnly := connConf().AeadOnly && nodeSupportsAEAD(t.node().Config)
	args = append(args, dataCipherArgs(ovpnVer, aeadOnly)...)
	if aeadOnly {
		args = append(args, compressionOffArgs(ovpnVer)...)
	}
	// 开着 WARP 时还要挡住服务器推送的 MTU：推送的优先级比命令行高，
	// 而这三层数字是按外层封装算出来的（见 warpPathMTU）。
	args = append(args, warpPullFilters(on, ovpnVer)...)
	args = append(args, ovpnTuning(budget, mssfix)...)
	cmd := exec.Command("ip", append([]string{"netns", "exec", ns, ovpnBin}, args...)...)

	// 自己 Wait 并把结果送进 channel：既回收了子进程不留僵尸，
	// 又让"进程已退出"变成可等待的事件，而不是靠轮询 ProcessState。
	done := make(chan error, 1)
	if err := cmdStart(cmd); err != nil {
		return fmt.Errorf("启动 openvpn 失败: %w", err)
	}
	go func() { done <- cmd.Wait() }()

	old := t.proc()
	p := newOvpnProc(cmd, done)
	t.setProc(p)
	// 上一节点的进程如果还在（换节点时走到这一步），现在可以收了：
	// 新进程已经起来了，旧 netns 也早就拆过，留着只会占着一个口。
	old.kill()

	waitErr := t.waitTunUp(ns, budget, done)
	if waitErr != nil {
		// 光说"连不上"没有价值：同一句错背后可能是认证被拒、证书过期、
		// 外层包发大了，也可能只是这个节点今天死了。把日志里那句和 WARP
		// 那一跳的实况一起带上，界面和作业里才看得见下一步该干什么。
		var hints []string
		if h := ovpnLogHint(logPath); h != "" {
			hints = append(hints, h)
		}
		if on {
			hints = append(hints, warpHopNote(ns, acc))
		}
		tail := ""
		if len(hints) > 0 {
			tail = "；" + strings.Join(hints, "；")
		}
		return fmt.Errorf("%v%s，详见 %s", waitErr, tail, logPath)
	}
	// 隧道起来了，之后这个进程的退出就只剩"意外"这一种：接着盯住它，
	// 让健康检查第一轮就能判死，而不是等满 3 轮防抖。
	go watchOvpnExit(p, logPath)
	return nil
}

// ovpnOwnFaults 是"我们的链路有问题"那一类：说出这句话时，换多少个节点都没用，
// 所以它必须优先于节点那类报错。
//
// 关键句只写"message too long"而不是整句 "write UDP: …"：openvpn 在不同版本里
// 打成 "write UDPv4: Message too long [nonblocking:1]"、"sendto …: Message too long"，
// 前缀一变就漏检，而那正是我们要认的那一种。
var ovpnOwnFaults = []struct{ needle, why string }{
	{"message too long", "外层包超过链路 MTU"},
	{"emsgsize", "外层包超过链路 MTU"},
	// AEAD-only 开着时的两种"是我们这个选择造成的"，必须排在通用的
	// "options error" 前面：那两句也都是 OPTIONS ERROR 开头，
	// 只报"参数没被接受"会把人指向完全错误的方向。
	// 关键句只取 "failed to negotiate cipher"：openvpn 对老节点和对我们表里
	// 缺算法两种情况分别印 "…Configure --data-ciphers-fallback…" 和
	// "…Add the server's cipher…"，只认其中一句就会漏掉一半。
	{"failed to negotiate cipher", "数据通道算法和节点谈不拢（开着 AEAD-only 就先关掉再试）"},
	{"pushed compression settings", "节点要求开压缩，AEAD-only 不让开"},
	{"options error", "参数没被接受"},
	{"unrecognized option", "参数没被接受"},
}

// ovpnNodeFaults 是"这个节点不行"那一类：程序本来就会顺着候选换下一个，
// 说清是哪一种只是为了让人知道为什么还在转。
// 顺序即同一行里的优先级：证书过期比"校验不过"更能指明下一步。
var ovpnNodeFaults = []struct{ needle, why string }{
	{"certificate has expired", "节点证书过期"},
	{"verify error", "节点证书校验不过"},
	{"auth_failed", "节点拒绝了用户名/密码"},
	{"authentication failed", "节点拒绝了用户名/密码"},
	// openvpn 2.7 起会把 TLS alert 发给对端并记一句 "… SSL alert: …"。
	// 这一句比"握手没走完"具体得多——是节点主动拒了握手，不是网络没到。
	{"ssl alert:", "节点在 TLS 层直接拒绝（见原文）"},
	{"tls_error", "TLS 握手没走完"},
	{"tls handshake failed", "TLS 握手没走完"},
	{"cannot resolve host address", "节点域名解析不出来"},
	{"network is unreachable", "母机到节点没有路由"},
	{"no route to host", "母机到节点没有路由"},
	{"connection reset", "节点掐了连接"},
	{"connection timed out", "节点的端口没应答"},
	{"peer connection initiated", "节点已应答，卡在后面的步骤"},
}

// whyHere 按表里的顺序找第一条命中的关键句，返回它的说法。
// 顺序就是同一行里的优先级——"Message too long" 和 "Connection timed out"
// 常常同时出现，但只有前者说明是我们把包发大了。
func whyHere(table []struct{ needle, why string }, lower string) string {
	for _, p := range table {
		if strings.Contains(lower, p.needle) {
			return p.why
		}
	}
	return ""
}

// ovpnErrorLine 从 openvpn 日志文本里挑最能说明问题的一句。
//
// 先给"我们自己的链路有问题"那一类（MTU、参数），因为换节点不会有用；
// 剩下的取**最后**匹配上的一条：openvpn 一轮里会按顺序试好几个 remote，
// 前面那些是过程，最后那句才是这一轮的结论。
func ovpnErrorLine(log string) string {
	own, node := "", ""
	for _, line := range strings.Split(log, "\n") {
		l := strings.ToLower(line)
		if w := whyHere(ovpnOwnFaults, l); w != "" {
			own = w + "：" + strings.TrimSpace(line)
			continue
		}
		if w := whyHere(ovpnNodeFaults, l); w != "" {
			node = w + "：" + strings.TrimSpace(line)
		}
	}
	if own != "" {
		return oneLine(own, 220)
	}
	return oneLine(node, 220)
}

// oneLine 把多行/带制表符的文本压成一行并截断：这句要进状态栏和作业标题，
// 留换行会把界面撑坏。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ovpnLogHint 读这条隧道日志的尾巴并挑一句。只读最后 64KB：
// 连不上的节点会被反复重试，整份读进来对一个错误提示没有意义。
func ovpnLogHint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return ""
	}
	const span = 64 << 10
	off := st.Size() - span
	if off < 0 {
		off = 0
	}
	buf := make([]byte, int(st.Size()-off))
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}
	return ovpnErrorLine(string(buf))
}

// tun0Up 看 netns 里的 tun0 是否已经拿到地址。
func tun0Up(ns string) bool {
	out, err := cmdOutput(exec.Command("ip", "netns", "exec", ns, "ip", "-4", "addr", "show", "tun0"))
	return err == nil && strings.Contains(string(out), "inet ")
}

// waitTunUp 等 tun0 就绪。进程提前退出或超过预算都立刻返回失败。
//
// 原来是一段固定 40 秒的 1 秒轮询：节点连不上要满 40 秒才知道，
// 一轮 6 个候选全挂就是 4 分钟。这里改成"事件 + 预算"，
// 进程一退就返回，好节点通常 2-3 秒就过。
func (t *Tunnel) waitTunUp(ns string, budget time.Duration, done chan error) error {
	deadline := time.After(budget)
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return fmt.Errorf("等待 tun0 就绪超时（预算 %.0f 秒）", budget.Seconds())
		case err := <-done:
			if err != nil {
				return fmt.Errorf("openvpn 退出: %v", err)
			}
			return fmt.Errorf("openvpn 意外结束")
		case <-tick.C:
			if tun0Up(ns) {
				return nil
			}
		}
	}
}

// serve 在母机上监听 SOCKS5 端口，出站连接则在 netns 内建立。
// 监听必须留在母机侧：netns 内的 loopback 与母机彼此独立，
// 监听在 netns 里的话外部根本连不上。
func (t *Tunnel) serve() error {
	// 端口要尽量保持不变，否则用户已经分发出去的客户端配置会失效。
	// 进程刚重启时旧监听可能还在 TIME_WAIT，这里给几秒重试窗口。
	port := t.port()
	var ln net.Listener
	var err error
	for i := 0; i < 6; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		// 确实被别的进程长期占用了，才换端口。
		// 组监听绑在 127.0.0.1，和这里要绑的 0.0.0.0 互斥，必须一起避开。
		taken := poolPorts()
		taken[port] = true
		alt, perr := freeRandomPort(taken)
		if perr != nil {
			return fmt.Errorf("监听 %d 失败且无备用端口: %w", port, err)
		}
		ln, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", alt))
		if err != nil {
			return fmt.Errorf("监听 %d 失败: %w", alt, err)
		}
		t.setPort(alt)
	}
	t.mu.Lock()
	t.listener = ln
	t.mu.Unlock()
	dial := dialerInNetns(t.nsName())

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 每次连接现取凭据：改口令后不必重建监听，新连接立刻按新凭据校验
			cred := t.credential()
			go serveSocks(conn, &cred, dial)
		}
	}()
	return nil
}

// credential 取一份凭据副本，避免读写并发。
func (t *Tunnel) credential() SocksCred {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Cred
}

// setCredential 换掉这条隧道的 SOCKS5 凭据。已建立的连接不受影响，
// 新连接立即按新凭据校验。
func (t *Tunnel) setCredential(c SocksCred) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Cred = c
}

func (t *Tunnel) setPort(p int) {
	t.mu.Lock()
	t.Port = p
	t.mu.Unlock()
}

// probeExitIP 通过隧道查询出口 IP，用于确认这条隧道确实换了 IP。
// 探测源与超时策略见 health.go 的 probeExitIPIn。
//
// 这里用健康检查的超时而不是 connect_budget：连接一个候选节点的总预算
// 已经是 tun0 就绪 + 一次出口查询，两者都用大预算会让"每个候选都要等两遍"，
// 上层重试整轮的等待也跟着翻倍。
func (t *Tunnel) probeExitIP() (string, error) {
	budget := 2 * time.Duration(connConf().HealthTimeout) * time.Second
	return probeExitIPIn(t.nsName(), budget)
}

// stop 停止这条隧道并清理它占用的所有资源。
//
// 拆 netns、删 veth、改 iptables 都是秒级的慢操作，原先整段在 t.mu 里跑，
// 期间所有读状态的 goroutine（包括界面轮询）都被卡在锁上。
// 现在锁只保护字段变更那几行，清理放到锁外。
func (t *Tunnel) stop() {
	t.mu.Lock()
	ln := t.listener
	proc := t.ovpn
	t.listener = nil
	t.ovpn = nil
	t.Status = "stopped"
	t.ExitIP = ""
	t.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	proc.kill()
	t.teardownNetns()
}
