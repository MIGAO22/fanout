package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// 补位判定的两处纯函数在这里单独钉住：它们决定"什么时候算掉线"和
// "拿哪条在线出口去顶替"，判错的后果分别是把好节点当死的换掉、
// 以及把用户刚建好的出口抢走。两边都很难从界面上一眼看出来。

func TestPoolGoneHostsNeedsFullWindow(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	first := map[string]time.Time{}
	// NS 非空 + starting 表示隧道还在、正在自动换节点重连——这种要等它自己回来
	members := []poolMember{
		{Host: "a.example", Up: true, NS: "fo0", Status: "up"},
		{Host: "b.example", Up: false, NS: "fo1", Status: "starting"},
	}

	// 第一次看到 b 不行：只是记下时间，不该就算死
	live, gone := poolGoneHosts(members, first, 5*time.Minute, now)
	if len(gone) != 0 {
		t.Fatalf("刚看到失败就判死，会把正在自动重连的出口换掉：%v", gone)
	}
	if live != 1 {
		t.Fatalf("live = %d, 想 1（只有 a 可用）", live)
	}
	if _, ok := first["b.example"]; !ok {
		t.Fatal("没记下 b 的首次失败时间，下一轮无从判断等了多久")
	}

	// 窗口未满：仍然不算死
	if _, gone := poolGoneHosts(members, first, 5*time.Minute, now.Add(4*time.Minute+59*time.Second)); len(gone) != 0 {
		t.Fatal("窗口没满就判死")
	}

	// 窗口满了才算死，且能反复调用（幂等）
	late := now.Add(5*time.Minute + time.Second)
	if _, gone := poolGoneHosts(members, first, 5*time.Minute, late); len(gone) != 1 || gone[0] != "b.example" {
		t.Fatalf("等满窗口后应判 b 死：%v", gone)
	}
	if _, gone := poolGoneHosts(members, first, 5*time.Minute, late); len(gone) != 1 {
		t.Fatalf("重复判定结果变了：%v", gone)
	}
}

func TestPoolGoneHostsRecoversAndKeepsOrder(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	dead := now.Add(-10 * time.Minute)
	first := map[string]time.Time{"b.example": dead, "c.example": dead}
	members := []poolMember{
		{Host: "a.example", Up: false, NS: "fo0", Status: "starting"},
		{Host: "b.example", Up: false, NS: "fo1", Status: "starting"},
		{Host: "c.example", Up: true, NS: "fo2", Status: "up"},
	}

	live, gone := poolGoneHosts(members, first, 5*time.Minute, now)
	// a 刚到（还没满窗口）、b 满窗口（判死）、c 活着（清账）——只有 b 该被顶替
	if len(gone) != 1 || gone[0] != "b.example" {
		t.Fatalf("gone = %v, 想只报判死的 [b.example]", gone)
	}
	if live != 1 {
		t.Fatalf("live = %d, 想 1", live)
	}
	// c 回来了 → 清账，否则它下次一掉就被立刻判死
	if _, ok := first["c.example"]; ok {
		t.Error("恢复的出口没清掉失效时间，下次掉线会被秒判死")
	}
	// a 还没到判定窗口 → 补进去，不能漏（漏了就是每轮重新开始计时）
	if got, ok := first["a.example"]; !ok || !got.Equal(now) {
		t.Errorf("a 的首次失败时间 = %v/%v, 想现在(%v)", got, ok, now)
	}
}

func TestPoolGoneHostsTunnelMissingIsImmediate(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	first := map[string]time.Time{}
	// NS 为空 = 这条出口已经没有隧道了（用户删掉，或槽位被回收）。
	// 没有任何东西会把它带回来，等满 90 秒只是让组白缺一条。
	members := []poolMember{
		{Host: "a.example", Up: false, NS: ""},
		{Host: "b.example", Up: false, NS: "fo1", Status: "starting"},
	}
	live, gone := poolGoneHosts(members, first, 90*time.Second, now)
	if len(gone) != 1 || gone[0] != "a.example" {
		t.Fatalf("gone = %v, 想立刻顶替没了的 a.example、给还在重连的 b.example 留窗口", gone)
	}
	if live != 0 {
		t.Fatalf("live = %d, 想 0", live)
	}
	// 没了的成员不该记计时：它已经不是"从什么时候开始看它不行"，
	// 而是根本不在表里了，留着只会攒垃圾。
	if _, ok := first["a.example"]; ok {
		t.Error("隧道已消失的成员还留着失效计时")
	}
	if _, ok := first["b.example"]; !ok {
		t.Error("隧道还在的成员没开始计时")
	}
}

// 重连已经跑完一轮还是没回来（failed/stopped）就是"节点挂了"的确定信号。
//
// 这一条是"要快"的关键：以前只看 Up，把自救中的和自救用完的算成同一种，
// 于是一条彻底死掉的出口也要白等满失效门槛，组里那段时间就少一个人。
func TestPoolGoneHostsDeadReconnectIsImmediate(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	first := map[string]time.Time{}
	members := []poolMember{
		{Host: "failed.example", Up: false, NS: "fo1", Status: "failed"},
		{Host: "stopped.example", Up: false, NS: "fo2", Status: "stopped"},
		{Host: "trying.example", Up: false, NS: "fo3", Status: "starting"},
	}
	live, gone := poolGoneHosts(members, first, 90*time.Second, now)
	if len(gone) != 2 || gone[0] != "failed.example" || gone[1] != "stopped.example" {
		t.Fatalf("gone = %v, 想立刻顶替自救用完的两条、只给还在试的留窗口", gone)
	}
	if live != 0 {
		t.Fatalf("live = %d, 想 0", live)
	}
	for _, h := range []string{"failed.example", "stopped.example"} {
		if _, ok := first[h]; ok {
			t.Errorf("%s 已经判死还留着计时", h)
		}
	}
	if _, ok := first["trying.example"]; !ok {
		t.Error("还在重连的那条没开始计时，下一轮又会从头等")
	}
}

// 空成员表不能把补位带进"整组判死"的分支：没有组员时既没活的也没该换的。
func TestPoolGoneHostsEmpty(t *testing.T) {
	live, gone := poolGoneHosts(nil, map[string]time.Time{}, 5*time.Minute, time.Now())
	if live != 0 || len(gone) != 0 {
		t.Fatalf("空组判成 live=%d gone=%v", live, gone)
	}
}

// 顶替优先用闲着的那批：共用别人组的出口虽然合法，但会把两个组的保活绑在一起，
// 那条一挂两个组同时缺人。只有闲的不够时才动用共用的。
func TestPoolReusePickPrefersSpareThenShared(t *testing.T) {
	cands := []poolFillCand{
		{Host: "busy1", Online: true, Shared: true},
		{Host: "spare1", Online: true, Shared: false},
		{Host: "down1", Online: false, Shared: false},
		{Host: "spare2", Online: true, Shared: false},
		{Host: "busy2", Online: true, Shared: true},
		{Host: "down2", Online: false, Shared: true},
	}

	if got := poolReusePick(cands, 3); !slices.Equal(got, []string{"spare1", "spare2", "busy1"}) {
		t.Fatalf("挑 3 个 = %v, 想先把闲的用完再借共用的，且不选掉线的", got)
	}
	// 只要 1 个：第一顺位那条必须是没挂在任何组里的
	if got := poolReusePick(cands, 1); len(got) != 1 || got[0] != "spare1" {
		t.Fatalf("挑 1 个 = %v, 想 [spare1]", got)
	}
	// 要得比能用的还多：给出全部能用的，不报错也不硬凑
	if got := poolReusePick(cands, 9); len(got) != 4 {
		t.Fatalf("要 9 个应给出 4 个在线的，实际 %v", got)
	}
	// need<=0 时不该给出任何候选
	if n := len(poolReusePick(cands, 0)); n != 0 {
		t.Fatalf("need=0 给出 %d 个候选", n)
	}
	// 全掉线时给出空：上层据此走"新建出口"这条最后手段
	if n := len(poolReusePick([]poolFillCand{{Host: "x", Online: false}}, 2)); n != 0 {
		t.Fatal("没有在线出口时仍给出了顶替候选")
	}
}

// retireFixture 造一个「组刚把人换掉、还剩一条死出口」的现场。
//
// poolHosts 是各组此刻的成员表（本次顶替已经写回，所以换掉的那个不在里面了），
// p 是后端，决定"有没有入站直接绑在这条出口上"问不问得到。
func retireFixture(t *testing.T, host, status string, poolHosts [][]string, p Panel) (*Manager, *PoolService) {
	t.Helper()
	usePanel(t, p)
	mgr := NewManager(4, t.TempDir())
	if status != "" {
		mgr.tunnels[3] = &Tunnel{Slot: 3, Node: Node{HostName: host, CountryCode: "DE"}, Status: status}
	}
	svc, _ := newTestPoolService(t, mgr)
	svc.store.Pools = nil
	for i, hs := range poolHosts {
		svc.store.Pools = append(svc.store.Pools,
			&Pool{ID: fmt.Sprintf("p%d", i+1), Name: "补位组", Mode: poolModeSingle, Hosts: hs})
	}
	return mgr, svc
}

// fillExit 是出口表里的一条出口：主机名与它此刻的状态。
type fillExit struct {
	Host   string
	Status string // up / failed / starting
}

// fillFixture 造一个「开了自动补齐」的现场。
//
// hosts 是组成员名单，exits 是此刻出口表里的全部出口（组里组外都算，
// 组外的就是拿来顶替的在线出口），nodes 是新建补位能挑到的节点（留空表示挑不到）。
func fillFixture(t *testing.T, size int, hosts []string, exits []fillExit, nodes []Node) (*Manager, *PoolService) {
	t.Helper()
	usePanel(t, &fakePanel{})
	mgr := NewManager(20, t.TempDir())
	mgr.nodes = nodes
	for i, e := range exits {
		mgr.tunnels[i+1] = &Tunnel{Slot: i + 1, Node: Node{HostName: e.Host}, Status: e.Status}
	}
	svc, _ := newTestPoolService(t, mgr)
	svc.store.Pools = []*Pool{{
		ID: "p1", Name: "补位组", Mode: poolModeSingle,
		Hosts: hosts, AutoFill: true, Size: size,
	}}
	return mgr, svc
}

// 顶替的第一来源是"此刻已经在线的出口"，而不是新建一条。
//
// 用户要的就是这件事：节点挂了以后组里立刻补上人，别等 openvpn 握手那几十秒，
// 也别白占一个新槽位。新建是最后手段。
func TestFillPoolPrefersOnlineExit(t *testing.T) {
	mgr, svc := fillFixture(t, 2, []string{"gate-dead", "gate-ok"},
		[]fillExit{{"gate-dead", "failed"}, {"gate-ok", "up"}, {"gate-free", "up"}},
		[]Node{{HostName: "brand-new", CountryCode: "US", Residential: true}})

	svc.fillPool(svc.pool("p1"))

	p := svc.pool("p1")
	if !slices.Equal(p.Hosts, []string{"gate-free", "gate-ok"}) {
		t.Fatalf("组员 = %v, 想把死掉的 gate-dead 原位换成在线的 gate-free", p.Hosts)
	}
	if p.Size != 2 {
		t.Fatalf("Size = %d, 想保持用户设的 2", p.Size)
	}
	for _, tn := range mgr.Tunnels() {
		if tn.host() == "brand-new" {
			t.Fatal("有现成在线的出口却还去新建，白占一个槽位")
		}
	}
	if len(mgr.Tunnels()) != 2 {
		t.Fatalf("还剩 %d 条出口，想收尾死的那条之后是 2 条", len(mgr.Tunnels()))
	}
	if note := svc.fillNotes()["p1"]; note != "" {
		t.Fatalf("补齐了还写着原因：%q", note)
	}
}

// 在线的顶完了仍不够，才轮到新建；这一轮挑不到节点时要把差几条写清楚，
// 而且绝不因为补不满就把用户设的条数改小——否则掉一次线，目标就永久降一档。
func TestFillPoolKeepsSizeWhenShort(t *testing.T) {
	// 目标 3 条，组里两条（一条判死），组外只有一条在线的能用，节点清单是空的。
	mgr, svc := fillFixture(t, 3, []string{"gate-dead", "gate-ok"},
		[]fillExit{{"gate-dead", "failed"}, {"gate-ok", "up"}, {"gate-free", "up"}}, nil)

	svc.fillPool(svc.pool("p1"))

	p := svc.pool("p1")
	if p.Size != 3 {
		t.Fatalf("Size = %d, 补不满也不许动用户设的 3", p.Size)
	}
	if !slices.Contains(p.Hosts, "gate-free") {
		t.Fatalf("组员 = %v, 想先把在线的 gate-free 顶进来", p.Hosts)
	}
	if note := svc.fillNotes()["p1"]; !strings.Contains(note, "还差 1 条") {
		t.Fatalf("note = %q, 想写明还差几条", note)
	}
	for _, tn := range mgr.Tunnels() {
		if tn.host() != "gate-ok" && tn.host() != "gate-free" {
			t.Fatalf("冒出了不该有的出口 %s", tn.host())
		}
	}
}

// 候选名单要排除：本组自己的成员（不能自己顶自己）、刚失败还在冷却的、
// 以及已经不在线的。漏掉任何一条，补位就会拿错人。
func TestFillCandidatesSkipsOwnMembersAndCooling(t *testing.T) {
	_, svc := fillFixture(t, 2, []string{"gate-dead", "gate-ok"},
		[]fillExit{
			{"gate-dead", "failed"}, {"gate-ok", "up"}, {"gate-free", "up"},
			{"down-gate", "failed"}, {"gate-shared", "up"}, {"gate-cool", "up"},
		}, nil)
	svc.store.Pools = append(svc.store.Pools,
		&Pool{ID: "p2", Name: "别的组", Mode: poolModeSingle, Hosts: []string{"gate-shared"}})
	svc.markFillBad("gate-cool")

	var spareOnline, shared, offlineFlagged, selfSeen, coolingSeen bool
	for _, c := range svc.fillCandidates("p1") {
		switch c.Host {
		case "gate-free":
			spareOnline = c.Online && !c.Shared
		case "gate-shared":
			shared = c.Shared
		case "down-gate":
			offlineFlagged = !c.Online
		case "gate-ok":
			selfSeen = true
		case "gate-cool":
			coolingSeen = true
		}
	}
	if !spareOnline {
		t.Error("gate-free 该作为在线、未挂在任何组里的候选出现")
	}
	if !shared {
		t.Error("gate-shared 挂在别的组里，候选要标成共用，好让 poolReusePick 排在闲的后面")
	}
	if !offlineFlagged {
		t.Error("down-gate 不在线，候选却报成可用")
	}
	if selfSeen {
		t.Error("本组成员不该是自己的顶替候选")
	}
	if coolingSeen {
		t.Error("刚补位失败还在冷却的出口不该再来一遍")
	}
}

// 用户把目标条数调大时，多出来的名额要凭空追加，不能顺手把还在的组员换掉。
func TestFillPoolGrowsForExtraSlot(t *testing.T) {
	_, svc := fillFixture(t, 3, []string{"gate-a", "gate-b"},
		[]fillExit{{"gate-a", "up"}, {"gate-b", "up"}, {"gate-c", "up"}}, nil)

	svc.fillPool(svc.pool("p1"))
	p := svc.pool("p1")
	if p.Size != 3 {
		t.Fatalf("Size = %d", p.Size)
	}
	if !slices.Equal(p.Hosts, []string{"gate-a", "gate-b", "gate-c"}) {
		t.Fatalf("组员 = %v, 想保留原次序、把在线的 gate-c 追加到最后", p.Hosts)
	}
}

// 刚重启那会儿组员全在自动重连：一个都不许顶掉，也不许往名单里加人。
//
// 这条是"要快"的另一半——快是给确定死了的成员用的。还在自救的成员大概率
// 自己回来，提前占位会把别的组的在线出口整批抢成自己的组员，等原来那批回来，
// 名单就比用户设的条数长了（并发模式下每次访问要多抢几路）。
func TestFillPoolWaitsForReconnectingMembers(t *testing.T) {
	_, svc := fillFixture(t, 3, []string{"gate-a", "gate-b", "gate-c"},
		[]fillExit{
			{"gate-a", "starting"}, {"gate-b", "starting"}, {"gate-c", "starting"},
			{"gate-spare", "up"},
		}, nil)

	svc.fillPool(svc.pool("p1"))

	p := svc.pool("p1")
	if !slices.Equal(p.Hosts, []string{"gate-a", "gate-b", "gate-c"}) {
		t.Fatalf("组员 = %v, 想一个都不动：它们还在自救", p.Hosts)
	}
	if p.Size != 3 {
		t.Fatalf("Size = %d, 想保持 3", p.Size)
	}
	if note := svc.fillNotes()["p1"]; !strings.Contains(note, "正在自动重连") {
		t.Fatalf("note = %q, 想写明差的那几条是在重连、不是缺人", note)
	}
}

// 顶替完成之后原来那条必须收掉。
//
// 判死的成员不会自己消失：它带着原节点在重连回路里无限重试，只在被删掉时才停。
// 补位每轮再搭一条新的进来，这些僵尸就把槽位一点点吃光，
// 于是界面只剩"槽位不够，先不补"——看着正是"节点没了也不自动换"。
func TestRetireMemberStopsReplacedTunnel(t *testing.T) {
	mgr, svc := retireFixture(t, "vpn-gate-dead", "starting",
		[][]string{{"vpn-gate-new"}}, &fakePanel{})

	svc.retireMember("vpn-gate-dead")
	if got := len(mgr.Tunnels()); got != 0 {
		t.Fatalf("被顶替掉的死出口还留着 %d 条，槽位没释放", got)
	}
}

// 四种情况下不能停：它又连上了、别的组还指着它、有入站直接绑着它、后端问不到。
// 误停的代价是把别人正在用的连接拆掉，比留一条僵尸严重得多。
func TestRetireMemberKeepsWhatOthersStillNeed(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		poolHosts [][]string
		panel     Panel
	}{
		{"补位这几分钟里它自己连回来了", "up", [][]string{{"vpn-gate-new"}}, &fakePanel{}},
		{"另一个出口组还指着它", "starting", [][]string{{"vpn-gate-new"}, {"vpn-gate-dead"}}, &fakePanel{}},
		{"有入站直接绑在这条出口上", "failed", [][]string{{"vpn-gate-new"}},
			&fakePanel{inbounds: []Inbound{{ID: 7, Port: 20071, BoundTo: "vpn-gate-dead"}}}},
		{"后端问不到答案时按有人绑着处理", "failed", [][]string{{"vpn-gate-new"}},
			&fakePanel{ibErr: fmt.Errorf("面板连不上")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr, svc := retireFixture(t, "vpn-gate-dead", c.status, c.poolHosts, c.panel)
			svc.retireMember("vpn-gate-dead")
			if len(mgr.Tunnels()) != 1 {
				t.Fatal("不该停的出口被停掉了")
			}
		})
	}
}

// 成员被顶替时隧道早已被用户删掉，是最常见的情况：没东西要收，也不能报错。
func TestRetireMemberNoTunnel(t *testing.T) {
	mgr, svc := retireFixture(t, "vpn-gate-dead", "", [][]string{{"vpn-gate-new"}}, &fakePanel{})
	svc.retireMember("vpn-gate-dead")
	svc.retireMember("")
	if len(mgr.Tunnels()) != 0 {
		t.Fatal("不该凭空造出隧道")
	}
}
