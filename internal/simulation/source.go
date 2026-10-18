// Package simulation 实现仅供本地沙箱使用的来源事件箱，不修改真实充值、注单或钱包。
package simulation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	routersdk "github.com/ex-business/ex-service-router/sdk"
	"io"
	"log/slog"
	"math/big"
	_ "modernc.org/sqlite"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	URL      string `json:"url"`
	Token    string `json:"token"`
	Key      string `json:"key"`
	Tenant   string `json:"tenant"`
	App      string `json:"app"`
	Source   string `json:"source"`
	Members  string `json:"member_database"`
	Database string `json:"database"`
}
type Source struct {
	DB     *sql.DB
	Config Config
	Client *http.Client
	lock   *os.File
}
type Event struct {
	OriginalID       string    `json:"original_event_id,omitempty"`
	OriginalBusiness string    `json:"original_business_id,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	Source           string    `json:"source_id"`
	ID               string    `json:"event_id"`
	Business         string    `json:"business_id"`
	Member           string    `json:"external_user_id"`
	Kind             string    `json:"kind"`
	Currency         string    `json:"currency"`
	Amount           string    `json:"amount_minor"`
	ValidBet         string    `json:"valid_bet_minor"`
	Venue            string    `json:"venue_type_id"`
	At               time.Time `json:"business_time"`
}
type Input struct {
	OriginalBusiness string `json:"original_business_id,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Business         string `json:"business_id"`
	Kind             string `json:"kind"`
	Amount           string `json:"amount"`
	ValidBet         string `json:"valid_bet"`
	Venue            string `json:"venue_type_id"`
	At               string `json:"business_time"`
}
type Record struct {
	EventID  string `json:"event_id"`
	Business string `json:"business_id"`
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
	Amount   string `json:"amount_minor"`
	ValidBet string `json:"valid_bet_minor"`
	At       string `json:"business_time"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Code     string `json:"last_code"`
	Next     int64  `json:"next_retry_at"`
}
type Profile struct {
	Timezone string `json:"timezone"`
	Currency string `json:"currency"`
	Scale    int    `json:"currency_scale"`
}

func id() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

// Open兼容既有Python来源库与关账表；同一库与旧自动任务共用进程锁，禁止两个自动发送器并跑。
func Open(c Config) (*Source, error) {
	u, parseErr := url.Parse(c.URL)
	if parseErr != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("模拟来源仅允许固定本地回环地址")
	}
	memberPath, _ := filepath.Abs(c.Members)
	sourcePath, _ := filepath.Abs(c.Database)
	if sourceInfo, e := os.Stat(c.Database); e == nil {
		if memberInfo, e := os.Stat(c.Members); e == nil && os.SameFile(sourceInfo, memberInfo) {
			return nil, errors.New("来源事件库必须独立于会员数据库")
		}
	}
	if memberPath == sourcePath {
		return nil, errors.New("来源事件库必须独立于会员数据库")
	}
	if len(c.Key) < 32 || len(c.Token) < 32 || c.Tenant == "" || c.App == "" || c.Source == "" || c.Members == "" || c.Database == "" {
		return nil, errors.New("模拟来源配置不完整")
	}
	if err := os.MkdirAll(filepath.Dir(c.Database), 0700); err != nil {
		return nil, err
	}
	lock, e := os.OpenFile(c.Database+".runner.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("该来源自动任务已运行，请停止旧来源任务后启用页面模拟")
	}
	db, e := sql.Open("sqlite", c.Database+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if e != nil {
		lock.Close()
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Source{DB: db, Config: c, lock: lock, Client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS meta(fingerprint TEXT NOT NULL); CREATE TABLE IF NOT EXISTS facts(kind TEXT NOT NULL,business_id TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,last_code TEXT NOT NULL DEFAULT '',PRIMARY KEY(kind,business_id)); CREATE TABLE IF NOT EXISTS watermarks(day TEXT PRIMARY KEY,payload TEXT NOT NULL,state TEXT NOT NULL); CREATE TABLE IF NOT EXISTS simulation_delivery(kind TEXT NOT NULL,business_id TEXT NOT NULL,lease_token TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(kind,business_id)); CREATE TABLE IF NOT EXISTS simulation_profile(id INTEGER PRIMARY KEY CHECK(id=1),body TEXT NOT NULL); CREATE TABLE IF NOT EXISTS simulation_actions(id TEXT PRIMARY KEY,member_id TEXT NOT NULL,kind TEXT NOT NULL,business_id TEXT NOT NULL,reason TEXT NOT NULL,created_at TEXT NOT NULL);`)
	if e != nil {
		s.Close()
		return nil, e
	}
	raw, _ := json.Marshal([]string{c.Tenant, c.App, c.Source, c.Members})
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	var old string
	e = db.QueryRow(`SELECT fingerprint FROM meta LIMIT 1`).Scan(&old)
	if errors.Is(e, sql.ErrNoRows) {
		_, e = db.Exec(`INSERT INTO meta VALUES(?)`, fingerprint)
	} else if e == nil && old != fingerprint {
		e = errors.New("来源作用域已变化，禁止复用旧事件库")
	}
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Source) Close() error {
	e := s.DB.Close()
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
	}
	return e
}

// call只调用固定佣金地址和受控路径；保留原事件字节及稳定幂等键，不跟随重定向泄露凭据。
func (s *Source) call(ctx context.Context, path string, raw []byte) (int, []byte, error) {
	method := "GET"
	if raw != nil {
		method = "POST"
	}
	r, e := http.NewRequestWithContext(ctx, method, s.Config.URL+path, bytes.NewReader(raw))
	if e != nil {
		return 0, nil, e
	}
	r.Header.Set("Authorization", "Bearer "+s.Config.Token)
	r.Header.Set("Content-Type", "application/json")
	if raw != nil {
		prefix := "affiliate-business-v1"
		if path == "/v1/affiliate/reversals" {
			prefix = "affiliate-reversal-v1"
		}
		key := ""
		if path == "/v1/affiliate/source-watermarks" {
			prefix = "affiliate-watermark-v1"
			sum := sha256.Sum256(raw)
			key = hex.EncodeToString(sum[:])
		}
		var event Event
		if e = json.Unmarshal(raw, &event); e != nil {
			return 0, nil, e
		}
		if key == "" {
			key = event.ID
		}
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("X-Request-ID", key)
		mac := hmac.New(sha256.New, []byte(s.Config.Key))
		mac.Write([]byte(prefix + "\n" + s.Config.Tenant + "\n" + s.Config.App + "\n" + s.Config.Source + "\n"))
		mac.Write(raw)
		r.Header.Set("X-Affiliate-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	response, e := routersdk.Do(s.Client, r)
	if e != nil {
		return 0, nil, e
	}
	defer response.Body.Close()
	body, e := io.ReadAll(io.LimitReader(response.Body, 1048577))
	if len(body) > 1048576 {
		return 0, nil, errors.New("来源响应过大")
	}
	return response.StatusCode, body, e
}

// Profile缓存已确认的币种口径，使下游短暂不可用时来源仍能保存不可变事实。
func (s *Source) Profile(ctx context.Context) (Profile, error) {
	code, raw, e := s.call(ctx, "/v1/affiliate/config", nil)
	if e != nil || code != 200 {
		var cached string
		if err := s.DB.QueryRowContext(ctx, `SELECT body FROM simulation_profile WHERE id=1`).Scan(&cached); err == nil {
			var profile Profile
			if json.Unmarshal([]byte(cached), &profile) == nil {
				return profile, nil
			}
		}
		return Profile{}, errors.New("无法读取已发布配置，首次使用需先连接佣金系统")
	}
	var v struct {
		Data struct {
			Current *struct {
				Config Profile `json:"config"`
			} `json:"current"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Data.Current == nil || v.Data.Current.Config.Currency == "" || v.Data.Current.Config.Scale < 0 || v.Data.Current.Config.Scale > 8 {
		return Profile{}, errors.New("请先发布有效的站点代理配置")
	}
	profile := v.Data.Current.Config
	if profile.Timezone == "" {
		profile.Timezone = "UTC"
	}
	if _, e = time.LoadLocation(profile.Timezone); e != nil {
		return Profile{}, errors.New("站点时区无效")
	}
	body, _ := json.Marshal(profile)
	_, e = s.DB.ExecContext(ctx, `INSERT INTO simulation_profile VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, string(body))
	return profile, e
}

var moneyPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// Minor用十进制字符串转换，无浮点和隐式四舍五入，最多38位最小货币单位。
func Minor(v string, scale int) (string, error) {
	if scale < 0 || scale > 8 || len(v) > 48 || !moneyPattern.MatchString(v) {
		return "", errors.New("金额格式无效")
	}
	p := strings.Split(v, ".")
	fraction := ""
	if len(p) == 2 {
		fraction = p[1]
	}
	if len(fraction) > scale {
		return "", errors.New("金额小数位超出币种精度")
	}
	n := strings.TrimLeft(p[0]+fraction+strings.Repeat("0", scale-len(fraction)), "0")
	if n == "" {
		n = "0"
	}
	if len(n) > 38 {
		return "", errors.New("金额超出范围")
	}
	return n, nil
}
func (s *Source) Record(ctx context.Context, member string, in Input, registered time.Time) (Event, error) {
	if reversalKind(in.Kind) {
		return s.recordReversal(ctx, member, in, registered)
	}
	if member == "" || strings.TrimSpace(in.Business) != in.Business || len(in.Business) == 0 || len([]rune(in.Business)) > 128 || (in.Kind != "DEPOSIT_SUCCEEDED" && in.Kind != "BET_SETTLED") {
		return Event{}, errors.New("业务单号或事件类型无效")
	}
	profile, e := s.Profile(ctx)
	if e != nil {
		return Event{}, e
	}
	amount, e := Minor(in.Amount, profile.Scale)
	if e != nil || amount == "0" {
		return Event{}, errors.New("金额必须为正数且符合币种精度")
	}
	valid := "0"
	if in.Kind == "BET_SETTLED" {
		valid, e = Minor(in.ValidBet, profile.Scale)
		if e != nil || len(in.Venue) == 0 || len(in.Venue) > 128 {
			return Event{}, errors.New("投注需填写有效投注金额和场馆")
		}
		a, _ := new(big.Int).SetString(amount, 10)
		b, _ := new(big.Int).SetString(valid, 10)
		if b.Cmp(a) > 0 {
			return Event{}, errors.New("有效投注不能超过投注金额")
		}
	} else if (in.ValidBet != "" && in.ValidBet != "0") || in.Venue != "" {
		return Event{}, errors.New("充值事件不含有效投注或场馆")
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if in.At != "" {
		at, e = time.Parse(time.RFC3339Nano, in.At)
		if e != nil {
			return Event{}, errors.New("业务时间必须包含时区")
		}
		if at.Nanosecond()%1000 != 0 {
			return Event{}, errors.New("业务时间最多支持微秒精度")
		}
		at = at.UTC().Truncate(time.Microsecond)
	}
	if at.Before(registered) || at.After(time.Now().Add(time.Minute)) {
		return Event{}, errors.New("业务时间必须在注册后且不能晚于当前时间")
	}
	event := Event{Source: s.Config.Source, ID: id(), Business: in.Business, Member: member, Kind: in.Kind, Currency: profile.Currency, Amount: amount, ValidBet: valid, Venue: in.Venue, At: at}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return event, e
	}
	defer tx.Rollback()
	var oldRaw string
	e = tx.QueryRowContext(ctx, `SELECT payload FROM facts WHERE kind=? AND business_id=?`, in.Kind, in.Business).Scan(&oldRaw)
	if e == nil {
		var old Event
		if json.Unmarshal([]byte(oldRaw), &old) != nil {
			return event, errors.New("原始事件损坏")
		}
		event.ID = old.ID
		if in.At == "" {
			event.At = old.At
		}
		if event != old {
			return event, errors.New("同一订单不可覆盖会员、金额、场馆或业务时间")
		}
		return old, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return event, e
	}
	raw, e := json.Marshal(event)
	if e != nil {
		return event, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO facts(kind,business_id,payload,state) VALUES(?,?,?,'PENDING')`, event.Kind, event.Business, string(raw))
	if e != nil {
		return event, e
	}
	if e = tx.Commit(); e == nil {
		slog.InfoContext(ctx, "模拟来源事件已保存待投递", "request_id", event.ID, "event_id", event.ID, "business_id", event.Business, "kind", event.Kind, "member_id", member)
	}
	return event, e
}
func (s *Source) Records(ctx context.Context, member string, pagination ...int) ([]Record, error) {
	limit, offset := 100, 0
	if len(pagination) == 2 {
		limit, offset = pagination[0], pagination[1]
	}
	rows, e := s.DB.QueryContext(ctx, `SELECT payload,state,attempts,last_code,COALESCE(d.next_at,0) FROM facts f LEFT JOIN simulation_delivery d ON d.kind=f.kind AND d.business_id=f.business_id WHERE json_extract(f.payload,'$.external_user_id')=? ORDER BY f.rowid DESC LIMIT ? OFFSET ?`, member, limit, offset)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var raw, state, code string
		var attempts int
		var next int64
		if e = rows.Scan(&raw, &state, &attempts, &code, &next); e != nil {
			return nil, e
		}
		var v Event
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return nil, e
		}
		out = append(out, Record{v.ID, v.Business, v.Kind, v.Currency, v.Amount, v.ValidBet, v.At.Format(time.RFC3339Nano), state, attempts, code, next})
	}
	return out, rows.Err()
}

// Resend只重置投递状态，固定原始载荷、事件编号和签名密钥；不允许复发已确认事件或抢占有效租约。
func (s *Source) Resend(ctx context.Context, member, kind, business, reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return errors.New("请填写补发原因")
	}
	now := time.Now().UnixMilli()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `UPDATE facts SET state='PENDING' WHERE kind=? AND business_id=? AND json_extract(payload,'$.external_user_id')=? AND (state IN ('PENDING','BLOCKED') OR (state='PROCESSING' AND COALESCE((SELECT lease_until FROM simulation_delivery d WHERE d.kind=facts.kind AND d.business_id=facts.business_id),0)<=?))`, kind, business, member, now)
	if e != nil {
		return e
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("事件不存在、不属于当前会员、正在投递或已经确认")
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO simulation_delivery VALUES(?,?,'',0,0) ON CONFLICT(kind,business_id) DO UPDATE SET lease_token='',lease_until=0,next_at=0`, kind, business)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO simulation_actions VALUES(?,?,?,?,?,?)`, id(), member, kind, business, reason, time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
		return e
	}
	return tx.Commit()
}

type job struct {
	Event    Event
	Raw      []byte
	Lease    string
	Attempts int
}

func (s *Source) claim(ctx context.Context) (job, error) {
	var j job
	now := time.Now().UnixMilli()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return j, e
	}
	defer tx.Rollback()
	e = tx.QueryRowContext(ctx, `SELECT f.payload,f.attempts FROM facts f LEFT JOIN simulation_delivery d ON d.kind=f.kind AND d.business_id=f.business_id WHERE f.state IN ('PENDING','PROCESSING') AND COALESCE(d.lease_until,0)<=? AND COALESCE(d.next_at,0)<=? ORDER BY f.rowid LIMIT 1`, now, now).Scan(&j.Raw, &j.Attempts)
	if e != nil {
		return j, e
	}
	if e = json.Unmarshal(j.Raw, &j.Event); e != nil {
		return j, e
	}
	j.Lease = id()
	j.Attempts++
	_, e = tx.ExecContext(ctx, `INSERT INTO simulation_delivery VALUES(?,?,?,?,0) ON CONFLICT(kind,business_id) DO UPDATE SET lease_token=excluded.lease_token,lease_until=excluded.lease_until`, j.Event.Kind, j.Event.Business, j.Lease, now+30000)
	if e != nil {
		return j, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE facts SET state='PROCESSING',attempts=? WHERE kind=? AND business_id=?`, j.Attempts, j.Event.Kind, j.Event.Business); e != nil {
		return j, e
	}
	return j, tx.Commit()
}
func (s *Source) Deliver(ctx context.Context) (bool, error) {
	j, e := s.claim(ctx)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	path := "/v1/affiliate/business-facts"
	if reversalKind(j.Event.Kind) {
		path = "/v1/affiliate/reversals"
	}
	status, raw, callErr := s.call(ctx, path, j.Raw)
	state, code := "PENDING", "DEPENDENCY_UNAVAILABLE"
	if callErr == nil {
		code = fmt.Sprintf("UPSTREAM_%d", status)
		if status == 200 {
			var v struct {
				Data struct {
					EventID  string `json:"event_id"`
					Business string `json:"business_id"`
					Late     bool   `json:"late_adjustment"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &v) == nil && v.Data.EventID == j.Event.ID && v.Data.Business == j.Event.Business {
				state = "DELIVERED"
				if v.Data.Late {
					state = "LATE_ADJUSTMENT"
				}
				code = ""
			} else {
				code = "INVALID_UPSTREAM_RESPONSE"
			}
		} else if status >= 400 && status < 500 && status != 408 && status != 429 {
			state = "BLOCKED"
		}
	}
	// 不保留上游原始错误正文，避免响应意外包含凭据；仅记录受控错误码。
	if state == "BLOCKED" {
		var v struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &v) == nil && regexp.MustCompile(`^[A-Z0-9_]{1,80}$`).MatchString(v.Error.Code) {
			code = v.Error.Code
		}
	}
	delay := time.Second * 30
	for i := 1; i < j.Attempts && delay < 15*time.Minute; i++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	return true, s.finish(ctx, j, state, code, delay)
}

// finish只接受当前租约结果，旧请求返回时不能覆盖接管后的投递状态。
func (s *Source) finish(ctx context.Context, j job, state, code string, delay time.Duration) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var lease string
	e = tx.QueryRowContext(ctx, `SELECT lease_token FROM simulation_delivery WHERE kind=? AND business_id=?`, j.Event.Kind, j.Event.Business).Scan(&lease)
	if e != nil {
		return e
	}
	if lease != j.Lease {
		return nil
	}
	_, e = tx.ExecContext(ctx, `UPDATE facts SET state=?,last_code=? WHERE kind=? AND business_id=?`, state, code, j.Event.Kind, j.Event.Business)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE simulation_delivery SET lease_token='',lease_until=0,next_at=? WHERE kind=? AND business_id=?`, time.Now().Add(delay).UnixMilli(), j.Event.Kind, j.Event.Business)
	if e != nil {
		return e
	}
	if e = tx.Commit(); e == nil {
		slog.InfoContext(ctx, "模拟来源事件投递完成", "request_id", j.Event.ID, "event_id", j.Event.ID, "business_id", j.Event.Business, "attempt", j.Attempts, "state", state, "result_code", code)
	}
	return e
}
func (s *Source) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := time.Time{}
	lastClose := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 20; i++ {
				worked, e := s.Deliver(ctx)
				if e != nil {
					if ctx.Err() == nil && time.Since(lastError) > 30*time.Second {
						slog.ErrorContext(ctx, "模拟来源投递异常，等待原事件重试", "error", e)
						lastError = time.Now()
					}
					break
				}
				if !worked {
					break
				}
			}
			if time.Since(lastClose) >= 5*time.Second {
				lastClose = time.Now()
				if e := s.CloseDays(ctx, 31); e != nil && ctx.Err() == nil && time.Since(lastError) > 30*time.Second {
					slog.WarnContext(ctx, "模拟来源关账等待处理", "error", e)
					lastError = time.Now()
				}
			}
		}
	}
}

// CloseDays复用原来源的runner_state、水位及摘要协议；存在未确认事实时不推进游标。
// 与造数共用SQLite事务锁，发送清单至确认之间不允许插入新事实，迟到事实由佣金侧原流程处理。
func (s *Source) CloseDays(ctx context.Context, limit int) error {
	profile, e := s.Profile(ctx)
	if e != nil {
		return e
	}
	zone, e := time.LoadLocation(profile.Timezone)
	if e != nil {
		return e
	}
	if _, e = s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS runner_state(id INTEGER PRIMARY KEY CHECK(id=1),next_day TEXT NOT NULL,timezone TEXT NOT NULL)`); e != nil {
		return e
	}
	today := time.Now().In(zone).Format("2006-01-02")
	for i := 0; i < limit; i++ {
		tx, e := s.DB.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		day, err := s.closeDay(ctx, tx, zone, today)
		if err != nil {
			tx.Rollback()
			return err
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		if day == "" {
			return nil
		}
		slog.InfoContext(ctx, "模拟来源业务日已完成关账", "source_id", s.Config.Source, "business_day", day)
	}
	return nil
}
func (s *Source) closeDay(ctx context.Context, tx *sql.Tx, zone *time.Location, today string) (string, error) {
	// 先取得写锁，防止读取清单之后另一请求插入事实导致提前关账。
	if _, e := tx.ExecContext(ctx, `UPDATE runner_state SET next_day=next_day WHERE id=1`); e != nil {
		return "", e
	}
	var day, storedZone string
	e := tx.QueryRowContext(ctx, `SELECT next_day,timezone FROM runner_state WHERE id=1`).Scan(&day, &storedZone)
	if errors.Is(e, sql.ErrNoRows) {
		day = today
		rows, err := tx.QueryContext(ctx, `SELECT payload FROM facts`)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return "", err
			}
			var event Event
			if err = json.Unmarshal([]byte(raw), &event); err != nil {
				rows.Close()
				return "", err
			}
			d := event.At.In(zone).Format("2006-01-02")
			if d < day {
				day = d
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO runner_state VALUES(1,?,?)`, day, zone.String()); err != nil {
			return "", err
		}
	} else if e != nil {
		return "", e
	} else if storedZone != zone.String() {
		return "", errors.New("站点时区变化，需核对历史业务日后迁移来源游标")
	}
	if day >= today {
		return "", nil
	}
	var existing string
	e = tx.QueryRowContext(ctx, `SELECT payload FROM watermarks WHERE day=?`, day).Scan(&existing)
	if errors.Is(e, sql.ErrNoRows) {
		parts := []string{}
		rows, e := tx.QueryContext(ctx, `SELECT payload,state FROM facts`)
		if e != nil {
			return "", e
		}
		for rows.Next() {
			var raw, state string
			if e = rows.Scan(&raw, &state); e != nil {
				rows.Close()
				return "", e
			}
			var event Event
			if e = json.Unmarshal([]byte(raw), &event); e != nil {
				rows.Close()
				return "", e
			}
			if event.At.In(zone).Format("2006-01-02") != day {
				continue
			}
			if state != "DELIVERED" && state != "LATE_ADJUSTMENT" {
				rows.Close()
				return "", errors.New("该业务日仍有未确认事件，等待投递或人工处理")
			}
			// 当天已知冲正须先确认后才可关账，但不进入成功事实摘要，原清单身份保持不变。
			if reversalKind(event.Kind) {
				continue
			}
			if state == "DELIVERED" {
				fields := []string{event.Source, event.Kind, event.Business, event.Member, event.Currency, event.Amount, event.ValidBet, event.Venue, event.At.UTC().Format(time.RFC3339Nano)}
				var risk struct {
					Risk *struct {
						IP      string `json:"ip_hash"`
						Device  string `json:"device_hash"`
						Account string `json:"account_hash"`
					} `json:"risk_identity"`
				}
				if e = json.Unmarshal([]byte(raw), &risk); e != nil {
					rows.Close()
					return "", e
				}
				if risk.Risk != nil {
					fields = append(fields, "risk-v1", risk.Risk.IP, risk.Risk.Device, risk.Risk.Account)
				}
				b, _ := json.Marshal(fields)
				hash := sha256.Sum256(b)
				parts = append(parts, event.Kind+"\n"+event.Business+"\n"+hex.EncodeToString(hash[:]))
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", e
		}
		sort.Strings(parts)
		raw, _ := json.Marshal(parts)
		hash := sha256.Sum256(raw)
		watermark := struct {
			Source string `json:"source_id"`
			Day    string `json:"business_day"`
			Count  int    `json:"fact_count"`
			Digest string `json:"facts_digest"`
		}{s.Config.Source, day, len(parts), hex.EncodeToString(hash[:])}
		raw, _ = json.Marshal(watermark)
		code, response, e := s.call(ctx, "/v1/affiliate/source-watermarks", raw)
		if e != nil || code != 200 {
			return "", errors.New("来源关账尚未被佣金系统确认")
		}
		var receipt struct {
			Data struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		if json.Unmarshal(response, &receipt) != nil || receipt.Data.Status != "SOURCE_CLOSED" {
			return "", errors.New("来源关账回执无效")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO watermarks VALUES(?,?,'SOURCE_CLOSED')`, day, string(raw)); e != nil {
			return "", e
		}
	} else if e != nil {
		return "", e
	}
	date, e := time.ParseInLocation("2006-01-02", day, zone)
	if e != nil {
		return "", e
	}
	_, e = tx.ExecContext(ctx, `UPDATE runner_state SET next_day=? WHERE id=1`, date.AddDate(0, 0, 1).Format("2006-01-02"))
	return day, e
}
