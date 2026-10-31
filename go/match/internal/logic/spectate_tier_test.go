package logic

// 观战档位与 L1 / L2 准入(battle-spectate-tiered-fanout.md §15.6)的 miniredis 测试:
// 档位只升不降;同 gate 第二名观众不再 Attach、换实例重新 Attach;Attach 失败不记账;
// 到阈值自动升 L2、配额满保持 L1、Promote 失败退还配额;L2 观众不调 battle;中继关闭时回到 DIRECT。
// 成局原件走 fakeSpectateFormations;意图结案与重放见 global_spectate_lobby_native_test.go(需要 TiDB)。
// battle 节点的 RPC 用 attachRelayGateFn / promoteSpectateBroadcastFn 顶替。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/config"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

const (
	tierDirect    = battlepb.ESpectateTier_SPECTATE_TIER_DIRECT
	tierRelay     = battlepb.ESpectateTier_SPECTATE_TIER_RELAY
	tierBroadcast = battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST

	deliveryLobby  = matchpb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY
	deliveryDirect = matchpb.ESpectateDelivery_SPECTATE_DELIVERY_DIRECT
	deliveryNone   = matchpb.ESpectateDelivery_SPECTATE_DELIVERY_NONE

	// testRelayDeadlineMs 是成局原件与桩 battle 的战斗期限,远在未来,配额 Lua 不会把它当过期项清掉。
	testRelayDeadlineMs = uint64(4102444800000)
)

// fakeRelayRPCs 记录 match 发给 battle 的 AttachRelayGate / PromoteSpectateBroadcast。
type fakeRelayRPCs struct {
	mu       sync.Mutex
	attaches []*battlepb.AttachRelayGateRequest
	promotes []*battlepb.PromoteSpectateBroadcastRequest
	// onAttach / onPromote 决定响应;nil = 成功,回请求里的实例与 testRelayDeadlineMs。
	onAttach  func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error)
	onPromote func(req *battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error)
}

func stubRelayRPCs(t *testing.T) *fakeRelayRPCs {
	t.Helper()
	f := &fakeRelayRPCs{}
	prevAttach, prevPromote := attachRelayGateFn, promoteSpectateBroadcastFn
	attachRelayGateFn = func(_ context.Context, _ string, req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		f.mu.Lock()
		f.attaches = append(f.attaches, req)
		onAttach := f.onAttach
		f.mu.Unlock()
		if onAttach != nil {
			return onAttach(req)
		}
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs}, nil
	}
	promoteSpectateBroadcastFn = func(_ context.Context, _ string, req *battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		f.mu.Lock()
		f.promotes = append(f.promotes, req)
		onPromote := f.onPromote
		f.mu.Unlock()
		if onPromote != nil {
			return onPromote(req)
		}
		return &battlepb.PromoteSpectateBroadcastResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs}, nil
	}
	t.Cleanup(func() { attachRelayGateFn, promoteSpectateBroadcastFn = prevAttach, prevPromote })
	return f
}

// newRelaySvcCtx 在 newGatherSvcCtx 之上打开中继:阈值 2、全服配额 2(用例按需改)。
func newRelaySvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newGatherSvcCtx(t)
	svcCtx.Config.Spectate = config.SpectateConf{
		RelayEnabled:               true,
		BroadcastMaxConcurrent:     2,
		BroadcastAutoGateThreshold: 2,
		TicketTTLSeconds:           60,
		TicketSecret:               testSpectateTicketSecret,
	}
	return svcCtx, mr
}

// raiseTier 把一场直接升到指定档位(用例前置)。
func raiseTier(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64, tier battlepb.ESpectateTier) {
	t.Helper()
	got, err := raiseSpectateTier(context.Background(), svcCtx, battleId, tier)
	require.NoError(t, err)
	require.Equal(t, tier, got)
}

func tierOf(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64) battlepb.ESpectateTier {
	t.Helper()
	tier, err := readSpectateTier(context.Background(), svcCtx, battleId)
	require.NoError(t, err)
	return tier
}

func broadcastQuotaCount(t *testing.T, svcCtx *svc.ServiceContext) int {
	t.Helper()
	count, err := svcCtx.MatchRedis.Zcard(spectateBroadcastActiveKey)
	require.NoError(t, err)
	return count
}

// lobbyResult 是一次 L1 / L2 准入的结论。
type lobbyResult struct {
	admission spectateLobbyAdmission
	closeTip  uint32
	err       error
}

// lobbyAdmit 按 WatchBattle 的判定走一次准入:档位读键,记录由成局原件现算,观众挂在指定 gate。
func lobbyAdmit(t *testing.T, svcCtx *svc.ServiceContext, playerId, battleId uint64, gate uint32, gateInstance string) lobbyResult {
	t.Helper()
	ctx := context.Background()
	tier, err := spectateAdmissionTier(ctx, svcCtx, battleId)
	require.NoError(t, err)
	require.NotEqual(t, tierDirect, tier, "用例前提:这场已升档")
	formation := stubSpectateFormations(t).formations[battleId]
	require.NotNil(t, formation, "用例前提:这场已登记 battle=%d", battleId)
	target := globalWatchTarget{formation: formation, sessionId: 1, gateNodeId: gate, gateInstanceId: gateInstance}
	admission, closeTip, err := NewWatchBattleLogic(ctx, svcCtx).admitGlobalLobby(playerId, target, tier)
	return lobbyResult{admission: admission, closeTip: closeTip, err: err}
}

// lobbyAdmitOnGate3 是挂在 gate 3 的观众。
func lobbyAdmitOnGate3(t *testing.T, svcCtx *svc.ServiceContext, playerId, battleId uint64) lobbyResult {
	t.Helper()
	return lobbyAdmit(t, svcCtx, playerId, battleId, 3, "gate-inst-3")
}

// requireLobbyAdmitted 断言准入成功、凭证可验签、绑定这名观众这一场与原实例和期限。
func requireLobbyAdmitted(t *testing.T, result lobbyResult, battleId, playerId uint64) {
	t.Helper()
	require.NoError(t, result.err)
	require.Zero(t, result.closeTip)
	payload := verifiedTicketPayload(t, result.admission.ticket, testSpectateTicketSecret)
	require.Equal(t, battleId, payload.GetBattleId())
	require.Equal(t, playerId, payload.GetPlayerId())
	require.Equal(t, testBattle7Instance, payload.GetBattleInstanceId())
	require.Equal(t, testRelayDeadlineMs, payload.GetDeadlineMs())
	require.Greater(t, payload.GetExpireAtMs(), nowMs())
}

// ---- 档位 ----

func TestSpectateTierOnlyRaisesAndKeepsTTL(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	ctx := context.Background()
	const battleId = uint64(883001)
	key := spectateTierKey(battleId)
	ttl := time.Duration(spectateTTLSeconds) * time.Second

	require.Equal(t, tierDirect, tierOf(t, svcCtx, battleId), "键不存在 = DIRECT")

	got, err := raiseSpectateTier(ctx, svcCtx, battleId, tierRelay)
	require.NoError(t, err)
	require.Equal(t, tierRelay, got)
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))
	require.Equal(t, ttl, mr.TTL(key), "首次写入 TTL = spectateTTLSeconds")

	mr.FastForward(10 * time.Second)
	got, err = raiseSpectateTier(ctx, svcCtx, battleId, tierBroadcast)
	require.NoError(t, err)
	require.Equal(t, tierBroadcast, got)
	require.Equal(t, ttl-10*time.Second, mr.TTL(key), "升档保持 TTL")

	for _, lower := range []battlepb.ESpectateTier{tierRelay, tierDirect} {
		got, err = raiseSpectateTier(ctx, svcCtx, battleId, lower)
		require.NoError(t, err)
		require.Equal(t, tierBroadcast, got, "只升不降:返回的是写后的实际档位")
		require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
	}
}

// 档位值读不懂:fail-closed,不猜 DIRECT(热门局的观众会全部回去撞 L0),也不猜高(会跳过 battle 的准入)。
func TestSpectateAdmissionTierCorruptValueFailsClosed(t *testing.T) {
	for _, value := range []string{"99", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			const battleId = uint64(883002)
			require.NoError(t, mr.Set(spectateTierKey(battleId), value))

			_, err := spectateAdmissionTier(context.Background(), svcCtx, battleId)
			require.Error(t, err)
		})
	}
}

// 本场的键共用一个 hash tag(§15.6),落同一 slot;全服配额 ZSET 不带本场的 tag。
func TestSpectateTierKeysShareBattleHashTag(t *testing.T) {
	const battleId = uint64(42)
	slot := redisHashSlot(spectateTierKey(battleId))
	require.Contains(t, spectateTierKey(battleId), "{42}")
	require.Equal(t, slot, redisHashSlot(spectateRelayGatesKey(battleId)))
	require.Equal(t, slot, redisHashSlot(spectateFeaturedKey(battleId)))
	require.Equal(t, slot, redisHashSlot(spectatePromoteTryKey(battleId)))
	require.NotEqual(t, slot, redisHashSlot(spectateTierKey(battleId+1)), "不同场次按 battle_id 分布")
}

// ---- 中继开关 ----

// 中继不可用:档位判定一律 DIRECT、满员不升档(调用方回 16021);没有密钥(不配 Spectate 密钥的档位)与开关关闭同样处理,
// 不签 gate 必然拒绝的凭证。已升档的场在开关关闭后回到上一档的行为,不看档位键(§11)。
func TestRelayUnusableStaysDirect(t *testing.T) {
	cases := []struct {
		name string
		conf config.SpectateConf
	}{
		{"开关关闭", config.SpectateConf{RelayEnabled: false, TicketSecret: testSpectateTicketSecret}},
		{"开关打开但没有密钥", config.SpectateConf{RelayEnabled: true}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			ctx := context.Background()
			fresh, raised := uint64(883070+i*2), uint64(883071+i*2)
			raiseTier(t, svcCtx, raised, tierRelay)
			svcCtx.Config.Spectate = c.conf

			tier, err := spectateDirectFullToRelay(ctx, svcCtx, fresh)
			require.NoError(t, err)
			require.Equal(t, tierDirect, tier)
			require.False(t, mr.Exists(spectateTierKey(fresh)), "不升档")

			tier, err = spectateAdmissionTier(ctx, svcCtx, raised)
			require.NoError(t, err)
			require.Equal(t, tierDirect, tier, "不看档位键")
		})
	}
}

// 直连席满且中继可用:升 RELAY 并返回写后的实际档位;已是更高档时不回落。
func TestSpectateDirectFullToRelayRaisesTier(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	ctx := context.Background()
	const fresh, broadcast = uint64(883080), uint64(883081)

	tier, err := spectateDirectFullToRelay(ctx, svcCtx, fresh)
	require.NoError(t, err)
	require.Equal(t, tierRelay, tier)
	require.Equal(t, tierRelay, tierOf(t, svcCtx, fresh))

	raiseTier(t, svcCtx, broadcast, tierBroadcast)
	tier, err = spectateDirectFullToRelay(ctx, svcCtx, broadcast)
	require.NoError(t, err)
	require.Equal(t, tierBroadcast, tier)
}

// ---- L1 ----

// 同 gate 第二名观众不再调 Attach;换一台 gate 要挂;gate 重启(同 node 新实例)要重新挂并覆盖账本。账本带 TTL。
func TestLobbyAttachesEachGateInstanceOnce(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0 // 这条用例不看升 L2
	relay := stubRelayRPCs(t)
	const battleId = uint64(883011)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	gatesKey := spectateRelayGatesKey(battleId)
	relayBase := metrics.SpectateTierValue(metrics.SpectateTierRelay)

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7411, battleId, 3, "gate-inst-3"), battleId, 7411)
	require.Len(t, relay.attaches, 1)
	attach := relay.attaches[0]
	require.Equal(t, battleId, attach.GetBattleId())
	require.Equal(t, uint32(3), attach.GetGateNodeId())
	require.Equal(t, "gate-inst-3", attach.GetGateInstanceId())
	require.Equal(t, testBattle7Instance, attach.GetTargetInstanceId())
	require.Equal(t, "gate-inst-3", mr.HGet(gatesKey, "3"))
	require.Equal(t, time.Duration(spectateTTLSeconds)*time.Second, mr.TTL(gatesKey))
	require.Empty(t, relay.promotes, "gate 数没到阈值不升 L2")

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7412, battleId, 3, "gate-inst-3"), battleId, 7412)
	require.Len(t, relay.attaches, 1, "同 gate 第二名观众不再调 Attach,实例与期限取自成局原件")

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7413, battleId, 4, "gate-inst-4"), battleId, 7413)
	require.Len(t, relay.attaches, 2, "新 gate 要挂")
	require.Equal(t, uint32(4), relay.attaches[1].GetGateNodeId())

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7414, battleId, 3, "gate-inst-3b"), battleId, 7414)
	require.Len(t, relay.attaches, 3, "同 node 换实例要重新挂")
	require.Equal(t, "gate-inst-3b", relay.attaches[2].GetGateInstanceId())
	require.Equal(t, "gate-inst-3b", mr.HGet(gatesKey, "3"), "账本跟随新实例")
	require.Equal(t, relayBase+4, metrics.SpectateTierValue(metrics.SpectateTierRelay))
}

// 账本有这台 gate 但成局原件没有期限:再问 battle 一次,不能签出缺字段的凭证。
func TestLobbyReattachesWhenSourceUnknown(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7415), uint64(883012)
	registerBattle(t, svcCtx, battleId, nowMs())
	stubSpectateFormations(t).formations[battleId].DeadlineMs = 0
	raiseTier(t, svcCtx, battleId, tierRelay)
	mr.HSet(spectateRelayGatesKey(battleId), "3", "gate-inst-3")

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.attaches, 1)
}

// 观众会话缺 gate 路由:中继帧必须带 target_gate_id 与 target_instance_id(AGENTS §7.2),不发 Attach。
func TestLobbyRejectsViewerWithoutGateRoute(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7416), uint64(883013)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	require.Error(t, lobbyAdmit(t, svcCtx, watcher, battleId, 0, "").err)
	require.Empty(t, relay.attaches)
}

// Attach 失败不记账:RPC 失败与 battle 的其它拒绝都不动索引、不留账;恢复后下一名观众照常挂上。
func TestLobbyAttachFailureIsNotRecorded(t *testing.T) {
	cases := []struct {
		name     string
		attach   func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error)
		closeTip uint32
	}{
		{name: "RPC 失败", attach: func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return nil, errors.New("battle 节点不可达")
		}},
		{name: "battle 拒绝(实例不符 / 非公开)", attach: func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "拒绝")}, nil
		}, closeTip: constants.ErrBattleNotWatchable},
		{name: "回包缺实例", attach: func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{DeadlineMs: testRelayDeadlineMs}, nil
		}},
		{name: "回包缺期限", attach: func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId()}, nil
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			relay := stubRelayRPCs(t)
			relay.onAttach = c.attach
			watcher, battleId := uint64(7420+i), uint64(883020+i)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)

			result := lobbyAdmitOnGate3(t, svcCtx, watcher, battleId)
			if c.closeTip != 0 {
				require.NoError(t, result.err, "能证明的拒绝按结论返回")
				require.Equal(t, c.closeTip, result.closeTip)
			} else {
				require.Error(t, result.err, "故障不是结论,同 request_id 重试")
				require.Zero(t, result.closeTip)
			}
			require.Empty(t, result.admission.ticket)
			require.Empty(t, mr.HGet(spectateRelayGatesKey(battleId), "3"), "先 RPC 后记账:失败不记账")
			require.Equal(t, 1, activeIndexSize(t, svcCtx), "战斗仍在打,索引不许剔除")

			relay.onAttach = nil
			requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Equal(t, "gate-inst-3", mr.HGet(spectateRelayGatesKey(battleId), "3"))
		})
	}
}

// battle 回房间不存在:回 16018 并清掉本场观战数据(成局结束后仍可能是 state 4,这场不能再留在列表与随机选场里)。
func TestLobbyAttachMissingRoomEvictsIndex(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	relay.onAttach = func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在")}, nil
	}
	const watcher, battleId = uint64(7430), uint64(883030)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	result := lobbyAdmitOnGate3(t, svcCtx, watcher, battleId)

	require.NoError(t, result.err)
	require.Equal(t, constants.ErrBattleNotWatchable, result.closeTip)
	require.Zero(t, activeIndexSize(t, svcCtx), "房间不存在 → 懒剔除")
}

// 本场已是广播档:battle 回 broadcast=true、没有挂 gate。档位同步成 BROADCAST,账本不记这台 gate,配额补记,凭证照发。
func TestLobbyAttachBroadcastReplySyncsTier(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs, Broadcast: true}, nil
	}
	const watcher, battleId = uint64(7435), uint64(883035)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	base := metrics.SpectateTierValue(metrics.SpectateTierBroadcast)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
	require.Empty(t, mr.HGet(spectateRelayGatesKey(battleId), "3"), "没有挂上的 gate 不记账")
	require.Empty(t, relay.promotes, "已是广播档,不再升")
	require.Equal(t, base+1, metrics.SpectateTierValue(metrics.SpectateTierBroadcast))
	score, err := mr.ZScore(spectateBroadcastActiveKey, strconv.FormatUint(battleId, 10))
	require.NoError(t, err, "battle 已在广播,配额账上必须有这场")
	require.Equal(t, float64(testRelayDeadlineMs), score)
}

// Promote 在 battle 已生效而回包超时时配额被退,名额又被别的场占走:Attach 回 broadcast=true 时超额也要记,
// 后面的抢占才会被挡住,实际 L2 场数不再继续突破 B。
func TestLobbyAttachBroadcastReplyRecordsQuotaOverLimit(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	relay := stubRelayRPCs(t)
	relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs, Broadcast: true}, nil
	}
	const watcher, battleId = uint64(7436), uint64(883036)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), "999999")
	require.NoError(t, err)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)

	require.Equal(t, 2, broadcastQuotaCount(t, svcCtx), "超额也记")
	claim, err := claimBroadcastQuota(context.Background(), svcCtx, 883037, testRelayDeadlineMs, nowMs())
	require.NoError(t, err)
	require.Equal(t, broadcastQuotaFull, claim, "后面的抢占被挡住")
}

// ---- L1 → L2 ----

// 有观众的 gate 数到阈值:抢配额 → Promote → 档位 BROADCAST,配额 score 换成 battle 回的真实期限;之后的观众不调任何 battle RPC。
func TestLobbyAutoPromotesAtGateThreshold(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(883040)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	okBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteOK)

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7441, battleId, 3, "gate-inst-3"), battleId, 7441)
	require.Empty(t, relay.promotes, "一台 gate 没到阈值")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7442, battleId, 4, "gate-inst-4"), battleId, 7442)
	require.Len(t, relay.promotes, 1)
	require.Equal(t, battleId, relay.promotes[0].GetBattleId())
	require.Equal(t, testBattle7Instance, relay.promotes[0].GetTargetInstanceId())
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
	require.Equal(t, okBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteOK))
	score, err := mr.ZScore(spectateBroadcastActiveKey, strconv.FormatUint(battleId, 10))
	require.NoError(t, err)
	require.Equal(t, float64(testRelayDeadlineMs), score)

	requireLobbyAdmitted(t, lobbyAdmit(t, svcCtx, 7443, battleId, 5, "gate-inst-5"), battleId, 7443)
	require.Len(t, relay.attaches, 2, "L2 观众不调 Attach")
	require.Len(t, relay.promotes, 1, "L2 观众不调 Promote")
}

// 配额满:不报错,留在 L1,观众照常进;名额空出来后下一名观众(同 gate,不用 Attach)再升。
func TestLobbyPromoteQuotaFullStaysRelay(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7445), uint64(883045)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), "999999")
	require.NoError(t, err)
	fullBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteQuotaFull)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Empty(t, relay.promotes, "配额满不调 battle")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
	require.Equal(t, fullBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteQuotaFull))

	_, err = svcCtx.MatchRedis.Zrem(spectateBroadcastActiveKey, "999999")
	require.NoError(t, err)
	mr.FastForward(spectatePromoteLeaseSeconds * time.Second) // 上一次尝试的租约到期
	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.promotes, 1, "名额空出来就升")
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
}

// Promote 失败:退还配额、保持 RELAY,观众不受影响;恢复后下一名观众升上去。
func TestLobbyPromoteFailureReleasesQuota(t *testing.T) {
	cases := []struct {
		name    string
		promote func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error)
	}{
		{"RPC 失败", func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
			return nil, errors.New("battle 节点不可达")
		}},
		{"battle 拒绝", func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
			return &battlepb.PromoteSpectateBroadcastResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "拒绝")}, nil
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
			relay := stubRelayRPCs(t)
			relay.onPromote = c.promote
			watcher, battleId := uint64(7450+i), uint64(883050+i)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
			failedBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteRPCFailed)

			requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Len(t, relay.promotes, 1)
			require.Zero(t, broadcastQuotaCount(t, svcCtx), "失败退还配额")
			require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId), "升档失败保持原档")
			require.Equal(t, failedBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteRPCFailed))

			relay.onPromote = nil
			mr.FastForward(spectatePromoteLeaseSeconds * time.Second) // 上一次尝试的租约到期
			requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
			require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
		})
	}
}

// 升档短租约:battle 过载时不让每名观众都去等 Promote 超时。租约期内只有一名观众发 RPC,其余直接按 RELAY 签票,
// 也不碰配额;租约到期后的下一名观众再试。
func TestPromoteLeaseLetsOneViewerCallBattle(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		return nil, errors.New("battle 节点过载")
	}
	const battleId = uint64(883057)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	throttledBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteThrottled)

	for _, watcher := range []uint64{7500, 7501, 7502} {
		requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	}
	require.Len(t, relay.promotes, 1, "租约期内只有一名观众发 RPC")
	require.Equal(t, throttledBase+2, metrics.SpectatePromoteValue(metrics.SpectatePromoteThrottled))
	require.Zero(t, broadcastQuotaCount(t, svcCtx), "失败已退还,其余观众没有碰配额")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))

	mr.FastForward(spectatePromoteLeaseSeconds * time.Second)
	relay.onPromote = nil
	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, 7502, battleId), battleId, 7502)
	require.Len(t, relay.promotes, 2, "租约到期后再试")
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
}

// 配额账:新占者的 Promote 超时,而别的请求已经把这场升上去(持有者卡过租约的极端时序):退还会抹掉成功者的账,本场继续广播却不计数。
func TestPromoteFailureKeepsQuotaWhenTierAlreadyBroadcast(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7456), uint64(883056)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		raiseTier(t, svcCtx, battleId, tierBroadcast)
		return nil, errors.New("Promote 回包超时")
	}
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "已是 BROADCAST 就不退")
}

// 配额账:退还前读不到档位时不退。最坏白占到本场结束,比突破 B 的代价小。
func TestPromoteFailureKeepsQuotaWhenTierUnreadable(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7459), uint64(883059)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		require.NoError(t, mr.Set(spectateTierKey(battleId), "not-a-number"))
		return nil, errors.New("Promote 回包超时")
	}
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
}

// 配额只由新占到的请求退还:本场已在集合里(另一名观众正在升档,或已经升上去)时,这次失败不能把配额退掉。
func TestPromoteFailureKeepsQuotaHeldByAnotherRequest(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		return nil, errors.New("battle 节点不可达")
	}
	const watcher, battleId = uint64(7455), uint64(883055)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), strconv.FormatUint(battleId, 10))
	require.NoError(t, err)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.promotes, 1)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "不是自己占的配额不退")
}

// Promote 回房间不存在:已挂接 gate 的观众不调 Attach,这是 match 唯一能知道场已结束的机会。按懒剔除口径拒绝这名观众、清掉本场全部观战数据,
// 不论配额是不是这次新占的。
func TestLobbyPromoteMissingRoomRejectsViewerAndEvicts(t *testing.T) {
	for i, heldByOther := range []bool{false, true} {
		t.Run(strconv.FormatBool(heldByOther), func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
			relay := stubRelayRPCs(t)
			relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
				return &battlepb.PromoteSpectateBroadcastResponse{ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在")}, nil
			}
			watcher, battleId := uint64(7510+i), uint64(883110+i)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
			if heldByOther {
				_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), strconv.FormatUint(battleId, 10))
				require.NoError(t, err)
			}

			result := lobbyAdmitOnGate3(t, svcCtx, watcher, battleId)

			require.NoError(t, result.err)
			require.Equal(t, constants.ErrBattleNotWatchable, result.closeTip)
			require.Empty(t, result.admission.ticket)
			require.Zero(t, broadcastQuotaCount(t, svcCtx))
			requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
		})
	}
}

// newKnownSourceBattle 铺一场已升档、观众所在 gate 已挂接的战斗:这名观众的准入不调任何 battle RPC。
func newKnownSourceBattle(t *testing.T, tier battlepb.ESpectateTier, seq int) (*svc.ServiceContext, *miniredis.Miniredis, uint64, uint64) {
	t.Helper()
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0
	watcher, battleId := uint64(7520+seq), uint64(883120+seq)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tier)
	mr.HSet(spectateRelayGatesKey(battleId), "3", "gate-inst-3")
	return svcCtx, mr, battleId, watcher
}

// battle 节点重启(同 node_id 新实例):已挂接 gate 的观众与 L2 观众都不调 battle,靠内存注册表发现来源实例已不存在,按房间不存在处理。
func TestLobbyRejectsTicketForReplacedInstance(t *testing.T) {
	for i, tier := range []battlepb.ESpectateTier{tierRelay, tierBroadcast} {
		t.Run(tier.String(), func(t *testing.T) {
			svcCtx, mr, battleId, watcher := newKnownSourceBattle(t, tier, i)
			relay := stubRelayRPCs(t)
			requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)

			svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, NodeUuid: "replaced-instance", Endpoint: "battle-7"})
			result := lobbyAdmitOnGate3(t, svcCtx, watcher, battleId)

			require.Equal(t, constants.ErrBattleNotWatchable, result.closeTip)
			require.Empty(t, result.admission.ticket)
			require.Empty(t, relay.attaches, "不增加 RPC")
			require.Empty(t, relay.promotes, "不增加 RPC")
			requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
		})
	}
}

// 注册表查不到不判:etcd 抖动、租约短暂失效时同样查不到,不代表实例已死,凭证照签。
func TestLobbySignsTicketWhenInstanceNotInRegistry(t *testing.T) {
	for i, tier := range []battlepb.ESpectateTier{tierRelay, tierBroadcast} {
		t.Run(tier.String(), func(t *testing.T) {
			svcCtx, _, battleId, watcher := newKnownSourceBattle(t, tier, i+10)
			relay := stubRelayRPCs(t)
			svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)

			requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Empty(t, relay.attaches)
			require.Empty(t, relay.promotes)
		})
	}
}

// B=0 = 不开 L2:到了阈值也不抢配额、不调 battle。
func TestLobbyBroadcastDisabledNeverPromotes(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 0
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7458), uint64(883058)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Empty(t, relay.promotes)
	require.Zero(t, broadcastQuotaCount(t, svcCtx))
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))
}

// 配额 Lua:先清过期项;未满才占;已在集合里不重复占也不算满。
func TestClaimBroadcastQuotaExpiresStaleAndCaps(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t) // B = 2
	ctx := context.Background()
	claim := func(battleId, expiryMs, nowMs uint64) broadcastQuotaClaim {
		t.Helper()
		got, err := claimBroadcastQuota(ctx, svcCtx, battleId, expiryMs, nowMs)
		require.NoError(t, err)
		return got
	}
	require.Equal(t, broadcastQuotaClaimed, claim(1, 5000, 1000))
	require.Equal(t, broadcastQuotaClaimed, claim(2, 6000, 1000))
	require.Equal(t, broadcastQuotaFull, claim(3, 7000, 1000), "配额满")
	require.Equal(t, broadcastQuotaHeld, claim(1, 5000, 1000), "已在集合里不占新名额")
	require.Equal(t, broadcastQuotaClaimed, claim(3, 7000, 5500), "第 1 场到期后被清,名额空出来")
	require.Equal(t, 2, broadcastQuotaCount(t, svcCtx))
}

// L2 观众不调任何 battle RPC:凭证的实例与期限取自成局原件。
func TestLobbyBroadcastTierViewerCallsNoBattleRPC(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7460), uint64(883060)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierBroadcast)
	base := metrics.SpectateTierValue(metrics.SpectateTierBroadcast)

	requireLobbyAdmitted(t, lobbyAdmitOnGate3(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Empty(t, relay.attaches)
	require.Empty(t, relay.promotes)
	require.Equal(t, base+1, metrics.SpectateTierValue(metrics.SpectateTierBroadcast))
}

// BROADCAST 档而成局原件缺来源:原件损坏,fail-closed,不签缺字段的凭证,也不去问 battle。
func TestLobbyBroadcastTierWithoutSourceFailsClosed(t *testing.T) {
	svcCtx, _ := newRelaySvcCtx(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7462), uint64(883062)
	registerBattle(t, svcCtx, battleId, nowMs())
	stubSpectateFormations(t).formations[battleId].DeadlineMs = 0
	raiseTier(t, svcCtx, battleId, tierBroadcast)

	result := lobbyAdmitOnGate3(t, svcCtx, watcher, battleId)

	require.Error(t, result.err)
	require.Empty(t, result.admission.ticket)
	require.Empty(t, relay.promotes)
}

// 观战记录由成局原件现算:L2 观众不调 battle,凭证的实例与期限全靠这里;原件缺失不能算出空记录。
func TestSpectateRecordFromFormationCarriesSource(t *testing.T) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{
		SpectateVisibility: visPublic,
		Players:            []*battlepb.BattlePlayerSnapshot{{PlayerName: "甲"}},
	})
	require.NoError(t, err)
	record, err := spectateRecordFromFormation(&matchpb.MatchFormationRecord{
		BattleId: 883090, TargetNodeId: 7, TargetInstanceId: "battle-instance", DeadlineMs: 9000,
		Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_SOLO), ConfigId: 2, CreatedMs: 3000, CreateRequest: raw,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(883090), record.GetSummary().GetBattleId())
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Equal(t, visPublic, record.GetVisibility())
	require.Equal(t, "battle-instance", record.GetBattleInstanceId())
	require.Equal(t, uint64(9000), record.GetDeadlineMs())
	require.Equal(t, []string{"甲"}, record.GetSummary().GetPlayerNames())

	_, err = spectateRecordFromFormation(&matchpb.MatchFormationRecord{BattleId: 883090})
	require.Error(t, err, "建房原件还没落库")
	_, err = spectateRecordFromFormation(&matchpb.MatchFormationRecord{BattleId: 883090, CreateRequest: []byte{0x08}})
	require.Error(t, err, "建房原件损坏")
}

// 懒剔除一并还掉 L2 配额与赛事局索引成员:全服集合的成员不随本场的键过期,不还就白占到期限。
// 档位、中继 gate 与赛事局标记也清掉:留着会让之后到达的观众按旧档位签出收不到帧的凭证。
func TestRemoveSpectateBattleReleasesGlobalSetMembers(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	const battleId = uint64(883079)
	seedBattleSpectateKeys(t, svcCtx, mr, battleId)

	removeSpectateBattle(svcCtx, battleId)

	requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
}

// seedBattleSpectateKeys 铺一场的全部观战键,全服集合上再放一场别的,用来证明清理不会误伤。
func seedBattleSpectateKeys(t *testing.T, svcCtx *svc.ServiceContext, mr *miniredis.Miniredis, battleId uint64) {
	t.Helper()
	member := strconv.FormatUint(battleId, 10)
	require.NoError(t, mr.Set(spectateFeaturedKey(battleId), "1"))
	mr.HSet(spectateRelayGatesKey(battleId), "3", "gate-inst-3")
	raiseTier(t, svcCtx, battleId, tierBroadcast)
	for _, key := range []string{spectateBattlesActiveKey, spectateBroadcastActiveKey, spectateFeaturedActiveKey} {
		_, err := mr.ZAdd(key, float64(testRelayDeadlineMs), member)
		require.NoError(t, err)
	}
}

// requireBattleSpectateKeysGone 断言一场的观战键全部清掉。
func requireBattleSpectateKeysGone(t *testing.T, svcCtx *svc.ServiceContext, mr *miniredis.Miniredis, battleId uint64) {
	t.Helper()
	for _, key := range []string{spectateTierKey(battleId), spectateRelayGatesKey(battleId), spectateFeaturedKey(battleId)} {
		require.False(t, mr.Exists(key), key)
	}
	member := strconv.FormatUint(battleId, 10)
	for _, key := range []string{spectateBattlesActiveKey, spectateBroadcastActiveKey, spectateFeaturedActiveKey} {
		_, err := svcCtx.MatchRedis.Zscore(key, member)
		require.ErrorIs(t, err, redis.Nil, key)
	}
}

// ---- 键生命周期 ----
// 下面这些函数只读写 Redis、不碰 SQL。经 TiDB 的完整路径见 global_spectate_lobby_native_test.go。

// 成局的期限 = 提交时刻 + 备战预算 + 模板时长,最长超过 1 小时:观战键的 TTL 要盖住它,再留收尾余量。
func TestSpectateTTLCoversLongestBattle(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	ctx := context.Background()
	const battleId = uint64(883130)
	longest := time.Duration(catalog.MaxBattleSeconds)*time.Second + time.Duration(authority.WorkLeaseMs)*time.Millisecond
	ttl := time.Duration(spectateTTLSeconds) * time.Second
	require.Greater(t, int64(ttl), int64(longest))

	raiseTier(t, svcCtx, battleId, tierRelay)
	recordRelayGate(ctx, svcCtx, battleId, spectateLobbyViewer{playerId: 1, gateNodeId: 3, gateInstanceId: "gate-inst-3"})
	require.NoError(t, markSpectateFeatured(ctx, svcCtx, battleId))
	for _, key := range []string{spectateTierKey(battleId), spectateRelayGatesKey(battleId), spectateFeaturedKey(battleId)} {
		require.Equal(t, ttl, mr.TTL(key), key)
	}
	score, err := mr.ZScore(spectateFeaturedActiveKey, strconv.FormatUint(battleId, 10))
	require.NoError(t, err)
	require.Greater(t, score, float64(nowMs()+uint64(longest.Milliseconds())), "赛事局索引的到期时刻同样要盖住战斗期限")
}

// 开局已超过 10 分钟的活场:档位不回落,随机选场与定期清理都不能把它当过期剔除。
func TestLongBattleSurvivesSpectateIndexMaintenance(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	const battleId = uint64(883131)
	member := strconv.FormatUint(battleId, 10)
	_, err := mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()-10*60*1000), member)
	require.NoError(t, err)
	_, err = mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), member)
	require.NoError(t, err)
	raiseTier(t, svcCtx, battleId, tierRelay)

	mr.FastForward(7 * time.Minute)

	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId), "档位只升不降,战斗结束前不能过期")
	picked, err := pickRandomBattle(svcCtx)
	require.NoError(t, err)
	require.Equal(t, battleId, picked)
	cleanupExpiredSpectateIndex(svcCtx)
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "活着的场不能被退配额")
}

// 终局提交后尽力清掉这场的观战数据:L2 配额立刻还给别的场,不再白占到期限;别的场不受影响。
func TestReleaseSpectateOnOutcomeClearsBattleKeys(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	const battleId, other = uint64(883132), uint64(883133)
	seedBattleSpectateKeys(t, svcCtx, mr, battleId)
	seedBattleSpectateKeys(t, svcCtx, mr, other)

	releaseSpectateOnOutcome(svcCtx, battleId)
	idle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, svcCtx.RelayAdmission.WaitIdle(idle))

	requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, other))
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
	claim, err := claimBroadcastQuota(context.Background(), svcCtx, 883134, testRelayDeadlineMs, nowMs())
	require.NoError(t, err)
	require.Equal(t, broadcastQuotaClaimed, claim, "名额还回来了")
}

// 本实例正在排空时任务发不出:不崩、不阻塞终局回执,键留给 TTL 与定期清理。
func TestReleaseSpectateOnOutcomeSkipsWhenDraining(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	const battleId = uint64(883135)
	seedBattleSpectateKeys(t, svcCtx, mr, battleId)
	svcCtx.RelayAdmission.CloseAdmission()

	releaseSpectateOnOutcome(svcCtx, battleId)

	require.True(t, mr.Exists(spectateTierKey(battleId)))
}

// 活跃集合与赛事局索引的过期成员靠这个低频循环回收。意图清理的节拍给一小时,这条用例里不会触发。
func TestSpectateMaintenanceLoopPrunesIndexes(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	const stale, live = "883140", "883141"
	_, err := mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()-uint64(spectateTTLSeconds)*1000-1000), stale)
	require.NoError(t, err)
	_, err = mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()-10*60*1000), live)
	require.NoError(t, err)
	_, err = mr.ZAdd(spectateFeaturedActiveKey, float64(nowMs()-1000), stale)
	require.NoError(t, err)
	_, err = mr.ZAdd(spectateFeaturedActiveKey, float64(testRelayDeadlineMs), live)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startGlobalSpectateMaintenance(ctx, svcCtx, 10*time.Millisecond, time.Hour)

	require.Eventually(t, func() bool {
		active, activeErr := mr.ZMembers(spectateBattlesActiveKey)
		featured, featuredErr := mr.ZMembers(spectateFeaturedActiveKey)
		return activeErr == nil && featuredErr == nil &&
			len(active) == 1 && active[0] == live && len(featured) == 1 && featured[0] == live
	}, 5*time.Second, 10*time.Millisecond)
}

// fakeIntentPurger 记下每一轮的翻页位置。
type fakeIntentPurger struct {
	afters []authority.SpectatorIntentCursor
	next   authority.SpectatorIntentCursor
	err    error
}

func (f *fakeIntentPurger) PurgeClosedSpectatorIntents(_ context.Context, after authority.SpectatorIntentCursor, _ int) (authority.SpectatorIntentCursor, int64, error) {
	f.afters = append(f.afters, after)
	return f.next, 0, f.err
}

// 意图清理的翻页:成功前进;失败不动游标,下一轮重试同一页。
func TestGlobalSpectatorIntentPurgeKeepsCursorOnFailure(t *testing.T) {
	first := authority.SpectatorIntentCursor{CreatedMs: 1, Player: 2, High: 3, Low: 4}
	purger := &fakeIntentPurger{next: first}
	purge := &globalSpectatorIntentPurge{}

	purge.round(context.Background(), purger)
	purger.err = errors.New("TiDB 不可用")
	purge.round(context.Background(), purger)
	purger.err = nil
	purge.round(context.Background(), purger)

	require.Equal(t, []authority.SpectatorIntentCursor{{}, first, first}, purger.afters)
}
