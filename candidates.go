package main

import "fmt"

// 重连时"该依次试哪些节点"的排序。抽出来是因为这一步决定
// 「节点没了之后这条出口到底换不换得回来」，而它原先埋在一堆锁里面没法测。

// orderCandidates 排出一条隧道这一轮该依次尝试的节点。
//
// 同地区的排在前面，跨地区的只在一种情况下补进来：preferCurrent 为假，
// 也就是这条隧道已经整轮失败过一次、同区那几个连同它自己都试完了。
// 这一步是整个补位链的短板所在——家宽节点成片消失是常态，一个地区空了之后
// 只给同区留备选，就等于让这条出口永远卡在"正在连接"：
// 下一轮刷新列表它还是不在，同区还是挑不到，谁都不会来换掉它。
// 首轮不跨区是刻意的：那时目标是按用户挑的地区把这条出口恢复原样，
// 手动新建的出口尤其不能一上来就跑到别的国家去。
//
// region 由调用方给出（节点列表里查不到时退回隧道自己身上记的那个），
// 为空表示不限制地区，此时跨区与同区是同一批。
func orderCandidates(current Node, region string, preferCurrent bool, maxTries int,
	pool []Node, used, avoid map[string]bool) []Node {
	var same, other []Node
	for _, n := range pool {
		if n.HostName == current.HostName || used[n.HostName] || avoid[n.HostName] {
			continue
		}
		if region == "" || n.CountryCode == region {
			same = append(same, n)
		} else {
			other = append(other, n)
		}
	}

	var out []Node
	if preferCurrent {
		out = append(out, current)
	}
	take := func(src []Node) {
		for _, n := range src {
			if len(out) >= maxTries {
				return
			}
			out = append(out, n)
		}
	}
	take(same)
	if !preferCurrent {
		take(other)
	}
	if !preferCurrent && len(out) < maxTries {
		// 别的地区也空了：还是给原来那条一次机会，它可能只是刚好抖了一下
		out = append(out, current)
	}
	return out
}

// swapNote 说清这一次换到了第几个候选，跨区时把地区写出来。
//
// 跨区必须吱一声：用户挑的是日本，出口悄悄变成罗马尼亚，
// 这种差异界面上看不出来就只会当成 bug 报上来。
func swapNote(i int, home, got string) string {
	if home != "" && got != "" && got != home {
		return fmt.Sprintf("同区没有空闲节点，已跨区换到 %s（第 %d 个候选）", got, i+1)
	}
	return fmt.Sprintf("已换到第 %d 个候选节点", i+1)
}
