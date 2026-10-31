package playerstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"db/internal/metrics"

	storepb "proto/player_store"
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

// ErrStorageFrozen 表示玩家正在搬库,暂时不能认领。调用方过一会儿重试。
var ErrStorageFrozen = errors.New("playerstore: player storage is frozen")

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
			if !errors.Is(err, ErrStorageFrozen) {
				logx.Errorf("[player_store] resolve storage of player %d: %v", req.GetPlayerId(), err)
			}
			return nil, status.Error(codes.Unavailable, "player store: storage not resolved")
		}
		storageID = resolved
	}
	db, err := s.open(ctx, storageID)
	if err != nil {
		metrics.ObservePlayerStore(opSave, metrics.PlayerStoreUnavailable, time.Since(start))
		logx.Errorf("[player_store] open storage %d for player %d: %v", storageID, req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: storage unavailable")
	}
	store, err := New(db, s.knownGen, s.limits)
	if err != nil {
		return nil, status.Error(codes.Internal, "player store: not configured")
	}
	resp, err := store.Save(ctx, req)
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
		if !errors.Is(err, ErrStorageFrozen) {
			logx.Errorf("[player_store] resolve storage of player %d: %v", req.GetPlayerId(), err)
		}
		return nil, status.Error(codes.Unavailable, "player store: storage not resolved")
	}
	db, err := s.open(ctx, storageID)
	if err != nil {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnavailable, time.Since(start))
		logx.Errorf("[player_store] open storage %d for player %d: %v", storageID, req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: storage unavailable")
	}
	store, err := New(db, s.knownGen, s.limits)
	if err != nil {
		return nil, status.Error(codes.Internal, "player store: not configured")
	}
	resp, err := store.LoadAndClaim(ctx, req)
	if err != nil {
		metrics.ObservePlayerStore(opClaim, metrics.PlayerStoreUnknown, time.Since(start))
		logx.Errorf("[player_store] claim player %d: %v", req.GetPlayerId(), err)
		return nil, status.Error(codes.Unavailable, "player store: outcome unknown")
	}
	resp.StorageId = storageID
	metrics.ObservePlayerStore(opClaim, resp.GetResult().String(), time.Since(start))
	return resp, nil
}
