package ingressproof

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// 只有来源证明验签成功才允许额外HTTPS域名；机器和旧直连默认仍使用原固定来源。
func PublicOrigin(r *http.Request, fallback string) string {
	fallback = strings.TrimSuffix(fallback, "/")
	if _, ok := r.Context().Value(sourceKey{}).(string); !ok {
		return fallback
	}
	raw := os.Getenv("WEB_PUBLIC_ORIGINS")
	if len(raw) > 8192 {
		return fallback
	}
	var values []string
	if json.Unmarshal([]byte(raw), &values) != nil || len(values) > 16 {
		return fallback
	}
	for _, value := range values {
		u, e := url.Parse(value)
		if e == nil && u.Scheme == "https" && u.User == nil && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && u.Host == r.Host {
			return value
		}
	}
	return fallback
}
func ConfiguredHTTPS(name string) string {
	raw := os.Getenv(name)
	u, e := url.Parse(raw)
	if e == nil && u.Scheme == "https" && u.User == nil && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == "" {
		return raw
	}
	return ""
}
