package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity 独立提供佣金接入所需的身份验证，不把会员密码或来源管理凭据发给浏览器。
// 仅由 cmd 在单独的回环端口挂载；每次内省都查询令牌存储，撤销后立即失效。
type Identity struct {
	Store                                                   *Store
	ClientID, Secret, Issuer, Audience, Tenant, App, Source string
	Now                                                     func() time.Time
	mu                                                      sync.Mutex
	attempts                                                map[string]attempt
}

func (i *Identity) Prepare(ctx context.Context) error {
	if i.ClientID == "" || len(i.Secret) < 32 || i.Issuer == "" || i.Audience == "" || i.Tenant == "" || i.App == "" || i.Source == "" {
		return errors.New("会员身份来源配置不完整")
	}
	_, err := i.Store.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS identity_tokens(token_hash TEXT PRIMARY KEY,member_id TEXT NOT NULL REFERENCES members(id),expires_at INTEGER NOT NULL)`)
	if i.Now == nil {
		i.Now = time.Now
	}
	i.attempts = map[string]attempt{}
	return err
}
func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func (i *Identity) allow(email string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.Now()
	for k, v := range i.attempts {
		if !now.Before(v.Until) {
			delete(i.attempts, k)
		}
	}
	// 全局容量及速率限制同时约束未知邮箱，防止随机输入耗尽密码派生和内存资源。
	if len(i.attempts) >= 4096 {
		return false
	}
	for _, key := range []string{"global", "email:" + email} {
		a := i.attempts[key]
		if !now.Before(a.Until) {
			a = attempt{Until: now.Add(time.Minute)}
		}
		a.Count++
		i.attempts[key] = a
		limit := 10
		if key == "global" {
			limit = 60
		}
		if a.Count > limit {
			return false
		}
	}
	return true
}
func (i *Identity) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", ID())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		user, secret, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(i.ClientID)) != 1 || subtle.ConstantTimeCompare([]byte(secret), []byte(i.Secret)) != 1 {
			fail(w, 401, "身份接入凭据无效")
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/integration/login":
			var in struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if decode(w, r, &in) != nil || len(in.Username) > 254 || len(in.Password) > 128 || in.Password == "" {
				fail(w, 400, "登录请求无效")
				return
			}
			email := strings.ToLower(strings.TrimSpace(in.Username))
			if !i.allow(email) {
				fail(w, 429, "登录尝试过于频繁")
				return
			}
			var id, hash, salt string
			err := i.Store.DB.QueryRowContext(r.Context(), `SELECT id,password_hash,salt FROM members WHERE email=?`, email).Scan(&id, &hash, &salt)
			missing := errors.Is(err, sql.ErrNoRows)
			if err != nil && !missing {
				fail(w, 503, "会员身份来源暂不可用")
				return
			}
			if missing {
				salt = "missing-account-salt"
				hash = "missing-account"
			}
			actual, err := hashPassword(in.Password, salt)
			if err != nil || subtle.ConstantTimeCompare([]byte(actual), []byte(hash)) != 1 || missing {
				fail(w, 401, "邮箱或密码错误")
				return
			}
			// 必须等可信注册事件完成，避免来源已注册但佣金侧尚无身份时发放可用凭据。
			m, err := i.Store.Member(r.Context(), id)
			if err != nil || m.Delivery != "SUCCEEDED" {
				fail(w, 409, "注册身份尚未同步完成，请稍后重试")
				return
			}
			token := "ms_" + ID()
			expires := i.Now().Add(time.Hour).Unix()
			tx, err := i.Store.DB.BeginTx(r.Context(), nil)
			if err != nil {
				fail(w, 503, "身份接入暂不可用")
				return
			}
			defer tx.Rollback()
			_, err = tx.ExecContext(r.Context(), `DELETE FROM identity_tokens WHERE expires_at<=?`, i.Now().Unix())
			var count int
			if err == nil {
				err = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM identity_tokens`).Scan(&count)
			}
			if err != nil || count >= 10000 {
				fail(w, 503, "身份接入暂不可用")
				return
			}
			if _, err = tx.ExecContext(r.Context(), `INSERT INTO identity_tokens VALUES(?,?,?)`, tokenHash(token), id, expires); err != nil {
				fail(w, 503, "身份接入暂不可用")
				return
			}
			if err = tx.Commit(); err != nil {
				fail(w, 503, "身份接入暂不可用")
				return
			}
			// 代理展示登录审计与凭据事务隔离；审计异常不改变登录结果，也不输出令牌。
			if auditErr := i.Store.RecordAffiliateLogin(r.Context(), id, r.RemoteAddr, i.Now()); auditErr != nil {
				slog.Warn("代理登录展示审计未完成", "request_id", w.Header().Get("X-Request-ID"), "member_id", id, "error", auditErr)
			}
			slog.Info("会员身份接入登录成功", "request_id", w.Header().Get("X-Request-ID"), "member_id", id, "result", "SUCCEEDED")
			reply(w, 200, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
		case "/integration/introspect", "/integration/revoke":
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if r.ParseForm() != nil || len(r.Form.Get("token")) > 256 {
				fail(w, 400, "令牌请求无效")
				return
			}
			token := r.Form.Get("token")
			if r.URL.Path == "/integration/revoke" {
				if _, err := i.Store.DB.ExecContext(r.Context(), `DELETE FROM identity_tokens WHERE token_hash=?`, tokenHash(token)); err != nil {
					fail(w, 503, "撤销暂不可用")
					return
				}
				reply(w, 200, map[string]bool{"ok": true})
				return
			}
			var id string
			var exp int64
			err := i.Store.DB.QueryRowContext(r.Context(), `SELECT t.member_id,t.expires_at FROM identity_tokens t JOIN registration_outbox o ON o.member_id=t.member_id WHERE t.token_hash=? AND o.status='SUCCEEDED'`, tokenHash(token)).Scan(&id, &exp)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				fail(w, 503, "身份内省暂不可用")
				return
			}
			if err != nil || exp <= i.Now().Unix() || !strings.HasPrefix(token, "ms_") {
				reply(w, 200, map[string]bool{"active": false})
				return
			}
			reply(w, 200, map[string]any{"active": true, "sub": id, "iss": i.Issuer, "aud": i.Audience, "exp": exp, "tenant_id": i.Tenant, "app_id": i.App, "source_id": i.Source})
		default:
			http.NotFound(w, r)
		}
	})
}
