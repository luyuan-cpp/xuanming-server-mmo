package playercontract

import (
	"context"
	"testing"

	"match/internal/metrics"
	"match/internal/svc"

	plpb "proto/player_locator"
	smpb "proto/scene_manager"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

// corruptProto 是一个被截断的 length-delimited 字段(声明 5 字节、实际只有 2 字节),
// 任何 message 解析它都会报错。
const corruptProto = "\x0a\x05ab"

// newSplitSvcCtx 起两个独立的 miniredis:shared 充当 SharedRedis(契约 key 所在库),
// private 充当 MatchRedis。契约读如果误走 MatchRedis,就读不到 shared 里铺的数据,测试会失败。
// 不设置 Kafka:本文件的推送用例都在在线判定处返回,不会走到 Kafka。
func newSplitSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *miniredis.Miniredis) {
	t.Helper()
	shared := miniredis.RunT(t)
	private := miniredis.RunT(t)
	sharedRds := redis.MustNewRedis(redis.RedisConf{Host: shared.Addr(), Type: "node"})
	privateRds := redis.MustNewRedis(redis.RedisConf{Host: private.Addr(), Type: "node"})
	return &svc.ServiceContext{
		MatchRedis:  privateRds,
		SharedRedis: sharedRds,
		Redis:       sharedRds,
	}, shared, private
}

func mustMarshal(t *testing.T, msg proto.Message) string {
	t.Helper()
	raw, err := proto.Marshal(msg)
	require.NoError(t, err)
	return string(raw)
}

func onlineSession(playerId uint64) *plpb.PlayerSession {
	return &plpb.PlayerSession{
		PlayerId:       playerId,
		SessionId:      7,
		GateId:         "3",
		GateInstanceId: "gate-uuid",
		State:          plpb.PlayerSessionState_SESSION_STATE_ONLINE,
	}
}

// key 字面量是与 C++ scene / scene_manager / player_locator 共用的跨进程契约,钉死。
func TestContractKeyLiterals(t *testing.T) {
	require.Equal(t, "player:session:42", SessionKey(42))
	require.Equal(t, "player:42:location", LocationKey(42))
	require.Equal(t, "battle:lock:42", BattleLockKey(42))

	const maxID = uint64(18446744073709551615)
	require.Equal(t, "player:session:18446744073709551615", SessionKey(maxID))
	require.Equal(t, "player:18446744073709551615:location", LocationKey(maxID))
	require.Equal(t, "battle:lock:18446744073709551615", BattleLockKey(maxID))

	// 按玩家分布的契约 key 不得带 hash tag。
	for _, key := range []string{SessionKey(1), LocationKey(1), BattleLockKey(1)} {
		require.NotContains(t, key, "{", key)
		require.NotContains(t, key, "}", key)
	}
}

func TestParseSession(t *testing.T) {
	session, err := ParseSession("")
	require.NoError(t, err)
	require.Nil(t, session, "空串 = 键不存在 / MGET 缺失项")

	session, err = ParseSession(mustMarshal(t, onlineSession(5)))
	require.NoError(t, err)
	require.Equal(t, uint64(5), session.GetPlayerId())

	_, err = ParseSession(corruptProto)
	require.Error(t, err)
}

func TestLoadSessionReadsSharedRedisOnly(t *testing.T) {
	svcCtx, shared, private := newSplitSvcCtx(t)
	ctx := context.Background()
	const pid = uint64(9001)

	// 只铺在 MatchRedis:契约读必须看不到。
	require.NoError(t, private.Set(SessionKey(pid), mustMarshal(t, onlineSession(pid))))
	session, err := LoadSession(ctx, svcCtx, pid)
	require.NoError(t, err)
	require.Nil(t, session)

	require.NoError(t, shared.Set(SessionKey(pid), mustMarshal(t, onlineSession(pid))))
	session, err = LoadSession(ctx, svcCtx, pid)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, uint32(7), session.GetSessionId())
	require.Equal(t, "3", session.GetGateId())
	require.Equal(t, "gate-uuid", session.GetGateInstanceId())
	require.True(t, IsSessionOnline(session))

	require.NoError(t, shared.Set(SessionKey(pid), corruptProto))
	_, err = LoadSession(ctx, svcCtx, pid)
	require.Error(t, err)
}

// 已取消的 ctx 必须截断 Redis 往返:team 的请求预算依赖这一点。
// 数据已铺好,如果实现没有把 ctx 传给 Redis,这次读会成功,测试失败。
func TestLoadSessionHonorsContext(t *testing.T) {
	svcCtx, shared, _ := newSplitSvcCtx(t)
	require.NoError(t, shared.Set(SessionKey(1), mustMarshal(t, onlineSession(1))))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoadSession(ctx, svcCtx, 1)
	require.Error(t, err)
}

func TestIsSessionOnline(t *testing.T) {
	require.False(t, IsSessionOnline(nil))
	for state, want := range map[plpb.PlayerSessionState]bool{
		plpb.PlayerSessionState_SESSION_STATE_UNKNOWN:       false,
		plpb.PlayerSessionState_SESSION_STATE_ONLINE:        true,
		plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING: false,
		plpb.PlayerSessionState_SESSION_STATE_OFFLINE:       false,
	} {
		require.Equal(t, want, IsSessionOnline(&plpb.PlayerSession{State: state}), state.String())
	}
}

func TestLoadLocation(t *testing.T) {
	svcCtx, shared, private := newSplitSvcCtx(t)
	ctx := context.Background()
	const pid = uint64(9002)

	loc, err := LoadLocation(ctx, svcCtx, pid)
	require.NoError(t, err)
	require.Nil(t, loc, "键不存在返回 (nil, nil)")

	raw := mustMarshal(t, &smpb.PlayerLocation{SceneId: 11, NodeId: "2", ZoneId: 102})
	require.NoError(t, private.Set(LocationKey(pid), raw))
	loc, err = LoadLocation(ctx, svcCtx, pid)
	require.NoError(t, err)
	require.Nil(t, loc, "只在 MatchRedis 的位置不是契约数据")

	require.NoError(t, shared.Set(LocationKey(pid), raw))
	loc, err = LoadLocation(ctx, svcCtx, pid)
	require.NoError(t, err)
	require.NotNil(t, loc)
	require.Equal(t, uint64(11), loc.GetSceneId())
	require.Equal(t, "2", loc.GetNodeId())
	require.Equal(t, uint32(102), loc.GetZoneId())

	require.NoError(t, shared.Set(LocationKey(pid), corruptProto))
	_, err = LoadLocation(ctx, svcCtx, pid)
	require.Error(t, err)
}

func TestIsBattleLocked(t *testing.T) {
	svcCtx, shared, private := newSplitSvcCtx(t)
	ctx := context.Background()

	require.NoError(t, private.Set(BattleLockKey(5), "880001"))
	locked, err := IsBattleLocked(ctx, svcCtx, 5)
	require.NoError(t, err)
	require.False(t, locked, "只在 MatchRedis 的锁不算数")

	require.NoError(t, shared.Set(BattleLockKey(5), "880001"))
	locked, err = IsBattleLocked(ctx, svcCtx, 5)
	require.NoError(t, err)
	require.True(t, locked)

	// Redis 出错按"有锁"处理(fail-closed)。ERR 前缀的错误 go-redis 不重试,结果确定。
	shared.SetError("ERR injected failure")
	locked, err = IsBattleLocked(ctx, svcCtx, 6)
	require.Error(t, err)
	require.True(t, locked)
}

// 不在线时不推送,返回 ErrPlayerOffline;会话读失败 / 解析失败是故障,不能被当成"不在线"。
func TestPushToPlayerOfflineAndFaults(t *testing.T) {
	svcCtx, shared, _ := newSplitSvcCtx(t)
	ctx := context.Background()
	msg := &plpb.PlayerSession{} // 任意 proto 消息

	err := PushToPlayer(ctx, svcCtx, 11, 1, msg)
	require.ErrorIs(t, err, ErrPlayerOffline, "会话键不存在")

	disconnecting := onlineSession(11)
	disconnecting.State = plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	require.NoError(t, shared.Set(SessionKey(11), mustMarshal(t, disconnecting)))
	err = PushToPlayer(ctx, svcCtx, 11, 1, msg)
	require.ErrorIs(t, err, ErrPlayerOffline, "断线等重连不算在线")

	require.NoError(t, shared.Set(SessionKey(12), corruptProto))
	err = PushToPlayer(ctx, svcCtx, 12, 1, msg)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPlayerOffline)

	shared.SetError("ERR injected failure")
	err = PushToPlayer(ctx, svcCtx, 13, 1, msg)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPlayerOffline)
}

// 本包不记 match_kafka_push_total:team 直接调 PushToPlayer,离线 / 故障若记在这里,
// 组队推送会灌进挑战链路指标,离线队友还会被算成 error。口径由 logic 的包装负责。
func TestPushToPlayerDoesNotRecordKafkaPushMetric(t *testing.T) {
	svcCtx, shared, _ := newSplitSvcCtx(t)
	ctx := context.Background()
	msg := &plpb.PlayerSession{}
	errBefore := metrics.KafkaPushValue("error")
	okBefore := metrics.KafkaPushValue("ok")

	require.ErrorIs(t, PushToPlayer(ctx, svcCtx, 21, 1, msg), ErrPlayerOffline)

	require.NoError(t, shared.Set(SessionKey(22), corruptProto))
	err := PushToPlayer(ctx, svcCtx, 22, 1, msg)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPlayerOffline)

	require.Equal(t, errBefore, metrics.KafkaPushValue("error"))
	require.Equal(t, okBefore, metrics.KafkaPushValue("ok"))
}
