package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"guild/internal/constants"
	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"
)

const (
	opGuildEmblemEquip  = "emblem_equip"
	opGuildEmblemUnlock = "emblem_unlock"
)

// EquipGuildEmblem 仅帮主可切换帮会徽记（0=默认免费，1..6=须已在本帮 unlocked_emblem_bits 中解锁）。
func (r *GuildRepo) EquipGuildEmblem(ctx context.Context, guildID, leaderID uint64, emblemID uint32) (*GuildData, error) {
	if emblemID > constants.MaxGuildEmblemID {
		return nil, ErrGuildEmblemNotUnlocked
	}
	var out *GuildData
	err := r.inTx(withRoleTargets(ctx, leaderID), opGuildEmblemEquip, func(ctx context.Context, tx *sql.Tx) error {
		var (
			curLeader uint64
			bits      uint64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT leader_id, unlocked_emblem_bits FROM guild WHERE guild_id = ? FOR UPDATE`,
			guildID).Scan(&curLeader, &bits)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildGone
		}
		if err != nil {
			return fmt.Errorf("lock guild %d for emblem equip: %w", guildID, err)
		}
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, leaderID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock emblem operator %d in guild %d: %w", leaderID, guildID, err)
		}
		if curLeader != leaderID || constants.Rank(role) != constants.RankLeader {
			return ErrRankTooLow
		}
		if emblemID != 0 {
			bit := uint64(1) << (emblemID - 1)
			if (bits & bit) == 0 {
				return ErrGuildEmblemNotUnlocked
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild SET emblem_skin_id = ? WHERE guild_id = ?`,
			emblemID, guildID); err != nil {
			return fmt.Errorf("update guild %d emblem_skin_id: %w", guildID, err)
		}
		loaded, err := loadGuild(ctx, tx, guildID)
		if err != nil {
			return err
		}
		out = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.invalidateGuildCache(ctx, guildID)
	return out, nil
}

// EmblemUnlockReserve 帮主个人真实灵石买断帮会定制徽记的预留事务入参。
type EmblemUnlockReserve struct {
	OpID            uint64
	PlayerID        uint64
	GuildID         uint64
	EmblemID        uint32
	DeadlineMs      uint64
	LeaseUntilMs    uint64
	LeaseToken      uint64
	NowMs           uint64
	Payload         []byte
	ClientRequestID uint64
}

// ReserveGuildEmblemUnlock 帮主个人真实灵石买断定制徽记预留（GUILD_DEBIT 流，绝不扣帮会资金）。
// 锁序严格沿用 guild → guild_member → guild_player_op_seq → guild_asset_op → guild_client_request。
func (r *EconomyRepo) ReserveGuildEmblemUnlock(ctx context.Context, in EmblemUnlockReserve) (Reserved, error) {
	if in.OpID == 0 || in.PlayerID == 0 || in.GuildID == 0 || in.EmblemID == 0 || in.EmblemID > constants.MaxGuildEmblemID || len(in.Payload) == 0 {
		return Reserved{}, fmt.Errorf("invalid emblem unlock reserve input")
	}
	const stream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
	bit := uint64(1) << (in.EmblemID - 1)
	var out Reserved
	err := r.guilds.inTx(withRoleTargets(ctx, in.PlayerID), opGuildEmblemUnlock, func(ctx context.Context, tx *sql.Tx) error {
		var (
			curLeader uint64
			bits      uint64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT leader_id, unlocked_emblem_bits FROM guild WHERE guild_id = ? FOR UPDATE`,
			in.GuildID).Scan(&curLeader, &bits)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildGone
		}
		if err != nil {
			return fmt.Errorf("lock guild %d for emblem unlock: %w", in.GuildID, err)
		}
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, in.GuildID, in.PlayerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock emblem buyer %d in guild %d: %w", in.PlayerID, in.GuildID, err)
		}
		if curLeader != in.PlayerID || constants.Rank(role) != constants.RankLeader {
			return ErrRankTooLow
		}
		if err := checkClientRequestTx(ctx, tx, in.PlayerID, in.ClientRequestID); err != nil {
			return err
		}
		if (bits & bit) != 0 {
			return ErrGuildEmblemAlreadyUnlocked
		}
		if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
			return err
		}
		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
		if err != nil {
			return err
		}
		if err := insertAssetOp(ctx, tx, &pb.GuildAssetOpRecord{
			OpId:          in.OpID,
			PlayerId:      in.PlayerID,
			Stream:        uint32(stream),
			Seq:           alloc.Seq,
			GuildId:       in.GuildID,
			Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_EMBLEM_UNLOCK,
			Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
			NextAttemptMs: in.LeaseUntilMs,
			DeadlineMs:    in.DeadlineMs,
			Payload:       in.Payload,
			RefId:         in.EmblemID,
			RefCount:      1,
			CreatedMs:     in.NowMs,
			UpdatedMs:     in.NowMs,
			LeaseUntilMs:  in.LeaseUntilMs,
			LeaseToken:    in.LeaseToken,
			TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_EMBLEM_UNLOCK),
			StreamEpoch:   alloc.Epoch,
		}); err != nil {
			return err
		}
		if err := insertClientRequestTx(ctx, tx, in.PlayerID, in.ClientRequestID, in.OpID, in.NowMs); err != nil {
			return err
		}
		out = Reserved{Seq: alloc.Seq, StreamEpoch: alloc.Epoch}
		return nil
	})
	if err != nil {
		return Reserved{}, err
	}
	return out, nil
}
