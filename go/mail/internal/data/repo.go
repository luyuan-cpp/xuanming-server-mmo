// Package data持有邮件、资格名单、领取意图，绝不持有玩家余额或背包。
package data

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	"shared/assetop"
	"shared/mailgrant"
	"shared/serviceauth"
)

const DatabaseName = "mmorpg_mail"

var (
	ErrInvalid    = errors.New("邮件参数无效")
	ErrNotFound   = errors.New("邮件不存在")
	ErrExpired    = errors.New("邮件已过期")
	ErrBoxFull    = errors.New("收件箱已满")
	ErrConflict   = errors.New("邮件请求冲突")
	ErrAttachment = errors.New("邮件附件无效")
	ErrProtected  = errors.New("邮件附件仍受保护")
	ErrDenied     = errors.New("邮件发件方无权操作")
)

type NextID func(context.Context) (uint64, error)

type Repo struct {
	limits         Limits
	db             *sql.DB
	mailIDs, opIDs NextID
	exists         func(uint32) bool
	guildDatabase  string
	now            func() time.Time
	seq            assetop.SeqTables
	roleAuthority  string
}

func New(db *sql.DB, mailIDs, opIDs NextID, exists func(uint32) bool, guildDatabase string) *Repo {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(guildDatabase) {
		panic("帮会权威库名无效")
	}
	seq, err := assetop.NewSeqTables("mail_player_op_seq", "mail_asset_op", 1)
	if err != nil {
		panic(err)
	}
	return &Repo{db: db, mailIDs: mailIDs, opIDs: opIDs, exists: exists, guildDatabase: guildDatabase, now: time.Now, seq: seq}
}
func Tables() []proto.Message {
	return []proto.Message{&mailpb.MailRecord{}, &mailpb.MailBoxRecord{}, &mailpb.MailStateRecord{}, &mailpb.MailPlayerOpSeqRecord{}, &mailpb.MailAssetOpRecord{}, &mailpb.MailCampaignRecord{}, &mailpb.MailCampaignShardRecord{}, &mailpb.MailCampaignRecipientRecord{}}
}
func (r *Repo) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		err = r.lockActor(ctx, tx)
		if err == nil {
			err = fn(tx)
		}
		if err == nil {
			return tx.Commit()
		}
		_ = tx.Rollback()
		var me *mysql.MySQLError
		if !errors.As(err, &me) || (me.Number != 1213 && me.Number != 1205) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ErrConflict
}

const messageColumns = "mail_id,owner_player_id,guild_id,kind,sender_kind,request_id,request_hash,title,body,attachments,sent_ms,expire_ms,campaign_id,manifest_generation"

type scanner interface{ Scan(...any) error }

func scanMessage(row scanner) (*mailpb.MailRecord, error) {
	m := &mailpb.MailRecord{}
	err := row.Scan(&m.MailId, &m.OwnerPlayerId, &m.GuildId, &m.Kind, &m.SenderKind, &m.RequestId, &m.RequestHash, &m.Title, &m.Body, &m.Attachments, &m.SentMs, &m.ExpireMs, &m.CampaignId, &m.ManifestGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}
func bundleBytes(in *mailpb.MailAttachments, exists func(uint32) bool) ([]byte, error) {
	b, err := mailgrant.Bundle(in, exists)
	if err != nil {
		return nil, ErrAttachment
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(b)
}
func hasAttachments(payload []byte) bool { return len(payload) > 0 }
func validateContent(in *mailpb.SendMailRequest) error {
	if in == nil || in.SenderKind == 0 || in.RequestId == 0 || !utf8.ValidString(in.Title) || !utf8.ValidString(in.Body) || utf8.RuneCountInString(in.Title) < 1 || utf8.RuneCountInString(in.Title) > 128 || utf8.RuneCountInString(in.Body) > 4096 || len(in.ProtoReflect().GetUnknown()) > 0 {
		return ErrInvalid
	}
	return nil
}

// 发件角色到sender_kind固定映射，禁止两个角色复用相同幂等域。
func SenderKind(caller string) uint32 {
	switch caller {
	case "admin":
		return 1
	case "guild":
		return 2
	case "rank":
		return 3
	case "trade":
		return 4
	case "battle":
		return 5
	}
	return 0
}
func (r *Repo) Send(ctx context.Context, caller string, kind mailpb.MailKind, in *mailpb.SendMailRequest) (*mailpb.SendMailResponse, error) {
	if err := validateContent(in); err != nil {
		return nil, err
	}
	if SenderKind(caller) == 0 || SenderKind(caller) != in.SenderKind {
		return nil, ErrDenied
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, err
	}
	// 先判重：曾成功的请求不会因收件箱已满、配置变化、时间过期而被改判。
	old, err := scanMessage(r.db.QueryRowContext(ctx, "SELECT "+messageColumns+" FROM mail_message WHERE sender_kind=? AND request_id=?", in.SenderKind, in.RequestId))
	if err == nil {
		actualKind := old.Kind
		if actualKind == 0 {
			c, e := scanCampaign(r.db.QueryRowContext(ctx, "SELECT "+campaignColumns+" FROM mail_campaign WHERE sender_kind=? AND campaign_id=? AND manifest_generation=?", old.SenderKind, old.CampaignId, old.ManifestGeneration))
			if e != nil {
				return nil, e
			}
			actualKind = c.Kind
		}
		if old.RequestHash != digest || actualKind != uint32(kind) {
			return nil, ErrConflict
		}
		return &mailpb.SendMailResponse{MailId: old.MailId, Duplicated: true}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	payload, err := bundleBytes(in.Attachments, r.exists)
	if err != nil {
		return nil, err
	}
	now := uint64(r.now().UnixMilli())
	limits := r.limits.Defaults()
	ttlDays := limits.MaxSystemMailTtlDays
	if kind == mailpb.MailKind_MAIL_KIND_PERSONAL {
		ttlDays = limits.MaxPersonalMailTtlDays
	}
	ttlMs := uint64(ttlDays) * 86400000
	expire := in.ExpireMs
	if expire == 0 {
		expire = now + ttlMs
	}
	if expire <= now || expire-now > ttlMs {
		return nil, ErrInvalid
	}
	if (kind == mailpb.MailKind_MAIL_KIND_SYSTEM || kind == mailpb.MailKind_MAIL_KIND_GUILD) && hasAttachments(payload) {
		return r.createCampaign(ctx, caller, kind, in, digest, payload, expire)
	}
	if in.CampaignId != 0 || in.ManifestGeneration != 0 || in.ExpectedRecipientCount != 0 || in.ExpectedShardCount != 0 || in.ManifestHash != "" {
		return nil, ErrInvalid
	}
	if kind == mailpb.MailKind_MAIL_KIND_PERSONAL {
		if in.RecipientPlayerId == 0 || in.GuildId != 0 {
			return nil, ErrInvalid
		}
	} else if kind == mailpb.MailKind_MAIL_KIND_SYSTEM {
		if caller != "admin" || in.RecipientPlayerId != 0 || in.GuildId != 0 {
			return nil, ErrDenied
		}
	} else if kind == mailpb.MailKind_MAIL_KIND_GUILD {
		if caller != "guild" || in.GuildId == 0 || in.RecipientPlayerId != 0 || hasAttachments(payload) {
			return nil, ErrDenied
		}
	} else {
		return nil, ErrInvalid
	}
	id, err := r.mailIDs(ctx)
	if err != nil {
		return nil, err
	}
	result := &mailpb.SendMailResponse{MailId: id}
	// 个人邮件按收件人串行(一人一行 mail_box),满箱计数因此是精确的。
	// 系统和帮会邮件曾共用 player_id=0 这一行全服排队,2026-10-30 撤掉(docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条):
	// 同一请求的并发首发由 (sender_kind, request_id) 唯一键裁决,输家转读赢家;MaxActiveSystemMails 变成软上限,并发发件可能略超,
	// 读侧 ListMails 有 LIMIT 硬上限,发件方只有运维和帮会公告。
	personal := kind == mailpb.MailKind_MAIL_KIND_PERSONAL
	if personal {
		if _, err = r.db.ExecContext(ctx, "INSERT IGNORE INTO mail_box (player_id,created_ms) VALUES (?,?)", in.RecipientPlayerId, now); err != nil {
			return nil, err
		}
	}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		if personal {
			var box uint64
			if err := tx.QueryRowContext(ctx, "SELECT player_id FROM mail_box WHERE player_id=? FOR UPDATE", in.RecipientPlayerId).Scan(&box); err != nil {
				return err
			}
		}
		// 同一请求已有邮件时回原 mail_id;不同请求占了这个键回 ErrConflict。
		replay := func() (bool, error) {
			previous, e := scanMessage(tx.QueryRowContext(ctx, "SELECT "+messageColumns+" FROM mail_message WHERE sender_kind=? AND request_id=?", in.SenderKind, in.RequestId))
			if errors.Is(e, ErrNotFound) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if previous.RequestHash != digest || previous.Kind != uint32(kind) {
				return false, ErrConflict
			}
			result.MailId = previous.MailId
			result.Duplicated = true
			return true, nil
		}
		if replayed, e := replay(); e != nil || replayed {
			return e
		}
		if personal {
			var count uint64
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message m JOIN mail_state s ON s.mail_id=m.mail_id AND s.player_id=m.owner_player_id WHERE m.owner_player_id=? AND s.deleted_ms=0 AND (m.expire_ms>? OR s.claim_state NOT IN (0,1,3))", in.RecipientPlayerId, now).Scan(&count); e != nil {
				return e
			}
			if count >= uint64(limits.MaxPersonalMailsPerPlayer) {
				return ErrBoxFull
			}
		}
		if !personal {
			var count uint64
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message WHERE kind IN (0,2,3) AND owner_player_id=0 AND expire_ms>?", now).Scan(&count); e != nil {
				return e
			}
			if count >= uint64(limits.MaxActiveSystemMails) {
				return ErrBoxFull
			}
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO mail_message ("+messageColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,0)", id, in.RecipientPlayerId, in.GuildId, uint32(kind), in.SenderKind, in.RequestId, digest, in.Title, in.Body, payload, now, expire); e != nil {
			if !isDuplicateKey(e) {
				return e
			}
			// 并发首发同一请求:唯一键裁决,转读赢家已提交的行。读不到说明撞的是别的键,按冲突回。
			replayed, re := replay()
			if re != nil {
				return re
			}
			if !replayed {
				return ErrConflict
			}
			return nil
		}
		if in.RecipientPlayerId != 0 {
			state := uint32(mailpb.MailClaimState_MAIL_CLAIM_NONE)
			if hasAttachments(payload) {
				state = uint32(mailpb.MailClaimState_MAIL_CLAIM_UNCLAIMED)
			}
			_, e := tx.ExecContext(ctx, "INSERT INTO mail_state (player_id,mail_id,read_ms,deleted_ms,claim_state,current_op_id,claim_version,reason_tip_id,updated_ms) VALUES (?,?,0,0,?,0,0,0,?)", in.RecipientPlayerId, id, state, now)
			return e
		}
		return nil
	})
	if isDuplicateKey(err) {
		return nil, ErrConflict
	}
	return result, err
}

// isDuplicateKey 判断是不是唯一键冲突(1062)。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
