package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	mailpb "proto/mail"
)

const campaignColumns = "sender_kind,campaign_id,manifest_generation,mail_id,request_id,producer,request_hash,title,body,attachments,expire_ms,expected_recipient_count,expected_shard_count,manifest_hash,state,created_ms,sealed_ms,kind,guild_id"

func scanCampaign(row scanner) (*mailpb.MailCampaignRecord, error) {
	c := &mailpb.MailCampaignRecord{}
	err := row.Scan(&c.SenderKind, &c.CampaignId, &c.ManifestGeneration, &c.MailId, &c.RequestId, &c.Producer, &c.RequestHash, &c.Title, &c.Body, &c.Attachments, &c.ExpireMs, &c.ExpectedRecipientCount, &c.ExpectedShardCount, &c.ManifestHash, &c.State, &c.CreatedMs, &c.SealedMs, &c.Kind, &c.GuildId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}
func validManifest(in *mailpb.SendMailRequest) bool {
	decoded, err := hex.DecodeString(in.ManifestHash)
	return in.RecipientPlayerId == 0 && in.CampaignId > 0 && in.ManifestGeneration > 0 && in.ExpectedRecipientCount > 0 && in.ExpectedRecipientCount <= 1000000 && in.ExpectedShardCount > 0 && in.ExpectedShardCount <= 10000 && err == nil && len(decoded) == 32 && in.ManifestHash == strings.ToLower(in.ManifestHash)
}
func (r *Repo) createCampaign(ctx context.Context, caller string, kind mailpb.MailKind, in *mailpb.SendMailRequest, digest string, payload []byte, expire uint64) (*mailpb.SendMailResponse, error) {
	if !validManifest(in) || !((kind == mailpb.MailKind_MAIL_KIND_SYSTEM && caller == "admin" && in.GuildId == 0) || (kind == mailpb.MailKind_MAIL_KIND_GUILD && caller == "guild" && in.GuildId != 0)) {
		return nil, ErrInvalid
	}
	id, err := r.mailIDs(ctx)
	if err != nil {
		return nil, err
	}
	now := uint64(r.now().UnixMilli())
	result := &mailpb.SendMailResponse{}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		// 防同 sender/request 被换 campaign 重复建意图,靠 mail_message 与 mail_campaign 上 (sender_kind, request_id) 的唯一键;
		// 不再排全服共用的 mail_box(0) 守卫(docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
		replay := func() (bool, error) {
			old, e := scanCampaign(tx.QueryRowContext(ctx, "SELECT "+campaignColumns+" FROM mail_campaign WHERE sender_kind=? AND request_id=? LIMIT 1", in.SenderKind, in.RequestId))
			if errors.Is(e, ErrNotFound) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if old.RequestHash != digest || old.Producer != caller {
				return false, ErrConflict
			}
			result.MailId = old.MailId
			result.Duplicated = true
			return true, nil
		}
		if replayed, e := replay(); e != nil || replayed {
			return e
		}
		// 软上限:并发建活动可能略超,理由同 Send。
		var active uint64
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message WHERE owner_player_id=0 AND kind IN (0,2,3) AND expire_ms>?", now).Scan(&active); e != nil {
			return e
		}
		if active >= uint64(r.limits.Defaults().MaxActiveSystemMails) {
			return ErrBoxFull
		}
		// kind=0仅保留全频道共享的发件幂等键；可见查询明确排除它。
		if _, e := tx.ExecContext(ctx, "INSERT INTO mail_message ("+messageColumns+") VALUES (?,0,?,0,?,?,?,?,?,?,?,?,?,?)", id, in.GuildId, in.SenderKind, in.RequestId, digest, in.Title, in.Body, payload, now, expire, in.CampaignId, in.ManifestGeneration); e != nil {
			if !isDuplicateKey(e) {
				return e
			}
			// 并发首建同一请求:唯一键裁决,转读赢家已提交的活动。读不到说明这个键被非活动邮件占了。
			replayed, re := replay()
			if re != nil {
				return re
			}
			if !replayed {
				return ErrConflict
			}
			return nil
		}
		// 消息行已经插成,活动行再撞键只可能是 (sender,campaign,generation) 被别的请求占了:整笔回滚,不留下孤儿消息行。
		if _, e := tx.ExecContext(ctx, "INSERT INTO mail_campaign ("+campaignColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,0,?,?)", in.SenderKind, in.CampaignId, in.ManifestGeneration, id, in.RequestId, caller, digest, in.Title, in.Body, payload, expire, in.ExpectedRecipientCount, in.ExpectedShardCount, in.ManifestHash, now, uint32(kind), in.GuildId); e != nil {
			if isDuplicateKey(e) {
				return ErrConflict
			}
			return e
		}
		result.MailId = id
		return nil
	})
	return result, err
}

// ShardHash的顺序和十进制编码是跨语言契约，不依赖repeated原始位置。
func ShardHash(ids []uint64) (string, []uint64, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return "", nil, ErrInvalid
	}
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	unique := sorted[:0]
	var b strings.Builder
	for _, id := range sorted {
		if id == 0 {
			return "", nil, ErrInvalid
		}
		if len(unique) > 0 && unique[len(unique)-1] == id {
			continue
		}
		unique = append(unique, id)
		fmt.Fprintf(&b, "%d\n", id)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), unique, nil
}
func (r *Repo) Import(ctx context.Context, caller string, in *mailpb.ImportMailRecipientsRequest) (*mailpb.ImportMailRecipientsResponse, error) {
	if in == nil || in.SenderKind != SenderKind(caller) || in.CampaignId == 0 || in.ManifestGeneration == 0 {
		return nil, ErrInvalid
	}
	hash, ids, err := ShardHash(in.PlayerIds)
	if err != nil {
		return nil, err
	}
	result := &mailpb.ImportMailRecipientsResponse{}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		c, e := scanCampaign(tx.QueryRowContext(ctx, "SELECT "+campaignColumns+" FROM mail_campaign WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? FOR UPDATE", in.SenderKind, in.CampaignId, in.ManifestGeneration))
		if e != nil {
			return e
		}
		if c.Producer != caller {
			return ErrDenied
		}
		if in.ShardIndex >= c.ExpectedShardCount {
			return ErrInvalid
		}
		var old string
		var count uint32
		e = tx.QueryRowContext(ctx, "SELECT shard_hash,recipient_count FROM mail_campaign_shard WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? AND shard_index=?", in.SenderKind, in.CampaignId, in.ManifestGeneration, in.ShardIndex).Scan(&old, &count)
		if e == nil {
			if old != hash || count != uint32(len(ids)) {
				return ErrConflict
			}
			result.ImportedCount = uint64(count)
			result.Duplicated = true
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if c.State != 1 {
			return ErrConflict
		}
		var imported uint64
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_campaign_recipient WHERE sender_kind=? AND campaign_id=? AND manifest_generation=?", in.SenderKind, in.CampaignId, in.ManifestGeneration).Scan(&imported); e != nil {
			return e
		}
		if imported+uint64(len(ids)) > c.ExpectedRecipientCount {
			return ErrInvalid
		}
		for _, id := range ids {
			var previous uint32
			e = tx.QueryRowContext(ctx, "SELECT shard_index FROM mail_campaign_recipient WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? AND player_id=?", in.SenderKind, in.CampaignId, in.ManifestGeneration, id).Scan(&previous)
			if e == nil {
				return ErrConflict
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO mail_campaign_recipient (sender_kind,campaign_id,manifest_generation,player_id,shard_index) VALUES (?,?,?,?,?)", in.SenderKind, in.CampaignId, in.ManifestGeneration, id, in.ShardIndex); e != nil {
				return e
			}
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO mail_campaign_shard (sender_kind,campaign_id,manifest_generation,shard_index,shard_hash,recipient_count) VALUES (?,?,?,?,?,?)", in.SenderKind, in.CampaignId, in.ManifestGeneration, in.ShardIndex, hash, len(ids))
		result.ImportedCount = uint64(len(ids))
		return e
	})
	return result, err
}
func (r *Repo) Seal(ctx context.Context, caller string, in *mailpb.SealMailCampaignRequest) (*mailpb.SendMailResponse, error) {
	if in == nil || in.SenderKind != SenderKind(caller) || in.CampaignId == 0 || in.ManifestGeneration == 0 {
		return nil, ErrInvalid
	}
	result := &mailpb.SendMailResponse{}
	err := r.tx(ctx, func(tx *sql.Tx) error {
		c, e := scanCampaign(tx.QueryRowContext(ctx, "SELECT "+campaignColumns+" FROM mail_campaign WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? FOR UPDATE", in.SenderKind, in.CampaignId, in.ManifestGeneration))
		if e != nil {
			return e
		}
		if c.Producer != caller {
			return ErrDenied
		}
		result.MailId = c.MailId
		if c.State == 2 {
			result.Duplicated = true
			return nil
		}
		now := uint64(r.now().UnixMilli())
		if c.ExpireMs <= now {
			return ErrExpired
		}
		rows, e := tx.QueryContext(ctx, "SELECT shard_index,shard_hash,recipient_count FROM mail_campaign_shard WHERE sender_kind=? AND campaign_id=? AND manifest_generation=? ORDER BY shard_index", c.SenderKind, c.CampaignId, c.ManifestGeneration)
		if e != nil {
			return e
		}
		var b strings.Builder
		b.WriteString("v1\n")
		var shards uint32
		var recipients uint64
		for rows.Next() {
			var index, count uint32
			var hash string
			if e = rows.Scan(&index, &hash, &count); e != nil {
				rows.Close()
				return e
			}
			if index != shards {
				rows.Close()
				return ErrConflict
			}
			fmt.Fprintf(&b, "%d:%s:%d\n", index, hash, count)
			shards++
			recipients += uint64(count)
		}
		e = rows.Err()
		_ = rows.Close()
		if e != nil {
			return e
		}
		sum := sha256.Sum256([]byte(b.String()))
		if shards != c.ExpectedShardCount || recipients != c.ExpectedRecipientCount || hex.EncodeToString(sum[:]) != c.ManifestHash {
			return ErrConflict
		}
		var actual uint64
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_campaign_recipient WHERE sender_kind=? AND campaign_id=? AND manifest_generation=?", c.SenderKind, c.CampaignId, c.ManifestGeneration).Scan(&actual); e != nil {
			return e
		}
		if actual != recipients {
			return ErrConflict
		}
		// 名单摘要与全部收件人提交后，才在同事务发布可见频道。
		if _, e = tx.ExecContext(ctx, "UPDATE mail_message SET kind=?,sent_ms=? WHERE mail_id=? AND kind=0", c.Kind, now, c.MailId); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE mail_campaign SET state=2,sealed_ms=? WHERE sender_kind=? AND campaign_id=? AND manifest_generation=?", now, c.SenderKind, c.CampaignId, c.ManifestGeneration)
		return e
	})
	return result, err
}
