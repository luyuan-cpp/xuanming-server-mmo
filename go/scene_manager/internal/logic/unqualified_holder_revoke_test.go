package logic

// 无资格持有者的带围栏撤销(docs/design/location-release-binding.md §17)。
// 夹具沿用 leave_release_test.go(leaveReleaseSeed / leaveReleaseRequest / installLeaveReleaseRecorder / leaveReleaseCount /
// requireHolderReleasePending / leaveReleaseRedisNow)、enter_holder_release_test.go(enterHolderRelease* / requireHandoffRejectedUnchanged)、
// leavescene_cas_test.go(leaveCasValue / leaveCasLocationRaw)与 owner_epoch_crosslang_test.go(cpp* 金样、cppFenceRaw、
// crossLangRepoRoot);本文件的辅助标识符统一加 unqualifiedRevoke 前缀。
//
// 时钟:等待计时只读 Redis 的 TIME,用例一律用 mr.SetTime 钉死 miniredis 的服务端时钟,不依赖真实墙钟。
// 并发:按 Redis 实际执行顺序重放(先到的脚本先执行),不起 goroutine 赌调度。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proto/scene_manager"
	"scene_manager/internal/config"
	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
	"shared/handofffence"
	"shared/ownerepoch"
)

// unqualifiedRevokeFence 按 C++ 原文格式 "E|S|N|A|K|T" 拼一个资格原件(admission / token 各取一个十六进制字符重复 32 次)。
func unqualifiedRevokeFence(epoch uint64, instance, generation, admission, state, token string) string {
	return strings.Join([]string{strconv.FormatUint(epoch, 10), instance, generation, strings.Repeat(admission, 32), state,
		strings.Repeat(token, 32)}, "|")
}

// unqualifiedRevokePreviousFence 是上一任在 epoch-1 下登记、已冻结发布的资格原件:location 记着 (节点 10, epoch),
// 节点 10 却从未在 epoch 下登记成功时 handoff hash 里留着的就是它(§17.1 的典型形态)。
func unqualifiedRevokePreviousFence(epoch uint64) string {
	return unqualifiedRevokeFence(epoch-1, "scene-previous", "4", "a", "F", "b")
}

// unqualifiedRevokeGrace 是用例里生效的宽限:svcCtx 的 Config 不写这一项,取缺省值。
var unqualifiedRevokeGrace = time.Duration(config.DefaultUnqualifiedHolderRevokeGraceSeconds) * time.Second

func unqualifiedRevokeSetFence(mr *miniredis.Miniredis, playerID uint64, raw string) {
	mr.HSet(handofffence.Key(playerID), handofffence.HandoffFenceField, raw)
}

func unqualifiedRevokeFenceValue(mr *miniredis.Miniredis, playerID uint64) string {
	return mr.HGet(handofffence.Key(playerID), handofffence.HandoffFenceField)
}

// unqualifiedRevokeStartTimer 把等待计时的起点摆在 Redis 时钟 now 之前 waited:location 身份是 leaveReleaseSeed /
// enterHolderReleaseSeed 摆的 (节点 10, 会话 11, identGenA)。
func unqualifiedRevokeStartTimer(t *testing.T, mr *miniredis.Miniredis, playerID, epoch uint64, now time.Time, waited time.Duration) {
	t.Helper()
	mr.SetTime(now)
	scope := leaveReleaseWaitScope(epoch, &scene_manager.PlayerLocation{NodeId: "10", SessionId: 11, LoginGeneration: identGenA})
	require.NoError(t, mr.Set(leaveReleaseWaitKey(playerID), scope+":"+strconv.FormatInt(now.Add(-waited).UnixMilli(), 10)))
}

// ── 纯判定 ──────────────────────────────────────────────────────────────────

func TestDecideUnqualifiedRevoke_Table(t *testing.T) {
	eligible := unqualifiedRevokeFacts{graceElapsed: true, epoch: 7, locationEpoch: 7, fencePresent: true, fenceValid: true, fenceEpoch: 6}
	cases := []struct {
		name   string
		mutate func(*unqualifiedRevokeFacts)
		want   unqualifiedRevokeVerdict
	}{
		{"原件属于上一代", func(*unqualifiedRevokeFacts) {}, unqualifiedRevokeEligible},
		{"原件比 E 新:协议写者写不出,按数据不一致不撤销", func(f *unqualifiedRevokeFacts) { f.fenceEpoch = 9 }, unqualifiedRevokeFenceAhead},
		{"原件缺失(从未有人登记过)", func(f *unqualifiedRevokeFacts) { f.fencePresent, f.fenceValid, f.fenceEpoch = false, false, 0 },
			unqualifiedRevokeEligible},
		{"宽限未过最先判", func(f *unqualifiedRevokeFacts) { f.graceElapsed, f.epoch, f.fenceValid = false, 0, false }, unqualifiedRevokeNotDue},
		{"E 为 0", func(f *unqualifiedRevokeFacts) { f.epoch, f.locationEpoch = 0, 0 }, unqualifiedRevokeEpochUnproven},
		{"location 记的 E 与 owner_epoch 键不一致", func(f *unqualifiedRevokeFacts) { f.locationEpoch = 6 }, unqualifiedRevokeEpochUnproven},
		{"代际先于原件判", func(f *unqualifiedRevokeFacts) { f.locationEpoch, f.fenceValid = 6, false }, unqualifiedRevokeEpochUnproven},
		{"原件写坏", func(f *unqualifiedRevokeFacts) { f.fenceValid, f.fenceEpoch = false, 0 }, unqualifiedRevokeFenceMalformed},
		{"原件属于 E:有进程在 E 下登记过", func(f *unqualifiedRevokeFacts) { f.fenceEpoch = 7 }, unqualifiedRevokeQualified},
		{"owner_epoch 缺键(期望 E 为 0)时不拿 location 记的值补", func(f *unqualifiedRevokeFacts) { f.epoch = 0 },
			unqualifiedRevokeEpochUnproven},
		{"零值不撤销", func(f *unqualifiedRevokeFacts) { *f = unqualifiedRevokeFacts{} }, unqualifiedRevokeNotDue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := eligible
			tc.mutate(&facts)
			assert.Equal(t, tc.want, decideUnqualifiedRevoke(facts))
		})
	}
}

func TestUnqualifiedRevokeVerdictNamesCoverEveryValue(t *testing.T) {
	seen := map[string]bool{}
	for v := unqualifiedRevokeVerdict(0); v < unqualifiedRevokeVerdictCount; v++ {
		name := v.String()
		require.NotEmpty(t, name)
		require.NotEqual(t, "unknown", name)
		require.False(t, seen[name], "名字重复: %s", name)
		seen[name] = true
	}
	assert.Equal(t, "unknown", unqualifiedRevokeVerdictCount.String())

	// 指标取值与两族指标的既有取值都不撞(scene_manager_leave_holder_release_total / scene_manager_enter_holder_release_total)。
	for v := leaveReleaseVerdict(0); v < leaveReleaseVerdictCount; v++ {
		assert.NotEqual(t, v.String(), unqualifiedRevokeMetricVerdict)
	}
	for v := enterHolderReleaseVerdict(0); v < enterHolderReleaseVerdictCount; v++ {
		assert.NotEqual(t, v.String(), unqualifiedRevokeMetricVerdict)
	}
	assert.NotContains(t, []string{enterHolderReleaseOutcomeDelivered, enterHolderReleaseOutcomeFailed}, unqualifiedRevokeMetricVerdict)
}

// 宽限:不写 = 120s;写了必须在 (85, 600] 内(启动时拒绝越界值);没经过校验的值(单测直接构造 Config)按缺省处理。
func TestUnqualifiedHolderRevokeGraceConfig(t *testing.T) {
	var c config.Config
	require.NoError(t, c.ValidateUnqualifiedHolderRevokeGrace(), "不写 = 缺省,合法")
	assert.Equal(t, 120*time.Second, c.EffectiveUnqualifiedHolderRevokeGrace())

	for _, seconds := range []int64{86, 120, 600} {
		c.UnqualifiedHolderRevokeGraceSeconds = seconds
		require.NoError(t, c.ValidateUnqualifiedHolderRevokeGrace(), "%d", seconds)
		assert.Equal(t, time.Duration(seconds)*time.Second, c.EffectiveUnqualifiedHolderRevokeGrace())
	}
	for _, seconds := range []int64{-1, 1, 85, 601} {
		c.UnqualifiedHolderRevokeGraceSeconds = seconds
		require.Error(t, c.ValidateUnqualifiedHolderRevokeGrace(), "%d 必须拒启", seconds)
		assert.Equal(t, 120*time.Second, c.EffectiveUnqualifiedHolderRevokeGrace(), "%d 没过校验:按缺省", seconds)
	}
}

// 等待计时键只在第一次登记时设 TTL、之后不续,要活过诊断预算与撤销宽限里更长的那个,再加余量。只按预算设的话,预算配得比宽限
// 短时(这里 1s 对 600s)键在宽限走完后只剩 1s,清理每 30s 才重试一次,大概率错过、键过期从头计时,撤销永远轮不到。
func TestNoteLeaveReleaseWait_TTLCoversRevokeGrace(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	sc.Config.UnqualifiedHolderRevokeGraceSeconds = config.MaxUnqualifiedHolderRevokeGraceSeconds
	grace := time.Duration(config.MaxUnqualifiedHolderRevokeGraceSeconds) * time.Second
	const (
		playerID    = uint64(8338)
		otherPlayer = uint64(8339)
		scope       = "3:10:11:42"
	)
	mr.SetTime(leaveReleaseRedisNow)

	noteLeaveReleaseWait(sc, playerID, scope, time.Second)
	assert.Equal(t, grace+leaveReleaseWaitKeyMargin, mr.TTL(leaveReleaseWaitKey(playerID)), "预算比宽限短:按宽限")
	noteLeaveReleaseWait(sc, otherPlayer, scope, 2*grace)
	assert.Equal(t, 2*grace+leaveReleaseWaitKeyMargin, mr.TTL(leaveReleaseWaitKey(otherPlayer)), "预算比宽限长:按预算")

	// 宽限走完的那一刻计时还在、起点没变,之后还有整整一个余量可撤。
	mr.SetTime(leaveReleaseRedisNow.Add(grace))
	mr.FastForward(grace)
	assert.Equal(t, grace, noteLeaveReleaseWait(sc, playerID, scope, time.Second))
	assert.Equal(t, leaveReleaseWaitKeyMargin, mr.TTL(leaveReleaseWaitKey(playerID)))
}

// ── 跨语言口径 ──────────────────────────────────────────────────────────────

// 撤销 Lua 判「资格原件属于哪一代、写没写坏」用的是 C++ kFenceLuaHelpers 的逐字副本,与 C++ 登记 / 释放 / 凭证脚本同一口径。
// 金样 cppHandoffFenceHelpers 由 TestSourceJudgeScriptAndInheritClearCopiesMatchCpp 钉住 C++ 源码;这里再直接比一次源码,
// 只拷了 go/ 的构建环境里找不到 C++ 源码时只比金样。
func TestUnqualifiedRevokeFenceParsingMatchesCpp(t *testing.T) {
	require.Equal(t, cppHandoffFenceHelpers, luaCppHandoffFenceHelpers)
	data, err := os.ReadFile(filepath.Join(crossLangRepoRoot, "cpp/libs/services/scene/player/system/handoff_sql_barrier.h"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),只比金样")
	}
	require.NoError(t, err)
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	assert.Equal(t, cppFenceRaw(t, source, "kFenceLuaHelpers"), luaCppHandoffFenceHelpers)
}

// 宽限下界 85s = C++ 入场登记截止(role_admission_system.cpp registerDeadline)+ 交接冻结硬上限(travel_freeze_cap.h kFreezeCap)。
// 任何一边改了,这里先失败,提醒同步 config.MinUnqualifiedHolderRevokeGraceSeconds 与设计文档 §17.4。
func TestUnqualifiedRevokeGraceFloorMatchesCppDeadlines(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(crossLangRepoRoot, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过跨语言检查")
		}
		require.NoError(t, err)
		return string(src)
	}
	seconds := func(source string, pattern *regexp.Regexp, what string) int64 {
		t.Helper()
		m := pattern.FindStringSubmatch(source)
		require.Len(t, m, 2, "找不到 %s", what)
		v, err := strconv.ParseInt(m[1], 10, 64)
		require.NoError(t, err)
		return v
	}
	admission := seconds(read("cpp/libs/services/scene/player/system/role_admission_system.cpp"),
		regexp.MustCompile(`registerDeadline\s*=\s*Now\(\)\s*\+\s*std::chrono::seconds\((\d+)\)`), "入场登记截止")
	freeze := seconds(read("cpp/libs/services/scene/player/system/travel_freeze_cap.h"),
		regexp.MustCompile(`kFreezeCap\{(\d+)\}`), "交接冻结硬上限")
	assert.Equal(t, admission+freeze, config.MinUnqualifiedHolderRevokeGraceSeconds)
}

// ── 撤销 Lua ────────────────────────────────────────────────────────────────

// unqualifiedRevokeLuaSeed 摆出撤销 Lua 的标准合格态:location (节点 10, E=7) 带 §16 侧车与 floor、owner_epoch 7、
// 资格原件是上一任在 6 下的 F、没有标记、等待计时在、场景与节点 10 的人数各 2。返回撤销目标与观察到的资格原件。
func unqualifiedRevokeLuaSeed(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64) (unqualifiedRevokeTarget, string) {
	t.Helper()
	loc := &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 7, UpdateTime: 100,
		SessionId: 11, LoginGeneration: identGenA}
	raw := leaveCasLocationRaw(t, loc)
	tag := locationRevisionTag(1, raw)
	require.NoError(t, mr.Set(getPlayerLocationKey(playerID), raw))
	require.NoError(t, mr.Set(locationRevisionKey(playerID), tag))
	require.NoError(t, mr.Set(locationRevisionFloorKey(playerID), revisionText(1)))
	require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "7"))
	fence := unqualifiedRevokePreviousFence(7)
	unqualifiedRevokeSetFence(mr, playerID, fence)
	require.NoError(t, mr.Set(leaveReleaseWaitKey(playerID), leaveReleaseWaitScope(7, loc)+":1790000000000"))
	seedSceneMembers(mr, sceneID, 2, playerID)
	require.NoError(t, mr.Set(sceneNodeKey(sceneID), "10"))
	seedNodeMembers(mr, testZoneId, "10", 2, playerID)
	return unqualifiedRevokeTarget{playerID: playerID, loc: loc, locationRaw: raw, revisionTag: tag, ownerRevision: 1,
		zoneID: testZoneId, epoch: 7}, fence
}

// 撤销 Lua 的每个返回码(§17.5):合格时推进围栏一格、删 location / 等待计时 / §16 侧车(floor 保留)、扣一次场景与节点人数,
// 资格原件一个字节不动;任何一项复核不过都一个字节不写。
func TestRevokeUnqualifiedHolderLua_Outcomes(t *testing.T) {
	cases := []struct {
		name string
		// arrange 改动摆好的合格态;返回值非空时替换「调用方观察到的资格原件」。
		arrange func(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64) (observedFence string, replace bool)
		want    string
	}{
		{"原件属于上一代", nil, unqualifiedRevokeOutcomeRevoked},
		{"原件缺失(从未有人登记过)", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			mr.HDel(handofffence.Key(playerID), handofffence.HandoffFenceField)
			return "", true
		}, unqualifiedRevokeOutcomeRevoked},
		{"更早一代的标记不挡", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "6:1757000000000"))
			return "", false
		}, unqualifiedRevokeOutcomeRevoked},
		{"节点映射已不在:只扣场景人数", func(_ *testing.T, mr *miniredis.Miniredis, _ uint64, sceneID uint64) (string, bool) {
			mr.Del(sceneNodeKey(sceneID))
			return "", false
		}, unqualifiedRevokeOutcomeRevoked},
		{"location 已被删掉", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			mr.Del(getPlayerLocationKey(playerID))
			return "", false
		}, unqualifiedRevokeOutcomeAlreadyGone},
		{"location 原字节已变", func(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64) (string, bool) {
			require.NoError(t, mr.Set(getPlayerLocationKey(playerID), leaveCasLocationRaw(t, &scene_manager.PlayerLocation{
				SceneId: sceneID, NodeId: "20", ZoneId: testZoneId, OwnerEpoch: 7, UpdateTime: 200})))
			return "", false
		}, unqualifiedRevokeOutcomeSuperseded},
		{"§16 侧车已变", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			raw, err := mr.Get(getPlayerLocationKey(playerID))
			require.NoError(t, err)
			require.NoError(t, mr.Set(locationRevisionKey(playerID), locationRevisionTag(2, raw)))
			return "", false
		}, unqualifiedRevokeOutcomeSuperseded},
		{"修订 floor 已推进", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(locationRevisionFloorKey(playerID), revisionText(2)))
			return "", false
		}, unqualifiedRevokeOutcomeSuperseded},
		{"owner_epoch 已推进", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "8"))
			return "", false
		}, unqualifiedRevokeOutcomeEpochMoved},
		{"owner_epoch 缺键:不能 INCR 出 1", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			mr.Del(ownerepoch.OwnerEpochKey(playerID))
			return "", false
		}, unqualifiedRevokeOutcomeEpochMoved},
		{"观察之后有登记在 E 下落地", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokeFence(7, "scene-x", "5", "c", "H", "d"))
			return "", false
		}, unqualifiedRevokeOutcomeFenceChanged},
		{"观察到的原件就属于 E(Go 预判之外的纵深防御)", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			held := unqualifiedRevokeFence(7, "scene-x", "5", "c", "H", "d")
			unqualifiedRevokeSetFence(mr, playerID, held)
			return held, true
		}, unqualifiedRevokeOutcomeFenceNotProof},
		{"观察到的原件比 E 新,且位数更多(按数值比,不按字典序)", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			ahead := unqualifiedRevokeFence(10, "scene-x", "5", "c", "H", "d")
			unqualifiedRevokeSetFence(mr, playerID, ahead)
			return ahead, true
		}, unqualifiedRevokeOutcomeFenceNotProof},
		{"原件写坏", func(_ *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			unqualifiedRevokeSetFence(mr, playerID, "garbage")
			return "garbage", true
		}, unqualifiedRevokeOutcomeFenceNotProof},
		{"E 代标记在", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "7:1757000000000"))
			return "", false
		}, unqualifiedRevokeOutcomeMarkerPresent},
		{"E 代标记带前导零", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "007:1757000000000"))
			return "", false
		}, unqualifiedRevokeOutcomeMarkerPresent},
		{"标记写坏", func(t *testing.T, mr *miniredis.Miniredis, playerID, _ uint64) (string, bool) {
			require.NoError(t, mr.Set(ownerepoch.HandoffKey(playerID), "bad"))
			return "", false
		}, unqualifiedRevokeOutcomeMarkerPresent},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t, "")
			playerID, sceneID := uint64(8341)+uint64(i), uint64(9441)+uint64(i)
			target, observed := unqualifiedRevokeLuaSeed(t, mr, playerID, sceneID)
			if tc.arrange != nil {
				if fence, replace := tc.arrange(t, mr, playerID, sceneID); replace {
					observed = fence
				}
			}
			nodeMapped := mr.Exists(sceneNodeKey(sceneID))
			fenceBefore := unqualifiedRevokeFenceValue(mr, playerID)
			before := mr.Dump()

			outcome, err := runUnqualifiedRevoke(sc, target, observed)
			require.NoError(t, err)
			require.Equal(t, tc.want, outcome)
			if tc.want == unqualifiedRevokeOutcomeAlreadyGone {
				// 第一步别的调用已做完,这里只补第二步:摘掉成员,其余一个字节都不写。
				assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
				assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
				member := strconv.FormatUint(playerID, 10)
				_, err = mr.SetAdd(instanceMembersKey(sceneID), member)
				require.NoError(t, err)
				_, err = mr.SetAdd(nodeMembersKey(testZoneId, "10"), member)
				require.NoError(t, err)
				assert.Equal(t, before, mr.Dump(), "除成员之外一个字节都不写")
				return
			}
			if tc.want != unqualifiedRevokeOutcomeRevoked {
				assert.Equal(t, before, mr.Dump(), "没撤销就一个字节都不写")
				return
			}
			assert.Equal(t, "8", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)), "围栏推进一格:E 下的一切写此后都被 owner CAS 拒绝")
			assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
			assert.False(t, mr.Exists(leaveReleaseWaitKey(playerID)), "等待计时随 location 一起删")
			assert.False(t, mr.Exists(locationRevisionKey(playerID)), "§16 侧车随 location 一起删")
			assert.Equal(t, revisionText(1), leaveCasValue(t, mr, locationRevisionFloorKey(playerID)), "floor 不随位置删除")
			assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
			if nodeMapped {
				assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
			} else {
				assert.Equal(t, "2", nodeMemberCount(mr, testZoneId, "10"), "找不到节点就不扣节点人数")
			}
			assert.Equal(t, fenceBefore, unqualifiedRevokeFenceValue(mr, playerID), "资格原件一个字节都不动")

			// 两个副本拿着同一份观察并发撤销(或调用方重发同一条 EVAL):后到的看到 location 已不在,不再推进、不再扣人数。
			afterFirst := mr.Dump()
			outcome, err = runUnqualifiedRevoke(sc, target, observed)
			require.NoError(t, err)
			assert.Equal(t, unqualifiedRevokeOutcomeAlreadyGone, outcome)
			assert.Equal(t, afterFirst, mr.Dump())
		})
	}
}

// 参数形状不对(E 为 0)= Go 与 Lua 的约定被改岔:报错、一个字节都不写,不猜一个值去 INCR。
func TestRevokeUnqualifiedHolderLua_RejectsMalformedArguments(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	target, observed := unqualifiedRevokeLuaSeed(t, mr, 8361, 9461)
	target.epoch = 0
	before := mr.Dump()
	_, err := runUnqualifiedRevoke(sc, target, observed)
	require.Error(t, err)
	assert.Equal(t, before, mr.Dump())
}

// 与在途入场登记的竞态(§17.6 第 2 条),用 C++ kRegisterFenceLua 的金样在同一个 Redis 上按两种先后执行:
//   - 登记先落地:资格原件变成 E 下的 H,撤销复核原件不过,回 fence_changed,一个字节都不写;
//   - 撤销先落地:owner_epoch 已是 E+1,登记脚本比 owner 失败(-1),资格原件不动 —— 那次入场失败,客户端重登走新铸造。
//
// 两种先后都不会出现两个持有者。
func TestRevokeUnqualifiedHolderRacesWithRegistration(t *testing.T) {
	register := func(t *testing.T, sc *svc.ServiceContext, playerID uint64, candidate, prior string) string {
		t.Helper()
		keys := []string{ownerepoch.OwnerEpochKey(playerID), handofffence.Key(playerID), ownerepoch.HandoffKey(playerID)}
		v, err := sc.Redis.Eval(cppLuaRegisterFence, keys, "7", candidate, prior)
		require.NoError(t, err)
		return fmt.Sprint(v)
	}
	candidate := unqualifiedRevokeFence(7, "scene-x", "5", "c", "H", "d")

	t.Run("registration_first", func(t *testing.T) {
		sc, mr := newTestSvcCtx(t, "")
		const playerID = uint64(8371)
		target, observed := unqualifiedRevokeLuaSeed(t, mr, playerID, 9471)
		require.Equal(t, "1", register(t, sc, playerID, candidate, observed))
		before := mr.Dump()

		outcome, err := runUnqualifiedRevoke(sc, target, observed)
		require.NoError(t, err)
		assert.Equal(t, unqualifiedRevokeOutcomeFenceChanged, outcome)
		assert.Equal(t, before, mr.Dump(), "登记已落地:撤销一个字节都不写")
		assert.Equal(t, candidate, unqualifiedRevokeFenceValue(mr, playerID))
	})

	t.Run("revoke_first", func(t *testing.T) {
		sc, mr := newTestSvcCtx(t, "")
		const playerID = uint64(8372)
		target, observed := unqualifiedRevokeLuaSeed(t, mr, playerID, 9472)
		outcome, err := runUnqualifiedRevoke(sc, target, observed)
		require.NoError(t, err)
		require.Equal(t, unqualifiedRevokeOutcomeRevoked, outcome)
		before := mr.Dump()

		assert.Equal(t, "-1", register(t, sc, playerID, candidate, observed), "owner 已推进:E 下的登记被拒")
		assert.Equal(t, before, mr.Dump())
		assert.Equal(t, observed, unqualifiedRevokeFenceValue(mr, playerID))
	})
}

// 「采纳 E+2」窗口(§17.6 第 3 条):交接回滚把 location 推到 (源节点 10, 9),源端取证已把资格撤成 7 下的 R、删掉了标记,
// 采纳(kResumeFenceLua 把 R 恢复成 9 下的 H)还没执行。用 C++ 金样按两种先后执行:
//   - 采纳先落地:原件变成 9 下的 H,撤销回 fence_changed,源实体照常解冻;
//   - 撤销先落地:采纳比 owner 失败(0),源实体保持冻结到硬上限后销毁 —— 冻结存盘在发布交接标记之前已两表落地,不丢进度。
func TestRevokeUnqualifiedHolderRacesWithTravelAdoption(t *testing.T) {
	seed := func(t *testing.T, mr *miniredis.Miniredis, playerID, sceneID uint64) (unqualifiedRevokeTarget, string, string, string) {
		t.Helper()
		target, _ := unqualifiedRevokeLuaSeed(t, mr, playerID, sceneID)
		loc := &scene_manager.PlayerLocation{SceneId: sceneID, NodeId: "10", ZoneId: testZoneId, OwnerEpoch: 9, UpdateTime: 300,
			SessionId: 11, LoginGeneration: identGenA, RollbackReceipt: "7:1757000000000"}
		raw := leaveCasLocationRaw(t, loc)
		require.NoError(t, mr.Set(getPlayerLocationKey(playerID), raw))
		mr.Del(locationRevisionKey(playerID))
		require.NoError(t, mr.Set(ownerepoch.OwnerEpochKey(playerID), "9"))
		revoked := unqualifiedRevokeFence(7, "scene-x", "3", "c", "R", "d")
		unqualifiedRevokeSetFence(mr, playerID, revoked)
		target.loc, target.locationRaw, target.revisionTag, target.epoch = loc, raw, "", 9
		return target, revoked, unqualifiedRevokeFence(9, "scene-x", "3", "c", "H", "e"), raw
	}
	resume := func(t *testing.T, sc *svc.ServiceContext, playerID uint64, revoked, next, raw string) string {
		t.Helper()
		keys := []string{ownerepoch.OwnerEpochKey(playerID), handofffence.Key(playerID), ownerepoch.HandoffKey(playerID),
			getPlayerLocationKey(playerID)}
		v, err := sc.Redis.Eval(cppLuaResumeFence, keys, revoked, next, "9", raw)
		require.NoError(t, err)
		return fmt.Sprint(v)
	}

	t.Run("adoption_first", func(t *testing.T) {
		sc, mr := newTestSvcCtx(t, "")
		const playerID = uint64(8381)
		target, revoked, next, raw := seed(t, mr, playerID, 9481)
		require.Equal(t, "1", resume(t, sc, playerID, revoked, next, raw))
		before := mr.Dump()
		outcome, err := runUnqualifiedRevoke(sc, target, revoked)
		require.NoError(t, err)
		assert.Equal(t, unqualifiedRevokeOutcomeFenceChanged, outcome)
		assert.Equal(t, before, mr.Dump())
	})

	t.Run("revoke_first", func(t *testing.T) {
		sc, mr := newTestSvcCtx(t, "")
		const playerID = uint64(8382)
		target, revoked, next, raw := seed(t, mr, playerID, 9482)
		outcome, err := runUnqualifiedRevoke(sc, target, revoked)
		require.NoError(t, err)
		require.Equal(t, unqualifiedRevokeOutcomeRevoked, outcome)
		assert.Equal(t, "10", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
		before := mr.Dump()
		assert.Equal(t, "0", resume(t, sc, playerID, revoked, next, raw), "owner 已推进:采纳不成立")
		assert.Equal(t, before, mr.Dump())
	})
}

// ── LeaveScene 入口 ─────────────────────────────────────────────────────────

// unqualifiedRevokeStoreFence 是一次被截下的存档写权行围栏调用,连同调用那一刻 Redis 的状态:用来证明写权行先于撤销 Lua 围好。
type unqualifiedRevokeStoreFence struct {
	ownerEpoch       uint64 // 传给 ReleaseDeadPlayerStoreHolder 的代次
	locationAtCall   bool   // 调用时 location 还在
	ownerEpochAtCall string // 调用时 owner_epoch 键的值
}

// installUnqualifiedRevokeStoreFenceRecorder 把 PlayerStoreHolders 换成记录器,每次返回 *replyErr(用例可以中途改它)。
func installUnqualifiedRevokeStoreFenceRecorder(t *testing.T, sc *svc.ServiceContext, mr *miniredis.Miniredis, playerID uint64,
	replyErr *error) func() []unqualifiedRevokeStoreFence {
	t.Helper()
	var calls []unqualifiedRevokeStoreFence
	sc.PlayerStoreHolders = releaserFunc(func(_ context.Context, player, ownerEpoch uint64) error {
		require.Equal(t, playerID, player)
		calls = append(calls, unqualifiedRevokeStoreFence{ownerEpoch: ownerEpoch,
			locationAtCall:   mr.Exists(getPlayerLocationKey(playerID)),
			ownerEpochAtCall: leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID))})
		return *replyErr
	})
	return func() []unqualifiedRevokeStoreFence { return append([]unqualifiedRevokeStoreFence(nil), calls...) }
}

// releaserFunc 把函数适配成 svc.PlayerStoreHolderReleaser,只给用例截下调用。
type releaserFunc func(ctx context.Context, player, ownerEpoch uint64) error

func (f releaserFunc) ReleaseDeadPlayerStoreHolder(ctx context.Context, player, ownerEpoch uint64) error {
	return f(ctx, player, ownerEpoch)
}

// 主时序(§17.1):location 记着 (节点 10, E=3),节点 10 从未在 3 下登记成功,资格原件还是上一任在 2 下的。
// 宽限内照旧保留 location、请持有节点释放、回 FailedPrecondition;宽限走完后的那次重试先请 data_service 按 E−1=2 围存档写权行
// (≤2 时抬到 3、清持有者、released=1),此刻 location 与 owner_epoch 都还没动;再推进围栏(owner_epoch 3 → 4)、删 location
// 与等待计时、扣一次人数、计 revoked_unqualified,回成功(清理完成,不再请持有节点释放)。资格原件一个字节都不动。
func TestLeaveScene_UnqualifiedHolderRevokedAfterGrace(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8331)
		sceneID  = uint64(9431)
	)
	var storeErr error
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	fence := unqualifiedRevokePreviousFence(3)
	unqualifiedRevokeSetFence(mr, playerID, fence)
	t0 := leaveReleaseRedisNow
	mr.SetTime(t0)
	revokedBefore := leaveReleaseCount(t, unqualifiedRevokeMetricVerdict)
	leave := func() error {
		_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
		return err
	}

	requireHolderReleasePending(t, leave())
	mr.SetTime(t0.Add(unqualifiedRevokeGrace - time.Millisecond))
	requireHolderReleasePending(t, leave())
	assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)), "宽限内不撤销")
	assert.True(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Len(t, calls(), 2, "宽限内照常请持有节点释放")
	assert.Empty(t, storeFences(), "宽限内不碰存档写权行")

	mr.SetTime(t0.Add(unqualifiedRevokeGrace))
	require.NoError(t, leave(), "撤销之后这次清理完成")
	assert.Equal(t, []unqualifiedRevokeStoreFence{{ownerEpoch: 2, locationAtCall: true, ownerEpochAtCall: "3"}}, storeFences(),
		"先按 E−1 围写权行(真源在前),这时 location 与 owner_epoch 还没动")
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Equal(t, "1", sceneMemberCount(mr, sceneID))
	assert.Equal(t, "1", nodeMemberCount(mr, testZoneId, "10"))
	assert.False(t, mr.Exists(leaveReleaseWaitKey(playerID)))
	assert.True(t, mr.Exists(locationRevisionFloorKey(playerID)), "floor 不随位置删除")
	assert.Equal(t, fence, unqualifiedRevokeFenceValue(mr, playerID), "资格原件不动")
	assert.Equal(t, revokedBefore+1, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))
	assert.Len(t, calls(), 2, "撤销那一次不再请释放")

	// 迟到的重试:location 已不在,回成功,不再推进、不再计数,也不再碰写权行。
	require.NoError(t, leave())
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Equal(t, revokedBefore+1, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))
	assert.Len(t, storeFences(), 1)
}

// 围写权行失败(data_service 暂时不可用):这次不撤销,location 与 owner_epoch 一个字节不动、照常请持有节点释放、回
// FailedPrecondition(等持有者释放),不计 revoked_unqualified;data_service 恢复后的下一次重试照常撤销。
// 不把顺序倒过来先撤销再补写权行:那样两步之间没有原子性,写权行那一步失败就要另记一笔欠账去补。
func TestLeaveScene_UnqualifiedRevokeWaitsWhenPlayerStoreFenceFails(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8334)
		sceneID  = uint64(9434)
	)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokePreviousFence(3))
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace)
	storeErr := errors.New("data_service down")
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	revokedBefore := leaveReleaseCount(t, unqualifiedRevokeMetricVerdict)
	leave := func() error {
		_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
		return err
	}

	requireHolderReleasePending(t, leave())
	assert.Len(t, storeFences(), 1, "试过围写权行")
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "围不成就不撤销")
	assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Len(t, calls(), 1, "照常请持有节点释放")
	assert.Equal(t, revokedBefore, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))

	storeErr = nil
	require.NoError(t, leave(), "data_service 恢复后照常撤销")
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Len(t, storeFences(), 2, "幂等:重试再围一次")
	assert.Equal(t, revokedBefore+1, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))
}

// 围写权行这一步本身:按 E−1 调;E ≤ 1 不调(建角写下的写权行就是 (1, 无持有者, released=1),没有更早的代次要挡);
// 没装配 PlayerStoreHolders(单测宿主)不调;data_service 报错原样带回(调用方不撤销)。
func TestFenceUnclaimedPlayerStore(t *testing.T) {
	const playerID = uint64(8336)
	cases := []struct {
		name       string
		epoch      uint64
		holders    *fakePlayerStoreHolders
		wantErr    bool
		wantEpochs []uint64
	}{
		{name: "按 E−1 调", epoch: 3, holders: &fakePlayerStoreHolders{}, wantEpochs: []uint64{2}},
		{name: "墙钟量级的代次", epoch: 1790000000000, holders: &fakePlayerStoreHolders{}, wantEpochs: []uint64{1789999999999}},
		{name: "E 为 1 不调", epoch: 1, holders: &fakePlayerStoreHolders{}},
		{name: "data_service 报错", epoch: 3, holders: &fakePlayerStoreHolders{err: errors.New("down")}, wantErr: true,
			wantEpochs: []uint64{2}},
		{name: "没装配不调", epoch: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, _ := newTestSvcCtx(t, "")
			if tc.holders != nil {
				sc.PlayerStoreHolders = tc.holders
			}
			err := fenceUnclaimedPlayerStore(context.Background(), sc, playerID, tc.epoch)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.holders != nil {
				assert.Equal(t, tc.wantEpochs, tc.holders.epochs)
			}
		})
	}
}

// 资格原件缺失(新角色第一次落点就没登记成功,handoff hash 里从来没有资格)与「属于上一代」同理:撤销。
func TestLeaveScene_UnqualifiedHolderWithoutAnyFenceRevoked(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8332)
		sceneID  = uint64(9432)
	)
	leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.NoError(t, err)
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.False(t, mr.Exists(handofffence.Key(playerID)), "撤销不替任何进程写资格")
}

// 宽限走完也不撤销的情形:资格原件证明有进程在 E 下登记过(正常持有 / 已释放而标记过期 / 登记被撤销 —— 都交给凭证与 GO-3)、
// 原件写坏、读不出或比 E 新、离场身份是 legacy(本次没有请持有节点释放)。location 原样保留、存档写权行不碰,照旧回 FailedPrecondition。
func TestLeaveScene_UnqualifiedRevokeRequiresProof(t *testing.T) {
	cases := []struct {
		name string
		// arrange 摆资格原件(或把屏障键弄坏);request 为空时用离场会话 (11, identGenA) 的请求。
		arrange      func(t *testing.T, mr *miniredis.Miniredis, playerID uint64)
		request      func(playerID, sceneID uint64) *scene_manager.LeaveSceneRequest
		wantReleases int
	}{
		{name: "E 下正在持有", arrange: func(_ *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokeFence(3, "scene-x", "5", "c", "H", "d"))
		}, wantReleases: 1},
		{name: "E 下已释放而标记过期", arrange: func(_ *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokeFence(3, "scene-x", "5", "c", "F", "d"))
		}, wantReleases: 1},
		{name: "E 下的登记被撤销", arrange: func(_ *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokeFence(3, "scene-x", "5", "c", "R", "d"))
		}, wantReleases: 1},
		{name: "原件写坏", arrange: func(_ *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			unqualifiedRevokeSetFence(mr, playerID, "garbage")
		}, wantReleases: 1},
		{name: "原件比 E 新(数据被动过)", arrange: func(_ *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokeFence(4, "scene-x", "5", "c", "H", "d"))
		}, wantReleases: 1},
		{name: "屏障键类型不对:读不出资格", arrange: func(t *testing.T, mr *miniredis.Miniredis, playerID uint64) {
			require.NoError(t, mr.Set(handofffence.Key(playerID), "not-a-hash"))
		}, wantReleases: 1},
		{name: "legacy 离场身份:没请过释放", arrange: func(*testing.T, *miniredis.Miniredis, uint64) {},
			request: func(playerID, sceneID uint64) *scene_manager.LeaveSceneRequest {
				return &scene_manager.LeaveSceneRequest{PlayerId: playerID, SceneId: sceneID}
			}, wantReleases: 0},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t, "")
			calls := installLeaveReleaseRecorder(t)
			playerID, sceneID := uint64(8391)+uint64(i), uint64(9491)+uint64(i)
			raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
			tc.arrange(t, mr, playerID)
			unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace+time.Minute)
			var storeErr error
			storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
			revokedBefore := leaveReleaseCount(t, unqualifiedRevokeMetricVerdict)
			req := leaveReleaseRequest(playerID, sceneID)
			if tc.request != nil {
				req = tc.request(playerID, sceneID)
			}

			_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(req)
			requireHolderReleasePending(t, err)
			assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "没有正证:location 原样保留")
			assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)), "没有正证:不推进围栏")
			assert.Empty(t, storeFences(), "没有正证:不碰存档写权行")
			assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
			assert.Len(t, calls(), tc.wantReleases)
			assert.Equal(t, revokedBefore, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))
		})
	}
}

// owner_epoch 键缺失(被单独删、数据被动过):LeaveScene 的释放判定退回 location 记的 E 继续等、照常请释放,但撤销不拿 location
// 自己记的值当证据 —— 预判 epoch_unproven,不读资格原件、不围存档写权行、不发撤销 Lua,也不替缺键补值。
// 以前传的是退回后的 E,预判照样放行,每次重试都白围一次写权行、再被撤销 Lua 以 -2 拒掉。
func TestLeaveScene_UnqualifiedRevokeNeedsOwnerEpochKey(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8337)
		sceneID  = uint64(9437)
	)
	var storeErr error
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokePreviousFence(3))
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace+time.Minute)
	mr.Del(ownerepoch.OwnerEpochKey(playerID))
	revokedBefore := leaveReleaseCount(t, unqualifiedRevokeMetricVerdict)

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)), "location 原样保留")
	assert.False(t, mr.Exists(ownerepoch.OwnerEpochKey(playerID)), "不替缺键补值")
	assert.Empty(t, storeFences(), "没有代际证据就不碰存档写权行")
	assert.Equal(t, "2", sceneMemberCount(mr, sceneID))
	assert.Len(t, calls(), 1, "释放判定照常请持有节点释放")
	assert.Equal(t, revokedBefore, leaveReleaseCount(t, unqualifiedRevokeMetricVerdict))
}

// 预判合格之后、撤销 Lua 执行之前,持有节点在 E 下的入场登记先落地(资格原件变成 E 下的 H):Lua 复核原件不过,一个字节不写,
// 这次清理照旧按 pending 回拒、照旧请持有节点释放。写权行此前已按 E−1 围过(停在「E、无持有者」),新登记的 X 按 E 认领照常成功,
// 所以先围写权行、后撤销落空是无害的。
func TestLeaveScene_UnqualifiedRevokeLosesToRegistrationLandingFirst(t *testing.T) {
	sc, mr := newTestSvcCtx(t, "")
	calls := installLeaveReleaseRecorder(t)
	const (
		playerID = uint64(8333)
		sceneID  = uint64(9433)
	)
	var storeErr error
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	raw := leaveReleaseSeed(t, mr, playerID, sceneID, "10", 3)
	unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokePreviousFence(3))
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace)
	held := unqualifiedRevokeFence(3, "scene-x", "5", "c", "H", "d")
	handoffKey := handofffence.Key(playerID)

	// 只有撤销 Lua 的 KEYS 里有 handoff hash 这个键;钩子在它执行之前把登记的效果摆上(先消费一次性标志再改数据,做法同 leave_release_test.go)。
	var fired atomic.Bool
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if (cmd != "EVAL" && cmd != "EVALSHA") || !slices.Contains(args, handoffKey) || !fired.CompareAndSwap(false, true) {
			return false
		}
		mr.HSet(handoffKey, handofffence.HandoffFenceField, held)
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	_, err := NewLeaveSceneLogic(context.Background(), sc).LeaveScene(leaveReleaseRequest(playerID, sceneID))
	require.True(t, fired.Load(), "钩子没触发,用例空转")
	requireHolderReleasePending(t, err)
	assert.Equal(t, raw, leaveCasValue(t, mr, getPlayerLocationKey(playerID)))
	assert.Equal(t, "3", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Equal(t, held, unqualifiedRevokeFenceValue(mr, playerID))
	assert.Len(t, calls(), 1, "没撤成:照旧请持有节点释放")
	assert.Equal(t, []unqualifiedRevokeStoreFence{{ownerEpoch: 2, locationAtCall: true, ownerEpochAtCall: "3"}}, storeFences(),
		"写权行只按 E−1 围:E 仍留给刚登记的持有者认领")
}

// ── EnterScene 持有者释放门 ─────────────────────────────────────────────────

// 登录撞门(§12)一侧:location 记着 (节点 10, E=3),节点 10 从未在 3 下登记成功。第一次撞门请过释放、开始计时;宽限走完后的撞门
// 在回 18 之前撤销(先按 E−1=2 围存档写权行,再 owner_epoch 3 → 4、删 location、扣旧场景与节点人数、计 revoked_unqualified);
// 下一次重登没有 location,按无持有者落点铸造 5、指向节点 20。
func TestEnterScene_LoginRevokesUnqualifiedHolderAfterGrace(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8761)
		holderScene = uint64(9661)
		targetScene = uint64(9662)
	)
	var storeErr error
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	fence := unqualifiedRevokePreviousFence(3)
	unqualifiedRevokeSetFence(mr, playerID, fence)
	requested := installEnterHolderReleaseRecorder(t, nil)
	t0 := leaveReleaseRedisNow
	mr.SetTime(t0)
	revokedBefore := enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict)
	login := enterHolderReleaseLogin(playerID, targetScene)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.True(t, mr.Exists(leaveReleaseWaitKey(playerID)), "请过释放即开始计时(与 LeaveScene 共用一把键)")
	require.Len(t, requested(), 1)

	mr.SetTime(t0.Add(unqualifiedRevokeGrace - time.Millisecond))
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)

	mr.SetTime(t0.Add(unqualifiedRevokeGrace))
	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHandoffPending, resp.ErrorCode, "撤销与否都照回 18")
	assert.Len(t, requested(), 3, "撤销之前照常请过一次释放")
	assert.False(t, mr.Exists(getPlayerLocationKey(playerID)))
	assert.Equal(t, "4", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)))
	assert.Equal(t, "0", sceneMemberCount(mr, holderScene), "旧场景人数随 location 扣掉")
	assert.Equal(t, "0", nodeMemberCount(mr, testZoneId, "10"))
	assert.Equal(t, "0", sceneMemberCount(mr, targetScene), "拒绝不预占目标场景人数")
	assert.Equal(t, fence, unqualifiedRevokeFenceValue(mr, playerID))
	assert.Equal(t, revokedBefore+1, enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict))
	assert.Equal(t, []unqualifiedRevokeStoreFence{{ownerEpoch: 2, locationAtCall: true, ownerEpochAtCall: "3"}}, storeFences(),
		"先按 E−1 围写权行,再动 Redis")

	resp, err = NewEnterSceneLogic(context.Background(), sc).EnterScene(login)
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.ErrorCode, "没有 location:按无持有者落点")
	assert.Equal(t, "5", leaveCasValue(t, mr, ownerepoch.OwnerEpochKey(playerID)), "在撤销推进过的围栏之上铸造")
	loc := identLocation(t, sc, playerID)
	require.NotNil(t, loc)
	assert.Equal(t, "20", loc.NodeId)
	assert.Equal(t, uint64(5), loc.OwnerEpoch)
	assert.Len(t, requested(), 3, "没有旧持有者可请")
	assert.Len(t, storeFences(), 1, "落点不碰写权行:新持有者以更大的代次认领")
}

// 登录撞门一侧围写权行失败:这次不撤销,照回 18、location 与 owner_epoch 一个字节不动,不计数;下一次撞门或 LeaveScene 再来。
func TestEnterScene_LoginRevokeWaitsWhenPlayerStoreFenceFails(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8763)
		holderScene = uint64(9665)
		targetScene = uint64(9666)
	)
	storeErr := errors.New("data_service down")
	storeFences := installUnqualifiedRevokeStoreFenceRecorder(t, sc, mr, playerID, &storeErr)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokePreviousFence(3))
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace)
	installEnterHolderReleaseRecorder(t, nil)
	revokedBefore := enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict)

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(enterHolderReleaseLogin(playerID, targetScene))
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.Len(t, storeFences(), 1, "试过围写权行")
	assert.Equal(t, "1", sceneMemberCount(mr, holderScene), "没撤销就不扣人数")
	assert.Equal(t, revokedBefore, enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict))
}

// scene 自发的请求(带关联号)被 18 拒时释放门不请释放,也就不检查撤销 —— 即使宽限早已走完、资格原件也不属于 E。
func TestEnterScene_SceneInitiatedRejectionNeverRevokes(t *testing.T) {
	sc, mr := newTestSvcCtxWithWorldScenes(t)
	const (
		playerID    = uint64(8762)
		holderScene = uint64(9663)
		targetScene = uint64(9664)
	)
	enterHolderReleaseSeed(t, sc, mr, playerID, holderScene, targetScene, enterHolderReleaseZombie)
	unqualifiedRevokeSetFence(mr, playerID, unqualifiedRevokePreviousFence(3))
	unqualifiedRevokeStartTimer(t, mr, playerID, 3, leaveReleaseRedisNow, unqualifiedRevokeGrace+time.Minute)
	requested := installEnterHolderReleaseRecorder(t, nil)
	revokedBefore := enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict)
	req := enterHolderReleaseLogin(playerID, targetScene)
	req.CorrelationId = 7

	resp, err := NewEnterSceneLogic(context.Background(), sc).EnterScene(req)
	requireHandoffRejectedUnchanged(t, sc, mr, playerID, resp, err, enterHolderReleaseZombie)
	assert.Empty(t, requested())
	assert.Equal(t, revokedBefore, enterHolderReleaseCount(t, unqualifiedRevokeMetricVerdict))
}
