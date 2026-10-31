package logic

import (
	"context"
	"fmt"

	"data_service/internal/constants"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ListPlayerSnapshots 只读审计表 player_snapshot,不碰存档。
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
