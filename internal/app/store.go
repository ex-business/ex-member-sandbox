package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"strings"
	"time"
)

type Store struct{ DB *sql.DB }
type Member struct {
	ID           string  `json:"member_id"`
	Email        string  `json:"email,omitempty"`
	Name         string  `json:"display_name"`
	RegisteredAt string  `json:"registered_at"`
	Delivery     string  `json:"delivery_status"`
	AgentID      string  `json:"agent_id"`
	ParentID     *string `json:"parent_agent_id"`
	Reason       string  `json:"reason"`
}

type RegistrationEvent struct {
	SchemaVersion  string  `json:"schema_version"`
	SourceID       string  `json:"source_id"`
	EventID        string  `json:"event_id"`
	ExternalUserID string  `json:"external_user_id"`
	DisplayName    string  `json:"display_name"`
	RegisteredAt   string  `json:"registered_at"`
	InvitationCode *string `json:"invitation_code"`
}

func ID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// 会员和待投递事件共用本地事务，数据库文件独立于佣金系统，重启后仍可续投。
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS members(id TEXT PRIMARY KEY,email TEXT NOT NULL UNIQUE,name TEXT NOT NULL,password_hash TEXT NOT NULL,salt TEXT NOT NULL,registered_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS registration_outbox(event_id TEXT PRIMARY KEY,member_id TEXT NOT NULL UNIQUE REFERENCES members(id),payload BLOB NOT NULL,status TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL,lease_token TEXT NOT NULL DEFAULT '',agent_id TEXT NOT NULL DEFAULT '',parent_id TEXT,last_code TEXT NOT NULL DEFAULT '');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Register(ctx context.Context, email, name, passwordHash, salt, source string, invitation *string) (Member, error) {
	at := time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	member := Member{ID: ID(), Email: strings.ToLower(strings.TrimSpace(email)), Name: strings.TrimSpace(name), RegisteredAt: at, Delivery: "PENDING"}
	event := RegistrationEvent{SchemaVersion: "1", SourceID: source, EventID: ID(), ExternalUserID: member.ID, DisplayName: member.Name, RegisteredAt: at, InvitationCode: invitation}
	raw, err := json.Marshal(event)
	if err != nil {
		return member, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return member, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO members VALUES(?,?,?,?,?,?)`, member.ID, member.Email, member.Name, passwordHash, salt, at); err != nil {
		return member, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registration_outbox(event_id,member_id,payload,status,next_at) VALUES(?,?,?,'PENDING',?)`, event.EventID, member.ID, raw, time.Now().UnixMilli()); err != nil {
		return member, err
	}
	return member, tx.Commit()
}
func (s *Store) Member(ctx context.Context, id string) (Member, error) {
	var m Member
	var parent sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT m.id,m.email,m.name,m.registered_at,o.status,o.agent_id,o.parent_id,o.last_code FROM members m JOIN registration_outbox o ON o.member_id=m.id WHERE m.id=?`, id).Scan(&m.ID, &m.Email, &m.Name, &m.RegisteredAt, &m.Delivery, &m.AgentID, &parent, &m.Reason)
	if parent.Valid {
		m.ParentID = &parent.String
	}
	return m, err
}
func (s *Store) Members(ctx context.Context, pagination ...int) ([]Member, error) {
	// 只返回原允许的展示字段；分页不放宽会员资料字段或登录限制。
	limit, offset := 100, 0
	if len(pagination) == 2 {
		limit, offset = pagination[0], pagination[1]
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT m.id,m.name,m.registered_at,o.status,o.agent_id,o.parent_id,o.last_code FROM members m JOIN registration_outbox o ON o.member_id=m.id ORDER BY m.registered_at DESC,m.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Member{}
	for rows.Next() {
		var m Member
		var p sql.NullString
		if err = rows.Scan(&m.ID, &m.Name, &m.RegisteredAt, &m.Delivery, &m.AgentID, &p, &m.Reason); err != nil {
			return nil, err
		}
		if p.Valid {
			m.ParentID = &p.String
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

type Job struct {
	EventID, MemberID, Lease string
	Raw                      []byte
	Attempts                 int
}

func (s *Store) Claim(ctx context.Context) (Job, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var j Job
	// 先持久化短租约再发起外部请求，调用期间不持有SQLite写事务；崩溃后租约到期继续投原事件。
	err = tx.QueryRowContext(ctx, `SELECT event_id,member_id,payload,attempts FROM registration_outbox WHERE status IN ('PENDING','PROCESSING') AND next_at<=? ORDER BY next_at,event_id LIMIT 1`, time.Now().UnixMilli()).Scan(&j.EventID, &j.MemberID, &j.Raw, &j.Attempts)
	if err != nil {
		return j, err
	}
	j.Lease = ID()
	j.Attempts++
	if _, err = tx.ExecContext(ctx, `UPDATE registration_outbox SET status='PROCESSING',attempts=?,lease_token=?,next_at=? WHERE event_id=?`, j.Attempts, j.Lease, time.Now().Add(30*time.Second).UnixMilli(), j.EventID); err != nil {
		return j, err
	}
	return j, tx.Commit()
}
func (s *Store) Finish(ctx context.Context, j Job, state, agent string, parent *string, code string) error {
	delay := time.Second << min(j.Attempts, 6)
	// 租约令牌阻止旧请求结果覆盖新一轮投递；原始payload和event_id始终不变。
	result, err := s.DB.ExecContext(ctx, `UPDATE registration_outbox SET status=?,agent_id=?,parent_id=?,last_code=?,next_at=? WHERE event_id=? AND lease_token=?`, state, agent, parent, code, time.Now().Add(delay).UnixMilli(), j.EventID, j.Lease)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("投递租约已失效")
	}
	return err
}
