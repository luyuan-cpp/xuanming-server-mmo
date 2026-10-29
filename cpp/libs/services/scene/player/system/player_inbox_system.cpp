#include "player/system/player_inbox_system.h"

#include <algorithm>
#include <chrono>
#include <cstddef>
#include <cstdlib>
#include <limits>
#include <memory>
#include <string>
#include <vector>

#include <hiredis/hiredis.h>
#include <muduo/contrib/hiredis/Hiredis.h>
#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include <node_config_manager.h>
#include "battle/system/player_battle.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "core/network/grpc_client_call.h"          // kGrpcStatusUnimplemented
#include "core/security/service_call_auth.h"
#include "player/system/role_admission_system.h"
#include "time/system/time.h"
#include "grpc_client/player_inbox/player_inbox_grpc_client.h"
#include "muduo_grpc/channel.h"                 // 选点读 muduo_grpc::Channel::GetState / ConnectivityState
#include "network/node_utils.h"                 // GetNodeInfo:Fetch 带上本节点 node_id(只用于对端日志)
#include "node/system/grpc_call_deadline.h"     // 在途超时从 PlayerInboxNodeService 的 gRPC deadline 派生
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_inbox_runtime_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/player_inbox_registry.h"
#include "player/system/player_inbox_rules.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "proto/common/base/common.pb.h"        // NodeInfo
#include "proto/common/base/node.pb.h"          // PlayerInboxNodeService
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "thread_context/redis_manager.h"
#include "utils/random/random.h"

namespace
{
	using player_inbox_rules::Durability;
	using player_inbox_rules::ItemAction;

	// 在途请求的兜底期限 = 该类节点的 gRPC deadline + 5s(上游比下游宽,grpc-muduo-transport.md §16.3)。
	// 生成的客户端保证每次调用在 deadline 内以应答或失败之一收场,正常走不到这里;它只防"完成通知永远不来"。
	constexpr uint64_t kInflightSlackMs = 5000;

	// ── 可注入依赖(AGENTS §11.2 显式依赖;生产取默认值)────────────────────────

	// 进程内的退避、去抖、超时一律用单调时钟(AGENTS §11.3):系统时间被回拨时不能让检查长期不放行。
	uint64_t DefaultSteadyNowMs()
	{
		return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
										 std::chrono::steady_clock::now().time_since_epoch())
										 .count());
	}

	// 只在候选里挑**挂着 muduo_grpc 通道且通道 READY** 的节点(照 id_segment_bootstrap.cpp 的 PickReadyDataServiceNode):
	// ConnectToGrpcNode 先 emplace MuduoGrpcChannelPtr 再 emplace NodeInfo,两者同在才是可用连接。
	// 通道不 READY 时发出去,TRANSIENT_FAILURE 立即换回 UNAVAILABLE,IDLE / CONNECTING 则排队等连接、连不上就占着在途状态
	// 直到 deadline;直接回"发不出去",由调用方退避。
	// GetState(true) 顺手把 READY 连接断开后回到 IDLE 的通道踢回 CONNECTING。收件箱是全局服务,不比对 zone。
	entt::entity PickReadyPlayerInboxNode()
	{
		auto &registry = tlsNodeContextManager.GetRegistry(common::base::PlayerInboxNodeService);
		std::vector<entt::entity> ready;
		for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
		{
			const auto &channel = registry.get<MuduoGrpcChannelPtr>(entity);
			if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady)
			{
				ready.push_back(entity);
			}
		}
		if (ready.empty())
		{
			return entt::null;
		}
		return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
	}

	bool DefaultSendFetch(const player_inbox::FetchPlayerInboxRequest &request)
	{
		const auto admission = RoleAdmissionSystem::ConfirmedIdentity(request.player_id());
		const char* secret = std::getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE");
		if (!admission || admission->owner_epoch() != request.owner_epoch() || secret == nullptr) return false;
		auto signedRequest = request;
		*signedRequest.mutable_admission() = *admission;
		if (!service_call_auth::Sign("/player_inbox.PlayerInbox/FetchItems", signedRequest,
			*signedRequest.mutable_auth(), "scene", secret, TimeSystem::NowMillisecondsUTC())) return false;
		const entt::entity node = PickReadyPlayerInboxNode();
		if (node == entt::null)
		{
			return false;
		}
		player_inbox::SendPlayerInboxFetchItems(
			tlsNodeContextManager.GetRegistry(common::base::PlayerInboxNodeService), node, signedRequest,
			[playerId = request.player_id(), domain = static_cast<uint32_t>(request.domain()),
			 correlationId = request.correlation_id(), original = *admission](const GrpcCallOutcome &outcome,
													 const player_inbox::FetchPlayerInboxResponse *reply)
			{
				const auto current = RoleAdmissionSystem::ConfirmedIdentity(playerId);
				if (!current || current->SerializeAsString() != original.SerializeAsString()) return;
				if (outcome.ok() && reply != nullptr)
				{
					PlayerInboxSystem::OnFetchReply(playerId, domain, correlationId, *reply);
					return;
				}
				PlayerInboxSystem::OnFetchFailed(playerId, domain, correlationId,
												 outcome.ok() ? kGrpcStatusInternal : outcome.code, outcome.message);
			});
		return true;
	}

	bool DefaultSendAck(const player_inbox::AckPlayerInboxRequest &request)
	{
		const auto admission = RoleAdmissionSystem::ConfirmedIdentity(request.player_id());
		const char* secret = std::getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE");
		if (!admission || admission->owner_epoch() != request.owner_epoch() || secret == nullptr) return false;
		auto signedRequest = request;
		*signedRequest.mutable_admission() = *admission;
		if (!service_call_auth::Sign("/player_inbox.PlayerInbox/AckItems", signedRequest,
			*signedRequest.mutable_auth(), "scene", secret, TimeSystem::NowMillisecondsUTC())) return false;
		const entt::entity node = PickReadyPlayerInboxNode();
		if (node == entt::null)
		{
			return false;
		}
		player_inbox::SendPlayerInboxAckItems(
			tlsNodeContextManager.GetRegistry(common::base::PlayerInboxNodeService), node, signedRequest,
			[playerId = request.player_id(), domain = static_cast<uint32_t>(request.domain()),
			 correlationId = request.correlation_id(), original = *admission](const GrpcCallOutcome &outcome,
														 const player_inbox::AckPlayerInboxResponse *reply)
			{
				const auto current = RoleAdmissionSystem::ConfirmedIdentity(playerId);
				if (!current || current->SerializeAsString() != original.SerializeAsString()) return;
				if (outcome.ok() && reply != nullptr)
				{
					PlayerInboxSystem::OnAckReply(playerId, domain, correlationId, *reply);
					return;
				}
				PlayerInboxSystem::OnAckFailed(playerId, domain, correlationId,
											   outcome.ok() ? kGrpcStatusInternal : outcome.code, outcome.message);
			});
		return true;
	}

	PlayerInboxSystem::PersistFn gPersistFn = &PlayerLifecycleSystem::SavePlayerToRedis;
	PlayerInboxSystem::NowMsFn gNowMsFn = &DefaultSteadyNowMs;
	PlayerInboxSystem::SendFetchFn gSendFetchFn = &DefaultSendFetch;
	PlayerInboxSystem::SendAckFn gSendAckFn = &DefaultSendAck;

	struct ConsumerConfigOverride
	{
		bool active{false};
		bool consumerEnabled{false};
		uint32_t periodicFetchMinutes{0};
	};
	ConsumerConfigOverride gConfigOverride;

	// 关联号:本节点(loop 线程)单调递增,不按实体从头计(设计 §7.2)。已销毁实体的在途回包因此不可能与
	// 新实体的请求同号;0 保留给"没有在途"。
	thread_local uint64_t tlsCorrelationSeq = 0;
	// 注册表自检失败 = 本节点停用消费(PlayerInboxSystem::Initialize)。
	thread_local bool tlsRegistryBroken = false;
	// 自检失败的原因与下一次补打 ERROR 的时刻(单调时钟)。只在启动时打一次的话,[10m] 窗口的日志告警亮 10 分钟
	// 就自己灭了,而本节点上所有在线玩家的处置一直不执行(评审 F3);Tick 按 kNodeStuckRelogIntervalMs 补打同一行。
	thread_local std::string tlsRegistryBrokenDetail;
	thread_local uint64_t tlsNextRegistryBrokenLogMs = 0;
	// 消费开关关闭时下一次补打 WARN 的时刻(单调时钟,评审 H1)。0 = 未排期:开关开着,或还没打过首条;
	// 开关一关,下一个 Tick 立即打首条。同一份节拍与 registry_broken 共用(TakeNodeRelogTurn)。
	thread_local uint64_t tlsNextConsumerDisabledLogMs = 0;

	bool IsBattleDomain(const PlayerInboxDomainState& state)
	{
		return state.domain == static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT);
	}

	uint64_t NextCorrelationId()
	{
		return ++tlsCorrelationSeq;
	}

	// 配置开关本身,不含注册表自检。metric=inbox_consumer_disabled 只按它判:自检失败另有 metric=inbox_registry_broken。
	bool ConfiguredConsumerEnabled()
	{
		if (gConfigOverride.active)
		{
			return gConfigOverride.consumerEnabled;
		}
		return tlsNodeConfigManager.GetGameConfig().player_inbox().consumer_enabled();
	}

	// 生效的消费开关:注册表自检失败时一律关。
	bool ConsumerEnabled()
	{
		return !tlsRegistryBroken && ConfiguredConsumerEnabled();
	}

	uint32_t PeriodicFetchMinutes()
	{
		if (gConfigOverride.active)
		{
			return gConfigOverride.periodicFetchMinutes;
		}
		return tlsNodeConfigManager.GetGameConfig().player_inbox().periodic_fetch_minutes();
	}

	uint32_t JitterPermille()
	{
		return tlsRandom.Rand<uint32_t>(0, player_inbox_rules::kJitterPermilleMax);
	}

	// metric= 标记供日志告警(评审 L13):本节点上所有在线玩家的处置都不执行,表面上与离线无异。
	void LogRegistryBroken()
	{
		LOG_ERROR << "[PlayerInbox] metric=inbox_registry_broken detail=" << tlsRegistryBrokenDetail
				  << "; registry self-check failed, inbox consumption disabled on this node";
	}

	// metric= 标记供日志告警(评审 H1):开关关着时,收件箱服务照常收单(入箱回 created=true),本节点却一条都不执行;
	// 条目停在 PENDING、delivery_count 为 0,服务端没有任何告警看得到。例行发布曾因开关不粘滞把它静默关掉。
	// 启动时打一条,之后 Tick 按 kNodeStuckRelogIntervalMs 补打同一行。键名 metric=inbox_consumer_disabled 是
	// 告警固定查询的契约,不要改。
	void LogConsumerDisabled()
	{
		LOG_WARN << "[PlayerInbox] metric=inbox_consumer_disabled consumer_enabled=0"
				 << " periodic_fetch_minutes=" << PeriodicFetchMinutes()
				 << " consumer_domains=" << player_inbox_registry::ConsumerDomains().size()
				 << "; player_inbox.consumer_enabled is off, inbox items are accepted by the service but not executed"
				 << " on this node";
	}

	// 节点级停用的补打节拍,registry_broken 与 consumer_disabled 共用(评审 F3、H1)。这类停用没有逐玩家的退避可数,
	// 只能按时间补打。nextLogMs 是下一条的时刻(单调时钟),0 = 未排期,立即到点。到点时返回 true,并把下一条
	// 排到 kNodeStuckRelogIntervalMs 之后;该间隔由 player_inbox_rules 的 static_assert 钉在 [10m] 告警窗口之内。
	bool TakeNodeRelogTurn(uint64_t &nextLogMs, uint64_t nowMs)
	{
		if (nextLogMs != 0 && nowMs < nextLogMs)
		{
			return false;
		}
		nextLogMs = nowMs + player_inbox_rules::kNodeStuckRelogIntervalMs;
		return true;
	}

	uint64_t InflightTimeoutMs()
	{
		return static_cast<uint64_t>(grpc_call_deadline::Get(common::base::PlayerInboxNodeService).count()) +
			   kInflightSlackMs;
	}

	// 收件箱只用 TipInfoMessage 表达错误(设计 §1.4);0 与 kSuccess 都是成功(serverbase.TipVerdict 同口径)。
	bool IsTipOk(uint32_t tipId)
	{
		return tipId == static_cast<uint32_t>(kCommon_errorOK) || tipId == static_cast<uint32_t>(kSuccess);
	}

	uint64_t GuidOf(entt::entity player)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return guid != nullptr ? guid->value : 0;
	}

	uint64_t OwnerEpochOf(entt::entity player)
	{
		const auto *ownerEpoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
		return ownerEpoch != nullptr ? ownerEpoch->epoch : 0;
	}

	// 异步回调里按 player_id 重新找实体:实体可能已销毁,槽位也可能已被别的玩家复用(Guid 不同)。
	entt::entity FindSamePlayer(uint64_t playerId)
	{
		if (playerId == 0)
		{
			return entt::null;
		}
		const entt::entity player = tlsEcs.GetPlayer(playerId);
		if (player == entt::null || !tlsEcs.actorRegistry.valid(player) || GuidOf(player) != playerId)
		{
			return entt::null;
		}
		return player;
	}

	PlayerInboxDomainState *FindDomainState(PlayerInboxRuntimeComp &runtime, uint32_t domain)
	{
		for (auto &state : runtime.domains)
		{
			if (state.domain == domain)
			{
				return &state;
			}
		}
		return nullptr;
	}

	// 回包 / PING 回调用:按 player_id 找回实体与该域状态,任何一步找不到都返回 nullptr(调用方丢弃)。
	PlayerInboxDomainState *FindLiveState(uint64_t playerId, uint32_t domain, entt::entity &player)
	{
		player = FindSamePlayer(playerId);
		if (player == entt::null)
		{
			return nullptr;
		}
		auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
		return runtime != nullptr ? FindDomainState(*runtime, domain) : nullptr;
	}

	// Tick 收集到的待办只记实体句柄;处理前按句柄重新取组件(收集与处理之间没有其他回调插入,句柄不会过期,
	// 但仍走 valid + try_get,不赌这一点)。
	PlayerInboxDomainState *FindStateByEntity(entt::entity player, uint32_t domain)
	{
		if (!tlsEcs.actorRegistry.valid(player))
		{
			return nullptr;
		}
		auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
		return runtime != nullptr ? FindDomainState(*runtime, domain) : nullptr;
	}

	// 按需状态组件的**唯一**延迟创建入口(docs/design/ecs-component-access-rules.md 规则 2、3):
	// 只由 OnPlayerLanded 在判定为新落地之后调用。其余路径一律 try_get,拿不到就丢弃。
	PlayerInboxRuntimeComp &StoreInboxRuntimeForWrite(entt::entity player)
	{
		// 此处延迟创建:实体第一次在本节点落地时才挂运行时组件("没有组件"本身就是新落地判据)。
		return tlsEcs.actorRegistry.get_or_emplace<PlayerInboxRuntimeComp>(player);
	}

	PlayerInboxDomainState &DomainStateForWrite(PlayerInboxRuntimeComp &runtime, uint32_t domain)
	{
		if (auto *existing = FindDomainState(runtime, domain); existing != nullptr)
		{
			return *existing;
		}
		auto &added = runtime.domains.emplace_back();
		added.domain = domain;
		return added;
	}

	const char *OutcomeVerb(uint32_t outcome)
	{
		switch (outcome)
		{
		case player_inbox::PLAYER_INBOX_OUTCOME_APPLIED:
			return "applied";
		case player_inbox::PLAYER_INBOX_OUTCOME_REJECTED:
			return "rejected";
		case player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED:
			return "skipped";
		case player_inbox::PLAYER_INBOX_OUTCOME_UNKNOWN:
			return "unknown";
		default:
			return "?";
		}
	}

	// ── 退避与停用 ──────────────────────────────────────────────────────────

	// 不会自愈的延后原因:不经人工处置,这名玩家的处置会一直不执行,而表面上与离线无异(评审 L13)。
	// 目前只有账本损坏 —— 损坏标签只在加载时挂、此后不摘,退避多少次都还是它。
	bool IsStuckDeferReason(PlayerInboxDeferReason reason)
	{
		return reason == PlayerInboxDeferReason::kLedgerInvalid;
	}

	// 记下延后原因并在 delayMs 之后续做。同一原因连续出现只在第一次打 INFO,免得收件箱不可用时
	// 每个在线玩家每分钟各刷一行(DEBUG 仍逐次记)。
	// 不会自愈的原因另打 WARN:首次一次,之后按 failStreak 每 kStuckWarnEveryFailures 次一次(封顶后最多约 8 分钟),
	// 节拍落在 scene 日志告警的 [10m] 窗口之内,玩家在线期间告警一直亮着、不会反复自愈(推导见
	// player_inbox_rules::kSceneLogAlertWindowMs,评审 L13、F3)。只影响日志,不改 failStreak 与退避。
	void ScheduleRetry(PlayerInboxDomainState &state, uint64_t playerId, PlayerInboxDeferReason reason,
					   uint64_t delayMs, uint64_t nowMs)
	{
		state.nextCheckMs = nowMs + delayMs;
		const bool reasonChanged = state.deferReason != reason;
		if (reasonChanged)
		{
			LOG_INFO << "[PlayerInbox] metric=inbox_deferred reason=" << PlayerInboxDeferReasonName(reason)
					 << " player_id=" << playerId << " domain=" << state.domain
					 << " phase=" << PlayerInboxPhaseName(state.phase) << " retry_in_ms=" << delayMs;
		}
		else
		{
			LOG_DEBUG << "[PlayerInbox] deferred again reason=" << PlayerInboxDeferReasonName(reason)
					  << " player_id=" << playerId << " domain=" << state.domain << " streak=" << state.failStreak
					  << " retry_in_ms=" << delayMs;
		}
		if (IsStuckDeferReason(reason) && (reasonChanged || player_inbox_rules::IsStuckWarnDue(state.failStreak)))
		{
			LOG_WARN << "[PlayerInbox] metric=inbox_ledger_invalid source=apply_gate player_id=" << playerId
					 << " domain=" << state.domain << " streak=" << state.failStreak
					 << "; inbox consumption stays deferred until the ledger is repaired (does not self-heal)";
		}
		state.deferReason = reason;
	}

	// 按 1s → 60s 加抖动退避(设计 §7.5)。消费关闭不安排重试:配置在进程内不会翻转。
	void Defer(PlayerInboxDomainState &state, uint64_t playerId, PlayerInboxDeferReason reason, uint64_t nowMs)
	{
		if (reason == PlayerInboxDeferReason::kDisabled)
		{
			state.nextCheckMs = 0;
			state.deferReason = reason;
			return;
		}
		if (state.failStreak < std::numeric_limits<uint32_t>::max())
		{
			++state.failStreak;
		}
		ScheduleRetry(state, playerId, reason, player_inbox_rules::BackoffMs(state.failStreak, JitterPermille()), nowMs);
	}

	// 停用这名玩家的这个域:本实体不再 Fetch、不再应用,直到实体下次在本节点新建(设计 §7.6、§7.7)。
	// ackQueue 与阶段由调用方决定(有的路径还要确认已定的前缀,有的路径什么都不确认)。
	void Halt(PlayerInboxDomainState &state, uint64_t playerId, PlayerInboxHaltReason reason, const std::string &detail)
	{
		state.halted = true;
		state.haltReason = reason;
		state.pendingItems.clear();
		state.nextCheckMs = 0;
		state.recheckRequested = false;
		state.hasMore = false;
		LOG_ERROR << "[PlayerInbox] metric=inbox_halted reason=" << PlayerInboxHaltReasonName(reason)
				  << " player_id=" << playerId << " domain=" << state.domain << " detail=" << detail
				  << "; this domain stays disabled for the player until the entity is recreated on this node";
	}

	// 一轮结束:回到 Idle。has_more 立即拉下一批;期间来过新触发则去抖后再拉一次。
	void FinishRound(PlayerInboxDomainState &state, uint64_t nowMs)
	{
		state.phase = PlayerInboxPhase::kIdle;
		state.correlationId = 0;
		state.inflightSinceMs = 0;
		state.durableMismatchReported = false;
		if (IsBattleDomain(state)) state.pendingItems.clear();
		if (state.halted)
		{
			state.nextCheckMs = 0;
		}
		else if (state.hasMore)
		{
			state.nextCheckMs = nowMs;
		}
		else if (state.recheckRequested)
		{
			state.nextCheckMs = std::max(nowMs, state.lastFetchMs + player_inbox_rules::kMinCheckGapMs);
		}
		else
		{
			state.nextCheckMs = 0;
		}
		state.hasMore = false;
		state.recheckRequested = false;
	}

	// ── 前置闸(设计 §7.5)──────────────────────────────────────────────────

	// 返回 kNone = 放行。拉取前判一次、应用前再判一次;确认不经过它。
	PlayerInboxDeferReason CheckApplyGate(entt::entity player, uint32_t domain)
	{
		if (!ConsumerEnabled())
		{
			return PlayerInboxDeferReason::kDisabled;
		}
		switch (player_mutability::ClassifyPlayerMutability(player))
		{
		case player_mutability::PlayerMutability::kMutable:
			break;
		case player_mutability::PlayerMutability::kFrozen:
			return PlayerInboxDeferReason::kFrozen;
		case player_mutability::PlayerMutability::kTravelHandoff:
			return PlayerInboxDeferReason::kHandoff;
		case player_mutability::PlayerMutability::kExiting:
			return PlayerInboxDeferReason::kExiting;
		case player_mutability::PlayerMutability::kOwnerEpochUnknown:
		case player_mutability::PlayerMutability::kCount:
		default:
			return PlayerInboxDeferReason::kEpochZero;
		}
		// 持久账本只由加载创建;缺了等同于账本无效(fail-closed,绝不补建一本空账本:空账本会把已执行过的
		// 欠款判成"没见过"再挂一次)。
		if (tlsEcs.actorRegistry.any_of<PlayerInboxLedgerInvalidComp>(player) ||
			!tlsEcs.actorRegistry.all_of<PlayerInboxLedgerComp>(player))
		{
			return PlayerInboxDeferReason::kLedgerInvalid;
		}
		// 照 player_rollback_handler 对 GM 写操作的战斗闸:战斗快照已经发出去,此刻改货币状态会与结算打架。
		if (player_inbox_registry::DomainRule(domain).requiresOutOfBattle && PlayerBattleSystem::IsInBattle(player))
		{
			return PlayerInboxDeferReason::kInBattle;
		}
		return PlayerInboxDeferReason::kNone;
	}

	// ── 落盘与确认(设计 §7.7)──────────────────────────────────────────────

	void OnPersistProbe(uint64_t playerId, uint32_t domain);

	// 请求一次存盘(500ms 节流),压了写就在同一条连接上紧跟一条 PING 走快路径。
	void RequestPersist(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		if (state.lastPersistRequestMs != 0 && nowMs - state.lastPersistRequestMs < player_inbox_rules::kPersistThrottleMs)
		{
			return;
		}
		// 不可改的实体不压存盘:冻结 / 交接在途时本节点无权再写盘,退出在途时退出存盘已经在飞
		// (它 marshal 的那一份本来就含账本)。Redis快照只是候选，确认仍须核同版本双SQL ACK。
		if (!player_mutability::IsPlayerMutable(player))
		{
			return;
		}
		state.lastPersistRequestMs = nowMs;
		if (!gPersistFn(player))
		{
			// false 可能是"盘上已是这一份",也可能是被拒存;一律交给 durable 判定按快照字节裁决。
			return;
		}
		// 快路径(照 player_battle.cpp 的 RequestSettlementPersist):玩家存盘客户端与 tlsRedis.GetZoneRedis()
		// 是同一个 hiredis 连接,回复按发送顺序处理;存盘成功回调同步更新 PlayerLastPersistedSnapshotComp。
		// 所以 PING 回来时,刚才那笔存盘若已直接发出,快照就已是新的。没赶上的由 tick 每秒复查兜住。
		auto &redis = tlsRedis.GetZoneRedis();
		if (!redis || !redis->connected())
		{
			return;
		}
		const uint32_t domain = state.domain;
		redis->command([playerId, domain](hiredis::Hiredis *, redisReply *) { OnPersistProbe(playerId, domain); },
					   "PING");
	}

	// 本批每一条结局是否都已出现在最后一次落盘快照里(规则见 player_inbox_rules::ClassifyDurability)。
	bool DecisionsInPersistedSnapshot(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state)
	{
		const auto *snapshotComp = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		if (snapshotComp == nullptr || !snapshotComp->HasSnapshot())
		{
			return false;
		}
		const auto &persisted = snapshotComp->snapshot->player_database_data().inbox_ledger();
		for (const auto &decision : state.ackQueue)
		{
			const auto durability = player_inbox_rules::ClassifyDurability(persisted, state.domain, decision);
			if (durability == Durability::kDurable)
			{
				continue;
			}
			if (durability != Durability::kNotYet && !state.durableMismatchReported)
			{
				// 盘上与内存不一致:结局固定被破坏,只可能是缺陷。不敢报 durable、不确认,继续等并告警
				// (口径同资产通道 IsAssetOpDurable)。每批只报一次。
				state.durableMismatchReported = true;
				LOG_ERROR << "[PlayerInbox] Persisted ledger disagrees with in-memory decision; not acknowledging player_id="
						  << playerId << " domain=" << state.domain << " seq=" << decision.seq()
						  << " outcome=" << OutcomeVerb(decision.outcome())
						  << " verdict=" << player_inbox_rules::DurabilityName(durability);
			}
			return false;
		}
		return true;
	}

	void SendAck(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		const uint64_t epoch = OwnerEpochOf(player);
		if (epoch == 0)
		{
			// 拉取前已过 epoch 闸,epoch 只增不减:走到这里只可能是组件被摘(缺陷)。发 0 会被判 EPOCH_REQUIRED
			// 并停用,不如保留 ackQueue 退避再看;实体若随后销毁,下次落地从账本补确认。
			if (state.ackFailStreak < std::numeric_limits<uint32_t>::max())
			{
				++state.ackFailStreak;
			}
			state.nextAckMs = nowMs + player_inbox_rules::BackoffMs(state.ackFailStreak, JitterPermille());
			LOG_ERROR << "[PlayerInbox] Ack postponed: owner_epoch missing on a player that already fetched player_id="
					  << playerId << " domain=" << state.domain;
			return;
		}

		player_inbox::AckPlayerInboxRequest request;
		request.set_player_id(playerId);
		request.set_domain(static_cast<player_inbox::PlayerInboxDomain>(state.domain));
		request.set_owner_epoch(epoch);
		const uint64_t correlationId = NextCorrelationId();
		request.set_correlation_id(correlationId);
		// 只用于对端日志(与 Fetch 填的是同一个本节点 id):服务端围栏 / Ack 异常的 ERROR 据此定位发送方(评审 L10)。
		request.set_scene_node_id(GetNodeInfo().node_id());
		for (const auto &decision : state.ackQueue)
		{
			auto *report = request.add_decisions();
			report->set_seq(decision.seq());
			report->set_outcome(static_cast<player_inbox::PlayerInboxOutcome>(decision.outcome()));
			report->set_reason(decision.reason());
			report->set_request_id(decision.request_id()); // 服务端再核对一次(IB-9)
		}

		if (!gSendAckFn(request))
		{
			if (state.ackFailStreak < std::numeric_limits<uint32_t>::max())
			{
				++state.ackFailStreak;
			}
			state.nextAckMs = nowMs + player_inbox_rules::BackoffMs(state.ackFailStreak, JitterPermille());
			LOG_DEBUG << "[PlayerInbox] Ack not sent: no READY PlayerInboxNodeService node player_id=" << playerId
					  << " domain=" << state.domain << " streak=" << state.ackFailStreak;
			return;
		}
		state.phase = PlayerInboxPhase::kAcking;
		state.correlationId = correlationId;
		state.inflightSinceMs = nowMs;
	}

	// 只读确认绑定原实体、域、轮次与完整快照。未知或过期回包只保留待确认条目，不推断SQL成功。
	void RequestSqlConfirmedAck(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		if (state.correlationId != 0 && nowMs - state.inflightSinceMs < InflightTimeoutMs()) return;
		const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		if (snapshot == nullptr || !snapshot->HasSnapshot()) return;
		const auto domain = state.domain;
		const auto probeId = NextCorrelationId();
		const auto battleItems = IsBattleDomain(state) ? state.pendingItems : std::vector<player_inbox::PlayerInboxItem>{};
		state.correlationId = probeId;
		state.inflightSinceMs = nowMs;
		const bool sent = RoleAdmissionSystem::ConfirmSnapshotSqlCommit(player, *snapshot->snapshot,
			[player = player, playerId, domain, probeId, battleItems](bool committed) {
				entt::entity live = entt::null;
				auto *current = FindLiveState(playerId, domain, live);
				if (current == nullptr || live != player || current->correlationId != probeId) return;
				current->correlationId = 0;
				current->inflightSinceMs = 0;
				if (current->phase != PlayerInboxPhase::kAwaitDurable || current->ackQueue.empty()) return;
				if (!committed || !DecisionsInPersistedSnapshot(player, playerId, *current)) return;
				if (IsBattleDomain(*current)) {
					// 原Fetch载荷随本轮保留到双ACK；不从当前角色重新构造终局或补身份。
					for (const auto& decision : current->ackQueue) {
						const auto item = std::find_if(battleItems.begin(), battleItems.end(), [&decision](const auto& row) {
							return row.seq() == decision.seq() && row.request_id() == decision.request_id()
								&& static_cast<uint32_t>(row.msg_type()) == decision.msg_type();
						});
						if (decision.outcome() != player_inbox::PLAYER_INBOX_OUTCOME_APPLIED || item == battleItems.end()
							|| !PlayerBattleSystem::CompleteInboxSettlement(player, item->payload())) return;
					}
					// 解冻会唤醒其他域；重新取当前实体/轮次，不能继续持跨回调可失效的引用。
					current = FindLiveState(playerId, domain, live);
					if (!current || live != player || current->phase != PlayerInboxPhase::kAwaitDurable) return;
				}
				SendAck(player, playerId, *current, gNowMsFn());
			});
		// false合同不回调；不要在同步完成的分支继续持有可能已转阶段的state引用。
		if (!sent) { state.correlationId = 0; state.inflightSinceMs = 0; }
	}

	// kAwaitDurable的推进：Redis快照含结局后，再等同E/q的双SQL ACK；不复用交易排空的可变floor缓存。
	void TryAck(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		if (state.phase != PlayerInboxPhase::kAwaitDurable)
		{
			return;
		}
		if (state.ackQueue.empty())
		{
			FinishRound(state, nowMs);
			return;
		}
		const auto *candidate = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		const bool needsCurrentCommit = candidate == nullptr || candidate->committed_owner_epoch != OwnerEpochOf(player)
			|| candidate->committed_save_seq == 0;
		if (needsCurrentCommit || !DecisionsInPersistedSnapshot(player, playerId, state))
		{
			if (state.lastPersistRequestMs == 0 || nowMs - state.lastPersistRequestMs >= player_inbox_rules::kResaveIntervalMs)
			{
				RequestPersist(player, playerId, state, nowMs);
			}
			return;
		}
		if (nowMs < state.nextAckMs)
		{
			return;
		}
		RequestSqlConfirmedAck(player, playerId, state, nowMs);
	}

	// 只切进 kAwaitDurable、不当场推进(调用方 ackQueue 非空)。之后由 PING 回调或 tick 的 TryAck 推进。
	void EnterAwaitDurable(PlayerInboxDomainState &state, uint64_t nowMs)
	{
		state.phase = PlayerInboxPhase::kAwaitDurable;
		state.correlationId = 0;
		state.inflightSinceMs = 0;
		state.nextAckMs = nowMs;
		state.ackFailStreak = 0;
		state.durableMismatchReported = false;
	}

	// 本批条目处理完(或在某一条停下)之后:有新记账就请求存盘;有待确认的进 kAwaitDurable,否则本轮结束。
	void AfterBatchDecided(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, bool recorded,
						   uint64_t nowMs)
	{
		if (recorded)
		{
			RequestPersist(player, playerId, state, nowMs);
		}
		if (state.ackQueue.empty())
		{
			FinishRound(state, nowMs);
			return;
		}
		EnterAwaitDurable(state, nowMs);
		TryAck(player, playerId, state, nowMs);
	}

	// 退出期即将丢掉本地缓存里**已投递、还没定结局**的条目时打一行可检索的 INFO(评审 M4 的缺口)。只记日志,不改状态。
	// 这些行在服务端 delivery_count ≥ 1、仍是 PENDING,场景侧没有结局可确认,离开时的补发 Ack(OnPlayerLeaving)
	// 管不到它们:退出被取消(重连)就照常重拉;否则只能等玩家下次落地重拉,或由 Go 侧的 24h 陈旧窗口
	// (loops.go 的 deliveredStaleAfter)把它们移出"已投递未确认"告警。值班据此与"真正卡住"区分开。
	// 必须在 DropPendingForRefetch 之前调(之后 pendingItems 已清空);pendingItems 按 seq 升序。
	void LogDeliveredUndecided(uint64_t playerId, const PlayerInboxDomainState &state, const char *source)
	{
		if (state.pendingItems.empty())
		{
			return;
		}
		LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=delivered_undecided source=" << source
				 << " player_id=" << playerId << " domain=" << state.domain << " items=" << state.pendingItems.size()
				 << " first_seq=" << state.pendingItems.front().seq() << " last_seq=" << state.pendingItems.back().seq()
				 << "; delivered entries without a decision are dropped and stay delivered-but-unacknowledged"
				 << " until they are fetched again";
	}

	// 丢掉本地缓存的条目,改为下次重新拉取(评审 L1;落地钩子里的归属变化同理,评审 L2)。
	// 已记账的前缀(只可能来自执行器 DEFER 之后的续做)不能跟着丢,也不能留到下一批:那些条目在服务端仍是
	// PENDING,下一次 Fetch 会再投一遍,按账本回放后同一 seq 在 ackQueue 里出现两次,服务端判 InvalidParameter。
	// 所以 ackQueue 非空时先进 kAwaitDurable 把前缀确认掉,FinishRound 再按 recheckRequested 重拉
	// (设计 §7.4「上一批全部确认之前不拉下一批」);重拉仍要先过前置闸,闸不过就在那里按原因退避。
	// 返回 true = ackQueue 非空、已置 recheckRequested,调用方须把本域转入 kAwaitDurable(不要再安排检查);
	// false = 已回到 kIdle,检查时刻由调用方安排。
	bool DropPendingForRefetch(PlayerInboxDomainState &state)
	{
		if (!IsBattleDomain(state) || state.ackQueue.empty()) state.pendingItems.clear();
		state.hasMore = false; // 丢掉的那批的 has_more 已不可信,重拉时由新回包重新决定
		if (state.ackQueue.empty())
		{
			state.phase = PlayerInboxPhase::kIdle;
			return false;
		}
		state.recheckRequested = true;
		return true;
	}

	// 水位已过这一条、原结局已不在环里时报的 UNKNOWN 结局(设计 §7.7)。ApplyPending 与退出期入队共用,一条口径。
	PlayerInboxDecision UnknownDecisionFor(const player_inbox::PlayerInboxItem &item)
	{
		PlayerInboxDecision unknown;
		unknown.set_seq(item.seq());
		unknown.set_outcome(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_UNKNOWN));
		unknown.set_reason(0);
		unknown.set_request_id(item.request_id());
		unknown.set_msg_type(static_cast<uint32_t>(item.msg_type()));
		return unknown;
	}

	// 退出期前置闸不过时,先把批首「账本里已有结局」的条目(回放 / UNKNOWN)放进确认队列,只丢掉从第一条需要执行的
	// 条目起的剩余部分(评审 LC-2)。这类条目只报告账本里的事实、不执行任何效果,确认也不受前置闸限制(头文件契约);
	// 跟着新条目一起丢掉的话,离开时的补发确认管不到它们,每次短会话都会再留一行已投递未确认。
	// 只在退出期做:其他闸(战斗、冻结、交接)下丢掉的条目之后还会被重拉、照常回放。
	// 账本缺失或已判坏时不分类(分类依据不可信),整批照旧丢弃。批内 seq 升序,遇到第一条需要执行的条目就停,
	// 所以 ackQueue 仍严格升序;DEFER 续做的批首必是新条目,这里什么都不会入队。
	void QueueDecidedPrefixOnExit(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state)
	{
		const auto &registry = tlsEcs.actorRegistry;
		if (registry.any_of<PlayerInboxLedgerInvalidComp>(player))
		{
			return;
		}
		const auto *ledger = registry.try_get<PlayerInboxLedgerComp>(player);
		if (ledger == nullptr)
		{
			return;
		}
		std::size_t queued = 0;
		for (; queued < state.pendingItems.size(); ++queued)
		{
			const auto &item = state.pendingItems[queued];
			PlayerInboxDecision replayed;
			const auto deliveryClass = player_inbox_rules::ClassifyDelivery(
				player_inbox_rules::FindCursor(*ledger, state.domain), item.seq(), item.request_id(),
				static_cast<uint32_t>(item.msg_type()), &replayed);
			const auto action = player_inbox_rules::PlanItem(deliveryClass, static_cast<uint32_t>(item.directive()));
			if (IsBattleDomain(state) && (item.msg_type() != player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT ||
				item.directive() != player_inbox::PLAYER_INBOX_DIRECTIVE_NONE ||
				action != ItemAction::kReplay || replayed.outcome() != player_inbox::PLAYER_INBOX_OUTCOME_APPLIED)) break;
			if (action == ItemAction::kReplay)
			{
				state.ackQueue.push_back(replayed);
				continue;
			}
			if (action == ItemAction::kAckUnknown)
			{
				state.ackQueue.push_back(UnknownDecisionFor(item));
				continue;
			}
			break;
		}
		if (queued == 0)
		{
			return;
		}
		LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=decided_prefix_queued source=apply_gate_exiting"
				 << " player_id=" << playerId << " domain=" << state.domain << " items=" << queued
				 << " first_seq=" << state.pendingItems.front().seq()
				 << " last_seq=" << state.pendingItems[queued - 1].seq()
				 << "; entries already decided in the ledger are acknowledged instead of being dropped";
		if (!IsBattleDomain(state)) state.pendingItems.erase(state.pendingItems.begin(),
								 state.pendingItems.begin() + static_cast<std::ptrdiff_t>(queued));
	}

	// ── 应用(设计 §7.6 第 3 步)────────────────────────────────────────────

	// 按 seq 升序逐条处理 pendingItems。在同一个 loop 帧里写效果、推进账本、压入最近结局环;
	// 在某一条停下时,已定的前缀照常进入确认流程(MISMATCH 除外:那时什么都不确认)。
	void ApplyPending(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		if (state.halted)
		{
			state.pendingItems.clear();
			AfterBatchDecided(player, playerId, state, false, nowMs);
			return;
		}
		if (const auto reason = CheckApplyGate(player, state.domain); reason != PlayerInboxDeferReason::kNone)
		{
			// 闸不过就丢掉本地副本、下次重新拉(评审 L1),不在本地等闸再续做:等闸的这段时间里(冻结、交接、战斗都
			// 可能很久)服务端可能给条目挂上 VOID,按旧副本续做会把已作废的处置照样执行并报 APPLIED。
			// 代价只是服务端 delivery_count 多 +1。只有执行器 DEFER(闸已过、条目已开始处理)才保留本地续做。
			// 退出期丢掉的这批多半再也拉不回来(玩家正在下线):账本里已有结局的前缀先入确认队列(评审 LC-2),
			// 其余留一行对账线索(评审 M4 的缺口)。按「是否挂着退出标记」判断,不按闸最终返回的原因:
			// 冻结 / 交接与退出同时存在时闸先报 kFrozen / kHandoff,但玩家同样正在下线(评审 LC-1)。
			if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
			{
				QueueDecidedPrefixOnExit(player, playerId, state);
				LogDeliveredUndecided(playerId, state, "apply_gate_exiting");
			}
			if (DropPendingForRefetch(state))
			{
				LOG_INFO << "[PlayerInbox] Apply gate closed (" << PlayerInboxDeferReasonName(reason)
						 << "); acknowledging the decided prefix before refetching player_id=" << playerId
						 << " domain=" << state.domain << " decisions=" << state.ackQueue.size();
				AfterBatchDecided(player, playerId, state, false, nowMs);
				return;
			}
			Defer(state, playerId, reason, nowMs);
			return;
		}
		auto &ledger = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player); // 闸已确认组件在

		bool recorded = false;
		for (std::size_t consumed = 0; consumed < state.pendingItems.size(); ++consumed)
		{
			const auto &item = state.pendingItems[consumed];
			const uint64_t seq = item.seq();
			const uint64_t requestId = item.request_id();
			const uint32_t msgType = static_cast<uint32_t>(item.msg_type());
			const auto directive = item.directive();

			// 先按账本分类,只有新条目才看指令,执行器最后才查(顺序的理由见 player_inbox_rules::PlanItem,评审 M1)。
			PlayerInboxDecision replayed;
			const auto deliveryClass = player_inbox_rules::ClassifyDelivery(
				player_inbox_rules::FindCursor(ledger, state.domain), seq, requestId, msgType, &replayed);
			const auto action = player_inbox_rules::PlanItem(deliveryClass, static_cast<uint32_t>(directive));
			if (IsBattleDomain(state) && (msgType != player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT ||
				directive != player_inbox::PLAYER_INBOX_DIRECTIVE_NONE || action == ItemAction::kAckUnknown ||
				(action == ItemAction::kReplay && replayed.outcome() != player_inbox::PLAYER_INBOX_OUTCOME_APPLIED))) {
				Halt(state, playerId, PlayerInboxHaltReason::kVersionSkew, "battle outcome requires original APPLIED decision");
				AfterBatchDecided(player, playerId, state, recorded, nowMs);
				return;
			}

			// 1. 重复投递:按 (seq, request_id, msg_type) 核对后回放(IB-9)。只报告账本里已有的事实,
			//    不需要执行器 —— 本二进制即使已不认识这个类型,回放原结局也是对的。
			if (action == ItemAction::kReplay)
			{
				state.ackQueue.push_back(replayed);
				continue;
			}
			if (action == ItemAction::kAckUnknown)
			{
				// 水位已过这一条、原结局已不在环里:只可能来自之前会话已落盘的水位,报 UNKNOWN(设计 §7.7)。
				state.ackQueue.push_back(UnknownDecisionFor(item));
				LOG_WARN << "[PlayerInbox] Redelivered entry is below the ledger watermark but no longer in the recent ring;"
						 << " acknowledging as UNKNOWN player_id=" << playerId << " domain=" << state.domain
						 << " seq=" << seq << " request_id=" << requestId;
				continue;
			}
			if (action == ItemAction::kHaltMismatch)
			{
				// ledger_* 是账本环里同一 seq 的那一条(ClassifyDelivery 在 kMismatch 时也拷出来,评审 F4):值班据此
				// 一眼区分"同 seq 被另一个请求占用"(可走 FORCE_MISMATCH)与账本本身的问题。不再打 class=:走到这里恒为 mismatch。
				LOG_ERROR << "[PlayerInbox] metric=inbox_replay_mismatch player_id=" << playerId << " domain=" << state.domain
						  << " seq=" << seq << " delivered_request_id=" << requestId << " delivered_msg_type=" << msgType
						  << " ledger_request_id=" << replayed.request_id() << " ledger_msg_type=" << replayed.msg_type();
				// 什么都不确认(设计 §7.6):本批已记账的前缀照常随存盘落地(效果与账本同一次落盘),但**不会**被之后的
				// 会话自动补确认 —— 这条 MISMATCH 行还是 PENDING,每次落地都会排在那些前缀之后再投一遍、再次停用。
				// 解卡靠服务端 VoidItem 的强制模式(FORCE_MISMATCH):服务端读该玩家已落盘的账本,自证环里同 seq 是另一个
				// request_id 之后把这一行终结为 VOIDED(评审 M1);之后的落地才会按账本回放、确认前缀。
				state.ackQueue.clear();
				Halt(state, playerId, PlayerInboxHaltReason::kReplayMismatch, "seq=" + std::to_string(seq));
				if (recorded)
				{
					RequestPersist(player, playerId, state, nowMs);
				}
				FinishRound(state, nowMs);
				return;
			}

			// 2. 新条目(seq 在水位之上)。
			PlayerInboxDecision decision;
			decision.set_seq(seq);
			decision.set_request_id(requestId);
			decision.set_msg_type(msgType);
			if (action == ItemAction::kSkipVoided)
			{
				// 作废:不查执行器、不解码载荷(设计 §7.9)。本二进制不认识这个类型也照样跳过 ——
				// 这正是运维用 VoidItem 解开"未知类型停用"的路径(评审 M1)。
				decision.set_outcome(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED));
				decision.set_reason(static_cast<uint32_t>(item.void_reason()));
			}
			else if (action != ItemAction::kExecute)
			{
				// kHaltUnknownDirective:不认识的指令同样是版本错位,停在这一条,不猜。
				LOG_ERROR << "[PlayerInbox] metric=inbox_unknown_type player_id=" << playerId << " domain=" << state.domain
						  << " seq=" << seq << " directive=" << static_cast<int>(directive) << " request_id=" << requestId
						  << " action=" << player_inbox_rules::ItemActionName(action);
				Halt(state, playerId, PlayerInboxHaltReason::kUnknownType,
					 "directive=" + std::to_string(static_cast<int>(directive)));
				AfterBatchDecided(player, playerId, state, recorded, nowMs);
				return;
			}
			else
			{
				// 本二进制不认识的类型,或类型不属于本域:版本错位(新类型先于 scene 升级被启用)。
				// 在这一条停下、不记账、不确认这一条,停用该域 —— 绝不能当成终态的 REJECTED。
				const auto executor = player_inbox_registry::FindExecutor(msgType, state.domain);
				if (executor == nullptr)
				{
					LOG_ERROR << "[PlayerInbox] metric=inbox_unknown_type player_id=" << playerId
							  << " domain=" << state.domain << " seq=" << seq << " msg_type=" << msgType
							  << " request_id=" << requestId;
					Halt(state, playerId, PlayerInboxHaltReason::kUnknownType, "msg_type=" + std::to_string(msgType));
					AfterBatchDecided(player, playerId, state, recorded, nowMs);
					return;
				}
				const PlayerInboxExecResult result = executor(player, item.payload());
				if (result.kind == PlayerInboxExecKind::kApplied)
				{
					decision.set_outcome(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED));
					decision.set_reason(0);
				}
				else if (result.kind == PlayerInboxExecKind::kRejected)
				{
					decision.set_outcome(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_REJECTED));
					decision.set_reason(result.reason);
				}
				else if (result.kind == PlayerInboxExecKind::kVersionSkew)
				{
					// 版本错位:与未知类型同口径 —— 停在这一条、不记账、不确认这一条,停用该域,已定前缀照常确认。
					// 不走下面 DEFER 的本地续做:这类错位会在会话内消失(例如那笔带新字段的欠款被还清删除),
					// 按本地旧副本续做看不到服务端之后挂的 VOID,会把已作废的处置执行掉并报 APPLIED(评审 L1)。
					// 这一行留在服务端 PENDING,实体重建后重拉(退出完成后再登录,或换节点);退出保留期内重登会复用实体,不重拉:已作废就记 SKIPPED,落到新版本节点就正常执行。
					// 告警走 Halt 打的 metric=inbox_halted reason=version_skew;这一行只补条目上下文。
					LOG_ERROR << "[PlayerInbox] Executor reported version skew player_id=" << playerId
							  << " domain=" << state.domain << " seq=" << seq
							  << " type=" << player_inbox_registry::TypeName(msgType) << " request_id=" << requestId
							  << " reason=" << result.reason;
					Halt(state, playerId, PlayerInboxHaltReason::kVersionSkew, "seq=" + std::to_string(seq));
					AfterBatchDecided(player, playerId, state, recorded, nowMs);
					return;
				}
				else
				{
					// DEFER:严格顺序,停在这一条;已处理的前缀从本地缓存里摘掉,剩下的按退避续做,不重新 Fetch。
					// 已定的前缀留在 ackQueue,等整批做完一起确认。续做时仍先过前置闸:闸不过就丢掉剩下的本地副本,
					// 先确认前缀再重拉(DropPendingForRefetch,评审 L1)。
					LOG_WARN << "[PlayerInbox] Executor deferred player_id=" << playerId << " domain=" << state.domain
							 << " seq=" << seq << " type=" << player_inbox_registry::TypeName(msgType)
							 << " reason=" << result.reason;
					state.pendingItems.erase(state.pendingItems.begin(),
											 state.pendingItems.begin() + static_cast<std::ptrdiff_t>(consumed));
					if (recorded)
					{
						RequestPersist(player, playerId, state, nowMs);
					}
					Defer(state, playerId, PlayerInboxDeferReason::kExecutorDefer, nowMs);
					return;
				}
			}

			// 记账:与效果同一帧,推进水位并压入环(超过 32 条挤掉最旧的)。
			if (!player_inbox_rules::RecordDecision(player_inbox_rules::MutableCursor(ledger, state.domain), decision))
			{
				// 分类刚说"未见",记账却拒绝:前置被破坏,只可能是缺陷。效果已经写了,结局却没记 ——
				// 停用该域、不确认这一条;已定的前缀照常确认。
				LOG_ERROR << "[PlayerInbox] Ledger precondition violated after the effect was applied player_id="
						  << playerId << " domain=" << state.domain << " seq=" << seq
						  << " outcome=" << OutcomeVerb(decision.outcome());
				Halt(state, playerId, PlayerInboxHaltReason::kLedgerWrite, "seq=" + std::to_string(seq));
				AfterBatchDecided(player, playerId, state, recorded, nowMs);
				return;
			}
			recorded = true;
			state.ackQueue.push_back(decision);
			// 可检索的审计行(设计 §7.6):库按备份恢复后,运维对照它与工单人工核对。
			LOG_INFO << "[PlayerInbox] " << OutcomeVerb(decision.outcome())
					 << " type=" << player_inbox_registry::TypeName(msgType) << " request_id=" << requestId
					 << " seq=" << seq << " player_id=" << playerId << " domain=" << state.domain
					 << " reason=" << decision.reason();
		}

		// Battle专域max_pending=1，原payload留至SQL确认后的通知/解冻；不会再次执行效果。
		if (!IsBattleDomain(state)) state.pendingItems.clear();
		state.failStreak = 0;
		state.deferReason = PlayerInboxDeferReason::kNone;
		AfterBatchDecided(player, playerId, state, recorded, nowMs);
	}

	// ── 拉取 ────────────────────────────────────────────────────────────────

	void StartFetch(entt::entity player, uint64_t playerId, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		if (!state.ackQueue.empty())
		{
			// 不变量:回到 kIdle 时 ackQueue 必为空(上一批全部确认之前不拉下一批,设计 §7.4)。走到这里只可能是缺陷。
			// 照发 Fetch 会让重投的条目按账本回放后与队列里的同一 seq 重复,服务端判 InvalidParameter、本端随即停用该域
			// (评审 L2);改为先确认、确认完再拉。确认不受前置闸限制,所以放在闸之前。
			LOG_ERROR << "[PlayerInbox] Fetch requested while decisions are still unacknowledged; acknowledging first"
					  << " player_id=" << playerId << " domain=" << state.domain << " decisions=" << state.ackQueue.size();
			state.recheckRequested = true;
			AfterBatchDecided(player, playerId, state, /*recorded=*/false, nowMs);
			return;
		}
		if (const auto reason = CheckApplyGate(player, state.domain); reason != PlayerInboxDeferReason::kNone)
		{
			Defer(state, playerId, reason, nowMs);
			return;
		}
		const auto &ledger = tlsEcs.actorRegistry.get<PlayerInboxLedgerComp>(player); // 闸已确认组件在
		const auto *cursor = player_inbox_rules::FindCursor(ledger, state.domain);
		const uint64_t epoch = OwnerEpochOf(player); // 闸已确认非 0

		player_inbox::FetchPlayerInboxRequest request;
		request.set_player_id(playerId);
		request.set_domain(static_cast<player_inbox::PlayerInboxDomain>(state.domain));
		request.set_owner_epoch(epoch);
		const uint64_t correlationId = NextCorrelationId();
		request.set_correlation_id(correlationId);
		request.set_max_items(player_inbox_rules::kFetchBatchMax);
		// 账本水位:服务端据此检测全局库被恢复(CursorAhead)并抬号(设计 §7.8)。
		request.set_consumer_decided_through(cursor != nullptr ? cursor->decided_through_seq() : 0);
		request.set_scene_node_id(GetNodeInfo().node_id());

		if (!gSendFetchFn(request))
		{
			Defer(state, playerId, PlayerInboxDeferReason::kNoNode, nowMs);
			return;
		}
		state.phase = PlayerInboxPhase::kFetching;
		state.correlationId = correlationId;
		state.inflightSinceMs = nowMs;
		state.sentEpoch = epoch;
		state.lastFetchMs = nowMs;
		state.recheckRequested = false;
	}

	// 回包形状校验:超过一批上限、seq 为 0 或不严格递增都是协议异常。返回空串 = 合法。
	std::string DescribeMalformedBatch(const player_inbox::FetchPlayerInboxResponse &response)
	{
		if (response.items_size() > static_cast<int>(player_inbox_rules::kFetchBatchMax))
		{
			return "items=" + std::to_string(response.items_size()) + " exceeds batch max " +
				   std::to_string(player_inbox_rules::kFetchBatchMax);
		}
		uint64_t previousSeq = 0;
		for (const auto &item : response.items())
		{
			if (item.seq() == 0)
			{
				return "seq 0 in batch";
			}
			if (item.seq() <= previousSeq)
			{
				return "seq " + std::to_string(item.seq()) + " not strictly ascending after " + std::to_string(previousSeq);
			}
			previousSeq = item.seq();
		}
		return {};
	}

	void OnPersistProbe(uint64_t playerId, uint32_t domain)
	{
		entt::entity player = entt::null;
		auto *state = FindLiveState(playerId, domain, player);
		if (state == nullptr || state->phase != PlayerInboxPhase::kAwaitDurable)
		{
			return;
		}
		TryAck(player, playerId, *state, gNowMsFn());
	}

	// 在途请求超过期限仍无完成通知:按传输失败收场(结果未知)。迟到的回包对不上关联号,会被丢弃。
	void HandleInflightTimeout(entt::entity player, PlayerInboxDomainState &state, uint64_t nowMs)
	{
		const uint64_t playerId = GuidOf(player);
		LOG_WARN << "[PlayerInbox] Request got no completion in time; treating as transport failure player_id=" << playerId
				 << " domain=" << state.domain << " phase=" << PlayerInboxPhaseName(state.phase)
				 << " corr=" << state.correlationId << " inflight_ms=" << (nowMs - state.inflightSinceMs);
		state.correlationId = 0;
		state.inflightSinceMs = 0;
		if (state.phase == PlayerInboxPhase::kFetching)
		{
			state.phase = PlayerInboxPhase::kIdle;
			Defer(state, playerId, PlayerInboxDeferReason::kTransport, nowMs);
			return;
		}
		// kAcking:保留 ackQueue,退避重发。
		state.phase = PlayerInboxPhase::kAwaitDurable;
		if (state.ackFailStreak < std::numeric_limits<uint32_t>::max())
		{
			++state.ackFailStreak;
		}
		state.nextAckMs = nowMs + player_inbox_rules::BackoffMs(state.ackFailStreak, JitterPermille());
	}

	struct DueWork
	{
		entt::entity player{entt::null};
		uint32_t domain{0};
	};
} // namespace

// ── 启动 ────────────────────────────────────────────────────────────────────

void PlayerInboxSystem::Initialize()
{
	tlsRegistryBrokenDetail = player_inbox_registry::SelfCheck();
	tlsRegistryBroken = !tlsRegistryBrokenDetail.empty();
	if (tlsRegistryBroken)
	{
		// 不阻断启动(IB-7):收件箱停用只是延迟处置,条目留在服务端 PENDING。之后由 Tick 周期性补打同一行。
		LogRegistryBroken();
		tlsNextRegistryBrokenLogMs = gNowMsFn() + player_inbox_rules::kNodeStuckRelogIntervalMs;
	}
	if (!ConfiguredConsumerEnabled())
	{
		// 开关关着(评审 H1):生效配置这一行升为 WARN、带 metric=,之后由 Tick 按同一节拍补打。
		// 只升级别、不拒启:关着本身是合法状态,上线开消费之前就是关的。
		LogConsumerDisabled();
		tlsNextConsumerDisabledLogMs = gNowMsFn() + player_inbox_rules::kNodeStuckRelogIntervalMs;
		return;
	}
	tlsNextConsumerDisabledLogMs = 0;
	LOG_INFO << "[PlayerInbox] consumer_enabled=" << ConsumerEnabled()
			 << " periodic_fetch_minutes=" << PeriodicFetchMinutes()
			 << " consumer_domains=" << player_inbox_registry::ConsumerDomains().size();
}

// ── 触发入口 ────────────────────────────────────────────────────────────────

void PlayerInboxSystem::OnPlayerLanded(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerId = GuidOf(player);
	if (playerId == 0)
	{
		return;
	}
	// 消费关闭:不建运行时组件、不发 Fetch;账本照常随存档往返(回滚下限,设计 §3.3、§11.7)。
	if (!ConsumerEnabled())
	{
		return;
	}
	const uint64_t epoch = OwnerEpochOf(player);
	const auto *existing = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
	if (!player_inbox_rules::IsNewLanding(existing != nullptr, existing != nullptr ? existing->landedEpoch : 0, epoch))
	{
		// 同节点换图、gate 补发的重复路由:不算落地。
		LOG_DEBUG << "[PlayerInbox] landing skipped (same owner_epoch) player_id=" << playerId << " epoch=" << epoch;
		return;
	}

	auto &runtime = StoreInboxRuntimeForWrite(player);
	runtime.landedEpoch = epoch;
	const uint64_t nowMs = gNowMsFn();
	const uint32_t periodicMinutes = PeriodicFetchMinutes();
	for (const uint32_t domain : player_inbox_registry::ConsumerDomains())
	{
		auto &state = DomainStateForWrite(runtime, domain);
		if (state.halted)
		{
			continue;
		}
		state.nextPeriodicMs =
			periodicMinutes > 0 ? nowMs + player_inbox_rules::PeriodicDelayMs(periodicMinutes, JitterPermille()) : 0;
		switch (state.phase)
		{
		case PlayerInboxPhase::kApplying:
			// 归属变过:本地缓存的条目是旧归属下拉到的,丢掉重拉(它们在服务端仍是 PENDING,不会丢)。
			if (DropPendingForRefetch(state))
			{
				// 执行器 DEFER 之前已记账的前缀还在 ackQueue:先确认它们,FinishRound 再按 recheckRequested 重拉
				// (设计 §7.4)。直接回 kIdle 会让下一次 Fetch 把这些条目再投一遍,回放后同一 seq 在 Ack 里出现两次(评审 L2)。
				// 这里只切状态、不当场发 Ack / 压存盘:落地钩子要保持开销小,下一个 tick(≤ 1 秒)由 TryAck 推进。
				state.failStreak = 0;
				state.deferReason = PlayerInboxDeferReason::kNone;
				EnterAwaitDurable(state, nowMs);
				break;
			}
			[[fallthrough]];
		case PlayerInboxPhase::kIdle:
			// 新落地:无条件拉一次,不去抖(设计 §7.3 第 1 条)。
			state.failStreak = 0;
			state.deferReason = PlayerInboxDeferReason::kNone;
			state.nextCheckMs = nowMs;
			break;
		default:
			// 单飞:在途的那一轮结束后再拉(设计 §7.4)。
			state.recheckRequested = true;
			break;
		}
	}
	LOG_DEBUG << "[PlayerInbox] landing scheduled a check player_id=" << playerId << " epoch=" << epoch;
}

void PlayerInboxSystem::OnBattleFreezeCleared(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
	if (runtime == nullptr)
	{
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	for (auto &state : runtime->domains)
	{
		if (state.halted || state.deferReason != PlayerInboxDeferReason::kInBattle)
		{
			continue;
		}
		if (state.phase == PlayerInboxPhase::kIdle || state.phase == PlayerInboxPhase::kApplying)
		{
			// 立即重查:不合并、不去抖(协调项 C1)。不加这一行,战斗结束后最多要再等 60 秒的退避。
			state.failStreak = 0;
			state.nextCheckMs = nowMs;
		}
	}
}

// 实体销毁时 ackQueue 跟着丢,这些行在服务端就停在"已投递未确认",直到玩家下次落地才补确认(设计 §7.7、§8 #7)。
// 典型场景是 GM 处置之后紧接着封号踢下线:玩家可能很久不再上线,告警被这一行一直顶着(评审 M4)。
// 这里在销毁前尽力补发一次 Ack,只为减少残留,不改变正确性:发不出去、回包丢了、没落盘,都照旧等下次落地。
// 覆盖范围只到"结局已定、已落盘"的行。已投递、还没定结局的行(退出时 Fetch 在途;Fetch 回包在退出期被前置闸丢掉;
// 执行器 DEFER 剩下的本地条目)没有结局可确认,本函数与 ApplyPending 只为它们打 metric=inbox_leave_ack
// result=fetch_inflight / delivered_undecided 供对账;这些行仍只能靠下次落地重拉,或 Go 侧的 24h 陈旧窗口。
void PlayerInboxSystem::OnPlayerLeaving(entt::entity player)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
	if (runtime == nullptr)
	{
		// 没在本节点落地过(或消费关闭):没有待确认的结局。离开路径不创建组件。
		return;
	}
	const uint64_t playerId = GuidOf(player);
	if (playerId == 0)
	{
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	for (auto &state : runtime->domains)
	{
		if (state.phase == PlayerInboxPhase::kFetching)
		{
			// 拉取在途(此时 ackQueue 必为空,StartFetch 的不变量):回包到达时实体已销毁,OnFetchReply 按 player_id 找不到
			// 实体就丢弃。服务端若已把条目标记为投递,这些行停在"已投递未确认",场景侧没有结局可确认(评审 M4 的缺口);
			// 这里不等回包,只留一行对账线索。
			LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=fetch_inflight player_id=" << playerId
					 << " domain=" << state.domain << " corr=" << state.correlationId
					 << "; the reply will be discarded, entries it delivered stay delivered-but-unacknowledged"
					 << " until the player next lands";
			continue;
		}
		if (state.phase == PlayerInboxPhase::kApplying)
		{
			// 执行器 DEFER 剩下、还没定结局的本地条目跟着实体丢弃,不论有没有已定前缀;前缀(若有)下面照常确认。
			LogDeliveredUndecided(playerId, state, "leaving");
		}
		if (state.ackQueue.empty())
		{
			continue;
		}
		if (state.phase == PlayerInboxPhase::kAcking)
		{
			// 这批的 Ack 已在途:服务端照样会处理它,不依赖实体还在;回包到达时按 player_id 找不到实体就丢弃。
			// 万一它随后失败,失败回调找不到实体只打 DEBUG,服务端这批会停在"已投递未确认";留一行带 corr 的对账线索
			// (评审 LC-3),值班可按 corr 对上 scene 日志,与真正卡住的行区分开。
			LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=ack_inflight player_id=" << playerId
					 << " domain=" << state.domain << " corr=" << state.correlationId
					 << " decisions=" << state.ackQueue.size()
					 << "; if this Ack fails after the entity is gone, these entries stay delivered-but-unacknowledged"
					 << " until the player next lands";
			continue;
		}
		if (state.phase == PlayerInboxPhase::kApplying)
		{
			// 执行器 DEFER 留下的已定前缀(ackQueue 非空,DropPendingForRefetch 必返回 true):剩下的本地条目跟着实体丢弃
			// (服务端仍是 PENDING,下次落地重拉),前缀照常确认。与 L1 / L2 同一做法,实体万一没被销毁,状态机也自洽。
			DropPendingForRefetch(state);
			EnterAwaitDurable(state, nowMs);
		}
		else if (state.phase != PlayerInboxPhase::kAwaitDurable)
		{
			// kIdle 带着非空 ackQueue 违反不变量(StartFetch 开头已有守卫并打 ERROR),离开时不再处理;kFetching 已在上面处理。
			continue;
		}
		// 退出只作最佳努力：本地快照与双SQL ACK都确认后才发Ack，实体先销毁则留到下次落地。
		// 没有就什么都不做,也不再压存盘(退出存盘就是最后一次),等下次落地从账本补确认。
		if (!DecisionsInPersistedSnapshot(player, playerId, state))
		{
			LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=not_durable player_id=" << playerId
					 << " domain=" << state.domain << " decisions=" << state.ackQueue.size()
					 << "; the entries stay delivered-but-unacknowledged until the player next lands";
			continue;
		}
		// 离开是最后一次机会,不看 Ack 退避(nextAckMs)。owner_epoch 此刻仍是本节点的当前值,服务端应判 MATCH;
		// 新 owner 若已推进围栏会判 STALE,无害:它加载的存档里已有这些结局,会自己补确认。
		// SendAck 的完成回调只捕获调用身份:实体已销毁时 OnAckReply / OnAckFailed 丢弃;
		// 同一玩家重新落地后的新实体关联号也对不上(节点级单调计数器),这里不登记额外等待状态。
		RequestSqlConfirmedAck(player, playerId, state, nowMs);
		const bool sent = state.phase == PlayerInboxPhase::kAcking;
		LOG_INFO << "[PlayerInbox] metric=inbox_leave_ack result=" << (sent ? "sent" : "sql_pending_or_not_sent")
				 << " player_id=" << playerId << " domain=" << state.domain << " decisions=" << state.ackQueue.size()
				 << " corr=" << state.correlationId << "; best effort, the reply is not awaited";
	}
}

void PlayerInboxSystem::OnNotifyEvent(uint64_t playerId, uint32_t domain)
{
	const entt::entity player = FindSamePlayer(playerId);
	if (player == entt::null)
	{
		// 提醒是"至多一次"的:玩家不在本节点就丢,下次落地会拉(设计 §6.3)。
		LOG_DEBUG << "[PlayerInbox] notify dropped: player not on this node player_id=" << playerId;
		return;
	}
	auto *runtime = tlsEcs.actorRegistry.try_get<PlayerInboxRuntimeComp>(player);
	if (runtime == nullptr)
	{
		LOG_DEBUG << "[PlayerInbox] notify dropped: player has not landed yet (the landing fetch covers it) player_id="
				  << playerId;
		return;
	}
	auto *state = FindDomainState(*runtime, domain);
	if (state == nullptr)
	{
		LOG_WARN << "[PlayerInbox] notify for a domain this binary does not consume dropped player_id=" << playerId
				 << " domain=" << domain;
		return;
	}
	if (state->halted)
	{
		return;
	}
	if (state->phase != PlayerInboxPhase::kIdle)
	{
		state->recheckRequested = true;
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	const uint64_t debounced = std::max(nowMs, state->lastFetchMs + player_inbox_rules::kMinCheckGapMs);
	if (state->nextCheckMs == 0 || state->nextCheckMs > debounced)
	{
		state->nextCheckMs = debounced;
	}
}

void PlayerInboxSystem::Tick()
{
	const uint64_t nowMs = gNowMsFn();
	const uint32_t periodicMinutes = PeriodicFetchMinutes();
	const uint64_t inflightTimeoutMs = InflightTimeoutMs();

	if (tlsRegistryBroken && TakeNodeRelogTurn(tlsNextRegistryBrokenLogMs, nowMs))
	{
		// 节点级停用不会自愈(要换二进制):按固定间隔补打,让 [10m] 窗口的告警在进程存活期间一直亮着(评审 F3)。
		LogRegistryBroken();
	}
	// 开关关着也是节点级、不会自愈的停用(要改配置重启),同一节拍补打(评审 H1)。开关开着时清掉排期:
	// 再关上时下一个 tick 立即打首条。这里只打日志,开关关着时不建运行时组件、不拉取的行为不变。
	if (ConfiguredConsumerEnabled())
	{
		tlsNextConsumerDisabledLogMs = 0;
	}
	else if (TakeNodeRelogTurn(tlsNextConsumerDisabledLogMs, nowMs))
	{
		LogConsumerDisabled();
	}

	// 先只读遍历收集到期项,再逐项处理:处理里会调执行器、请求存盘,不在遍历中做这些。
	// 本函数只用 view / try_get,不创建任何组件(AGENTS §7.5)。
	std::vector<DueWork> timeouts;
	std::vector<DueWork> acks;
	std::vector<DueWork> checks;
	for (auto &&[entity, runtime] : tlsEcs.actorRegistry.view<PlayerInboxRuntimeComp>().each())
	{
		for (auto &state : runtime.domains)
		{
			// 在线有界兜底:计时只记在运行时,不进存档(设计 §7.3 第 3 条)。
			if (periodicMinutes > 0 && !state.halted && state.nextPeriodicMs != 0 && nowMs >= state.nextPeriodicMs)
			{
				state.nextPeriodicMs = nowMs + player_inbox_rules::PeriodicDelayMs(periodicMinutes, JitterPermille());
				if (state.phase == PlayerInboxPhase::kIdle)
				{
					if (state.nextCheckMs == 0)
					{
						state.nextCheckMs = nowMs;
					}
				}
				else
				{
					state.recheckRequested = true;
				}
			}

			switch (state.phase)
			{
			case PlayerInboxPhase::kFetching:
			case PlayerInboxPhase::kAcking:
				if (state.inflightSinceMs != 0 && nowMs - state.inflightSinceMs > inflightTimeoutMs)
				{
					timeouts.push_back(DueWork{entity, state.domain});
				}
				break;
			case PlayerInboxPhase::kAwaitDurable:
				acks.push_back(DueWork{entity, state.domain});
				break;
			case PlayerInboxPhase::kIdle:
			case PlayerInboxPhase::kApplying:
				if (!state.halted && state.nextCheckMs != 0 && nowMs >= state.nextCheckMs)
				{
					checks.push_back(DueWork{entity, state.domain});
				}
				break;
			case PlayerInboxPhase::kCount:
			default:
				break;
			}
		}
	}

	for (const auto &work : timeouts)
	{
		auto *state = FindStateByEntity(work.player, work.domain);
		if (state != nullptr &&
			(state->phase == PlayerInboxPhase::kFetching || state->phase == PlayerInboxPhase::kAcking))
		{
			HandleInflightTimeout(work.player, *state, nowMs);
		}
	}

	// 等落盘与确认:每秒复查一次快照(纯内存比较),每 5 秒补请求一次存盘。不受前置闸与检查预算限制。
	for (const auto &work : acks)
	{
		auto *state = FindStateByEntity(work.player, work.domain);
		if (state != nullptr && state->phase == PlayerInboxPhase::kAwaitDurable)
		{
			TryAck(work.player, GuidOf(work.player), *state, nowMs);
		}
	}

	// 到期的检查:单个 tick 最多发起 kTickCheckBudget 次拉取,其余保持到期、下一 tick 再做(设计 §7.4)。
	uint32_t budget = player_inbox_rules::kTickCheckBudget;
	for (const auto &work : checks)
	{
		auto *state = FindStateByEntity(work.player, work.domain);
		if (state == nullptr || state->halted || state->nextCheckMs == 0 || nowMs < state->nextCheckMs)
		{
			continue;
		}
		const uint64_t playerId = GuidOf(work.player);
		if (playerId == 0)
		{
			continue;
		}
		if (state->phase == PlayerInboxPhase::kIdle)
		{
			if (budget == 0)
			{
				continue;
			}
			--budget;
			state->nextCheckMs = 0;
			StartFetch(work.player, playerId, *state, nowMs);
		}
		else if (state->phase == PlayerInboxPhase::kApplying)
		{
			state->nextCheckMs = 0;
			ApplyPending(work.player, playerId, *state, nowMs);
		}
	}
}

// ── gRPC 回包 ───────────────────────────────────────────────────────────────

void PlayerInboxSystem::OnFetchReply(uint64_t playerId, uint32_t domain, uint64_t correlationId,
								   const player_inbox::FetchPlayerInboxResponse &response)
{
	// 回显逐字段检查,不能因某一项为 0 而绕过其它项的不一致;丢弃后由在途超时收敛。
	if ((response.player_id() != 0 && response.player_id() != playerId) ||
		(response.domain() != 0 && static_cast<uint32_t>(response.domain()) != domain) ||
		(response.correlation_id() != 0 && response.correlation_id() != correlationId))
	{
		LOG_ERROR << "[PlayerInbox] Fetch reply echo mismatch captured_player_id=" << playerId
				  << " captured_domain=" << domain << " captured_corr=" << correlationId
				  << " echoed_player_id=" << response.player_id()
				  << " echoed_domain=" << static_cast<uint32_t>(response.domain())
				  << " echoed_corr=" << response.correlation_id();
		return;
	}
	entt::entity player = entt::null;
	auto *state = FindLiveState(playerId, domain, player);
	// 按调用身份找实体,仍校验当前阶段和关联号,拒绝已超时、被顶替或实体重建前的调用。
	if (state == nullptr || state->phase != PlayerInboxPhase::kFetching || correlationId == 0 ||
		state->correlationId != correlationId)
	{
		LOG_DEBUG << "[PlayerInbox] Fetch reply discarded (entity gone, recreated or request superseded) player_id="
				  << playerId << " domain=" << domain << " corr=" << correlationId;
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	state->phase = PlayerInboxPhase::kIdle;
	state->correlationId = 0;
	state->inflightSinceMs = 0;

	const auto fence = response.fence();
	if (fence == player_inbox::PLAYER_INBOX_FENCE_STALE)
	{
		LOG_ERROR << "[PlayerInbox] metric=inbox_stale_owner op=fetch player_id=" << playerId << " domain=" << domain
				  << " sent_epoch=" << state->sentEpoch;
		ScheduleRetry(*state, playerId, PlayerInboxDeferReason::kStaleOwner, player_inbox_rules::kStaleOwnerBackoffMs,
					  nowMs);
		return;
	}
	if (fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_REQUIRED || fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_AHEAD)
	{
		// 缺陷或伪造(IB-8):停用,不重试。
		Halt(*state, playerId,
			 fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_REQUIRED ? PlayerInboxHaltReason::kEpochRequired
																	   : PlayerInboxHaltReason::kEpochAhead,
			 "op=fetch sent_epoch=" + std::to_string(state->sentEpoch));
		FinishRound(*state, nowMs);
		return;
	}
	if (const uint32_t tip = response.error_message().id(); !IsTipOk(tip))
	{
		// 服务端故障(例如 kPlayerInboxStoreUnavailable):退避,不计协议异常。
		if (state->deferReason != PlayerInboxDeferReason::kServerError)
		{
			LOG_WARN << "[PlayerInbox] Fetch answered with tip=" << tip << " player_id=" << playerId
					 << " domain=" << domain;
		}
		Defer(*state, playerId, PlayerInboxDeferReason::kServerError, nowMs);
		return;
	}
	if (fence == player_inbox::PLAYER_INBOX_FENCE_NO_STREAM || response.items_size() == 0)
	{
		// 本轮结束。空批不信 has_more,免得对着一个空回包反复空转。
		state->failStreak = 0;
		state->deferReason = PlayerInboxDeferReason::kNone;
		state->hasMore = false;
		FinishRound(*state, nowMs);
		return;
	}
	if (fence != player_inbox::PLAYER_INBOX_FENCE_FIRST && fence != player_inbox::PLAYER_INBOX_FENCE_MATCH &&
		fence != player_inbox::PLAYER_INBOX_FENCE_ADVANCE)
	{
		LOG_ERROR << "[PlayerInbox] Fetch reply carries items under unexpected fence=" << static_cast<int>(fence)
				  << " player_id=" << playerId << " domain=" << domain;
		Defer(*state, playerId, PlayerInboxDeferReason::kServerError, nowMs);
		return;
	}
	if (const std::string malformed = DescribeMalformedBatch(response); !malformed.empty())
	{
		Halt(*state, playerId, PlayerInboxHaltReason::kBatchMalformed, malformed);
		FinishRound(*state, nowMs);
		return;
	}

	if (IsBattleDomain(*state) && response.items_size() > 1) {
		Halt(*state, playerId, PlayerInboxHaltReason::kBatchMalformed, "battle domain max_pending=1");
		FinishRound(*state, nowMs);
		return;
	}
	state->hasMore = response.has_more();
	state->pendingItems.assign(response.items().begin(), response.items().end());
	state->phase = PlayerInboxPhase::kApplying;
	// 回包到达、准备应用之前再判一次前置闸(在 ApplyPending 里)。
	ApplyPending(player, playerId, *state, nowMs);
}

void PlayerInboxSystem::OnFetchFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode,
									  const std::string &detail)
{
	entt::entity player = entt::null;
	auto *state = FindLiveState(playerId, domain, player);
	if (state == nullptr || state->phase != PlayerInboxPhase::kFetching || correlationId == 0 ||
		state->correlationId != correlationId)
	{
		LOG_DEBUG << "[PlayerInbox] Fetch failure discarded (entity gone or request superseded) player_id=" << playerId
				  << " code=" << grpcCode;
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	state->phase = PlayerInboxPhase::kIdle;
	state->correlationId = 0;
	state->inflightSinceMs = 0;
	if (grpcCode == kGrpcStatusUnimplemented)
	{
		// 收件箱还是旧版本:长退避,记 INFO,不计故障(设计 §3.3、§7.6)。
		if (state->deferReason != PlayerInboxDeferReason::kUnimplemented)
		{
			LOG_INFO << "[PlayerInbox] Inbox service does not implement FetchItems yet (older version); backing off"
					 << " player_id=" << playerId << " domain=" << domain;
		}
		ScheduleRetry(*state, playerId, PlayerInboxDeferReason::kUnimplemented,
					  player_inbox_rules::kUnimplementedBackoffMs, nowMs);
		return;
	}
	if (state->deferReason != PlayerInboxDeferReason::kTransport)
	{
		LOG_WARN << "[PlayerInbox] Fetch failed (result unknown) player_id=" << playerId << " domain=" << domain
				 << " code=" << grpcCode << " detail=" << detail;
	}
	Defer(*state, playerId, PlayerInboxDeferReason::kTransport, nowMs);
}

void PlayerInboxSystem::OnAckReply(uint64_t playerId, uint32_t domain, uint64_t correlationId,
								 const player_inbox::AckPlayerInboxResponse &response)
{
	if ((response.player_id() != 0 && response.player_id() != playerId) ||
		(response.domain() != 0 && static_cast<uint32_t>(response.domain()) != domain) ||
		(response.correlation_id() != 0 && response.correlation_id() != correlationId))
	{
		LOG_ERROR << "[PlayerInbox] Ack reply echo mismatch captured_player_id=" << playerId
				  << " captured_domain=" << domain << " captured_corr=" << correlationId
				  << " echoed_player_id=" << response.player_id()
				  << " echoed_domain=" << static_cast<uint32_t>(response.domain())
				  << " echoed_corr=" << response.correlation_id();
		return;
	}
	entt::entity player = entt::null;
	auto *state = FindLiveState(playerId, domain, player);
	if (state == nullptr || state->phase != PlayerInboxPhase::kAcking || correlationId == 0 ||
		state->correlationId != correlationId)
	{
		LOG_DEBUG << "[PlayerInbox] Ack reply discarded (entity gone, recreated or request superseded) player_id="
				  << playerId << " domain=" << domain << " corr=" << correlationId;
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	state->phase = PlayerInboxPhase::kAwaitDurable;
	state->correlationId = 0;
	state->inflightSinceMs = 0;

	const auto fence = response.fence();
	if (fence == player_inbox::PLAYER_INBOX_FENCE_STALE)
	{
		// 新 owner 已经推进了围栏。没有关系:它加载的存档里已有这些结局,会自己补确认(设计 §7.7、§7.10)。
		LOG_WARN << "[PlayerInbox] Ack fenced off as STALE; the newer owner re-acknowledges from its persisted ledger"
				 << " player_id=" << playerId << " domain=" << domain << " decisions=" << state->ackQueue.size();
		state->ackQueue.clear();
		FinishRound(*state, nowMs);
		// 已停用的域(停用前确认的是已定前缀)不再安排重试:FinishRound 已把 nextCheckMs 置 0,再排一次只会打出
		// 「60 秒后重试」的 inbox_deferred,把永久停用伪装成暂时延后(评审 SM-1)。
		if (!state->halted)
		{
			ScheduleRetry(*state, playerId, PlayerInboxDeferReason::kStaleOwner, player_inbox_rules::kStaleOwnerBackoffMs,
						  nowMs);
		}
		return;
	}
	if (fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_REQUIRED || fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_AHEAD)
	{
		state->ackQueue.clear();
		Halt(*state, playerId,
			 fence == player_inbox::PLAYER_INBOX_FENCE_EPOCH_REQUIRED ? PlayerInboxHaltReason::kEpochRequired
																	   : PlayerInboxHaltReason::kEpochAhead,
			 "op=ack");
		FinishRound(*state, nowMs);
		return;
	}
	if (const uint32_t tip = response.error_message().id(); !IsTipOk(tip))
	{
		if (tip == static_cast<uint32_t>(kInvalidParameter))
		{
			// 确定性协议错误(评审 L2):服务端判这份报告列表不合法(空、超过上限、seq 为 0 或不严格升序、结局未知、
			// 域未知)。同一份 ackQueue 重发只会得到同一个答复,当故障退避就是无限重发 —— 按协议异常停用、不重试。
			// 这些结局都已落盘(Ack 只在 durable 之后发),实体下次在本节点新建时会按账本回放、重新确认。
			LOG_ERROR << "[PlayerInbox] metric=inbox_ack_anomaly player_id=" << playerId << " domain=" << domain
					  << " detail=tip=" << tip << " decisions=" << state->ackQueue.size();
			state->ackQueue.clear();
			Halt(*state, playerId, PlayerInboxHaltReason::kAckAnomaly, "op=ack tip=" + std::to_string(tip));
			FinishRound(*state, nowMs);
			return;
		}
		// 服务端故障:保留 ackQueue,退避重发(设计 §7.7)。
		if (state->ackFailStreak < std::numeric_limits<uint32_t>::max())
		{
			++state->ackFailStreak;
		}
		state->nextAckMs = nowMs + player_inbox_rules::BackoffMs(state->ackFailStreak, JitterPermille());
		LOG_WARN << "[PlayerInbox] Ack answered with tip=" << tip << " player_id=" << playerId << " domain=" << domain
				 << " streak=" << state->ackFailStreak;
		return;
	}

	// 逐条结果:FINALIZED / ALREADY_SAME 视为成功;其余全是协议异常 —— 记 ERROR、停用、不重试(设计 §7.7)。
	std::string anomaly;
	if (response.results_size() != static_cast<int>(state->ackQueue.size()))
	{
		anomaly = "results=" + std::to_string(response.results_size()) +
				  " decisions=" + std::to_string(state->ackQueue.size());
	}
	else
	{
		for (int i = 0; i < response.results_size(); ++i)
		{
			const auto &result = response.results(i);
			const auto &decision = state->ackQueue[static_cast<std::size_t>(i)];
			if (result.seq() != decision.seq())
			{
				anomaly = "result[" + std::to_string(i) + "] seq=" + std::to_string(result.seq()) +
						  " expected=" + std::to_string(decision.seq());
				break;
			}
			if (result.result() != player_inbox::PLAYER_INBOX_ACK_FINALIZED &&
				result.result() != player_inbox::PLAYER_INBOX_ACK_ALREADY_SAME)
			{
				anomaly = "seq=" + std::to_string(result.seq()) + " result=" +
						  player_inbox::PlayerInboxAckResult_Name(result.result()) +
						  " state=" + player_inbox::PlayerInboxItemState_Name(result.state());
				break;
			}
		}
	}
	if (!anomaly.empty())
	{
		LOG_ERROR << "[PlayerInbox] metric=inbox_ack_anomaly player_id=" << playerId << " domain=" << domain
				  << " detail=" << anomaly;
		state->ackQueue.clear();
		Halt(*state, playerId, PlayerInboxHaltReason::kAckAnomaly, anomaly);
		FinishRound(*state, nowMs);
		return;
	}

	LOG_DEBUG << "[PlayerInbox] Acknowledged " << state->ackQueue.size() << " decision(s) player_id=" << playerId
			  << " domain=" << domain;
	state->ackQueue.clear();
	state->ackFailStreak = 0;
	FinishRound(*state, nowMs);
}

void PlayerInboxSystem::OnAckFailed(uint64_t playerId, uint32_t domain, uint64_t correlationId, uint32_t grpcCode,
									const std::string &detail)
{
	entt::entity player = entt::null;
	auto *state = FindLiveState(playerId, domain, player);
	if (state == nullptr || state->phase != PlayerInboxPhase::kAcking || correlationId == 0 ||
		state->correlationId != correlationId)
	{
		LOG_DEBUG << "[PlayerInbox] Ack failure discarded (entity gone or request superseded) player_id=" << playerId
				  << " code=" << grpcCode;
		return;
	}
	const uint64_t nowMs = gNowMsFn();
	state->phase = PlayerInboxPhase::kAwaitDurable;
	state->correlationId = 0;
	state->inflightSinceMs = 0;
	// Ack 请求失败:保留 ackQueue,退避重发。实体若在这期间被销毁,下次落地时 Fetch 会再次返回这些 PENDING 条目,
	// 届时从账本补确认(设计 §7.7)。
	if (grpcCode == kGrpcStatusUnimplemented)
	{
		state->nextAckMs = nowMs + player_inbox_rules::kUnimplementedBackoffMs;
		LOG_INFO << "[PlayerInbox] Inbox service does not implement AckItems yet (older version); backing off"
				 << " player_id=" << playerId << " domain=" << domain;
		return;
	}
	if (state->ackFailStreak < std::numeric_limits<uint32_t>::max())
	{
		++state->ackFailStreak;
	}
	state->nextAckMs = nowMs + player_inbox_rules::BackoffMs(state->ackFailStreak, JitterPermille());
	LOG_WARN << "[PlayerInbox] Ack failed (result unknown; will resend) player_id=" << playerId << " domain=" << domain
			 << " code=" << grpcCode << " detail=" << detail << " streak=" << state->ackFailStreak;
}

// ── 测试注入点 ──────────────────────────────────────────────────────────────

void PlayerInboxSystem::SetPersistFnForTest(PersistFn fn)
{
	gPersistFn = (fn == nullptr) ? &PlayerLifecycleSystem::SavePlayerToRedis : fn;
}

void PlayerInboxSystem::SetNowFnForTest(NowMsFn fn)
{
	gNowMsFn = (fn == nullptr) ? &DefaultSteadyNowMs : fn;
}

void PlayerInboxSystem::SetTransportForTest(SendFetchFn sendFetch, SendAckFn sendAck)
{
	gSendFetchFn = (sendFetch == nullptr) ? &DefaultSendFetch : sendFetch;
	gSendAckFn = (sendAck == nullptr) ? &DefaultSendAck : sendAck;
}

void PlayerInboxSystem::SetConsumerConfigForTest(bool overrideConfig, bool consumerEnabled, uint32_t periodicFetchMinutes)
{
	gConfigOverride.active = overrideConfig;
	gConfigOverride.consumerEnabled = consumerEnabled;
	gConfigOverride.periodicFetchMinutes = periodicFetchMinutes;
}
