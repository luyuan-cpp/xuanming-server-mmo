package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// bountyPayoutCounterRefID:每日领赏计数行(WAR_BOUNTY_PAYOUT)只有一行,ref_id 固定为 1。
const bountyPayoutCounterRefID uint32 = 1

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
			// 公费档实扣的帮会资金记在单上:到期与撕单按它退,不按退款那一刻的配表(配表热更不会让退款多退或少退)。
			EscrowGuildFunds: in.CostGuildFunds,
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `"+guildWarBountyTable+"` (`bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms`,`escrow_guild_funds`) VALUES (?,?,?,?,?,?,?,?,?,?,?,0,0,0,?,?,0,?)",
			rec.BountyId, rec.ZoneId, rec.IssuerPlayerId, rec.IssuerGuildId, rec.TargetPlayerId, rec.TargetGuildId,
			rec.Tier, rec.EscrowSilver, rec.PublicBoard, rec.Status, rec.EscrowOpId, rec.CreatedAtMs, rec.ExpiresAtMs,
			rec.EscrowGuildFunds); err != nil {
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

// guildWarBountyColumns 是 guild_war_bounty 全部列的读取顺序,与 scanBountyRow 一一对应(全部读路径共用,加列只改这两处)。
const guildWarBountyColumns = "`bounty_id`,`zone_id`,`issuer_player_id`,`issuer_guild_id`,`target_player_id`,`target_guild_id`,`tier`,`escrow_silver`,`public_board`,`status`,`escrow_op_id`,`counter_kills`,`last_counter_attacker_id`,`hunter_player_id`,`created_at_ms`,`expires_at_ms`,`resolved_at_ms`,`escrow_guild_funds`"

// scanBountyRow 按 guildWarBountyColumns 的列序扫一行;没有行返回 (nil, nil)。
func scanBountyRow(scan func(dest ...any) error) (*pb.GuildWarBountyRecord, error) {
	rec := &pb.GuildWarBountyRecord{}
	err := scan(&rec.BountyId, &rec.ZoneId, &rec.IssuerPlayerId, &rec.IssuerGuildId, &rec.TargetPlayerId,
		&rec.TargetGuildId, &rec.Tier, &rec.EscrowSilver, &rec.PublicBoard, &rec.Status, &rec.EscrowOpId,
		&rec.CounterKills, &rec.LastCounterAttackerId, &rec.HunterPlayerId, &rec.CreatedAtMs, &rec.ExpiresAtMs, &rec.ResolvedAtMs,
		&rec.EscrowGuildFunds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (r *WarRepo) ListActiveBounties(ctx context.Context, zoneID uint32, myGuildID uint64, nowMs uint64) ([]*pb.GuildWarBountyRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+guildWarBountyColumns+" FROM `"+guildWarBountyTable+"` WHERE `zone_id` = ? AND `status` IN (1, 2) AND `expires_at_ms` > ? AND (`public_board` = 1 OR `issuer_guild_id` = ?) ORDER BY `created_at_ms` DESC LIMIT 50",
		zoneID, nowMs, myGuildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBounties(rows)
}

func (r *WarRepo) GetBounty(ctx context.Context, bountyID uint64) (*pb.GuildWarBountyRecord, error) {
	return scanBountyRow(r.db.QueryRowContext(ctx,
		"SELECT "+guildWarBountyColumns+" FROM `"+guildWarBountyTable+"` WHERE `bounty_id` = ?", bountyID).Scan)
}

func (r *WarRepo) ActiveBountyByTarget(ctx context.Context, targetPlayerID uint64, nowMs uint64) (*pb.GuildWarBountyRecord, error) {
	return scanBountyRow(r.db.QueryRowContext(ctx,
		"SELECT "+guildWarBountyColumns+" FROM `"+guildWarBountyTable+"` WHERE `target_player_id` = ? AND `status` = 2 AND `expires_at_ms` > ? LIMIT 1",
		targetPlayerID, nowMs).Scan)
}

// BountyExpiryCursor 是到期收尾翻页的位置:按 (expires_at_ms, bounty_id) 升序,取上一页最后一行的这两列;零值 = 从头开始。
type BountyExpiryCursor struct {
	ExpiresAtMs uint64
	BountyID    uint64
}

// ListDueBounties 普通读一页「ACTIVE 且 expires_at_ms <= dueBeforeMs」的追杀令,按 (expires_at_ms, bounty_id) 升序、
// 从 after 之后开始,至多 limit 行(走索引 (status, expires_at_ms))。只给到期收尾当候选:结论由 ExpireBounty 在行锁内复核。
// 后台按游标翻页而不是每轮只取最老的一页:退款一时进不了队(发布者未决指令已满)的行会留在原处,不能挡住排在后面的。
func (r *WarRepo) ListDueBounties(ctx context.Context, dueBeforeMs uint64, after BountyExpiryCursor, limit int) ([]*pb.GuildWarBountyRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+guildWarBountyColumns+" FROM `"+guildWarBountyTable+"` WHERE `status` = ? AND `expires_at_ms` <= ? AND `expires_at_ms` >= ? AND (`expires_at_ms` > ? OR `bounty_id` > ?) ORDER BY `expires_at_ms` ASC, `bounty_id` ASC LIMIT ?",
		uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE), dueBeforeMs, after.ExpiresAtMs, after.ExpiresAtMs, after.BountyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBounties(rows)
}

// 终态追杀令的保留期清理(r2-gw#7)。终态 = 3 COMPLETED / 4 TORN_BY_TARGET / 5 EXPIRED / 6 ESCROW_FAILED,它们不会再变:
// 扣款终结只改 status = 1 的行,结算第 7 步与到期收尾在行锁内只认 ACTIVE。1 ESCROW_PENDING 与 2 ACTIVE 永远不删。
// 候选走索引 (status, expires_at_ms) 普通读;删按清理短事务的写法(asset_store.go execCleanupDelete 同一纪律):
// 一行一个事务,主键点锁 → 锁内复核仍是终态且过了保留期 → 完整主键点删(TiDB 走点写快路径,不与同一行的悲观写者各持一半)。
const sqlSelectPurgeableBounties = "SELECT `bounty_id` FROM `" + guildWarBountyTable + "` WHERE `status` IN (?,?,?,?) AND `expires_at_ms` < ? LIMIT ?"

// terminalBountyStatuses 是可被保留期清理删除的四个终态,顺序与 sqlSelectPurgeableBounties 的 IN (?,?,?,?) 一一对应。
var terminalBountyStatuses = []uint32{
	uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED),
	uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_TORN_BY_TARGET),
	uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED),
	uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ESCROW_FAILED),
}

// PurgeTerminalBounties 删至多 limit 张「终态且 expires_at_ms < expiredBeforeMs」的追杀令,返回删掉的行数(r2-gw#7)。
// 按到期时刻判龄:终态时刻不晚于到期后约 16 分钟(到期收尾的宽限 + 一轮),保留期远长于这点差。
// 删掉的行谁都不再需要:扣款终结只找 status = 1 的行;结算按 bounty_id 读到终态行与读不到行是同一结论(都不动追杀令,ResolveBounty);
// 战报的追杀结果存在对局行自己的列里;bounty_id 是发号器永久 ID,不会被新单复用。单行失败就停在这一行,剩下的下一轮再删。
func (r *WarRepo) PurgeTerminalBounties(ctx context.Context, expiredBeforeMs uint64, limit int) (int, error) {
	if limit <= 0 || expiredBeforeMs == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(terminalBountyStatuses)+2)
	for _, status := range terminalBountyStatuses {
		args = append(args, status)
	}
	rows, err := r.db.QueryContext(ctx, sqlSelectPurgeableBounties, append(args, expiredBeforeMs, limit)...)
	if err != nil {
		return 0, fmt.Errorf("list purgeable bounties: %w", err)
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan purgeable bounty: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("iterate purgeable bounties: %w", err)
	}
	deleted := 0
	for _, id := range ids {
		var removed bool
		err := r.guilds.inTx(ctx, "war_purge_bounty", func(ctx context.Context, tx *sql.Tx) error {
			removed = false
			b, err := loadBountyTx(ctx, tx, id, true)
			if err != nil {
				return err
			}
			if b == nil || !slices.Contains(terminalBountyStatuses, b.GetStatus()) || b.GetExpiresAtMs() >= expiredBeforeMs {
				return nil // 已被别的副本删掉;不再符合条件的(不该出现)不删
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM `"+guildWarBountyTable+"` WHERE `bounty_id` = ?", id); err != nil {
				return err
			}
			removed = true
			return nil
		})
		if err != nil {
			return deleted, fmt.Errorf("purge terminal bounty %d: %w", id, err)
		}
		if removed {
			deleted++
		}
	}
	return deleted, nil
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
	Record             *pb.GuildWarBattleRecord
	WeekKey            uint32
	DayKey             uint32
	NowMs              uint64
	ProtectSec         uint32
	PeaceCooldownSec   uint32
	BountyProtectSec   uint32
	DailyPayoutLimit   uint32
	AttackerGuildFunds uint64
	DefenderGuildFunds uint64
	AttackerMeritAdd   uint64
	DefenderMeritAdd   uint64
	// BannerDelta 是事务外按快照算好的灵脉变化,自带算它时的快照;第 4 步在行锁内逐项比对,不符回 ErrWarSettleStale(r2-gw#3)。
	BannerDelta   *war.BannerDelta
	PlayerCredits []PlayerSettlementCredit
	// 追杀令(第 7 步,r2-gw#1 / #2)。BountyID 为 0 = 本局没有挂钩追杀令。BountyFacts 只取结算消息与开战时服务端冻结的上下文。
	// 两枚指令号在事务前发好(领赏 / 撕单退发布者),本局用不上就丢弃,号不要求连续。
	BountyID          uint64
	BountyFacts       war.BountyFacts
	BountyRewardOpID  uint64
	BountyRefundOpID  uint64
	BuildSilverBundle func(silver uint64) ([]byte, error)
	LeaseUntilMs      uint64
	LeaseToken        uint64
}

type SettleWarBattleOutput struct {
	CreatedOps       []AssetOpRow
	BountyTag        uint32
	FirstWinRecorded bool
	// BountyHeldRecipientID 非 0:追杀令本局本该结单,但这名领赏人收不了,整笔不转移、追杀令原样保留(调用方打日志)。
	// 原因见 BountyHeldReason。
	BountyHeldRecipientID uint64
	BountyHeldReason      BountyHeldReason
	// BountyIssuerGuildGone:公费档撕单要退给发单帮的那一半资金,因为帮会已解散而没退(调用方打日志)。
	BountyIssuerGuildGone bool
	// OwedCredits:没能当场插指令、在同一事务里记进待入账表的入账(本流未决已满,或护栏冻结中),由待入队物品循环补插(r2-gw#5 / #6)。
	OwedCredits []WarCreditOwed
	// FencedPlayers / RemovedPlayers:护栏副本冻结中(资产入账改记待入账)/ 已删除(这一局给他的什么都不写)的目标,升序(调用方打日志)。
	FencedPlayers, RemovedPlayers []uint64
}

// BountyHeldReason 是追杀令本局整笔不转移的原因(日志用)。
type BountyHeldReason uint8

const (
	// BountyHeldNone:没有扣下。
	BountyHeldNone BountyHeldReason = iota
	// BountyHeldDailyLimit:领赏人当日领赏已满。
	BountyHeldDailyLimit
	// BountyHeldRecipientRemoved:领赏人角色已删除。赏金转给谁都不对,托管留着,没人结单就到期原路退回发布者(r2-gw#6)。
	BountyHeldRecipientRemoved
)

// warSettleOp 是结算事务第 3 步要插的一条 GUILD_CREDIT 指令:战后奖励与追杀令转账并成一个按 player_id 升序的序列。
type warSettleOp struct {
	playerID  uint64
	guildID   uint64
	opID      uint64
	payload   []byte
	kind      pb.GuildAssetOpKind
	txType    rollbackpb.TransactionType
	refID     uint32
	periodKey uint32
}

// bountyTransferOps 把追杀令去向里的银两转账变成第 3 步的指令:领赏(占当日领赏计数,period_key = 游戏日)与
// 撕单退发布者本人的另一半(不占计数)。都记 ref_id = 档位,与发单时的预扣指令同口径。
func bountyTransferOps(plan war.BountyResolution, b *pb.GuildWarBountyRecord, in SettleWarBattleInput) ([]warSettleOp, error) {
	var ops []warSettleOp
	add := func(playerID, guildID, opID, silver uint64, periodKey uint32) error {
		if silver == 0 {
			return nil
		}
		if playerID == 0 || opID == 0 || in.BuildSilverBundle == nil {
			return fmt.Errorf("war settle: bounty %d transfer of %d silver to player %d lacks a pre-minted op id or bundle builder", in.BountyID, silver, playerID)
		}
		payload, err := in.BuildSilverBundle(silver)
		if err != nil {
			return fmt.Errorf("war settle: build bounty %d bundle: %w", in.BountyID, err)
		}
		ops = append(ops, warSettleOp{
			playerID: playerID, guildID: guildID, opID: opID, payload: payload,
			kind:   pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT,
			txType: rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT,
			refID:  b.GetTier(), periodKey: periodKey,
		})
		return nil
	}
	// 领赏人不是猎手(攻方)就是目标(守方),指令记本局对应一方的帮(与同局战后奖励同口径,只作审计)。
	rewardGuild := in.Record.GetAttackerGuildId()
	if plan.RewardRecipientID != in.BountyFacts.HunterID {
		rewardGuild = in.Record.GetDefenderGuildId()
	}
	if err := add(plan.RewardRecipientID, rewardGuild, in.BountyRewardOpID, plan.RewardSilver, in.DayKey); err != nil {
		return nil, err
	}
	if err := add(b.GetIssuerPlayerId(), b.GetIssuerGuildId(), in.BountyRefundOpID, plan.IssuerRefundSilver, 0); err != nil {
		return nil, err
	}
	return ops, nil
}

// SettleWarBattle 在一个事务里落一局帮战的全部结算(锁序 护栏副本 → G → M → Q → O → C → 灵脉 → 周进度 → 个人态 → 追杀令 → 对局
// → 宿敌 → 待入账)。
//
// 两类基准来自锁序靠后、却要在靠前步骤里用到的数据,一律「先普通读定计划、到它的锁序位置再锁住复核」,不符回 ErrWarSettleStale
// (整笔回滚,调用方重读重算):
//   - 灵脉(r2-gw#3):BannerDelta 是调用方按事务外快照算的;第 4 步点锁本区全部灵脉,快照不符即回冲突;
//   - 追杀令(r2-gw#1 / #2):第 0 步普通读追杀令与领赏人当日计数定出去向,第 1 / 2 / 3 步据此加退款资金、锁成员行、插转账指令,
//     第 3b 步占领赏计数(被并发占满即冲突),第 7 步点锁追杀令重算一遍,与计划不同即冲突。转账与状态变更因此同事务提交,
//     不会出现「追杀令已结、赏金没发」或「赏金发了、追杀令没结」。
//
// 入账按人分流(r2-gw#5 / #6,war_credit_owed.go):第 F 步锁全部入账目标的护栏副本;某人收不了(护栏冻结中、本流未决已满)
// 只把他那一笔记进待入账表,已删除的人这一局什么都不写 —— 一个人的状态挡不住同局其他人的结算。
func (r *WarRepo) SettleWarBattle(ctx context.Context, in SettleWarBattleInput) (SettleWarBattleOutput, error) {
	var out SettleWarBattleOutput
	rec := in.Record
	// 第 7 步会改 rec 的两列;inTx 遇 1213 会整段重跑闭包,每次都从这份原值开始,不会重复累加。
	baseOutcomeTag, baseJackpotSilver := rec.BountyOutcomeTag, rec.JackpotSilver
	var refundedIssuerGuild uint64
	// fenceHealed:第 F 步已经让 Guard.RunTx 自愈过一次护栏副本(跨闭包的重跑保留),之后还冻结的才按真冻结分流。
	var fenceHealed bool
	err := r.guilds.inTx(ctx, "war_settle_battle", func(ctx context.Context, tx *sql.Tx) error {
		out = SettleWarBattleOutput{}
		refundedIssuerGuild = 0
		rec.BountyOutcomeTag, rec.JackpotSilver = baseOutcomeTag, baseJackpotSilver
		// 幂等预查（普通读）。
		var existingID uint64
		if err := tx.QueryRowContext(ctx, "SELECT `battle_id` FROM `"+guildWarBattleTable+"` WHERE `battle_id` = ?", rec.BattleId).Scan(&existingID); err == nil {
			return ErrWarBattleAlreadyDone
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// 0. 追杀令去向计划(普通读,不加锁;第 7 步在锁内复核)。领赏人当日已领满:整笔不转移,追杀令原样保留 ——
		//    满上限还结单,要么把托管退回发布者(发布者白拿回本金、猎手白打),要么没收(销毁玩家的钱),两样都不对。
		var plan war.BountyResolution
		var bounty *pb.GuildWarBountyRecord
		if in.BountyID != 0 {
			b, err := loadBountyTx(ctx, tx, in.BountyID, false)
			if err != nil {
				return err
			}
			bounty = b
			plan = war.ResolveBounty(b, in.BountyFacts)
			if plan.RewardSilver > 0 && in.DailyPayoutLimit > 0 {
				used, err := readBountyPayoutCountTx(ctx, tx, plan.RewardRecipientID, in.DayKey)
				if err != nil {
					return err
				}
				if used >= in.DailyPayoutLimit {
					out.BountyHeldRecipientID, out.BountyHeldReason = plan.RewardRecipientID, BountyHeldDailyLimit
					plan = war.BountyResolution{}
				}
			}
		}

		// F. 护栏副本(r2-gw#6;事务里第一把锁,先于 guild 行):本局每个可能被写到的玩家 —— 参战者、追杀令领赏人、撕单退款的发布者 ——
		//    按 player_id 升序锁副本逐个判。冻结中:资产入账改记待入账行(帮贡、个人态照写,它们不是挂起项);已删除:他的行一律不写,
		//    领赏人已删除则追杀令整笔不转移(托管不销毁,到期原路退回)。
		targets := make([]uint64, 0, len(in.PlayerCredits)+2)
		for _, c := range in.PlayerCredits {
			targets = append(targets, c.PlayerID)
		}
		if plan.Touch {
			targets = append(targets, plan.RewardRecipientID)
			if plan.IssuerRefundSilver > 0 {
				targets = append(targets, bounty.GetIssuerPlayerId())
			}
		}
		verdicts, err := r.guilds.lockCreditFences(ctx, tx, targets, &fenceHealed)
		if err != nil {
			return err
		}
		out.FencedPlayers = playersWithVerdict(verdicts, creditFenceFrozen)
		out.RemovedPlayers = playersWithVerdict(verdicts, creditFenceRemoved)
		if plan.RewardSilver > 0 && verdicts[plan.RewardRecipientID] == creditFenceRemoved {
			out.BountyHeldRecipientID, out.BountyHeldReason = plan.RewardRecipientID, BountyHeldRecipientRemoved
			plan = war.BountyResolution{}
		}

		// 1. 按 guild_id 升序锁涉及帮会（锁序位置 1: guild）并加帮会资金(含公费档撕单退给发单帮的那一半)。
		guildFunds := map[uint64]uint64{}
		if rec.AttackerGuildId != 0 && in.AttackerGuildFunds > 0 {
			guildFunds[rec.AttackerGuildId] += in.AttackerGuildFunds
		}
		if rec.DefenderGuildId != 0 && in.DefenderGuildFunds > 0 {
			guildFunds[rec.DefenderGuildId] += in.DefenderGuildFunds
		}
		if plan.IssuerRefundGuildFunds > 0 {
			guildFunds[bounty.GetIssuerGuildId()] += plan.IssuerRefundGuildFunds
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
				if plan.IssuerRefundGuildFunds > 0 && gid == bounty.GetIssuerGuildId() {
					refundedIssuerGuild = gid
				}
			} else if plan.IssuerRefundGuildFunds > 0 && gid == bounty.GetIssuerGuildId() {
				out.BountyIssuerGuildGone = true // 帮会已解散:这一半不退(调用方打日志),目标那一半照发
			}
		}

		// 2. 按 (guild_id, player_id) 升序锁成员并加帮贡（锁序位置 3: guild_member）。撕单退银两给发布者时一并锁他的成员行
		//    (只作 seq 建行守卫,不加帮贡;领赏与退款都不要求此刻仍在帮内,与战后奖励一致)。
		credits := append([]PlayerSettlementCredit(nil), in.PlayerCredits...)
		memberLocks := append([]PlayerSettlementCredit(nil), credits...)
		if plan.IssuerRefundSilver > 0 {
			memberLocks = append(memberLocks, PlayerSettlementCredit{PlayerID: bounty.GetIssuerPlayerId(), GuildID: bounty.GetIssuerGuildId()})
		}
		sort.Slice(memberLocks, func(i, j int) bool {
			if memberLocks[i].GuildID != memberLocks[j].GuildID {
				return memberLocks[i].GuildID < memberLocks[j].GuildID
			}
			return memberLocks[i].PlayerID < memberLocks[j].PlayerID
		})
		for _, c := range memberLocks {
			if c.GuildID == 0 || c.PlayerID == 0 || verdicts[c.PlayerID] == creditFenceRemoved {
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

		// 3. 按 player_id 升序建 seq 并插 GUILD_CREDIT 资产指令（锁序位置 5 -> 6）:战后奖励与追杀令转账并成一个序列,全程单调。
		sort.Slice(credits, func(i, j int) bool { return credits[i].PlayerID < credits[j].PlayerID })
		ops := make([]warSettleOp, 0, len(credits)+2)
		for _, c := range credits {
			if c.PlayerID == 0 || c.OpID == 0 || len(c.CreditBundle) == 0 {
				continue
			}
			kind := c.OpKind
			if kind == 0 {
				kind = pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_WAR_REWARD
			}
			txType := c.TxType
			if txType == 0 {
				txType = rollbackpb.TransactionType_TX_GUILD_WAR_REWARD
			}
			ops = append(ops, warSettleOp{playerID: c.PlayerID, guildID: c.GuildID, opID: c.OpID, payload: c.CreditBundle,
				kind: kind, txType: txType, refID: rec.LeylineId, periodKey: in.WeekKey})
		}
		if plan.Touch {
			transfers, err := bountyTransferOps(plan, bounty, in)
			if err != nil {
				return err
			}
			ops = append(ops, transfers...)
		}
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].playerID < ops[j].playerID })
		// 收不了的人(护栏冻结中、本流未决已满)只把他那一笔记进待入账行,在全序末尾(第 10 步)插入;已删除的人这一笔放弃(r2-gw#5 / #6)。
		var owed []WarCreditOwed
		lease := warOpLease{nextAttemptMs: in.LeaseUntilMs, untilMs: in.LeaseUntilMs, token: in.LeaseToken}
		for _, o := range ops {
			verdict := verdicts[o.playerID]
			if verdict == creditFenceRemoved {
				continue
			}
			credit := WarCreditOwed{PlayerID: o.playerID, OpID: o.opID, GuildID: o.guildID, Kind: o.kind, TxType: o.txType,
				RefID: o.refID, PeriodKey: o.periodKey, Payload: o.payload, SourceID: rec.BattleId}
			created, owe, err := r.creditOrOweTx(ctx, tx, credit, verdict == creditFenceFrozen, lease, in.NowMs)
			if err != nil {
				return err
			}
			if owe != nil {
				owed = append(owed, *owe)
				continue
			}
			out.CreatedOps = append(out.CreatedOps, *created)
		}

		// 3b. 占领赏人当日领赏次数(锁序 C;上一步已经锁住他的 GUILD_CREDIT seq 行 —— 领赏记进待入账行时也照锁 ——
		//     满足计数写者的守卫前提)。领赏记进待入账行也占次数:钱只是晚到,不是没领。
		//     第 0 步普通读时还没满、此刻被并发领赏占满:整笔重读重算,下一次会走「整笔不转移」。
		if plan.RewardSilver > 0 && in.DailyPayoutLimit > 0 {
			limited, err := upsertCounterWithLimit(ctx, tx, plan.RewardRecipientID,
				pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT, bountyPayoutCounterRefID, in.DayKey, 1, in.DailyPayoutLimit, in.NowMs)
			if err != nil {
				return err
			}
			if limited {
				return ErrWarSettleStale
			}
		}

		// 4. 更新灵脉归属、旗势与护盾（锁序 13: guild_war_leyline）。先按 leyline_id 升序点锁本区全部灵脉,与算 BannerDelta 用的
		//    事务外快照逐项比对(目标灵脉的主人 / 旗势 / 护盾,攻方是否已占别的灵脉):并发的两场结算由此串行,后提交的那场
		//    读到的快照已过时 → 回冲突重算,不会覆盖先提交的扣减、不会重复发破旗赏金,也不会让一帮同时占两座(r2-gw#3)。
		if rec.LeylineId > 0 && in.BannerDelta != nil {
			rows, err := lockZoneLeylinesTx(ctx, tx, rec.ZoneId)
			if err != nil {
				return err
			}
			if cur, ownsOther := war.FindLeylineBasis(rows, rec.LeylineId, rec.AttackerGuildId); !in.BannerDelta.BasisHolds(cur, ownsOther) {
				return ErrWarSettleStale
			}
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

		// 6. 更新个人帮战状态（战后 10s 保护、脱战 60s 冷却、连胜、周战功、MVP 榜更新；锁序 16）。已删除的人不建、不写(墓碑之后
		//    本库不许再有他的业务写入,删角收尾才不用两阶段)。
		for _, c := range credits {
			if verdicts[c.PlayerID] == creditFenceRemoved {
				continue
			}
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
			// 被追杀销单后给目标 30 分钟免再挂保护。目标一定在本局守方名单里(ResolveBounty 要求守方首人就是目标),行已在本循环锁住。
			if plan.Status == uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_COMPLETED) && c.PlayerID == bounty.GetTargetPlayerId() {
				st.BountyProtectUntilMs = in.NowMs + uint64(in.BountyProtectSec)*1000
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

		// 7. 追杀令结单 / 反杀计数 / 撕单（锁序 17: guild_war_bounty）。点锁之后按锁内的行再算一次去向,与第 0 步的计划
		//    逐字段相同才写:中间被别的结算、到期收尾改过(状态、反杀数、过期)就回冲突重算,第 1~3b 步按旧计划做的转账一并回滚。
		//    写回的是锁内算出的整行新值,本局不涉及追杀令(计划 Touch=false)就根本不碰这张表。
		if plan.Touch {
			locked, err := loadBountyTx(ctx, tx, in.BountyID, true)
			if err != nil {
				return err
			}
			if war.ResolveBounty(locked, in.BountyFacts) != plan {
				return ErrWarSettleStale
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE `"+guildWarBountyTable+"` SET `status` = ?, `counter_kills` = ?, `last_counter_attacker_id` = ?, `hunter_player_id` = ?, `resolved_at_ms` = ? WHERE `bounty_id` = ?",
				plan.Status, plan.CounterKills, plan.LastCounterAttackerID, plan.HunterPlayerID, plan.ResolvedAtMs, in.BountyID); err != nil {
				return err
			}
			out.BountyTag = plan.OutcomeTag
			rec.BountyOutcomeTag = plan.OutcomeTag
			rec.JackpotSilver += plan.RewardSilver
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

		// 10. 待入账行(锁序位置 24,全序末尾;r2-gw#5 / #6):第 3 步没能当场插指令的那几笔,与本局其余结算同一事务提交。
		//     第 8 步的对局幂等闸门已保证这是本局首次落定;转换时原样用这里的 op_id,同一笔不会入账两次。
		if err := insertWarCreditOwedTx(ctx, tx, owed); err != nil {
			return err
		}
		out.OwedCredits = owed
		return nil
	})
	if err == nil {
		if rec.AttackerGuildId != 0 {
			r.guilds.invalidateAfterCommit(ctx, "war_settle", rec.AttackerGuildId)
		}
		if rec.DefenderGuildId != 0 && rec.DefenderGuildId != rec.AttackerGuildId {
			r.guilds.invalidateAfterCommit(ctx, "war_settle", rec.DefenderGuildId)
		}
		if refundedIssuerGuild != 0 && refundedIssuerGuild != rec.AttackerGuildId && refundedIssuerGuild != rec.DefenderGuildId {
			r.guilds.invalidateAfterCommit(ctx, "war_settle", refundedIssuerGuild) // 公费档撕单把一半资金退给了发单帮
		}
	}
	return out, err
}

// ExpireBountyInput 是一张到期追杀令的收尾输入。Bounty 是候选普通读到的行,退款额按它算,锁内复核不变。
type ExpireBountyInput struct {
	Bounty       *pb.GuildWarBountyRecord
	RefundOpID   uint64 // 银两档退款指令号(事务前发好);公费档不需要
	RefundBundle []byte
	NowMs        uint64
}

// ExpireBountyResult:Expired=false 表示锁内复核时它已不是到期的 ACTIVE(被结单 / 撕单 / 别的副本已收尾),整笔回滚、什么都没写。
type ExpireBountyResult struct {
	Expired         bool
	Op              AssetOpRow // 银两档退给发布者的 GUILD_CREDIT 指令
	HasOp           bool
	IssuerGuildGone bool // 公费档:发单帮已解散,资金不退(调用方打日志)
	// RefundOwed:银两档退款没能当场插指令(发布者本流未决已满,或护栏冻结中),已在同一事务记进待入账行,由待入队物品循环补插。
	RefundOwed bool
	// RefundAbandoned:发布者角色已删除,银两退款放弃(追杀令照样置 EXPIRED,调用方打日志)。
	RefundAbandoned bool
}

// ExpireBounty 收尾一张到期的 ACTIVE 追杀令(r2-gw#2):同一事务置 EXPIRED 并原路退回托管 —— 银两档给发布者插 GUILD_CREDIT
// (TX_GUILD_BOUNTY_PAYOUT,不持租约、立刻可被重投循环领走),公费档把实扣的帮会资金加回发单帮。
// 发布者收不了(本流未决已满、护栏冻结中)时退款记进待入账行,追杀令照样到期;角色已删除时银两退款放弃(r2-gw#5 / #6)。
//
// 锁序:发布者护栏副本(银两档)→ G(公费档退资金)→ M(发布者成员行,seq 建行守卫)→ Q → O(银两档退款指令)
// → guild_war_bounty(位置 17,主键点锁 + 状态复核)→ 待入账行(位置 24)。
// 多副本同时收尾同一张:后到者在护栏副本、G / Q 或追杀令行上排在先到者后面,锁到时状态已不是 ACTIVE → 回滚,只退一次;
// 与结算第 7 步之间同理(谁先提交谁算数,另一方锁内复核失败)。
func (r *WarRepo) ExpireBounty(ctx context.Context, in ExpireBountyInput) (ExpireBountyResult, error) {
	b := in.Bounty
	if b == nil || b.GetBountyId() == 0 || in.NowMs == 0 {
		return ExpireBountyResult{}, errors.New("war expire bounty: needs a candidate bounty and a non-zero now")
	}
	silver, funds := war.BountyExpiryRefund(b)
	if silver > 0 && (in.RefundOpID == 0 || len(in.RefundBundle) == 0) {
		return ExpireBountyResult{}, fmt.Errorf("war expire bounty %d: silver refund %d needs a pre-minted op id and bundle", b.GetBountyId(), silver)
	}
	var res ExpireBountyResult
	var fenceHealed bool
	err := r.guilds.inTx(ctx, "war_expire_bounty", func(ctx context.Context, tx *sql.Tx) error {
		res = ExpireBountyResult{}
		// 银两档退给发布者本人:先锁他的护栏副本(事务第一把锁,r2-gw#6)。公费档退的是帮会资金,不涉及任何角色。
		issuerVerdict := creditFenceOpen
		if silver > 0 {
			verdicts, err := r.guilds.lockCreditFences(ctx, tx, []uint64{b.GetIssuerPlayerId()}, &fenceHealed)
			if err != nil {
				return err
			}
			issuerVerdict = verdicts[b.GetIssuerPlayerId()]
		}
		if funds > 0 {
			ok, err := lockRowExists(ctx, tx, sqlLockGuildFunds, b.GetIssuerGuildId())
			if err != nil {
				return err
			}
			if ok {
				if _, err := tx.ExecContext(ctx, "UPDATE `guild` SET `funds` = `funds` + ? WHERE `guild_id` = ?", funds, b.GetIssuerGuildId()); err != nil {
					return err
				}
			} else {
				res.IssuerGuildGone = true
			}
		}
		var owed []WarCreditOwed
		switch {
		case silver > 0 && issuerVerdict == creditFenceRemoved:
			// 角色已删除:退给谁都不对,也不能再写他的行(删角收尾删过他的 seq 行)。追杀令照样到期,托管随角色一起作废。
			res.RefundAbandoned = true
		case silver > 0:
			// 发布者此刻不在原帮也照退(钱是他本人的),成员行只作 seq 建行守卫。
			if _, err := lockRowExists(ctx, tx, sqlLockMemberRole, b.GetIssuerGuildId(), b.GetIssuerPlayerId()); err != nil {
				return err
			}
			refund := WarCreditOwed{PlayerID: b.GetIssuerPlayerId(), OpID: in.RefundOpID, GuildID: b.GetIssuerGuildId(),
				Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_BOUNTY_PAYOUT, TxType: rollbackpb.TransactionType_TX_GUILD_BOUNTY_PAYOUT,
				RefID: b.GetTier(), Payload: in.RefundBundle, SourceID: b.GetBountyId()}
			created, owe, err := r.creditOrOweTx(ctx, tx, refund, issuerVerdict == creditFenceFrozen, warOpLease{nextAttemptMs: in.NowMs}, in.NowMs)
			if err != nil {
				return err
			}
			if owe != nil {
				owed = append(owed, *owe)
				res.RefundOwed = true
			} else {
				res.Op, res.HasOp = *created, true
			}
		}
		locked, err := loadBountyTx(ctx, tx, b.GetBountyId(), true)
		if err != nil {
			return err
		}
		if locked == nil || locked.GetStatus() != uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_ACTIVE) || locked.GetExpiresAtMs() > in.NowMs ||
			locked.GetIssuerPlayerId() != b.GetIssuerPlayerId() || locked.GetIssuerGuildId() != b.GetIssuerGuildId() {
			return errWarBountyNotDue
		}
		if lockedSilver, lockedFunds := war.BountyExpiryRefund(locked); lockedSilver != silver || lockedFunds != funds {
			return errWarBountyNotDue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE `"+guildWarBountyTable+"` SET `status` = ?, `resolved_at_ms` = ? WHERE `bounty_id` = ?",
			uint32(pb.GuildWarBountyStatus_GUILD_WAR_BOUNTY_STATUS_EXPIRED), in.NowMs, b.GetBountyId()); err != nil {
			return err
		}
		if err := insertWarCreditOwedTx(ctx, tx, owed); err != nil {
			return err
		}
		res.Expired = true
		return nil
	})
	if errors.Is(err, errWarBountyNotDue) {
		return ExpireBountyResult{}, nil
	}
	if err == nil && funds > 0 && !res.IssuerGuildGone {
		r.guilds.invalidateAfterCommit(ctx, "war_expire_bounty", b.GetIssuerGuildId())
	}
	return res, err
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

// loadBountyTx 在事务里按主键读一张追杀令。forUpdate=true 是写前的完整主键点锁(锁序位置 17,TiDB 走 Point_Get);
// false 是结算定计划用的普通读(不加锁,结论在锁序末尾由点锁复核)。
func loadBountyTx(ctx context.Context, tx *sql.Tx, bountyID uint64, forUpdate bool) (*pb.GuildWarBountyRecord, error) {
	q := "SELECT " + guildWarBountyColumns + " FROM `" + guildWarBountyTable + "` WHERE `bounty_id` = ?"
	if forUpdate {
		q += " FOR UPDATE"
	}
	return scanBountyRow(tx.QueryRowContext(ctx, q, bountyID).Scan)
}

func scanBounties(rows *sql.Rows) ([]*pb.GuildWarBountyRecord, error) {
	var out []*pb.GuildWarBountyRecord
	for rows.Next() {
		rec, err := scanBountyRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// lockZoneLeylinesTx 按 leyline_id 升序逐行点锁本区全部灵脉(锁序位置 13;完整主键等值 + FOR UPDATE,不做范围锁),
// 返回锁到的行(行由 EnsureZoneLeylines 预建,缺行就不在结果里)。所有结算者同序取锁,彼此只会排队、不会成环。
func lockZoneLeylinesTx(ctx context.Context, tx *sql.Tx, zoneID uint32) ([]*pb.GuildWarLeylineRecord, error) {
	out := make([]*pb.GuildWarLeylineRecord, 0, len(war.AllLeylines))
	for _, meta := range war.AllLeylines { // AllLeylines 按 ID 升序
		rec := &pb.GuildWarLeylineRecord{}
		err := tx.QueryRowContext(ctx,
			"SELECT `zone_id`,`leyline_id`,`owner_guild_id`,`banner_hp`,`shield_until_ms`,`captured_at_ms`,`last_dividend_at_ms`,`dividend_week_key`,`weekly_dividend_count`,`last_siege_alert_ms`,`updated_ms` FROM `"+guildWarLeylineTable+"` WHERE `zone_id` = ? AND `leyline_id` = ? FOR UPDATE",
			zoneID, meta.ID).Scan(&rec.ZoneId, &rec.LeylineId, &rec.OwnerGuildId, &rec.BannerHp, &rec.ShieldUntilMs,
			&rec.CapturedAtMs, &rec.LastDividendAtMs, &rec.DividendWeekKey, &rec.WeeklyDividendCount,
			&rec.LastSiegeAlertMs, &rec.UpdatedMs)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock leyline zone=%d id=%d: %w", zoneID, meta.ID, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// readBountyPayoutCountTx 普通读某人某游戏日已领的追杀 / 撕单赏金次数(完整主键等值;没有计数行 = 0)。
// 只给结算定计划用;真正占次数的是持着 seq 行之后的 upsertCounterWithLimit。
func readBountyPayoutCountTx(ctx context.Context, tx *sql.Tx, playerID uint64, dayKey uint32) (uint32, error) {
	var used uint32
	err := tx.QueryRowContext(ctx,
		"SELECT `used_count` FROM `guild_daily_counter` WHERE `player_id` = ? AND `counter_kind` = ? AND `ref_id` = ? AND `period_key` = ?",
		playerID, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_WAR_BOUNTY_PAYOUT), bountyPayoutCounterRefID, dayKey).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
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
