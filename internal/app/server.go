package app

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"ex-member-sandbox/internal/ingressproof"
	"ex-member-sandbox/internal/simulation"
	"io"
	"log/slog"
	driver "modernc.org/sqlite"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed web/*
var web embed.FS

type Session struct {
	MemberID string
	Expires  time.Time
}
type Server struct {
	Simulation      *simulation.Source
	Store           *Store
	Commission      *Commission
	PublicURL       string
	MemberPortalURL string
	mu              sync.Mutex
	sessions        map[string]Session
	attempts        map[string]attempt
}
type attempt struct {
	Count int
	Until time.Time
}

func NewServer(s *Store, c *Commission, publicURL string) *Server {
	return &Server{Store: s, Commission: c, PublicURL: publicURL, sessions: map[string]Session{}, attempts: map[string]attempt{}}
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求格式无效")
	}
	return nil
}
func hashPassword(password, salt string) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, []byte(salt), 600000, 32)
	return hex.EncodeToString(key), err
}
func (s *Server) session(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("member_session")
	if err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.sessions[cookie.Value]
	if ok && item.Expires.After(time.Now()) {
		return item.MemberID, true
	}
	delete(s.sessions, cookie.Value)
	return "", false
}
func (s *Server) login(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 清理过期本地会话，限制内存增长；重新登录撤销当前浏览器旧会话。
	for key, v := range s.sessions {
		if time.Now().After(v.Expires) {
			delete(s.sessions, key)
		}
	}
	if c, err := r.Cookie("member_session"); err == nil {
		delete(s.sessions, c.Value)
	}
	token := ID()
	s.sessions[token] = Session{MemberID: id, Expires: time.Now().Add(8 * time.Hour)}
	http.SetCookie(w, &http.Cookie{Name: "member_session", Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(ingressproof.PublicOrigin(r, s.PublicURL), "https://"), SameSite: http.SameSiteStrictMode, MaxAge: 28800})
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", ID())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		// 启用入口模式时必须先验证网关来源，未启用时保留原有直连兼容。
		if err := ingressproof.Verify(r, os.Getenv("WEB_ENTRY_KEY"), 1<<20); err != nil {
			fail(w, 403, "请求须经受信网关入口")
			return
		}
		// 固定本机Host且所有写入必须同源，防止恶意网页借浏览器访问本地服务。
		expected, _ := url.Parse(ingressproof.PublicOrigin(r, s.PublicURL))
		if r.Host != expected.Host {
			fail(w, 403, "请求主机不匹配")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Scheme != expected.Scheme || origin.Host != expected.Host {
				fail(w, 403, "请求来源不匹配")
				return
			}
		}
		// 模拟业务路由仍经过既有Host和Origin校验，身份限制由独立处理器执行。
		if s.handleSimulation(w, r) {
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			reply(w, 200, map[string]string{"status": "ok", "mode": "local-sandbox"})
		case "GET /readyz":
			if s.Store.DB.PingContext(r.Context()) != nil {
				fail(w, 503, "本地数据库暂不可用")
			} else {
				reply(w, 200, map[string]string{"status": "ready"})
			}
		case "POST /api/register":
			s.register(w, r)
		case "POST /api/login":
			s.authenticate(w, r)
		case "POST /api/logout":
			if c, err := r.Cookie("member_session"); err == nil {
				s.mu.Lock()
				delete(s.sessions, c.Value)
				s.mu.Unlock()
			}
			http.SetCookie(w, &http.Cookie{Name: "member_session", Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(ingressproof.PublicOrigin(r, s.PublicURL), "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
			reply(w, 200, map[string]bool{"ok": true})
		case "GET /api/member-portal":
			if _, ok := s.session(r); !ok {
				fail(w, 401, "请先注册或登录")
				return
			}
			portal := s.MemberPortalURL
			if strings.HasPrefix(ingressproof.PublicOrigin(r, s.PublicURL), "https://") {
				if configured := ingressproof.ConfiguredHTTPS("WEB_MEMBER_PORTAL_URL"); configured != "" {
					portal = configured + "/"
				}
			}
			reply(w, 200, map[string]string{"url": portal})
		case "GET /api/me":
			id, ok := s.session(r)
			if !ok {
				fail(w, 401, "请先注册或登录")
				return
			}
			m, err := s.Store.Member(r.Context(), id)
			if err != nil {
				fail(w, 503, "会员信息暂不可用")
				return
			}
			reply(w, 200, m)
		case "GET /api/members":
			if _, ok := s.session(r); !ok {
				fail(w, 401, "请先登录")
				return
			}
			page, size, explicit, pageErr := pageRequest(r)
			if pageErr != nil {
				fail(w, 400, pageErr.Error())
				return
			}
			var params []int
			if explicit {
				params = []int{size + 1, (page - 1) * size}
			}
			items, err := s.Store.Members(r.Context(), params...)
			if err != nil {
				fail(w, 503, "会员列表暂不可用")
				return
			}
			if explicit {
				more := len(items) > size
				if more {
					items = items[:size]
				}
				reply(w, 200, map[string]any{"items": items, "page": page, "page_size": size, "has_more": more})
			} else {
				reply(w, 200, map[string]any{"items": items})
			}
		case "POST /api/invitation":
			id, ok := s.session(r)
			if !ok {
				fail(w, 401, "请先登录")
				return
			}
			m, err := s.Store.Member(r.Context(), id)
			if err != nil {
				fail(w, 503, "会员信息暂不可用")
				return
			}
			link, err := s.Commission.Invitation(r.Context(), m, ingressproof.PublicOrigin(r, s.PublicURL)+"/")
			if err != nil {
				fail(w, 409, err.Error())
				return
			}
			reply(w, 200, map[string]string{"invitation_url": link})
		case "GET /", "GET /app.js", "GET /style.css":
			name := strings.TrimPrefix(r.URL.Path, "/")
			kind := "text/html; charset=utf-8"
			if name == "" {
				name = "index.html"
			}
			if name == "app.js" {
				kind = "text/javascript; charset=utf-8"
			}
			if name == "style.css" {
				kind = "text/css; charset=utf-8"
			}
			body, err := web.ReadFile("web/" + name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", kind)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	})
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email      string  `json:"email"`
		Name       string  `json:"display_name"`
		Password   string  `json:"password"`
		Invitation *string `json:"invitation_code"`
	}
	if decode(w, r, &input) != nil {
		fail(w, 400, "注册请求格式无效")
		return
	}
	address, err := mail.ParseAddress(input.Email)
	if err != nil || address.Address != input.Email || len(input.Email) > 254 || utf8.RuneCountInString(strings.TrimSpace(input.Name)) < 1 || utf8.RuneCountInString(input.Name) > 64 || len(input.Password) < 10 || len(input.Password) > 128 || input.Invitation != nil && (len(*input.Invitation) < 1 || len(*input.Invitation) > 128) {
		fail(w, 400, "请填写有效邮箱、姓名及10至128位密码")
		return
	}
	var salt [16]byte
	if _, err = rand.Read(salt[:]); err != nil {
		fail(w, 503, "注册暂不可用")
		return
	}
	saltText := hex.EncodeToString(salt[:])
	hash, err := hashPassword(input.Password, saltText)
	if err != nil {
		fail(w, 503, "注册暂不可用")
		return
	}
	m, err := s.Store.Register(r.Context(), input.Email, input.Name, hash, saltText, s.Commission.Source, input.Invitation)
	if err != nil {
		var sqlite *driver.Error
		if errors.As(err, &sqlite) && sqlite.Code() == 2067 {
			fail(w, 409, "邮箱已注册，请登录，原邀请归属不会覆盖")
			return
		}
		slog.Error("本地会员注册失败", "request_id", w.Header().Get("X-Request-ID"), "error", err)
		fail(w, 503, "注册暂不可用，请稍后重试")
		return
	}
	s.login(w, r, m.ID)
	slog.Info("本地会员注册完成，事件已持久化", "request_id", w.Header().Get("X-Request-ID"), "member_id", m.ID)
	reply(w, 201, m)
}
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if decode(w, r, &input) != nil || len(input.Password) > 128 || len(input.Email) > 254 {
		fail(w, 400, "登录请求格式无效")
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	s.mu.Lock()
	// 清理过期登录限速记录并设置本机沙箱容量上限，避免任意邮箱输入持续占用内存。
	for key, value := range s.attempts {
		if value.Until.Before(time.Now()) {
			delete(s.attempts, key)
		}
	}
	if len(s.attempts) >= 4096 {
		s.mu.Unlock()
		fail(w, 429, "登录请求过多，请稍后重试")
		return
	}
	a := s.attempts[email]
	if a.Until.Before(time.Now()) {
		a = attempt{Until: time.Now().Add(time.Minute)}
	}
	a.Count++
	s.attempts[email] = a
	s.mu.Unlock()
	if a.Count > 10 {
		fail(w, 429, "登录尝试过于频繁，请稍后重试")
		return
	}
	var id, hash, salt string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,password_hash,salt FROM members WHERE email=?`, email).Scan(&id, &hash, &salt)
	if errors.Is(err, sql.ErrNoRows) {
		salt = "missing-account-salt"
		hash = "missing-account"
	} else if err != nil {
		fail(w, 503, "登录暂不可用")
		return
	}
	actual, err := hashPassword(input.Password, salt)
	if err != nil || !hmac.Equal([]byte(actual), []byte(hash)) {
		fail(w, 401, "邮箱或密码错误")
		return
	}
	s.mu.Lock()
	delete(s.attempts, email)
	s.mu.Unlock()
	s.login(w, r, id)
	m, err := s.Store.Member(r.Context(), id)
	if err != nil {
		fail(w, 503, "会员信息暂不可用")
		return
	}
	reply(w, 200, m)
}

func Serve(ctx context.Context, addr string, s *Server, onReady ...func(context.Context) func()) error {
	server := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	go s.Commission.Run(ctx, s.Store)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	for _, hook := range onReady {
		if hook != nil {
			if closeRegistration := hook(ctx); closeRegistration != nil {
				defer closeRegistration()
			}
		}
	}
	slog.Info("临时会员服务已启动", "address", addr)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
