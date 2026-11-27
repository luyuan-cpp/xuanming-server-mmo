package logic

// 包内单测的公共夹具。logic 的成局、票据与观战原件都在 SQL,用 TiDB 的流程见 global_*_native_test.go 与 authority 包;
// 这里的用例只覆盖不经 SQL 的判定,PlayerRedis / MatchRedis 用 miniredis。

import (
	"context"
	"strconv"
	"testing"

	"match/internal/config"
	"match/internal/playercontract"
	"match/internal/svc"
	"shared/clusterredis"
	"shared/drainproof"
	"shared/playerredis"

	plpb "proto/player_locator"
	smpb "proto/scene_manager"

	"github.com/alicebob/miniredis/v2"
	redv9 "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// newTestSvcCtx 直接构造 ServiceContext(NewServiceContext 需要 etcd 与 SQL),
// MatchRedis / PlayerRedis 指向同一个 miniredis,各带各自域的键守卫(与生产同一套构造器):键发错句柄,用例当场报错。
// 没有 Authority:用例碰到 SQL 就 panic,说明它不属于这一层。
func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	matchRedis, err := clusterredis.New(clusterredis.Config{Addrs: []string{mr.Addr()}, PoolSizePerNode: 16}, clusterredis.MatchPrivateKeys)
	require.NoError(t, err)
	t.Cleanup(func() { _ = matchRedis.Close() })
	playerRedis, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}, PoolSizePerNode: 16})
	require.NoError(t, err)
	t.Cleanup(func() { _ = playerRedis.Close() })
	return &svc.ServiceContext{
		RelayAdmission: drainproof.New(),
		Config:         config.Config{MatchedTicketTTLSeconds: 30},
		MatchRedis:     matchRedis,
		PlayerRedis:    playerRedis,
	}, mr
}

// zaddSpectateMember 直接往一个观战索引桶里写成员(ZADD),用来铺坏成员、孤儿这类正常写路径写不出来的数据。
func zaddSpectateMember(t *testing.T, svcCtx *svc.ServiceContext, key string, score int64, member string) {
	t.Helper()
	require.NoError(t, svcCtx.MatchRedis.ZAdd(context.Background(), key, redv9.Z{Score: float64(score), Member: member}).Err())
}

// setPlayerLocation 模拟 scene_manager 写 player:{id}:location。
func setPlayerLocation(t *testing.T, mr *miniredis.Miniredis, playerId uint64, zoneId uint32) {
	t.Helper()
	raw, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 1, NodeId: "1", ZoneId: zoneId})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playercontract.LocationKey(playerId), string(raw)))
}

// setPlayerSessionOnGate 模拟 player_locator 写 player:session:{id}:在线、挂在指定 gate。
func setPlayerSessionOnGate(t *testing.T, mr *miniredis.Miniredis, playerId uint64, gateId, gateInstance string) {
	t.Helper()
	raw, err := proto.Marshal(&plpb.PlayerSession{
		SessionId:      uint32(playerId % 1000),
		GateId:         gateId,
		GateInstanceId: gateInstance,
		State:          plpb.PlayerSessionState_SESSION_STATE_ONLINE,
		Account:        "acc_" + strconv.FormatUint(playerId, 10),
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playercontract.SessionKey(playerId), string(raw)))
}

// setPlayerSessionOnline 模拟 player_locator 写 player:session:{id}:在线、挂在 gate 3。
func setPlayerSessionOnline(t *testing.T, mr *miniredis.Miniredis, playerId uint64) {
	t.Helper()
	setPlayerSessionOnGate(t, mr, playerId, "3", "gate-inst-3")
}

// setSessionMinorPlayDeadline 给已写好的 player:session:{id} 补上未成年人当天的截止时刻
// (PlayerSession.minor_play_deadline_ms,login 进游戏时写;0 = 不限)。
func setSessionMinorPlayDeadline(t *testing.T, mr *miniredis.Miniredis, playerId, deadlineMs uint64) {
	t.Helper()
	raw, err := mr.Get(playercontract.SessionKey(playerId))
	require.NoError(t, err)
	session := &plpb.PlayerSession{}
	require.NoError(t, proto.Unmarshal([]byte(raw), session))
	session.MinorPlayDeadlineMs = deadlineMs
	updated, err := proto.Marshal(session)
	require.NoError(t, err)
	require.NoError(t, mr.Set(playercontract.SessionKey(playerId), string(updated)))
}
