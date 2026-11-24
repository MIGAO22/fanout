package main

import (
	"sync"
	"time"
)

// ExitInbound 是挂在某个出口上的一个 3x-ui 入站。
type ExitInbound struct {
	ID       int    `json:"id"`
	Port     int    `json:"port"`
	Remark   string `json:"remark"`
	Protocol string `json:"protocol"`
	Enable   bool   `json:"enable"`
	Tag      string `json:"tag"`
}

// Exit 是界面上的一行：一条隧道加上挂在它出口的所有入站。
// 用户脑子里的单位是"一个出口"，不是"一条隧道"和"一个入站"两样东西。
type Exit struct {
	Slot    int       `json:"slot"`
	Port    int       `json:"port"` // SOCKS5 端口
	Host    string    `json:"host"`
	Region  string    `json:"region"`
	Country string    `json:"country"`
	ExitIP  string    `json:"exit_ip"`
	Status  string    `json:"status"`
	Err     string    `json:"err,omitempty"`
	Since   time.Time `json:"since"`
	// SOCKS5 凭据：界面要能看、能复制、能改
	SocksUser string        `json:"socks_user"`
	SocksPass string        `json:"socks_pass"`
	Inbounds  []ExitInbound `json:"inbounds"`
	// WarpIP 是这条出口此刻真正连着的 Cloudflare 接入点，WarpLive 表示
	// WireGuard 握手还新鲜。两个都是空/假值时界面不显示这一项——
	// 没开中转本来就不该有话要说。
	//
	// 注意它和 ExitIP 是两件事：ExitIP 是流量的终点（节点看到的来源），
	// WarpIP 只是中间那一跳。开中转后 ExitIP 仍然是节点的出口，不变。
	WarpIP   string `json:"warp_ip,omitempty"`
	WarpLive bool   `json:"warp_live"`
	// Aead 说这条出口当前绑的节点在配置里自己声明了 AEAD，且没硬要开压缩——
	// 也就是设置里那个 AEAD-only 开关对它真的有用。界面只给 true 的那几条挂标记：
	// VPN Gate 那批 SoftEther 生成的配置写死 AES-128-CBC，全铺标记等于没有标记。
	Aead bool `json:"aead"`
}

// WarpOverview 是中转开关本身的状态，显示在出口总览的头部。
type WarpOverview struct {
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`  // 这台机器有没有条件跑中转
	Reason  string `json:"reason"` // 没条件时的原话，界面直接印出来
	// NeedTools 说清"Ready 是怎么来的"：wg 还没装，但包管理器认得出来，
	// 拨开开关时第一步就把它装上。界面据此把"不可用"换成一句会动手的说明。
	NeedTools bool   `json:"need_tools"`
	PkgCmd    string `json:"pkg_cmd"` // 将执行的那几条装包命令，摊开给用户看
	// LastErr 是上一次"翻开开关却没成功"的原话。开关现在是关着的，就说明
	// 这件事还没解决——它只待在作业里会在几分钟后消失，然后所有人重新报告
	// 的都只是"连不上节点"。
	LastErr string `json:"last_err,omitempty"`
	Version string `json:"version"` // 注册时上报给 Cloudflare 的客户端版本
	Count   int    `json:"count"`   // 已注册的 WARP 账号数
	Live    int    `json:"live"`    // 其中此刻握手新鲜的出口数
	Stale   int    `json:"stale"`   // 按旧客户端版本注册的账号数，非 0 说明"更新"没做完
}

// PoolMemberView 是组内一条出口的界面视图。
type PoolMemberView struct {
	Host      string `json:"host"`
	Slot      int    `json:"slot"`
	Status    string `json:"status"`
	ExitIP    string `json:"exit_ip"`
	LatencyMs int64  `json:"latency_ms"` // 0 表示还没有样本
	Cooling   bool   `json:"cooling"`
	// Aead 同 Exit.Aead：组员那条隧道的节点吃不吃 AEAD-only，只进 chip 的悬浮说明。
	Aead bool `json:"aead"`
}

// PoolView 是一个出口组在界面上的一行。
type PoolView struct {
	ID        string           `json:"id"`
	Identity  string           `json:"identity"` // 入站绑定串用的就是它
	Name      string           `json:"name"`
	Mode      string           `json:"mode"`
	ModeLabel string           `json:"mode_label"`
	ModeDesc  string           `json:"mode_desc"`
	RRStep    int              `json:"rr_step"`
	Port      int              `json:"port"`
	Running   bool             `json:"running"`
	Up        int              `json:"up"`
	Total     int              `json:"total"`
	SocksUser string           `json:"socks_user"`
	SocksPass string           `json:"socks_pass"`
	Members   []PoolMemberView `json:"members"`
	Inbounds  []ExitInbound    `json:"inbounds"`
	// AutoFill 开了就有 Size 这个要维持的可用成员数；FillNote 是还没补齐时的原因。
	AutoFill bool   `json:"auto_fill"`
	Size     int    `json:"size"`
	FillNote string `json:"fill_note,omitempty"`
}

// ExitsView 是主界面需要的全部数据。
type ExitsView struct {
	Exits []Exit `json:"exits"`
	// Direct 是没绑到任何出口的入站，仍然要能看见，否则用户会以为它们不见了
	Direct []ExitInbound `json:"direct"`
	// Pools 是出口组，排在单出口之后；没有组时是空数组而不是 null，
	// 前端每几秒轮询一次，省得它为 null 特判。
	Pools []PoolView `json:"pools"`
	Panel string     `json:"panel"` // 面板不可用时的原因，空表示正常
	// Backend 是 "3x-ui" 或 "native"。界面据此决定是否提供新建入站入口：
	// 接管面板时入站归面板管，自建模式才由 fanout 自己建。
	Backend string `json:"backend"`
	// PanelInfo 是后端的一行说明，显示在标题旁
	PanelInfo string `json:"panel_info"`
	// PublicIP 是母机公网 IPv4，前端用它当 SOCKS5/分享链接的连接地址
	PublicIP string `json:"public_ip"`
	// Warp 是中转开关的状态。这个接口每几秒就被轮询一次，所以它只能由
	// 本地状态算出来：里面没有任何网络请求，读 WireGuard 那几步都有缓存。
	Warp WarpOverview `json:"warp"`
}

// inboundCache 给入站列表做很短的缓存。界面每几秒轮询一次，
// 而每次读入站都要顺带解析一遍完整的 Xray 配置，没必要每次都真的去问面板。
type inboundCache struct {
	mu   sync.Mutex
	at   time.Time
	list []Inbound
	err  error
}

const inboundCacheTTL = 2500 * time.Millisecond

var ibCache inboundCache

func cachedInbounds(live map[string]bool) ([]Inbound, error) {
	ibCache.mu.Lock()
	defer ibCache.mu.Unlock()
	if time.Since(ibCache.at) < inboundCacheTTL {
		return ibCache.list, ibCache.err
	}

	var list []Inbound
	x, err := openPanel()
	if err == nil {
		list, err = x.Inbounds(live)
	}
	ibCache.at, ibCache.list, ibCache.err = time.Now(), list, err
	return list, err
}

// invalidateInbounds 在写操作之后调用，让下一次读立刻反映改动。
func invalidateInbounds() {
	ibCache.mu.Lock()
	ibCache.at = time.Time{}
	ibCache.mu.Unlock()
}

// ExitsOf 把隧道和入站 join 成界面直接可用的形态。
func (m *Manager) ExitsOf() ExitsView {
	tunnels := m.Tunnels()
	view := ExitsView{Exits: make([]Exit, 0, len(tunnels)), PublicIP: hostPublicIP()}

	// 先填后端类型：入站读取失败时界面仍要知道当前是哪种模式
	if p, err := openPanel(); err == nil {
		view.Backend = p.Kind()
		view.PanelInfo = p.Describe()
	}

	live := map[string]bool{}
	for _, t := range tunnels {
		if t.status() == "up" {
			live[sanitizeTag(t.host())] = true
		}
	}

	byHost := map[string]int{}
	for i, t := range tunnels {
		// 一次取一份一致性快照：分开读 Status/ExitIP/Node 会撞上重连过程中的
		// 中间态，界面于是偶尔显示"已连通但没有出口 IP"这种自相矛盾的行。
		st := t.state()
		byHost[sanitizeTag(st.Node.HostName)] = i
		cred := t.credential()
		view.Exits = append(view.Exits, Exit{
			Slot: t.Slot, Port: st.Port, Host: st.Node.HostName,
			Region: st.Node.CountryCode, Country: nodeLabel(st.Node),
			ExitIP: st.ExitIP, Status: st.Status, Err: st.Err, Since: st.Since,
			SocksUser: cred.User, SocksPass: cred.Pass,
			Aead: nodeSupportsAEAD(st.Node.Config),
		})
	}

	// 中转开着才去读 WireGuard 实况：关掉时那是一次都不该付的空 fork。
	// 读的是内核里真正在用的接入点，而不是注册时存下的那个（见 warpStatus）。
	view.Warp = warpOverview()
	if view.Warp.Enabled {
		for i, t := range tunnels {
			if view.Exits[i].Status != "up" {
				continue
			}
			st := warpStatus(t.nsName(), warpDevName(instTag, t.Slot))
			view.Exits[i].WarpIP = st.Endpoint
			view.Exits[i].WarpLive = st.Live
			if st.Live {
				view.Warp.Live++
			}
		}
	}

	view.Pools = poolViews(tunnels)
	byPool := map[string]int{}
	for i, pv := range view.Pools {
		byPool[pv.Identity] = i
	}
	// 组的可用性写进同一张 live 表：入站的"绑定是否在线"对组和隧道是一个含义
	mergePoolLive(live)

	list, err := cachedInbounds(live)
	if err != nil {
		view.Panel = err.Error()
		return view
	}

	for _, ib := range list {
		row := ExitInbound{
			ID: ib.ID, Port: ib.Port, Remark: ib.Remark,
			Protocol: ib.Protocol, Enable: ib.Enable, Tag: ib.Tag,
		}
		if i, ok := byHost[ib.BoundTo]; ib.BoundTo != "" && ok {
			view.Exits[i].Inbounds = append(view.Exits[i].Inbounds, row)
			continue
		}
		if i, ok := byPool[ib.BoundTo]; ib.BoundTo != "" && ok {
			view.Pools[i].Inbounds = append(view.Pools[i].Inbounds, row)
			continue
		}
		view.Direct = append(view.Direct, row)
	}
	return view
}

// poolViews 把出口组连同此刻的组员状态摊给界面。
//
// 组里的成员已经换了节点（主机名对不上任何隧道）时仍然列出来，标成 unknown：
// 悄悄从界面上少一条，用户只会以为组变弱了，看不出是改名导致的。
func poolViews(tunnels []*Tunnel) []PoolView {
	if poolsSvc == nil {
		return []PoolView{}
	}
	byHost := map[string]*Tunnel{}
	for _, t := range tunnels {
		byHost[t.host()] = t
	}

	out := make([]PoolView, 0, len(poolsSvc.pools()))
	notes := poolsSvc.fillNotes()
	for _, p := range poolsSvc.pools() {
		pv := PoolView{
			ID: p.ID, Identity: p.identity(), Name: p.Name, Mode: p.Mode,
			ModeLabel: poolModeLabels[p.Mode], ModeDesc: poolModeDesc[p.Mode],
			RRStep: p.RRStep, Port: p.Port,
			SocksUser: p.Cred.User, SocksPass: p.Cred.Pass,
			AutoFill: p.AutoFill, Size: p.Size, FillNote: notes[p.ID],
		}
		live := map[string]bool{}
		for _, e := range egressPools() {
			if e.Identity == pv.Identity {
				pv.Running, pv.Up = true, e.Up
			}
		}
		for _, m := range poolsSvc.resolveMembers(p.ID) {
			pv.Total++
			if m.Up {
				live[m.Host] = true
			}
			status := "missing"
			aead := false
			if t := byHost[m.Host]; t != nil {
				status = t.status()
				aead = nodeSupportsAEAD(t.node().Config)
			}
			pv.Members = append(pv.Members, PoolMemberView{
				Host: m.Host, Slot: m.Slot, Status: status, ExitIP: m.ExitIP,
				LatencyMs: m.Latency.Milliseconds(), Cooling: m.Cooling, Aead: aead,
			})
		}
		out = append(out, pv)
	}
	return out
}
