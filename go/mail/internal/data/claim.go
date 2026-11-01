package data

import (
	"context"
	"database/sql"
	"math"

	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	base "proto/common/base"
	componentpb "proto/common/rollback"
	mailpb "proto/mail"
	"shared/assetop"
	table "shared/generated/pb/table"
)

func (r *Repo) Claim(ctx context.Context, player, id uint64) (*mailpb.MailActionResult, error) {
	if err := validateActor(ctx, player); err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, ErrInvalid
	}
	opID, err := r.opIDs(ctx)
	if err != nil {
		return nil, err
	}
	now := uint64(r.now().UnixMilli())
	guild := r.guildOf(ctx, player)
	result := &mailpb.MailActionResult{MailId: id}
	err = r.tx(ctx, func(tx *sql.Tx) error {
		if e := r.lockBox(ctx, tx, player); e != nil {
			return e
		}
		m, s, e := r.materialize(ctx, tx, player, guild, id, now)
		if e != nil {
			return e
		}
		result.ClaimState = mailpb.MailClaimState(s.ClaimState)
		result.AssetOpId = s.CurrentOpId
		// 已领取/在途/人工保护的重试只回原状态，绝不分配第二条资产指令。
		if s.ClaimState != uint32(mailpb.MailClaimState_MAIL_CLAIM_UNCLAIMED) {
			return nil
		}
		if m.ExpireMs <= now {
			return ErrExpired
		}
		if !hasAttachments(m.Attachments) {
			return ErrAttachment
		}
		if s.ClaimVersion == math.MaxUint64 {
			return ErrConflict
		}
		bundle := &assetpb.AssetBundle{}
		if proto.Unmarshal(m.Attachments, bundle) != nil {
			return ErrAttachment
		}
		if e = assetop.EnsureSeqRowTx(ctx, tx, r.seq, player, assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, now); e != nil {
			return e
		}
		alloc, e := assetop.AllocateSeq(ctx, tx, r.seq, player, assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, assetop.DefaultLimits, now)
		if e != nil {
			return e
		}
		version := s.ClaimVersion + 1
		_, e = tx.ExecContext(ctx, "INSERT INTO mail_asset_op (op_id,player_id,stream,seq,stream_epoch,mail_id,claim_version,status,payload,tx_type,attempts,next_attempt_ms,lease_until_ms,lease_token,last_outcome,last_reason,durable,reason_tip_id,created_ms,updated_ms,last_send_ms,resolved_by,resolve_reason) VALUES (?,?,?,?,?,?,?,1,?,?,0,?,0,0,0,0,0,0,?,?,0,'','')", opID, player, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT), alloc.Seq, alloc.Epoch, id, version, m.Attachments, uint32(componentpb.TransactionType_TX_MAIL_ATTACHMENT), now, now, now)
		if e != nil {
			return e
		}
		changed, e := tx.ExecContext(ctx, "UPDATE mail_state SET claim_state=2,current_op_id=?,claim_version=?,reason_tip_id=0,read_ms=IF(read_ms=0,?,read_ms),updated_ms=? WHERE player_id=? AND mail_id=? AND claim_state=1 AND claim_version=?", opID, version, now, now, player, id, s.ClaimVersion)
		if e != nil {
			return e
		}
		n, e := changed.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		result.ClaimState = mailpb.MailClaimState_MAIL_CLAIM_PENDING
		result.AssetOpId = opID
		result.ErrorMessage = &base.TipInfoMessage{Id: uint32(table.MailError_kMailClaimPending)}
		return nil
	})
	return result, err
}
