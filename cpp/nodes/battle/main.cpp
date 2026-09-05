#include "muduo/net/EventLoop.h"
#include "muduo/base/Logging.h"

#include "node/system/node/node_entry.h"

#include "handler/rpc/battle_handler.h"
// 生成骨架:proto/battle/battle_node.proto 重生成后由生成器写出
// handler/grpc/battle_node.h(class BattleNodeImpl,形态同 scene 的
// handler/grpc/scene_node_service.h:runInLoop + promise 投递到 loop 线程)。
#include "handler/grpc/battle_node.h"
#include "handler/grpc/battle_client_player_service.h"
#include "logic/battle_room_manager.h"

#include "data/battle_table_fingerprint.h"

#include "proto/common/base/node.pb.h"

#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/cooldown_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"

#include <memory>

using namespace muduo;
using namespace muduo::net;

namespace
{

    // battle 节点运行时状态:两个 gRPC 服务实现。
    // 房间状态本身在 BattleRoomManager 单例里(纯内存,崩溃即战斗作废,设计文档 §8)。
    struct BattleRuntimeContext
    {
        // match → battle 内部 gRPC(CreateBattle / DestroyBattle)
        std::unique_ptr<BattleNodeImpl> battleNodeService;
        // gate → battle 客户端消息(SubmitBattleAction / GetBattleState,
        // 会话权威身份从 x-session-detail-bin metadata 解出)
        std::unique_ptr<BattleClientPlayerGrpcImpl> clientPlayerService;
    };

    struct BattleNodeHooks
    {
        // 表加载完成钩子:battle 依赖 Skill/Buff/Cooldown/SkillPermission/Monster/Dungeon
        // 六张表(引擎经 TableBattleDataProvider 读取)。框架 LoadTablesAsync 全量加载,
        // 这里只做就绪校验:空表说明数据目录配置有问题,尽早在日志里暴露。
        struct TableLoadHandler
        {
            static void OnLoaded()
            {
                const auto skillRows = SkillTableManager::Instance().FindAll().data_size();
                const auto buffRows = BuffTableManager::Instance().FindAll().data_size();
                const auto cooldownRows = CooldownTableManager::Instance().FindAll().data_size();
                const auto permissionRows = SkillPermissionTableManager::Instance().FindAll().data_size();
                const auto dungeonRows = DungeonTableManager::Instance().FindAll().data_size();
                const auto monsterRows = MonsterTableManager::Instance().FindAll().data_size();

                LOG_INFO << "battle 战斗表加载完成: skill=" << skillRows
                         << " buff=" << buffRows
                         << " cooldown=" << cooldownRows
                         << " skill_permission=" << permissionRows
                         << " dungeon=" << dungeonRows
                         << " monster=" << monsterRows;

                if (skillRows == 0 || buffRows == 0 || dungeonRows == 0 || monsterRows == 0)
                {
                    LOG_ERROR << "battle 关键战斗表为空,回合引擎将无法开局,请检查表数据目录";
                }

                // 战斗配表指纹:表加载完成后算一次并缓存,CreateBattle 与 match/scene 带来的
                // 指纹比对(设计文档 cross-zone-matchmaking.md §10);启动日志里打出来便于跨节点排障
                LOG_INFO << "battle 战斗配表指纹: table_fingerprint="
                         << turnbattle::BattleTableFingerprint::Refresh();
            }
        };

        // 刻意不声明 KafkaCommandType:battle 一期不消费 battle-{id} topic,
        // 上行全走 gRPC(gate/match),出站全走 Kafka producer(设计文档 D6)。
    };

} // namespace

int main(int argc, char *argv[])
{
    return node::entry::RunSimpleNodeMainWithOwnedContext<BattleHandler, BattleRuntimeContext, BattleNodeHooks>(
        common::base::BattleNodeService,
        // battle 无出站拨号:入站是 match/gate 的 gRPC,出站全走 Kafka producer,
        // 对端路由信息(gate/scene 实例)全部由 BattlePlayerSnapshot 携带,不查 etcd 定位。
        Node::CanConnectNodeTypeList{},
        [](Node &node, BattleRuntimeContext &context)
        {
            // gRPC 服务要在 etcd 端口分配之前注册:框架看到 GetGrpcServices()
            // 非空才会派生 gRPC 端口(TCP + 30000)并在 server 就绪后发布服务发现
            // (node_allocator.cpp / PublishDiscoveryAfterGrpcReady)。
            context.battleNodeService = std::make_unique<BattleNodeImpl>(*node.GetLoop());
            context.clientPlayerService = std::make_unique<BattleClientPlayerGrpcImpl>(*node.GetLoop());
            node.RegisterGrpcService(context.battleNodeService.get());
            node.RegisterGrpcService(context.clientPlayerService.get());

            // 停机收尾:所有在打战斗作废(只解绑 gate 会话,不发结算;
            // scene 侧 reaper 按 InBattleComp.deadline_ms 解冻,设计文档 §3.2)。
            // 框架随后 flush Kafka producer,解绑事件不会丢在队列里。
            node.SetBeforeShutdown([](Node &)
                                   { BattleRoomManager::Instance().AbortAllRooms("node_shutdown"); });
        });
}
