package logic

// 观战档位与 L1 / L2 准入(battle-spectate-tiered-fanout.md §15.6)的 miniredis 测试,走 Redis 路径(本地模式):
// 档位只升不降;L0 满员转 L1 回 LOBBY + 可验签的凭证;同 gate 第二名观众不再 Attach、换实例重新 Attach;Attach 失败不记账;
// 到阈值自动升 L2、配额满保持 L1、Promote 失败退还配额;L2 观众不调 battle;中继关闭时满员回 16021。
// 档位判定与准入函数两条路径共用;全局路径特有的意图结案与重放见 global_spectate_lobby_native_test.go(需要 TiDB)。
// battle 节点的 RPC 用 attachRelayGateFn / promoteSpectateBroadcastFn 顶替(与 addObserverFn 同一模式)。

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
	plpb "proto/player_locator"

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

	// testRelayDeadlineMs 是桩 battle 回的战斗期限,远在未来,配额 Lua 不会把它当过期项清掉。
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
	require.NoError(t, mr.Set(playerSessionKey(playerId), string(raw)))
}

// directSeatsFull 是 battle 对 AddObserver 回「直连席已满」。
func directSeatsFull(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
	return &battlepb.AddObserverResponse{ErrorMessage: tipErr(uint32(table.MatchError_kMatchSpectateFull), "观战人数已满")}, nil
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

// requireLobbyAdmitted 断言回包是 LOBBY 且凭证可验签、绑定这名观众这一场与桩 battle 的实例和期限。
func requireLobbyAdmitted(t *testing.T, resp *matchpb.WatchBattleResponse, battleId, playerId uint64) {
	t.Helper()
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, battleId, resp.GetBattleId())
	require.Equal(t, deliveryLobby, resp.GetDelivery())
	payload := verifiedTicketPayload(t, resp.GetSpectateTicket(), testSpectateTicketSecret)
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
	ttl := time.Duration(spectateTTLSeconds(svcCtx)) * time.Second

	require.Equal(t, tierDirect, tierOf(t, svcCtx, battleId), "键不存在 = DIRECT")

	got, err := raiseSpectateTier(ctx, svcCtx, battleId, tierRelay)
	require.NoError(t, err)
	require.Equal(t, tierRelay, got)
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))
	require.Equal(t, ttl, mr.TTL(key), "首次写入 TTL 同观战记录")

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

// 档位值读不懂:fail-closed,不猜,回 16004、不打 battle。
func TestWatchBattleCorruptTierFailsClosed(t *testing.T) {
	for _, value := range []string{"99", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			observers := stubObserverRPCs(t)
			relay := stubRelayRPCs(t)
			const watcher, battleId = uint64(7400), uint64(883002)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			require.NoError(t, mr.Set(spectateTierKey(battleId), value))

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
			require.Equal(t, deliveryNone, resp.GetDelivery())
			require.Empty(t, observers.adds)
			require.Empty(t, relay.attaches)
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
	require.NotEqual(t, slot, redisHashSlot(spectateTierKey(battleId+1)), "不同场次按 battle_id 分布")
}

// ---- L0 → L1 ----

// 直连席满且中继开启:升 RELAY,本次请求继续做 L1 准入,回 LOBBY + 可验签的凭证,不回 16021、不留观战标记;
// 实例与期限记进观战记录且保持其 TTL,gate 记进 HASH 且带 TTL。
func TestWatchBattleDirectFullTurnsToRelayWithVerifiableTicket(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	observers.onAdd = directSeatsFull
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7401), uint64(883010)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	ttl := time.Duration(spectateTTLSeconds(svcCtx)) * time.Second
	lobbyBase, fullBase := metrics.WatchBattleValue("lobby"), metrics.WatchBattleValue("full")
	relayBase := metrics.SpectateTierValue(metrics.SpectateTierRelay)

	mr.FastForward(5 * time.Second)
	resp := watchBattle(t, svcCtx, watcher, battleId)

	requireLobbyAdmitted(t, resp, battleId, watcher)
	require.Empty(t, resp.GetSpectatorOperationId(), "L1 观众没有 battle 侧的 op")
	require.Equal(t, lobbyBase+1, metrics.WatchBattleValue("lobby"))
	require.Equal(t, fullBase, metrics.WatchBattleValue("full"), "转 L1 不计满员")
	require.Equal(t, relayBase+1, metrics.SpectateTierValue(metrics.SpectateTierRelay))

	require.Len(t, observers.adds, 1)
	require.Empty(t, observers.removes)
	require.Empty(t, watchingMark(t, mr, watcher), "L1 观众没有观战标记")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))

	require.Len(t, relay.attaches, 1)
	attach := relay.attaches[0]
	require.Equal(t, battleId, attach.GetBattleId())
	require.Equal(t, uint32(3), attach.GetGateNodeId())
	require.Equal(t, "gate-inst-3", attach.GetGateInstanceId())
	require.Equal(t, testBattle7Instance, attach.GetTargetInstanceId())
	require.Empty(t, relay.promotes, "gate 数没到阈值不升 L2")

	gatesKey := spectateRelayGatesKey(battleId)
	require.Equal(t, "gate-inst-3", mr.HGet(gatesKey, "3"))
	require.Equal(t, ttl, mr.TTL(gatesKey))

	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.Equal(t, testBattle7Instance, record.GetBattleInstanceId())
	require.Equal(t, testRelayDeadlineMs, record.GetDeadlineMs())
	require.Equal(t, ttl-5*time.Second, mr.TTL(spectateBattleKey(battleId)), "写回实例与期限不延长记录的 TTL")
}

// 满员转 L1:这个 op 的标记必须在 L1 准入之前就回滚。L1 观众没有观战标记,残留的标记只会让下一次 WatchBattle 去取消一个
// 从未登记的 op,gather 也会多发一次 Remove。
func TestWatchBattleDirectFullReleasesMarkBeforeRelayAdmission(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7490), uint64(883091)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	observers.onAdd = func(req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		require.NotEmpty(t, watchingMark(t, mr, watcher), "标记先于 Add 生效")
		return directSeatsFull(req)
	}
	relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		require.Empty(t, watchingMark(t, mr, watcher), "转 L1 之前标记必须已回滚")
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs}, nil
	}

	resp := watchBattle(t, svcCtx, watcher, battleId)

	requireLobbyAdmitted(t, resp, battleId, watcher)
	require.Len(t, relay.attaches, 1)
	require.Empty(t, observers.removes, "battle 明确回满员,这个 op 不会登记,不用取消")
}

// 满员但回滚标记失败(Redis 故障):按 16004 收场,不升档、不转 L1;残留的标记由下一次 WatchBattle 取消后清掉。
func TestWatchBattleDirectFullMarkRollbackFailureDoesNotTurnToRelay(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	observers.onAdd = directSeatsFull
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7491), uint64(883092)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	failWatchingMarkRelease(t, mr)

	resp := watchBattle(t, svcCtx, watcher, battleId)

	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Equal(t, deliveryNone, resp.GetDelivery())
	require.Empty(t, relay.attaches, "回滚失败不转 L1")
	require.False(t, mr.Exists(spectateTierKey(battleId)), "回滚失败不升档")
	require.True(t, proto.Equal(observers.adds[0], watchingOriginal(t, mr, watcher)))
}

// 同 gate 第二名观众不再调 Attach,也不再撞 L0;换一台 gate 要挂;gate 重启(同 node 新实例)要重新挂并覆盖账本。
func TestWatchBattleAttachesEachGateInstanceOnce(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0 // 这条用例不看升 L2
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(883011)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	gatesKey := spectateRelayGatesKey(battleId)

	setPlayerSessionOnGate(t, mr, 7411, "3", "gate-inst-3")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7411, battleId), battleId, 7411)
	require.Len(t, relay.attaches, 1)

	setPlayerSessionOnGate(t, mr, 7412, "3", "gate-inst-3")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7412, battleId), battleId, 7412)
	require.Len(t, relay.attaches, 1, "同 gate 第二名观众不再调 Attach,实例与期限取自观战记录")

	setPlayerSessionOnGate(t, mr, 7413, "4", "gate-inst-4")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7413, battleId), battleId, 7413)
	require.Len(t, relay.attaches, 2, "新 gate 要挂")
	require.Equal(t, uint32(4), relay.attaches[1].GetGateNodeId())

	setPlayerSessionOnGate(t, mr, 7414, "3", "gate-inst-3b")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7414, battleId), battleId, 7414)
	require.Len(t, relay.attaches, 3, "同 node 换实例要重新挂")
	require.Equal(t, "gate-inst-3b", relay.attaches[2].GetGateInstanceId())
	require.Equal(t, "gate-inst-3b", mr.HGet(gatesKey, "3"), "账本跟随新实例")

	require.Empty(t, observers.adds, "已升档的场不再撞 L0")
}

// 账本有这台 gate 但记录里没有实例与期限(被 gather 的同值补写盖掉):再问 battle 一次,不能签出缺字段的凭证。
func TestWatchBattleReattachesWhenRecordLostSource(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(883012)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	mr.HSet(spectateRelayGatesKey(battleId), "3", "gate-inst-3")
	setPlayerSessionOnline(t, mr, 7415)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7415, battleId), battleId, 7415)
	require.Len(t, relay.attaches, 1)
}

// Attach 失败不记账:RPC 失败与 battle 的其它拒绝都回 16018、不动索引、不留账;恢复后下一名观众照常挂上。
func TestWatchBattleAttachFailureIsNotRecorded(t *testing.T) {
	cases := []struct {
		name   string
		attach func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error)
	}{
		{"RPC 失败", func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return nil, errors.New("battle 节点不可达")
		}},
		{"battle 拒绝(实例不符 / 非公开)", func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "拒绝")}, nil
		}},
		{"回包缺实例", func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{DeadlineMs: testRelayDeadlineMs}, nil
		}},
		{"回包缺期限", func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId()}, nil
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			stubObserverRPCs(t)
			relay := stubRelayRPCs(t)
			relay.onAttach = c.attach
			watcher, battleId := uint64(7420+i), uint64(883020+i)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
			base := metrics.WatchBattleValue("rejected")

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
			require.Equal(t, deliveryNone, resp.GetDelivery())
			require.Empty(t, resp.GetSpectateTicket())
			require.Equal(t, base+1, metrics.WatchBattleValue("rejected"))
			require.Empty(t, mr.HGet(spectateRelayGatesKey(battleId), "3"), "先 RPC 后记账:失败不记账")
			require.True(t, mr.Exists(spectateBattleKey(battleId)), "战斗仍在打,索引不许剔除")

			relay.onAttach = nil
			requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Equal(t, "gate-inst-3", mr.HGet(spectateRelayGatesKey(battleId), "3"))
		})
	}
}

// battle 回房间不存在:走现有懒剔除口径,指定场回「不存在」,随机模式换一场。
func TestWatchBattleAttachMissingRoomEvictsIndex(t *testing.T) {
	missing := func(*battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在")}, nil
	}
	t.Run("指定场", func(t *testing.T) {
		svcCtx, mr := newRelaySvcCtx(t)
		stubObserverRPCs(t)
		relay := stubRelayRPCs(t)
		relay.onAttach = missing
		const watcher, battleId = uint64(7430), uint64(883030)
		setPlayerSessionOnline(t, mr, watcher)
		registerBattle(t, svcCtx, battleId, nowMs())
		raiseTier(t, svcCtx, battleId, tierRelay)

		resp := watchBattle(t, svcCtx, watcher, battleId)
		require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
		require.False(t, mr.Exists(spectateBattleKey(battleId)), "房间不存在 → 懒剔除记录")
		require.Zero(t, activeIndexSize(t, svcCtx))
	})
	t.Run("随机模式换一场", func(t *testing.T) {
		svcCtx, mr := newRelaySvcCtx(t)
		stubObserverRPCs(t)
		relay := stubRelayRPCs(t)
		const watcher = uint64(7431)
		setPlayerSessionOnline(t, mr, watcher)
		for _, battleId := range []uint64{883031, 883032} {
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
		}
		var first uint64
		relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
			if first == 0 {
				first = req.GetBattleId()
				return missing(req)
			}
			return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs}, nil
		}

		resp := watchBattle(t, svcCtx, watcher, 0)
		require.NotEqual(t, first, resp.GetBattleId(), "必须换到另一场")
		requireLobbyAdmitted(t, resp, resp.GetBattleId(), watcher)
		require.False(t, mr.Exists(spectateBattleKey(first)), "已收尾的那场被懒剔除")
	})
}

// 本场已是广播档:battle 回 broadcast=true、没有挂 gate。档位同步成 BROADCAST,账本不记这台 gate,配额补记,凭证照发。
func TestWatchBattleAttachBroadcastReplySyncsTier(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs, Broadcast: true}, nil
	}
	const watcher, battleId = uint64(7435), uint64(883035)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	base := metrics.SpectateTierValue(metrics.SpectateTierBroadcast)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
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
func TestWatchBattleAttachBroadcastReplyRecordsQuotaOverLimit(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	relay.onAttach = func(req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
		return &battlepb.AttachRelayGateResponse{BattleInstanceId: req.GetTargetInstanceId(), DeadlineMs: testRelayDeadlineMs, Broadcast: true}, nil
	}
	const watcher, battleId = uint64(7436), uint64(883036)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), "999999")
	require.NoError(t, err)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)

	require.Equal(t, 2, broadcastQuotaCount(t, svcCtx), "超额也记")
	claim, err := claimBroadcastQuota(context.Background(), svcCtx, 883037, testRelayDeadlineMs, nowMs())
	require.NoError(t, err)
	require.Equal(t, broadcastQuotaFull, claim, "后面的抢占被挡住")
}

// ---- L1 → L2 ----

// 有观众的 gate 数到阈值:抢配额 → Promote → 档位 BROADCAST,配额 score 换成 battle 回的真实期限;之后的观众不调任何 battle RPC。
func TestWatchBattleAutoPromotesAtGateThreshold(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(883040)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	okBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteOK)

	setPlayerSessionOnGate(t, mr, 7441, "3", "gate-inst-3")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7441, battleId), battleId, 7441)
	require.Empty(t, relay.promotes, "一台 gate 没到阈值")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))

	setPlayerSessionOnGate(t, mr, 7442, "4", "gate-inst-4")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7442, battleId), battleId, 7442)
	require.Len(t, relay.promotes, 1)
	require.Equal(t, battleId, relay.promotes[0].GetBattleId())
	require.Equal(t, testBattle7Instance, relay.promotes[0].GetTargetInstanceId())
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
	require.Equal(t, okBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteOK))
	score, err := mr.ZScore(spectateBroadcastActiveKey, strconv.FormatUint(battleId, 10))
	require.NoError(t, err)
	require.Equal(t, float64(testRelayDeadlineMs), score)

	setPlayerSessionOnGate(t, mr, 7443, "5", "gate-inst-5")
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7443, battleId), battleId, 7443)
	require.Len(t, relay.attaches, 2, "L2 观众不调 Attach")
	require.Len(t, relay.promotes, 1, "L2 观众不调 Promote")
	require.Empty(t, observers.adds, "L2 观众不撞 L0")
}

// 配额满:不报错,留在 L1,观众照常进;名额空出来后下一名观众(同 gate,不用 Attach)再升。
func TestWatchBattlePromoteQuotaFullStaysRelay(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 1
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7445), uint64(883045)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), "999999")
	require.NoError(t, err)
	fullBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteQuotaFull)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Empty(t, relay.promotes, "配额满不调 battle")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
	require.Equal(t, fullBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteQuotaFull))

	_, err = svcCtx.MatchRedis.Zrem(spectateBroadcastActiveKey, "999999")
	require.NoError(t, err)
	mr.FastForward(spectatePromoteLeaseSeconds * time.Second) // 上一次尝试的租约到期
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.promotes, 1, "名额空出来就升")
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
}

// Promote 失败:退还配额、保持 RELAY,观众不受影响;恢复后下一名观众升上去。
func TestWatchBattlePromoteFailureReleasesQuota(t *testing.T) {
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
			stubObserverRPCs(t)
			relay := stubRelayRPCs(t)
			relay.onPromote = c.promote
			watcher, battleId := uint64(7450+i), uint64(883050+i)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
			failedBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteRPCFailed)

			requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Len(t, relay.promotes, 1)
			require.Zero(t, broadcastQuotaCount(t, svcCtx), "失败退还配额")
			require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId), "升档失败保持原档")
			require.Equal(t, failedBase+1, metrics.SpectatePromoteValue(metrics.SpectatePromoteRPCFailed))

			relay.onPromote = nil
			mr.FastForward(spectatePromoteLeaseSeconds * time.Second) // 上一次尝试的租约到期
			requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
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
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		return nil, errors.New("battle 节点过载")
	}
	const battleId = uint64(883057)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	throttledBase := metrics.SpectatePromoteValue(metrics.SpectatePromoteThrottled)

	for _, watcher := range []uint64{7500, 7501, 7502} {
		setPlayerSessionOnline(t, mr, watcher)
		requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	}
	require.Len(t, relay.promotes, 1, "租约期内只有一名观众发 RPC")
	require.Equal(t, throttledBase+2, metrics.SpectatePromoteValue(metrics.SpectatePromoteThrottled))
	require.Zero(t, broadcastQuotaCount(t, svcCtx), "失败已退还,其余观众没有碰配额")
	require.Equal(t, tierRelay, tierOf(t, svcCtx, battleId))

	mr.FastForward(spectatePromoteLeaseSeconds * time.Second)
	relay.onPromote = nil
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7502, battleId), battleId, 7502)
	require.Len(t, relay.promotes, 2, "租约到期后再试")
	require.Equal(t, tierBroadcast, tierOf(t, svcCtx, battleId))
}

// 租约键与本场的键共用 hash tag(§15.6 约定)。
func TestSpectatePromoteTryKeySharesBattleHashTag(t *testing.T) {
	require.Equal(t, redisHashSlot(spectateTierKey(42)), redisHashSlot(spectatePromoteTryKey(42)))
}

// 配额账:新占者的 Promote 超时,而别的请求已经把这场升上去(持有者卡过租约的极端时序):退还会抹掉成功者的账,本场继续广播却不计数。
func TestPromoteFailureKeepsQuotaWhenTierAlreadyBroadcast(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7456), uint64(883056)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		raiseTier(t, svcCtx, battleId, tierBroadcast)
		return nil, errors.New("Promote 回包超时")
	}
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "已是 BROADCAST 就不退")
}

// 配额账:退还前读不到档位时不退。最坏白占到本场结束,比突破 B 的代价小。
func TestPromoteFailureKeepsQuotaWhenTierUnreadable(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7459), uint64(883059)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		require.NoError(t, mr.Set(spectateTierKey(battleId), "not-a-number"))
		return nil, errors.New("Promote 回包超时")
	}
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx))
}

// Promote 回房间不存在:已挂接 gate 的观众不调 Attach,这是 match 唯一能知道场已结束的机会。按懒剔除口径拒绝这名观众、清掉本场全部观战数据,
// 不论配额是不是这次新占的。
func TestWatchBattlePromoteMissingRoomRejectsViewerAndEvicts(t *testing.T) {
	for i, heldByOther := range []bool{false, true} {
		t.Run(strconv.FormatBool(heldByOther), func(t *testing.T) {
			svcCtx, mr := newRelaySvcCtx(t)
			svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
			stubObserverRPCs(t)
			relay := stubRelayRPCs(t)
			relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
				return &battlepb.PromoteSpectateBroadcastResponse{ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在")}, nil
			}
			watcher, battleId := uint64(7510+i), uint64(883110+i)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			raiseTier(t, svcCtx, battleId, tierRelay)
			if heldByOther {
				_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), strconv.FormatUint(battleId, 10))
				require.NoError(t, err)
			}

			resp := watchBattle(t, svcCtx, watcher, battleId)

			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
			require.Equal(t, deliveryNone, resp.GetDelivery())
			require.Empty(t, resp.GetSpectateTicket())
			require.Zero(t, broadcastQuotaCount(t, svcCtx))
			requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
		})
	}
}

// battle 节点重启(同 node_id 新实例):已挂接 gate 的观众与 L2 观众都不调 battle,靠内存注册表发现来源实例已不存在,按房间不存在处理。
func TestWatchBattleRejectsTicketForReplacedInstance(t *testing.T) {
	for i, tier := range []battlepb.ESpectateTier{tierRelay, tierBroadcast} {
		t.Run(tier.String(), func(t *testing.T) {
			svcCtx, mr, battleId, watcher := newKnownSourceBattle(t, tier, i)
			relay := stubRelayRPCs(t)
			stubObserverRPCs(t)
			requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)

			svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, NodeUuid: "replaced-instance", Endpoint: "battle-7"})
			resp := watchBattle(t, svcCtx, watcher, battleId)

			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
			require.Empty(t, resp.GetSpectateTicket())
			require.Empty(t, relay.attaches, "不增加 RPC")
			require.Empty(t, relay.promotes, "不增加 RPC")
			requireBattleSpectateKeysGone(t, svcCtx, mr, battleId)
		})
	}
}

// 注册表查不到不判:etcd 抖动、租约短暂失效时同样查不到,不代表实例已死,凭证照签。
func TestWatchBattleSignsTicketWhenInstanceNotInRegistry(t *testing.T) {
	for i, tier := range []battlepb.ESpectateTier{tierRelay, tierBroadcast} {
		t.Run(tier.String(), func(t *testing.T) {
			svcCtx, _, battleId, watcher := newKnownSourceBattle(t, tier, i+10)
			relay := stubRelayRPCs(t)
			stubObserverRPCs(t)
			svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)

			requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
			require.Empty(t, relay.attaches)
			require.Empty(t, relay.promotes)
		})
	}
}

// newKnownSourceBattle 铺一场已升档、记录里有来源实例、观众所在 gate 已挂接的战斗:这名观众的准入不调任何 battle RPC。
func newKnownSourceBattle(t *testing.T, tier battlepb.ESpectateTier, seq int) (*svc.ServiceContext, *miniredis.Miniredis, uint64, uint64) {
	t.Helper()
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 0
	watcher, battleId := uint64(7520+seq), uint64(883120+seq)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tier)
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	rememberSpectateSource(context.Background(), svcCtx, record, spectateSource{battleInstanceId: testBattle7Instance, deadlineMs: testRelayDeadlineMs})
	mr.HSet(spectateRelayGatesKey(battleId), "3", "gate-inst-3")
	return svcCtx, mr, battleId, watcher
}

// 配额只由新占到的请求退还:本场已在集合里(另一名观众正在升档,或已经升上去)时,这次失败不能把配额退掉。
func TestPromoteFailureKeepsQuotaHeldByAnotherRequest(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	relay.onPromote = func(*battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
		return nil, errors.New("battle 节点不可达")
	}
	const watcher, battleId = uint64(7455), uint64(883055)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	_, err := mr.ZAdd(spectateBroadcastActiveKey, float64(testRelayDeadlineMs), strconv.FormatUint(battleId, 10))
	require.NoError(t, err)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Len(t, relay.promotes, 1)
	require.Equal(t, 1, broadcastQuotaCount(t, svcCtx), "不是自己占的配额不退")
}

// B=0 = 不开 L2:到了阈值也不抢配额、不调 battle。
func TestWatchBattleBroadcastDisabledNeverPromotes(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Config.Spectate.BroadcastMaxConcurrent = 0
	svcCtx.Config.Spectate.BroadcastAutoGateThreshold = 1
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7458), uint64(883058)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
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

// L2 观众不调任何 battle RPC:凭证的实例与期限取自观战记录。
func TestWatchBattleBroadcastTierViewerCallsNoBattleRPC(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7460), uint64(883060)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierBroadcast)
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	rememberSpectateSource(context.Background(), svcCtx, record, spectateSource{battleInstanceId: testBattle7Instance, deadlineMs: testRelayDeadlineMs})
	base := metrics.SpectateTierValue(metrics.SpectateTierBroadcast)

	requireLobbyAdmitted(t, watchBattle(t, svcCtx, watcher, battleId), battleId, watcher)
	require.Empty(t, observers.adds)
	require.Empty(t, relay.attaches)
	require.Empty(t, relay.promotes)
	require.Equal(t, base+1, metrics.SpectateTierValue(metrics.SpectateTierBroadcast))
}

// BROADCAST 档而记录里没有实例与期限(升档者记账失败):问 battle 一次(幂等)并记下,后面的观众不再问。
func TestWatchBattleBroadcastTierRecoversLostSourceOnce(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const battleId = uint64(883062)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierBroadcast)

	setPlayerSessionOnline(t, mr, 7462)
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7462, battleId), battleId, 7462)
	require.Len(t, relay.promotes, 1)

	setPlayerSessionOnline(t, mr, 7463)
	requireLobbyAdmitted(t, watchBattle(t, svcCtx, 7463, battleId), battleId, 7463)
	require.Len(t, relay.promotes, 1)
	require.Empty(t, relay.attaches)
}

// ---- 中继关闭 ----

// 中继不可用:满员回 16021,不写档位、不 Attach;没有密钥(dev / test)与开关关闭同样处理,不签 gate 必然拒绝的凭证。
func TestWatchBattleDirectFullWithRelayUnusableReturnsFull(t *testing.T) {
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
			svcCtx.Config.Spectate = c.conf
			observers := stubObserverRPCs(t)
			observers.onAdd = directSeatsFull
			relay := stubRelayRPCs(t)
			watcher, battleId := uint64(7470+i), uint64(883070+i)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			base := metrics.WatchBattleValue("full")

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrSpectateFull, resp.GetErrorMessage().GetId())
			require.Equal(t, deliveryNone, resp.GetDelivery())
			require.Empty(t, resp.GetSpectateTicket())
			require.Equal(t, base+1, metrics.WatchBattleValue("full"))
			require.False(t, mr.Exists(spectateTierKey(battleId)), "不升档")
			require.Empty(t, relay.attaches)
			require.Empty(t, watchingMark(t, mr, watcher), "失败回滚标记")
		})
	}
}

// 开关回滚:已升档的场在开关关闭后回到上一档的行为,观众重新走 L0,不看档位键(§11)。
func TestWatchBattleRelayDisabledIgnoresRaisedTier(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7475), uint64(883075)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	svcCtx.Config.Spectate.RelayEnabled = false

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, deliveryDirect, resp.GetDelivery())
	require.Len(t, observers.adds, 1)
	require.Empty(t, relay.attaches)
}

// L0 成功回 DIRECT、不带凭证;中继开着也不影响直连席还没满的场。
func TestWatchBattleDirectSeatAnswersDirectDelivery(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	observers := stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7476), uint64(883076)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	base := metrics.SpectateTierValue(metrics.SpectateTierDirect)

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, deliveryDirect, resp.GetDelivery())
	require.Empty(t, resp.GetSpectateTicket())
	require.NotEmpty(t, resp.GetSpectatorOperationId())
	require.Len(t, observers.adds, 1)
	require.Empty(t, relay.attaches)
	require.False(t, mr.Exists(spectateTierKey(battleId)))
	require.Equal(t, base+1, metrics.SpectateTierValue(metrics.SpectateTierDirect))
}

// 互斥检查先于档位:排队中的玩家不会被放进 L1(软互斥的只读检查,Q1)。
func TestWatchBattleRelayTierStillRejectsQueuedPlayer(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7477), uint64(883077)
	setPlayerLocation(t, mr, watcher, 1)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	raiseTier(t, svcCtx, battleId, tierRelay)
	require.Zero(t, joinQueue1v1(t, svcCtx, watcher).ErrorCode)

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrSpectateWhileQueued, resp.GetErrorMessage().GetId())
	require.Empty(t, relay.attaches)
	require.Empty(t, resp.GetSpectateTicket())
}

// 可见性闸先于档位:私密局即使残留档位键也不能进 L1 / L2。
func TestWatchBattleRelayTierStillRejectsPrivateBattle(t *testing.T) {
	svcCtx, mr := newRelaySvcCtx(t)
	stubObserverRPCs(t)
	relay := stubRelayRPCs(t)
	const watcher, battleId = uint64(7478), uint64(883078)
	setPlayerSessionOnline(t, mr, watcher)
	registerPrivateBattle(t, svcCtx, battleId, false)
	raiseTier(t, svcCtx, battleId, tierBroadcast)

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Empty(t, resp.GetSpectateTicket())
	require.Empty(t, relay.attaches)
	require.Empty(t, relay.promotes)
}

// 全局模式的观战记录由成局原件现算:L2 观众不调 battle,凭证的实例与期限全靠这里;原件缺失不能算出空记录。
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
	require.NoError(t, mr.Set(spectateBattleKey(battleId), "record"))
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
	for _, key := range []string{spectateBattleKey(battleId), spectateTierKey(battleId), spectateRelayGatesKey(battleId), spectateFeaturedKey(battleId)} {
		require.False(t, mr.Exists(key), key)
	}
	member := strconv.FormatUint(battleId, 10)
	for _, key := range []string{spectateBattlesActiveKey, spectateBroadcastActiveKey, spectateFeaturedActiveKey} {
		_, err := svcCtx.MatchRedis.Zscore(key, member)
		require.ErrorIs(t, err, redis.Nil, key)
	}
}

// ---- 全局模式的键生命周期 ----
// 下面这些函数只读写 Redis、不碰 SQL,Authority 只用来选中全局路径(Store 没有连接池,碰到 SQL 就 panic)。
// 经 TiDB 的完整路径见 global_spectate_lobby_native_test.go。

func newGlobalRedisSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newRelaySvcCtx(t)
	svcCtx.Authority = &authority.Store{}
	return svcCtx, mr
}

// 全局成局的期限 = 提交时刻 + 备战预算 + 模板时长,最长超过 1 小时:观战键的 TTL 要盖住它,再留收尾余量。本地模式的 TTL 不变。
func TestGlobalSpectateTTLCoversLongestBattle(t *testing.T) {
	svcCtx, mr := newGlobalRedisSvcCtx(t)
	ctx := context.Background()
	const battleId = uint64(883130)
	longest := time.Duration(catalog.MaxBattleSeconds)*time.Second + time.Duration(authority.WorkLeaseMs)*time.Millisecond
	ttl := time.Duration(spectateTTLSeconds(svcCtx)) * time.Second
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

	svcCtx.Authority = nil
	require.Equal(t, 300+60, spectateTTLSeconds(svcCtx), "本地模式按 BattleMaxDurationSeconds")
}

// 开局已超过本地 TTL(360 秒)的活场:全局模式下档位不回落,随机选场与定期清理都不能把它当过期剔除。
func TestGlobalModeLongBattleSurvivesPastLocalTTL(t *testing.T) {
	svcCtx, mr := newGlobalRedisSvcCtx(t)
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
	svcCtx, mr := newGlobalRedisSvcCtx(t)
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
	svcCtx, mr := newGlobalRedisSvcCtx(t)
	const battleId = uint64(883135)
	seedBattleSpectateKeys(t, svcCtx, mr, battleId)
	svcCtx.RelayAdmission.CloseAdmission()

	releaseSpectateOnOutcome(svcCtx, battleId)

	require.True(t, mr.Exists(spectateTierKey(battleId)))
}

// 全局模式没有 matcher 轮,活跃集合与赛事局索引的过期成员靠这个低频循环回收。意图清理的节拍给一小时,这条用例里不会触发。
func TestGlobalSpectateMaintenanceLoopPrunesIndexes(t *testing.T) {
	svcCtx, mr := newGlobalRedisSvcCtx(t)
	const stale, live = "883140", "883141"
	_, err := mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()-uint64(spectateTTLSeconds(svcCtx))*1000-1000), stale)
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
