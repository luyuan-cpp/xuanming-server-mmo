#include "client_request_ledger.h"
#include "player_equipment_material_reward.h"

#include <unordered_set>
#include <utility>

#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>
#include <google/protobuf/message_lite.h>
#include <muduo/base/Logging.h>
#include <openssl/sha.h>

#include "thread_context/ecs_context.h"

static_assert(kClientRequestFingerprintBytes <= SHA256_DIGEST_LENGTH, "the fingerprint is a prefix of a SHA-256 digest");

std::string ClientRequestFingerprintDigest(const google::protobuf::MessageLite &requestWithoutKey)
{
	std::string bytes;
	bool serialized = false;
	{
		google::protobuf::io::StringOutputStream stream(&bytes);
		google::protobuf::io::CodedOutputStream coded(&stream);
		// 确定性序列化(map 字段按键排序):同一份请求两次序列化逐字节相同,重发才认得出是同一个请求。
		coded.SetSerializationDeterministic(true);
		serialized = requestWithoutKey.SerializeToCodedStream(&coded) && !coded.HadError();
	} // CodedOutputStream 析构时才把缓冲刷进 bytes,所以要等这个块结束
	if (!serialized)
	{
		// proto3 没有必填字段、写内存不会失败,正常走不到。真走到了,指纹可能不完整,只打日志不拦请求:
		// 后果最多是把两个不同的请求当成同一个,而这要求它们还用了同一个键。
		// 包一层 std::string:新版 protobuf 的 GetTypeName 返回 string_view 一类,muduo 的 LogStream 没有对应重载(同 codec.cpp)。
		LOG_ERROR << "[ClientRequest] fingerprint serialization failed: type=" << std::string(requestWithoutKey.GetTypeName());
	}
	unsigned char digest[SHA256_DIGEST_LENGTH];
	SHA256(reinterpret_cast<const unsigned char *>(bytes.data()), bytes.size(), digest);
	return std::string(reinterpret_cast<const char *>(digest), kClientRequestFingerprintBytes);
}

namespace client_request_ledger
{
namespace
{
// 调用方保证 key != 0。
const ClientRequestRecord *FindRecord(const ClientRequestLedgerComp &ledger, uint64_t key)
{
	for (const auto &record : ledger.records())
	{
		if (record.key() == key)
		{
			return &record;
		}
	}
	return nullptr;
}

// 旧存档没有 next_seq(为 0):按 1 处理,第一条记录的 seq 就是 1。
uint64_t EffectiveNextSeq(const ClientRequestLedgerComp &ledger)
{
	return ledger.next_seq() == 0 ? 1 : ledger.next_seq();
}

// 淘汰 seq 最小的那条。records 的下标不承载顺序,所以拿末尾那条填坑,不用整体挪动。调用方保证非空。
void EvictOldest(ClientRequestLedgerComp &ledger)
{
	auto &records = *ledger.mutable_records();
	int oldest = 0;
	for (int i = 1; i < records.size(); ++i)
	{
		if (records.Get(i).seq() < records.Get(oldest).seq())
		{
			oldest = i;
		}
	}
	records.SwapElements(oldest, records.size() - 1);
	records.RemoveLast();
}
} // namespace

ClientRequestVerdict Check(const ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind,
						   std::string_view fingerprint, ClientRequestResult &result)
{
	if (key == 0)
	{
		return ClientRequestVerdict::kNew;
	}
	const ClientRequestRecord *record = FindRecord(ledger, key);
	if (record == nullptr)
	{
		return ClientRequestVerdict::kNew;
	}
	if (record->kind() != static_cast<uint32_t>(kind) || record->fingerprint() != fingerprint)
	{
		return ClientRequestVerdict::kMismatch;
	}
	result.enhanceSuccess = record->enhance_success();
	result.enhanceLevel = record->enhance_level();
	result.enhancePityCount = record->enhance_pity_count();
	result.createdSchemeId = record->created_scheme_id();
	result.equipmentItemId = record->equipment_item_id();
	result.forgeRevision = record->forge_revision();
	result.materialRewardRevision = record->material_reward_revision();
	result.craftingMaterialCount = record->crafting_material_count();
	result.enhancementStoneCount = record->enhancement_stone_count();
	result.craftingTradableCount = record->crafting_tradable_count();
	result.enhancementTradableCount = record->enhancement_tradable_count();
	result.seasonCatchupPoints = record->season_catchup_points();
	result.seasonCatchupSpent = record->season_catchup_spent();
	result.trialMilestoneId = record->trial_milestone_id();
	result.trialMilestoneSandCount = record->trial_milestone_sand_count();
	result.trialMilestoneVoucherCount = record->trial_milestone_voucher_count();
	result.redeemedArtifactItemId = record->redeemed_artifact_item_id();
	result.redeemedArtifactConfigId = record->redeemed_artifact_config_id();
	result.equipmentQualityTier = record->equipment_quality_tier();
	result.equipmentPolishPityCount = record->equipment_polish_pity_count();
	result.equipmentGuaranteedByPity = record->equipment_guaranteed_by_pity();
	result.craftedSkillBookId = record->crafted_skill_book_id();
	result.craftedSkillTableId = record->crafted_skill_table_id();
	result.scrollCraftRevision = record->scroll_craft_revision();
	result.scrollBonusFragments = record->scroll_bonus_fragments();
	result.travelPassClaimedCount = record->travel_pass_claimed_count();
	result.equipLoadoutUnlockedCount = record->equip_loadout_unlocked_count();
	result.equipLoadoutUsedItemId = record->equip_loadout_used_item_id();
	result.equipLoadoutSpentBindDiamond = record->equip_loadout_spent_bind_diamond();
	result.equipLoadoutSpentDiamond = record->equip_loadout_spent_diamond();
	result.rewardedAdCurrencyType = record->rewarded_ad_currency_type();
	result.rewardedAdAmount = record->rewarded_ad_amount();
	result.sealedBookItemId = record->sealed_book_item_id();
	return ClientRequestVerdict::kReplay;
}

bool Remember(ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result)
{
	if (key == 0 || FindRecord(ledger, key) != nullptr)
	{
		return false;
	}
	// 用 while:加载进来的旧存档可能多于 kCapacity 条(容量下调过),一次淘汰到位。
	while (ledger.records_size() >= kCapacity)
	{
		EvictOldest(ledger);
	}
	const uint64_t seq = EffectiveNextSeq(ledger);
	auto *record = ledger.add_records();
	record->set_key(key);
	record->set_kind(static_cast<uint32_t>(kind));
	record->set_fingerprint(std::move(fingerprint));
	record->set_seq(seq);
	record->set_enhance_success(result.enhanceSuccess);
	record->set_enhance_level(result.enhanceLevel);
	record->set_enhance_pity_count(result.enhancePityCount);
	record->set_created_scheme_id(result.createdSchemeId);
	record->set_equipment_item_id(result.equipmentItemId);
	record->set_forge_revision(result.forgeRevision);
	record->set_material_reward_revision(result.materialRewardRevision);
	record->set_crafting_material_count(result.craftingMaterialCount);
	record->set_enhancement_stone_count(result.enhancementStoneCount);
	record->set_crafting_tradable_count(result.craftingTradableCount);
	record->set_enhancement_tradable_count(result.enhancementTradableCount);
	record->set_season_catchup_points(result.seasonCatchupPoints);
	record->set_season_catchup_spent(result.seasonCatchupSpent);
	record->set_trial_milestone_id(result.trialMilestoneId);
	record->set_trial_milestone_sand_count(result.trialMilestoneSandCount);
	record->set_trial_milestone_voucher_count(result.trialMilestoneVoucherCount);
	record->set_redeemed_artifact_item_id(result.redeemedArtifactItemId);
	record->set_redeemed_artifact_config_id(result.redeemedArtifactConfigId);
	record->set_equipment_quality_tier(result.equipmentQualityTier);
	record->set_equipment_polish_pity_count(result.equipmentPolishPityCount);
	record->set_equipment_guaranteed_by_pity(result.equipmentGuaranteedByPity);
	record->set_crafted_skill_book_id(result.craftedSkillBookId);
	record->set_crafted_skill_table_id(result.craftedSkillTableId);
	record->set_scroll_craft_revision(result.scrollCraftRevision);
	record->set_scroll_bonus_fragments(result.scrollBonusFragments);
	record->set_travel_pass_claimed_count(result.travelPassClaimedCount);
	record->set_equip_loadout_unlocked_count(result.equipLoadoutUnlockedCount);
	record->set_equip_loadout_used_item_id(result.equipLoadoutUsedItemId);
	record->set_equip_loadout_spent_bind_diamond(result.equipLoadoutSpentBindDiamond);
	record->set_equip_loadout_spent_diamond(result.equipLoadoutSpentDiamond);
	record->set_rewarded_ad_currency_type(result.rewardedAdCurrencyType);
	record->set_rewarded_ad_amount(result.rewardedAdAmount);
	record->set_sealed_book_item_id(result.sealedBookItemId);
	ledger.set_next_seq(seq + 1);
	return true;
}

std::string Validate(const ClientRequestLedgerComp &ledger)
{
	const auto& material = ledger.material_reward();
	if (!material.migrated_to_v2())
	{
		// 兼容迁移前老存档：不得携带未迁移的 v2 字段，沿用旧日限与 10 份步长不变量。
		uint64_t dayStart = 0, nextDay = 0;
		if (material.lifetime_earned() != 0 || material.tradable_earned_total() != 0 ||
			material.stage1_tradable_confirmed() != 0 ||
			material.available_tradable_units() != 0 || material.week_starts_at_ms() != 0 ||
			material.earned_this_week() != 0 ||
			material.earned_today() > PlayerEquipmentMaterialRewardSystem::kDailyLimit ||
			material.earned_today() % PlayerEquipmentMaterialRewardSystem::kBattleUnits != 0 ||
			material.available_units() % PlayerEquipmentMaterialRewardSystem::kBattleUnits != 0 ||
			(material.day_starts_at_ms() == 0 && (material.revision() != 0 || material.earned_today() != 0 || material.available_units() != 0)) ||
			(material.day_starts_at_ms() != 0 && (material.earned_today() == 0 ||
				!PlayerEquipmentMaterialRewardSystem::ResolveDay(material.day_starts_at_ms(), dayStart, nextDay) ||
				dayStart != material.day_starts_at_ms())))
			return "invalid equipment material reward ledger";
	}
	else
	{
		// 两阶段 v2 账本不变量（合入 A）：首套 660（跨角色可交易<=60）+ 二阶段每周 150（月卡胜局解锁可交易），5 份步长。
		uint64_t weekStart = 0, nextWeek = 0;
		if (material.lifetime_earned() > PlayerEquipmentMaterialRewardSystem::kLifetimeLimit ||
			material.lifetime_earned() % PlayerEquipmentMaterialRewardSystem::kMinClaimStepUnits != 0 ||
			material.tradable_earned_total() > PlayerEquipmentMaterialRewardSystem::kTradableLifetimeLimit ||
			material.tradable_earned_total() > material.lifetime_earned() / PlayerEquipmentMaterialRewardSystem::kBattleUnits ||
			material.stage1_tradable_confirmed() > material.tradable_earned_total() ||
			material.available_units() % PlayerEquipmentMaterialRewardSystem::kMinClaimStepUnits != 0 ||
			material.available_tradable_units() > material.available_units() ||
			(material.lifetime_earned() < PlayerEquipmentMaterialRewardSystem::kLifetimeLimit &&
				material.available_tradable_units() > material.tradable_earned_total()) ||
			material.earned_this_week() > PlayerEquipmentMaterialRewardSystem::kWeeklyLimit ||
			material.earned_this_week() % PlayerEquipmentMaterialRewardSystem::kMinClaimStepUnits != 0 ||
			(material.week_starts_at_ms() == 0 && material.earned_this_week() != 0) ||
			(material.week_starts_at_ms() != 0 && (material.earned_this_week() == 0 ||
				material.lifetime_earned() < PlayerEquipmentMaterialRewardSystem::kLifetimeLimit ||
				!PlayerEquipmentMaterialRewardSystem::ResolveWeek(material.week_starts_at_ms(), weekStart, nextWeek) ||
				weekStart != material.week_starts_at_ms())))
			return "invalid equipment material reward ledger";
	}
	if (ledger.records_size() > kMaxRecordsOnLoad)
	{
		return "too many records: " + std::to_string(ledger.records_size());
	}
	const uint64_t nextSeq = EffectiveNextSeq(ledger);
	std::unordered_set<uint64_t> keys;
	std::unordered_set<uint64_t> seqs;
	std::unordered_set<uint64_t> forgeRevisions;
	std::unordered_set<uint64_t> materialRevisions;
	for (const auto &record : ledger.records())
	{
		if (record.kind() == static_cast<uint32_t>(ClientRequestKind::kEquipmentMaterialReward))
		{
			const uint64_t total = uint64_t{record.crafting_material_count()} + record.enhancement_stone_count();
			if (record.material_reward_revision() == 0 || record.material_reward_revision() > material.revision() ||
				total == 0 || total > PlayerEquipmentMaterialRewardSystem::kMaxClaimBatchUnits ||
				total % PlayerEquipmentMaterialRewardSystem::kMinClaimStepUnits != 0 ||
				record.crafting_tradable_count() > record.crafting_material_count() ||
				record.enhancement_tradable_count() > record.enhancement_stone_count() ||
				!materialRevisions.insert(record.material_reward_revision()).second)
				return "invalid equipment material reward receipt";
		}
		if (record.kind() == static_cast<uint32_t>(ClientRequestKind::kEquipmentCraft) &&
			(record.equipment_item_id() == 0 || (record.equipment_item_id() >> 63) != 0 || record.forge_revision() == 0 ||
			 record.forge_revision() > ledger.forge_revision() || !forgeRevisions.insert(record.forge_revision()).second))
		{
			return "invalid equipment craft receipt";
		}
		if (record.key() == 0)
		{
			return "a record has key 0";
		}
		if (!keys.insert(record.key()).second)
		{
			return "duplicate key " + std::to_string(record.key());
		}
		if (record.fingerprint().size() != kClientRequestFingerprintBytes)
		{
			return "fingerprint of key " + std::to_string(record.key()) + " is " +
				   std::to_string(record.fingerprint().size()) + " bytes";
		}
		if (record.seq() == 0 || record.seq() >= nextSeq)
		{
			return "seq " + std::to_string(record.seq()) + " of key " + std::to_string(record.key()) +
				   " is outside [1, next_seq=" + std::to_string(nextSeq) + ")";
		}
		if (!seqs.insert(record.seq()).second)
		{
			return "duplicate seq " + std::to_string(record.seq());
		}
	}
	return {};
}
} // namespace client_request_ledger

namespace client_request_dedup
{
ClientRequestVerdict Check(entt::entity player, uint64_t key, ClientRequestKind kind, std::string_view fingerprint,
						   ClientRequestResult &result)
{
	if (key == 0)
	{
		return ClientRequestVerdict::kNew;
	}
	const auto &registry = tlsEcs.actorRegistry;
	if (registry.any_of<ClientRequestLedgerInvalidComp>(player))
	{
		LOG_WARN << "[ClientRequest] metric=client_request_unavailable reason=ledger_invalid kind="
				 << ClientRequestKindName(kind) << " player_id=" << tlsEcs.GetPlayerId(player)
				 << " client_request_id=" << key << " (keyed request refused without executing)";
		return ClientRequestVerdict::kUnavailable;
	}
	// 只读:try_get,不创建组件。持久数据只由加载创建,缺了 = 加载不变量被破坏,不现造一本空账本来放行。
	const auto *ledger = registry.try_get<ClientRequestLedgerComp>(player);
	if (ledger == nullptr)
	{
		LOG_ERROR << "[ClientRequest] metric=client_request_unavailable reason=ledger_missing kind="
				  << ClientRequestKindName(kind) << " player_id=" << tlsEcs.GetPlayerId(player)
				  << " client_request_id=" << key << " (load invariant broken; keyed request refused without executing)";
		return ClientRequestVerdict::kUnavailable;
	}
	const auto verdict = client_request_ledger::Check(*ledger, key, kind, fingerprint, result);
	if (verdict == ClientRequestVerdict::kReplay)
	{
		LOG_INFO << "[ClientRequest] metric=client_request_replay kind=" << ClientRequestKindName(kind)
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
	}
	else if (verdict == ClientRequestVerdict::kMismatch)
	{
		LOG_WARN << "[ClientRequest] metric=client_request_mismatch kind=" << ClientRequestKindName(kind)
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key
				 << " (key was used for a different operation; refused without executing)";
	}
	return verdict;
}

void Remember(entt::entity player, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result)
{
	if (key == 0)
	{
		return;
	}
	auto &registry = tlsEcs.actorRegistry;
	auto *ledger = registry.try_get<ClientRequestLedgerComp>(player);
	if (ledger == nullptr || registry.any_of<ClientRequestLedgerInvalidComp>(player))
	{
		// 接入点第 2 步已经按 kUnavailable 返回,正常走不到:走到了说明那个接入点的顺序写错了。
		LOG_ERROR << "[ClientRequest] Remember skipped, ledger missing or invalid: kind=" << ClientRequestKindName(kind)
				  << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
		return;
	}
	if (!client_request_ledger::Remember(*ledger, key, kind, std::move(fingerprint), result))
	{
		// 只有接入点没先 Check 就执行才会走到这里:说明那个接入点的顺序写错了。
		LOG_ERROR << "[ClientRequest] Remember refused, key is already in the ledger: kind=" << ClientRequestKindName(kind)
				  << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
	}
}
} // namespace client_request_dedup
