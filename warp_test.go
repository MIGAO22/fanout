package main

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这批用例钉的是"WARP 那条链路会不会悄悄不成立"。
// 它比一般功能危险的地方在于：写错之后流量不是报错，而是照旧从母机出去，
// 出口 IP 还能显示出一个"看起来对"的地址，健康检查也过不去报警。
// 所以注册响应的解读、路由的先后顺序、以及"关掉时一个字都不该变"这三件事
// 都在这里断言，而不是等到真机上去祈祷。

// warpRegFixture 是实测抓到的注册响应（字段名和形状照真的那份）。
// 之所以连没用的字段也留着：这个接口一旦改形状，最先崩的就是这里的解析，
// 用真实报文而不是最小报文才测得到。
const warpRegFixture = `{
  "id": "3f2c1e0a-9b7d-4c6e-8a1f-2d5b7c9e0f13",
  "key": "hB3U4kK6UZ0O2mQvVa1nYyt3Ld9FbR7iXsZ0oCjPq1M=",
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.fixture",
  "type": "a",
  "name": "fanout",
  "account": {"id": "acct-1", "account_type": "free", "warp_plus": true, "role": "child"},
  "config": {
    "client_id": "0f9d0000-1111-2222-3333-444455556666",
    "peers": [
      {
        "public_key": "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
        "endpoint": {
          "v4": "162.159.192.6:0",
          "v6": "[2606:4700:d0::a29f:c006]:0",
          "host": "engage.cloudflareclient.com:2408",
          "ports": [2408, 500, 1701, 4500]
        },
        "allowed_ips": ["0.0.0.0/0", "::/0"]
      }
    ],
    "interface": {
      "addresses": {"v4": "172.16.0.2", "v6": "2606:4700:110:8c91:320c:fcfc:fd0e:ba60"},
      "dns": {"servers": "1.1.1.1"}
    },
    "services": {"http_proxy": "172.16.0.1:2480"}
  },
  "warp_enabled": false,
  "enabled": true,
  "place": 0,
  "locale": "en-US",
  "policy": {"tunnel_protocol": "masque", "post_quantum": "enabled_with_downgrades"}
}`

func TestWarpParseRealRegistration(t *testing.T) {
	resp, err := parseWarpReg([]byte(warpRegFixture))
	if err != nil {
		t.Fatalf("解析注册响应失败: %v", err)
	}
	pub := resp.Key // 真实响应就是把本机送上去的公钥回显在这里
	acc, err := warpAccountOf(12, resp, "priv-bytes", pub, warpClientVersion,
		time.Date(2026, 11, 24, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("注册记录生成失败: %v", err)
	}
	// 接入点必须是 IPv4 且端口来自 host，而不是响应里那个没用的 "162.159.192.6:0"
	if acc.Endpoint != "162.159.192.6" {
		t.Errorf("接入点 = %q，想要 162.159.192.6", acc.Endpoint)
	}
	if acc.Port != warpPort {
		t.Errorf("端口 = %d，想要 %d", acc.Port, warpPort)
	}
	if acc.PeerKey != warpPeerPub {
		t.Errorf("peer 公钥 = %q，想要官方那个", acc.PeerKey)
	}
	if acc.TunIP != "172.16.0.2" {
		t.Errorf("隧道地址 = %q，响应里给的是 172.16.0.2", acc.TunIP)
	}
	if acc.ID != "3f2c1e0a-9b7d-4c6e-8a1f-2d5b7c9e0f13" || acc.Token == "" {
		t.Errorf("id/token 没从响应里取出来: %q / %q", acc.ID, acc.Token)
	}
	if acc.CreatedAt != "2026-11-24T03:04:05Z" {
		t.Errorf("created_at = %q，要 UTC 的 RFC3339", acc.CreatedAt)
	}
	if acc.devName() != warpDevName(instTag, 12) {
		t.Errorf("devName 和 warpDevName 对不上")
	}
}

func TestWarpAccountRejectsKeyMismatch(t *testing.T) {
	resp, err := parseWarpReg([]byte(warpRegFixture))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 对端记住的公钥和手上这对不一致时，链路只会静默黑洞，必须当场报错
	if _, err := warpAccountOf(3, resp, "p", "OTHERKEY=", warpClientVersion, time.Now()); err == nil {
		t.Fatal("公钥不一致却成功了")
	}
}

func TestWarpRegRespRequiresUsableFields(t *testing.T) {
	cases := map[string]string{
		"缺 id":    `{"token":"t","config":{"peers":[{"public_key":"k","endpoint":{"v4":"1.2.3.4:0"}}]}}`,
		"缺 token": `{"id":"i","config":{"peers":[{"public_key":"k","endpoint":{"v4":"1.2.3.4:0"}}]}}`,
		"没 peer":  `{"id":"i","token":"t","config":{"peers":[]}}`,
		"门户页":     `<html><body>captive portal</body></html>`,
	}
	for name, body := range cases {
		if _, err := parseWarpReg([]byte(body)); err == nil {
			t.Errorf("%s：本该报错却成功了", name)
		}
	}
}

func TestWarpEndpointPortPreference(t *testing.T) {
	// 实测形状：v4 是 "162.159.192.6:0"（端口那段没填），真端口在 host 里
	if ip, port, err := warpEndpoint("162.159.192.6:0", "engage.cloudflareclient.com:2408", []int{2408, 500}); err != nil ||
		ip != "162.159.192.6" || port != 2408 {
		t.Errorf("host 的端口该顶上，得到 %s:%d (%v)", ip, port, err)
	}
	// v4 自己带了真端口时以它为准
	if ip, port, err := warpEndpoint("162.159.192.7:2409", "engage.cloudflareclient.com:2408", []int{500}); err != nil ||
		ip != "162.159.192.7" || port != 2409 {
		t.Errorf("v4 自带端口该保留，得到 %s:%d (%v)", ip, port, err)
	}
	if _, port, err := warpEndpoint("162.159.192.7:0", "engage.cloudflareclient.com", []int{1701, 4500}); err != nil || port != 1701 {
		t.Errorf("该取 ports 第一个非零值，得到 %d (%v)", port, err)
	}
	if _, port, err := warpEndpoint("162.159.192.7", "", nil); err != nil || port != warpPort {
		t.Errorf("什么都没给时该退回 2408，得到 %d (%v)", port, err)
	}
	// 只有 IPv6 时不能硬编一个 v4 出来，宁可报"这台机器开不了中转"
	if _, _, err := warpEndpoint("", "[2606:4700:d0::a29f:c006]:0", []int{2408}); err == nil {
		t.Error("没有 IPv4 接入点却成功了")
	}
	if _, _, err := warpEndpoint("engage.cloudflareclient.com:2408", "", nil); err == nil {
		t.Error("接入点不是 IP 却成功了")
	}
}

func TestWarpKeyPairIsWireGuardCompatible(t *testing.T) {
	priv, pub, err := newWarpKeys()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	rawPriv, err := base64.StdEncoding.DecodeString(priv)
	if err != nil || len(rawPriv) != 32 {
		t.Fatalf("私钥不是 32 字节的 base64: %v (%d 字节)", err, len(rawPriv))
	}
	rawPub, err := base64.StdEncoding.DecodeString(pub)
	if err != nil || len(rawPub) != 32 {
		t.Fatalf("公钥不是 32 字节的 base64: %v (%d 字节)", err, len(rawPub))
	}
	// 内核 wg 会拿私钥自己算公钥，这里必须和交出去的那份一致，否则永远握不上手
	k, err := ecdh.X25519().NewPrivateKey(rawPriv)
	if err != nil {
		t.Fatalf("私钥内核不认: %v", err)
	}
	if got := base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()); got != pub {
		t.Errorf("私钥算出的公钥 %q 和交出去的 %q 不一致", got, pub)
	}
}

func TestWarpRegistrationRequestBody(t *testing.T) {
	req, err := newWarpRegReq("PUBKEY=", "")
	if err != nil {
		t.Fatalf("拼报文失败: %v", err)
	}
	blob, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("报文不是字符串对象: %v", err)
	}
	// 实测：多带 locale/tz/fmdash 之类字段会吃 400，只发这五个才通
	want := map[string]string{
		"key": "PUBKEY=", "version": warpClientVersion,
		"os": "linux", "type": "WireGuard",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q，想要 %q", k, got[k], v)
		}
	}
	if len(got) != 5 {
		t.Errorf("字段数 = %d，想要 5 个（多一个都可能被判无效注册）：%s", len(got), blob)
	}
	id, ok := got["install_id"]
	if !ok || len(id) != 16 || strings.TrimLeft(id, "0123456789abcdef") != "" {
		t.Errorf("install_id %q 不是 16 位小写十六进制", id)
	}
	// 每次调用都要新生成一个，别让所有出口共用一个装机标识
	other, _ := warpInstallID()
	if other == id {
		t.Error("装机标识两次一样，等于告诉对端是同一台机器")
	}
	if v, ok := warpVersionParts(req.Version); !ok || len(v) != 4 {
		t.Errorf("内置上报版本 %q 不是四段号，检查更新会误判", req.Version)
	}
}

func TestWarpSetupCommandsOrder(t *testing.T) {
	in := warpSetupInput{
		Dev: "fow12", KeyFile: "/var/lib/fanout/fow12.wgkey",
		PeerKey: warpPeerPub, Endpoint: "162.159.192.6", Port: 2408,
		TunIP: "172.16.0.2", MTU: 1420,
		VethGw: "10.99.12.1", Veth: "fop12",
		DirectIPs: []string{"8.8.8.8"},
	}
	cmds := warpSetupCmds(in)
	if len(cmds) != 7 {
		t.Fatalf("命令条数 = %d，想要 7 条：%v", len(cmds), cmds)
	}
	// 1~2：接入点和 DNS 先钉在直连侧。反过来先造设备，默认路由一换
	// Cloudflare 自己就进了隧道，WireGuard 外层包再也出不去。
	if got := strings.Join(cmds[0], " "); got != "ip route add 162.159.192.6/32 via 10.99.12.1 dev fop12" {
		t.Errorf("第一条不是接入点直连例外：%s", got)
	}
	if got := strings.Join(cmds[1], " "); got != "ip route add 8.8.8.8/32 via 10.99.12.1 dev fop12" {
		t.Errorf("第二条不是 nameserver 直连例外：%s", got)
	}
	// 最后一条必须是 replace（netns 里已有一条 via 母机的默认路由，add 会 File exists）
	last := strings.Join(cmds[len(cmds)-1], " ")
	if last != "ip route replace default dev fow12" {
		t.Errorf("最后一条 = %q，必须是把默认路由换成 WARP 设备", last)
	}
	// 中间几条按顺序：建设备 → 配地址 → 设密钥 → 启用
	if got := strings.Join(cmds[2], " "); !strings.Contains(got, "type wireguard mtu 1420") {
		t.Errorf("第三条不是建 WireGuard 设备：%s", got)
	}
	if got := strings.Join(cmds[3], " "); !strings.Contains(got, "172.16.0.2/32 dev fow12") {
		t.Errorf("第四条不是配隧道地址：%s", got)
	}
	wgset := strings.Join(cmds[4], " ")
	for _, want := range []string{"private-key /var/lib/fanout/fow12.wgkey", "peer " + warpPeerPub,
		"endpoint 162.159.192.6:2408", "allowed-ips 0.0.0.0/0",
		"persistent-keepalive " + strconv.Itoa(warpKeepalive)} {
		if !strings.Contains(wgset, want) {
			t.Errorf("wg set 缺 %q：%s", want, wgset)
		}
	}
	if got := strings.Join(cmds[5], " "); got != "ip link set dev fow12 up" {
		t.Errorf("第六条不是启用设备：%s", got)
	}
	// 默认路由换过去之前，直连例外必须已经在位
	for i, c := range cmds[:2] {
		if !strings.Contains(strings.Join(c, " "), "route add") {
			t.Errorf("第 %d 条之前不该动设备：%v", i+1, c)
		}
	}
}

func TestWarpSetupKeepsNonIPDirectOutOfRoutes(t *testing.T) {
	// resolv.conf 里出现域名或坏值时不能拼出 ip route add xxx/32，那会让整套装架失败
	in := warpSetupInput{Dev: "fow1", KeyFile: "k", PeerKey: warpPeerPub,
		Endpoint: "162.159.192.6", Port: 2408, TunIP: "172.16.0.2", MTU: 1420,
		VethGw: "10.1.1.1", Veth: "fop1", DirectIPs: []string{"fd00::1", "not-an-ip", "1.1.1.1"}}
	var routes []string
	for _, c := range warpSetupCmds(in) {
		if len(c) > 3 && c[1] == "route" && c[2] == "add" {
			routes = append(routes, strings.Join(c, " "))
		}
	}
	if len(routes) != 2 {
		t.Fatalf("直连例外条数 = %d，只该有接入点和 1.1.1.1：%v", len(routes), routes)
	}
	for _, r := range routes {
		if strings.Contains(r, "not-an-ip") || strings.Contains(r, "fd00") {
			t.Errorf("把不能下路由的地址塞进了命令：%s", r)
		}
	}
}

func TestWarpSplitPinsGateOntoWarp(t *testing.T) {
	cmds := warpSplitCmds("fow7", "tun0", []string{"61.215.116.134", "210.167.4.5"})
	if len(cmds) != 4 {
		t.Fatalf("条数 = %d，想要 2 条 Gate /32 + 2 条 /1：%v", len(cmds), cmds)
	}
	if got := strings.Join(cmds[0], " "); got != "ip route replace 61.215.116.134/32 dev fow7" {
		t.Errorf("Gate 地址没钉在 WARP 上：%s", got)
	}
	if got := strings.Join(cmds[2], " "); got != "ip route add 0.0.0.0/1 dev tun0" {
		t.Errorf("用户流量没接进隧道：%s", got)
	}
	if got := strings.Join(cmds[3], " "); got != "ip route add 128.0.0.0/1 dev tun0" {
		t.Errorf("用户流量只接了一半：%s", got)
	}
	// 顺序不能反：先两条 /1 的话 Gate 的控制通道会先被吞进 tun0，隧道自己吃自己
	for _, c := range cmds[:2] {
		if !strings.Contains(strings.Join(c, " "), "/32") {
			t.Errorf("前两条必须是 Gate 的 /32：%v", c)
		}
	}
	if got := warpSplitCmds("fow7", "tun0", []string{"bogus", "1.2.3.4"}); len(got) != 3 {
		t.Errorf("非 IP 的地址该跳过，得到 %v", got)
	}
	if got := warpSplitCmds("fow7", "tun0", nil); len(got) != 2 {
		t.Errorf("一个 Gate 地址都没有时仍要装分流，得到 %v", got)
	}
}

func TestWarpGateAddresses(t *testing.T) {
	cfg := strings.Join([]string{
		"client-config-sample by vpngate",
		"remote 61.215.116.134 1194 udp",
		"remote 61.215.116.134 443 tcp", // 同址不同端口，路由只该有一条
		"remote-entry gate.example.net 1194 udp",
		"# remote 203.0.113.9 1194 udp", // 注释掉的不能当真
		"server 198.18.0.7",
		"dev tun",
	}, "\n")
	hosts := warpGateHosts(cfg)
	want := "61.215.116.134 gate.example.net 198.18.0.7"
	if strings.Join(hosts, " ") != want {
		t.Errorf("warpGateHosts = %v，想要 [%s]", hosts, want)
	}
	got := warpSplitCmds("fow1", "tun0", warpGateIPs(cfg, "203.0.113.10"))
	var pinned []string
	for _, c := range got {
		if len(c) > 3 && c[1] == "route" && strings.HasSuffix(c[3], "/32") {
			pinned = append(pinned, c[3])
		}
	}
	// IPv4 字面量按序去重；注释里那个不算；CSV 的 IP 列兜底也钉上
	if strings.Join(pinned, " ") != "198.18.0.7/32 203.0.113.10/32 61.215.116.134/32" {
		t.Errorf("钉住的地址 = %v", pinned)
	}
	// 主机名要单独解析，否则拿不到 /32，Gate 就掉进隧道里
	names := warpGateNames(cfg)
	if len(names) != 1 || names[0] != "gate.example.net" {
		t.Errorf("warpGateNames = %v，想要只有一个主机名", names)
	}
	// 全是 IP 的配置不该产生解析动作（VPN Gate 大多数是这种）
	if n := warpGateNames("remote 1.2.3.4 1194 udp\n"); len(n) != 0 {
		t.Errorf("纯 IP 配置不该有主机名，得到 %v", n)
	}
	if got := warpGateIPs("", "203.0.113.9"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("配置里一个 IP 都没提时该用 CSV 的 IP 兜底，得到 %v", got)
	}
	if got := warpGateIPs("remote 2001:db8::1 1194 udp\n", ""); len(got) != 0 {
		t.Errorf("IPv6 不该下成 v4 路由，得到 %v", got)
	}
}

func TestWarpNameserversFromResolvConf(t *testing.T) {
	got := warpNameservers("# generated\nnameserver 8.8.8.8\nnameserver 8.8.8.8\nsearch lan\nnameserver 1.1.1.1\n")
	if strings.Join(got, " ") != "8.8.8.8 1.1.1.1" {
		t.Errorf("nameserver = %v，想要去重后的两个 IPv4", got)
	}
	if warpNameservers("nameserver fe80::1\n") != nil {
		t.Error("IPv6 不该被当成直连例外")
	}
}

func TestWarpOffChangesNothing(t *testing.T) {
	// 关掉中转时，参数、预算、MTU 必须回到没有这个功能时的值——
	// 这是"默认关闭 = 零回归"唯一的证据。
	wgMTU, tunMTU, mssfix := warpPathMTU(false)
	if wgMTU != 0 || tunMTU != 0 || mssfix != 1380 {
		t.Errorf("关闭时的 MTU = %d/%d/%d，想要 0/0/1380（openvpn 原默认）", wgMTU, tunMTU, mssfix)
	}
	if args := warpOpenVPNArgs(false); len(args) != 0 {
		t.Errorf("关闭时不该给 openvpn 加任何参数，得到 %v", args)
	}
	if b := warpBudget(45*time.Second, false); b != 45*time.Second {
		t.Errorf("关闭时预算 = %v，不该变", b)
	}
	onWG, onTun, onMSS := warpPathMTU(true)
	if onWG != 1420 || onTun != 1380 || onMSS != 1340 {
		t.Errorf("开启时 MTU = %d/%d/%d，想要 1420/1380/1340（两层封装）", onWG, onTun, onMSS)
	}
	if onMSS >= onTun || onTun >= onWG {
		t.Error("MTU 没有逐层递减，封装后会开始分片")
	}
	// --route-nopull 是整套分流的前提：不挡掉推送的 redirect-gateway，
	// tun0 会和 WARP 抢默认路由并撞 File exists
	args := strings.Join(warpOpenVPNArgs(true), " ")
	if !strings.Contains(args, "--route-nopull") || !strings.Contains(args, "--tun-mtu 1380") {
		t.Errorf("开启时的参数 = %q", args)
	}
	if b := warpBudget(45*time.Second, true); b != 45*time.Second+warpExtraBudget {
		t.Errorf("开启时预算 = %v，要多出 WARP 那一跳", b)
	}
}

func TestWarpDevNamesFitAndDiffer(t *testing.T) {
	seen := map[string]bool{}
	for _, tag := range []string{"", "a1", "zz9zz9"} {
		for slot := 1; slot <= 24; slot++ {
			dev := warpDevName(tag, slot)
			if len(dev) > 15 {
				t.Fatalf("设备名 %q 超过内核的 15 字符上限", dev)
			}
			if !strings.HasPrefix(dev, "fow") {
				t.Errorf("设备名 %q 认不出是 fanout 的，清理逻辑会漏掉它", dev)
			}
			if seen[dev] {
				t.Errorf("设备名 %q 撞了，两条出口会共用一个 WireGuard 接口", dev)
			}
			seen[dev] = true
		}
	}
	if warpKeyFileName("fow12") != "fow12.wgkey" {
		t.Error("私钥文件名和设备名对不上")
	}
}

func TestWarpLiveFromWgDump(t *testing.T) {
	pub := "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
	// 列序抄 wireguard-tools 的 dump_print：公钥、预共享、接入点、允许网段、
	// 最近握手、收、发、keepalive。认错列的表现是界面上那个 Cloudflare IP
	// 变成 "0.0.0.0/0"，或者在线状态永远是灰的。
	fresh := pub + "\t(none)\t162.159.193.5:2408\t0.0.0.0/0\t3521\t45210\t38991\t25"
	stale := pub + "\t(none)\t162.159.192.6:2408\t0.0.0.0/0,::/0\t120\t45210\t38991\toff"
	never := pub + "\t(none)\t(none)\t0.0.0.0/0\t0\t0\t0\t25"

	peers := parseWgDump(fresh + "\n")
	if len(peers) != 1 {
		t.Fatalf("没解析出 peer：%v", peers)
	}
	p := peers[0]
	if p.PubKey != pub || p.Handshake != 3521 || p.Endpoint != "162.159.193.5:2408" ||
		p.Rx != 45210 || p.Tx != 38991 || p.Keepalive != 25 {
		t.Errorf("解析结果不对：%+v", p)
	}
	if !warpLive(p, 3600, 1_777_000_000) {
		t.Error("开机秒尺子下的新鲜握手被判成不在线")
	}
	// 同一行换个尺子（对端给的是 epoch 秒）也必须判对，不能因为换算错就一直报绿
	epochPeer := warpPeer{PubKey: pub, Endpoint: "162.159.193.5:2408", Handshake: 1_777_000_000 - 30}
	if !warpLive(epochPeer, 3600, 1_777_000_000) {
		t.Error("epoch 尺子下的新鲜握手被判成不在线")
	}
	if warpLive(warpPeer{PubKey: pub, Endpoint: "1.2.3.4:2408", Handshake: 1_777_000_100}, 4000, 1_777_000_000) {
		t.Error("握手时间比现在还晚（时钟回拨/对表）却判在线")
	}
	// 越过保鲜期就该说"不新鲜"，不能继续报绿——用户是靠这个字段判断中转生没生效的
	if warpLive(p, 3600+int64(warpFreshAfter.Seconds()), 1_777_000_000) {
		t.Error("超过保鲜期仍判在线")
	}

	ep, live := warpLiveEndpoint(parseWgDump(fresh), 3600, 1_777_000_000)
	if !live || ep != "162.159.193.5:2408" {
		t.Errorf("新鲜握手 = %q/%v，想要在线且报出内核里真正在用的接入点", ep, live)
	}
	// 握手过期时仍然把"最后连的是哪个 Cloudflare IP"报出来，但 live=false，
	// 界面据此灰掉它，而不是假装还连着
	stalePeers := parseWgDump(stale + "\n")
	if len(stalePeers) != 1 {
		t.Fatalf("带两个网段的 allowed-ips 列没解析出来：%v", stalePeers)
	}
	ep, live = warpLiveEndpoint(stalePeers, 100000, 1_777_000_000)
	if live {
		t.Error("很久没握手还报在线")
	}
	if ep != "162.159.192.6:2408" {
		t.Errorf("过期时仍应报出上次的接入点，得到 %q", ep)
	}
	if stalePeers[0].Keepalive != 0 {
		t.Errorf(`keepalive 那列是 "off" 时应该是 0，得到 %d`, stalePeers[0].Keepalive)
	}
	if ep, live := warpLiveEndpoint(parseWgDump(never), 100000, 1_777_000_000); live || ep != "" {
		t.Errorf("从没握过手 = %q/%v，两个都该是空/假", ep, live)
	}
	if ep, live := warpLiveEndpoint(nil, 100000, 1_777_000_000); live || ep != "" {
		t.Error("设备不存在时不该有状态")
	}
	// 设备行（私钥/公钥/端口/掩码四列）、空行、报错行都不能被当成 peer
	noise := "\n\n(none)\t" + pub + "\t51820\toff\nwg: Permission denied\nnot\tenough\n" + pub + "\tshort\n"
	if n := len(parseWgDump(noise)); n != 0 {
		t.Errorf("把噪声解析成了 %d 条 peer：%v", n, parseWgDump(noise))
	}
}

func TestWarpHandshakeAgeHandlesBothRulers(t *testing.T) {
	const wall, up = int64(1_777_000_000), int64(40_000)
	cases := []struct {
		name string
		hs   int64
		age  int64
		ok   bool
	}{
		{"开机秒", up - 90, 90, true},
		{"epoch 秒", wall - 45, 45, true},
		{"从没握手", 0, 0, false},
		{"负数（时钟错乱）", -5, 0, false},
		{"在未来", wall + 60, 0, false},
	}
	for _, c := range cases {
		age, ok := warpHandshakeAge(c.hs, up, wall)
		if ok != c.ok || (ok && age != c.age) {
			t.Errorf("%s: warpHandshakeAge(%d, %d, %d) = %d/%v，想要 %d/%v",
				c.name, c.hs, up, wall, age, ok, c.age, c.ok)
		}
	}
	// 判断错了在线状态就全反：epoch 值被当成开机秒会得到一个巨大的"负年龄"，
	// 于是每条掉线的出口都显示为在线
	if _, ok := warpHandshakeAge(wall-5, up, wall); !ok {
		t.Error("epoch 时间戳没能认出来")
	}
}

func TestWarpVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"2026.8.2115.0", "2026.8.2115.0", false}, // 四段相等必须说"没更新"，否则会永远提示可升级
		{"2026.3.846.0", "2026.8.2115.0", true},
		{"2025.6.1335.0", "2026.3.846.0", true},
		{"2026.8.2115.0", "2026.8.2116.0", true},  // 第三段（build）
		{"2026.8.2115.1", "2026.8.2115.0", false}, // 第四段（revision）
		{"2026.8.2115", "2026.8.2115.0", false},   // 缺的段按 0 算
		{"2026.8.2115.0-1", "2026.8.2115.0", false},
		{"v2026.8.2115.0", "2026.8.2115.0", false},
		{"2024.4.133-1", "2026.8.2115.0", true}, // 老 Debian 带 -1 修订
	}
	for _, c := range cases {
		if got := warpVersionLess(c.a, c.b); got != c.less {
			t.Errorf("warpVersionLess(%q, %q) = %v，想要 %v", c.a, c.b, got, c.less)
		}
	}
	// 内置上报的号必须能当基准用
	if _, ok := warpVersionParts(warpClientVersion); !ok {
		t.Errorf("常量 warpClientVersion = %q 解析不出版本号", warpClientVersion)
	}
	for _, v := range []string{"", "latest", "2026.x.1"} {
		if _, ok := warpVersionParts(v); ok {
			t.Errorf("%q 不该被当成版本号", v)
		}
	}
}

func TestWarpPkgIndexParsing(t *testing.T) {
	body := strings.Join([]string{
		"Package: cloudflared",
		"Version: 2026.10.2",
		"Architecture: amd64",
		"",
		"Package: cloudflare-warp",
		"Version: 2026.8.2115.0",
		"Architecture: amd64",
		"",
	}, "\r\n")
	if got := parseWarpPkgVersion(body); got != "2026.8.2115.0" {
		t.Errorf("版本号 = %q，要 cloudflare-warp 那段的，不是别的包", got)
	}
	if got := parseWarpPkgVersion("Package: nginx\nVersion: 1.2.3\n"); got != "" {
		t.Errorf("索引里没有 cloudflare-warp 却返回 %q，会被当成版本号贴到界面上", got)
	}
	if got := parseWarpPkgVersion("<html>502 bad gateway</html>"); got != "" {
		t.Errorf("被劫持的响应不该有版本号，得到 %q", got)
	}
	// 基准不能落在老发行版上：bullseye/focal 常年落后，拿它比会说"已是最新"
	cn := parseWarpCodenames(warpCodenames)
	if len(cn) < 2 || cn[0] == "bullseye" || cn[0] == "focal" {
		t.Errorf("codename 顺序 = %v，第一项必须是当前发行版", cn)
	}
	if got := parseWarpCodenames("noble,, jammy ,"); len(got) != 2 || got[1] != "jammy" {
		t.Errorf("候选列表解析 = %v", got)
	}
}

func TestWarpStoreRoundTrip(t *testing.T) {
	accs := []WarpAccount{
		{Slot: 1, ID: "id1", Token: "t1", PrivKey: "p1", PubKey: "q1",
			PeerKey: warpPeerPub, Endpoint: "162.159.192.6", Port: 2408,
			TunIP: "172.16.0.2", Version: warpClientVersion, CreatedAt: "2026-11-24T03:04:05Z"},
		{Slot: 2, ID: "id2", Token: "t2", PrivKey: "p2", PubKey: "q2",
			PeerKey: warpPeerPub, Endpoint: "162.159.193.5", Port: 2408,
			TunIP: "172.16.0.2", Version: "2026.3.846.0"},
	}
	blob, err := renderWarpStore(accs)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	got, err := parseWarpStore(blob)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got) != 2 || got[0].ID != "id1" || got[1].Version != "2026.3.846.0" {
		t.Fatalf("读回的不是那两条：%+v", got)
	}
	if !json.Valid(blob) || !strings.Contains(string(blob), `"version"`) {
		t.Error("落盘格式没有版本号，以后字段变了没法认")
	}
	// 密钥必须留在文件里：丢了就得重新注册，同一条出口会在 Cloudflare 攒一串账号
	if !strings.Contains(string(blob), "p1") {
		t.Error("私钥没落盘")
	}
}

func TestWarpStoreDropsUnusableRecords(t *testing.T) {
	// 有 id 没私钥的半条记录最坏：上层以为"已经注册过了"而永远不再注册
	bad := `{"version":1,"accounts":[
	  {"slot":1,"id":"id1","token":"t","priv_key":"","peer_key":"k","endpoint":"1.2.3.4"},
	  {"slot":2,"id":"id2","token":"t","priv_key":"p","peer_key":"","endpoint":"1.2.3.4"},
	  {"slot":3,"id":"id3","token":"t","priv_key":"p","peer_key":"k","endpoint":""},
	  {"slot":4,"id":"id4","token":"t","priv_key":"p","peer_key":"k","endpoint":"1.2.3.4","port":0,"tun_ip":""}
	]}`
	got, err := parseWarpStore([]byte(bad))
	if err != nil {
		t.Fatalf("读盘失败: %v", err)
	}
	if len(got) != 1 || got[0].Slot != 4 {
		t.Fatalf("只该留下能用那条：%+v", got)
	}
	// 缺的端口/地址要补上默认值，否则 wg set 会拼出 endpoint 1.2.3.4:0
	if got[0].Port != warpPort || got[0].TunIP != warpTunIP {
		t.Errorf("默认值没补：port=%d tun=%q", got[0].Port, got[0].TunIP)
	}
	if _, err := parseWarpStore([]byte("{oops")); err == nil {
		t.Error("坏 JSON 该报错，让上层按空处理并留一行日志")
	}
	if _, err := renderWarpStore(nil); err != nil {
		t.Errorf("空列表不该写坏文件：%v", err)
	}
}

func TestWarpAccountEndpointIsIPOnly(t *testing.T) {
	// 存进记录的 Endpoint 必须不带端口：界面要把它当"当前连的是哪个 Cloudflare IP"
	// 直接显示，而 wg set 那步才是 JoinHostPort 拼出来的
	resp, err := parseWarpReg([]byte(warpRegFixture))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	acc, err := warpAccountOf(2, resp, "p", resp.Key, warpClientVersion, time.Now())
	if err != nil {
		t.Fatalf("生成记录失败: %v", err)
	}
	if strings.Contains(acc.Endpoint, ":") {
		t.Errorf("接入点里混进了端口：%q", acc.Endpoint)
	}
	if ip := net.ParseIP(acc.Endpoint); ip == nil || ip.To4() == nil {
		t.Errorf("接入点不是 IPv4：%q", acc.Endpoint)
	}
	if acc.Port <= 0 || acc.Port > 65535 {
		t.Errorf("端口非法：%d", acc.Port)
	}
}

// haveAllTools 是什么命令都有的那台机器：装包计划只该看发行版身份，
// 不该被"这台恰好没装 apt"影响。
func haveAllTools(string) bool { return true }

// 自动装依赖这件事现在写在程序里，所以计划本身要钉死：认错发行版就是在
// 人家机器上跑错的包管理器，漏掉非交互参数就是让命令停在"Do you want to
// continue? [Y/n]"上——界面表现为"装依赖中"卡死，而没人看得见那个提问。
func TestWireguardInstallPlanPerDistro(t *testing.T) {
	cases := []struct {
		name, osRel, family, last string
		have                      func(string) bool
	}{
		{"debian", "ID=debian\n", "deb", "apt-get install -y wireguard-tools", haveAllTools},
		{"ubuntu", "ID=ubuntu\nID_LIKE=debian\n", "deb", "apt-get install -y wireguard-tools", haveAllTools},
		// CentOS 7 只有 yum：计划里 dnf 排在前面是对的（RHEL 8+ 没 yum 了），
		// 所以这条要按"机器上真只有 yum"来测。
		{"centos7", "ID=centos\nID_LIKE=\"rhel centos fedora\"\n", "rpm", "yum -y install wireguard-tools",
			func(n string) bool { return n == "yum" }},
		{"rocky", "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n", "rpm", "dnf -y install wireguard-tools", haveAllTools},
		{"alpine", "ID=alpine\n", "alpine", "apk add --no-cache wireguard-tools", haveAllTools},
		{"arch", "ID=arch\n", "arch", "pacman -Sy --noconfirm --needed wireguard-tools", haveAllTools},
		{"suse", "ID=opensuse-leap\n", "suse", "zypper --non-interactive install -y wireguard-tools", haveAllTools},
	}
	for _, c := range cases {
		steps, family, err := wireguardInstallPlan(c.osRel, c.have)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if family != c.family {
			t.Errorf("%s: 家族认成 %q，想要 %q", c.name, family, c.family)
		}
		if len(steps) == 0 {
			t.Errorf("%s: 一条命令都没有", c.name)
			continue
		}
		last := strings.Join(steps[len(steps)-1], " ")
		if last != c.last {
			t.Errorf("%s: 装包那条是 %q，想要 %q", c.name, last, c.last)
		}
	}
}

// 只点 wireguard-tools 这一个包：顺手把机器上的包全升一遍会连带升内核、
// 重启无关服务，那不是"拨开一个开关"该做的事。
func TestWireguardInstallPlanDoesNotUpgradeEverything(t *testing.T) {
	for _, osRel := range []string{"ID=debian\n", "ID=fedora\n", "ID=arch\n", "ID=alpine\n"} {
		steps, _, err := wireguardInstallPlan(osRel, haveAllTools)
		if err != nil {
			t.Fatalf("%s: %v", osRel, err)
		}
		for _, argv := range steps {
			joined := strings.Join(argv, " ")
			if strings.Contains(joined, " upgrade ") || strings.Contains(joined, " full-upgrade") ||
				strings.HasSuffix(joined, " update") || strings.HasSuffix(joined, " -Sy") {
				t.Errorf("%s: 这条不像只装一个包：%q", osRel, joined)
			}
		}
	}
}

// 认得出发行版却没有对应的包管理器：报错要点名是哪一半缺，
// 不能让人以为 wg 命令本身坏了。
func TestWireguardInstallPlanMissingPkgManager(t *testing.T) {
	only := func(n string) bool { return n == "curl" }
	steps, family, err := wireguardInstallPlan("ID=debian\n", only)
	if err == nil {
		t.Fatal("没有 apt-get 却给出了装包计划")
	}
	if steps != nil {
		t.Errorf("失败时还返回了命令：%v", steps)
	}
	if family != "deb" {
		t.Errorf("家族应为 deb，得到 %q", family)
	}
	if !strings.Contains(err.Error(), "apt-get") || !strings.Contains(err.Error(), wireguardPkg) {
		t.Errorf("报错没说要装什么、缺什么：%q", err)
	}
}

// 认不出的发行版（OpenWrt 这类）要指回手动安装：这里最坏的表现是
// 界面说"会自动装"，而程序其实不知道该跑什么。
func TestWireguardInstallPlanUnknownDistro(t *testing.T) {
	steps, family, err := wireguardInstallPlan("ID=openwrt\nNAME=\"OpenWrt\"\n", haveAllTools)
	if err == nil {
		t.Fatal("OpenWrt 不该被当成能自动装")
	}
	if family != "" || steps != nil {
		t.Errorf("认不出时不该给出家族或命令：%q %v", family, steps)
	}
	if !strings.Contains(err.Error(), "手动安装") {
		t.Errorf("报错该留一句手动方案：%q", err)
	}
}

// 包管理器说"没有这个包"时那句报错是没用的：得补上最常见的下一步。
func TestWireguardInstallHint(t *testing.T) {
	cases := []struct {
		family, out, want string
		deny              string
	}{
		{"rpm", "Error: Unable to find a match: wireguard-tools", "epel", "apt-get"},
		{"deb", "E: Unable to locate package wireguard-tools", "apt-get update", "epel"},
		{"deb", "E: Package 'wireguard-tools' has no installation candidate", "apt-get update", ""},
		// 真故障不该被翻成"你去接个源"：那会让人把时间花在没坏的地方。
		{"deb", "dpkg: error processing archive /var/cache/apt/archives/x.deb", "", ""},
		{"rpm", "Segmentation fault (core dumped)", "", ""},
	}
	for _, c := range cases {
		got := wireguardInstallHint(c.family, c.out)
		if c.want != "" && !strings.Contains(strings.ToLower(got), c.want) {
			t.Errorf("%s/%q 该提示 %q，得到 %q", c.family, c.out, c.want, got)
		}
		if c.want == "" && got != "" {
			t.Errorf("%s/%q 不该提示什么，得到 %q", c.family, c.out, got)
		}
		if c.deny != "" && strings.Contains(got, c.deny) {
			t.Errorf("%s/%q 不该提到 %q：%q", c.family, c.out, c.deny, got)
		}
	}
}

// 包名各家族一致，所以计划里出现的包名必须是同一个常量：
// 写花一个字母，表现是"装完了却还是找不到 wg"，而那句话看着像程序的 bug。
func TestWireguardPlanUsesTheSamePackageName(t *testing.T) {
	for _, osRel := range []string{"ID=debian\n", "ID=fedora\n", "ID=alpine\n"} {
		steps, _, err := wireguardInstallPlan(osRel, haveAllTools)
		if err != nil {
			t.Fatalf("%s: %v", osRel, err)
		}
		if !strings.Contains(strings.Join(steps[len(steps)-1], " "), wireguardPkg) {
			t.Errorf("%s: 最后一条没装 %q：%v", osRel, wireguardPkg, steps)
		}
	}
}
