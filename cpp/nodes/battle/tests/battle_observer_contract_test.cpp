// 真实Manager接口回归：仅basic Node本机身份和EventLoop，不启动服务/注册/Redis/Kafka。
// 通过既有MutableNodeInfo临时撤下可达地址，确定性复现幂等补签失败；不加生产故障hook。
// 断线重连宽限用例另在夹具的 loop_ 上起一对回环 TcpServer / TcpClient,只为拿真实的服务端侧连接
// (见 LoopbackDirectConnection);仍不启动 BattleClientEdge,直连断开由用例直接调 DetachDirectConnection 模拟。
// 观众帧编码一次的用例把一个不 Install 的 BattleClientEdge 借给管理器,只用它的出站分帧,在回环客户端侧收字节比对。
// 观战中继 / 广播的用例把出口换成捕获函数(SetSpectateFrameSink),逐条核对 Kafka 记录;结算回合走 HandleSetAutoBattle,不等定时器。
#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include "client/battle_client_edge.h"
#include "data/table_battle_data_provider.h"
#include "logic/battle_room_manager.h"
#include "logic/battle_spectate_relay.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "network/codec/codec.h"
#include "node/system/node/node.h"
#include "node_config_manager.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/match/match_service.pb.h"
#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/match_error_tip.pb.h"
#include "time/system/time.h"

// 借空闲回环端口用(FindFreeLoopbackPort)。放在项目头之后:Windows 上 muduo 头已按正确顺序引入过 WinSock。
#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>
#endif

namespace {
constexpr uint64_t kFirstPlayer = 59001;
constexpr uint64_t kSecondPlayer = 59002;
constexpr uint64_t kObserver = 59003;
// 填满观众席用的观众号段(与参战者 / kObserver 不重叠)。
constexpr uint64_t kFillerObserverBase = 59100;
// 观众席上限只认 BattleRoomManager 的公开常量,这里只收窄成循环与造号用的类型。
constexpr uint32_t kRoomObserverCap = static_cast<uint32_t>(BattleRoomManager::kMaxObserversPerRoom);
// 夹具默认的节点观众席配额:与 dev/test(battle_max_connections=0)装配后的值相同,远大于任何用例的观众数。
// 管理器是进程级单例,每个用例前后都复位到它,注入小配额的用例不会漏到后面的用例里。
constexpr size_t kFixtureObserverSeatQuota =
    BattleRoomManager::ObserverSeatQuotaFor(BattleClientEdge::kHardMaxConnections);
constexpr char kInstance[] = "a1094888-4208-482e-b187-a28a98ca2b54";
constexpr char kFirstOperation[] = "aa094888-4208-482e-b187-a28a98ca2b54";
constexpr char kSecondOperation[] = "bb094888-4208-482e-b187-a28a98ca2b54";
constexpr char kPlacement[] = "cc094888-4208-482e-b187-a28a98ca2b54";
// 中继 gate:两台的 node_id 对默认的 256 分区取模后落在不同分区。
constexpr uint32_t kRelayGate = 12;
constexpr uint32_t kOtherRelayGate = 300;
constexpr char kGateInstance[] = "relay-gate-instance";
constexpr char kOtherGateInstance[] = "other-relay-gate-instance";
constexpr char kDefaultRelayTopic[] = "gate-spectate_g1";
constexpr char kDefaultBroadcastTopic[] = "spectate-bcast_g1";
std::atomic<uint64_t> nextBattleId{590000};

// 观战帧的 Kafka 消息体就是 SpectateFrameEvent(不套信封)。
contracts::kafka::SpectateFrameEvent ParseFrame(const battle_spectate_relay::FrameRecord& record) {
    contracts::kafka::SpectateFrameEvent frame;
    EXPECT_TRUE(frame.ParseFromString(record.payload));
    return frame;
}

// client_frame 必须能还原成客户端信封,信封里是 messageId 对应的 S2C。
template <typename S2C>
S2C ParseClientFrame(const contracts::kafka::SpectateFrameEvent& frame, const uint32_t messageId) {
    ::MessageContent envelope;
    EXPECT_TRUE(envelope.ParseFromString(frame.client_frame()));
    EXPECT_EQ(messageId, envelope.message_id());
    S2C message;
    EXPECT_TRUE(message.ParseFromString(envelope.serialized_message()));
    return message;
}

// 发给单台 gate 的中继帧:默认配置下的 topic / 分区 / key 与事件目标字段。
void ExpectRelayRecord(const battle_spectate_relay::FrameRecord& record, const uint64_t battleId,
                       const uint32_t gateNodeId, const std::string& gateInstanceId) {
    EXPECT_EQ(kDefaultRelayTopic, record.topic);
    EXPECT_EQ(static_cast<int32_t>(gateNodeId % 256), record.partition);
    EXPECT_EQ(std::to_string(battleId), record.key);
    const auto frame = ParseFrame(record);
    EXPECT_EQ(battleId, frame.battle_id());
    EXPECT_EQ(kInstance, frame.battle_instance_id());
    EXPECT_EQ(gateNodeId, frame.target_gate_id());
    EXPECT_EQ(gateInstanceId, frame.target_instance_id());
}

// 全服广播帧:默认配置下的 topic、按 battle_id 定的分区,两个目标字段留空。
void ExpectBroadcastRecord(const battle_spectate_relay::FrameRecord& record, const uint64_t battleId) {
    EXPECT_EQ(kDefaultBroadcastTopic, record.topic);
    EXPECT_EQ(static_cast<int32_t>(battleId % 16), record.partition);
    EXPECT_EQ(std::to_string(battleId), record.key);
    const auto frame = ParseFrame(record);
    EXPECT_EQ(battleId, frame.battle_id());
    EXPECT_EQ(kInstance, frame.battle_instance_id());
    EXPECT_EQ(0u, frame.target_gate_id());
    EXPECT_TRUE(frame.target_instance_id().empty());
}

// 第 index 个填充观众的观战操作号:合法 UUID 形状(小写十六进制、非全零),彼此不同,
// 也不与上面几个固定操作号相撞。
std::string FillerOperation(uint32_t index) {
    char buffer[40];
    std::snprintf(buffer, sizeof(buffer), "%08x-4208-482e-b187-a28a98ca2b54", 0x10000000u + index);
    return buffer;
}

// ---- 回环直连(断线重连宽限 / 观众帧编码一次用例)----
// 夹具的 loop_ 就在测试线程上、平时不跑。Attach / Detach 只要一条真实的服务端侧连接,
// 这里在 loop_ 上起一对回环 TcpServer / TcpClient,只在建连、收帧与拆除时限时驱动 loop()。

// 只作活性上限,不作判据(同 rpc_channel_loopback_test 的 kLiveness)。
constexpr double kLoopLivenessSec = 5.0;
// 驱动期间检查完成条件的间隔。Linux 版由 timerfd 按此间隔唤醒 poll;Windows 版 poll 固定 100ms 超时、
// 返回后才扫定时器(EventLoop.cc kPollTimeMs),空闲时实际检查粒度约 100ms,只影响用例耗时、不影响判据。
constexpr double kLoopPollIntervalSec = 0.005;

// 向 OS 借一个空闲回环端口(muduo 的 TcpServer 不暴露实际绑定端口,同 rpc_channel_loopback_test)。失败返回 0。
uint16_t FindFreeLoopbackPort() {
#ifdef _WIN32
    const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s == INVALID_SOCKET) return 0;
    int len = sizeof(sockaddr_in);
#else
    const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (s < 0) return 0;
    socklen_t len = sizeof(sockaddr_in);
#endif
    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    uint16_t port = 0;
    if (::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) == 0
        && ::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len) == 0) {
        port = ntohs(addr.sin_port);
    }
#ifdef _WIN32
    ::closesocket(s);
#else
    ::close(s);
#endif
    return port;
}

// 在测试线程上驱动 loop,直到 done() 为真或活性上限到期,返回 done() 是否成立。条件满足后经两跳
// queueInLoop 再 quit(同 Node 关机,AGENTS.md §11.7):此前排进队列的 connectDestroyed 等按 FIFO 先跑完。
// 定时器回调与排队的两跳 functor 只捕获 loop 自身与 weak_ptr<状态>,返回前两只定时器都已取消;
// 活性上限与第一跳同轮触发时第二跳会留在队列里,本次驱动返回后状态已析构,它在下次驱动里 lock 失败即不 quit。
bool DriveLoopUntil(muduo::net::EventLoop& loop, std::function<bool()> done) {
    struct DriveState {
        std::function<bool()> done;
        bool quitting = false;
    };
    const auto state = std::make_shared<DriveState>();
    state->done = std::move(done);
    auto* const loopPtr = &loop;
    const auto checkTimer = loop.runEvery(kLoopPollIntervalSec, [loopPtr, weak = std::weak_ptr<DriveState>(state)] {
        const auto current = weak.lock();
        if (!current || current->quitting || !current->done()) return;
        current->quitting = true;
        loopPtr->queueInLoop([loopPtr, weak] {
            loopPtr->queueInLoop([loopPtr, weak] {
                if (weak.lock()) loopPtr->quit();
            });
        });
    });
    const auto livenessTimer = loop.runAfter(kLoopLivenessSec, [loopPtr] { loopPtr->quit(); });
    loop.loop();
    loop.cancel(checkTimer);
    loop.cancel(livenessTimer);
    return state->done();
}

// 一条真实的回环 TCP 连接,只为拿服务端侧连接喂给 AttachDirectConnection / DetachDirectConnection
// (二者只比对连接身份,Detach 不要求连接真的断开)。连接回调只捕获 weak_ptr<Probe>,本类只存服务端连接的
// weak(AGENTS.md §11.7)。ServerSide() 每次 lock() 出一个临时强引用,调用方不要长期持有:强引用会让
// 服务端 fd 不关,拆除时客户端等不到 EOF。
// 拆除:客户端 disconnect(半关写端)→ 服务端读到 EOF 关闭 → 客户端读到 EOF 关闭;两端都 DOWN 且
// connectDestroyed 排空后才析构 TcpClient / TcpServer —— 二者把裸 this 绑进连接的 close 回调,必须活到
// 连接关完;连接析构时也必须已是 kDisconnected(TcpConnection.cc:71)。
class LoopbackDirectConnection {
public:
    explicit LoopbackDirectConnection(muduo::net::EventLoop& loop) : loop_(loop) {}
    ~LoopbackDirectConnection() { Close(); }
    LoopbackDirectConnection(const LoopbackDirectConnection&) = delete;
    LoopbackDirectConnection& operator=(const LoopbackDirectConnection&) = delete;

    // 建连并等两端都 UP;借不到端口或活性上限内没连上返回 false。
    bool Open(const std::string& name) {
        const uint16_t port = FindFreeLoopbackPort();
        if (port == 0) return false;
        server_ = std::make_unique<muduo::net::TcpServer>(
            &loop_, muduo::net::InetAddress(port, /*loopbackOnly=*/true), name + "_server");
        server_->setConnectionCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr& conn) {
            const auto probe = weak.lock();
            if (!probe) return;
            if (conn->connected()) {
                probe->serverSide = conn;
                probe->serverUp = true;
            } else {
                probe->serverDown = true;
            }
        });
        server_->start();
        client_ = std::make_unique<muduo::net::TcpClient>(
            &loop_, muduo::net::InetAddress("127.0.0.1", port), name + "_client");
        client_->setConnectionCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr& conn) {
            const auto probe = weak.lock();
            if (!probe) return;
            if (conn->connected()) probe->clientUp = true;
            else probe->clientDown = true;
        });
        // 客户端侧按到达顺序收下全部字节,供用例比对服务端经直连写出的帧
        client_->setMessageCallback([weak = std::weak_ptr<Probe>(probe_)](const muduo::net::TcpConnectionPtr&,
                                                                          muduo::net::Buffer* buffer, muduo::Timestamp) {
            const auto probe = weak.lock();
            if (!probe) {
                buffer->retrieveAll();
                return;
            }
            probe->clientReceived.append(buffer->retrieveAllAsString());
        });
        client_->connect();
        return DriveLoopUntil(loop_, [probe = probe_] { return probe->serverUp && probe->clientUp; });
    }

    muduo::net::TcpConnectionPtr ServerSide() const { return probe_->serverSide.lock(); }
    // 客户端侧至今收到的全部字节。只在测试线程(即 loop_ 线程)读。
    const std::string& ClientReceived() const { return probe_->clientReceived; }

private:
    struct Probe {
        std::weak_ptr<muduo::net::TcpConnection> serverSide;
        bool serverUp = false;
        bool serverDown = false;
        bool clientUp = false;
        bool clientDown = false;
        std::string clientReceived;
    };

    void Close() {
        if (!client_ && !server_) return;
        if (client_) client_->disconnect();
        const bool closed = DriveLoopUntil(loop_, [probe = probe_] {
            return (!probe->serverUp || probe->serverDown) && (!probe->clientUp || probe->clientDown);
        });
        if (!closed) {
            // 活性上限内没关完:此时析构会让连接带着非 kDisconnected 状态析构(TcpConnection.cc:71),
            // 或让连接的 close 回调打到已析构的 TcpServer / TcpClient。宁可泄漏这对测试端点并报失败。
            ADD_FAILURE() << "loopback direct connection did not close within liveness bound";
            (void)client_.release();
            (void)server_.release();
            return;
        }
        client_.reset();
        server_.reset();
        // 再转一圈:TcpClient 析构排队的 Connector 收尾等 functor 在本用例内跑完
        (void)DriveLoopUntil(loop_, [] { return true; });
    }

    muduo::net::EventLoop& loop_;
    const std::shared_ptr<Probe> probe_ = std::make_shared<Probe>();
    std::unique_ptr<muduo::net::TcpServer> server_;
    std::unique_ptr<muduo::net::TcpClient> client_;
};

// 用例作用域内把直连面借给管理器,出作用域即收回。声明在直连面之后 = 先于它析构:用例中途失败提前返回时,
// TearDown 再销毁房间也不会经已析构的直连面写帧。
class ClientEdgeBinding {
public:
    explicit ClientEdgeBinding(BattleClientEdge& edge) { BattleRoomManager::Instance().SetClientEdge(&edge); }
    ~ClientEdgeBinding() { BattleRoomManager::Instance().SetClientEdge(nullptr); }
    ClientEdgeBinding(const ClientEdgeBinding&) = delete;
    ClientEdgeBinding& operator=(const ClientEdgeBinding&) = delete;
};

class ObserverFixtureEnvironment {
public:
    ObserverFixtureEnvironment(const char* name, const char* value) : name_(name) {
        if (const auto* previous = std::getenv(name)) previous_ = previous;
        Put(value);
    }
    ~ObserverFixtureEnvironment() { Put(previous_ ? previous_->c_str() : nullptr); }
private:
    void Put(const char* value) const {
#ifdef _WIN32
        (void)_putenv_s(name_.c_str(), value ? value : "");
#else
        if (value) (void)setenv(name_.c_str(), value, 1);
        else (void)unsetenv(name_.c_str());
#endif
    }
    std::string name_;
    std::optional<std::string> previous_;
};

// v1未知房间按生产合同不能Destroy；接口回归在独立子进程中建房，父进程不留下房间或定时器。
// threadsafe模式重新执行测试进程；退出码携带全部断言结果，OS退出负责回收该受控子进程资源。
void AssertInFreshOutcomeProcess(const std::function<void()>& check) {
    struct RestoreDeathStyle {
        std::string previous;
        ~RestoreDeathStyle() { GTEST_FLAG_SET(death_test_style, previous); }
    } restore{GTEST_FLAG_GET(death_test_style)};
    GTEST_FLAG_SET(death_test_style, "threadsafe");
    ASSERT_EXIT({ check(); std::_Exit(::testing::Test::HasFailure() ? 1 : 0); },
                ::testing::ExitedWithCode(0), "");
}

class BattleObserverContract : public testing::Test {
protected:
    void SetUp() override {
        savedGlobal_ = gNodeConfigManager;
        savedThread_ = tlsNodeConfigManager;
        ASSERT_EQ(nullptr, gNode);
        loop_ = std::make_unique<muduo::net::EventLoop>();
        const auto nonce = std::chrono::steady_clock::now().time_since_epoch().count();
        logDirectory_ = std::filesystem::temp_directory_path() / ("battle-observer-interface-" + std::to_string(nonce));
        ASSERT_TRUE(std::filesystem::create_directory(logDirectory_));
        RecordProperty("log_directory", logDirectory_.string());
        // 此构造器不执行Initialize，不加载网络客户端或生成的业务注册表。
        node_ = std::make_unique<Node>(loop_.get(), (logDirectory_ / "node").string());
        auto& self = node_->MutableNodeInfo();
        self.set_node_id(87);
        self.set_node_type(common::base::BattleNodeService);
        self.set_node_uuid(kInstance);
        self.mutable_endpoint()->set_ip("127.0.0.1");
        self.mutable_endpoint()->set_port(18087);
        originalNode_ = self;
        gNodeConfigManager.GetBaseDeployConfig().set_battle_token_secret("unit-observer-only-ticket-key-0123456789abcdef");
        gNodeConfigManager.GetBaseDeployConfig().clear_gate_token_secret();
        gNodeConfigManager.GetBaseDeployConfig().mutable_kafka()->clear_brokers();
        tlsNodeConfigManager = gNodeConfigManager;
        // 房间身份只依赖测试序列；日志目录nonce不参与任何业务断言或授权。
        battleId_ = nextBattleId.fetch_add(1, std::memory_order_relaxed);
        // TimeSystem没有可注入时钟。本接口测试不推进/等待时间，只在同一原件中
        // 固定一次真实准入时刻，验证失败补签、幂等与操作隔离；不声称覆盖时钟边界。
        originalNowMs_ = TimeSystem::NowMillisecondsUTC();
        manager().SetRoomLifecycleHooks({});
        manager().SetClientEdge(nullptr);
        // 房间引擎的数据源由管理器注入(生产在 main.cpp 表加载后装),不注入则 Create() 一律 kInvalidTableData。
        // 用读全局表的生产实现:表没加载,空表足够开 PVP 1v1(没有技能 / 宠物 / 副本行要查)。
        manager().SetDataProvider(std::make_shared<turnbattle::TableBattleDataProvider>());
        manager().SetObserverSeatQuota(kFixtureObserverSeatQuota);
        manager().SetSpectateFrameSink([this](const battle_spectate_relay::FrameRecord& record) {
            spectateFrames_.push_back(record);
            return spectateSinkAccepts_;
        });
    }

    void TearDown() override {
        if (node_) {
            // 夹具房间连同用例另建的房间一起销毁:管理器是进程级单例,漏掉的房间(连同观众)会带进后续用例,
            // 还会占用节点观众席配额
            std::vector<uint64_t> rooms = extraBattles_;
            rooms.push_back(battleId_);
            for (const auto battleId : rooms) Destroy(battleId);
            manager().SetClientEdge(nullptr);
            node_.reset(); // 既有Node两跳排空完成后才销毁EventLoop。
        }
        manager().SetObserverSeatQuota(kFixtureObserverSeatQuota);
        manager().SetSpectateFrameSink(nullptr);
        manager().SetDataProvider(nullptr);
        loop_.reset();
        gNodeConfigManager = savedGlobal_;
        tlsNodeConfigManager = savedThread_;
    }

    static BattleRoomManager& manager() { return BattleRoomManager::Instance(); }

    // 按夹具原件的放置操作销毁房间;房间不存在时幂等。
    static void Destroy(const uint64_t battleId) {
        DestroyBattleRequest request;
        request.set_battle_id(battleId);
        request.set_placement_operation_id(kPlacement);
        request.set_target_instance_id(kInstance);
        manager().HandleDestroyBattle(request);
    }

    CreateBattleRequest OriginalCreation(uint32_t outcomeProtocol = 0) const {
        CreateBattleRequest request;
        request.set_battle_id(battleId_);
        request.set_outcome_protocol_version(outcomeProtocol);
        request.set_match_mode(::match::MATCH_MODE_1V1);
        request.set_seed(1);
        request.set_created_at_ms(originalNowMs_);
        request.set_deadline_ms(originalNowMs_ + 180000);
        request.set_placement_operation_id(kPlacement);
        request.set_target_instance_id(kInstance);
        request.set_required_physical_region(1);
        request.set_admit_before_ms(originalNowMs_ + 30000);
        // 夹具建的都是公开局:私密 / 未填可见性的房间会被 AddObserver 的第二道闸拒掉全部观众。
        // 要私密局的用例经 CreateExtraRoom 指定。
        request.set_spectate_visibility(::SPECTATE_VISIBILITY_PUBLIC);
        for (uint32_t side = 0; side < 2; ++side) {
            auto* player = request.add_players();
            player->set_player_id(side == 0 ? kFirstPlayer : kSecondPlayer);
            player->set_player_name("observer-interface-participant");
            player->set_team_index(side);
            player->set_level(1);
            player->set_max_health(1000);
            player->set_max_mana(100);
            player->mutable_base_attributes()->set_health(1000);
            player->mutable_base_attributes()->set_mana(100);
            player->mutable_base_attributes()->set_speed(12);
            auto* route = player->mutable_routing();
            route->set_session_id(side + 1);
            route->set_gate_node_id(1);
            route->set_gate_instance_id("interface-no-live-gate");
            route->set_scene_node_id(1);
            route->set_scene_instance_id("interface-no-live-scene");
            route->set_zone_id(1);
        }
        return request;
    }

    QueryBattleCreationResponse CreationState() const {
        QueryBattleCreationRequest query;
        query.set_battle_id(battleId_);
        query.set_placement_operation_id(kPlacement);
        query.set_target_instance_id(kInstance);
        QueryBattleCreationResponse state;
        manager().HandleQueryBattleCreation(query, state);
        return state;
    }

    void Create() {
        const auto request = OriginalCreation();
        CreateBattleResponse reply;
        manager().HandleCreateBattle(request, reply);
        ASSERT_EQ(0u, reply.error_message().id());
        QueryBattleCreationRequest query;
        query.set_battle_id(battleId_);
        query.set_placement_operation_id(kPlacement);
        query.set_target_instance_id(kInstance);
        QueryBattleCreationResponse state;
        manager().HandleQueryBattleCreation(query, state);
        ASSERT_EQ(BATTLE_CREATION_PRESENT, state.state());
    }

    // 夹具房间之外另建一间(可见性由用例指定),TearDown 统一销毁。
    void CreateExtraRoom(const uint64_t battleId, const ::eSpectateVisibility visibility) {
        auto request = OriginalCreation();
        request.set_battle_id(battleId);
        request.set_spectate_visibility(visibility);
        extraBattles_.push_back(battleId);
        CreateBattleResponse reply;
        manager().HandleCreateBattle(request, reply);
        ASSERT_EQ(0u, reply.error_message().id());
    }

    AddObserverRequest Original(const std::string& operation) const {
        AddObserverRequest request;
        request.set_battle_id(battleId_);
        request.set_observer_player_id(kObserver);
        request.set_observer_name("公开观众名");
        request.set_spectator_operation_id(operation);
        request.set_target_instance_id(kInstance);
        request.set_admit_before_ms(originalNowMs_ + 30000);
        request.mutable_routing()->set_session_id(3);
        request.mutable_routing()->set_gate_node_id(1);
        request.mutable_routing()->set_gate_instance_id("interface-no-live-gate");
        request.mutable_routing()->set_zone_id(1);
        return request;
    }

    ObserverAdmissionState Query(const AddObserverRequest& original) {
        QueryObserverRequest query;
        query.set_battle_id(original.battle_id());
        query.set_observer_player_id(original.observer_player_id());
        query.set_spectator_operation_id(original.spectator_operation_id());
        query.set_target_instance_id(original.target_instance_id());
        QueryObserverResponse reply;
        manager().HandleQueryObserver(query, reply);
        return reply.state();
    }

    RemoveObserverResponse Remove(const AddObserverRequest& original) {
        RemoveObserverRequest request;
        request.set_battle_id(original.battle_id());
        request.set_observer_player_id(original.observer_player_id());
        request.set_spectator_operation_id(original.spectator_operation_id());
        request.set_target_instance_id(original.target_instance_id());
        request.set_admit_before_ms(original.admit_before_ms());
        RemoveObserverResponse reply;
        manager().HandleRemoveObserver(request, reply);
        return reply;
    }

    void AssertTicket(const AddObserverRequest& original) {
        IssueBattleTicketRequest request;
        request.set_battle_id(battleId_);
        request.set_player_id(kObserver);
        request.set_target_instance_id(kInstance);
        request.set_spectator_operation_id(original.spectator_operation_id());
        IssueBattleTicketResponse reply;
        manager().HandleIssueBattleTicket(request, reply);
        ASSERT_EQ(0u, reply.error_message().id());
        BattleTicketPayload payload;
        ASSERT_TRUE(payload.ParseFromString(reply.assignment().token_payload()));
        EXPECT_EQ(original.spectator_operation_id(), payload.spectator_operation_id());
        EXPECT_EQ(kInstance, payload.battle_instance_id());
        EXPECT_FALSE(reply.assignment().token_signature().empty());
    }

    // 与 Original 相同,只换观众号;用于填满观众席。
    AddObserverRequest OriginalFor(uint64_t observerId, const std::string& operation) const {
        auto request = Original(operation);
        request.set_observer_player_id(observerId);
        return request;
    }

    // 与 OriginalFor 相同,再换房间;用于在 CreateExtraRoom 建的房间里登记。
    AddObserverRequest OriginalIn(uint64_t battleId, uint64_t observerId, const std::string& operation) const {
        auto request = OriginalFor(observerId, operation);
        request.set_battle_id(battleId);
        return request;
    }

    uint32_t AddError(const AddObserverRequest& original) {
        AddObserverResponse reply;
        manager().HandleAddObserver(original, reply);
        return reply.error_message().id();
    }

    uint32_t TicketError(const AddObserverRequest& original) {
        IssueBattleTicketRequest request;
        request.set_battle_id(original.battle_id());
        request.set_player_id(original.observer_player_id());
        request.set_target_instance_id(original.target_instance_id());
        request.set_spectator_operation_id(original.spectator_operation_id());
        IssueBattleTicketResponse reply;
        manager().HandleIssueBattleTicket(request, reply);
        return reply.error_message().id();
    }

    // 把观众席填满:第 index 名用 kFillerObserverBase + index 与 FillerOperation(index)。
    void FillRoom(std::vector<AddObserverRequest>& seated) {
        for (uint32_t index = 0; index < kRoomObserverCap; ++index) {
            seated.push_back(OriginalFor(kFillerObserverBase + index, FillerOperation(index)));
            ASSERT_EQ(0u, AddError(seated.back())) << "filler index=" << index;
        }
    }

    AttachRelayGateResponse Attach(const uint64_t battleId, const uint32_t gateNodeId, const std::string& gateInstanceId,
                                   const std::string& targetInstanceId = kInstance) {
        AttachRelayGateRequest request;
        request.set_battle_id(battleId);
        request.set_gate_node_id(gateNodeId);
        request.set_gate_instance_id(gateInstanceId);
        request.set_target_instance_id(targetInstanceId);
        AttachRelayGateResponse reply;
        manager().HandleAttachRelayGate(request, reply);
        return reply;
    }

    PromoteSpectateBroadcastResponse Promote(const uint64_t battleId, const std::string& targetInstanceId = kInstance) {
        PromoteSpectateBroadcastRequest request;
        request.set_battle_id(battleId);
        request.set_target_instance_id(targetInstanceId);
        PromoteSpectateBroadcastResponse reply;
        manager().HandlePromoteSpectateBroadcast(request, reply);
        return reply;
    }

    void SetAuto(const uint64_t battleId, const uint64_t playerId, const bool enabled) {
        SessionDetails session;
        session.set_player_id(playerId);
        SetAutoBattleRequest request;
        request.set_battle_id(battleId);
        request.set_enabled(enabled);
        SetAutoBattleResponse reply;
        manager().HandleSetAutoBattle(session, request, reply);
        ASSERT_EQ(0u, reply.error_message().id());
    }

    // 结算一回合,不等定时器:第二名参战者挂机,第一名切回手动再切自动,「未就绪 → 全员就绪」的翻转立即结算。
    // 夹具的 1000 气血几回合内打不死人(普攻基础伤害 10),房间保持 ONGOING,TearDown 能正常销毁。
    void DriveOneRound(const uint64_t battleId) {
        ASSERT_NO_FATAL_FAILURE(SetAuto(battleId, kSecondPlayer, true));
        ASSERT_NO_FATAL_FAILURE(SetAuto(battleId, kFirstPlayer, false));
        ASSERT_NO_FATAL_FAILURE(SetAuto(battleId, kFirstPlayer, true));
    }

    // 取走至今捕获的观战帧。
    std::vector<battle_spectate_relay::FrameRecord> TakeFrames() { return std::exchange(spectateFrames_, {}); }

    ObserverFixtureEnvironment global_{"GLOBAL_MATCH_ENABLED", "true"};
    ObserverFixtureEnvironment region_{"PHYSICAL_REGION", "1"};
    NodeConfigManager savedGlobal_;
    NodeConfigManager savedThread_;
    std::unique_ptr<muduo::net::EventLoop> loop_;
    std::unique_ptr<Node> node_;
    NodeInfo originalNode_;
    std::filesystem::path logDirectory_;
    uint64_t battleId_{0};
    uint64_t originalNowMs_{0};
    // CreateExtraRoom 建的房间,TearDown 与 battleId_ 一起销毁。
    std::vector<uint64_t> extraBattles_;
    // 观战帧出口捕获到的记录,以及出口对它们的回答(false = 模拟没进生产者队列)。
    std::vector<battle_spectate_relay::FrameRecord> spectateFrames_;
    bool spectateSinkAccepts_ = true;
};

TEST_F(BattleObserverContract, AssignmentFailureRemovesOriginalOperationAndNewOperationRecovers) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto first = Original(kFirstOperation);
    AddObserverResponse added;
    manager().HandleAddObserver(first, added);
    ASSERT_EQ(0u, added.error_message().id());
    ASSERT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));

    // 保持真实房间/实例/原Add不变，只让既有签票地址判定失败。
    node_->MutableNodeInfo().clear_endpoint();
    node_->MutableNodeInfo().clear_client_endpoint();
    AddObserverResponse failed;
    manager().HandleAddObserver(first, failed);
    node_->MutableNodeInfo() = originalNode_;
    ASSERT_EQ(static_cast<uint32_t>(::kServiceUnavailable), failed.error_message().id());
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(first));

    // 不靠Remove补偿掩盖Add失败分支；新op必须已经可以接管空槽并补签。
    const auto next = Original(kSecondOperation);
    AddObserverResponse recovered;
    manager().HandleAddObserver(next, recovered);
    ASSERT_EQ(0u, recovered.error_message().id());
    ASSERT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(next));
    ASSERT_NO_FATAL_FAILURE(AssertTicket(next));
    EXPECT_TRUE(Remove(first).cancelled());
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(next));
}

TEST_F(BattleObserverContract, CancelBeforeRoomAddAndLateOldRemoveCannotRemoveNewOperation) {
    const auto cancelled = Original(kFirstOperation);
    const auto proof = Remove(cancelled);
    ASSERT_TRUE(proof.cancelled());
    EXPECT_EQ(kFirstOperation, proof.spectator_operation_id());
    EXPECT_EQ(kInstance, proof.target_instance_id());
    ASSERT_NO_FATAL_FAILURE(Create());
    AddObserverResponse late;
    manager().HandleAddObserver(cancelled, late);
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), late.error_message().id());
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(cancelled));

    const auto next = Original(kSecondOperation);
    AddObserverResponse added;
    manager().HandleAddObserver(next, added);
    ASSERT_EQ(0u, added.error_message().id());
    AddObserverResponse replay;
    manager().HandleAddObserver(next, replay);
    ASSERT_EQ(0u, replay.error_message().id());
    auto changed = next;
    changed.set_observer_name("改过的原件必须拒绝");
    AddObserverResponse rejected;
    manager().HandleAddObserver(changed, rejected);
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), rejected.error_message().id());
    EXPECT_TRUE(Remove(cancelled).cancelled());
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(next));
    ASSERT_NO_FATAL_FAILURE(AssertTicket(next));
}

// 观众席满回 match 段的专用码(battle→match 与 match→客户端共用),不再借用限流码;
// 已登记观众的同 op 原件重放先于满员判定,满员后照样成功;被拒的 op 是终态。
TEST_F(BattleObserverContract, FullRoomRejectsWithSpectateFullWhileSeatedReplaySucceeds) {
    ASSERT_NO_FATAL_FAILURE(Create());
    std::vector<AddObserverRequest> seated;
    ASSERT_NO_FATAL_FAILURE(FillRoom(seated));

    const auto overflow = OriginalFor(kFillerObserverBase + kRoomObserverCap, FillerOperation(kRoomObserverCap));
    const auto rejected = AddError(overflow);
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), rejected);
    EXPECT_NE(static_cast<uint32_t>(::kRateLimitExceeded), rejected);
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(overflow));

    EXPECT_EQ(0u, AddError(seated.front()));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(seated.front()));
    EXPECT_EQ(0u, AddError(seated.back()));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(seated.back()));

    // 拒绝即终态:同 op 重放不会重新判定成准入,回首次拒绝码(前台与后台恢复并发发同一个 op 时结论一致),
    // Remove 拿得到 cancelled 证明
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(overflow));
    EXPECT_TRUE(Remove(overflow).cancelled());
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(overflow));
}

// 登记后一直没建直连的席位:连接宽限内保持 ACTIVE(补签不延长宽限),宽限后被回收为 ABSENT;
// 回收封闭原 op(同 op 不复活),新 op 可以重新准入。时刻由回收入口显式传入,不等真实 30s。
TEST_F(BattleObserverContract, NeverConnectedSeatReclaimedAfterConnectGraceWithoutRevival) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto first = Original(kFirstOperation);
    const auto before = std::chrono::steady_clock::now();
    ASSERT_EQ(0u, AddError(first));
    const auto after = std::chrono::steady_clock::now();
    ASSERT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));

    // 期限落在 [before, after] + 宽限 之内:早于 before + 宽限 的时刻一律不回收
    manager().ReclaimExpiredObserverSeats(
        battleId_, before + BattleRoomManager::kObserverConnectGrace - std::chrono::milliseconds(1));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));
    // 补签成功但不续期:下面仍按登记时的期限回收
    ASSERT_NO_FATAL_FAILURE(AssertTicket(first));

    manager().ReclaimExpiredObserverSeats(battleId_, after + BattleRoomManager::kObserverConnectGrace);
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(first));
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), TicketError(first));

    // 墓碑已封闭(原准入截止未过)或已删(截止已过,由时间校验挡住),同 op 都回 kInvalidParameter
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), AddError(first));
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(first));
    EXPECT_TRUE(Remove(first).cancelled());

    const auto next = Original(kSecondOperation);
    ASSERT_EQ(0u, AddError(next));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(next));
    ASSERT_NO_FATAL_FAILURE(AssertTicket(next));
}

// 满员时被拒的第 21 名,在占座者到期回收后换新 op 即可进入;它原来被拒的 op 仍是终态。
TEST_F(BattleObserverContract, ReclaimedSeatsAdmitTheTwentyFirstObserver) {
    ASSERT_NO_FATAL_FAILURE(Create());
    std::vector<AddObserverRequest> seated;
    ASSERT_NO_FATAL_FAILURE(FillRoom(seated));
    const auto filled = std::chrono::steady_clock::now();

    const auto waiting = OriginalFor(kFillerObserverBase + kRoomObserverCap, FillerOperation(kRoomObserverCap));
    ASSERT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(waiting));

    manager().ReclaimExpiredObserverSeats(battleId_, filled + BattleRoomManager::kObserverConnectGrace);
    for (const auto& request : seated) {
        EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(request)) << "observer=" << request.observer_player_id();
    }

    // 腾出席位后,原 op 仍回首次拒绝码(拒绝即终态),换新 op 才能进
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(waiting));
    const auto retry = OriginalFor(kFillerObserverBase + kRoomObserverCap, FillerOperation(kRoomObserverCap + 1));
    ASSERT_EQ(0u, AddError(retry));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(retry));
}

// 挂上直连即停止连接宽限;直连断开后开始重连宽限,期内保持 ACTIVE(补签不续期),期满被回收,
// 回收后旧票重连握手、补签、同 op Add 都不能复活。断开用 DetachDirectConnection 模拟直连面的
// 断开回调(它只比对连接身份),回收时刻由回收入口显式传入。
TEST_F(BattleObserverContract, DirectConnectionStopsSeatClockAndDetachStartsReconnectGrace) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto first = Original(kFirstOperation);
    ASSERT_EQ(0u, AddError(first));
    const auto admitted = std::chrono::steady_clock::now();

    LoopbackDirectConnection direct(*loop_);
    ASSERT_TRUE(direct.Open("observer_direct"));
    uint32_t gateSessionId = 0;
    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, kObserver, ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 direct.ServerSide(), &gateSessionId, kFirstOperation));
    EXPECT_EQ(first.routing().session_id(), gateSessionId);

    // 挂上直连即停表:远超连接宽限也不回收
    manager().ReclaimExpiredObserverSeats(
        battleId_, admitted + BattleRoomManager::kObserverConnectGrace + std::chrono::hours(1));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));

    const auto beforeDetach = std::chrono::steady_clock::now();
    manager().DetachDirectConnection(battleId_, kObserver, direct.ServerSide());
    const auto afterDetach = std::chrono::steady_clock::now();

    // 期限落在 [beforeDetach, afterDetach] + 重连宽限之内:早于 beforeDetach + 宽限 的时刻一律不回收
    manager().ReclaimExpiredObserverSeats(
        battleId_, beforeDetach + BattleRoomManager::kObserverReconnectGrace - std::chrono::milliseconds(1));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));
    ASSERT_NO_FATAL_FAILURE(AssertTicket(first));

    manager().ReclaimExpiredObserverSeats(battleId_, afterDetach + BattleRoomManager::kObserverReconnectGrace);
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(first));
    EXPECT_FALSE(manager().AttachDirectConnection(battleId_, kObserver, ::BATTLE_TICKET_ROLE_OBSERVER,
                                                  direct.ServerSide(), nullptr, kFirstOperation));
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), TicketError(first));
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), AddError(first));
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(first));
}

// 断线后在重连宽限内换一条新直连挂上即停表;旧连接晚到的断开回调按身份比对什么都不摘,
// 也不会重新开始计时。远超两段宽限的时刻回收,席位仍在。
TEST_F(BattleObserverContract, ReconnectWithinGraceKeepsSeatAndStaleDetachIsIgnored) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto first = Original(kFirstOperation);
    ASSERT_EQ(0u, AddError(first));

    LoopbackDirectConnection previous(*loop_);
    LoopbackDirectConnection current(*loop_);
    ASSERT_TRUE(previous.Open("observer_previous"));
    ASSERT_TRUE(current.Open("observer_current"));

    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, kObserver, ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 previous.ServerSide(), nullptr, kFirstOperation));
    manager().DetachDirectConnection(battleId_, kObserver, previous.ServerSide());
    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, kObserver, ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 current.ServerSide(), nullptr, kFirstOperation));
    manager().DetachDirectConnection(battleId_, kObserver, previous.ServerSide());
    EXPECT_TRUE(manager().OwnsDirectConnection(battleId_, kObserver, ::BATTLE_TICKET_ROLE_OBSERVER,
                                               current.ServerSide(), kFirstOperation));

    const auto reattached = std::chrono::steady_clock::now();
    manager().ReclaimExpiredObserverSeats(
        battleId_, reattached + BattleRoomManager::kObserverConnectGrace + BattleRoomManager::kObserverReconnectGrace);
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));
    ASSERT_NO_FATAL_FAILURE(AssertTicket(first));
}

// 可见性第二道闸(battle-spectate-tiered-fanout.md §10 第 6 条):私密局与未填可见性的局(默认拒绝)一律回
// kInvalidParameter —— 房间还在,不能回 kEntityIsNull(match 会懒剔除观战索引),也不是满员。拒绝封闭原 op:
// 名单里没有这名观众、补签被拒,同 op 迟到的 Add 不会翻成准入,Remove 拿得到取消证明。
TEST_F(BattleObserverContract, NonPublicBattleRejectsObserverAsTerminalInvalidParameter) {
    const std::vector<std::pair<::eSpectateVisibility, const char*>> cases{
        {::SPECTATE_VISIBILITY_PRIVATE, kFirstOperation},
        {::SPECTATE_VISIBILITY_UNSPECIFIED, kSecondOperation}};
    for (const auto& [visibility, operation] : cases) {
        SCOPED_TRACE(::eSpectateVisibility_Name(visibility));
        const auto battleId = nextBattleId.fetch_add(1, std::memory_order_relaxed);
        ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(battleId, visibility));
        const auto original = OriginalIn(battleId, kObserver, operation);

        const auto rejected = AddError(original);
        EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), rejected);
        EXPECT_NE(static_cast<uint32_t>(::kEntityIsNull), rejected);
        EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(original));
        EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), TicketError(original));

        EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), AddError(original));
        EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(original));
        EXPECT_TRUE(Remove(original).cancelled());
        EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(original));
    }
}

// 节点观众席配额按全部房间的名单人数之和计(含还没建直连的观众):两个房间都远没满(单房间上限 20),
// 跨房间凑满注入的配额后,下一名回与单房间满员同一个码;已登记观众的同 op 重放先于配额判定,照常成功。
// 任一房间回收一席后换新 op 即可进入,被拒的 op 仍是终态。配额由用例直接注入,不依赖装配时的连接上限。
TEST_F(BattleObserverContract, NodeObserverQuotaSpansRoomsAndReopensAfterReclaim) {
    constexpr size_t kInjectedQuota = 3;
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto otherBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(otherBattle, ::SPECTATE_VISIBILITY_PUBLIC));
    manager().SetObserverSeatQuota(kInjectedQuota);

    const auto first = OriginalFor(kFillerObserverBase, FillerOperation(0));
    const auto second = OriginalFor(kFillerObserverBase + 1, FillerOperation(1));
    ASSERT_EQ(0u, AddError(first));
    ASSERT_EQ(0u, AddError(second));
    const auto elsewhere = OriginalIn(otherBattle, kFillerObserverBase + 2, FillerOperation(2));
    ASSERT_EQ(0u, AddError(elsewhere));
    const auto elsewhereAdmitted = std::chrono::steady_clock::now();

    const auto fourth = OriginalFor(kFillerObserverBase + 3, FillerOperation(3));
    const auto rejected = AddError(fourth);
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), rejected);
    EXPECT_NE(static_cast<uint32_t>(::kRateLimitExceeded), rejected);
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(fourth));
    EXPECT_EQ(0u, AddError(first));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(first));

    // 另一房间那一席过了连接宽限被回收,节点腾出一席
    manager().ReclaimExpiredObserverSeats(otherBattle, elsewhereAdmitted + BattleRoomManager::kObserverConnectGrace);
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(elsewhere));

    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(fourth));
    const auto retry = OriginalFor(kFillerObserverBase + 3, FillerOperation(4));
    ASSERT_EQ(0u, AddError(retry));
    EXPECT_EQ(OBSERVER_ADMISSION_ACTIVE, Query(retry));
    // 再次凑满配额:下一名照样被拒,换到另一房间也一样
    const auto fifth = OriginalIn(otherBattle, kFillerObserverBase + 4, FillerOperation(5));
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(fifth));
}

// 配额公式钉在编译期:观众最多占有效连接上限的四分之一,连接上限再小也坐得下一个满员房间。
static_assert(BattleRoomManager::ObserverSeatQuotaFor(4096) == 1024);
static_assert(BattleRoomManager::ObserverSeatQuotaFor(0) == BattleRoomManager::kMaxObserversPerRoom);
static_assert(BattleRoomManager::ObserverSeatQuotaFor(4 * BattleRoomManager::kMaxObserversPerRoom + 3) ==
              BattleRoomManager::kMaxObserversPerRoom);

// 生产的注入路径:借用直连面时按 battle_max_connections 算出节点配额(装配时一次,之后准入不读配置)。
// 84 条连接 → 21 席:一个满员房间之外只剩一席,另一房间第二名观众即被拒。
TEST_F(BattleObserverContract, ClientEdgeBindingDerivesObserverQuotaFromConnectionCap) {
    constexpr uint32_t kMaxConnections = 84;
    static_assert(BattleRoomManager::ObserverSeatQuotaFor(kMaxConnections) == BattleRoomManager::kMaxObserversPerRoom + 1);
    gNodeConfigManager.GetBaseDeployConfig().set_battle_max_connections(kMaxConnections);
    const auto edge = std::make_shared<BattleClientEdge>();
    const ClientEdgeBinding binding(*edge);
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto otherBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(otherBattle, ::SPECTATE_VISIBILITY_PUBLIC));
    std::vector<AddObserverRequest> seated;
    ASSERT_NO_FATAL_FAILURE(FillRoom(seated));

    const auto lastSeat = OriginalIn(otherBattle, kFillerObserverBase + kRoomObserverCap, FillerOperation(kRoomObserverCap));
    ASSERT_EQ(0u, AddError(lastSeat));
    const auto overQuota =
        OriginalIn(otherBattle, kFillerObserverBase + kRoomObserverCap + 1, FillerOperation(kRoomObserverCap + 1));
    EXPECT_EQ(static_cast<uint32_t>(::kMatchSpectateFull), AddError(overQuota));
    EXPECT_EQ(OBSERVER_ADMISSION_ABSENT, Query(overQuota));
}

// 观众帧编码一次(battle-spectate-tiered-fanout.md §10 第 3 条):同一帧只编码一份,逐条直连写出。共享的那份字节
// 不能被第一条连接的发送清空(对共享 Buffer 调 send(Buffer*) 会 retrieveAll),所以每名有直连的观众都必须收到
// 与单发分帧逐字节相同的完整帧;夹在中间、没有直连的观众只是被跳过,不打断后面的观众。
// 作废房间推 SpectateEnd(ABORTED) 是不依赖引擎出招的确定路径;回合结果的观众帧走同一个出口(PushSpectatorFrame)。
TEST_F(BattleObserverContract, SpectatorFrameEncodedOnceReachesEveryDirectObserverIntact) {
    // 回环连接先于建房打开:建房之后到销毁之前不驱动 loop,房间的回合 / 确认定时器没有机会触发,
    // 两条直连上只会出现这一帧
    LoopbackDirectConnection firstDirect(*loop_);
    LoopbackDirectConnection lastDirect(*loop_);
    ASSERT_TRUE(firstDirect.Open("spectator_first"));
    ASSERT_TRUE(lastDirect.Open("spectator_last"));
    const auto edge = std::make_shared<BattleClientEdge>();
    const ClientEdgeBinding binding(*edge);
    ASSERT_NO_FATAL_FAILURE(Create());

    // 名单按观众号有序遍历:没有直连的那名排在两名有直连的观众中间
    const auto first = OriginalFor(kFillerObserverBase, FillerOperation(0));
    const auto withoutDirect = OriginalFor(kFillerObserverBase + 1, FillerOperation(1));
    const auto last = OriginalFor(kFillerObserverBase + 2, FillerOperation(2));
    ASSERT_EQ(0u, AddError(first));
    ASSERT_EQ(0u, AddError(withoutDirect));
    ASSERT_EQ(0u, AddError(last));
    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, first.observer_player_id(), ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 firstDirect.ServerSide(), nullptr, first.spectator_operation_id()));
    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, last.observer_player_id(), ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 lastDirect.ServerSide(), nullptr, last.spectator_operation_id()));

    Destroy(battleId_);

    // 期望字节按单发 Send 的分帧(ProtobufCodec::fillEmptyBuffer)独立算一遍,不经被测的编码出口
    ::SpectateEndS2C end;
    end.set_battle_id(battleId_);
    end.set_outcome(::BATTLE_OUTCOME_ONGOING);
    end.set_reason(::SPECTATE_END_BATTLE_ABORTED);
    ::MessageContent content;
    content.set_message_id(BattleClientPlayerNotifySpectateEndMessageId);
    content.set_serialized_message(end.SerializeAsString());
    muduo::net::Buffer framed;
    ProtobufCodec::fillEmptyBuffer(&framed, content);
    const std::string expected = framed.retrieveAllAsString();

    ASSERT_TRUE(DriveLoopUntil(*loop_, [&] {
        return firstDirect.ClientReceived().size() >= expected.size()
            && lastDirect.ClientReceived().size() >= expected.size();
    }));
    EXPECT_EQ(expected, firstDirect.ClientReceived());
    EXPECT_EQ(expected, lastDirect.ClientReceived());
}

// 查询能力来自本节点此刻可签名状态；空/短/仅空白密钥均不能宣告v1。
TEST_F(BattleObserverContract, OutcomeCapabilityRequiresTrimmedSigningKey) {
    const std::vector<std::pair<const char*, uint32_t>> cases{
        {nullptr, 0}, {"", 0}, {" \t\r\n", 0},
        {" 1234567890123456789012345678901 ", 0},
        {" \t12345678901234567890123456789012\r\n", 1}};
    for (const auto& [secret, expected] : cases) {
        ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", secret);
        const auto state = CreationState();
        EXPECT_EQ(BATTLE_CREATION_ABSENT, state.state());
        EXPECT_EQ(kInstance, state.target_instance_id());
        EXPECT_EQ(expected, state.supported_outcome_protocol_version());
        EXPECT_EQ(0u, state.adopted_outcome_protocol_version());
    }
}

// 真正Create入口拒绝缺密钥的新v1操作，且不遗留房间或采用回执；合法密钥才准创建。
TEST_F(BattleObserverContract, NewOutcomeV1CreationFailsClosedUntilSigningKeyReady) {
    ASSERT_NO_FATAL_FAILURE(AssertInFreshOutcomeProcess([&] {
        const auto original = OriginalCreation(1);
        for (const char* secret : {"", "1234567890123456789012345678901", " \t\r\n"}) {
            ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", secret);
            CreateBattleResponse refused;
            manager().HandleCreateBattle(original, refused);
            EXPECT_EQ(static_cast<uint32_t>(::kFeatureUnavailable), refused.error_message().id());
            EXPECT_EQ(0u, refused.adopted_outcome_protocol_version());
            const auto state = CreationState();
            EXPECT_EQ(BATTLE_CREATION_ABSENT, state.state());
            EXPECT_EQ(0u, state.supported_outcome_protocol_version());
            EXPECT_EQ(0u, state.adopted_outcome_protocol_version());
        }
        ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "12345678901234567890123456789012");
        CreateBattleResponse created;
        manager().HandleCreateBattle(original, created);
        EXPECT_EQ(0u, created.error_message().id());
        EXPECT_EQ(1u, created.adopted_outcome_protocol_version());
        EXPECT_EQ(kInstance, created.target_instance_id());
        EXPECT_EQ(BATTLE_CREATION_PRESENT, CreationState().state());
    }));
}

// 已采用的原操作不随运行时缺密钥降为legacy；同原件幂等仍回原adopted，新操作继续拒绝。
TEST_F(BattleObserverContract, ExistingOutcomeV1AdoptionSurvivesSigningKeyLossWithoutFallback) {
    ASSERT_NO_FATAL_FAILURE(AssertInFreshOutcomeProcess([&] {
        const auto original = OriginalCreation(1);
        {
            ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "12345678901234567890123456789012");
            CreateBattleResponse created;
            manager().HandleCreateBattle(original, created);
            EXPECT_EQ(0u, created.error_message().id());
            EXPECT_EQ(1u, created.adopted_outcome_protocol_version());
        }
        ObserverFixtureEnvironment missing("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "");
        const auto state = CreationState();
        EXPECT_EQ(BATTLE_CREATION_PRESENT, state.state());
        EXPECT_EQ(0u, state.supported_outcome_protocol_version());
        EXPECT_EQ(1u, state.adopted_outcome_protocol_version());
        CreateBattleResponse replayed;
        manager().HandleCreateBattle(original, replayed);
        EXPECT_EQ(0u, replayed.error_message().id());
        EXPECT_EQ(1u, replayed.adopted_outcome_protocol_version());
        EXPECT_EQ(kInstance, replayed.target_instance_id());
        auto changed = original;
        changed.set_seed(original.seed() + 1);
        CreateBattleResponse invalid;
        manager().HandleCreateBattle(changed, invalid);
        EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), invalid.error_message().id());
        auto next = original;
        next.set_battle_id(original.battle_id() + 1);
        CreateBattleResponse refused;
        manager().HandleCreateBattle(next, refused);
        EXPECT_EQ(static_cast<uint32_t>(::kFeatureUnavailable), refused.error_message().id());
        EXPECT_EQ(0u, refused.adopted_outcome_protocol_version());
        EXPECT_EQ(1u, CreationState().adopted_outcome_protocol_version());
    }));
}

// ---- 观战中继(L1)/ 全服广播(L2)(battle-spectate-tiered-fanout.md §15.4)----

// 校验顺序是契约:实例 → 房间 → 公开。实例不对时即使房间不存在也回 kInvalidParameter,不能触发 match 懒剔除观战索引。
// 任何一项不过都不挂 gate、不发帧,回包也不带实例与期限。
TEST_F(BattleObserverContract, AttachRelayGateRejectionsFollowContractOrderWithoutSideEffects) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto privateBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    const auto unsetBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    const auto missingBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(privateBattle, ::SPECTATE_VISIBILITY_PRIVATE));
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(unsetBattle, ::SPECTATE_VISIBILITY_UNSPECIFIED));

    const auto expectRejected = [](const AttachRelayGateResponse& reply, const uint32_t tip) {
        EXPECT_EQ(tip, reply.error_message().id());
        EXPECT_TRUE(reply.battle_instance_id().empty());
        EXPECT_EQ(0u, reply.deadline_ms());
        EXPECT_FALSE(reply.broadcast());
    };
    const auto invalid = static_cast<uint32_t>(::kInvalidParameter);
    expectRejected(Attach(battleId_, kRelayGate, kGateInstance, "another-battle-instance"), invalid);
    expectRejected(Attach(missingBattle, kRelayGate, kGateInstance, "another-battle-instance"), invalid);
    expectRejected(Attach(missingBattle, kRelayGate, kGateInstance), static_cast<uint32_t>(::kEntityIsNull));
    expectRejected(Attach(privateBattle, kRelayGate, kGateInstance), invalid);
    expectRejected(Attach(unsetBattle, kRelayGate, kGateInstance), invalid);
    // 空目标会关掉 gate 侧的目标过滤(AGENTS.md §7.2)
    expectRejected(Attach(battleId_, 0, kGateInstance), invalid);
    expectRejected(Attach(battleId_, kRelayGate, ""), invalid);
    EXPECT_TRUE(TakeFrames().empty());

    // 被拒的 gate 没进中继表:结算一回合没有任何帧
    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    EXPECT_TRUE(TakeFrames().empty());
}

// 校验口径同 Attach;被拒不升档:之后 Attach 仍然能挂上,不回 broadcast。
TEST_F(BattleObserverContract, PromoteRejectionsFollowContractOrderAndDoNotPromote) {
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto privateBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    const auto unsetBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    const auto missingBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(privateBattle, ::SPECTATE_VISIBILITY_PRIVATE));
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(unsetBattle, ::SPECTATE_VISIBILITY_UNSPECIFIED));

    const auto expectRejected = [](const PromoteSpectateBroadcastResponse& reply, const uint32_t tip) {
        EXPECT_EQ(tip, reply.error_message().id());
        EXPECT_TRUE(reply.battle_instance_id().empty());
        EXPECT_EQ(0u, reply.deadline_ms());
    };
    const auto invalid = static_cast<uint32_t>(::kInvalidParameter);
    expectRejected(Promote(battleId_, "another-battle-instance"), invalid);
    expectRejected(Promote(missingBattle, "another-battle-instance"), invalid);
    expectRejected(Promote(missingBattle), static_cast<uint32_t>(::kEntityIsNull));
    expectRejected(Promote(privateBattle), invalid);
    expectRejected(Promote(unsetBattle), invalid);
    EXPECT_TRUE(TakeFrames().empty());

    const auto attached = Attach(battleId_, kRelayGate, kGateInstance);
    EXPECT_EQ(0u, attached.error_message().id());
    EXPECT_FALSE(attached.broadcast());
}

// 新挂接只给这台 gate 补发一帧 STATE(161,observer_count = 0);同实例重复挂接只回包;同 node_id 新实例覆盖旧实例,
// 只给新实例发一帧。此后每回合的帧只到最新实例。
TEST_F(BattleObserverContract, AttachRelayGateIsIdempotentAndNewInstanceReplacesOldWithOneStateFrame) {
    ASSERT_NO_FATAL_FAILURE(Create());

    const auto first = Attach(battleId_, kRelayGate, kGateInstance);
    ASSERT_EQ(0u, first.error_message().id());
    EXPECT_EQ(kInstance, first.battle_instance_id());
    EXPECT_EQ(originalNowMs_ + 180000, first.deadline_ms());
    EXPECT_FALSE(first.broadcast());
    auto frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectRelayRecord(frames[0], battleId_, kRelayGate, kGateInstance);
    const auto stateFrame = ParseFrame(frames[0]);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_STATE, stateFrame.kind());
    EXPECT_EQ(originalNowMs_ + 180000, stateFrame.deadline_ms());
    const auto state = ParseClientFrame<SpectateStateS2C>(stateFrame, BattleClientPlayerNotifySpectateStateMessageId);
    EXPECT_EQ(0u, state.observer_count());
    EXPECT_EQ(battleId_, state.state().battle_id());
    EXPECT_EQ(state.state().round_index(), stateFrame.round_index());
    EXPECT_EQ(2, state.state().actors_size());
    EXPECT_NE(0u, state.state().action_deadline_ms());

    const auto repeated = Attach(battleId_, kRelayGate, kGateInstance);
    EXPECT_EQ(0u, repeated.error_message().id());
    EXPECT_EQ(kInstance, repeated.battle_instance_id());
    EXPECT_EQ(first.deadline_ms(), repeated.deadline_ms());
    EXPECT_TRUE(TakeFrames().empty());

    ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kOtherGateInstance).error_message().id());
    frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectRelayRecord(frames[0], battleId_, kRelayGate, kOtherGateInstance);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_STATE, ParseFrame(frames[0]).kind());

    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectRelayRecord(frames[0], battleId_, kRelayGate, kOtherGateInstance);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_TURN, ParseFrame(frames[0]).kind());
}

// 每回合的中继条数 = 挂接 gate 数;topic 名与分区走部署配置(代号、命令 topic 分区数);事件字段齐全,
// client_frame 能还原成 158 的 MessageContent,里面是观众视角的回合结果。
TEST_F(BattleObserverContract, EveryRoundSendsOneRelayFramePerAttachedGateOverConfiguredRoute) {
    auto& kafka = *tlsNodeConfigManager.GetBaseDeployConfig().mutable_kafka();
    kafka.set_command_topic_generation(3);
    kafka.set_command_topic_partitions(8);
    // 带上装备战斗属性:观众帧必须把它裁掉(RedactStateForViewer)
    auto creation = OriginalCreation();
    for (auto& player : *creation.mutable_players()) player.mutable_combat()->Clear();
    CreateBattleResponse created;
    manager().HandleCreateBattle(creation, created);
    ASSERT_EQ(0u, created.error_message().id());

    std::map<uint32_t, std::string> gates{{9, "gate-nine"}, {16, "gate-sixteen"}};
    for (const auto& [gateNodeId, gateInstanceId] : gates) {
        ASSERT_EQ(0u, Attach(battleId_, gateNodeId, gateInstanceId).error_message().id());
    }
    const auto attachFrames = TakeFrames();
    ASSERT_EQ(gates.size(), attachFrames.size());
    const uint32_t firstRound = ParseFrame(attachFrames[0]).round_index();

    for (uint32_t round = 0; round < 2; ++round) {
        SCOPED_TRACE(round);
        ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
        const auto frames = TakeFrames();
        ASSERT_EQ(gates.size(), frames.size());
        size_t index = 0;
        for (const auto& [gateNodeId, gateInstanceId] : gates) {
            const auto& record = frames[index++];
            EXPECT_EQ("gate-spectate_g3", record.topic);
            EXPECT_EQ(static_cast<int32_t>(gateNodeId % 8), record.partition);
            EXPECT_EQ(std::to_string(battleId_), record.key);
            const auto frame = ParseFrame(record);
            EXPECT_EQ(battleId_, frame.battle_id());
            EXPECT_EQ(kInstance, frame.battle_instance_id());
            EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_TURN, frame.kind());
            EXPECT_EQ(firstRound + round, frame.round_index());
            EXPECT_EQ(originalNowMs_ + 180000, frame.deadline_ms());
            EXPECT_EQ(gateNodeId, frame.target_gate_id());
            EXPECT_EQ(gateInstanceId, frame.target_instance_id());
            const auto turn = ParseClientFrame<TurnResultS2C>(frame, BattleClientPlayerNotifySpectateTurnResultMessageId);
            EXPECT_EQ(battleId_, turn.battle_id());
            EXPECT_EQ(frame.round_index(), turn.round_index());
            EXPECT_EQ(0, turn.state().self_items_size());
            EXPECT_EQ(2, turn.state().actors_size());
            for (const auto& actor : turn.state().actors()) {
                EXPECT_FALSE(actor.has_combat());
                EXPECT_EQ(0, actor.skill_cooldown_rounds_size());
            }
        }
        // 下一回合多挂一台,条数跟着变
        gates.emplace(23, "gate-twenty-three");
        ASSERT_EQ(0u, Attach(battleId_, 23, "gate-twenty-three").error_message().id());
        (void)TakeFrames();
    }
}

// 升档单向幂等:首次往广播 topic 发一帧 STATE,此后每回合与终局帧都只发广播 1 条、不再发中继;之后的 Attach 回 broadcast = true,
// 不挂 gate。
TEST_F(BattleObserverContract, PromoteIsIdempotentAndLaterRoundsOnlyBroadcast) {
    ASSERT_NO_FATAL_FAILURE(Create());
    ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kGateInstance).error_message().id());
    ASSERT_EQ(0u, Attach(battleId_, kOtherRelayGate, kOtherGateInstance).error_message().id());
    (void)TakeFrames();

    const auto promoted = Promote(battleId_);
    ASSERT_EQ(0u, promoted.error_message().id());
    EXPECT_EQ(kInstance, promoted.battle_instance_id());
    EXPECT_EQ(originalNowMs_ + 180000, promoted.deadline_ms());
    auto frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectBroadcastRecord(frames[0], battleId_);
    const auto stateFrame = ParseFrame(frames[0]);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_STATE, stateFrame.kind());
    EXPECT_EQ(0u, ParseClientFrame<SpectateStateS2C>(stateFrame, BattleClientPlayerNotifySpectateStateMessageId)
                      .observer_count());

    const auto repeated = Promote(battleId_);
    EXPECT_EQ(0u, repeated.error_message().id());
    EXPECT_EQ(kInstance, repeated.battle_instance_id());
    EXPECT_TRUE(TakeFrames().empty());

    for (int round = 0; round < 2; ++round) {
        ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
        frames = TakeFrames();
        ASSERT_EQ(1u, frames.size());
        ExpectBroadcastRecord(frames[0], battleId_);
        const auto turnFrame = ParseFrame(frames[0]);
        EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_TURN, turnFrame.kind());
        (void)ParseClientFrame<TurnResultS2C>(turnFrame, BattleClientPlayerNotifySpectateTurnResultMessageId);
    }

    const auto late = Attach(battleId_, 77, "late-gate-instance");
    EXPECT_EQ(0u, late.error_message().id());
    EXPECT_TRUE(late.broadcast());
    EXPECT_EQ(kInstance, late.battle_instance_id());
    EXPECT_EQ(originalNowMs_ + 180000, late.deadline_ms());
    EXPECT_TRUE(TakeFrames().empty());

    // END 也只发广播,升档前挂的两台 gate 不再单发:与最后一回合的 TURN 同分区保序。中继 END 可能越过广播上的 TURN,
    // gate 收尾后把那一帧丢掉,观众就看不到最后一回合(§15.4)。
    Destroy(battleId_);
    frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectBroadcastRecord(frames[0], battleId_);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_END, ParseFrame(frames[0]).kind());
}

// 结束 / 作废:没升档时每台中继 gate 各一帧 END,升过档只发广播一帧(编好的 166,reason 与 L0 同值),不要求房间有直连观众;
// 房间没了就没有第二帧;从没开过通道的房间不发。
TEST_F(BattleObserverContract, EndFrameReachesEveryChannelOnceWithDirectObserverReason) {
    ASSERT_NO_FATAL_FAILURE(Create());
    ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kGateInstance).error_message().id());
    ASSERT_EQ(0u, Attach(battleId_, kOtherRelayGate, kOtherGateInstance).error_message().id());
    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    const auto turnFrames = TakeFrames();
    ASSERT_EQ(4u, turnFrames.size()); // 两帧 STATE + 两帧 TURN
    const auto lastTurnRound = ParseFrame(turnFrames.back()).round_index();

    Destroy(battleId_);
    auto frames = TakeFrames();
    ASSERT_EQ(2u, frames.size());
    ExpectRelayRecord(frames[0], battleId_, kRelayGate, kGateInstance);
    ExpectRelayRecord(frames[1], battleId_, kOtherRelayGate, kOtherGateInstance);
    for (const auto& record : frames) {
        const auto frame = ParseFrame(record);
        EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_END, frame.kind());
        EXPECT_GE(frame.round_index(), lastTurnRound);
        const auto end = ParseClientFrame<SpectateEndS2C>(frame, BattleClientPlayerNotifySpectateEndMessageId);
        EXPECT_EQ(battleId_, end.battle_id());
        EXPECT_EQ(::SPECTATE_END_BATTLE_ABORTED, end.reason());
        EXPECT_EQ(::BATTLE_OUTCOME_ONGOING, end.outcome());
    }
    Destroy(battleId_);
    EXPECT_TRUE(TakeFrames().empty());

    const auto promotedBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(promotedBattle, ::SPECTATE_VISIBILITY_PUBLIC));
    ASSERT_EQ(0u, Promote(promotedBattle).error_message().id());
    (void)TakeFrames();
    Destroy(promotedBattle);
    frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ExpectBroadcastRecord(frames[0], promotedBattle);
    EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_END, ParseFrame(frames[0]).kind());

    const auto quietBattle = nextBattleId.fetch_add(1, std::memory_order_relaxed);
    ASSERT_NO_FATAL_FAILURE(CreateExtraRoom(quietBattle, ::SPECTATE_VISIBILITY_PUBLIC));
    Destroy(quietBattle);
    EXPECT_TRUE(TakeFrames().empty());
}

// L0 直连与中继共用同一份观众帧:中继帧里的客户端信封加上直连分帧,必须与直连观众收到的字节逐字节相同。
TEST_F(BattleObserverContract, DirectObserverAndRelayGateShareTheSameSpectatorEnvelope) {
    // 回环连接先于建房打开,理由同 SpectatorFrameEncodedOnceReachesEveryDirectObserverIntact
    LoopbackDirectConnection direct(*loop_);
    ASSERT_TRUE(direct.Open("spectator_shared"));
    const auto edge = std::make_shared<BattleClientEdge>();
    const ClientEdgeBinding binding(*edge);
    ASSERT_NO_FATAL_FAILURE(Create());
    const auto observer = OriginalFor(kFillerObserverBase, FillerOperation(0));
    ASSERT_EQ(0u, AddError(observer));
    ASSERT_TRUE(manager().AttachDirectConnection(battleId_, observer.observer_player_id(), ::BATTLE_TICKET_ROLE_OBSERVER,
                                                 direct.ServerSide(), nullptr, observer.spectator_operation_id()));
    ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kGateInstance).error_message().id());
    (void)TakeFrames();

    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    const auto frames = TakeFrames();
    ASSERT_EQ(1u, frames.size());
    ::MessageContent envelope;
    ASSERT_TRUE(envelope.ParseFromString(ParseFrame(frames[0]).client_frame()));
    EXPECT_EQ(BattleClientPlayerNotifySpectateTurnResultMessageId, envelope.message_id());
    muduo::net::Buffer framed;
    ProtobufCodec::fillEmptyBuffer(&framed, envelope);
    const std::string expected = framed.retrieveAllAsString();

    ASSERT_TRUE(DriveLoopUntil(*loop_, [&] { return direct.ClientReceived().size() >= expected.size(); }));
    EXPECT_EQ(expected, direct.ClientReceived());
    Destroy(battleId_);
}

// 帧没发出去只计数:挂接照常成功,回合照常推进,不重试;下一帧带全量状态,自愈。
TEST_F(BattleObserverContract, RefusedSpectateFramesNeitherFailAttachNorBlockRounds) {
    ASSERT_NO_FATAL_FAILURE(Create());
    spectateSinkAccepts_ = false;
    const auto attached = Attach(battleId_, kRelayGate, kGateInstance);
    EXPECT_EQ(0u, attached.error_message().id());
    EXPECT_EQ(kInstance, attached.battle_instance_id());
    EXPECT_EQ(1u, TakeFrames().size());

    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    const auto refused = TakeFrames();
    ASSERT_EQ(1u, refused.size());

    spectateSinkAccepts_ = true;
    ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
    const auto healed = TakeFrames();
    ASSERT_EQ(1u, healed.size());
    EXPECT_EQ(ParseFrame(refused[0]).round_index() + 1, ParseFrame(healed[0]).round_index());
}

// 停机口径与 L0 相同(§5.3):结果未落定的房间不发 END(不能发一个可能与真实结果矛盾的 ABORTED),中继观众靠 gate 的
// 空闲超时收尾;没有结果要等的房间照常发 END(ABORTED)。v1 房间按生产合同删不掉,放在独立子进程里。
TEST_F(BattleObserverContract, ShutdownSendsNoEndFrameForUndecidedRoomButEndsAbortableRoom) {
    ASSERT_NO_FATAL_FAILURE(AssertInFreshOutcomeProcess([&] {
        ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "12345678901234567890123456789012");
        CreateBattleResponse undecided;
        manager().HandleCreateBattle(OriginalCreation(1), undecided);
        ASSERT_EQ(0u, undecided.error_message().id());
        auto abortable = OriginalCreation();
        abortable.set_battle_id(battleId_ + 1);
        CreateBattleResponse created;
        manager().HandleCreateBattle(abortable, created);
        ASSERT_EQ(0u, created.error_message().id());
        ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kGateInstance).error_message().id());
        ASSERT_EQ(0u, Attach(abortable.battle_id(), kRelayGate, kGateInstance).error_message().id());
        (void)TakeFrames();

        manager().AbortAllRooms("unit_shutdown");
        const auto frames = TakeFrames();
        ASSERT_EQ(1u, frames.size());
        ExpectRelayRecord(frames[0], abortable.battle_id(), kRelayGate, kGateInstance);
        const auto frame = ParseFrame(frames[0]);
        EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_END, frame.kind());
        const auto end = ParseClientFrame<SpectateEndS2C>(frame, BattleClientPlayerNotifySpectateEndMessageId);
        EXPECT_EQ(::SPECTATE_END_BATTLE_ABORTED, end.reason());
    }));
}

// 等结果回执期间,终局已算出但还没拿到决定:最后一回合的 TURN 不提前发,新挂的 gate 收到的 STATE 是冻结的可见状态,
// 不提前泄露结果(与 L0 首帧同口径)。1 点气血一回合分出胜负;测试里没有 match 可连,房间一直等着,所以放在独立子进程里。
TEST_F(BattleObserverContract, RelayStateFrameDuringOutcomeWaitDoesNotLeakTerminalResult) {
    ASSERT_NO_FATAL_FAILURE(AssertInFreshOutcomeProcess([&] {
        ObserverFixtureEnvironment environment("MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "12345678901234567890123456789012");
        auto creation = OriginalCreation(1);
        for (auto& player : *creation.mutable_players()) {
            player.set_max_health(1);
            player.mutable_base_attributes()->set_health(1);
        }
        CreateBattleResponse created;
        manager().HandleCreateBattle(creation, created);
        ASSERT_EQ(0u, created.error_message().id());
        ASSERT_EQ(0u, Attach(battleId_, kRelayGate, kGateInstance).error_message().id());
        (void)TakeFrames();

        ASSERT_NO_FATAL_FAILURE(DriveOneRound(battleId_));
        EXPECT_TRUE(TakeFrames().empty());

        ASSERT_EQ(0u, Attach(battleId_, kOtherRelayGate, kOtherGateInstance).error_message().id());
        const auto frames = TakeFrames();
        ASSERT_EQ(1u, frames.size());
        ExpectRelayRecord(frames[0], battleId_, kOtherRelayGate, kOtherGateInstance);
        const auto frame = ParseFrame(frames[0]);
        EXPECT_EQ(contracts::kafka::SPECTATE_FRAME_STATE, frame.kind());
        const auto state = ParseClientFrame<SpectateStateS2C>(frame, BattleClientPlayerNotifySpectateStateMessageId);
        EXPECT_EQ(::BATTLE_OUTCOME_ONGOING, state.state().outcome());
        EXPECT_EQ(2, state.state().actors_size());
        for (const auto& actor : state.state().actors()) EXPECT_FALSE(actor.is_dead());
    }));
}

// 「0 = 硬上限」的规则只在直连面:节点观众席配额经 SetClientEdge 取同一个值。
TEST_F(BattleObserverContract, ClientEdgeEffectiveMaxConnectionsFallsBackToHardCap) {
    const auto edge = std::make_shared<BattleClientEdge>();
    gNodeConfigManager.GetBaseDeployConfig().set_battle_max_connections(0);
    EXPECT_EQ(BattleClientEdge::kHardMaxConnections, edge->EffectiveMaxConnections());
    gNodeConfigManager.GetBaseDeployConfig().set_battle_max_connections(84);
    EXPECT_EQ(84u, edge->EffectiveMaxConnections());
}
} // namespace
