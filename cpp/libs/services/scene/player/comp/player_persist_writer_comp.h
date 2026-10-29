#pragma once

#include <string>

#include <boost/uuid/uuid.hpp>
#include <boost/uuid/uuid_generators.hpp>
#include <boost/uuid/uuid_io.hpp>

// 严格提交屏障的写方令牌(docs/design/mixed-version-persist-field-protection.md §5.2,ARGV[11])。
//
// 一个加载实体一个令牌:随屏障写入 save_barrier hash 的 schema_writer,Lua 据此判断"上一次屏障写入是不是本实体写的"。
// 不是的时候才做覆盖世代校验 C3 —— 那正是接手别人写的数据、截断可能被引入的时刻。
//
// 为什么按实体、不按 owner_epoch 或 player_id:同一实体会换代次(E→E+1 复用实体、B5 回滚采纳),同一代次也会先后
// 加载出两个实体(同落点重连、废黜后回流、同号新进程接替);两种都要判对,只有"每个加载实体独有"的令牌做得到。
// 所以令牌必须不能被"同节点同玩家再加载一次"复现:用随机 UUID,不用 entt 实体号(会回收)或节点号 + 玩家号。
//
// 运行时派生数据(ecs-component-access-rules.md):不入库、不进快照;在存盘写入口懒建,实体销毁即作废。
// 读路径不建。令牌一旦生成,在实体生命期内不变。
struct PlayerPersistWriterComp
{
    std::string token;
    // 本实体在 warn 模式下被判过倒退、且已计数打日志。严格屏障之后对它的每次写入都会再返回 2(证据留在 Redis 的
    // schema_regressed,防回包丢失),计数只记第一次(PlayerLifecycleSystem::HandlePlayerSaveSchemaWarned)。
    bool regressionWarnReported{false};
};

namespace player_persist_writer
{

// 生成一个新令牌:随机 UUID 的规范文本(36 字节,只含小写十六进制与 '-'),满足 IsValidRedisSaveSchemaWriter。
// 只在实体第一次走严格屏障存盘时调用一次,不在热路径上。
[[nodiscard]] inline std::string NewToken()
{
    thread_local boost::uuids::random_generator generator;
    return boost::uuids::to_string(generator());
}

} // namespace player_persist_writer
