package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"modernc.org/sqlite"
	"net"
	"os"
	"path/filepath"
	"time"
)

// 代理展示资料独立于身份令牌；登录审计失败由调用方记录，不撤销已成功的登录事务。
func (s *Store) ensureAffiliateProfiles(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS affiliate_profiles(member_id TEXT PRIMARY KEY REFERENCES members(id),display_id TEXT NOT NULL UNIQUE,vip_level INTEGER,avatar_url TEXT NOT NULL DEFAULT '',last_login_at TEXT,last_login_ip TEXT NOT NULL DEFAULT '')`)
	return err
}
func (s *Store) ensureAffiliateProfile(ctx context.Context, id string) error {
	if err := s.ensureAffiliateProfiles(ctx); err != nil {
		return err
	}
	var existing string
	err := s.DB.QueryRowContext(ctx, `SELECT display_id FROM affiliate_profiles WHERE member_id=?`, id).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for retry := 0; retry < 8; retry++ {
		n, err := rand.Int(rand.Reader, big.NewInt(9000000))
		if err != nil {
			return err
		}
		display := fmt.Sprint(1000000 + n.Int64())
		_, err = s.DB.ExecContext(ctx, `INSERT INTO affiliate_profiles(member_id,display_id) VALUES(?,?) ON CONFLICT(member_id) DO NOTHING`, id, display)
		if err == nil {
			return nil
		}
		var conflict *sqlite.Error
		if !errors.As(err, &conflict) || conflict.Code() != 2067 {
			return err
		}
	}
	return fmt.Errorf("无法分配唯一会员展示ID")
}
func (s *Store) RecordAffiliateLogin(ctx context.Context, id, remote string, at time.Time) error {
	if err := s.ensureAffiliateProfile(ctx, id); err != nil {
		return err
	}
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("登录连接IP无效")
	}
	// 只采用实际连接，身份来源不信任Forwarded或浏览器自报IP；BFF场景记录BFF连接地址。
	_, err = s.DB.ExecContext(ctx, `UPDATE affiliate_profiles SET last_login_at=?,last_login_ip=? WHERE member_id=? AND (last_login_at IS NULL OR last_login_at<=?)`, at.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"), ip, id, at.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"))
	return err
}
func (s *Store) ExportAffiliateProfiles(ctx context.Context, path, tenant, app, source string) error {
	if err := s.ensureAffiliateProfiles(ctx); err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM members ORDER BY id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.ensureAffiliateProfile(ctx, id); err != nil {
			return err
		}
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT m.id,m.name,p.display_id,p.vip_level,p.avatar_url,p.last_login_at,p.last_login_ip FROM members m JOIN affiliate_profiles p ON p.member_id=m.id ORDER BY m.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	profiles := []map[string]any{}
	for rows.Next() {
		var id, name, display, avatar, ip string
		var vip sql.NullInt64
		var login sql.NullString
		if err = rows.Scan(&id, &name, &display, &vip, &avatar, &login, &ip); err != nil {
			return err
		}
		var v any
		var at any
		if vip.Valid {
			v = vip.Int64
		}
		if login.Valid {
			at = login.String
		}
		profiles = append(profiles, map[string]any{"source_id": source, "external_user_id": id, "display_id": display, "display_name": name, "vip_level": v, "avatar_url": avatar, "last_login_at": at, "last_login_ip": ip})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal([]any{map[string]any{"tenant_id": tenant, "app_id": app, "profiles": profiles}})
	if err != nil {
		return err
	}
	// 同目录临时文件原子替换，读取方不会读到半写快照；私有文件不可供其他系统用户读取。
	f, err := os.CreateTemp(filepath.Dir(path), ".affiliate-profiles-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
