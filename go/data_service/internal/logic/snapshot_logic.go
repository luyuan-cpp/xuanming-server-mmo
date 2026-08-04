package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// snapshotData is the JSON-serialized format stored in player_snapshot.data.
// Each key is a Redis field name, value is the raw bytes.
type snapshotData struct {
	Fields map[string][]byte `json:"fields"`
}

// ── CreatePlayerSnapshot ───────────────────────────────────────

type CreateSnapshotReq struct {
	PlayerID     uint64
	SnapshotType uint32
	Reason       string
	Operator     string
}

type CreateSnapshotResp struct {
	ErrorCode  uint32
	SnapshotID uint64
	CreatedAt  uint64
}

func CreatePlayerSnapshot(ctx context.Context, svcCtx *svc.ServiceContext, req *CreateSnapshotReq) (*CreateSnapshotResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("snapshot store is unavailable")
	}
	if svcCtx.Router == nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeRedis},
			fmt.Errorf("player data router is unavailable")
	}

	// 1. Load current player data from Redis
	loadResp, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: req.PlayerID})
	if err != nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if loadResp == nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeRedis},
			fmt.Errorf("load player %d returned nil response", req.PlayerID)
	}
	if loadResp.ErrorCode != constants.ErrCodeOK {
		return &CreateSnapshotResp{ErrorCode: loadResp.ErrorCode}, nil
	}
	if len(loadResp.Data) == 0 {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeNotFound}, nil
	}

	// 2. Serialize snapshot data
	sd := snapshotData{Fields: loadResp.Data}
	blob, err := json.Marshal(&sd)
	if err != nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("marshal snapshot: %w", err)
	}

	// 3. Look up zone
	zoneID, err := svcCtx.Router.GetPlayerHomeZone(ctx, req.PlayerID)
	if err != nil {
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeRedis},
			fmt.Errorf("resolve home zone for snapshot player %d: %w", req.PlayerID, err)
	}

	// 4. Persist to MySQL
	now := uint64(time.Now().Unix())
	row := &store.SnapshotRow{
		PlayerID:     req.PlayerID,
		ZoneID:       zoneID,
		SnapshotType: req.SnapshotType,
		CreatedAt:    now,
		Reason:       req.Reason,
		Operator:     req.Operator,
		Data:         blob,
	}

	id, err := svcCtx.SnapshotStore.InsertSnapshot(ctx, row)
	if err != nil {
		logx.Errorf("insert snapshot for player %d: %v", req.PlayerID, err)
		return &CreateSnapshotResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}

	logx.Infof("[Snapshot] created id=%d player=%d zone=%d type=%d operator=%s",
		id, req.PlayerID, zoneID, req.SnapshotType, req.Operator)

	return &CreateSnapshotResp{SnapshotID: id, CreatedAt: now}, nil
}

// ── ListPlayerSnapshots ────────────────────────────────────────

type ListSnapshotsReq struct {
	PlayerID   uint64
	BeforeTime uint64
	Limit      uint32
}

type SnapshotInfoItem struct {
	SnapshotID    uint64
	PlayerID      uint64
	ZoneID        uint32
	SnapshotType  uint32
	CreatedAt     uint64
	Reason        string
	Operator      string
	DataSizeBytes uint32
}

type ListSnapshotsResp struct {
	ErrorCode uint32
	Snapshots []*SnapshotInfoItem
}

func ListPlayerSnapshots(ctx context.Context, svcCtx *svc.ServiceContext, req *ListSnapshotsReq) (*ListSnapshotsResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &ListSnapshotsResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return &ListSnapshotsResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("snapshot store is unavailable")
	}

	metas, err := svcCtx.SnapshotStore.ListSnapshotsMeta(ctx, req.PlayerID, req.BeforeTime, req.Limit)
	if err != nil {
		logx.Errorf("list snapshots for player %d: %v", req.PlayerID, err)
		return &ListSnapshotsResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}

	items := make([]*SnapshotInfoItem, 0, len(metas))
	for _, m := range metas {
		items = append(items, &SnapshotInfoItem{
			SnapshotID:    m.ID,
			PlayerID:      m.PlayerID,
			ZoneID:        m.ZoneID,
			SnapshotType:  m.SnapshotType,
			CreatedAt:     m.CreatedAt,
			Reason:        m.Reason,
			Operator:      m.Operator,
			DataSizeBytes: m.DataSizeBytes,
		})
	}

	return &ListSnapshotsResp{Snapshots: items}, nil
}

// ── GetPlayerSnapshotDiff (soft rollback support) ──────────────

type SnapshotDiffReq struct {
	PlayerID   uint64
	SnapshotID uint64
	TargetTime uint64
}

type FieldDiffItem struct {
	Field          string
	SnapshotValue  []byte
	CurrentValue   []byte
	OnlyInSnapshot bool
	OnlyInCurrent  bool
}

type SnapshotDiffResp struct {
	ErrorCode      uint32
	SnapshotIDUsed uint64
	SnapshotTime   uint64
	Diffs          []*FieldDiffItem
}

func GetPlayerSnapshotDiff(ctx context.Context, svcCtx *svc.ServiceContext, req *SnapshotDiffReq) (*SnapshotDiffResp, error) {
	if req == nil || req.PlayerID == 0 {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeSnapshotDBError},
			fmt.Errorf("snapshot store is unavailable")
	}
	if svcCtx.Router == nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeRedis},
			fmt.Errorf("player data router is unavailable")
	}

	// 1. Resolve snapshot
	snap, err := resolveSnapshot(ctx, svcCtx, req.PlayerID, req.SnapshotID, req.TargetTime)
	if err != nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeSnapshotDBError}, err
	}
	if snap == nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeSnapshotNotFound}, nil
	}

	// 2. Deserialize snapshot
	var sd snapshotData
	if err := json.Unmarshal(snap.Data, &sd); err != nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeSnapshotDBError}, fmt.Errorf("unmarshal snapshot: %w", err)
	}

	// 3. Load current data
	loadResp, err := LoadPlayerData(ctx, svcCtx, &LoadPlayerDataReq{PlayerID: req.PlayerID})
	if err != nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if loadResp == nil {
		return &SnapshotDiffResp{ErrorCode: constants.ErrCodeRedis},
			fmt.Errorf("load player %d returned nil response", req.PlayerID)
	}
	if loadResp.ErrorCode != constants.ErrCodeOK {
		return &SnapshotDiffResp{ErrorCode: loadResp.ErrorCode}, nil
	}

	// 4. Compute diff
	diffs := computeDiff(sd.Fields, loadResp.Data)

	return &SnapshotDiffResp{
		SnapshotIDUsed: snap.ID,
		SnapshotTime:   snap.CreatedAt,
		Diffs:          diffs,
	}, nil
}

func computeDiff(snapshotFields, currentFields map[string][]byte) []*FieldDiffItem {
	visited := make(map[string]bool)
	var diffs []*FieldDiffItem

	for field, snapVal := range snapshotFields {
		visited[field] = true
		curVal, exists := currentFields[field]
		if !exists {
			diffs = append(diffs, &FieldDiffItem{
				Field:          field,
				SnapshotValue:  snapVal,
				OnlyInSnapshot: true,
			})
			continue
		}
		if string(snapVal) != string(curVal) {
			diffs = append(diffs, &FieldDiffItem{
				Field:         field,
				SnapshotValue: snapVal,
				CurrentValue:  curVal,
			})
		}
	}

	for field, curVal := range currentFields {
		if !visited[field] {
			diffs = append(diffs, &FieldDiffItem{
				Field:         field,
				CurrentValue:  curVal,
				OnlyInCurrent: true,
			})
		}
	}
	return diffs
}

// resolveSnapshot finds a snapshot by ID or by target_time.
func resolveSnapshot(ctx context.Context, svcCtx *svc.ServiceContext, playerID, snapshotID, targetTime uint64) (*store.SnapshotRow, error) {
	if svcCtx == nil || svcCtx.SnapshotStore == nil {
		return nil, fmt.Errorf("snapshot store is unavailable")
	}
	if snapshotID > 0 {
		snap, err := svcCtx.SnapshotStore.GetSnapshotByID(ctx, snapshotID)
		if err != nil {
			return nil, err
		}
		if snap != nil && snap.PlayerID != playerID {
			return nil, nil // snapshot belongs to a different player
		}
		return snap, nil
	}
	if targetTime > 0 {
		return svcCtx.SnapshotStore.GetLatestSnapshotBefore(ctx, playerID, targetTime)
	}
	return nil, nil
}
