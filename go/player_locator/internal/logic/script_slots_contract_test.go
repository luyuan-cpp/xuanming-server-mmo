package logic

// 玩家域每段多键脚本的 KEYS 同 slot 的契约(docs/design/redis-player-store-cluster.md §5.3、§5.9、§14.1)。
//
// 上了 Cluster,一段 Lua 的全部 KEYS 不同 slot 就是 CROSSSLOT;现在开发环境还是单实例,跨 slot 不报错,所以由这里提前拦住。
// KEYS 由生产代码的函数给出,脚本加键时这里跟着变。

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "proto/player_locator"
	"shared/playerredis"
	"shared/redisslot"
)

func TestScriptKeysShareThePlayerSlot(t *testing.T) {
	const gate, otherGate = "3f0c6a52-6d1e-4b7a-9a41-0c2f7e5b8d10", "b7e2d4c1-8a3f-4e6b-9c0d-1f2a3b4c5d6e"
	for _, pid := range []uint64{1, 42, 9001, 1234567890123, 18446744073709551615} {
		slot := redisslot.PlayerSlot(pid)
		scripts := map[string][]string{
			"replaceSession 首登":            replaceSessionKeys(pid, "", gate),
			"replaceSession 换 gate":        replaceSessionKeys(pid, otherGate, gate),
			"replaceSession 同 gate":        replaceSessionKeys(pid, gate, gate),
			"replaceSession 无实例号":          replaceSessionKeys(pid, gate, ""),
			"replaceSession 两侧都没有索引":       replaceSessionKeys(pid, "", ""),
			"setDisconnecting 有索引":         setDisconnectingKeys(pid, gate),
			"setDisconnecting 无索引":         setDisconnectingKeys(pid, ""),
			"deleteSessionIfUnchanged 有索引": deleteSessionKeys(pid, gate),
			"deleteSessionIfUnchanged 无索引": deleteSessionKeys(pid, ""),
			"claimExpiredLeases":           claimExpiredLeasesKeys(slot),
			"pinLeaseClaimPayload":         pinLeaseClaimPayloadKeys(pid),
			"commitLeaseExpiry / rearm":    sessionLifecycleKeys(pid),
			"ackLeaseClaim":                ackLeaseClaimKeys(pid),
			"renewLeaseClaims":             renewLeaseClaimsKeys(slot),
			"enqueueOfflineCleanup":        enqueueOfflineCleanupKeys(pid),
			"pruneGateIndexMember":         pruneGateIndexMemberKeys(pid, gate),
			"releaseGateIndexIfEmpty":      releaseGateIndexKeys(gateSlot{slot: slot, gate: gate}),
			"repairGateIndexMember":        repairGateIndexMemberKeys(pid, gate),
		}
		for name, keys := range scripts {
			require.GreaterOrEqual(t, len(keys), 2, name)
			for _, key := range keys {
				assert.NotEmpty(t, key, "%s: KEYS 里不得有空串,哨兵在 ARGV 里", name)
				assert.True(t, redisslot.HasHashTag(key), "%s: %s 必须带 tag", name, key)
				assert.Equal(t, slot, redisslot.HashSlot(key), "%s: %s 不在玩家的 slot 上", name, key)
			}
			assert.NoError(t, playerredis.CheckKeys(keys), name)
		}
	}
}

// 控制域的键一律不带 hash tag,不和玩家键混。
func TestControlDomainKeysCarryNoHashTag(t *testing.T) {
	for _, key := range []string{
		gateIndexReadyKey, gateIndexGenKey, gateIndexLastAuditKey, gateIndexAuditCooldownKey, reconcilerLeaderKey,
	} {
		assert.False(t, redisslot.HasHashTag(key), key)
	}
}

// 走一遍真实的会话生命周期、清扫与补建,核对玩家域上发出的每一条 EVAL:全部 KEYS 带 tag 且同 slot,脚本一段都不漏。
func TestEveryPlayerDomainEvalStaysInOneSlot(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	ctx := context.Background()
	seen := map[string]bool{}
	rec := newCommandRecorder()
	rec.before = func(_ context.Context, cmd redis.Cmder) {
		if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
			return
		}
		numKeys, err := strconv.Atoi(cmdArg(cmd, 2))
		require.NoError(t, err)
		require.Positive(t, numKeys, "玩家域脚本必须声明键")
		keys := make([]string, 0, numKeys)
		for i := 0; i < numKeys; i++ {
			keys = append(keys, cmdArg(cmd, 3+i))
		}
		assert.NoError(t, playerredis.CheckKeys(keys), "%v", keys)
		if cmd.Name() == "evalsha" {
			seen[cmdArg(cmd, 1)] = true
		}
	}
	sc.PlayerRedis.AddHook(rec)
	base := time.Now()

	// 首登、顶号换 gate、断线、认领(含钉载荷)、续期、AFK 续租、再认领、提交、ack。
	const playerID = uint64(9001)
	slot := redisslot.PlayerSlot(playerID)
	require.NoError(t, callSetSession(sc, indexTestSession(playerID, 100, idxGateA, 1)))
	require.NoError(t, callSetSession(sc, indexTestSession(playerID, 200, idxGateB, 2)))
	_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 200, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	claims, err := claimExpiredLeases(ctx, sc, slot, base.Add(2*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	renewed, err := renewLeaseClaims(ctx, sc, claims, base.Add(50*time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, renewed)
	require.NoError(t, rearmLeaseClaim(ctx, sc, claims[0], 1))
	claims, err = claimExpiredLeases(ctx, sc, slot, base.Add(10*time.Second), 10, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	committed, err := commitLeaseExpiry(ctx, sc, claims[0])
	require.NoError(t, err)
	require.Equal(t, 1, committed)
	require.NoError(t, ackLeaseClaim(ctx, sc, claims[0]))

	// 登出:删会话,LeaveScene 失败入队。
	const logoutID = uint64(9002)
	fake := &fakeSceneManagerClient{}
	fake.setLeaveErr(errors.New("scene manager unavailable"))
	sc.SceneManagerClient = fake
	logout := indexTestSession(logoutID, 100, idxGateA, 1)
	logout.SceneId = 50
	require.NoError(t, callSetSession(sc, logout))
	_, err = NewMarkOfflineLogic(ctx, sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId: logoutID, ExpectedSessionId: 100, ExpectedSessionVersion: 1,
	})
	require.NoError(t, err)

	// 清扫:死 gate 上的 ONLINE 转换,脏成员摘除,索引空了释放。
	require.NoError(t, callSetSession(sc, indexTestSession(9003, 100, idxGateC, 1)))
	seedIndexMember(t, mr, idxGateC, 9004)
	sweeper := newTestSweeper(t, sc, mr, newFakeClusterView())
	mustIndexRoundOK(t, sweeper)
	mustIndexRoundOK(t, sweeper)

	// 补建:漏进索引的 ONLINE 会话补回来。
	writeRawSession(t, mr, indexTestSession(9005, 100, idxGateA, 1))
	step := newTestReconciler(t, sc, newFakeClusterView(idxGateA)).repairStep(ctx)
	require.Equal(t, reconcileResultOK, step.result)
	require.Equal(t, 1, step.report.repaired)

	// 分片:心跳、接管、续约、释放。
	shard := newTestShard(sc, shardRoleLeaseMonitor, "replica-a", newTestClock())
	shard.cycle(ctx)
	shard.cycle(ctx)
	shard.Stop()

	for name, script := range map[string]*redis.Script{
		"replaceSessionAndCancelLease": replaceSessionAndCancelLeaseScript,
		"setDisconnecting":             setDisconnectingScript,
		"deleteSessionIfUnchanged":     deleteSessionIfUnchangedScript,
		"claimExpiredLeases":           claimExpiredLeasesScript,
		"pinLeaseClaimPayload":         pinLeaseClaimPayloadScript,
		"commitLeaseExpiry":            commitLeaseExpiryScript,
		"ackLeaseClaim":                ackLeaseClaimScript,
		"renewLeaseClaims":             renewLeaseClaimsScript,
		"rearmLeaseClaim":              rearmLeaseClaimScript,
		"enqueueOfflineCleanup":        enqueueOfflineCleanupScript,
		"pruneGateIndexMember":         pruneGateIndexMemberScript,
		"releaseGateIndexIfEmpty":      releaseGateIndexIfEmptyScript,
		"repairGateIndexMember":        repairGateIndexMemberScript,
		"shardHeartbeat":               shardHeartbeatScript,
		"shardRenew":                   shardRenewScript,
		"shardRelease":                 shardReleaseScript,
	} {
		assert.True(t, seen[script.Hash()], "脚本 %s 没有走到,这个用例覆盖不了它", name)
	}
}

// 认领脚本只碰本 slot 的租约键,不读其他玩家的会话:认领结束时载荷还没钉,会话键也没被声明过。
func TestClaimScriptDeclaresOnlyLeaseKeys(t *testing.T) {
	sc, _ := newTestSvcCtx(t)
	ctx := context.Background()
	const playerID = uint64(9101)
	slot := redisslot.PlayerSlot(playerID)
	require.NoError(t, callSetSession(sc, indexTestSession(playerID, 100, idxGateA, 1)))
	_, err := NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: 100, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)

	result, err := claimExpiredLeasesScript.Run(ctx, sc.PlayerRedis, claimExpiredLeasesKeys(slot),
		time.Now().Add(2*time.Second).Unix(), time.Now().Add(30*time.Second).Unix(), 10, "batch").Slice()
	require.NoError(t, err)

	require.Len(t, result, 2, "只返回成员与令牌")
	assert.Equal(t, "9101", redisResultString(result[0]))
	assert.Equal(t, "batch:9101", redisResultString(result[1]))
	lease := leaseKeysForSlot(slot)
	assert.NotContains(t, claimExpiredLeasesKeys(slot), sessionKey(playerID))
	exists, err := sc.PlayerRedis.HExists(ctx, lease.claimPayloads, "9101").Result()
	require.NoError(t, err)
	assert.False(t, exists, "载荷由逐人的脚本钉,不在认领脚本里读会话")
}
