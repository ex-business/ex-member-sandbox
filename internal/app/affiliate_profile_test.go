package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAffiliateProfilesStableIDLoginAndPrivateExport(t *testing.T) {
	s := testStore(t)
	m := addMember(t, s, nil)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.RecordAffiliateLogin(ctx, m.ID, "127.0.0.1:1234", now); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.DB.QueryRow(`SELECT display_id FROM affiliate_profiles WHERE member_id=?`, m.ID).Scan(&id); err != nil || len(id) != 7 {
		t.Fatal(id, err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RecordAffiliateLogin(ctx, m.ID, "203.0.113.9:4321", now.Add(-time.Hour)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var ip, at, after string
	s.DB.QueryRow(`SELECT display_id,last_login_ip,last_login_at FROM affiliate_profiles WHERE member_id=?`, m.ID).Scan(&after, &ip, &at)
	if id != after || ip != "127.0.0.1" {
		t.Fatal("展示ID变化或旧登录覆盖新登录", after, ip, at)
	}
	file := filepath.Join(t.TempDir(), "profiles.json")
	if err := s.ExportAffiliateProfiles(ctx, file, "site", "app", "sandbox"); err != nil {
		t.Fatal(err)
	}
	stat, _ := os.Stat(file)
	if stat.Mode().Perm() != 0600 {
		t.Fatal(stat.Mode())
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), m.Email) || strings.Contains(string(raw), "password") || strings.Contains(string(raw), "token") {
		t.Fatal("快照含凭据或邮箱")
	}
	var value []map[string]any
	if json.Unmarshal(raw, &value) != nil || len(value) != 1 {
		t.Fatal(string(raw))
	}
	profile := value[0]["profiles"].([]any)[0].(map[string]any)
	if profile["display_id"] != id || profile["vip_level"] != nil {
		t.Fatal(profile)
	}
}
func TestAffiliateAuditFailureDoesNotChangeIdentityLogin(t *testing.T) {
	s := testStore(t)
	salt := "audit-salt"
	hash, _ := hashPassword("test-password", salt)
	m, e := s.Register(context.Background(), "audit@example.test", "审计隔离会员", hash, salt, "sandbox", nil)
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Exec(`UPDATE registration_outbox SET status='SUCCEEDED',agent_id='agent-test' WHERE member_id=?`, m.ID)
	s.DB.Exec(`CREATE TABLE affiliate_profiles(broken_column TEXT)`)
	identity := &Identity{Store: s, ClientID: "web", Secret: strings.Repeat("s", 32), Issuer: "sandbox", Audience: "commission", Tenant: "site", App: "app", Source: "sandbox", Now: time.Now}
	if e = identity.Prepare(context.Background()); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/integration/login", strings.NewReader(`{"username":"audit@example.test","password":"test-password"}`))
	r.SetBasicAuth("web", identity.Secret)
	w := httptest.NewRecorder()
	identity.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "access_token") {
		t.Fatal("独立审计异常影响登录", w.Code, w.Body.String())
	}
}
