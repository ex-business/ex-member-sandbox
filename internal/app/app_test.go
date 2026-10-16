package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "members.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}
func addMember(t *testing.T, s *Store, code *string) Member {
	t.Helper()
	m, err := s.Register(context.Background(), ID()+"@example.com", "测试会员", "not-a-real-password-hash", "test-salt", "sandbox", code)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOutboxRetryAndSignature(t *testing.T) {
	s := testStore(t)
	m := addMember(t, s, nil)
	var attempts atomic.Int32
	var first []byte
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(strings.Repeat("r", 32)))
		mac.Write([]byte("registration-v1\nsite\napp\nsandbox\n"))
		mac.Write(raw)
		if r.Header.Get("X-Registration-Signature") != hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("Authorization") != "Bearer token" {
			t.Error("来源签名或服务端身份不正确")
		}
		var event RegistrationEvent
		if json.Unmarshal(raw, &event) != nil || r.Header.Get("Idempotency-Key") != event.EventID {
			t.Error("未沿用原事件号")
		}
		mu.Lock()
		if first == nil {
			first = append([]byte{}, raw...)
		} else if !bytes.Equal(first, raw) {
			t.Error("重试改变了注册事实")
		}
		mu.Unlock()
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"agent_id": "agent-one", "parent_agent_id": nil, "external_user_id": m.ID, "source_id": "sandbox"}})
	}))
	defer upstream.Close()
	c := &Commission{URL: upstream.URL, Token: "token", Key: strings.Repeat("r", 32), Tenant: "site", App: "app", Source: "sandbox", Client: upstream.Client()}
	if worked, err := c.Deliver(context.Background(), s); err != nil || !worked {
		t.Fatal(worked, err)
	}
	saved, _ := s.Member(context.Background(), m.ID)
	if saved.Delivery != "PENDING" || saved.AgentID != "" {
		t.Fatal(saved)
	}
	if _, err := s.DB.Exec(`UPDATE registration_outbox SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Deliver(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.Member(context.Background(), m.ID)
	if saved.Delivery != "SUCCEEDED" || saved.AgentID != "agent-one" {
		t.Fatal(saved)
	}
	if worked, err := c.Deliver(context.Background(), s); err != nil || worked {
		t.Fatal("重复投递已完成注册", worked, err)
	}
}
func TestPermanentFailureAndLeaseRecovery(t *testing.T) {
	s := testStore(t)
	m := addMember(t, s, nil)
	first, err := s.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE registration_outbox SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Lease == second.Lease || !bytes.Equal(first.Raw, second.Raw) {
		t.Fatal("恢复必须使用新租约及原载荷")
	}
	if err = s.Finish(context.Background(), first, "SUCCEEDED", "stale-agent", nil, ""); err == nil {
		t.Fatal("旧租约结果覆盖成功")
	}
	if err = s.Finish(context.Background(), second, "PENDING", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	_, _ = s.DB.Exec(`UPDATE registration_outbox SET next_at=0`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }))
	defer upstream.Close()
	c := &Commission{URL: upstream.URL, Client: upstream.Client()}
	if _, err = c.Deliver(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Member(context.Background(), m.ID)
	if saved.Delivery != "BLOCKED" || saved.Reason != "UPSTREAM_409" {
		t.Fatal(saved)
	}
	if worked, err := c.Deliver(context.Background(), s); worked || err != nil {
		t.Fatal("业务拒绝不得无限重投")
	}
}
func TestDuplicateRegistrationAtomicity(t *testing.T) {
	s := testStore(t)
	m := addMember(t, s, nil)
	invite := "changed-invite"
	if _, err := s.Register(context.Background(), strings.ToUpper(m.Email), "覆盖姓名", "hash", "salt", "sandbox", &invite); err == nil {
		t.Fatal("重复会员注册应拒绝")
	}
	var members, outbox int
	s.DB.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&members)
	s.DB.QueryRow(`SELECT COUNT(*) FROM registration_outbox`).Scan(&outbox)
	if members != 1 || outbox != 1 {
		t.Fatalf("非原子注册 %d %d", members, outbox)
	}
	var event RegistrationEvent
	var raw []byte
	s.DB.QueryRow(`SELECT payload FROM registration_outbox`).Scan(&raw)
	json.Unmarshal(raw, &event)
	if event.InvitationCode != nil {
		t.Fatal("重复请求改写了首次邀请")
	}
}
func TestMemberHTTPBoundary(t *testing.T) {
	store := testStore(t)
	s := NewServer(store, &Commission{Source: "sandbox"}, "http://127.0.0.1:18280/")
	handler := s.Handler()
	call := func(path string, data any, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		method := "GET"
		var raw []byte
		if data != nil {
			raw, _ = json.Marshal(data)
			method = "POST"
		}
		req := httptest.NewRequest(method, "http://127.0.0.1:18280"+path, bytes.NewReader(raw))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	input := map[string]any{"email": "user@example.com", "display_name": "测试用户", "password": "local-demo-password", "invitation_code": nil}
	if res := call("/api/register", input, nil, "https://evil.example"); res.Code != 403 {
		t.Fatal("允许跨来源注册")
	}
	if res := call("/api/members", nil, nil, ""); res.Code != 401 {
		t.Fatal("匿名读取会员")
	}
	res := call("/api/register", input, nil, "http://127.0.0.1:18280")
	if res.Code != 201 {
		t.Fatal(res.Code, res.Body.String())
	}
	cookies := res.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	if strings.Contains(res.Body.String(), "password") || strings.Contains(res.Body.String(), "salt") {
		t.Fatal("泄漏密码")
	}
	res = call("/api/me", nil, cookies[0], "")
	if res.Code != 200 {
		t.Fatal(res.Code)
	}
	res = call("/api/members", nil, cookies[0], "")
	if strings.Contains(res.Body.String(), "user@example.com") {
		t.Fatal("列表泄漏邮箱")
	}
	res = call("/api/register", input, nil, "http://127.0.0.1:18280")
	if res.Code != 409 {
		t.Fatal("重复注册", res.Code, res.Body.String())
	}
	login := map[string]string{"email": "user@example.com", "password": "wrong-password"}
	if res = call("/api/login", login, nil, "http://127.0.0.1:18280"); res.Code != 401 {
		t.Fatal("错误密码登录")
	}
	login["password"] = "local-demo-password"
	if res = call("/api/login", login, nil, "http://127.0.0.1:18280"); res.Code != 200 {
		t.Fatal("正常登录失败")
	}
	if res = call("/api/logout", map[string]any{}, cookies[0], "http://127.0.0.1:18280"); res.Code != 200 {
		t.Fatal(res.Code)
	}
	if res = call("/api/me", nil, cookies[0], ""); res.Code != 401 {
		t.Fatal("退出后会话仍有效")
	}
}
func TestDatabasePersistsRegistration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "members.db")
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	m := addMember(t, s, nil)
	s.DB.Close()
	s, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	saved, err := s.Member(context.Background(), m.ID)
	if err != nil || saved.Delivery != "PENDING" {
		t.Fatal(saved, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	j, err := s.Claim(ctx)
	if err != nil || j.MemberID != m.ID {
		t.Fatal(j, err)
	}
}
