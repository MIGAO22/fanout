package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ipOracles 是查出口 IP 的公共服务，按顺序试，第一个给出合法 IPv4 的作数。
//
// 只认 api.ipify.org 是有代价的：它自己抽风一次，这一轮所有正常隧道都会被
// 判成掉线，跟着触发一批毫无必要的重连。
// 都用 http 而不是 https：探测走的是隧道本身，TLS 握手会让每次探测多一个
// 来回，而健康检查是按秒算的。
//
// 顺序刻意保持固定：建立隧道时记录基准出口 IP 用的也是这条链，
// 两边从同一个源开始，才不会因为"不同目标走了不同出口路由"而误判。
var ipOracles = []string{
	"http://api.ipify.org",
	"http://ipv4.icanhazip.com",
	"http://4.ident.me",
	"http://ifconfig.me/ip",
	"http://ipecho.net/plain",
}

// probeExitIPIn 在指定 netns 里查出口 IPv4。
// budget 是所有候选源加起来的总预算，单个源的超时不会超过剩下的时间。
func probeExitIPIn(ns string, budget time.Duration) (string, error) {
	if budget < 2*time.Second {
		budget = 2 * time.Second
	}
	deadline := time.Now().Add(budget)
	reason := "没有可用探测源"
	for _, u := range ipOracles {
		left := time.Until(deadline)
		if left <= 0 {
			reason = "探测超时"
			break
		}
		secs := int(left.Seconds())
		if secs > 15 {
			secs = 15 // curl --max-time 给太大没意义，母机上还会攒一堆挂死的进程
		}
		out, err := cmdOutput(exec.Command("ip", "netns", "exec", ns,
			"curl", "-s", "--max-time", strconv.Itoa(secs), u))
		if err != nil {
			reason = err.Error()
			continue
		}
		ip := strings.TrimSpace(string(out))
		// 必须是 IPv4：出口 IP 后面要拿去和基准值逐字比对，
		// 收到 IPv6 或一段 HTML 错误页都算这个源没给答案
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			return ip, nil
		}
		reason = "返回不是 IPv4: " + strconv.Quote(ip)
	}
	return "", fmt.Errorf("查询出口 IP 失败: %s", reason)
}

// WatchHealth 周期检查每条隧道是否还能出网，掉线的自动换节点重连。
// VPN Gate 是志愿者节点，运行中掉线很常见。
//
// 间隔每轮都重读设置，因此界面改了参数下一轮就生效；
// connWake 让改完立刻进入下一轮，不必等满一个旧间隔。
func (m *Manager) WatchHealth() {
	fails := map[int]int{}
	for {
		interval := time.Duration(connConf().HealthInterval) * time.Second
		select {
		case <-time.After(interval):
		case <-connWake:
		}
		m.healthSweep(fails)
	}
}

// healthSweep 跑一轮全量探测并处理判掉线的隧道。
//
// 原来是一串串行 for：每条 up 的隧道都要现开一个 curl 子进程，
// 20 条出口 × 6 秒超时就是 120 秒，而外层 time.Tick 每 10 秒触发一次且
// 不排队——Tick 的通道容量只有 1，绝大多数轮次被直接丢弃，
// 真正确认一条隧道死了平均要四分钟。现在并行探测，一轮就是几秒。
func (m *Manager) healthSweep(fails map[int]int) {
	cfg := connConf()
	tunnels := m.Tunnels()

	// 隧道被停掉之后它的槽位计数要清掉，否则 fails 这个 map 只增不减，
	// 而且槽位是复用的：1 号槽位停掉再新建，会继承上一任留下的失败计数。
	alive := make(map[int]bool, len(tunnels))
	for _, t := range tunnels {
		alive[t.Slot] = true
	}
	for slot := range fails {
		if !alive[slot] {
			delete(fails, slot)
		}
	}

	type verdict struct {
		t   *Tunnel
		ok  bool
		why string
	}
	ch := make(chan verdict, len(tunnels))

	// 开着 WARP 时先把每条 netns 的转发状态对齐一遍，再谈探测。
	// 这一步管的是两种"界面和实际不一致"：openvpn 自己重连会把装在 tun0 上的
	// 分流路由一起带走（流量原路走 Cloudflare 出去），而隧道没连着时默认路由
	// 还指着 WireGuard 设备（客户端照样能上网，只是站在 Cloudflare 的数据中心）。
	// 两种都要在探测之前修掉，否则探测拿到的出口 IP 说的不是节点。
	if warpOn() && warpSvc != nil {
		for _, t := range tunnels {
			acc := warpSvc.get(t.Slot)
			if acc == nil {
				continue
			}
			if did := t.warpGuardTunnel(acc); did != "" {
				log.Printf("fanout: 出口 %d (%s) %s", t.Slot, t.host(), did)
			}
		}
	}

	var wg sync.WaitGroup
	// 并发上限：探测本身是子进程 + 走隧道出网，全放开了在 40 条出口时
	// 会把母机 CPU 和隧道带宽一起打满，反而把好隧道挤掉线。
	sem := make(chan struct{}, 8)
	for _, t := range tunnels {
		if t.status() != "up" {
			continue
		}
		wg.Add(1)
		go func(t *Tunnel) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			got, err := m.probeTunnel(t)
			ok := err == nil && got == t.exitIP()
			why := tunnelProbeWhy(got, err)
			// 开着 WARP 时探测到的出口 IP 落在 Cloudflare 宣布的网段里，说的不是节点：
			// 流量只走到了中转就出去了。这种必须算掉线（换节点重连），
			// 不能只把 IP 显示成节点的那个——客户端会以为自己在日本，其实站在 Cloudflare。
			if err == nil && warpOn() && warpExitIsTransitOnly(got) {
				ok = false
				why = "出口变成 " + got + "（只走到 Cloudflare 中转，没走节点）"
			}
			ch <- verdict{t: t, ok: ok, why: why}
		}(t)
	}
	go func() { wg.Wait(); close(ch) }()

	var down []*Tunnel
	for v := range ch {
		if v.ok {
			fails[v.t.Slot] = 0
			continue
		}
		fails[v.t.Slot]++
		if fails[v.t.Slot] < cfg.HealthFailures {
			log.Printf("隧道 %d (%s) 探测失败 %d 次: %s",
				v.t.Slot, v.t.host(), fails[v.t.Slot], v.why)
			continue
		}
		fails[v.t.Slot] = 0
		down = append(down, v.t)
	}
	if len(down) == 0 {
		return
	}

	// 一轮最多重连这么几条。Gate 的机房常常整段一起掉，全放开了重连
	// 只会一起失败：几十条隧道同时去刷同一份节点列表、抢同一批候选节点，
	// 反而把还能用的节点也拖死。剩下的下一轮再看，通常那批节点已经恢复。
	n := cfg.ReconnectLimit
	if len(down) > n {
		log.Printf("%d 条出口同时判掉线，本轮只重连 %d 条，其余下一轮再看", len(down), n)
	}
	for i, t := range down {
		if i >= n {
			break
		}
		// 抖开起手时间，别几条隧道同一毫秒去拉节点列表
		if i > 0 {
			time.Sleep(time.Duration(200+rand.Intn(800)) * time.Millisecond)
		}
		log.Printf("隧道 %d (%s) 已掉线，正在换节点重连", t.Slot, t.host())
		m.reconnect(t, t.host())
	}
}

func tunnelProbeWhy(ip string, err error) string {
	if err != nil {
		return err.Error()
	}
	return "出口变成 " + ip
}

// probeTunnel 查一次这条隧道当前的出口 IP。
// 预算给健康检查超时的两倍：多源轮流试，别因为第一个源抽风就误杀。
func (m *Manager) probeTunnel(t *Tunnel) (string, error) {
	budget := time.Duration(connConf().HealthTimeout) * time.Second * 2
	return probeExitIPIn(t.nsName(), budget)
}

// tunnelHealthy 判断隧道是否还真的走在 VPN 上。
//
// 只看"能不能出网"是不够的：netns 通过 veth 走母机 NAT，
// openvpn 死掉后照样能出网，只是出口变回了母机 IP。
// 所以要比对出口 IP 是否仍是建立隧道时拿到的那个。
func (m *Manager) tunnelHealthy(t *Tunnel) bool {
	got, err := m.probeTunnel(t)
	return err == nil && got == t.exitIP()
}

// reconnect 就地把一条隧道换到别的节点上，保持槽位与端口不变，
// 这样已经分发出去的客户端配置仍然可用。
//
// oldHost 必须是本次重连前那条隧道真正绑着的节点名。调用方若已经
// 改过 t.Node（比如手动换节点），就要把改之前的名字传进来，
// 否则 rebind 找不到旧绑定，入站会掉成孤儿。
func (m *Manager) reconnect(t *Tunnel, oldHost string) {
	t.setStatus("starting", "正在换节点重连")
	t.setExitIP("")

	// 动手之前先把"原来绑的是谁"落盘。
	//
	// 换节点是两步：连上新节点、再把入站改绑过来。两步之间崩溃或重启的话，
	// 存盘的隧道已经是新节点、入站却还指着旧节点，那个入站就成了孤儿。
	// 这条线索留在盘上，重启恢复时能照着把它接回来。
	t.setPrevHost(oldHost)
	if err := m.saveState(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}

	go func() {
		// 杀进程和拆 netns 都是秒级慢操作。放在这里而不是调用方，
		// 是因为调用方是健康检查主循环：串行等 20 条隧道的清理，
		// 一轮 sweep 就全被这几秒吃掉了。
		t.proc().kill()
		t.setProc(nil)
		t.teardownNetns()

		// 通知延后到 rebind/resync 之后：那两步会把入站改绑到新节点，
		// 提前重建配置会因为入站还指着旧节点名而丢掉路由规则
		m.bringUpPersist(t, false, true)
		if t.status() != "up" {
			return
		}
		// 出站 tag 跟着节点名走，换了节点就要把原来指向它的入站重新绑过去，
		// 否则面板里的路由会指向一个已经不存在的出站。
		if t.host() != oldHost {
			if err := m.rebind(oldHost, t); err != nil {
				log.Printf("重连后同步 3x-ui 绑定失败: %v", err)
				return
			}
		} else if err := m.resync(t); err != nil {
			// 节点名没变也要重写一次出站：出口 IP 可能变了，
			// 而且上一轮换节点时留下的绑定需要重新指回来。
			log.Printf("重连后重写 3x-ui 出站失败: %v", err)
			return
		}
		// 入站已经跟过来了，标记可以清了
		t.setPrevHost("")
		if err := m.saveState(); err != nil {
			log.Printf("保存状态失败: %v", err)
		}
	}()
}
