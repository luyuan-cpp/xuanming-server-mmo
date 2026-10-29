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

namespace turnbattle {

bool TableBattleDataProvider::LoadBattleRewards(const std::string& path) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input || input.tellg() <= 0 || input.tellg() > 1024 * 1024) {
        LOG_ERROR << "Battle reward config unavailable or exceeds 1 MiB: " << path;
        return false;
    }
    std::string json(static_cast<size_t>(input.tellg()), '\0');
    input.seekg(0);
    if (!input.read(json.data(), static_cast<std::streamsize>(json.size()))) return false;
    BattleRewardConfig candidate;
    const auto status = google::protobuf::util::JsonStringToMessage(json, &candidate);
    if (!status.ok()) {
        LOG_ERROR << "Invalid battle reward config: " << status.ToString();
        return false;
    }
    if (candidate.profiles_size() == 0 || candidate.profiles_size() > 256) return false;
    std::set<std::pair<uint32_t, std::string>> keys;
    for (const auto& profile : candidate.profiles()) {
        if (profile.strategy_key().size() > 64 ||
            !keys.emplace(profile.battle_config_id(), profile.strategy_key()).second ||
            profile.item_draws() > 100 || profile.random_items_size() > 256 ||
            (profile.item_draws() > 0 && profile.random_items_size() == 0)) return false;
        std::set<uint32_t> currencies;
        for (const auto& currency : profile.currencies()) {
            if (currency.currency_type() >= kCurrencyMax ||
                currency.amount() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
                !currencies.insert(currency.currency_type()).second) return false;
        }
        std::set<uint32_t> items;
        for (const auto& item : profile.random_items()) {
            const auto* row = FindItem(item.item_table_id());
            if (row == nullptr || row->max_stack_size() == 0 || item.count() == 0 || item.weight() == 0 ||
                !items.insert(item.item_table_id()).second ||
                static_cast<uint64_t>(item.count()) * profile.item_draws() > UINT32_MAX) return false;
        }
    }
    // 未单独配置的副本必须有明确默认档；缺配置不能悄悄退回逐怪发奖。
    if (keys.count({0, ""}) == 0) return false;
    rewards.Swap(&candidate);
    return true;
}

const BattleRewardProfile* TableBattleDataProvider::FindBattleReward(
    uint32_t battleConfigId, const std::string& strategyKey) const {
    for (const auto& key : {std::make_pair(battleConfigId, strategyKey),
                            std::make_pair(battleConfigId, std::string{}),
                            std::make_pair(0u, strategyKey), std::make_pair(0u, std::string{})}) {
        for (const auto& profile : rewards.profiles()) {
            if (profile.battle_config_id() == key.first && profile.strategy_key() == key.second) return &profile;
        }
    }
    return nullptr;
}

const SkillTable* TableBattleDataProvider::FindSkill(uint32_t skillTableId) const {
    // Silent 版:查不到不刷错误日志,是否算错误由引擎按调用场景判定
    return SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
}

const BuffTable* TableBattleDataProvider::FindBuff(uint32_t buffTableId) const {
    return BuffTableManager::Instance().FindByIdSilent(buffTableId).first;
}

const SkillPermissionTable* TableBattleDataProvider::FindSkillPermission(uint32_t combatStateId) const {
    return SkillPermissionTableManager::Instance().FindByIdSilent(combatStateId).first;
}

const DungeonTable* TableBattleDataProvider::FindDungeon(uint32_t dungeonTableId) const {
    return DungeonTableManager::Instance().FindByIdSilent(dungeonTableId).first;
}

const MonsterTable* TableBattleDataProvider::FindMonster(uint32_t monsterTableId) const {
    return MonsterTableManager::Instance().FindByIdSilent(monsterTableId).first;
}

const ItemTable* TableBattleDataProvider::FindItem(uint32_t itemTableId) const {
    return ItemTableManager::Instance().FindByIdSilent(itemTableId).first;
}

uint64_t TableBattleDataProvider::GetCooldownDurationMs(uint32_t cooldownTableId) const {
    // CooldownTable.duration 毫秒口径,与实时战斗 CoolDownTimeMillisecondSystem 一致
    const auto* row = CooldownTableManager::Instance().FindByIdSilent(cooldownTableId).first;
    return row == nullptr ? 0 : row->duration();
}

std::vector<uint32_t> TableBattleDataProvider::GetDungeonMonsterIds(uint32_t dungeonTableId) const {
    // 读 DungeonTable.monster 怪物组列(2026-09-02 加列);缺行/空组时返回空表,
    // 引擎按玩家人数生成兜底怪(InitMonsters),保证 PVE 一定有对手。
    const auto* row = DungeonTableManager::Instance().FindByIdSilent(dungeonTableId).first;
    if (row == nullptr) {
        return {};
    }
    std::vector<uint32_t> ids;
    ids.reserve(static_cast<size_t>(row->monster_size()));
    for (int i = 0; i < row->monster_size(); ++i) {
        if (row->monster(i) != 0) {  // 表内跨列留空处会补 0,跳过
            ids.push_back(row->monster(i));
        }
    }
    return ids;
}

uint32_t TableBattleDataProvider::GetDungeonMonstersPerPlayer(uint32_t dungeonTableId) const {
    // 每名玩家对应一整组怪:单人打到的还是表里那一组(只数、种类、任务目标、掉落都与按人数放大之前
    // 相同),N 人就是这一组重复 N 遍。不能取更小的数 —— 只取组头几只的话,人少的队伍永远遇不到
    // 组里靠后的怪,以它们为目标的任务和它们独有的掉落就做不了了。
    // DungeonTable 没有单独的"每人几只"列,组的只数就是每人的只数;空组返回 0,引擎走兜底怪。
    return static_cast<uint32_t>(GetDungeonMonsterIds(dungeonTableId).size());
}

double TableBattleDataProvider::GetSkillDamage(uint32_t skillTableId, double casterLevel) {
    // damage 表达式两步调用:先 SetDamageParam({casterLevel}) 再 GetDamage,
    // 与实时战斗 CalculateSkillDamage 的用法一致
    SkillTableManager::Instance().SetDamageParam({casterLevel});
    return SkillTableManager::Instance().GetDamage(skillTableId);
}

double TableBattleDataProvider::GetBuffHealthRegeneration(uint32_t buffTableId, double level, double lostHealth) {
    // 镜像 ModifierBuffImplSystem::OnHealthRegenerationBasedOnLostHealth 的参数口径
    BuffTableManager::Instance().SetHealthRegenerationParam({level, lostHealth});
    return BuffTableManager::Instance().GetHealthRegeneration(buffTableId);
}

double TableBattleDataProvider::GetBuffBonusDamage(uint32_t buffTableId) {
    return BuffTableManager::Instance().GetBonusDamage(buffTableId);
}

}  // namespace turnbattle
