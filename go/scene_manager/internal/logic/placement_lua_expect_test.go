package logic

// 两段落点 Lua 的 location 原文期望(ARGV[4] / ARGV[5],docs/design/scene-enter-chain-residuals.md §5.2 ③)。
// 直接 EVAL 脚本,覆盖 any / missing / present 三种模式、ARGV[4] 缺省、未知模式 fail-closed、不铸造版的重发
// 幂等分支、铸造版的重放识别仍先于原文检查。owner_epoch_test.go 里只传 3 个 ARGV 的旧用例不改,它们本身就是
// 「缺省 = any」的回归。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

func luaExpectKeys(playerID uint64) []string {
	return []string{ownerepoch.OwnerEpochKey(playerID), getPlayerLocationKey(playerID), ownerepoch.HandoffKey(playerID)}
}

// luaExpectEval 执行脚本并把返回值转成字符串(go-zero 返回 int64)。铸造脚本要求 ARGV[9] = 目标值,
// 这里按 mintLuaArgs 补齐(目标 = ARGV[1] + 1,与墙钟地板不起作用时相同),本文件只测 location 原文期望。
func luaExpectEval(t *testing.T, sc *svc.ServiceContext, script string, playerID uint64, args ...any) string {
	t.Helper()
	if script == luaMintEpochAndSetLocation {
		args = mintLuaArgs(args...)
	}
	result, err := sc.Redis.Eval(script, luaExpectKeys(playerID), args...)
	require.NoError(t, err)
	return fmt.Sprint(result)
}

// luaExpectSeed 摆 epoch 键与 location;loc 为空串时不写 location。
func luaExpectSeed(t *testing.T, sc *svc.ServiceContext, playerID uint64, epoch string, loc string) {
	t.Helper()
	require.NoError(t, sc.Redis.Set(ownerepoch.OwnerEpochKey(playerID), epoch))
	if loc != "" {
		require.NoError(t, sc.Redis.Set(getPlayerLocationKey(playerID), loc))
	}
}

func TestMintLuaLocationExpectModes(t *testing.T) {
	t.Run("argv4_omitted_is_any", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6801)
		luaExpectSeed(t, sc, playerID, "1", "someone-else")
		assert.Equal(t, "2", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "new", ""))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
	})
	for _, mode := range []string{"", "any"} {
		t.Run("explicit_any_"+mode, func(t *testing.T) {
			sc, _ := newTestSvcCtxWithWorldScenes(t)
			const playerID = uint64(6802)
			luaExpectSeed(t, sc, playerID, "1", "someone-else")
			assert.Equal(t, "2", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "new", "", mode, "ignored"))
			assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
		})
	}
	t.Run("missing_rejects_existing_location", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6803)
		luaExpectSeed(t, sc, playerID, "0", "appeared")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "0", "new", "", "missing", ""))
		assert.Equal(t, "appeared", identLocationRaw(t, sc, playerID))
		assert.Equal(t, "0", ownerEpochRaw(t, sc, playerID), "不符时不得 INCR")
	})
	t.Run("missing_accepts_absent_location", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6804)
		assert.Equal(t, "1", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "0", "new", "", "missing", ""))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
	})
	t.Run("present_accepts_observed_bytes", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6805)
		luaExpectSeed(t, sc, playerID, "4", "observed")
		assert.Equal(t, "5", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "4", "new", "", "present", "observed"))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
	})
	t.Run("present_rejects_rewritten_location", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6806)
		luaExpectSeed(t, sc, playerID, "4", "rewritten")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "4", "new", "", "present", "observed"))
		assert.Equal(t, "rewritten", identLocationRaw(t, sc, playerID))
		assert.Equal(t, "4", ownerEpochRaw(t, sc, playerID))
	})
	t.Run("present_rejects_deleted_location", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6807)
		luaExpectSeed(t, sc, playerID, "4", "")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "4", "new", "", "present", "observed"))
		assert.Equal(t, "", identLocationRaw(t, sc, playerID))
		assert.Equal(t, "4", ownerEpochRaw(t, sc, playerID))
	})
	t.Run("unknown_mode_fails_closed", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6808)
		luaExpectSeed(t, sc, playerID, "1", "observed")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "new", "", "bogus", "observed"))
		assert.Equal(t, "observed", identLocationRaw(t, sc, playerID))
	})
	t.Run("location_mismatch_checked_before_marker", func(t *testing.T) {
		// 原文不符与标记不符同时成立时回 0(原文检查在标记比对之前),不是 -1。
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6809)
		luaExpectSeed(t, sc, playerID, "1", "rewritten")
		require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), "1:999"))
		assert.Equal(t, "0", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "new", "1:1757000000000", "present", "observed"))
	})
}

// 铸造版的重放识别(epoch 已前进一格、location 已是本请求要写的值、标记仍在)排在原文检查之前,
// 加了原文期望之后行为不变:重发时 location 已不等于观察原文,也照样认成本请求已生效。
func TestMintLuaReplayRecognitionPrecedesLocationExpect(t *testing.T) {
	sc, _ := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID = uint64(6810)
		marker   = "1:1757000000000"
	)
	luaExpectSeed(t, sc, playerID, "1", "observed")
	require.NoError(t, sc.Redis.Set(ownerepoch.HandoffKey(playerID), marker))

	assert.Equal(t, "2", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "placed", marker, "present", "observed"))
	// go-redis 原样重发同一条 EVAL:epoch 已是 2、location 已是 placed。
	assert.Equal(t, "-2", luaExpectEval(t, sc, luaMintEpochAndSetLocation, playerID, "1", "placed", marker, "present", "observed"),
		"重放识别必须先于原文检查,否则会被误判成并发推进")
	assert.Equal(t, "2", ownerEpochRaw(t, sc, playerID), "重发不得再次 INCR")
}

func TestSetLocationLuaLocationExpectModes(t *testing.T) {
	t.Run("argv4_omitted_is_any", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6821)
		luaExpectSeed(t, sc, playerID, "3", "someone-else")
		assert.Equal(t, "1", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", ""))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
	})
	t.Run("epoch_mismatch_rejects_first", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6822)
		luaExpectSeed(t, sc, playerID, "4", "new")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "present", "observed"),
			"epoch 不符时即使 location 已是要写的值也回 0")
	})
	t.Run("already_written_is_idempotent", func(t *testing.T) {
		// go-redis 原样重发:首发已写,location 已是 ARGV[2],不再等于观察原文 —— 认成功,一个字节都不改。
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6823)
		luaExpectSeed(t, sc, playerID, "3", "new")
		assert.Equal(t, "1", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "present", "observed"))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
		assert.Equal(t, "1", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "missing", ""))
	})
	t.Run("present_accepts_observed_bytes", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6824)
		luaExpectSeed(t, sc, playerID, "3", "observed")
		assert.Equal(t, "1", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "present", "observed"))
		assert.Equal(t, "new", identLocationRaw(t, sc, playerID))
		assert.Equal(t, "3", ownerEpochRaw(t, sc, playerID), "不铸造")
	})
	t.Run("present_rejects_rewritten_or_deleted_location", func(t *testing.T) {
		sc, mr := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6825)
		luaExpectSeed(t, sc, playerID, "3", "rewritten")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "present", "observed"))
		assert.Equal(t, "rewritten", identLocationRaw(t, sc, playerID))
		mr.Del(getPlayerLocationKey(playerID))
		assert.Equal(t, "0", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "present", "observed"))
		assert.False(t, mr.Exists(getPlayerLocationKey(playerID)), "不得复活已删的 location")
	})
	t.Run("missing_modes", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6826)
		luaExpectSeed(t, sc, playerID, "0", "appeared")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "0", "new", "", "missing", ""))
		assert.Equal(t, "appeared", identLocationRaw(t, sc, playerID))

		const absentPlayer = uint64(6827)
		assert.Equal(t, "1", luaExpectEval(t, sc, luaSetLocationIfEpoch, absentPlayer, "0", "new", "", "missing", ""))
		assert.Equal(t, "new", identLocationRaw(t, sc, absentPlayer))
	})
	t.Run("unknown_mode_fails_closed", func(t *testing.T) {
		sc, _ := newTestSvcCtxWithWorldScenes(t)
		const playerID = uint64(6828)
		luaExpectSeed(t, sc, playerID, "3", "observed")
		assert.Equal(t, "0", luaExpectEval(t, sc, luaSetLocationIfEpoch, playerID, "3", "new", "", "bogus", "observed"))
		assert.Equal(t, "observed", identLocationRaw(t, sc, playerID))
	})
}
