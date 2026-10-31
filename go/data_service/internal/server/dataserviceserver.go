package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"data_service/internal/constants"
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

// RegisterPlayerZone 登记 player → home_zone 映射,绝不覆盖(见 routing.Router.RegisterPlayerZone)。
//
// 本 RPC 的响应体是 emptypb.Empty,没有 error_code 字段可放码,所以失败一律走 gRPC
// status;码用 constants 里的常量拼进 message(`error_code=<n>`),运维按同一根轴排障。
//
//   - 映射不存在 / 已存在且相同    → OK(幂等)
//   - 已存在且不同                 → AlreadyExists + ErrCodeZoneMappingConflict,消息带既有 zone
//   - player_id 或 storage_id 为 0、home 越界 → Unavailable + ErrCodeInvalidRequest
//   - 库这一段故障(store 没装配、SQL / 锁等待、route_meta 缺失或冻结)→ Unavailable + ErrCodeRouteStoreError
//
// 提交点是全局库 player_route 的事务,提交即完成。login 失败时照常 fail-closed 并释放名字,重试落到幂等分支。
//
// 不看合服状态(docs/design/player-data-tidb-scaling-plan.md §7.1):合服过程中在源区建号是允许的,
// 他的 home_zone 就是源区,GetPlayerHomeZone 解析之后落到目标区。这里写入的永远是原始 home_zone。
//
// storage_id 与映射在同一行里原子钉落点(player-storage-placement.md §8.1);
// 只有映射这次真的写成才会写落点,幂等 / 冲突都不碰它。
func (s *DataServiceServer) RegisterPlayerZone(ctx context.Context, req *data_service.RegisterPlayerZoneRequest) (*emptypb.Empty, error) {
	err := s.svcCtx.Router.RegisterPlayerZone(ctx, req.GetPlayerId(), req.GetHomeZoneId(), req.GetStorageId())
	if err == nil {
		return &emptypb.Empty{}, nil
	}

	var conflict *routing.HomeZoneConflictError
	switch {
	case errors.As(err, &conflict):
		// 有人试图把一名已有归属的玩家写到另一个 zone,必须留下证据。
		logx.Errorf("[RegisterPlayerZone] refused overwrite: player=%d existing_zone=%d requested_zone=%d",
			conflict.PlayerID, conflict.Existing, conflict.Requested)
		return nil, status.Errorf(codes.AlreadyExists, "error_code=%d: %s",
			constants.ErrCodeZoneMappingConflict, conflict.Error())
	case errors.Is(err, routing.ErrInvalidRegistration):
		// 刻意不用 InvalidArgument:login 只看有没有 err,靠码 14 区分。
		logx.Errorf("[RegisterPlayerZone] refused invalid registration: player=%d zone=%d: %v", req.PlayerId, req.HomeZoneId, err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeInvalidRequest, err)
	default:
		logx.Errorf("[RegisterPlayerZone] route store failure: player=%d zone=%d: %v", req.PlayerId, req.HomeZoneId, err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
	}
}

// GetPlayerHomeZone 的线上契约(proto 注释不归本服务改,以此处为准):
//   - 映射存在           → HomeZoneId=**已解析到最终区**的归属 zone, err=nil
//   - 映射不存在         → codes.NotFound,消息含 "no home zone mapping"
//   - 全局库故障         → codes.Unavailable + ErrCodeRouteStoreError(绝不 NotFound)
//   - 合服映射缓存没装配 → codes.Unavailable + ErrCodeRouteStoreError(fail-closed)
//   - 调用方已放弃       → 如实回它的取消原因(Canceled / DeadlineExceeded),不报成 Unavailable + 码 30:
//     那是全局库的故障口径,调用方自己的截止时间不该触发它(同 logic/persisted_player_database.go)
//
// 为什么在出口解析(docs/design/player-data-tidb-scaling-plan.md §7.1):合服只写 zone_merged_into,玩家行和路由行都不动,
// home_zone 还是源区;而按区分组的调用方(榜单、频道、市场、公会、进场判断)几乎都经这两个 RPC 取归属,要的是最终区。
// 要原始 home_zone 的只有登记写入和路由工具,它们都不走这里;go/db 选库读 storage_id,不看 home_zone。
//
// 为什么「不存在」要用一个**专门的码**而不是 HomeZoneId=0 + nil:调用方必须能把
// 「这名玩家确实没有映射」和「我没查到,因为库挂了」分开。前者是数据状态,正确处理是回退到
// 账号 blob 里的建角 zone;后者是故障,回退同样发生但必须被告警看见。用同一个
// (0, nil) 表示两者,等于让每一次库故障都伪装成一批"老玩家"。
//
// login 侧(go/login/internal/logic/pkg/homezone.IsUnmapped)同时认 NotFound 与文案;
// 所以 NotFound 的消息必须继续包含 "no home zone mapping" 这段文本,不能改写。
//
// 每次调用记 data_service_home_zone_read_seconds{rpc="home"},结局按返回的 gRPC code 归类(homeZoneReadResult)。
func (s *DataServiceServer) GetPlayerHomeZone(ctx context.Context, req *data_service.GetPlayerHomeZoneRequest) (_ *data_service.GetPlayerHomeZoneResponse, err error) {
	defer observeHomeZoneRead(metrics.HomeZoneRPCHome, time.Now(), &err)
	home, err := s.svcCtx.Router.GetPlayerHomeZone(ctx, req.GetPlayerId())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		if errors.Is(err, routing.ErrHomeZoneNotMapped) {
			// err.Error() 形如 "no home zone mapping for player 42";文案是契约,见上。
			return nil, status.Errorf(codes.NotFound, "error_code=%d: %v", constants.ErrCodeNotFound, err)
		}
		logx.Errorf("[DataRoute] GetPlayerHomeZone player=%d: %v", req.GetPlayerId(), err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
	}
	zones := s.svcCtx.MergedZones.Snapshot()
	if zones == nil {
		logx.Errorf("[DataRoute] GetPlayerHomeZone player=%d: merged zone cache not assembled", req.GetPlayerId())
		return nil, errMergedZonesUnavailable()
	}
	return &data_service.GetPlayerHomeZoneResponse{HomeZoneId: zones.Final(home)}, nil
}

// BatchGetPlayerHomeZone 与单查刻意不同:**缺席的 id 直接不出现在 map 里**,不是错误。
// 批量查的调用方(login 的角色列表)天然要处理"部分有部分没有",给整批返回 NotFound
// 会把一个正常状态升级成整批失败;单查只有一个 id,没有"部分"可言,所以那边用 NotFound。
// 任一块读库失败整批 Unavailable + ErrCodeRouteStoreError,不回半份结果(半份会被调用方当成「这些人没有归属」)。
// 调用方已放弃(ctx 取消 / 超时)时同单查,回它的取消原因。结果同单查解析到最终区。
//
// 每次调用记 data_service_home_zone_read_seconds{rpc="batch"}(结局同单查归类,批量查不会有 not_found)与本批 id 个数。
func (s *DataServiceServer) BatchGetPlayerHomeZone(ctx context.Context, req *data_service.BatchGetPlayerHomeZoneRequest) (_ *data_service.BatchGetPlayerHomeZoneResponse, err error) {
	metrics.ObserveHomeZoneBatchIDs(len(req.GetPlayerIds()))
	defer observeHomeZoneRead(metrics.HomeZoneRPCBatch, time.Now(), &err)
	mapping, err := s.svcCtx.Router.BatchGetPlayerHomeZone(ctx, req.GetPlayerIds())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		logx.Errorf("[DataRoute] BatchGetPlayerHomeZone ids=%d: %v", len(req.GetPlayerIds()), err)
		return nil, status.Errorf(codes.Unavailable, "error_code=%d: %v", constants.ErrCodeRouteStoreError, err)
	}
	zones := s.svcCtx.MergedZones.Snapshot()
	if zones == nil {
		logx.Errorf("[DataRoute] BatchGetPlayerHomeZone ids=%d: merged zone cache not assembled", len(req.GetPlayerIds()))
		return nil, errMergedZonesUnavailable()
	}
	for playerID, home := range mapping {
		mapping[playerID] = zones.Final(home)
	}
	return &data_service.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: mapping}, nil
}

// GetMergedZones 回答全部合服映射(源区 → 目标区,没有沿链解析),读的是缓存里的快照。没有全局库连接的服务(guild、trade)
// 用它重建 placement.MergedZones。缓存不可用回 Unavailable:调用方不能把「读不到」当成「没有合服」。
func (s *DataServiceServer) GetMergedZones(context.Context, *emptypb.Empty) (*data_service.GetMergedZonesResponse, error) {
	zones := s.svcCtx.MergedZones.Snapshot()
	if zones == nil {
		logx.Errorf("[DataRoute] GetMergedZones: merged zone cache not assembled")
		return nil, errMergedZonesUnavailable()
	}
	return &data_service.GetMergedZonesResponse{MergedInto: zones.Merges()}, nil
}

// errMergedZonesUnavailable 是合服映射缓存没装配时的回复。
func errMergedZonesUnavailable() error {
	return status.Errorf(codes.Unavailable, "error_code=%d: merged zone cache not assembled", constants.ErrCodeRouteStoreError)
}

// observeHomeZoneRead 在两个归属读 handler 的 defer 里记耗时与结局;start 在 defer 语句处求值,即 handler 入口,
// 所以每一条出口(含提前返回的失败分支)都被计入。
func observeHomeZoneRead(rpc string, start time.Time, err *error) {
	metrics.ObserveHomeZoneRead(rpc, homeZoneReadResult(*err), time.Since(start))
}

// homeZoneReadResult 按 handler 实际回给调用方的 gRPC code 归类,与上面两段契约同口径:只有映射的权威缺席(NotFound)
// 算 not_found;全局库故障(Unavailable)与调用方取消 / 超时(Canceled / DeadlineExceeded)都是 error ——
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

// ── 快照元数据(只读审计表)──

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

// ── Batch Recall / Transaction Log Query ──────

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
	snapshot, err := logic.GetPlayerStoreLedger(ctx, s.svcCtx, req.GetPlayerId())
	if err != nil {
		return nil, err
	}
	return &data_service.GetPlayerAssetOpLedgerResponse{Found: snapshot.Found, Ledger: snapshot.Ledger, PlayerReleased: snapshot.Released}, nil
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
