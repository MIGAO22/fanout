package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func upMember(host string, lat time.Duration) poolMember {
	return poolMember{Host: host, NS: "fo-" + host, Up: true, Latency: lat}
}

func firstHost(t *testing.T, c poolChoice) string {
	t.Helper()
	if len(c.Order) == 0 {
		t.Fatal("选路结果为空")
	}
	return c.Order[0].Host
}

// TestPoolSingleSticky 对应"单一出口"的语义：咬住一条，掉了才换，换完继续咬住新的。
func TestPoolSingleSticky(t *testing.T) {
	pp := newPoolPolicy(poolModeSingle, 5)
	members := []poolMember{upMember("a", 0), upMember("b", 0), upMember("c", 0)}

	for i := 0; i < 4; i++ {
		if got := firstHost(t, pp.pick(fmt.Sprintf("t%d", i), members)); got != "a" {
			t.Fatalf("第 %d 次选到 %s，单一出口应一直停在 a", i+1, got)
		}
	}

	// a 掉了：换到下一条，并且此后新目标也要停在新的这条上，不能又飘回列表首条
	down := []poolMember{members[0], upMember("b", 0), upMember("c", 0)}
	down[0].Up = false
	if got := firstHost(t, pp.pick("t9", down)); got != "b" {
		t.Fatalf("a 掉线后选到 %s，期望 b", got)
	}
	if got := firstHost(t, pp.pick("t10", down)); got != "b" {
		t.Fatalf("换完出口后新目标落到 %s，期望继续停在 b", got)
	}
}

// TestPoolSingleDegradeOrder 验证降级顺序：首选排最前，其余按勾选次序跟着。
func TestPoolSingleDegradeOrder(t *testing.T) {
	pp := newPoolPolicy(poolModeSingle, 5)
	members := []poolMember{upMember("a", 0), upMember("b", 0), upMember("c", 0)}
	c := pp.pick("t0", members)
	if c.Concurrent {
		t.Fatal("单一出口不该并发抢跑")
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if c.Order[i].Host != w {
			t.Fatalf("降级顺序 = %v，期望 %v", hostsOf(c.Order), want)
		}
	}
}

func hostsOf(ms []poolMember) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Host)
	}
	return out
}

// TestPoolRoundRobinStep 对应"每访问 5 次换一个出口"。
// 计数按新目标走：同一站点开 10 个连接仍算一次换。
func TestPoolRoundRobinStep(t *testing.T) {
	pp := newPoolPolicy(poolModeRoundRobin, 5)
	members := []poolMember{upMember("a", 0), upMember("b", 0), upMember("c", 0)}

	for i := 0; i < 5; i++ {
		if got := firstHost(t, pp.pick(fmt.Sprintf("site-%d", i), members)); got != "a" {
			t.Fatalf("前 5 个目标应都在 a，第 %d 个跑到 %s", i+1, got)
		}
	}
	for i := 5; i < 10; i++ {
		if got := firstHost(t, pp.pick(fmt.Sprintf("site-%d", i), members)); got != "b" {
			t.Fatalf("第 %d 个目标应在 b，实际 %s", i+1, got)
		}
	}
	if got := firstHost(t, pp.pick("site-10", members)); got != "c" {
		t.Fatalf("第 11 个目标应轮到 c，实际 %s", got)
	}
	// 走到列表末尾要绕回第一条
	for i := 11; i < 15; i++ {
		pp.pick(fmt.Sprintf("site-%d", i), members)
	}
	if got := firstHost(t, pp.pick("site-15", members)); got != "a" {
		t.Fatalf("整组走完应回到 a，实际 %s", got)
	}
}

// TestPoolAffinityKeepsTargetStable 是"按目标粘性"的核心：
// 同一个目标反复访问必须走同一条出口，否则一次浏览会在同一站点上反复换 IP。
func TestPoolAffinityKeepsTargetStable(t *testing.T) {
	for _, mode := range []string{poolModeRoundRobin, poolModeBalance} {
		pp := newPoolPolicy(mode, 1)
		members := []poolMember{upMember("a", 0), upMember("b", 0)}
		first := firstHost(t, pp.pick("example.com", members))
		for i := 0; i < 6; i++ {
			pp.pick("other.com", members) // 期间别的目标照常轮转
			if got := firstHost(t, pp.pick("example.com", members)); got != first {
				t.Fatalf("%s 模式下同目标第 %d 次换到了 %s（首次 %s）", mode, i+1, got, first)
			}
		}
	}
}

// TestPoolBalancePicksFastest 对应"当前出口卡顿自动切走"。
func TestPoolBalancePicksFastest(t *testing.T) {
	pp := newPoolPolicy(poolModeBalance, 1)
	slow := upMember("slow", 2*time.Second)
	fast := upMember("fast", 50*time.Millisecond)

	// 先粘到慢的那条：模拟它一开始是唯一的样本
	if got := firstHost(t, pp.pick("t0", []poolMember{slow})); got != "slow" {
		t.Fatalf("初始选到 %s，期望 slow", got)
	}
	// 快出口出现且慢出口明显更差：应当切走
	if got := firstHost(t, pp.pick("t0", []poolMember{slow, fast})); got != "fast" {
		t.Fatalf("慢出口 2s / 快出口 50ms 时仍停在 %s，应切到 fast", got)
	}
}

// TestPoolBalanceNoFlapping 防止来回跳：两者都快时，几百毫秒的差异不值得换 IP。
func TestPoolBalanceNoFlapping(t *testing.T) {
	pp := newPoolPolicy(poolModeBalance, 1)
	a := upMember("a", 100*time.Millisecond)
	b := upMember("b", 40*time.Millisecond)
	if got := firstHost(t, pp.pick("t0", []poolMember{a})); got != "a" {
		t.Fatalf("初始应停在 a，实际 %s", got)
	}
	if got := firstHost(t, pp.pick("t0", []poolMember{a, b})); got != "a" {
		t.Fatalf("100ms vs 40ms 都在阈值下，却切到了 %s", got)
	}
}

// TestPoolBalanceSamplesUnknown 验证没有样本的成员会被优先实测，
// 否则新加进组的出口永远轮不上，实测延迟也就永远是空的。
func TestPoolBalanceSamplesUnknown(t *testing.T) {
	pp := newPoolPolicy(poolModeBalance, 1)
	known := upMember("known", 80*time.Millisecond)
	fresh := upMember("fresh", 0)
	if got := firstHost(t, pp.pick("t0", []poolMember{known, fresh})); got != "fresh" {
		t.Fatalf("有样本的一直优先，新成员 %s 永远测不到", got)
	}
}

// TestPoolConcurrentRacesAll 对应"同时发送所有出口，谁最快用谁"。
func TestPoolConcurrentRacesAll(t *testing.T) {
	pp := newPoolPolicy(poolModeConcurrent, 5)
	members := []poolMember{upMember("a", 0), upMember("b", 0), {Host: "c", Up: false}}
	c := pp.pick("t0", members)
	if !c.Concurrent {
		t.Fatal("并发模式没有置 Concurrent")
	}
	if len(c.Order) != 2 {
		t.Fatalf("并发候选 = %v，应只含在线的两条", hostsOf(c.Order))
	}
}

// TestPoolCoolingDeprioritized：刚失败的成员暂时不优先，但仍要排在降级队列里，
// 全组都在冷却时也得挨个再试一次，否则一次抖动就能让组永久起不来。
func TestPoolCoolingDeprioritized(t *testing.T) {
	pp := newPoolPolicy(poolModeSingle, 5)
	cooling := upMember("a", 0)
	cooling.Cooling = true
	fresh := upMember("b", 0)

	c := pp.pick("t0", []poolMember{cooling, fresh})
	if got := firstHost(t, c); got != "b" {
		t.Fatalf("冷却中的 a 仍被优先选中（选了 %s）", got)
	}
	if len(c.Order) != 2 || c.Order[1].Host != "a" {
		t.Fatalf("降级队列 = %v，冷却成员仍应留在备选里", hostsOf(c.Order))
	}

	all := []poolMember{cooling}
	if got := firstHost(t, pp.pick("t1", all)); got != "a" {
		t.Fatalf("全组都在冷却时应照试，实际选到 %s", got)
	}
}

func TestPoolNoUpMember(t *testing.T) {
	pp := newPoolPolicy(poolModeSingle, 5)
	c := pp.pick("t0", []poolMember{{Host: "a", Up: false}, {Host: "b"}})
	if len(c.Order) != 0 {
		t.Fatalf("没有在线成员时应返回空，实际 %v", hostsOf(c.Order))
	}
}

// TestPoolForgetMember：出口掉线后，原本粘在它上面的目标要重新分配，
// 否则每条新连接都会先撞一次已死的成员。
func TestPoolForgetMember(t *testing.T) {
	pp := newPoolPolicy(poolModeRoundRobin, 1)
	members := []poolMember{upMember("a", 0), upMember("b", 0)}
	pp.pick("t0", members)
	pp.pick("t1", members)
	pp.forgetMember("a")
	for k, v := range pp.affi {
		if v == "a" {
			t.Fatalf("forgetMember(a) 之后 %s 仍粘在 a 上", k)
		}
	}
	// 别的成员名下的粘性要留着，那是无关的另一笔账
	if pp.affi["t1"] != "b" {
		t.Fatalf("t1 的粘性被误清了：%v", pp.affi)
	}
	for _, k := range pp.affiOrder {
		if _, ok := pp.affi[k]; !ok {
			t.Fatalf("affiOrder 残留 %s，与 affi 不一致", k)
		}
	}
}

// TestPoolAffinityCap：一个客户端一天就能划出上千个目标，
// 粘性表不封顶会变成常驻内存泄漏。
func TestPoolAffinityCap(t *testing.T) {
	pp := newPoolPolicy(poolModeRoundRobin, 1)
	members := []poolMember{upMember("a", 0), upMember("b", 0)}
	for i := 0; i < poolAffinityMax+500; i++ {
		pp.pick(fmt.Sprintf("t%d", i), members)
	}
	if len(pp.affi) > poolAffinityMax {
		t.Fatalf("粘性表涨到 %d，超过上限 %d", len(pp.affi), poolAffinityMax)
	}
	if len(pp.affiOrder) != len(pp.affi) {
		t.Fatalf("淘汰后顺序表 %d 与映射 %d 不符", len(pp.affiOrder), len(pp.affi))
	}
	// 最新的键必须还在：被淘汰的应该是最早那些
	if _, ok := pp.affi[fmt.Sprintf("t%d", poolAffinityMax+499)]; !ok {
		t.Fatal("最新目标被过早淘汰")
	}
}

func TestTargetKey(t *testing.T) {
	cases := map[string]string{
		"93.184.216.34:443": "93.184.216.34",
		"example.com:8080":  "example.com",
		"no-port":           "no-port",
	}
	for in, want := range cases {
		if got := targetKey(in); got != want {
			t.Errorf("targetKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPoolIdentityRoundTrip(t *testing.T) {
	p := &Pool{ID: "p7"}
	id, ok := poolIdentityToID(p.identity())
	if !ok || id != "p7" {
		t.Fatalf("identity %q 还原成 (%q,%v)，期望 (p7,true)", p.identity(), id, ok)
	}
	if _, ok := poolIdentityToID("vpn-gate.example.jp"); ok {
		t.Fatal("节点主机名被误判成出口组")
	}
	if _, ok := poolIdentityToID("pool-"); ok {
		t.Fatal("空前缀不应算作组身份")
	}
	// 组身份过 sanitizeTag 必须原样，否则 BoundTo 与查找键会对不上
	if got := sanitizeTag(p.identity()); got != "pool-p7" {
		t.Fatalf("sanitizeTag(identity) = %s", got)
	}
}

func TestPoolNormalizeAndValidate(t *testing.T) {
	p := &Pool{ID: "p1", Mode: "nonsense", RRStep: 0, Hosts: []string{"a", " a ", "", "b", "a"}}
	p.normalize()
	if p.Mode != poolModeSingle {
		t.Errorf("非法模式没有回落： %s", p.Mode)
	}
	if p.RRStep != poolDefaultRRStep {
		t.Errorf("步长 0 没有回落默认： %d", p.RRStep)
	}
	if len(p.Hosts) != 2 || p.Hosts[0] != "a" || p.Hosts[1] != "b" {
		t.Errorf("去重去空失败或顺序变了：%v", p.Hosts)
	}
	if p.Name == "" {
		t.Error("空组名应补一个可用的默认名")
	}

	if err := (&Pool{ID: "p2", Mode: poolModeSingle, RRStep: 5}).validate(); err == nil {
		t.Error("空组员应校验失败")
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("h%d", i)
	}
	if err := (&Pool{Mode: poolModeSingle, RRStep: 5, Hosts: many}).validate(); err == nil {
		t.Error("超过上限的组员应校验失败")
	}
}

// TestPoolStoreRoundTrip 保证 ID 不复用：
// 复用旧 ID 会让还指着它的入站在新组上悄悄复活，流量从意外的出口出去。
func TestPoolStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &poolStore{Next: 1}
	st.Pools = append(st.Pools, &Pool{ID: "p1", Name: "日本组", Mode: poolModeBalance, RRStep: 5, Hosts: []string{"a", "b"}})
	if err := st.save(dir); err != nil {
		t.Fatal(err)
	}

	got, err := loadPoolStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pools) != 1 || got.Pools[0].Name != "日本组" || got.Pools[0].Mode != poolModeBalance {
		t.Fatalf("读回内容与写入不符：%+v", got.Pools)
	}
	if got.Next < 2 {
		t.Fatalf("Next = %d，删组后新建会复用 p1", got.Next)
	}

	// 手写一个 Next 偏小的档：加载要按现存 ID 把它抬上去
	blob := []byte(`{"next":1,"pools":[{"id":"p9","name":"x","mode":"single","rr_step":5,"hosts":["a"]}]}`)
	if err := os.WriteFile(filepath.Join(dir, "pools.json"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	got2, err := loadPoolStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Next <= 9 {
		t.Fatalf("Next = %d，没按现存 ID 抬高", got2.Next)
	}
}

func TestLoadPoolStoreMissingFile(t *testing.T) {
	st, err := loadPoolStore(t.TempDir())
	if err != nil {
		t.Fatalf("没有存档时应给空库而不是报错: %v", err)
	}
	if st.Next != 1 || len(st.Pools) != 0 {
		t.Fatalf("空库形态不对： %+v", st)
	}
}

func TestPoolStoreRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(poolStatePath(dir), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPoolStore(dir); err == nil {
		t.Fatal("坏存档必须报错，静默清空会让用户的组全没")
	}
}
