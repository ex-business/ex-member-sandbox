package ingressproof

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type sourceKey struct{}

func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(sourceKey{}).(string); ok {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// 仅由服务端私有配置启用；证明绑定原始字节和连接来源，不使用浏览器自报转发头。
func Verify(r *http.Request, key string, limit int64) error {
	if key == "" {
		return nil
	}
	ip, at, digest, signature := r.Header.Get("X-Entry-Client-IP"), r.Header.Get("X-Entry-Time"), r.Header.Get("X-Entry-Body-SHA256"), r.Header.Get("X-Entry-Signature")
	timestamp, e := strconv.ParseInt(at, 10, 64)
	delta := time.Now().Unix() - timestamp
	if len(key) < 32 || e != nil || delta > 30 || delta < -30 || net.ParseIP(ip) == nil || len(digest) != 64 || len(signature) != 64 {
		return errors.New("入口来源证明缺失或过期")
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if r.Body != nil {
		r.Body.Close()
	}
	if e != nil || int64(len(raw)) > limit {
		return errors.New("入口请求体无效或超限")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	hash := sha256.Sum256(raw)
	actual := hex.EncodeToString(hash[:])
	if actual != digest {
		return errors.New("入口正文摘要不匹配")
	}
	parts := []string{"web-entry-v1", r.Method, r.URL.RequestURI(), r.Host, ip, at, actual}
	for _, part := range parts {
		if strings.ContainsAny(part, "\r\n") {
			return errors.New("入口证明字段无效")
		}
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(strings.Join(parts, "\n")))
	got, e := hex.DecodeString(signature)
	if e != nil || !hmac.Equal(mac.Sum(nil), got) {
		return errors.New("入口来源证明验签失败")
	}
	*r = *r.WithContext(context.WithValue(r.Context(), sourceKey{}, ip))
	return nil
}
