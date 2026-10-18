package ingressproof

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSignedSourceAndTampering(t *testing.T) {
	key := strings.Repeat("k", 64)
	for _, change := range []string{"valid", "ip", "body", "path", "expired", "missing"} {
		raw := `{ "amount":1 }`
		r := httptest.NewRequest("POST", "http://127.0.0.1:19986/api/action", strings.NewReader(raw))
		at := strconv.FormatInt(time.Now().Unix(), 10)
		ip := "192.0.2.10"
		hash := sha256.Sum256([]byte(raw))
		digest := hex.EncodeToString(hash[:])
		m := hmac.New(sha256.New, []byte(key))
		m.Write([]byte(strings.Join([]string{"web-entry-v1", r.Method, r.URL.RequestURI(), r.Host, ip, at, digest}, "\n")))
		r.Header.Set("X-Entry-Client-IP", ip)
		r.Header.Set("X-Entry-Time", at)
		r.Header.Set("X-Entry-Body-SHA256", digest)
		r.Header.Set("X-Entry-Signature", hex.EncodeToString(m.Sum(nil)))
		switch change {
		case "ip":
			r.Header.Set("X-Entry-Client-IP", "8.8.8.8")
		case "body":
			r.Body = io.NopCloser(strings.NewReader("{}"))
		case "path":
			r.URL.Path = "/api/other"
		case "expired":
			r.Header.Set("X-Entry-Time", "1000000000")
		case "missing":
			r.Header.Del("X-Entry-Signature")
		}
		err := Verify(r, key, 1024)
		if change == "valid" {
			if err != nil || ClientIP(r) != ip {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(r.Body)
			if string(got) != raw {
				t.Fatal("正文改变")
			}
		} else if err == nil {
			t.Fatal("未拒绝篡改", change)
		}
	}
}
