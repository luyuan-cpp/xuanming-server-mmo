package clientplayerloginlogic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/panjf2000/ants/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/playernamereg"
	"login/internal/logic/pkg/sessionmanager"
	"login/internal/svc"
	pbbase "proto/common/base"
	comppb "proto/common/component"
	dbpb "proto/common/database"
	loginpb "proto/login"
	plpb "proto/player_locator"
	"shared/generated/pb/table"
	"shared/ownerepoch"
	"shared/savebarrier"
)

const (
	identityPlayerID  = uint64(7001)
	identityLockKey   = "player_locker:7001"
	identityLockToken = "lock-token-7001"
)

// identityBlobKey 与 backfillPlayerIdentity 用同一种拼法(PlayerAllData 全名 + ":{" + player_id + "}")。
func identityBlobKey() string {
	return fmt.Sprintf("%s:{%d}", (&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName(), identityPlayerID)
}

// newIdentityBackfillHarness 起 miniredis(支持 EVAL),种下玩家登录锁与一份 PlayerAllData 存档,
// 返回逻辑对象、redis 客户端、miniredis 句柄与种下的原始字节。
func newIdentityBackfillHarness(t *testing.T, player *dbpb.PlayerDatabase) (*EnterGameLogic, *redis.Client, *miniredis.Miniredis, []byte) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	if err := rdb.Set(ctx, identityLockKey, identityLockToken, time.Minute).Err(); err != nil {
		t.Fatalf("seed player lock: %v", err)
	}
	blob, err := proto.Marshal(&dbpb.PlayerAllData{PlayerDatabaseData: player})
	if err != nil {
		t.Fatalf("marshal player blob: %v", err)
	}
	if err := rdb.Set(ctx, identityBlobKey(), blob, time.Hour).Err(); err != nil {
		t.Fatalf("seed player blob: %v", err)
	}
	d := fixtureDirectory(t, rdb)
	d.seed("identity-account", &pbbase.AccountSimplePlayer{PlayerId: identityPlayerID})
	return NewEnterGameLogic(ctx, &svc.ServiceContext{RedisClient: rdb, AccountDirectory: d}), rdb, mr, blob
}

func identityState(classID uint32, name string) enterGameSessionState {
	return enterGameSessionState{
		account: "identity-account", ownerRevision: 1,
		playerID:        identityPlayerID,
		classID:         classID,
		playerName:      name,
		playerLockKey:   identityLockKey,
		playerLockToken: identityLockToken,
	}
}

func readIdentityBlob(t *testing.T, rdb *redis.Client) ([]byte, *dbpb.PlayerDatabase) {
	t.Helper()
	raw, err := rdb.Get(context.Background(), identityBlobKey()).Bytes()
	if err != nil {
		t.Fatalf("read player blob: %v", err)
	}
	parent := &dbpb.PlayerAllData{}
	if err := proto.Unmarshal(raw, parent); err != nil {
		t.Fatalf("unmarshal player blob: %v", err)
	}
	return raw, parent.GetPlayerDatabaseData()
}

// 首次入场:职业与名字在同一次 CAS 里写进存档,缓存 TTL 保持(脚本的 KEEPTTL 语义不变)。
func TestBackfillPlayerIdentity_WritesClassAndNameTogether(t *testing.T) {
	l, rdb, mr, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})

	if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
		t.Fatalf("backfillPlayerIdentity: %v", err)
	}

	_, player := readIdentityBlob(t, rdb)
	if got := player.GetUint32PbComponent().GetClass(); got != 3 {
		t.Fatalf("class = %d, want 3", got)
	}
	if got := player.GetProfileComponent().GetName(); got != "云中君" {
		t.Fatalf("profile name = %q, want 云中君", got)
	}
	if player.GetPlayerId() != identityPlayerID {
		t.Fatalf("player_id = %d, want %d", player.GetPlayerId(), identityPlayerID)
	}
	if ttl := mr.TTL(identityBlobKey()); ttl <= 0 {
		t.Fatalf("blob TTL = %v after backfill, want the original TTL kept", ttl)
	}
}

// 存档里已有名字:不覆盖。v1 无改名,已有值与注册表一致;覆盖只会在 scene 并发存盘时制造竞态。
func TestBackfillPlayerIdentity_KeepsExistingName(t *testing.T) {
	t.Run("class still filled", func(t *testing.T) {
		l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{
			PlayerId:         identityPlayerID,
			ProfileComponent: &comppb.PlayerProfileComp{Name: "湘夫人"},
		})

		if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
			t.Fatalf("backfillPlayerIdentity: %v", err)
		}

		_, player := readIdentityBlob(t, rdb)
		if got := player.GetProfileComponent().GetName(); got != "湘夫人" {
			t.Fatalf("profile name = %q, want the stored 湘夫人 kept", got)
		}
		if got := player.GetUint32PbComponent().GetClass(); got != 3 {
			t.Fatalf("class = %d, want 3 (class is still missing and must be filled)", got)
		}
	})

	t.Run("nothing missing writes nothing", func(t *testing.T) {
		l, rdb, _, seeded := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{
			PlayerId:          identityPlayerID,
			Uint32PbComponent: &comppb.PlayerUint32Comp{Class: 2},
			ProfileComponent:  &comppb.PlayerProfileComp{Name: "湘夫人"},
		})

		if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
			t.Fatalf("backfillPlayerIdentity: %v", err)
		}

		raw, player := readIdentityBlob(t, rdb)
		if !bytes.Equal(raw, seeded) {
			t.Fatal("blob rewritten although class and name were both present")
		}
		if player.GetUint32PbComponent().GetClass() != 2 || player.GetProfileComponent().GetName() != "湘夫人" {
			t.Fatalf("stored identity changed: class=%d name=%q",
				player.GetUint32PbComponent().GetClass(), player.GetProfileComponent().GetName())
		}
	})
}

// 存档里已有职业、只缺名字:只补名字,已有职业不被账号记录里的 classID 覆盖。
// 这是回档(profile_component 随旧 player_database 回到空值)与早于名字功能的旧存档走的分支,
// 也是提前返回条件放宽后才有的「只写名字」CAS 路径;needClass 的守卫坏了只有这条用例能抓到。
func TestBackfillPlayerIdentity_NameOnlyKeepsStoredClass(t *testing.T) {
	l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{
		PlayerId:          identityPlayerID,
		Uint32PbComponent: &comppb.PlayerUint32Comp{Class: 2},
	})

	if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
		t.Fatalf("backfillPlayerIdentity: %v", err)
	}

	_, player := readIdentityBlob(t, rdb)
	if got := player.GetProfileComponent().GetName(); got != "云中君" {
		t.Fatalf("profile name = %q, want 云中君", got)
	}
	if got := player.GetUint32PbComponent().GetClass(); got != 2 {
		t.Fatalf("class = %d, want the stored 2 kept (account classID must not overwrite it)", got)
	}
}

// 账号记录没名字(旧角色 / 注册表查不到):只补职业,不写出一个空的名字组件。
// 反过来职业为 0 但有名字:只补名字,不猜职业。
func TestBackfillPlayerIdentity_FillsOnlyWhatTheAccountKnows(t *testing.T) {
	t.Run("no name only class", func(t *testing.T) {
		l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})

		if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "")); err != nil {
			t.Fatalf("backfillPlayerIdentity: %v", err)
		}

		_, player := readIdentityBlob(t, rdb)
		if got := player.GetUint32PbComponent().GetClass(); got != 3 {
			t.Fatalf("class = %d, want 3", got)
		}
		if player.GetProfileComponent() != nil {
			t.Fatalf("profile component = %v, want absent when there is no name to write", player.GetProfileComponent())
		}
	})

	t.Run("no class only name", func(t *testing.T) {
		l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})

		if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(0, "云中君")); err != nil {
			t.Fatalf("backfillPlayerIdentity: %v", err)
		}

		_, player := readIdentityBlob(t, rdb)
		if got := player.GetProfileComponent().GetName(); got != "云中君" {
			t.Fatalf("profile name = %q, want 云中君", got)
		}
		if got := player.GetUint32PbComponent().GetClass(); got != 0 {
			t.Fatalf("class = %d, want 0 (must not guess a class)", got)
		}
	})
}

// 没东西可补(classID==0 且 playerName=="")或已有会话:提前返回,连存档都不读。
// ServiceContext 里故意不放 RedisClient —— 只要碰一下 Redis 就是 nil 指针 panic。
func TestBackfillPlayerIdentity_EarlyReturnDoesNotTouchRedis(t *testing.T) {
	l := NewEnterGameLogic(context.Background(), &svc.ServiceContext{})

	// 连登录锁都没有:提前返回必须先于锁检查,否则旧角色入场会被「缺少玩家登录锁」拦下。
	if err := l.backfillPlayerIdentity(context.Background(), nil, enterGameSessionState{playerID: identityPlayerID}); err != nil {
		t.Fatalf("nothing to fill must return nil, got %v", err)
	}
	if err := l.backfillPlayerIdentity(context.Background(), &sessionmanager.PlayerSession{}, identityState(3, "云中君")); err != nil {
		t.Fatalf("existing session must return nil, got %v", err)
	}
}

// player_locator 会话或 SceneManager 位置键还在:scene 持有权威数据,脚本回 2,不写也不报错。
func TestBackfillPlayerIdentity_LiveSessionSkipsWrite(t *testing.T) {
	liveKeys := map[string]string{
		"session":  fmt.Sprintf("player:session:%d", identityPlayerID),
		"location": fmt.Sprintf("player:%d:location", identityPlayerID),
	}
	for name, liveKey := range liveKeys {
		t.Run(name, func(t *testing.T) {
			l, rdb, _, seeded := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})
			if err := rdb.Set(context.Background(), liveKey, "live", 0).Err(); err != nil {
				t.Fatalf("seed %s: %v", liveKey, err)
			}

			if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
				t.Fatalf("script result 2 must not be an error, got %v", err)
			}

			if raw, _ := readIdentityBlob(t, rdb); !bytes.Equal(raw, seeded) {
				t.Fatal("blob rewritten while a live session / location still owns the data")
			}
		})
	}
}

// 身份补齐只改身份字段:父快照里本版本不认识的字段(各层未知字段)与两张表的持久 schema 世代戳
// 必须原样保留(docs/design/mixed-version-persist-field-protection.md §5.4:backfill 不改逻辑,Go 原样保留)。
// 丢了未知字段 = 截断新版本 scene 写下的数据;丢了或改了戳 = 下一跳按错误的加载世代判定或盖戳。
func TestBackfillPlayerIdentity_PreservesUnknownFieldsAndSchemaStamps(t *testing.T) {
	l, rdb, _, _ := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})
	unknownField := func(num protowire.Number, v uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
	}
	topUnknown := unknownField(1000001, 11)
	rowUnknown := unknownField(1000002, 22)
	row1Unknown := unknownField(1000003, 33)
	profileUnknown := unknownField(1000004, 44)

	parent := &dbpb.PlayerAllData{
		PlayerDatabaseData: &dbpb.PlayerDatabase{
			PlayerId:         identityPlayerID,
			PersistSchemaGen: 7,
			// 只带未知字段的身份组件:backfill 往里补名字时,这一层的未知字段也得留着。
			ProfileComponent: &comppb.PlayerProfileComp{},
		},
		PlayerDatabase_1Data: &dbpb.PlayerDatabase_1{PlayerId: identityPlayerID, PersistSchemaGen: 6},
	}
	parent.ProtoReflect().SetUnknown(topUnknown)
	parent.PlayerDatabaseData.ProtoReflect().SetUnknown(rowUnknown)
	parent.PlayerDatabase_1Data.ProtoReflect().SetUnknown(row1Unknown)
	parent.PlayerDatabaseData.ProfileComponent.ProtoReflect().SetUnknown(profileUnknown)
	seeded, err := proto.Marshal(parent)
	if err != nil {
		t.Fatalf("marshal parent with unknown fields: %v", err)
	}
	if err := rdb.Set(context.Background(), identityBlobKey(), seeded, time.Hour).Err(); err != nil {
		t.Fatalf("seed parent with unknown fields: %v", err)
	}

	if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err != nil {
		t.Fatalf("backfillPlayerIdentity: %v", err)
	}

	raw, player := readIdentityBlob(t, rdb)
	if bytes.Equal(raw, seeded) {
		t.Fatal("backfill did not write; the round trip is not exercised")
	}
	if player.GetUint32PbComponent().GetClass() != 3 || player.GetProfileComponent().GetName() != "云中君" {
		t.Fatalf("identity not filled: class=%d name=%q",
			player.GetUint32PbComponent().GetClass(), player.GetProfileComponent().GetName())
	}
	after := &dbpb.PlayerAllData{}
	if err := proto.Unmarshal(raw, after); err != nil {
		t.Fatalf("unmarshal backfilled parent: %v", err)
	}
	for name, tc := range map[string]struct{ got, want []byte }{
		"PlayerAllData":                     {after.ProtoReflect().GetUnknown(), topUnknown},
		"player_database":                   {after.GetPlayerDatabaseData().ProtoReflect().GetUnknown(), rowUnknown},
		"player_database_1":                 {after.GetPlayerDatabase_1Data().ProtoReflect().GetUnknown(), row1Unknown},
		"player_database.profile_component": {after.GetPlayerDatabaseData().GetProfileComponent().ProtoReflect().GetUnknown(), profileUnknown},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s unknown fields = %x, want %x (backfill must carry them through unchanged)", name, tc.got, tc.want)
		}
	}
	if got := after.GetPlayerDatabaseData().GetPersistSchemaGen(); got != 7 {
		t.Errorf("player_database persist_schema_gen = %d, want 7 kept", got)
	}
	if got := after.GetPlayerDatabase_1Data().GetPersistSchemaGen(); got != 6 {
		t.Errorf("player_database_1 persist_schema_gen = %d, want 6 kept", got)
	}
	// backfill 不碰 player_database_1:整张表(含未知字段,proto.Equal 会比较它们)逐字段不变。
	if !proto.Equal(after.GetPlayerDatabase_1Data(), parent.GetPlayerDatabase_1Data()) {
		t.Errorf("player_database_1 changed: got %v, want %v", after.GetPlayerDatabase_1Data(), parent.GetPlayerDatabase_1Data())
	}
}

// 登录锁已被别人接管(令牌不符):脚本回 -1,报错且不写 —— 失锁的旧登录链不能继续落盘。
func TestBackfillPlayerIdentity_LostLockFailsWithoutWrite(t *testing.T) {
	l, rdb, _, seeded := newIdentityBackfillHarness(t, &dbpb.PlayerDatabase{PlayerId: identityPlayerID})
	if err := rdb.Set(context.Background(), identityLockKey, "someone-else", time.Minute).Err(); err != nil {
		t.Fatalf("overwrite lock: %v", err)
	}

	if err := l.backfillPlayerIdentity(context.Background(), nil, identityState(3, "云中君")); err == nil {
		t.Fatal("want an error when the player lock token no longer matches")
	}

	if raw, _ := readIdentityBlob(t, rdb); !bytes.Equal(raw, seeded) {
		t.Fatal("blob rewritten by a login chain that lost its lock")
	}
}

// resolveEnterName:账号记录有名字就用它,一次 RPC 都不多;只有缺名才回源一次。
func TestResolveEnterName(t *testing.T) {
	t.Run("account name wins without lookup", func(t *testing.T) {
		stub := &stubRoleNameLookup{names: map[uint64]string{identityPlayerID: "注册表里的名字"}}
		if got := resolveEnterName(context.Background(), "云中君", stub, identityPlayerID); got != "云中君" {
			t.Fatalf("name = %q, want the account name", got)
		}
		if stub.calls != 0 {
			t.Fatalf("Lookup calls = %d, want 0 for a role that already has a name", stub.calls)
		}
	})

	t.Run("missing name looks up once", func(t *testing.T) {
		stub := &stubRoleNameLookup{names: map[uint64]string{identityPlayerID: "云中君"}}
		if got := resolveEnterName(context.Background(), "", stub, identityPlayerID); got != "云中君" {
			t.Fatalf("name = %q, want 云中君 from the registry", got)
		}
		if stub.calls != 1 || len(stub.lastIDs) != 1 || stub.lastIDs[0] != identityPlayerID {
			t.Fatalf("Lookup calls=%d ids=%v, want exactly one call for [%d]", stub.calls, stub.lastIDs, identityPlayerID)
		}
		if !stub.hadDeadline || stub.remaining <= 0 || stub.remaining > playernamereg.DefaultLookupTimeout {
			t.Fatalf("lookup must be bounded by DefaultLookupTimeout, got deadline=%v remaining=%v",
				stub.hadDeadline, stub.remaining)
		}
	})

	t.Run("absent from registry", func(t *testing.T) {
		stub := &stubRoleNameLookup{}
		if got := resolveEnterName(context.Background(), "", stub, identityPlayerID); got != "" {
			t.Fatalf("name = %q, want empty when the registry has no row", got)
		}
		if stub.calls != 1 {
			t.Fatalf("Lookup calls = %d, want 1", stub.calls)
		}
	})

	t.Run("lookup error returns empty", func(t *testing.T) {
		stub := &stubRoleNameLookup{err: errors.New("data_service down")}
		if got := resolveEnterName(context.Background(), "", stub, identityPlayerID); got != "" {
			t.Fatalf("name = %q, want empty on lookup failure (fail-open)", got)
		}
		if stub.calls != 1 {
			t.Fatalf("Lookup calls = %d, want 1 (no retry on the enter path)", stub.calls)
		}
	})

	// nil 指针装进接口后不等于 nil 接口:前者靠 Client.Lookup 的 nil 接收者分支,后者靠 names == nil。
	t.Run("registry not wired", func(t *testing.T) {
		var nilClient *playernamereg.Client
		if got := resolveEnterName(context.Background(), "", nilClient, identityPlayerID); got != "" {
			t.Fatalf("nil *Client: name = %q, want empty", got)
		}
		if got := resolveEnterName(context.Background(), "", nil, identityPlayerID); got != "" {
			t.Fatalf("nil interface: name = %q, want empty", got)
		}
	})
}

// ── EnterGame 接线 ────────────────────────────────────────────────────────────
//
// 上面的用例都是手工把名字塞进 state.playerName / 直接打纯函数。生产代码里 playerName 只有
// 两个来源(账号记录命中处、self-heal 处),而且必须早于 `enterCtx := flowState` 的值拷贝 ——
// 这两行被删掉或挪到拷贝之后,上面的用例照样全绿,名字却再也补不进存档。下面这组从
// EnterGame 入口打进去,看名字有没有真的落到 PlayerAllData 上。
//
// 入场链是异步的,这里让它走最短的真实路径:PlayerAllData 已在 Redis(预加载走快路径,不碰
// Kafka / dispatcher)→ player_locator 回「无会话」(FirstLogin,身份补齐会执行)→ SetSession
// 失败,链在身份补齐**之后**、gate 绑定 / scene_manager **之前**收尾并释放玩家锁。

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

// newEnterWiringHarness 起 miniredis,种下登录会话、账号 blob(players 即账号记录里的角色)和一份
// 只有 player_id 的 PlayerAllData;名字注册表按 registry 应答。
// EnterGame 直接读全局 config.AppConfig,这里一并填成可跑的最小配置(与建角 harness 同做法)。
func newEnterWiringHarness(t *testing.T, players []*pbbase.AccountSimplePlayer, registry map[uint64]string) *enterWiringHarness {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	installParentIdentityInfoForTest(mr)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	config.AppConfig.Node.SessionExpireMin = 30
	config.AppConfig.Locker.PlayerLockTTL = 120
	config.AppConfig.Account.CacheExpire = time.Hour

	details := &pbbase.SessionDetails{SessionId: enterWiringSessionID}
	ctx := ctxkeys.WithSessionDetails(context.Background(), details)
	if err := loginsession.Save(ctx, rdb, loginsession.RefOf(details), enterWiringAccount); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	accountBlob, err := proto.Marshal(&dbpb.UserAccounts{
		SimplePlayers: &pbbase.AccountSimplePlayerList{Players: players},
	})
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	if err := rdb.Set(ctx, constants.GetAccountDataKey(enterWiringAccount), accountBlob, 0).Err(); err != nil {
		t.Fatalf("seed account blob: %v", err)
	}
	playerBlob, err := proto.Marshal(savebarrier.InitialPlayerData(identityPlayerID))
	if err != nil {
		t.Fatalf("marshal player blob: %v", err)
	}
	if err := rdb.Set(ctx, identityBlobKey(), playerBlob, time.Hour).Err(); err != nil {
		t.Fatalf("seed player blob: %v", err)
	}
	// 这是已持久提交的入场夹具，不能把缺屏障的零序号父键当快路径。
	if err := rdb.Set(ctx, ownerepoch.OwnerEpochKey(identityPlayerID), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	version := savebarrier.Version(1, 1)
	if err := rdb.HSet(ctx, savebarrier.Key(identityPlayerID), "pending", version, "player_database", version,
		"player_database_1", version, savebarrier.VerifiedIdentityField, strings.Repeat("a", 40)+":"+strings.Repeat("b", 40)).Err(); err != nil {
		t.Fatal(err)
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
		RedisClient: rdb, AccountDirectory: fixtureDirectory(t, rdb),
		PlayerNames:         playernamereg.New(h.registry),
		PlayerLocatorClient: h.locator,
		PreloadPool:         pool,
	})
	return h
}

// waitChain 等异步入场链收尾(含释放玩家锁)。ants 的 ReleaseTimeout 会等在跑的 worker 退出,
// 所以这是有上界的等待,不靠 sleep 轮询;5s 只是「链卡死」时的失败上界,不是时序假设。
func (h *enterWiringHarness) waitChain(t *testing.T) {
	t.Helper()
	if err := h.pool.ReleaseTimeout(5 * time.Second); err != nil {
		t.Fatalf("async enter chain did not finish: %v", err)
	}
}

func TestEnterGame_PlayerNameReachesIdentityBackfill(t *testing.T) {
	cases := map[string]struct {
		players []*pbbase.AccountSimplePlayer
		// selfHeal:账号 blob 里没有该角色,归属靠 player_id → account 反向索引证明。
		selfHeal    bool
		registry    map[uint64]string
		wantName    string
		wantClass   uint32
		wantLookups int
	}{
		// 正常角色:名字与职业同源于账号记录,一次 RPC 都不多。
		"account record has name": {
			players:     []*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}},
			registry:    map[uint64]string{identityPlayerID: "不该被用到"},
			wantName:    "云中君",
			wantClass:   3,
			wantLookups: 0,
		},
		// 缺名记录(早于名字功能的旧角色、此前 self-heal 恢复出的空记录):回源一次。
		"nameless account record looks up once": {
			players:     []*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3}},
			registry:    map[uint64]string{identityPlayerID: "云中君"},
			wantName:    "云中君",
			wantClass:   3,
			wantLookups: 1,
		},
		// self-heal:恢复出的记录带名字写回账号 blob,同一个名字也进存档;职业无从得知,不猜。
		"self-heal restores id and name": {
			selfHeal:    true,
			registry:    map[uint64]string{identityPlayerID: "云中君"},
			wantName:    "云中君",
			wantClass:   0,
			wantLookups: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newEnterWiringHarness(t, tc.players, tc.registry)
			ctx := context.Background()
			if tc.selfHeal {
				if err := h.rdb.Set(ctx, constants.PlayerToAccountKey(identityPlayerID), enterWiringAccount, 0).Err(); err != nil {
					t.Fatalf("seed reverse index: %v", err)
				}
			}

			resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
			if err != nil {
				t.Fatalf("EnterGame: %v", err)
			}
			if tc.selfHeal {
				if resp.GetErrorMessage().GetId() != uint32(table.LoginError_kLoginEnterGameGuid) {
					t.Fatalf("旧反向缓存不得恢复SQL缺失角色: %v", resp)
				}
				h.waitChain(t)
				if h.locator.setSessionCalls.Load() != 0 {
					t.Fatal("SQL缺角色不得绑定会话")
				}
				return
			}
			if resp.GetErrorMessage() != nil {
				t.Fatalf("EnterGame rejected with tip %d, want the async chain started", resp.GetErrorMessage().GetId())
			}
			h.waitChain(t)

			// 链必须真的走到了 persist(身份补齐在它前面),否则下面读到的空名说明不了问题。
			if got := h.locator.setSessionCalls.Load(); got != 1 {
				t.Fatalf("SetSession calls = %d, want 1 (chain must get past identity backfill)", got)
			}

			_, player := readIdentityBlob(t, h.rdb)
			if got := player.GetProfileComponent().GetName(); got != tc.wantName {
				t.Fatalf("profile name = %q, want %q (playerName must be set before flowState is copied into the chain)",
					got, tc.wantName)
			}
			if got := player.GetUint32PbComponent().GetClass(); got != tc.wantClass {
				t.Fatalf("class = %d, want %d", got, tc.wantClass)
			}
			if h.registry.calls != tc.wantLookups {
				t.Fatalf("BatchGetPlayerName calls = %d, want %d", h.registry.calls, tc.wantLookups)
			}

		})
	}
}
