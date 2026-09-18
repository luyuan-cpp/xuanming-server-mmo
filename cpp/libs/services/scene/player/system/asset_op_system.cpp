#include "player/system/asset_op_system.h"

#include <array>
#include <cstddef>
#include <iterator>
#include <limits>
#include <string>
#include <string_view>
#include <unordered_set>

#include "muduo/base/Logging.h"

#include "battle/system/player_battle.h"
#include "engine/core/time/system/time.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/asset_op_ledger.h"
#include "player/system/player_lifecycle.h"
#include "proto/common/component/player_comp.pb.h" // UnregisterPlayer
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
// ── RPC ─────────────────────────────────────────────────────────────────────

// 三个入口的差别只有:允许的流方向、是否校验 tx 白名单、应用段做什么。
// 最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2:不用宏生成声明)。
enum class AssetOpRpc : uint8_t
{
	kDebit,
	kAbortDebit,
	kCredit,
	kCount
};

// 这三个字面量同时是签名 canonical 串的第 3 行(§4.32)与日志里的 rpc 字段,
// **改名就是改协议**:Go 侧 assetop 必须同字面量,否则全部验签失败。
// 用 const char* 而不是 string_view:muduo 的 LogStream 没有 string_view 重载
// (LogStream.h:120/138/144 只有 const char* / string / StringPiece)。
constexpr const char* kAssetOpRpcNames[] = {"debit", "abort_debit", "credit"};
static_assert(std::size(kAssetOpRpcNames) == static_cast<size_t>(AssetOpRpc::kCount),
			  "rpc 名字表与 AssetOpRpc 脱节");

const char* RpcName(AssetOpRpc rpc)
{
	return kAssetOpRpcNames[static_cast<size_t>(rpc)];
}

// ── 流 ↔ 方向 ↔ tx 白名单(§4.8)────────────────────────────────────────────
//
// 本表只管"流 ↔ tx_type";"流 ↔ 调用方"由验签白名单管(§4.32)。
// SYSTEM_CREDIT 在 v1 没有合法调用方(验签一律拒),这一行只是预留。

enum class AssetOpDirection : uint8_t
{
	kDebit,
	kCredit,
	kCount
};

constexpr int kAssetOpMaxTxPerStream = 3;

struct AssetOpStreamRule
{
	AssetOpStream stream;
	AssetOpDirection direction;
	uint8_t txCount;                                        // allowedTx 前 txCount 项有效
	std::array<TransactionType, kAssetOpMaxTxPerStream> allowedTx; // 余位用 TX_UNKNOWN 补
};

constexpr AssetOpStreamRule kAssetOpStreamRules[] = {
	{ASSET_OP_STREAM_GUILD_DEBIT, AssetOpDirection::kDebit, 1,
	 {TX_GUILD_DONATE, TX_UNKNOWN, TX_UNKNOWN}},
	{ASSET_OP_STREAM_GUILD_CREDIT, AssetOpDirection::kCredit, 2,
	 {TX_GUILD_SHOP, TX_GUILD_ACTIVITY_REWARD, TX_UNKNOWN}},
	{ASSET_OP_STREAM_TRADE_DEBIT, AssetOpDirection::kDebit, 1,
	 {TX_AUCTION_SELL, TX_UNKNOWN, TX_UNKNOWN}},
	{ASSET_OP_STREAM_TRADE_CREDIT, AssetOpDirection::kCredit, 2,
	 {TX_AUCTION_BUY, TX_TRADE, TX_UNKNOWN}},
	{ASSET_OP_STREAM_SYSTEM_CREDIT, AssetOpDirection::kCredit, 3,
	 {TX_SYSTEM_GRANT, TX_MAIL_ATTACHMENT, TX_GM_GRANT}},
};
static_assert(std::size(kAssetOpStreamRules) == 5, "流规则表与 AssetOpStream 的 5 条流脱节");

const AssetOpStreamRule* FindStreamRule(AssetOpStream stream)
{
	for (const auto& rule : kAssetOpStreamRules)
	{
		if (rule.stream == stream)
		{
			return &rule;
		}
	}
	return nullptr; // 含 ASSET_OP_STREAM_UNSPECIFIED 与未知值
}

bool IsTxAllowed(const AssetOpStreamRule& rule, uint32_t txType)
{
	for (uint8_t index = 0; index < rule.txCount; ++index)
	{
		if (static_cast<uint32_t>(rule.allowedTx[index]) == txType)
		{
			return true;
		}
	}
	return false;
}

// ── 可注入依赖(AGENTS §11.2 显式依赖;生产取默认值)────────────────────────

int64_t DefaultNowMs()
{
	return static_cast<int64_t>(TimeSystem::NowMillisecondsUTC());
}

PlayerAssetOpSystem::PersistFn g_persistFn = &PlayerLifecycleSystem::SavePlayerToRedis;
PlayerAssetOpSystem::AddCurrencyFn g_addCurrencyFn = &CurrencySystem::AddCurrency;
PlayerAssetOpSystem::NowMsFn g_nowMsFn = &DefaultNowMs;

// nullptr 的含义由 asset_op_auth.h 约定:走默认实现(读环境变量
// MMORPG_ASSET_OP_SECRET_<CALLER> 并缓存)。密钥值**不进日志、不进仓库**。
AssetOpSecretLookup g_secretLookup = nullptr;

// ── 响应小工具 ──────────────────────────────────────────────────────────────

void SetReason(::AssetOpResponse& response, uint32_t tipId)
{
	response.mutable_reason()->set_id(tipId);
}

void Answer(::AssetOpResponse& response, ::AssetOpOutcome outcome, uint32_t tipId)
{
	response.set_outcome(outcome);
	SetReason(response, tipId);
}

// ── 可改判定 ────────────────────────────────────────────────────────────────

// 该实体此刻能不能被改(不变量 I1 单写者的本地一侧)。
//
// **刻意不复用 PlayerLifecycleSystem::IsCrossZoneFrozen**:它只看 PlayerFrozenComp,
// 漏掉"归属交接已挂但存盘还没落地"的窗口。而 SavePlayerToRedis 在
// `PlayerTravelHandoffComp.requestedAtMs != 0` 时直接跳过写盘并返回 false
// (player_lifecycle.cpp:1050-1058)—— 与"脏比较相等、盘上已是最新"共用同一个返回值。
// 若在交接在途时记账,这个 false 会被误当成"结局已在盘上",把没落地的结局报成 durable,
// Go 随即终结,而这份内存态马上会随实体销毁一起丢。所以交接一挂上就必须在**记账之前**
// 回 RETRY。UnregisterPlayer(退出存盘在途)同理:退出优先,不接受新改动。
bool IsPlayerMutable(entt::entity player)
{
	return !tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp, UnregisterPlayer>(player);
}

// ── durable 判定(§4.6)─────────────────────────────────────────────────────

// 只看"确实落盘的字节":PlayerLastPersistedSnapshotComp.snapshot 是
// HandlePlayerAsyncSaved 回调里写入的、刚写进 Redis 的整份 PlayerAllData。
// 某 seq 的结局出现在这份快照的账本里 = durable。不另建"已持久化水位",不存在两份真相。
bool IsAssetOpDurable(entt::entity player, AssetOpStream stream, uint64_t epoch, uint64_t seq,
					  bool expectApplied)
{
	const auto* snapshotComp = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
	if (snapshotComp == nullptr || !snapshotComp->HasSnapshot())
	{
		return false;
	}

	const auto& persisted = snapshotComp->snapshot->player_database_data().asset_op_ledger();
	const auto* ledger = FindAssetOpStream(persisted, stream);
	if (ledger == nullptr || ledger->stream_epoch() != epoch)
	{
		return false;
	}

	const auto state = ClassifyAssetOpSeq(ledger, epoch, seq);
	if (state == AssetOpSeqState::kApplied || state == AssetOpSeqState::kRejected)
	{
		const bool appliedOnDisk = (state == AssetOpSeqState::kApplied);
		if (appliedOnDisk != expectApplied)
		{
			// 盘上的结局与内存里的结局不同:结局固定(I2)被破坏,只可能是 bug。
			// 不敢报 durable,让 Go 继续重查并告警。
			LOG_ERROR << "[AssetOp] 快照结局与内存结局不一致 stream=" << static_cast<int>(stream)
					  << " epoch=" << epoch << " seq=" << seq
					  << " on_disk=" << std::string(AssetOpSeqStateName(state))
					  << " expect_applied=" << expectApplied;
			return false;
		}
		return true;
	}
	return false;
}

// 触发一次存盘并记下时刻。返回 SavePlayerToRedis 的原值:
// true = 确实压了一次写、稍后有落盘回调;false = 脏比较判定与上次落盘逐字段相等、本次不写。
// **前置**:IsPlayerMutable(player) 为真 —— 否则 false 还可能来自"交接在途跳过写盘",
// 那是"没落盘",不是"已在盘上"(见 IsPlayerMutable 的说明)。
bool RequestAssetOpPersist(entt::entity player, int64_t nowMs)
{
	auto& persistRequest = tlsEcs.actorRegistry.get_or_emplace<PlayerAssetOpPersistRequestComp>(player);
	persistRequest.lastRequestMs = nowMs;
	return g_persistFn(player);
}

// 记账之后统一走这里:立刻触发存盘,如实回报 durable,**不在 gRPC 调用里等落盘**(§4.6)。
bool PersistAndProbeDurable(entt::entity player, const ::AssetOpRequest& request, bool expectApplied,
							int64_t nowMs)
{
	const bool wrote = RequestAssetOpPersist(player, nowMs);
	const bool durable =
		IsAssetOpDurable(player, request.stream(), request.stream_epoch(), request.seq(), expectApplied);
	if (!wrote && !durable)
	{
		// 没写盘 = 内存与上次落盘快照逐字段相等;结局既然已在内存里,就必然也在快照里。
		// 走到这里说明脏比较或快照维护有 bug,如实报 durable=false 让 Go 继续重查。
		LOG_ERROR << "[AssetOp] 存盘被脏比较跳过,但结局不在落盘快照里 player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq();
	}
	return durable;
}

// 已见 seq 的只读答复:回原结局,并在允许时补一次存盘把 durable 追平(§4.9 第 4 步)。
// **不经过任何闸门**:结局早已固定,冻结/战斗都不该改变答复。
void AnswerSeenSeq(entt::entity player, const AssetOpStreamLedger& ledger, const ::AssetOpRequest& request,
				   AssetOpSeqState state, int64_t nowMs, ::AssetOpResponse& response)
{
	const bool applied = (state == AssetOpSeqState::kApplied);
	if (applied)
	{
		response.set_outcome(ASSET_OP_OUTCOME_APPLIED);
		if (IsAssetOpPartial(ledger, request.seq()))
		{
			response.set_partial(true);
			SetReason(response, kAssetPartialApplied);
		}
	}
	else
	{
		// 原因环被挤掉时回 0,Go 按"通用拒绝"映射;中止占位本来就是 0。
		Answer(response, ASSET_OP_OUTCOME_REJECTED, AssetOpRejectionReason(ledger, request.seq()));
	}

	bool durable =
		IsAssetOpDurable(player, request.stream(), request.stream_epoch(), request.seq(), applied);
	if (!durable && IsPlayerMutable(player))
	{
		const auto* persistRequest = tlsEcs.actorRegistry.try_get<PlayerAssetOpPersistRequestComp>(player);
		const int64_t lastRequestMs = persistRequest == nullptr ? 0 : persistRequest->lastRequestMs;
		// 限频:Go 按 100/200/400ms 重查,不限频会把 marshal + proto 比较打成热路径。
		if (nowMs - lastRequestMs >= kAssetOpResaveMinIntervalMs)
		{
			durable = PersistAndProbeDurable(player, request, applied, nowMs);
		}
	}
	response.set_durable(durable);
}

// ── 记账 ────────────────────────────────────────────────────────────────────

// 前置:刚刚 Classify 出 kUnseen / kAheadOfWindow,且此后没有任何人动过账本。
// 返回 false = 前置被破坏(账本未改动,调用方按 UNKNOWN 处理并告警)。
bool RecordOutcome(PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
				   AssetOpRecordKind kind, uint32_t reasonTipId)
{
	auto& ledger = MutableAssetOpStream(ledgerComp, request.stream());
	return RecordAssetOpOutcome(ledger, request.stream_epoch(), request.seq(), kind, reasonTipId);
}

// 记一次拒绝并答复。reasonTipId = 0 是"中止占位"。
void RejectAndRecord(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp,
					 const ::AssetOpRequest& request, uint32_t reasonTipId, int64_t nowMs,
					 ::AssetOpResponse& response)
{
	if (!RecordOutcome(ledgerComp, request, AssetOpRecordKind::kRejected, reasonTipId))
	{
		LOG_ERROR << "[AssetOp] 记账前置被破坏(拒绝) player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	}
	Answer(response, ASSET_OP_OUTCOME_REJECTED, reasonTipId);
	response.set_durable(PersistAndProbeDurable(player, request, /*expectApplied=*/false, nowMs));
}

// ── 包内容校验(§4.9 第 8 步:确定性失败,要记账)──────────────────────────

bool IsCurrencyAmountValid(const ::CurrencyAmount& currency)
{
	return currency.currency_type() < static_cast<uint32_t>(kCurrencyMax) && currency.amount() >= 1 &&
		   currency.amount() <= static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
}

// 聚宝斋 P2 的 guid 扣物 / 宝宝字段(AssetBundle.item_uuids / pet_id)本批**不支持**:
// 没实现就必须显式拒绝,不能默默忽略(AGENTS §11.3 不得静默降级)。P2 实现这两条时
// 在本函数与 ApplyDebit / ApplyCredit 里成对放开。
bool HasUnsupportedP2Fields(const ::AssetBundle& bundle)
{
	return bundle.item_uuids_size() > 0 || bundle.pet_id() != 0;
}

bool ValidateDebitBundle(const ::AssetBundle& bundle, std::string& why)
{
	if (HasUnsupportedP2Fields(bundle))
	{
		why = "debit 不支持 item_uuids / pet_id(聚宝斋 P2)";
		return false;
	}
	if (bundle.currencies_size() != 1 || bundle.items_size() != 0)
	{
		why = "debit v1 只收恰好 1 条货币、0 件物品";
		return false;
	}
	if (!IsCurrencyAmountValid(bundle.currencies(0)))
	{
		why = "debit 货币类型或数额越界";
		return false;
	}
	return true;
}

bool ValidateCreditBundle(const ::AssetBundle& bundle, std::string& why)
{
	if (HasUnsupportedP2Fields(bundle))
	{
		why = "credit 不支持 item_uuids / pet_id(聚宝斋 P2)";
		return false;
	}
	if (bundle.currencies_size() + bundle.items_size() < 1)
	{
		why = "credit 包为空";
		return false;
	}
	if (bundle.currencies_size() > 4 || bundle.items_size() > 16)
	{
		why = "credit 条目超上限(货币 4 / 物品 16)";
		return false;
	}

	std::unordered_set<uint32_t> seenCurrencyTypes;
	for (const auto& currency : bundle.currencies())
	{
		if (!IsCurrencyAmountValid(currency))
		{
			why = "credit 货币类型或数额越界";
			return false;
		}
		if (!seenCurrencyTypes.insert(currency.currency_type()).second)
		{
			why = "credit 同一货币类型重复";
			return false;
		}
	}

	std::unordered_set<uint32_t> seenConfigIds;
	for (const auto& item : bundle.items())
	{
		if (item.count() < 1)
		{
			why = "credit 物品数量为 0";
			return false;
		}
		if (!seenConfigIds.insert(item.config_id()).second)
		{
			why = "credit 同一物品 config_id 重复";
			return false;
		}
		if (ItemTableManager::Instance().FindByIdSilent(item.config_id()).first == nullptr)
		{
			why = "credit 物品 config_id 不存在于 Item 表";
			return false;
		}
	}
	return true;
}

// ── 应用段 ──────────────────────────────────────────────────────────────────

void ApplyDebit(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
				int64_t nowMs, ::AssetOpResponse& response)
{
	const auto& currency = request.bundle().currencies(0);
	const auto type = static_cast<CurrencyType>(currency.currency_type());
	const auto amount = static_cast<int64_t>(currency.amount());

	if (!CurrencySystem::CanAfford(player, type, amount))
	{
		RejectAndRecord(player, ledgerComp, request, kAssetCurrencyInsufficient, nowMs, response);
		return;
	}

	const uint32_t error = CurrencySystem::DeductCurrency(player, type, amount,
														 static_cast<TransactionType>(request.tx_type()),
														 request.correlation_id());
	if (error != kSuccess)
	{
		// 前面的闸门(冻结 / 组件齐备 / 余额)都已通过,这里不该失败。不记账、回 RETRY:
		// 记成 REJECTED 会把一个我们没看懂的失败变成终局,玩家钱没扣、帮会却按拒绝退款。
		LOG_ERROR << "[AssetOp] DeductCurrency 在闸门之后仍失败 player_id=" << request.player_id()
				  << " seq=" << request.seq() << " currency_type=" << currency.currency_type()
				  << " amount=" << currency.amount() << " err=" << error;
		Answer(response, ASSET_OP_OUTCOME_RETRY, error);
		return;
	}

	if (!RecordOutcome(ledgerComp, request, AssetOpRecordKind::kApplied, 0))
	{
		// 钱已经扣了却记不上账:重投会再扣一次。只能大声报错,交人工按流水
		// (correlation_id)对账补偿。正常路径不可达。
		LOG_ERROR << "[AssetOp] 扣款已生效但记账前置被破坏 player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq() << " corr=" << request.correlation_id();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	}
	Answer(response, ASSET_OP_OUTCOME_APPLIED, 0);
	response.set_durable(PersistAndProbeDurable(player, request, /*expectApplied=*/true, nowMs));
}

// Credit 应用(§4.33,顺序固定):货币封禁预检 → 物品 guid 余量预检 → 物品 → 货币 → 结局。
// 先物品后货币:只有物品会"暂时失败"(背包满),先做它就不会留下半截。
// 一旦已有改动后又失败,记 kAppliedPartial 并回 APPLIED + partial=true,
// **不再**把部分发放记成完整成功(AGENTS §11.3:错误不得伪装成功)。
void ApplyCredit(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
				 int64_t nowMs, ::AssetOpResponse& response)
{
	const auto& bundle = request.bundle();
	const auto txType = static_cast<TransactionType>(request.tx_type());

	// (a) 货币封禁预检:封禁是终局拒绝,必须在任何改动之前判掉。
	for (const auto& currency : bundle.currencies())
	{
		const auto type = static_cast<CurrencyType>(currency.currency_type());
		if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency, currency.currency_type()) ||
			CurrencySystem::IsCurrencyBlocked(player, type))
		{
			RejectAndRecord(player, ledgerComp, request, kAssetBlocked, nowMs, response);
			return;
		}
	}

	// (b) 物品 guid 号段余量预检。Σcount 是新实例数的上界(每堆至少 1 个),只会偏严。
	// 号段发不出号时 BagService 会整批拒,失败面前移到这里就不会先扣号再半途而废。
	ItemCountMap itemsToAdd;
	uint64_t guidsNeeded = 0;
	for (const auto& item : bundle.items())
	{
		itemsToAdd[item.config_id()] = item.count();
		guidsNeeded += item.count();
	}
	if (!itemsToAdd.empty())
	{
		const uint64_t available = tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available();
		if (available < guidsNeeded)
		{
			LOG_WARN << "[AssetOp] item guid 号段余量不足,暂不发放 player_id=" << request.player_id()
					 << " seq=" << request.seq() << " needed=" << guidsNeeded << " available=" << available;
			Answer(response, ASSET_OP_OUTCOME_RETRY, 0);
			return;
		}
	}

	bool anyMutated = false;
	uint32_t failedError = kSuccess;
	std::string failedWhat;

	// (c) 物品
	if (!itemsToAdd.empty())
	{
		auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
		const PlayerItemBlockList emptyBlockList;
		const auto* blockList = tlsEcs.actorRegistry.try_get<PlayerItemBlockList>(player);
		// mutated 是 §4.11 给 BagService::AddItems 追加的出参:
		//   AddItems(entity, bag, blockList, items, txType, correlationId, extra, bool* mutated)
		// 语义 = "这次调用有没有真的动过包"(ReserveForBatchAdd 淘汰了实例,或任何一条 AddItem
		// 写出了 guid)。设计稿写的是 7 参签名(那时还没有 extra),落码以工作区现签名为准:
		// extra 是战斗掉落在用的参数,删掉会打断既有调用点。
		bool mutated = false;
		const uint32_t error =
			BagService::AddItems(player, bags.bags[kInventory],
								 blockList == nullptr ? emptyBlockList : *blockList, itemsToAdd, txType,
								 request.correlation_id(), /*extra=*/{}, &mutated);
		if (error == kSuccess)
		{
			anyMutated = true;
		}
		else if (!mutated)
		{
			// 零改动:资产与账本都不动,按错误类别选结局。
			if (error == kBagItemNotStacked)
			{
				// 空间不足:暂时条件,Go 稍后重投同一 seq(超过业务时限由帮会改发中止)。
				Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull);
			}
			else if (error == kInvalidParameter)
			{
				// BagService 对冻结 / 封禁一律回 kInvalidParameter。冻结已在闸门挡掉,
				// 走到这里就是封禁 —— 终局拒绝,记账,Go 按业务退款。
				RejectAndRecord(player, ledgerComp, request, kAssetBlocked, nowMs, response);
			}
			else
			{
				Answer(response, ASSET_OP_OUTCOME_RETRY, error);
			}
			return;
		}
		else
		{
			// 已经写进去一部分才失败:不能重投(会重复发放),只能记部分发放。
			anyMutated = true;
			failedError = error;
			failedWhat = "items";
		}
	}

	// (d) 货币。补缴抵扣(欠款先扣)属于正常应用,不算失败。
	if (failedError == kSuccess)
	{
		for (const auto& currency : bundle.currencies())
		{
			const auto type = static_cast<CurrencyType>(currency.currency_type());
			const uint32_t error = g_addCurrencyFn(player, type, static_cast<int64_t>(currency.amount()),
												   txType, request.correlation_id());
			if (error == kSuccess)
			{
				anyMutated = true;
				continue;
			}
			if (!anyMutated)
			{
				// 一点没改:安全重投,不记账。
				Answer(response, ASSET_OP_OUTCOME_RETRY, error);
				return;
			}
			failedError = error;
			failedWhat = "currency_type=" + std::to_string(currency.currency_type());
			break;
		}
	}

	// (e) 结局
	const bool partial = (failedError != kSuccess);
	const auto kind = partial ? AssetOpRecordKind::kAppliedPartial : AssetOpRecordKind::kApplied;
	if (!RecordOutcome(ledgerComp, request, kind, 0))
	{
		LOG_ERROR << "[AssetOp] 发放已生效但记账前置被破坏 player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq() << " corr=" << request.correlation_id();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	}

	if (partial)
	{
		// 人工补偿的唯一依据,ERROR 级别:哪笔操作、卡在哪一条、什么错。
		LOG_ERROR << "[AssetOp] partial player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq() << " corr=" << request.correlation_id()
				  << " failed=" << failedWhat << " err=" << failedError;
		Answer(response, ASSET_OP_OUTCOME_APPLIED, kAssetPartialApplied);
		response.set_partial(true);
	}
	else
	{
		Answer(response, ASSET_OP_OUTCOME_APPLIED, 0);
	}
	response.set_durable(PersistAndProbeDurable(player, request, /*expectApplied=*/true, nowMs));
}

// ── 统一处理流程(§4.9)─────────────────────────────────────────────────────
//
// 顺序固定,与 PrepareBattle 的入口层惯例一致(player_battle.cpp:812-860):
// 参数/签名 → 玩家是否在本节点 → 冻结(含传送在途)→ 战斗中 → 业务。
void Decide(AssetOpRpc rpc, const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	const bool isAbort = (rpc == AssetOpRpc::kAbortDebit);

	// 1. 信封校验(不记账)
	if (request.player_id() == 0 || request.seq() == 0 || request.stream_epoch() == 0)
	{
		LOG_WARN << "[AssetOp] 信封非法 rpc=" << RpcName(rpc) << " player_id=" << request.player_id()
				 << " seq=" << request.seq() << " epoch=" << request.stream_epoch();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
		return;
	}
	const auto* rule = FindStreamRule(request.stream());
	if (rule == nullptr)
	{
		LOG_WARN << "[AssetOp] 未知流 rpc=" << RpcName(rpc) << " stream=" << static_cast<int>(request.stream());
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
		return;
	}
	// AssetAbortDebit 收全部 5 条流(它只是给未见 seq 记一个拒绝占位),也不校验 tx_type。
	if (!isAbort)
	{
		const auto wanted =
			(rpc == AssetOpRpc::kDebit) ? AssetOpDirection::kDebit : AssetOpDirection::kCredit;
		if (rule->direction != wanted || !IsTxAllowed(*rule, request.tx_type()))
		{
			LOG_WARN << "[AssetOp] 流方向或 tx 白名单不符 rpc=" << RpcName(rpc)
					 << " stream=" << static_cast<int>(request.stream()) << " tx_type=" << request.tx_type();
			Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
			return;
		}
	}

	// 1b. 验签(不记账)。scene gRPC 是明文不安全凭据(node.cpp:623),集群内任何进程都能连,
	// 所以签名是主防线;失败一律 fail-closed,不做 dev 放行(§4.32)。
	const int64_t nowMs = g_nowMsFn();
	if (const auto verdict = VerifyAssetOpAuth(RpcName(rpc), request, nowMs, g_secretLookup);
		verdict != AssetOpAuthVerdict::kOk)
	{
		LOG_WARN << "[AssetOp] 验签失败 rpc=" << RpcName(rpc)
				 << " verdict=" << AssetOpAuthVerdictName(verdict) << " caller=" << request.auth().caller()
				 << " stream=" << static_cast<int>(request.stream()) << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetAuthFailed);
		return;
	}

	// 2. 找人(不记账)
	const auto playerIt = tlsEcs.playerList.find(request.player_id());
	if (playerIt == tlsEcs.playerList.end() || !tlsEcs.actorRegistry.valid(playerIt->second))
	{
		Answer(response, ASSET_OP_OUTCOME_NOT_HERE, kAssetPlayerNotHere);
		return;
	}
	const entt::entity player = playerIt->second;

	// 3. 账本损坏(加载时 Validate 判的,fail-closed 关闭该玩家的资产通道)
	if (tlsEcs.actorRegistry.any_of<PlayerAssetOpLedgerInvalidComp>(player))
	{
		LOG_ERROR << "[AssetOp] 账本损坏,拒绝一切资产操作 player_id=" << request.player_id()
				  << " rpc=" << RpcName(rpc) << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBlocked);
		return;
	}

	// 账本组件由 PlayerDatabaseMessageFieldsUnmarshal 无条件挂上(§4.5)。缺了就是加载路径坏了:
	// **绝不** get_or_emplace 一本空账本 —— 那会把已经应用过的 seq 重新看成未见,直接二次扣款。
	auto* ledgerComp = tlsEcs.actorRegistry.try_get<PlayerAssetOpLedgerComp>(player);
	if (ledgerComp == nullptr)
	{
		LOG_ERROR << "[AssetOp] 实体上没有账本组件(加载未完成或接线缺失) player_id="
				  << request.player_id() << " rpc=" << RpcName(rpc) << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
		return;
	}

	// 4. 分类
	const auto* ledger = FindAssetOpStream(*ledgerComp, request.stream());
	const auto state = ClassifyAssetOpSeq(ledger, request.stream_epoch(), request.seq());
	switch (state)
	{
	case AssetOpSeqState::kInvalid:
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
		return;
	case AssetOpSeqState::kStaleEpoch:
	case AssetOpSeqState::kBehindWindow:
	case AssetOpSeqState::kJumpTooFar:
		// 三者都说明 Go 侧守卫失效、帮会库被重置/恢复,或 Redis 回退过。
		// 不记账、不猜结局,reason 留 0,Go 计 assetop_unknown_total 并转人工(§4.38)。
		LOG_ERROR << "[AssetOp] seq 不可采信 state=" << std::string(AssetOpSeqStateName(state))
				  << " player_id=" << request.player_id() << " rpc=" << RpcName(rpc)
				  << " stream=" << static_cast<int>(request.stream())
				  << " req_epoch=" << request.stream_epoch()
				  << " ledger_epoch=" << (ledger == nullptr ? 0 : ledger->stream_epoch())
				  << " seq=" << request.seq()
				  << " watermark=" << (ledger == nullptr ? 0 : ledger->watermark())
				  << " max_seq=" << (ledger == nullptr ? 0 : ledger->max_seq());
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	case AssetOpSeqState::kApplied:
	case AssetOpSeqState::kRejected:
		// ledger 理论上必非空(空账本只会分出 kUnseen / kJumpTooFar / kInvalid),
		// 但"已见结局"是资产正确性的地基,不拿一次解引用去赌分类实现。
		if (ledger == nullptr)
		{
			LOG_ERROR << "[AssetOp] 分类为已见但流不存在 player_id=" << request.player_id()
					  << " stream=" << static_cast<int>(request.stream()) << " seq=" << request.seq();
			Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
			return;
		}
		AnswerSeenSeq(player, *ledger, request, state, nowMs, response);
		return;
	case AssetOpSeqState::kUnseen:
	case AssetOpSeqState::kAheadOfWindow:
		break; // 继续走闸门
	case AssetOpSeqState::kCount:
	default:
		LOG_ERROR << "[AssetOp] 未知 seq 状态 state=" << static_cast<int>(state);
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	}

	// 5. 改动闸门(未见 seq 才走到这里;已见 seq 上面已返回)
	//
	// 闸只放在这一层。**绝不下沉** BagService / CurrencySystem:战斗结算、任务发奖等内部
	// 路径共用那两个模块,下沉冻结/战斗闸会让结算永久卡死(D48 红线)。
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player))
	{
		// 冻结 + 交接在途都算"这个实体的改动权已经不在本节点",详见 IsPlayerMutable。
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetFrozen);
		return;
	}
	if (tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player))
	{
		// 退出存盘在途:退出优先,不接受新改动。玩家重新上线后 Go 重投同一 seq。
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
		return;
	}

	// 6. 中止占位:给未见 seq 记一个 REJECTED(reason=0),此后这条 seq 永远拒绝。
	// 战斗中也允许:它不改任何资产,只关一扇门。
	if (isAbort)
	{
		RejectAndRecord(player, *ledgerComp, request, 0, nowMs, response);
		return;
	}

	// 7. 应用闸门
	if (PlayerBattleSystem::IsInBattle(player))
	{
		// 结算未落地前改资产会与战斗快照打架(备战 PREPARING 与 FIGHTING 都算)。
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetInBattle);
		return;
	}
	if (tlsEcs.actorRegistry.try_get<CurrencyComp>(player) == nullptr)
	{
		LOG_WARN << "[AssetOp] 玩家缺 CurrencyComp(仍在加载) player_id=" << request.player_id();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
		return;
	}
	if (request.bundle().items_size() > 0 && tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player) == nullptr)
	{
		LOG_WARN << "[AssetOp] 玩家缺 PlayerBagsComp(仍在加载) player_id=" << request.player_id();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
		return;
	}

	// 8. 包内容校验(确定性失败 → 记 REJECTED,免得 Go 无限重投同一个坏包)
	std::string why;
	const bool bundleOk = (rpc == AssetOpRpc::kDebit) ? ValidateDebitBundle(request.bundle(), why)
													  : ValidateCreditBundle(request.bundle(), why);
	if (!bundleOk)
	{
		LOG_WARN << "[AssetOp] 包内容非法 rpc=" << RpcName(rpc) << " player_id=" << request.player_id()
				 << " seq=" << request.seq() << " why=" << why;
		RejectAndRecord(player, *ledgerComp, request, kAssetInvalidBundle, nowMs, response);
		return;
	}

	// 9 / 10. 应用
	if (rpc == AssetOpRpc::kDebit)
	{
		ApplyDebit(player, *ledgerComp, request, nowMs, response);
	}
	else
	{
		ApplyCredit(player, *ledgerComp, request, nowMs, response);
	}
}

// 每次调用结束打一行(§4.9 第 11 步)。日志不是指标,可以带 player_id
// (AGENTS §9 只禁止把 player_id 当 Prometheus label)。
void Process(AssetOpRpc rpc, const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	response.set_outcome(ASSET_OP_OUTCOME_UNKNOWN);
	response.mutable_reason()->set_id(0);
	response.set_durable(false);
	response.set_partial(false);

	Decide(rpc, request, response);

	LOG_INFO << "[AssetOp] rpc=" << RpcName(rpc) << " player_id=" << request.player_id()
			 << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
			 << " seq=" << request.seq() << " corr=" << request.correlation_id()
			 << " outcome=" << static_cast<int>(response.outcome()) << " reason=" << response.reason().id()
			 << " durable=" << response.durable() << " partial=" << response.partial();
}
} // namespace

void PlayerAssetOpSystem::Debit(const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	Process(AssetOpRpc::kDebit, request, response);
}

void PlayerAssetOpSystem::AbortDebit(const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	Process(AssetOpRpc::kAbortDebit, request, response);
}

void PlayerAssetOpSystem::Credit(const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	Process(AssetOpRpc::kCredit, request, response);
}

void PlayerAssetOpSystem::SetPersistFnForTest(PersistFn fn)
{
	g_persistFn = (fn == nullptr) ? &PlayerLifecycleSystem::SavePlayerToRedis : fn;
}

void PlayerAssetOpSystem::SetAddCurrencyFnForTest(AddCurrencyFn fn)
{
	g_addCurrencyFn = (fn == nullptr) ? &CurrencySystem::AddCurrency : fn;
}

void PlayerAssetOpSystem::SetSecretLookupForTest(AssetOpSecretLookup fn)
{
	// nullptr = 恢复默认(读环境变量并缓存),与 VerifyAssetOpAuth 的约定一致。
	g_secretLookup = fn;
}

void PlayerAssetOpSystem::SetNowFnForTest(NowMsFn fn)
{
	g_nowMsFn = (fn == nullptr) ? &DefaultNowMs : fn;
}
