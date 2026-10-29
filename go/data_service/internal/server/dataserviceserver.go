package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"data_service/internal/constants"
	"data_service/internal/guildcheck"
	"data_service/internal/logic"
	"data_service/internal/metrics"
	"data_service/internal/routing"
	"data_service/internal/store"
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
	// 归属读与账号目录的直方图在第一次 scrape 之前就要有全部序列(见 metrics.PrimeLoginReads);可重复调用。
	metrics.PrimeLoginReads()
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
//   - mapping Redis 故障        → Unavailable + ErrCodeRedis(闸门查询失败也走这里:fail-closed)
//
// storage_id != 0 时与映射同一段 Lua 原子钉落点记录(player-storage-placement.md §8.1);
// 上面四种结果的语义不变 —— 只有映射这次真的写成才会写落点,幂等 / 冲突 / 被围栏拒绝都不碰它。
//
// RouteStore.Mode=db_first(docs/design/player-route-mapping-durability-proposal.md §4)时上面四条照旧
// (合服围栏另有两条路径:库内分片、发布脚本的提交点复核,都是 FailedPrecondition + 22),另多两种 Unavailable,
// **gRPC code 的集合不变**(OK / AlreadyExists / FailedPrecondition / Unavailable),新码只体现在 message 里:
//   - 库这一段故障(store 没装配、SQL / 锁等待、route_meta 缺失或冻结、缓存认得玩家而库里没行)→ Unavailable + ErrCodeRouteStoreError
//   - player_id 为 0 或 home_zone 越界 → Unavailable + ErrCodeInvalidRequest
//
// 已提交但发布失败 / 未确认落盘、发布脚本发现缓存里是另一份映射,都是 Unavailable + ErrCodeRedis:
// login 照常 fail-closed 并释放名字,重试落到幂等分支补发。login 只看有没有 err。
func (s *DataServiceServer) RegisterPlayerZone(ctx context.Context, req *data_service.RegisterPlayerZoneRequest) (*emptypb.Empty, error) {
	err := s.svcCtx.Router.RegisterPlayerZone(ctx, req.GetPlayerId(), req.GetHomeZoneId(), req.GetStorageId())
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
	case errors.Is(err, routing.ErrInvalidRegistration):
		// 只有 db_first 可达。刻意不用 InvalidArgument:gRPC code 集合保持与 redis 模式相同(规格 g.2 第 14 条),靠码 14 区分。
		logx.Errorf("[RegisterPlayerZone] refused invalid registration: player=%d zone=%d: %v", req.PlayerId, req.HomeZoneId, err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeInvalidRequest, err)
	case errors.Is(err, routing.ErrRouteStore):
		logx.Errorf("[RegisterPlayerZone] route store failure: player=%d zone=%d: %v", req.PlayerId, req.HomeZoneId, err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
	default:
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
}

// GetPlayerHomeZone 的线上契约(proto 注释不归本服务改,以此处为准):
//   - 映射存在           → HomeZoneId=归属 zone, HomeZoneMerging=该 zone 是否在合服围栏内, err=nil
//   - 映射不存在         → codes.NotFound,消息含 "no home zone mapping"
//   - mapping Redis 故障 → codes.Unavailable
//
// HomeZoneMerging 与 HomeZoneId 同一段 Lua 读出(见 routing.GetPlayerHomeZoneAndMergeFence);
// 围栏读失败按 true 返回(fail-closed)。scene_manager 据此在任何写之前拒绝进场(§8.2 / A16);
// 旧版 data_service 不填该字段,调用方读到 false,行为与今天一致。
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
//
// RouteStore.ReadRequiresReady=true(第 3 批读方门禁,docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.2)
// 时多一种失败,**gRPC code 的集合不变**:缓存不可信、回源全局库又失败 → Unavailable + ErrCodeRouteStoreError(码 30,与
// mapping Redis 的码 1 分开,值班一眼分清是全局库坏了)。库里没有这名玩家 → 照旧 NotFound(那是库的权威缺席)。
// 判定顺序是先 ErrRouteReadUnavailable 再 NotFound:一次库故障绝不能被报成「这名玩家没有归属」。
// 再之前先看调用方是否已放弃(ctx 取消 / 超时,M1-2,2026-10-21):如实回它的取消原因(Canceled / DeadlineExceeded),
// 不报成 Unavailable + 码 1 / 30 —— 那是本服务或全局库的故障口径,调用方自己的截止时间不该触发它(同 logic/persisted_player_database.go)。
//
// 每次调用记 data_service_home_zone_read_seconds{rpc="home"},结局按返回的 gRPC code 归类(homeZoneReadResult)。
func (s *DataServiceServer) GetPlayerHomeZone(ctx context.Context, req *data_service.GetPlayerHomeZoneRequest) (_ *data_service.GetPlayerHomeZoneResponse, err error) {
	defer observeHomeZoneRead(metrics.HomeZoneRPCHome, time.Now(), &err)
	zoneID, merging, err := s.svcCtx.Router.GetPlayerHomeZoneAndMergeFence(ctx, req.GetPlayerId())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		if errors.Is(err, routing.ErrRouteReadUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
		}
		if errors.Is(err, routing.ErrHomeZoneNotMapped) {
			// err.Error() 形如 "no home zone mapping for player 42";文案是契约,见上。
			return nil, status.Errorf(codes.NotFound, "error_code=%d: %v", constants.ErrCodeNotFound, err)
		}
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
	return &data_service.GetPlayerHomeZoneResponse{HomeZoneId: zoneID, HomeZoneMerging: merging}, nil
}

// BatchGetPlayerHomeZone 与单查刻意不同:**缺席的 id 直接不出现在 map 里**,不是错误。
// 批量查的调用方(login 的角色列表)天然要处理"部分有部分没有",给整批返回 NotFound
// 会把一个正常状态升级成整批失败;单查只有一个 id,没有"部分"可言,所以那边用 NotFound。
// 值畸形与单查同口径,整批回 Unavailable(2026-10-12 起;此前畸形值被当成缺席静默跳过)。
// ReadRequiresReady=true 时缓存不可信的块回源全局库,任一块回源失败整批 Unavailable + ErrCodeRouteStoreError(码 30),
// 不回半份结果(半份会被调用方当成「这些人没有归属」)。调用方已放弃(ctx 取消 / 超时)时同单查,回它的取消原因(M1-2)。
//
// 每次调用记 data_service_home_zone_read_seconds{rpc="batch"}(结局同单查归类,批量查不会有 not_found)与本批 id 个数。
func (s *DataServiceServer) BatchGetPlayerHomeZone(ctx context.Context, req *data_service.BatchGetPlayerHomeZoneRequest) (_ *data_service.BatchGetPlayerHomeZoneResponse, err error) {
	metrics.ObserveHomeZoneBatchIDs(len(req.GetPlayerIds()))
	defer observeHomeZoneRead(metrics.HomeZoneRPCBatch, time.Now(), &err)
	mapping, err := s.svcCtx.Router.BatchGetPlayerHomeZone(ctx, req.PlayerIds)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		if errors.Is(err, routing.ErrRouteReadUnavailable) {
			return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
		}
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRedis, err)
	}
	return &data_service.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: mapping}, nil
}

// observeHomeZoneRead 在两个归属读 handler 的 defer 里记耗时与结局;start 在 defer 语句处求值,即 handler 入口,
// 所以每一条出口(含提前返回的失败分支)都被计入。
func observeHomeZoneRead(rpc string, start time.Time, err *error) {
	metrics.ObserveHomeZoneRead(rpc, homeZoneReadResult(*err), time.Since(start))
}

// homeZoneReadResult 按 handler 实际回给调用方的 gRPC code 归类,与上面两段契约同口径:只有映射的权威缺席(NotFound)
// 算 not_found;mapping Redis / 全局库故障(Unavailable)与调用方取消 / 超时(Canceled / DeadlineExceeded)都是 error ——
// 后者同样是一次没拿到归属的读,压测里要看见它的尾延迟。
func homeZoneReadResult(err error) string {
	switch status.Code(err) {
	case codes.OK:
		return metrics.HomeZoneReadResultOK
	case codes.NotFound:
		return metrics.HomeZoneReadResultNotFound
	default:
		return metrics.HomeZoneReadResultError
	}
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

// RemapHomeZoneForMerge **已停用**(2026-10-12,docs/design/player-route-mapping-durability-proposal.md §4.3 的 W2):
// 一律回 FailedPrecondition + ErrCodeNotImplemented,零变更。改映射只走 tools/merge_zone(按 CAS 改写、
// 第 2 批起库先写);Router 上对应的实现已删除。
//
// 停用的理由:它对扫到的 player:zone:* 做无条件 SET(tools/merge_zone/merge_run.go 的 CAS 改写注释写了危害),
// 且只写 Redis、绕过 player_route 真源 —— db_first 之后它改出来的值库里不知道,下一次发布会被判成冲突或被覆盖回去。
// 仓库内没有调用方(规格 g.2 第 12 条)。
//
// 拒绝发生在**鉴权之前**:不再有"带对 token 就能执行"的路径,也就不该让调用方以为换个 token 能过。
// 每次被调都留 ERROR 日志并带调用方身份:还在调它的只可能是忘了升级的运维脚本,必须让人看见。
func (s *DataServiceServer) RemapHomeZoneForMerge(ctx context.Context, req *data_service.RemapHomeZoneForMergeRequest) (*data_service.RemapHomeZoneForMergeResponse, error) {
	const rpcName = "RemapHomeZoneForMerge"
	logx.Errorf("[admin] %s refused: the RPC is retired (proposal §4.3); remap mappings with tools/merge_zone. source=%d target=%d dry_run=%t %s",
		rpcName, req.GetSourceZoneId(), req.GetTargetZoneId(), req.GetDryRun(), callerIdentity(ctx))
	return nil, status.Errorf(codes.FailedPrecondition,
		"error_code=%d: %s is retired since 2026-10-12 (player-route-mapping-durability-proposal.md §4.3); remap mappings with tools/merge_zone",
		constants.ErrCodeNotImplemented, rpcName)
}

// DeletePlayerData 删玩家数据键。失败一律走响应体的 in-band error_code,**唯一的例外**是
// delete_zone_mapping=true:映射删除分支已停用(logic.ErrZoneMappingDeletionDisabled,proposal §4.3 的 W3),
// 回 gRPC FailedPrecondition + ErrCodeNotImplemented,且在任何副作用之前拒绝(数据键与映射都不动)。
// 走 status 而不是 in-band 码,是为了让还带着这个开关的调用方当场失败,而不是看着一个"错误码非 0 的响应"继续往下走。
func (s *DataServiceServer) DeletePlayerData(ctx context.Context, req *data_service.DeletePlayerDataRequest) (*data_service.DeletePlayerDataResponse, error) {
	resp, err := logic.DeletePlayerData(ctx, s.svcCtx, &logic.DeletePlayerDataReq{
		PlayerID:          req.PlayerId,
		DeleteZoneMapping: req.DeleteZoneMapping,
	})
	if errors.Is(err, logic.ErrZoneMappingDeletionDisabled) {
		logx.Errorf("[DeletePlayerData] refused delete_zone_mapping=true: player=%d %s", req.GetPlayerId(), callerIdentity(ctx))
		return nil, status.Errorf(codes.FailedPrecondition, "error_code=%d: %v", constants.ErrCodeNotImplemented, err)
	}
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

// ── 三个 Rollback*:一律要求 x-admin-token(docs/design/guild-phase2/07-rollback-fail-closed.md §7.6.3 Q2 = ②)──
//
// 为什么不是"只有 accept_guild_divergence=true 才要 token":三个 handler 原先零鉴权、operator 是自报字符串,
// 任何能拨到 data_service 的进程发一次不带放行的 Rollback* 就能 (i) 从拒绝响应里读到目标玩家的资产流水样本,
// (ii) 让目标玩家 / 整个 zone 被栅栏挡住登录最长"沉降 30s + 检查预算"。一律要 token 两条都没有了,
// 也就不需要"未鉴权只回计数"的分支。口径同 RemapHomeZoneForMerge:没配 AdminToken = 回档 RPC 整体停用。
// 鉴权在任何 logic(栅栏、审计、快照读)之前;拒绝走 gRPC status(PermissionDenied + ErrCodeAdminAuthRequired)。
//
// 响应:logic 对规则拒绝(RollbackGuildDivergence)与写后分歧(RollbackGuildDivergedAfterWrite)不返回 err,
// 分歧计数与前 20 条样本才带得出去;返回 err 的是故障,只回 error_code(原样)。

func (s *DataServiceServer) RollbackPlayer(ctx context.Context, req *data_service.RollbackPlayerRequest) (*data_service.RollbackPlayerResponse, error) {
	const rpcName = "RollbackPlayer"
	if err := s.authorizeAdmin(ctx, rpcName); err != nil {
		return nil, err
	}
	caller := callerIdentity(ctx)
	logx.Infof("[admin] %s authorized: player_id=%d snapshot_id=%d target_time=%d scope=%d accept_guild_divergence=%t operator=%q %s",
		rpcName, req.GetPlayerId(), req.GetSnapshotId(), req.GetTargetTime(), req.GetScope(), req.GetAcceptGuildDivergence(), req.GetOperator(), caller)
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackPlayerResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackPlayer(ctx, s.svcCtx, &logic.RollbackPlayerReq{
		PlayerID:              req.PlayerId,
		SnapshotID:            req.SnapshotId,
		TargetTime:            req.TargetTime,
		Scope:                 uint32(req.Scope),
		Fields:                req.Fields,
		Reason:                req.Reason,
		Operator:              req.Operator,
		AcceptGuildDivergence: req.GetAcceptGuildDivergence(),
		Caller:                caller,
	})
	if err != nil {
		return &data_service.RollbackPlayerResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackPlayerResponse{
		ErrorCode:                  resp.ErrorCode,
		SnapshotIdUsed:             resp.SnapshotIDUsed,
		PreRollbackSnapshotId:      resp.PreRollbackSnapshotID,
		FieldsRestored:             resp.FieldsRestored,
		GuildDivergenceCount:       resp.GuildDivergenceCount,
		GuildDivergences:           toProtoGuildDivergences(resp.GuildDivergences),
		GuildUnprovablePlayerCount: resp.GuildUnprovablePlayerCount,
	}, nil
}

func (s *DataServiceServer) RollbackZone(ctx context.Context, req *data_service.RollbackZoneRequest) (*data_service.RollbackZoneResponse, error) {
	const rpcName = "RollbackZone"
	if err := s.authorizeAdmin(ctx, rpcName); err != nil {
		return nil, err
	}
	caller := callerIdentity(ctx)
	logx.Infof("[admin] %s authorized: zone=%d target_time=%d accept_guild_divergence=%t operator=%q %s",
		rpcName, req.GetZoneId(), req.GetTargetTime(), req.GetAcceptGuildDivergence(), req.GetOperator(), caller)
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackZoneResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackZone(ctx, s.svcCtx, &logic.RollbackZoneReq{
		ZoneID:                req.ZoneId,
		TargetTime:            req.TargetTime,
		Reason:                req.Reason,
		Operator:              req.Operator,
		AcceptGuildDivergence: req.GetAcceptGuildDivergence(),
		Caller:                caller,
	})
	if err != nil {
		return &data_service.RollbackZoneResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackZoneResponse{
		ErrorCode:                  resp.ErrorCode,
		PlayersAffected:            resp.PlayersAffected,
		PlayersFailed:              resp.PlayersFailed,
		FailedPlayerIds:            resp.FailedPlayerIDs,
		OrphanPlayerIds:            resp.OrphanPlayerIDs,
		OrphansCleaned:             resp.OrphansCleaned,
		GuildDivergenceCount:       resp.GuildDivergenceCount,
		GuildDivergences:           toProtoGuildDivergences(resp.GuildDivergences),
		GuildUnprovablePlayerCount: resp.GuildUnprovablePlayerCount,
	}, nil
}

func (s *DataServiceServer) RollbackAll(ctx context.Context, req *data_service.RollbackAllRequest) (*data_service.RollbackAllResponse, error) {
	const rpcName = "RollbackAll"
	if err := s.authorizeAdmin(ctx, rpcName); err != nil {
		return nil, err
	}
	caller := callerIdentity(ctx)
	logx.Infof("[admin] %s authorized: target_time=%d accept_guild_divergence=%t operator=%q %s",
		rpcName, req.GetTargetTime(), req.GetAcceptGuildDivergence(), req.GetOperator(), caller)
	if s.svcCtx.SnapshotStore == nil {
		return &data_service.RollbackAllResponse{ErrorCode: constants.ErrCodeSnapshotDBError}, nil
	}
	resp, err := logic.RollbackAll(ctx, s.svcCtx, &logic.RollbackAllReq{
		TargetTime:            req.TargetTime,
		Reason:                req.Reason,
		Operator:              req.Operator,
		AcceptGuildDivergence: req.GetAcceptGuildDivergence(),
		Caller:                caller,
	})
	if err != nil {
		return &data_service.RollbackAllResponse{ErrorCode: resp.ErrorCode}, nil
	}
	return &data_service.RollbackAllResponse{
		ErrorCode:                  resp.ErrorCode,
		ZonesProcessed:             resp.ZonesProcessed,
		PlayersAffected:            resp.PlayersAffected,
		PlayersFailed:              resp.PlayersFailed,
		GuildDivergenceCount:       resp.GuildDivergenceCount,
		GuildDivergences:           toProtoGuildDivergences(resp.GuildDivergences),
		GuildUnprovablePlayerCount: resp.GuildUnprovablePlayerCount,
	}, nil
}

// toProtoGuildDivergences 把帮会闸的分歧样本搬进 proto(logic 已截到前 20 条)。
func toProtoGuildDivergences(rows []guildcheck.GuildDivergence) []*data_service.RollbackGuildDivergence {
	if len(rows) == 0 {
		return nil
	}
	out := make([]*data_service.RollbackGuildDivergence, 0, len(rows))
	for _, d := range rows {
		out = append(out, &data_service.RollbackGuildDivergence{
			OpId:              d.OpID,
			PlayerId:          d.PlayerID,
			GuildId:           d.GuildID,
			Kind:              d.Kind,
			Status:            d.Status,
			FundsDelta:        d.FundsDelta,
			ContributionDelta: d.ContributionDelta,
			UpdatedMs:         d.UpdatedMs,
		})
	}
	return out
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

// ── Player name registry(全服唯一名字,设计 docs/design/guild-phase2/03-names.md §3.1)──
//
// 三个 handler 都是**薄的**:编排在 logic,判重在 store 的唯一键,规则在 shared/playername。
// 这一层只做两件 logic 不该管的事 —— 读 gRPC metadata 判定是不是运维调用,
// 以及把 logic 的错误哨兵翻成 gRPC status + error_code。
//
// 【可达面】这三个 RPC **只在服务间调用**(login 建角、guild 查成员名),不进客户端
// 可达面:不改 MessageLimiter、不改 gate 路由、不进 session.ClientMethods(§3.18)。
// 也正因为没有网关那层过滤,Release 的时间窗是唯一挡住"内网任一进程都能释放在役角色
// 名字"的东西(见 config.PlayerNameConfig.ReleaseWindow)。

// ReservePlayerName 登记名字。响应里的 result 是**业务结果不是错误**:
// 名字不合规(2)与被别人占用(1)都返回 OK + result,因为它们是 login 要拿去翻成
// tip 文案的正常分支;只有"库不可用""这个 id 已有别名"这类真故障才走 gRPC error。
//
// owner_player_id 只在 result=1 时填,且**只给 login 判重试**(§3.1):login 拿它对照
// "占用者是不是本账号刚建的那个号"。login 绝不能把它下发客户端 —— 那等于开放一个
// 按名字查 player_id 的接口。
func (s *DataServiceServer) ReservePlayerName(ctx context.Context, req *data_service.ReservePlayerNameRequest) (*data_service.ReservePlayerNameResponse, error) {
	result, owner, err := logic.ReservePlayerName(ctx, s.svcCtx, req.GetPlayerId(), req.GetName())
	if err != nil {
		return nil, playerNameStatus(ctx, "ReservePlayerName", req.GetPlayerId(), err)
	}
	return &data_service.ReservePlayerNameResponse{Result: result, OwnerPlayerId: owner}, nil
}

// ReleasePlayerName 条件释放:player_id 与归一化后的 name 都对上才删。
//
// 两条调用路径,靠 metadata 里有没有 x-admin-token 区分:
//   - login 的建角补偿(没有 token):只能删 PlayerName.ReleaseWindow 之内的登记。
//     窗外 → FailedPrecondition + ErrCodeAdminAuthRequired,提示去走运维路径。
//   - 运维清孤儿(带 token):不限登记时间。
//
// 【为什么"带了键就必须验过"而不是"验不过就降级成非 admin"】降级会把一次**鉴权失败**
// 变成一次静默的普通释放:调用方以为自己在用运维权限清一条老记录,实际删掉的可能是
// 窗口内刚建的号,而日志里没有任何"token 不对"的痕迹。带错 token 是配置事故,必须报错。
func (s *DataServiceServer) ReleasePlayerName(ctx context.Context, req *data_service.ReleasePlayerNameRequest) (*emptypb.Empty, error) {
	const rpcName = "ReleasePlayerName"

	admin := false
	if hasAdminTokenMetadata(ctx) {
		if err := s.authorizeAdmin(ctx, rpcName); err != nil {
			return nil, err
		}
		admin = true
		// 运维路径每次都留痕:这是唯一能删掉任意年龄登记的入口。
		logx.Infof("[admin] %s authorized: player_id=%d %s", rpcName, req.GetPlayerId(), callerIdentity(ctx))
	}

	if err := logic.ReleasePlayerName(ctx, s.svcCtx, req.GetPlayerId(), req.GetName(), admin); err != nil {
		return nil, playerNameStatus(ctx, rpcName, req.GetPlayerId(), err)
	}
	return &emptypb.Empty{}, nil
}

// BatchGetPlayerName 批量查展示名。口径与 BatchGetPlayerHomeZone 一致:
// **缺席的 id 直接不出现在 map 里,不是错误**(早于本功能建的角色、已释放的名字都属于
// 这一类,调用方按空名展示)。但"没查成"必须是错误:半份结果会被上游当成"这些人确实
// 没名字"缓存并展示出去,而那是错的。
func (s *DataServiceServer) BatchGetPlayerName(ctx context.Context, req *data_service.BatchGetPlayerNameRequest) (*data_service.BatchGetPlayerNameResponse, error) {
	names, err := logic.BatchGetPlayerName(ctx, s.svcCtx, req.GetPlayerIds())
	if err != nil {
		// playerID 传 0:这是批量接口,日志里放单个 id 没有意义(id 清单可能有几百个,
		// 而 Prometheus label 更不允许放)。
		return nil, playerNameStatus(ctx, "BatchGetPlayerName", 0, err)
	}
	return &data_service.BatchGetPlayerNameResponse{Names: names}, nil
}

// GetPlayerAssetOpLedger 读玩家**已落盘**的资产账本(docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.2),
// 供 guild 重投循环对长期离线的玩家提前终结已记账的行(go/shared/assetop.DataServiceLedger)。
//
// 只读、不鉴权,与 BatchGetPlayerName 同级:账本是 seq 位图 + 少量拒绝码,不含资产数额。
// 本 RPC 没有 in-band error_code,契约就是 gRPC code;语义表与 code 全在 logic 里一处定义,
// logic 返回的 error 已经是 status,这里只做搬运 —— 不要在这里再包一层映射。
func (s *DataServiceServer) GetPlayerAssetOpLedger(ctx context.Context, req *data_service.GetPlayerAssetOpLedgerRequest) (*data_service.GetPlayerAssetOpLedgerResponse, error) {
	// 副本核验只对 Redis blob 读有意义;SQL 证据分支不核验,两者同时要求是调用方写错了请求,不猜它要哪一份。
	if req.GetRequireSqlCommit() && req.GetVerifyRedisCopy() {
		return nil, status.Error(codes.InvalidArgument, "GetPlayerAssetOpLedger: verify_redis_copy applies only to the Redis blob read, not with require_sql_commit")
	}
	// 资产自动放弃的闸(docs/design/zone-redis-regression-guard-20261022.md §4.5):blob 与实例身份、身份戳同一段 Lua 读出。
	if req.GetVerifyRedisCopy() {
		ledger, found, verified, err := logic.GetVerifiedPlayerAssetOpLedger(ctx, s.svcCtx, req.GetPlayerId())
		if err != nil {
			return nil, err
		}
		return &data_service.GetPlayerAssetOpLedgerResponse{Found: found, Ledger: ledger, RedisCopyVerified: verified}, nil
	}
	if req.GetRequireSqlCommit() {
		ledger, found, err := logic.GetSQLCommittedAssetLedger(ctx, s.svcCtx, req.GetPlayerId())
		if err != nil {
			return nil, err
		}
		return &data_service.GetPlayerAssetOpLedgerResponse{Found: found, Ledger: ledger, SqlCommitted: true}, nil
	}
	ledger, found, err := logic.GetPlayerAssetOpLedger(ctx, s.svcCtx, req.GetPlayerId())
	if err != nil {
		return nil, err
	}
	return &data_service.GetPlayerAssetOpLedgerResponse{Found: found, Ledger: ledger}, nil
}

// GetPlayerInboxLedger 读玩家**已落盘**的收件箱账本(docs/design/player-inbox-service.md §5.1、§9.3),
// 供 go/player_inbox 的对账(定时作业与运维 ReconcilePlayer)比对 decided_through_seq。
//
// 只读、不鉴权,与 GetPlayerAssetOpLedger 同级:账本只有 seq 水位与最近 32 条结局(seq / outcome / tip id /
// request_id / msg_type),不含载荷与资产数额。读法、错误语义与形状都照 GetPlayerAssetOpLedger:
// 没有 in-band error_code,契约就是 gRPC code;logic 返回的 error 已经是 status,这里只做搬运。
func (s *DataServiceServer) GetPlayerInboxLedger(ctx context.Context, req *data_service.GetPlayerInboxLedgerRequest) (*data_service.GetPlayerInboxLedgerResponse, error) {
	ledger, found, err := logic.GetPlayerInboxLedger(ctx, s.svcCtx, req.GetPlayerId())
	if err != nil {
		return nil, err
	}
	return &data_service.GetPlayerInboxLedgerResponse{Found: found, Ledger: ledger}, nil
}

// hasAdminTokenMetadata 只判"这次调用**声称**自己是运维调用",不做任何校验。
// 校验一律交给 authorizeAdmin(常数时间比较 + 未配置即停用)。
//
// 键存在但值为空串也算"声称"(md.Get 返回 [""],长度 1):否则一个把 token 配成空串的
// 脚本会悄悄走进非 admin 路径,得到一个"成功但其实什么都没删"的响应。
func hasAdminTokenMetadata(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	return len(md.Get(adminTokenMetadataKey)) > 0
}

// playerNameStatus 把 logic / store 的错误哨兵翻成 gRPC status,映射表见 §3.1。
//
// 码与 codes 的对应必须**穷举**:default 分支一律当成"库炸了"(Unavailable +
// ErrCodePlayerNameDBError)。方向是宁可把一个新错误误报成故障,也不能把它漏成
// InvalidArgument —— 后者会让 login 把一次基础设施故障当成"玩家名字有问题",
// 给玩家弹一个永远改不对的提示。
func playerNameStatus(ctx context.Context, rpcName string, playerID uint64, err error) error {
	switch {
	case errors.Is(err, store.ErrPlayerNameInvalidArgument), errors.Is(err, store.ErrPlayerNameBatchTooLarge):
		// 调用方参数错(player_id=0、名字归一化后为空、一次查超过 PlayerNameBatchLimit 个)。
		// 不记日志:这条路径是调用方 bug,错误已经回给它了,记了只会在被刷时淹掉真故障。
		return status.Errorf(codes.InvalidArgument, "error_code=%d: %v", constants.ErrCodeInvalidRequest, err)

	case errors.Is(err, logic.ErrPlayerNameConflict):
		// 这个 player_id 名下已经有**另一个**名字 = 发号器把一个在役角色的 id 又发了一次。
		// 这是 ID 安全事件,不是普通业务失败,必须有人看:留 ERROR 并带上调用方身份。
		logx.Errorf("[player-name] %s conflict: player_id=%d already holds a different name (id reuse — check id_segment watermark) %s: %v",
			rpcName, playerID, callerIdentity(ctx), err)
		return status.Errorf(codes.FailedPrecondition, "error_code=%d: %v", constants.ErrCodePlayerNameConflict, err)

	case errors.Is(err, logic.ErrPlayerNameReleaseOutsideWindow):
		// 设计写的是 WARN;go-zero 的 logx 没有 WARN 级别(只有 debug/info/error/severe),
		// 这里取 ERROR:被拒的释放要么是有人拿错了接口,要么是运维忘了带 token,
		// 两种都需要有人回头看一眼,淹在 Info 里等于没有。
		logx.Errorf("[player-name] %s refused: player_id=%d registered outside PlayerName.ReleaseWindow and no valid %s was presented "+
			"(use the admin path to clean up orphans) %s",
			rpcName, playerID, adminTokenMetadataKey, callerIdentity(ctx))
		return status.Errorf(codes.FailedPrecondition, "error_code=%d: %v", constants.ErrCodeAdminAuthRequired, err)

	case errors.Is(err, logic.ErrPlayerNameStoreUnavailable):
		// store 没装配起来(建表失败 / 连不上全局库 / PlayerName 配置非法)。
		// 启动期已经有一条带原因的 ERROR,这里不再重复刷屏,只把码翻出去。
		return status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodePlayerNameDBError, err)

	default:
		logx.Errorf("[player-name] %s failed: player_id=%d %s: %v", rpcName, playerID, callerIdentity(ctx), err)
		return status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodePlayerNameDBError, err)
	}
}
