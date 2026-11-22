package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 出口组的 HTTP 层。列表数据跟着 /api/exits 一起给（界面本来就每几秒轮询它），
// 这里只管增删改与策略元数据。

// apiPools 给编辑器返回可选策略与现有组。
func apiPools(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type modeOpt struct {
			Mode  string `json:"mode"`
			Label string `json:"label"`
			Desc  string `json:"desc"`
		}
		modes := make([]modeOpt, 0, len(poolModes))
		for _, pm := range poolModes {
			modes = append(modes, modeOpt{pm, poolModeLabels[pm], poolModeDesc[pm]})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"pools":     poolViews(m.Tunnels()),
			"modes":     modes,
			"rr_step":   poolDefaultRRStep,
			"max_hosts": 32,
		})
	}
}

// apiPoolSave 新建或修改一个组。id 为空表示新建。
func apiPoolSave(m *Manager) http.HandlerFunc {
	type req struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Mode   string   `json:"mode"`
		RRStep int      `json:"rr_step"`
		Hosts  []string `json:"hosts"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只接受 POST"})
			return
		}
		if poolsSvc == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "出口组功能未就绪"})
			return
		}
		var in req
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
			return
		}
		if !validPoolMode(in.Mode) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知的选路策略"})
			return
		}

		p := &Pool{
			ID: strings.TrimSpace(in.ID), Name: in.Name, Mode: in.Mode,
			RRStep: in.RRStep, Hosts: in.Hosts,
		}
		if err := checkPoolHosts(p, m.Tunnels()); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := poolsSvc.setPool(p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// 组的出站要立刻进配置，否则下一个绑定会指向不存在的 tag
		if err := m.SyncPoolEgress(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("组已保存，但写入节点链接后端失败：%v", err),
			})
			return
		}
		invalidateInbounds()
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name})
	}
}

// checkPoolHosts 校验组员都是此刻真实存在的出口。
//
// 宁可报错也不静默丢成员：少一条组员意味着少一层保活，
// 而界面上看起来组还是那个组，这种差异最难查。
func checkPoolHosts(p *Pool, tunnels []*Tunnel) error {
	known := map[string]bool{}
	for _, t := range tunnels {
		known[t.host()] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, h := range p.Hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if !known[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("这些出口当前不存在，请重新勾选：%s", strings.Join(missing, "、"))
	}
	// 保留勾选顺序：single 模式默认落在第一条上，轮询也按这个顺序走，
	// 打乱会让用户精心排的第一条备选变成随机一条。
	deduped := make([]string, 0, len(seen))
	for _, h := range p.Hosts {
		h = strings.TrimSpace(h)
		if h == "" || !seen[h] {
			continue
		}
		seen[h] = false // 只收第一次出现
		deduped = append(deduped, h)
	}
	p.Hosts = deduped
	return nil
}

// apiPoolDelete 删组，并先把指着它的入站解绑。
//
// 顺序很要紧：Xray 里留着一条指向不存在 outbund 的规则会让整份配置加载失败，
// 连带其他入站一起掉线。所以先解绑、再删组、最后重写出站。
func apiPoolDelete(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只接受 POST"})
			return
		}
		if poolsSvc == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "出口组功能未就绪"})
			return
		}
		var in struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
			return
		}
		p := poolsSvc.pool(in.ID)
		if p == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "出口组不存在"})
			return
		}

		identity := p.identity()
		panel, err := openPanel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		live := map[string]bool{}
		list, err := panel.Inbounds(live)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		var unbound []int
		for _, ib := range list {
			if ib.BoundTo != sanitizeTag(identity) {
				continue
			}
			if err := panel.Bind(ib.Tag, "", m.Tunnels()); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"error": fmt.Sprintf("解绑入站 %d 失败，组未删除：%v", ib.ID, err),
				})
				return
			}
			unbound = append(unbound, ib.ID)
		}

		if err := poolsSvc.deletePool(p.ID); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if err := m.SyncPoolEgress(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("组已删除，但清理后端出站失败：%v", err),
			})
			return
		}
		invalidateInbounds()
		writeJSON(w, http.StatusOK, map[string]any{"ok": "已删除", "unbound": unbound})
	}
}

// apiPoolCred 换组的 SOCKS5 凭据。
//
// 这是本机内部端口（只绑 127.0.0.1），换口令不影响客户端——
// 客户端连的是入站，不是这个端口。留着它是为了和单出口那侧的操作保持一致。
func apiPoolCred(m *Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只接受 POST"})
			return
		}
		if poolsSvc == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "出口组功能未就绪"})
			return
		}
		var in struct {
			ID   string `json:"id"`
			User string `json:"user"`
			Pass string `json:"pass"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
			return
		}
		cred := SocksCred{User: strings.TrimSpace(in.User), Pass: strings.TrimSpace(in.Pass)}
		if cred.User == "" && cred.Pass == "" {
			gen, err := newSocksCred()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			cred = gen
		} else if err := validateCred(cred); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := poolsSvc.setCred(in.ID, cred); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// 后端出站里带着这套凭据，改完必须重写一次才连得上
		if err := m.SyncPoolEgress(); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("口令已改，但同步到节点链接后端失败：%v", err),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"user": cred.User, "pass": cred.Pass})
	}
}
