package main

import (
	"strings"
	"testing"
)

// 这批用例钉的是"节点没了之后这条出口换不换得回来"。
// 报上来的现象是出口组里的节点消失后一直不顶替，根因在候选排序：
// 只给同地区的备选，而家宽节点成片消失时同区就是空的，
// 于是重连回路永远只在死节点上空转。

func nodesOf(list ...string) []Node {
	out := make([]Node, 0, len(list))
	for _, spec := range list {
		// spec 形如 jp2:JP
		name, region := spec, ""
		if i := strings.IndexByte(spec, ':'); i >= 0 {
			name, region = spec[:i], spec[i+1:]
		}
		out = append(out, Node{HostName: name, CountryCode: region})
	}
	return out
}

func namesOf(list []Node) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.HostName)
	}
	return out
}

func TestOrderCandidatesFirstRoundStaysInRegion(t *testing.T) {
	pool := nodesOf("jp2:JP", "jp3:JP", "us1:US", "de1:DE")
	cur := Node{HostName: "jp1", CountryCode: "JP"}

	got := orderCandidates(cur, "JP", true, 6, pool, nil, nil)
	if strings.Join(namesOf(got), ",") != "jp1,jp2,jp3" {
		t.Fatalf("首轮必须只在本地区试（当前节点打头），实际 %v", namesOf(got))
	}
}

// 核心回归：一整轮都失败之后同区没人了，必须跨区补上。
// 改之前这一支只会返回当前那个死节点，出口就永久卡在"正在连接"。
func TestOrderCandidatesWidensAfterFailedRound(t *testing.T) {
	cur := Node{HostName: "gone.example", CountryCode: "JP"}
	pool := nodesOf("us1:US", "de1:DE", "gone.example:JP")

	got := orderCandidates(cur, "JP", false, 6, pool, map[string]bool{"gone.example": true}, nil)
	if strings.Join(namesOf(got), ",") != "us1,de1,gone.example" {
		t.Fatalf("同区空了应跨区顶替、死节点垫底，实际 %v", namesOf(got))
	}
}

// 跨区是兜底不是并列：同区还有空闲节点时轮不到外地节点。
func TestOrderCandidatesPrefersRegionOverWidening(t *testing.T) {
	cur := Node{HostName: "jp1", CountryCode: "JP"}
	pool := nodesOf("us1:US", "jp2:JP", "de1:DE", "jp3:JP")

	got := orderCandidates(cur, "JP", false, 6, pool, nil, nil)
	if strings.Join(namesOf(got), ",") != "jp2,jp3,us1,de1,jp1" {
		t.Fatalf("顺序应是同区→跨区→原来的死节点，实际 %v", namesOf(got))
	}
}

// 预算照样要守住：同区已经排满时不再往里掺跨区节点。
func TestOrderCandidatesRespectsMaxTries(t *testing.T) {
	cur := Node{HostName: "jp1", CountryCode: "JP"}
	pool := nodesOf("jp2:JP", "jp3:JP", "us1:US")

	got := orderCandidates(cur, "JP", false, 2, pool, nil, nil)
	if strings.Join(namesOf(got), ",") != "jp2,jp3" {
		t.Fatalf("超出 maxTries 的候选不该进来，实际 %v", namesOf(got))
	}
}

// used / avoid 两重过滤跨区之后仍然有效：
// 别的出口占着的节点、用户明确换掉的节点，跨区也不能捡回来。
func TestOrderCandidatesWideningStillFilters(t *testing.T) {
	cur := Node{HostName: "jp1", CountryCode: "JP"}
	pool := nodesOf("us1:US", "us2:US", "de1:DE")
	used := map[string]bool{"us1": true}
	avoid := map[string]bool{"us2": true}

	got := orderCandidates(cur, "JP", false, 6, pool, used, avoid)
	if strings.Join(namesOf(got), ",") != "de1,jp1" {
		t.Fatalf("被占用/被换掉的节点跨区时也要跳过，实际 %v", namesOf(got))
	}
}

// 地区本身未知时没什么可守的，整份列表都是候选。
func TestOrderCandidatesUnknownRegion(t *testing.T) {
	cur := Node{HostName: "vpn9", CountryCode: ""}
	pool := nodesOf("us1:US", "jp1:JP")

	got := orderCandidates(cur, "", true, 6, pool, nil, nil)
	if strings.Join(namesOf(got), ",") != "vpn9,us1,jp1" {
		t.Fatalf("查不到地区时不该再做地区限制，实际 %v", namesOf(got))
	}
}

// 全机器的节点都被别的出口占着：至少还要给原来那条一次机会，
// 返回空列表等于让重连回路连一次握手都不做。
func TestOrderCandidatesAlwaysReturnsSomething(t *testing.T) {
	cur := Node{HostName: "jp1", CountryCode: "JP"}
	pool := nodesOf("jp2:JP", "us1:US")
	used := map[string]bool{"jp2": true, "us1": true}

	for _, prefer := range []bool{true, false} {
		got := orderCandidates(cur, "JP", prefer, 6, pool, used, nil)
		if len(got) != 1 || got[0].HostName != "jp1" {
			t.Fatalf("preferCurrent=%v 时应只剩原来那条兜底，实际 %v", prefer, namesOf(got))
		}
	}
}

func TestSwapNoteMentionsCrossRegion(t *testing.T) {
	if got := swapNote(1, "JP", "JP"); got != "已换到第 2 个候选节点" {
		t.Errorf("同区换候选的文案变了：%q", got)
	}
	got := swapNote(1, "JP", "DE")
	if !strings.Contains(got, "跨区") || !strings.Contains(got, "DE") {
		t.Errorf("跨区换节点必须写明地区，否则用户以为还是原来那个国家：%q", got)
	}
	// 地区有一边读不出来时不要瞎断言"跨区"
	for _, c := range [][2]string{{"", "DE"}, {"JP", ""}} {
		if got := swapNote(0, c[0], c[1]); strings.Contains(got, "跨区") {
			t.Errorf("home=%q got=%q 时不该说跨区：%q", c[0], c[1], got)
		}
	}
}
