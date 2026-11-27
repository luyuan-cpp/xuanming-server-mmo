#include "player_craft.h"

#include <algorithm>
#include <array>
#include <cstdint>
#include <limits>
#include <map>
#include <optional>
#include <random>
#include <set>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <hiredis/hiredis.h>
#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>

#include "battle/system/player_battle.h"
#include "core/type_define/player_guid_comp.h"
#include "engine/core/time/system/time.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "network/node_utils.h"
#include "player/system/client_request_ledger.h"
#include "player/system/equip_craft_quality_rules.h"
#include "player/system/player_attribute.h"
#include "player/system/player_equipment_v1.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mail_outbox.h"
#include "player/system/player_pet.h"
#include "player/system/player_skill.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_craft_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/component/team_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "table/code/item_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillscrollcraft_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace {
constexpr uint64_t kMaxPayableGold = static_cast<uint64_t>(std::numeric_limits<int64_t>::max());
constexpr double kProxyDirectoryRefreshIntervalSec = 5.0;

struct PendingProxyReputation {
    uint32_t orders{0};
    uint32_t supreme{0};
    uint32_t rare{0};
    uint64_t netGold{0};
    std::string lastBuyer;
};

struct HotProxyCounters {
    uint32_t orders24h{0};
    uint32_t supreme24h{0};
};

bool gCraftUnlockGateEnabled = false;
PlayerCraftSystem::CraftRollFn gCraftRollFn = nullptr;
PlayerCraftSystem::PersistFn gCraftPersistFn = &PlayerLifecycleSystem::SavePlayerForAssetCommit;
thread_local std::map<uint64_t, CraftProxyStallInfo> tlsProxyStallDirectory;
thread_local std::map<uint64_t, PendingProxyReputation> tlsPendingProxyReputation;
thread_local std::map<uint64_t, HotProxyCounters> tlsHotProxyCounters;
// 本节点正在从台账取走待回账的宗师:同一名宗师至多一次在途,入账(含存盘排程)期间不重入。
thread_local std::set<uint64_t> tlsProxyReputationTakesInFlight;
muduo::net::EventLoop* gProxyDirectoryLoop = nullptr;
muduo::net::TimerId gProxyDirectoryTimerId{};
bool gProxyDirectoryTimerActive = false;

uint32_t SaturatingAddU32(uint32_t a, uint32_t b) {
    const uint64_t sum = uint64_t{a} + uint64_t{b};
    return static_cast<uint32_t>(std::min<uint64_t>(sum, std::numeric_limits<uint32_t>::max()));
}

uint64_t SaturatingAddU64(uint64_t a, uint64_t b) {
    if (std::numeric_limits<uint64_t>::max() - a < b) return std::numeric_limits<uint64_t>::max();
    return a + b;
}

std::string BuyerDisplayNameOf(entt::entity player, uint64_t playerId) {
    if (const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
        profile != nullptr && !profile->name().empty()) {
        return profile->name();
    }
    return "侠士#" + std::to_string(playerId);
}

bool ShareActiveTeam(entt::entity a, entt::entity b) {
    if (!tlsEcs.actorRegistry.valid(a) || !tlsEcs.actorRegistry.valid(b)) return false;
    const auto* ta = tlsEcs.actorRegistry.try_get<TeamId>(a);
    const auto* tb = tlsEcs.actorRegistry.try_get<TeamId>(b);
    return ta != nullptr && tb != nullptr && ta->team_id() != 0 && ta->team_id() == tb->team_id();
}

uint32_t ActiveCraftTitleId(const PlayerCraftComp& craft) {
    const uint32_t id = craft.equipped_craft_title_id();
    if (id == 0) return 0;
    return equipcraft::IsCraftTitleUnlocked(id, craft.forge_mastery_exp(), craft.scroll_mastery_exp(),
                                            craft.supreme_forged_count(), craft.rare_books_crafted_count(),
                                            craft.proxy_orders_completed())
        ? id : 0u;
}

bool ValidSharedStall(const CraftProxyStallInfo& stall) {
    if (stall.master_player_id() == 0) return false;
    const bool forgeOk = stall.forge_mastery_tier() >= equipcraft::kMasteryMaster &&
        equipcraft::ValidProxyFee(stall.proxy_forge_fee_gold());
    const bool scrollOk = stall.scroll_mastery_tier() >= equipcraft::kMasteryMaster &&
        equipcraft::ValidProxyFee(stall.proxy_scroll_fee_gold());
    return forgeOk || scrollOk;
}

uint32_t RollBelow(uint32_t bound) {
    if (bound <= 1) return 0;
    if (gCraftRollFn != nullptr) return gCraftRollFn(bound) % bound;
    thread_local std::mt19937_64 rng{std::random_device{}()};
    return static_cast<uint32_t>(std::uniform_int_distribution<uint32_t>(0, bound - 1)(rng));
}

const ItemTable* ItemRow(uint32_t id) {
    return ItemTableManager::Instance().FindByIdSilent(id).first;
}

const SkillTable* SkillRow(uint32_t id) {
    return SkillTableManager::Instance().FindByIdSilent(id).first;
}

const SkillScrollCraftTable* ScrollRow(uint32_t id) {
    return SkillScrollCraftTableManager::Instance().FindByIdSilent(id).first;
}

bool ValidStackableMaterial(uint32_t id, uint32_t count) {
    const auto* row = ItemRow(id);
    return id != 0 && count != 0 && row != nullptr && row->max_stack_size() != 0 &&
        row->equip_kind() == 0 && row->battle_usable() == 0;
}

std::string DisplayNameOf(entt::entity player, uint64_t playerId) {
    if (const auto* profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
        profile != nullptr && !profile->name().empty()) {
        return profile->name();
    }
    return "宗师#" + std::to_string(playerId);
}

std::vector<uint32_t> CollectScrollSkillIds(uint32_t scrollId) {
    std::vector<uint32_t> ids;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (skill.scroll_id() == scrollId && skill.season_id() == 0 && skill.battle_model() == 1) {
            ids.push_back(skill.id());
        }
    }
    std::sort(ids.begin(), ids.end());
    return ids;
}

uint32_t CheckPlayerReady(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return kEntityIsNull;
    const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    const auto* ledger = tlsEcs.actorRegistry.try_get<ClientRequestLedgerComp>(player);
    if (guid == nullptr || guid->value == 0 || craft == nullptr || bags == nullptr || ledger == nullptr ||
        tlsEcs.actorRegistry.any_of<ClientRequestLedgerInvalidComp>(player) ||
        !client_request_ledger::Validate(*ledger).empty()) {
        return kServiceUnavailable;
    }
    return kSuccess;
}

uint32_t CheckWritable(entt::entity player) {
    if (!PlayerLifecycleSystem::IsPlayerMutable(player) || PlayerBattleSystem::IsInBattle(player)) {
        return kSkillUpgradeBusy;
    }
    return kSuccess;
}

bool BuildLocalStallSnapshot(entt::entity entity, uint64_t pid, CraftProxyStallInfo& out) {
    out.Clear();
    if (pid == 0 || !tlsEcs.actorRegistry.valid(entity)) return false;
    const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(entity);
    if (craft == nullptr || !craft->proxy_stall_open()) return false;
    const uint32_t forgeTier = equipcraft::MasteryTierForExp(craft->forge_mastery_exp());
    const uint32_t scrollTier = equipcraft::MasteryTierForExp(craft->scroll_mastery_exp());
    const bool forgeOk = forgeTier >= equipcraft::kMasteryMaster &&
        equipcraft::ValidProxyFee(craft->proxy_forge_fee_gold());
    const bool scrollOk = scrollTier >= equipcraft::kMasteryMaster &&
        equipcraft::ValidProxyFee(craft->proxy_scroll_fee_gold());
    if (!forgeOk && !scrollOk) return false;
    out.set_master_player_id(pid);
    out.set_master_name(DisplayNameOf(entity, pid));
    out.set_forge_mastery_tier(forgeTier);
    out.set_scroll_mastery_tier(scrollTier);
    out.set_top_quality_rate_bps(equipcraft::ForgeTopQualityRateBps(forgeTier));
    out.set_scroll_bonus_rate_bps(equipcraft::ScrollBonusFragmentRateBps(scrollTier));
    out.set_proxy_forge_fee_gold(forgeOk ? craft->proxy_forge_fee_gold() : 0u);
    out.set_proxy_scroll_fee_gold(scrollOk ? craft->proxy_scroll_fee_gold() : 0u);
    out.set_proxy_orders_completed(craft->proxy_orders_completed());
    out.set_supreme_forged_count(craft->supreme_forged_count());
    out.set_rare_books_crafted_count(craft->rare_books_crafted_count());
    out.set_master_showcase_tag(
        craft->master_showcase_tag() <= equipcraft::kMaxMasterShowcaseTag ? craft->master_showcase_tag() : 0u);
    out.set_equipped_craft_title_id(ActiveCraftTitleId(*craft));
    out.set_team_free_proxy(craft->team_free_proxy());
    out.set_viewer_gets_free_proxy(false);
    return true;
}

void PublishStallToRedis(uint64_t masterId, const CraftProxyStallInfo& stall) {
    if (!tlsControlRedis.connected() || masterId == 0) return;
    const std::string key = PlayerCraftSystem::ProxyStallDirectoryKey(GetZoneId());
    const std::string field = std::to_string(masterId);
    const std::string bytes = stall.SerializeAsString();
    const int ret = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply*) {},
        "HSET %s %s %b", key.c_str(), field.c_str(), bytes.data(), bytes.size());
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyDir] Failed to publish proxy stall to control Redis: master_id=" << masterId
                 << " ret=" << ret;
    }
}

void RemoveStallFromRedis(uint64_t masterId) {
    if (!tlsControlRedis.connected() || masterId == 0) return;
    const std::string key = PlayerCraftSystem::ProxyStallDirectoryKey(GetZoneId());
    const std::string field = std::to_string(masterId);
    const int ret = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply*) {},
        "HDEL %s %s", key.c_str(), field.c_str());
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyDir] Failed to remove proxy stall from control Redis: master_id=" << masterId
                 << " ret=" << ret;
    }
}

void OnProxyDirectoryReply(redisReply* reply) {
    if (reply == nullptr) return;
    if (reply->type == REDIS_REPLY_ERROR) {
        LOG_WARN << "[CraftProxyDir] Redis rejected directory refresh; keeping cached stalls: err="
                 << (reply->str != nullptr ? reply->str : "");
        return;
    }
    if (reply->type != REDIS_REPLY_ARRAY || (reply->elements % 2) != 0) {
        LOG_WARN << "[CraftProxyDir] Unexpected directory reply shape; keeping cached stalls: reply_type="
                 << reply->type;
        return;
    }
    std::map<uint64_t, CraftProxyStallInfo> refreshed;
    for (std::size_t i = 0; i + 1 < reply->elements; i += 2) {
        const redisReply* val = reply->element[i + 1];
        if (val == nullptr || val->type != REDIS_REPLY_STRING || val->str == nullptr ||
            val->len > static_cast<std::size_t>(std::numeric_limits<int>::max())) {
            continue;
        }
        CraftProxyStallInfo parsed;
        if (!parsed.ParseFromArray(val->str, static_cast<int>(val->len)) || !ValidSharedStall(parsed)) {
            continue;
        }
        parsed.set_viewer_gets_free_proxy(false);
        refreshed[parsed.master_player_id()] = std::move(parsed);
    }
    tlsProxyStallDirectory = std::move(refreshed);
}

void RedisHIncrByReputation(uint64_t masterId, const char* suffix, int64_t delta) {
    if (!tlsControlRedis.connected() || masterId == 0 || delta == 0) return;
    const std::string key = PlayerCraftSystem::ProxyReputationLedgerKey(GetZoneId());
    const std::string field = std::to_string(masterId) + suffix;
    const int ret = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply*) {},
        "HINCRBY %s %s %lld", key.c_str(), field.c_str(), static_cast<long long>(delta));
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyRep] Failed to update control Redis counter: master_id=" << masterId
                 << " field=" << field << " delta=" << delta << " ret=" << ret;
    }
}

void RedisHSetReputationBuyer(uint64_t masterId, const std::string& buyerName) {
    if (!tlsControlRedis.connected() || masterId == 0 || buyerName.empty()) return;
    const std::string key = PlayerCraftSystem::ProxyReputationLedgerKey(GetZoneId());
    const std::string field = std::to_string(masterId) + ":b";
    const int ret = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply*) {},
        "HSET %s %s %b", key.c_str(), field.c_str(), buyerName.data(), buyerName.size());
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyRep] Failed to store last buyer in control Redis: master_id=" << masterId
                 << " ret=" << ret;
    }
}

void ApplyReputationToCraftComp(PlayerCraftComp& craft, uint32_t ordersDelta, uint32_t supremeDelta,
                                uint32_t rareDelta, uint64_t netGoldDelta, const std::string& buyerName) {
    const uint32_t beforeMask = equipcraft::UnlockedCraftTitleMask(
        craft.forge_mastery_exp(), craft.scroll_mastery_exp(), craft.supreme_forged_count(),
        craft.rare_books_crafted_count(), craft.proxy_orders_completed());
    craft.set_proxy_orders_completed(SaturatingAddU32(craft.proxy_orders_completed(), ordersDelta));
    craft.set_supreme_forged_count(SaturatingAddU32(craft.supreme_forged_count(), supremeDelta));
    craft.set_rare_books_crafted_count(SaturatingAddU32(craft.rare_books_crafted_count(), rareDelta));
    const uint32_t afterMask = equipcraft::UnlockedCraftTitleMask(
        craft.forge_mastery_exp(), craft.scroll_mastery_exp(), craft.supreme_forged_count(),
        craft.rare_books_crafted_count(), craft.proxy_orders_completed());
    const uint32_t newlyUnlocked = equipcraft::NewlyUnlockedCraftTitleMask(beforeMask, afterMask);

    craft.set_unread_proxy_digest_orders(SaturatingAddU32(craft.unread_proxy_digest_orders(), ordersDelta));
    craft.set_unread_proxy_digest_supreme(SaturatingAddU32(craft.unread_proxy_digest_supreme(), supremeDelta));
    craft.set_unread_proxy_digest_rare(SaturatingAddU32(craft.unread_proxy_digest_rare(), rareDelta));
    craft.set_unread_proxy_digest_net_gold(SaturatingAddU64(craft.unread_proxy_digest_net_gold(), netGoldDelta));
    craft.set_unread_proxy_digest_new_titles_mask(craft.unread_proxy_digest_new_titles_mask() | newlyUnlocked);
    if (!buyerName.empty()) {
        craft.set_unread_proxy_digest_last_buyer(buyerName);
    }
}

bool ParseReputationField(std::string_view raw, uint64_t& masterId, std::string_view& suffix) {
    const std::size_t sep = raw.find(':');
    if (sep == std::string_view::npos || sep == 0 || sep + 1 >= raw.size()) return false;
    uint64_t parsed = 0;
    for (std::size_t i = 0; i < sep; ++i) {
        const char ch = raw[i];
        if (ch < '0' || ch > '9') return false;
        const uint64_t digit = static_cast<uint64_t>(ch - '0');
        if (parsed > (std::numeric_limits<uint64_t>::max() - digit) / 10u) return false;
        parsed = parsed * 10u + digit;
    }
    if (parsed == 0) return false;
    masterId = parsed;
    suffix = raw.substr(sep + 1);
    return true;
}

int64_t ParseNonNegativeInt64(std::string_view text) {
    if (text.empty()) return 0;
    bool neg = false;
    std::size_t idx = 0;
    if (text[0] == '-') {
        neg = true;
        idx = 1;
    }
    if (idx >= text.size()) return 0;
    int64_t val = 0;
    for (; idx < text.size(); ++idx) {
        const char ch = text[idx];
        if (ch < '0' || ch > '9') return 0;
        const int64_t digit = ch - '0';
        if (val > (std::numeric_limits<int64_t>::max() - digit) / 10) {
            return neg ? 0 : std::numeric_limits<int64_t>::max();
        }
        val = val * 10 + digit;
    }
    return neg ? 0 : val;
}

// ── 口碑待回账的取走与入账(r2-craft#4,设计见 craft-proxy-reputation-digest-and-hot-ranking.md §2.3)──
// 取走 = 同一段脚本里先 HMGET 再 HDEL:两个节点同时取同一名宗师,只有先执行的一方拿到值。字段与
// RecordProxyOrderReputation 写的一致(<id>:o/s/r/g/b);热榜的 :24o / :24s 不取。
constexpr char kTakeProxyReputationLua[] =
    "local k=KEYS[1] local p=ARGV[1] "
    "local v=redis.call('HMGET',k,p..':o',p..':s',p..':r',p..':g',p..':b') "
    "redis.call('HDEL',k,p..':o',p..':s',p..':r',p..':g',p..':b') "
    "return v";

// 取走脚本的应答是 5 项数组,缺的字段为 nil;负数(旧版回账多扣出来的)按 0 算,这一次已一并删掉。形状不对 = 结果未知。
std::optional<PlayerCraftSystem::TakenProxyReputation> ParseTakenReputation(const redisReply* reply) {
    if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 5) return std::nullopt;
    std::array<std::string_view, 5> values{};
    for (std::size_t i = 0; i < values.size(); ++i) {
        const redisReply* el = reply->element[i];
        if (el == nullptr) return std::nullopt;
        if (el->type == REDIS_REPLY_NIL) continue;
        if (el->type != REDIS_REPLY_STRING || el->str == nullptr) return std::nullopt;
        values[i] = std::string_view(el->str, el->len);
    }
    const auto asU32 = [](std::string_view text) {
        return static_cast<uint32_t>(std::min<int64_t>(ParseNonNegativeInt64(text), std::numeric_limits<uint32_t>::max()));
    };
    PlayerCraftSystem::TakenProxyReputation taken;
    taken.orders = asU32(values[0]);
    taken.supreme = asU32(values[1]);
    taken.rare = asU32(values[2]);
    taken.netGold = static_cast<uint64_t>(ParseNonNegativeInt64(values[3]));
    if (!values[4].empty()) taken.lastBuyer.assign(values[4].data(), values[4].size());
    return taken;
}

bool RedisTakeReputation(uint64_t masterId, PlayerCraftSystem::ProxyReputationTakeDone done) {
    if (!tlsControlRedis.connected() || masterId == 0) return false;
    const std::string key = PlayerCraftSystem::ProxyReputationLedgerKey(GetZoneId());
    const std::string master = std::to_string(masterId);
    const int ret = tlsControlRedis.command(
        [done = std::move(done)](hiredis::Hiredis*, redisReply* reply) { done(ParseTakenReputation(reply)); },
        "EVAL %s 1 %s %s", kTakeProxyReputationLua, key.c_str(), master.c_str());
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyRep] Reputation take command was not sent; pending stays in Redis: master_id=" << masterId
                 << " ret=" << ret;
        return false;
    }
    return true;
}

PlayerCraftSystem::ProxyReputationLedger WithDefaultLedger(PlayerCraftSystem::ProxyReputationLedger ledger) {
    if (!ledger.add) ledger.add = &RedisHIncrByReputation;
    if (!ledger.setLastBuyer) ledger.setLastBuyer = &RedisHSetReputationBuyer;
    if (!ledger.take) ledger.take = &RedisTakeReputation;
    return ledger;
}

// 当前生效的台账。只在 scene loop 线程读写;函数内静态,不依赖别的翻译单元的初始化顺序。
PlayerCraftSystem::ProxyReputationLedger& Ledger() {
    static PlayerCraftSystem::ProxyReputationLedger ledger = WithDefaultLedger({});
    return ledger;
}

void GiveBackProxyReputation(uint64_t masterId, const PlayerCraftSystem::TakenProxyReputation& taken) {
    if (taken.orders > 0) Ledger().add(masterId, ":o", static_cast<int64_t>(taken.orders));
    if (taken.supreme > 0) Ledger().add(masterId, ":s", static_cast<int64_t>(taken.supreme));
    if (taken.rare > 0) Ledger().add(masterId, ":r", static_cast<int64_t>(taken.rare));
    if (taken.netGold > 0 && taken.netGold <= kMaxPayableGold) {
        Ledger().add(masterId, ":g", static_cast<int64_t>(taken.netGold));
    }
    if (!taken.lastBuyer.empty()) Ledger().setLastBuyer(masterId, taken.lastBuyer);
}

// 取走的应答回到 loop:按 player_id 重新找实体。数额只认取走的值;宗师已离开或存盘排不上,原样还回台账,下一次再取。
void ApplyTakenProxyReputation(uint64_t masterId, const std::optional<PlayerCraftSystem::TakenProxyReputation>& taken) {
    if (!taken.has_value()) {
        LOG_WARN << "[CraftProxyRep] Reputation take result unknown; nothing applied, the pending delta is either still in Redis"
                 << " or lost (display-only counters): master_id=" << masterId;
        return;
    }
    if (taken->orders == 0 && taken->supreme == 0 && taken->rare == 0 && taken->netGold == 0) {
        // 别的节点已取走,或本节点镜像过时(在途刷新带回的旧值):什么都不记,镜像这一条作废,下一次刷新以台账为准。
        tlsPendingProxyReputation.erase(masterId);
        return;
    }
    const entt::entity player = tlsEcs.GetPlayer(masterId);
    auto* craft = tlsEcs.actorRegistry.valid(player) && PlayerLifecycleSystem::IsPlayerMutable(player)
        ? tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player)
        : nullptr;
    if (craft == nullptr) {
        GiveBackProxyReputation(masterId, *taken);
        LOG_INFO << "[CraftProxyRep] Master is no longer mutable here; taken reputation given back to Redis: master_id="
                 << masterId << " orders=" << taken->orders;
        return;
    }
    const PlayerCraftComp backup = *craft;
    ApplyReputationToCraftComp(*craft, taken->orders, taken->supreme, taken->rare, taken->netGold, taken->lastBuyer);
    if (gCraftPersistFn == nullptr || !gCraftPersistFn(player)) {
        *craft = backup;
        GiveBackProxyReputation(masterId, *taken);
        LOG_WARN << "[CraftProxyRep] Failed to persist drained proxy reputation; taken delta given back to Redis: master_id="
                 << masterId << " orders=" << taken->orders;
        return;
    }
    // 镜像扣掉取走的部分(只影响西市列表的叠加展示;取走之后本节点又记的增量留着)。存盘排程之后再查,不跨调用持迭代器。
    if (const auto mirror = tlsPendingProxyReputation.find(masterId); mirror != tlsPendingProxyReputation.end()) {
        auto& cur = mirror->second;
        cur.orders = cur.orders > taken->orders ? cur.orders - taken->orders : 0u;
        cur.supreme = cur.supreme > taken->supreme ? cur.supreme - taken->supreme : 0u;
        cur.rare = cur.rare > taken->rare ? cur.rare - taken->rare : 0u;
        cur.netGold = cur.netGold > taken->netGold ? cur.netGold - taken->netGold : 0u;
        if (cur.orders == 0 && cur.supreme == 0 && cur.rare == 0 && cur.netGold == 0) {
            tlsPendingProxyReputation.erase(mirror);
        }
    }
    CraftProxyStallInfo updatedStall;
    if (BuildLocalStallSnapshot(player, masterId, updatedStall)) {
        tlsProxyStallDirectory[masterId] = updatedStall;
        PublishStallToRedis(masterId, updatedStall);
    }
    LOG_INFO << "[CraftProxyRep] Drained offline proxy reputation: master_id=" << masterId
             << " orders=" << taken->orders << " supreme=" << taken->supreme
             << " rare=" << taken->rare << " net_gold=" << taken->netGold;
}

void OnProxyReputationReply(redisReply* reply) {
    if (reply == nullptr) return;
    if (reply->type == REDIS_REPLY_ERROR) {
        LOG_WARN << "[CraftProxyRep] Redis rejected reputation refresh; keeping cached counters: err="
                 << (reply->str != nullptr ? reply->str : "");
        return;
    }
    if (reply->type != REDIS_REPLY_ARRAY || (reply->elements % 2) != 0) {
        LOG_WARN << "[CraftProxyRep] Unexpected reputation reply shape; keeping cached counters: reply_type="
                 << reply->type;
        return;
    }
    std::map<uint64_t, PendingProxyReputation> pending;
    std::map<uint64_t, HotProxyCounters> hot;
    for (std::size_t i = 0; i + 1 < reply->elements; i += 2) {
        const redisReply* keyEl = reply->element[i];
        const redisReply* valEl = reply->element[i + 1];
        if (keyEl == nullptr || valEl == nullptr || keyEl->type != REDIS_REPLY_STRING ||
            valEl->type != REDIS_REPLY_STRING || keyEl->str == nullptr || valEl->str == nullptr) {
            continue;
        }
        uint64_t masterId = 0;
        std::string_view suffix;
        if (!ParseReputationField(std::string_view(keyEl->str, keyEl->len), masterId, suffix)) continue;
        const std::string_view valView(valEl->str, valEl->len);
        if (suffix == "o") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) pending[masterId].orders = static_cast<uint32_t>(std::min<int64_t>(n, std::numeric_limits<uint32_t>::max()));
        } else if (suffix == "s") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) pending[masterId].supreme = static_cast<uint32_t>(std::min<int64_t>(n, std::numeric_limits<uint32_t>::max()));
        } else if (suffix == "r") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) pending[masterId].rare = static_cast<uint32_t>(std::min<int64_t>(n, std::numeric_limits<uint32_t>::max()));
        } else if (suffix == "g") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) pending[masterId].netGold = static_cast<uint64_t>(n);
        } else if (suffix == "b") {
            pending[masterId].lastBuyer.assign(valView.data(), valView.size());
        } else if (suffix == "24o") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) hot[masterId].orders24h = static_cast<uint32_t>(std::min<int64_t>(n, std::numeric_limits<uint32_t>::max()));
        } else if (suffix == "24s") {
            const int64_t n = ParseNonNegativeInt64(valView);
            if (n > 0) hot[masterId].supreme24h = static_cast<uint32_t>(std::min<int64_t>(n, std::numeric_limits<uint32_t>::max()));
        }
    }
    for (auto it = pending.begin(); it != pending.end();) {
        if (it->second.orders == 0 && it->second.supreme == 0 && it->second.rare == 0 && it->second.netGold == 0) {
            it = pending.erase(it);
        } else {
            ++it;
        }
    }
    tlsPendingProxyReputation = std::move(pending);
    tlsHotProxyCounters = std::move(hot);

    // 同步完成后立即为本节点已在线且有待回账口碑的宗师执行回账。
    std::vector<entt::entity> toDrain;
    for (const auto& [masterId, _] : tlsPendingProxyReputation) {
        const entt::entity e = tlsEcs.GetPlayer(masterId);
        if (tlsEcs.actorRegistry.valid(e)) toDrain.push_back(e);
    }
    for (const entt::entity e : toDrain) {
        PlayerCraftSystem::DrainPendingProxyReputation(e);
    }
}

void RequestProxyDirectoryRefresh() {
    if (!tlsControlRedis.connected()) return;
    const std::string key = PlayerCraftSystem::ProxyStallDirectoryKey(GetZoneId());
    const int ret = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply* reply) { OnProxyDirectoryReply(reply); },
        "HGETALL %s", key.c_str());
    if (ret != REDIS_OK) {
        LOG_WARN << "[CraftProxyDir] Directory refresh command was not sent; keeping cached stalls: ret=" << ret;
    }
    const std::string repKey = PlayerCraftSystem::ProxyReputationLedgerKey(GetZoneId());
    const int repRet = tlsControlRedis.command(
        [](hiredis::Hiredis*, redisReply* reply) { OnProxyReputationReply(reply); },
        "HGETALL %s", repKey.c_str());
    if (repRet != REDIS_OK) {
        LOG_WARN << "[CraftProxyRep] Reputation refresh command was not sent; keeping cached counters: ret=" << repRet;
    }
}

bool CanCreditProxyMaster(entt::entity master, uint64_t netFee) {
    if (netFee == 0) return true;
    if (tlsEcs.actorRegistry.try_get<CurrencyComp>(master) == nullptr ||
        tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(master) == nullptr) return false;
    if (GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                            static_cast<uint32_t>(kCurrencyGold)) ||
        CurrencySystem::IsCurrencyBlocked(master, kCurrencyGold)) return false;
    return !CurrencySystem::CreditOverflows(master, kCurrencyGold, netFee);
}

void FillWantedOrderInfo(const SkillBookWantedOrder& order, SkillBookWantedOrderInfo& out) {
    out.Clear();
    out.set_order_id(order.order_id());
    out.set_poster_player_id(order.poster_player_id());
    out.set_poster_name(order.poster_name());
    out.set_skill_table_id(order.skill_table_id());
    if (const auto* skill = SkillRow(order.skill_table_id()); skill != nullptr) {
        out.set_skill_name(skill->name());
    }
    out.set_scroll_id(order.scroll_id());
    if (const auto* scroll = ScrollRow(order.scroll_id()); scroll != nullptr) {
        out.set_scroll_name(scroll->name());
    }
    out.set_require_master_signature(order.require_master_signature());
    out.set_reward_gold(order.reward_gold());
    out.set_seller_net_gold(equipcraft::ProxyNetGold(order.reward_gold()));
    out.set_created_at_sec(order.created_at_sec());
}

int FindWantedOrderIndex(const PlayerCraftComp& craft, uint64_t orderId) {
    for (int i = 0; i < craft.wanted_orders_size(); ++i) {
        if (craft.wanted_orders(i).order_id() == orderId) return i;
    }
    return -1;
}

void RemoveWantedOrderAt(PlayerCraftComp& craft, int index) {
    auto* orders = craft.mutable_wanted_orders();
    if (index < 0 || index >= orders->size()) return;
    if (index != orders->size() - 1) {
        orders->SwapElements(index, orders->size() - 1);
    }
    orders->RemoveLast();
}

void FillStateSnapshot(entt::entity player, SkillCraftingStateSnapshot& out) {
    out.Clear();
    const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];

    out.set_scroll_craft_revision(craft.scroll_craft_revision());
    PlayerCraftSystem::FillMasterySnapshots(player, *out.mutable_forge_mastery(), *out.mutable_scroll_mastery());

    std::vector<const SkillScrollCraftTable*> scrolls;
    for (const auto& row : SkillScrollCraftTableManager::Instance().FindAll().data()) {
        if (row.enabled() != 0) scrolls.push_back(&row);
    }
    std::sort(scrolls.begin(), scrolls.end(), [](const auto* a, const auto* b) { return a->id() < b->id(); });
    for (const auto* row : scrolls) {
        auto* entry = out.add_scrolls();
        entry->set_scroll_id(row->id());
        entry->set_name(row->name());
        entry->set_description(row->description());
        entry->set_rarity_tier(row->rarity_tier());
        entry->set_fragment_item_id(row->fragment_item_id());
        entry->set_random_fragment_count(row->random_fragment_count());
        entry->set_random_gold_cost(row->random_gold_cost());
        entry->set_targeted_fragment_count(row->targeted_fragment_count());
        entry->set_targeted_gold_cost(row->targeted_gold_cost());
        entry->set_dismantle_fragment_count(row->dismantle_fragment_count());
        entry->set_rare_scroll_item_id(row->rare_scroll_item_id());
        entry->set_rare_scroll_count(row->rare_scroll_count());
        entry->set_rare_ink_item_id(row->rare_ink_item_id());
        entry->set_rare_ink_count(row->rare_ink_count());
        entry->set_rare_gold_cost(row->rare_gold_cost());
        for (const auto skillId : CollectScrollSkillIds(row->id())) {
            entry->add_skill_table_ids(skillId);
        }
    }

    std::vector<CraftedSkillBookEntry> books(craft.skill_books().begin(), craft.skill_books().end());
    std::sort(books.begin(), books.end(), [](const auto& a, const auto& b) { return a.book_id() < b.book_id(); });
    for (const auto& b : books) *out.add_skill_books() = b;

    std::vector<uint32_t> unlocked(craft.book_unlocked_skill_ids().begin(), craft.book_unlocked_skill_ids().end());
    std::sort(unlocked.begin(), unlocked.end());
    unlocked.erase(std::unique(unlocked.begin(), unlocked.end()), unlocked.end());
    for (const auto id : unlocked) out.add_book_unlocked_skill_ids(id);

    out.set_gold_balance(CurrencySystem::GetBalance(player, kCurrencyGold));
    out.set_fragment_owned(inventory.GetTotalItemCount(equipcraft::kScrollFragmentConfigId));
    out.set_rare_scroll_owned(inventory.GetTotalItemCount(equipcraft::kRareScrollPageConfigId));
    out.set_rare_ink_owned(inventory.GetTotalItemCount(equipcraft::kRareScrollInkConfigId));
    out.set_proxy_stall_open(craft.proxy_stall_open());
    out.set_proxy_forge_fee_gold(craft.proxy_forge_fee_gold());
    out.set_proxy_scroll_fee_gold(craft.proxy_scroll_fee_gold());
    out.set_craft_unlock_gate_enabled(PlayerCraftSystem::IsCraftUnlockGateEnabled());
    out.set_proxy_orders_completed(craft.proxy_orders_completed());
    out.set_supreme_forged_count(craft.supreme_forged_count());
    out.set_rare_books_crafted_count(craft.rare_books_crafted_count());
    out.set_master_showcase_tag(craft.master_showcase_tag());
    out.set_equipped_craft_title_id(ActiveCraftTitleId(craft));
    for (const auto tid : equipcraft::UnlockedCraftTitleIds(
             craft.forge_mastery_exp(), craft.scroll_mastery_exp(), craft.supreme_forged_count(),
             craft.rare_books_crafted_count(), craft.proxy_orders_completed())) {
        out.add_unlocked_craft_title_ids(tid);
    }
    out.set_team_free_proxy(craft.team_free_proxy());
    out.set_claimed_scroll_milestone_bits(craft.claimed_scroll_milestone_bits());
    std::vector<SkillBookWantedOrder> orders(craft.wanted_orders().begin(), craft.wanted_orders().end());
    std::sort(orders.begin(), orders.end(), [](const auto& a, const auto& b) { return a.order_id() < b.order_id(); });
    for (const auto& o : orders) {
        FillWantedOrderInfo(o, *out.add_my_wanted_orders());
    }
    if (craft.unread_proxy_digest_orders() > 0 || craft.unread_proxy_digest_net_gold() > 0 ||
        craft.unread_proxy_digest_new_titles_mask() != 0) {
        auto* digest = out.mutable_unread_proxy_digest();
        digest->set_orders_completed(craft.unread_proxy_digest_orders());
        digest->set_supreme_forged_count(craft.unread_proxy_digest_supreme());
        digest->set_rare_books_crafted_count(craft.unread_proxy_digest_rare());
        digest->set_net_gold_earned(craft.unread_proxy_digest_net_gold());
        digest->set_last_buyer_name(craft.unread_proxy_digest_last_buyer());
        digest->set_newly_unlocked_title_mask(craft.unread_proxy_digest_new_titles_mask());
    }
}

int FindBookIndex(const PlayerCraftComp& craft, uint64_t bookId) {
    for (int i = 0; i < craft.skill_books_size(); ++i) {
        if (craft.skill_books(i).book_id() == bookId) return i;
    }
    return -1;
}

void RemoveBookAt(PlayerCraftComp& craft, int index) {
    auto* books = craft.mutable_skill_books();
    if (index < 0 || index >= books->size()) return;
    if (index != books->size() - 1) {
        books->SwapElements(index, books->size() - 1);
    }
    books->RemoveLast();
}
} // namespace

bool PlayerCraftSystem::ValidateTables() {
    if (PlayerEquipmentV1System::ValidateQualityTable() != kSuccess) {
        LOG_ERROR << "[PlayerCraft] EquipAffixQuality table validation failed";
        return false;
    }
    const auto& scrolls = SkillScrollCraftTableManager::Instance().FindAll().data();
    if (scrolls.empty()) {
        LOG_ERROR << "[PlayerCraft] SkillScrollCraft table is empty";
        return false;
    }
    std::map<uint32_t, uint32_t> scrollRarity;
    for (const auto& scroll : scrolls) {
        if (scroll.id() == 0 || scroll.rarity_tier() > 1) {
            LOG_ERROR << "[PlayerCraft] invalid SkillScrollCraft row id=" << scroll.id()
                      << " rarity_tier=" << scroll.rarity_tier();
            return false;
        }
        if (scroll.enabled() == 0) continue;
        scrollRarity[scroll.id()] = scroll.rarity_tier();
        if (scroll.rarity_tier() == 0) {
            if (!ValidStackableMaterial(scroll.fragment_item_id(), scroll.random_fragment_count()) ||
                scroll.targeted_fragment_count() < scroll.random_fragment_count() ||
                scroll.dismantle_fragment_count() == 0 ||
                scroll.dismantle_fragment_count() > scroll.random_fragment_count()) {
                LOG_ERROR << "[PlayerCraft] invalid regular scroll recipe id=" << scroll.id();
                return false;
            }
        } else {
            if (!ValidStackableMaterial(scroll.rare_scroll_item_id(), scroll.rare_scroll_count()) ||
                !ValidStackableMaterial(scroll.rare_ink_item_id(), scroll.rare_ink_count())) {
                LOG_ERROR << "[PlayerCraft] invalid rare scroll recipe id=" << scroll.id();
                return false;
            }
        }
    }

    uint32_t totalScrollSkills = 0;
    uint32_t rareScrollSkills = 0;
    std::map<uint32_t, uint32_t> perScrollCount;
    for (const auto& skill : SkillTableManager::Instance().FindAll().data()) {
        if (skill.scroll_id() == 0) continue;
        const auto it = scrollRarity.find(skill.scroll_id());
        if (it == scrollRarity.end() || skill.season_id() != 0 || skill.battle_model() != 1 ||
            skill.rarity_tier() != it->second) {
            LOG_ERROR << "[PlayerCraft] skill scroll binding invalid skill_id=" << skill.id()
                      << " scroll_id=" << skill.scroll_id() << " rarity_tier=" << skill.rarity_tier();
            return false;
        }
        ++totalScrollSkills;
        ++perScrollCount[skill.scroll_id()];
        if (skill.rarity_tier() == 1) ++rareScrollSkills;
    }
    for (const auto& [scrollId, rarity] : scrollRarity) {
        (void)rarity;
        if (perScrollCount[scrollId] == 0) {
            LOG_ERROR << "[PlayerCraft] enabled scroll has no skills scroll_id=" << scrollId;
            return false;
        }
    }
    // 硬红线：永久技能中稀世绝学占比严锁 <= 10%。
    if (totalScrollSkills == 0 ||
        uint64_t{rareScrollSkills} * 10000u > uint64_t{totalScrollSkills} * equipcraft::kMaxRareSkillRatioBps) {
        LOG_ERROR << "[PlayerCraft] rare skill ratio exceeds 10% hard cap: rare=" << rareScrollSkills
                  << " total=" << totalScrollSkills;
        return false;
    }
    return true;
}

bool PlayerCraftSystem::IsCraftUnlockGateEnabled() {
    return gCraftUnlockGateEnabled;
}

void PlayerCraftSystem::SetCraftUnlockGateForTest(bool enabled) {
    gCraftUnlockGateEnabled = enabled;
}

void PlayerCraftSystem::SetCraftRollForTest(CraftRollFn fn) {
    gCraftRollFn = fn;
}

bool PlayerCraftSystem::IsSkillBookUnlocked(entt::entity player, uint32_t skillTableId) {
    if (skillTableId == 0 || !tlsEcs.actorRegistry.valid(player)) return false;
    const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
    if (craft == nullptr) return false;
    for (const auto id : craft->book_unlocked_skill_ids()) {
        if (id == skillTableId) return true;
    }
    return false;
}

void PlayerCraftSystem::FillMasterySnapshots(entt::entity player, SkillCraftMasterySnapshot& forgeOut,
                                             SkillCraftMasterySnapshot& scrollOut) {
    forgeOut.Clear();
    scrollOut.Clear();
    uint32_t forgeExp = 0;
    uint32_t scrollExp = 0;
    if (tlsEcs.actorRegistry.valid(player)) {
        if (const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player); craft != nullptr) {
            forgeExp = craft->forge_mastery_exp();
            scrollExp = craft->scroll_mastery_exp();
        }
    }
    const uint32_t forgeTier = equipcraft::MasteryTierForExp(forgeExp);
    forgeOut.set_tier(forgeTier);
    forgeOut.set_tier_name(equipcraft::MasteryTierName(forgeTier));
    forgeOut.set_exp(forgeExp);
    forgeOut.set_next_tier_exp(equipcraft::MasteryNextExp(forgeExp));
    forgeOut.set_bonus_rate_bps(equipcraft::ForgeTopQualityRateBps(forgeTier));

    const uint32_t scrollTier = equipcraft::MasteryTierForExp(scrollExp);
    scrollOut.set_tier(scrollTier);
    scrollOut.set_tier_name(equipcraft::MasteryTierName(scrollTier));
    scrollOut.set_exp(scrollExp);
    scrollOut.set_next_tier_exp(equipcraft::MasteryNextExp(scrollExp));
    scrollOut.set_bonus_rate_bps(equipcraft::ScrollBonusFragmentRateBps(scrollTier));
}

uint32_t PlayerCraftSystem::GetState(entt::entity player, GetSkillCraftingStateResponse& response) {
    response.Clear();
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return err;
    }
    DrainPendingProxyReputation(player);
    FillStateSnapshot(player, *response.mutable_state());
    return kSuccess;
}

void PlayerCraftSystem::CraftSkillBook(entt::entity player, const CraftSkillBookRequest& request,
                                       CraftSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kSkillScrollCraft, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_bonus_fragments_granted(first.scrollBonusFragments);
        const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
        if (const int idx = FindBookIndex(craft, first.craftedSkillBookId); idx >= 0) {
            *response.mutable_crafted_book() = craft.skill_books(idx);
        } else {
            response.mutable_crafted_book()->set_book_id(first.craftedSkillBookId);
            response.mutable_crafted_book()->set_skill_table_id(first.craftedSkillTableId);
        }
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }
    // 书架满了不制书:扣料扣银之前判;提交段核修订号未变,书架在两次检查之间不会变多。
    if (static_cast<uint32_t>(craft.skill_books_size()) >= equipcraft::kMaxBookshelfBooks) {
        fail(kSkillBookshelfFull);
        return;
    }

    const auto* scroll = ScrollRow(request.scroll_id());
    if (scroll == nullptr || scroll->enabled() == 0) {
        fail(kSkillScrollInvalid);
        return;
    }
    const auto pool = CollectScrollSkillIds(scroll->id());
    if (pool.empty()) {
        fail(kSkillScrollInvalid);
        return;
    }

    const auto mode = request.mode();
    uint32_t chosenSkillId = 0;
    ItemCountMap consumed;
    uint64_t baseGoldCost = 0;
    if (mode == CraftSkillBookRequest::RANDOM_REGULAR) {
        if (scroll->rarity_tier() != 0 || request.target_skill_table_id() != 0) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = pool[RollBelow(static_cast<uint32_t>(pool.size()))];
        consumed[scroll->fragment_item_id()] += scroll->random_fragment_count();
        baseGoldCost = scroll->random_gold_cost();
    } else if (mode == CraftSkillBookRequest::TARGETED_REGULAR) {
        if (scroll->rarity_tier() != 0 ||
            !std::binary_search(pool.begin(), pool.end(), request.target_skill_table_id())) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = request.target_skill_table_id();
        consumed[scroll->fragment_item_id()] += scroll->targeted_fragment_count();
        baseGoldCost = scroll->targeted_gold_cost();
    } else if (mode == CraftSkillBookRequest::TARGETED_RARE) {
        if (scroll->rarity_tier() != 1 ||
            !std::binary_search(pool.begin(), pool.end(), request.target_skill_table_id())) {
            fail(kSkillScrollInvalid);
            return;
        }
        chosenSkillId = request.target_skill_table_id();
        consumed[scroll->rare_scroll_item_id()] += scroll->rare_scroll_count();
        consumed[scroll->rare_ink_item_id()] += scroll->rare_ink_count();
        baseGoldCost = scroll->rare_gold_cost();
    } else {
        fail(kInvalidParameter);
        return;
    }

    // 解析跨节点/离线名匠共享代制摊位（零物品移交；买家本地原子制书与署名，95% 净银两走 asset_op 异步入账）。
    const uint64_t buyerId = tlsEcs.GetPlayerId(player);
    uint64_t proxyMasterId = request.proxy_master_player_id();
    uint64_t proxyFeeGold = 0;
    uint64_t proxyNetGold = 0;
    std::string proxyMasterName;
    uint32_t effectiveScrollTier = equipcraft::MasteryTierForExp(craft.scroll_mastery_exp());
    PlayerMailOutboxSystem::PreparedProxyFeeMail preparedProxyMail;
    if (proxyMasterId != 0) {
        CraftProxyStallInfo stall;
        if (!PlayerCraftSystem::ResolveSharedProxyStall(buyerId, proxyMasterId, false, true, stall)) {
            fail(kEquipProxyMasterUnavailable);
            return;
        }
        const entt::entity localMaster = tlsEcs.GetPlayer(proxyMasterId);
        const bool teamFree = stall.team_free_proxy() && ShareActiveTeam(player, localMaster);
        proxyFeeGold = teamFree ? 0u : stall.proxy_scroll_fee_gold();
        proxyNetGold = equipcraft::ProxyNetGold(proxyFeeGold);
        if (proxyNetGold > 0 &&
            GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                                static_cast<uint32_t>(kCurrencyGold))) {
            fail(kEquipProxyMasterUnavailable);
            return;
        }
        proxyMasterName = equipcraft::FormatMasterSignature(stall.equipped_craft_title_id(), stall.master_name());
        effectiveScrollTier = equipcraft::kMasteryMaster;
    }

    // 防洗白硬隔离：代工费(proxyFeeGold)永远只扣流通银两(kCurrencyGold)；
    // 仅稀世绝学定向制书(TARGETED_RARE，产物必定永久绑定)的系统手续费允许优先抵扣绑定银两。
    const bool allowBindForBaseFee = (mode == CraftSkillBookRequest::TARGETED_RARE);
    const uint64_t strictGold = proxyFeeGold + (allowBindForBaseFee ? 0u : baseGoldCost);
    const uint64_t preferBindGold = allowBindForBaseFee ? baseGoldCost : 0u;
    if ((strictGold > 0 || preferBindGold > 0) &&
        !CurrencySystem::CanAffordSplitGold(player, strictGold, preferBindGold)) {
        fail(kSkillGoldNotEnough);
        return;
    }
    const auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    const auto matVerdict = BagService::CheckRemovable(player, inventory, consumed);
    if (matVerdict == BagService::RemovalVerdict::kInsufficient) {
        fail(kSkillCraftMaterialNotEnough);
        return;
    }
    if (matVerdict != BagService::RemovalVerdict::kEnough) {
        fail(kFeatureUnavailable);
        return;
    }
    if (proxyMasterId != 0 && proxyNetGold > 0) {
        const auto payoutErr = PlayerMailOutboxSystem::PrepareProxyFeePayout(
            player, proxyMasterId, proxyNetGold, request.client_request_id(),
            DisplayNameOf(player, buyerId), preparedProxyMail);
        if (payoutErr != kSuccess) {
            fail(payoutErr);
            return;
        }
    }

    // 巧匠/宗师拓印常规书时按 5%/10% 概率返还 2 张绑定悟道残页（不提高稀世产出）。
    uint32_t bonusFragments = 0;
    std::vector<InitItemParam> gained;
    if (scroll->rarity_tier() == 0) {
        const uint32_t bonusRate = equipcraft::ScrollBonusFragmentRateBps(effectiveScrollTier);
        if (bonusRate > 0 && RollBelow(10000u) < bonusRate) {
            if (BagService::CheckInventoryExchangeSpace(player, consumed,
                                                        {{scroll->fragment_item_id(), equipcraft::kScrollBonusFragmentsCount}}) == kSuccess) {
                bonusFragments = equipcraft::kScrollBonusFragmentsCount;
                InitItemParam bonusParam;
                bonusParam.itemPBComp.set_item_id(kInvalidGuid);
                bonusParam.itemPBComp.set_config_id(scroll->fragment_item_id());
                bonusParam.itemPBComp.set_size(bonusFragments);
                bonusParam.itemPBComp.set_bound(true);
                gained.push_back(std::move(bonusParam));
            }
        }
    }

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    // 常规卷宗可交易残页充足时优先纯耗可交易残页，避免巧匠/宗师返还的少量绑定残页误染毒后续可交易技能书。
    const bool preferTradableOnly =
        (mode != CraftSkillBookRequest::TARGETED_RARE) &&
        BagService::RemovableTradableCount(player, inventory, scroll->fragment_item_id()) >=
            consumed.at(scroll->fragment_item_id());
    CraftedSkillBookEntry newBook;
    bool consumedBound = false;
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, consumed, gained, {}, request.client_request_id(), "{\"operation\":\"skill_scroll_craft\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            if ((strictGold > 0 || preferBindGold > 0) &&
                !CurrencySystem::CanAffordSplitGold(player, strictGold, preferBindGold)) {
                return kSkillGoldNotEnough;
            }
            if (proxyMasterId != 0) {
                CraftProxyStallInfo liveStall;
                if (!PlayerCraftSystem::ResolveSharedProxyStall(buyerId, proxyMasterId, false, true, liveStall)) {
                    return kEquipProxyMasterUnavailable;
                }
                const entt::entity localMaster = tlsEcs.GetPlayer(proxyMasterId);
                const bool liveTeamFree = liveStall.team_free_proxy() && ShareActiveTeam(player, localMaster);
                const uint64_t liveFee = liveTeamFree ? 0u : liveStall.proxy_scroll_fee_gold();
                if (liveFee != proxyFeeGold) {
                    return kEquipProxyMasterUnavailable;
                }
                if (proxyNetGold > 0 &&
                    GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                                        static_cast<uint32_t>(kCurrencyGold))) {
                    return kEquipProxyMasterUnavailable;
                }
            }

            const uint64_t bookId = craft.next_book_seq() == 0 ? 1u : craft.next_book_seq();
            newBook.Clear();
            newBook.set_book_id(bookId);
            newBook.set_skill_table_id(chosenSkillId);
            newBook.set_scroll_id(scroll->id());
            newBook.set_rarity_tier(scroll->rarity_tier());
            // 防洗白硬隔离：稀世绝学消耗天工秘墨（或任何绑定材料）产物永久仅自用绑定；常规书按实扣材料绑定性决定。
            newBook.set_bound(mode == CraftSkillBookRequest::TARGETED_RARE ? true : consumedBound);
            if (effectiveScrollTier == equipcraft::kMasteryMaster) {
                const uint64_t signerId = proxyMasterId != 0 ? proxyMasterId : buyerId;
                newBook.set_crafter_player_id(signerId);
                newBook.set_crafter_name(proxyMasterId != 0
                    ? proxyMasterName
                    : equipcraft::FormatMasterSignature(ActiveCraftTitleId(craft), DisplayNameOf(player, signerId)));
            }

            ClientRequestResult applied;
            applied.craftedSkillBookId = bookId;
            applied.craftedSkillTableId = chosenSkillId;
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = bonusFragments;

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kSkillScrollCraft, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            if (strictGold > 0 || preferBindGold > 0) {
                const auto deductErr = CurrencySystem::DeductSplitGold(
                    player, strictGold, preferBindGold, TX_SKILL_SCROLL_CRAFT, request.client_request_id());
                if (deductErr != kSuccess) return kSkillGoldNotEnough;
            }
            if (proxyMasterId != 0 && proxyNetGold > 0) {
                // 95% 净银两写入买家同事务 WALLET 持久组的待寄邮件出箱，经邮件服务 asset_op 异步入账。
                PlayerMailOutboxSystem::CommitPreparedProxyFee(player, preparedProxyMail,
                                                               TimeSystem::NowMillisecondsUTC());
            }
            *craft.add_skill_books() = newBook;
            craft.set_next_book_seq(bookId + 1);
            craft.set_scroll_craft_revision(revBefore + 1);
            if (proxyMasterId == 0) {
                if (craft.scroll_mastery_exp() < std::numeric_limits<uint32_t>::max()) {
                    craft.set_scroll_mastery_exp(craft.scroll_mastery_exp() + 1);
                }
                if (mode == CraftSkillBookRequest::TARGETED_RARE &&
                    craft.rare_books_crafted_count() < std::numeric_limits<uint32_t>::max()) {
                    craft.set_rare_books_crafted_count(craft.rare_books_crafted_count() + 1u);
                }
            }
            ledger.Swap(&nextLedger);
            return kSuccess;
        }, preferTradableOnly, &consumedBound);

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    if (proxyMasterId != 0) {
        PlayerCraftSystem::RecordProxyOrderReputation(
            proxyMasterId, 0u, mode == CraftSkillBookRequest::TARGETED_RARE ? 1u : 0u,
            proxyNetGold, BuyerDisplayNameOf(player, buyerId));
    }
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_scroll_craft");
    *response.mutable_crafted_book() = newBook;
    response.set_bonus_fragments_granted(bonusFragments);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::LearnSkillBook(entt::entity player, const LearnSkillBookRequest& request,
                                       LearnSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kLearnSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_learned_skill_table_id(first.craftedSkillTableId);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const uint32_t skillId = craft.skill_books(bookIdx).skill_table_id();
    const auto* row = SkillRow(skillId);
    if (row == nullptr || row->season_id() != 0 || row->battle_model() != 1) {
        fail(kInvalidTableId);
        return;
    }

    const uint32_t requiredMask = request.pet_id() == 0 ? 1u : 2u;
    if ((row->caster_mask() & requiredMask) == 0) {
        fail(kSkillNotInClass);
        return;
    }
    if (IsSkillBookUnlocked(player, skillId)) {
        fail(kSkillAlreadyBookUnlocked);
        return;
    }

    if (request.pet_id() == 0) {
        if (tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player) == nullptr) {
            fail(kServiceUnavailable);
            return;
        }
    } else {
        // 先校验宝宝存在性与可写状态，确保后面不发生半写。
        if (const auto petErr = PetSystem::CheckCanUnlockSkillFromBook(player, request.pet_id());
            petErr != kSuccess) {
            fail(petErr);
            return;
        }
    }

    ClientRequestResult applied;
    applied.craftedSkillBookId = request.book_id();
    applied.craftedSkillTableId = skillId;
    applied.scrollCraftRevision = craft.scroll_craft_revision() + 1;

    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kLearnSkillBook, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    RemoveBookAt(craft, bookIdx);
    craft.add_book_unlocked_skill_ids(skillId);
    craft.set_scroll_craft_revision(applied.scrollCraftRevision);

    // 永久解锁且绝不顶掉任何已有技能。
    if (request.pet_id() == 0) {
        auto& skillList = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
        int existingIdx = -1;
        for (int i = 0; i < skillList.skill_list_size(); ++i) {
            if (skillList.skill_list(i).skill_table_id() == skillId) {
                existingIdx = i;
                break;
            }
        }
        if (existingIdx < 0) {
            auto* added = skillList.add_skill_list();
            added->set_skill_table_id(skillId);
            added->set_rank(1);
        } else if (skillList.skill_list(existingIdx).rank() == 0) {
            skillList.mutable_skill_list(existingIdx)->set_rank(1);
        }
        if (PlayerSkillSystem::IsPassiveSkill(*row)) {
            PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kSkillChanged);
            PlayerAttributeSystem::PushPanel(player);
        }
    } else {
        (void)PetSystem::UnlockSkillFromBook(player, request.pet_id(), skillId);
    }

    ledger.Swap(&nextLedger);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_learned");
    response.set_learned_skill_table_id(skillId);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::DismantleSkillBook(entt::entity player, const DismantleSkillBookRequest& request,
                                           DismantleSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kDismantleSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_refunded_fragments(first.scrollBonusFragments);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const CraftedSkillBookEntry book = craft.skill_books(bookIdx);
    if (book.rarity_tier() != 0) {
        fail(kSkillBookNotDismantlable);
        return;
    }
    const auto* scroll = ScrollRow(book.scroll_id());
    if (scroll == nullptr || scroll->dismantle_fragment_count() == 0 ||
        !ValidStackableMaterial(scroll->fragment_item_id(), scroll->dismantle_fragment_count())) {
        fail(kSkillBookNotDismantlable);
        return;
    }

    InitItemParam refundParam;
    refundParam.itemPBComp.set_item_id(kInvalidGuid);
    refundParam.itemPBComp.set_config_id(scroll->fragment_item_id());
    refundParam.itemPBComp.set_size(scroll->dismantle_fragment_count());
    refundParam.itemPBComp.set_bound(book.bound());

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, {refundParam}, {}, request.client_request_id(), "{\"operation\":\"skill_book_dismantle\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            const int currentIdx = FindBookIndex(craft, request.book_id());
            if (currentIdx < 0) return kSkillBookNotFound;

            ClientRequestResult applied;
            applied.craftedSkillBookId = book.book_id();
            applied.craftedSkillTableId = book.skill_table_id();
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = scroll->dismantle_fragment_count();

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kDismantleSkillBook, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            RemoveBookAt(craft, currentIdx);
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_dismantled");
    response.set_refunded_fragments(scroll->dismantle_fragment_count());
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::SealSkillBook(entt::entity player, const SealSkillBookRequest& request,
                                      SealSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kSealSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_item_id(first.sealedBookItemId);
        response.set_skill_table_id(first.craftedSkillTableId);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int bookIdx = FindBookIndex(craft, request.book_id());
    if (bookIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const CraftedSkillBookEntry book = craft.skill_books(bookIdx);
    // 防洗白硬隔离：仅未绑定常规技能书允许封卷入包流通；稀世绝学或绑定书一律拒绝。
    if (book.bound() || book.rarity_tier() != 0 || book.skill_table_id() == 0 ||
        !SkillTableManager::Instance().Exists(book.skill_table_id())) {
        fail(kInvalidParameter);
        return;
    }
    const auto* sealedItemRow = ItemTableManager::Instance().FindByIdSilent(equipcraft::kSealedSkillBookConfigId).first;
    if (sealedItemRow == nullptr || sealedItemRow->max_stack_size() != 1 || sealedItemRow->equip_kind() != 0) {
        fail(kInvalidTableData);
        return;
    }

    InitItemParam sealedParam;
    sealedParam.itemPBComp.set_item_id(kInvalidGuid);
    sealedParam.itemPBComp.set_config_id(equipcraft::kSealedSkillBookConfigId);
    sealedParam.itemPBComp.set_size(1);
    sealedParam.itemPBComp.set_bound(false);
    auto* sealedData = sealedParam.itemPBComp.mutable_sealed_skill_book();
    sealedData->set_skill_table_id(book.skill_table_id());
    sealedData->set_scroll_id(book.scroll_id());
    sealedData->set_rarity_tier(0);
    if (!book.crafter_name().empty()) sealedData->set_crafter_name(book.crafter_name());
    if (book.crafter_player_id() != 0) sealedData->set_crafter_player_id(book.crafter_player_id());

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    Guid createdItemId = 0;
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, {sealedParam}, {}, request.client_request_id(), "{\"operation\":\"skill_book_seal\"}", written,
        [&](const std::vector<GuidVector>& createdGuids) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            if (createdGuids.size() != 1 || createdGuids[0].size() != 1 || createdGuids[0][0] == 0) {
                return kServiceUnavailable;
            }
            const int currentIdx = FindBookIndex(craft, request.book_id());
            if (currentIdx < 0) return kSkillBookNotFound;

            createdItemId = createdGuids[0][0];
            ClientRequestResult applied;
            applied.craftedSkillBookId = book.book_id();
            applied.craftedSkillTableId = book.skill_table_id();
            applied.scrollCraftRevision = revBefore + 1;
            applied.sealedBookItemId = createdItemId;

            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kSealSkillBook, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            RemoveBookAt(craft, currentIdx);
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });

    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_sealed");
    response.set_item_id(createdItemId);
    response.set_skill_table_id(book.skill_table_id());
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::UnsealSkillBook(entt::entity player, const UnsealSkillBookRequest& request,
                                        UnsealSkillBookResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.item_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kUnsealSkillBook, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
        if (const int idx = FindBookIndex(craft, first.craftedSkillBookId); idx >= 0) {
            *response.mutable_unsealed_book() = craft.skill_books(idx);
        } else {
            response.mutable_unsealed_book()->set_book_id(first.craftedSkillBookId);
            response.mutable_unsealed_book()->set_skill_table_id(first.craftedSkillTableId);
        }
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }

    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        craft.next_book_seq() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const auto* item = inventory.GetItemCompByGuid(request.item_id());
    if (item == nullptr) {
        fail(kSkillBookNotFound);
        return;
    }
    if (item->config_id() != equipcraft::kSealedSkillBookConfigId || item->size() != 1 ||
        !item->has_sealed_skill_book()) {
        fail(kInvalidParameter);
        return;
    }
    const auto& sealed = item->sealed_skill_book();
    if (sealed.skill_table_id() == 0 || sealed.rarity_tier() != 0 ||
        !SkillTableManager::Instance().Exists(sealed.skill_table_id())) {
        fail(kInvalidParameter);
        return;
    }
    if (BagService::CheckRemovableInstances(player, inventory, {request.item_id()}) != BagService::RemovalVerdict::kEnough) {
        fail(kFeatureUnavailable);
        return;
    }
    // 书架满了不开卷,封卷道籍留在背包里。
    if (static_cast<uint32_t>(craft.skill_books_size()) >= equipcraft::kMaxBookshelfBooks) {
        fail(kSkillBookshelfFull);
        return;
    }

    const uint64_t bookId = craft.next_book_seq() == 0 ? 1u : craft.next_book_seq();
    const uint64_t revBefore = craft.scroll_craft_revision();
    CraftedSkillBookEntry newBook;
    newBook.set_book_id(bookId);
    newBook.set_skill_table_id(sealed.skill_table_id());
    newBook.set_scroll_id(sealed.scroll_id());
    newBook.set_rarity_tier(0);
    // 防洗白：若封卷道具本身带绑定标记，开卷回书架后严格继承绑定。
    newBook.set_bound(item->bound());
    if (!sealed.crafter_name().empty()) newBook.set_crafter_name(sealed.crafter_name());
    if (sealed.crafter_player_id() != 0) newBook.set_crafter_player_id(sealed.crafter_player_id());

    ClientRequestResult applied;
    applied.craftedSkillBookId = bookId;
    applied.craftedSkillTableId = sealed.skill_table_id();
    applied.scrollCraftRevision = revBefore + 1;
    applied.sealedBookItemId = request.item_id();

    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kUnsealSkillBook, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    const auto removeErr = BagService::RemoveItem(player, inventory, request.item_id());
    if (removeErr != kSuccess) {
        fail(removeErr);
        return;
    }

    *craft.add_skill_books() = newBook;
    craft.set_next_book_seq(bookId + 1);
    craft.set_scroll_craft_revision(revBefore + 1);
    ledger.Swap(&nextLedger);

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_book_unsealed");
    *response.mutable_unsealed_book() = newBook;
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::SetProxyStall(entt::entity player, const SetCraftProxyStallRequest& request,
                                      SetCraftProxyStallResponse& response) {
    response.Clear();
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    if (request.master_showcase_tag() > equipcraft::kMaxMasterShowcaseTag) {
        fail(kInvalidParameter);
        return;
    }
    if (request.equipped_craft_title_id() != 0 &&
        !equipcraft::IsCraftTitleUnlocked(request.equipped_craft_title_id(), craft.forge_mastery_exp(),
                                          craft.scroll_mastery_exp(), craft.supreme_forged_count(),
                                          craft.rare_books_crafted_count(), craft.proxy_orders_completed())) {
        fail(kInvalidParameter);
        return;
    }

    const bool forgeMaster = equipcraft::MasteryTierForExp(craft.forge_mastery_exp()) >= equipcraft::kMasteryMaster;
    const bool scrollMaster = equipcraft::MasteryTierForExp(craft.scroll_mastery_exp()) >= equipcraft::kMasteryMaster;
    if (!request.open()) {
        craft.set_proxy_stall_open(false);
        if (forgeMaster || scrollMaster) {
            craft.set_master_showcase_tag(request.master_showcase_tag());
            craft.set_equipped_craft_title_id(request.equipped_craft_title_id());
            craft.set_team_free_proxy(request.team_free_proxy());
        }
        SyncPlayerProxyStall(player);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "craft_proxy_stall_closed");
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }

    if (!forgeMaster && !scrollMaster) {
        fail(kSkillProxyStallRequiresMaster);
        return;
    }
    if (forgeMaster && !equipcraft::ValidProxyFee(request.proxy_forge_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (!forgeMaster && request.proxy_forge_fee_gold() != 0 && !equipcraft::ValidProxyFee(request.proxy_forge_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (scrollMaster && !equipcraft::ValidProxyFee(request.proxy_scroll_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }
    if (!scrollMaster && request.proxy_scroll_fee_gold() != 0 && !equipcraft::ValidProxyFee(request.proxy_scroll_fee_gold())) {
        fail(kSkillProxyFeeInvalid);
        return;
    }

    craft.set_proxy_stall_open(true);
    craft.set_proxy_forge_fee_gold(forgeMaster ? request.proxy_forge_fee_gold() : 0u);
    craft.set_proxy_scroll_fee_gold(scrollMaster ? request.proxy_scroll_fee_gold() : 0u);
    craft.set_master_showcase_tag(request.master_showcase_tag());
    craft.set_equipped_craft_title_id(request.equipped_craft_title_id());
    craft.set_team_free_proxy(request.team_free_proxy());
    SyncPlayerProxyStall(player);
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "craft_proxy_stall_opened");
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::ListProxyStalls(entt::entity player, ListCraftProxyStallsResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    std::map<uint64_t, CraftProxyStallInfo> merged = tlsProxyStallDirectory;
    for (const auto& [pid, entity] : tlsEcs.playerList) {
        if (pid == 0 || !tlsEcs.actorRegistry.valid(entity)) continue;
        if (!PlayerLifecycleSystem::IsPlayerMutable(entity)) {
            merged.erase(pid);
            continue;
        }
        CraftProxyStallInfo live;
        if (BuildLocalStallSnapshot(entity, pid, live)) {
            merged[pid] = std::move(live);
        } else {
            merged.erase(pid);
        }
    }
    std::vector<CraftProxyStallInfo> rows;
    rows.reserve(merged.size());
    for (auto& [pid, stall] : merged) {
        if (!ValidSharedStall(stall)) continue;
        if (const auto pit = tlsPendingProxyReputation.find(pid); pit != tlsPendingProxyReputation.end()) {
            stall.set_proxy_orders_completed(SaturatingAddU32(stall.proxy_orders_completed(), pit->second.orders));
            stall.set_supreme_forged_count(SaturatingAddU32(stall.supreme_forged_count(), pit->second.supreme));
            stall.set_rare_books_crafted_count(SaturatingAddU32(stall.rare_books_crafted_count(), pit->second.rare));
        }
        if (const auto hit = tlsHotProxyCounters.find(pid); hit != tlsHotProxyCounters.end()) {
            stall.set_recent_24h_orders(hit->second.orders24h);
            stall.set_recent_24h_supreme(hit->second.supreme24h);
        } else {
            stall.set_recent_24h_orders(0u);
            stall.set_recent_24h_supreme(0u);
        }
        stall.set_hot_rank(0u);
        const entt::entity localMaster = tlsEcs.GetPlayer(pid);
        stall.set_viewer_gets_free_proxy(stall.team_free_proxy() && ShareActiveTeam(player, localMaster));
        rows.push_back(std::move(stall));
    }

    // 评选全区西市「极品热榜」Top 3（热榜分 = 近 24h 极品*3 + 近 24h 单数，再按累计极品与口碑稳定破平局）。
    std::vector<std::size_t> rankIdx;
    rankIdx.reserve(rows.size());
    for (std::size_t i = 0; i < rows.size(); ++i) {
        const auto& s = rows[i];
        if (s.recent_24h_orders() > 0 || s.proxy_orders_completed() > 0 || s.supreme_forged_count() > 0) {
            rankIdx.push_back(i);
        }
    }
    std::sort(rankIdx.begin(), rankIdx.end(), [&](std::size_t a, std::size_t b) {
        const auto& sa = rows[a];
        const auto& sb = rows[b];
        const uint64_t scoreA = equipcraft::HotRankScore(sa.recent_24h_orders(), sa.recent_24h_supreme());
        const uint64_t scoreB = equipcraft::HotRankScore(sb.recent_24h_orders(), sb.recent_24h_supreme());
        if (scoreA != scoreB) return scoreA > scoreB;
        if (sa.recent_24h_supreme() != sb.recent_24h_supreme()) return sa.recent_24h_supreme() > sb.recent_24h_supreme();
        if (sa.supreme_forged_count() != sb.supreme_forged_count()) return sa.supreme_forged_count() > sb.supreme_forged_count();
        if (sa.proxy_orders_completed() != sb.proxy_orders_completed()) return sa.proxy_orders_completed() > sb.proxy_orders_completed();
        return sa.master_player_id() < sb.master_player_id();
    });
    for (std::size_t r = 0; r < rankIdx.size() && r < equipcraft::kHotRankTopLimit; ++r) {
        rows[rankIdx[r]].set_hot_rank(static_cast<uint32_t>(r + 1u));
    }
    for (auto& stall : rows) {
        *response.add_stalls() = std::move(stall);
    }
}

void PlayerCraftSystem::AckProxyDigest(entt::entity player, const AckCraftProxyDigestRequest&,
                                       AckCraftProxyDigestResponse& response) {
    response.Clear();
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }
    DrainPendingProxyReputation(player);
    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    const bool hadDigest = craft.unread_proxy_digest_orders() > 0 ||
        craft.unread_proxy_digest_supreme() > 0 ||
        craft.unread_proxy_digest_rare() > 0 ||
        craft.unread_proxy_digest_net_gold() > 0 ||
        !craft.unread_proxy_digest_last_buyer().empty() ||
        craft.unread_proxy_digest_new_titles_mask() != 0;
    if (hadDigest) {
        craft.set_unread_proxy_digest_orders(0u);
        craft.set_unread_proxy_digest_supreme(0u);
        craft.set_unread_proxy_digest_rare(0u);
        craft.set_unread_proxy_digest_net_gold(0u);
        craft.clear_unread_proxy_digest_last_buyer();
        craft.set_unread_proxy_digest_new_titles_mask(0u);
        PlayerLifecycleSystem::RequestKeyProgressSave(player, "craft_proxy_digest_acked");
    }
    FillStateSnapshot(player, *response.mutable_state());
}

std::string PlayerCraftSystem::ProxyStallDirectoryKey(uint32_t zoneId) {
    return "craft_proxy_stall:" + redis_slot::ZoneControlTag(redis_slot::ControlFamily::kSceneControl, zoneId);
}

std::string PlayerCraftSystem::ProxyReputationLedgerKey(uint32_t zoneId) {
    return "craft_proxy_reputation:" + redis_slot::ZoneControlTag(redis_slot::ControlFamily::kSceneControl, zoneId);
}

void PlayerCraftSystem::StartProxyStallDirectory(muduo::net::EventLoop* loop) {
    if (loop == nullptr) {
        LOG_ERROR << "[CraftProxyDir] StartProxyStallDirectory: loop is null";
        return;
    }
    if (gProxyDirectoryTimerActive && gProxyDirectoryLoop != nullptr) {
        gProxyDirectoryLoop->cancel(gProxyDirectoryTimerId);
        gProxyDirectoryTimerActive = false;
    }
    gProxyDirectoryLoop = loop;
    gProxyDirectoryTimerId = loop->runEvery(kProxyDirectoryRefreshIntervalSec, [] { RequestProxyDirectoryRefresh(); });
    gProxyDirectoryTimerActive = true;
    RequestProxyDirectoryRefresh();
    LOG_INFO << "[CraftProxyDir] Proxy stall directory refresh started, interval=" << kProxyDirectoryRefreshIntervalSec
             << "s";
}

void PlayerCraftSystem::StopProxyStallDirectory() {
    if (!gProxyDirectoryTimerActive || gProxyDirectoryLoop == nullptr) return;
    gProxyDirectoryLoop->cancel(gProxyDirectoryTimerId);
    gProxyDirectoryTimerActive = false;
    LOG_INFO << "[CraftProxyDir] Proxy stall directory refresh stopped";
}

void PlayerCraftSystem::RecordProxyOrderReputation(uint64_t masterPlayerId, uint32_t supremeDelta, uint32_t rareDelta,
                                                   uint64_t netGoldEarned, const std::string& buyerName) {
    if (masterPlayerId == 0) return;
    auto& hot = tlsHotProxyCounters[masterPlayerId];
    hot.orders24h = SaturatingAddU32(hot.orders24h, 1u);
    hot.supreme24h = SaturatingAddU32(hot.supreme24h, supremeDelta);
    Ledger().add(masterPlayerId, ":24o", 1);
    if (supremeDelta > 0) {
        Ledger().add(masterPlayerId, ":24s", static_cast<int64_t>(supremeDelta));
    }

    const entt::entity localMaster = tlsEcs.GetPlayer(masterPlayerId);
    if (tlsEcs.actorRegistry.valid(localMaster) && PlayerLifecycleSystem::IsPlayerMutable(localMaster)) {
        if (auto* masterCraft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(localMaster);
            masterCraft != nullptr) {
            ApplyReputationToCraftComp(*masterCraft, 1u, supremeDelta, rareDelta, netGoldEarned, buyerName);
            SyncPlayerProxyStall(localMaster);
            PlayerLifecycleSystem::RequestKeyProgressSave(localMaster, "craft_proxy_reputation_updated");
            return;
        }
    }

    // 宗师离线或在跨节点：写入共享待回账台账（本地镜像 + Redis HINCRBY 原子累加）。
    auto& pending = tlsPendingProxyReputation[masterPlayerId];
    pending.orders = SaturatingAddU32(pending.orders, 1u);
    pending.supreme = SaturatingAddU32(pending.supreme, supremeDelta);
    pending.rare = SaturatingAddU32(pending.rare, rareDelta);
    pending.netGold = SaturatingAddU64(pending.netGold, netGoldEarned);
    if (!buyerName.empty()) {
        pending.lastBuyer = buyerName;
    }
    Ledger().add(masterPlayerId, ":o", 1);
    if (supremeDelta > 0) Ledger().add(masterPlayerId, ":s", static_cast<int64_t>(supremeDelta));
    if (rareDelta > 0) Ledger().add(masterPlayerId, ":r", static_cast<int64_t>(rareDelta));
    if (netGoldEarned > 0 && netGoldEarned <= kMaxPayableGold) {
        Ledger().add(masterPlayerId, ":g", static_cast<int64_t>(netGoldEarned));
    }
    Ledger().setLastBuyer(masterPlayerId, buyerName);
}

bool PlayerCraftSystem::DrainPendingProxyReputation(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player) || !PlayerLifecycleSystem::IsPlayerMutable(player)) return false;
    const uint64_t pid = tlsEcs.GetPlayerId(player);
    if (pid == 0 || tlsProxyReputationTakesInFlight.count(pid) > 0) return false;
    if (tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player) == nullptr) return false;
    const auto it = tlsPendingProxyReputation.find(pid);
    if (it == tlsPendingProxyReputation.end()) return false;
    const PendingProxyReputation& hint = it->second;
    if (hint.orders == 0 && hint.supreme == 0 && hint.rare == 0 && hint.netGold == 0) {
        tlsPendingProxyReputation.erase(it);
        return false;
    }

    // 镜像只说明「可能有待回账」:它可能过时(别的节点已回过账,或在途的整表刷新带回了回账前的值),照它的数额入账
    // 会把同一批口碑记两次(r2-craft#4)。数额以原子取走的为准,入账与「还回去」都在应答里做。
    tlsProxyReputationTakesInFlight.insert(pid);
    const bool sent = Ledger().take(pid, [pid](std::optional<TakenProxyReputation> taken) {
        ApplyTakenProxyReputation(pid, taken);
        tlsProxyReputationTakesInFlight.erase(pid);
    });
    if (!sent) {
        tlsProxyReputationTakesInFlight.erase(pid);
        return false;
    }
    return true;
}

void PlayerCraftSystem::SyncPlayerProxyStall(entt::entity player) {
    if (!tlsEcs.actorRegistry.valid(player)) return;
    const uint64_t pid = tlsEcs.GetPlayerId(player);
    if (pid == 0) return;
    DrainPendingProxyReputation(player);
    CraftProxyStallInfo snapshot;
    if (BuildLocalStallSnapshot(player, pid, snapshot)) {
        tlsProxyStallDirectory[pid] = snapshot;
        PublishStallToRedis(pid, snapshot);
        return;
    }
    if (tlsProxyStallDirectory.erase(pid) > 0) {
        RemoveStallFromRedis(pid);
    } else if (const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(player);
               craft != nullptr && !craft->proxy_stall_open()) {
        RemoveStallFromRedis(pid);
    }
}

bool PlayerCraftSystem::ResolveSharedProxyStall(uint64_t buyerPlayerId, uint64_t masterPlayerId,
                                                bool requireForge, bool requireScroll,
                                                CraftProxyStallInfo& out) {
    out.Clear();
    if (masterPlayerId == 0 || masterPlayerId == buyerPlayerId) return false;
    const entt::entity localMaster = tlsEcs.GetPlayer(masterPlayerId);
    if (tlsEcs.actorRegistry.valid(localMaster)) {
        if (!PlayerLifecycleSystem::IsPlayerMutable(localMaster)) return false;
        if (!BuildLocalStallSnapshot(localMaster, masterPlayerId, out)) {
            tlsProxyStallDirectory.erase(masterPlayerId);
            return false;
        }
        tlsProxyStallDirectory[masterPlayerId] = out;
    } else {
        const auto it = tlsProxyStallDirectory.find(masterPlayerId);
        if (it == tlsProxyStallDirectory.end() || !ValidSharedStall(it->second)) return false;
        out = it->second;
    }
    if (requireForge && (out.forge_mastery_tier() < equipcraft::kMasteryMaster ||
                         !equipcraft::ValidProxyFee(out.proxy_forge_fee_gold()))) {
        out.Clear();
        return false;
    }
    if (requireScroll && (out.scroll_mastery_tier() < equipcraft::kMasteryMaster ||
                          !equipcraft::ValidProxyFee(out.proxy_scroll_fee_gold()))) {
        out.Clear();
        return false;
    }
    return true;
}

void PlayerCraftSystem::SetPersistForTest(PersistFn fn) {
    gCraftPersistFn = fn == nullptr ? &PlayerLifecycleSystem::SavePlayerForAssetCommit : fn;
}

void PlayerCraftSystem::UpsertProxyStallForTest(const CraftProxyStallInfo& stall) {
    if (ValidSharedStall(stall)) {
        CraftProxyStallInfo clean = stall;
        clean.set_viewer_gets_free_proxy(false);
        tlsProxyStallDirectory[clean.master_player_id()] = std::move(clean);
    }
}

void PlayerCraftSystem::RemoveProxyStallForTest(uint64_t masterPlayerId) {
    tlsProxyStallDirectory.erase(masterPlayerId);
    tlsPendingProxyReputation.erase(masterPlayerId);
    tlsHotProxyCounters.erase(masterPlayerId);
    tlsProxyReputationTakesInFlight.erase(masterPlayerId);
}

void PlayerCraftSystem::ClearProxyStallDirectoryForTest() {
    tlsProxyStallDirectory.clear();
    tlsPendingProxyReputation.clear();
    tlsHotProxyCounters.clear();
    tlsProxyReputationTakesInFlight.clear();
}

void PlayerCraftSystem::SetProxyReputationLedgerForTest(ProxyReputationLedger ledger) {
    Ledger() = WithDefaultLedger(std::move(ledger));
}

void PlayerCraftSystem::ClaimScrollCollectionMilestone(entt::entity player,
                                                       const ClaimScrollCollectionMilestoneRequest& request,
                                                       ClaimScrollCollectionMilestoneResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.scroll_id() == 0 ||
        (request.milestone_tier() != equipcraft::kScrollMilestoneHalfTier &&
         request.milestone_tier() != equipcraft::kScrollMilestoneFullTier)) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kClaimScrollMilestone, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_rewarded_fragments(first.scrollBonusFragments);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const auto* scroll = ScrollRow(request.scroll_id());
    if (scroll == nullptr || scroll->enabled() == 0) {
        fail(kSkillScrollInvalid);
        return;
    }
    const auto scrollSkills = CollectScrollSkillIds(request.scroll_id());
    if (scrollSkills.empty()) {
        fail(kSkillScrollInvalid);
        return;
    }
    std::set<uint32_t> unlocked(craft.book_unlocked_skill_ids().begin(), craft.book_unlocked_skill_ids().end());
    uint32_t unlockedInScroll = 0;
    for (const auto sid : scrollSkills) {
        if (unlocked.count(sid) != 0) ++unlockedInScroll;
    }
    const uint32_t required = request.milestone_tier() == equipcraft::kScrollMilestoneHalfTier
        ? equipcraft::ScrollHalfThreshold(static_cast<uint32_t>(scrollSkills.size()))
        : static_cast<uint32_t>(scrollSkills.size());
    if (unlockedInScroll < required) {
        fail(kInvalidParameter);
        return;
    }
    const uint32_t bitIdx = equipcraft::ScrollMilestoneBitIndex(request.scroll_id(), request.milestone_tier());
    if (equipcraft::HasBit(craft.claimed_scroll_milestone_bits(), bitIdx)) {
        fail(kInvalidParameter);
        return;
    }

    const uint32_t rewardCount = equipcraft::ScrollMilestoneRewardFragments(request.milestone_tier());
    const uint32_t fragItemId = scroll->fragment_item_id() != 0
        ? scroll->fragment_item_id() : equipcraft::kScrollFragmentConfigId;
    if (!ValidStackableMaterial(fragItemId, rewardCount)) {
        fail(kInvalidTableData);
        return;
    }
    if (const auto spaceErr = BagService::CheckInventoryExchangeSpace(player, {}, {{fragItemId, rewardCount}}, false, true);
        spaceErr != kSuccess) {
        fail(spaceErr);
        return;
    }

    std::vector<InitItemParam> gained;
    InitItemParam param;
    param.itemPBComp.set_item_id(kInvalidGuid);
    param.itemPBComp.set_config_id(fragItemId);
    param.itemPBComp.set_size(rewardCount);
    param.itemPBComp.set_bound(true);
    gained.push_back(std::move(param));

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, gained, {}, request.client_request_id(), "{\"operation\":\"claim_scroll_milestone\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess ||
                equipcraft::HasBit(craft.claimed_scroll_milestone_bits(), bitIdx)) {
                return kFeatureUnavailable;
            }
            ClientRequestResult applied;
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = rewardCount;
            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kClaimScrollMilestone, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            equipcraft::SetBit(*craft.mutable_claimed_scroll_milestone_bits(), bitIdx);
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });
    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "scroll_milestone_claimed");
    response.set_rewarded_fragments(rewardCount);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::BatchDismantleSkillBooks(entt::entity player, const BatchDismantleSkillBooksRequest& request,
                                                 BatchDismantleSkillBooksResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.book_ids_size() <= 0 ||
        static_cast<uint32_t>(request.book_ids_size()) > equipcraft::kMaxBatchDismantleCount) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kBatchDismantleSkillBooks, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_dismantled_count(first.craftedSkillTableId);
        response.set_refunded_fragments(first.scrollBonusFragments);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    std::set<uint64_t> uniqueBookIds;
    std::map<uint32_t, uint32_t> refundByItem;
    uint32_t totalRefund = 0;
    for (const auto bookId : request.book_ids()) {
        if (bookId == 0 || !uniqueBookIds.insert(bookId).second) {
            fail(kInvalidParameter);
            return;
        }
        const int idx = FindBookIndex(craft, bookId);
        if (idx < 0) {
            fail(kSkillBookNotFound);
            return;
        }
        const auto& book = craft.skill_books(idx);
        if (book.rarity_tier() != 0) {
            fail(kSkillBookNotDismantlable);
            return;
        }
        const auto* scroll = ScrollRow(book.scroll_id());
        if (scroll == nullptr || scroll->dismantle_fragment_count() == 0 ||
            !ValidStackableMaterial(scroll->fragment_item_id(), scroll->dismantle_fragment_count())) {
            fail(kSkillBookNotDismantlable);
            return;
        }
        refundByItem[scroll->fragment_item_id()] += scroll->dismantle_fragment_count();
        totalRefund += scroll->dismantle_fragment_count();
    }

    ItemCountMap gainedCounts;
    std::vector<InitItemParam> gained;
    for (const auto& [itemId, count] : refundByItem) {
        gainedCounts[itemId] = count;
        InitItemParam param;
        param.itemPBComp.set_item_id(kInvalidGuid);
        param.itemPBComp.set_config_id(itemId);
        param.itemPBComp.set_size(count);
        param.itemPBComp.set_bound(true);
        gained.push_back(std::move(param));
    }
    if (const auto spaceErr = BagService::CheckInventoryExchangeSpace(player, {}, gainedCounts, false, true);
        spaceErr != kSuccess) {
        fail(spaceErr);
        return;
    }

    const auto ledgerBefore = ledger.SerializeAsString();
    const uint64_t revBefore = craft.scroll_craft_revision();
    const uint32_t dismantledCount = static_cast<uint32_t>(uniqueBookIds.size());
    std::vector<GuidVector> written;
    const auto exErr = BagService::ExchangeInventoryExact(
        player, {}, gained, {}, request.client_request_id(), "{\"operation\":\"batch_dismantle_skill_books\"}", written,
        [&](const std::vector<GuidVector>&) -> uint32_t {
            if (ledger.SerializeAsString() != ledgerBefore || craft.scroll_craft_revision() != revBefore ||
                CheckWritable(player) != kSuccess) {
                return kFeatureUnavailable;
            }
            for (const auto bookId : uniqueBookIds) {
                if (FindBookIndex(craft, bookId) < 0) return kSkillBookNotFound;
            }
            ClientRequestResult applied;
            applied.craftedSkillTableId = dismantledCount;
            applied.scrollCraftRevision = revBefore + 1;
            applied.scrollBonusFragments = totalRefund;
            ClientRequestLedgerComp nextLedger = ledger;
            if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                                 ClientRequestKind::kBatchDismantleSkillBooks, fingerprint, applied) ||
                !client_request_ledger::Validate(nextLedger).empty()) {
                return kServiceUnavailable;
            }
            for (const auto bookId : uniqueBookIds) {
                RemoveBookAt(craft, FindBookIndex(craft, bookId));
            }
            craft.set_scroll_craft_revision(revBefore + 1);
            ledger.Swap(&nextLedger);
            return kSuccess;
        });
    if (exErr != kSuccess) {
        fail(exErr);
        return;
    }

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_books_batch_dismantled");
    response.set_dismantled_count(dismantledCount);
    response.set_refunded_fragments(totalRefund);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::PostWantedOrder(entt::entity player, const PostSkillBookWantedOrderRequest& request,
                                        PostSkillBookWantedOrderResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.skill_table_id() == 0 ||
        !equipcraft::ValidWantedOrderGold(request.reward_gold())) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kPostWantedOrder, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        const auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
        if (const int idx = FindWantedOrderIndex(craft, first.wantedOrderId); idx >= 0) {
            FillWantedOrderInfo(craft.wanted_orders(idx), *response.mutable_order());
        } else {
            response.mutable_order()->set_order_id(first.wantedOrderId);
            response.mutable_order()->set_reward_gold(first.wantedOrderGold);
        }
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        craft.next_wanted_order_seq() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }
    if (static_cast<uint32_t>(craft.wanted_orders_size()) >= equipcraft::kMaxActiveWantedOrders) {
        fail(kInvalidParameter);
        return;
    }

    const auto* skill = SkillRow(request.skill_table_id());
    if (skill == nullptr || skill->season_id() != 0 || skill->battle_model() != 1 || skill->rarity_tier() != 0) {
        fail(kSkillScrollInvalid);
        return;
    }
    const auto* scroll = ScrollRow(skill->scroll_id());
    if (scroll == nullptr || scroll->enabled() == 0 || scroll->rarity_tier() != 0) {
        fail(kSkillScrollInvalid);
        return;
    }
    // 防洗白硬隔离：悬赏求书严格仅允许托管流通银两（禁止用绑定银两发榜撤单或小号揭榜套现）。
    if (!CurrencySystem::CanAffordSplitGold(player, request.reward_gold(), 0u)) {
        fail(kSkillGoldNotEnough);
        return;
    }

    const uint64_t orderId = craft.next_wanted_order_seq() == 0 ? 1u : craft.next_wanted_order_seq();
    const uint64_t revBefore = craft.scroll_craft_revision();
    const uint64_t playerId = tlsEcs.GetPlayerId(player);
    SkillBookWantedOrder order;
    order.set_order_id(orderId);
    order.set_poster_player_id(playerId);
    order.set_poster_name(DisplayNameOf(player, playerId));
    order.set_skill_table_id(skill->id());
    order.set_scroll_id(scroll->id());
    order.set_require_master_signature(request.require_master_signature());
    order.set_reward_gold(request.reward_gold());
    order.set_created_at_sec(TimeSystem::NowMillisecondsUTC() / 1000u);

    ClientRequestResult applied;
    applied.wantedOrderId = orderId;
    applied.wantedOrderGold = request.reward_gold();
    applied.scrollCraftRevision = revBefore + 1;
    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kPostWantedOrder, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }
    const auto deductErr = CurrencySystem::DeductSplitGold(
        player, request.reward_gold(), 0u, TX_SKILL_SCROLL_CRAFT, request.client_request_id());
    if (deductErr != kSuccess) {
        fail(kSkillGoldNotEnough);
        return;
    }

    *craft.add_wanted_orders() = order;
    craft.set_next_wanted_order_seq(orderId + 1);
    craft.set_scroll_craft_revision(revBefore + 1);
    ledger.Swap(&nextLedger);

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_wanted_order_posted");
    FillWantedOrderInfo(order, *response.mutable_order());
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::CancelWantedOrder(entt::entity player, const CancelSkillBookWantedOrderRequest& request,
                                          CancelSkillBookWantedOrderResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    if (request.client_request_id() == 0 || request.order_id() == 0) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kCancelWantedOrder, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_refunded_gold(first.wantedOrderGold);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    auto& craft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& ledger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    if (craft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        ledger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != craft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const int idx = FindWantedOrderIndex(craft, request.order_id());
    if (idx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    const uint64_t refundGold = craft.wanted_orders(idx).reward_gold();
    if (!CanCreditProxyMaster(player, refundGold)) {
        fail(kServiceUnavailable);
        return;
    }

    const uint64_t revBefore = craft.scroll_craft_revision();
    ClientRequestResult applied;
    applied.wantedOrderId = request.order_id();
    applied.wantedOrderGold = refundGold;
    applied.scrollCraftRevision = revBefore + 1;
    ClientRequestLedgerComp nextLedger = ledger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kCancelWantedOrder, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    RemoveWantedOrderAt(craft, idx);
    (void)CurrencySystem::AddCurrency(player, kCurrencyGold, static_cast<int64_t>(refundGold),
                                      TX_CRAFT_PROXY_FEE, request.client_request_id());
    craft.set_scroll_craft_revision(revBefore + 1);
    ledger.Swap(&nextLedger);

    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_wanted_order_canceled");
    response.set_refunded_gold(refundGold);
    FillStateSnapshot(player, *response.mutable_state());
}

void PlayerCraftSystem::ListWantedOrders(entt::entity player, const ListSkillBookWantedOrdersRequest& request,
                                         ListSkillBookWantedOrdersResponse& response) {
    response.Clear();
    if (!tlsEcs.actorRegistry.valid(player)) {
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }
    const uint64_t selfId = tlsEcs.GetPlayerId(player);
    std::vector<SkillBookWantedOrder> collected;
    for (const auto& [pid, entity] : tlsEcs.playerList) {
        if (pid == 0 || !tlsEcs.actorRegistry.valid(entity) || !PlayerLifecycleSystem::IsPlayerMutable(entity)) {
            continue;
        }
        if (request.only_mine() && pid != selfId) continue;
        const auto* craft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(entity);
        if (craft == nullptr) continue;
        for (const auto& order : craft->wanted_orders()) {
            if (request.filter_skill_table_id() != 0 && order.skill_table_id() != request.filter_skill_table_id()) {
                continue;
            }
            collected.push_back(order);
        }
    }
    std::sort(collected.begin(), collected.end(), [](const auto& a, const auto& b) {
        if (a.reward_gold() != b.reward_gold()) return a.reward_gold() > b.reward_gold();
        if (a.created_at_sec() != b.created_at_sec()) return a.created_at_sec() < b.created_at_sec();
        return a.order_id() < b.order_id();
    });
    for (const auto& o : collected) {
        FillWantedOrderInfo(o, *response.add_orders());
    }
}

void PlayerCraftSystem::FulfillWantedOrder(entt::entity player, const FulfillSkillBookWantedOrderRequest& request,
                                           FulfillSkillBookWantedOrderResponse& response) {
    response.Clear();
    response.set_client_request_id(request.client_request_id());
    const auto fail = [&](uint32_t tip) {
        response.mutable_error_message()->set_id(tip);
        if (CheckPlayerReady(player) == kSuccess) {
            FillStateSnapshot(player, *response.mutable_state());
        }
    };
    if (const auto err = CheckPlayerReady(player); err != kSuccess) {
        response.mutable_error_message()->set_id(err);
        return;
    }
    const bool fromSealedBag = request.sealed_item_id() != 0;
    const bool fromBookshelf = request.bookshelf_book_id() != 0;
    if (request.client_request_id() == 0 || request.poster_player_id() == 0 || request.order_id() == 0 ||
        fromSealedBag == fromBookshelf) {
        fail(kInvalidParameter);
        return;
    }

    const auto fingerprint = ClientRequestFingerprint(request);
    ClientRequestResult first;
    const auto verdict = client_request_dedup::Check(player, request.client_request_id(),
                                                     ClientRequestKind::kFulfillWantedOrder, fingerprint, first);
    if (verdict == ClientRequestVerdict::kReplay) {
        response.set_replayed(true);
        response.set_order_id(first.wantedOrderId);
        response.set_seller_net_gold(first.wantedOrderGold);
        FillStateSnapshot(player, *response.mutable_state());
        return;
    }
    if (verdict == ClientRequestVerdict::kMismatch) {
        fail(kInvalidParameter);
        return;
    }
    if (verdict != ClientRequestVerdict::kNew) {
        fail(kServiceUnavailable);
        return;
    }
    if (const auto busy = CheckWritable(player); busy != kSuccess) {
        fail(busy);
        return;
    }

    const uint64_t sellerId = tlsEcs.GetPlayerId(player);
    if (request.poster_player_id() == sellerId) {
        fail(kInvalidParameter);
        return;
    }

    auto& sellerCraft = tlsEcs.actorRegistry.get<PlayerCraftComp>(player);
    auto& sellerLedger = tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player);
    auto& sellerInventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
    if (sellerCraft.scroll_craft_revision() == std::numeric_limits<uint64_t>::max() ||
        sellerLedger.next_seq() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    if (request.expected_revision() != sellerCraft.scroll_craft_revision()) {
        fail(kSkillCraftRevisionChanged);
        return;
    }

    const entt::entity poster = tlsEcs.GetPlayer(request.poster_player_id());
    if (!tlsEcs.actorRegistry.valid(poster) || !PlayerLifecycleSystem::IsPlayerMutable(poster)) {
        fail(kSkillBookNotFound);
        return;
    }
    auto* posterCraft = tlsEcs.actorRegistry.try_get<PlayerCraftComp>(poster);
    if (posterCraft == nullptr ||
        posterCraft->next_book_seq() == std::numeric_limits<uint64_t>::max() ||
        posterCraft->scroll_craft_revision() == std::numeric_limits<uint64_t>::max()) {
        fail(kServiceUnavailable);
        return;
    }
    const int orderIdx = FindWantedOrderIndex(*posterCraft, request.order_id());
    if (orderIdx < 0) {
        fail(kSkillBookNotFound);
        return;
    }
    // 发榜人书架满了拒绝这次交付(卖家的书与银两都不动),发榜人腾出位置后可再揭。
    if (static_cast<uint32_t>(posterCraft->skill_books_size()) >= equipcraft::kMaxBookshelfBooks) {
        fail(kSkillWantedPosterBookshelfFull);
        return;
    }
    const SkillBookWantedOrder order = posterCraft->wanted_orders(orderIdx);

    std::string deliveredCrafterName;
    uint64_t deliveredCrafterId = 0;
    uint32_t deliveredScrollId = order.scroll_id();
    int sellerBookIdx = -1;
    if (fromSealedBag) {
        const auto* item = sellerInventory.GetItemCompByGuid(request.sealed_item_id());
        if (item == nullptr) {
            fail(kSkillBookNotFound);
            return;
        }
        if (item->config_id() != equipcraft::kSealedSkillBookConfigId || item->size() != 1 ||
            item->bound() || !item->has_sealed_skill_book()) {
            fail(kInvalidParameter);
            return;
        }
        const auto& sealed = item->sealed_skill_book();
        if (sealed.rarity_tier() != 0 || sealed.skill_table_id() != order.skill_table_id()) {
            fail(kInvalidParameter);
            return;
        }
        if (order.require_master_signature() &&
            (sealed.crafter_name().empty() || sealed.crafter_player_id() == 0)) {
            fail(kInvalidParameter);
            return;
        }
        if (BagService::CheckRemovableInstances(player, sellerInventory, {request.sealed_item_id()}) !=
            BagService::RemovalVerdict::kEnough) {
            fail(kFeatureUnavailable);
            return;
        }
        deliveredCrafterName = sealed.crafter_name();
        deliveredCrafterId = sealed.crafter_player_id();
        if (sealed.scroll_id() != 0) deliveredScrollId = sealed.scroll_id();
    } else {
        sellerBookIdx = FindBookIndex(sellerCraft, request.bookshelf_book_id());
        if (sellerBookIdx < 0) {
            fail(kSkillBookNotFound);
            return;
        }
        const auto& book = sellerCraft.skill_books(sellerBookIdx);
        if (book.bound() || book.rarity_tier() != 0 || book.skill_table_id() != order.skill_table_id()) {
            fail(kInvalidParameter);
            return;
        }
        if (order.require_master_signature() &&
            (book.crafter_name().empty() || book.crafter_player_id() == 0)) {
            fail(kInvalidParameter);
            return;
        }
        deliveredCrafterName = book.crafter_name();
        deliveredCrafterId = book.crafter_player_id();
        if (book.scroll_id() != 0) deliveredScrollId = book.scroll_id();
    }

    const uint64_t netGold = equipcraft::ProxyNetGold(order.reward_gold());
    if (!CanCreditProxyMaster(player, netGold)) {
        fail(kServiceUnavailable);
        return;
    }

    const uint64_t sellerRevBefore = sellerCraft.scroll_craft_revision();
    ClientRequestResult applied;
    applied.wantedOrderId = order.order_id();
    applied.wantedOrderGold = netGold;
    applied.scrollCraftRevision = sellerRevBefore + 1;
    ClientRequestLedgerComp nextLedger = sellerLedger;
    if (!client_request_ledger::Remember(nextLedger, request.client_request_id(),
                                         ClientRequestKind::kFulfillWantedOrder, fingerprint, applied) ||
        !client_request_ledger::Validate(nextLedger).empty()) {
        fail(kServiceUnavailable);
        return;
    }

    if (fromSealedBag) {
        const auto removeErr = BagService::RemoveItem(player, sellerInventory, request.sealed_item_id());
        if (removeErr != kSuccess) {
            fail(removeErr);
            return;
        }
    } else {
        RemoveBookAt(sellerCraft, sellerBookIdx);
    }

    const uint64_t posterBookId = posterCraft->next_book_seq() == 0 ? 1u : posterCraft->next_book_seq();
    CraftedSkillBookEntry deliveredBook;
    deliveredBook.set_book_id(posterBookId);
    deliveredBook.set_skill_table_id(order.skill_table_id());
    deliveredBook.set_scroll_id(deliveredScrollId);
    deliveredBook.set_rarity_tier(0);
    deliveredBook.set_bound(false);
    if (!deliveredCrafterName.empty()) deliveredBook.set_crafter_name(deliveredCrafterName);
    if (deliveredCrafterId != 0) deliveredBook.set_crafter_player_id(deliveredCrafterId);

    RemoveWantedOrderAt(*posterCraft, orderIdx);
    *posterCraft->add_skill_books() = deliveredBook;
    posterCraft->set_next_book_seq(posterBookId + 1);
    posterCraft->set_scroll_craft_revision(posterCraft->scroll_craft_revision() + 1);

    if (netGold > 0) {
        (void)CurrencySystem::AddCurrency(player, kCurrencyGold, static_cast<int64_t>(netGold),
                                          TX_CRAFT_PROXY_FEE, request.client_request_id());
    }
    sellerCraft.set_scroll_craft_revision(sellerRevBefore + 1);
    sellerLedger.Swap(&nextLedger);

    PlayerLifecycleSystem::RequestKeyProgressSave(poster, "skill_wanted_order_fulfilled_buyer");
    PlayerLifecycleSystem::RequestKeyProgressSave(player, "skill_wanted_order_fulfilled_seller");
    response.set_order_id(order.order_id());
    response.set_seller_net_gold(netGold);
    FillStateSnapshot(player, *response.mutable_state());
}
