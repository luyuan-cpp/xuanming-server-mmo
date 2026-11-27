package playerstore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"db/internal/metrics"

	storepb "proto/player_store"
	"shared/authoritysql"
	"shared/serviceauth"
)

const (
	methodSavePlayer   = "/playerstore.PlayerStore/SavePlayer"
	methodLoadAndClaim = "/playerstore.PlayerStore/LoadAndClaim"

	// SceneSecretEnv 是 scene 调用存盘接口的密钥所在的环境变量。只有 scene 能存盘和认领。
	SceneSecretEnv = "MMORPG_PLAYER_STORE_SECRET_SCENE"
	sceneCaller    = "scene"

	opSave  = "save"
	opClaim = "claim"
)

// StoreOpener 按落点编号给出已打开的库。返回的错误一律按「落点库暂不可用」处理。
type StoreOpener func(ctx context.Context, storageID uint32) (*sql.DB, error)

// StorageResolver 查玩家的落点。认领时用;Save 只有不带落点的空行释放才用,其余的落点在认领时已经返回给 scene。
type StorageResolver func(ctx context.Context, playerID uint64) (uint32, error)

// ServerOptions 是 Server 的依赖。
type ServerOptions struct {
	Secret   string
	Open     StoreOpener
	Resolve  StorageResolver
	KnownGen uint32
	Limits   Limits
}

// Server 是 PlayerStore gRPC 服务:鉴权、选库、记指标,其余都在 Store 里。
type Server struct {
	storepb.UnimplementedPlayerStoreServer
	verifier *serviceauth.Verifier
	open     StoreOpener
	resolve  StorageResolver
	knownGen uint32
	limits   Limits
}

// NewServer 校验依赖。密钥不合格直接返回错误,调用方拒绝启动。
func NewServer(opt ServerOptions) (*Server, error) {
	if opt.Open == nil || opt.Resolve == nil {
		return nil, errors.New("playerstore: store opener and storage resolver are required")
	}
	verifier, err := serviceauth.New(map[string]string{sceneCaller: opt.Secret})
	if err != nil {
		return nil, err
	}
	if err := opt.Limits.validate(); err != nil {
		return nil, err
	}
	metrics.PrimePlayerStore(opSave, playerStoreResultNames(storepb.SavePlayerResult_name))
	metrics.PrimePlayerStore(opClaim, playerStoreResultNames(storepb.LoadAndClaimResult_name))
	return &Server{verifier: verifier, open: opt.Open, resolve: opt.Resolve, knownGen: opt.KnownGen, limits: opt.Limits}, nil
}

// playerStoreResultNames 是一个 op 可能出现的全部 result:proto 枚举名(0 值不会出现)加三个没拿到判定结果的取值。
func playerStoreResultNames(enum map[int32]string) []string {
	names := []string{metrics.PlayerStoreDenied, metrics.PlayerStoreUnavailable, metrics.PlayerStoreUnknown}
	for value, name := range enum {
		if value != 0 {
			names = append(names, name)
		}
	}
	return names
}

// storeBreakerName 是存档库 storageID 的熔断在 go-zero 具名熔断表里的名字。编号只来自验过签的 scene 请求与目录路由,个数就是存档库数。
func storeBreakerName(storageID uint32) string {
	return "player_store/" + strconv.FormatUint(uint64(storageID), 10)
}

// enterStore 过存档库 storageID 的熔断(go-zero 的自适应熔断,每个库一个)。
//
// 熔断按库、不按方法:一个库失联时,指向它的调用立即回暂不可用,不再各自占着连接等超时;指向别的库的调用不受影响。
// zrpc 自带的服务端熔断以方法名为键,做不到后一点,go/db 的入口把它关了(db.go;依据见
// docs/design/db-task-store-unavailable-deferral.md 文首 2026-11-20)。
//
// 放行时返回 settle,调用方拿到这次调用的结局之后必须调一次;熔断开着时返回 breaker.ErrServiceUnavailable,这次调用不碰库。
// 闸门冻结(搬库窗口)记成功:闸门答了话,库没有坏。调用方自己取消时成功、失败都不记:挂断说明不了库的好坏,
// 记成成功会在库失联时顶着熔断不让它打开(与 data_service 的建连熔断同一口径,internal/svc/db_breaker.go)。
func enterStore(ctx context.Context, storageID uint32) (settle func(outcome error), err error) {
	promise, err := breaker.GetBreaker(storeBreakerName(storageID)).Allow()
	if err != nil {
		return nil, err
	}
	return func(outcome error) {
		switch {
		case outcome == nil || errors.Is(outcome, authoritysql.ErrWriterFrozen):
			promise.Accept()
		case errors.Is(ctx.Err(), context.Canceled):
			// 不结算:go-zero 的 promise 不占名额,丢掉它就是这一次不进统计。
		default:
			promise.Reject(outcome.Error())
		}
	}, nil
}

// SavePlayer 见 Store.Save。返回 gRPC 错误就是结果未知,scene 用同样的内容、同一个序号重试。
func (s *Server) SavePlayer(ctx context.Context, req *storepb.SavePlayerRequest) (*storepb.SavePlayerResponse, error) {
	start := time.Now()
	if caller, err := s.verifier.Verify(ctx, methodSavePlayer, req); err != nil || caller != sceneCaller {
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreDenied, time.Since(start))
		return nil, status.Error(codes.PermissionDenied, "player store: caller denied")
	}
	storageID := req.GetStorageId()
	if storageID == 0 {
		// 实体已经没了的空行释放:scene 手里没有落点,这里查。带行的存盘必须带认领时返回的落点:
		// 现查的落点可能已经不是认领时那个,写错库比拒绝更糟。
		if !req.GetRelease() || len(req.GetRows()) != 0 {
			metrics.ObservePlayerStore(opSave, storepb.SavePlayerResult_SAVE_PLAYER_INVALID.String(), time.Since(start))
			return &storepb.SavePlayerResponse{Result: storepb.SavePlayerResult_SAVE_PLAYER_INVALID}, nil
		}
		resolved, err := s.resolve(ctx, req.GetPlayerId())
		if err != nil {
			metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnavailable, time.Since(start))
			logx.Errorf("[player_store] resolve storage of player %d: %v", req.GetPlayerId(), err)
			return nil, status.Error(codes.Unavailable, "player store: storage not resolved")
		}
		storageID = resolved
	}
	settle, err := enterStore(ctx, storageID)
	if err != nil {
		// 这个库的熔断开着。不逐次打日志:原因看同一个库被放行的那几次调用打的行。
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnavailable, time.Since(start))
		return nil, status.Error(codes.Unavailable, "player store: storage breaker open")
	}
	db, err := s.open(ctx, storageID)
	if err != nil {
		settle(err)
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnavailable, time.Since(start))
		logx.Errorf("[player_store] open storage %d for player %d: %v", storageID, req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: storage unavailable")
	}
	store, err := New(db, s.knownGen, s.limits)
	if err != nil {
		settle(nil) // 装配问题,不是这个库的故障
		return nil, status.Error(codes.Internal, "player store: not configured")
	}
	resp, err := store.Save(ctx, req)
	settle(err)
	if errors.Is(err, authoritysql.ErrWriterFrozen) {
		// 闸门冻结(搬库切换窗口):这笔没写进去,结局是确定的。scene 按原序号退避重试,不算未知结局,也不打错误日志。
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnavailable, time.Since(start))
		return nil, status.Error(codes.Unavailable, "player store: writer gate frozen")
	}
	if err != nil {
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnknown, time.Since(start))
		logx.Errorf("[player_store] save player %d seq %d: %v", req.GetPlayerId(), req.GetSaveSeq(), err)
		return nil, status.Error(codes.Unavailable, "player store: outcome unknown")
	}
	metrics.ObservePlayerStore(opSave, resp.GetResult().String(), time.Since(start))
	if resp.GetResult() == storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED {
		for _, row := range req.GetRows() {
			metrics.ObservePlayerStoreRowBytes(row.GetKind().String(), len(row.GetPayload()))
		}
	}
	return resp, nil
}

// LoadAndClaim 见 Store.LoadAndClaim。
func (s *Server) LoadAndClaim(ctx context.Context, req *storepb.LoadAndClaimRequest) (*storepb.LoadAndClaimResponse, error) {
	start := time.Now()
	if caller, err := s.verifier.Verify(ctx, methodLoadAndClaim, req); err != nil || caller != sceneCaller {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreDenied, time.Since(start))
		return nil, status.Error(codes.PermissionDenied, "player store: caller denied")
	}
	storageID, err := s.resolve(ctx, req.GetPlayerId())
	if err != nil {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnavailable, time.Since(start))
		logx.Errorf("[player_store] resolve storage of player %d: %v", req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: storage not resolved")
	}
	settle, err := enterStore(ctx, storageID)
	if err != nil {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnavailable, time.Since(start))
		return nil, status.Error(codes.Unavailable, "player store: storage breaker open")
	}
	db, err := s.open(ctx, storageID)
	if err != nil {
		settle(err)
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnavailable, time.Since(start))
		logx.Errorf("[player_store] open storage %d for player %d: %v", storageID, req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: storage unavailable")
	}
	store, err := New(db, s.knownGen, s.limits)
	if err != nil {
		settle(nil)
		return nil, status.Error(codes.Internal, "player store: not configured")
	}
	resp, err := store.LoadAndClaim(ctx, req)
	settle(err)
	if errors.Is(err, authoritysql.ErrWriterFrozen) {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnavailable, time.Since(start))
		return nil, status.Error(codes.Unavailable, "player store: writer gate frozen")
	}
	if err != nil {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnknown, time.Since(start))
		logx.Errorf("[player_store] claim player %d: %v", req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: outcome unknown")
	}
	resp.StorageId = storageID
	metrics.ObservePlayerStore(opClaim, resp.GetResult().String(), time.Since(start))
	return resp, nil
}
