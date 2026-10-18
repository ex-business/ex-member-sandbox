package app

import (
	"bytes"
	"context"
	"encoding/json"
	"ex-member-sandbox/internal/simulation"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulationRoutesKeepIdentityOriginAndOriginalRegistration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	memberPath := filepath.Join(dir, "members.db")
	store, e := Open(memberPath)
	if e != nil {
		t.Fatal(e)
	}
	defer store.DB.Close()
	member := addMember(t, store, nil)
	other := addMember(t, store, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"data":{"current":{"config":{"currency":"USD","currency_scale":2,"timezone":"UTC"}}}}`))
			return
		}
		w.WriteHeader(503)
	}))
	defer upstream.Close()
	commission := &Commission{URL: upstream.URL, Token: strings.Repeat("t", 32), Key: strings.Repeat("r", 32), Tenant: "site", App: "commission", Source: "sandbox"}
	sourceConfig := simulation.Config{URL: upstream.URL, Token: commission.Token, Key: strings.Repeat("k", 32), Tenant: "site", App: "commission", Source: "sandbox", Members: memberPath, Database: filepath.Join(dir, "facts.db")}
	raw, _ := json.Marshal(sourceConfig)
	configPath := filepath.Join(dir, "source.json")
	if e = os.WriteFile(configPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	source, e := LoadSimulation(configPath, memberPath, commission, store)
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	server := NewServer(store, commission, "http://127.0.0.1:19386/")
	handler := server.Handler()
	request := func(method, path string, body any, cookie *http.Cookie, origin string, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://127.0.0.1:19386"+path, bytes.NewReader(raw))
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d %s", method, path, w.Code, want, w.Body)
		}
		out := map[string]any{}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	request("GET", "/api/simulation/events", nil, nil, "", 404)
	server.Simulation = source
	request("GET", "/api/simulation/events", nil, nil, "", 401)
	cookieFor := func(member string) *http.Cookie {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://127.0.0.1:19386/", nil)
		server.login(w, r, member)
		return w.Result().Cookies()[0]
	}
	cookie := cookieFor(member.ID)
	otherCookie := cookieFor(other.ID)
	request("GET", "/api/simulation/events", nil, cookie, "", 200)
	in := simulation.Input{Business: "same-order", Kind: "DEPOSIT_SUCCEEDED", Amount: "50.00"}
	request("POST", "/api/simulation/events", in, cookie, "https://evil.example", 403)
	request("POST", "/api/simulation/events", in, cookie, "http://127.0.0.1:19386", 409)
	if _, e = store.DB.Exec(`UPDATE registration_outbox SET status='SUCCEEDED' WHERE member_id=?`, member.ID); e != nil {
		t.Fatal(e)
	}
	request("POST", "/api/simulation/events", in, cookie, "http://127.0.0.1:19386", 202)
	out := request("GET", "/api/simulation/events", nil, otherCookie, "", 200)
	if len(out["items"].([]any)) != 0 {
		t.Fatal("不能查看其他会员事实")
	}
	resend := map[string]string{"kind": "DEPOSIT_SUCCEEDED", "business_id": in.Business, "reason": "测试补发"}
	request("POST", "/api/simulation/resend", resend, otherCookie, "http://127.0.0.1:19386", 409)
	request("POST", "/api/simulation/resend", resend, cookie, "http://127.0.0.1:19386", 202)
	var original []byte
	if e = store.DB.QueryRow(`SELECT payload FROM registration_outbox WHERE member_id=?`, other.ID).Scan(&original); e != nil {
		t.Fatal(e)
	}
	registration := map[string]string{"kind": "REGISTRATION", "reason": "恢复注册投递"}
	request("POST", "/api/simulation/resend", registration, otherCookie, "http://127.0.0.1:19386", 202)
	var after []byte
	store.DB.QueryRow(`SELECT payload FROM registration_outbox WHERE member_id=?`, other.ID).Scan(&after)
	if !bytes.Equal(original, after) {
		t.Fatal("补发不得改写注册事实或邀请归属")
	}
	request("POST", "/api/simulation/resend", registration, cookie, "http://127.0.0.1:19386", 409)
	wrong := httptest.NewRequest("GET", "http://evil.example/api/simulation/events", nil)
	wrong.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, wrong)
	if w.Code != 403 {
		t.Fatal("模拟接口不得绕过Host校验")
	}
	if _, e = store.Member(ctx, member.ID); e != nil {
		t.Fatal("原会员查询必须保持", e)
	}
}
