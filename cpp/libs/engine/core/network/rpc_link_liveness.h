#pragma once

// 节点链路的存活判定(docs/design/muduo-rpc-channel.md §21)。纯逻辑:不做 I/O,不读时钟,时刻全由调用方传入。
//
// 两端对称:开了心跳的一端,写空闲满 interval 就发一帧 HEARTBEAT,读空闲满 idleTimeout 就判对端已死。
// 收发路径只置位(OnReceived / OnSent),热路径不读时钟。每次 Check 把置位折算成「本次检查时刻有过收 / 发」,
// 折算最多晚一个 checkPeriod,方向保守:心跳只会晚发,判死只会晚判,都不会提前。
//
// 由 RpcChannel 持有,只在它的 loop 线程上使用,本身不加锁。

#include <chrono>

struct RpcLinkHeartbeat
{
    std::chrono::milliseconds interval;      // 写空闲满这么久,发一帧 HEARTBEAT
    std::chrono::milliseconds idleTimeout;   // 读空闲满这么久,判对端已死,关闭连接
    std::chrono::milliseconds checkPeriod;   // 检查节拍
};

// 合法条件:interval、checkPeriod 都为正;checkPeriod 不长于 interval;idleTimeout 能容下健康链路上最长的静默。
// 最长静默是 interval + 2 × checkPeriod:发送方在节拍上才判出写空闲,接收方在节拍上才记下收到,各晚一个节拍。
constexpr bool IsValidLinkHeartbeat(const RpcLinkHeartbeat& heartbeat)
{
    return heartbeat.interval.count() > 0 && heartbeat.checkPeriod.count() > 0 &&
           heartbeat.checkPeriod <= heartbeat.interval &&
           heartbeat.interval + 2 * heartbeat.checkPeriod < heartbeat.idleTimeout;
}

// 节点链路(gate 出站 RpcClient ↔ scene 入站 RpcServer)两端共用这一份,推导见设计 §21.4:
//   interval    5s:空闲链路每 5s 每个方向 14 字节;一个判死窗口内能收到 3 帧。
//   idleTimeout 15s = 3 × interval:健康空闲链路上相邻两次「收到」最长约 7s,余约 8s 给两端 loop 卡顿与网络抖动。
//               与客户端请求超时(15s)、gate→scene 调用超时(12s)同量级,远短于 etcd 租约与系统 keepalive;
//               与 Java NodeLink 同值(link-ping-interval 5s / link-idle-timeout 15s)。
//   checkPeriod 1s:精度 ±1s;每个 channel 每秒一个定时器。
// 编译期常量,两端同一份,不走配置:只改一端就等于混跑(§21.6)。
inline constexpr RpcLinkHeartbeat kNodeLinkHeartbeat{
    std::chrono::seconds(5), std::chrono::seconds(15), std::chrono::seconds(1)};
static_assert(IsValidLinkHeartbeat(kNodeLinkHeartbeat));
static_assert(kNodeLinkHeartbeat.idleTimeout == 3 * kNodeLinkHeartbeat.interval,
              "idleTimeout tolerates two delayed heartbeats; see docs/design/muduo-rpc-channel.md section 21.4");

class RpcLinkLiveness
{
public:
    using Clock = std::chrono::steady_clock;

    struct Verdict
    {
        Clock::duration checkGap{};   // 距上一次检查(首次为距 start)的时长
        bool inboundSeen{false};      // 上一次检查之后收到过字节:读空闲从本次检查起重新计
        bool idleDeferred{false};     // 读空闲已满,但本次检查迟到(本端 loop 卡过),留到下一次检查再判
        bool closeIdle{false};        // 读空闲已满:调用方关闭连接。此时不发心跳
        bool sendHeartbeat{false};    // 写空闲已满:调用方随即发一帧 HEARTBEAT。已记为本次检查的发送
    };

    // start 是基线时刻(channel 创建时),此前视为刚收发过。options 必须满足 IsValidLinkHeartbeat,由调用方校验。
    RpcLinkLiveness(const RpcLinkHeartbeat& options, Clock::time_point start)
        : options_(options), lastReceivedAt_(start), lastSentAt_(start), lastCheckAt_(start)
    {
    }

    void OnReceived() { receivedSinceCheck_ = true; }   // 收到任何字节
    void OnSent() { sentSinceCheck_ = true; }           // 发出任何非心跳帧;心跳由 Check 自己记

    Verdict Check(Clock::time_point now)
    {
        Verdict verdict;
        verdict.checkGap = now - lastCheckAt_;
        // 距上一次检查超过两个节拍,说明本端 loop 卡过:对端的帧可能还在接收缓冲里没读。
        // Linux muduo 的 timerfd 与连接 fd 在同一次 epoll_wait 里返回,处理先后不定,一次也未必返回全部就绪的 fd。
        // 所以这一次不判对端已死,留到下一次检查(那时积压的输入已经读过)。
        const bool late = verdict.checkGap > 2 * options_.checkPeriod;
        lastCheckAt_ = now;
        if (receivedSinceCheck_)
        {
            receivedSinceCheck_ = false;
            lastReceivedAt_ = now;
            verdict.inboundSeen = true;
        }
        if (sentSinceCheck_)
        {
            sentSinceCheck_ = false;
            lastSentAt_ = now;
        }
        if (now - lastReceivedAt_ >= options_.idleTimeout)
        {
            if (!late)
            {
                verdict.closeIdle = true;
                return verdict;
            }
            verdict.idleDeferred = true;
        }
        if (now - lastSentAt_ >= options_.interval)
        {
            verdict.sendHeartbeat = true;
            lastSentAt_ = now;
        }
        return verdict;
    }

    const RpcLinkHeartbeat& options() const { return options_; }

    // 距最近一次确认收到的时长,日志用。
    Clock::duration SilentFor(Clock::time_point now) const { return now - lastReceivedAt_; }

private:
    RpcLinkHeartbeat options_;
    Clock::time_point lastReceivedAt_;
    Clock::time_point lastSentAt_;
    Clock::time_point lastCheckAt_;
    bool receivedSinceCheck_{false};
    bool sentSinceCheck_{false};
};
