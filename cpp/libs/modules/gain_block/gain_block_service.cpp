#include "gain_block_service.h"

#include <algorithm>

#include <muduo/base/Logging.h>

#include "proto/common/component/player_item_gain_block_comp.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

// ---------------------------------------------------------------------------
// Thread-local storage — one set per GainType, per scene thread.
// ---------------------------------------------------------------------------

namespace
{

struct GlobalBlockSets
{
    std::unordered_set<uint32_t> currency;
    std::unordered_set<uint32_t> item;
};

thread_local GlobalBlockSets tls_globalBlocks;

std::unordered_set<uint32_t> &ResolveSet(GainBlockService::GainType type)
{
    switch (type)
    {
    case GainBlockService::GainType::kCurrency:
        return tls_globalBlocks.currency;
    case GainBlockService::GainType::kItem:
        return tls_globalBlocks.item;
    }
    // Fallback (should never reach here).
    return tls_globalBlocks.item;
}

// empty sentinel for safety
const std::unordered_set<uint32_t> kEmptySet;

} // namespace

// ---------------------------------------------------------------------------
// Global block / unblock
// ---------------------------------------------------------------------------

void GainBlockService::BlockGlobal(GainType type, uint32_t id)
{
    auto &s = ResolveSet(type);
    if (s.insert(id).second)
    {
        LOG_INFO << "GainBlockService: GLOBAL block type="
                 << static_cast<uint32_t>(type) << " id=" << id;
    }
}

void GainBlockService::UnblockGlobal(GainType type, uint32_t id)
{
    auto &s = ResolveSet(type);
    if (s.erase(id) > 0)
    {
        LOG_INFO << "GainBlockService: GLOBAL unblock type="
                 << static_cast<uint32_t>(type) << " id=" << id;
    }
}

bool GainBlockService::IsGloballyBlocked(GainType type, uint32_t id)
{
    return ResolveSet(type).contains(id);
}

const std::unordered_set<uint32_t> &GainBlockService::GlobalBlockedIds(GainType type)
{
    return ResolveSet(type);
}

void GainBlockService::ClearAllGlobalBlocks()
{
    tls_globalBlocks.currency.clear();
    tls_globalBlocks.item.clear();

    LOG_INFO << "GainBlockService: all global blocks cleared";
}

// ---------------------------------------------------------------------------
// Global-only facade (currency callers pair it with CurrencySystem::IsCurrencyBlocked)
// ---------------------------------------------------------------------------

bool GainBlockService::IsGainBlocked(GainType type, uint32_t id)
{
    return IsGloballyBlocked(type, id);
}

// ---------------------------------------------------------------------------
// 物品获得:全服 + 单玩家(docs/design/player-inbox-service.md §2.2)
// ---------------------------------------------------------------------------

bool GainBlockService::IsItemGainBlocked(entt::entity player, uint32_t configId)
{
    return IsGloballyBlocked(GainType::kItem, configId) || IsPlayerItemGainBlocked(player, configId);
}

bool GainBlockService::IsPlayerItemGainBlocked(entt::entity player, uint32_t configId)
{
    // try_get 按完整句柄与版本检查归属,entt::null / 陈旧句柄直接得到 nullptr,不创建组件。
    // 缺组件的口径见头文件:不补建,存盘侧整份拒存兜底。
    const auto *blocks = tlsEcs.actorRegistry.try_get<PlayerItemGainBlockComp>(player);
    if (blocks == nullptr)
    {
        return false;
    }
    // 按值查找,不依赖存档里的顺序(写入侧保持升序,读取侧不拿它做二分,坏顺序也查得到)。
    const auto &ids = blocks->item_config_ids();
    return std::find(ids.begin(), ids.end(), configId) != ids.end();
}

uint32_t GainBlockService::BlockPlayerItemGain(entt::entity player, uint32_t configId)
{
    if (configId == 0)
    {
        LOG_ERROR << "GainBlockService::BlockPlayerItemGain: item config 0 is not a valid config, entity="
                  << entt::to_integral(player);
        return kInvalidParameter;
    }
    // 持久数据只取不建(ecs-component-access-rules.md 规则 4):缺了是加载不变量被破坏,
    // 补一份空表再写进去会让下一次存盘拿它覆盖盘上真实的封禁表。
    auto *blocks = tlsEcs.actorRegistry.try_get<PlayerItemGainBlockComp>(player);
    if (blocks == nullptr)
    {
        LOG_ERROR << "GainBlockService::BlockPlayerItemGain: PlayerItemGainBlockComp missing (load invariant broken), entity="
                  << entt::to_integral(player) << " config_id=" << configId;
        return kInvalidParameter;
    }
    auto *ids = blocks->mutable_item_config_ids();
    if (std::find(ids->begin(), ids->end(), configId) != ids->end())
    {
        return kSuccess; // 已封禁:天然幂等
    }
    // 写入后排成升序:同一组封禁在任何节点上序列化出同一份字节(存盘脏比较不会因顺序误判有变化)。
    ids->Add(configId);
    std::sort(ids->begin(), ids->end());
    LOG_INFO << "GainBlockService: player item gain blocked config_id=" << configId
             << " entity=" << entt::to_integral(player) << " blocked_count=" << ids->size();
    return kSuccess;
}

uint32_t GainBlockService::UnblockPlayerItemGain(entt::entity player, uint32_t configId)
{
    if (configId == 0)
    {
        LOG_ERROR << "GainBlockService::UnblockPlayerItemGain: item config 0 is not a valid config, entity="
                  << entt::to_integral(player);
        return kInvalidParameter;
    }
    auto *blocks = tlsEcs.actorRegistry.try_get<PlayerItemGainBlockComp>(player);
    if (blocks == nullptr)
    {
        LOG_ERROR << "GainBlockService::UnblockPlayerItemGain: PlayerItemGainBlockComp missing (load invariant broken), entity="
                  << entt::to_integral(player) << " config_id=" << configId;
        return kInvalidParameter;
    }
    auto *ids = blocks->mutable_item_config_ids();
    // 连同可能存在的重复项一起删(写入侧不会写出重复,这里不依赖这一点);其余元素保持原有相对顺序。
    const auto kept = std::remove(ids->begin(), ids->end(), configId);
    const int removed = static_cast<int>(ids->end() - kept);
    if (removed == 0)
    {
        return kSuccess; // 本来没封禁:天然幂等
    }
    ids->Truncate(ids->size() - removed);
    LOG_INFO << "GainBlockService: player item gain unblocked config_id=" << configId
             << " entity=" << entt::to_integral(player) << " blocked_count=" << ids->size();
    return kSuccess;
}
