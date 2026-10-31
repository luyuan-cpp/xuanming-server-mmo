package clientplayerloginlogic

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/panjf2000/ants/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/playernamereg"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	pbbase "proto/common/base"
	dbpb "proto/common/database"
	plpb "proto/player_locator"
)

const (
	identityPlayerID  = uint64(7001)
	identityLockKey   = "player_locker:{7001}"
	identityLockToken = "lock-token-7001"
)

// EnterGame 接线:入场链是异步的,这里让它走最短的真实路径:player_locator 回「无会话」(FirstLogin),
// 之后 SetSession 回错误,链停在 gate 绑定 / scene_manager 之前。

const (
	enterWiringAccount   = "acct-enter-wiring"
	enterWiringSessionID = uint32(5151)
)

// enterWiringLocator 是 player_locator 的假客户端,只实现入场链在这段路上会碰的 RPC(GetSession /
// SetSession / Reconnect);别的走内嵌的 nil 接口,调到即 panic。计数与最后一次请求用原子量:
// 它们在入场链的 worker 上写。SetSession 与 Reconnect 都回错误,让链停在 persist。
type enterWiringLocator struct {
	plpb.PlayerLocatorClient
	// existing 非 nil 时 GetSession 回这条会话记录(顶号 / 短线重连用例);nil = 无会话(首登)。
	// 只在 EnterGame 之前写,之后只读。
	existing *plpb.PlayerSession

	setSessionCalls atomic.Int32
	lastSetSession  atomic.Pointer[plpb.SetSessionRequest]
	reconnectCalls  atomic.Int32
	lastReconnect   atomic.Pointer[plpb.ReconnectRequest]
}

func (f *enterWiringLocator) GetSession(context.Context, *plpb.GetSessionRequest, ...grpc.CallOption) (*plpb.GetSessionResponse, error) {
	if f.existing == nil {
		return &plpb.GetSessionResponse{}, nil
	}
	return &plpb.GetSessionResponse{Found: true, Session: proto.Clone(f.existing).(*plpb.PlayerSession)}, nil
}

func (f *enterWiringLocator) SetSession(_ context.Context, in *plpb.SetSessionRequest, _ ...grpc.CallOption) (*pbbase.Empty, error) {
	f.setSessionCalls.Add(1)
	f.lastSetSession.Store(in)
	return nil, errors.New("enter wiring test stops the chain at persist")
}

func (f *enterWiringLocator) Reconnect(_ context.Context, in *plpb.ReconnectRequest, _ ...grpc.CallOption) (*plpb.ReconnectResponse, error) {
	f.reconnectCalls.Add(1)
	f.lastReconnect.Store(in)
	return nil, errors.New("enter wiring test stops the chain at persist")
}

type enterWiringHarness struct {
	l        *EnterGameLogic
	rdb      *redis.Client
	registry *fakeNameRegistryClient
	locator  *enterWiringLocator
	pool     *ants.Pool
}

// newEnterWiringHarness 起 miniredis,种下登录会话和账号 blob(players 即账号记录里的角色);名字注册表按 registry 应答。
// EnterGame 直接读全局 config.AppConfig,这里一并填成可跑的最小配置(与建角 harness 同做法)。
func newEnterWiringHarness(t *testing.T, players []*pbbase.AccountSimplePlayer, registry map[uint64]string) *enterWiringHarness {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.Locker.PlayerLockTTL = 120
	config.AppConfig.Account.CacheExpire = time.Hour

	details := &pbbase.SessionDetails{SessionId: enterWiringSessionID, GateInstanceId: "gate-enter-wiring", SessionIncarnation: 1}
	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	seedLoginSession(t, ctx, rdb, details, enterWiringAccount)
	accountBlob, err := proto.Marshal(&dbpb.UserAccounts{
		SimplePlayers: &pbbase.AccountSimplePlayerList{Players: players},
	})
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	if err := rdb.Set(ctx, constants.GetAccountDataKey(enterWiringAccount), accountBlob, 0).Err(); err != nil {
		t.Fatalf("seed account blob: %v", err)
	}
	pool, err := ants.NewPool(1)
	if err != nil {
		t.Fatalf("preload pool: %v", err)
	}
	t.Cleanup(pool.Release)

	h := &enterWiringHarness{
		rdb:      rdb,
		registry: &fakeNameRegistryClient{names: registry},
		locator:  &enterWiringLocator{},
		pool:     pool,
	}
	h.l = NewEnterGameLogic(ctx, &svc.ServiceContext{
		RedisClient: rdb, PlayerRedis: rdb, LoginRedis: rdb, AccountDirectory: fixtureDirectory(t, rdb),
		PlayerNames:         playernamereg.New(h.registry),
		PlayerLocatorClient: h.locator,
		PreloadPool:         pool,
	})
	return h
}

// seedLoginSession 种下一条连接的登录态,与 Login 提交的第 1 步是同一段脚本。
func seedLoginSession(t *testing.T, ctx context.Context, rdb redis.UniversalClient, details *pbbase.SessionDetails, account string) {
	t.Helper()
	ref, ok := loginsession.RefOf(details)
	if !ok {
		t.Fatalf("seed session: incomplete connection identity %v", details)
	}
	if err := sessionfence.CommitLogin(ctx, rdb, ref.Fence(), account, loginsession.TTL()); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// waitChain 等异步入场链收尾(含释放玩家锁)。ants 的 ReleaseTimeout 会等在跑的 worker 退出,
// 所以这是有上界的等待,不靠 sleep 轮询;5s 只是「链卡死」时的失败上界,不是时序假设。
func (h *enterWiringHarness) waitChain(t *testing.T) {
	t.Helper()
	if err := h.pool.ReleaseTimeout(5 * time.Second); err != nil {
		t.Fatalf("async enter chain did not finish: %v", err)
	}
}
