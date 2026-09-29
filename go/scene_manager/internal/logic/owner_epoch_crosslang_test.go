package logic

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gproto "google.golang.org/protobuf/proto"

	"proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// ---------------------------------------------------------------------------
// 跨语言 Lua 金样(GO-2 根治,cross-zone-scene-travel.md §12.8)
//
// GO-2 的安全性建立在三段跑在同一个 Redis 上的原子 Lua 之间的先后关系上,它们分属两种语言:
//
//   - scene_manager 的回滚 luaRollbackPlayerPlacement(本包 owner_epoch.go,直接引用包内常量);
//   - 目标 scene 载入时的 A2′ kLuaInheritClear(C++ exit_release_mark.h);
//   - 源 scene 的原子取证 kLuaJudgeTravelOutcome(C++ player_lifecycle.h 的 travel_outcome)。
//
// C++ 那两段没有 Go 侧的生成物,这里**逐字复制**,在 miniredis 上与 Go 的回滚 / 铸造 Lua 一起跑:
// 锁住「A2′ 与回滚互斥」「取证只删本族标记并读得到回执」这两条跨语言契约。
// **修改须两边同步**:改了 C++ 的任何一段,必须同步改这里的副本(TestSourceJudgeScriptAndInheritClearCopiesMatchCpp
// 在仓库完整时会逐字比对并报出漂移;只拷了 go/ 的构建环境里找不到 C++ 源码则跳过比对)。
// kLuaJudgeTravelOutcome 的副本去掉了首行 "#!lua":miniredis 内置的 gopher-lua 不认 shebang(真 Redis ≥ 7 由它
// 让脚本在只读副本 / MISCONF 下整体被拒,那一条性质只能靠 runbook 实跑验证)。
// ---------------------------------------------------------------------------

// cppLuaInheritClear 逐字复制自 cpp/libs/services/scene/player/system/exit_release_mark.h 的
// exit_release_mark::kLuaInheritClear。修改须两边同步。
//
//	KEYS[1] = player:{id}:owner_epoch   KEYS[2] = player:{id}:handoff   ARGV[1] = N(本次路由的 owner_epoch)
//	-1 = owner_epoch ≠ N(一个标记都不删,拒建实体);0 = 没有标记;1 = 删掉更旧代际;2 = 删掉的恰是 N;
//	3 = 标记代际比 N 新(保留);4 = 标记写坏(删掉)。
const cppLuaInheritClear = "local cur = redis.call('GET', KEYS[1]) " +
	"if cur == false then cur = '0' end " +
	"if cur ~= ARGV[1] then return -1 end " +
	"local mark = redis.call('GET', KEYS[2]) " +
	"if mark == false then return 0 end " +
	"local e = string.match(mark, '^(%d+):%d+$') " +
	"if e == nil then redis.call('DEL', KEYS[2]) return 4 end " +
	"e = (string.gsub(e, '^0+(%d)', '%1')) " +
	"local n = ARGV[1] " +
	"if #e > #n or (#e == #n and e > n) then return 3 end " +
	"redis.call('DEL', KEYS[2]) " +
	"if e == n then return 2 end " +
	"return 1"

// cppJudgeShebang 是 C++ kLuaJudgeTravelOutcome 的首行,副本里去掉了它(见文件头)。
const cppJudgeShebang = "#!lua\n"

// cppLuaJudgeTravelOutcome 逐字复制自 cpp/libs/services/scene/player/system/player_lifecycle.h 的
// travel_outcome::kLuaJudgeTravelOutcome(去掉首行 "#!lua")。修改须两边同步。
//
//	KEYS[1] = player:{id}:handoff   KEYS[2] = player:{id}:owner_epoch   KEYS[3] = player:{id}:location
//	ARGV[1] = 本次交接的 requestedAtMs(标记 "{E}:{t}" 里的 t)
//	只删后缀与 ARGV[1] 逐字节相同的那一族标记,再 MGET owner_epoch 与 location。
const cppLuaJudgeTravelOutcome = "local v = redis.call('MGET', KEYS[1])[1] " +
	"if v and string.match(v, '^%d+:(%d+)$') == ARGV[1] then redis.call('DEL', KEYS[1]) end " +
	"return redis.call('MGET', KEYS[2], KEYS[3])"

// crossLangRepoRoot 是仓库根相对本包目录(go/scene_manager/internal/logic,go test 的工作目录)的路径。
var crossLangRepoRoot = filepath.Join("..", "..", "..", "..")

// cppStringConstant 从 C++ 头文件里取出 `*<name> =` 之后、第一个字面量之外的分号之前的全部字符串字面量,
// 按 C++ 相邻字面量拼接的规则连起来。只处理 \n 与「反斜杠 + 任意字符」两种转义(这些 Lua 常量只用到 \n)。
// 文件不存在返回 ("", false);文件在而常量找不到 / 没有闭合时让用例失败(改名或改写法也要被发现)。
func cppStringConstant(t *testing.T, relPath, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(crossLangRepoRoot, filepath.FromSlash(relPath)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	require.NoError(t, err)
	src := string(data)
	decl := "*" + name + " ="
	start := strings.Index(src, decl)
	require.GreaterOrEqual(t, start, 0, "%s 里找不到常量 %s", relPath, name)

	var out strings.Builder
	inLiteral := false
	for i := start + len(decl); i < len(src); i++ {
		c := src[i]
		if !inLiteral {
			switch c {
			case '"':
				inLiteral = true
			case ';':
				return out.String(), true
			}
			continue
		}
		switch {
		case c == '"':
			inLiteral = false
		case c == '\\' && i+1 < len(src):
			i++
			if src[i] == 'n' {
				out.WriteByte('\n')
			} else {
				out.WriteByte(src[i])
			}
		default:
			out.WriteByte(c)
		}
	}
	t.Fatalf("%s 的常量 %s 没有以分号结束", relPath, name)
	return "", false
}

// 仓库完整时逐字比对两段副本与 C++ 原文:副本一旦漂移,下面两个金样验证的就不再是线上跑的脚本。
func TestSourceJudgeScriptAndInheritClearCopiesMatchCpp(t *testing.T) {
	inherit, ok := cppStringConstant(t, "cpp/libs/services/scene/player/system/exit_release_mark.h", "kLuaInheritClear")
	if !ok {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过逐字比对")
	}
	assert.Equal(t, inherit, cppLuaInheritClear, "cppLuaInheritClear 与 C++ exit_release_mark::kLuaInheritClear 不一致:两边必须同步")

	judge, ok := cppStringConstant(t, "cpp/libs/services/scene/player/system/player_lifecycle.h", "kLuaJudgeTravelOutcome")
	require.True(t, ok, "exit_release_mark.h 在而 player_lifecycle.h 不在:仓库布局变了,请同步本用例的路径")
	assert.Equal(t, judge, cppJudgeShebang+cppLuaJudgeTravelOutcome,
		"cppLuaJudgeTravelOutcome 与 C++ travel_outcome::kLuaJudgeTravelOutcome(去掉 #!lua 首行)不一致:两边必须同步")
}

// 令牌互斥(§12.8 第六节):目标节点 B 载入时的 A2′ 与 scene_manager 的凭标记回滚是同一 Redis 上的两段原子 Lua,
// 只能先后执行,两种先后都不会出现双持有。
func TestRollbackAndInheritClearAreMutuallyExclusive(t *testing.T) {
	log := NewEnterSceneLogic(context.Background(), &svc.ServiceContext{}).Logger

	// setup 摆出「源节点 10 持有 epoch 1、已写标记 1:…;凭它铸出 2,落点是节点 20(目标 B)」,返回落点与回滚计划。
	setup := func(t *testing.T, playerID uint64) (*svc.ServiceContext, *miniredis.Miniredis, placedLocation, routeRollbackPlan) {
		t.Helper()
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		require.NoError(t, UpdatePlayerLocation(context.Background(), sc, playerID, 7381, "10", testZoneId))
		require.Equal(t, "1", ownerEpochRaw(t, sc, playerID))
		oldRaw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
		oldLoc, err := GetPlayerLocation(context.Background(), sc, playerID)
		require.NoError(t, err)
		writeHandoffMarker(t, sc, playerID, 1)
		marker, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		placed, err := placePlayerLocation(sc, playerID, 7382, "20", testZoneId,
			placementGuard{observedEpoch: 1, mint: true, requiredMarker: marker})
		require.NoError(t, err)
		require.Equal(t, uint64(2), placed.epoch)
		plan, err := planRouteRollback(oldLoc, oldRaw, testZoneId, placed, marker, false)
		require.NoError(t, err)
		require.Equal(t, "3:1757000000000", plan.forwardMarker)
		return sc, mr, placed, plan
	}
	// inheritClear 以 N 执行一次 C++ 的 A2′,返回它的结果码。
	inheritClear := func(t *testing.T, sc *svc.ServiceContext, playerID uint64, n uint64) string {
		t.Helper()
		result, err := sc.Redis.Eval(cppLuaInheritClear,
			[]string{ownerepoch.OwnerEpochKey(playerID), ownerepoch.HandoffKey(playerID)}, strconv.FormatUint(n, 10))
		require.NoError(t, err)
		return fmt.Sprint(result)
	}

	t.Run("inherit_clear_first_consumes_the_token", func(t *testing.T) {
		const playerID = uint64(6381)
		sc, mr, placed, plan := setup(t, playerID)

		// B 载入(N = 2):owner_epoch 核对通过,删掉代际 1 的标记(比 N 旧,结果码 1)。
		assert.Equal(t, "1", inheritClear(t, sc, playerID, 2))
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)))

		markerGoneBefore := enterSceneRollbackCount(t, rollbackOutcomeMarkerGone)
		restored, echo := rollbackPlayerPlacement(sc, log, playerID, placed, plan)
		assert.False(t, restored, "令牌已被 B 消费:回滚一个字节都不动")
		assert.Equal(t, uint64(0), echo, "没回滚就不回显")
		assert.Equal(t, markerGoneBefore+1, enterSceneRollbackCount(t, rollbackOutcomeMarkerGone))
		assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "归属留在 B")
		raw, _ := sc.Redis.Get(getPlayerLocationKey(playerID))
		assert.Equal(t, placed.raw, raw, "location 仍是本次落点:源端取证读到 (2, 指向 B) 自行销毁")
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "marker_gone 不写转写标记")
	})

	t.Run("rollback_first_makes_inherit_clear_refuse", func(t *testing.T) {
		const playerID = uint64(6382)
		sc, mr, placed, plan := setup(t, playerID)

		restored, echo := rollbackPlayerPlacement(sc, log, playerID, placed, plan)
		require.True(t, restored)
		require.Equal(t, uint64(3), echo)

		// 迟到投递的路由让 B 载入(N = 2):owner_epoch 已是 3,A2′ 回 -1,B 拒建实体、发不出 DBTask(2)。
		assert.Equal(t, "-1", inheritClear(t, sc, playerID, 2))
		forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		assert.Equal(t, plan.forwardMarker, forwarded, "核对不过一个标记都不删:转写标记留给源端取证 / 改派")
		assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID))

		// 源端之后按回滚后的 epoch 重载(同落点重连,N = 3)时,A2′ 删掉转写标记(代际恰是 N,结果码 2)。
		assert.Equal(t, "2", inheritClear(t, sc, playerID, 3))
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)))
	})
}

// 源端原子取证(§12.8 3.1):只删本次交接这一族标记(原标记与回滚转写出来的那份),别人的不碰;
// 同一段脚本里读到的 location 带着与本次标记原文逐字节相同的回滚回执。
func TestSourceJudgeScriptDeletesOnlyItsOwnLineageAndSeesReceipt(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const savedAt = "1757000000000"
	judge := func(t *testing.T, playerID uint64) []any {
		t.Helper()
		result, err := sc.Redis.Eval(cppLuaJudgeTravelOutcome,
			[]string{ownerepoch.HandoffKey(playerID), ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID)},
			savedAt)
		require.NoError(t, err)
		reply, ok := result.([]any)
		require.True(t, ok, "应答必须是数组,实际 %T", result)
		require.Len(t, reply, 2, "两元素:owner_epoch、location")
		return reply
	}

	t.Run("deletes_only_its_own_lineage", func(t *testing.T) {
		cases := []struct {
			mark    string
			deleted bool
		}{
			{"5:" + savedAt, true},
			{"7:" + savedAt, true},
			{"6:1757000009999", false},
			{"5:" + savedAt + "1", false},
			{"garbage", false},
		}
		for i, c := range cases {
			playerID := uint64(6391 + i)
			require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), c.mark))
			judge(t, playerID)
			assert.Equal(t, !c.deleted, mr.Exists(ownerepoch.HandoffKey(playerID)), "mark=%s", c.mark)
		}
	})

	t.Run("missing_keys_are_nil_not_errors", func(t *testing.T) {
		reply := judge(t, 6397)
		assert.Nil(t, reply[0], "owner_epoch 缺键:nil(C++ 按 0)")
		assert.Nil(t, reply[1], "location 缺键:nil(C++ 按解析失败)")
	})

	t.Run("rollback_receipt_is_visible_after_lineage_delete", func(t *testing.T) {
		const playerID = uint64(6398)
		marker := "5:" + savedAt
		old := &scene_manager.PlayerLocation{
			SceneId: 7398, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 5, UpdateTime: 1_757_000_000,
		}
		oldBytes, err := gproto.Marshal(old)
		require.NoError(t, err)
		require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), string(oldBytes)))
		require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), "5"))
		require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), marker))

		// 凭 "5:t" 铸出 6(落点节点 20),推路由失败:bump 回滚到 7,回执 "5:t",标记转写成 "7:t"。
		placed, err := placePlayerLocation(sc, playerID, 7399, "20", testZoneId,
			placementGuard{observedEpoch: 5, mint: true, requiredMarker: marker})
		require.NoError(t, err)
		require.Equal(t, uint64(6), placed.epoch)
		plan, err := planRouteRollback(old, string(oldBytes), testZoneId, placed, marker, false)
		require.NoError(t, err)
		restored, echo := rollbackPlayerPlacement(sc, NewEnterSceneLogic(context.Background(), sc).Logger, playerID, placed, plan)
		require.True(t, restored)
		require.Equal(t, uint64(7), echo)
		forwarded, _ := sc.Redis.Get(ownerepoch.HandoffKey(playerID))
		require.Equal(t, "7:"+savedAt, forwarded)

		reply := judge(t, playerID)
		assert.False(t, mr.Exists(ownerepoch.HandoffKey(playerID)), "转写标记与原标记同一族:一并删掉")
		assert.Equal(t, "7", reply[0])
		locRaw, ok := reply[1].(string)
		require.True(t, ok, "location 必须是字符串,实际 %T", reply[1])
		assert.Equal(t, plan.restoredRaw, locRaw)
		loc := &scene_manager.PlayerLocation{}
		require.NoError(t, gproto.Unmarshal([]byte(locRaw), loc))
		assert.Equal(t, marker, loc.RollbackReceipt,
			"回执 == 本次标记原文:C++ 按 HandoffRedisValue(markEpoch, requestedAtMs) 逐字节比对")
		assert.Equal(t, uint64(7), loc.OwnerEpoch, "location 记的 epoch 与键一致(ClassifyOwnership 的交叉校验)")
		assert.Equal(t, "10", loc.NodeId, "location 指回源节点")
		assert.Equal(t, testZoneId, loc.ZoneId)

		// 源端采纳之后,任何凭这一族标记的铸造都过不了门:标记已被取证原子删掉(铸造 Lua 回 -1)。
		result, err := sc.Redis.Eval(luaMintEpochAndSetLocation,
			[]string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)},
			"7", "late-bytes", "7:"+savedAt)
		require.NoError(t, err)
		assert.Equal(t, "-1", fmt.Sprint(result))
		assert.Equal(t, "7", ownerEpochRaw(t, sc, playerID))
	})
}
