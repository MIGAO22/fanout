package main

// 出口组的自动补位。
//
// 打开组的 AutoFill 之后，某个成员长时间连不上就起一条新出口顶替它，
// 让组始终维持 Size 条可用成员。这是整个程序里唯一会"自己开出口"的路径，
// 所以三道护栏都是必需的而不是保险：等满失效门槛（免得和自动换节点重连抢活）、
// 留槽位水位（免得连崩几轮把 maxSlots 吃光）、失败节点冷却（免得反复撞同一批死节点）。

import (
	"fmt"
	"log"
	"slices"
	"time"
)

// fillLoop 按固定节奏巡检一遍开了自动补齐的组。
//
// 补位一条出口最坏要等满整个连接预算（几十秒），所以巡检不追求准点，
// 上一轮没跑完就直接跳过这一轮——反正下一轮还会再来。
func (s *PoolService) fillLoop() {
	tk := time.NewTicker(poolFillInterval)
	defer tk.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tk.C:
			s.fillOnce()
		}
	}
}

func (s *PoolService) fillOnce() {
	s.mu.Lock()
	if s.filling {
		s.mu.Unlock()
		return
	}
	s.filling = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.filling = false
		s.mu.Unlock()
	}()

	for _, p := range s.pools() {
		if !p.AutoFill {
			continue
		}
		s.fillPool(p)
	}
	s.pruneFillBad()
}

// fillPool 把一个组补回 Size 条可用成员。
//
// 两条路，按代价排：先把**此刻已经在线的出口**顶进来（只改成员名单，瞬时、
// 不花连接预算），不够再新建出口（要起 openvpn、等 tun0、等出口 IP，几十秒起步，
// 还要占槽位）。新建是最后手段，不是默认手段。
//
// 目标条数 `p.Size` 在这里只读不写：用户设的"维持可用条数"不会因为掉线、
// 也不会因为补不满而变小。补不齐时把差多少、为什么写进 note 给界面看，
// 下一轮接着试。
func (s *PoolService) fillPool(p *Pool) {
	now := time.Now()
	// 失效计时取快照、算完再整体换回：poolGoneHosts 要就地增删这张表，
	// 直接把锁内的 map 递出去，就等于在锁外改它——删组那侧的 dropFillLedger
	// 正在同一把锁下删这同一个 key，-race 下就是数据竞争。
	first := s.fillDeadSnapshot(p.ID)
	members := s.resolveMembers(p.ID)
	live, gone := poolGoneHosts(members, first, poolFillDeadAfter, now)
	s.mergeFillDead(p.ID, first)
	short := p.Size - live
	if short <= 0 {
		s.setFillNote(p.ID, "")
		return
	}

	// 名额只给两种坑：已经判死的（gone），以及名单本身就比目标短的那几个
	// （用户把条数调大了）。还在自救窗口里的那些不给——它们大概率自己回来，
	// 提前占位会让组员名单比用户设的条数长，而且刚重启时组员全在这一档，
	// 不给就把别的组的在线出口整批抢成自己的组员了。
	need := min(short, len(gone)+max(0, p.Size-len(members)))
	if need <= 0 {
		s.setFillNote(p.ID, waitingNote(short))
		return
	}

	// 第一优先：复用在线出口。挑的时候避开本组成员、刚失败还在冷却的那批。
	filled := 0
	for _, host := range poolReusePick(s.fillCandidates(p.ID), need) {
		if s.reuseOne(p, host, slotOf(gone, filled)) {
			filled++
		}
	}
	if filled > 0 {
		log.Printf("出口组 %s 用在线出口顶回 %d 条（目标 %d）", p.Name, filled, p.Size)
	}

	// 第二优先（最后手段）：新建出口。地区不限——用户要的是"数量立刻回来"，
	// 不是"地区一模一样"；跟着死成员的地区挑，恰恰最容易挑到同一批一起挂的机房。
	// 循环按"还差几条"收敛：不然一次就能把可用槽位全开走。
	left := need - filled
	created := 0
	reason := ""
	room := s.mgr.SlotHeadroom() - poolFillSlotFloor
	if room < 0 {
		room = 0
	}
	for created < left && room > 0 {
		room--
		picks, err := s.mgr.pickNodes("", 1, s.fillAvoid(p.ID))
		if err != nil {
			reason = err.Error()
			break
		}
		node := picks[0]
		if !s.fillOne(p, node, slotOf(gone, filled+created)) {
			s.markFillBad(node.HostName)
			reason = fmt.Sprintf("试了 %s 还是连不上（冷却 %d 分钟）",
				node.HostName, int(poolFillPenalty.Minutes()))
			continue
		}
		created++
	}
	if created > 0 {
		log.Printf("出口组 %s 另新建 %d 条成员补位（在线顶替 %d 条，目标 %d）",
			p.Name, created, filled, p.Size)
	}
	filled += created
	if created < left && reason == "" {
		reason = fmt.Sprintf("在线出口都用上了，新建又受槽位限制（要留 %d 个余量）", poolFillSlotFloor)
	}

	leftover := p.Size - live - filled
	switch {
	case leftover <= 0:
		s.setFillNote(p.ID, "")
	case reason != "":
		s.setFillNote(p.ID, fmt.Sprintf("已顶回 %d 条，还差 %d 条：%s", filled, leftover, reason))
	default:
		// 剩下的都还在自救窗口里，不是缺人，下一轮再看
		s.setFillNote(p.ID, waitingNote(leftover))
	}
}

// waitingNote 解释"还差的那几条正自己在重连，这轮不该动它们"。
func waitingNote(n int) string {
	return fmt.Sprintf("另有 %d 条正在自动重连，等满 %d 秒没回来才顶替",
		n, int(poolFillDeadAfter.Seconds()))
}

// fillCandidates 列出此刻能拿来顶替的现有出口：在线、不在这个组里、不在冷却里。
// Shared 标出它是否已经挂在某个组的成员名单上，供 poolReusePick 排序。
func (s *PoolService) fillCandidates(id string) []poolFillCand {
	inPool := map[string]bool{}
	shared := map[string]bool{}
	for _, p := range s.pools() {
		for _, h := range p.Hosts {
			shared[h] = true
			if p.ID == id {
				inPool[h] = true
			}
		}
	}
	s.mu.Lock()
	cooling := map[string]bool{}
	for h, until := range s.fillBad {
		if now2 := time.Now(); now2.Before(until) {
			cooling[h] = true
		}
	}
	s.mu.Unlock()

	var out []poolFillCand
	for _, t := range s.mgr.Tunnels() {
		h := t.host()
		if h == "" || inPool[h] || cooling[h] {
			continue
		}
		out = append(out, poolFillCand{Host: h, Online: t.status() == "up", Shared: shared[h]})
	}
	return out
}

// reuseOne 把一个已经在线的出口顶进组里，只改成员名单，不起新出口。
//
// 和 fillOne 一样先写回成员、再收尾旧的：反过来做的话中途写回失败，
// 组里就同时少了两个人。
func (s *PoolService) reuseOne(p *Pool, host, oldHost string) bool {
	if err := s.swapMember(p.ID, oldHost, host); err != nil {
		log.Printf("出口组 %s 顶替 %s 失败: %v", p.Name, host, err)
		return false
	}
	s.forgetFillDead(p.ID, oldHost)
	s.retireMember(oldHost)
	log.Printf("出口组 %s：用在线的 %s 顶替了 %s", p.Name, host, orNone(oldHost))
	return true
}

// fillOne 起一条新出口并等它真的能用，成功了才顶替进组里。
//
// 顺序不能反：先写进 Hosts 再等 up 的话，这条起不来的主机会立刻被下一轮
// 判成失效成员，于是每一轮都补一条、每一轮都失败，槽位被垃圾隧道填满。
func (s *PoolService) fillOne(p *Pool, node Node, oldHost string) bool {
	t, err := s.mgr.Start(node)
	if err != nil {
		log.Printf("出口组 %s 补位起不来: %v", p.Name, err)
		return false
	}
	if st := s.mgr.waitUp(t); st != "up" {
		// 起不来就别占着槽位和端口，交给下一轮挑别人
		if serr := s.mgr.Stop(t.Slot); serr != nil {
			log.Printf("出口组 %s 回收失败的补位出口: %v", p.Name, serr)
		}
		return false
	}
	if err := s.swapMember(p.ID, oldHost, node.HostName); err != nil {
		log.Printf("出口组 %s 补位成功但写回成员失败: %v", p.Name, err)
		return false
	}
	// 顶替完成后清掉这条旧记录的计时：它已经不在这个组里了，
	// 留着会让 fillDead 越来越长，也会让同名节点日后回来时被误判。
	s.forgetFillDead(p.ID, oldHost)
	// 顺序要紧：先确认新那条真的接上了位置，再收尾旧的。
	// 反过来做的话中途 Start 失败，等于既没补上人、又把原来的拆了。
	s.retireMember(oldHost)
	log.Printf("出口组 %s：用 %s 顶替了 %s", p.Name, node.HostName, orNone(oldHost))
	return true
}

// retireMember 顶替完成后收掉原来那条出口。
//
// 不收它会一直占着槽位：被判死的成员在自己的重连回路里循环重试，
// 只有用户删掉它才会停。补位每轮搭一条新的进来，死的就攒成一群僵尸，
// 最后把槽位上限吃光——那时补位只剩"槽位不够，先不补"这一句话，
// 看上去正是"节点没了也不会自动换"。
//
// 三条前提缺一条就不动它：
//   - 它自己回来了（补位从挑节点到等 up 中间隔着几分钟，够同区节点重新上线）；
//   - 还有别的组指着它，停了就是拆别人的保活；
//   - 有入站直接绑在它身上。组的绑定走 pool-<id>，绑到组的不会命中这一条；
//     问后端要不到答案时按"绑着"处理——宁可留一条僵尸，
//     也不能把谁正在用的连接悄悄停掉。
func (s *PoolService) retireMember(host string) {
	if host == "" {
		return
	}
	var t *Tunnel
	for _, x := range s.mgr.Tunnels() {
		if x.host() == host {
			t = x
			break
		}
	}
	if t == nil || t.status() == "up" {
		return
	}
	if s.hostInPool(host) {
		log.Printf("出口组顶替后保留 %s：还有出口组指着它", host)
		return
	}
	if s.hostBound(host) {
		log.Printf("出口组顶替后保留 %s：还有入站直接绑着它", host)
		return
	}
	if err := s.mgr.Stop(t.Slot); err != nil {
		log.Printf("出口组顶替后收尾 %s 失败: %v", host, err)
		return
	}
	log.Printf("出口组顶替后停掉了原来的出口 %s（槽位 %d 已释放）", host, t.Slot)
}

// hostInPool 判断此刻还有没有组把这个主机名当成员。
// 调用它的时候本次顶替已经写回存储了，所以本组不会算进来。
func (s *PoolService) hostInPool(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.store.Pools {
		if slices.Contains(p.Hosts, host) {
			return true
		}
	}
	return false
}

// hostBound 问后端有没有入站直接绑在这条出口上。
//
// 走 openPanel 现问，不走 cachedInbounds：那份缓存是给界面用的，
// 里面存着按界面口径算好的 BoundUp，补位拿 nil 去填会把它刷成"全部离线"。
func (s *PoolService) hostBound(host string) bool {
	x, err := openPanel()
	if err != nil {
		return true
	}
	list, err := x.Inbounds(nil)
	if err != nil {
		return true
	}
	tag := sanitizeTag(host)
	for _, ib := range list {
		if ib.BoundTo == tag {
			return true
		}
	}
	return false
}

// swapMember 只改 Hosts 这一项，不走 setPool。
//
// setPool 是整份覆盖，而补位从挑节点到等 up 中间隔着几十秒，
// 这期间用户很可能已经在界面上动过这个组了。整份覆盖会静默吞掉他的修改，
// 表现是"我明明加了那条，过一会儿又没了"。
func (s *PoolService) swapMember(id, oldHost, newHost string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.store.get(id)
	if p == nil {
		return fmt.Errorf("出口组 %s 已不存在", id)
	}
	if slices.Contains(p.Hosts, newHost) {
		return nil
	}
	if i := slices.Index(p.Hosts, oldHost); oldHost != "" && i >= 0 {
		p.Hosts[i] = newHost
	} else {
		p.Hosts = append(p.Hosts, newHost)
	}
	return s.store.save(s.dir)
}

// fillAvoid 是这一轮不该拿来补位的节点：组里现有的成员，加上刚失败还在冷却的。
func (s *PoolService) fillAvoid(id string) map[string]bool {
	avoid := map[string]bool{}
	s.mu.Lock()
	for h, until := range s.fillBad {
		if time.Now().Before(until) {
			avoid[h] = true
		}
	}
	s.mu.Unlock()
	for _, m := range s.resolveMembers(id) {
		avoid[m.Host] = true
	}
	return avoid
}

func (s *PoolService) markFillBad(host string) {
	s.mu.Lock()
	s.fillBad[host] = time.Now().Add(poolFillPenalty)
	s.mu.Unlock()
}

func (s *PoolService) pruneFillBad() {
	now := time.Now()
	s.mu.Lock()
	for h, until := range s.fillBad {
		if now.After(until) {
			delete(s.fillBad, h)
		}
	}
	s.mu.Unlock()
}

// fillDeadSnapshot 复制一份这个组的失效计时。
// 副本交得出锁外，是因为之后只有补位这一个 goroutine 用它。
func (s *PoolService) fillDeadSnapshot(id string) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.fillDead[id]
	out := make(map[string]time.Time, len(src))
	for h, t := range src {
		out[h] = t
	}
	return out
}

// mergeFillDead 用算完的副本整体换掉账本。
//
// 是"换"而不是"并回去"：poolGoneHosts 只会增删当前成员对应的键，
// 已经不在这个组里的成员（被顶替掉、被用户删掉）留在表里就是漏账，
// 整体替换顺手就把它们清干净了。组在这几十秒里被删掉的话就别再写回去。
func (s *PoolService) mergeFillDead(id string, first map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.get(id) == nil {
		delete(s.fillDead, id)
		return
	}
	s.fillDead[id] = first
}

func (s *PoolService) forgetFillDead(id, host string) {
	if host == "" {
		return
	}
	s.mu.Lock()
	if m := s.fillDead[id]; m != nil {
		delete(m, host)
	}
	s.mu.Unlock()
}

// dropFillLedger 清掉一个组的全部补位账本，删组时用。
func (s *PoolService) dropFillLedger(id string) {
	s.mu.Lock()
	delete(s.fillDead, id)
	delete(s.fillNote, id)
	s.mu.Unlock()
}

func (s *PoolService) setFillNote(id, note string) {
	s.mu.Lock()
	if note == "" {
		delete(s.fillNote, id)
	} else {
		s.fillNote[id] = note
	}
	s.mu.Unlock()
}

// fillNotes 给界面用：组 ID → 为什么还没补齐。没原因就不出现。
func (s *PoolService) fillNotes() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.fillNote))
	for k, v := range s.fillNote {
		out[k] = v
	}
	return out
}

// slotOf 取第 i 个补位名额要顶替的旧成员。越界表示这是净增（用户调大了目标数）。
func slotOf(gone []string, i int) string {
	if i < len(gone) {
		return gone[i]
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "（净增名额）"
	}
	return s
}
