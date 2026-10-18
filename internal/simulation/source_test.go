package simulation

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

func testSource(t *testing.T, handler http.HandlerFunc) *Source {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	s, e := Open(Config{URL: upstream.URL, Token: strings.Repeat("t", 32), Key: strings.Repeat("k", 32), Tenant: "site", App: "commission", Source: "sandbox", Members: filepath.Join(dir, "members.db"), Database: filepath.Join(dir, "source.db")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func profile(w http.ResponseWriter) {
	io.WriteString(w, `{"data":{"current":{"config":{"currency":"USD","currency_scale":2,"timezone":"UTC"}}}}`)
}
func TestRetryUsesOriginalBytesAndStableIdentity(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	var lock sync.Mutex
	var payloads [][]byte
	var keys []string
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			profile(w)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		lock.Lock()
		payloads = append(payloads, append([]byte{}, raw...))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		lock.Unlock()
		mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
		mac.Write([]byte("affiliate-business-v1\nsite\ncommission\nsandbox\n"))
		mac.Write(raw)
		if r.Header.Get("X-Affiliate-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("签名未绑定原始正文和作用域")
		}
		var e Event
		json.Unmarshal(raw, &e)
		w.WriteHeader(int(status.Load()))
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"event_id": e.ID, "business_id": e.Business}})
	})
	ctx := context.Background()
	input := Input{Business: "deposit", Kind: "DEPOSIT_SUCCEEDED", Amount: "50.00"}
	event, e := s.Record(ctx, "member", input, time.Now().Add(-time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	rows, _ := s.Records(ctx, "member")
	if rows[0].State != "PENDING" || rows[0].Attempts != 1 || rows[0].Next <= time.Now().UnixMilli() {
		t.Fatal("临时失败应退避", rows)
	}
	if worked, e := s.Deliver(ctx); worked || e != nil {
		t.Fatal("退避期不可忙重试", worked, e)
	}
	status.Store(200)
	if e = s.Resend(ctx, "another-member", input.Kind, input.Business, "测试越权"); e == nil {
		t.Fatal("不得补发其他会员事实")
	}
	if e = s.Resend(ctx, "member", input.Kind, input.Business, "恢复依赖后补发"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	rows, _ = s.Records(ctx, "member")
	if rows[0].State != "DELIVERED" || rows[0].Attempts != 2 {
		t.Fatal(rows)
	}
	if !bytes.Equal(payloads[0], payloads[1]) || keys[0] != event.ID || keys[1] != event.ID {
		t.Fatal("重试改写了载荷或幂等键")
	}
	input.Amount = "60.00"
	if _, e = s.Record(ctx, "member", input, time.Now().Add(-time.Hour)); e == nil {
		t.Fatal("不得覆盖订单金额")
	}
	if e = s.Resend(ctx, "member", input.Kind, input.Business, "已确认不可补发"); e == nil {
		t.Fatal("成功事件不可重复排队")
	}
	if others, _ := s.Records(ctx, "another-member"); len(others) != 0 {
		t.Fatal("记录泄露其他会员")
	}
}
func TestBlockedRecoveryAndExpiredLease(t *testing.T) {
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			profile(w)
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":"PERMISSION_DENIED"}}`)
	})
	ctx := context.Background()
	_, e := s.Record(ctx, "m", Input{Business: "bet", Kind: "BET_SETTLED", Amount: "100", ValidBet: "80", Venue: "SPORTS"}, time.Now().Add(-time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	rows, _ := s.Records(ctx, "m")
	if rows[0].State != "BLOCKED" || rows[0].Code != "PERMISSION_DENIED" {
		t.Fatal(rows)
	}
	if worked, _ := s.Deliver(ctx); worked {
		t.Fatal("永久错误不得自动重投")
	}
	if e = s.Resend(ctx, "m", "BET_SETTLED", "bet", ""); e == nil {
		t.Fatal("补发原因必填")
	}
	if e = s.Resend(ctx, "m", "BET_SETTLED", "bet", "已修正来源权限"); e != nil {
		t.Fatal(e)
	}
	first, e := s.claim(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(`UPDATE simulation_delivery SET lease_until=0`); e != nil {
		t.Fatal(e)
	}
	second, e := s.claim(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if first.Lease == second.Lease {
		t.Fatal("接管必须更换租约")
	}
	if e = s.finish(ctx, second, "DELIVERED", "", time.Second); e != nil {
		t.Fatal(e)
	}
	if e = s.finish(ctx, first, "BLOCKED", "OLD_RESULT", time.Second); e != nil {
		t.Fatal(e)
	}
	rows, _ = s.Records(ctx, "m")
	if rows[0].State != "DELIVERED" {
		t.Fatal("旧租约覆盖了新结果", rows)
	}
	if _, e = Open(s.Config); e == nil {
		t.Fatal("同源自动任务必须互斥")
	}
}
func TestOfflineCreationAfterKnownProfileAndDecimalSafety(t *testing.T) {
	var offline atomic.Bool
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(503)
			return
		}
		profile(w)
	})
	if _, e := s.Profile(context.Background()); e != nil {
		t.Fatal(e)
	}
	offline.Store(true)
	if _, e := s.Record(context.Background(), "m", Input{Business: "offline", Kind: "DEPOSIT_SUCCEEDED", Amount: "1.23"}, time.Now().Add(-time.Hour)); e != nil {
		t.Fatal("下游不可用时应先保存来源事实", e)
	}
	for _, v := range []string{"1e3", "-1", "1.001", "NaN", "01", "99999999999999999999999999999999999999"} {
		if _, e := Minor(v, 2); e == nil {
			t.Fatal("不精确金额未拒绝", v)
		}
	}
	if v, e := Minor("90071992547409.93", 2); e != nil || v != "9007199254740993" {
		t.Fatal("大金额丢失精度", v, e)
	}
}
func TestWatermarkWaitsForFactsAndKeepsCursor(t *testing.T) {
	var closes atomic.Int32
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			profile(w)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/affiliate/source-watermarks" {
			closes.Add(1)
			var m map[string]any
			json.Unmarshal(raw, &m)
			if m["fact_count"] != float64(1) || m["business_day"] != "2020-10-17" || m["facts_digest"] != "f0703e0171f088fd1d8985f04544281a8c877b2be7159778839a4a3c9f55817f" {
				t.Error("来源水位错误", m)
			}
			io.WriteString(w, `{"data":{"status":"SOURCE_CLOSED"}}`)
			return
		}
		var e Event
		json.Unmarshal(raw, &e)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"event_id": e.ID, "business_id": e.Business}})
	})
	ctx := context.Background()
	_, e := s.Record(ctx, "m", Input{Business: "historic", Kind: "DEPOSIT_SUCCEEDED", Amount: "10", At: "2020-10-17T12:00:00Z"}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CloseDays(ctx, 1); e == nil || closes.Load() != 0 {
		t.Fatal("未确认事实不可关账", e)
	}
	if _, e = s.Deliver(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.CloseDays(ctx, 1); e != nil {
		t.Fatal(e)
	}
	var day string
	if e = s.DB.QueryRow(`SELECT next_day FROM runner_state`).Scan(&day); e != nil || day != "2020-10-18" || closes.Load() != 1 {
		t.Fatal("水位未确认不得推进", day, e)
	}
}
