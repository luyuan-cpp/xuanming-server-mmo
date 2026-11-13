#include "player/system/player_mail_outbox.h"

#include <cstdlib>
#include <iterator>
#include <limits>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "core/network/grpc_client_call.h"
#include "core/security/service_call_auth.h"
#include "core/security/token_security.h"
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
	using Dependencies = PlayerMailOutboxSystem::Dependencies;

	// 邮件服务按调用方身份定死的发件角色:go/mail/internal/data/repo.go 的 SenderKind("battle") = 5,
	// 签名的调用方名是 "battle",验签密钥是 mail 的 Producers.battle(go/mail/etc/mail.yaml)。三处同一个身份。
	constexpr uint32_t kBattleSenderKind = 5;
	constexpr std::string_view kBattleCaller = "battle";
	constexpr char kBattleProducerSecretEnv[] = "MAIL_PRODUCER_SECRET_BATTLE";
	// 与 service_call_auth::Signature、go/shared/domainhost Config.Secrets 同一个下限:去掉首尾空白后至少 32 字节。
	constexpr std::size_t kMinSecretBytes = 32;

	constexpr const char* kSendStartNames[] = {"sent", "no_ready_node", "no_secret", "invalid_entry", "sign_failed"};
	static_assert(std::size(kSendStartNames) == static_cast<std::size_t>(SendStart::kCount),
				  "kSendStartNames 与 SendStart 逐项对应");
	constexpr const char* kSendVerdictNames[] = {"delivered", "rejected", "unknown"};
	static_assert(std::size(kSendVerdictNames) == static_cast<std::size_t>(SendVerdict::kCount),
				  "kSendVerdictNames 与 SendVerdict 逐项对应");

	// 寄送的按需状态(只在内存,docs/design/ecs-component-access-rules.md「按需状态」):至多一封在途,失败退避。
	// Tick 第一次处理这名玩家的待寄条目(寄送或请求存盘)时建;待寄队列寄空时摘;实体销毁自然清理。只由本文件读写。
	struct MailOutboxSendStateComp
	{
		uint64_t inFlightRequestId{0}; // 0 = 没有在途
		uint64_t inFlightAttempt{0};   // 在途那一次的尝试号:判丢失后重寄同一条,请求号相同,靠它认出迟到的旧回答
		uint64_t inFlightSinceMs{0};
		uint64_t nextAttemptMs{0};     // 失败退避到这个时刻(UTC 毫秒);0 = 随时
		uint32_t failStreak{0};        // 连续失败次数(含没发出去、在途丢失),寄到一封归 0
		uint64_t attempts{0};          // 已发起的尝试次数,尝试号从 1 起
		uint64_t lastSaveRequestMs{0}; // 上一次因「条目还没落盘」请求存盘的时刻;0 = 还没请求过
	};

	uint64_t InFlightLostAfterMs()
	{
		// 在途记录必须比调用期限宽(grpc_call_deadline.h:依赖调用结果的看门狗一律从 Get 派生)。
		return static_cast<uint64_t>(grpc_call_deadline::Get(common::base::MailNodeService).count()) +
			   PlayerMailOutboxSystem::kInFlightLostMarginMs;
	}

	// 发件密钥:没配或不足 32 字节时返回空串。每次现读环境变量(与别的调用密钥同一做法),不缓存、不进日志。
	std::string BattleProducerSecret()
	{
		const char* secret = std::getenv(kBattleProducerSecretEnv);
		if (secret == nullptr || token_security::TrimAscii(secret).size() < kMinSecretBytes)
		{
			return {};
		}
		return secret;
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

	bool DefaultSenderConfigured()
	{
		return !BattleProducerSecret().empty();
	}

	SendStart DefaultSend(const uint64_t playerId, const MailOutboxEntry& entry, PlayerMailOutboxSystem::SendDone done)
	{
		const std::string secret = BattleProducerSecret();
		if (secret.empty())
		{
			return SendStart::kNoSecret;
		}
		mailpb::SendMailRequest request;
		if (!PlayerMailOutboxSystem::BuildSendRequest(playerId, entry, request))
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
		// 回调只捕获 done(不捕获实体句柄,GrpcDone 契约)。MailAdmin 的回包没有签名字段(rank 发奖同此),
		// 信的是发现到的 mail 节点,与存盘、收件箱的回包同一个信任层级(battle-overflow-mail.md「残余」)。
		mailpb::SendMailAdminSendPersonalMail(
			registry, node, request,
			[done = std::move(done)](const GrpcCallOutcome& outcome, const mailpb::SendMailResponse* reply) {
				PlayerMailOutboxSystem::SendOutcome result;
				result.callOk = outcome.ok() && reply != nullptr;
				result.signatureRejected = mail_outbox_rules::IsSignatureRejected(outcome.code);
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

	void DefaultRequestSave(entt::entity player, const char* reason)
	{
		PlayerLifecycleSystem::RequestKeyProgressSave(player, reason);
	}

	uint64_t DefaultNowMs()
	{
		return TimeSystem::NowMillisecondsUTC();
	}

	// 没填的成员用生产实现补齐。
	Dependencies WithDefaults(Dependencies dependencies)
	{
		if (!dependencies.senderConfigured)
		{
			dependencies.senderConfigured = &DefaultSenderConfigured;
		}
		if (!dependencies.send)
		{
			dependencies.send = &DefaultSend;
		}
		if (!dependencies.isDurable)
		{
			dependencies.isDurable = &DefaultIsDurable;
		}
		if (!dependencies.requestSave)
		{
			dependencies.requestSave = &DefaultRequestSave;
		}
		if (!dependencies.nowMs)
		{
			dependencies.nowMs = &DefaultNowMs;
		}
		return dependencies;
	}

	// 当前生效的依赖。只在 scene loop 线程读写;函数内静态,不依赖别的翻译单元的初始化顺序。
	Dependencies& Deps()
	{
		static Dependencies dependencies = WithDefaults({});
		return dependencies;
	}

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
		auto& state = tlsEcs.actorRegistry.get_or_emplace<MailOutboxSendStateComp>(player);
		const auto* outbox = tlsEcs.actorRegistry.try_get<PlayerMailOutboxComp>(player);
		const auto* entry = outbox != nullptr ? mail_outbox_rules::Find(*outbox, requestId) : nullptr;
		if (entry == nullptr)
		{
			return;
		}
		const uint64_t attempt = ++state.attempts;
		const auto started = Deps().send(playerId, *entry, [playerId, requestId, attempt](PlayerMailOutboxSystem::SendOutcome outcome) {
			PlayerMailOutboxSystem::OnSendDone(playerId, requestId, attempt, outcome, Deps().nowMs());
		});
		if (started == SendStart::kSent)
		{
			state.inFlightRequestId = requestId;
			state.inFlightAttempt = attempt;
			state.inFlightSinceMs = nowMs;
			return;
		}
		RecordFailure(state, /*rejected=*/false, nowMs);
		if (!mail_outbox_rules::IsFailureLogDue(state.failStreak))
		{
			return;
		}
		const char* reason = static_cast<std::size_t>(started) < std::size(kSendStartNames)
								 ? kSendStartNames[static_cast<std::size_t>(started)]
								 : "unknown";
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

	// 有条目、却没有一条在已提交的快照里:刚加载(这个代次还没提交过),或写进条目的那一笔存盘还没落定。
	// 隔 kResaveIntervalMs 请求一次存盘,不每拍都压(存盘路径故障时不变成重试风暴)。
	void RequestSaveIfDue(entt::entity player, const uint64_t nowMs)
	{
		auto& state = tlsEcs.actorRegistry.get_or_emplace<MailOutboxSendStateComp>(player);
		if (state.lastSaveRequestMs != 0 && nowMs >= state.lastSaveRequestMs &&
			nowMs - state.lastSaveRequestMs < mail_outbox_rules::kResaveIntervalMs)
		{
			return;
		}
		state.lastSaveRequestMs = nowMs;
		Deps().requestSave(player, "mail_outbox_pending");
	}
} // namespace

bool PlayerMailOutboxSystem::IsSenderConfigured()
{
	return Deps().senderConfigured();
}

bool PlayerMailOutboxSystem::BuildSendRequest(const uint64_t playerId, const MailOutboxEntry& entry,
											  mailpb::SendMailRequest& request)
{
	// 判重先于一切依赖当前时间的校验(go/mail Repo.Send),所以不带过期时刻时重寄不会变成新请求;
	// 有效期按首次落库那一刻 + 个人邮件有效期算,玩家总有完整的领取期。
	if (playerId == 0 || !mail_outbox_rules::ValidateEntry(entry).empty())
	{
		return false;
	}
	mailpb::SendMailRequest built;
	built.set_sender_kind(kBattleSenderKind);
	built.set_request_id(entry.request_id());
	built.set_recipient_player_id(playerId);
	built.set_title(entry.title());
	built.set_body(entry.body());
	auto* attachments = built.mutable_attachments();
	for (const auto& item : entry.items())
	{
		auto* line = attachments->add_items();
		line->set_config_id(item.config_id());
		line->set_count(static_cast<uint32_t>(item.count())); // ValidateEntry 已保证每封合计不超过 4096
	}
	request = std::move(built);
	return true;
}

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
	if (!IsSenderConfigured())
	{
		// 没有部署 mail 的环境:不欠寄不出去的物品,结算照旧整笔延后(启动时已打过一行 WARN,这里不再逐次报)。
		LOG_DEBUG << "[MailOutbox] Overflow not taken: battle mail sender is not configured, player_id=" << playerId
				  << " battle_id=" << battleId;
		return kServiceUnavailable;
	}
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
	TickAt(Deps().nowMs());
}

void PlayerMailOutboxSystem::TickAt(const uint64_t nowMs)
{
	// 先收集再处置:寄送与请求存盘都会挂按需状态组件,不能边遍历边改组件集合。
	struct Due
	{
		entt::entity entity;
		uint64_t playerId;
		uint64_t requestId; // 0 = 没有一条可寄,只需要请求存盘
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
				state->inFlightAttempt = 0; // 那一次的回答若迟到,按「不是当前」处理;寄到了照样删条目
				RecordFailure(*state, /*rejected=*/false, nowMs);
				continue;
			}
			if (nowMs < state->nextAttemptMs)
			{
				continue;
			}
		}
		uint64_t sendable = 0;
		for (const auto& entry : outbox.entries())
		{
			if (Deps().isDurable(entity, entry.request_id()))
			{
				sendable = entry.request_id();
				break;
			}
		}
		due.push_back({entity, playerId, sendable});
	}
	for (const auto& item : due)
	{
		if (item.requestId != 0)
		{
			SendOne(item.entity, item.playerId, item.requestId, nowMs);
		}
		else
		{
			RequestSaveIfDue(item.entity, nowMs);
		}
	}
}

void PlayerMailOutboxSystem::OnSendDone(const uint64_t playerId, const uint64_t requestId, const uint64_t attempt,
										const SendOutcome outcome, const uint64_t nowMs)
{
	const auto player = tlsEcs.GetPlayer(playerId);
	if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayerId(player) != playerId)
	{
		return; // 玩家已不在本节点:条目在他的存档里,下一个持有者再寄(邮件判重,不会多一封)
	}
	auto* state = tlsEcs.actorRegistry.try_get<MailOutboxSendStateComp>(player);
	const bool current =
		state != nullptr && state->inFlightRequestId == requestId && state->inFlightAttempt == attempt;
	if (current)
	{
		state->inFlightRequestId = 0;
		state->inFlightAttempt = 0;
	}
	const auto verdict = outcome.signatureRejected
		? SendVerdict::kRejected
		: mail_outbox_rules::ClassifySend(outcome.callOk, outcome.tipId, outcome.mailId);
	if (verdict != SendVerdict::kDelivered)
	{
		if (!current)
		{
			return; // 不是当前在途那一次的失败回答(那一次已按在途丢失处理过):不再记一次
		}
		RecordFailure(*state, verdict == SendVerdict::kRejected, nowMs);
		if (verdict == SendVerdict::kRejected && (outcome.signatureRejected || mail_outbox_rules::NeedsManualFollowUp(outcome.tipId)))
		{
			// 每次都打,不走下面「第 1 次与每 8 次」的节拍:这类拒收按 30 秒起、封顶 5 分钟的间隔重试,一条卡住的条目
			// 至多 5 分钟一行,值班查询 metric=mail_outbox_stuck 的 [10m] 窗口里始终看得到。限频的话封顶后 40 分钟才一行。
			LOG_ERROR << "[MailOutbox] metric=mail_outbox_stuck player_id=" << playerId << " request_id=" << requestId
					  << " reason=" << (outcome.signatureRejected ? "signature_rejected" : "mail_rejected")
						  << " tip=" << outcome.tipId << " fail_streak=" << state->failStreak
					  << ",mail rejected this entry and retrying will not change that; the items stay owed in the outbox";
			return;
		}
		if (!mail_outbox_rules::IsFailureLogDue(state->failStreak))
		{
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
		Deps().requestSave(player, "mail_outbox_delivered");
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

void PlayerMailOutboxSystem::SetDependenciesForTest(Dependencies dependencies)
{
	Deps() = WithDefaults(std::move(dependencies));
}
