package main

import (
	"net"
	"strings"
)

// 出口 netns 的出站白名单。这个文件里的函数都是纯的：把"该下哪些路由"
// 算成一张命令表，真执行留在 setupNetns，好让判据能被单测钉住。

// netnsDirect 是 netns 在"隧道还没起来"这段时间被允许够到的那几条路。
type netnsDirect struct {
	Gw  string   // 母机侧 veth 地址（10.x.y.1）
	Dev string   // netns 侧网卡名（fopN）
	IPs []string // 节点地址 + netns 用的 DNS
}

// netnsDirectCmds 生成"只放行 openvpn 自己要用的地址"的几条路由，不含
// ip netns exec 前缀。这里绝不出现 default：这是整个函数的存在理由。
//
// 起因是 netns 原来装着一条 `default via <母机>`，本意只是让 openvpn 连得上节点，
// 实际效果却是这条 netns 随时都能从母机出网：openvpn 一掉线，tun0 连同挂在它上面的
// 默认路由一起消失，客户端的下一条连接就顺着母机 NAT 走了出去——出口变成 VPS 自己，
// 而界面上那条出口还挂着上个节点的出口 IP，要等满几轮健康检查才被判死。
// 更糟的是这段窗口取决于"发现得多快"，而探测本身还得走这条已经不可信的路。
//
// 拿掉默认路由之后，"没连上"在路由层面就是"没有路"，不需要谁去撤什么：
//   - openvpn 靠这几条 /32 连节点、查 DNS——隧道起来之前它也只有这两件事要做；
//   - 隧道的默认路由由节点推送（开 WARP 时由 warpSetupCmds 换成 WireGuard 设备）；
//   - 隧道一掉，默认路由随 tun0 一起没了，剩下全是 /32，谁也出不去。
//
// 用 replace 而不是 add：换节点时同一个 netns 会被反复重架，节点地址也可能和
// DNS 撞上同一条，add 第二次就回 File exists，把"路由其实是对的"报成失败。
func netnsDirectCmds(in netnsDirect) [][]string {
	if in.Gw == "" || in.Dev == "" {
		return nil
	}
	var cmds [][]string
	for _, ip := range dedupeSorted(in.IPs) {
		// 只放行 IPv4 字面量。netns 是纯 IPv4 的（见 setupNetns），
		// 混进 v6 会拼出 "ip route replace fd00::1/32 via 10.x.y.1" 这种必然失败的命令，
		// 而它的表现是这条出口建不起来，看不出是 DNS 里夹了个 v6。
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			continue
		}
		cmds = append(cmds, []string{"ip", "route", "replace", parsed.String() + "/32",
			"via", in.Gw, "dev", in.Dev})
	}
	return cmds
}

// routeHasDefault 认 `ip -o route show [default]` 的输出里有没有默认路由。
// 内核印成 "default via 10.0.0.1 dev fop1"，也可能是 "default dev tun0"；
// 没有路由时是空输出。"not a default route" 这类废话行不算。
func routeHasDefault(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == "default" {
			return true
		}
	}
	return false
}
