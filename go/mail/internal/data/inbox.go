package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	base "proto/common/base"
	mailpb "proto/mail"
	"shared/mailgrant"
)

func (r *Repo) visiblePredicate() string {
	return `(m.kind IN (1,2,3) AND (m.owner_player_id=? OR (m.owner_player_id=0 AND (
(m.campaign_id>0 AND EXISTS (SELECT 1 FROM mail_campaign_recipient q WHERE q.sender_kind=m.sender_kind AND q.campaign_id=m.campaign_id AND q.manifest_generation=m.manifest_generation AND q.player_id=?))
OR (m.kind=2 AND m.campaign_id=0)
OR (m.kind=3 AND m.campaign_id=0 AND EXISTS (SELECT 1 FROM ` + r.guildDatabase + `.guild_member gm WHERE gm.player_id=? AND gm.guild_id=m.guild_id))))))`
}

func columns(prefix string) string {
	return prefix + strings.ReplaceAll(messageColumns, ",", ","+prefix)
}
func stateColumns() string {
	return "COALESCE(s.read_ms,0),COALESCE(s.deleted_ms,0),COALESCE(s.claim_state,IF(LENGTH(m.attachments)>0,1,0)),COALESCE(s.current_op_id,0),COALESCE(s.claim_version,0),COALESCE(s.reason_tip_id,0),COALESCE(s.updated_ms,0)"
}
func scanView(row scanner) (*mailpb.MailView, error) {
	m := &mailpb.MailRecord{}
	s := &mailpb.MailStateRecord{}
	err := row.Scan(&m.MailId, &m.OwnerPlayerId, &m.GuildId, &m.Kind, &m.SenderKind, &m.RequestId, &m.RequestHash, &m.Title, &m.Body, &m.Attachments, &m.SentMs, &m.ExpireMs, &m.CampaignId, &m.ManifestGeneration, &s.ReadMs, &s.DeletedMs, &s.ClaimState, &s.CurrentOpId, &s.ClaimVersion, &s.ReasonTipId, &s.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	bundle := &assetpb.AssetBundle{}
	if err = proto.Unmarshal(m.Attachments, bundle); err != nil {
		return nil, ErrAttachment
	}
	view, err := mailgrant.View(bundle)
	if err != nil {
		return nil, ErrAttachment
	}
	result := &mailpb.MailView{MailId: m.MailId, Kind: mailpb.MailKind(m.Kind), Title: m.Title, Body: m.Body, SentMs: m.SentMs, ExpireMs: m.ExpireMs, IsRead: s.ReadMs != 0, ClaimState: mailpb.MailClaimState(s.ClaimState), Attachments: view, AssetOpId: s.CurrentOpId}
	if s.ReasonTipId != 0 {
		result.ClaimReason = &base.TipInfoMessage{Id: s.ReasonTipId}
	}
	return result, nil
}

func (r *Repo) List(ctx context.Context, player uint64, in *mailpb.ListMailsRequest) (*mailpb.ListMailsResponse, error) {
	if err := validateActor(ctx, player); err != nil {
		return nil, err
	}
	if player == 0 || in == nil {
		return nil, ErrInvalid
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	now := uint64(r.now().UnixMilli())
	// 两条只读查询共享同一RR快照，列表和计数不会混用两个版本；写事务仍固定RC。
	// TiDB8.5默认不支持START TRANSACTION READ ONLY；只发SELECT，不能为此开启全局noop。
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = r.lockActor(ctx, tx); err != nil {
		return nil, err
	}
	base := " FROM mail_message m LEFT JOIN mail_state s ON s.player_id=? AND s.mail_id=m.mail_id WHERE " + r.visiblePredicate() + " AND COALESCE(s.deleted_ms,0)=0 AND (m.expire_ms>? OR s.claim_state NOT IN (0,1,3))"
	result := &mailpb.ListMailsResponse{ServerNowMs: now}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(COALESCE(s.read_ms,0)=0),0),COALESCE(SUM(m.expire_ms>? AND COALESCE(s.claim_state,IF(LENGTH(m.attachments)>0,1,0))=1),0)"+base, now, player, player, player, player, now).Scan(&result.UnreadCount, &result.ClaimableCount); err != nil {
		return nil, err
	}
	args := []any{player, player, player, player, now}
	query := "SELECT " + columns("m.") + "," + stateColumns() + base
	if in.BeforeMailId != 0 {
		// 游标是上一页最后一封的 mail_id。id 是打散过的,不代表先后,要换算成 (sent_ms, mail_id)。
		var beforeSentMs uint64
		err = tx.QueryRowContext(ctx, "SELECT sent_ms FROM mail_message WHERE mail_id=?", in.BeforeMailId).Scan(&beforeSentMs)
		if errors.Is(err, sql.ErrNoRows) {
			// 游标那封已被清理,定位不到了:这一页到头,重新打开邮箱能看到其余的。
			if err = tx.Commit(); err != nil {
				return nil, err
			}
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		query += " AND (m.sent_ms<? OR (m.sent_ms=? AND m.mail_id<?))"
		args = append(args, beforeSentMs, beforeSentMs, in.BeforeMailId)
	}
	query += " ORDER BY m.sent_ms DESC,m.mail_id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		view, e := scanView(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		if len(result.Mails) == int(limit) {
			result.NextBeforeMailId = result.Mails[len(result.Mails)-1].MailId
			break
		}
		result.Mails = append(result.Mails, view)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Repo) lockBox(ctx context.Context, tx *sql.Tx, player uint64) error {
	if player == 0 {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "INSERT IGNORE INTO mail_box (player_id,created_ms) VALUES (?,?)", player, uint64(r.now().UnixMilli())); err != nil {
		return err
	}
	var guard uint64
	return tx.QueryRowContext(ctx, "SELECT player_id FROM mail_box WHERE player_id=? FOR UPDATE", player).Scan(&guard)
}

// 调用方先锁mail_box；所有物化/领取/清理共用此顺序，避免不存在状态行的并发升级。
func (r *Repo) materialize(ctx context.Context, tx *sql.Tx, player, id, now uint64) (*mailpb.MailRecord, *mailpb.MailStateRecord, error) {
	m, err := scanMessage(tx.QueryRowContext(ctx, "SELECT "+columns("m.")+" FROM mail_message m WHERE m.mail_id=? AND "+r.visiblePredicate(), id, player, player, player))
	if err != nil {
		return nil, nil, err
	}
	s := &mailpb.MailStateRecord{PlayerId: player, MailId: id}
	err = tx.QueryRowContext(ctx, "SELECT read_ms,deleted_ms,claim_state,current_op_id,claim_version,reason_tip_id,updated_ms FROM mail_state WHERE player_id=? AND mail_id=? FOR UPDATE", player, id).Scan(&s.ReadMs, &s.DeletedMs, &s.ClaimState, &s.CurrentOpId, &s.ClaimVersion, &s.ReasonTipId, &s.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		if m.ExpireMs <= now {
			return nil, nil, ErrExpired
		}
		if hasAttachments(m.Attachments) {
			s.ClaimState = 1
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO mail_state (player_id,mail_id,read_ms,deleted_ms,claim_state,current_op_id,claim_version,reason_tip_id,updated_ms) VALUES (?,?,0,0,?,0,0,0,?)", player, id, s.ClaimState, now)
	}
	if err != nil {
		return nil, nil, err
	}
	if s.DeletedMs != 0 {
		return nil, nil, ErrNotFound
	}
	return m, s, nil
}
func (r *Repo) Read(ctx context.Context, player, id uint64) (*mailpb.MailView, error) {
	if err := validateActor(ctx, player); err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, ErrInvalid
	}
	now := uint64(r.now().UnixMilli())
	var result *mailpb.MailView
	err := r.tx(ctx, func(tx *sql.Tx) error {
		if e := r.lockBox(ctx, tx, player); e != nil {
			return e
		}
		m, s, e := r.materialize(ctx, tx, player, id, now)
		if e != nil {
			return e
		}
		if m.ExpireMs <= now && (s.ClaimState == 0 || s.ClaimState == 1 || s.ClaimState == 3) {
			return ErrExpired
		}
		if _, e = tx.ExecContext(ctx, "UPDATE mail_state SET read_ms=IF(read_ms=0,?,read_ms),updated_ms=? WHERE player_id=? AND mail_id=?", now, now, player, id); e != nil {
			return e
		}
		result, e = scanView(tx.QueryRowContext(ctx, "SELECT "+columns("m.")+","+stateColumns()+" FROM mail_message m JOIN mail_state s ON s.mail_id=m.mail_id AND s.player_id=? WHERE m.mail_id=?", player, id))
		return e
	})
	return result, err
}

func (r *Repo) Delete(ctx context.Context, player uint64, ids []uint64, clear bool) ([]uint64, []uint64, error) {
	if err := validateActor(ctx, player); err != nil {
		return nil, nil, err
	}
	if (!clear && (len(ids) == 0 || len(ids) > 100)) || player == 0 {
		return nil, nil, ErrInvalid
	}
	now := uint64(r.now().UnixMilli())
	var deleted, protected []uint64
	err := r.tx(ctx, func(tx *sql.Tx) error {
		deleted = nil
		protected = nil
		if e := r.lockBox(ctx, tx, player); e != nil {
			return e
		}
		targets := ids
		if clear {
			rows, e := tx.QueryContext(ctx, "SELECT m.mail_id FROM mail_message m JOIN mail_state s ON s.mail_id=m.mail_id AND s.player_id=? WHERE s.read_ms>0 AND s.deleted_ms=0 AND "+r.visiblePredicate()+" ORDER BY m.mail_id LIMIT 1000", player, player, player, player)
			if e != nil {
				return e
			}
			targets = nil
			for rows.Next() {
				var id uint64
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return e
				}
				targets = append(targets, id)
			}
			e = rows.Err()
			_ = rows.Close()
			if e != nil {
				return e
			}
		}
		seen := map[uint64]bool{}
		for _, id := range targets {
			if id == 0 || seen[id] {
				continue
			}
			seen[id] = true
			m, s, e := r.materialize(ctx, tx, player, id, now)
			if errors.Is(e, ErrNotFound) || errors.Is(e, ErrExpired) {
				continue
			}
			if e != nil {
				return e
			}
			if (s.ClaimState != 0 && s.ClaimState != 1 && s.ClaimState != 3) || (s.ClaimState == 1 && m.ExpireMs > now) {
				protected = append(protected, id)
				continue
			}
			if _, e = tx.ExecContext(ctx, "UPDATE mail_state SET deleted_ms=?,updated_ms=? WHERE player_id=? AND mail_id=?", now, now, player, id); e != nil {
				return e
			}
			deleted = append(deleted, id)
		}
		return nil
	})
	return deleted, protected, err
}
