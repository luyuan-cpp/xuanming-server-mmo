package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/loginsession"
	"login/internal/svc"
	pbbase "proto/common/base"
	pbdb "proto/common/database"
	dspb "proto/data_service"
	loginpb "proto/login"
	"shared/generated/pb/table"
	"shared/idsegment"
)

// fakeRegisterClient 只实现 RegisterPlayerZone;别的 RPC 走内嵌的 nil 接口,
// 调到即 panic —— 正好暴露「建角路径碰了不该碰的 RPC」。
type fakeRegisterClient struct {
	dspb.DataServiceClient
	err   error
	calls int
	last  *dspb.RegisterPlayerZoneRequest
}

func (f *fakeRegisterClient) RegisterPlayerZone(_ context.Context, in *dspb.RegisterPlayerZoneRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.calls++
	f.last = in
	if f.err != nil {
		return nil, f.err
	}
	return &emptypb.Empty{}, nil
}

const (
	hzAccount   = "acct-home-zone"
	hzSessionID = uint32(4242)
	hzZoneID    = uint32(7)
	hzPlayerID  = uint64(9001)
)

// newCreatePlayerHarness 起一个 miniredis + 已有账号(零角色)的 CreatePlayer 逻辑对象。
// CreatePlayer 直接读全局 config.AppConfig,所以这里一并把它填成可跑的最小配置。
func newCreatePlayerHarness(t *testing.T, resolver *homezone.Resolver) (*CreatePlayerLogic, *redis.Client, context.Context) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	config.AppConfig.Node.ZoneId = hzZoneID
	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.Locker.AccountLockTTL = 10
	config.AppConfig.Account.MaxPlayersPerAccount = 5
	config.AppConfig.Account.CacheExpire = time.Hour

	ctx := ctxkeys.WithSessionDetails(context.Background(), &pbbase.SessionDetails{SessionId: hzSessionID})
	if err := loginsession.Save(ctx, rdb, hzSessionID, hzAccount); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	blob, err := proto.Marshal(&pbdb.UserAccounts{})
	if err != nil {
		t.Fatalf("marshal empty account: %v", err)
	}
	if err := rdb.Set(ctx, constants.GetAccountDataKey(hzAccount), blob, 0).Err(); err != nil {
		t.Fatalf("seed account blob: %v", err)
	}

	l := NewCreatePlayerLogic(ctx, &svc.ServiceContext{
		RedisClient:    rdb,
		PlayerIDMinter: &idsegment.Minter{Name: "player_id", Segment: &fakeSegment{id: hzPlayerID}},
		HomeZone:       resolver,
	})
	return l, rdb, ctx
}

// storedPlayers 读回账号 blob 里真正落盘的角色列表。
func storedPlayers(t *testing.T, ctx context.Context, rdb *redis.Client) []*pbbase.AccountSimplePlayer {
	t.Helper()
	raw, err := rdb.Get(ctx, constants.GetAccountDataKey(hzAccount)).Bytes()
	if err != nil {
		t.Fatalf("read account blob: %v", err)
	}
	acct := &pbdb.UserAccounts{}
	if err := proto.Unmarshal(raw, acct); err != nil {
		t.Fatalf("unmarshal account blob: %v", err)
	}
	return acct.GetSimplePlayers().GetPlayers()
}

// 成功路径:登记发出去了(参数 = 新 player_id + 本 zone),而且是在落盘之前发的;
// 落盘结果照旧(账号 blob 里有新角色 + 反查映射)。
func TestCreatePlayer_RegistersHomeZoneThenPersists(t *testing.T) {
	fake := &fakeRegisterClient{}
	l, rdb, ctx := newCreatePlayerHarness(t, homezone.New(fake, 0, 0, 0))

	resp, err := l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}

	if fake.calls != 1 {
		t.Fatalf("RegisterPlayerZone calls = %d, want exactly 1", fake.calls)
	}
	if fake.last.GetPlayerId() != hzPlayerID || fake.last.GetHomeZoneId() != hzZoneID {
		t.Fatalf("registered %d→zone %d, want %d→zone %d",
			fake.last.GetPlayerId(), fake.last.GetHomeZoneId(), hzPlayerID, hzZoneID)
	}

	players := storedPlayers(t, ctx, rdb)
	if len(players) != 1 || players[0].GetPlayerId() != hzPlayerID {
		t.Fatalf("persisted players = %v, want exactly player %d", players, hzPlayerID)
	}
	if players[0].GetZoneId() != hzZoneID {
		t.Fatalf("persisted zone_id = %d, want %d", players[0].GetZoneId(), hzZoneID)
	}
	if got, err := rdb.Get(ctx, constants.PlayerToAccountKey(hzPlayerID)).Result(); err != nil || got != hzAccount {
		t.Fatalf("reverse mapping = %q/%v, want %q", got, err, hzAccount)
	}
}

// 失败路径(RPC 报错 / 客户端未接线 / resolver 为 nil):建角必须整体失败,
// 且**一个字节都不能落盘** —— 否则就造出「账号里有、映射里没有」的玩家,
// 合服重映射永远扫不到他。
func TestCreatePlayer_HomeZoneRegistrationFailsClosed(t *testing.T) {
	cases := map[string]struct {
		resolver *homezone.Resolver
	}{
		"rpc unavailable": {resolver: homezone.New(&fakeRegisterClient{err: status.Error(codes.Unavailable, "data_service down")}, 0, 0, 0)},
		"rpc error":       {resolver: homezone.New(&fakeRegisterClient{err: errors.New("boom")}, 0, 0, 0)},
		"nil client":      {resolver: homezone.New(nil, 0, 0, 0)},
		"nil resolver":    {resolver: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			l, rdb, ctx := newCreatePlayerHarness(t, tc.resolver)

			resp, err := l.CreatePlayer(&loginpb.CreatePlayerRequest{})
			if err != nil {
				t.Fatalf("CreatePlayer must fail via tip, not transport error: %v", err)
			}
			if resp.ErrorMessage == nil {
				t.Fatal("create succeeded without a player:zone mapping")
			}
			if got := resp.ErrorMessage.Id; got != uint32(table.LoginError_kLoginDataSerializeFailed) {
				t.Fatalf("tip = %d, want the existing id-gen/serialize tip %d",
					got, uint32(table.LoginError_kLoginDataSerializeFailed))
			}
			if len(resp.Players) != 0 {
				t.Fatalf("response carries %d player(s) after a failed create", len(resp.Players))
			}

			if players := storedPlayers(t, ctx, rdb); len(players) != 0 {
				t.Fatalf("account blob persisted %d player(s) despite the failure", len(players))
			}
			if err := rdb.Get(ctx, constants.PlayerToAccountKey(hzPlayerID)).Err(); !errors.Is(err, redis.Nil) {
				t.Fatalf("reverse mapping written despite the failure (err=%v)", err)
			}
		})
	}
}

// 建角锁必须被释放:失败路径是提前 return,漏了 defer 释放的话同一账号会被
// kLoginInProgress 卡满一个 AccountLockTTL。用「同一个 logic 再跑一次」来验。
func TestCreatePlayer_ReleasesLockOnHomeZoneFailure(t *testing.T) {
	l, _, _ := newCreatePlayerHarness(t, homezone.New(&fakeRegisterClient{err: errors.New("down")}, 0, 0, 0))

	first, err := l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil || first.ErrorMessage == nil {
		t.Fatalf("first create must fail closed, got resp=%v err=%v", first, err)
	}
	second, err := l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if got := second.ErrorMessage.GetId(); got == uint32(table.LoginError_kLoginInProgress) {
		t.Fatal("account lock leaked: retry blocked by kLoginInProgress")
	}
}

// registerHomeZone 单测:成功返回 nil;任何失败都返回既有的 kLoginDataSerializeFailed。
func TestRegisterHomeZone(t *testing.T) {
	config.AppConfig.Node.ZoneId = hzZoneID

	ok := &fakeRegisterClient{}
	l := NewCreatePlayerLogic(context.Background(), &svc.ServiceContext{HomeZone: homezone.New(ok, 0, 0, 0)})
	if tip := l.registerHomeZone(hzAccount, hzPlayerID); tip != nil {
		t.Fatalf("unexpected tip %v", tip)
	}
	if ok.calls != 1 || ok.last.GetHomeZoneId() != hzZoneID {
		t.Fatalf("register call = %d/%v", ok.calls, ok.last)
	}

	failing := map[string]*homezone.Resolver{
		"rpc error":    homezone.New(&fakeRegisterClient{err: errors.New("down")}, 0, 0, 0),
		"nil client":   homezone.New(nil, 0, 0, 0),
		"nil resolver": nil,
	}
	for name, r := range failing {
		t.Run(name, func(t *testing.T) {
			l := NewCreatePlayerLogic(context.Background(), &svc.ServiceContext{HomeZone: r})
			tip := l.registerHomeZone(hzAccount, hzPlayerID)
			if tip == nil {
				t.Fatal("want a failure tip")
			}
			if tip.Id != uint32(table.LoginError_kLoginDataSerializeFailed) {
				t.Fatalf("tip = %d, want %d", tip.Id, uint32(table.LoginError_kLoginDataSerializeFailed))
			}
		})
	}
}
