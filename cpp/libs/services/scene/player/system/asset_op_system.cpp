#include "player/system/asset_op_system.h"

#include <array>
#include <iterator>
#include <limits>
#include <string>
#include <string_view>
#include <unordered_set>
#include <algorithm>
#include <openssl/sha.h>

#include "muduo/base/Logging.h"

#include "battle/system/player_battle.h"
#include "engine/core/time/system/time.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/asset_op_ledger.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h" // HasFencedOwnership / IsPlayerMutable(与玩家收件箱共用,D12)
#include "player/system/player_pet.h"
#include "modules/transaction_log/transaction_log_system.h"
#include "table/code/petrule_table.h"
#include "table/code/pet_table.h"
#include "proto/common/component/player_pet_comp.pb.h"
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
	{ASSET_OP_STREAM_TRADE_DEBIT, AssetOpDirection::kDebit, 3,
	 {TX_AUCTION_SELL, TX_TRADE, TX_AUCTION_BUY}},
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

PlayerAssetOpSystem::PersistFn g_persistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
PlayerAssetOpSystem::AddCurrencyFn g_addCurrencyFn = &CurrencySystem::AddCurrency;
PlayerAssetOpSystem::NowMsFn g_nowMsFn = &DefaultNowMs;

// nullptr 的含义由 asset_op_auth.h 约定:走默认实现(读环境变量
// MMORPG_ASSET_OP_SECRET_<CALLER> 并缓存)。密钥值**不进日志、不进仓库**。
AssetOpSecretLookup g_secretLookup = nullptr;

constexpr size_t kInstanceBytesLimit = kAssetOpMaxInstanceBytes;
constexpr size_t kSnapshotBytesLimit = kAssetOpMaxSnapshotBytes;
constexpr size_t kPlayerReceiptBytesLimit = kAssetOpMaxPlayerReceiptBytes;

std::string ReceiptDigest(const AssetOpRequest& request)
{
	AssetOpRequest stable = request;
	stable.mutable_auth()->set_timestamp_ms(0);
	stable.mutable_auth()->clear_signature_hex();
	const auto canonical = AssetOpCanonical("debit", stable);
	unsigned char digest[SHA256_DIGEST_LENGTH];
	SHA256(reinterpret_cast<const unsigned char*>(canonical.data()), canonical.size(), digest);
	return {reinterpret_cast<const char*>(digest), sizeof(digest)};
}

const AssetOpReceipt* FindReceipt(const AssetOpStreamLedger& ledger, uint64_t seq)
{
	for (const auto& receipt : ledger.receipts()) if (receipt.seq() == seq) return &receipt;
	return nullptr;
}

void StoreReceipt(PlayerAssetOpLedgerComp& comp, const AssetOpRequest& request, const AssetSnapshot& snapshot)
{
	auto& ledger = MutableAssetOpStream(comp, request.stream());
	AssetOpReceipt* receipt = nullptr;
	for (auto& existing : *ledger.mutable_receipts())
	{
		if (existing.seq() == request.seq()) { receipt = &existing; break; }
	}
	if (receipt == nullptr) receipt = ledger.add_receipts();
	receipt->set_seq(request.seq());
	receipt->set_correlation_id(request.correlation_id());
	receipt->set_request_digest(ReceiptDigest(request));
	if (snapshot.items_size() != 0 || snapshot.has_pet()) *receipt->mutable_snapshot() = snapshot;
	else receipt->clear_snapshot();
	std::sort(ledger.mutable_receipts()->begin(), ledger.mutable_receipts()->end(),
		[](const auto& lhs, const auto& rhs) { return lhs.seq() < rhs.seq(); });
}

bool ReceiptMatches(const AssetOpReceipt* receipt, const AssetOpRequest& request)
{
	return receipt != nullptr && receipt->correlation_id() == request.correlation_id() &&
		receipt->request_digest() == ReceiptDigest(request);
}

bool HasReceiptRoom(const PlayerAssetOpLedgerComp& comp, const AssetOpRequest& request, const AssetSnapshot& snapshot)
{
	if (snapshot.ByteSizeLong() > kSnapshotBytesLimit) return false;
	// 预算按本次滑窗/换纪元后的状态核算，不能被即将剪掉的旧原件永久堵住。
	// 资产 RPC 非 tick 热路径；有界复制同时避免预检修改真实账本。
	auto prospective = comp;
	auto& ledger = MutableAssetOpStream(prospective, request.stream());
	if (!RecordAssetOpOutcome(ledger, request.stream_epoch(), request.seq(), AssetOpRecordKind::kApplied, 0)) return false;
	StoreReceipt(prospective, request, snapshot);
	size_t size = 0;
	for (const auto& stream : prospective.streams()) for (const auto& receipt : stream.receipts()) size += receipt.snapshot().ByteSizeLong();
	return size <= kPlayerReceiptBytesLimit;
}

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

// HasFencedOwnership / IsPlayerMutable 已原样抽到 player/system/player_mutability.h(D12,与玩家收件箱共用,
// 行为零变化)。判定依据、"为什么刻意不复用 IsCrossZoneFrozen"的论证都在那个头文件里。
//
// IsPlayerMutable 在本文件的调用点只有 AnswerSeenSeq 的补存。Decide 第 5 步的改动闸**不**调用它,而是逐条
// 内联同样的判定(为了按原因回不同的 reason:kAssetFrozen / kAssetPlayerNotHere);新增可改判定时,
// player_mutability.h 与 Decide 第 5 步两处要同改。
using player_mutability::HasFencedOwnership;
using player_mutability::IsPlayerMutable;

// ── durable 判定(§4.6)─────────────────────────────────────────────────────

// 只看"确实落盘的字节":PlayerLastPersistedSnapshotComp.snapshot 是
// HandlePlayerAsyncSaved 回调里写入的、刚写进 Redis 的整份 PlayerAllData。
// 某 seq 的结局出现在这份快照的账本里 = durable。不另建"已持久化水位",不存在两份真相。
bool IsAssetOpDurable(entt::entity player, const AssetOpRequest& request, bool expectApplied)
{
	const auto* snapshotComp = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
	if (snapshotComp == nullptr || !snapshotComp->HasSnapshot())
	{
		return false;
	}

	const auto& persisted = snapshotComp->snapshot->player_database_data().asset_op_ledger();
	const auto* ledger = FindAssetOpStream(persisted, request.stream());
	if (ledger == nullptr || ledger->stream_epoch() != request.stream_epoch() ||
		!ReceiptMatches(FindReceipt(*ledger, request.seq()), request))
	{
		return false;
	}

	const auto state = ClassifyAssetOpSeq(ledger, request.stream_epoch(), request.seq());
	if (state == AssetOpSeqState::kApplied || state == AssetOpSeqState::kRejected)
	{
		const bool appliedOnDisk = (state == AssetOpSeqState::kApplied);
		if (appliedOnDisk != expectApplied)
		{
			// 盘上的结局与内存里的结局不同:结局固定(I2)被破坏,只可能是 bug。
			// 不敢报 durable,让 Go 继续重查并告警。
			LOG_ERROR << "[AssetOp] Snapshot outcome differs from in-memory outcome stream=" << static_cast<int>(request.stream())
					  << " epoch=" << request.stream_epoch() << " seq=" << request.seq()
					  << " on_disk=" << AssetOpSeqStateName(state)
					  << " expect_applied=" << expectApplied;
			return false;
		}
		return true;
	}
	return false;
}

// 触发一次存盘并记下时刻。返回 SavePlayerToRedis 的原值:
// true = 确实压了一次写、稍后有落盘回调;false 仅表示没有压入新写,
// 包括与旧快照相等、交接已发起、缺身份/持久组件拒存。不能从 bool 推断具体原因或 durable。
// **前置**:IsPlayerMutable(player) 为真;durable 始终由真实落盘快照单独判定。
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
		IsAssetOpDurable(player, request, expectApplied);
	if (!wrote && !durable)
	{
		// bool facade 分不出相等跳过、交接跳过和拒存。这里只报告未排入新写且缺少落盘证据,
		// 如实保留 durable=false 让 Go 重查;具体拒存原因由保存入口日志和累计计数说明。
		LOG_ERROR << "[AssetOp] No save queued and outcome is absent from persisted snapshot player_id=" << request.player_id()
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
	const auto* receipt = FindReceipt(ledger, request.seq());
	// 旧位图没有原请求身份，不能替新请求背书；也不能清账本后重新扣发。
	if (!ReceiptMatches(receipt, request))
	{ Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle); return; }
	if (receipt->has_snapshot()) *response.mutable_snapshot() = receipt->snapshot();
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
		IsAssetOpDurable(player, request, applied);
	if (IsPlayerMutable(player))
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
//
// **先用只读指针复核前置,再 MutableAssetOpStream 建流**:反过来的话,前置不成立时
// 账本里会留下一条 stream_epoch = 0 的空流(asset_op_ledger.cpp:227-246 建流时写 0,
// 真实纪元靠 RecordAssetOpOutcome 内部的纪元重置补;而它前置失败时 :296-303 直接
// return false,什么也不写)。那条空流一旦随存盘落地,下次加载 ValidateAssetOpLedger
// 就因 "stream_epoch = 0" 判整本账本损坏,该玩家的资产通道被永久 fail-closed ——
// 一次瞬时 bug 放大成持久锁死。建流的这条调用约定写在 asset_op_ledger.h:66-68。
bool RecordOutcome(PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
				   AssetOpRecordKind kind, uint32_t reasonTipId)
{
	const auto* existing = FindAssetOpStream(ledgerComp, request.stream());
	const auto state = ClassifyAssetOpSeq(existing, request.stream_epoch(), request.seq());
	if (state != AssetOpSeqState::kUnseen && state != AssetOpSeqState::kAheadOfWindow)
	{
		return false;
	}

	auto& ledger = MutableAssetOpStream(ledgerComp, request.stream());
	if (!RecordAssetOpOutcome(ledger, request.stream_epoch(), request.seq(), kind, reasonTipId)) return false;
	StoreReceipt(ledgerComp, request, AssetSnapshot{});
	return true;
}

// 记一次拒绝并答复。reasonTipId = 0 是"中止占位"。
void RejectAndRecord(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp,
					 const ::AssetOpRequest& request, uint32_t reasonTipId, int64_t nowMs,
					 ::AssetOpResponse& response)
{
	if (!RecordOutcome(ledgerComp, request, AssetOpRecordKind::kRejected, reasonTipId))
	{
		LOG_ERROR << "[AssetOp] Ledger precondition violated(rejected) player_id=" << request.player_id()
				  << " stream=" << static_cast<int>(request.stream()) << " epoch=" << request.stream_epoch()
				  << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, 0);
		return;
	}
	Answer(response, ASSET_OP_OUTCOME_REJECTED, reasonTipId);
	response.set_durable(PersistAndProbeDurable(player, request, /*expectApplied=*/false, nowMs));
}

// ── 尚未支持的 P2 字段(§4.9 第 8 步:确定性失败,要记账)────────────────────

// 聚宝斋 P2 的 guid 扣物 / 宝宝字段(AssetBundle.item_uuids / pet_id):v1 的 scene
// **尚未实现**按 guid 扣装备 / 扣宝宝(那是 P3 托管的活),所以带这两个字段的包一律拒。
// 没实现就必须显式拒绝,不能默默忽略(AGENTS §11.3 不得静默降级)。P3 实现这两条时
// 在本函数与 ApplyDebit / ApplyCredit 里成对放开。
//
// **2026-09-19 起这是记账式拒绝**(§4.9 第 8 步),不再是信封档的"忽略本次请求"。
// 当初之所以要放进信封档,是因为这两个字段没进签名串:攻击者能在一条**合法签名**的
// 请求上追加 pet_id=1 重发,把该 (纪元, seq) 永久钉成 REJECTED(I2 结局固定),
// 帮会随后重投的真实捐献再也扣不成 —— 那等于"谁都能杀掉任意一条在途 seq"。
// 现在 canonical 已扩成 `…;u=…;p=…`(asset_op_auth.cpp 的 AppendBundleCanonical 与
// Go assetop.writeBundleCanonical 同批改,两边 golden 已更新),改任一字段签名即失效,
// 追加攻击不再存在,于是按规格记成终局 REJECTED 才是对的:Go 那边能当场终结并退回业务,
// 不必让 outbox 行永远 PENDING 重投。
//
// P3 实现按 guid 扣物之后,删掉这个函数与两处调用,改为真正的包内容校验。
bool HasUnsupportedP2Fields(const ::AssetBundle& bundle)
{
	return bundle.item_uuids_size() > 8 || bundle.item_instances_size() > 8;
}

// ── 包内容校验(§4.9 第 8 步:确定性失败,要记账)──────────────────────────

// 只判金额范围。币种是否认识不在这里判:不认识的币种是版本错位,不是确定性失败,见第 8b 步。
bool IsCurrencyAmountValid(const ::CurrencyAmount& currency)
{
	return currency.amount() >= 1 && currency.amount() <= static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
}

// 包里第一个本二进制不认识的币种;都认识时返回 nullptr。
const ::CurrencyAmount* FindCurrencyUnknownToThisVersion(const ::AssetBundle& bundle)
{
	for (const auto& currency : bundle.currencies())
		if (!IsKnownCurrencyType(currency.currency_type())) return &currency;
	return nullptr;
}

bool ValidateDebitBundle(const ::AssetBundle& bundle, std::string& why)
{
	if (HasUnsupportedP2Fields(bundle))
	{
		why = "debit v1 does not support item/pet deductions by guid(item_uuids / pet_id)";
		return false;
	}
	if (bundle.currencies_size() > 4 || bundle.items_size() > 16 || bundle.item_instances_size() != 0 || bundle.has_pet_instance())
	{
		why = "debit entry limit or credit-only payload";
		return false;
	}
	if (bundle.currencies_size() + bundle.items_size() + bundle.item_uuids_size() == 0 && bundle.pet_id() == 0)
	{
		why = "debit bundle is empty";
		return false;
	}
	std::unordered_set<uint32_t> types, configs;
	std::unordered_set<uint64_t> guids;
	for (const auto& currency : bundle.currencies())
		if (!IsCurrencyAmountValid(currency) || !types.insert(currency.currency_type()).second) { why = "invalid debit currency"; return false; }
	for (const auto& item : bundle.items())
	{
		const auto* row = ItemTableManager::Instance().FindByIdSilent(item.config_id()).first;
		if (item.count() == 0 || item.count() > 4096 || row == nullptr || row->max_stack_size() <= 1 || !configs.insert(item.config_id()).second)
		{ why = "quantity debit requires distinct stackable configurations"; return false; }
	}
	for (auto guid : bundle.item_uuids()) if (guid == 0 || !guids.insert(guid).second) { why = "duplicate or zero instance"; return false; }
	return true;
}

bool ValidateCreditBundle(const ::AssetBundle& bundle, std::string& why)
{
	if (HasUnsupportedP2Fields(bundle) || bundle.item_uuids_size() > 0 || bundle.pet_id() != 0)
	{
		why = "credit v1 does not support item/pet grants by guid(item_uuids / pet_id)";
		return false;
	}
	if (bundle.currencies_size() + bundle.items_size() + bundle.item_instances_size() < 1 && !bundle.has_pet_instance())
	{
		why = "credit bundle is empty";
		return false;
	}
	if (bundle.currencies_size() > 4 || bundle.items_size() > 16)
	{
		why = "credit entry limit exceeded(4 currencies / 16 items)";
		return false;
	}

	std::unordered_set<uint32_t> seenCurrencyTypes;
	for (const auto& currency : bundle.currencies())
	{
		if (!IsCurrencyAmountValid(currency))
		{
			why = "credit currency type or amount out of range";
			return false;
		}
		if (!seenCurrencyTypes.insert(currency.currency_type()).second)
		{
			why = "credit contains duplicate currency type";
			return false;
		}
	}

	std::unordered_set<uint32_t> seenConfigIds;
	uint64_t totalGrants = 0;
	for (const auto& item : bundle.items())
	{
		totalGrants += item.count();
		if (item.count() < 1 || totalGrants > 4096)
		{
			why = "credit item count is 0";
			return false;
		}
		if (!seenConfigIds.insert(item.config_id()).second)
		{
			why = "credit contains duplicate item config_id";
			return false;
		}
		if (ItemTableManager::Instance().FindByIdSilent(item.config_id()).first == nullptr)
		{
			why = "credit item config_id is absent from Item table";
			return false;
		}
	}
	std::unordered_set<uint64_t> identities;
	for (const auto& transfer : bundle.item_instances())
	{
		ItemComp item;
		const auto* row = ItemTableManager::Instance().FindByIdSilent(transfer.config_id()).first;
		if (transfer.instance_proto().size() > kInstanceBytesLimit || !item.ParseFromString(transfer.instance_proto()) ||
			item.item_id() == 0 || item.item_id() != transfer.item_uuid() || item.config_id() != transfer.config_id() ||
			item.size() != 1 || transfer.stack_size() != 1 || row == nullptr || row->max_stack_size() != 1 || !identities.insert(item.item_id()).second)
		{ why = "invalid complete item instance"; return false; }
	}
	if (bundle.has_pet_instance())
	{
		const auto& transfer = bundle.pet_instance();
		PetInstance pet;
		if (transfer.instance_proto().size() > kInstanceBytesLimit || !pet.ParseFromString(transfer.instance_proto()) ||
			pet.pet_id() == 0 || pet.pet_id() != transfer.pet_id() || pet.pet_table_id() != transfer.pet_table_id() ||
			pet.level() != transfer.level() || pet.name() != transfer.name() || PetTableManager::Instance().FindByIdSilent(pet.pet_table_id()).first == nullptr)
		{ why = "invalid complete pet instance"; return false; }
	}
	return true;
}

// ── 应用段 ──────────────────────────────────────────────────────────────────

void ApplyCurrencyDebit(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
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
		LOG_ERROR << "[AssetOp] DeductCurrency still failed after admission check player_id=" << request.player_id()
				  << " seq=" << request.seq() << " currency_type=" << currency.currency_type()
				  << " amount=" << currency.amount() << " err=" << error;
		Answer(response, ASSET_OP_OUTCOME_RETRY, error);
		return;
	}

	if (!RecordOutcome(ledgerComp, request, AssetOpRecordKind::kApplied, 0))
	{
		// 钱已经扣了却记不上账:重投会再扣一次。只能大声报错,交人工按流水
		// (correlation_id)对账补偿。正常路径不可达。
		LOG_ERROR << "[AssetOp] Deduction applied but ledger precondition violated player_id=" << request.player_id()
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
void ApplyDebit(entt::entity player, PlayerAssetOpLedgerComp& ledgerComp, const ::AssetOpRequest& request,
	int64_t nowMs, ::AssetOpResponse& response)
{
	const auto& bundle = request.bundle();
	if (bundle.currencies_size() == 1 && bundle.items_size() == 0 && bundle.item_uuids_size() == 0 && bundle.pet_id() == 0)
	{
		ApplyCurrencyDebit(player, ledgerComp, request, nowMs, response);
		return;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	const auto txType = static_cast<TransactionType>(request.tx_type());
	ItemCountMap quantities;
	for (const auto& item : bundle.items()) quantities[item.config_id()] = item.count();
	std::vector<Guid> guids(bundle.item_uuids().begin(), bundle.item_uuids().end());
	AssetSnapshot snapshot;
	for (const auto& currency : bundle.currencies())
	{
		if (!CurrencySystem::CanAfford(player, static_cast<CurrencyType>(currency.currency_type()), static_cast<int64_t>(currency.amount())))
		{ RejectAndRecord(player, ledgerComp, request, kAssetCurrencyInsufficient, nowMs, response); return; }
	}
	if (!quantities.empty() || !guids.empty())
	{
		if (bags == nullptr) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere); return; }
		auto& bag = bags->bags[kInventory];
		// 按数量扣出只认可互换的普通单位(ItemStore::IsCountableByConfig)。只有算上带更新版本字段的单位才够,
		// 是版本错位:回 RETRY、不记账,等玩家落到认识这些字段的节点再扣;记成终态会让对侧白白退款或中止,
		// 当成普通单位扣走则会把带新属性(例如绑定)的物品交易出去(混版本设计 §4.4 第 6 批)。
		switch (bag.CheckQuantities(quantities))
		{
		case ItemStore::QuantityVerdict::kEnough:
			break;
		case ItemStore::QuantityVerdict::kVersionSkew:
			LOG_WARN << "[AssetOp] metric=persist_version_skew where=asset_debit player_id=" << request.player_id()
					 << " seq=" << request.seq() << " correlation_id=" << request.correlation_id()
					 << "; items carrying newer-version fields are not debited by quantity, answering RETRY";
			Answer(response, ASSET_OP_OUTCOME_RETRY, kFeatureUnavailable);
			return;
		case ItemStore::QuantityVerdict::kInsufficient:
			RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response);
			return;
		}
		if (!guids.empty() && bag.ReserveForBatchRemove(guids) != kSuccess)
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		for (auto guid : guids)
		{
			const auto* item = bag.GetItemCompByGuid(guid);
			if (item == nullptr || item->ByteSizeLong() > kInstanceBytesLimit)
			{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
			auto* transfer = snapshot.add_items();
			transfer->set_item_uuid(guid); transfer->set_config_id(item->config_id()); transfer->set_stack_size(item->size());
			transfer->set_instance_proto(item->SerializeAsString());
		}
	}
	if (bundle.pet_id() != 0)
	{
		const auto* pets = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
		if (pets == nullptr) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere); return; }
		if (pets->active_pet_id() == bundle.pet_id())
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		for (const auto& pet : pets->pets()) if (pet.pet_id() == bundle.pet_id())
		{
			if (pet.ByteSizeLong() > kInstanceBytesLimit)
			{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
			auto* transfer = snapshot.mutable_pet();
			transfer->set_pet_id(pet.pet_id()); transfer->set_pet_table_id(pet.pet_table_id());
			transfer->set_name(pet.name()); transfer->set_level(pet.level()); transfer->set_instance_proto(pet.SerializeAsString());
		}
		if (!snapshot.has_pet()) { RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
	}
	if (!HasReceiptRoom(ledgerComp, request, snapshot)) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull); return; }
	// 所有可预知失败均在副作用前复核。提交阶段若内部不变量损坏，记 partial 阻止对侧入账。
	bool mutated = false;
	uint32_t error = kSuccess;
	if (!quantities.empty())
	{
		error = BagService::RemoveItemsExact(player, bags->bags[kInventory], quantities, txType, request.correlation_id());
		mutated = error == kSuccess;
	}
	if (error == kSuccess && !guids.empty())
	{
		bool removed = false;
		error = BagService::RemoveItemsByGuid(player, bags->bags[kInventory], guids, txType, request.correlation_id(), nullptr, &removed);
		mutated = mutated || removed;
	}
	if (error == kSuccess && bundle.pet_id() != 0)
	{
		PetInstance removed;
		error = PetSystem::RemovePetForTrade(player, bundle.pet_id(), removed);
		mutated = mutated || error == kSuccess;
	}
	if (error == kSuccess) for (const auto& currency : bundle.currencies())
	{
		error = CurrencySystem::DeductCurrency(player, static_cast<CurrencyType>(currency.currency_type()),
			static_cast<int64_t>(currency.amount()), txType, request.correlation_id());
		if (error != kSuccess) break;
		mutated = true;
	}
	if (error != kSuccess && !mutated) { Answer(response, ASSET_OP_OUTCOME_RETRY, error); return; }
	const bool partial = error != kSuccess;
	if (!RecordOutcome(ledgerComp, request, partial ? AssetOpRecordKind::kAppliedPartial : AssetOpRecordKind::kApplied, 0))
	{ Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle); return; }
	StoreReceipt(ledgerComp, request, snapshot);
	*response.mutable_snapshot() = snapshot;
	Answer(response, ASSET_OP_OUTCOME_APPLIED, partial ? kAssetPartialApplied : 0);
	response.set_partial(partial);
	response.set_durable(PersistAndProbeDurable(player, request, true, nowMs));
}

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
			LOG_WARN << "[AssetOp] Insufficient item guid segment capacity;grant deferred player_id=" << request.player_id()
					 << " seq=" << request.seq() << " needed=" << guidsNeeded << " available=" << available;
			Answer(response, ASSET_OP_OUTCOME_RETRY, 0);
			return;
		}
	}

	std::vector<ItemComp> transferredItems;
	PetInstance transferredPet;
	auto* destinationBags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	ItemCountMap totalIncoming = itemsToAdd;
	for (const auto& transfer : bundle.item_instances())
	{
		ItemComp item;
		if (!item.ParseFromString(transfer.instance_proto()))
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, item.config_id()))
		{ RejectAndRecord(player, ledgerComp, request, kAssetBlocked, nowMs, response); return; }
		// 名下已有这个实例身份就拒:固定包、动态包,以及携带着的本版本不认识的包类型记录都要查
		// (PlayerBagsComp::ContainsGuid,混版本设计 §12 第 5 批)。
		if (destinationBags->ContainsGuid(item.item_id()))
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		++totalIncoming[item.config_id()];
		item.clear_acquire_seq();
		transferredItems.push_back(std::move(item));
	}
	if (!totalIncoming.empty() && destinationBags->bags[kInventory].CheckSpaceFor(totalIncoming) != kSuccess)
	{ Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull); return; }
	for (const auto& item : itemsToAdd) if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, item.first))
	{ RejectAndRecord(player, ledgerComp, request, kAssetBlocked, nowMs, response); return; }
	if (bundle.has_pet_instance())
	{
		if (!transferredPet.ParseFromString(bundle.pet_instance().instance_proto()))
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		const auto* pets = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
		if (pets == nullptr) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere); return; }
		const auto& rows = PetRuleTableManager::Instance().FindAll().data();
		const auto maxPets = rows.size() > 0 && rows.Get(0).max_pets() > 0 ? rows.Get(0).max_pets() : 1;
		if (static_cast<uint32_t>(pets->pets_size()) >= maxPets) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull); return; }
		for (const auto& pet : pets->pets()) if (pet.pet_id() == transferredPet.pet_id())
		{ RejectAndRecord(player, ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
		// 完整实例来自经签名的托管快照；配表调低上限不应改写已有资产。
		// 是否可出战由宠物系统按当前主人等级与配置另行校验。
	}

	bool anyMutated = false;
	uint32_t failedError = kSuccess;
	std::string failedWhat;

	// (c) 物品
	if (!itemsToAdd.empty())
	{
		auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player);
		// BagService 候选提交的任何拒绝都保持物品批次零改动。
		const uint32_t error = BagService::AddItems(
			player, bags.bags[kInventory], itemsToAdd, txType, request.correlation_id());
		if (error != kSuccess)
		{
			if (error == kBagItemNotStacked)
			{
				Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull);
			}
			else if (error == kInvalidParameter)
			{
				RejectAndRecord(player, ledgerComp, request, kAssetBlocked, nowMs, response);
			}
			else
			{
				Answer(response, ASSET_OP_OUTCOME_RETRY, error);
			}
			return;
		}
		anyMutated = true;
	}

	if (failedError == kSuccess) for (auto& item : transferredItems)
	{
		const auto guid = item.item_id();
		const auto config = item.config_id();
		const auto error = destinationBags->bags[kInventory].PutInstance(std::move(item));
		if (error != kSuccess) { failedError = error; failedWhat = "item_instance"; break; }
		TransactionLogSystem::LogItemCreate(player, guid, config, 1, txType, request.correlation_id());
		anyMutated = true;
	}
	if (failedError == kSuccess && bundle.has_pet_instance())
	{
		const auto error = PetSystem::RestorePetFromSnapshot(player, transferredPet);
		if (error != kSuccess) { failedError = error; failedWhat = "pet_instance"; }
		else anyMutated = true;
	}
	if (failedError != kSuccess && !anyMutated)
	{ Answer(response, ASSET_OP_OUTCOME_RETRY, failedError); return; }

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
		LOG_ERROR << "[AssetOp] Grant applied but ledger precondition violated player_id=" << request.player_id()
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
		LOG_WARN << "[AssetOp] Invalid envelope rpc=" << RpcName(rpc) << " player_id=" << request.player_id()
				 << " seq=" << request.seq() << " epoch=" << request.stream_epoch();
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
		return;
	}
	const auto* rule = FindStreamRule(request.stream());
	if (rule == nullptr)
	{
		LOG_WARN << "[AssetOp] Unknown stream rpc=" << RpcName(rpc) << " stream=" << static_cast<int>(request.stream());
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
			LOG_WARN << "[AssetOp] Stream direction or tx allowlist mismatch rpc=" << RpcName(rpc)
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
		LOG_WARN << "[AssetOp] Signature verification failed rpc=" << RpcName(rpc)
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
		LOG_ERROR << "[AssetOp] Ledger corrupted;all asset operations rejected player_id=" << request.player_id()
				  << " rpc=" << RpcName(rpc) << " seq=" << request.seq();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBlocked);
		return;
	}

	// 账本组件由 PlayerDatabaseMessageFieldsUnmarshal 无条件挂上(§4.5)。缺了就是加载路径坏了:
	// **绝不** get_or_emplace 一本空账本 —— 那会把已经应用过的 seq 重新看成未见,直接二次扣款。
	auto* ledgerComp = tlsEcs.actorRegistry.try_get<PlayerAssetOpLedgerComp>(player);
	if (ledgerComp == nullptr)
	{
		LOG_ERROR << "[AssetOp] Entity lacks ledger component(load incomplete or missing wiring) player_id="
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
		LOG_ERROR << "[AssetOp] Untrusted seq state=" << AssetOpSeqStateName(state)
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
			LOG_ERROR << "[AssetOp] Classified as seen but stream does not exist player_id=" << request.player_id()
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
		LOG_ERROR << "[AssetOp] Unknown seq state state=" << static_cast<int>(state);
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
	if (!HasFencedOwnership(player))
	{
		// 归属未铸造(owner_epoch == 0 或组件缺失):本实体的存盘没有围栏,记下的结局可能被旧节点晚到的
		// 退出存盘抹掉。放在第 6 步之前 —— 中止占位的 REJECTED 也是一笔账本改动,同样会被抹掉。
		// reason 用 kAssetFrozen("角色迁移中,稍后自动重试"):对玩家就是"换一次节点就好";
		// 不用 kAssetBlocked,那是货币封禁的文案。区分靠下面这行日志的固定关键字(告警按它计数,
		// 08-save-owner-fence.md §8.2.4)。WARN 而非 ERROR:兼容窗口内这是预期状态,该响的是它在共享环境出现。
		const auto* ownerEpoch = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
		LOG_WARN << "[AssetOp] blocked: owner_epoch unknown rpc=" << RpcName(rpc)
				 << " player_id=" << request.player_id() << " stream=" << static_cast<int>(request.stream())
				 << " seq=" << request.seq()
				 << " epoch_comp=" << (ownerEpoch == nullptr ? "missing" : "zero");
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetFrozen);
		return;
	}

	// APPLIED、REJECTED 和中止都须保存窗口内身份；预算拒绝必须先于任何资产改动。
	if (!HasReceiptRoom(*ledgerComp, request, AssetSnapshot{}))
	{ Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetBagFull); return; }

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
	if (!tlsEcs.actorRegistry.all_of<CurrencyComp, PlayerCurrencyComp>(player))
	{
		// 两份数据都由加载恢复。尤其 Credit 先发物品再加币,缺债务镜像必须在物品副作用前拦住,
		// 不能由后面的 AddCurrency 拒绝才发现,否则这笔资产指令已经部分应用。
		LOG_ERROR << "[AssetOp] Asset application refused: persisted currency component missing (load invariant broken) player_id="
				  << request.player_id()
				  << " has_currency=" << tlsEcs.actorRegistry.all_of<CurrencyComp>(player)
				  << " has_player_currency=" << tlsEcs.actorRegistry.all_of<PlayerCurrencyComp>(player);
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
		return;
	}
	// 帮会捐献保留单币种契约；物品与多币种托管只由交易流承载。
	// 这是纯包校验，必须先于背包加载依赖，避免坏请求被当作暂不可用反复重投。
	if (rpc == AssetOpRpc::kDebit && request.stream() == ASSET_OP_STREAM_GUILD_DEBIT &&
		(request.bundle().currencies_size() != 1 || request.bundle().items_size() != 0 ||
		 request.bundle().item_uuids_size() != 0 || request.bundle().item_instances_size() != 0 ||
		 request.bundle().pet_id() != 0 || request.bundle().has_pet_instance()))
	{
		RejectAndRecord(player, *ledgerComp, request, kAssetInvalidBundle, nowMs, response);
		return;
	}
	// 8. 包内容校验(确定性失败 → 记 REJECTED,免得 Go 无限重投同一个坏包)
	if (request.stream() != ASSET_OP_STREAM_TRADE_DEBIT && request.stream() != ASSET_OP_STREAM_TRADE_CREDIT &&
		(request.bundle().item_uuids_size() > 0 || request.bundle().item_instances_size() > 0 || request.bundle().pet_id() != 0 || request.bundle().has_pet_instance()))
	{ RejectAndRecord(player, *ledgerComp, request, kAssetInvalidBundle, nowMs, response); return; }
	std::string why;
	const bool bundleOk = (rpc == AssetOpRpc::kDebit) ? ValidateDebitBundle(request.bundle(), why)
													  : ValidateCreditBundle(request.bundle(), why);
	if (!bundleOk)
	{
		LOG_WARN << "[AssetOp] Invalid payload rpc=" << RpcName(rpc) << " player_id=" << request.player_id()
				 << " seq=" << request.seq() << " why=" << why;
		RejectAndRecord(player, *ledgerComp, request, kAssetInvalidBundle, nowMs, response);
		return;
	}
	// 8b. 本二进制不认识的币种:版本错位,回 RETRY、不记账、不存盘。Go 各入口已按同一张取值表
	// (proto/common/base/currency_type.proto)拒绝写错的值,这里只可能是更新版本加的币种;记成终态
	// 会让对侧白白退款或中止,等玩家落到认识它的节点再应用(混版本设计 §12)。
	// 排在中止(第 6 步)与所有确定性包校验之后:截止时刻照常能关门,与版本无关的坏包仍先记终态;
	// 排在任何改动之前:ApplyDebit / ApplyCredit 从此见不到不认识的币种。
	if (const auto* unknown = FindCurrencyUnknownToThisVersion(request.bundle()); unknown != nullptr)
	{
		LOG_WARN << "[AssetOp] metric=persist_version_skew where=asset_currency rpc=" << RpcName(rpc)
				 << " player_id=" << request.player_id() << " stream=" << static_cast<int>(request.stream())
				 << " seq=" << request.seq() << " currency_type=" << unknown->currency_type()
				 << "; currency unknown to this version, answering RETRY";
		Answer(response, ASSET_OP_OUTCOME_RETRY, kFeatureUnavailable);
		return;
	}
	// 坏包必须先记终局拒绝；合法交易托管的 Debit 与 Credit 都可能需要背包，不能仅保留旧单币种 Debit 的假设。
	if ((request.bundle().items_size() > 0 || request.bundle().item_uuids_size() > 0 || request.bundle().item_instances_size() > 0) && tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player) == nullptr)
	{
		LOG_WARN << "[AssetOp] Player lacks PlayerBagsComp(still loading) player_id=" << request.player_id();
		Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere);
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

void PlayerAssetOpSystem::AckReceipt(const ::AssetOpRequest& request, ::AssetOpResponse& response)
{
	response.Clear();
	const auto nowMs = g_nowMsFn();
	if (request.player_id() == 0 || request.seq() == 0 || request.stream_epoch() == 0 || request.correlation_id() == 0 ||
		request.auth().caller() != "trade" || VerifyAssetOpAuth("ack_receipt", request, nowMs, g_secretLookup) != AssetOpAuthVerdict::kOk)
	{ Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetAuthFailed); return; }
	const auto it = tlsEcs.playerList.find(request.player_id());
	if (it == tlsEcs.playerList.end() || !tlsEcs.actorRegistry.valid(it->second))
	{ Answer(response, ASSET_OP_OUTCOME_NOT_HERE, kAssetPlayerNotHere); return; }
	const auto player = it->second;
	if (!IsPlayerMutable(player) || tlsEcs.actorRegistry.any_of<PlayerAssetOpLedgerInvalidComp>(player))
	{ Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetFrozen); return; }
	auto* comp = tlsEcs.actorRegistry.try_get<PlayerAssetOpLedgerComp>(player);
	if (comp == nullptr) { Answer(response, ASSET_OP_OUTCOME_RETRY, kAssetPlayerNotHere); return; }
	for (auto& stream : *comp->mutable_streams())
	{
		if (stream.stream() != request.stream() || stream.stream_epoch() != request.stream_epoch()) continue;
		if (request.seq() <= stream.watermark())
		{
			// 仅清理已在业务 SQL 保存原件的待办；不能据此恢复原资产结局。
			Answer(response, ASSET_OP_OUTCOME_APPLIED, 0);
			return;
		}
		for (auto& receipt : *stream.mutable_receipts()) if (receipt.seq() == request.seq())
		{
			if (!ReceiptMatches(&receipt, request) ||
				ClassifyAssetOpSeq(&stream, request.stream_epoch(), request.seq()) != AssetOpSeqState::kApplied ||
				IsAssetOpPartial(stream, request.seq()))
			{ Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle); return; }
			// 原件已入交易 SQL 才能 ACK；释放大快照，保留窗口内请求身份。
			const bool changed = receipt.has_snapshot();
			receipt.clear_snapshot();
			const auto* last = tlsEcs.actorRegistry.try_get<PlayerAssetOpPersistRequestComp>(player);
			if (changed || last == nullptr || nowMs - last->lastRequestMs >= kAssetOpResaveMinIntervalMs)
				RequestAssetOpPersist(player, nowMs);
			Answer(response, ASSET_OP_OUTCOME_APPLIED, 0);
			const auto* persisted = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
			if (persisted != nullptr && persisted->HasSnapshot())
			{
				const auto* saved = FindAssetOpStream(persisted->snapshot->player_database_data().asset_op_ledger(), request.stream());
				const auto* savedReceipt = saved == nullptr ? nullptr : FindReceipt(*saved, request.seq());
				response.set_durable(saved != nullptr && saved->stream_epoch() == request.stream_epoch() &&
					ReceiptMatches(savedReceipt, request) && !savedReceipt->has_snapshot());
			}
			return;
		}
		// 旧位图无摘要不等于完成释放，交由业务待办保持 UNKNOWN。
		Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
		return;
	}
	Answer(response, ASSET_OP_OUTCOME_UNKNOWN, kAssetInvalidBundle);
}

void PlayerAssetOpSystem::SetPersistFnForTest(PersistFn fn)
{
	g_persistFn = (fn == nullptr) ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
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
