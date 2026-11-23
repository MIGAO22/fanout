package main

import (
	"encoding/json"
	"fmt"
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
}

// kill 结束进程。幂等：进程已退或从没起来都不报错。
func (o *ovpnProc) kill() {
	if o == nil || o.cmd == nil || o.cmd.Process == nil {
		return
	}
	_ = o.cmd.Process.Kill()
	// 把 done 排空一次，避免 Wait 的结果没人收
	select {
	case <-o.done:
	default:
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
	if err := run("ip", "netns", "exec", ns, "ip", "route", "add", "default", "via", sub+".1"); err != nil {
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
func ovpnTuning(budget time.Duration) []string {
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
		// 表现是"能连上但网页打不开"。
		"--mssfix", "1380",
	}
}

// startOpenVPN 在 netns 内拉起 openvpn，并等待 tun0 拿到地址。
// 就绪与否以"tun0 上有了 IPv4"为准——这是 SOCKS5 能出网的真正前提。
func (t *Tunnel) startOpenVPN(dir string) error {
	ns := t.nsName()
	cfgPath := filepath.Join(dir, ns+".ovpn")
	if err := os.WriteFile(cfgPath, []byte(t.node().Config), 0600); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	authPath := filepath.Join(dir, "auth.txt")
	if err := os.WriteFile(authPath, []byte("vpn\nvpn\n"), 0600); err != nil {
		return fmt.Errorf("写凭据失败: %w", err)
	}

	budget := time.Duration(connConf().ConnectBudget) * time.Second

	logPath := filepath.Join(dir, ns+".log")
	args := []string{
		"--config", cfgPath,
		"--auth-user-pass", authPath,
		"--auth-nocache",
		"--dev", "tun0",
		"--verb", "3",
		"--log", logPath,
	}
	args = append(args, dataCipherArgs(parseOpenVPNVersion(probeVersion("openvpn", "--version")))...)
	args = append(args, ovpnTuning(budget)...)
	cmd := exec.Command("ip", append([]string{"netns", "exec", ns, "openvpn"}, args...)...)

	// 自己 Wait 并把结果送进 channel：既回收了子进程不留僵尸，
	// 又让"进程已退出"变成可等待的事件，而不是靠轮询 ProcessState。
	done := make(chan error, 1)
	if err := cmdStart(cmd); err != nil {
		return fmt.Errorf("启动 openvpn 失败: %w", err)
	}
	go func() { done <- cmd.Wait() }()

	old := t.proc()
	t.setProc(&ovpnProc{cmd: cmd, done: done})
	// 上一节点的进程如果还在（换节点时走到这一步），现在可以收了：
	// 新进程已经起来了，旧 netns 也早就拆过，留着只会占着一个口。
	old.kill()

	waitErr := t.waitTunUp(ns, budget, done)
	if waitErr != nil {
		return fmt.Errorf("%v，详见 %s", waitErr, logPath)
	}
	return nil
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
