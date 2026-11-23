package main

import (
	"fmt"
	"testing"
	"time"
)

// 补位判定的两处纯函数在这里单独钉住：它们决定"什么时候算掉线"和
// "新出口去哪个地区找"，判错的后果分别是把好节点当死的换掉、
// 以及把用户的地区偏好悄悄改掉。两边都很难从界面上一眼看出来。

func TestPoolGoneHostsNeedsFullWindow(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	first := map[string]time.Time{}
	// NS 非空表示隧道还在、只是没连上——这种要等它自己重连
	members := []poolMember{
		{Host: "a.example", Up: true, NS: "fo0"},
		{Host: "b.example", Up: false, NS: "fo1"},
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
		{Host: "a.example", Up: false, NS: "fo0"},
		{Host: "b.example", Up: false, NS: "fo1"},
		{Host: "c.example", Up: true, NS: "fo2"},
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
	// 没有任何东西会把它带回来，等满 5 分钟只是让组白缺一条。
	members := []poolMember{
		{Host: "a.example", Up: false, NS: ""},
		{Host: "b.example", Up: false, NS: "fo1"},
	}
	live, gone := poolGoneHosts(members, first, 5*time.Minute, now)
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

func TestPoolGoneHostsEmpty(t *testing.T) {
	live, gone := poolGoneHosts(nil, map[string]time.Time{}, 5*time.Minute, time.Now())
	if live != 0 || len(gone) != 0 {
		t.Fatalf("空组判成 live=%d gone=%v", live, gone)
	}
}

func TestPoolFillFollow(t *testing.T) {
	gone := []string{"b.example", "d.example"}

	// 补 1 个：跟随第一个掉线的地区
	if got := poolFillFollow(1, gone); len(got) != 1 || got[0] != "b.example" {
		t.Fatalf("补 1 个 = %v, 想 [b.example]（跟随掉线那个的地区）", got)
	}
	// 补 2 个：各自跟随
	if got := poolFillFollow(2, gone); len(got) != 2 || got[0] != "b.example" || got[1] != "d.example" {
		t.Fatalf("补 2 个 = %v", got)
	}
	// 补 3 个：掉线的只有 2 个，多出的名额没有地区可跟，留空让上层挑任意地区
	got := poolFillFollow(3, gone)
	if len(got) != 3 || got[2] != "" {
		t.Fatalf("补 3 个 = %v, 想第 3 项是空串（不限地区）", got)
	}
	// need <= 0 时不该给出任何候选
	if n := len(poolFillFollow(0, gone)); n != 0 {
		t.Fatalf("need=0 给出 %d 个名额", n)
	}
	// 一条都没记失效（用户把目标条数调大了）也要补满名额，且都不限地区
	any := poolFillFollow(2, nil)
	if len(any) != 2 || any[0] != "" || any[1] != "" {
		t.Fatalf("没有掉线记录时应给两个不限地区的名额：%v", any)
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

// 补位要跟住失效成员的地区，而"节点从列表里消失了"恰恰是最需要跟住的那次。
func TestMemberRegionFallsBackToTunnel(t *testing.T) {
	// 列表里只剩 US 节点，jp-gone 只活在隧道自己的记录里
	mgr := mgrWith([]Node{{HostName: "us1", CountryCode: "US", Residential: true}})
	mgr.tunnels[2] = &Tunnel{Slot: 2, Node: Node{HostName: "jp-gone", CountryCode: "JP"}, Status: "starting"}

	if got := mgr.MemberRegion("jp-gone"); got != "JP" {
		t.Fatalf("节点没了就该退回出口记着的地区，实际 %q", got)
	}
	// 列表里查得到时以列表为准（节点可能改过归属）
	if got := mgr.MemberRegion("us1"); got != "US" {
		t.Fatalf("列表里的地区优先，实际 %q", got)
	}
	// 两边都不知道才留空，让上层按任意地区处理
	if got := mgr.MemberRegion("never-existed"); got != "" {
		t.Fatalf("查不到时应返回空串，实际 %q", got)
	}
}
