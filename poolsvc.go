package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// PoolService 管着所有出口组的运行态：每组一条本机 SOCKS5 监听，
// 收到 CONNECT 后按组的策略在成员里挑一条（或几条）隧道去拨号。
//
// 组的成员身份存的是节点主机名，每次拨号现查隧道表，所以出口重连、
// 换节点、掉线都不需要重建监听——策略看到的一直是此刻的可用情况。

// poolsSvc 是全局实例。
//
// 之所以用包级变量而不是把 *PoolService 一路塞进 Panel 的每个方法：
// 三种后端解析出站只需要"这个绑定串对应哪个本机端口"，
// 为了一个端口去改一遍 Panel 接口和全部调用点不值得。
// main 启动时装配；没装配时相关路径一律按"没有出口组"处理。
var poolsSvc *PoolService

// errPoolNoMember 表示组内此刻一条可用出口都没有。
// 宁可让这条连接立刻失败并报错，也绝不能退回母机直连：
// 那等于把用户的真实出口 IP 暴露出去，而这正是这个工具要避免的事。
var errPoolNoMember = errors.New("出口组内没有可用出口")

type memberStat struct {
	ewma     time.Duration
	fails    int
	badUntil time.Time
	probed   time.Time
}

// poolRun 是一个组的运行时状态。
type poolRun struct {
	ln     net.Listener
	policy *poolPolicy

	mu   sync.Mutex
	stat map[string]*memberStat
}

type PoolService struct {
	mu    sync.Mutex
	dir   string
	mgr   *Manager
	store *poolStore
	run   map[string]*poolRun
	// probing 防止多个组同时探同一个成员，或上一轮还没完又开一轮。
	probing map[string]bool
	stop    chan struct{}
	stopOn  sync.Once
}

// newPoolService 读盘、给每个组起监听，并开始后台测速。
func newPoolService(dir string, mgr *Manager) (*PoolService, error) {
	st, err := loadPoolStore(dir)
	if err != nil {
		return nil, err
	}
	s := &PoolService{
		dir:     dir,
		mgr:     mgr,
		store:   st,
		run:     map[string]*poolRun{},
		probing: map[string]bool{},
		stop:    make(chan struct{}),
	}
	for _, p := range st.Pools {
		if err := s.ensureListener(p); err != nil {
			log.Printf("出口组 %s 监听没起来: %v（绑定关系保留，下次启动重试）", p.Name, err)
		}
	}
	go s.probeLoop()
	return s, nil
}

func (s *PoolService) Close() {
	s.stopOn.Do(func() { close(s.stop) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, pr := range s.run {
		if pr.ln != nil {
			_ = pr.ln.Close()
		}
		delete(s.run, id)
	}
}

// ensureListener 让监听与组对齐：端口尽量不变（已分发的配置才会继续有效），
// 起不来才换端口并写回存储。
func (s *PoolService) ensureListener(p *Pool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pr, ok := s.run[p.ID]; ok && pr.ln != nil {
		return nil
	}

	port := p.Port
	ln, err := listenPool(port)
	if err != nil {
		alt, altErr := freeRandomPort(s.takenPortsLocked())
		if altErr != nil {
			return fmt.Errorf("监听 %d 失败且无备用端口: %v", port, err)
		}
		if ln, err = listenPool(alt); err != nil {
			return fmt.Errorf("监听 %d 也失败: %v", alt, err)
		}
		p.Port = alt
		if saveErr := s.store.save(s.dir); saveErr != nil {
			log.Printf("出口组 %s 换端口后存盘失败: %v", p.Name, saveErr)
		}
	}

	pr := &poolRun{
		ln:     ln,
		policy: newPoolPolicy(p.Mode, p.RRStep),
		stat:   map[string]*memberStat{},
	}
	if s.run == nil {
		s.run = map[string]*poolRun{}
	}
	s.run[p.ID] = pr

	go func(id string) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 凭据每次现取：改口令后不必重建监听
			cred := s.credOf(id)
			go serveSocks(conn, cred, s.dialerFor(id))
		}
	}(p.ID)
	return nil
}

// listenPool 只绑 127.0.0.1：出口组的监听是给本机 Xray 用的内部件，
// 不像单出口端口那样要对外发给客户端，没必要敞开。
func listenPool(port int) (net.Listener, error) {
	var err error
	for i := 0; i < 4; i++ {
		var ln net.Listener
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}

// takenPortsLocked 汇总已占用的端口，避免新组抢了隧道或别的组的端口。
// 调用方必须持有 s.mu。
func (s *PoolService) takenPortsLocked() map[int]bool {
	taken := externalUsedPorts()
	for _, pr := range s.run {
		if pr.ln != nil {
			if tcp, ok := pr.ln.Addr().(*net.TCPAddr); ok {
				taken[tcp.Port] = true
			}
		}
	}
	for _, p := range s.store.Pools {
		if p.Port > 0 {
			taken[p.Port] = true
		}
	}
	for _, t := range s.mgr.Tunnels() {
		taken[t.port()] = true
	}
	return taken
}

// dropListener 停掉某个组的监听（删组时用）。
func (s *PoolService) dropListener(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pr, ok := s.run[id]; ok {
		if pr.ln != nil {
			_ = pr.ln.Close()
		}
		delete(s.run, id)
	}
}

func (s *PoolService) credOf(id string) *SocksCred {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.store.get(id); p != nil {
		c := p.Cred
		return &c
	}
	return nil
}

// setPool 增改一个组：落盘并把监听与策略对齐。
func (s *PoolService) setPool(p *Pool) error {
	p.normalize()
	if err := p.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	pools := s.store.Pools
	replaced := false
	for i, ex := range pools {
		if ex.ID == p.ID {
			// 端口与凭据不因编辑而变：变了等于把已分发的链接作废
			p.Port, p.Cred = ex.Port, ex.Cred
			pools[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		p.ID = fmt.Sprintf("p%d", s.store.Next)
		s.store.Next++
		if p.Cred.User == "" {
			cred, err := newSocksCred()
			if err != nil {
				s.mu.Unlock()
				return err
			}
			p.Cred = cred
		}
		s.store.Pools = append(s.store.Pools, p)
	}
	err := s.store.save(s.dir)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	// 策略对象要跟着模式/步长换，但粘性账本得留着，
	// 否则每改一次设置，同一批目标就会集体换出口。
	s.mu.Lock()
	if pr, ok := s.run[p.ID]; ok {
		policy := newPoolPolicy(p.Mode, p.RRStep)
		policy.affi, policy.affiOrder = pr.policy.affi, pr.policy.affiOrder
		pr.policy = policy
	}
	s.mu.Unlock()

	return s.ensureListener(p)
}

// deletePool 删组。调用方负责先把指着它的入站解绑，否则会留下
// 指向不存在 outbund 的路由规则。
func (s *PoolService) deletePool(id string) error {
	s.mu.Lock()
	idx := -1
	for i, p := range s.store.Pools {
		if p.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return fmt.Errorf("出口组不存在")
	}
	s.store.Pools = append(s.store.Pools[:idx], s.store.Pools[idx+1:]...)
	err := s.store.save(s.dir)
	s.mu.Unlock()
	s.dropListener(id)
	return err
}

// renameHost 把组内记录的成员主机名从 old 改成 new。
//
// 一条出口换了节点，主机名就变了；组的身份（pool-<id>）不变，所以 Xray
// 那边一个字节都不用重写，只改我们自己的账本。漏了这一步的话，
// 组员会一直指向一个已经不存在的节点名，组就慢慢空掉了。
func (s *PoolService) renameHost(oldHost, newHost string) bool {
	if s == nil {
		return false
	}
	if oldHost == "" || newHost == "" || oldHost == newHost {
		return false
	}
	s.mu.Lock()
	changed := false
	for _, p := range s.store.Pools {
		for i, h := range p.Hosts {
			if h == oldHost {
				p.Hosts[i] = newHost
				changed = true
			}
		}
	}
	var err error
	if changed {
		err = s.store.save(s.dir)
	}
	s.mu.Unlock()
	if changed && err != nil {
		log.Printf("出口组成员改名存盘失败: %v", err)
	}
	return changed
}

// pools 返回存储里所有组的深拷贝。
//
// 给出去的是拷贝而不是 *Pool：界面每几秒读一次，而 setCred/ensureListener
// 会原地改同一个对象，直接共享指针在 -race 下就是数据竞争。
func (s *PoolService) pools() []*Pool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.store.sorted()
	out := make([]*Pool, 0, len(src))
	for _, p := range src {
		out = append(out, p.clone())
	}
	return out
}

func (s *PoolService) pool(id string) *Pool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.get(id).clone()
}

// setCred 换掉组的 SOCKS5 凭据。新连接立刻按新凭据校验，已建立的不受影响。
func (s *PoolService) setCred(id string, c SocksCred) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.store.get(id)
	if p == nil {
		return fmt.Errorf("出口组不存在")
	}
	p.Cred = c
	return s.store.save(s.dir)
}

// resolveMembers 把组里记的主机名换成此刻的隧道状态。
func (s *PoolService) resolveMembers(id string) []poolMember {
	p := s.pool(id)
	if p == nil {
		return nil
	}
	byHost := map[string]*Tunnel{}
	for _, t := range s.mgr.Tunnels() {
		byHost[t.host()] = t
	}
	pr := s.runOf(id)
	out := make([]poolMember, 0, len(p.Hosts))
	for _, h := range p.Hosts {
		m := poolMember{Host: h}
		if t := byHost[h]; t != nil {
			st := t.state()
			m.Slot = t.Slot
			m.NS = t.nsName()
			m.ExitIP = st.ExitIP
			m.Up = st.Status == "up"
		}
		if pr != nil {
			pr.mu.Lock()
			if ms := pr.stat[h]; ms != nil {
				m.Latency = ms.ewma
				m.Cooling = ms.fails > 0 && time.Now().Before(ms.badUntil)
			}
			pr.mu.Unlock()
		}
		out = append(out, m)
	}
	return out
}

// poolEgress 是三种后端解析一个组出口需要的信息。
type poolEgress struct {
	Identity string // pool-<id>，入站绑定串用的就是它
	Tag      string // 写进 Xray 的 outboundTag
	Port     int    // 本机 SOCKS5 端口
	Cred     SocksCred
	Up       int // 此刻可用的成员数，界面用来标红
}

// egressPools 列出所有可作为出口的组。
//
// 没装配 PoolService（测试、或还没跑到 main）时返回 nil，调用方按"没有组"处理。
// 只列监听真的起来的组：端口没起来就写进配置，会得到一条连不上的出站，
// 挂在它上面的入站全部超时，反而不如让它掉回直连并报错来得可诊断。
func egressPools() []poolEgress {
	if poolsSvc == nil {
		return nil
	}
	return poolsSvc.egresses()
}

// egressPool 按绑定串取一个组出口，不是组或组没在跑时返回 false。
func egressPool(identity string) (poolEgress, bool) {
	if poolsSvc == nil {
		return poolEgress{}, false
	}
	return poolsSvc.egress(identity)
}

func (s *PoolService) egresses() []poolEgress {
	type base struct {
		id        string
		identity  string
		tag       string
		port      int
		cred      SocksCred
		listening bool
	}
	var items []base

	// 字段在锁内取快照：ensureListener 会在锁内改 p.Port，
	// 拿着 *Pool 出锁再读就是数据竞争。
	s.mu.Lock()
	for _, p := range s.store.sorted() {
		pr := s.run[p.ID]
		items = append(items, base{
			id: p.ID, identity: p.identity(), tag: egressTag(p.identity()),
			port: p.Port, cred: p.Cred, listening: pr != nil && pr.ln != nil,
		})
	}
	s.mu.Unlock()

	out := make([]poolEgress, 0, len(items))
	for _, it := range items {
		if !it.listening {
			continue
		}
		up := 0
		for _, m := range s.resolveMembers(it.id) {
			if m.Up {
				up++
			}
		}
		out = append(out, poolEgress{
			Identity: it.identity, Tag: it.tag, Port: it.port, Cred: it.cred, Up: up,
		})
	}
	return out
}

func (s *PoolService) egress(identity string) (poolEgress, bool) {
	for _, e := range s.egresses() {
		if e.Identity == identity {
			return e, true
		}
	}
	return poolEgress{}, false
}

// poolPorts 返回所有组监听占用的端口，供分配新端口时避开。
//
// 组监听绑在 127.0.0.1，入站和隧道绑在 0.0.0.0，两者在 Linux 上会互斥：
// 已经被 127.0.0.1:8080 占住的端口，再 bind 0.0.0.0:8080 会 EADDRINUSE。
// 所以只要组起来了，这个端口就必须算作已占用，不然新建入站会撞上去。
func poolPorts() map[int]bool {
	out := map[int]bool{}
	if poolsSvc == nil {
		return out
	}
	for _, e := range poolsSvc.egresses() {
		out[e.Port] = true
	}
	return out
}

// mergePoolLive 把组出口并进调用方的 live 表（键与 sanitizeTag(主机名) 同形），
// 这样入站列表里的"绑定的出口是否在线"对组同样成立。
//
// 组的可用性看组内还有没有活着的成员：成员全掉时组监听虽然还在听，
// 但每条拨号都会立刻失败，界面上不该继续显示绿色。
func mergePoolLive(live map[string]bool) {
	for _, e := range egressPools() {
		live[sanitizeTag(e.Identity)] = e.Up > 0
	}
}

func (s *PoolService) runOf(id string) *poolRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run[id]
}

// dialerFor 返回给 serveSocks 用的 dial：一次调用就完成一次按策略的选路。
// 现有 SOCKS5 协议栈不用为出口组改任何东西，这是唯一的接入点。
func (s *PoolService) dialerFor(id string) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		// 不记日志：一次页面访问就是十几条 CONNECT，逐条打会淹掉 journal。
		conn, _, err := s.dial(id, network, addr)
		return conn, err
	}
}

func (s *PoolService) dial(id, network, addr string) (net.Conn, string, error) {
	pr := s.runOf(id)
	if pr == nil {
		return nil, "", fmt.Errorf("出口组未在运行")
	}
	members := s.resolveMembers(id)

	// UDP 中继的"拨号"是在成员里建一条转发通道，抢跑没有意义
	//（谁先通不代表谁更适合跑这路 UDP），所以并发模式在这里退回按序挑选。
	choice := pr.choose(targetKey(addr), members, network != "udp")
	if len(choice.Order) == 0 {
		return nil, "", errPoolNoMember
	}

	if choice.Concurrent {
		return s.raceDial(pr, choice.Order, network, addr)
	}

	var lastErr error
	for _, m := range choice.Order {
		if !m.Up || m.NS == "" {
			continue
		}
		t0 := time.Now()
		conn, err := dialerInNetnsTimeout(m.NS, poolTryTimeout)(network, addr)
		use := time.Since(t0)
		if err != nil {
			lastErr = err
			pr.noteFail(m.Host)
			// 换下一条之前，把这个成员名下的粘性目标放掉，
			// 否则下一条连接还会先撞它一次。
			pr.policy.forgetMember(m.Host)
			continue
		}
		pr.noteOK(m.Host, use)
		return conn, m.Host, nil
	}
	if lastErr == nil {
		lastErr = errPoolNoMember
	}
	return nil, "", lastErr
}

// raceDial 同时向所有成员发起，第一个成功的胜出，其余关掉。
//
// CONNECT 阶段并发是安全的：抢的只是"哪条隧道先连上目标"，
// 客户端的字节一个都还没发，不存在重复请求的副作用。
func (s *PoolService) raceDial(pr *poolRun, members []poolMember, network, addr string) (net.Conn, string, error) {
	type outcome struct {
		conn net.Conn
		host string
		use  time.Duration
		err  error
	}
	cands := make([]poolMember, 0, len(members))
	for _, m := range members {
		if m.Up && m.NS != "" {
			cands = append(cands, m)
		}
	}
	if len(cands) == 0 {
		return nil, "", errPoolNoMember
	}

	// 缓冲区等于候选数：选出赢家后即使暂时没人收，拨号 goroutine 也不会堵死
	ch := make(chan outcome, len(cands))
	for _, m := range cands {
		go func(m poolMember) {
			t0 := time.Now()
			c, err := dialerInNetnsTimeout(m.NS, poolTryTimeout)(network, addr)
			ch <- outcome{c, m.Host, time.Since(t0), err}
		}(m)
	}

	var firstErr error
	for i := 0; i < len(cands); i++ {
		r := <-ch
		if r.err == nil {
			pr.noteOK(r.host, r.use)
			// 其余结果仍会到达，交给后台收尾：迟到的连接必须关掉并记账，
			// 否则输掉的那些 socket 会一直挂在志愿者节点上白占会话。
			go func(remaining int) {
				for j := 0; j < remaining; j++ {
					rr := <-ch
					if rr.conn != nil {
						_ = rr.conn.Close()
					}
					if rr.err == nil {
						pr.noteOK(rr.host, rr.use)
					} else {
						pr.noteFail(rr.host)
					}
				}
			}(len(cands) - i - 1)
			return r.conn, r.host, nil
		}
		if r.conn != nil {
			_ = r.conn.Close()
		}
		pr.noteFail(r.host)
		if firstErr == nil {
			firstErr = r.err
		}
	}
	if firstErr == nil {
		firstErr = errPoolNoMember
	}
	return nil, "", firstErr
}

// choose 在锁内做一次选路决策。
func (pr *poolRun) choose(key string, members []poolMember, allowRace bool) poolChoice {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	c := pr.policy.pick(key, members)
	if !allowRace {
		c.Concurrent = false
	}
	return c
}

func (pr *poolRun) noteOK(host string, use time.Duration) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	ms := pr.stat[host]
	if ms == nil {
		ms = &memberStat{}
		pr.stat[host] = ms
	}
	if ms.ewma == 0 {
		ms.ewma = use
	} else {
		ms.ewma = time.Duration(poolEWMAAlpha*float64(use) + (1-poolEWMAAlpha)*float64(ms.ewma))
	}
	if ms.fails > 0 {
		ms.fails--
	}
	ms.badUntil = time.Time{}
	ms.probed = time.Now()
}

// noteFail 记一次失败：失败越多冷却越久，但冷却只影响优先顺序，不会把成员永久排除。
func (pr *poolRun) noteFail(host string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	ms := pr.stat[host]
	if ms == nil {
		ms = &memberStat{}
		pr.stat[host] = ms
	}
	ms.fails++
	cool := poolCoolBase << min(ms.fails, 5)
	if cool > poolCoolMax {
		cool = poolCoolMax
	}
	ms.badUntil = time.Now().Add(cool)
}

// probeLoop 定期补样本：balance 模式只看真实流量的话，闲一会儿所有成员
// 都没有近期数据，下一批连接就又变成盲选了。
func (s *PoolService) probeLoop() {
	tk := time.NewTicker(poolProbeInterval)
	defer tk.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tk.C:
			s.probeStale()
		}
	}
}

func (s *PoolService) probeStale() {
	for _, p := range s.pools() {
		if p.Mode != poolModeBalance && p.Mode != poolModeSingle {
			continue
		}
		pr := s.runOf(p.ID)
		if pr == nil {
			continue
		}
		for _, m := range s.resolveMembers(p.ID) {
			if !m.Up || m.NS == "" {
				continue
			}
			pr.mu.Lock()
			ms := pr.stat[m.Host]
			stale := ms == nil || ms.ewma == 0 || time.Since(ms.probed) > poolProbeStale
			pr.mu.Unlock()
			if !stale {
				continue
			}
			s.mu.Lock()
			// 按「组 + 成员」记在途：同一节点可以同时在几个组里，
			// 只按主机名去重的话另一个组永远补不到样本。
			key := p.ID + "|" + m.Host
			busy := s.probing[key]
			if !busy {
				s.probing[key] = true
			}
			s.mu.Unlock()
			if busy {
				continue
			}
			go func(host, ns string) {
				t0 := time.Now()
				c, err := dialerInNetnsTimeout(ns, poolTryTimeout)("tcp", probeTarget)
				if err == nil && c != nil {
					_ = c.Close()
					pr.noteOK(host, time.Since(t0))
				} else {
					pr.noteFail(host)
				}
				s.mu.Lock()
				delete(s.probing, key)
				s.mu.Unlock()
			}(m.Host, m.NS)
		}
	}
}
