package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 出口组：一条节点链接同时绑多个出口，按策略在组内选路，目的是保活。
//
// VPN Gate 的家宽隧道说掉就掉、说卡就卡，单绑一条时客户端只能跟着遭殃。
// 组内还有别的出口可用时，这件事不该让客户端感知到。
//
// 实现方式：每个组是一条只监听 127.0.0.1 的 SOCKS5（见 poolsvc.go）。
// Xray 侧看到的仍然是一个普通 socks 出站，组内怎么选、失败怎么降级、
// 要不要并发抢跑全在 fanout 这一层，所以 3x-ui / 自建 / xray-cf-lite
// 三种后端都不用为这个功能改配置格式。

// 四种策略。
const (
	// poolModeSingle 单一出口：整组粘在同一条上，只有它不通才换下一条。
	poolModeSingle = "single"
	// poolModeRoundRobin 轮询：每 RRStep 个新目标换一条，循环整组。
	poolModeRoundRobin = "roundrobin"
	// poolModeBalance 负载均衡：按实测拨号延迟挑最优，当前这条明显变差就切走。
	// 起点不固定——没有样本的成员会被优先拿去实测，跑几轮自然选出快的。
	poolModeBalance = "balance"
	// poolModeConcurrent 并发：一次访问同时向组内所有可用出口发起，谁先通用谁。
	poolModeConcurrent = "concurrent"
)

// poolTagPrefix 是出口组在绑定串与 outboundTag 里的身份前缀。
//
// 绑定串平时装的是 sanitizeTag(节点主机名)，两者必须能区分开。
// VPN Gate 的主机名形如 vpn01.xxx.ac.jp，sanitizeTag 后不会以 pool- 开头。
const poolTagPrefix = "pool-"

const (
	poolDefaultRRStep = 5
	poolMaxRRStep     = 1000

	// poolTryTimeout 是组内单条成员的拨号预算。比单出口那条的 15 秒短得多：
	// 保活靠的就是快速降级，光等第一条就等满 15 秒等于没做。
	poolTryTimeout = 8 * time.Second

	// poolAffinityMax 限制目标粘性表的规模。键是目标域名/IP（不含端口），
	// 一个正常客户端一天就能划出上千个目标，不设上限会一直涨。
	poolAffinityMax = 4096

	// balance 判定"粘性这条已经不值得粘"的门槛：既要比当前最优慢一倍，
	// 又要绝对值超过 floor。只比倍数的话，80ms 和 30ms 的差异就会来回切，
	// 而这两种体感都一样，切了反而多一次 IP 变更。
	poolSwitchFloor = 600 * time.Millisecond
	poolSwitchRatio = 2

	// poolEWMAAlpha 是延迟样本的平滑系数。取 0.3：单个毛刺不足以把一条
	// 成员踢出优先位，但连续变慢三轮就能反映出来。
	poolEWMAAlpha = 0.3

	// poolCoolBase / poolCoolMax 是拨号失败后的冷却时长，按失败次数指数放大。
	// 冷却只影响"优先选谁"，不影响"要不要试它"——全组都在冷却时照样会挨个试，
	// 否则一次网络抖动就能让整个组永远起不来。
	poolCoolBase = 10 * time.Second
	poolCoolMax  = 3 * time.Minute

	// poolProbeInterval / poolProbeStale 是后台测速的节奏：
	// balance 模式要是只靠真实流量取样，闲半小时后所有样本都过期了，
	// 下一批连接就得重新踩。
	poolProbeInterval = 20 * time.Second
	poolProbeStale    = 2 * time.Minute

	// ── 自动补齐（AutoFill）───────────────────────────────

	// poolFillInterval 是补位巡检的节奏。比测速慢一倍是有意的：补位要起
	// openvpn 进程、占槽位，属于这条链路上最贵的动作，不该跟着 UI 一起急。
	poolFillInterval = 30 * time.Second

	// poolFillDeadAfter 是"这条成员放弃了吗"的等待门槛。
	//
	// fanout 本来就会自动换同区节点重连，VPN Gate 的掉线绝大多数几分钟内自愈。
	// 一掉就补会和那条路径打架：两条出口同时抢一个地区，补出来的那条
	// 其实根本不需要。等满这段时间还起不来，才认定这个位置真空了。
	poolFillDeadAfter = 5 * time.Minute

	// poolFillSlotFloor 是补位必须留下的槽位余量。
	// 自动补位是唯一会"自己开出口"的路径，不加水位的话某组连崩几轮
	// 就能把 maxSlots 吃光，用户手动开出口反倒开不了。
	poolFillSlotFloor = 2

	// poolFillPenalty 是某个节点补位失败的冷却：这段时间内不再拿它当补位候选。
	// 没有这层，每轮巡检都会从头再撞一遍同一批死节点。
	poolFillPenalty = 10 * time.Minute

	// poolMaxSize 与成员上限一致：目标数超过成员上限没有意义。
	poolMaxSize = 32
)

// probeTarget 是测速用的拨号地址。用 IP 而不是域名：测的是隧道到公网的
// 往返，不该把 DNS 的时间算进来；1.1.1.1 是 anycast，任何出口过去都够近。
var probeTarget = "1.1.1.1:443"

// poolModes 是界面下拉与校验共用的顺序。
var poolModes = []string{poolModeSingle, poolModeRoundRobin, poolModeBalance, poolModeConcurrent}

var poolModeLabels = map[string]string{
	poolModeSingle:     "单一出口",
	poolModeRoundRobin: "轮询出口",
	poolModeBalance:    "负载均衡出口",
	poolModeConcurrent: "并发出口",
}

// poolModeDesc 给界面当提示语，说清各自会怎么换出口。
var poolModeDesc = map[string]string{
	poolModeSingle:     "固定走一条，不通才换组内下一条",
	poolModeRoundRobin: "每 N 个新目标换一条，循环整组",
	poolModeBalance:    "按实测延迟挑最快的，变卡自动切走",
	poolModeConcurrent: "同时向全组发起，谁先通用谁（占多路会话）",
}

func validPoolMode(m string) bool {
	for _, v := range poolModes {
		if v == m {
			return true
		}
	}
	return false
}

// Pool 是一个出口组。
type Pool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
	// Hosts 是成员出口的节点主机名（原样，与 Panel.Bind 的 hostname 同形）。
	// 用主机名而不是槽位：槽位重启后会重新分配，拿它当身份会让组员悄悄换人。
	Hosts  []string  `json:"hosts"`
	RRStep int       `json:"rr_step"`
	Port   int       `json:"port"`
	Cred   SocksCred `json:"cred"`
	// AutoFill 打开后，某个成员长时间连不上就自动起一条新出口顶替它。
	// 关闭时一切照旧：组员只有用户自己动得了。零值是关闭，旧 pools.json 不用迁移。
	AutoFill bool `json:"auto_fill"`
	// Size 是要维持的可用成员数。开开关那一刻取当前成员数，之后恒定——
	// 不然「补回原来的数量」这个目标会随掉线一起缩水，越补越少。
	Size int `json:"size"`
}

// identity 是这个组在绑定串与 outboundTag 里的身份。
func (p *Pool) identity() string { return poolTagPrefix + p.ID }

// poolIdentityToID 把绑定串还原成组 ID；不是组的返回 false。
func poolIdentityToID(identity string) (string, bool) {
	if !strings.HasPrefix(identity, poolTagPrefix) {
		return "", false
	}
	id := strings.TrimPrefix(identity, poolTagPrefix)
	return id, id != ""
}

// clone 深拷贝一个组。nil 传进来返回 nil，调用方不用再判空。
func (p *Pool) clone() *Pool {
	if p == nil {
		return nil
	}
	c := *p
	c.Hosts = append([]string(nil), p.Hosts...)
	return &c
}

// normalize 收敛成可用形态：去空去重成员、补齐模式与步长。
func (p *Pool) normalize() {
	if !validPoolMode(p.Mode) {
		p.Mode = poolModeSingle
	}
	if p.RRStep < 1 {
		p.RRStep = poolDefaultRRStep
	}
	if p.RRStep > poolMaxRRStep {
		p.RRStep = poolMaxRRStep
	}
	seen := map[string]bool{}
	hosts := make([]string, 0, len(p.Hosts))
	for _, h := range p.Hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	p.Hosts = hosts
	if p.Size > poolMaxSize {
		p.Size = poolMaxSize
	}
	if p.AutoFill && p.Size < 1 {
		// 开开关时把目标数钉成"现在这几个"。不钉的话 Size 是 0，
		// 判据会变成"一个都不用补"，开关看着像坏了。
		p.Size = len(p.Hosts)
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = "出口组 " + p.ID
	}
	p.Name = strings.TrimSpace(p.Name)
}

func (p *Pool) validate() error {
	if len(p.Hosts) == 0 {
		return fmt.Errorf("出口组至少要选一个出口")
	}
	if len(p.Hosts) > 32 {
		return fmt.Errorf("一个出口组最多 32 个出口")
	}
	if len(p.Name) > 64 {
		return fmt.Errorf("组名最长 64 个字符")
	}
	return nil
}

// poolStore 是出口组的持久状态。
//
// Next 只增不减：组 ID 一旦复用，某个还指着旧 ID 的入站就会在新组上复活，
// 那种串台没有任何报错，只会让流量从意外的出口出去。
type poolStore struct {
	Next  int     `json:"next"`
	Pools []*Pool `json:"pools"`
}

func poolStatePath(dir string) string { return filepath.Join(dir, "pools.json") }

func loadPoolStore(dir string) (*poolStore, error) {
	blob, err := os.ReadFile(poolStatePath(dir))
	if os.IsNotExist(err) {
		return &poolStore{Next: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var st poolStore
	if err := json.Unmarshal(blob, &st); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", poolStatePath(dir), err)
	}
	highest := 0
	for _, p := range st.Pools {
		p.normalize()
		if n := parsePoolSeq(p.ID); n > highest {
			highest = n
		}
	}
	if st.Next <= highest {
		st.Next = highest + 1
	}
	if st.Next < 1 {
		st.Next = 1
	}
	return &st, nil
}

func (s *poolStore) save(dir string) error {
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := poolStatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, poolStatePath(dir))
}

// parsePoolSeq 取出 "p12" 里的数字，取不出算 0。
func parsePoolSeq(id string) int {
	n := 0
	for i, r := range id {
		if i == 0 && r == 'p' {
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func (s *poolStore) get(id string) *Pool {
	for _, p := range s.Pools {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *poolStore) sorted() []*Pool {
	out := make([]*Pool, len(s.Pools))
	copy(out, s.Pools)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return parsePoolSeq(out[i].ID) < parsePoolSeq(out[j].ID)
	})
	return out
}

// poolMember 是组内一个成员在选路时的样子。
type poolMember struct {
	Host string
	// NS 是这条出口的网络命名空间，拨号要切进去。
	NS string
	// Slot/ExitIP 只为界面服务，选路不看。
	Slot    int
	ExitIP  string
	Up      bool
	Latency time.Duration // 实测 EWMA，0 表示还没有样本
	Cooling bool          // 刚失败过，暂时不优先选它
}

// poolChoice 是一次选路的结果。
type poolChoice struct {
	// Order 是按优先级要尝试的成员。并发模式下这是"同时发起"的全集，
	// 不是依次尝试的队列。
	Order      []poolMember
	Concurrent bool
}

// poolPolicy 是跨连接复用的选路状态。除构造外都只在持有锁时调用。
type poolPolicy struct {
	Mode string
	Step int
	// current 只给 single 模式用：整组粘在同一条上，掉了才换。
	current string
	// cursor 是轮询计数（按新目标递增）。
	cursor int
	// affi 是目标 → 成员主机名。同一目标固定走同一出口，
	// 免得一次浏览在同一站点上反复跳 IP。
	affi map[string]string
	// affiOrder 记 affi 的插入顺序，超限时按它 FIFO 淘汰。
	affiOrder []string
}

func newPoolPolicy(mode string, step int) *poolPolicy {
	if step < 1 {
		step = poolDefaultRRStep
	}
	return &poolPolicy{Mode: mode, Step: step, affi: map[string]string{}}
}

// pick 决定这个目标该走哪条（或哪几条）成员，会改写内部状态。
func (pp *poolPolicy) pick(target string, members []poolMember) poolChoice {
	// 冷却中的成员不是"排除"，而是"排到最后"：优先位让给健康的，
	// 但前面几条都拨不通时它还得是备选——冷却只是几十秒前的一次失败，
	// 真到全组都不顺的时候，挨个再试一遍比直接报错强。
	avail := make([]poolMember, 0, len(members))
	cold := make([]poolMember, 0, len(members))
	for _, m := range members {
		if !m.Up {
			continue
		}
		if m.Cooling {
			cold = append(cold, m)
		} else {
			avail = append(avail, m)
		}
	}
	if len(avail) == 0 {
		// 全在冷却：还是要给一次机会，否则一次抖动就能让整组永远起不来。
		avail, cold = cold, nil
	}
	if len(avail) == 0 {
		return poolChoice{}
	}

	// withCold 把降级候选补在选中的那条后面。
	withCold := func(chosen string) []poolMember {
		return append(putFirst(avail, chosen), cold...)
	}

	if pp.Mode == poolModeConcurrent {
		// 并发只抢健康的那几条：拉一条刚失败的出口一起抢只会多添一路超时。
		return poolChoice{Order: append([]poolMember(nil), avail...), Concurrent: true}
	}

	if pp.Mode == poolModeSingle {
		if pp.current != "" && memberOf(avail, pp.current) != nil {
			return poolChoice{Order: withCold(pp.current)}
		}
		// 第一次，或原先那条已经不在了：按列表顺序定在第一条可用的。
		// 不重新挑"最快"是刻意的——单一出口的语义就是稳定，
		// 换节点由用户或健康检查决定，选路层不要抢这个决定。
		pp.current = avail[0].Host
		return poolChoice{Order: withCold(pp.current)}
	}

	if h, ok := pp.affi[target]; ok {
		if m := memberOf(avail, h); m != nil && pp.stillSticky(*m, avail) {
			return poolChoice{Order: withCold(h)}
		}
		delete(pp.affi, target)
	}

	var chosen string
	switch pp.Mode {
	case poolModeBalance:
		chosen = bestByLatency(avail).Host
	default: // roundrobin
		chosen = avail[(pp.cursor/pp.Step)%len(avail)].Host
		pp.cursor++
	}
	pp.bind(target, chosen)
	return poolChoice{Order: withCold(chosen)}
}

// stillSticky 判断原先粘的成员还值不值得继续粘。
func (pp *poolPolicy) stillSticky(m poolMember, avail []poolMember) bool {
	// 轮询的语义就是"这个归它"，只要还活着就不因为快慢改主意。
	if pp.Mode != poolModeBalance {
		return true
	}
	if m.Latency == 0 {
		return true
	}
	best := bestByLatency(avail)
	if best.Latency == 0 {
		return true
	}
	return m.Latency <= poolSwitchFloor || m.Latency <= best.Latency*time.Duration(poolSwitchRatio)
}

func (pp *poolPolicy) bind(target, host string) {
	if _, ok := pp.affi[target]; !ok {
		if len(pp.affi) >= poolAffinityMax && len(pp.affiOrder) > 0 {
			oldest := pp.affiOrder[0]
			pp.affiOrder = pp.affiOrder[1:]
			delete(pp.affi, oldest)
		}
		pp.affiOrder = append(pp.affiOrder, target)
	}
	pp.affi[target] = host
}

// forgetMember 把绑到某条成员上的粘性全部撤掉：那条掉了，
// 下一次访问该重新分配，而不是继续按老账走一条已经不存在的出口。
func (pp *poolPolicy) forgetMember(host string) {
	for k, v := range pp.affi {
		if v == host {
			delete(pp.affi, k)
		}
	}
	kept := pp.affiOrder[:0]
	for _, k := range pp.affiOrder {
		if _, ok := pp.affi[k]; ok {
			kept = append(kept, k)
		}
	}
	pp.affiOrder = kept
	if pp.current == host {
		pp.current = ""
	}
}

func memberOf(ms []poolMember, host string) *poolMember {
	for i := range ms {
		if ms[i].Host == host {
			return &ms[i]
		}
	}
	return nil
}

// putFirst 把 host 那条提到最前面，其余顺序不变——那是失败时的降级顺序。
func putFirst(ms []poolMember, host string) []poolMember {
	out := make([]poolMember, 0, len(ms))
	if m := memberOf(ms, host); m != nil {
		out = append(out, *m)
	}
	for _, m := range ms {
		if m.Host != host {
			out = append(out, m)
		}
	}
	return out
}

// bestByLatency 挑延迟最优的一条。没有样本的排最前：
// 新成员得先被实测一次才有资格参与比较，否则它永远轮不上。
func bestByLatency(ms []poolMember) poolMember {
	out := append([]poolMember(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Latency, out[j].Latency
		if a == 0 {
			return b != 0
		}
		if b == 0 {
			return false
		}
		return a < b
	})
	return out[0]
}

// targetKey 从 host:port 里取出主机部分作为粘性键。
// 同一个站点的 80/443 该走同一条出口，网站看到的是一个站、不是一堆端口。
func targetKey(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// poolGoneHosts 把成员分成"此刻能用"和"该被顶替"两拨。
//
// 掉线分两种，等不等要分开看：
//
//   - 隧道还在、只是没连上（NS 非空）：等。fanout 自己会换同区节点重连，
//     刚掉两分钟的出口大概率回得来，这时候补位是白烧一个槽位，
//     还会和重连抢同一个地区。firstDead 记的就是"从什么时候开始看它不行"。
//   - 隧道整个没了（NS 为空）：不等。没有任何东西会把它带回来，
//     再等 5 分钟只是让组白白缺人。启动时恢复的隧道是以 starting 进表的，
//     不会走到这一支，所以不用担心一起进程就把整组拆了重补。
//
// 返回的 gone 保持成员表里的先后次序：补上来的新出口顶替原位，
// 组的成员顺序不会因一次补位而重排（单一模式的默认首选就是第一个，
// 重排等于悄悄改了用户的选路起点）。
func poolGoneHosts(members []poolMember, firstDead map[string]time.Time, after time.Duration, now time.Time) (live int, gone []string) {
	for _, m := range members {
		if m.Up {
			// 活了。记账本清掉，下次再掉要从头等满 after，
			// 否则一条"掉→回→掉"的隧道第二次掉线时会被立刻判死。
			delete(firstDead, m.Host)
			live++
			continue
		}
		if m.NS == "" {
			gone = append(gone, m.Host)
			delete(firstDead, m.Host)
			continue
		}
		first, ok := firstDead[m.Host]
		if !ok {
			firstDead[m.Host] = now
			continue
		}
		if now.Sub(first) >= after {
			gone = append(gone, m.Host)
		}
	}
	return live, gone
}

// poolFillFollow 给每个补位名额指定"跟谁的地区"。
//
// 返回的是失效成员的主机名而不是国家码：地区查得到查不到是调用方的事
// （隧道可能已经被删了，只能回头从节点清单里查），纯函数这里只管配对。
// 名额多于失效成员时（比如用户把目标数调大了），多出来的那些给空串，
// 意思是"没有原来的地区可跟，任意地区都行"——再窄化成某个地区就成了凭空猜。
func poolFillFollow(need int, gone []string) []string {
	if need <= 0 {
		return nil
	}
	out := make([]string, 0, need)
	for i := 0; i < need; i++ {
		if i < len(gone) {
			out = append(out, gone[i])
			continue
		}
		out = append(out, "")
	}
	return out
}
