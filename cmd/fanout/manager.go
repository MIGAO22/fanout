package main

import (
	"fmt"
	"log"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Manager 维护所有隧道，负责分配槽位与端口。
type Manager struct {
	mu       sync.RWMutex
	tunnels  map[int]*Tunnel
	nodes    []Node
	fetched  time.Time
	workDir  string
	maxSlots int
	jobs     JobStore

	// 节点列表的并发控制：一次拉取要下几十 MB 的 CSV，
	// 多个入口（界面轮询、重试、新建向导）同时触发会把带宽和 VPN Gate 一起打爆。
	fetchMu    sync.Mutex
	fetchWait  chan fetchResult
	fetchFailA time.Time
	// nodesMode 记住这份列表是按哪种打分策略排的，换了策略就要重排。
	nodesMode string
}

// fetchResult 是一次节点列表拉取的产物，通过 channel 广播给所有等待者。
type fetchResult struct {
	count int
	err   error
}

// 拉取节点列表的整体超时，以及失败后的冷却时间。
//
// 冷却是有意的：拉取失败时列表还是旧的（可能已过 TTL），若不加冷却，
// 每次挑节点都会重新去拉那份几十 MB 的 CSV，故障期间等于自我 DDoS。
const (
	nodeFetchTimeout  = 60 * time.Second
	nodeFetchCooldown = 45 * time.Second
)

func NewManager(maxSlots int, workDir string) *Manager {
	return &Manager{
		tunnels:  map[int]*Tunnel{},
		workDir:  workDir,
		maxSlots: maxSlots,
	}
}

// RefreshNodes 无条件重新拉取节点列表。
func (m *Manager) RefreshNodes() (int, error) {
	return m.refreshNodes()
}

// ensureNodes 保证手上这份节点列表够新。
//
// TTL 内直接用缓存；过期才去拉，且同一时刻只允许一次拉取在跑，
// 其余调用者等它的结果。列表还没拉到过时（启动阶段）必须真拉一次。
// 拉取失败退回旧列表，让挑节点照常进行——用略旧的节点列表远比开不出出口好。
func (m *Manager) ensureNodes() (int, error) {
	cfg := connConf()
	ttl := time.Duration(cfg.NodeTTL) * time.Second
	m.mu.RLock()
	have, age, mode := len(m.nodes), time.Since(m.fetched), m.nodesMode
	m.mu.RUnlock()

	// 换了打分策略也当作过期：不然改完设置立刻开出口，挑到的还是按旧策略
	// 排的队，要等一次自然刷新才见效。mode 为空是旧状态文件恢复来的列表，
	// 没记策略，照重排处理。
	if have > 0 && age < ttl && mode == cfg.ScoreMode {
		return have, nil
	}
	if have > 0 && nodeFetchCooldown > 0 && time.Since(m.fetchFailAt()) < nodeFetchCooldown {
		return have, nil
	}
	return m.refreshNodes()
}

func (m *Manager) fetchFailAt() time.Time {
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()
	return m.fetchFailA
}

// refreshNodes 拉一次节点列表，并发的调用共用同一次结果。
func (m *Manager) refreshNodes() (int, error) {
	m.fetchMu.Lock()
	if m.fetchWait != nil {
		ch := m.fetchWait
		m.fetchMu.Unlock()
		res := <-ch
		return res.count, res.err
	}
	ch := make(chan fetchResult, 1)
	m.fetchWait = ch
	m.fetchMu.Unlock()

	res := fetchResult{}
	nodes, err := fetchNodes(nodeFetchTimeout)
	if err != nil {
		res.err = err
	} else {
		m.mu.Lock()
		m.nodes = nodes
		m.fetched = time.Now()
		m.nodesMode = connConf().ScoreMode
		m.mu.Unlock()
		res.count = len(nodes)
	}

	m.fetchMu.Lock()
	if err != nil {
		m.fetchFailA = time.Now()
	} else {
		m.fetchFailA = time.Time{}
	}
	m.fetchWait = nil
	m.fetchMu.Unlock()

	ch <- res
	return res.count, res.err
}

func (m *Manager) Nodes() ([]Node, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Node, len(m.nodes))
	copy(out, m.nodes)
	return out, m.fetched
}

func (m *Manager) Tunnels() []*Tunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// SlotHeadroom 返回还能再开几条出口。自动补位靠它守水位。
func (m *Manager) SlotHeadroom() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	free := m.maxSlots - len(m.tunnels)
	if free < 0 {
		return 0
	}
	return free
}

// nodeRegionLocked 从当前节点清单里查国家码。调用方必须已持 m.mu。
func (m *Manager) nodeRegionLocked(host string) string {
	for _, n := range m.nodes {
		if n.HostName == host {
			return n.CountryCode
		}
	}
	return ""
}

// freeSlot 找一个未占用的槽位。槽位同时决定端口与网段。
func (m *Manager) freeSlot() (int, error) {
	for i := 1; i <= m.maxSlots; i++ {
		if _, used := m.tunnels[i]; !used {
			return i, nil
		}
	}
	return 0, fmt.Errorf("槽位已满（上限 %d）", m.maxSlots)
}

// Start 为指定节点开一条隧道，返回分配到的本地端口。
func (m *Manager) Start(node Node) (*Tunnel, error) {
	m.mu.Lock()
	slot, err := m.freeSlot()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// 端口随机取，避免固定规律撞上机器上的其他服务
	taken := map[int]bool{}
	for _, other := range m.tunnels {
		taken[other.port()] = true
	}
	// 出口组的监听绑在 127.0.0.1，和这里要绑的 0.0.0.0 互斥，必须一起避开
	for p := range poolPorts() {
		taken[p] = true
	}
	port, err := freeRandomPort(taken)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	cred, err := newSocksCred()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	t := &Tunnel{
		Slot:   slot,
		Port:   port,
		Node:   node,
		Status: "starting",
		Since:  time.Now(),
		Cred:   cred,
	}
	m.tunnels[slot] = t
	m.mu.Unlock()

	go m.bringUp(t, true)
	return t, nil
}

// bringUp 把一条隧道拉起来。
//
// notify 决定成功后是否立刻重建后端配置。换节点重连时要传 false：
// 那条路径随后会调 rebind/resync 把入站改绑到新节点，在那之前重建配置
// 会因为入站还指着旧节点名而把路由规则丢掉。
func (m *Manager) bringUp(t *Tunnel, notify bool) {
	m.bringUpPersist(t, notify, false)
}

// bringUpPersist 把一条隧道拉起来。
//
// persist=false（手动新建）：走一轮候选，全失败就标 failed，让用户能立刻看到并重试。
// persist=true（自动重连 / 重启恢复）：一轮全失败不放弃，退避后刷新节点列表再来一轮，
// 一直循环到连上或这条隧道被用户停掉。VPN Gate 死节点多，"当前都不可用"往往只是
// 这一批候选恰好都挂了，过一会儿就有新节点，不该让出口永久躺死。
func (m *Manager) bringUpPersist(t *Tunnel, notify bool, persist bool) {
	cfg := connConf()
	backoff := time.Duration(cfg.BackoffMin) * time.Second
	ceil := time.Duration(cfg.BackoffMax) * time.Second
	// 自动重连的目标是"恢复这条出口"，第一轮先试当前节点；
	// 手动换节点时 t.Node 已经是用户挑的那个，必须打头。
	prefer := true
	lastErr := ""
	for {
		ok, err := m.tryCandidates(t, notify, prefer)
		if ok {
			return
		}
		lastErr = err
		// 隧道已被用户停掉或从管理器移除，别再重试
		if !persist || !m.tunnelActive(t) {
			if persist {
				return
			}
			t.setStatus("failed", oneLine(lastErr, 500))
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			return
		}

		// 一整轮都没连上，说明当前这个节点（以及这批候选）不行了：
		// 下一轮别再拿它打头，否则会反复在同一批死节点上空转。
		prefer = false
		t.setStatus("starting", fmt.Sprintf("暂无可用节点，%.0f 秒后重试（%s）",
			backoff.Seconds(), oneLine(lastErr, 320)))
		log.Printf("隧道 %d 一轮候选均失败，%.0f 秒后刷新节点重试", t.Slot, backoff.Seconds())
		time.Sleep(backoff)
		if !m.tunnelActive(t) {
			return
		}
		if _, err := m.RefreshNodes(); err != nil {
			log.Printf("重试前刷新节点列表失败: %v", err)
		}
		if backoff < ceil {
			backoff *= 2
			if backoff > ceil {
				backoff = ceil
			}
		}
	}
}

// tryCandidates 走一轮候选节点，成功返回 true；失败时第二个返回值是最后一个候选
// 的真实报错。失败本身不改 Status（留给调用方决定），但**原因必须带出去**——
// 界面只会显示"failed / 暂无可用节点"，而那两种情况下下一步该做的事完全不同：
// 节点死了就等下一轮，认证被拒是节点名单的问题，WARP 没握手则是换多少节点都没用。
// preferCurrent 为真时把隧道当前绑定的节点排在最前（见 candidatesFor）。
func (m *Manager) tryCandidates(t *Tunnel, notify bool, preferCurrent bool) (bool, string) {
	// VPN Gate 是志愿者节点，列表里有相当比例已下线或满员（AUTH_FAILED），
	// 连不上就顺着候选列表换下一个，不必让用户手动试。
	candidates := m.candidatesFor(t, preferCurrent)
	// 本轮起点的地区，用来说明某一次换候选是不是跨区换的。
	home := t.node().CountryCode
	lastErr := "没有可用候选节点"
	for i, node := range candidates {
		if !m.tunnelActive(t) {
			return false, lastErr
		}
		// 其他隧道可能在重试期间占用了这个节点，跳过以免多个端口撞同一出口 IP
		if i > 0 && m.nodeInUse(node.HostName, t.Slot) {
			continue
		}
		t.setNode(node)
		if i > 0 {
			t.setStatus("starting", swapNote(i, home, node.CountryCode))
		} else {
			t.setStatus("starting", "")
		}

		ip, err := m.tryNode(t)
		if err == nil {
			// 状态、出口 IP、计时器一次写完：分开赋值会让界面轮询读到
			// status=up 却 exit_ip 还空着的中间态，进而把这条出口显示成异常。
			t.setUp(ip)
			if serr := m.saveState(); serr != nil {
				log.Printf("保存状态失败: %v", serr)
			}
			if notify {
				m.notifyPanel()
			}
			return true, ""
		}
		lastErr = err.Error()
		// 每个候选失败都写一行日志：journalctl 里能看到这一轮撞过哪些节点，
		// 而界面上只有最后那一条。
		log.Printf("fanout: 出口 %d 用 %s 没连上: %v", t.Slot, node.HostName, err)
		t.teardownNetns()
	}
	return false, lastErr
}

// tunnelActive 判断这条隧道是否还归管理器所有且未被用户停掉。
// 用指针比对：Stop 会从 map 里删除并把 Status 置 stopped，
// 重连循环据此退出，避免对着一条已经不存在的隧道空转。
func (m *Manager) tunnelActive(t *Tunnel) bool {
	if t.status() == "stopped" {
		return false
	}
	m.mu.RLock()
	cur, ok := m.tunnels[t.Slot]
	m.mu.RUnlock()
	return ok && cur == t
}

// tryNode 尝试用当前节点把隧道拉起来，成功时返回出口 IP。
func (m *Manager) tryNode(t *Tunnel) (string, error) {
	if err := t.setupNetns(); err != nil {
		return "", err
	}
	// WARP 中转必须在 openvpn 之前架好：隧道一起来就会抢默认路由，
	// 那时再改路由已经晚了。开关没开时这里是空操作，链路和以前完全一样。
	acc, err := warpForTunnel(t)
	if err != nil {
		return "", err
	}
	if err := t.startOpenVPN(m.workDir, acc); err != nil {
		return "", err
	}
	if acc != nil {
		// tun0 通了才谈得上把用户流量接进节点。这一步失败要当"这个节点没连上"
		// 处理：漏掉它的话流量停在 Cloudflare，拿到的出口 IP 是 WARP 的，
		// 界面显示已连通、实际根本没走节点——比连不上更难查。
		if err := t.warpSplit(acc); err != nil {
			return "", err
		}
	}
	if t.getListener() == nil {
		if err := t.serve(); err != nil {
			return "", err
		}
	}
	ip, err := t.probeExitIP()
	if err != nil {
		if acc != nil {
			// 隧道起来了却查不到出口 IP：可能是节点那边没放行，也可能是外层包
			// 太大传不动（openvpn 的握手小包过了，探测的大包过不去）。
			// 这两种在界面上长得一样，但 WARP 那一跳的实况能把它们分开。
			return "", fmt.Errorf("%v；%s", err, warpHopNote(t.nsName(), acc))
		}
		// 没开中转时先看默认路由在不在：netns 建架故意不装默认路由（见 netnsDirectCmds），
		// 隧道的默认路由只能由节点推送。缺了就说明这个节点连上了却不接流量，
		// 报"探测失败"会把人往网络问题上引，而实际该做的是换一个节点。
		if !netnsHasDefault(t.nsName()) {
			return "", fmt.Errorf("%v；节点没把默认路由推进隧道（连上了也不接流量），详见 %s",
				err, t.nsName())
		}
		return "", err
	}
	if acc != nil && warpExitIsTransitOnly(ip) {
		// 分流没装上（或装了又被 openvpn 重连冲掉）时就会走到这里：探测确实出得去，
		// 但出去的是 Cloudflare 而不是节点。把它当"这个节点没连上"处理，
		// 让候选列表往下走，而不是记下这个 IP 从此一直显示成已连通。
		return "", fmt.Errorf("出口只走到了 Cloudflare 中转（%s），没走节点 %s", ip, t.host())
	}
	return ip, nil
}

// candidatesFor 以这条隧道当前的节点打头，后面跟上同地区的其他节点作为备选。
//
// preferCurrent 时打头的一定是 t.Node：自动重连的第一轮、以及用户手动换节点之后，
// 目标都是"就用这一个"。反之在一整轮都失败之后要把当前节点排到最后——
// 它刚把这一轮带崩，再让它打头就是反复撞同一块石头。
// 备选会避开用户手动换掉过的节点——那些是他明确不想要的 IP，
// 让重连悄悄换回去等于撤销了他的操作。
// 排序规则本身在 orderCandidates，包括"同区掏空之后跨区补齐"那一条。
func (m *Manager) candidatesFor(t *Tunnel, preferCurrent bool) []Node {
	maxTries := connConf().MaxTries
	first := t.node()
	avoid := t.swapAvoid()

	m.mu.RLock()
	defer m.mu.RUnlock()

	used := map[string]bool{first.HostName: true}
	for _, other := range m.tunnels {
		used[other.host()] = true
	}

	// 地区决定了备选范围，缺失时先从当前列表补一次，
	// 否则会退化成"任意地区都算同区"。
	region := first.CountryCode
	if region == "" {
		region = m.nodeRegionLocked(first.HostName)
	}

	return orderCandidates(first, region, preferCurrent, maxTries, m.nodePoolLocked(), used, avoid)
}

// Stop 停掉一条隧道并释放槽位。
func (m *Manager) Stop(slot int) error {
	invalidateInbounds()
	m.mu.Lock()
	t, ok := m.tunnels[slot]
	if ok {
		delete(m.tunnels, slot)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}
	t.stop()
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.notifyPanel()
	return nil
}

// Swap 把一条隧道换到同地区的另一个节点上，端口与已分发的客户端配置保持不变。
//
// 与健康检查的自动重连不同：那边优先重连原节点（目标是恢复），
// 这里用户是嫌当前出口 IP 不好用，必须真的换一个。
//
// 换过的节点会记进历史一并避开。只排除"当前这个"是不够的：
// A 换成 B 之后 A 就空出来了，而候选是按速度排的，A 往往又排在最前面，
// 于是再点一次就换回了 A。历史让每次点击都真的换一个没用过的。
func (m *Manager) Swap(slot int) error {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}
	if t.status() == "starting" {
		return fmt.Errorf("这个出口正在连接中，稍等一下")
	}

	node, err := m.pickSwapTarget(t)
	if err != nil {
		return err
	}
	oldHost := t.host()
	t.setNode(node)
	m.reconnect(t, oldHost)
	return nil
}

// pickSwapTarget 给"换节点"挑下一个目标，并把该跳过的节点记进历史。
//
// 记两个：换下来的那个，以及刚挑中的这个。挑中的也记是因为真机上踩到过——
// 它连不上时会被候选列表换成别人，但它自己没进历史，
// 于是下次点换节点又从它开始试一遍，白等一轮握手超时。
func (m *Manager) pickSwapTarget(t *Tunnel) (Node, error) {
	avoid := t.swapAvoid()
	picks, err := m.pickNodes(t.node().CountryCode, 1, avoid)
	if err != nil && len(avoid) > 1 {
		// 这个地区的节点都换过一轮了。清掉历史重新开始，
		// 总比告诉用户"没得换了"好——转一圈之后原来那些节点未必还是当初的状态。
		t.forgetSwaps()
		picks, err = m.pickNodes(t.node().CountryCode, 1, t.swapAvoid())
	}
	if err != nil {
		return Node{}, err
	}
	t.rememberSwap(t.host())
	t.rememberSwap(picks[0].HostName)
	return picks[0], nil
}

// StopAll 停掉所有隧道并清空状态文件。
func (m *Manager) StopAll() {
	for _, t := range m.Tunnels() {
		_ = m.Stop(t.Slot)
	}
}

// SetCred 改一条出口的 SOCKS5 凭据。cred 两个字段都为空表示随机重置。
//
// 改完要通知后端：本机 Xray 的 socks 出站里带着这套凭据，
// 不同步的话面板侧的节点会立刻连不上自己的出口。
func (m *Manager) SetCred(slot int, cred SocksCred) (SocksCred, error) {
	m.mu.RLock()
	t, ok := m.tunnels[slot]
	m.mu.RUnlock()
	if !ok {
		return SocksCred{}, fmt.Errorf("槽位 %d 没有运行中的隧道", slot)
	}

	if cred.User == "" && cred.Pass == "" {
		gen, err := newSocksCred()
		if err != nil {
			return SocksCred{}, err
		}
		cred = gen
	}
	if err := validateCred(cred); err != nil {
		return SocksCred{}, err
	}

	t.setCredential(cred)
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}
	m.syncCred(t)
	return cred, nil
}

// ReconcileOutbounds 在启动恢复隧道后跑一次，把后端出站对齐到当前隧道（含 SOCKS5 凭据）。
//
// 只为 3x-ui 模式而生：它的 OnTunnelsChanged 是空操作，重启不会重写面板出站，
// 而从旧版本升上来时面板里持久化的 socks 出站没有认证字段，端口一旦要认证就连不上。
// 自建模式恢复时每条隧道 up 都会重建配置，本就自洽，这里跳过免得多重启一次 Xray。
func (m *Manager) ReconcileOutbounds() {
	p, err := openPanel()
	if err != nil {
		log.Printf("启动对账面板出站跳过（后端不可用）: %v", err)
		return
	}
	if p.Kind() != "3x-ui" {
		return
	}

	// 等隧道尽量都起完再重写一次，避免只覆盖到先 up 的那几条
	deadline := time.Now().Add(90 * time.Second)
	for {
		tunnels := m.Tunnels()
		if len(tunnels) == 0 {
			return
		}
		var up *Tunnel
		settled := true
		for _, t := range tunnels {
			if t.status() == "up" && up == nil {
				up = t
			}
			if t.status() == "starting" {
				settled = false
			}
		}
		if (settled || time.Now().After(deadline)) && up != nil {
			if err := m.resync(up); err != nil {
				log.Printf("启动对账面板出站失败: %v", err)
			}
			return
		}
		if settled || time.Now().After(deadline) {
			return // 全 failed，没有可写的出站
		}
		time.Sleep(2 * time.Second)
	}
}

// SyncPoolEgress 让后端把出口组的 socks 出站补写进配置。
//
// 增删改组之后都要调一次：组的绑定是一条指向本机端口的 outbound，
// 配置里没有那条出站时 Xray 会直接拒绝整份规则。
// 没有隧道时照样要写——先建组、后拉起出口是常见顺序。
func (m *Manager) SyncPoolEgress() error {
	p, err := openPanel()
	if err != nil {
		return err
	}
	tunnels := m.Tunnels()
	// ResyncOutbound 的语义是"重写 fanout 那批出站"，具体那条隧道并不参与计算，
	// 三个后端都是整段重建，所以传 nil 是安全的。
	return p.ResyncOutbound(nil, tunnels)
}

// syncCred 把新凭据写进后端的 socks 出站。
//
// 两种后端的做法不同：自建模式整份重建配置，3x-ui 模式只改出站那一段。
// 都走 ResyncOutbound，接口语义正好是"重写这条隧道对应的出站"。
func (m *Manager) syncCred(t *Tunnel) {
	if err := m.resync(t); err != nil {
		log.Printf("同步 SOCKS5 凭据到节点链接后端失败: %v", err)
	}
}

// Shutdown 停掉运行态但保留状态文件，让下次启动能恢复同样的隧道。
func (m *Manager) Shutdown() {
	for _, t := range m.Tunnels() {
		t.stop()
	}
}

// prepareHost 打开转发开关。netns 出网依赖它。
func prepareHost() error {
	if err := cmdRun(exec.Command("sysctl", "-qw", "net.ipv4.ip_forward=1")); err != nil {
		return fmt.Errorf("开启 ip_forward 失败: %w", err)
	}
	return nil
}

// nodeInUse 判断某节点是否已被别的隧道占用。
func (m *Manager) nodeInUse(host string, exceptSlot int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for slot, t := range m.tunnels {
		if slot != exceptSlot && t.host() == host {
			return true
		}
	}
	return false
}

// rebind 在隧道换节点后，把原先指向旧节点的入站改绑到新节点。
//
// 后端不可用要把错误报给调用方：reconnect 靠"这一步成功"才清掉 prevHost 标记，
// 悄悄吞掉错误会让入站永久指着旧节点，而界面上看着一切正常。
func (m *Manager) rebind(oldHost string, t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return fmt.Errorf("节点链接后端不可用: %w", err)
	}
	// 出口组只改自己账本里的成员名：组的身份是 pool-<id>，
	// 换了节点不需要动 Xray 一个字，这也正是这层设计想要的效果。
	// 绑到组的入站由 x.Rebind 按旧节点 tag 匹配，天然不会被卷进来。
	poolsSvc.renameHost(oldHost, t.host())
	return x.Rebind(oldHost, t, m.Tunnels())
}

// resync 在节点没换但重连过之后，把后端的出站配置刷新一遍。
func (m *Manager) resync(t *Tunnel) error {
	x, err := openPanel()
	if err != nil {
		return fmt.Errorf("节点链接后端不可用: %w", err)
	}
	return x.ResyncOutbound(t, m.Tunnels())
}

// notifyPanel 告诉后端隧道集合变了。
//
// 自建模式下出站是由隧道列表现算出来的，不通知的话新开的出口在 Xray 里
// 没有对应的 socks 出站，绑定会指向一个不存在的 tag。接管 3x-ui 时是空操作。
// 后端不可用不该让开关出口失败，所以只记日志。
func (m *Manager) notifyPanel() {
	p, err := openPanel()
	if err != nil {
		log.Printf("同步节点链接后端失败: %v", err)
		return
	}
	if err := p.OnTunnelsChanged(m.Tunnels()); err != nil {
		log.Printf("同步节点链接后端失败: %v", err)
	}
}
