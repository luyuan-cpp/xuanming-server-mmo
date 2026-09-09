#pragma once

#include <cstdint>
#include <functional>
#include <string>

#include "engine/core/type_define/type_define.h"
#include "core/time/comp/timer_task_comp.h"

// ─────────────────────────────────────────────────────────────────────────
// 永久 guid 号段客户端(Leaf-segment;docs/design/node-id-overhaul-plan-20260908.md §6 / §7.5)。
// go/shared/idsegment/client.go 的 C++ 同构:同一套双 buffer / 单飞预取 / 范围校验 /
// 退避语义,只是把"阻塞等续段"换成事件循环下的"立刻返回 false"。
//
// # 一种 GUID 一个实例(§7.5 第 3 条)
//
// 这个类不认识"item"或"txlog":它只是一个按 biz_tag 领段、按段发号的通用客户端。
// 每种永久 guid(item / txlog / snapshot,以后 pet / guild …)在 GuidSegmentRegistry 里
// 各持一个实例:各自的当前段 / 下一段、预取触发、退避、统计标签互不相干;它们只共享
// 一条到 data_service 的 gRPC 通道(由 scene 的 id_segment_bootstrap 负责,响应按请求编号分发)。
//
// # 为什么不用 snowflake
//
// 永久身份只需要「全局唯一 + 不重发」,不需要把铸号时间和铸号节点编进 id 里。snowflake 的
// 唯一性建立在 worker 槽租约 + 时钟单调之上,10 万台 scene 每秒 10 万次水位写 + 续约超出单个
// etcd 一个数量级;号段没有 lease、没有时钟、没有 worker:持有者经 DataService.AllocateIdSegment
// 用一次 CAS 从 id_segment 表领走 [lo, hi),协调器只在续段时碰一次,对节点数不敏感。
//
// # 双 buffer(§6.5 弱依赖)
//
// 同时持有当前段和一个预取的下一段。当前段剩余 ≤ prefetchAt·段长 时后台预取(单飞:
// 同一实例同一时刻最多一个在途请求);两段都空时 TryNext 返回 false 并触发续段 —— 事件循环
// **绝不阻塞**。于是 data_service 不可用时每个 scene 还能发完手里两段;库恢复后自动续。
// 启动时首段是硬依赖(scene DependencyGate "id segments ready"),运行期是弱依赖。
//
// # 动态 step(§7.5 第 4 条,Leaf 口径)
//
// 记录每一段从"成为当前段"到"发完"撑了多久:不到 stepGrowBelowSec(15 分钟)就用完 →
// 下次领段 step 翻倍(封顶 maxStep);超过 stepShrinkAboveSec(30 分钟)才用完 → 减半
// (下限 minStep)。目的只是把每天重启一次浪费的尾巴(≤ 2×step)压到最小,不是正确性需求。
//
// # 不持久化游标(§7.5 第 6 条)
//
// 一段在 AllocateIdSegment 提交时就已被数据库判定为用掉,进程崩溃只产生空洞不产生重号;
// 优雅关服也不把尾巴还回去(会破坏"每段必比上一段大"这条让客户端能整段拒收服务端错误的不变量)。
//
// # 传输不可靠的处理
//
// 生成的 gRPC 客户端在 status 非 OK 时只打日志、不回调 handler,所以每次发送都配一个
// fetchTimeout 定时器:到期即当失败,退避(500ms → ×2 → 5s 封顶)重试。晚到的响应
// 只要范围校验通过仍然收下(没浪费),放不下(两段都满)才丢弃。
//
// # 服务端范围校验(fail-closed)
//
// lo ≥ 1、hi > lo、hi ≤ maxIdExclusive(2^55)、lo ≥ 上一段 hi(同一客户端拿到的范围
// 必须单调递增)。任一不满足即 REJECTED:这段**绝不使用**并大声记日志 —— 服务端把号
// 发重了比发不出号严重得多(撞号会静默串档,不报错)。
//
// # 与存量 snowflake 号不撞
//
// 号段从 1 起、上限 2^55;存量 item guid / tx_id / snapshot_id(epoch 2026-03,秒<<32)≥ 6.7e16,
// 两个区间永不相交,新旧号在同一张表 / 同一个玩家 blob 里共存、不迁数据。
//
// # 线程模型
//
// 单线程事件循环语义(thread_local 注册表持有实例)。所有入口(TryNext / OnResponse /
// 定时器回调)都在同一线程串行执行,内部不加锁;回调里可以安全地再进入本对象。
//
// # 可测性
//
// 传输(SendFn)、定时器(ScheduleFn)、时钟(ClockFn)都是注入的:生产侧接 DataService gRPC、
// TimerTaskComp 与 steady_clock;单测用假传输记录请求、手动投递响应与触发定时器、手动拨时钟。
// ─────────────────────────────────────────────────────────────────────────
class GuidSegmentClient
{
public:
    // 值域上界(不含):2^55,与 id_segment 表的 CHECK 一致。只能调小不能调大 ——
    // 调大就与存量 snowflake 号相交。
    static constexpr uint64_t kDefaultMaxIdExclusive = uint64_t{1} << 55;

    struct Options
    {
        std::string kindName;   // 注册表里的种类名,日志 / 指标标签(item / txlog / snapshot),必填
        std::string bizTag;     // id_segment.biz_tag;空 = 与 kindName 相同
        uint32_t initialStep{0}; // 首次领段长度,必填 > 0
        uint32_t minStep{0};    // 动态 step 下限;0 = initialStep(不缩)
        uint32_t maxStep{0};    // 动态 step 上限;0 = initialStep(不扩)
        double prefetchAt{0.1}; // 当前段剩余 ≤ prefetchAt·段长 时预取,(0, 1]
        uint64_t maxIdExclusive{kDefaultMaxIdExclusive};
        double retryBackoffMinSec{0.5}; // 首次失败后的重试间隔
        double retryBackoffMaxSec{5.0}; // 指数退避封顶
        double fetchTimeoutSec{3.0};    // 单次领段无响应视为失败的期限
        double stepGrowBelowSec{15.0 * 60.0};    // 一段撑不到这么久就用完 → step ×2
        double stepShrinkAboveSec{30.0 * 60.0};  // 一段撑过这么久才用完 → step ÷2
    };

    // 发一次领段请求。返回 false = 此刻发不出去(没有已连接的 DataService 节点 / 共享通道忙),
    // 由客户端按退避重试;返回 true 后响应经 OnResponse 送回,或由 fetchTimeout 兜底。
    using SendFn = std::function<bool(const std::string &bizTag, uint32_t step)>;

    enum class TimerKind : uint8_t
    {
        kRetry,
        kFetchTimeout,
    };
    // delaySec 后在本线程回调一次;同类定时器重新安排时旧的可以取消也可以不取消 ——
    // 回调内部靠代数(generation / seq)识别过期开火,所以两种实现都正确。
    using ScheduleFn = std::function<void(TimerKind kind, double delaySec, std::function<void()> fn)>;

    // 单调时钟(秒)。只用来量"一段撑了多久",不参与唯一性。
    using ClockFn = std::function<double()>;

    struct Stats
    {
        uint64_t issued{0};          // 已发出的 id 数
        uint64_t fetches{0};         // 校验通过并落下的段数
        uint64_t fetchErrors{0};     // 失败的领段次数(含 error_code≠0 / 超时 / 发不出 / 范围违规)
        uint64_t rangeViolations{0}; // 被拒绝的服务端范围数;正常恒 0,非 0 = 服务端有 bug
        uint64_t unavailable{0};     // TryNext 返回 false 的次数
        uint64_t sendUnavailable{0}; // SendFn 返回 false 的次数(没有 DataService 节点 / 通道忙)
        uint64_t timeouts{0};        // fetchTimeout 到期次数
        uint64_t droppedRanges{0};   // 两段都满时丢弃的晚到范围数(浪费但无害)
        uint64_t rangesConsumed{0};  // 已经发完的段数(动态 step 的样本数)
        uint64_t stepChanges{0};     // 动态 step 实际改变的次数
        uint64_t currentRemaining{0};
        uint64_t nextRemaining{0};
        bool nextReady{false};
        bool inflight{false};
        bool retryPending{false};
        uint64_t highWater{0}; // 至今校验通过的最大 hi;后续范围的 lo 必须 ≥ 它
        double backoffSec{0};  // 下一次失败后将采用的退避
        uint32_t currentStep{0};      // 下一次领段会请求的 step
        double lastRangeLastedSec{0}; // 最近一段从成为当前段到发完撑了多久
    };

    GuidSegmentClient() = default;
    GuidSegmentClient(const GuidSegmentClient &) = delete;
    GuidSegmentClient &operator=(const GuidSegmentClient &) = delete;

    // 配置并启用。不做任何 IO:第一段在 Warm() / 首次 TryNext 时才去领。
    // schedule 为空时用内置 TimerTaskComp(需要当前线程有 muduo EventLoop);clock 为空时用 steady_clock。
    // 选项自相矛盾(kindName 空、initialStep=0、min>initial>max、prefetchAt 越界、maxIdExclusive 超 2^55、
    // 阈值倒挂)返回 false 且保持未启用。
    bool Enable(Options options, SendFn send, ScheduleFn schedule = nullptr, ClockFn clock = nullptr);

    // 停止续段(取消定时器、不再发请求),手里的号照常发完;关机路径用。幂等。
    void Shutdown();

    // 回到未启用的初始状态(清空段、计数与定时器)。单测隔离用;生产不调。
    void Reset();

    [[nodiscard]] bool IsEnabled() const { return enabled_; }

    // 至少还有一个号可发(当前段或下一段非空)。scene 的 DependencyGate 靠它挡玩家进入。
    [[nodiscard]] bool IsReady() const;

    // 手里两段合计还能发多少号。批量入包用它做整批预检(要 n 个号就得先有 n 个)。
    [[nodiscard]] uint64_t Available() const;

    // 发一个 id。当前段有号立即返回 true;用完则切到预取段;两段都空返回 false
    // (同时触发续段,**不阻塞**)。未启用恒 false。
    bool TryNext(Guid &out);

    // 触发首段领取(幂等:已有号 / 已在途 / 退避中都是空操作)。启动时调一次,
    // 好让"配置错 / data_service 不通"尽早暴露在启动日志里。
    void Warm();

    // AllocateIdSegment 的响应入口(scene 侧的传输层按请求编号转调到对应实例)。
    // errorCode 0 = 成功,[lo, hi) 半开;其余取值来自 data_service 自己的错误码轴。
    void OnResponse(uint32_t errorCode, uint64_t lo, uint64_t hi);

    [[nodiscard]] Stats GetStats() const;
    [[nodiscard]] std::string Describe() const;
    [[nodiscard]] const Options &options() const { return options_; }
    [[nodiscard]] const std::string &KindName() const { return options_.kindName; }

private:
    struct Segment
    {
        uint64_t pos{0}; // 下一个要发出的 id
        uint64_t hi{0};  // 不含
        // 剩余 ≤ 它就预取下一段。按**这一段**的长度算而不是按配置的 step 算:动态 step 下
        // 相邻两段长度可能差一倍,按配置算会把预取时机整体挪前 / 挪后。
        uint64_t prefetchThreshold{1};
        [[nodiscard]] uint64_t Remaining() const { return pos < hi ? hi - pos : 0; }
    };

    [[nodiscard]] Segment MakeSegment(uint64_t lo, uint64_t hi) const;
    void BecomeCurrent(const Segment &segment);
    void OnCurrentExhausted();
    [[nodiscard]] double NowSec() const;
    void MaybePrefetch();
    void StartFetch(const char *why);
    void OnFetchFailed(const char *why, bool alreadyLogged);
    void OnFetchTimeout(uint64_t seq);
    void ScheduleRetry();
    void OnRetryTimer(uint64_t gen);
    void Schedule(TimerKind kind, double delaySec, std::function<void()> fn);
    [[nodiscard]] const char *ValidateRange(uint64_t lo, uint64_t hi) const;

    Options options_;
    SendFn send_;
    ScheduleFn schedule_;
    ClockFn clock_;
    bool enabled_{false};
    bool stopped_{false}; // Shutdown() 之后:不再发请求,只发手里的号

    Segment cur_;
    Segment next_;
    bool hasNext_{false};
    double curStartedAtSec_{0}; // cur_ 成为当前段的时刻(NowSec 口径)

    uint32_t step_{0};           // 下一次领段请求的 step(动态)
    double lastRangeLastedSec_{0};
    uint64_t rangesConsumed_{0};
    uint64_t stepChanges_{0};

    bool inflight_{false};
    uint64_t fetchSeq_{0}; // 每次发送 +1;超时回调带着当时的 seq,只对当次生效
    bool retryPending_{false};
    uint64_t retryGen_{0}; // 每次安排重试 +1;Reset 也 +1,让旧回调作废
    double backoffSec_{0.5};
    uint64_t highWater_{0};
    uint32_t consecutiveFailures_{0};

    uint64_t issued_{0};
    uint64_t fetches_{0};
    uint64_t fetchErrors_{0};
    uint64_t rangeViolations_{0};
    uint64_t unavailable_{0};
    uint64_t sendUnavailable_{0};
    uint64_t timeouts_{0};
    uint64_t droppedRanges_{0};

    // 内置定时器(schedule_ 为空时使用)。两类各一个:RunAfter 重新武装会取消上一枚,
    // 恰好是"同类最多一枚在挂"的语义。
    TimerTaskComp retryTimer_;
    TimerTaskComp fetchTimeoutTimer_;
};
