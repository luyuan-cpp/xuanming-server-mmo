package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	"shared/assetop"

	"guild/internal/war"
)

// 帮会全天帮战 V2 仓储（锁序 13..19，全部接在 guild_name_hold=12 之后，绝不回头锁旧表）。
const (
	guildWarLeylineTable         = "guild_war_leyline"
	guildWarGarrisonTable        = "guild_war_garrison"
	guildWarWeeklyProgressTable  = "guild_war_weekly_progress"
	guildWarPlayerStateTable     = "guild_war_player_state"
	guildWarBountyTable          = "guild_war_bounty"
	guildWarBattleTable          = "guild_war_battle"
	guildWarRivalryTable         = "guild_war_rivalry"
	guildWarFeudTable            = "guild_war_feud"
)

var (
	ErrWarPeaceCooldown      = errors.New("war: peace cooldown active")
	ErrWarGarrisonFull       = errors.New("war: garrison slots full")
	ErrWarGarrisonCooldown   = errors.New("war: garrison refresh cooldown")
	ErrWarMilestoneLocked    = errors.New("war: milestone locked or merit insufficient")
	ErrWarMilestoneClaimed   = errors.New("war: milestone already claimed")
	ErrWarDividendEmpty      = errors.New("war: no claimable dividend shares")
	ErrWarBountyActive       = errors.New("war: target already has active bounty or immunity")
	ErrWarBountyLimit        = errors.New("war: bounty daily limit reached")
	ErrWarTrackCooldown      = errors.New("war: track cooldown active")
	ErrWarBattleAlreadyDone  = errors.New("war: battle already settled")
	ErrWarFeudCooldown       = errors.New("war: feud issue cooldown active")
	ErrWarFeudAlreadyActive  = errors.New("war: guild already has pending or active feud")
	ErrWarFeudNotFound       = errors.New("war: feud not found or resolved")
	// ErrWarSettleStale:结算事务在行锁内发现事务外读到的基准(灵脉快照、追杀令普通读、领赏计数)已被并发结算改掉,
	// 整笔回滚、什么都没写。调用方整笔重读重算(r2-gw#3),有界次数用完再交给 Kafka 重投。
	ErrWarSettleStale = errors.New("war: settlement basis changed by a concurrent writer")
	// errWarBountyNotDue:到期收尾锁住追杀令后发现它已不是到期的 ACTIVE(被结单 / 撕单 / 别的副本已收尾),整笔回滚。
	errWarBountyNotDue = errors.New("war: bounty no longer due for expiry")
)

type WarRepo struct {
	guilds *GuildRepo
	db     *sql.DB
	seq    assetop.SeqTables
}

func NewWarRepo(guilds *GuildRepo) (*WarRepo, error) {
	if guilds == nil {
		return nil, errors.New("guild war repo: nil GuildRepo")
	}
	seq, err := GuildSeqTables()
	if err != nil {
		return nil, fmt.Errorf("guild war repo: %w", err)
	}
	return &WarRepo{guilds: guilds, db: guilds.db, seq: seq}, nil
}

// EnsureZoneLeylines 确保指定 zone 的 3 座灵脉行存在（幂等 INSERT IGNORE）。
func (r *WarRepo) EnsureZoneLeylines(ctx context.Context, zoneID uint32, nowMs uint64) error {
	if zoneID == 0 {
		return nil
	}
	for _, meta := range war.AllLeylines {
		if _, err := r.db.ExecContext(ctx,
			"INSERT IGNORE INTO `"+guildWarLeylineTable+"` (`zone_id`,`leyline_id`,`owner_guild_id`,`banner_hp`,`shield_until_ms`,`captured_at_ms`,`last_dividend_at_ms`,`dividend_week_key`,`weekly_dividend_count`,`last_siege_alert_ms`,`updated_ms`) VALUES (?,?,0,?,0,0,?,0,0,0,?)",
			zoneID, meta.ID, war.DefaultLeylineCaptureInitHP, nowMs, nowMs); err != nil {
			return fmt.Errorf("ensure leyline zone=%d id=%d: %w", zoneID, meta.ID, err)
		}
	}
	return nil
}

func (r *WarRepo) ListLeylines(ctx context.Context, zoneID uint32) ([]*pb.GuildWarLeylineRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT `zone_id`,`leyline_id`,`owner_guild_id`,`banner_hp`,`shield_until_ms`,`captured_at_ms`,`last_dividend_at_ms`,`dividend_week_key`,`weekly_dividend_count`,`last_siege_alert_ms`,`updated_ms` FROM `"+guildWarLeylineTable+"` WHERE `zone_id` = ? ORDER BY `leyline_id` ASC",
		zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarLeylineRecord
	for rows.Next() {
		rec := &pb.GuildWarLeylineRecord{}
		if err := rows.Scan(&rec.ZoneId, &rec.LeylineId, &rec.OwnerGuildId, &rec.BannerHp, &rec.ShieldUntilMs,
			&rec.CapturedAtMs, &rec.LastDividendAtMs, &rec.DividendWeekKey, &rec.WeeklyDividendCount,
			&rec.LastSiegeAlertMs, &rec.UpdatedMs); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *WarRepo) ListGarrison(ctx context.Context, guildID uint64) ([]*pb.GuildWarGarrisonRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT `guild_id`,`slot_index`,`player_id`,`player_name`,`level`,`appearance_id`,`pet_name`,`pet_table_id`,`snapshot_payload`,`updated_ms` FROM `"+guildWarGarrisonTable+"` WHERE `guild_id` = ? ORDER BY `slot_index` ASC",
		guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarGarrisonRecord
	for rows.Next() {
		rec := &pb.GuildWarGarrisonRecord{}
		if err := rows.Scan(&rec.GuildId, &rec.SlotIndex, &rec.PlayerId, &rec.PlayerName, &rec.Level,
			&rec.AppearanceId, &rec.PetName, &rec.PetTableId, &rec.SnapshotPayload, &rec.UpdatedMs); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *WarRepo) GetWeeklyProgress(ctx context.Context, guildID uint64, weekKey uint32) (*pb.GuildWarWeeklyProgressRecord, error) {
	rec := &pb.GuildWarWeeklyProgressRecord{GuildId: guildID, WeekKey: weekKey}
	err := r.db.QueryRowContext(ctx,
		"SELECT `guild_merit`,`dividend_pool_count`,`mvp1_player_id`,`mvp1_merit`,`mvp2_player_id`,`mvp2_merit`,`first_win_player1_id`,`first_win_player2_id`,`first_win_at_ms`,`updated_ms` FROM `"+guildWarWeeklyProgressTable+"` WHERE `guild_id` = ? AND `week_key` = ?",
		guildID, weekKey).Scan(&rec.GuildMerit, &rec.DividendPoolCount, &rec.Mvp1PlayerId, &rec.Mvp1Merit,
		&rec.Mvp2PlayerId, &rec.Mvp2Merit, &rec.FirstWinPlayer1Id, &rec.FirstWinPlayer2Id, &rec.FirstWinAtMs, &rec.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, nil
	}
	return rec, err
}

func (r *WarRepo) GetPlayerWarState(ctx context.Context, playerID uint64, zoneID uint32, weekKey uint32) (*pb.GuildWarPlayerStateRecord, error) {
	rec := &pb.GuildWarPlayerStateRecord{PlayerId: playerID, ZoneId: zoneID, WeeklyWeekKey: weekKey}
	err := r.db.QueryRowContext(ctx,
		"SELECT `zone_id`,`peace_mode`,`peace_ready_at_ms`,`open_challenge`,`protect_until_ms`,`streak_count`,`streak_distinct_count`,`streak_last_opponent_id`,`weekly_week_key`,`weekly_merit`,`milestone_claimed_mask`,`dividend_claimed_count`,`bounty_protect_until_ms`,`last_garrison_refresh_ms`,`last_track_ms`,`updated_ms` FROM `"+guildWarPlayerStateTable+"` WHERE `player_id` = ?",
		playerID).Scan(&rec.ZoneId, &rec.PeaceMode, &rec.PeaceReadyAtMs, &rec.OpenChallenge, &rec.ProtectUntilMs,
		&rec.StreakCount, &rec.StreakDistinctCount, &rec.StreakLastOpponentId, &rec.WeeklyWeekKey, &rec.WeeklyMerit,
		&rec.MilestoneClaimedMask, &rec.DividendClaimedCount, &rec.BountyProtectUntilMs, &rec.LastGarrisonRefreshMs,
		&rec.LastTrackMs, &rec.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	if rec.WeeklyWeekKey != weekKey {
		rec.WeeklyWeekKey = weekKey
		rec.WeeklyMerit = 0
		rec.MilestoneClaimedMask = 0
		rec.DividendClaimedCount = 0
	}
	return rec, nil
}

func (r *WarRepo) ListOpenChallengePlayers(ctx context.Context, zoneID uint32, limit int) ([]*pb.GuildWarPlayerStateRecord, error) {
	if limit <= 0 || limit > 64 {
		limit = 30
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT `player_id`,`zone_id`,`peace_mode`,`peace_ready_at_ms`,`open_challenge`,`protect_until_ms`,`streak_count`,`streak_distinct_count`,`weekly_merit` FROM `"+guildWarPlayerStateTable+"` WHERE `zone_id` = ? AND `open_challenge` = 1 AND `peace_mode` = 0 ORDER BY `updated_ms` DESC LIMIT ?",
		zoneID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarPlayerStateRecord
	for rows.Next() {
		rec := &pb.GuildWarPlayerStateRecord{}
		if err := rows.Scan(&rec.PlayerId, &rec.ZoneId, &rec.PeaceMode, &rec.PeaceReadyAtMs, &rec.OpenChallenge,
			&rec.ProtectUntilMs, &rec.StreakCount, &rec.StreakDistinctCount, &rec.WeeklyMerit); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *WarRepo) ListStreakLeaders(ctx context.Context, zoneID uint32, limit int) ([]*pb.GuildWarPlayerStateRecord, error) {
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT `player_id`,`zone_id`,`streak_count`,`streak_distinct_count` FROM `"+guildWarPlayerStateTable+"` WHERE `zone_id` = ? AND `streak_count` >= 3 AND `streak_distinct_count` >= 2 ORDER BY `streak_count` DESC, `updated_ms` DESC LIMIT ?",
		zoneID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarPlayerStateRecord
	for rows.Next() {
		rec := &pb.GuildWarPlayerStateRecord{}
		if err := rows.Scan(&rec.PlayerId, &rec.ZoneId, &rec.StreakCount, &rec.StreakDistinctCount); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *WarRepo) UpsertPlayerStance(ctx context.Context, playerID uint64, zoneID uint32, weekKey uint32, peaceMode, openChallenge bool, nowMs uint64, cooldownSec uint32) (*pb.GuildWarPlayerStateRecord, error) {
	var out *pb.GuildWarPlayerStateRecord
	err := r.guilds.inTx(ctx, "war_set_stance", func(ctx context.Context, tx *sql.Tx) error {
		cur, err := lockPlayerWarStateTx(ctx, tx, playerID, zoneID, weekKey, nowMs)
		if err != nil {
			return err
		}
		// 从战意(0)切回和平免战(1)需经过脱战 60s 冷却；和平免战开启时自动关闭挂牌迎战。
		if peaceMode && cur.PeaceMode == 0 && nowMs < cur.PeaceReadyAtMs {
			return ErrWarPeaceCooldown
		}
		if peaceMode {
			cur.PeaceMode = 1
			cur.OpenChallenge = 0
		} else {
			if cur.PeaceMode == 1 && cooldownSec > 0 {
				// 主动开启战意后立刻进入冷却窗口，防打完立刻秒切和平躲反击。
				cur.PeaceReadyAtMs = nowMs + uint64(cooldownSec)*1000
			}
			cur.PeaceMode = 0
			if openChallenge {
				cur.OpenChallenge = 1
			} else {
				cur.OpenChallenge = 0
			}
		}
		cur.ZoneId = zoneID
		cur.UpdatedMs = nowMs
		if err := savePlayerWarStateTx(ctx, tx, cur); err != nil {
			return err
		}
		out = cur
		return nil
	})
	return out, err
}

func (r *WarRepo) UpsertGarrison(ctx context.Context, guildID, playerID uint64, zoneID, weekKey uint32, withdraw bool, rec *pb.GuildWarGarrisonRecord, nowMs uint64, maxSlots, cooldownSec uint32) ([]*pb.GuildWarGarrisonRecord, uint64, error) {
	var slots []*pb.GuildWarGarrisonRecord
	var nextRefreshMs uint64
	err := r.guilds.inTx(ctx, "war_garrison", func(ctx context.Context, tx *sql.Tx) error {
		ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, guildID, playerID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotGuildMember
		}
		existing, err := lockGuildGarrisonTx(ctx, tx, guildID)
		if err != nil {
			return err
		}
		pstate, err := lockPlayerWarStateTx(ctx, tx, playerID, zoneID, weekKey, nowMs)
		if err != nil {
			return err
		}
		var mySlot uint32
		used := map[uint32]bool{}
		for _, s := range existing {
			used[s.SlotIndex] = true
			if s.PlayerId == playerID {
				mySlot = s.SlotIndex
			}
		}
		if withdraw {
			if mySlot != 0 {
				if _, err := tx.ExecContext(ctx, "DELETE FROM `"+guildWarGarrisonTable+"` WHERE `guild_id` = ? AND `slot_index` = ?", guildID, mySlot); err != nil {
					return err
				}
			}
		} else {
			cdMs := uint64(cooldownSec) * 1000
			if mySlot != 0 && pstate.LastGarrisonRefreshMs > 0 && nowMs < pstate.LastGarrisonRefreshMs+cdMs {
				return ErrWarGarrisonCooldown
			}
			targetSlot := mySlot
			if targetSlot == 0 {
				for idx := uint32(1); idx <= maxSlots; idx++ {
					if !used[idx] {
						targetSlot = idx
						break
					}
				}
				if targetSlot == 0 {
					return ErrWarGarrisonFull
				}
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `"+guildWarGarrisonTable+"` (`guild_id`,`slot_index`,`player_id`,`player_name`,`level`,`appearance_id`,`pet_name`,`pet_table_id`,`snapshot_payload`,`updated_ms`) VALUES (?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE `player_id`=VALUES(`player_id`),`player_name`=VALUES(`player_name`),`level`=VALUES(`level`),`appearance_id`=VALUES(`appearance_id`),`pet_name`=VALUES(`pet_name`),`pet_table_id`=VALUES(`pet_table_id`),`snapshot_payload`=VALUES(`snapshot_payload`),`updated_ms`=VALUES(`updated_ms`)",
				guildID, targetSlot, playerID, rec.PlayerName, rec.Level, rec.AppearanceId, rec.PetName, rec.PetTableId, rec.SnapshotPayload, nowMs); err != nil {
				return err
			}
			pstate.LastGarrisonRefreshMs = nowMs
			pstate.UpdatedMs = nowMs
			if err := savePlayerWarStateTx(ctx, tx, pstate); err != nil {
				return err
			}
			nextRefreshMs = nowMs + cdMs
		}
		slots, err = lockGuildGarrisonTx(ctx, tx, guildID)
		return err
	})
	return slots, nextRefreshMs, err
}

// RemovePlayerGarrison 离帮 / 被踢时清掉本人守旗快照。
func (r *WarRepo) RemovePlayerGarrison(ctx context.Context, playerID uint64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM `"+guildWarGarrisonTable+"` WHERE `player_id` = ?", playerID)
	return err
}

// CleanupGuildWarOnDisband 解散帮会时清守旗快照并释放其占领的灵脉。
func (r *WarRepo) CleanupGuildWarOnDisband(ctx context.Context, guildID uint64, nowMs uint64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM `"+guildWarGarrisonTable+"` WHERE `guild_id` = ?", guildID); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, "UPDATE `"+guildWarLeylineTable+"` SET `owner_guild_id` = 0, `shield_until_ms` = 0, `updated_ms` = ? WHERE `owner_guild_id` = ?", nowMs, guildID)
	return err
}

type ClaimMilestoneInput struct {
	GuildID          uint64
	PlayerID         uint64
	ZoneID           uint32
	WeekKey          uint32
	MilestoneID      uint32
	RequiredMerit    uint64
	MinPersonalMerit uint64
	ContributionGain uint64
	OpID             uint64
	BundleBytes      []byte
	LeaseUntilMs     uint64
	LeaseToken       uint64
	NowMs            uint64
}

func (r *WarRepo) ClaimMilestone(ctx context.Context, in ClaimMilestoneInput) (AssetOpRow, bool, error) {
	var createdOp AssetOpRow
	var hasAssetOp bool
	err := r.guilds.inTx(ctx, "war_claim_milestone", func(ctx context.Context, tx *sql.Tx) error {
		// 锁序：M(guild_member) -> Q(seq) -> O(asset_op) -> weekly_progress -> player_state。
		ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, in.GuildID, in.PlayerID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotGuildMember
		}
		// 普通读先检是否已达标与已领（锁内会再复核），避免不合格请求白分配 seq。
		prog, err := loadWeeklyProgressTx(ctx, tx, in.GuildID, in.WeekKey, false)
		if err != nil {
			return err
		}
		if prog.GuildMerit < in.RequiredMerit {
			return ErrWarMilestoneLocked
		}
		bit := uint32(1) << (in.MilestoneID - 1)
		if len(in.BundleBytes) > 0 && in.OpID != 0 {
			stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
			if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
				return err
			}
			alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
			if err != nil {
				return err
			}
			opRec := &pb.GuildAssetOpRecord{
				OpId:          in.OpID,
				PlayerId:      in.PlayerID,
				Stream:        uint32(stream),
				Seq:           alloc.Seq,
				GuildId:       in.GuildID,
				Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD,
				Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
				NextAttemptMs: in.LeaseUntilMs,
				Payload:       in.BundleBytes,
				RefId:         in.MilestoneID,
				RefCount:      1,
				PeriodKey:     in.WeekKey,
				CreatedMs:     in.NowMs,
				UpdatedMs:     in.NowMs,
				LeaseUntilMs:  in.LeaseUntilMs,
				LeaseToken:    in.LeaseToken,
				TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_WAR_REWARD),
				StreamEpoch:   alloc.Epoch,
			}
			if err := insertAssetOp(ctx, tx, opRec); err != nil {
				return err
			}
			createdOp = assetOpRowFrom(opRec)
			hasAssetOp = true
		}
		pstate, err := lockPlayerWarStateTx(ctx, tx, in.PlayerID, in.ZoneID, in.WeekKey, in.NowMs)
		if err != nil {
			return err
		}
		if pstate.WeeklyMerit < in.MinPersonalMerit {
			return ErrWarMilestoneLocked
		}
		if (pstate.MilestoneClaimedMask & bit) != 0 {
			return ErrWarMilestoneClaimed
		}
		pstate.MilestoneClaimedMask |= bit
		pstate.UpdatedMs = in.NowMs
		if err := savePlayerWarStateTx(ctx, tx, pstate); err != nil {
			return err
		}
		if in.ContributionGain > 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `guild_member` SET `contribution_balance` = `contribution_balance` + ?, `contribution_total` = `contribution_total` + ? WHERE `guild_id` = ? AND `player_id` = ?",
				in.ContributionGain, in.ContributionGain, in.GuildID, in.PlayerID); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		r.guilds.invalidateAfterCommit(ctx, "war_claim_milestone", in.GuildID)
	}
	return createdOp, hasAssetOp, err
}

type ClaimDividendInput struct {
	GuildID          uint64
	PlayerID         uint64
	ZoneID           uint32
	WeekKey          uint32
	MinPersonalMerit uint64
	GuildWeeklyCap   uint32
	PlayerWeeklyCap  uint32
	IntervalSec      uint32
	SilverPerShare   uint64
	ContribPerShare  uint64
	OpID             uint64
	BuildBundle      func(silver uint64) ([]byte, error)
	LeaseUntilMs     uint64
	LeaseToken       uint64
	NowMs            uint64
}

type ClaimDividendResult struct {
	SharesClaimed uint32
	SilverGain    uint64
	ContribGain   uint64
	Op            AssetOpRow
	HasOp         bool
}

func (r *WarRepo) ClaimDividend(ctx context.Context, in ClaimDividendInput) (ClaimDividendResult, error) {
	var res ClaimDividendResult
	err := r.guilds.inTx(ctx, "war_claim_dividend", func(ctx context.Context, tx *sql.Tx) error {
		ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, in.GuildID, in.PlayerID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotGuildMember
		}
		// 先在锁序前预读估算可领份数，按需推进灵脉每小时分红（严格锁序：M -> Q -> O -> leyline -> weekly_progress -> player_state）。
		// 注意：先锁 Q/O 后再锁 leyline -> weekly_progress -> player_state 保证锁序单调；如最终算出 0 份直接回滚不留脏 op。
		var availShares uint32
		if prog, err := loadWeeklyProgressTx(ctx, tx, in.GuildID, in.WeekKey, false); err == nil {
			availShares = prog.DividendPoolCount
		}
		// 估算尚未结算进 weekly_progress 的整小时份数。
		pendingHours := estimatePendingDividendSharesTx(ctx, tx, in.ZoneID, in.GuildID, in.WeekKey, in.IntervalSec, in.GuildWeeklyCap, in.NowMs)
		estPool := availShares + pendingHours
		if estPool > in.GuildWeeklyCap {
			estPool = in.GuildWeeklyCap
		}
		pstatePreview, err := loadPlayerWarStatePlainTx(ctx, tx, in.PlayerID, in.ZoneID, in.WeekKey)
		if err != nil {
			return err
		}
		if pstatePreview.WeeklyMerit < in.MinPersonalMerit {
			return ErrWarMilestoneLocked
		}
		if pstatePreview.DividendClaimedCount >= in.PlayerWeeklyCap || pstatePreview.DividendClaimedCount >= estPool {
			return ErrWarDividendEmpty
		}
		claimable := estPool - pstatePreview.DividendClaimedCount
		if pstatePreview.DividendClaimedCount+claimable > in.PlayerWeeklyCap {
			claimable = in.PlayerWeeklyCap - pstatePreview.DividendClaimedCount
		}

		// 先按最终确定的份数预留资产指令（锁序 Q -> O），若锁住 leyline/progress 后实算与估算不同则回滚重试。
		silverTotal := uint64(claimable) * in.SilverPerShare
		if silverTotal > 0 && in.OpID != 0 && in.BuildBundle != nil {
			bundleBytes, err := in.BuildBundle(silverTotal)
			if err != nil {
				return err
			}
			stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
			if err := ensureSeqRowTx(ctx, tx, in.PlayerID, stream, in.NowMs); err != nil {
				return err
			}
			alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
			if err != nil {
				return err
			}
			opRec := &pb.GuildAssetOpRecord{
				OpId:          in.OpID,
				PlayerId:      in.PlayerID,
				Stream:        uint32(stream),
				Seq:           alloc.Seq,
				GuildId:       in.GuildID,
				Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD,
				Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
				NextAttemptMs: in.LeaseUntilMs,
				Payload:       bundleBytes,
				RefId:         in.WeekKey,
				RefCount:      claimable,
				PeriodKey:     in.WeekKey,
				CreatedMs:     in.NowMs,
				UpdatedMs:     in.NowMs,
				LeaseUntilMs:  in.LeaseUntilMs,
				LeaseToken:    in.LeaseToken,
				TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_WAR_REWARD),
				StreamEpoch:   alloc.Epoch,
			}
			if err := insertAssetOp(ctx, tx, opRec); err != nil {
				return err
			}
			res.Op = assetOpRowFrom(opRec)
			res.HasOp = true
		}

		added, err := accrueGuildDividendTx(ctx, tx, in.ZoneID, in.GuildID, in.WeekKey, in.IntervalSec, in.GuildWeeklyCap, in.NowMs)
		if err != nil {
			return err
		}
		prog, err := loadWeeklyProgressTx(ctx, tx, in.GuildID, in.WeekKey, true)
		if err != nil {
			return err
		}
		if added > 0 {
			prog.DividendPoolCount += added
			if prog.DividendPoolCount > in.GuildWeeklyCap {
				prog.DividendPoolCount = in.GuildWeeklyCap
			}
			prog.UpdatedMs = in.NowMs
			if err := saveWeeklyProgressTx(ctx, tx, prog); err != nil {
				return err
			}
		}
		pstate, err := lockPlayerWarStateTx(ctx, tx, in.PlayerID, in.ZoneID, in.WeekKey, in.NowMs)
		if err != nil {
			return err
		}
		if pstate.WeeklyMerit < in.MinPersonalMerit {
			return ErrWarMilestoneLocked
		}
		realClaimable := uint32(0)
		if prog.DividendPoolCount > pstate.DividendClaimedCount && pstate.DividendClaimedCount < in.PlayerWeeklyCap {
			realClaimable = prog.DividendPoolCount - pstate.DividendClaimedCount
			if pstate.DividendClaimedCount+realClaimable > in.PlayerWeeklyCap {
				realClaimable = in.PlayerWeeklyCap - pstate.DividendClaimedCount
			}
		}
		if realClaimable == 0 {
			return ErrWarDividendEmpty
		}
		if realClaimable != claimable {
			return ErrWriteConflict
		}
		pstate.DividendClaimedCount += realClaimable
		pstate.UpdatedMs = in.NowMs
		if err := savePlayerWarStateTx(ctx, tx, pstate); err != nil {
			return err
		}
		contribTotal := uint64(realClaimable) * in.ContribPerShare
		if contribTotal > 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `guild_member` SET `contribution_balance` = `contribution_balance` + ?, `contribution_total` = `contribution_total` + ? WHERE `guild_id` = ? AND `player_id` = ?",
				contribTotal, contribTotal, in.GuildID, in.PlayerID); err != nil {
				return err
			}
		}
		res.SharesClaimed = realClaimable
		res.SilverGain = silverTotal
		res.ContribGain = contribTotal
		return nil
	})
	if err == nil {
		r.guilds.invalidateAfterCommit(ctx, "war_claim_dividend", in.GuildID)
	}
	return res, err
}

type PostBountyInput struct {
	BountyID         uint64
	ZoneID           uint32
	WeekKey          uint32
	DayKey           uint32
	IssuerPlayerID   uint64
	IssuerGuildID    uint64
	TargetPlayerID   uint64
	TargetGuildID    uint64
	Tier             pb.GuildWarBountyTier
	CostSilver       uint64
	CostGuildFunds   uint64
	PoolSilver       uint64
	PublicBoard      bool
	PublicDailyLimit uint32
	TTLSec           uint32
	OpID             uint64
	DebitBundle      []byte
	LeaseUntilMs     uint64
	LeaseToken       uint64
	NowMs            uint64
}

func (r *WarRepo) PostBounty(ctx context.Context, in PostBountyInput) (*pb.GuildWarBountyRecord, AssetOpRow, bool, error) {
	var created *pb.GuildWarBountyRecord
	var opRow AssetOpRow
	var hasOp bool
	err := r.guilds.inTx(ctx, "war_post_bounty", func(ctx context.Context, tx *sql.Tx) error {
		// 锁序：G(如公费档扣帮会资金) -> M(issuer) -> Q -> O -> C(公费日限) -> player_state(target保护期) -> bounty -> rivalry。
		if in.CostGuildFunds > 0 {
			var funds uint64
			err := tx.QueryRowContext(ctx, "SELECT `funds` FROM `guild` WHERE `guild_id` = ? FOR UPDATE", in.IssuerGuildID).Scan(&funds)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGuildGone
			}
			if err != nil {
				return err
			}
			if funds < in.CostGuildFunds {
				return ErrFundsInsufficient
			}
			if _, err := tx.ExecContext(ctx, "UPDATE `guild` SET `funds` = `funds` - ? WHERE `guild_id` = ?", in.CostGuildFunds, in.IssuerGuildID); err != nil {
				return err
			}
		}
		var role uint32
		err := tx.QueryRowContext(ctx, sqlLockMemberRole, in.IssuerGuildID, in.IssuerPlayerID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return err
		}
		if in.Tier == pb.GuildWarBountyTier_GUILD_WAR_BOUNTY_TIER_GUILD_PUBLIC && role == 0 {
			return ErrRankTooLow
		}
		// 银两档：走 GUILD_DEBIT TX_GUILD_BOUNTY_ESCROW 扣款，终结后激活追杀令(1->2)；公费档直接 ACTIVE(2)。
		bountyStatus := uint32(2)
		escrowOpID := uint64(0)
		if in.CostSilver > 0 && in.OpID != 0 && len(in.DebitBundle) > 0 {
			stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT
			if err := ensureSeqRowTx(ctx, tx, in.IssuerPlayerID, stream, in.NowMs); err != nil {
				return err
			}
			alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, in.IssuerPlayerID, stream, assetop.DefaultLimits, in.NowMs)
			if err != nil {
				return err
			}
			opRec := &pb.GuildAssetOpRecord{
				OpId:          in.OpID,
				PlayerId:      in.IssuerPlayerID,
				Stream:        uint32(stream),
				Seq:           alloc.Seq,
				GuildId:       in.IssuerGuildID,
				Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_ESCROW,
				Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
				NextAttemptMs: in.LeaseUntilMs,
				DeadlineMs:    in.NowMs + 60_000,
				Payload:       in.DebitBundle,
				RefId:         uint32(in.Tier),
				RefCount:      1,
				PeriodKey:     in.DayKey,
				CreatedMs:     in.NowMs,
				UpdatedMs:     in.NowMs,
				LeaseUntilMs:  in.LeaseUntilMs,
				LeaseToken:    in.LeaseToken,
				TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_BOUNTY_ESCROW),
				StreamEpoch:   alloc.Epoch,
			}
			if err := insertAssetOp(ctx, tx, opRec); err != nil {
				return err
			}
			opRow = assetOpRowFrom(opRec)
			hasOp = true
			bountyStatus = 1 // ESCROW_PENDING
			escrowOpID = in.OpID
		} else if in.PublicDailyLimit > 0 {
			limited, err := upsertCounterWithLimit(ctx, tx, in.IssuerGuildID,
				pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_PUBLIC_BOUNTY, 1, in.DayKey, 1, in.PublicDailyLimit, in.NowMs)
			if err != nil {
				return err
			}
			if limited {
				return ErrWarBountyLimit
			}
		}

		// 检查目标是否处于 30 分钟免再挂保护期或已有未结追杀令。
		targetState, err := lockPlayerWarStateTx(ctx, tx, in.TargetPlayerID, in.ZoneID, in.WeekKey, in.NowMs)
		if err != nil {
			return err
		}
		if in.NowMs < targetState.BountyProtectUntilMs {
			return ErrWarBountyActive
		}
		var activeCnt int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM `"+guildWarBountyTable+"` WHERE `target_player_id` = ? AND `status` IN (1, 2) AND `expires_at_ms` > ?",
			in.TargetPlayerID, in.NowMs).Scan(&activeCnt); err != nil {
			return err
		}
		if activeCnt > 0 {
			return ErrWarBountyActive
		}

		pubFlag := uint32(0)
		if in.PublicBoard {
			pubFlag = 1
		}
		expiresAt := in.NowMs + uint64(in.TTLSec)*1000
		rec := &pb.GuildWarBountyRecord{
			BountyId:       in.BountyID,
			ZoneId:         in.ZoneID,
			IssuerPlayerId: in.IssuerPlayerID,
			IssuerGuildId:  in.IssuerGuildID,
			TargetPlayerId: in.TargetPlayerID,
			TargetGuildId:  in.TargetGuildID,
			Tier:           uint32(in.Tier),
			EscrowSilver:   in.PoolSilver,
			PublicBoard:    pubFlag,
			Status:         bountyStatus,
			EscrowOpId:     escrowOpID,
			CreatedAtMs:    in.NowMs,
			ExpiresAtMs:    expiresAt,
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `"+guildWarBountyTable+"` (`bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms`) VALUES (?,?,?,?,?,?,?,?,?,?,?,0,0,0,?,?,0)",
			rec.BountyId, rec.ZoneId, rec.IssuerPlayerId, rec.IssuerGuildId, rec.TargetPlayerId, rec.TargetGuildId,
			rec.Tier, rec.EscrowSilver, rec.PublicBoard, rec.Status, rec.EscrowOpId, rec.CreatedAtMs, rec.ExpiresAtMs); err != nil {
			return err
		}
		// 互挂追杀令直接标记本周宿敌（guild_war_rivalry 锁序 19 > bounty 17）。
		if in.IssuerGuildID != 0 && in.TargetGuildID != 0 && in.IssuerGuildID != in.TargetGuildID {
			if err := markRivalryTx(ctx, tx, in.IssuerGuildID, in.TargetGuildID, in.WeekKey, 0, true, in.NowMs); err != nil {
				return err
			}
		}
		created = rec
		return nil
	})
	if err == nil && in.CostGuildFunds > 0 {
		r.guilds.invalidateAfterCommit(ctx, "war_post_bounty", in.IssuerGuildID)
	}
	return created, opRow, hasOp, err
}

func (r *WarRepo) ListActiveBounties(ctx context.Context, zoneID uint32, myGuildID uint64, nowMs uint64) ([]*pb.GuildWarBountyRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT `bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms` FROM `"+guildWarBountyTable+"` WHERE `zone_id` = ? AND `status` IN (1, 2) AND `expires_at_ms` > ? AND (`public_board` = 1 OR `issuer_guild_id` = ?) ORDER BY `created_at_ms` DESC LIMIT 50",
		zoneID, nowMs, myGuildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBounties(rows)
}

func (r *WarRepo) GetBounty(ctx context.Context, bountyID uint64) (*pb.GuildWarBountyRecord, error) {
	rec := &pb.GuildWarBountyRecord{}
	err := r.db.QueryRowContext(ctx,
		"SELECT `bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms` FROM `"+guildWarBountyTable+"` WHERE `bounty_id` = ?",
		bountyID).Scan(&rec.BountyId, &rec.ZoneId, &rec.IssuerPlayerId, &rec.IssuerGuildId, &rec.TargetPlayerId,
		&rec.TargetGuildId, &rec.Tier, &rec.EscrowSilver, &rec.PublicBoard, &rec.Status, &rec.EscrowOpId,
		&rec.CounterKills, &rec.LastCounterAttackerId, &rec.HunterPlayerId, &rec.CreatedAtMs, &rec.ExpiresAtMs, &rec.ResolvedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

func (r *WarRepo) ActiveBountyByTarget(ctx context.Context, targetPlayerID uint64, nowMs uint64) (*pb.GuildWarBountyRecord, error) {
	rec := &pb.GuildWarBountyRecord{}
	err := r.db.QueryRowContext(ctx,
		"SELECT `bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms` FROM `"+guildWarBountyTable+"` WHERE `target_player_id` = ? AND `status` = 2 AND `expires_at_ms` > ? LIMIT 1",
		targetPlayerID, nowMs).Scan(&rec.BountyId, &rec.ZoneId, &rec.IssuerPlayerId, &rec.IssuerGuildId, &rec.TargetPlayerId,
		&rec.TargetGuildId, &rec.Tier, &rec.EscrowSilver, &rec.PublicBoard, &rec.Status, &rec.EscrowOpId,
		&rec.CounterKills, &rec.LastCounterAttackerId, &rec.HunterPlayerId, &rec.CreatedAtMs, &rec.ExpiresAtMs, &rec.ResolvedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

func (r *WarRepo) MarkTrackCooldown(ctx context.Context, playerID uint64, zoneID, weekKey uint32, nowMs uint64, cooldownSec uint32) (uint64, error) {
	var nextAt uint64
	err := r.guilds.inTx(ctx, "war_track_bounty", func(ctx context.Context, tx *sql.Tx) error {
		st, err := lockPlayerWarStateTx(ctx, tx, playerID, zoneID, weekKey, nowMs)
		if err != nil {
			return err
		}
		cdMs := uint64(cooldownSec) * 1000
		if st.LastTrackMs > 0 && nowMs < st.LastTrackMs+cdMs {
			return ErrWarTrackCooldown
		}
		st.LastTrackMs = nowMs
		st.UpdatedMs = nowMs
		if err := savePlayerWarStateTx(ctx, tx, st); err != nil {
			return err
		}
		nextAt = nowMs + cdMs
		return nil
	})
	return nextAt, err
}

func (r *WarRepo) ListRivals(ctx context.Context, guildID uint64, weekKey uint32) ([]uint64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT `guild_id_low`,`guild_id_high`,`clash_count`,`bounty_marked` FROM `"+guildWarRivalryTable+"` WHERE `week_key` = ? AND (`guild_id_low` = ? OR `guild_id_high` = ?)",
		weekKey, guildID, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rivals []uint64
	for rows.Next() {
		var low, high uint64
		var clash, bmark uint32
		if err := rows.Scan(&low, &high, &clash, &bmark); err != nil {
			return nil, err
		}
		if war.IsRivalryActive(clash, bmark != 0) {
			if low == guildID {
				rivals = append(rivals, high)
			} else {
				rivals = append(rivals, low)
			}
		}
	}
	return rivals, rows.Err()
}

func (r *WarRepo) ListRecentBattles(ctx context.Context, guildID uint64, limit int) ([]*pb.GuildWarBattleRecord, error) {
	if limit <= 0 || limit > 30 {
		limit = 30
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT `battle_id`,`zone_id`,`war_battle_kind`,`leyline_id`,`bounty_id`,`attacker_guild_id`,`defender_guild_id`,`outcome`,`total_rounds`,`banner_hp_before`,`banner_hp_after`,`banner_shielded`,`bounty_outcome_tag`,`jackpot_contribution`,`jackpot_silver`,`diminish_percent`,`attacker_contribution`,`attacker_silver`,`attacker_funds`,`attacker_merit`,`defender_contribution`,`defender_silver`,`defender_funds`,`defender_merit`,`summary_payload`,`finished_at_ms` FROM `"+guildWarBattleTable+"` WHERE `attacker_guild_id` = ? OR `defender_guild_id` = ? ORDER BY `finished_at_ms` DESC LIMIT ?",
		guildID, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarBattleRecord
	for rows.Next() {
		rec := &pb.GuildWarBattleRecord{}
		if err := rows.Scan(&rec.BattleId, &rec.ZoneId, &rec.WarBattleKind, &rec.LeylineId, &rec.BountyId,
			&rec.AttackerGuildId, &rec.DefenderGuildId, &rec.Outcome, &rec.TotalRounds, &rec.BannerHpBefore,
			&rec.BannerHpAfter, &rec.BannerShielded, &rec.BountyOutcomeTag, &rec.JackpotContribution,
			&rec.JackpotSilver, &rec.DiminishPercent, &rec.AttackerContribution, &rec.AttackerSilver,
			&rec.AttackerFunds, &rec.AttackerMerit, &rec.DefenderContribution, &rec.DefenderSilver,
			&rec.DefenderFunds, &rec.DefenderMerit, &rec.SummaryPayload, &rec.FinishedAtMs); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *WarRepo) GetWarBattle(ctx context.Context, battleID uint64) (*pb.GuildWarBattleRecord, error) {
	rec := &pb.GuildWarBattleRecord{}
	err := r.db.QueryRowContext(ctx,
		"SELECT `battle_id`,`zone_id`,`war_battle_kind`,`leyline_id`,`bounty_id`,`attacker_guild_id`,`defender_guild_id`,`outcome`,`total_rounds`,`banner_hp_before`,`banner_hp_after`,`banner_shielded`,`bounty_outcome_tag`,`jackpot_contribution`,`jackpot_silver`,`diminish_percent`,`attacker_contribution`,`attacker_silver`,`attacker_funds`,`attacker_merit`,`defender_contribution`,`defender_silver`,`defender_funds`,`defender_merit`,`summary_payload`,`finished_at_ms` FROM `"+guildWarBattleTable+"` WHERE `battle_id` = ?",
		battleID).Scan(&rec.BattleId, &rec.ZoneId, &rec.WarBattleKind, &rec.LeylineId, &rec.BountyId,
		&rec.AttackerGuildId, &rec.DefenderGuildId, &rec.Outcome, &rec.TotalRounds, &rec.BannerHpBefore,
		&rec.BannerHpAfter, &rec.BannerShielded, &rec.BountyOutcomeTag, &rec.JackpotContribution,
		&rec.JackpotSilver, &rec.DiminishPercent, &rec.AttackerContribution, &rec.AttackerSilver,
		&rec.AttackerFunds, &rec.AttackerMerit, &rec.DefenderContribution, &rec.DefenderSilver,
		&rec.DefenderFunds, &rec.DefenderMerit, &rec.SummaryPayload, &rec.FinishedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// PlayerSettlementCredit 单名参战者在帮战结算事务中的资产、帮贡与战功入账计划。
type PlayerSettlementCredit struct {
	PlayerID         uint64
	GuildID          uint64
	TeamIndex        uint32
	Won              bool
	Draw             bool
	ContributionGain uint64
	SilverGain       uint64
	MeritGain        uint64
	OpID             uint64
	CreditBundle     []byte
	TxType           rollbackpb.TransactionType
	OpKind           pb.GuildAssetOpKind
	OpponentID       uint64
	RepeatOpponent   bool
}

type SettleWarBattleInput struct {
	Record              *pb.GuildWarBattleRecord
	WeekKey             uint32
	DayKey              uint32
	NowMs               uint64
	ProtectSec          uint32
	PeaceCooldownSec    uint32
	BountyProtectSec    uint32
	CounterKillTear     uint32
	DailyPayoutLimit    uint32
	AttackerGuildFunds  uint64
	DefenderGuildFunds  uint64
	AttackerMeritAdd    uint64
	DefenderMeritAdd    uint64
	BannerDelta         *war.BannerDelta
	PlayerCredits       []PlayerSettlementCredit
	ActiveBountyID      uint64
	BountyHunterWon     bool
	BountyTargetWon     bool
	BountyTargetID      uint64
	BountyHunterID      uint64
	BountyPayoutOpID    uint64
	BountyPayoutBundle  func(silver uint64) ([]byte, error)
	LeaseUntilMs        uint64
	LeaseToken          uint64
}

type SettleWarBattleOutput struct {
	CreatedOps         []AssetOpRow
	BountyTag          uint32
	BountyPayoutSilver uint64
	BountyRecipientID  uint64
	FirstWinRecorded   bool
}

func (r *WarRepo) SettleWarBattle(ctx context.Context, in SettleWarBattleInput) (SettleWarBattleOutput, error) {
	var out SettleWarBattleOutput
	rec := in.Record
	err := r.guilds.inTx(ctx, "war_settle_battle", func(ctx context.Context, tx *sql.Tx) error {
		// 幂等预查（普通读）。
		var existingID uint64
		if err := tx.QueryRowContext(ctx, "SELECT `battle_id` FROM `"+guildWarBattleTable+"` WHERE `battle_id` = ?", rec.BattleId).Scan(&existingID); err == nil {
			return ErrWarBattleAlreadyDone
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// 1. 按 guild_id 升序锁涉及帮会（锁序位置 1: guild）并加帮会资金。
		guildFunds := map[uint64]uint64{}
		if rec.AttackerGuildId != 0 && in.AttackerGuildFunds > 0 {
			guildFunds[rec.AttackerGuildId] += in.AttackerGuildFunds
		}
		if rec.DefenderGuildId != 0 && in.DefenderGuildFunds > 0 {
			guildFunds[rec.DefenderGuildId] += in.DefenderGuildFunds
		}
		guildIDs := make([]uint64, 0, len(guildFunds))
		for gid := range guildFunds {
			guildIDs = append(guildIDs, gid)
		}
		sort.Slice(guildIDs, func(i, j int) bool { return guildIDs[i] < guildIDs[j] })
		for _, gid := range guildIDs {
			if ok, err := lockRowExists(ctx, tx, sqlLockGuildFunds, gid); err != nil {
				return err
			} else if ok {
				if _, err := tx.ExecContext(ctx, "UPDATE `guild` SET `funds` = `funds` + ? WHERE `guild_id` = ?", guildFunds[gid], gid); err != nil {
					return err
				}
			}
		}

		// 2. 按 (guild_id, player_id) 升序锁成员并加帮贡（锁序位置 3: guild_member）。
		credits := append([]PlayerSettlementCredit(nil), in.PlayerCredits...)
		sort.Slice(credits, func(i, j int) bool {
			if credits[i].GuildID != credits[j].GuildID {
				return credits[i].GuildID < credits[j].GuildID
			}
			return credits[i].PlayerID < credits[j].PlayerID
		})
		for _, c := range credits {
			if c.GuildID == 0 || c.PlayerID == 0 {
				continue
			}
			if ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, c.GuildID, c.PlayerID); err != nil {
				return err
			} else if ok && c.ContributionGain > 0 {
				if _, err := tx.ExecContext(ctx,
					"UPDATE `guild_member` SET `contribution_balance` = `contribution_balance` + ?, `contribution_total` = `contribution_total` + ? WHERE `guild_id` = ? AND `player_id` = ?",
					c.ContributionGain, c.ContributionGain, c.GuildID, c.PlayerID); err != nil {
					return err
				}
			}
		}

		// 3. 按 player_id 升序建 seq 并插 GUILD_CREDIT 资产指令（锁序位置 5 -> 6）。
		sort.Slice(credits, func(i, j int) bool { return credits[i].PlayerID < credits[j].PlayerID })
		stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
		for _, c := range credits {
			if c.PlayerID == 0 || c.OpID == 0 || len(c.CreditBundle) == 0 {
				continue
			}
			if err := ensureSeqRowTx(ctx, tx, c.PlayerID, stream, in.NowMs); err != nil {
				return err
			}
			alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, c.PlayerID, stream, assetop.DefaultLimits, in.NowMs)
			if err != nil {
				return err
			}
			kind := c.OpKind
			if kind == 0 {
				kind = pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD
			}
			txType := c.TxType
			if txType == 0 {
				txType = rollbackpb.TransactionType_TX_GUILD_WAR_REWARD
			}
			opRec := &pb.GuildAssetOpRecord{
				OpId:          c.OpID,
				PlayerId:      c.PlayerID,
				Stream:        uint32(stream),
				Seq:           alloc.Seq,
				GuildId:       c.GuildID,
				Kind:          kind,
				Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
				NextAttemptMs: in.LeaseUntilMs,
				Payload:       c.CreditBundle,
				RefId:         rec.LeylineId,
				RefCount:      1,
				PeriodKey:     in.WeekKey,
				CreatedMs:     in.NowMs,
				UpdatedMs:     in.NowMs,
				LeaseUntilMs:  in.LeaseUntilMs,
				LeaseToken:    in.LeaseToken,
				TxType:        uint32(txType),
				StreamEpoch:   alloc.Epoch,
			}
			if err := insertAssetOp(ctx, tx, opRec); err != nil {
				return err
			}
			out.CreatedOps = append(out.CreatedOps, assetOpRowFrom(opRec))
		}

		// 4. 更新灵脉归属、旗势与护盾（锁序 13: guild_war_leyline）。
		if rec.LeylineId > 0 && in.BannerDelta != nil {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `"+guildWarLeylineTable+"` SET `owner_guild_id` = ?, `banner_hp` = ?, `shield_until_ms` = ?, `captured_at_ms` = CASE WHEN ? THEN ? ELSE `captured_at_ms` END, `updated_ms` = ? WHERE `zone_id` = ? AND `leyline_id` = ?",
				in.BannerDelta.OwnerAfter, in.BannerDelta.HPAfter, in.BannerDelta.ShieldUntilAfter,
				in.BannerDelta.CapturedByWinner, in.NowMs, in.NowMs, rec.ZoneId, rec.LeylineId); err != nil {
				return err
			}
		}

		// 5. 更新帮会周进度 + 本周前 2 名功臣 + 建帮首胜史记（锁序 15: guild_war_weekly_progress）。
		updateProg := func(gid uint64, addMerit uint64, teamIdx uint32, teamWon bool) error {
			if gid == 0 || (addMerit == 0 && !teamWon) {
				return nil
			}
			prog, err := loadWeeklyProgressTx(ctx, tx, gid, in.WeekKey, true)
			if err != nil {
				return err
			}
			prog.GuildMerit += addMerit
			prog.UpdatedMs = in.NowMs
			if err := saveWeeklyProgressTx(ctx, tx, prog); err != nil {
				return err
			}
			if teamWon {
				hist, err := loadWeeklyProgressTx(ctx, tx, gid, 0, true)
				if err != nil {
					return err
				}
				if hist.FirstWinAtMs == 0 {
					var p1, p2 uint64
					for _, c := range credits {
						if c.GuildID == gid && c.TeamIndex == teamIdx {
							if p1 == 0 {
								p1 = c.PlayerID
							} else if p2 == 0 {
								p2 = c.PlayerID
								break
							}
						}
					}
					hist.FirstWinPlayer1Id = p1
					hist.FirstWinPlayer2Id = p2
					hist.FirstWinAtMs = in.NowMs
					hist.UpdatedMs = in.NowMs
					if err := saveWeeklyProgressTx(ctx, tx, hist); err != nil {
						return err
					}
					out.FirstWinRecorded = true
				}
			}
			return nil
		}
		if err := updateProg(rec.AttackerGuildId, in.AttackerMeritAdd, 0, rec.Outcome == 1); err != nil {
			return err
		}
		if rec.DefenderGuildId != 0 && rec.DefenderGuildId != rec.AttackerGuildId {
			if err := updateProg(rec.DefenderGuildId, in.DefenderMeritAdd, 1, rec.Outcome == 2); err != nil {
				return err
			}
		}

		// 6. 更新个人帮战状态（战后 10s 保护、脱战 60s 冷却、连胜、周战功、MVP 榜更新；锁序 16）。
		for _, c := range credits {
			st, err := lockPlayerWarStateTx(ctx, tx, c.PlayerID, rec.ZoneId, in.WeekKey, in.NowMs)
			if err != nil {
				return err
			}
			st.ProtectUntilMs = in.NowMs + uint64(in.ProtectSec)*1000
			st.PeaceReadyAtMs = in.NowMs + uint64(in.PeaceCooldownSec)*1000
			st.WeeklyMerit += c.MeritGain
			if c.Won {
				// 同对手 30 分钟内不叠连胜。
				if !c.RepeatOpponent {
					st.StreakCount++
					if c.OpponentID != 0 && c.OpponentID != st.StreakLastOpponentId {
						st.StreakDistinctCount++
						st.StreakLastOpponentId = c.OpponentID
					}
				}
			} else if !c.Draw {
				st.StreakCount = 0
				st.StreakDistinctCount = 0
				st.StreakLastOpponentId = 0
			}
			st.UpdatedMs = in.NowMs
			if err := savePlayerWarStateTx(ctx, tx, st); err != nil {
				return err
			}
			if c.GuildID != 0 && st.WeeklyMerit > 0 {
				if err := updateWeeklyMvpTx(ctx, tx, c.GuildID, in.WeekKey, c.PlayerID, st.WeeklyMerit, in.NowMs); err != nil {
					return err
				}
			}
		}

		// 7. 追杀令结单或反杀计数 / 反杀 2 次撕单没收 50%（锁序 17: guild_war_bounty）。
		if in.ActiveBountyID != 0 && (in.BountyHunterWon || in.BountyTargetWon) {
			b, err := lockBountyTx(ctx, tx, in.ActiveBountyID)
			if err != nil {
				return err
			}
			if b != nil && b.Status == 2 && b.ExpiresAtMs > in.NowMs {
				if in.BountyHunterWon {
					b.Status = 3 // COMPLETED
					b.HunterPlayerId = in.BountyHunterID
					b.ResolvedAtMs = in.NowMs
					out.BountyTag = 2 // 追杀成功
					out.BountyPayoutSilver = b.EscrowSilver
					out.BountyRecipientID = in.BountyHunterID
					// 被追杀销单后给目标 30 分钟免再挂保护。
					if _, err := tx.ExecContext(ctx,
						"UPDATE `"+guildWarPlayerStateTable+"` SET `bounty_protect_until_ms` = ?, `updated_ms` = ? WHERE `player_id` = ?",
						in.NowMs+uint64(in.BountyProtectSec)*1000, in.NowMs, b.TargetPlayerId); err != nil {
						return err
					}
				} else if in.BountyTargetWon && in.BountyHunterID != 0 && in.BountyHunterID != b.LastCounterAttackerId {
					b.CounterKills++
					b.LastCounterAttackerId = in.BountyHunterID
					if b.CounterKills >= in.CounterKillTear {
						b.Status = 4 // TORN_BY_TARGET
						b.ResolvedAtMs = in.NowMs
						out.BountyTag = 4 // 反杀撕单并没收 50%
						out.BountyPayoutSilver = b.EscrowSilver / 2
						out.BountyRecipientID = b.TargetPlayerId
					} else {
						out.BountyTag = 3 // 反杀成功(1/2)
					}
				}
				if _, err := tx.ExecContext(ctx,
					"UPDATE `"+guildWarBountyTable+"` SET `status` = ?, `counter_kills` = ?, `last_counter_attacker_id` = ?, `hunter_player_id` = ?, `resolved_at_ms` = ? WHERE `bounty_id` = ?",
					b.Status, b.CounterKills, b.LastCounterAttackerId, b.HunterPlayerId, b.ResolvedAtMs, b.BountyId); err != nil {
					return err
				}
				if out.BountyTag != 0 {
					rec.BountyOutcomeTag = out.BountyTag
				}
				if out.BountyPayoutSilver > 0 {
					rec.JackpotSilver += out.BountyPayoutSilver
				}
			}
		}

		// 8. 写入终局幂等记录（锁序 18: guild_war_battle）。
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `"+guildWarBattleTable+"` (`battle_id`,`zone_id`,`war_battle_kind`,`leyline_id`,`bounty_id`,`attacker_guild_id`,`defender_guild_id`,`outcome`,`total_rounds`,`banner_hp_before`,`banner_hp_after`,`banner_shielded`,`bounty_outcome_tag`,`jackpot_contribution`,`jackpot_silver`,`diminish_percent`,`attacker_contribution`,`attacker_silver`,`attacker_funds`,`attacker_merit`,`defender_contribution`,`defender_silver`,`defender_funds`,`defender_merit`,`summary_payload`,`finished_at_ms`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			rec.BattleId, rec.ZoneId, rec.WarBattleKind, rec.LeylineId, rec.BountyId,
			rec.AttackerGuildId, rec.DefenderGuildId, rec.Outcome, rec.TotalRounds,
			rec.BannerHpBefore, rec.BannerHpAfter, rec.BannerShielded, rec.BountyOutcomeTag,
			rec.JackpotContribution, rec.JackpotSilver, rec.DiminishPercent,
			rec.AttackerContribution, rec.AttackerSilver, rec.AttackerFunds, rec.AttackerMerit,
			rec.DefenderContribution, rec.DefenderSilver, rec.DefenderFunds, rec.DefenderMerit,
			rec.SummaryPayload, rec.FinishedAtMs); err != nil {
			return err
		}

		// 9. 更新两帮本周宿敌交锋次数（锁序 19: guild_war_rivalry）。
		if rec.AttackerGuildId != 0 && rec.DefenderGuildId != 0 && rec.AttackerGuildId != rec.DefenderGuildId {
			if err := markRivalryTx(ctx, tx, rec.AttackerGuildId, rec.DefenderGuildId, in.WeekKey, 1, false, in.NowMs); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		if rec.AttackerGuildId != 0 {
			r.guilds.invalidateAfterCommit(ctx, "war_settle", rec.AttackerGuildId)
		}
		if rec.DefenderGuildId != 0 && rec.DefenderGuildId != rec.AttackerGuildId {
			r.guilds.invalidateAfterCommit(ctx, "war_settle", rec.DefenderGuildId)
		}
	}
	return out, err
}

// EnqueueBountyPayout 单独为追杀/撕单获赏方落一条 GUILD_CREDIT TX_GUILD_BOUNTY_PAYOUT 指令（受每日 5 次上限约束）。
func (r *WarRepo) EnqueueBountyPayout(ctx context.Context, playerID, guildID uint64, dayKey uint32, dailyLimit uint32, opID uint64, bundleBytes []byte, leaseUntilMs, leaseToken, nowMs uint64) (AssetOpRow, bool, error) {
	var row AssetOpRow
	var enqueued bool
	err := r.guilds.inTx(ctx, "war_bounty_payout", func(ctx context.Context, tx *sql.Tx) error {
		if ok, err := lockRowExists(ctx, tx, sqlLockMemberRole, guildID, playerID); err != nil || !ok {
			return err
		}
		stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
		if err := ensureSeqRowTx(ctx, tx, playerID, stream, nowMs); err != nil {
			return err
		}
		alloc, err := assetop.AllocateSeq(ctx, tx, r.seq, playerID, stream, assetop.DefaultLimits, nowMs)
		if err != nil {
			return err
		}
		if dailyLimit > 0 {
			limited, err := upsertCounterWithLimit(ctx, tx, playerID,
				pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT, 1, dayKey, 1, dailyLimit, nowMs)
			if err != nil {
				return err
			}
			if limited {
				return nil
			}
		}
		opRec := &pb.GuildAssetOpRecord{
			OpId:          opID,
			PlayerId:      playerID,
			Stream:        uint32(stream),
			Seq:           alloc.Seq,
			GuildId:       guildID,
			Kind:          pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT,
			Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
			NextAttemptMs: leaseUntilMs,
			Payload:       bundleBytes,
			RefId:         1,
			RefCount:      1,
			PeriodKey:     dayKey,
			CreatedMs:     nowMs,
			UpdatedMs:     nowMs,
			LeaseUntilMs:  leaseUntilMs,
			LeaseToken:    leaseToken,
			TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT),
			StreamEpoch:   alloc.Epoch,
		}
		if err := insertAssetOp(ctx, tx, opRec); err != nil {
			return err
		}
		row = assetOpRowFrom(opRec)
		enqueued = true
		return nil
	})
	return row, enqueued, err
}

// ── 内部事务辅助 ─────────────────────────────────────────────────────────────

func lockGuildGarrisonTx(ctx context.Context, tx *sql.Tx, guildID uint64) ([]*pb.GuildWarGarrisonRecord, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT `guild_id`,`slot_index`,`player_id`,`player_name`,`level`,`appearance_id`,`pet_name`,`pet_table_id`,`snapshot_payload`,`updated_ms` FROM `"+guildWarGarrisonTable+"` WHERE `guild_id` = ? ORDER BY `slot_index` ASC FOR UPDATE",
		guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.GuildWarGarrisonRecord
	for rows.Next() {
		rec := &pb.GuildWarGarrisonRecord{}
		if err := rows.Scan(&rec.GuildId, &rec.SlotIndex, &rec.PlayerId, &rec.PlayerName, &rec.Level,
			&rec.AppearanceId, &rec.PetName, &rec.PetTableId, &rec.SnapshotPayload, &rec.UpdatedMs); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func loadPlayerWarStatePlainTx(ctx context.Context, tx *sql.Tx, playerID uint64, zoneID, weekKey uint32) (*pb.GuildWarPlayerStateRecord, error) {
	rec := &pb.GuildWarPlayerStateRecord{PlayerId: playerID, ZoneId: zoneID, WeeklyWeekKey: weekKey}
	err := tx.QueryRowContext(ctx,
		"SELECT `zone_id`,`peace_mode`,`peace_ready_at_ms`,`open_challenge`,`protect_until_ms`,`streak_count`,`streak_distinct_count`,`streak_last_opponent_id`,`weekly_week_key`,`weekly_merit`,`milestone_claimed_mask`,`dividend_claimed_count`,`bounty_protect_until_ms`,`last_garrison_refresh_ms`,`last_track_ms`,`updated_ms` FROM `"+guildWarPlayerStateTable+"` WHERE `player_id` = ?",
		playerID).Scan(&rec.ZoneId, &rec.PeaceMode, &rec.PeaceReadyAtMs, &rec.OpenChallenge, &rec.ProtectUntilMs,
		&rec.StreakCount, &rec.StreakDistinctCount, &rec.StreakLastOpponentId, &rec.WeeklyWeekKey, &rec.WeeklyMerit,
		&rec.MilestoneClaimedMask, &rec.DividendClaimedCount, &rec.BountyProtectUntilMs, &rec.LastGarrisonRefreshMs,
		&rec.LastTrackMs, &rec.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, nil
	}
	if err != nil {
		return nil, err
	}
	if rec.WeeklyWeekKey != weekKey {
		rec.WeeklyWeekKey = weekKey
		rec.WeeklyMerit = 0
		rec.MilestoneClaimedMask = 0
		rec.DividendClaimedCount = 0
	}
	return rec, nil
}

func lockPlayerWarStateTx(ctx context.Context, tx *sql.Tx, playerID uint64, zoneID, weekKey uint32, nowMs uint64) (*pb.GuildWarPlayerStateRecord, error) {
	if _, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO `"+guildWarPlayerStateTable+"` (`player_id`,`zone_id`,`peace_mode`,`peace_ready_at_ms`,`open_challenge`,`protect_until_ms`,`streak_count`,`streak_distinct_count`,`streak_last_opponent_id`,`weekly_week_key`,`weekly_merit`,`milestone_claimed_mask`,`dividend_claimed_count`,`bounty_protect_until_ms`,`last_garrison_refresh_ms`,`last_track_ms`,`updated_ms`) VALUES (?,?,0,0,0,0,0,0,0,?,0,0,0,0,0,0,?)",
		playerID, zoneID, weekKey, nowMs); err != nil {
		return nil, err
	}
	rec := &pb.GuildWarPlayerStateRecord{PlayerId: playerID}
	if err := tx.QueryRowContext(ctx,
		"SELECT `zone_id`,`peace_mode`,`peace_ready_at_ms`,`open_challenge`,`protect_until_ms`,`streak_count`,`streak_distinct_count`,`streak_last_opponent_id`,`weekly_week_key`,`weekly_merit`,`milestone_claimed_mask`,`dividend_claimed_count`,`bounty_protect_until_ms`,`last_garrison_refresh_ms`,`last_track_ms`,`updated_ms` FROM `"+guildWarPlayerStateTable+"` WHERE `player_id` = ? FOR UPDATE",
		playerID).Scan(&rec.ZoneId, &rec.PeaceMode, &rec.PeaceReadyAtMs, &rec.OpenChallenge, &rec.ProtectUntilMs,
		&rec.StreakCount, &rec.StreakDistinctCount, &rec.StreakLastOpponentId, &rec.WeeklyWeekKey, &rec.WeeklyMerit,
		&rec.MilestoneClaimedMask, &rec.DividendClaimedCount, &rec.BountyProtectUntilMs, &rec.LastGarrisonRefreshMs,
		&rec.LastTrackMs, &rec.UpdatedMs); err != nil {
		return nil, err
	}
	if rec.WeeklyWeekKey != weekKey {
		rec.WeeklyWeekKey = weekKey
		rec.WeeklyMerit = 0
		rec.MilestoneClaimedMask = 0
		rec.DividendClaimedCount = 0
	}
	return rec, nil
}

func savePlayerWarStateTx(ctx context.Context, tx *sql.Tx, rec *pb.GuildWarPlayerStateRecord) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE `"+guildWarPlayerStateTable+"` SET `zone_id`=?,`peace_mode`=?,`peace_ready_at_ms`=?,`open_challenge`=?,`protect_until_ms`=?,`streak_count`=?,`streak_distinct_count`=?,`streak_last_opponent_id`=?,`weekly_week_key`=?,`weekly_merit`=?,`milestone_claimed_mask`=?,`dividend_claimed_count`=?,`bounty_protect_until_ms`=?,`last_garrison_refresh_ms`=?,`last_track_ms`=?,`updated_ms`=? WHERE `player_id`=?",
		rec.ZoneId, rec.PeaceMode, rec.PeaceReadyAtMs, rec.OpenChallenge, rec.ProtectUntilMs,
		rec.StreakCount, rec.StreakDistinctCount, rec.StreakLastOpponentId, rec.WeeklyWeekKey, rec.WeeklyMerit,
		rec.MilestoneClaimedMask, rec.DividendClaimedCount, rec.BountyProtectUntilMs, rec.LastGarrisonRefreshMs,
		rec.LastTrackMs, rec.UpdatedMs, rec.PlayerId)
	return err
}

func loadWeeklyProgressTx(ctx context.Context, tx *sql.Tx, guildID uint64, weekKey uint32, forUpdate bool) (*pb.GuildWarWeeklyProgressRecord, error) {
	if forUpdate {
		if _, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO `"+guildWarWeeklyProgressTable+"` (`guild_id`,`week_key`,`guild_merit`,`dividend_pool_count`,`mvp1_player_id`,`mvp1_merit`,`mvp2_player_id`,`mvp2_merit`,`first_win_player1_id`,`first_win_player2_id`,`first_win_at_ms`,`updated_ms`) VALUES (?,?,0,0,0,0,0,0,0,0,0,0)",
			guildID, weekKey); err != nil {
			return nil, err
		}
	}
	q := "SELECT `guild_merit`,`dividend_pool_count`,`mvp1_player_id`,`mvp1_merit`,`mvp2_player_id`,`mvp2_merit`,`first_win_player1_id`,`first_win_player2_id`,`first_win_at_ms`,`updated_ms` FROM `" + guildWarWeeklyProgressTable + "` WHERE `guild_id` = ? AND `week_key` = ?"
	if forUpdate {
		q += " FOR UPDATE"
	}
	rec := &pb.GuildWarWeeklyProgressRecord{GuildId: guildID, WeekKey: weekKey}
	err := tx.QueryRowContext(ctx, q, guildID, weekKey).Scan(&rec.GuildMerit, &rec.DividendPoolCount,
		&rec.Mvp1PlayerId, &rec.Mvp1Merit, &rec.Mvp2PlayerId, &rec.Mvp2Merit,
		&rec.FirstWinPlayer1Id, &rec.FirstWinPlayer2Id, &rec.FirstWinAtMs, &rec.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, nil
	}
	return rec, err
}

func saveWeeklyProgressTx(ctx context.Context, tx *sql.Tx, rec *pb.GuildWarWeeklyProgressRecord) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE `"+guildWarWeeklyProgressTable+"` SET `guild_merit`=?,`dividend_pool_count`=?,`mvp1_player_id`=?,`mvp1_merit`=?,`mvp2_player_id`=?,`mvp2_merit`=?,`first_win_player1_id`=?,`first_win_player2_id`=?,`first_win_at_ms`=?,`updated_ms`=? WHERE `guild_id`=? AND `week_key`=?",
		rec.GuildMerit, rec.DividendPoolCount, rec.Mvp1PlayerId, rec.Mvp1Merit, rec.Mvp2PlayerId, rec.Mvp2Merit,
		rec.FirstWinPlayer1Id, rec.FirstWinPlayer2Id, rec.FirstWinAtMs, rec.UpdatedMs, rec.GuildId, rec.WeekKey)
	return err
}

func updateWeeklyMvpTx(ctx context.Context, tx *sql.Tx, guildID uint64, weekKey uint32, playerID, merit, nowMs uint64) error {
	prog, err := loadWeeklyProgressTx(ctx, tx, guildID, weekKey, false)
	if err != nil {
		return err
	}
	if playerID == prog.Mvp1PlayerId {
		prog.Mvp1Merit = merit
	} else if playerID == prog.Mvp2PlayerId {
		prog.Mvp2Merit = merit
		if prog.Mvp2Merit > prog.Mvp1Merit {
			prog.Mvp1PlayerId, prog.Mvp2PlayerId = prog.Mvp2PlayerId, prog.Mvp1PlayerId
			prog.Mvp1Merit, prog.Mvp2Merit = prog.Mvp2Merit, prog.Mvp1Merit
		}
	} else if merit > prog.Mvp1Merit {
		prog.Mvp2PlayerId, prog.Mvp2Merit = prog.Mvp1PlayerId, prog.Mvp1Merit
		prog.Mvp1PlayerId, prog.Mvp1Merit = playerID, merit
	} else if merit > prog.Mvp2Merit {
		prog.Mvp2PlayerId, prog.Mvp2Merit = playerID, merit
	} else {
		return nil
	}
	prog.UpdatedMs = nowMs
	return saveWeeklyProgressTx(ctx, tx, prog)
}

func estimatePendingDividendSharesTx(ctx context.Context, tx *sql.Tx, zoneID uint32, guildID uint64, weekKey, intervalSec, capShares uint32, nowMs uint64) uint32 {
	if intervalSec == 0 || guildID == 0 {
		return 0
	}
	var lastDiv uint64
	var divWeek, weeklyCnt uint32
	err := tx.QueryRowContext(ctx,
		"SELECT `last_dividend_at_ms`,`dividend_week_key`,`weekly_dividend_count` FROM `"+guildWarLeylineTable+"` WHERE `zone_id` = ? AND `owner_guild_id` = ? LIMIT 1",
		zoneID, guildID).Scan(&lastDiv, &divWeek, &weeklyCnt)
	if err != nil {
		return 0
	}
	if divWeek != weekKey {
		weeklyCnt = 0
	}
	if weeklyCnt >= capShares || lastDiv == 0 || nowMs <= lastDiv {
		return 0
	}
	elapsed := uint32((nowMs - lastDiv) / (uint64(intervalSec) * 1000))
	if weeklyCnt+elapsed > capShares {
		elapsed = capShares - weeklyCnt
	}
	return elapsed
}

func accrueGuildDividendTx(ctx context.Context, tx *sql.Tx, zoneID uint32, guildID uint64, weekKey, intervalSec, capShares uint32, nowMs uint64) (uint32, error) {
	if intervalSec == 0 || guildID == 0 {
		return 0, nil
	}
	var leylineID uint32
	var lastDiv uint64
	var divWeek, weeklyCnt uint32
	err := tx.QueryRowContext(ctx,
		"SELECT `leyline_id`,`last_dividend_at_ms`,`dividend_week_key`,`weekly_dividend_count` FROM `"+guildWarLeylineTable+"` WHERE `zone_id` = ? AND `owner_guild_id` = ? LIMIT 1 FOR UPDATE",
		zoneID, guildID).Scan(&leylineID, &lastDiv, &divWeek, &weeklyCnt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if divWeek != weekKey {
		divWeek = weekKey
		weeklyCnt = 0
	}
	stepMs := uint64(intervalSec) * 1000
	if lastDiv == 0 {
		lastDiv = nowMs
	}
	if nowMs <= lastDiv || weeklyCnt >= capShares {
		return 0, nil
	}
	elapsed := uint32((nowMs - lastDiv) / stepMs)
	if elapsed == 0 {
		return 0, nil
	}
	if weeklyCnt+elapsed > capShares {
		elapsed = capShares - weeklyCnt
	}
	nextLast := lastDiv + uint64(elapsed)*stepMs
	weeklyCnt += elapsed
	if _, err := tx.ExecContext(ctx,
		"UPDATE `"+guildWarLeylineTable+"` SET `last_dividend_at_ms`=?,`dividend_week_key`=?,`weekly_dividend_count`=?,`updated_ms`=? WHERE `zone_id`=? AND `leyline_id`=?",
		nextLast, divWeek, weeklyCnt, nowMs, zoneID, leylineID); err != nil {
		return 0, err
	}
	return elapsed, nil
}

func lockBountyTx(ctx context.Context, tx *sql.Tx, bountyID uint64) (*pb.GuildWarBountyRecord, error) {
	rec := &pb.GuildWarBountyRecord{}
	err := tx.QueryRowContext(ctx,
		"SELECT `bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms` FROM `"+guildWarBountyTable+"` WHERE `bounty_id` = ? FOR UPDATE",
		bountyID).Scan(&rec.BountyId, &rec.ZoneId, &rec.IssuerPlayerId, &rec.IssuerGuildId, &rec.TargetPlayerId,
		&rec.TargetGuildId, &rec.Tier, &rec.EscrowSilver, &rec.PublicBoard, &rec.Status, &rec.EscrowOpId,
		&rec.CounterKills, &rec.LastCounterAttackerId, &rec.HunterPlayerId, &rec.CreatedAtMs, &rec.ExpiresAtMs, &rec.ResolvedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

func scanBounties(rows *sql.Rows) ([]*pb.GuildWarBountyRecord, error) {
	var out []*pb.GuildWarBountyRecord
	for rows.Next() {
		rec := &pb.GuildWarBountyRecord{}
		if err := rows.Scan(&rec.BountyId, &rec.ZoneId, &rec.IssuerPlayerId, &rec.IssuerGuildId, &rec.TargetPlayerId,
			&rec.TargetGuildId, &rec.Tier, &rec.EscrowSilver, &rec.PublicBoard, &rec.Status, &rec.EscrowOpId,
			&rec.CounterKills, &rec.LastCounterAttackerId, &rec.HunterPlayerId, &rec.CreatedAtMs, &rec.ExpiresAtMs, &rec.ResolvedAtMs); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func markRivalryTx(ctx context.Context, tx *sql.Tx, g1, g2 uint64, weekKey uint32, addClash uint32, bountyMark bool, nowMs uint64) error {
	low, high := g1, g2
	if low > high {
		low, high = high, low
	}
	bm := uint32(0)
	if bountyMark {
		bm = 1
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO `"+guildWarRivalryTable+"` (`guild_id_low`,`guild_id_high`,`week_key`,`clash_count`,`bounty_marked`,`updated_ms`) VALUES (?,?,?,?,?,?) ON DUPLICATE KEY UPDATE `clash_count` = `clash_count` + VALUES(`clash_count`), `bounty_marked` = GREATEST(`bounty_marked`, VALUES(`bounty_marked`)), `updated_ms` = VALUES(`updated_ms`)",
		low, high, weekKey, addClash, bm, nowMs)
	return err
}

// ── 帮战 V2.5 · 宿敌约战战书仓储（锁序 22: guild_war_feud，独立短事务，绝不回头锁旧表）──

func scanFeudRow(scan func(dest ...any) error) (*pb.GuildWarFeudRecord, error) {
	rec := &pb.GuildWarFeudRecord{}
	err := scan(&rec.FeudId, &rec.ZoneId, &rec.IssuerGuildId, &rec.IssuerGuildName,
		&rec.TargetGuildId, &rec.TargetGuildName, &rec.Status, &rec.IssuerWins, &rec.TargetWins,
		&rec.IssuerMerit, &rec.TargetMerit, &rec.CreatedAtMs, &rec.AcceptDeadlineMs,
		&rec.StartedAtMs, &rec.EndsAtMs, &rec.WinnerGuildId, &rec.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// GetActiveOrPendingFeud 返回本帮当前待应答或激战中的约战战书，并惰性收口已超时的战书。
func (r *WarRepo) GetActiveOrPendingFeud(ctx context.Context, guildID uint64, nowMs uint64) (*pb.GuildWarFeudRecord, *pb.GuildWarFeudRecord, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT `feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms` FROM `"+guildWarFeudTable+"` WHERE (`issuer_guild_id`=? OR `target_guild_id`=?) AND `status` IN (1,2) ORDER BY `created_at_ms` DESC LIMIT 1",
		guildID, guildID)
	rec, err := scanFeudRow(row.Scan)
	if err != nil || rec == nil {
		return nil, nil, err
	}
	if rec.GetStatus() == uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_PENDING) && rec.GetAcceptDeadlineMs() > 0 && nowMs >= rec.GetAcceptDeadlineMs() {
		_, _ = r.db.ExecContext(ctx,
			"UPDATE `"+guildWarFeudTable+"` SET `status`=?,`updated_ms`=? WHERE `feud_id`=? AND `status`=1",
			uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_EXPIRED), nowMs, rec.GetFeudId())
		return nil, nil, nil
	}
	if rec.GetStatus() == uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_ACTIVE) && rec.GetEndsAtMs() > 0 && nowMs >= rec.GetEndsAtMs() {
		winner := war.ResolveFeudWinner(rec.GetIssuerGuildId(), rec.GetTargetGuildId(), rec.GetIssuerWins(), rec.GetTargetWins(), rec.GetIssuerMerit(), rec.GetTargetMerit())
		res, uErr := r.db.ExecContext(ctx,
			"UPDATE `"+guildWarFeudTable+"` SET `status`=?,`winner_guild_id`=?,`updated_ms`=? WHERE `feud_id`=? AND `status`=2",
			uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_FINISHED), winner, nowMs, rec.GetFeudId())
		if uErr == nil {
			if aff, _ := res.RowsAffected(); aff > 0 {
				rec.Status = uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_FINISHED)
				rec.WinnerGuildId = winner
				rec.UpdatedMs = nowMs
				return nil, rec, nil
			}
		}
		return nil, nil, nil
	}
	return rec, nil, nil
}

func (r *WarRepo) GetLastFinishedFeud(ctx context.Context, guildID uint64) (*pb.GuildWarFeudRecord, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT `feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms` FROM `"+guildWarFeudTable+"` WHERE (`issuer_guild_id`=? OR `target_guild_id`=?) AND `status`=3 ORDER BY `ends_at_ms` DESC LIMIT 1",
		guildID, guildID)
	return scanFeudRow(row.Scan)
}

func (r *WarRepo) GetLastFeudIssuedAt(ctx context.Context, issuerGuildID uint64) (uint64, error) {
	var created uint64
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(`created_at_ms`), 0) FROM `"+guildWarFeudTable+"` WHERE `issuer_guild_id`=? AND `status` IN (1,2,3)",
		issuerGuildID).Scan(&created)
	return created, err
}

func (r *WarRepo) IssueFeud(ctx context.Context, rec *pb.GuildWarFeudRecord, cooldownSec uint32, nowMs uint64) (*pb.GuildWarFeudRecord, error) {
	if _, _, err := r.GetActiveOrPendingFeud(ctx, rec.GetIssuerGuildId(), nowMs); err != nil {
		return nil, err
	}
	if _, _, err := r.GetActiveOrPendingFeud(ctx, rec.GetTargetGuildId(), nowMs); err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var busy int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(1) FROM `"+guildWarFeudTable+"` WHERE `status` IN (1,2) AND (`issuer_guild_id` IN (?,?) OR `target_guild_id` IN (?,?))",
		rec.GetIssuerGuildId(), rec.GetTargetGuildId(), rec.GetIssuerGuildId(), rec.GetTargetGuildId()).Scan(&busy); err != nil {
		return nil, err
	}
	if busy > 0 {
		return nil, ErrWarFeudAlreadyActive
	}
	var lastIssued uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(`created_at_ms`), 0) FROM `"+guildWarFeudTable+"` WHERE `issuer_guild_id`=? AND `status` IN (1,2,3)",
		rec.GetIssuerGuildId()).Scan(&lastIssued); err != nil {
		return nil, err
	}
	if lastIssued > 0 && nowMs < lastIssued+uint64(cooldownSec)*1000 {
		return nil, ErrWarFeudCooldown
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `"+guildWarFeudTable+"` (`feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms`) VALUES (?,?,?,?,?,?,1,0,0,0,0,?,?,0,0,0,?)",
		rec.GetFeudId(), rec.GetZoneId(), rec.GetIssuerGuildId(), rec.GetIssuerGuildName(),
		rec.GetTargetGuildId(), rec.GetTargetGuildName(), rec.GetCreatedAtMs(), rec.GetAcceptDeadlineMs(), nowMs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rec.Status = uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_PENDING)
	rec.UpdatedMs = nowMs
	return rec, nil
}

func (r *WarRepo) RespondFeud(ctx context.Context, feudID, responderGuildID uint64, accept bool, durationSec uint32, nowMs uint64) (*pb.GuildWarFeudRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		"SELECT `feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms` FROM `"+guildWarFeudTable+"` WHERE `feud_id`=? FOR UPDATE",
		feudID)
	rec, err := scanFeudRow(row.Scan)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.GetStatus() != uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_PENDING) {
		return nil, ErrWarFeudNotFound
	}
	if rec.GetAcceptDeadlineMs() > 0 && nowMs >= rec.GetAcceptDeadlineMs() {
		_, _ = tx.ExecContext(ctx, "UPDATE `"+guildWarFeudTable+"` SET `status`=?,`updated_ms`=? WHERE `feud_id`=?",
			uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_EXPIRED), nowMs, feudID)
		_ = tx.Commit()
		return nil, ErrWarFeudNotFound
	}
	if accept {
		if responderGuildID != rec.GetTargetGuildId() {
			return nil, ErrWarFeudNotFound
		}
		endsAt := nowMs + uint64(durationSec)*1000
		if _, err := tx.ExecContext(ctx,
			"UPDATE `"+guildWarFeudTable+"` SET `status`=?,`started_at_ms`=?,`ends_at_ms`=?,`updated_ms`=? WHERE `feud_id`=?",
			uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_ACTIVE), nowMs, endsAt, nowMs, feudID); err != nil {
			return nil, err
		}
		rec.Status = uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_ACTIVE)
		rec.StartedAtMs = nowMs
		rec.EndsAtMs = endsAt
		rec.UpdatedMs = nowMs
	} else {
		if responderGuildID != rec.GetTargetGuildId() && responderGuildID != rec.GetIssuerGuildId() {
			return nil, ErrWarFeudNotFound
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE `"+guildWarFeudTable+"` SET `status`=?,`updated_ms`=? WHERE `feud_id`=?",
			uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_DECLINED), nowMs, feudID); err != nil {
			return nil, err
		}
		rec.Status = uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_DECLINED)
		rec.UpdatedMs = nowMs
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rec, nil
}

// GetActiveFeudBetween 查询两帮当前是否处于 2 小时宿敌约战激战窗口内。
func (r *WarRepo) GetActiveFeudBetween(ctx context.Context, g1, g2 uint64, nowMs uint64) (*pb.GuildWarFeudRecord, error) {
	if g1 == 0 || g2 == 0 || g1 == g2 {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx,
		"SELECT `feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms` FROM `"+guildWarFeudTable+"` WHERE `status`=2 AND ((`issuer_guild_id`=? AND `target_guild_id`=?) OR (`issuer_guild_id`=? AND `target_guild_id`=?)) LIMIT 1",
		g1, g2, g2, g1)
	rec, err := scanFeudRow(row.Scan)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.GetEndsAtMs() > 0 && nowMs >= rec.GetEndsAtMs() {
		return nil, nil
	}
	return rec, nil
}

// RecordFeudBattle 计入一场 2 小时约战比分与双方战功；若时间到则原子收官并返回 finished=true。
func (r *WarRepo) RecordFeudBattle(ctx context.Context, feudID, atkGuildID, defGuildID uint64, atkWon, defWon bool, atkMerit, defMerit, nowMs uint64) (*pb.GuildWarFeudRecord, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		"SELECT `feud_id`,`zone_id`,`issuer_guild_id`,`issuer_guild_name`,`target_guild_id`,`target_guild_name`,`status`,`issuer_wins`,`target_wins`,`issuer_merit`,`target_merit`,`created_at_ms`,`accept_deadline_ms`,`started_at_ms`,`ends_at_ms`,`winner_guild_id`,`updated_ms` FROM `"+guildWarFeudTable+"` WHERE `feud_id`=? FOR UPDATE",
		feudID)
	rec, err := scanFeudRow(row.Scan)
	if err != nil || rec == nil || rec.GetStatus() != uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_ACTIVE) {
		return nil, false, err
	}
	if atkGuildID == rec.GetIssuerGuildId() && defGuildID == rec.GetTargetGuildId() {
		if atkWon {
			rec.IssuerWins++
		} else if defWon {
			rec.TargetWins++
		}
		rec.IssuerMerit += atkMerit
		rec.TargetMerit += defMerit
	} else if atkGuildID == rec.GetTargetGuildId() && defGuildID == rec.GetIssuerGuildId() {
		if atkWon {
			rec.TargetWins++
		} else if defWon {
			rec.IssuerWins++
		}
		rec.TargetMerit += atkMerit
		rec.IssuerMerit += defMerit
	}
	finished := rec.GetEndsAtMs() > 0 && nowMs >= rec.GetEndsAtMs()
	if finished {
		rec.Status = uint32(pb.GuildWarFeudStatus_GUILD_WAR_FEUD_STATUS_FINISHED)
		rec.WinnerGuildId = war.ResolveFeudWinner(rec.GetIssuerGuildId(), rec.GetTargetGuildId(), rec.GetIssuerWins(), rec.GetTargetWins(), rec.GetIssuerMerit(), rec.GetTargetMerit())
	}
	rec.UpdatedMs = nowMs
	if _, err := tx.ExecContext(ctx,
		"UPDATE `"+guildWarFeudTable+"` SET `status`=?,`issuer_wins`=?,`target_wins`=?,`issuer_merit`=?,`target_merit`=?,`winner_guild_id`=?,`updated_ms`=? WHERE `feud_id`=?",
		rec.GetStatus(), rec.GetIssuerWins(), rec.GetTargetWins(), rec.GetIssuerMerit(), rec.GetTargetMerit(), rec.GetWinnerGuildId(), nowMs, feudID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return rec, finished, nil
}
