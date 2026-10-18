package simulation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"time"
)

func reversalKind(kind string) bool { return kind == "DEPOSIT_REFUNDED" || kind == "BET_CANCELLED" }

// recordReversal 只允许当前登录会员冲正自己的模拟原单；关联字段由服务端原记录补齐，不能自报他人事件。
func (s *Source) recordReversal(ctx context.Context, member string, in Input, registered time.Time) (Event, error) {
	if member == "" || in.Business == "" || len(in.Business) > 128 || strings.TrimSpace(in.Business) != in.Business || in.OriginalBusiness == "" || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 128 {
		return Event{}, errors.New("请填写退款撤单编号、原业务单号及原因")
	}
	originalKind := "DEPOSIT_SUCCEEDED"
	if in.Kind == "BET_CANCELLED" {
		originalKind = "BET_SETTLED"
	}
	var original Event
	var raw string
	e := s.DB.QueryRowContext(ctx, `SELECT payload FROM facts WHERE kind=? AND business_id=?`, originalKind, in.OriginalBusiness).Scan(&raw)
	if e != nil || json.Unmarshal([]byte(raw), &original) != nil || original.Member != member {
		return Event{}, errors.New("原业务单不存在或不属于当前会员")
	}
	profile, e := s.Profile(ctx)
	if e != nil {
		return Event{}, e
	}
	if profile.Currency != original.Currency {
		return Event{}, errors.New("原币种已变化，请核对来源配置后处理历史订单")
	}
	amount, e := Minor(in.Amount, profile.Scale)
	if e != nil || amount == "0" {
		return Event{}, errors.New("退款撤单金额必须为正数")
	}
	valid := "0"
	if in.Kind == "BET_CANCELLED" {
		valid, e = Minor(in.ValidBet, profile.Scale)
		if e != nil {
			return Event{}, e
		}
	} else if in.ValidBet != "" && in.ValidBet != "0" {
		return Event{}, errors.New("充值退款不包含有效投注")
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if in.At != "" {
		at, e = time.Parse(time.RFC3339Nano, in.At)
		if e != nil || at.Nanosecond()%1000 != 0 {
			return Event{}, errors.New("业务时间需包含时区且最多微秒精度")
		}
		at = at.UTC()
	}
	if at.Before(original.At) || at.Before(registered) || at.After(time.Now()) {
		return Event{}, errors.New("冲正时间不得早于原事件或晚于当前时间")
	}
	event := Event{Source: s.Config.Source, ID: id(), Business: in.Business, Member: member, Kind: in.Kind, Currency: original.Currency, Amount: amount, ValidBet: valid, At: at, OriginalID: original.ID, OriginalBusiness: original.Business, Reason: strings.TrimSpace(in.Reason)}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return event, e
	}
	defer tx.Rollback()
	// 先取得SQLite写锁再检查累计，防止两个请求各自通过上限校验后超退。
	if _, e = tx.ExecContext(ctx, `UPDATE facts SET state=state WHERE kind=? AND business_id=?`, originalKind, original.Business); e != nil {
		return event, e
	}
	var old string
	e = tx.QueryRowContext(ctx, `SELECT payload FROM facts WHERE kind=? AND business_id=?`, in.Kind, in.Business).Scan(&old)
	if e == nil {
		var prior Event
		if json.Unmarshal([]byte(old), &prior) != nil {
			return event, errors.New("原冲正事件损坏")
		}
		event.ID = prior.ID
		if in.At == "" {
			event.At = prior.At
		}
		if event != prior {
			return event, errors.New("同一冲正编号不能覆盖原始事实")
		}
		return prior, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return event, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT payload FROM facts WHERE kind=? AND json_extract(payload,'$.original_event_id')=?`, in.Kind, original.ID)
	if e != nil {
		return event, e
	}
	a, _ := new(big.Int).SetString(amount, 10)
	v, _ := new(big.Int).SetString(valid, 10)
	if v.Cmp(a) > 0 {
		rows.Close()
		return event, errors.New("冲正有效投注不能超过冲正金额")
	}
	for rows.Next() {
		var b []byte
		var prior Event
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return event, e
		}
		if json.Unmarshal(b, &prior) != nil {
			rows.Close()
			return event, errors.New("冲正记录损坏")
		}
		pa, _ := new(big.Int).SetString(prior.Amount, 10)
		pv, _ := new(big.Int).SetString(prior.ValidBet, 10)
		a.Add(a, pa)
		v.Add(v, pv)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return event, e
	}
	oa, _ := new(big.Int).SetString(original.Amount, 10)
	ov, _ := new(big.Int).SetString(original.ValidBet, 10)
	if a.Cmp(oa) > 0 || v.Cmp(ov) > 0 || new(big.Int).Sub(ov, v).Cmp(new(big.Int).Sub(oa, a)) > 0 {
		return event, errors.New("累计退款或撤单超过原事件金额")
	}
	body, _ := json.Marshal(event)
	if _, e = tx.ExecContext(ctx, `INSERT INTO facts(kind,business_id,payload,state) VALUES(?,?,?,'PENDING')`, event.Kind, event.Business, body); e != nil {
		return event, e
	}
	e = tx.Commit()
	if e == nil {
		slog.InfoContext(ctx, "模拟退款撤单已保存待投递", "request_id", event.ID, "original_event_id", event.OriginalID, "business_id", event.Business, "member_id", member)
	}
	return event, e
}
