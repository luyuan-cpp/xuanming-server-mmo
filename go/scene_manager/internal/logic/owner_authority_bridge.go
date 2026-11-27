package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/logwindow"
	"shared/ownerauthority"
)

const ownerAuthorityShadowLogWindowMs int64 = 5000

var ownerAuthorityShadowLogWindow logwindow.Window

func logOwnerAuthorityFailure(logger logx.Logger, enforce bool, format string, args ...any) {
	if logger == nil {
		return
	}
	if enforce {
		logger.Errorf(format, args...)
		return
	}
	if ok, streak := ownerAuthorityShadowLogWindow.Admit(time.Now().UnixMilli(), ownerAuthorityShadowLogWindowMs); ok {
		logger.Errorf(format+" shadow_error_streak=%d", append(args, streak)...)
	}
}

func noteOwnerAuthoritySuccess(logger logx.Logger, enforce bool) {
	if enforce {
		return
	}
	if failed, _ := ownerAuthorityShadowLogWindow.Recovered(); failed > 0 && logger != nil {
		logger.Infof("[OwnerAuthority] shadow path recovered after failed_total=%d", failed)
	}
}

func ownerAuthorityMode(svcCtx *svc.ServiceContext) string {
	if svcCtx == nil || svcCtx.OwnerAuthority == nil {
		return ""
	}
	if m := svcCtx.Config.OwnerAuthority.Mode; m == "enforce" {
		return "enforce"
	}
	return "shadow"
}

func ownerAuthorityTarget(nodeID string, sceneID uint64, zoneID uint32) ownerauthority.OwnerTarget {
	return ownerauthority.OwnerTarget{
		PodName:       nodeID,
		InstanceUID:   nodeID,
		InstanceEpoch: 1,
		AssignmentID:  fmt.Sprintf("z%d:s%d", zoneID, sceneID),
		ReleaseTrack:  "scene",
	}
}

func classifyOwnerAuthorityErr(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ownerauthority.ErrBarrierNotOpen):
		return "barrier"
	case errors.Is(err, ownerauthority.ErrEpochConflict):
		return "conflict"
	case errors.Is(err, ownerauthority.ErrSourceRevisionStale):
		return "stale_rev"
	case errors.Is(err, ownerauthority.ErrIdentityMismatch):
		return "mismatch"
	default:
		return "error"
	}
}

// syncOwnerAuthorityEnter bridges EnterScene into linearizable SQL ownerauthority.
// In shadow mode it dual-writes and logs divergences without blocking the Redis path.
// In enforce mode it gates EnterScene fail-closed before Kafka route publication and returns
// the committed record so callers can release it if Kafka publication fails.
func syncOwnerAuthorityEnter(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	logger logx.Logger,
	playerID, sceneID uint64,
	nodeID string,
	zoneID uint32,
	redisEpoch, ownerRevision uint64,
	enforce bool,
) (ownerauthority.OwnerRecord, error) {
	if svcCtx == nil || svcCtx.OwnerAuthority == nil {
		return ownerauthority.OwnerRecord{}, nil
	}
	mode := "shadow"
	if enforce {
		mode = "enforce"
	}
	target := ownerAuthorityTarget(nodeID, sceneID, zoneID)
	cur, found, err := svcCtx.OwnerAuthority.Query(ctx, playerID)
	if err != nil {
		metrics.ObserveOwnerAuthorityOp(mode, "enter", "query_error")
		logOwnerAuthorityFailure(logger, enforce, "[OwnerAuthority] enter query failed mode=%s player=%d scene=%d node=%s err=%v",
			mode, playerID, sceneID, nodeID, err)
		if enforce {
			return ownerauthority.OwnerRecord{}, err
		}
		return ownerauthority.OwnerRecord{}, nil
	}

	var expectEpoch uint64
	if found {
		expectEpoch = cur.OwnerEpoch
	}
	rec, err := svcCtx.OwnerAuthority.BeginTransition(ctx, ownerauthority.BeginRequest{
		PlayerID:                playerID,
		ExpectEpoch:             expectEpoch,
		TargetType:              ownerauthority.OwnerInstance,
		Target:                  target,
		SourceRevision:          ownerRevision,
		AllowSameSourceRevision: true,
		Detail:                  fmt.Sprintf("enter zone=%d scene=%d node=%s redis_epoch=%d", zoneID, sceneID, nodeID, redisEpoch),
	})
	if err != nil {
		res := classifyOwnerAuthorityErr(err)
		metrics.ObserveOwnerAuthorityOp(mode, "enter", res)
		logOwnerAuthorityFailure(logger, enforce, "[OwnerAuthority] begin transition failed mode=%s player=%d scene=%d node=%s result=%s err=%v",
			mode, playerID, sceneID, nodeID, res, err)
		if enforce {
			return ownerauthority.OwnerRecord{}, err
		}
		return ownerauthority.OwnerRecord{}, nil
	}

	admitted, err := svcCtx.OwnerAuthority.Admit(ctx, ownerauthority.AdmitRequest{
		PlayerID:    playerID,
		OwnerEpoch:  rec.OwnerEpoch,
		OperationID: rec.OperationID,
		Target:      target,
	})
	if err != nil {
		res := classifyOwnerAuthorityErr(err)
		metrics.ObserveOwnerAuthorityOp(mode, "enter", res)
		logOwnerAuthorityFailure(logger, enforce, "[OwnerAuthority] admit failed mode=%s player=%d epoch=%d scene=%d node=%s result=%s err=%v",
			mode, playerID, rec.OwnerEpoch, sceneID, nodeID, res, err)
		if enforce {
			return ownerauthority.OwnerRecord{}, err
		}
		return ownerauthority.OwnerRecord{}, nil
	}

	noteOwnerAuthoritySuccess(logger, enforce)
	metrics.ObserveOwnerAuthorityOp(mode, "enter", "ok")
	return admitted, nil
}

// syncOwnerAuthorityRollback releases a newly committed SQL owner_record when downstream
// Kafka route publication fails after enforce-mode EnterScene admission.
func syncOwnerAuthorityRollback(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	logger logx.Logger,
	rec ownerauthority.OwnerRecord,
) {
	if svcCtx == nil || svcCtx.OwnerAuthority == nil || rec.OwnerEpoch == 0 || rec.OperationID == "" {
		return
	}
	_, released, err := svcCtx.OwnerAuthority.Release(ctx, ownerauthority.ReleaseRequest{
		PlayerID:    rec.PlayerID,
		OwnerEpoch:  rec.OwnerEpoch,
		OperationID: rec.OperationID,
		Detail:      "enter_route_rollback",
	})
	if err != nil {
		metrics.ObserveOwnerAuthorityOp("enforce", "rollback", "error")
		if logger != nil {
			logger.Errorf("[OwnerAuthority] rollback release failed player=%d epoch=%d err=%v",
				rec.PlayerID, rec.OwnerEpoch, err)
		}
		return
	}
	if released {
		metrics.ObserveOwnerAuthorityOp("enforce", "rollback", "ok")
	} else {
		metrics.ObserveOwnerAuthorityOp("enforce", "rollback", "noop")
	}
}

// syncOwnerAuthorityLeave releases matching ownership in SQL ownerauthority after LeaveScene.
func syncOwnerAuthorityLeave(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	logger logx.Logger,
	playerID, sceneID uint64,
	nodeID string,
	zoneID uint32,
) error {
	mode := ownerAuthorityMode(svcCtx)
	if mode == "" {
		return nil
	}
	enforce := mode == "enforce"
	cur, found, err := svcCtx.OwnerAuthority.Query(ctx, playerID)
	if err != nil {
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "query_error")
		logOwnerAuthorityFailure(logger, enforce, "[OwnerAuthority] leave query failed mode=%s player=%d scene=%d err=%v",
			mode, playerID, sceneID, err)
		if enforce {
			return err
		}
		return nil
	}
	if !found || cur.Phase == ownerauthority.PhaseNone {
		noteOwnerAuthoritySuccess(logger, enforce)
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "noop")
		return nil
	}
	if nodeID != "" && !cur.Target.Equal(ownerAuthorityTarget(nodeID, sceneID, zoneID)) {
		noteOwnerAuthoritySuccess(logger, enforce)
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "target_changed")
		return nil
	}
	_, released, err := svcCtx.OwnerAuthority.Release(ctx, ownerauthority.ReleaseRequest{
		PlayerID:    playerID,
		OwnerEpoch:  cur.OwnerEpoch,
		OperationID: cur.OperationID,
		Detail:      fmt.Sprintf("leave zone=%d scene=%d node=%s", zoneID, sceneID, nodeID),
	})
	if err != nil {
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "error")
		logOwnerAuthorityFailure(logger, enforce, "[OwnerAuthority] release failed mode=%s player=%d epoch=%d err=%v",
			mode, playerID, cur.OwnerEpoch, err)
		if enforce {
			return err
		}
		return nil
	}
	noteOwnerAuthoritySuccess(logger, enforce)
	if released {
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "ok")
	} else {
		metrics.ObserveOwnerAuthorityOp(mode, "leave", "noop")
	}
	return nil
}
