package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// WebSettings 是落盘的可改配置：管理界面的监听端口与地址，外加几个功能开关。
// 界面改完即时生效。访问口令与访问路径各有专门的文件（password / basepath），
// 不放这里，但都能在设置面板里改。
type WebSettings struct {
	// Port 是管理界面监听端口。
	Port int `json:"port"`
	// ListenAddr 是监听地址：空或 0.0.0.0 表示所有网卡；127.0.0.1 表示只本机。
	ListenAddr string `json:"listen_addr"`
	// ResidentialOnly 决定挑节点时是否只用志愿者家宽，跳过 vpngate 自营机房。
	// 用指针是为了区分"没配过"和"明确关掉"：老版本升上来的配置文件里没有这个
	// 字段，nil 按默认的开启处理。
	ResidentialOnly *bool `json:"residential_only,omitempty"`
	// SubToken 是订阅地址里的口令。订阅要免登录才能被客户端拉取，
	// 所以这串就是它唯一的门槛，等同于密码，不要外传。
	SubToken string `json:"sub_token,omitempty"`
	// Warp 是"出口流量先过一层 Cloudflare WARP"。用指针的理由和 ResidentialOnly
	// 一样：老配置文件里没这个字段，nil 表示没配过（按关闭处理）。
	Warp *WarpSettings `json:"warp,omitempty"`
	// Conn 是连接层的可调参数：健康检查节奏、重连退避、面板写入合并之类。
	// 这些原来全是写死的常量，但 VPN Gate 的质量随地区和时间差很多，
	// 同一套阈值在美国 VPS 和家用宽带上不可能都合适，只能交给使用者。
	Conn ConnSettings `json:"conn,omitempty"`
}

// ConnSettings 是连接层的调参入口。全部以秒为单位（面板写入合并用毫秒），
// 零值/越界一律回落到默认值，见 normalized。
type ConnSettings struct {
	// HealthInterval 是健康检查的轮询间隔（秒）。
	HealthInterval int `json:"health_interval,omitempty"`
	// HealthFailures 是连续失败几次才判掉线。
	HealthFailures int `json:"health_failures,omitempty"`
	// HealthTimeout 是单次探测的超时（秒）。
	HealthTimeout int `json:"health_timeout,omitempty"`
	// BackoffMin / BackoffMax 是自动重连的退避区间（秒）。
	BackoffMin int `json:"backoff_min,omitempty"`
	BackoffMax int `json:"backoff_max,omitempty"`
	// ConnectBudget 是单个节点从拉起 openvpn 到拿到出口 IP 的总预算（秒）。
	// 超过就判这个节点连不上、换下一个候选——家宽节点冷启动可能很慢，
	// 但让一条出口在同一个坏节点上磨几分钟，不如早点换人。
	ConnectBudget int `json:"connect_budget,omitempty"`
	// MaxTries 是一轮里最多试几个候选节点。
	MaxTries int `json:"max_tries,omitempty"`
	// PanelDebounce 是面板配置写入的合并窗口（毫秒）。
	// 批量开 N 条出口时，每条 up 都会请求重建配置；不合并的话
	// 自建模式会连着重启 N 次 Xray，已连上的客户端被反复打断。
	PanelDebounce int `json:"panel_debounce,omitempty"`
	// NodeTTL 是节点列表的缓存时长（秒）。
	// VPN Gate 那份 CSV 有几十 MB，多条隧道同时重连时不该各拉各的。
	NodeTTL int `json:"node_ttl,omitempty"`
	// ScoreMode 决定挑节点时看什么：speed 只看速度、idle 偏爱没人用的、
	// ping 只看延迟、balanced 综合这三样。
	ScoreMode string `json:"score_mode,omitempty"`
	// ReconnectLimit 是一轮健康检查里最多同时重连几条出口。
	// VPN Gate 的机房常常整段一起掉线，全放开了重连等于自己打自己。
	ReconnectLimit int `json:"reconnect_limit,omitempty"`
}

// 连接层默认参数。改这里要同步改 web/app.js 里输入框的占位默认值。
const (
	defHealthInterval = 10
	defHealthFailures = 3
	defHealthTimeout  = 6
	defBackoffMin     = 5
	defBackoffMax     = 60
	defConnectBudget  = 35
	defMaxTries       = 6
	defPanelDebounce  = 1500
	defNodeTTL        = 600
	defScoreMode      = "balanced"
	defReconnectLimit = 2
)

// normalized 把零值和离谱值夹回可用区间。
// 界面上填错不该让程序退化成"每 0 秒全量重连"这种自伤行为。
func (c ConnSettings) normalized() ConnSettings {
	clamp := func(v, def, lo, hi int) int {
		if v <= 0 {
			return def
		}
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	c.HealthInterval = clamp(c.HealthInterval, defHealthInterval, 3, 3600)
	c.HealthFailures = clamp(c.HealthFailures, defHealthFailures, 1, 20)
	c.HealthTimeout = clamp(c.HealthTimeout, defHealthTimeout, 2, 120)
	c.BackoffMin = clamp(c.BackoffMin, defBackoffMin, 1, 600)
	c.BackoffMax = clamp(c.BackoffMax, defBackoffMax, c.BackoffMin, 3600)
	c.ConnectBudget = clamp(c.ConnectBudget, defConnectBudget, 10, 600)
	c.MaxTries = clamp(c.MaxTries, defMaxTries, 1, 30)
	c.PanelDebounce = clamp(c.PanelDebounce, defPanelDebounce, 0, 60000)
	c.NodeTTL = clamp(c.NodeTTL, defNodeTTL, 30, 86400)
	c.ReconnectLimit = clamp(c.ReconnectLimit, defReconnectLimit, 1, 16)
	switch c.ScoreMode {
	case "speed", "idle", "ping", "balanced":
	default:
		c.ScoreMode = defScoreMode
	}
	return c
}

// connConf 取当前连接层参数（已夹过界）。
func connConf() ConnSettings { return getWebSettings().Conn.normalized() }

// WarpSettings 是 WARP 中转的配置。
type WarpSettings struct {
	// Enabled 是总开关。开着之后每条出口的 netns 里默认路由进 Cloudflare，
	// 用户流量再经 openvpn 送到节点——即"母机 → WARP → 出口节点"。
	Enabled bool `json:"enabled"`
	// ClientVersion 是注册时上报给 Cloudflare 的客户端版本号，空=fanout 内置那个。
	// 对端按这个号下发接入配置，官方发了新版而这里还报旧号，
	// 有被判成过期客户端、拿不到接入点的风险。
	ClientVersion string `json:"client_version,omitempty"`
}

// normalized 把填歪的版本号退回内置值。上报一个不像版本号的字符串没有任何意义，
// 只会让 Cloudflare 那边的下发结果变得无法预期。
func (w WarpSettings) normalized() WarpSettings {
	if _, ok := warpVersionParts(w.ClientVersion); !ok {
		w.ClientVersion = ""
	}
	return w
}

// warpConf 取当前 WARP 配置（已夹过界）。
func warpConf() WarpSettings { return getWebSettings().warp().normalized() }

// warpOn 返回中转开关是否打开。没配过时是关闭：这层链路会把出口
// 吊在 Cloudflare 上，没经过用户同意不该默认替他改路由。
func warpOn() bool { return warpConf().Enabled }

// warpReportVersion 返回注册时该上报的版本号。
func warpReportVersion() string {
	if v := warpConf().ClientVersion; v != "" {
		return v
	}
	return warpClientVersion
}

// setWarp 改 WARP 配置并落盘。改版本号不影响已经注册好的账号——
// 想按新号重新注册要走内置客户端那张卡的"更新"。
func setWarp(w WarpSettings) error {
	w = w.normalized()
	webSettingsMu.Lock()
	webSettingsCur.Warp = &w
	webSettingsMu.Unlock()
	return saveWebSettings()
}

// warp 取出 WARP 配置。指针为 nil 表示老的设置文件里根本没这一项，
// 按零值（关闭 + 用内置版本号）处理。
func (s WebSettings) warp() WarpSettings {
	if s.Warp == nil {
		return WarpSettings{}
	}
	return *s.Warp
}

// residentialOnly 返回"只用家宽"是否开启。没配过时默认开：
// fanout 存在的意义就是把家宽扇成出口，机房 IP 对用户没价值。
func (s WebSettings) residentialOnly() bool {
	if s.ResidentialOnly == nil {
		return true
	}
	return *s.ResidentialOnly
}

// residentialOnly 是给业务代码用的简写，省掉每处都去取一遍设置。
func residentialOnly() bool { return getWebSettings().residentialOnly() }

// setResidentialOnly 改"只用家宽"开关并落盘。
func setResidentialOnly(v bool) error {
	webSettingsMu.Lock()
	webSettingsCur.ResidentialOnly = &v
	webSettingsMu.Unlock()
	return saveWebSettings()
}

// setConn 改连接层参数并落盘。写入前先夹一遍界，
// 免得界面填了个 0 就把健康检查间隔存成 0 秒。
func setConn(c ConnSettings) error {
	c = c.normalized()
	webSettingsMu.Lock()
	webSettingsCur.Conn = c
	webSettingsMu.Unlock()
	return saveWebSettings()
}

// connWake 让长跑循环立刻察觉参数变了：健康检查原来用 time.Tick，
// 改了间隔要等下一轮才生效，最坏得等一个旧间隔。
var connWake = make(chan struct{}, 1)

func notifyConnChange() {
	select {
	case connWake <- struct{}{}:
	default: // 已经有一次唤醒在排队，不必重复
	}
}

var (
	webSettingsMu   sync.RWMutex
	webSettingsCur  WebSettings
	webSettingsPath string
)

func webSettingsFilePath(dir string) string { return filepath.Join(dir, "settings.json") }

// loadWebSettings 读盘并返回当前配置。
//
// portExplicit 表示用户在命令行显式给了 -web。界面上改过端口之后会落盘，
// 之前这里一律以盘上为准，导致再带 -web 启动会被静默忽略——用户敲了参数却
// 连不上，也没有任何提示。显式指定时以命令行为准并写回，让参数说话算话。
func loadWebSettings(dir string, defaultPort int, portExplicit bool) (WebSettings, error) {
	webSettingsPath = webSettingsFilePath(dir)

	s := WebSettings{Port: defaultPort, ListenAddr: ""}
	blob, err := os.ReadFile(webSettingsPath)
	switch {
	case os.IsNotExist(err):
		webSettingsMu.Lock()
		webSettingsCur = s
		webSettingsMu.Unlock()
		return s, saveWebSettings()
	case err != nil:
		return s, err
	}
	if err := json.Unmarshal(blob, &s); err != nil {
		return s, err
	}
	if s.Port == 0 {
		s.Port = defaultPort
	}
	changed := false
	if portExplicit && s.Port != defaultPort {
		s.Port = defaultPort
		changed = true
	}
	webSettingsMu.Lock()
	webSettingsCur = s
	webSettingsMu.Unlock()
	if changed {
		return s, saveWebSettings()
	}
	return s, nil
}

func getWebSettings() WebSettings {
	webSettingsMu.RLock()
	defer webSettingsMu.RUnlock()
	return webSettingsCur
}

func saveWebSettings() error {
	webSettingsMu.RLock()
	blob, err := json.MarshalIndent(webSettingsCur, "", "  ")
	webSettingsMu.RUnlock()
	if err != nil {
		return err
	}
	tmp := webSettingsPath + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, webSettingsPath)
}

// normalizeListenAddr 把用户填的监听地址规整成合法值：空 / 0.0.0.0 / 127.0.0.1 / 具体 IP。
func normalizeListenAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == "0.0.0.0" || strings.EqualFold(addr, "all") {
		return "", nil
	}
	if ip := net.ParseIP(addr); ip != nil {
		return addr, nil
	}
	return "", fmt.Errorf("监听地址必须是合法 IP，或留空表示所有网卡")
}

// validatePort 校验端口范围。
func validatePort(p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("端口必须在 1-65535 之间")
	}
	return nil
}

// listenAddrString 拼出 net.Listen 用的地址串。
func (s WebSettings) listenAddrString() string {
	return net.JoinHostPort(s.ListenAddr, strconv.Itoa(s.Port))
}
