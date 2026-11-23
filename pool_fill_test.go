package main

import (
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
