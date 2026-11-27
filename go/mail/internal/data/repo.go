// Package data持有邮件、资格名单、领取意图，绝不持有玩家余额或背包。
package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	"shared/assetop"
	"shared/authoritysql"
	"shared/mailgrant"
	"shared/rolefence"
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
	// ErrRecipientDeleted:个人邮件的收件人角色已删除(本库护栏副本是墓碑,docs/design/account-deletion.md §6.2)。
	// 是确定的拒收,发件方按终态处理(rank 的奖励直接放弃),不再重投。
	ErrRecipientDeleted = errors.New("收件人角色已删除")
)

type NextID func(context.Context) (uint64, error)

type Repo struct {
	limits         Limits
	db             *sql.DB
	mailIDs, opIDs NextID
	exists         func(uint32) bool
	guilds         GuildDirectory
	now            func() time.Time
	seq            assetop.SeqTables
	guard          *rolefence.Guard
}

func New(db *sql.DB, mailIDs, opIDs NextID, exists func(uint32) bool, guilds GuildDirectory) *Repo {
	if guilds == nil {
		panic("guild directory not wired")
	}
	seq, err := assetop.NewSeqTables("mail_player_op_seq", "mail_asset_op", 1)
	if err != nil {
		panic(err)
	}
	return &Repo{db: db, mailIDs: mailIDs, opIDs: opIDs, exists: exists, guilds: guilds, now: time.Now, seq: seq}
}
func Tables() []proto.Message {
	return []proto.Message{&mailpb.MailRecord{}, &mailpb.MailBoxRecord{}, &mailpb.MailStateRecord{}, &mailpb.MailPlayerOpSeqRecord{}, &mailpb.MailAssetOpRecord{}, &mailpb.MailCampaignRecord{}, &mailpb.MailCampaignShardRecord{}, &mailpb.MailCampaignRecipientRecord{}, &mailpb.MailRequestReceiptRecord{}}
}

// rowQuerier 是 *sql.DB 与 *sql.Tx 共有的单行读。
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// priorReceipt 按发件判重收据找同一请求首次建的那封(邮件行可能已被过期清理删掉)。没有收据回 (nil, nil)。
func priorReceipt(ctx context.Context, q rowQuerier, senderKind uint32, requestID uint64) (*mailpb.MailRequestReceiptRecord, error) {
	receipt := &mailpb.MailRequestReceiptRecord{SenderKind: senderKind, RequestId: requestID}
	err := q.QueryRowContext(ctx, "SELECT mail_id,request_hash,kind FROM mail_request_receipt WHERE sender_kind=? AND request_id=?", senderKind, requestID).Scan(&receipt.MailId, &receipt.RequestHash, &receipt.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
func (r *Repo) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return r.runGuarded(ctx, func() error { return r.txRetry(ctx, fn) })
}

// txRetry 跑一笔可整笔重跑的邮件事务:先锁行动者行,再跑 fn。骨架是 assetop.WithTxRetry(READ COMMITTED、最多 3 次、
// 指数退避带抖动、可取消),判定是全仓唯一的 authoritysql.IsRetryableTxError(1213 / 1205 / 9007 与守卫判定的提交前中止,
// 结局未知不重试;R6 残余,原来这里只认 1213 / 1205、退避不带抖动)。提交结果未知不自动重做业务。
func (r *Repo) txRetry(ctx context.Context, fn func(*sql.Tx) error) error {
	return assetop.WithTxRetry(ctx, r.db, 3, authoritysql.IsRetryableTxError, func(tx *sql.Tx) error {
		if err := r.lockActor(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

const messageColumns = "mail_id,owner_player_id,guild_id,kind,sender_kind,request_id,request_hash,title,body,attachments,sent_ms,expire_ms,campaign_id,manifest_generation"

// approvalColumns 是邮件行与活动行上的双人复核留痕(docs/design/admin-identity-and-dual-control.md §5.8):
// 建行时写发件请求带的批准记录,不带时写空串。只写不读,所以不进 messageColumns / campaignColumns 与扫描。
const approvalColumns = "approval_id,proposer,approver"

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
	case "match":
		return 6
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
	// 先判重：曾成功的请求不会因收件箱已满、配置变化、时间过期而被改判。先看收据:邮件行被过期清理删掉之后它还在,
	// 同一请求再来回原 mail_id(r2-craft#3);再看邮件行(活动邮件的占位行、收据上线之前建的邮件)。
	if receipt, e := priorReceipt(ctx, r.db, in.SenderKind, in.RequestId); e != nil {
		return nil, e
	} else if receipt != nil {
		if receipt.RequestHash != digest || receipt.Kind != uint32(kind) {
			return nil, ErrConflict
		}
		return &mailpb.SendMailResponse{MailId: receipt.MailId, Duplicated: true}, nil
	}
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
		// 收件人角色已删除就拒收,也不给他建邮箱行。这里是不加锁的预检,提交点在事务里锁副本再核(lockLiveRecipient)。
		if fence, e := rolefence.Read(ctx, r.db, in.RecipientPlayerId); e == nil && fence.Account == "" {
			return nil, ErrRecipientDeleted
		} else if e != nil && !errors.Is(e, rolefence.ErrMissing) {
			return nil, e
		}
		if _, err = r.db.ExecContext(ctx, "INSERT IGNORE INTO mail_box (player_id,created_ms) VALUES (?,?)", in.RecipientPlayerId, now); err != nil {
			return nil, err
		}
	}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		if personal {
			if err := lockLiveRecipient(ctx, tx, in.RecipientPlayerId); err != nil {
				return err
			}
			var box uint64
			if err := tx.QueryRowContext(ctx, "SELECT player_id FROM mail_box WHERE player_id=? FOR UPDATE", in.RecipientPlayerId).Scan(&box); err != nil {
				return err
			}
		}
		// 同一请求已有邮件时回原 mail_id;不同请求占了这个键回 ErrConflict。收据在前,同 Send 开头的判重。
		replay := func() (bool, error) {
			if receipt, e := priorReceipt(ctx, tx, in.SenderKind, in.RequestId); e != nil {
				return false, e
			} else if receipt != nil {
				if receipt.RequestHash != digest || receipt.Kind != uint32(kind) {
					return false, ErrConflict
				}
				result.MailId = receipt.MailId
				result.Duplicated = true
				return true, nil
			}
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
			if e := checkSharedMailCapacity(ctx, tx, in.GuildId, now, limits); e != nil {
				return e
			}
		}
		// 收据先于邮件行写:它的主键是同一请求并发首发的裁决点。撞键 = 别的事务已建了这一封,转读赢家已提交的行;
		// 读不到说明撞的是别的键,按冲突回。这时本事务还什么都没写。
		if _, e := tx.ExecContext(ctx, "INSERT INTO mail_request_receipt (sender_kind,request_id,mail_id,request_hash,kind,created_ms) VALUES (?,?,?,?,?,?)", in.SenderKind, in.RequestId, id, digest, uint32(kind), now); e != nil {
			if !isDuplicateKey(e) {
				return e
			}
			replayed, re := replay()
			if re != nil {
				return re
			}
			if !replayed {
				return ErrConflict
			}
			return nil
		}
		approval := in.GetApproval()
		if _, e := tx.ExecContext(ctx, "INSERT INTO mail_message ("+messageColumns+","+approvalColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,0,?,?,?)", id, in.RecipientPlayerId, in.GuildId, uint32(kind), in.SenderKind, in.RequestId, digest, in.Title, in.Body, payload, now, expire, approval.GetApprovalId(), approval.GetProposer(), approval.GetApprover()); e != nil {
			if !isDuplicateKey(e) {
				return e
			}
			// 收据刚由本事务写下,邮件行却撞了唯一键:只可能是收据上线之前建的、没有收据的旧邮件被并发写入。整笔回滚
			// (连同这条收据),不在这一笔里答复;发件方按故障重试,下一次 replay 读到那封旧邮件。
			return fmt.Errorf("邮件行唯一键已被占用却没有收据: sender_kind=%d request_id=%d", in.SenderKind, in.RequestId)
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

// checkSharedMailCapacity 判共享邮件(owner_player_id=0)还能不能再发一封,按可见范围分开计数(缺口清单第 27 条):
//   - guildID=0 是系统邮件(文本与活动),人人可见,全服共用 MaxActiveSystemMails;
//   - 非 0 是帮会邮件(文本与活动),只对本帮可见,按这个帮计数,上限 MaxActiveGuildMailsPerGuild。
//
// 两类正好按 guild_id 是否为 0 分开:Send 与 CreateCampaign 都校验系统邮件 guild_id=0、帮会邮件非 0。
// 活动的幂等占位行(kind=0)与它的消息行都算在各自那一类里,与改动前的口径相同。
// 软上限:并发发件可能略超,理由见 Send 的注释;读侧 ListMails 另有 LIMIT 硬上限。
func checkSharedMailCapacity(ctx context.Context, tx *sql.Tx, guildID, now uint64, limits Limits) error {
	query := "SELECT COUNT(*) FROM mail_message WHERE owner_player_id=0 AND guild_id=0 AND kind IN (0,2) AND expire_ms>?"
	args := []any{now}
	limit := limits.MaxActiveSystemMails
	if guildID != 0 {
		query = "SELECT COUNT(*) FROM mail_message WHERE owner_player_id=0 AND guild_id=? AND expire_ms>?"
		args = []any{guildID, now}
		limit = limits.MaxActiveGuildMailsPerGuild
	}
	var count uint64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count >= uint64(limit) {
		return ErrBoxFull
	}
	return nil
}

// isDuplicateKey 判断是不是唯一键冲突(1062)。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
