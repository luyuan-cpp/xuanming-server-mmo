// 真实Manager接口回归：仅basic Node本机身份和EventLoop，不启动服务/注册/Redis/Kafka。
// 通过既有MutableNodeInfo临时撤下可达地址，确定性复现幂等补签失败；不加生产故障hook。
// 断线重连宽限用例另在夹具的 loop_ 上起一对回环 TcpServer / TcpClient,只为拿真实的服务端侧连接
// (见 LoopbackDirectConnection);仍不启动 BattleClientEdge,直连断开由用例直接调 DetachDirectConnection 模拟。
#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include "logic/battle_room_manager.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "node/system/node/node.h"
#include "node_config_manager.h"
#include "proto/common/base/node.pb.h"
#include "proto/match/match_service.pb.h"
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
constexpr char kInstance[] = "a1094888-4208-482e-b187-a28a98ca2b54";
constexpr char kFirstOperation[] = "aa094888-4208-482e-b187-a28a98ca2b54";
constexpr char kSecondOperation[] = "bb094888-4208-482e-b187-a28a98ca2b54";
constexpr char kPlacement[] = "cc094888-4208-482e-b187-a28a98ca2b54";
std::atomic<uint64_t> nextBattleId{590000};

// 第 index 个填充观众的观战操作号:合法 UUID 形状(小写十六进制、非全零),彼此不同,
// 也不与上面几个固定操作号相撞。
std::string FillerOperation(uint32_t index) {
    char buffer[40];
    std::snprintf(buffer, sizeof(buffer), "%08x-4208-482e-b187-a28a98ca2b54", 0x10000000u + index);
    return buffer;
}

// ---- 回环直连(断线重连宽限用例)----
// 夹具的 loop_ 就在测试线程上、平时不跑。Attach / Detach 只要一条真实的服务端侧连接,
// 这里在 loop_ 上起一对回环 TcpServer / TcpClient,只在建连与拆除时限时驱动 loop()。

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
        client_->connect();
        return DriveLoopUntil(loop_, [probe = probe_] { return probe->serverUp && probe->clientUp; });
    }

    muduo::net::TcpConnectionPtr ServerSide() const { return probe_->serverSide.lock(); }

private:
    struct Probe {
        std::weak_ptr<muduo::net::TcpConnection> serverSide;
        bool serverUp = false;
        bool serverDown = false;
        bool clientUp = false;
        bool clientDown = false;
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
    }

    void TearDown() override {
        if (node_) {
            DestroyBattleRequest request;
            request.set_battle_id(battleId_);
            request.set_placement_operation_id(kPlacement);
            request.set_target_instance_id(kInstance);
            manager().HandleDestroyBattle(request);
            manager().SetClientEdge(nullptr);
            node_.reset(); // 既有Node两跳排空完成后才销毁EventLoop。
        }
        loop_.reset();
        gNodeConfigManager = savedGlobal_;
        tlsNodeConfigManager = savedThread_;
    }

    static BattleRoomManager& manager() { return BattleRoomManager::Instance(); }

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

    // 拒绝即终态:同 op 重放不会重新判定成准入,Remove 拿得到 cancelled 证明
    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), AddError(overflow));
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

    EXPECT_EQ(static_cast<uint32_t>(::kInvalidParameter), AddError(waiting));
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
} // namespace
