package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestAuth() *Auth {
	return &Auth{
		password: "secret",
		sessions: map[string]time.Time{},
		fails:    map[string]*loginFails{},
	}
}

// 连续失败达到阈值后，该 IP 应被冷却挡下。
func TestLoginThrottleBlocksAfterMaxFails(t *testing.T) {
	a := newTestAuth()
	const ip = "203.0.113.7"

	for i := 0; i < loginMaxFails; i++ {
		if a.blocked(ip) {
			t.Fatalf("第 %d 次失败前不该被封", i)
		}
		a.recordFail(ip)
	}
	if !a.blocked(ip) {
		t.Fatalf("连续 %d 次失败后应进入冷却", loginMaxFails)
	}
}

// 登录成功要清零，之前的失败不该累积到下一轮。
func TestLoginThrottleClearOnSuccess(t *testing.T) {
	a := newTestAuth()
	const ip = "203.0.113.8"

	for i := 0; i < loginMaxFails-1; i++ {
		a.recordFail(ip)
	}
	a.clearFails(ip)
	if a.blocked(ip) {
		t.Fatal("清零后不该被封")
	}
	// 清零后再错一次也不该立刻触发冷却
	a.recordFail(ip)
	if a.blocked(ip) {
		t.Fatal("清零后单次失败不该被封")
	}
}

// 不同 IP 的失败互不牵连。
func TestLoginThrottleIsolatesIPs(t *testing.T) {
	a := newTestAuth()
	for i := 0; i < loginMaxFails; i++ {
		a.recordFail("198.51.100.1")
	}
	if a.blocked("198.51.100.2") {
		t.Fatal("一个 IP 被封不该波及另一个 IP")
	}
}

// 冷却到期后应自动解封。
func TestLoginThrottleUnblocksAfterExpiry(t *testing.T) {
	a := newTestAuth()
	const ip = "203.0.113.9"
	for i := 0; i < loginMaxFails; i++ {
		a.recordFail(ip)
	}
	if !a.blocked(ip) {
		t.Fatal("应先进入冷却")
	}
	// 手动把封禁时间拨到过去，模拟冷却结束
	a.mu.Lock()
	a.fails[ip].blocked = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if a.blocked(ip) {
		t.Fatal("冷却到期后应解封")
	}
}

// 退出必须两边都作废：只清 cookie 的话，被抄走的 token 在 TTL 内照样能进门。
func TestLogoutRevokesSession(t *testing.T) {
	a := newTestAuth()
	tok, err := a.issue()
	if err != nil {
		t.Fatalf("发会话失败: %v", err)
	}
	if !a.valid(tok) {
		t.Fatal("刚发的 token 就该有效")
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("退出请求不该再往下走到受保护的 handler")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 想 200", rec.Code)
	}
	if a.valid(tok) {
		t.Error("token 仍有效：cookie 清了但会话没收回")
	}
	c := cookiesByName(rec.Result().Cookies())[sessionCookie]
	if c == nil || c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("cookie 没被清掉: %+v", c)
	}
	// 别的会话不受影响——同一台机器上可能开着两个浏览器
	tok2, err := a.issue()
	if err != nil {
		t.Fatalf("发第二个会话失败: %v", err)
	}
	if !a.valid(tok2) {
		t.Error("退出把别人的会话也踢了")
	}
}

// 直接敲地址栏退出要落回登录页，而且得带着随机访问路径前缀，
// 否则 /xxxx/login 会跳到 /login，那条路在 StripBasePath 那里根本不存在。
func TestLogoutRedirectsUnderBasePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "basepath"), []byte("gwTest\n"), 0600); err != nil {
		t.Fatalf("写 basepath: %v", err)
	}
	if _, err := initBasePath(dir); err != nil {
		t.Fatalf("initBasePath: %v", err)
	}
	t.Cleanup(func() { _, _ = setBasePath("") })
	a := newTestAuth()

	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	rec := httptest.NewRecorder()
	a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("GET /logout 不该穿透到受保护的 handler")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 想 %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/gwTest/login" {
		t.Fatalf("跳转 = %q, 想 \"/gwTest/login\"", got)
	}
}

func cookiesByName(cs []*http.Cookie) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}
