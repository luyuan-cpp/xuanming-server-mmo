package logic

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/svc"
	"shared/handofffence"
	"shared/ownerepoch"
	"shared/playerredis"
)

// ---------------------------------------------------------------------------
// 真实 Redis 上的 logic 脚本与两段回执全链(docs/design/zone-redis-regression-guard-20261022.md §4.3.2、§4.3.4)
//
// 铸造通道本身(WAITAOF 同一条 pipeline、份数不足、探针、写回推进偏移、池耗尽)由 svc/owner_epoch_mint_realredis_test.go
// 用仿写脚本测;这里只测 logic 自己的脚本与调用链在真实 Redis 上的行为:
//   - 铸造脚本 luaMintEpochAndSetLocation 的重放识别把 owner_epoch 原值写回一次,确实传播;CAS 落败的分支不传播;
//   - 确认脚本 luaConfirmPendingRollback 原样时写回原值并传播;
//   - 两段回执全链:回滚走共享连接(playerredis 句柄),确认走铸造通道 + WAITAOF,确认之后才改成正式回执;
//     份数要不到时回执停在待确认形态。
//
// 环境同 svc 那份:一个专用的单实例 Redis(appendonly yes、appendfsync everysec、没有副本、测试期间没有别的写入),
// 地址放在 MMORPG_TEST_REDIS_ADDR;没设时 Skip,设了却不满足前提时直接失败。7.2 与 8.x 各跑一轮。
//
// 复制偏移的断言都用「两项份数都配 0」的铸造通道发:WAITAOF 一旦阻塞,Redis 会发 REPLCONF GETACK,它本身也会推进
// master_repl_offset(没有副本时同样 +1),「偏移前进 / 不变」就无从归因。
// ---------------------------------------------------------------------------

// realRedisAddr 取专用测试 Redis 的地址;没设时 Skip。
func realRedisAddr(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("MMORPG_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("需要专用 MMORPG_TEST_REDIS_ADDR(单实例、appendonly yes、无副本)")
	}
	return addr
}

func realRedisContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newRealRedisInspector 建旁路观察用的客户端(不经铸造通道):准备与读取键、读复制偏移、清理。顺带核对环境前提。
func newRealRedisInspector(t *testing.T, ctx context.Context, addr string) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Ping(ctx).Err(), "连不上 MMORPG_TEST_REDIS_ADDR=%s", addr)
	aof, err := c.ConfigGet(ctx, "appendonly").Result()
	require.NoError(t, err)
	require.Equal(t, "yes", aof["appendonly"], "测试 Redis 必须 appendonly yes")
	info, err := c.Info(ctx, "replication").Result()
	require.NoError(t, err)
	require.Equal(t, "master", infoField(t, info, "role"), "测试 Redis 必须是主节点")
	require.Equal(t, "0", infoField(t, info, "connected_slaves"), "测试 Redis 必须没有副本(份数不足的用例依赖它)")
	return c
}

// infoField 从 INFO 文本里取一个字段。
func infoField(t *testing.T, info, field string) string {
	t.Helper()
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), field+":"); ok {
			return v
		}
	}
	t.Fatalf("INFO 里没有 %s 字段: %q", field, info)
	return ""
}

// masterReplOffset 读 INFO replication 的 master_repl_offset。没有副本时,只要有写传播它照样前进
// (Redis 7.2 起为 AOF 刷盘记账,replication.c replicationFeedSlaves)。
func masterReplOffset(t *testing.T, ctx context.Context, c *goredis.Client) int64 {
	t.Helper()
	info, err := c.Info(ctx, "replication").Result()
	require.NoError(t, err)
	offset, err := strconv.ParseInt(infoField(t, info, "master_repl_offset"), 10, 64)
	require.NoError(t, err)
	return offset
}

// newRealRedisMinter 建连真实 Redis 的铸造通道(生产实现)。墙钟恒 0:地板不起作用,目标值 = 观察值 + 1,与 mintLuaArgs 一致。
func newRealRedisMinter(t *testing.T, addr string, numLocal, numReplicas, timeoutMs int) *svc.RedisOwnerEpochMinter {
	t.Helper()
	minter, err := svc.NewRedisOwnerEpochMinter(playerredis.Config{Addrs: []string{addr}},
		config.OwnerEpochMintConfig{WaitAOFNumLocal: &numLocal, WaitAOFNumReplicas: &numReplicas, WaitAOFTimeoutMs: timeoutMs},
		func() uint64 { return 0 })
	require.NoError(t, err)
	t.Cleanup(func() { _ = minter.Close() })
	return minter
}

// newRealRedisServiceContext 只装两段回执全链要用的两样:玩家域共享句柄(生产同款 playerredis.New,带守卫)与铸造通道。
func newRealRedisServiceContext(t *testing.T, addr string, minter svc.OwnerEpochMinter) *svc.ServiceContext {
	t.Helper()
	player, err := playerredis.New(playerredis.Config{Addrs: []string{addr}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = player.Close() })
	return &svc.ServiceContext{PlayerRedis: player, OwnerEpochMint: minter}
}

// realRedisPlayerID 挑一个本次独占的玩家号(高位随机,不与业务数据撞),核对它的键都不存在,结束时删掉。
func realRedisPlayerID(t *testing.T, ctx context.Context, c *goredis.Client) uint64 {
	t.Helper()
	playerID := uint64(9_000_000_000_000) + rand.Uint64N(1_000_000_000)
	keys := append(placementKeys(playerID), rollbackPlacementKeys(playerID)...)
	keys = append(keys, ownerepoch.ConfirmedKey(playerID)) // 确认过的铸造与落盘确认会记已确认位点
	n, err := c.Exists(ctx, keys...).Result()
	require.NoError(t, err)
	require.Zero(t, n, "测试玩家号 %d 的键已存在,换一个实例或清库后再跑", playerID)
	t.Cleanup(func() { _ = c.Del(context.Background(), keys...).Err() })
	return playerID
}

// 写回原值必须传播,只读分支不传播:
//   - 铸造脚本的重放识别(首发已生效、原样重发)回 -新值,并把 owner_epoch 原值写回一次 —— 偏移前进;
//   - 铸造脚本 CAS 落败回 0 —— 偏移不动;
//   - 确认脚本三把键原样未变时回 1 并写回原值 —— 偏移前进;任一键变了回 0 —— 偏移不动。
func TestRealRedisLogicScriptsWriteBackPropagatesAndMissesDoNot(t *testing.T) {
	addr := realRedisAddr(t)
	ctx := realRedisContext(t)
	inspect := newRealRedisInspector(t, ctx, addr)
	playerID := realRedisPlayerID(t, ctx, inspect)
	quiet := newRealRedisMinter(t, addr, 0, 0, 0) // 不发 WAITAOF:偏移只因脚本自己的写而变(见文件头)
	marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
	require.NoError(t, inspect.Set(ctx, ownerepoch.OwnerEpochKey(playerID), "1", 0).Err())
	require.NoError(t, inspect.Set(ctx, ownerepoch.HandoffKey(playerID), marker, 0).Err())
	mintArgs := mintLuaArgs("1", "placed-bytes", marker)

	first, err := quiet.EvalAndConfirm(ctx, luaMintEpochAndSetLocation, placementKeys(playerID), mintArgs...)
	require.NoError(t, err)
	require.Equal(t, "2", fmt.Sprint(first.Reply))

	before := masterReplOffset(t, ctx, inspect)
	replay, err := quiet.EvalAndConfirm(ctx, luaMintEpochAndSetLocation, placementKeys(playerID), mintArgs...)
	require.NoError(t, err)
	assert.Equal(t, "-2", fmt.Sprint(replay.Reply), "重放识别命中")
	assert.Greater(t, masterReplOffset(t, ctx, inspect), before, "识别分支写回原值,必须传播")

	before = masterReplOffset(t, ctx, inspect)
	stale, err := quiet.EvalAndConfirm(ctx, luaMintEpochAndSetLocation, placementKeys(playerID),
		mintLuaArgs("1", "someone-else-bytes", marker)...)
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(stale.Reply))
	assert.Equal(t, before, masterReplOffset(t, ctx, inspect), "CAS 落败的分支不传播任何写")

	keys := rollbackReceiptKeys(playerID)
	before = masterReplOffset(t, ctx, inspect)
	confirmed, err := quiet.EvalAndConfirm(ctx, luaConfirmPendingRollback, keys, "2", "placed-bytes", "")
	require.NoError(t, err)
	assert.Equal(t, "1", fmt.Sprint(confirmed.Reply))
	assert.Greater(t, masterReplOffset(t, ctx, inspect), before, "确认脚本原样时写回原值,必须传播")

	before = masterReplOffset(t, ctx, inspect)
	changed, err := quiet.EvalAndConfirm(ctx, luaConfirmPendingRollback, keys, "2", "someone-else-bytes", "")
	require.NoError(t, err)
	assert.Equal(t, "0", fmt.Sprint(changed.Reply))
	assert.Equal(t, before, masterReplOffset(t, ctx, inspect), "location 变了:确认脚本什么都不传播")
}

// realRedisMarkedMint 在真实 Redis 上摆出「凭标记铸造之后」的状态:源节点的旧 location(epoch 1)、本次所凭标记,
// 再经铸造通道铸出 2、写到目标节点。返回旧 location、原文、所凭标记与这次落点;铸造的落盘结论原样交回。
func realRedisMarkedMint(t *testing.T, ctx context.Context, sc *svc.ServiceContext, inspect *goredis.Client,
	playerID uint64) (*scene_manager.PlayerLocation, string, string, placedLocation, error) {
	t.Helper()
	old := &scene_manager.PlayerLocation{SceneId: 7001, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 1, UpdateTime: 12345}
	oldBytes, err := gproto.Marshal(old)
	require.NoError(t, err)
	oldRaw := string(oldBytes)
	marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
	require.NoError(t, inspect.Set(ctx, getPlayerLocationKey(playerID), oldRaw, 0).Err())
	require.NoError(t, inspect.Set(ctx, ownerepoch.OwnerEpochKey(playerID), "1", 0).Err())
	require.NoError(t, inspect.Set(ctx, ownerepoch.HandoffKey(playerID), marker, 0).Err())

	placed, err := placePlayerLocationCtx(ctx, sc, playerID, 7002, "20", testZoneId, placementGuard{
		observedEpoch: 1, mint: true, requiredMarker: marker, expectLoc: expectObservedLocation(oldRaw),
	})
	require.Equal(t, uint64(2), placed.epoch, "铸造已写进 Redis")
	return old, oldRaw, marker, placed, err
}

// 两段回执全链(推路由失败那一支):铸造确认过,推路由失败后回滚。回滚经共享连接把 epoch 推到 3、location 退回源节点
// 并带待确认回执;随后铸造通道写回原值 + WAITAOF 确认落盘,回执才改成正式回执。改写之后修订侧车与原文一致、读得回来。
func TestRealRedisRollbackConfirmsDurabilityBeforeFinalReceipt(t *testing.T) {
	addr := realRedisAddr(t)
	ctx := realRedisContext(t)
	inspect := newRealRedisInspector(t, ctx, addr)
	playerID := realRedisPlayerID(t, ctx, inspect)
	sc := newRealRedisServiceContext(t, addr, newRealRedisMinter(t, addr, 1, 0, 1500))

	old, oldRaw, marker, placed, err := realRedisMarkedMint(t, ctx, sc, inspect, playerID)
	require.NoError(t, err, "单实例 1 0:铸造确认落盘")

	restored, echo := rollbackPlacementAfterPushFailure(ctx, sc, logx.WithContext(ctx), playerID, old, oldRaw, testZoneId, placed, marker, false)
	require.True(t, restored)
	assert.Equal(t, uint64(3), echo)

	epoch, err := inspect.Get(ctx, ownerepoch.OwnerEpochKey(playerID)).Result()
	require.NoError(t, err)
	assert.Equal(t, "3", epoch, "单调回滚:铸出 2 → 3")
	loc, _, _, err := getPlayerLocationSnapshot(sc, playerID)
	require.NoError(t, err, "改写回执之后修订侧车必须与原文一致")
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId, "location 退回源节点")
	assert.Equal(t, uint64(3), loc.OwnerEpoch)
	assert.Equal(t, marker, loc.RollbackReceipt, "回滚确认落盘之后才是正式回执,源端据此采纳 3")
	forwarded, err := inspect.Get(ctx, ownerepoch.HandoffKey(playerID)).Result()
	require.NoError(t, err)
	assert.Equal(t, "3:1757000000000", forwarded)
}

// 两段回执全链(铸造没确认落盘那一支):单实例配 1 1,份数永远要不到。铸造写进内存却回 not_durable;回滚照样执行,
// 回执停在待确认形态 —— 源端不得采纳(B5′),下一次要下发这个 epoch 之前再确认。
func TestRealRedisRollbackWithoutDurabilityLeavesPendingReceipt(t *testing.T) {
	addr := realRedisAddr(t)
	ctx := realRedisContext(t)
	inspect := newRealRedisInspector(t, ctx, addr)
	playerID := realRedisPlayerID(t, ctx, inspect)
	sc := newRealRedisServiceContext(t, addr, newRealRedisMinter(t, addr, 1, 1, 200))

	old, oldRaw, marker, placed, err := realRedisMarkedMint(t, ctx, sc, inspect, playerID)
	require.True(t, errors.Is(err, errOwnerEpochMintNotDurable), "没有副本:铸造没确认落盘,got %v", err)

	restored, echo := rollbackPlacementAfterPushFailure(ctx, sc, logx.WithContext(ctx), playerID, old, oldRaw, testZoneId, placed, marker, false)
	require.True(t, restored, "回滚本身走共享连接,照样执行")
	assert.Equal(t, uint64(3), echo)

	loc, _, _, err := getPlayerLocationSnapshot(sc, playerID)
	require.NoError(t, err)
	require.NotNil(t, loc)
	assert.Equal(t, "10", loc.NodeId)
	assert.Equal(t, uint64(3), loc.OwnerEpoch)
	assert.Equal(t, ownerepoch.PendingRollbackReceipt(marker), loc.RollbackReceipt, "确认不到份数:回执停在待确认形态")
}

// 作废分支在真实 Redis 上的行为(§4.3.5)。miniredis 的 Lua 不是真实 Redis 的那一个实现,这里核对脚本本身:
//   - 不凭标记、首发还没执行:收尾的回滚脚本把 epoch 写到铸出值之后(回 6),迟到的铸造 CAS 不过(回 0),一个字节都不写;
//   - 凭标记、首发还没执行:只删掉所凭的标记(回 7),epoch 与 location 不动,迟到的铸造过不了标记比对(回 -1)。
func TestRealRedisRollbackVoidsMintThatHasNotApplied(t *testing.T) {
	addr := realRedisAddr(t)
	ctx := realRedisContext(t)
	inspect := newRealRedisInspector(t, ctx, addr)
	sc := newRealRedisServiceContext(t, addr, newRealRedisMinter(t, addr, 0, 0, 0))

	t.Run("unmarked", func(t *testing.T) {
		playerID := realRedisPlayerID(t, ctx, inspect)
		placed := placedLocation{epoch: 1, minted: true, raw: "placed-bytes", observedKnown: true}
		plan := routeRollbackPlan{mode: rollbackModeBump, epochAfter: 2, voidUnapplied: true}
		got, err := playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), rollbackPlacementArgs(placed, plan)...)
		require.NoError(t, err)
		require.Equal(t, "6", fmt.Sprint(got))
		epoch, err := inspect.Get(ctx, ownerepoch.OwnerEpochKey(playerID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "2", epoch, "epoch 越过本该铸出的 1")

		late, err := playerRedisEval(sc, luaMintEpochAndSetLocation, placementKeys(playerID),
			mintLuaArgs("0", "placed-bytes", "", "missing")...)
		require.NoError(t, err)
		assert.Equal(t, "0", fmt.Sprint(late), "迟到的首发 CAS 不过")
		n, err := inspect.Exists(ctx, getPlayerLocationKey(playerID)).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "迟到的首发一个字节都写不进去")
	})

	t.Run("marked", func(t *testing.T) {
		playerID := realRedisPlayerID(t, ctx, inspect)
		marker := ownerepoch.Handoff{Epoch: 1, SavedAtMs: 1757000000000}.String()
		require.NoError(t, inspect.Set(ctx, ownerepoch.OwnerEpochKey(playerID), "1", 0).Err())
		require.NoError(t, inspect.Set(ctx, getPlayerLocationKey(playerID), "observed-bytes", 0).Err())
		require.NoError(t, inspect.Set(ctx, ownerepoch.HandoffKey(playerID), marker, 0).Err())
		placed := placedLocation{epoch: 2, minted: true, raw: "placed-bytes",
			observedEpoch: 1, observedRaw: "observed-bytes", observedKnown: true}
		plan := routeRollbackPlan{mode: rollbackModeBump, restoredRaw: "restored-bytes", epochAfter: 3,
			requiredMarker: marker, forwardMarker: "3:1757000000000", voidUnapplied: true}
		got, err := playerRedisEval(sc, luaRollbackPlayerPlacement, rollbackPlacementKeys(playerID), rollbackPlacementArgs(placed, plan)...)
		require.NoError(t, err)
		require.Equal(t, "7", fmt.Sprint(got))
		n, err := inspect.Exists(ctx, ownerepoch.HandoffKey(playerID)).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "所凭的标记被删掉")

		late, err := playerRedisEval(sc, luaMintEpochAndSetLocation, placementKeys(playerID),
			mintLuaArgs("1", "placed-bytes", marker, "present", "observed-bytes")...)
		require.NoError(t, err)
		assert.Equal(t, "-1", fmt.Sprint(late), "迟到的首发过不了标记比对")
		epoch, err := inspect.Get(ctx, ownerepoch.OwnerEpochKey(playerID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "1", epoch, "epoch 不动")
		raw, err := inspect.Get(ctx, getPlayerLocationKey(playerID)).Result()
		require.NoError(t, err)
		assert.Equal(t, "observed-bytes", raw, "location 不动")
	})
}

// realRedisPlayerState 读这名玩家名下(键带 {playerID} tag)的全部键:有没有过期时间与 DUMP 原文。
// 剩余 TTL 每毫秒都在变,只记有没有。
func realRedisPlayerState(t *testing.T, ctx context.Context, c *goredis.Client, playerID uint64) map[string]string {
	t.Helper()
	keys, err := c.Keys(ctx, fmt.Sprintf("*{%d}*", playerID)).Result()
	require.NoError(t, err)
	state := make(map[string]string, len(keys))
	for _, key := range keys {
		ttl, err := c.PTTL(ctx, key).Result()
		require.NoError(t, err)
		dump, err := c.Dump(ctx, key).Result()
		require.NoError(t, err)
		state[key] = fmt.Sprintf("expires=%t dump=%x", ttl > 0, dump)
	}
	return state
}

// 不凭标记的回滚先看交接资格原件(luaRollbackPlayerPlacement),在真实 Redis 上跑 miniredis 那张表
// (TestRollbackPlayerPlacementJudgesUnmarkedMintByHandoffFence):原件缺失或代次更早 → 回滚;铸出值下持有 / 已撤销 → 不动;
// 更新或写坏 → 不当证据,不动。「登记先到」用 C++ 入场登记脚本的逐字副本(cppLuaRegisterFence)写原件;回滚先到时,
// 迟到的登记拿着铸出值被拒。不动的分支核对玩家名下每个键的 DUMP 与键集合;回滚的分支只许 owner_epoch、location 与修订侧车变。
func TestRealRedisUnmarkedRollbackJudgesByHandoffFence(t *testing.T) {
	addr := realRedisAddr(t)
	ctx := realRedisContext(t)
	inspect := newRealRedisInspector(t, ctx, addr)
	sc := newRealRedisServiceContext(t, addr, newRealRedisMinter(t, addr, 1, 0, 1500))
	register := func(t *testing.T, playerID, epoch uint64) string {
		t.Helper()
		got, err := playerRedisEval(sc, cppLuaRegisterFence,
			[]string{ownerepoch.OwnerEpochKey(playerID), handofffence.Key(playerID), ownerepoch.HandoffKey(playerID)},
			strconv.FormatUint(epoch, 10), holderFenceForTest(epoch, replacedNewInstance, handofffence.HandoffFenceHeld), "")
		require.NoError(t, err)
		return fmt.Sprint(got)
	}
	writeFence := func(raw func(minted uint64) string) func(*testing.T, uint64, uint64) {
		return func(t *testing.T, playerID, minted uint64) {
			require.NoError(t, inspect.HSet(ctx, handofffence.Key(playerID), handofffence.HandoffFenceField, raw(minted)).Err())
		}
	}
	cases := []struct {
		name       string
		seed       func(t *testing.T, playerID, minted uint64) // nil = 不写原件
		rolledBack bool
	}{
		{"no_fence_rolls_back", nil, true},
		{"older_fence_rolls_back", writeFence(func(m uint64) string {
			return holderFenceForTest(m-1, replacedOldInstance, handofffence.HandoffFenceFrozen)
		}), true},
		{"registered_first_keeps_placement", func(t *testing.T, playerID, minted uint64) {
			require.Equal(t, "1", register(t, playerID, minted), "前提:目标节点在铸出值下登记成功")
		}, false},
		{"revoked_at_minted_epoch_keeps_placement", writeFence(func(m uint64) string {
			return holderFenceForTest(m, replacedNewInstance, handofffence.HandoffFenceRevoked)
		}), false},
		{"fence_ahead_is_not_proof", writeFence(func(m uint64) string {
			return holderFenceForTest(m+5, replacedNewInstance, handofffence.HandoffFenceHeld)
		}), false},
		{"malformed_fence_is_not_proof", writeFence(func(uint64) string { return "not-a-fence" }), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			playerID := realRedisPlayerID(t, ctx, inspect)
			require.NoError(t, inspect.Set(ctx, ownerepoch.OwnerEpochKey(playerID), "4", 0).Err())
			placed, err := placePlayerLocationCtx(ctx, sc, playerID, 7340, "10", testZoneId, placementGuard{observedEpoch: 4, mint: true})
			require.NoError(t, err, "单实例 1 0:铸造确认落盘")
			require.Greater(t, placed.epoch, uint64(4))
			if c.seed != nil {
				c.seed(t, playerID, placed.epoch)
			}
			plan, err := planRouteRollback(nil, "", testZoneId, placed, "", false)
			require.NoError(t, err)
			before := realRedisPlayerState(t, ctx, inspect, playerID)

			restored, echo := rollbackPlayerPlacement(sc, logx.WithContext(ctx), playerID, placed, plan)
			after := realRedisPlayerState(t, ctx, inspect, playerID)
			if !c.rolledBack {
				assert.False(t, restored)
				assert.Zero(t, echo, "没回滚就不回显")
				assert.Equal(t, before, after, "不回滚:玩家名下的键一个字节都不动,也不多不少")
				return
			}
			assert.True(t, restored)
			assert.Equal(t, placed.epoch+1, echo)
			epochKey := ownerepoch.OwnerEpochKey(playerID)
			epoch, err := inspect.Get(ctx, epochKey).Result()
			require.NoError(t, err)
			assert.Equal(t, strconv.FormatUint(placed.epoch+1, 10), epoch, "单调回滚:铸出值再 +1")
			want, got := maps.Clone(before), maps.Clone(after)
			for _, key := range []string{epochKey, getPlayerLocationKey(playerID), locationRevisionKey(playerID)} {
				delete(want, key)
			}
			delete(got, epochKey)
			assert.Equal(t, want, got, "本次之前没有 location:回滚只改 owner_epoch、删 location 与修订侧车,别的键原样")

			// 回滚先到:迟到的入场登记拿着铸出值被拒,一个字节都不写。
			assert.Equal(t, "-1", register(t, playerID, placed.epoch))
			assert.Equal(t, after, realRedisPlayerState(t, ctx, inspect, playerID))
		})
	}
}
