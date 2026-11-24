package main

import (
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const vpngateAPI = "https://www.vpngate.net/api/iphone/"

// vpngateMirror 是直连拿不到节点列表时的兜底（Cloudflare Worker 反代）。
// 用 FANOUT_VPNGATE_MIRROR 可以换成自己的地址，设成空字符串就只走直连。
const vpngateMirror = "https://p.xy.kg/vpngate"

// mirrorKey 只是让反代不被爬虫和端口扫描白嫖，不是安全边界。
const mirrorKey = "8rhIFzFKRJMFAe-xP5OQPclDEvSjKlHo"

func mirrorURL() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_MIRROR"); ok {
		return strings.TrimSpace(v)
	}
	return vpngateMirror
}

func mirrorAccessKey() string {
	if v, ok := os.LookupEnv("FANOUT_VPNGATE_MIRROR_KEY"); ok {
		return strings.TrimSpace(v)
	}
	return mirrorKey
}

// Node 是一个 VPN Gate 节点。
type Node struct {
	HostName    string  `json:"hostname"`
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Ping        int     `json:"ping"`
	SpeedMbps   float64 `json:"speed_mbps"`
	Sessions    int     `json:"sessions"`
	Config      string  `json:"-"` // 解码后的 .ovpn 内容
	// Residential 表示这是志愿者的家庭宽带，不是 VPN Gate 自己的机房服务器。
	Residential bool `json:"residential"`
}

// vpngate 自营服务器所在的网段。这些是学术网络里的机房机器，
// 不是志愿者家宽：出口 IP 一眼能看出来是数据中心，而且更容易满员。
var vpngateOwnNets = []string{"219.100.37.0/24"}

// isResidential 判断一个节点是不是志愿者家宽。
//
// 两条判据都是实测出来的：
//   - hostname 以 public-vpn- 开头的是 vpngate 自营服务器
//   - 219.100.37.0/24 段同样是自营（学术网络机房）
//
// 剩下的是志愿者用自家宽带跑的，出口 IP 属于普通运营商。
func isResidential(hostName, ip string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(hostName)), "public-vpn-") {
		return false
	}
	addr := net.ParseIP(strings.TrimSpace(ip))
	if addr == nil {
		// IP 解析不出来时不敢断言，按家宽放过，让连通性测试去淘汰
		return true
	}
	for _, cidr := range vpngateOwnNets {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil && block.Contains(addr) {
			return false
		}
	}
	return true
}

// fetchNodes 拉取并解析 VPN Gate 的节点列表。
// 先直连；连不上或者拿回来的内容不对（被拦截、返回门户页）就换反代再试一次。
// 返回的列表已按速度降序排列。
func fetchNodes(timeout time.Duration) ([]Node, error) {
	return fetchNodesWith(vpngateAPI, timeout)
}

// fetchNodesWith 把直连地址拆成参数，方便测试两条分支。
func fetchNodesWith(direct string, timeout time.Duration) ([]Node, error) {
	nodes, err := fetchNodesFrom(direct, "", timeout)
	if err == nil {
		return nodes, nil
	}
	mirror := mirrorURL()
	if mirror == "" {
		return nil, err
	}
	nodes, mirrorErr := fetchNodesFrom(mirror, mirrorAccessKey(), timeout)
	if mirrorErr != nil {
		return nil, fmt.Errorf("直连失败(%v)；反代也失败: %w", err, mirrorErr)
	}
	return nodes, nil
}

func fetchNodesFrom(url, key string, timeout time.Duration) ([]Node, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("拉取节点列表失败: %w", err)
	}
	if key != "" {
		req.Header.Set("X-Fanout-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取节点列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取节点列表失败: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取节点列表失败: %w", err)
	}
	return parseNodeCSV(string(raw))
}

// parseNodeCSV 解析 VPN Gate 的 CSV。首行是 "*vpn_servers"，
// 第二行是以 '#' 开头的表头，末行是 "*"。
func parseNodeCSV(body string) ([]Node, error) {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		kept = append(kept, strings.TrimPrefix(line, "#"))
	}
	if len(kept) < 2 {
		return nil, fmt.Errorf("节点列表格式异常: 有效行不足")
	}

	r := csv.NewReader(strings.NewReader(strings.Join(kept, "\n")))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析节点 CSV 失败: %w", err)
	}

	header := records[0]
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	need := []string{"HostName", "IP", "CountryLong", "CountryShort", "Ping", "Speed", "OpenVPN_ConfigData_Base64"}
	for _, k := range need {
		if _, ok := idx[k]; !ok {
			return nil, fmt.Errorf("节点列表缺少字段 %s", k)
		}
	}

	var nodes []Node
	for _, rec := range records[1:] {
		get := func(k string) string {
			i := idx[k]
			if i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		cfgB64 := get("OpenVPN_ConfigData_Base64")
		if cfgB64 == "" || get("HostName") == "" {
			continue
		}
		cfg, err := base64.StdEncoding.DecodeString(cfgB64)
		if err != nil {
			continue
		}
		ping, _ := strconv.Atoi(get("Ping"))
		speed, _ := strconv.ParseFloat(get("Speed"), 64)
		sessions, _ := strconv.Atoi(get("NumVpnSessions"))
		nodes = append(nodes, Node{
			HostName:    get("HostName"),
			IP:          get("IP"),
			Residential: isResidential(get("HostName"), get("IP")),
			Country:     get("CountryLong"),
			CountryCode: get("CountryShort"),
			Ping:        ping,
			SpeedMbps:   speed / 1e6,
			Sessions:    sessions,
			Config:      string(cfg),
		})
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("节点列表为空")
	}
	scoreNodes(nodes, connConf().ScoreMode)
	return nodes, nil
}

// nodeScore 给一个节点打分，分高者优先挑到。
//
// 只用 SpeedMbps 排序是不够的，那列数字有两个毛病：
//   - 它是 VPN Gate 别人跑过的历史测速，新上线的节点常常是 0，
//     按它排序等于把"没测过"当成"最慢"，好节点被压到最后。
//   - 它完全不反映此刻这个节点有多挤。NumVpnSessions 才是实时负载，
//     志愿者家宽挂了 30 个会话时实测速度能掉到标称值的零头。
//
// Ping 是到 VPN Gate 日本主站的延迟，不是到本机的，只能当相对参考，
// 所以权重给得比负载还低。
func nodeScore(n Node, mode string) float64 {
	speed := 0.25 // 没测过速：不给 0，也不给满分
	if n.SpeedMbps > 0 {
		// 饱和曲线：40 Mbps 记半分。线性归一会让 900 和 300 差出三倍，
		// 而家宽出口真正拉开体验的是"够不够用"，不是绝对带宽。
		speed = n.SpeedMbps / (n.SpeedMbps + 40)
	}
	// 会话数 0 表示空着，是好事；50 个以上基本挤满了
	load := 1.0
	if n.Sessions > 0 {
		s := float64(n.Sessions)
		if s > 50 {
			s = 50
		}
		load = 1 - s/50
	}
	ping := 0.5 // 没测过：当中游
	if n.Ping > 0 {
		p := float64(n.Ping)
		if p > 600 {
			p = 600
		}
		ping = 1 - p/600
	}

	var ws, wl, wp float64
	switch mode {
	case "speed":
		ws, wl, wp = 0.80, 0.10, 0.10
	case "idle":
		ws, wl, wp = 0.10, 0.55, 0.35
	case "ping":
		ws, wl, wp = 0.10, 0.20, 0.70
	default: // balanced
		ws, wl, wp = 0.45, 0.30, 0.25
	}
	return ws*speed + wl*load + wp*ping
}

// scoreNodes 按策略就地排序。稳定排序，同分的保持列表原有顺序。
func scoreNodes(nodes []Node, mode string) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodeScore(nodes[i], mode) > nodeScore(nodes[j], mode)
	})
}
