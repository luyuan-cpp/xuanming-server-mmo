#pragma once

#include <algorithm>
#include <cstdint>
#include <optional>
#include <string>

#include "entt/src/entt/entity/registry.hpp"
#include "muduo/base/Logging.h"
#include "engine/core/type_define/type_define.h"
#include "persist_schema/persist_schema_gen.h"
#include "proto/common/database/player_cache.pb.h"
#include "services/scene/player/comp/player_persist_carry_comp.h"
#include "services/scene/player/comp/player_persist_writer_comp.h"
#include "services/scene/player/system/player_data_loader.h"
#include "thread_context/ecs_context.h"

// 玩家持久数据的统一拼装入口(docs/design/mixed-version-persist-field-protection.md §4.2)。
//
// 存盘、退出收敛比较、跨 zone 迁移、回档快照都走这里:比的、写的、快照的是同一个形状。
// 生成的 PlayerAllDataMessageFieldsMarshal(player_data_loader.h)只拼组件;本入口再补两张子表的 player_id,
// 并给两表盖覆盖世代 max(本二进制世代, 加载世代)。各表顶层未知字段由对应表的手写 Marshal 从 Carry 组件带回。
// 只在 scene 逻辑线程上调用。
namespace player_persist
{

// 戳归一:0(未盖戳)按世代 1 处理(设计 §3.3)。
[[nodiscard]] inline uint32_t EffectiveSchemaGen(uint32_t raw)
{
    return raw == 0 ? 1u : raw;
}

// 加载世代 = 两表加载时戳的最小值(0 按 1):取最小值,不让一张表被截断的事实被另一张表的高戳洗白(设计 §3.3)。
// 任一 Carry 组件缺失(加载不变量被破坏)返回 nullopt。
[[nodiscard]] inline std::optional<uint32_t> LoadedSchemaGen(entt::entity player)
{
    const auto *main = tlsEcs.actorRegistry.try_get<PlayerDatabaseCarryComp>(player);
    const auto *second = tlsEcs.actorRegistry.try_get<PlayerDatabase1CarryComp>(player);
    if (main == nullptr || second == nullptr)
    {
        return std::nullopt;
    }
    return std::min(EffectiveSchemaGen(main->loadedSchemaGen), EffectiveSchemaGen(second->loadedSchemaGen));
}

// 返回 false = 持久组件缺失(含两个 Carry 组件)或携带的未知字段损坏,out 保持原样;调用方必须拒绝这次写入。
// 不打压测探针、不写 last_save_seq 与围栏元数据:这些只在真正写盘的路径上写(SavePlayerToRedisImpl)。
[[nodiscard]] inline bool MarshalPlayerForPersist(entt::entity player, Guid playerId, PlayerAllData &out)
{
    const auto loaded = LoadedSchemaGen(player);
    if (!loaded)
    {
        LOG_ERROR << "[PlayerSave] Persisted component(s) missing, refusing to marshal player " << playerId
                  << " (load invariant broken; nothing is written): missing=PlayerDatabaseCarryComp/PlayerDatabase1CarryComp";
        return false;
    }
    if (!PlayerAllDataMessageFieldsMarshal(player, out))
    {
        return false;
    }
    const uint32_t gen = std::max(persist_schema::kPersistSchemaGen, *loaded);
    auto *main = out.mutable_player_database_data();
    auto *second = out.mutable_player_database_1_data();
    main->set_player_id(playerId);
    second->set_player_id(playerId);
    main->set_persist_schema_gen(gen);
    second->set_persist_schema_gen(gen);
    return true;
}

// ── 严格屏障覆盖世代校验 C3 的 C++ 侧参数(设计 §5.2)────────────────────────────────────────────

// 写方令牌(ARGV[11]):本实体第一次走严格屏障存盘时生成,实体生命期内不变;实体销毁即作废,重新加载得到新令牌。
// 运行时派生数据,只在存盘写入口调用(懒建);读路径用 try_get。
[[nodiscard]] inline const std::string &WriterTokenForSave(entt::entity player)
{
    auto &writer = tlsEcs.actorRegistry.get_or_emplace<PlayerPersistWriterComp>(player);
    if (writer.token.empty())
    {
        writer.token = player_persist_writer::NewToken();
    }
    return writer.token;
}

} // namespace player_persist
