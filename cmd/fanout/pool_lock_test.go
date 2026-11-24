package main

import (
	"net"
	"testing"
	"time"
)

// newTestPoolService 造一个「组已经在监听」的运行态，但不走 newPoolService：
// 那会读盘、起后台测速，把这个锁序用例拖进无关的依赖里。
func newTestPoolService(t *testing.T, mgr *Manager) (*PoolService, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p := &Pool{
		ID: "p1", Name: "锁序组", Mode: poolModeSingle,
		Hosts: []string{"vpn-gate-example"}, Port: listenPort(ln),
	}
	s := &PoolService{
		dir:     t.TempDir(),
		mgr:     mgr,
		store:   &poolStore{Next: 2, Pools: []*Pool{p}},
		run:     map[string]*poolRun{"p1": {ln: ln, policy: newPoolPolicy(p.Mode, p.RRStep), stat: map[string]*memberStat{}}},
		probing: map[string]bool{},
		stop:    make(chan struct{}),
		// 补位的三本账要和 newPoolService 一起建：漏了的话 fillPool 一写就 panic，
		// 测试红在无关的地方，看不出补位本身对不对。
		fillDead: map[string]map[string]time.Time{},
		fillBad:  map[string]time.Time{},
		fillNote: map[string]string{},
	}
	notePoolPort(p.Port)
	t.Cleanup(clearPoolPorts)
	return s, ln
}

// TestPoolPortsDoesNotTakeManagerLock 钉住一条锁序不变式。
//
// 起因是发布出去过的一个死锁：Manager.Start 持有 m.mu 写锁时调 poolPorts()
// 避让组端口，而组端口当时是现问 PoolService 的——
// poolPorts → egresses → resolveMembers → mgr.Tunnels 会去要同一把 m.mu 的读锁。
// Go 的 RWMutex 不可重入，同一个 goroutine 拿着写锁再 RLock 就是永久停摆，
// 而且是扣着 m.mu 停摆：新建出口、健康巡检、存状态、界面轮询从此全部卡死。
// 只要有一个组在监听就会踩到，所以这里必须先让组处于监听态。
func TestPoolPortsDoesNotTakeManagerLock(t *testing.T) {
	prev := poolsSvc
	defer func() { poolsSvc = prev }()

	mgr := NewManager(4, t.TempDir())
	svc, ln := newTestPoolService(t, mgr)
	poolsSvc = svc

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	ports := make(chan map[int]bool, 1)
	go func() { ports <- poolPorts() }()

	select {
	case got := <-ports:
		if !got[listenPort(ln)] {
			t.Fatalf("组端口 %d 没进避让表，新建隧道会撞上它：%v", listenPort(ln), got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poolPorts 在等 m.mu —— 锁序又成环了，Manager.Start 会当场永久停摆")
	}
}

// TestManagerTunnelsDoesNotTakePoolLock 钉住反方向的不变式。
//
// takenPortsLocked 会持着 s.mu 调 mgr.Tunnels()，这条边是允许存在的；
// 但反过来 Tunnels 绝不能去碰 PoolService 的锁，否则两条边凑成一个环，
// 上面那个用例就不再是「同一把锁重入」而是两个 goroutine 互相等死。
func TestManagerTunnelsDoesNotTakePoolLock(t *testing.T) {
	prev := poolsSvc
	defer func() { poolsSvc = prev }()

	mgr := NewManager(4, t.TempDir())
	svc, _ := newTestPoolService(t, mgr)
	poolsSvc = svc

	// 顺带验证组端口确实算在避让表里：ensureListener 换端口之后必须成立。
	svc.mu.Lock()
	taken := svc.takenPortsLocked()
	if !taken[svc.store.Pools[0].Port] {
		svc.mu.Unlock()
		t.Fatal("组端口没算进 takenPortsLocked，新建组会抢了别人的端口")
	}

	// 还持着 s.mu 的时候问 Manager 要隧道列表：能回来就说明 Manager 不依赖 PoolService 的锁。
	done := make(chan int, 1)
	go func() { done <- len(mgr.Tunnels()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mgr.Tunnels 在等 s.mu —— 和 takenPortsLocked 凑成锁环了")
	}
	svc.mu.Unlock()
}
