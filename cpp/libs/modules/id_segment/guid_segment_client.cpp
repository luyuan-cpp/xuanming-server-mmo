#include "guid_segment_client.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <sstream>
#include <utility>

#include "muduo/base/Logging.h"

namespace
{
    // 发不出去 / 续段失败的 ERROR 日志节流:退避封顶 5s 时每次都记就是 12 行/分钟,
    // 首次记一条,之后每 12 次(≈ 每分钟)记一条,足够看出"一直在失败"又不刷屏。
    constexpr uint32_t kFailureLogEveryN = 12;
} // namespace

bool GuidSegmentClient::Enable(Options options, SendFn send, ScheduleFn schedule, ClockFn clock)
{
    if (options.kindName.empty())
    {
        LOG_ERROR << "[idsegment] Enable rejected: empty kindName";
        return false;
    }
    if (options.bizTag.empty())
    {
        options.bizTag = options.kindName;
    }
    if (options.initialStep == 0)
    {
        LOG_ERROR << "[idsegment] Enable rejected: initialStep must be > 0 (kind=" << options.kindName << ")";
        return false;
    }
    // 0 = "不动":上下限都钉在初值上,动态 step 退化成固定 step。
    if (options.minStep == 0)
    {
        options.minStep = options.initialStep;
    }
    if (options.maxStep == 0)
    {
        options.maxStep = options.initialStep;
    }
    if (!(options.minStep <= options.initialStep && options.initialStep <= options.maxStep))
    {
        LOG_ERROR << "[idsegment] Enable rejected: need minStep <= initialStep <= maxStep, got " << options.minStep
                  << " / " << options.initialStep << " / " << options.maxStep << " (kind=" << options.kindName << ")";
        return false;
    }
    if (!(options.prefetchAt > 0.0 && options.prefetchAt <= 1.0))
    {
        LOG_ERROR << "[idsegment] Enable rejected: prefetchAt=" << options.prefetchAt
                  << " must be within (0, 1] (kind=" << options.kindName << ")";
        return false;
    }
    if (options.maxIdExclusive == 0 || options.maxIdExclusive > kDefaultMaxIdExclusive)
    {
        // 调大就与存量 snowflake 号的值域相交,见头文件。
        LOG_ERROR << "[idsegment] Enable rejected: maxIdExclusive=" << options.maxIdExclusive
                  << " must be within (0, 2^55] (kind=" << options.kindName << ")";
        return false;
    }
    if (options.stepGrowBelowSec < 0.0 || options.stepShrinkAboveSec < options.stepGrowBelowSec)
    {
        // 倒挂的话同一段既要翻倍又要减半,规则没有意义。
        LOG_ERROR << "[idsegment] Enable rejected: need 0 <= stepGrowBelowSec <= stepShrinkAboveSec, got "
                  << options.stepGrowBelowSec << " / " << options.stepShrinkAboveSec << " (kind=" << options.kindName
                  << ")";
        return false;
    }
    if (!send)
    {
        LOG_ERROR << "[idsegment] Enable rejected: null SendFn (kind=" << options.kindName << ")";
        return false;
    }
    if (options.retryBackoffMinSec <= 0.0)
    {
        options.retryBackoffMinSec = 0.5;
    }
    if (options.retryBackoffMaxSec < options.retryBackoffMinSec)
    {
        options.retryBackoffMaxSec = options.retryBackoffMinSec;
    }
    if (options.fetchTimeoutSec <= 0.0)
    {
        options.fetchTimeoutSec = 3.0;
    }

    Reset();
    options_ = std::move(options);
    send_ = std::move(send);
    schedule_ = std::move(schedule);
    clock_ = std::move(clock);
    step_ = options_.initialStep;
    backoffSec_ = options_.retryBackoffMinSec;
    enabled_ = true;
    LOG_INFO << "[idsegment] enabled kind=" << options_.kindName << " biz_tag=" << options_.bizTag
             << " step=" << step_ << " [" << options_.minStep << ", " << options_.maxStep << "]"
             << " prefetch_at=" << options_.prefetchAt << " max_id_exclusive=" << options_.maxIdExclusive
             << " fetch_timeout_sec=" << options_.fetchTimeoutSec;
    return true;
}

void GuidSegmentClient::Shutdown()
{
    if (!enabled_ || stopped_)
    {
        return;
    }
    stopped_ = true;
    // 让已挂的定时器回调作废;内置定时器顺手取消。在途请求的响应仍会经 OnResponse
    // 落下(白拿的号不用退还,号段模式本来就允许浪费)。
    ++retryGen_;
    ++fetchSeq_;
    retryPending_ = false;
    inflight_ = false;
    retryTimer_.Cancel();
    fetchTimeoutTimer_.Cancel();
    LOG_INFO << "[idsegment] shutdown: no further segment fetches; " << Describe();
}

void GuidSegmentClient::Reset()
{
    retryTimer_.Cancel();
    fetchTimeoutTimer_.Cancel();
    // 代数只增不减:注入式定时器(单测)可能还攥着旧回调,靠代数不等让它们空转。
    ++retryGen_;
    ++fetchSeq_;
    options_ = Options{};
    send_ = nullptr;
    schedule_ = nullptr;
    clock_ = nullptr;
    enabled_ = false;
    stopped_ = false;
    cur_ = Segment{};
    next_ = Segment{};
    hasNext_ = false;
    curStartedAtSec_ = 0;
    step_ = 0;
    lastRangeLastedSec_ = 0;
    rangesConsumed_ = 0;
    stepChanges_ = 0;
    inflight_ = false;
    retryPending_ = false;
    backoffSec_ = 0.5;
    highWater_ = 0;
    consecutiveFailures_ = 0;
    issued_ = fetches_ = fetchErrors_ = rangeViolations_ = 0;
    unavailable_ = sendUnavailable_ = timeouts_ = droppedRanges_ = 0;
}

bool GuidSegmentClient::IsReady() const
{
    return enabled_ && (cur_.Remaining() > 0 || (hasNext_ && next_.Remaining() > 0));
}

uint64_t GuidSegmentClient::Available() const
{
    if (!enabled_)
    {
        return 0;
    }
    return cur_.Remaining() + (hasNext_ ? next_.Remaining() : 0);
}

double GuidSegmentClient::NowSec() const
{
    if (clock_)
    {
        return clock_();
    }
    return std::chrono::duration<double>(std::chrono::steady_clock::now().time_since_epoch()).count();
}

GuidSegmentClient::Segment GuidSegmentClient::MakeSegment(uint64_t lo, uint64_t hi) const
{
    Segment segment;
    segment.pos = lo;
    segment.hi = hi;
    const uint64_t length = hi > lo ? hi - lo : 0;
    segment.prefetchThreshold = static_cast<uint64_t>(std::ceil(options_.prefetchAt * static_cast<double>(length)));
    if (segment.prefetchThreshold < 1)
    {
        segment.prefetchThreshold = 1;
    }
    return segment;
}

void GuidSegmentClient::BecomeCurrent(const Segment &segment)
{
    cur_ = segment;
    curStartedAtSec_ = NowSec();
}

// 当前段刚发出最后一个号:量它撑了多久,按 Leaf 口径调下一次领段的 step。
// 只在这里调、只调 step_:已经在途的请求不受影响(它带着发出时的 step),
// 所以效果最早体现在再下一次领段上 —— 一段的滞后,对"压浪费"这个目的无所谓。
void GuidSegmentClient::OnCurrentExhausted()
{
    const double lasted = NowSec() - curStartedAtSec_;
    ++rangesConsumed_;
    lastRangeLastedSec_ = lasted;

    uint64_t next = step_;
    if (lasted < options_.stepGrowBelowSec)
    {
        next = std::min<uint64_t>(uint64_t{step_} * 2, options_.maxStep);
    }
    else if (lasted > options_.stepShrinkAboveSec)
    {
        next = std::max<uint64_t>(uint64_t{step_} / 2, options_.minStep);
    }
    if (next != step_)
    {
        ++stepChanges_;
        LOG_INFO << "[idsegment] kind=" << options_.kindName << " range lasted " << lasted << "s: step " << step_
                 << " -> " << next << " (bounds [" << options_.minStep << ", " << options_.maxStep << "])";
        step_ = static_cast<uint32_t>(next);
    }
}

bool GuidSegmentClient::TryNext(Guid &out)
{
    if (!enabled_)
    {
        return false;
    }
    if (cur_.Remaining() == 0 && hasNext_)
    {
        BecomeCurrent(next_);
        next_ = Segment{};
        hasNext_ = false;
    }
    if (cur_.Remaining() > 0)
    {
        out = static_cast<Guid>(cur_.pos);
        ++cur_.pos;
        ++issued_;
        if (cur_.Remaining() == 0)
        {
            OnCurrentExhausted();
        }
        MaybePrefetch();
        return true;
    }
    // 两段都空:不阻塞,记一次拒发并把续段催起来(在途 / 退避中则什么都不做)。
    ++unavailable_;
    StartFetch("exhausted");
    return false;
}

void GuidSegmentClient::Warm()
{
    if (!enabled_ || IsReady())
    {
        return;
    }
    StartFetch("warm");
}

void GuidSegmentClient::MaybePrefetch()
{
    // 剩余触底就预取;已有下一段 / 有在途 / 退避中则不重复 —— 这就是"单飞"。
    if (!hasNext_ && !inflight_ && !retryPending_ && cur_.Remaining() <= cur_.prefetchThreshold)
    {
        StartFetch("prefetch");
    }
}

void GuidSegmentClient::StartFetch(const char *why)
{
    if (!enabled_ || stopped_ || inflight_ || retryPending_ || hasNext_)
    {
        return;
    }
    ++fetchSeq_;
    inflight_ = true;
    if (!send_(options_.bizTag, step_))
    {
        // 没有可用的 DataService 节点 / 共享通道忙:不必等超时,直接按退避重试。
        inflight_ = false;
        ++sendUnavailable_;
        OnFetchFailed("send unavailable (no ready DataService node, or the shared channel is busy)",
                      /*alreadyLogged=*/false);
        return;
    }
    LOG_DEBUG << "[idsegment] fetch started kind=" << options_.kindName << " why=" << why << " seq=" << fetchSeq_
              << " step=" << step_ << " " << Describe();
    // 生成的 gRPC 客户端在 status 非 OK 时不回调 handler,超时是唯一的失败通知。
    Schedule(TimerKind::kFetchTimeout, options_.fetchTimeoutSec,
             [this, seq = fetchSeq_] { OnFetchTimeout(seq); });
}

void GuidSegmentClient::OnFetchTimeout(uint64_t seq)
{
    // 只对当次发送生效:响应已到(inflight_ 已清)或又发了新的一次(seq 变了)都忽略。
    if (!inflight_ || seq != fetchSeq_)
    {
        return;
    }
    inflight_ = false;
    ++timeouts_;
    OnFetchFailed("fetch timeout (transport failure or data_service not answering)", /*alreadyLogged=*/false);
}

void GuidSegmentClient::OnFetchFailed(const char *why, bool alreadyLogged)
{
    ++fetchErrors_;
    ++consecutiveFailures_;
    if (!alreadyLogged && (consecutiveFailures_ == 1 || consecutiveFailures_ % kFailureLogEveryN == 0))
    {
        LOG_ERROR << "[idsegment] kind=" << options_.kindName << " biz_tag=" << options_.bizTag
                  << " allocate failed: " << why << " (consecutive_failures=" << consecutiveFailures_
                  << " next_retry_sec=" << backoffSec_ << ") " << Describe();
    }
    ScheduleRetry();
}

void GuidSegmentClient::ScheduleRetry()
{
    if (stopped_ || retryPending_)
    {
        return;
    }
    retryPending_ = true;
    const double delay = backoffSec_;
    backoffSec_ = std::min(backoffSec_ * 2.0, options_.retryBackoffMaxSec);
    const uint64_t gen = ++retryGen_;
    Schedule(TimerKind::kRetry, delay, [this, gen] { OnRetryTimer(gen); });
}

void GuidSegmentClient::OnRetryTimer(uint64_t gen)
{
    if (gen != retryGen_ || !retryPending_)
    {
        return;
    }
    retryPending_ = false;
    // 退避期间晚到的响应可能已经把段补上了;只在"现在确实需要"时才重发,
    // 否则会把预取时机提前到不该发的时候。
    if (hasNext_ || cur_.Remaining() > cur_.prefetchThreshold)
    {
        return;
    }
    StartFetch("retry");
}

void GuidSegmentClient::Schedule(TimerKind kind, double delaySec, std::function<void()> fn)
{
    if (schedule_)
    {
        schedule_(kind, delaySec, std::move(fn));
        return;
    }
    // 内置:同类定时器重新武装即取消上一枚。没有 EventLoop 的线程上 RunAfter 是空操作,
    // 那种环境(单测)必须注入 schedule_。
    (kind == TimerKind::kRetry ? retryTimer_ : fetchTimeoutTimer_).RunAfter(delaySec, std::move(fn));
}

const char *GuidSegmentClient::ValidateRange(uint64_t lo, uint64_t hi) const
{
    if (lo < 1)
    {
        return "lo < 1";
    }
    if (hi <= lo)
    {
        return "hi <= lo";
    }
    if (hi > options_.maxIdExclusive)
    {
        return "hi > max_id_exclusive(2^55): would intersect the snowflake domain";
    }
    if (lo < highWater_)
    {
        return "lo < previous hi: ranges regressed/overlapped";
    }
    return nullptr;
}

void GuidSegmentClient::OnResponse(uint32_t errorCode, uint64_t lo, uint64_t hi)
{
    if (!enabled_)
    {
        LOG_WARN << "[idsegment] response while disabled, ignored: error_code=" << errorCode
                 << " range=[" << lo << ", " << hi << ")";
        return;
    }
    // 无论是不是本次在途的响应,先解开单飞:超时回调靠 inflight_/seq 识别自己已过期。
    inflight_ = false;

    if (errorCode != 0)
    {
        std::ostringstream why;
        why << "server error_code=" << errorCode;
        const std::string text = why.str();
        OnFetchFailed(text.c_str(), /*alreadyLogged=*/false);
        return;
    }

    if (const char *reason = ValidateRange(lo, hi); reason != nullptr)
    {
        ++rangeViolations_;
        // 大声:这是服务端 bug 的直接证据,而且离「静默串档」只差一步。
        LOG_ERROR << "[idsegment] REJECTED range [" << lo << ", " << hi << ") for kind=" << options_.kindName
                  << " biz_tag=" << options_.bizTag << ": " << reason << " (high_water=" << highWater_
                  << ") -- refusing to mint from it "
                  << "(server-side bug: an overlapping or out-of-domain range would silently collide with existing ids)";
        OnFetchFailed(reason, /*alreadyLogged=*/true);
        return;
    }

    // 校验通过即视为已从服务端领走:不管落不落得下,水位都要抬(单调性靠它)。
    highWater_ = hi;
    consecutiveFailures_ = 0;
    backoffSec_ = options_.retryBackoffMinSec;

    if (cur_.Remaining() == 0 && !hasNext_)
    {
        BecomeCurrent(MakeSegment(lo, hi));
    }
    else if (!hasNext_)
    {
        next_ = MakeSegment(lo, hi);
        hasNext_ = true;
    }
    else
    {
        // 超时后又重发、结果两份都到了:多出来的这一段作废(号段模式允许浪费)。
        ++droppedRanges_;
        LOG_WARN << "[idsegment] dropping surplus range [" << lo << ", " << hi << ") kind=" << options_.kindName
                 << ": both buffers already full (late response after timeout+retry)";
        return;
    }
    ++fetches_;
    LOG_INFO << "[idsegment] kind=" << options_.kindName << " biz_tag=" << options_.bizTag << " got range [" << lo
             << ", " << hi << ") " << Describe();
}

GuidSegmentClient::Stats GuidSegmentClient::GetStats() const
{
    Stats stats;
    stats.issued = issued_;
    stats.fetches = fetches_;
    stats.fetchErrors = fetchErrors_;
    stats.rangeViolations = rangeViolations_;
    stats.unavailable = unavailable_;
    stats.sendUnavailable = sendUnavailable_;
    stats.timeouts = timeouts_;
    stats.droppedRanges = droppedRanges_;
    stats.rangesConsumed = rangesConsumed_;
    stats.stepChanges = stepChanges_;
    stats.currentRemaining = cur_.Remaining();
    stats.nextRemaining = hasNext_ ? next_.Remaining() : 0;
    stats.nextReady = hasNext_;
    stats.inflight = inflight_;
    stats.retryPending = retryPending_;
    stats.highWater = highWater_;
    stats.backoffSec = backoffSec_;
    stats.currentStep = step_;
    stats.lastRangeLastedSec = lastRangeLastedSec_;
    return stats;
}

std::string GuidSegmentClient::Describe() const
{
    std::ostringstream out;
    out << "(kind=" << options_.kindName << " enabled=" << enabled_ << " stopped=" << stopped_ << " step=" << step_
        << " cur=[" << cur_.pos << "," << cur_.hi << ")"
        << " next=" << (hasNext_ ? "[" + std::to_string(next_.pos) + "," + std::to_string(next_.hi) + ")" : "none")
        << " inflight=" << inflight_ << " retry_pending=" << retryPending_ << " issued=" << issued_
        << " fetches=" << fetches_ << " errors=" << fetchErrors_ << " violations=" << rangeViolations_
        << " unavailable=" << unavailable_ << " high_water=" << highWater_ << ")";
    return out.str();
}
