package main

import (
	"strings"
	"testing"
)

// 这一组用例守的是一句话：netns 在建架阶段不许出现默认路由。
// 默认路由一旦装在母机那条 veth 上，openvpn 掉线之后客户端就会从 VPS 真 IP 出去，
// 而界面上还挂着上个节点的出口 IP——这正好是 fanout 存在的反面。

func TestNetnsDirectCmdsNeverInstallDefault(t *testing.T) {
	cmds := netnsDirectCmds(netnsDirect{
		Gw:  "10.99.12.1",
		Dev: "fop12",
		IPs: []string{"61.215.116.134", "8.8.8.8", "219.100.37.198"},
	})
	if len(cmds) != 3 {
		t.Fatalf("三条地址该出三条命令，得到 %d 条：%v", len(cmds), cmds)
	}
	for _, c := range cmds {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "default") {
			t.Errorf("白名单里出现了默认路由：%s", joined)
		}
		if !strings.HasSuffix(joined, "via 10.99.12.1 dev fop12") {
			t.Errorf("例外路由没有指向母机那条 veth：%s", joined)
		}
		if !strings.Contains(joined, "/32") {
			t.Errorf("只该按单地址放行，别顺手放开一片网段：%s", joined)
		}
		if strings.ContainsAny(joined, "&|;$`") {
			t.Errorf("命令是 argv 数组，不该出现 shell 元字符：%s", joined)
		}
	}
	// 必须用 replace：换节点时同一个 netns 会反复重架，
	// add 第二次撞 File exists，会把"路由其实是对的"报成建架失败。
	for _, c := range cmds {
		if c[2] != "replace" {
			t.Errorf("应当幂等地 replace，得到：%v", c)
		}
	}
}

func TestNetnsDirectCmdsShape(t *testing.T) {
	got := netnsDirectCmds(netnsDirect{Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"1.2.3.4"}})
	want := "ip route replace 1.2.3.4/32 via 10.0.0.1 dev fop1"
	if len(got) != 1 || strings.Join(got[0], " ") != want {
		t.Fatalf("命令形状不对：得到 %v，想要 %q", got, want)
	}
	// 顺序稳定：同一份配置每次架出来应该长得一样，否则日志和 diff 都没法看
	first := flatCmds(netnsDirectCmds(netnsDirect{
		Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"9.9.9.9", "1.1.1.1", "9.9.9.9"}}))
	second := flatCmds(netnsDirectCmds(netnsDirect{
		Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"1.1.1.1", "9.9.9.9"}}))
	if first != second {
		t.Errorf("去重+排序后顺序仍随输入变化：\n%s\n---\n%s", first, second)
	}
}

// flatCmds 把命令表压成可比对的多行文本。
func flatCmds(cmds [][]string) string {
	var lines []string
	for _, c := range cmds {
		lines = append(lines, strings.Join(c, " "))
	}
	return strings.Join(lines, "\n")
}

func TestNetnsDirectCmdsRejectsUnroutable(t *testing.T) {
	// v6、域名、空串、缺网关卡住时拼出来的东西要么让内核报错、
	// 要么静默少一条路由——前者表现为"这条出口建不起来"，后者更难查。
	cases := []struct {
		name string
		in   netnsDirect
		want int
	}{
		{"只有 v6", netnsDirect{Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"fd00::1"}}, 0},
		{"域名（没解析成 IP 就不该下路由）", netnsDirect{Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"vpn.example.com"}}, 0},
		{"空串与空白", netnsDirect{Gw: "10.0.0.1", Dev: "fop1", IPs: []string{"", "  "}}, 0},
		{"混着放也只留 IPv4", netnsDirect{Gw: "10.0.0.1", Dev: "fop1",
			IPs: []string{"8.8.8.8", "fd00::1", "", "1.1.1.1"}}, 2},
		{"缺网关", netnsDirect{Dev: "fop1", IPs: []string{"8.8.8.8"}}, 0},
		{"缺网卡", netnsDirect{Gw: "10.0.0.1", IPs: []string{"8.8.8.8"}}, 0},
		{"一条地址都没有", netnsDirect{Gw: "10.0.0.1", Dev: "fop1"}, 0},
	}
	for _, c := range cases {
		if got := len(netnsDirectCmds(c.in)); got != c.want {
			t.Errorf("%s：想要 %d 条，得到 %d 条（%v）", c.name, c.want, got, netnsDirectCmds(c.in))
		}
	}
}

// 节点侧那批地址是从 .ovpn 里捞的（remote / remote-entry），
// 建架时真正的输入是"配置 + CSV 的 IP + DNS"。这里把两头接起来验一次，
// 免得白名单写对了解析写错了——症状是隧道连上但客户端出不去。
func TestNetnsDirectCoversWhatOpenVPNNeeds(t *testing.T) {
	cfg := "dev tun\nproto udp\nremote 61.215.116.134 1194\nremote backup.example.net 443\n; remote 203.0.113.9 80\nresolv-retry infinite\n"
	ips := append(warpGateIPs(cfg, "219.100.37.198"), "8.8.8.8")
	cmds := netnsDirectCmds(netnsDirect{Gw: "10.0.0.1", Dev: "fop1", IPs: ips})
	var got string
	for _, c := range cmds {
		got += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"61.215.116.134/32", "219.100.37.198/32", "8.8.8.8/32"} {
		if !strings.Contains(got, want) {
			t.Errorf("白名单里必须有 %s（节点地址 + CSV 兜底 + DNS），实际：\n%s", want, got)
		}
	}
	// 注释掉的那行不是候选，钉它只会多一条没人用的路由
	if strings.Contains(got, "203.0.113.9") {
		t.Errorf("把注释掉的 remote 钉进了白名单：\n%s", got)
	}
	// 主机名形式的 remote 不能凭空变成路由——它得先解析，这里断言的是
	// "没解析过的主机名不会悄悄漏掉"：由 nodeEgressIPs 负责补，缺了要在界面上看得见。
	if strings.Contains(got, "backup.example.net") {
		t.Errorf("主机名不能直接下路由：\n%s", got)
	}
}

func TestRouteHasDefault(t *testing.T) {
	yes := []string{
		"default via 10.99.12.1 dev fop1 \n",
		"default dev tun0 table main proto kernel scope link src 10.8.0.6 \n",
		"default via 10.0.0.1 dev eth0 metric 100\ndefault via 10.0.0.2 dev eth1\n",
		"  default   dev   fow1  \n",
	}
	no := []string{
		"",
		"\n\n",
		"10.8.0.0/24 dev tun0 proto kernel scope link src 10.8.0.6 \n",
		"Error: can't open route for source fd00::1.\n",
		"default", // 光一个词不是路由行
		"notdefault via 1.1.1.1 dev eth0\n",
	}
	for _, s := range yes {
		if !routeHasDefault(s) {
			t.Errorf("这句明明有默认路由：%q", s)
		}
	}
	for _, s := range no {
		if routeHasDefault(s) {
			t.Errorf("这句不该算有默认路由：%q", s)
		}
	}
}
