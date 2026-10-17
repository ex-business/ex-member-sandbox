package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityLoginIntrospectionAndRevocation(t *testing.T) {
	store, e := Open(filepath.Join(t.TempDir(), "members.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.DB.Close()
	salt := "test-identity-salt"
	hash, _ := hashPassword("test-password", salt)
	m, e := store.Register(context.Background(), "member@example.test", "会员", hash, salt, "member-sandbox", nil)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	identity := &Identity{Store: store, ClientID: "web", Secret: strings.Repeat("s", 32), Issuer: "sandbox", Audience: "commission", Tenant: "tenant", App: "app", Source: "member-sandbox", Now: func() time.Time { return now }}
	if e = identity.Prepare(context.Background()); e != nil {
		t.Fatal(e)
	}
	call := func(path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if auth {
			r.SetBasicAuth("web", identity.Secret)
		}
		if path != "/integration/login" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		identity.Handler().ServeHTTP(w, r)
		return w
	}
	login := `{"username":"member@example.test","password":"test-password"}`
	if w := call("/integration/login", login, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("/integration/login", login, true); w.Code != 409 {
		t.Fatal("未同步会员必须拒绝", w.Code, w.Body)
	}
	_, e = store.DB.Exec(`UPDATE registration_outbox SET status='SUCCEEDED',agent_id='agent-test' WHERE member_id=?`, m.ID)
	if e != nil {
		t.Fatal(e)
	}
	if w := call("/integration/login", `{"username":"member@example.test","password":"wrong"}`, true); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("/integration/login", `{"username":"member@example.test","password":"test-password","member_user_id":"forged"}`, true); w.Code != 400 {
		t.Fatal("禁止自报内部会员", w.Code)
	}
	w := call("/integration/login", login, true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Token string `json:"access_token"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &out); e != nil || !strings.HasPrefix(out.Token, "ms_") {
		t.Fatal(e, w.Body)
	}
	var stored string
	if e = store.DB.QueryRow(`SELECT token_hash FROM identity_tokens`).Scan(&stored); e != nil || stored == out.Token {
		t.Fatal("仅允许保存令牌摘要", e)
	}
	form := url.Values{"token": {out.Token}}.Encode()
	w = call("/integration/introspect", form, true)
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	if result["active"] != true || result["sub"] != m.ID || result["tenant_id"] != "tenant" || result["source_id"] != "member-sandbox" {
		t.Fatal(w.Body)
	}
	now = now.Add(2 * time.Hour)
	w = call("/integration/introspect", form, true)
	if !strings.Contains(w.Body.String(), `"active":false`) {
		t.Fatal(w.Body)
	}
	now = now.Add(-2 * time.Hour)
	if w = call("/integration/revoke", form, true); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = call("/integration/introspect", form, true)
	if !strings.Contains(w.Body.String(), `"active":false`) {
		t.Fatal(w.Body)
	}
}
