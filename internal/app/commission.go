package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	routersdk "github.com/ex-business/ex-service-router/sdk"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Commission 是唯一佣金侧边界；未来替换适配器即可接其他用户来源，无需复制会员存储逻辑。
type Commission struct {
	URL, Token, Key, Tenant, App, Source string
	Client                               *http.Client
}

func (c *Commission) call(ctx context.Context, path, key string, raw []byte, signed bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Request-ID", key)
	if signed {
		mac := hmac.New(sha256.New, []byte(c.Key))
		mac.Write([]byte("registration-v1\n" + c.Tenant + "\n" + c.App + "\n" + c.Source + "\n"))
		mac.Write(raw)
		req.Header.Set("X-Registration-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	res, err := routersdk.Do(c.Client, req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, body, err
}
func (c *Commission) Invitation(ctx context.Context, m Member, publicURL string) (string, error) {
	if m.Delivery != "SUCCEEDED" || m.AgentID == "" {
		return "", errors.New("代理身份尚未同步完成")
	}
	raw, _ := json.Marshal(map[string]string{"source_id": c.Source, "agent_id": m.AgentID})
	status, body, err := c.call(ctx, "/v1/registration-invitations", "member-invite-v2-"+m.ID, raw, false)
	if err != nil || status != 200 {
		return "", errors.New("邀请链接暂不可用，请稍后重试")
	}
	var response struct {
		Data struct {
			Code     string `json:"invitation_code"`
			AgentID  string `json:"agent_id"`
			SourceID string `json:"source_id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || response.Data.Code == "" || response.Data.AgentID != m.AgentID || response.Data.SourceID != c.Source {
		return "", errors.New("邀请响应校验失败")
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("ic", response.Data.Code)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func (c *Commission) Deliver(ctx context.Context, s *Store) (bool, error) {
	j, err := s.Claim(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status, body, callErr := c.call(ctx, "/v1/registrations", j.EventID, j.Raw, true)
	state, agent, code := "PENDING", "", "DEPENDENCY_UNAVAILABLE"
	var parent *string
	if callErr == nil && status == 200 {
		var response struct {
			Data struct {
				AgentID  string  `json:"agent_id"`
				ParentID *string `json:"parent_agent_id"`
				External string  `json:"external_user_id"`
				Source   string  `json:"source_id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &response) == nil && response.Data.AgentID != "" && response.Data.External == j.MemberID && response.Data.Source == c.Source {
			state, agent, parent, code = "SUCCEEDED", response.Data.AgentID, response.Data.ParentID, ""
		} else {
			code = "INVALID_UPSTREAM_RESPONSE"
		}
	} else if callErr == nil {
		code = fmt.Sprintf("UPSTREAM_%d", status)
		// 业务拒绝保留注册事实并明确阻塞，不抹掉邀请码伪装成功；临时故障按原事件退避重投。
		if status >= 400 && status < 500 && status != 408 && status != 429 {
			state = "BLOCKED"
		}
	}
	if err = s.Finish(ctx, j, state, agent, parent, code); err != nil {
		return true, err
	}
	if state == "SUCCEEDED" {
		slog.Info("注册事件投递成功", "event_id", j.EventID, "member_id", j.MemberID, "agent_id", agent, "attempt", j.Attempts)
	} else if state == "BLOCKED" {
		slog.Warn("注册事件被业务拒绝，保留原事实", "event_id", j.EventID, "member_id", j.MemberID, "result_code", code)
	}
	return true, nil
}
func (c *Commission) Run(ctx context.Context, s *Store) {
	lastError := time.Time{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 20; i++ {
				worked, err := c.Deliver(ctx, s)
				if err != nil {
					if ctx.Err() == nil && time.Since(lastError) > 30*time.Second {
						lastError = time.Now()
						slog.Error("注册事件投递异常，等待恢复", "error", err)
					}
					break
				}
				if !worked {
					break
				}
			}
		}
	}
}
