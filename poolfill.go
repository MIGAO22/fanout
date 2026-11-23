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
// 一次最多补 need 条，补不上就把原因写进 note 给界面看，不重试到成功为止。
func (s *PoolService) fillPool(p *Pool) {
	now := time.Now()
	// 失效计时取快照、算完再整体换回：poolGoneHosts 要就地增删这张表，
	// 直接把锁内的 map 递出去，就等于在锁外改它——删组那侧的 dropFillLedger
	// 正在同一把锁下删这同一个 key，-race 下就是数据竞争。
	first := s.fillDeadSnapshot(p.ID)
	live, gone := poolGoneHosts(s.resolveMembers(p.ID), first, poolFillDeadAfter, now)
	s.mergeFillDead(p.ID, first)
	need := p.Size - live
	if need <= 0 {
		s.setFillNote(p.ID, "")
		return
	}

	headroom := s.mgr.SlotHeadroom()
	if headroom-need < poolFillSlotFloor {
		s.setFillNote(p.ID, fmt.Sprintf(
			"还差 %d 条，但槽位只剩 %d 个：自动补齐要留 %d 个余量，先不补", need, headroom, poolFillSlotFloor))
		return
	}

	filled := 0
	for i, follow := range poolFillFollow(need, gone) {
		region := s.mgr.NodeRegion(follow)
		picks, err := s.mgr.pickNodes(region, 1, s.fillAvoid(p.ID))
		if err != nil {
			s.setFillNote(p.ID, fmt.Sprintf("还差 %d 条：%v", need-i, err))
			return
		}
		node := picks[0]
		if !s.fillOne(p, node, slotOf(gone, i)) {
			s.markFillBad(node.HostName)
			s.setFillNote(p.ID, fmt.Sprintf("补位没成：试了 %s，还是连不上（冷却 %d 分钟）",
				node.HostName, int(poolFillPenalty.Minutes())))
			continue
		}
		filled++
	}
	if filled > 0 {
		log.Printf("出口组 %s 自动补齐 %d 条成员（目标 %d）", p.Name, filled, p.Size)
		s.setFillNote(p.ID, "")
	}
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
	log.Printf("出口组 %s：用 %s 顶替了 %s", p.Name, node.HostName, orNone(oldHost))
	return true
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
