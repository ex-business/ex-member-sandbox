package app

import (
	"context"
	"encoding/json"
	"errors"
	"ex-member-sandbox/internal/simulation"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadSimulation只读取私有来源配置，不能通过网页指定上游、凭据、站点或数据库路径。
func LoadSimulation(path, memberDB string, c *Commission, store *Store) (*simulation.Source, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return nil, errors.New("模拟配置必须为私有普通文件，权限0600")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var cfg simulation.Config
	if e = json.Unmarshal(raw, &cfg); e != nil {
		return nil, errors.New("模拟来源配置格式无效")
	}
	memberPath, e := filepath.Abs(memberDB)
	if e != nil {
		return nil, e
	}
	sourceMember, e := filepath.Abs(cfg.Members)
	if e != nil {
		return nil, e
	}
	if memberPath != sourceMember || cfg.URL != c.URL || cfg.Tenant != c.Tenant || cfg.App != c.App || cfg.Source != c.Source {
		return nil, errors.New("模拟来源与会员作用域不一致")
	}
	source, e := simulation.Open(cfg)
	if e != nil {
		return nil, e
	}
	// 注册事实沿用原outbox，仅新增本模块自己的人工补发审计表，更新和审计同事务。
	if _, e = store.DB.Exec(`CREATE TABLE IF NOT EXISTS simulation_registration_actions(id TEXT PRIMARY KEY,event_id TEXT NOT NULL,member_id TEXT NOT NULL,reason TEXT NOT NULL,created_at TEXT NOT NULL)`); e != nil {
		source.Close()
		return nil, e
	}
	return source, nil
}
func (s *Server) handleSimulation(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/simulation" && r.URL.Path != "/simulation.js" && !strings.HasPrefix(r.URL.Path, "/api/simulation/") {
		return false
	}
	if s.Simulation == nil {
		http.NotFound(w, r)
		return true
	}
	member, ok := s.session(r)
	if !ok {
		if r.Method == "GET" && r.URL.Path == "/simulation" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			fail(w, 401, "请先登录会员")
		}
		return true
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /simulation", "GET /simulation.js":
		name := "simulation.html"
		kind := "text/html; charset=utf-8"
		if r.URL.Path == "/simulation.js" {
			name = "simulation.js"
			kind = "text/javascript; charset=utf-8"
		}
		raw, e := web.ReadFile("web/" + name)
		if e != nil {
			http.NotFound(w, r)
			return true
		}
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(raw)
	case "GET /api/simulation/status":
		reply(w, 200, map[string]bool{"enabled": true})
	case "GET /api/simulation/profile":
		profile, e := s.Simulation.Profile(r.Context())
		if e != nil {
			fail(w, 503, e.Error())
			break
		}
		reply(w, 200, profile)
	case "GET /api/simulation/events":
		page, size, explicit, pageErr := pageRequest(r)
		if pageErr != nil {
			fail(w, 400, pageErr.Error())
			break
		}
		var params []int
		factSize := size
		if explicit {
			offset := (page - 1) * size
			if page == 1 {
				factSize--
			} else {
				offset--
			}
			params = []int{factSize + 1, offset}
		}
		records, e := s.Simulation.Records(r.Context(), member, params...)
		if e != nil {
			fail(w, 503, "无法读取来源事件记录")
			break
		}
		var registration struct {
			ID       string `json:"event_id"`
			State    string `json:"state"`
			Attempts int    `json:"attempts"`
			Code     string `json:"last_code"`
			Next     int64  `json:"next_retry_at"`
		}
		e = s.Store.DB.QueryRowContext(r.Context(), `SELECT event_id,status,attempts,last_code,next_at FROM registration_outbox WHERE member_id=?`, member).Scan(&registration.ID, &registration.State, &registration.Attempts, &registration.Code, &registration.Next)
		if e != nil {
			fail(w, 503, "无法读取注册投递记录")
			break
		}
		more := false
		if explicit && len(records) > factSize {
			more = true
			records = records[:factSize]
		}
		reply(w, 200, map[string]any{"registration": registration, "items": records, "page": page, "page_size": size, "has_more": more})
	case "POST /api/simulation/events":
		var in simulation.Input
		if decode(w, r, &in) != nil {
			fail(w, 400, "模拟事件格式无效")
			break
		}
		current, e := s.Store.Member(r.Context(), member)
		if e != nil || current.Delivery != "SUCCEEDED" {
			fail(w, 409, "请先等待注册事件投递成功")
			break
		}
		registered, e := time.Parse(time.RFC3339Nano, current.RegisteredAt)
		if e != nil {
			fail(w, 503, "会员注册时间不可用")
			break
		}
		event, e := s.Simulation.Record(r.Context(), member, in, registered)
		if e != nil {
			fail(w, 409, e.Error())
			break
		}
		reply(w, 202, map[string]string{"event_id": event.ID, "business_id": event.Business, "status": "SAVED"})
	case "POST /api/simulation/resend":
		var in struct {
			Kind     string `json:"kind"`
			Business string `json:"business_id"`
			Reason   string `json:"reason"`
		}
		if decode(w, r, &in) != nil {
			fail(w, 400, "补发请求格式无效")
			break
		}
		var e error
		if in.Kind == "REGISTRATION" {
			e = s.resendRegistration(r.Context(), member, in.Reason)
		} else {
			e = s.Simulation.Resend(r.Context(), member, in.Kind, in.Business, in.Reason)
		}
		if e != nil {
			fail(w, 409, e.Error())
			break
		}
		slog.InfoContext(r.Context(), "来源事件已按原事实重新排队", "request_id", w.Header().Get("X-Request-ID"), "member_id", member, "kind", in.Kind, "business_id", in.Business)
		reply(w, 202, map[string]string{"status": "QUEUED"})
	default:
		http.NotFound(w, r)
	}
	return true
}
func (s *Server) resendRegistration(ctx context.Context, member, reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return errors.New("请填写补发原因")
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var event string
	e = tx.QueryRowContext(ctx, `SELECT event_id FROM registration_outbox WHERE member_id=? AND (status IN ('PENDING','BLOCKED') OR (status='PROCESSING' AND next_at<=?))`, member, time.Now().UnixMilli()).Scan(&event)
	if e != nil {
		return errors.New("注册事件正在投递或已经确认，无需补发")
	}
	_, e = tx.ExecContext(ctx, `UPDATE registration_outbox SET status='PENDING',next_at=0,lease_token=? WHERE member_id=?`, ID(), member)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO simulation_registration_actions VALUES(?,?,?,?,?)`, ID(), event, member, reason, time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
		return e
	}
	return tx.Commit()
}
