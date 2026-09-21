// Package metrics 暴露 friend 服务的 Prometheus 指标:S2C 推送、申请频控、在线查询、
// sweep 积压(终态好友申请)与 sweep 看到的可回收零好友容量行,共五个。
// 端口约定见契约 §7 分工表:friend 的 /metrics 是 :9180(`etc/friend.yaml` 的 MetricsListenAddr)。
//
// 两条硬约束(AGENTS.md §9):
//  1. label 全是**有限枚举**,取值集合就是本文件的常量。player_id / 好友 id 一律只进日志 ——
//     玩家 id 进 label 会让时间序列数目随在线人数线性增长,Prometheus 内存直接爆。
//  2. 指标是**懒注册**的:CounterVec / GaugeVec 在包级变量就建好,MustRegister 只在 Start 的
//     sync.Once 里做。所以没调 Start 的单测(以及 -migrate 模式的进程)照样可以安全 Inc,
//     既不会 panic 重复注册,也不需要在测试里假装启动一个 HTTP 端点。
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

// subsystem 决定指标名前缀:friend_push_total / friend_rate_quota_total / …
// 改它等于改全部指标名,会让已有告警规则与看板静默失配。
const subsystem = "friend"

// outcome label 的全部取值(低基数,全集就是下面这些)。
// 不同指标只用其中一个子集,见各 Observe* 的注释。
const (
	OutcomeOK      = "ok"      // 成功:推送已写入 Kafka / 目标在线
	OutcomeOffline = "offline" // 目标不在线:不是错误,是最常见的正常分支
	OutcomeError   = "error"   // 依赖故障:Kafka 写失败、Redis 不可用
	// 频控用的一对:allowed = 放行,rejected = 被配额挡下(业务预期内的拒绝)。
	OutcomeAllowed  = "allowed"
	OutcomeRejected = "rejected"
)

// reason label 的全部取值,与 proto 的 FriendEventReason 一一对应。
// 新增推送类型 = proto 加枚举 + 这里加常量 + register 的预建列表加一项;
// 漏掉最后一步只会让新序列首次出现时才被看板发现,不会报错,所以三处必须同改。
const (
	ReasonRequestReceived = "request_received" // FRIEND_EVENT_REASON_REQUEST_RECEIVED:收到好友申请
	ReasonRequestAccepted = "request_accepted" // FRIEND_EVENT_REASON_REQUEST_ACCEPTED:申请被同意
)

// sweep 的 mode label 取值。与 config.SweepMode* 同值。
//
// 这里刻意重复了字面量而没有 import friend/internal/config:metrics 必须保持叶子包 ——
// 一旦 config 将来想记一个"配置被降级"之类的指标,import 就成了循环依赖。
// 代价是这两个字面量与 config 的枚举必须同改(config.Validate 的 options 标签是它们的事实源)。
//
// 为什么导出:生产代码里这对字面量共三份(config / data / 本包),本包这份原先不可导出,
// 对齐断言够不到它。而它漂移时是**静默的** —— register() 预建的 0 值序列落在一个 label 上、
// Set* 写的是另一个,"序列长期不更新 = sweep 根本没在跑"这个唯一信号就此失效。
// 导出之后由 logic 包的测试(friend_logic_test.go 的 TestSweepModeConstantsAreTheWireLiterals,
// logic 本来就 import 本包,不成环)机械钉住三份逐字一致。
// 导出**不是**让调用方拿它当 mode 传:写侧仍然传 config.SweepMode*(见 Set* 的注释)。
const (
	SweepModeReportOnly = "report_only"
	SweepModeDelete     = "delete"
)

var (
	// pushTotal:S2C 推送是 at-most-once 的(契约 §5),没有重试也没有回执,
	// 所以"发了多少 / 有多少因为对方离线而丢弃 / 有多少因为 Kafka 故障而丢"只能靠这个计数器看。
	pushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "push_total",
		Help:      "Friend S2C pushes by reason (request_received|request_accepted) and outcome (ok|offline|error).",
	}, []string{"reason", "outcome"})

	// rateQuotaTotal:好友申请的每分钟配额。rejected 常态性升高说明阈值定得太紧(骚扰被挡也算),
	// 与 ok 的比值是调 RequestQuotaPerMinute 的唯一依据 —— 只记 rejected 无法判断分母。
	rateQuotaTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rate_quota_total",
		Help:      "Friend request quota checks by outcome (allowed|rejected|error).",
	}, []string{"outcome"})

	// onlineLookupTotal:读 player:session:{id}(共享 Redis 的跨运行时契约 key)判在线。
	// error 与 offline 必须分开:回落到共享库失败时若也记成 offline,会表现为"全服突然都离线"
	// 而没有任何异常指标。
	onlineLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "online_lookup_total",
		Help:      "player:session lookups for online state by outcome (ok|offline|error).",
	}, []string{"outcome"})

	// sweepPendingRows:上一轮 sweep 扫到的待清理行数(过期好友申请)。
	// 用 Gauge 而不是 Counter:它是"当前积压"不是"累计处理量";report_only 模式下
	// 这个值只会涨不会降,正是判断"能不能切 delete"的依据。
	sweepPendingRows = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "sweep_pending_rows",
		Help:      "Rows the last sweep pass found eligible for cleanup, by sweep mode (report_only|delete).",
	}, []string{"mode"})

	// sweepIdleCapacityRows:上一轮 sweep 看到的"零好友且超过保留期"的 friend_capacity 行数。
	// 与 sweepPendingRows 分成两个指标而不是加一个 table label:两者的告警含义不同 ——
	// 终态申请行随正常社交行为自然累积;零好友容量行的增长可由客户端驱动(对任意 target 发申请
	// 就会建行,见 logic/sweep.go 文件头),它在 report_only 下持续上涨就是"有人在刷"的信号。
	// ⚠ 同样受 BatchLimit 封顶:等于 BatchLimit 只说明"可回收行 ≥ 一批",不是精确值。
	sweepIdleCapacityRows = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "sweep_idle_capacity_rows",
		Help:      "friend_capacity rows with zero friends and older than the retention period that the last sweep pass saw, capped at BatchLimit, by sweep mode (report_only|delete).",
	}, []string{"mode"})

	registerOnce sync.Once
)

// ObservePush 记录一次 S2C 推送的终态。
// reason 取 Reason* 之一,outcome 取 OutcomeOK / OutcomeOffline / OutcomeError。
func ObservePush(reason, outcome string) {
	pushTotal.WithLabelValues(reason, outcome).Inc()
}

// ObserveRateQuota 记录一次申请配额检查。
// outcome 取 OutcomeAllowed / OutcomeRejected / OutcomeError。
// **error 是配额 fail-open 的唯一信号**(见 logic/rate_quota.go 的风险依据):
// Redis 出错时配额直接放行,除了这条序列没有任何地方看得见,必须配告警。
func ObserveRateQuota(outcome string) {
	rateQuotaTotal.WithLabelValues(outcome).Inc()
}

// ObserveOnlineLookup 记录一次在线状态查询。
// outcome 取 OutcomeOK(在线)/ OutcomeOffline(不在线)/ OutcomeError(Redis 故障)。
func ObserveOnlineLookup(outcome string) {
	onlineLookupTotal.WithLabelValues(outcome).Inc()
}

// SetSweepPendingRows 刷新某个 sweep 模式下的待清理行数。
// mode 取 config.SweepMode*(report_only|delete);rows 用 float64 是 Gauge 的原生类型,
// 调用方直接传行数即可,不要先做比例换算 —— 比例应在 PromQL 里算。
func SetSweepPendingRows(mode string, rows float64) {
	sweepPendingRows.WithLabelValues(mode).Set(rows)
}

// SetSweepIdleCapacityRows 刷新某个 sweep 模式下"零好友且超过保留期"的 friend_capacity 行数。
// mode 与 rows 的约定同 SetSweepPendingRows:mode 只许是两个合法模式之一(未知模式不要调,
// 拿拼错的串当 label 会造出一条没人认识的序列);合法模式下每轮都要调,**包括 0**。
func SetSweepIdleCapacityRows(mode string, rows float64) {
	sweepIdleCapacityRows.WithLabelValues(mode).Set(rows)
}

// register 把指标注册进默认 registry,并预建全部 label 组合的 0 值序列。
//
// 为什么必须预建:Prometheus 里"从未发生过"的序列**根本不存在**,
// `rate(friend_push_total{outcome="error"}[5m]) > 0` 这类规则在序列缺失时既不报警也不报错,
// 与"一切正常"长得完全一样。预建成 0 之后,缺失就只可能是抓取本身出了问题。
//
// 注意 sweepPendingRows / sweepIdleCapacityRows 的两个 mode 都会被预建:某个 mode 的值为 0 只代表
// "这个模式下没有积压或没启用",**不能**用它判断当前生效的是哪个模式
// (生效模式看配置与启动日志)。"sweep 根本没在跑"要靠这两个 Gauge 长期不更新来判断。
func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			pushTotal,
			rateQuotaTotal,
			onlineLookupTotal,
			sweepPendingRows,
			sweepIdleCapacityRows,
		)
		for _, reason := range []string{ReasonRequestReceived, ReasonRequestAccepted} {
			for _, outcome := range []string{OutcomeOK, OutcomeOffline, OutcomeError} {
				pushTotal.WithLabelValues(reason, outcome)
			}
		}
		for _, outcome := range []string{OutcomeAllowed, OutcomeRejected, OutcomeError} {
			rateQuotaTotal.WithLabelValues(outcome)
		}
		for _, outcome := range []string{OutcomeOK, OutcomeOffline, OutcomeError} {
			onlineLookupTotal.WithLabelValues(outcome)
		}
		for _, mode := range []string{SweepModeReportOnly, SweepModeDelete} {
			sweepPendingRows.WithLabelValues(mode)
			sweepIdleCapacityRows.WithLabelValues(mode)
		}
	})
}

// Start 启动 Prometheus /metrics 端点;addr 为空则关闭(与 trade / match 同模式)。
//
// 端点是"起不来也要让进程活着"的辅助设施:ListenAndServe 失败只打日志,不 panic ——
// 本机同时起两个 friend 副本时端口撞车很常见,不该因此拒绝对外提供好友服务。
// 反过来,注册只做一次:Start 被重复调用(例如单测)不会 panic。
func Start(addr string) {
	if addr == "" {
		logx.Info("[friend] MetricsListenAddr empty; Prometheus /metrics endpoint disabled")
		return
	}
	register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout 防慢速请求占着连接不放(gosec G112 也要求显式设置)。
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("[friend] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[friend] metrics HTTP server exited: %v", err)
		}
	}()
}
