package simulation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReversalOwnerLimitAndSignedDelivery(t *testing.T) {
	received := 0
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			profile(w)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var event Event
		if json.Unmarshal(raw, &event) != nil {
			t.Fatal("事件格式错误")
		}
		if reversalKind(event.Kind) {
			if r.URL.Path != "/v1/affiliate/reversals" {
				t.Error(r.URL.Path)
			}
			mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
			mac.Write([]byte("affiliate-reversal-v1\nsite\ncommission\nsandbox\n"))
			mac.Write(raw)
			if r.Header.Get("X-Affiliate-Signature") != hex.EncodeToString(mac.Sum(nil)) {
				t.Error("冲正签名错误")
			}
			received++
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"event_id": event.ID, "business_id": event.Business}})
	})
	ctx := context.Background()
	registered := time.Now().Add(-time.Hour)
	original, e := s.Record(ctx, "member", Input{Kind: "DEPOSIT_SUCCEEDED", Business: "deposit", Amount: "10.00"}, registered)
	if e != nil {
		t.Fatal(e)
	}
	input := Input{Kind: "DEPOSIT_REFUNDED", Business: "refund1", OriginalBusiness: "deposit", Amount: "4.00", Reason: "模拟退款"}
	if _, e = s.Record(ctx, "other", input, registered); e == nil {
		t.Fatal("允许跨会员退款")
	}
	r, e := s.Record(ctx, "member", input, registered)
	if e != nil || r.OriginalID != original.ID || r.Amount != "400" {
		t.Fatal(r, e)
	}
	again, e := s.Record(ctx, "member", input, registered)
	if e != nil || again.ID != r.ID {
		t.Fatal("冲正未保持原始身份", e)
	}
	input.Business = "over"
	input.Amount = "7.00"
	if _, e = s.Record(ctx, "member", input, registered); e == nil {
		t.Fatal("超额退款成功")
	}
	input.Business = "refund2"
	input.Amount = "6.00"
	if _, e = s.Record(ctx, "member", input, registered); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if ok, e := s.Deliver(ctx); e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	if received != 2 {
		t.Fatal("未投递两笔部分退款", received)
	}
}

func TestPendingReversalBlocksCloseWithoutChangingManifest(t *testing.T) {
	closes := 0
	s := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			profile(w)
			return
		}
		if r.URL.Path == "/v1/affiliate/source-watermarks" {
			closes++
			w.Write([]byte(`{"data":{"status":"SOURCE_CLOSED"}}`))
			return
		}
		w.WriteHeader(503)
	})
	ctx := context.Background()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	original, e := s.Record(ctx, "member", Input{Kind: "DEPOSIT_SUCCEEDED", Business: "original-day", Amount: "10.00", At: at.Format(time.RFC3339Nano)}, at.Add(-time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Exec(`UPDATE facts SET state='DELIVERED' WHERE business_id=?`, original.Business)
	_, e = s.Record(ctx, "member", Input{Kind: "DEPOSIT_REFUNDED", Business: "refund-day", OriginalBusiness: original.Business, Amount: "1.00", Reason: "退款", At: at.Add(time.Minute).Format(time.RFC3339Nano)}, at.Add(-time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CloseDays(ctx, 1); e == nil || closes != 0 {
		t.Fatal("未确认退款仍关闭来源日期", e, closes)
	}
	s.DB.Exec(`UPDATE facts SET state='DELIVERED' WHERE business_id='refund-day'`)
	if e = s.CloseDays(ctx, 1); e != nil || closes != 1 {
		t.Fatal("确认退款后无法按原成功事实关账", e, closes)
	}
}
