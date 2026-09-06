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
#include "client/battle_client_edge.h"
#include "battle_security.h"
#include "node_config_manager.h"

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
        // 客户端直连面(设计文档 §18):装在节点自身 TCP 端口上,票据握手 + 战斗消息就地派发
        std::unique_ptr<BattleClientEdge> clientEdge;
    };

    // 启动门禁:空 battle_token_secret 在生产模式下**拒绝启动**(照 gate 的
    // ValidateGateTokenSecretOrDie:弱入口必须由显式运行模式 BATTLE_RUN_MODE 授权,
    // 配置里恰好少了一项不算授权)。同时校验 battle_max_connections=0 只在 dev/test 合法。
    // 调用时机必须晚于 Node 构造(LoadConfigs 之后),所以放在 configure 回调第一句。
    void ValidateBattleClientEdgeConfigOrDie()
    {
        const auto &resolution = battle_security::ResolveRunModeOnce();
        if (!resolution.recognized)
        {
            LOG_WARN << "Unrecognized " << battle_security::kRunModeEnv << "='" << resolution.raw
                     << "', falling back to run_mode=prod. Valid values: prod|dev|test.";
        }
        const auto mode = resolution.mode;
        const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();

        switch (battle_security::ClassifyTokenSecret(deploy.battle_token_secret(), mode))
        {
        case battle_security::TokenSecretVerdict::kEnforce:
            LOG_INFO << "Battle direct-connect ticket verification ENFORCED, run_mode="
                     << battle_security::RunModeName(mode);
            // 非空只是最低门槛。§18.6 的两条硬要求也在这里守:≥32 字节(HMAC-SHA256 输出
            // 32 字节,更短的密钥把暴力搜索空间塌到密钥长度上,票据可伪造为任意玩家),
            // 且 ≠ GateTokenSecret(分域:任一泄露不影响另一侧,D24)。prod 拒启,dev/test 只 WARN。
            switch (battle_security::ClassifySecretStrength(deploy.battle_token_secret(),
                                                            deploy.gate_token_secret()))
            {
            case battle_security::SecretStrengthVerdict::kOk:
                break;
            case battle_security::SecretStrengthVerdict::kTooShort:
                if (battle_security::IsNonProdMode(mode))
                {
                    LOG_WARN << "battle_token_secret is shorter than "
                             << battle_security::kMinTokenSecretBytes
                             << " bytes; tolerated only because run_mode=" << battle_security::RunModeName(mode);
                }
                else
                {
                    LOG_FATAL << "Refusing to start: battle_token_secret must be at least "
                              << battle_security::kMinTokenSecretBytes << " bytes while run_mode=prod.";
                }
                break;
            case battle_security::SecretStrengthVerdict::kSameAsGate:
                if (battle_security::IsNonProdMode(mode))
                {
                    LOG_WARN << "battle_token_secret equals gate_token_secret; tolerated only because run_mode="
                             << battle_security::RunModeName(mode)
                             << ". Production must use a distinct secret (trust-domain separation).";
                }
                else
                {
                    LOG_FATAL << "Refusing to start: battle_token_secret equals gate_token_secret while run_mode=prod"
                              << " (trust domains must be separate, design D24).";
                }
                break;
            }
            break;
        case battle_security::TokenSecretVerdict::kDevBypass:
            LOG_WARN << "SECURITY WARNING: battle_token_secret is EMPTY and "
                     << battle_security::kRunModeEnv << "=" << battle_security::RunModeName(mode)
                     << " -- direct-connect ticket signatures will NOT be verified."
                     << " NEVER run this configuration in production.";
            break;
        case battle_security::TokenSecretVerdict::kRefuse:
            // LOG_FATAL 会 abort:带着空密钥的 battle 直连面等于谁拿到 battle_id 都能进房
            LOG_FATAL << "Refusing to start: battle_token_secret is empty while run_mode=prod."
                      << " Set BattleTokenSecret in etc/base_deploy_config.yaml, or set "
                      << battle_security::kRunModeEnv << "=dev|test for a local run.";
            break;
        }

        if (deploy.battle_max_connections() == 0 && !battle_security::IsNonProdMode(mode))
        {
            LOG_FATAL << "Refusing to start: battle_max_connections=0 while run_mode=prod."
                      << " Set BattleMaxConnections in etc/base_deploy_config.yaml (1..65535).";
        }
    }

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

        // 刻意不声明 KafkaCommandType:battle 不消费 battle-{id} topic。
        //
        // 上下行的当前口径(§18 直连落地后,D6 已降级为回落路径):
        //   * 客户端上行:优先走本节点 TCP 端口上的直连面(BattleClientEdge);
        //     没有直连的玩家才经 gate→BattleClientPlayer gRPC 中继进来。
        //   * 客户端下行:BattleRoomManager::PushToPlayer 有直连即直发,否则回落
        //     Kafka gate-{id} 的 PushToPlayerEvent。
        //   * 控制面(match→battle 的 CreateBattle/DestroyBattle/AddObserver/
        //     RemoveObserver/IssueBattleTicket):恒走 gRPC,不受直连影响 —— 调用方是
        //     Go 服务,而自家 RPC0 信封没有请求关联 id,配不出应答
        //     (选型定谳见 docs/design/battle-transport-decision.md)。
        //   * 结算 / 绑定事件:仍恒走 Kafka producer。
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

            // 客户端直连面(设计文档 §18):先过安全门禁,再装配。
            ValidateBattleClientEdgeConfigOrDie();
            context.clientEdge = std::make_unique<BattleClientEdge>();
            BattleRoomManager::Instance().SetClientEdge(context.clientEdge.get());

            // 节点自身的 TCP 端口(NodeInfo.endpoint,框架分配并发布到 etcd)原本挂的是节点间
            // RpcCodec。battle 以 PROTOCOL_GRPC 注册(node.cpp 按 IsGrpcOnlyNodeType 设
            // protocol_type),发现方 node_connector 按 protocol_type 分派,只会拨它的 gRPC 端口、
            // 不会经这个 TCP 端口握手 —— 所以可以把它整个让给客户端直连
            //(与 gate main.cpp 覆写 GetTcpServer 回调同一时机:StartRpcServer 之后)。
            node.SetAfterStart([&context](Node &n)
                               {
                context.clientEdge->Install(n.GetTcpServer());
                const auto &ep = n.GetNodeInfo().endpoint();
                LOG_INFO << "battle 客户端直连面已就绪: endpoint=" << ep.ip() << ":" << ep.port()
                         << " max_connections=" << gNodeConfigManager.GetBaseDeployConfig().battle_max_connections()
                         << " run_mode=" << battle_security::RunModeName(battle_security::CurrentRunMode()); });

            // 停机收尾:所有在打战斗作废(只解绑 gate 会话,不发结算;
            // scene 侧 reaper 按 InBattleComp.deadline_ms 解冻,设计文档 §3.2),
            // 随后断开全部客户端直连。框架随后 flush Kafka producer,解绑事件不会丢在队列里。
            node.SetBeforeShutdown([&context](Node &)
                                   {
                BattleRoomManager::Instance().AbortAllRooms("node_shutdown");
                if (context.clientEdge)
                {
                    context.clientEdge->DisconnectAll("node_shutdown");
                } });
        });
}
