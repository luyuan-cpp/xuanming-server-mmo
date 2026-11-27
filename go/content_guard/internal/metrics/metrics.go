// Package metrics 是内容安全服务的 Prometheus 指标与 /metrics、/healthz 端口。
//
// 标签只有场景、结局、类别、失败原因这几个固定集合,不带 player_id,也不带被检文字(AGENTS §9)。
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

var (
	checks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "content_guard",
		Name:      "checks_total",
		Help:      "Text checks by scene, verdict (pass / masked / reject / unavailable / invalid) and deciding category.",
	}, []string{"scene", "verdict", "category"})

	checkSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "content_guard",
		Name:      "check_seconds",
		Help:      "Time spent deciding one text, by scene.",
		Buckets:   []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1},
	}, []string{"scene"})

	lexiconVersion = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "content_guard",
		Name:      "lexicon_version",
		Help:      "Version of the lexicon in use; 0 means only the built-in words are loaded.",
	})

	lexiconWords = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "content_guard",
		Name:      "lexicon_words",
		Help:      "Words compiled into the matcher, by kind (deny / allow).",
	}, []string{"kind"})

	lexiconLoadedTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "content_guard",
		Name:      "lexicon_loaded_timestamp_seconds",
		Help:      "Unix time when the lexicon in use was loaded.",
	})

	lexiconLoadFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "content_guard",
		Name:      "lexicon_load_failures_total",
		Help:      "Failed attempts to load the lexicon from etcd, by reason (etcd / corrupt / apply / watch). The previous lexicon stays in use.",
	}, []string{"reason"})

	platformReviews = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "content_guard",
		Name:      "platform_reviews_total",
		Help:      "Calls to an external platform reviewer, by provider (wechat / douyin) and outcome (pass / reject / unavailable / skip / deferred).",
	}, []string{"provider", "outcome"})

	registerOnce sync.Once
)

// 外部平台审核一次调用的结局(platform_reviews_total 的 outcome,docs/design/rename-and-content-safety.md §6.9)。
// deferred = 世界频道先行的短预算没等到答复,交给后台补审,那一次另记结局,所以它不算失败;告警只按 unavailable 算失败率。
const (
	PlatformOutcomePass        = "pass"
	PlatformOutcomeReject      = "reject"
	PlatformOutcomeUnavailable = "unavailable"
	PlatformOutcomeSkip        = "skip"
	PlatformOutcomeDeferred    = "deferred"
)

// Register 注册全部指标,可重复调用。
func Register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(checks, checkSeconds, lexiconVersion, lexiconWords, lexiconLoadedTimestamp, lexiconLoadFailures, platformReviews)
	})
}

// ObserveCheck 记一次判定。
func ObserveCheck(scene, verdict, category string, elapsed time.Duration) {
	checks.WithLabelValues(scene, verdict, category).Inc()
	checkSeconds.WithLabelValues(scene).Observe(elapsed.Seconds())
}

// SetLexicon 记当前生效的词库。
func SetLexicon(version uint64, denyWords, allowWords int, loadedAt time.Time) {
	lexiconVersion.Set(float64(version))
	lexiconWords.WithLabelValues("deny").Set(float64(denyWords))
	lexiconWords.WithLabelValues("allow").Set(float64(allowWords))
	lexiconLoadedTimestamp.Set(float64(loadedAt.Unix()))
}

// IncLoadFailure 记一次词库加载失败。
func IncLoadFailure(reason string) {
	lexiconLoadFailures.WithLabelValues(reason).Inc()
}

// ObservePlatformReview 记一次外部平台审核调用的结局。provider 与 outcome 都是固定集合(outcome 取上面的 PlatformOutcome* 常量)。
func ObservePlatformReview(provider, outcome string) {
	platformReviews.WithLabelValues(provider, outcome).Inc()
}

// Start 在 addr 上开 /metrics、/healthz 与 /livez;addr 为空则不开。
//   - /healthz 在 ready() 为真时回 200,否则 503:K8s 的就绪探针用它;
//   - /livez 只要进程活着就回 200:存活探针用它。等词库期间 gRPC 口还没监听,存活不能看 gRPC 口,
//     否则「没有词库」会被当成「进程坏了」反复重启。
func Start(addr string, ready func() bool) {
	Register()
	if addr == "" {
		logx.Info("[metrics] MetricsListenAddr empty; Prometheus /metrics, /healthz and /livez disabled")
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if ready != nil && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("[metrics] Prometheus /metrics listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[metrics] HTTP server exited: %v", err)
		}
	}()
}
