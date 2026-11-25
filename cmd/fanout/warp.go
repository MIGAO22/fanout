package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WARP 中转层的纯逻辑：向 Cloudflare 注册要发的报文、注册响应怎么解读、
// 在出口 netns 里该下哪几条命令、版本号怎么比。
//
// 这个文件只吃参数吐数据，不碰内核也不碰网络（真正执行命令和发 HTTP 的在
// warprun.go）。拆开是有原因的：fanout 的 netns 代码在非 Linux 上根本编不过，
// 而"默认路由换成 WARP"这种事一旦写错，表现是流量悄悄从母机出去——
// 用户看不出来，健康检查也看不出来（母机 IP 照样能出网）。
// 所以路由与命令的构造必须能单独跑断言，而不是只在真机上祈祷。

const (
	// warpAPIVer 是免费 WARP 注册接口的版本段。它不是客户端版本号，
	// 官方 warp-cli 和 wgcf 用的都是这一段；换段等于换一份接入配置修订。
	warpAPIVer = "v0a2317"
	// warpPeerPub 是 WireGuard peer 的公钥。正常情况下注册响应里就带着它，
	// 这份常量只在响应缺字段时兜底——Cloudflare 多年没换过这个密钥。
	warpPeerPub = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
	// warpTunIP 是 WARP 分配给客户端的隧道地址，官方客户端也固定用这一个。
	warpTunIP = "172.16.0.2"
	// warpPort 是 WireGuard 数据面端口。注册响应里 ports 还会给 500/1701/4500，
	// 那是 IPsec 和新版 MASQUE 用的，内核 WireGuard 只走 2408。
	warpPort = 2408
	// warpClientVersion 是注册时上报给 Cloudflare 的"客户端版本"。
	// 它决定对端下发哪一套接入配置，太老的号有被判过期的风险，
	// 所以跟着官方 apt 源的最新稳定号走（检查更新的基准见 warpPkgIndex）。
	warpClientVersion = "2026.8.2115.0"
	// warpPkgIndex 是 Cloudflare 官方 apt 源里 cloudflare-warp 的包索引。
	// 官方版本号直接写在里面，纯文本、读它不需要验签名，也不需要 GitHub
	// （cloudflare 没有发 WARP 客户端的 GitHub release，试过全是 404）。
	warpPkgIndex = "https://pkg.cloudflareclient.com/dists/%s/main/binary-amd64/Packages"
	// warpCodenames 按新旧排，取第一个问得到的。同一时刻各 codename
	// 一般是同一个号，但老发行版（bullseye/focal）会落后，不能拿它当基准。
	warpCodenames = "noble,bookworm,jammy,trixie"
	// warpKeepalive 是空载时保活探测的间隔（秒）。母机侧 MASQUERADE 会把
	// 长时间没包的 UDP 映射丢掉，没有保活就得等下一次连接重新握手。
	warpKeepalive = 25
	// warpExtraBudget 是开 WARP 之后单个节点多出来的预算：一次 WireGuard
	// 握手加上内核配设备的几条命令。不加这一段，节点会因为多走了一跳
	// 被判成"连不上"而白白换掉。
	warpExtraBudget = 12 * time.Second
	// warpFreshAfter 是"最近握手多久算还活着"。wg 的握手周期本身是
	// 静默 120 秒以上才会重新握手，所以这个值只要大于它就行。
	warpFreshAfter = 3 * time.Minute
)

// WarpAccount 是一条出口对应的 WARP 注册记录。
//
// 每个槽位一份独立密钥：几条出口共用一个注册的话，母机 conntrack 里
// "同一个五元组指向不同内层目的地"会互相踩，而且所有出口拿到的是
// 同一个 Cloudflare 出口 IP，中转就退化成"多绕一跳"而不是"多条独立线路"。
// 注册记录落盘复用：每次重连都重新注册等于反复给 Cloudflare 开新账号。
type WarpAccount struct {
	Slot      int    `json:"slot"`
	ID        string `json:"id"`    // Cloudflare 侧的注册 id
	Token     string `json:"token"` // 撤销注册时用
	PrivKey   string `json:"priv_key"`
	PubKey    string `json:"pub_key"`
	PeerKey   string `json:"peer_key"`
	Endpoint  string `json:"endpoint"` // 纯 IP，不含端口
	Port      int    `json:"port"`
	TunIP     string `json:"tun_ip"`
	Version   string `json:"version"` // 注册时上报的客户端版本
	CreatedAt string `json:"created_at"`
}

// devName 返回这条记录对应的 WireGuard 设备名（在出口自己的 netns 里）。
func (a *WarpAccount) devName() string { return warpDevName(instTag, a.Slot) }

// warpDevName 单独收 tag 做参数，是为了让"设备名会不会超长/撞名"这种
// 判断能在不带 netns 依赖的地方直接断言。网卡名上限 15 字符。
func warpDevName(tag string, slot int) string {
	return fmt.Sprintf("fow%s%d", tag, slot)
}

// warpKeyFileName 是 v2.0.11 及之前私钥落盘用的文件名，现在只剩"把老机器上留下的
// 私钥文件删掉"这一件事还认它——私钥本身改成了通过管道递给 wg（见 warpKeyStdin）。
func warpKeyFileName(dev string) string { return dev + ".wgkey" }

// warpKeyStdin 是喂给 wg 的"私钥文件路径"。
//
// `wg set … private-key <路径>` 里那个路径只是拿去 fopen 的，而 /proc/self/fd/0
// 就是我们接在子进程标准输入上的那根管道——于是私钥一个字都不必落到磁盘上。
//
// 换掉落盘的直接原因是现场那句 `wg set fow1: fopen: Permission denied`：
// fanout 自己以 root 写的 0600 文件，wg 却打不开（这台机器上有我们看不见的东西
// 在拦文件访问，SELinux、挂载选项、面板的安全模块都可能）。继续猜哪种都白猜，
// 少开一个文件就把这一整类都绕过去了，顺带也不再需要清理私钥残留。
const warpKeyStdin = "/proc/self/fd/0"

// warpKeyFor 决定这条命令要不要接私钥：只有 wg 那几条会去读它，
// ip link / ip addr / ip route 没必要拿到密钥，就别把它们写进那根管道。
func warpKeyFor(c []string, priv string) string {
	if len(c) > 0 && c[0] == "wg" {
		return priv + "\n"
	}
	return ""
}

// newWarpKeys 生成一对 X25519 密钥，base64 存下来。
// 用标准库而不是额外拉 wireguard-go：fanout 只想要那 32 字节密钥，
// 数据面交给内核，没必要再带一个用户态实现进来。
func newWarpKeys() (priv string, pub string, err error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("生成 WireGuard 密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key.Bytes()),
		base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

// warpInstallID 生成装机标识。Cloudflare 拿它区分设备，格式是 16 位十六进制；
// 每台出口各一个，别都用同一个，那等于告诉对端这些是同一台机器。
func warpInstallID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成装机标识失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// warpRegReq 是注册请求的报文。字段名是 Cloudflare 定的，
// 少任何一个都会被判成无效注册。
type warpRegReq struct {
	Key       string `json:"key"`
	InstallID string `json:"install_id"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Type      string `json:"type"`
}

// newWarpRegReq 拼一份注册请求。version 留空时用 fanout 内置那个号。
func newWarpRegReq(pub, version string) (warpRegReq, error) {
	id, err := warpInstallID()
	if err != nil {
		return warpRegReq{}, err
	}
	if strings.TrimSpace(version) == "" {
		version = warpClientVersion
	}
	return warpRegReq{
		Key: pub, InstallID: id, Version: version,
		OS: "linux", Type: "WireGuard",
	}, nil
}

// warpRegResp 是注册响应里 fanout 要用的那部分。
// 响应里还有 account/policy/services 一坨，都跟内核 WireGuard 无关，不接。
type warpRegResp struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Token  string `json:"token"`
	Config struct {
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4    string `json:"v4"`
				Host  string `json:"host"`
				Ports []int  `json:"ports"`
			} `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}

// parseWarpReg 解注册响应。只解得出 JSON 但缺关键字段也算失败——
// 半份配置拼出来的设备是"起来了但发不出去"，那种状态最难查。
func parseWarpReg(blob []byte) (*warpRegResp, error) {
	var r warpRegResp
	if err := json.Unmarshal(blob, &r); err != nil {
		return nil, fmt.Errorf("看不懂 Cloudflare 的注册响应: %w", err)
	}
	if r.ID == "" || r.Token == "" {
		return nil, fmt.Errorf("注册响应里没有 id 或 token")
	}
	if len(r.Config.Peers) == 0 {
		return nil, fmt.Errorf("注册响应里没有 WireGuard peer")
	}
	return &r, nil
}

// warpAccountOf 把注册响应换成落盘的记录。
func warpAccountOf(slot int, resp *warpRegResp, priv, pub, version string, now time.Time) (*WarpAccount, error) {
	peer := resp.Config.Peers[0]
	peerKey := strings.TrimSpace(peer.PublicKey)
	if peerKey == "" {
		peerKey = warpPeerPub // 响应没给就用官方公钥，这个值多年未变
	}
	ip, port, err := warpEndpoint(peer.Endpoint.V4, peer.Endpoint.Host, peer.Endpoint.Ports)
	if err != nil {
		return nil, err
	}
	tun := strings.TrimSpace(resp.Config.Interface.Addresses.V4)
	if tun == "" {
		tun = warpTunIP
	}
	if pub != "" && resp.Key != "" && pub != resp.Key {
		// 对端记住的公钥和我们要用的不是同一对，链路必然握不上手，
		// 与其让它在 netns 里静默黑洞，不如现在就报错。
		return nil, fmt.Errorf("注册响应的公钥与本机生成的不一致")
	}
	return &WarpAccount{
		Slot: slot, ID: resp.ID, Token: resp.Token,
		PrivKey: priv, PubKey: pub, PeerKey: peerKey,
		Endpoint: ip, Port: port, TunIP: tun,
		Version:   version,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

// warpEndpoint 从响应里挑出一个能用内核 WireGuard 直连的 IPv4 接入点。
//
// 响应给的是 "162.159.192.6:0" 这种端口为 0 的形式（端口在 host/ports 里另给），
// 所以取端口的顺序是：v4 自带的 → host 自带的 → ports 里第一个 → 2408。
// 只认 IPv4：出口 netns 里只有 IPv4 路由（见 netns.go），拿 IPv6 接入点配上去
// 只会得到一个永远握不上手的设备。
func warpEndpoint(v4, host string, ports []int) (string, int, error) {
	ip := strings.TrimSpace(v4)
	selfPort := 0
	if h, p, err := net.SplitHostPort(ip); err == nil {
		ip = h
		selfPort, _ = strconv.Atoi(p) // 实测这里给的是 "…:0"，0 表示没指定
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", 0, fmt.Errorf("注册响应里没有可用的 IPv4 接入点（%q）", v4)
	}
	port := 0
	for _, cand := range []int{selfPort, hostPort(host)} {
		if cand > 0 {
			port = cand
			break
		}
	}
	if port == 0 {
		for _, p := range ports {
			if p > 0 {
				port = p
				break
			}
		}
	}
	if port == 0 {
		port = warpPort
	}
	return parsed.String(), port, nil
}

// hostPort 取 "engage.cloudflareclient.com:2408" 里的端口，取不到返回 0。
func hostPort(host string) int {
	_, p, err := net.SplitHostPort(strings.TrimSpace(host))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return n
}

// warpPathMTU 返回开 WARP 之后该用的三层 MTU。
//
// under 是母机真正出网那块网卡的 MTU。写死 1500 是不够的：VPS 的默认出口
// 经常不是 1500（1450 的隧道、1400 的 overlay、拨号线的 1492），而 WireGuard
// 的外层包是带 DF 的——超了就静默丢掉，表现是"握手成了但什么都传不动"，
// 而 openvpn 最吃大包的一步正好是 TLS 握手。于是症状变成"所有节点都连不上"，
// 看着像节点死了，其实是我们把包发大了。
//
// 母机网卡按 1500 算时：WireGuard 外层要 8(UDP)+4(WireGuard 头)+20(IP)=32 字节，
// 所以设备给 1420；openvpn 的 UDP 包装再吃掉约 40，tun 只能到 1380。
// mssfix 再留一档余量，夹住 TCP；UDP 大包（QUIC 常见）靠 tun 的 1380 挡。
// 关掉 WARP 时返回的 tunMTU 是 0，意思是"照 openvpn 自己的默认来"，
// 和没这个功能时一字不差。
func warpPathMTU(on bool, under int) (wgMTU, tunMTU, mssfix int) {
	if !on {
		return 0, 0, 1380
	}
	if under <= 0 || under > 1500 {
		under = 1500 // 读不到、或离谱到不像话：按以前的 1500 算，行为不变
	}
	wg := under - 60
	if wg > 1420 {
		wg = 1420
	}
	if wg < 1280 {
		wg = 1280 // WireGuard 自己规定的最小 MTU，再小内核会直接拒绝配置设备
	}
	tun := wg - 40
	return wg, tun, tun - 40
}

// warpDefaultDev 从 /proc/net/route 里找默认路由（目的 00000000）走的那块网卡。
// 有多条默认路由时拿第一条——和内核一样，不做策略路由的猜测。
func warpDefaultDev(blob string) string {
	lines := strings.Split(blob, "\n")
	if len(lines) < 2 {
		return "" // 只有表头或空文件
	}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// warpUnderMTU 在 warprun.go（要读 /proc 与 /sys），这里只留纯函数。

// warpOpenVPNArgs 是开着 WARP 时要额外加给 openvpn 的参数。
//
// --route-nopull 是整套设计的前提：VPN Gate 的配置几乎都推 redirect-gateway，
// 让它照推就会拿 tun0 去抢 WARP 那条默认路由（两条默认路由还会互相撞
// File exists，openvpn 直接报路由失败）。关掉 pull 之后由 warpSplitCmds
// 自己装分流，链路才是确定的。
//
// --tun-mtu 必须一起给：不指定的话 tun0 是 1500，比 WireGuard 设备的
// 1420 还大，UDP 大包（QUIC 常见）会在外层被丢掉，表现是"网页能打、
// 语音一说话就卡"。TCP 有 mssfix 夹着，感觉不到。
//
// 开关关掉时返回空切片——非 WARP 那条路径一个参数都不加，
// 行为和没这个功能时一字不差。
//
// tunMTU 由调用方算好递进来（见 warpMTUs）：这个文件不碰 /proc，
// 而且同一个连接里三层 MTU 必须出自同一次读取，分两次读会读到不同的网卡 MTU。
func warpOpenVPNArgs(on bool, tunMTU int) []string {
	if !on {
		return nil
	}
	return []string{"--route-nopull", "--tun-mtu", strconv.Itoa(tunMTU)}
}

// warpPullFilters 挡住服务器推送的 MTU 相关选项。
//
// 必须挡的原因：openvpn 里**服务器推送的优先级高于命令行**，VPN Gate 上确实有节点
// 推 tun-mtu / mssfix。一被盖回去，外层就超过链路扛得住的大小，表现是"握手成了、
// 什么都传不动"——而 openvpn 最吃大包的 TLS 握手正好卡在这一步，于是每条出口、
// 每个候选节点都以超时收场，看着像 VPN Gate 全死了。
//
// --pull-filter 是 2.4 才有的选项，2.3 传上去会变成 Unrecognized option 直接退出，
// 所以按版本给（和数据通道算法那套同一个理由）。关掉中转时返回空切片。
//
// 参数名只有一个连字符的位置要说清楚：openvpn 里 `--pull` 是**不带任何参数**的选项，
// 过滤器是另一个选项 `--pull-filter <accept|ignore|reject> <名字前缀>`。
// 写成 `--pull filter ignore tun-mtu` 会被当成"pull 后面多带了三个参数"，
// openvpn 直接回 `Options error: Unrecognized option or missing or extra
// parameter(s) in [CMD-LINE]:1: pull`，进程当场退出——现场那句就是这个，
// 表现是"开了中转之后每条出口都连不上"，而 WARP 那一跳其实根本没被试过。
func warpPullFilters(on bool, ver string) []string {
	if !on {
		return nil
	}
	major, minor, ok := parseVersionParts(ver)
	if !ok || major < 2 || (major == 2 && minor < 4) {
		return nil
	}
	var out []string
	for _, k := range []string{"tun-mtu", "link-mtu", "mssfix"} {
		out = append(out, "--pull-filter", "ignore", k)
	}
	return out
}

// warpBudget 给单节点连接预算加上 WARP 那一跳的开销。
func warpBudget(base time.Duration, on bool) time.Duration {
	if !on {
		return base
	}
	return base + warpExtraBudget
}

// wireguardPkg 是给 `wg` 这条命令提供文件的包名。各家族的名字恰好一致
// （RHEL 系它在 EPEL 里），所以不用按发行版改名。
const wireguardPkg = "wireguard-tools"

// wireguardInstallPlan 把发行版身份换成"该跑哪几条命令把 wg 装上"。
//
// 只装工具、不碰内核：设备是内核建的（ip link add type wireguard），
// 内核真没有的话 netns 里那条命令的报错本来就写得很清楚，这里装什么都救不了。
// 也只点这一个包，不顺手把机器上的包全升一遍——那会连带升内核、重启无关服务。
func wireguardInstallPlan(osRelease string, have func(string) bool) ([][]string, string, error) {
	switch family := distroFamily(parseOSRelease(osRelease)); family {
	case "deb":
		if !have("apt-get") {
			return nil, "deb", fmt.Errorf("认出来是 Debian/Ubuntu 系，但机器上没有 apt-get，装不了 %s", wireguardPkg)
		}
		return [][]string{
			{"apt-get", "update", "-qq"},
			{"apt-get", "install", "-y", wireguardPkg},
		}, "deb", nil
	case "rpm":
		// RHEL 8+ 只有 dnf，CentOS 7 那批只有 yum，参数一致
		switch {
		case have("dnf"):
			return [][]string{{"dnf", "-y", "install", wireguardPkg}}, "rpm", nil
		case have("yum"):
			return [][]string{{"yum", "-y", "install", wireguardPkg}}, "rpm", nil
		}
		return nil, "rpm", fmt.Errorf("认出来是 RHEL/CentOS/Fedora 系，但机器上没有 dnf 或 yum，装不了 %s", wireguardPkg)
	case "alpine":
		if !have("apk") {
			return nil, "alpine", fmt.Errorf("认出来是 Alpine，但机器上没有 apk，装不了 %s", wireguardPkg)
		}
		// busybox 的 apk 没有 --needed 那套，--no-cache 免得把索引留在磁盘上
		return [][]string{{"apk", "add", "--no-cache", wireguardPkg}}, "alpine", nil
	case "arch":
		if !have("pacman") {
			return nil, "arch", fmt.Errorf("认出来是 Arch 系，但机器上没有 pacman，装不了 %s", wireguardPkg)
		}
		return [][]string{{"pacman", "-Sy", "--noconfirm", "--needed", wireguardPkg}}, "arch", nil
	case "suse":
		if !have("zypper") {
			return nil, "suse", fmt.Errorf("认出来是 openSUSE 系，但机器上没有 zypper，装不了 %s", wireguardPkg)
		}
		return [][]string{{"zypper", "--non-interactive", "install", "-y", wireguardPkg}}, "suse", nil
	}
	return nil, "", fmt.Errorf("认不出这台机器的发行版（%s 里没有已支持的 ID），装不了 %s：请手动安装后再拨开关",
		osReleasePath, wireguardPkg)
}

// wireguardInstallHint 给"包管理器说它没有这个包"补一句下一步。
//
// 包管理器在这件事上的报错是没用的：Unable to find a match: wireguard-tools
// 后面什么都不说，用户只能自己猜。而这里最常见的两种原因其实有固定解法。
func wireguardInstallHint(family, out string) string {
	s := strings.ToLower(out)
	switch family {
	case "rpm":
		if strings.Contains(s, "unable to find a match") || strings.Contains(s, "no package") ||
			strings.Contains(s, "nothing to do") {
			return "RHEL/CentOS 系通常是没接 EPEL：先跑 dnf install epel-release（CentOS 7 用 yum）再拨开关"
		}
	case "deb":
		if strings.Contains(s, "unable to locate package") || strings.Contains(s, "no installation candidate") {
			return "多半是 apt 索引太旧：在服务器上跑一次 apt-get update 再拨开关"
		}
	}
	return ""
}

// warpSetupInput 是配 WireGuard 需要知道的东西。
type warpSetupInput struct {
	Dev     string
	PeerKey string
	// 私钥不在这里：它经 warpKeyStdin 从管道递给 wg，不进命令行也不落盘。
	Endpoint string
	Port     int
	TunIP    string
	MTU      int
	// VethGw/Veth 是 netns 通往母机的那条链路（10.x.y.1 / fopN）。
	// Cloudflare 的接入点必须继续走它直连，否则 WireGuard 的外层包
	// 会被自己的默认路由送回 WireGuard，绕成一个环。
	VethGw string
	Veth   string
	// DirectIPs 是同样要留在直连侧的地址，实际就是 netns 里 resolv.conf
	// 那几个 nameserver：openvpn 连的节点是主机名时得先解析出来，
	// 而那一步发生在隧道里，没有直连的 DNS 就永远起不来。
	DirectIPs []string
}

// warpSetupCmds 生成 netns 里把 WARP 架起来的几条命令（不含 ip netns exec 前缀）。
//
// 顺序是有讲究的：先开直连例外，再造设备，最后 replace 默认路由。
// 反过来会把 Cloudflare 自己挡在隧道里，设备起了也握不上手。
// 默认路由用 replace 而不是 add：这一步会被健康检查反复补装（见 warpGuardCmds），
// add 第二次就回 File exists，于是"路由其实是对的"被当成失败。
func warpSetupCmds(in warpSetupInput) [][]string {
	cmds := [][]string{}
	for _, ip := range append([]string{in.Endpoint}, in.DirectIPs...) {
		// 只给 IPv4 下例外：netns 里那条 veth 是纯 IPv4 的（见 netns.go），
		// 给 IPv6 地址拼出 "ip route replace fd00::1/32 via 10.x.y.1" 会让整套装架失败，
		// 而失败的表现是这条出口连不上，看不出是 DNS 里混了个 v6。
		//
		// 用 replace 不用 add：setupNetns 已经把节点和 DNS 逐条 /32 放行过了
		// （见 netnsDirectCmds），这里再 add 同一条会撞 File exists，
		// 症状是"开了 WARP 之后每条出口都建不起来"。
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			continue
		}
		cmds = append(cmds, []string{"ip", "route", "replace", ip + "/32", "via", in.VethGw, "dev", in.Veth})
	}
	cmds = append(cmds,
		// mtu 必须排在 type 之前：`ip link add` 的通用选项（mtu/address/…）由
		// 前面的解析器吃，`type` 之后的尾巴全交给那个类型自己的解析器，而
		// wireguard 只认 private-key/peer 那几个。写成 `type wireguard mtu 1420`
		// 会被判成 "Garbage instead of arguments \"mtu ...\""，于是每台机器的
		// 每一条出口都在架设备这一步死掉——注册成功、包也装好了，症状却是"连不上节点"。
		[]string{"ip", "link", "add", "dev", in.Dev, "mtu", strconv.Itoa(in.MTU), "type", "wireguard"},
		[]string{"ip", "addr", "add", in.TunIP + "/32", "dev", in.Dev},
		[]string{"wg", "set", in.Dev,
			"private-key", warpKeyStdin,
			"peer", in.PeerKey,
			"endpoint", net.JoinHostPort(in.Endpoint, strconv.Itoa(in.Port)),
			"allowed-ips", "0.0.0.0/0",
			"persistent-keepalive", strconv.Itoa(warpKeepalive)},
		[]string{"ip", "link", "set", "dev", in.Dev, "up"},
		// 到这一步，netns 里除了上面那几条 /32 全都走 WARP。
		// WireGuard 没握手时这些包就是黑洞——要的就是这个效果：
		// 中转没成立时宁可连不上，也不能悄悄从母机出去。
		[]string{"ip", "route", "replace", "default", "dev", in.Dev})
	return cmds
}

// wireguardErrorHint 把配 WireGuard 时最常见的几句报错翻成一个下一步。
//
// 为什么要有它：`exit status 1` 后面那句 stderr 才是原因，可它要么长得看不懂
// （Operation not supported），要么在界面上被截断吃掉。用户能看到的只有
// "连不上节点"，于是每一轮都在猜。认不出来的原话一律返回空串——
// 宁可少说一句，也不要在界面上编一个原因出来。
func wireguardErrorHint(raw string) string {
	s := strings.ToLower(raw)
	switch {
	case strings.Contains(s, "garbage instead of arguments"):
		// 这句是工具自己的参数解析器说的，不是内核拒绝：mtu 写在了 type 后面（v2.0.9 那个坑）、
		// 或这台的 wireguard-tools 太老不认 persistent-keepalive，都长这样。
		return "这台机器上的 ip/wg 不认这套参数：升级 wireguard-tools 与 iproute2 后再拨开关"
	case strings.Contains(s, "unable to read key"), strings.Contains(s, "bad private key"),
		strings.Contains(s, "failed to open key file"):
		return "WireGuard 私钥没能递给 wg（它读标准输入这一步失败了）"
	case strings.Contains(s, "bad peer public key"):
		return "WARP 服务端公钥没被接受"
	case strings.Contains(s, "operation not supported"), strings.Contains(s, "not supported"):
		return "这台机器的内核没有 WireGuard：先 modprobe wireguard，装不上就要补内核模块或升内核"
	case strings.Contains(s, "cannot find device"), strings.Contains(s, "no such device"):
		return "WireGuard 设备没建起来"
	case strings.Contains(s, "permission denied"), strings.Contains(s, "operation not permitted"):
		return "权限不够：root 之外还有 SELinux、capability 这一层"
	case strings.Contains(s, "protocol error"), strings.Contains(s, "network is down"):
		return "wg 与内核版本对不上：升级 wireguard-tools"
	}
	return ""
}

// warpArgv 把一条架桥命令换成 exec 需要的 (程序, 参数)。
//
// 每条命令自己就带着程序名（`ip link add …`、`wg set …`），所以在母机上要照原样跑；
// 只有进出口 netns 时才多一层前缀 `ip netns exec <ns> <整条>`。
// v2.0.10 的自检在母机上又套了一次 ip，于是执行的是 `ip ip link add …`，
// iproute2 把第二个 ip 当成对象名，回 `Object "ip" is unknown, try "ip help".`——
// 自检自己先失败并把开关退回，所有出口反而一条都没试。
func warpArgv(ns string, c []string) (string, []string) {
	if ns == "" {
		return c[0], c[1:]
	}
	return "ip", append([]string{"netns", "exec", ns}, c...)
}

// warpCmdTag 是报错开头那句"哪一步"：只留前三个参数。
// 密钥、接入点、私钥文件路径都不进报错，否则命令回显会把内核那句挤到截断之外。
func warpCmdTag(ns string, c []string) string {
	head := c
	if len(head) > 3 {
		head = head[:3]
	}
	tag := strings.Join(head, " ")
	if ns != "" {
		tag = ns + "：" + tag
	}
	return tag
}

// warpProbeDev 是自检用的设备名：不碰路由、不碰 DNS，只问内核和 wg
// "这套参数你到底接不接受"。名字短且固定，方便在母机上认出残骸。
const warpProbeDev = "foarpprobe"

// warpProbeCmds 生成自检的命令（不含执行）。
//
// 为什么在母机 netns 里做而不是复用某条出口的 netns：要验的只有"建设备 + 配密钥"
// 这两步，路由一碰就牵扯到真出口。设备保持 DOWN、不加地址、不加路由，拆完就走，
// 母机的转发行为一个字都不变。
//
// cleanup 既在开头跑一次（清上次崩在这里留下的残骸，失败忽略），也在结尾跑一次
// ——自检失败更要收尾，否则母机上留一台名叫 foarpprobe 的设备，
// 下次来看的人不知道该不该删。
//
// 这套命令刻意不含 route：自检通过与否都不该改变任何一条流量的走向。
func warpProbeCmds(peerKey, endpoint string, port, mtu int) (setup [][]string, cleanup []string) {
	setup = [][]string{
		{"ip", "link", "add", "dev", warpProbeDev, "mtu", strconv.Itoa(mtu), "type", "wireguard"},
		{"wg", "set", warpProbeDev,
			"private-key", warpKeyStdin,
			"peer", peerKey,
			"endpoint", net.JoinHostPort(endpoint, strconv.Itoa(port)),
			"allowed-ips", "0.0.0.0/0",
			"persistent-keepalive", strconv.Itoa(warpKeepalive)},
	}
	return setup, []string{"ip", "link", "del", warpProbeDev}
}

// warpGatePinCmds 把节点（Gate）的地址钉在 WireGuard 设备上。
//
// 这两件事都靠它：开着中转时 openvpn 必须经 Cloudflare 才连得上节点；
// 而隧道没连着的时候 netns 里会被撤掉默认路由（见 warpGuardCmds），
// 那条 /32 就是 openvpn 唯一还能用的路——没有它，换节点时 openvpn 连新地址
// 都会 "Network is unreachable"，隧道永远起不来。
// 所以这批路由必须在 openvpn 启动之前就装好，不能等到 tun0 起来之后。
func warpGatePinCmds(dev string, gateIPs []string) [][]string {
	var cmds [][]string
	for _, ip := range gateIPs {
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			continue
		}
		cmds = append(cmds, []string{"ip", "route", "replace", ip + "/32", "dev", dev})
	}
	return cmds
}

// warpSplitCmds 生成 tun0 起来之后把用户流量接进 Gate 的命令。
//
// 两条 /1 合起来就是默认路由，但优先级低于任何 /32——这是 def1 那套写法：
// openvpn 推的 redirect-gateway 会被 --route-nopull 挡掉（见 startOpenVPN），
// Gate 自己的地址必须钉在 WARP 上，否则控制通道被路由进 tun0，
// 隧道自己吃自己。
func warpSplitCmds(dev, tun string, gateIPs []string) [][]string {
	cmds := warpGatePinCmds(dev, gateIPs)
	// 两条 /1 用 replace 而不是 add：这一步会被健康检查反复补装（见 warpGuardCmds），
	// add 第二次就回 File exists，于是"路由其实是对的"被当成失败。
	return append(cmds,
		[]string{"ip", "route", "replace", "0.0.0.0/1", "dev", tun},
		[]string{"ip", "route", "replace", "128.0.0.0/1", "dev", tun})
}

// warpGuardCmds 把一条 netns 的转发状态对齐到隧道的实际状态。
//
// 为什么需要它：分流路由是装在 tun0 上的，而 openvpn 每次自己重连（节点的端口没应答、
// 连接被掐、认证重试）都会重建 tun0——路由跟着接口一起没了，而 fanout 只在 tryNode
// 里装过一次。真机上量到的结果就是：netns 里只剩 `default dev fowN`，用户流量原路
// 走 Cloudflare 出去，SOCKS 照常能用，界面却说这条出口有问题。那种
// "看着在走节点、其实只走了中转" 比连不上更难查。
//
// 两个方向都要管：
//   - tun0 有地址：默认路由和两条 /1 都得在位（全用 replace，补装多少次都一样）
//   - tun0 没地址：默认路由必须撤掉。隧道没连着就不许有流量出去——宁可让客户端
//     立刻得到"连不上"，也不能让它静默换个 IP 上网。
func warpGuardCmds(dev, tun string, gateIPs []string, tunUp bool) [][]string {
	if !tunUp {
		return [][]string{{"ip", "route", "del", "default"}}
	}
	cmds := [][]string{{"ip", "route", "replace", "default", "dev", dev}}
	return append(cmds, warpSplitCmds(dev, tun, gateIPs)...)
}

// warpRouteState 是 `ip -o route show` 里能读出来的三件事。
type warpRouteState struct {
	TunUp    bool // tun0 有地址（内核那条直连路由在）
	SplitOK  bool // 两条 /1 都在 tun0 上
	DefOnDev bool // 默认路由走的是这台 WireGuard 设备
}

// parseWarpRoutes 解析路由表。用 `ip -o` 的输出（一条路由一行）。
//
// tun0 的判据是内核为它生成的那条 link 路由（`10.211.1.2 dev tun0 proto kernel scope link src …`），
// 接口一没地址这行就消失，正是我们要的信号。
func parseWarpRoutes(routeOut, dev string) warpRouteState {
	var st warpRouteState
	split := map[string]bool{}
	for _, line := range strings.Split(routeOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		devAt := -1
		for i, tok := range f {
			if tok == "dev" && i+1 < len(f) {
				devAt = i + 1
				break
			}
		}
		if devAt < 0 {
			continue
		}
		switch {
		case f[devAt] == "tun0" && has(f, "proto") && has(f, "kernel"):
			st.TunUp = true
		case f[devAt] == "tun0" && (f[0] == "0.0.0.0/1" || f[0] == "128.0.0.0/1"):
			split[f[0]] = true
		case f[0] == "default" && f[devAt] == dev:
			st.DefOnDev = true
		}
	}
	st.SplitOK = split["0.0.0.0/1"] && split["128.0.0.0/1"]
	return st
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// warpGuardNeeded 判断这条 netns 的路由要不要动。
//
// 隧道连着而分流不全 → 要补装；隧道没连着而默认路由还指着 WireGuard 设备 → 要切断。
// 其余情况一律不碰：健康检查每几十秒就会再来一遍，不需要每次都写路由。
func warpGuardNeeded(st warpRouteState) bool {
	if st.TunUp {
		return !st.SplitOK || !st.DefOnDev
	}
	return st.DefOnDev
}

// warpCloudflareV4 是 Cloudflare 名下的 IPv4 段。
//
// 来源有两层，都核对过：官方公告的 anycast 段（https://www.cloudflare.com/ips/，
// 页面最后更新 2023-09-28），以及 ARIN/RIPE/AFRINIC 上登记为 CLOUDFLARENET 的
// 分配段。这里按分配段写——公告那份把 104.16.0.0/12 拆成了 /13 加 /14，
// 于是 104.28.x 不在里面，而真机上漏出去的那两个出口 IP（104.28.247.68、
// 104.28.215.69）恰恰是 104.28.x：whois 查出来 NetName 就是 CLOUDFLARENET。
// 用公告那份会漏判，用分配这份不会。
//
// 判据的意义：WARP 的接入点和出口都在这几段里，所以"探测到的出口 IP 是 Cloudflare 的"
// 基本等价于"流量只走到了中转，没走节点"。误报的代价是换节点重连，
// 漏报的代价是客户端以为自己在日本、其实站在 Cloudflare 的数据中心——
// 这个方向上宁可多判。
var warpCloudflareV4 = []string{
	"104.16.0.0/12", "172.64.0.0/13", "162.158.0.0/15", "198.41.128.0/17",
	"108.162.192.0/18", "173.245.48.0/20", "141.101.64.0/18", "188.114.96.0/20",
	"190.93.240.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"131.0.72.0/22", "197.234.240.0/22",
}

// warpExitIsTransitOnly 说这个出口 IP 落在 Cloudflare 宣布的网段里。
// 认不出来（空串、非 IPv4）时返回 false——判据只在明确命中时才拦，
// 否则一个探测源抽风就能把好好的出口判死。
func warpExitIsTransitOnly(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return false
	}
	for _, c := range warpCloudflareV4 {
		if _, net4, err := net.ParseCIDR(c); err == nil && net4.Contains(parsed) {
			return true
		}
	}
	return false
}

// warpGateHosts 抠出配置里所有服务器地址，IP 和主机名都要，按出现顺序去重。
// VPN Gate 的配置一般直接写 IP（remote 1.2.3.4 1194 udp），少数写主机名。
func warpGateHosts(cfg string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		// 注释掉的 remote 不算：VPN Gate 的配置常把备用端口整行注掉，
		// 把它当真地址只会多钉一条没人用的路由。
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// remote-entry 的形式是 remote-entry <host> <port> <proto>，
		// 它和 remote 都会真的发起连接，两个都得认。
		switch f[0] {
		case "remote", "server", "remote-entry":
			add(f[1])
		}
	}
	return out
}

// warpGateIPs 返回其中能直接下路由的 IPv4 字面量。
// nodeIP 是 CSV 里那个 IP 列，配置里一个 IP 都没提时拿它兜底。
func warpGateIPs(cfg, nodeIP string) []string {
	var out []string
	for _, h := range append(warpGateHosts(cfg), nodeIP) {
		ip := net.ParseIP(h)
		if ip != nil && ip.To4() != nil {
			out = append(out, ip.String())
		}
	}
	return dedupeSorted(out)
}

// warpGateNames 返回配置里以主机名出现的服务器地址，是 warpGateIPs 的补集。
// VPN Gate 的 .ovpn 大多数直接写 IP，但确有节点写域名——那些不解析出来就没法
// 把它钉在 WARP 设备上，流量会绕进隧道再解析，链路死循环。
func warpGateNames(cfg string) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range warpGateHosts(cfg) {
		if net.ParseIP(h) != nil || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// dedupeSorted 去重并排序，让命令序列不随配置里的出现顺序变化——
// 落盘和日志里同一台机器应该每次长得一样。
func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// warpNameservers 从 resolv.conf 内容里取 nameserver 地址，
// 用于把 DNS 钉在直连侧（见 warpSetupInput.DirectIPs）。
func warpNameservers(resolv string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(resolv, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(f[1])
		if ip == nil || ip.To4() == nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		out = append(out, ip.String())
	}
	return out
}

// warpPeer 是 `wg show <dev> dump` 里的一行。
type warpPeer struct {
	PubKey    string
	Handshake int64 // 上次握手的时间戳，0 表示还没握过（可能是开机秒也可能是 epoch 秒）
	Endpoint  string
	Rx, Tx    int64
	Keepalive int
}

// up 表示这条 peer 已经握手过并且拿到了真实接入点。
// 没握手时 wg 打印的是 "0" 和 "(none)"。
func (p warpPeer) up() bool { return p.Handshake > 0 && p.Endpoint != "" }

// parseWgDump 解析 `wg show <dev> dump` 的 peer 行。
//
// 列序抄的是 wireguard-tools 的 dump_print（src/show.c）：
// 名称 自公钥 预共享密钥 接入点 允许的网段 最近握手 收 发 keepalive，
// 我们按设备名查，所以没有最前面那个接口名列。
// 认错列的后果是"永远显示不在线"或"把 0.0.0.0/0 当接入点贴到界面上"，
// 所以这里按列名一条条对齐，并靠公钥那列（定长 44 字符 base64）挡掉
// 设备行和其它噪声——设备行只有四列，将来万一形状变了也不会被当成 peer。
func parseWgDump(out string) []warpPeer {
	var peers []warpPeer
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || len(f[0]) != 44 {
			continue
		}
		hs, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil {
			hs = 0
		}
		rx, _ := strconv.ParseInt(f[5], 10, 64)
		tx, _ := strconv.ParseInt(f[6], 10, 64)
		ep := f[2]
		if ep == "(none)" {
			ep = ""
		}
		// keepalive 那列没开时打的是 "off"
		ka, _ := strconv.Atoi(f[7])
		peers = append(peers, warpPeer{PubKey: f[0], Handshake: hs, Endpoint: ep, Rx: rx, Tx: tx, Keepalive: ka})
	}
	return peers
}

// warpHandshakeAge 把 dump 里那列握手时间换算成"多少秒之前"。
//
// 为什么要两把尺子：wg 打印的是内核直接给的秒数，而它从什么时候算起并不统一——
// 内核态的 WireGuard 给的是开机秒（CLOCK_BOOTTIME），用户态实现和某些版本转换后
// 给的是 Unix 时间戳。钉死一种的下场是"永远判成在线"或"永远判成掉线"，
// 而这两种在界面上都看不出来。数值落在哪一段就说明它是哪种尺子量的。
// 时间戳在未来（对表过、时钟回拨）时返回 false：宁可少报一个在线，
// 也不能把没连上的链路说成连着。
func warpHandshakeAge(hs, nowUptime, nowWall int64) (int64, bool) {
	switch {
	case hs <= 0:
		return 0, false
	case hs <= nowUptime:
		return nowUptime - hs, true
	case hs <= nowWall:
		return nowWall - hs, true
	default:
		return 0, false
	}
}

// warpLive 判断 dump 出来的 peer 现在算不算还连著。
func warpLive(p warpPeer, nowUptime, nowWall int64) bool {
	if !p.up() {
		return false
	}
	age, ok := warpHandshakeAge(p.Handshake, nowUptime, nowWall)
	return ok && age <= int64(warpFreshAfter.Seconds())
}

// warpLiveEndpoint 从 dump 里挑出当前真正在用的接入点。
// 一台设备只配了一个 peer，取第一个活着的即可；都不活时退回那个"有握手记录"的，
// 界面上仍要说清最后一次连的是哪个 Cloudflare IP。
func warpLiveEndpoint(peers []warpPeer, nowUptime, nowWall int64) (string, bool) {
	for _, p := range peers {
		if warpLive(p, nowUptime, nowWall) {
			return p.Endpoint, true
		}
	}
	for _, p := range peers {
		if p.up() {
			return p.Endpoint, false
		}
	}
	return "", false
}

// warpVersionLess 比较 2026.8.2115.0 这种点分号。
//
// 不能复用 update.go 的 versionLess：parseSemver 明确只认三段，四段一律算解析失败，
// 退回到"不相等就当有更新"，于是每次检查更新都会提示可以升级 WARP。
// Cloudflare 的号是四段（年.月.Build.Revision），所以这里要自己按段比。
func warpVersionLess(a, b string) bool {
	as, aok := warpVersionParts(a)
	bs, bok := warpVersionParts(b)
	if !aok || !bok {
		return strings.TrimSpace(a) != strings.TrimSpace(b)
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// warpVersionParts 把版本号拆成数字段。带非数字后缀（2026.8.2115.0-1）的先截掉。
func warpVersionParts(v string) ([]int, bool) {
	v = plainVersion(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// parseWarpPkgVersion 从 apt 的 Packages 索引里读 cloudflare-warp 的版本号。
// 索引是纯文本段落，只取第一条 Package: cloudflare-warp 之后的 Version:。
func parseWarpPkgVersion(body string) string {
	inWarp := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "Package:"):
			inWarp = strings.TrimSpace(strings.TrimPrefix(line, "Package:")) == "cloudflare-warp"
		case inWarp && strings.HasPrefix(line, "Version:"):
			return strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		}
	}
	return ""
}

// parseWarpCodenames 把 warpCodenames 常量拆成候选列表。
func parseWarpCodenames(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// warpStore 是 WARP 注册的落盘形态。版本号留着，是因为这份记录会被
// 以后版本读到，字段变了要能认出来（而不是把残缺记录当成可用的账号）。
type warpStore struct {
	Version  int           `json:"version"`
	Accounts []WarpAccount `json:"accounts"`
}

const warpStoreVersion = 1

func renderWarpStore(accs []WarpAccount) ([]byte, error) {
	if accs == nil {
		accs = []WarpAccount{}
	}
	return json.MarshalIndent(warpStore{Version: warpStoreVersion, Accounts: accs}, "", "  ")
}

// parseWarpStore 读盘，顺手丢掉不可用的条目。
// 半条记录（有 id 没私钥）留在这儿只会让上层以为"已经注册过了"而永远不再注册。
func parseWarpStore(blob []byte) ([]WarpAccount, error) {
	var st warpStore
	if err := json.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("解析 WARP 注册记录失败: %w", err)
	}
	out := make([]WarpAccount, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		if a.ID == "" || a.PrivKey == "" || a.Endpoint == "" || a.PeerKey == "" {
			continue
		}
		if a.Port <= 0 {
			a.Port = warpPort
		}
		if a.TunIP == "" {
			a.TunIP = warpTunIP
		}
		out = append(out, a)
	}
	return out, nil
}
