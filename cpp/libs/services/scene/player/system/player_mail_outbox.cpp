#include "player/system/player_mail_outbox.h"

#include <cstdlib>
#include <iterator>
#include <limits>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "core/network/grpc_client_call.h"
#include "core/security/service_call_auth.h"
#include "grpc_client/mail/mail_admin_grpc_client.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "node/system/grpc_call_deadline.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/role_admission_system.h"
#include "proto/common/base/common.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/mail/mail_admin.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "time/system/time.h"
#include "utils/random/random.h"

namespace
{
	using mail_outbox_rules::MailOutboxSource;
	using mail_outbox_rules::SendVerdict;
	using SendStart = PlayerMailOutboxSystem::SendStart;

	// 邮件服务按调用方身份定死的发件角色:go/mail/internal/data/repo.go 的 SenderKind("battle") = 5,
	// 签名的调用方名是 "battle",验签密钥是 mail 的 Producers.battle(go/mail/etc/mail.yaml)。三处同一个身份。
	constexpr uint32_t kBattleSenderKind = 5;
	constexpr std::string_view kBattleCaller = "battle";
	constexpr char kBattleProducerSecretEnv[] = "MAIL_PRODUCER_SECRET_BATTLE";

	constexpr const char* kSendStartNames[] = {"sent", "no_ready_node", "no_secret", "invalid_entry", "sign_failed"};
	static_assert(std::size(kSendStartNames) == static_cast<std::size_t>(SendStart::kCount),
				  "kSendStartNames 与 SendStart 逐项对应");
	constexpr const char* kSendVerdictNames[] = {"delivered", "rejected", "unknown"};
	static_assert(std::size(kSendVerdictNames) == static_cast<std::size_t>(SendVerdict::kCount),
				  "kSendVerdictNames 与 SendVerdict 逐项对应");

	// 寄送的按需状态(只在内存,docs/design/ecs-component-access-rules.md「按需状态」):至多一封在途,失败退避。
	// 玩家有可寄条目、第一次寄送时建;待寄队列寄空时摘;实体销毁自然清理。只由本文件读写。
	struct MailOutboxSendStateComp
	{
		uint64_t inFlightRequestId{0}; // 0 = 没有在途
		uint64_t inFlightSinceMs{0};
		uint64_t nextAttemptMs{0};     // 失败退避到这个时刻(UTC 毫秒);0 = 随时
		uint32_t failStreak{0};        // 连续失败次数(含没发出去、在途丢失),寄到一封归 0
	};

	uint64_t InFlightLostAfterMs()
	{
		// 在途记录必须比调用期限宽(grpc_call_deadline.h:依赖调用结果的看门狗一律从 Get 派生)。
		return static_cast<uint64_t>(grpc_call_deadline::Get(common::base::MailNodeService).count()) +
			   PlayerMailOutboxSystem::kInFlightLostMarginMs;
	}

	// 就绪的 mail 节点里随机挑一个:任一副本读写的都是同一个全局库,某个副本卡住时下一次大概率换到别的副本。
	entt::entity PickReadyMailNode(entt::registry& registry)
	{
		std::vector<entt::entity> ready;
		for (const auto node : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
		{
			const auto& channel = registry.get<MuduoGrpcChannelPtr>(node);
			if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady)
			{
				ready.push_back(node);
			}
		}
		if (ready.empty())
		{
			return entt::null;
		}
		return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
	}

	// 请求由条目逐字段确定性重建,不带过期时刻(邮件按首次落库那一刻 + 个人邮件有效期算,判重先于一切依赖当前时间的校验,
	// 重寄不会因此变成新请求)。条目形状不合法返回 false,不发。
	bool BuildRequest(const uint64_t playerId, const MailOutboxEntry& entry, mailpb::SendMailRequest& request)
	{
		if (!mail_outbox_rules::ValidateEntry(entry).empty())
		{
			return false;
		}
		request.set_sender_kind(kBattleSenderKind);
		request.set_request_id(entry.request_id());
		request.set_recipient_player_id(playerId);
		request.set_title(entry.title());
		request.set_body(entry.body());
		auto* attachments = request.mutable_attachments();
		for (const auto& item : entry.items())
		{
			auto* line = attachments->add_items();
			line->set_config_id(item.config_id());
			line->set_count(static_cast<uint32_t>(item.count())); // ValidateEntry 已保证每封合计不超过 4096
		}
		return true;
	}

	SendStart DefaultSend(const uint64_t playerId, const MailOutboxEntry& entry, PlayerMailOutboxSystem::SendDone done)
	{
		const char* secret = std::getenv(kBattleProducerSecretEnv);
		if (secret == nullptr)
		{
			return SendStart::kNoSecret;
		}
		mailpb::SendMailRequest request;
		if (!BuildRequest(playerId, entry, request))
		{
			return SendStart::kInvalidEntry;
		}
		if (!service_call_auth::Sign(mailpb::kMailAdminSendPersonalMailGrpcPath, request, *request.mutable_auth(),
									 kBattleCaller, secret, TimeSystem::NowMillisecondsUTC()))
		{
			return SendStart::kSignFailed;
		}
		auto& registry = tlsNodeContextManager.GetRegistry(common::base::MailNodeService);
		const auto node = PickReadyMailNode(registry);
		if (node == entt::null)
		{
			return SendStart::kNoReadyNode;
		}
		// 回调只捕获 done(不捕获实体句柄,GrpcDone 契约)。回包不签名:MailAdmin 的结果只用来决定删不删条目,
		// 删错的代价是重寄一次拿回原邮件,不会多发。
		mailpb::SendMailAdminSendPersonalMail(
			registry, node, request,
			[done = std::move(done)](const GrpcCallOutcome& outcome, const mailpb::SendMailResponse* reply) {
				PlayerMailOutboxSystem::SendOutcome result;
				result.callOk = outcome.ok() && reply != nullptr;
				if (reply != nullptr)
				{
					result.tipId = reply->error_message().id();
					result.mailId = reply->mail_id();
				}
				done(result);
			});
		return SendStart::kSent;
	}

	// 可以寄 = 最近一次已提交的存档快照里已经有这一条(与收件箱确认、资产通道的落盘判定同一个依据)。
	bool DefaultIsDurable(entt::entity player, const uint64_t requestId)
	{
		const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
		if (persisted == nullptr || !persisted->HasSnapshot() ||
			!RoleAdmissionSystem::IsSnapshotCommitted(player, *persisted->snapshot))
		{
			return false;
		}
		return mail_outbox_rules::Find(persisted->snapshot->player_database_data().mail_outbox(), requestId) != nullptr;
	}

	PlayerMailOutboxSystem::SendFn gSend = &DefaultSend;
	PlayerMailOutboxSystem::DurableFn gIsDurable = &DefaultIsDurable;

	void RecordFailure(MailOutboxSendStateComp& state, const bool rejected, const uint64_t nowMs)
	{
		if (state.failStreak < std::numeric_limits<uint32_t>::max())
		{
			++state.failStreak;
		}
		state.nextAttemptMs = nowMs + mail_outbox_rules::RetryDelayMs(rejected, state.failStreak);
	}

	void SendOne(entt::entity player, const uint64_t playerId, const uint64_t requestId, const uint64_t nowMs)
	{
		const auto* outbox = tlsEcs.actorRegistry.try_get<PlayerMailOutboxComp>(player);
		const auto* entry = outbox != nullptr ? mail_outbox_rules::Find(*outbox, requestId) : nullptr;
		if (entry == nullptr)
		{
			return;
		}
		const auto started = gSend(playerId, *entry, [playerId, requestId](PlayerMailOutboxSystem::SendOutcome outcome) {
			PlayerMailOutboxSystem::OnSendDone(playerId, requestId, outcome, TimeSystem::NowMillisecondsUTC());
		});
		auto& state = tlsEcs.actorRegistry.get_or_emplace<MailOutboxSendStateComp>(player);
		if (started == SendStart::kSent)
		{
			state.inFlightRequestId = requestId;
			state.inFlightSinceMs = nowMs;
			return;
		}
		RecordFailure(state, /*rejected=*/false, nowMs);
		if (!mail_outbox_rules::IsFailureLogDue(state.failStreak))
		{
			return;
		}
		const char* reason = kSendStartNames[static_cast<std::size_t>(started)];
		if (started == SendStart::kNoReadyNode)
		{
			LOG_WARN << "[MailOutbox] metric=mail_outbox_send_failed player_id=" << playerId << " request_id=" << requestId
					 << " reason=" << reason << " fail_streak=" << state.failStreak
					 << ",no ready mail node; the entry stays queued and is retried";
			return;
		}
		LOG_ERROR << "[MailOutbox] metric=mail_outbox_stuck player_id=" << playerId << " request_id=" << requestId
				  << " reason=" << reason << " fail_streak=" << state.failStreak
				  << ",mail could not be sent (missing or short " << kBattleProducerSecretEnv
				  << ", or an invalid entry); the items stay owed in the outbox until this is fixed";
	}
} // namespace

uint32_t PlayerMailOutboxSystem::PrepareBattleOverflow(entt::entity player, const uint64_t battleId,
													   const mail_outbox_rules::MailItems& items, PreparedMails& out)
{
	out.clear();
	auto mails = mail_outbox_rules::SplitIntoMails(items);
	if (battleId == 0 || mails.empty() || !tlsEcs.actorRegistry.valid(player))
	{
		return kInvalidParameter;
	}
	const uint64_t playerId = tlsEcs.GetPlayerId(player);
	const auto* outbox = tlsEcs.actorRegistry.try_get<PlayerMailOutboxComp>(player);
	if (outbox == nullptr)
	{
		// 持久数据只在加载时创建,缺了是加载不变量被破坏:不补建,这一批不收(结算整笔延后)。
		LOG_ERROR << "[MailOutbox] Overflow refused: persisted PlayerMailOutboxComp missing (load invariant broken), player_id="
				  << playerId << " battle_id=" << battleId;
		return kServiceUnavailable;
	}
	if (static_cast<std::size_t>(outbox->entries_size()) + mails.size() > mail_outbox_rules::kMaxEntries)
	{
		LOG_WARN << "[MailOutbox] metric=mail_outbox_full player_id=" << playerId << " battle_id=" << battleId
				 << " queued=" << outbox->entries_size() << " needed=" << mails.size()
				 << " limit=" << mail_outbox_rules::kMaxEntries
				 << ",the outbox cannot take this overflow; the battle settlement is deferred until mail catches up";
		return kBagItemNotStacked;
	}
	PreparedMails prepared;
	prepared.reserve(mails.size());
	for (auto& mail : mails)
	{
		Guid requestId = kInvalidGuid;
		if (!tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).TryNext(requestId) || requestId == kInvalidGuid || requestId == 0)
		{
			LOG_WARN << "[MailOutbox] metric=mail_outbox_request_id_shortage player_id=" << playerId << " battle_id=" << battleId
					 << ",no txlog id available for the mail request; the battle settlement is deferred and retried";
			return kServiceUnavailable;
		}
		prepared.push_back(PreparedMail{requestId, std::move(mail)});
	}
	out = std::move(prepared);
	return kSuccess;
}

void PlayerMailOutboxSystem::CommitPrepared(entt::entity player, const uint64_t battleId, const PreparedMails& prepared,
											const uint64_t nowMs)
{
	auto* outbox = tlsEcs.actorRegistry.try_get<PlayerMailOutboxComp>(player);
	for (const auto& mail : prepared)
	{
		if (outbox == nullptr)
		{
			// 走不到:PrepareBattleOverflow 同一帧核过组件在。真走到了背包已经发布,只能留证据人工补发。
			LOG_ERROR << "[MailOutbox][ITEMS_LOST] PlayerMailOutboxComp vanished between prepare and commit, player_id="
					  << tlsEcs.GetPlayerId(player) << " battle_id=" << battleId << " request_id=" << mail.requestId
					  << " item_lines=" << mail.items.size() << ",manual compensation required";
			continue;
		}
		mail_outbox_rules::Append(*outbox, mail.requestId, MailOutboxSource::kBattleOverflow, battleId,
								  kBattleOverflowTitle, kBattleOverflowBody, mail.items, nowMs);
	}
}

void PlayerMailOutboxSystem::Tick()
{
	TickAt(TimeSystem::NowMillisecondsUTC());
}

void PlayerMailOutboxSystem::TickAt(const uint64_t nowMs)
{
	// 先收集再寄:寄送会挂按需状态组件。
	struct Due
	{
		entt::entity entity;
		uint64_t playerId;
		uint64_t requestId;
	};
	std::vector<Due> due;
	for (auto&& [entity, outbox] : tlsEcs.actorRegistry.view<PlayerMailOutboxComp>().each())
	{
		if (outbox.entries_size() == 0)
		{
			continue;
		}
		const uint64_t playerId = tlsEcs.GetPlayerId(entity);
		if (playerId == 0 || tlsEcs.GetPlayer(playerId) != entity || !player_mutability::IsPlayerMutable(entity))
		{
			continue; // 只寄登记在本节点、此刻可改的玩家;交接完成后在目的地寄,退出后下次加载再寄
		}
		if (auto* state = tlsEcs.actorRegistry.try_get<MailOutboxSendStateComp>(entity); state != nullptr)
		{
			if (state->inFlightRequestId != 0)
			{
				const uint64_t inFlightMs = nowMs > state->inFlightSinceMs ? nowMs - state->inFlightSinceMs : 0;
				if (inFlightMs < InFlightLostAfterMs())
				{
					continue;
				}
				LOG_WARN << "[MailOutbox] metric=mail_outbox_send_lost player_id=" << playerId
						 << " request_id=" << state->inFlightRequestId << " in_flight_ms=" << inFlightMs
						 << ",no answer within the call deadline; treated as unknown and retried";
				state->inFlightRequestId = 0;
				RecordFailure(*state, /*rejected=*/false, nowMs);
				continue;
			}
			if (nowMs < state->nextAttemptMs)
			{
				continue;
			}
		}
		for (const auto& entry : outbox.entries())
		{
			if (gIsDurable(entity, entry.request_id()))
			{
				due.push_back({entity, playerId, entry.request_id()});
				break;
			}
		}
	}
	for (const auto& item : due)
	{
		SendOne(item.entity, item.playerId, item.requestId, nowMs);
	}
}

void PlayerMailOutboxSystem::OnSendDone(const uint64_t playerId, const uint64_t requestId, const SendOutcome outcome,
										const uint64_t nowMs)
{
	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
	{
		return; // 玩家已不在本节点:条目在他的存档里,下一个持有者再寄(邮件判重,不会多一封)
	}
	auto* state = tlsEcs.actorRegistry.try_get<MailOutboxSendStateComp>(player);
	const bool current = state != nullptr && state->inFlightRequestId == requestId;
	if (current)
	{
		state->inFlightRequestId = 0;
	}
	const auto verdict = mail_outbox_rules::ClassifySend(outcome.callOk, outcome.tipId, outcome.mailId);
	if (verdict != SendVerdict::kDelivered)
	{
		if (!current)
		{
			return; // 不是当前在途那封的失败回答(已按在途丢失处理过):不再记一次
		}
		RecordFailure(*state, verdict == SendVerdict::kRejected, nowMs);
		if (!mail_outbox_rules::IsFailureLogDue(state->failStreak))
		{
			return;
		}
		if (verdict == SendVerdict::kRejected && mail_outbox_rules::NeedsManualFollowUp(outcome.tipId))
		{
			LOG_ERROR << "[MailOutbox] metric=mail_outbox_stuck player_id=" << playerId << " request_id=" << requestId
					  << " reason=mail_rejected tip=" << outcome.tipId << " fail_streak=" << state->failStreak
					  << ",mail rejected this entry and retrying will not change that; the items stay owed in the outbox";
			return;
		}
		LOG_WARN << "[MailOutbox] metric=mail_outbox_send_failed player_id=" << playerId << " request_id=" << requestId
				 << " verdict=" << kSendVerdictNames[static_cast<std::size_t>(verdict)] << " tip=" << outcome.tipId
				 << " call_ok=" << outcome.callOk << " fail_streak=" << state->failStreak
				 << ",the entry stays queued and is retried";
		return;
	}
	// 寄到了(含判重回原邮件)。删条目是存档改动:此刻不可改(交接、退出中)就留着,下一个持有者重寄会拿回同一封,再删。
	if (!player_mutability::IsPlayerMutable(player))
	{
		LOG_INFO << "[MailOutbox] Mail delivered but player is not mutable now; entry kept for the next holder, player_id="
				 << playerId << " request_id=" << requestId << " mail_id=" << outcome.mailId;
		return;
	}
	auto* outbox = tlsEcs.actorRegistry.try_get<PlayerMailOutboxComp>(player);
	if (const auto* entry = outbox != nullptr ? mail_outbox_rules::Find(*outbox, requestId) : nullptr; entry != nullptr)
	{
		const uint32_t source = entry->source();
		const uint64_t sourceId = entry->source_id();
		mail_outbox_rules::Remove(*outbox, requestId);
		LOG_INFO << "[MailOutbox] metric=mail_outbox_delivered player_id=" << playerId << " request_id=" << requestId
				 << " mail_id=" << outcome.mailId << " source=" << mail_outbox_rules::MailOutboxSourceName(source)
				 << " source_id=" << sourceId;
		// 删条目尽快落盘;没落盘就崩,下一次加载重寄,邮件回原邮件,再删一次。
		PlayerLifecycleSystem::RequestKeyProgressSave(player, "mail_outbox_delivered");
	}
	if (state != nullptr)
	{
		state->failStreak = 0;
		state->nextAttemptMs = 0;
	}
	if (outbox == nullptr || outbox->entries_size() == 0)
	{
		tlsEcs.actorRegistry.remove<MailOutboxSendStateComp>(player);
	}
}

void PlayerMailOutboxSystem::SetSendForTest(SendFn fn)
{
	gSend = fn ? std::move(fn) : SendFn(&DefaultSend);
}

void PlayerMailOutboxSystem::SetDurableCheckForTest(DurableFn fn)
{
	gIsDurable = fn ? std::move(fn) : DurableFn(&DefaultIsDurable);
}
