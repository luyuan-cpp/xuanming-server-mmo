#include "data/table_battle_data_provider.h"

#include <fstream>
#include <limits>
#include <set>
#include <utility>
#include "google/protobuf/util/json_util.h"
#include "modules/currency/constants/currency.h"
#include "muduo/base/Logging.h"

#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/cooldown_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/table_loader.h"

namespace turnbattle {

namespace {

// 启动边界读取奖励配置:空文件、读不出或超过 1 MiB 都按不可用处理
bool ReadSmallConfigFile(const std::string& path, const char* label, std::string& content) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input || input.tellg() <= 0 || input.tellg() > 1024 * 1024) {
        LOG_ERROR << label << " unavailable or exceeds 1 MiB: " << path;
        return false;
    }
    content.assign(static_cast<size_t>(input.tellg()), '\0');
    input.seekg(0);
    return static_cast<bool>(input.read(content.data(), static_cast<std::streamsize>(content.size())));
}

std::shared_ptr<const TableSet> CaptureActiveBattleTableSet() {
    auto set = std::make_shared<TableSet>();
    set->skillTable = SkillTableManager::Instance().CurrentSnapshot();
    set->buffTable = BuffTableManager::Instance().CurrentSnapshot();
    set->cooldownTable = CooldownTableManager::Instance().CurrentSnapshot();
    set->skillPermissionTable = SkillPermissionTableManager::Instance().CurrentSnapshot();
    set->dungeonTable = DungeonTableManager::Instance().CurrentSnapshot();
    set->monsterTable = MonsterTableManager::Instance().CurrentSnapshot();
    set->itemTable = ItemTableManager::Instance().CurrentSnapshot();
    set->petTable = PetTableManager::Instance().CurrentSnapshot();
    return set;
}

bool ValidateRewardConfigWithItemLookup(
    const BattleRewardConfig& candidate,
    const std::function<const ItemTable*(uint32_t)>& findItem,
    std::string& err) {
    if (candidate.profiles_size() == 0 || candidate.profiles_size() > 256) {
        err = "battle reward config must have 1..256 profiles";
        return false;
    }
    std::set<std::pair<uint32_t, std::string>> keys;
    for (const auto& profile : candidate.profiles()) {
        if (profile.strategy_key().size() > 64 ||
            !keys.emplace(profile.battle_config_id(), profile.strategy_key()).second ||
            profile.item_draws() > 100 || profile.random_items_size() > 256 ||
            (profile.item_draws() > 0 && profile.random_items_size() == 0)) {
            err = "battle reward config contains invalid or duplicate profile";
            return false;
        }
        std::set<uint32_t> currencies;
        for (const auto& currency : profile.currencies()) {
            if (!IsKnownCurrencyType(currency.currency_type()) ||
                currency.amount() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
                !currencies.insert(currency.currency_type()).second) {
                err = "battle reward config contains invalid currency entry";
                return false;
            }
        }
        std::set<uint32_t> items;
        for (const auto& item : profile.random_items()) {
            const auto* row = findItem(item.item_table_id());
            if (row == nullptr || row->max_stack_size() == 0 || item.count() == 0 || item.weight() == 0 ||
                !items.insert(item.item_table_id()).second ||
                static_cast<uint64_t>(item.count()) * profile.item_draws() > UINT32_MAX) {
                err = "battle reward config references unknown or invalid item_table_id=" +
                      std::to_string(item.item_table_id());
                return false;
            }
        }
    }
    // 未单独配置的副本必须有明确默认档;缺配置不能悄悄退回逐怪发奖。
    if (keys.count({0, ""}) == 0) {
        err = "battle reward config missing default profile (0, \"\")";
        return false;
    }
    return true;
}

}  // namespace

TableBattleDataProvider::TableBattleDataProvider()
    : tableSet(CaptureActiveBattleTableSet()),
      rewards(std::make_shared<BattleRewardConfig>()) {}

TableBattleDataProvider::TableBattleDataProvider(
    std::shared_ptr<const TableSet> boundTableSet,
    std::shared_ptr<const BattleRewardConfig> sharedRewards)
    : tableSet(boundTableSet ? std::move(boundTableSet) : CaptureActiveBattleTableSet()),
      rewards(sharedRewards ? std::move(sharedRewards) : std::make_shared<BattleRewardConfig>()) {}

bool TableBattleDataProvider::LoadBattleRewards(const std::string& path) {
    std::string json;
    if (!ReadSmallConfigFile(path, "Battle reward config", json)) return false;
    auto candidate = std::make_shared<BattleRewardConfig>();
    const auto status = google::protobuf::util::JsonStringToMessage(json, candidate.get());
    if (!status.ok()) {
        LOG_ERROR << "Invalid battle reward config: " << status.ToString();
        return false;
    }
    std::string err;
    if (!ValidateRewardConfigWithItemLookup(
            *candidate,
            [this](uint32_t id) { return FindItem(id); },
            err)) {
        LOG_ERROR << "Battle reward config validation failed: " << err;
        return false;
    }
    rewards = std::move(candidate);
    return true;
}

bool TableBattleDataProvider::ValidateRewardsAgainst(
    const BattleRewardConfig& rewardConfig,
    const TableSet& candidate,
    std::string& err) {
    return ValidateRewardConfigWithItemLookup(
        rewardConfig,
        [&candidate](uint32_t id) -> const ItemTable* {
            return candidate.itemTable ? candidate.itemTable->FindRowById(id) : nullptr;
        },
        err);
}

const BattleRewardProfile* TableBattleDataProvider::FindBattleReward(
    uint32_t battleConfigId, const std::string& strategyKey) const {
    if (!rewards) return nullptr;
    for (const auto& key : {std::make_pair(battleConfigId, strategyKey),
                            std::make_pair(battleConfigId, std::string{}),
                            std::make_pair(0u, strategyKey), std::make_pair(0u, std::string{})}) {
        for (const auto& profile : rewards->profiles()) {
            if (profile.battle_config_id() == key.first && profile.strategy_key() == key.second) return &profile;
        }
    }
    return nullptr;
}

const SkillTable* TableBattleDataProvider::FindSkill(uint32_t skillTableId) const {
    return (tableSet && tableSet->skillTable) ? tableSet->skillTable->FindRowById(skillTableId) : nullptr;
}

const BuffTable* TableBattleDataProvider::FindBuff(uint32_t buffTableId) const {
    return (tableSet && tableSet->buffTable) ? tableSet->buffTable->FindRowById(buffTableId) : nullptr;
}

const SkillPermissionTable* TableBattleDataProvider::FindSkillPermission(uint32_t combatStateId) const {
    return (tableSet && tableSet->skillPermissionTable)
               ? tableSet->skillPermissionTable->FindRowById(combatStateId)
               : nullptr;
}

const DungeonTable* TableBattleDataProvider::FindDungeon(uint32_t dungeonTableId) const {
    return (tableSet && tableSet->dungeonTable) ? tableSet->dungeonTable->FindRowById(dungeonTableId) : nullptr;
}

const MonsterTable* TableBattleDataProvider::FindMonster(uint32_t monsterTableId) const {
    return (tableSet && tableSet->monsterTable) ? tableSet->monsterTable->FindRowById(monsterTableId) : nullptr;
}

const PetTable* TableBattleDataProvider::FindPet(uint32_t petTableId) const {
    return (tableSet && tableSet->petTable) ? tableSet->petTable->FindRowById(petTableId) : nullptr;
}

const ItemTable* TableBattleDataProvider::FindItem(uint32_t itemTableId) const {
    return (tableSet && tableSet->itemTable) ? tableSet->itemTable->FindRowById(itemTableId) : nullptr;
}

uint64_t TableBattleDataProvider::GetCooldownDurationMs(uint32_t cooldownTableId) const {
    const auto* row =
        (tableSet && tableSet->cooldownTable) ? tableSet->cooldownTable->FindRowById(cooldownTableId) : nullptr;
    return row == nullptr ? 0 : row->duration();
}

std::vector<uint32_t> TableBattleDataProvider::GetDungeonMonsterIds(uint32_t dungeonTableId) const {
    const auto* row = FindDungeon(dungeonTableId);
    if (row == nullptr) {
        return {};
    }
    std::vector<uint32_t> ids;
    ids.reserve(static_cast<size_t>(row->monster_size()));
    for (int i = 0; i < row->monster_size(); ++i) {
        if (row->monster(i) != 0) {
            ids.push_back(row->monster(i));
        }
    }
    return ids;
}

uint32_t TableBattleDataProvider::GetDungeonMonstersPerPlayer(uint32_t dungeonTableId) const {
    return static_cast<uint32_t>(GetDungeonMonsterIds(dungeonTableId).size());
}

double TableBattleDataProvider::GetSkillDamage(uint32_t skillTableId, double casterLevel) {
    return (tableSet && tableSet->skillTable)
               ? tableSet->skillTable->EvaluateDamage(skillTableId, {casterLevel})
               : 0.0;
}

double TableBattleDataProvider::GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) {
    return (tableSet && tableSet->buffTable)
               ? tableSet->buffTable->EvaluateHealthRegeneration(buffTableId, {level, lostHealth})
               : 0.0;
}

double TableBattleDataProvider::GetBuffBonusDamage(uint32_t buffTableId) {
    return (tableSet && tableSet->buffTable)
               ? tableSet->buffTable->EvaluateBonusDamage(buffTableId)
               : 0.0;
}

}  // namespace turnbattle
