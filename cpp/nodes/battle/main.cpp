#include "muduo/net/EventLoop.h"
#include "muduo/base/Logging.h"

#include "node/system/node/node_entry.h"
#include "node/system/etcd/etcd_transport.h"
#include "node/system/node/client_endpoint.h"
#include "infra/agones/agones_client_endpoint_source.h"
#include "infra/agones/agones_gameserver_lifecycle.h"

#include "handler/rpc/battle_handler.h"
// 生成骨架:proto/battle/battle_node.proto 重生成后由生成器写出
// handler/grpc/battle_node.h(class BattleNodeImpl : muduo_grpc::Service,形态同 scene 的
// handler/grpc/scene_node_service.h:全部在节点 EventLoop 线程上执行,不创建线程)。
#include "handler/grpc/battle_node.h"
#include "logic/battle_room_manager.h"
#include "client/battle_client_edge.h"
#include "client/network_probe_security.h"
#include "battle_security.h"
#include "node_config_manager.h"

#include "data/battle_table_fingerprint.h"
#include "data/table_battle_data_provider.h"

#include "proto/common/base/node.pb.h"

#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/cooldown_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/dungeon_table.h"
#include "table/code/monster_table.h"
#include "table/code/item_table.h"

#include <memory>
#include <string>
#include <utility>
#include <cstdlib>
#include <filesystem>

using namespace muduo;
using namespace muduo::net;

namespace
{

    // Agones 排空标签键(集群外入口 D83)。跨语言字符串契约:Fleet 的
    // allocationOverflow.labels 与运维手动 `kubectl label gs` 用的是同一个键
    // (部署生成器 tools/scripts/lib/k8s_client_entry.ps1),改名必须两边同改。
    // lifecycle 只看键是否存在、不看值;解除排空 = 删掉标签。
    constexpr char kAgonesDrainLabelKey[] = "mmorpg.io/drain";

    // EventLoop 心跳周期(D84)。worker 在心跳超过 LifecycleOptions.loopStaleAfter(默认 10s)
    // 未更新时停发 /health,让 Agones 判 Unhealthy 并替换卡死的实例;周期必须远小于该阈值。
    constexpr double kLoopHeartbeatIntervalSec = 1.0;

    // 日志用:"ip:port";未设置(podip 形态的 client_endpoint)打 "(unset)"。
    std::string EndpointForLog(const EndpointComp &endpoint)
    {
        if (endpoint.ip().empty() && endpoint.port() == 0)
        {
            return "(unset)";
        }
        return endpoint.ip() + ":" + std::to_string(endpoint.port());
    }

    // Agones 生命周期(D81–D84):房间是单元,第一个房间建出之前 allocate、最后一个房间移除之后回 Ready。
    // 放在 SetAfterStart 里:此时 gRPC server 已监听、etcd 已发布、直连面已装好,进程确实能接房间了,
    // 才有资格 POST /ready(提前 Ready 会让 Agones 把还接不住客户端的实例标成可分配)。
    // 非 Agones 环境(本地 / Deployment + hostPort)读不到 AGONES_SDK_HTTP_PORT,或 Windows 构建没有 curl
    // 传输层(未定义 MMORPG_AGONES_CURL):一律 StartDisabled —— 不起线程、不发 HTTP、许可恒放行。
    void StartAgonesLifecycle()
    {
        auto &lifecycle = agones::GameServerLifecycle::Instance();
        const auto agonesEnv = agones::ReadAgonesEnv();
        std::unique_ptr<agones::HttpTransport> transport;
        if (agonesEnv.enabled)
        {
            transport = agones::MakeDefaultHttpTransport();
        }
        if (transport == nullptr)
        {
            LOG_INFO << "battle Agones lifecycle disabled (enabled=" << agonesEnv.enabled
                     << ", transport=" << (agonesEnv.enabled ? "unavailable" : "n/a") << ")";
            lifecycle.StartDisabled();
            return;
        }

        // 与 scene 的差别:battle 开启 EventLoop 心跳绑定(D84)与排空标签(D83)。
        agones::LifecycleOptions options;
        options.requireLoopHeartbeat = true;
        options.drainLabelKey = kAgonesDrainLabelKey;
        lifecycle.Start(std::move(transport), agonesEnv.BaseUrl(), std::move(options));
    }

    // battle 节点运行时状态:match 控制面 gRPC 服务 + 客户端直连面。
    // 房间状态本身在 BattleRoomManager 单例里(纯内存,崩溃即战斗作废,设计文档 §8)。
    // gate → battle 的客户端消息 gRPC 中继(BattleClientPlayerGrpcImpl)已随收缩删除
    // (turn-based §22 D66/D67):直连是战斗唯一通路。
    struct BattleRuntimeContext
    {
        // match → battle 内部 gRPC(CreateBattle / DestroyBattle / AddObserver / RemoveObserver /
        // IssueBattleTicket)
        std::unique_ptr<BattleNodeImpl> battleNodeService;
        // 客户端直连面(设计文档 §18):装在节点自身 TCP 端口上,票据握手 + 战斗消息就地派发
        std::shared_ptr<BattleClientEdge> clientEdge;
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
        if(mode==battle_security::RunMode::kProd&&!network_probe::Enabled())
            LOG_FATAL << "生产battle必须开启GLOBAL_MATCH_ENABLED并使用全球事务权威放置";
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
        // 表加载完成钩子:battle 依赖 Skill/Buff/Cooldown/SkillPermission/Monster/Dungeon/Item
        // 七张表(引擎经 TableBattleDataProvider 读取,2026-09-17 起含 Item:战斗内用药效果读表)。
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
                const auto itemRows = ItemTableManager::Instance().FindAll().data_size();

                LOG_INFO << "battle combat tables loaded: skill=" << skillRows
                         << " buff=" << buffRows
                         << " cooldown=" << cooldownRows
                         << " skill_permission=" << permissionRows
                         << " dungeon=" << dungeonRows
                         << " monster=" << monsterRows
                         << " item=" << itemRows;

                // Item 空表只影响"战斗内用药",不影响开局,单独告警不混进致命项
                if (itemRows == 0)
                {
                    LOG_WARN << "battle Item table is empty:all battle items unavailable(ITEM actions will be rejected)";
                }

                if (skillRows == 0 || buffRows == 0 || dungeonRows == 0 || monsterRows == 0)
                {
                    LOG_ERROR << "battle critical combat tables are empty;turn engine cannot create battles;check table data directory";
                }

                // 战斗配表指纹:表加载完成后算一次并缓存,CreateBattle 与 match/scene 带来的
                // 指纹比对(设计文档 cross-zone-matchmaking.md §10);启动日志里打出来便于跨节点排障
                LOG_INFO << "battle combat table fingerprint: table_fingerprint="
                         << turnbattle::BattleTableFingerprint::Refresh();

                // 奖励独立于人数/怪物表；只在启动边界读文件，建房时不做磁盘 I/O。
                std::filesystem::path rewardPath = "etc/battle_rewards.json";
                if (const char* overridePath = std::getenv("BATTLE_REWARD_CONFIG_PATH"); overridePath && *overridePath)
                    rewardPath = overridePath;
                auto provider = std::make_shared<turnbattle::TableBattleDataProvider>();
                if (!provider->LoadBattleRewards(rewardPath.string())) {
                    LOG_FATAL << "Battle reward config rejected: " << rewardPath.string();
                }
                BattleRoomManager::Instance().SetDataProvider(std::move(provider));
            }
        };

        // 客户端地址来源工厂(集群外入口 D79 / D81):只在 CLIENT_ENDPOINT_SOURCE=agones 时,由 Node 在
        // 构造期(InitRpcServer,etcd 分配与发布之前)调用一次,向本机 Agones sidecar 取
        // status.address + ports["client"] 作为自报的 client_endpoint(有界阻塞,最坏约 60s)。
        // Agones 未启用(无 AGONES_SDK_HTTP_PORT / AGONES_ENABLED=0)或本构建没有 curl 传输层(Windows)时
        // 返回 nullptr,Node 随即致命退出 —— 配了 agones 来源却不在 Agones 里跑是部署错误,fail-closed。
        struct ClientEndpointSourceFactory
        {
            static std::unique_ptr<client_endpoint::ExternalSource> Make()
            {
                return agones::MakeClientEndpointSourceFromEnv();
            }
        };

        // 刻意不声明 KafkaCommandType:battle 不消费 battle-{id} topic。
        //
        // 上下行的当前口径(直连收缩后,turn-based §22 D66-D68):
        //   * 客户端上行:只有本节点 TCP 端口上的直连面(BattleClientEdge);gate 与路由服
        //     都拒绝 BattleClientPlayer 的上行,不再有 gRPC 中继。
        //   * 客户端下行:战斗帧 BattleRoomManager::PushBattleFrame 只走直连,无活直连即丢弃;
        //     只有 NotifyBattleAssigned / NotifyBattleStart 经 PushLobbyAnnouncement 在无直连时
        //     回落 Kafka gate-cmd 的 PushToPlayerEvent(它们发生在直连建立之前)。
        //   * 控制面(match→battle 的 CreateBattle/DestroyBattle/AddObserver/
        //     RemoveObserver/IssueBattleTicket):恒走 gRPC,不受直连影响 —— 调用方是
        //     Go 服务,而自家 RPC0 信封没有请求关联 id,配不出应答
        //     (选型定谳见 docs/design/battle-transport-decision.md)。
        //   * 结算 / 确认 / 对局结果事件:恒走 Kafka producer;不再有任何 gate 绑定事件。
    };

} // namespace

int main(int argc, char *argv[])
{
    // 检查旧的 NODE_ETCD_TRANSPORT 环境变量(etcd 只剩 muduo_h2 传输;设成 grpc 会打 WARN),须在 Node 构造前调用。
    etcd_transport::InstallFromEnv();
    return node::entry::RunSimpleNodeMainWithOwnedContext<BattleHandler, BattleRuntimeContext, BattleNodeHooks>(
        common::base::BattleNodeService,
        // Match是全局SQL终局权威，按框架全局池发现，不限当前zone；个人奖励只由其持久Inbox重投。
        // gate/scene旧路由仍来自快照，不在这里新增玩家位置查询。
        Node::CanConnectNodeTypeList{common::base::MatchNodeService},
        [](Node &node, BattleRuntimeContext &context)
        {
            node.MutableNodeInfo().set_physical_region(network_probe::PhysicalRegion());
            if(network_probe::Enabled())
            {
                if(network_probe::PhysicalRegion()==0||network_probe::Secret().size()<32)
                    LOG_FATAL << "全球匹配要求非零 PHYSICAL_REGION 及至少32字节 MATCH_PROBE_SECRET";
            }
            // gRPC 服务要在 etcd 端口分配之前注册:框架看到 HasGrpcServices()
            // 为真才会派生 gRPC 端口(TCP + 30000)并在 server 就绪后发布服务发现
            // (node_allocator.cpp / PublishDiscoveryAfterGrpcReady)。context 先于 Node 构造、晚于 Node 析构,
            // 满足「服务必须活过 Node」(方法表闭包捕获服务的 this)。
            context.battleNodeService = std::make_unique<BattleNodeImpl>(*node.GetLoop());
            node.RegisterGrpcService(*context.battleNodeService);

            // 客户端直连面(设计文档 §18):先过安全门禁,再装配。
            ValidateBattleClientEdgeConfigOrDie();
            context.clientEdge = std::make_shared<BattleClientEdge>();
            BattleRoomManager::Instance().SetClientEdge(context.clientEdge.get());

            // 房间 = Agones 单元(D82 / D85):房间表(battle_room_table.h,只能经 Emplace / Erase 增删)
            // 每次插表 / 移除恰好回调一次,转发到 GameServerLifecycle 的单元计数,key = battle_id(活跃房间之间唯一)。
            // lifecycle 是进程级静态单例,寿命覆盖 EventLoop,回调里不捕获任何对象。
            // lifecycle 为 Disabled 时这两个调用只记账、不发 HTTP。
            BattleRoomManager::Instance().SetRoomLifecycleHooks(BattleRoomManager::RoomLifecycleHooks{
                [](const uint64_t battleId)
                { agones::GameServerLifecycle::Instance().OnUnitCreated(battleId); },
                [](const uint64_t battleId)
                { agones::GameServerLifecycle::Instance().OnUnitDestroyed(battleId); }});

            // 节点自身的 TCP 端口(NodeInfo.endpoint,框架分配并发布到 etcd)原本挂的是节点间
            // RpcCodec。battle 以 PROTOCOL_GRPC 注册(node.cpp 按 IsGrpcOnlyNodeType 设
            // protocol_type),发现方 node_connector 按 protocol_type 分派,只会拨它的 gRPC 端口、
            // 不会经这个 TCP 端口握手 —— 所以可以把它整个让给客户端直连
            //(与 gate main.cpp 覆写 GetTcpServer 回调同一时机:StartRpcServer 之后)。
            node.SetAfterStart([&context](Node &n)
                               {
                context.clientEdge->Install(n.GetTcpServer());

                // 两个地址都打出来(集群外入口 D76):endpoint 是集群内身份(直连面监听在它的端口上),
                // client_endpoint 是自报的客户端可达地址(未设置 = podip 形态,客户端直接用 endpoint);
                // client_facing 是票据里真正下发的地址(ClientFacing,与 BuildAssignment 同一规则)。
                // NodeInfo 由 Node 持有且跨线程读写不同步;此处在 loop 线程读取。
                const auto &info = n.GetNodeInfo();
                const bool required = n.ClientEndpointRequired();
                const auto clientFacing = client_endpoint::ClientFacing(info, required);
                LOG_INFO << "battle client direct connection endpoint ready: endpoint=" << EndpointForLog(info.endpoint())
                         << " client_endpoint=" << EndpointForLog(info.client_endpoint())
                         << " client_facing=" << (clientFacing ? EndpointForLog(*clientFacing) : std::string("(none)"))
                         << " client_endpoint_required=" << (required ? 1 : 0)
                         << " max_connections=" << gNodeConfigManager.GetBaseDeployConfig().battle_max_connections()
                         << " run_mode=" << battle_security::RunModeName(battle_security::CurrentRunMode());
                if (!clientFacing)
                {
                    // Node 构造期已校验过 CLIENT_ENDPOINT_*,正常走不到;真走到了每张票都会签发失败
                    // (CreateBattle / AddObserver / IssueBattleTicket 一律 kServiceUnavailable),必须响。
                    LOG_ERROR << "battle has no client-reachable address:all ticket issuance will be rejected(fail-closed)";
                }

                // 直连面装好之后才起 Agones lifecycle:POST /ready 意味着可分配,分配出去的房间
                // 玩家马上就要来连直连面。
                StartAgonesLifecycle();

                // EventLoop 心跳(D84):/health 只在 loop 还在转时才发。lifecycle 为 Disabled 时只是一次
                // 原子写,没有读者。lifecycle 是静态单例,回调不捕获任何对象(§11.7 无需 weak 绑定)。
                n.GetLoop()->runEvery(kLoopHeartbeatIntervalSec, []
                                      { agones::GameServerLifecycle::Instance().TouchLoopHeartbeat(); });

                // 最后才开建房准入闸(battle_admission_gate.h):etcd 发布早于本回调,match 可能已经拨过来;
                // 开闸之前 lifecycle 还是默认的 Disabled,许可会不经 allocate 放行,所以那段时间的 CreateBattle
                // 由闸拒绝(UNAVAILABLE battle_not_allocatable,match 换节点)。开闸之后才走正常的许可判定。
                if (!context.battleNodeService->OpenAdmission())
                {
                    LOG_WARN << "battle shutdown started before startup completed;room admission gate remains closed";
                } });

            // 停机收尾,顺序固定:
            //   0) 关建房准入闸(终态)。必须先于作废:本 hook 返回后框架才进入 gRPC 排空,排空期间已收完的
            //      CreateBattle 照常派发,已放行、还排在 loop 队列里的许可回调也会在本任务之后执行 —— 不先关闸,
            //      它们会在作废之后照常建房、发确认事件,房间随进程退出无声丢失。关闸与作废在同一个 loop 任务里,
            //      排在后面的建房在守护段第 1 道闸或许可回调里的复核时必被拒
            //      (UNAVAILABLE battle_not_allocatable,match 换节点),与 lifecycle 当时的状态无关;
            //      房间不再增加;
            //   1) 所有房间停定时器、关直连,不铸造任何终局(观众不收 SpectateEnd,scene 侧 reaper 按冻结到期解冻,
            //      设计文档 §3.2)。已算出、还没拿到 SQL 决定的终局立即再提交一轮,还在打的房间由 match 在权威截止后判 ABORT;
            //      房间留在表里,拿到决定才删,不回 Ready;
            //   2) 断开全部客户端直连;
            //   3) 停 Agones lifecycle worker 并 join(Stop 幂等)。
            //      Stop 经取消令牌中止在途的 allocate / health 等,
            //      不等满超时,通常毫秒级、最坏约 1s(libcurl 空闲期进度回调粒度;grpc-muduo-transport.md §15.16)。
            //      Stop 同时把还在等许可的 CreateBattle 以「拒绝」投进 loop 队列(本任务之后执行,回 battle_not_allocatable)。
            //      刻意**不**调 /shutdown:SIGTERM 通常就是 Agones 删 Pod 发来的,再回敬 /shutdown 是递归删除。
            // 框架随后进入 gRPC 排空(全部在 loop 内:停听、两段 GOAWAY、已收完的调用照常派发,约 3s 硬上限;
            // docs/design/grpc-muduo-transport.md §15.10),并 flush Kafka producer。
            // 2026-10-23 修复(审计 r2-battle#2):第 1) 步已算出、还没拿到 SQL 决定的终局不作废,立即再提交一轮;
            // 下面的排空谓词等它们落定,拆除前钩子才装停止闸。
            auto closeSource = [&context](Node &n)
                                   {
                // 只替换后续新连接的回调；既有连接保留原 DOWN 回调，ConnectionCount 归零前不能签 P1。
                if (n.IsServiceStarted()) {
                    n.GetTcpServer().setConnectionCallback([](const TcpConnectionPtr& conn) { if (conn->connected()) conn->forceClose(); });
                    n.GetTcpServer().setMessageCallback([](const TcpConnectionPtr& conn, muduo::net::Buffer* buffer, Timestamp) { buffer->retrieveAll(); conn->forceClose(); });
                }
                context.battleNodeService->CloseAdmission();
                BattleRoomManager::Instance().AbortAllRooms("node_shutdown");
                if (context.clientEdge)
                {
                    context.clientEdge->DisconnectAll("node_shutdown");
                }
                agones::GameServerLifecycle::Instance().Stop(); };
            node.SetBeforeShutdown(closeSource);
            node.SetMaintenanceInboundRelay(); // match已接收调用的下游；直连客户端/建房根单独关闭。
            node.SetMaintenanceSourceClose(closeSource);
            node.SetMaintenanceRelayStop([](Node &) { BattleRoomManager::Instance().StopSqlOutcomes(); });
            // 停机排空门:补提交的 v1 终局都拿到 COMPLETE / ABORT 才放行收尾,出站到 match 的通道在此之前一直可用
            // (FinalizeShutdownInLoop 才关)。有界:Node 的排空预算(node.cpp kShutdownDrainBudget,15s)到了照常收尾,
            // battle 的 terminationGracePeriodSeconds(tools/scripts/lib/k8s_client_entry.ps1 New-BattleFleetYaml 默认 30s)
            // 覆盖这段等待。没有待提交终局时立即为真。
            // 单例寿命覆盖 loop,回调不捕获任何对象。
            node.SetShutdownDrainComplete([&context](Node &)
                                          { return (!context.clientEdge || context.clientEdge->ConnectionCount() == 0) && BattleRoomManager::Instance().SqlOutcomesFlushed(); });
            // 拆除前钩子:出站通道已关、在途调用的失败回调已跑完。此后到达的回复不能有任何副作用,所以停止闸在这里才装,
            // 并把排空预算内仍没落定的终局逐局打进日志(match 在权威截止后判 ABORT)。
            node.SetBeforeTeardown([](Node &)
                                   { BattleRoomManager::Instance().StopSqlOutcomes(); });
        });
}
