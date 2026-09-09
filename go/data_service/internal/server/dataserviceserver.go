package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"data_service/internal/constants"
	"data_service/internal/logic"
	"data_service/internal/routing"
	"data_service/internal/svc"
	"proto/data_service"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type DataServiceServer struct {
	svcCtx *svc.ServiceContext
	data_service.UnimplementedDataServiceServer
}

func NewDataServiceServer(svcCtx *svc.ServiceContext) *DataServiceServer {
	return &DataServiceServer{svcCtx: svcCtx}
}

func (s *DataServiceServer) LoadPlayerData(ctx context.Context, req *data_service.LoadPlayerDataRequest) (*data_service.LoadPlayerDataResponse, error) {
	resp, err := logic.LoadPlayerData(ctx, s.svcCtx, &logic.LoadPlayerDataReq{
		PlayerID: req.PlayerId,
		Fields:   req.Fields,
	})
	if err != nil {
		return &data_service.LoadPlayerDataResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.LoadPlayerDataResponse{
		ErrorCode: resp.ErrorCode,
		Data:      resp.Data,
		Version:   resp.Version,
	}, nil
}

func (s *DataServiceServer) SavePlayerData(ctx context.Context, req *data_service.SavePlayerDataRequest) (*data_service.SavePlayerDataResponse, error) {
	resp, err := logic.SavePlayerData(ctx, s.svcCtx, &logic.SavePlayerDataReq{
		PlayerID:        req.PlayerId,
		Data:            req.Data,
		ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		return &data_service.SavePlayerDataResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.SavePlayerDataResponse{
		ErrorCode:  resp.ErrorCode,
		NewVersion: resp.NewVersion,
	}, nil
}

func (s *DataServiceServer) GetPlayerField(ctx context.Context, req *data_service.GetPlayerFieldRequest) (*data_service.GetPlayerFieldResponse, error) {
	val, err := logic.GetPlayerField(ctx, s.svcCtx, req.PlayerId, req.Field)
	if err != nil {
		return &data_service.GetPlayerFieldResponse{ErrorCode: constants.ErrCodeRedis}, nil
	}
	return &data_service.GetPlayerFieldResponse{Value: val}, nil
}

func (s *DataServiceServer) SetPlayerField(ctx context.Context, req *data_service.SetPlayerFieldRequest) (*data_service.SetPlayerFieldResponse, error) {
	resp, err := logic.SetPlayerField(ctx, s.svcCtx, req.PlayerId, req.Field, req.Value, req.ExpectedVersion)
	if err != nil {
		return &data_service.SetPlayerFieldResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.SetPlayerFieldResponse{
		ErrorCode:  resp.ErrorCode,
		NewVersion: resp.NewVersion,
	}, nil
}

// RegisterPlayerZone 登记 player → home_zone 映射,SETNX 语义(见 routing.RegisterPlayerZone)。
//
// 本 RPC 的响应体是 emptypb.Empty,没有 error_code 字段可放码,所以失败一律走 gRPC
// status;码用 constants 里的常量拼进 message(`error_code=<n>`),运维按同一根轴排障。
//
//   - 映射不存在 / 已存在且相同 → OK(幂等)
//   - 已存在且不同             → AlreadyExists + ErrCodeZoneMappingConflict,消息带既有 zone
//   - 目标 zone 正在合服        → FailedPrecondition + ErrCodeZoneMergeInProgress
//   - mapping Redis 故障        → Unavailable(闸门查询失败也走这里:fail-closed)
func (s *DataServiceServer) RegisterPlayerZone(ctx context.Context, req *data_service.RegisterPlayerZoneRequest) (*emptypb.Empty, error) {
	err := s.svcCtx.Router.RegisterPlayerZone(ctx, req.PlayerId, req.HomeZoneId)
	if err == nil {
		return &emptypb.Empty{}, nil
	}

	var conflict *routing.HomeZoneConflictError
	switch {
	case errors.As(err, &conflict):
		// 有人试图把一名已有归属的玩家写到另一个 zone。合服之后这通常意味着
		// 「按建角 zone 重新登记」的老路径又被走了一遍,必须留下证据。
		logx.Errorf("[RegisterPlayerZone] refused overwrite: player=%d existing_zone=%d requested_zone=%d",
			conflict.PlayerID, conflict.Existing, conflict.Requested)
		return nil, status.Errorf(codes.AlreadyExists, "error_code=%d: %s",
			constants.ErrCodeZoneMappingConflict, conflict.Error())
	case errors.Is(err, routing.ErrZoneMergeInProgress):
		logx.Errorf("[RegisterPlayerZone] refused by merge fence: player=%d zone=%d", req.PlayerId, req.HomeZoneId)
		return nil, status.Errorf(codes.FailedPrecondition, "error_code=%d: %v",
			constants.ErrCodeZoneMergeInProgress, err)
	default:
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
}

// GetPlayerHomeZone 的线上契约(proto 注释不归本服务改,以此处为准):
//   - 映射存在           → HomeZoneId=归属 zone, err=nil
//   - 映射不存在         → codes.NotFound,消息含 "no home zone mapping"
//   - mapping Redis 故障 → codes.Unavailable
//
// 为什么「不存在」要用一个**专门的码**而不是 HomeZoneId=0 + nil:调用方必须能把
// 「这名玩家确实没有映射」和「我没查到,因为 Redis 挂了」分开。前者是数据状态
// (存量玩家建号早于 CreatePlayer 注册映射,或合服回填还没跑),正确处理是回退到
// 账号 blob 里的建角 zone;后者是故障,回退同样发生但必须被告警看见。用同一个
// (0, nil) 表示两者,等于让每一次 mapping Redis 故障都伪装成一批"老玩家",
// 合服当天最需要这条信号时它恰好是哑的。
//
// login 侧(go/login/internal/logic/pkg/homezone.IsUnmapped)同时认 NotFound 与
// 老版本的 Unknown + 文案,两种服务端可以在滚动升级期间并存;所以 NotFound 的
// 消息必须继续包含 "no home zone mapping" 这段文本,不能改写。
func (s *DataServiceServer) GetPlayerHomeZone(ctx context.Context, req *data_service.GetPlayerHomeZoneRequest) (*data_service.GetPlayerHomeZoneResponse, error) {
	zoneID, err := s.svcCtx.Router.GetPlayerHomeZone(ctx, req.PlayerId)
	if err != nil {
		if errors.Is(err, routing.ErrHomeZoneNotMapped) {
			// err.Error() 形如 "no home zone mapping for player 42";文案是契约,见上。
			return nil, status.Errorf(codes.NotFound, "error_code=%d: %v", constants.ErrCodeNotFound, err)
		}
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
	return &data_service.GetPlayerHomeZoneResponse{HomeZoneId: zoneID}, nil
}

// BatchGetPlayerHomeZone 与单查刻意不同:**缺席的 id 直接不出现在 map 里**,不是错误。
// 批量查的调用方(login 的角色列表)天然要处理"部分有部分没有",给整批返回 NotFound
// 会把一个正常状态升级成整批失败;单查只有一个 id,没有"部分"可言,所以那边用 NotFound。
func (s *DataServiceServer) BatchGetPlayerHomeZone(ctx context.Context, req *data_service.BatchGetPlayerHomeZoneRequest) (*data_service.BatchGetPlayerHomeZoneResponse, error) {
	mapping, err := s.svcCtx.Router.BatchGetPlayerHomeZone(ctx, req.PlayerIds)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
	return &data_service.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: mapping}, nil
}

// adminTokenMetadataKey 是运维 RPC 的凭据所在的 gRPC metadata 键。
// 必须全小写:gRPC 在传输层把 metadata key 规范成小写,写成 X-Admin-Token
// 在服务端用 md.Get("X-Admin-Token") 也能取到(go-grpc 的 MD.Get 自己会转小写),
// 但直接读 map 就取不到 —— 这里统一用小写字面量,不给自己留这个坑。
const adminTokenMetadataKey = "x-admin-token"

// authorizeAdmin 校验运维凭据。返回的 error 已经是 gRPC status,可直接回给调用方。
//
// 配置里 AdminToken 为空 = 整个 RPC 停用(fail closed),原因见 config.AdminToken。
// 比较用 subtle.ConstantTimeCompare:token 是共享口令,朴素的 == 会随比较位置提前
// 返回,给逐字节爆破留下时间侧信道。长度不同也照样跑完一次固定开销的比较。
func (s *DataServiceServer) authorizeAdmin(ctx context.Context, rpcName string) error {
	want := s.svcCtx.Config.AdminToken
	if want == "" {
		logx.Errorf("[admin] %s refused: AdminToken not configured (the RPC is disabled) caller=%s", rpcName, callerIdentity(ctx))
		return status.Errorf(codes.PermissionDenied,
			"error_code=%d: %s is disabled because AdminToken is not configured", constants.ErrCodeAdminAuthRequired, rpcName)
	}
	var got string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(adminTokenMetadataKey); len(vals) > 0 {
			got = vals[0]
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		// 只记"有没有带",绝不记 token 本身 —— 日志会进 Loki,写进去就等于泄露。
		logx.Errorf("[admin] %s refused: bad or missing %s (present=%t) caller=%s",
			rpcName, adminTokenMetadataKey, got != "", callerIdentity(ctx))
		return status.Errorf(codes.PermissionDenied,
			"error_code=%d: %s requires a valid %s", constants.ErrCodeAdminAuthRequired, rpcName, adminTokenMetadataKey)
	}
	return nil
}

// callerIdentity 拼一条"谁在调"的日志片段:对端地址 + 调用方自报的 user-agent /
// x-operator。地址是唯一不可伪造的部分,后两者只是给人看的线索,不能当鉴权依据。
func callerIdentity(ctx context.Context) string {
	addr := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	agent, operator := "", ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("user-agent"); len(v) > 0 {
			agent = v[0]
		}
		if v := md.Get("x-operator"); len(v) > 0 {
			operator = v[0]
		}
	}
	return fmt.Sprintf("peer=%s user-agent=%q operator=%q", addr, agent, operator)
}

// RemapHomeZoneForMerge 把所有 player:zone:*==source 的映射改写成 target(合服)。
//
// 一次调用改写全服玩家的归属,而且**没有反向操作**(要改回去得先知道谁原本在哪个
// zone,而那份信息刚被覆盖掉)。所以三道闸全部满足才执行:
//
//  1. AdminToken 已配置,且 metadata 的 x-admin-token 与之相符 —— 否则
//     PermissionDenied + ErrCodeAdminAuthRequired。没配 = 停用,不是免鉴权。
//  2. 源 zone 已立 merge:in_progress:{source} 标记 —— 否则 FailedPrecondition +
//     ErrCodeMergeFenceMissing。合服工具必须先立标记(那个标记同时在挡住
//     RegisterPlayerZone),线上误调用因此被挡在门外。
//  3. 参数合法(source/target 非零且不同)。
//
// 每一次调用(通过 / 拒绝 / 结果)都留日志并带上调用方身份与影响条数:这是事后
// 唯一能回答"谁在什么时候把哪批玩家搬到了哪里"的记录。
func (s *DataServiceServer) RemapHomeZoneForMerge(ctx context.Context, req *data_service.RemapHomeZoneForMergeRequest) (*data_service.RemapHomeZoneForMergeResponse, error) {
	const rpcName = "RemapHomeZoneForMerge"
	if err := s.authorizeAdmin(ctx, rpcName); err != nil {
		return nil, err
	}
	if req.SourceZoneId == 0 || req.TargetZoneId == 0 || req.SourceZoneId == req.TargetZoneId {
		logx.Errorf("[admin] %s refused: bad zones source=%d target=%d %s",
			rpcName, req.SourceZoneId, req.TargetZoneId, callerIdentity(ctx))
		return nil, status.Errorf(codes.InvalidArgument,
			"error_code=%d: source_zone_id and target_zone_id must be non-zero and different", constants.ErrCodeInvalidRequest)
	}

	logx.Infof("[admin] %s authorized: source=%d target=%d dry_run=%t %s",
		rpcName, req.SourceZoneId, req.TargetZoneId, req.DryRun, callerIdentity(ctx))

	matched, updated, err := s.svcCtx.Router.RemapHomeZoneForMerge(ctx, req.SourceZoneId, req.TargetZoneId, req.DryRun)
	if err != nil {
		if errors.Is(err, routing.ErrMergeFenceMissing) {
			logx.Errorf("[admin] %s refused: merge fence %s absent (source zone still accepts new mappings) %s",
				rpcName, routing.MergeFenceKey(req.SourceZoneId), callerIdentity(ctx))
			return nil, status.Errorf(codes.FailedPrecondition, "error_code=%d: %v", constants.ErrCodeMergeFenceMissing, err)
		}
		// 中途失败时 matched/updated 是**已经发生**的改写量,必须记下来:
		// 重跑是幂等的(按值匹配),但运维需要知道这次跑到哪儿停的。
		logx.Errorf("[admin] %s FAILED after matched=%d updated=%d: source=%d target=%d dry_run=%t %s: %v",
			rpcName, matched, updated, req.SourceZoneId, req.TargetZoneId, req.DryRun, callerIdentity(ctx), err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
	logx.Infof("[admin] %s done: source=%d target=%d dry_run=%t matched=%d updated=%d %s",
		rpcName, req.SourceZoneId, req.TargetZoneId, req.DryRun, matched, updated, callerIdentity(ctx))
	return &data_service.RemapHomeZoneForMergeResponse{
		ErrorCode:      constants.ErrCodeOK,
		PlayersMatched: uint32(matched),
		PlayersUpdated: uint32(updated),
	}, nil
}

func (s *DataServiceServer) DeletePlayerData(ctx context.Context, req *data_service.DeletePlayerDataRequest) (*data_service.DeletePlayerDataResponse, error) {
	resp, err := logic.DeletePlayerData(ctx, s.svcCtx, &logic.DeletePlayerDataReq{
		PlayerID:          req.PlayerId,
		DeleteZoneMapping: req.DeleteZoneMapping,
	})
	if err != nil {
		return &data_service.DeletePlayerDataResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.DeletePlayerDataResponse{
		ErrorCode:   resp.ErrorCode,
		KeysDeleted: resp.KeysDeleted,
	}, nil
}

// ── Snapshot / Rollback Handlers ───────────────────────────────

func (s *DataServiceServer) CreatePlayerSnapshot(ctx context.Context, req *data_service.CreatePlayerSnapshotRequest) (*data_service.CreatePlayerSnapshotResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.CreatePlayerSnapshotResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.CreatePlayerSnapshot(ctx, s.svcCtx, &logic.CreateSnapshotReq{
		PlayerID:     req.PlayerId,
		SnapshotType: uint32(req.Type),
		Reason:       req.Reason,
		Operator:     req.Operator,
	})
	if err != nil {
		return &data_service.CreatePlayerSnapshotResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.CreatePlayerSnapshotResponse{
		ErrorCode:  resp.ErrorCode,
		SnapshotId: resp.SnapshotID,
		CreatedAt:  resp.CreatedAt,
	}, nil
}

func (s *DataServiceServer) ListPlayerSnapshots(ctx context.Context, req *data_service.ListPlayerSnapshotsRequest) (*data_service.ListPlayerSnapshotsResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.ListPlayerSnapshotsResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.ListPlayerSnapshots(ctx, s.svcCtx, &logic.ListSnapshotsReq{
		PlayerID:   req.PlayerId,
		BeforeTime: req.BeforeTime,
		Limit:      req.Limit,
	})
	if err != nil {
		return &data_service.ListPlayerSnapshotsResponse{ErrorCode: resp.ErrorCode}, nil
	}

	infos := make([]*data_service.SnapshotInfo, 0, len(resp.Snapshots))
	for _, item := range resp.Snapshots {
		infos = append(infos, &data_service.SnapshotInfo{
			SnapshotId:    item.SnapshotID,
			PlayerId:      item.PlayerID,
			ZoneId:        item.ZoneID,
			Type:          data_service.SnapshotType(item.SnapshotType),
			CreatedAt:     item.CreatedAt,
			Reason:        item.Reason,
			Operator:      item.Operator,
			DataSizeBytes: item.DataSizeBytes,
		})
	}
	return &data_service.ListPlayerSnapshotsResponse{
		ErrorCode: resp.ErrorCode,
		Snapshots: infos,
	}, nil
}

func (s *DataServiceServer) GetPlayerSnapshotDiff(ctx context.Context, req *data_service.GetPlayerSnapshotDiffRequest) (*data_service.GetPlayerSnapshotDiffResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.GetPlayerSnapshotDiffResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.GetPlayerSnapshotDiff(ctx, s.svcCtx, &logic.SnapshotDiffReq{
		PlayerID:   req.PlayerId,
		SnapshotID: req.SnapshotId,
		TargetTime: req.TargetTime,
	})
	if err != nil {
		return &data_service.GetPlayerSnapshotDiffResponse{ErrorCode: resp.ErrorCode}, nil
	}

	diffs := make([]*data_service.FieldDiff, 0, len(resp.Diffs))
	for _, d := range resp.Diffs {
		diffs = append(diffs, &data_service.FieldDiff{
			Field:          d.Field,
			SnapshotValue:  d.SnapshotValue,
			CurrentValue:   d.CurrentValue,
			OnlyInSnapshot: d.OnlyInSnapshot,
			OnlyInCurrent:  d.OnlyInCurrent,
		})
	}
	return &data_service.GetPlayerSnapshotDiffResponse{
		ErrorCode:      resp.ErrorCode,
		SnapshotIdUsed: resp.SnapshotIDUsed,
		SnapshotTime:   resp.SnapshotTime,
		Diffs:          diffs,
	}, nil
}

func (s *DataServiceServer) RollbackPlayer(ctx context.Context, req *data_service.RollbackPlayerRequest) (*data_service.RollbackPlayerResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackPlayerResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackPlayer(ctx, s.svcCtx, &logic.RollbackPlayerReq{
		PlayerID:   req.PlayerId,
		SnapshotID: req.SnapshotId,
		TargetTime: req.TargetTime,
		Scope:      uint32(req.Scope),
		Fields:     req.Fields,
		Reason:     req.Reason,
		Operator:   req.Operator,
	})
	if err != nil {
		return &data_service.RollbackPlayerResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackPlayerResponse{
		ErrorCode:             resp.ErrorCode,
		SnapshotIdUsed:        resp.SnapshotIDUsed,
		PreRollbackSnapshotId: resp.PreRollbackSnapshotID,
		FieldsRestored:        resp.FieldsRestored,
	}, nil
}

func (s *DataServiceServer) RollbackZone(ctx context.Context, req *data_service.RollbackZoneRequest) (*data_service.RollbackZoneResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackZoneResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackZone(ctx, s.svcCtx, &logic.RollbackZoneReq{
		ZoneID:     req.ZoneId,
		TargetTime: req.TargetTime,
		Reason:     req.Reason,
		Operator:   req.Operator,
	})
	if err != nil {
		return &data_service.RollbackZoneResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackZoneResponse{
		ErrorCode:       resp.ErrorCode,
		PlayersAffected: resp.PlayersAffected,
		PlayersFailed:   resp.PlayersFailed,
		FailedPlayerIds: resp.FailedPlayerIDs,
		OrphanPlayerIds: resp.OrphanPlayerIDs,
		OrphansCleaned:  resp.OrphansCleaned,
	}, nil
}

func (s *DataServiceServer) RollbackAll(ctx context.Context, req *data_service.RollbackAllRequest) (*data_service.RollbackAllResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackAllResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackAll(ctx, s.svcCtx, &logic.RollbackAllReq{
		TargetTime: req.TargetTime,
		Reason:     req.Reason,
		Operator:   req.Operator,
	})
	if err != nil {
		return &data_service.RollbackAllResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackAllResponse{
		ErrorCode:       resp.ErrorCode,
		ZonesProcessed:  resp.ZonesProcessed,
		PlayersAffected: resp.PlayersAffected,
		PlayersFailed:   resp.PlayersFailed,
	}, nil
}

// ── Batch Recall / Transaction Log Query / Event Snapshot ──────

func (s *DataServiceServer) BatchRecallItems(ctx context.Context, req *data_service.BatchRecallItemsRequest) (*data_service.BatchRecallItemsResponse, error) {
	resp, err := logic.BatchRecallItems(ctx, s.svcCtx, &logic.BatchRecallReq{
		PlayerIDs:    req.PlayerIds,
		ItemConfigID: req.ItemConfigId,
		CurrencyType: req.CurrencyType,
		TimeStart:    req.TimeStart,
		TimeEnd:      req.TimeEnd,
		TxTypes:      req.TxTypes,
		Reason:       req.Reason,
		Operator:     req.Operator,
		DryRun:       req.DryRun,
	})
	if err != nil {
		return &data_service.BatchRecallItemsResponse{ErrorCode: resp.ErrorCode}, nil
	}

	results := make([]*data_service.RecallResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		results = append(results, &data_service.RecallResult{
			PlayerId:     r.PlayerID,
			ItemUuid:     r.ItemUUID,
			ItemConfigId: r.ItemConfigID,
			Amount:       r.Amount,
			Success:      r.Success,
			ErrorDetail:  r.ErrorDetail,
		})
	}
	return &data_service.BatchRecallItemsResponse{
		ErrorCode:     resp.ErrorCode,
		TotalMatched:  resp.TotalMatched,
		TotalRecalled: resp.TotalRecalled,
		TotalFailed:   resp.TotalFailed,
		Results:       results,
	}, nil
}

func (s *DataServiceServer) QueryTransactionLog(ctx context.Context, req *data_service.QueryTransactionLogRequest) (*data_service.QueryTransactionLogResponse, error) {
	resp, err := logic.QueryTransactionLog(ctx, s.svcCtx, &logic.QueryTxLogReq{
		PlayerID:     req.PlayerId,
		TimeStart:    req.TimeStart,
		TimeEnd:      req.TimeEnd,
		TxTypes:      req.TxTypes,
		ItemConfigID: req.ItemConfigId,
		CurrencyType: req.CurrencyType,
		Limit:        req.Limit,
		Offset:       req.Offset,
	})
	if err != nil {
		return &data_service.QueryTransactionLogResponse{ErrorCode: resp.ErrorCode}, nil
	}

	rows := make([]*data_service.TransactionLogRow, 0, len(resp.Rows))
	for _, r := range resp.Rows {
		rows = append(rows, &data_service.TransactionLogRow{
			TxId:          r.TxID,
			Timestamp:     r.Timestamp,
			TxType:        r.TxType,
			FromPlayer:    r.FromPlayer,
			ToPlayer:      r.ToPlayer,
			ItemUuid:      r.ItemUUID,
			ItemConfigId:  r.ItemConfigID,
			ItemQuantity:  r.ItemQuantity,
			CurrencyType:  r.CurrencyType,
			CurrencyDelta: r.CurrencyDelta,
			BalanceBefore: r.BalanceBefore,
			BalanceAfter:  r.BalanceAfter,
			CorrelationId: r.CorrelationID,
			Extra:         r.Extra,
		})
	}
	return &data_service.QueryTransactionLogResponse{
		ErrorCode:  resp.ErrorCode,
		Rows:       rows,
		TotalCount: resp.TotalCount,
	}, nil
}

func (s *DataServiceServer) CreateEventSnapshot(ctx context.Context, req *data_service.CreateEventSnapshotRequest) (*data_service.CreateEventSnapshotResponse, error) {
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.CreateEventSnapshotResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.CreateEventSnapshot(ctx, s.svcCtx, &logic.CreateEventSnapshotReq{
		PlayerID:    req.PlayerId,
		EventType:   uint32(req.EventType),
		EventDetail: req.EventDetail,
		Operator:    req.Operator,
	})
	if err != nil {
		return &data_service.CreateEventSnapshotResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.CreateEventSnapshotResponse{
		ErrorCode:  resp.ErrorCode,
		SnapshotId: resp.SnapshotID,
		CreatedAt:  resp.CreatedAt,
	}, nil
}

// ── Id Segment (Leaf-segment 号段发号) ──────────────────────────

func (s *DataServiceServer) AllocateIdSegment(ctx context.Context, req *data_service.AllocateIdSegmentRequest) (*data_service.AllocateIdSegmentResponse, error) {
	resp, err := logic.AllocateIdSegment(ctx, s.svcCtx, &logic.AllocateIdSegmentReq{
		BizTag: req.GetBizTag(),
		Step:   req.GetStep(),
	})
	if err != nil {
		return &data_service.AllocateIdSegmentResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.AllocateIdSegmentResponse{
		ErrorCode: resp.ErrorCode,
		Lo:        resp.Lo,
		Hi:        resp.Hi,
	}, nil
}
