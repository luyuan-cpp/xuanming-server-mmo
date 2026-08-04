package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	snapshotTypePreRollback uint32 = 3 // matches SnapshotType.SNAPSHOT_PRE_ROLLBACK
	rollbackTypePlayer      uint32 = 1
	rollbackTypeZone        uint32 = 2
	rollbackTypeServer      uint32 = 3
)

func validateRollbackDependencies(svcCtx *svc.ServiceContext) (uint32, error) {
	if svcCtx == nil {
		return constants.ErrCodeSnapshotDBError, fmt.Errorf("service context is unavailable")
	}
	if svcCtx.SnapshotStore == nil {
		return constants.ErrCodeSnapshotDBError, fmt.Errorf("snapshot/audit store is unavailable")
	}
	if svcCtx.Router == nil {
		return constants.ErrCodeRedis, fmt.Errorf("player data router is unavailable")
	}
	return constants.ErrCodeOK, nil
}

func rollbackFenceErrorCode(err error) uint32 {
	if errors.Is(err, svc.ErrRollbackTargetOnline) {
		return constants.ErrCodePlayerOnline
	}
	return constants.ErrCodeRollbackFailed
}

func validateRollbackRelease(release func(), scope string) (func(), uint32, error) {
	if release == nil {
		return nil, constants.ErrCodeRollbackFailed,
			fmt.Errorf("rollback fence for %s returned a nil release function", scope)
	}
	return release, constants.ErrCodeOK, nil
}

func acquirePlayerRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] player %d rejected: cross-service offline epoch fence is not configured", playerID)
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquirePlayer(ctx, playerID)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire rollback fence for player %d: %w", playerID, err)
	}
	return validateRollbackRelease(release, fmt.Sprintf("player %d", playerID))
}

func acquireZoneRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] zone %d rejected: cross-service offline epoch fence is not configured", zoneID)
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquireZone(ctx, zoneID)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire rollback fence for zone %d: %w", zoneID, err)
	}
	return validateRollbackRelease(release, fmt.Sprintf("zone %d", zoneID))
}

func acquireServerRollbackFence(ctx context.Context, svcCtx *svc.ServiceContext, zoneIDs []uint32) (func(), uint32, error) {
	if svcCtx.RollbackFence == nil {
		logx.Errorf("[Rollback] server rollback rejected: cross-service offline epoch fence is not configured")
		return nil, constants.ErrCodeNotImplemented, nil
	}
	release, err := svcCtx.RollbackFence.AcquireServer(ctx, zoneIDs)
	if err != nil {
		return nil, rollbackFenceErrorCode(err), fmt.Errorf("acquire server rollback fence: %w", err)
	}
	return validateRollbackRelease(release, "server")
}

func insertRollbackAudit(ctx context.Context, svcCtx *svc.ServiceContext, row *store.AuditLogRow) error {
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return fmt.Errorf("snapshot/audit store is unavailable")
	}
	if err := svcCtx.SnapshotStore.InsertAuditLog(ctx, row); err != nil {
		return fmt.Errorf("insert rollback audit: %w", err)
	}
	return nil
}

// ── RollbackPlayer ─────────────────────────────────────────────

type RollbackPlayerReq struct {
	PlayerID   uint64
	SnapshotID uint64
	TargetTime uint64
	Scope      uint32   // 0=full, 1=partial
	Fields     []string // only for partial
	Reason     string
	Operator   string
}

type RollbackPlayerResp struct {
	ErrorCode             uint32
	SnapshotIDUsed        uint64
	PreRollbackSnapshotID uint64
	FieldsRestored        []string
}

func RollbackPlayer(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackPlayerReq) (*RollbackPlayerResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if req.SnapshotID == 0 && req.TargetTime == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}

	release, code, err := acquirePlayerRollbackFence(ctx, svcCtx, req.PlayerID)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}
	defer release()

	// 先写请求意图再做任何修改：审计库不可用时，高风险回档必须零变更。
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		PlayerID:     req.PlayerID,
		RollbackType: rollbackTypePlayer,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("write player rollback intent audit: %w", err)
	}

	resp, rollbackErr := rollbackSinglePlayer(ctx, svcCtx, req)
	if resp == nil {
		resp = &RollbackPlayerResp{ErrorCode: constants.ErrCodeRollbackFailed}
	}

	var affected, failed uint32
	if rollbackErr == nil && resp.ErrorCode == constants.ErrCodeOK {
		affected = 1
	} else {
		failed = 1
	}

	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		PlayerID:              req.PlayerID,
		RollbackType:          rollbackTypePlayer,
		SnapshotIDUsed:        resp.SnapshotIDUsed,
		PreRollbackSnapshotID: resp.PreRollbackSnapshotID,
		TargetTime:            req.TargetTime,
		PlayersAffected:       affected,
		PlayersFailed:         failed,
		Reason:                fmt.Sprintf("RESULT code=%d: %s", resp.ErrorCode, req.Reason),
		Operator:              req.Operator,
		CreatedAt:             uint64(time.Now().Unix()),
	}); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		metrics.ObserveRollback("player", "failed", 0, 0)
		return resp, fmt.Errorf("write player rollback result audit: %w", err)
	}

	if rollbackErr != nil || resp.ErrorCode != constants.ErrCodeOK {
		metrics.ObserveRollback("player", "failed", 0, 0)
		logx.Errorf("[Rollback] player %d failed: err=%v code=%d", req.PlayerID, rollbackErr, resp.ErrorCode)
		return resp, rollbackErr
	}
	metrics.ObserveRollback("player", "ok", 1, 0)

	logx.Infof("[Rollback] player %d restored from snapshot %d (pre-rollback=%d) by %s: %s",
		req.PlayerID, resp.SnapshotIDUsed, resp.PreRollbackSnapshotID, req.Operator, req.Reason)

	return resp, nil
}

func rollbackSinglePlayer(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackPlayerReq) (*RollbackPlayerResp, error) {
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackPlayerResp{ErrorCode: code}, err
	}

	// 1. Resolve target snapshot
	snap, err := resolveSnapshot(ctx, svcCtx, req.PlayerID, req.SnapshotID, req.TargetTime)
	if err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if snap == nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotNotFound}, nil
	}

	// 2. Deserialize snapshot data
	var sd snapshotData
	if err := json.Unmarshal(snap.Data, &sd); err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("unmarshal snapshot %d: %w", snap.ID, err)
	}

	// 3. Create pre-rollback safety snapshot (so we can undo the rollback if needed)
	preSnap, err := CreatePlayerSnapshot(ctx, svcCtx, &CreateSnapshotReq{
		PlayerID:     req.PlayerID,
		SnapshotType: snapshotTypePreRollback,
		Reason:       fmt.Sprintf("pre-rollback safety snapshot (target snapshot=%d)", snap.ID),
		Operator:     req.Operator,
	})
	if err != nil {
		code := constants.ErrCodeSnapshotDBError
		if preSnap != nil && preSnap.ErrorCode != constants.ErrCodeOK {
			code = preSnap.ErrorCode
		}
		return &RollbackPlayerResp{ErrorCode: code, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d: %w", req.PlayerID, err)
	}
	if preSnap == nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d returned nil response", req.PlayerID)
	}
	if preSnap.ErrorCode != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: preSnap.ErrorCode, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d failed with code %d", req.PlayerID, preSnap.ErrorCode)
	}
	if preSnap.SnapshotID == 0 {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeSnapshotDBError, SnapshotIDUsed: snap.ID},
			fmt.Errorf("create pre-rollback safety snapshot for player %d returned snapshot id 0", req.PlayerID)
	}
	preRollbackID := preSnap.SnapshotID

	// 4. Determine which fields to restore
	fieldsToRestore := sd.Fields
	if req.Scope == 1 && len(req.Fields) > 0 {
		// Partial rollback — only restore requested fields
		filtered := make(map[string][]byte, len(req.Fields))
		for _, f := range req.Fields {
			if val, ok := sd.Fields[f]; ok {
				filtered[f] = val
			}
		}
		fieldsToRestore = filtered
	}

	if len(fieldsToRestore) == 0 {
		return &RollbackPlayerResp{
			ErrorCode:             constants.ErrCodeSnapshotNotFound,
			SnapshotIDUsed:        snap.ID,
			PreRollbackSnapshotID: preRollbackID,
		}, nil
	}

	// 5. Write snapshot data to Redis (overwrite current player data)
	saveResp, err := SavePlayerData(ctx, svcCtx, &SavePlayerDataReq{
		PlayerID:        req.PlayerID,
		Data:            fieldsToRestore,
		ExpectedVersion: 0, // skip version check — this is an admin override
	})
	if err != nil {
		return &RollbackPlayerResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if saveResp.ErrorCode != constants.ErrCodeOK {
		return &RollbackPlayerResp{ErrorCode: saveResp.ErrorCode}, nil
	}

	// 6. Collect field names
	restoredFields := make([]string, 0, len(fieldsToRestore))
	for f := range fieldsToRestore {
		restoredFields = append(restoredFields, f)
	}

	return &RollbackPlayerResp{
		SnapshotIDUsed:        snap.ID,
		PreRollbackSnapshotID: preRollbackID,
		FieldsRestored:        restoredFields,
	}, nil
}

// ── RollbackZone ───────────────────────────────────────────────

type RollbackZoneReq struct {
	ZoneID     uint32
	TargetTime uint64
	Reason     string
	Operator   string
}

type RollbackZoneResp struct {
	ErrorCode       uint32
	PlayersAffected uint32
	PlayersFailed   uint32
	FailedPlayerIDs []uint64
	// OrphanPlayerIDs 是**候选**名单:zone 内当前存在、但在 target_time 之前没有
	// 任何快照的玩家。它不代表这些角色确实是 target_time 之后创建的,也不代表
	// 它们被删除了 —— 见 reportOrphanCandidates。
	OrphanPlayerIDs []uint64
	// OrphansCleaned 恒为 0:本服务不再自动删除孤儿候选。字段保留是为了不破坏
	// 已有调用方与 proto 兼容性(只减少语义、不改编号)。
	OrphansCleaned uint32
}

func RollbackZone(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackZoneReq) (*RollbackZoneResp, error) {
	if req == nil || req.ZoneID == 0 || req.TargetTime == 0 {
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackZoneResp{ErrorCode: code}, err
	}

	release, code, err := acquireZoneRollbackFence(ctx, svcCtx, req.ZoneID)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackZoneResp{ErrorCode: code}, err
	}
	defer release()

	resp, rollbackErr := rollbackZoneWithFenceHeld(ctx, svcCtx, req)
	outcome := "ok"
	if rollbackErr != nil || resp.ErrorCode != constants.ErrCodeOK {
		outcome = "failed"
	} else if resp.PlayersFailed > 0 && resp.PlayersAffected > 0 {
		outcome = "partial"
	} else if resp.PlayersFailed > 0 {
		outcome = "failed"
	}
	metrics.ObserveRollback("zone", outcome, resp.PlayersAffected, resp.OrphansCleaned)
	return resp, rollbackErr
}

// rollbackZoneWithFenceHeld 只能在上层持有 zone 或 server 级跨服务
// 离线 epoch 栅栏时调用。它不会自行做一次有 TOCTOU 窗口的“在线查询”。
func rollbackZoneWithFenceHeld(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackZoneReq) (result *RollbackZoneResp, retErr error) {
	result = &RollbackZoneResp{}
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		ZoneID:       req.ZoneID,
		RollbackType: rollbackTypeZone,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		result.ErrorCode = constants.ErrCodeSnapshotDBError
		return result, fmt.Errorf("write zone rollback intent audit: %w", err)
	}

	var affected, failed uint32
	defer func() {
		if result == nil {
			result = &RollbackZoneResp{ErrorCode: constants.ErrCodeRollbackFailed}
		}
		auditErr := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
			ZoneID:          req.ZoneID,
			RollbackType:    rollbackTypeZone,
			TargetTime:      req.TargetTime,
			PlayersAffected: affected,
			PlayersFailed:   failed,
			OrphansCleaned:  0,
			Reason:          fmt.Sprintf("RESULT code=%d: %s", result.ErrorCode, req.Reason),
			Operator:        req.Operator,
			CreatedAt:       uint64(time.Now().Unix()),
		})
		if auditErr != nil {
			result.ErrorCode = constants.ErrCodeSnapshotDBError
			retErr = fmt.Errorf("write zone rollback result audit: %w", auditErr)
		}
	}()

	logx.Infof("[Rollback] zone=%d target_time=%d operator=%s reason=%s",
		req.ZoneID, req.TargetTime, req.Operator, req.Reason)

	// NOTE: Guild/friend data is NOT rolled back.
	// Player Redis blob does not contain guild_id or friend references;
	// guild/friend state is owned by separate Go services and queried via gRPC.
	// Any minor inconsistencies (e.g. stale guild membership) are self-healing
	// through normal guild/friend operations.

	// ── Phase 1: Rollback player data from individual snapshots ─
	// 1. Get all players in this zone that have snapshots
	playerIDs, err := svcCtx.SnapshotStore.GetSnapshotPlayerIDsByZone(ctx, req.ZoneID, req.TargetTime)
	if err != nil {
		logx.Errorf("[Rollback] zone %d: failed to list players: %v", req.ZoneID, err)
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}

	logx.Infof("[Rollback] zone %d: found %d players to rollback", req.ZoneID, len(playerIDs))

	// Build set for fast lookup
	snapshotPlayerSet := make(map[uint64]bool, len(playerIDs))
	for _, pid := range playerIDs {
		snapshotPlayerSet[pid] = true
	}

	// 2. Rollback each player
	var failedIDs []uint64

	for _, pid := range playerIDs {
		resp, err := rollbackSinglePlayer(ctx, svcCtx, &RollbackPlayerReq{
			PlayerID:   pid,
			TargetTime: req.TargetTime,
			Reason:     fmt.Sprintf("zone %d rollback: %s", req.ZoneID, req.Reason),
			Operator:   req.Operator,
		})
		if err != nil || resp.ErrorCode != constants.ErrCodeOK {
			failed++
			failedIDs = append(failedIDs, pid)
			logx.Errorf("[Rollback] zone %d player %d failed: err=%v code=%d",
				req.ZoneID, pid, err, resp.ErrorCode)
			continue
		}
		affected++
	}

	// ── Phase 2: 报告孤儿候选(只报告,不删除)────────────────
	orphanIDs, err := reportOrphanCandidates(ctx, svcCtx, req.ZoneID, snapshotPlayerSet)
	if err != nil {
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	// “无快照”不能提前等价成“zone 不存在”。先扫描当前映射，才能把整个
	// 无快照 zone 的玩家全部作为候选返回。只有快照与当前映射都为空时才是空 zone。
	if len(playerIDs) == 0 && len(orphanIDs) == 0 {
		return &RollbackZoneResp{ErrorCode: constants.ErrCodeZoneNotFound}, nil
	}
	var orphansCleaned uint32 // 恒为 0:本服务不再据此删除任何玩家数据,见下方说明

	logx.Infof("[Rollback] zone %d complete: affected=%d failed=%d orphans_cleaned=%d",
		req.ZoneID, affected, failed, orphansCleaned)

	return &RollbackZoneResp{
		PlayersAffected: affected,
		PlayersFailed:   failed,
		FailedPlayerIDs: failedIDs,
		OrphanPlayerIDs: orphanIDs,
		OrphansCleaned:  orphansCleaned,
	}, nil
}

// reportOrphanCandidates 列出「zone 内当前存在、但在 target_time 之前没有任何快照」
// 的玩家,**只报告,不做任何删除**。
//
// 为什么改成只报告(这里原来会硬删,是一条 P0):
//
//	判定依据 GetSnapshotPlayerIDsByZone 查的是 `player_snapshot WHERE zone_id=? AND
//	created_at<=?`,也就是"该玩家在这个 zone 有一份 target_time 之前的快照"。
//	而快照**只在显式触发时才产生** —— CreatePlayerSnapshot / CreateEventSnapshot 的
//	GM 调用,加上回档自己打的 pre-rollback 安全快照;全仓没有任何周期性快照任务。
//	于是"没有快照"根本不等于"target_time 之后才创建":
//	  * 从没触发过快照事件的普通老玩家 —— 绝大多数玩家都是这一类;
//	  * 快照已被 DeleteOldSnapshots 按保留期删掉的老玩家;
//	  * 从别的 zone 迁过来、历史快照的 zone_id 还停在旧 zone 的玩家。
//
//	旧实现对这些人执行 DeletePlayerData(DeleteZoneMapping=true) 并调 login
//	RemovePlayersFromAccounts 把角色从账号里摘掉,而且**不走** rollbackSinglePlayer
//	的 pre-rollback 安全快照 —— 删完无从恢复。一次例行的 zone 回档就能把整个 zone
//	的老玩家抹掉。
//
// 要恢复自动清理,前提是拿到**权威的角色创建时间**(login/account 记录,或玩家数据
// 里的 created_at 字段)并按它判定,而不是拿快照存在性当代理;在那之前这里 fail-closed。
// 候选名单仍然通过响应的 OrphanPlayerIDs 返回,供人工核对后走单独的删除工具。
func reportOrphanCandidates(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, snapshotPlayerSet map[uint64]bool) ([]uint64, error) {
	currentPlayers, err := svcCtx.Router.GetAllPlayerIDsInZone(ctx, zoneID)
	if err != nil {
		logx.Errorf("[Rollback] zone %d: failed to scan current players for orphan report: %v", zoneID, err)
		return nil, fmt.Errorf("scan zone %d orphan candidates: %w", zoneID, err)
	}

	var orphanIDs []uint64
	for _, pid := range currentPlayers {
		if snapshotPlayerSet[pid] {
			continue // 在 target_time 之前有快照,肯定不是新建角色
		}
		orphanIDs = append(orphanIDs, pid)
	}

	if len(orphanIDs) > 0 {
		logx.Errorf("[Rollback] zone %d: %d players have no snapshot at or before target_time. "+
			"NOT deleted — snapshot absence does not prove the character was created after target_time. "+
			"Candidate IDs are returned in OrphanPlayerIDs for manual review.",
			zoneID, len(orphanIDs))
	}
	return orphanIDs, nil
}

// ── RollbackAll (full server) ──────────────────────────────────

type RollbackAllReq struct {
	TargetTime uint64
	Reason     string
	Operator   string
}

type RollbackAllResp struct {
	ErrorCode       uint32
	ZonesProcessed  uint32
	PlayersAffected uint32
	PlayersFailed   uint32
}

func RollbackAll(ctx context.Context, svcCtx *svc.ServiceContext, req *RollbackAllReq) (*RollbackAllResp, error) {
	if req == nil || req.TargetTime == 0 {
		return &RollbackAllResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if code, err := validateRollbackDependencies(svcCtx); err != nil {
		return &RollbackAllResp{ErrorCode: code}, err
	}

	zoneIDs := svcCtx.Router.AllZoneIDs()
	release, code, err := acquireServerRollbackFence(ctx, svcCtx, zoneIDs)
	if code != constants.ErrCodeOK || err != nil {
		return &RollbackAllResp{ErrorCode: code}, err
	}
	defer release()

	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		RollbackType: rollbackTypeServer,
		TargetTime:   req.TargetTime,
		Reason:       fmt.Sprintf("STARTED: %s", req.Reason),
		Operator:     req.Operator,
		CreatedAt:    uint64(time.Now().Unix()),
	}); err != nil {
		return &RollbackAllResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("write server rollback intent audit: %w", err)
	}

	logx.Infof("[Rollback] FULL SERVER rollback target_time=%d operator=%s reason=%s",
		req.TargetTime, req.Operator, req.Reason)

	var totalAffected, totalFailed, zonesProcessed uint32
	var rollbackErr error
	resultCode := constants.ErrCodeOK

	for _, zoneID := range zoneIDs {
		// server 级栅栏已覆盖全部 zone，不再逐 zone 重复 Acquire。
		resp, zoneErr := rollbackZoneWithFenceHeld(ctx, svcCtx, &RollbackZoneReq{
			ZoneID:     zoneID,
			TargetTime: req.TargetTime,
			Reason:     fmt.Sprintf("server rollback: %s", req.Reason),
			Operator:   req.Operator,
		})
		if resp != nil {
			totalAffected += resp.PlayersAffected
			totalFailed += resp.PlayersFailed
		}
		if zoneErr != nil {
			resultCode = constants.ErrCodeRollbackFailed
			if resp != nil && resp.ErrorCode != constants.ErrCodeOK {
				resultCode = resp.ErrorCode
			}
			rollbackErr = fmt.Errorf("zone %d failed during server rollback: %w", zoneID, zoneErr)
			logx.Errorf("[Rollback] %v", rollbackErr)
			// 任一 zone 的审计/存储前置失败都不能被“继续下一个”吞掉。
			break
		}
		zonesProcessed++
	}

	resp := &RollbackAllResp{
		ErrorCode:       resultCode,
		ZonesProcessed:  zonesProcessed,
		PlayersAffected: totalAffected,
		PlayersFailed:   totalFailed,
	}
	if err := insertRollbackAudit(ctx, svcCtx, &store.AuditLogRow{
		RollbackType:    rollbackTypeServer,
		TargetTime:      req.TargetTime,
		PlayersAffected: totalAffected,
		PlayersFailed:   totalFailed,
		Reason:          fmt.Sprintf("RESULT code=%d: %s", resultCode, req.Reason),
		Operator:        req.Operator,
		CreatedAt:       uint64(time.Now().Unix()),
	}); err != nil {
		resp.ErrorCode = constants.ErrCodeSnapshotDBError
		metrics.ObserveRollback("server", "failed", totalAffected, 0)
		return resp, fmt.Errorf("write server rollback result audit: %w", err)
	}

	if rollbackErr != nil {
		metrics.ObserveRollback("server", "failed", totalAffected, 0)
		return resp, rollbackErr
	}

	logx.Infof("[Rollback] FULL SERVER complete: zones=%d affected=%d failed=%d",
		zonesProcessed, totalAffected, totalFailed)

	rbOutcome := "ok"
	if totalFailed > 0 && totalAffected == 0 {
		rbOutcome = "failed"
	} else if totalFailed > 0 {
		rbOutcome = "partial"
	}
	metrics.ObserveRollback("server", rbOutcome, totalAffected, 0)

	return resp, nil
}
