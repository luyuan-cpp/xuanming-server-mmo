// Package metrics exposes Prometheus instrumentation for the db service.
//
// go/db 现在只做一件事:PlayerStore 存盘 gRPC。指标分三类:RPC 结局与耗时、提交行大小、
// 落点库的打开与连接池。label 都是封闭集合,不含 player_id(AGENTS.md §9)。
package metrics

import (
	"net/http"
	"shared/sqlpoolstats"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "db"

var latencyBuckets = []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

var (
	// result 的取值是 proto 枚举名加下面三个 PlayerStoreXxx。
	playerStoreRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "player_store_requests_total",
		Help:      "PlayerStore RPC outcomes. op: save | claim.",
	}, []string{"op", "result"})

	playerStoreSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "player_store_seconds",
		Help:      "PlayerStore RPC latency, including the writer gate statements. op: save | claim.",
		Buckets:   latencyBuckets,
	}, []string{"op"})

	playerStoreRowBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "player_store_row_bytes",
		Help:      "Payload size of committed player_store rows by kind.",
		Buckets:   prometheus.ExponentialBuckets(256, 4, 8),
	}, []string{"kind"})

	// placementStoreOpenTotal 记每一次**真正发生**的按需打开落点库;打开失败后的冷却期内
	// 直接复用上次错误的请求不重复计数,所以 error 的速率就是「每个冷却期一次」,不会被请求量放大。
	placementStoreOpenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "placement_store_open_total",
		Help:      "On-demand placement store opens (player-storage-placement.md 6.1). result: ok | error (ping / schema gate / unknown database) | rejected (neither in the allow list nor admitted by AllowStoreFamilies).",
	}, []string{"result"})

	// placementOpenStores 是本进程当前常驻的落点库个数(含本 zone 库)。
	placementOpenStores = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "placement_open_stores",
		Help:      "Placement stores (MySQL databases) currently open in this db process, including the zone's own database.",
	})

	registerOnce sync.Once
)

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(playerStoreRequestsTotal, playerStoreSeconds, playerStoreRowBytes,
			placementStoreOpenTotal, placementOpenStores)
		for _, result := range []string{StoreOpenOK, StoreOpenError, StoreOpenRejected} {
			placementStoreOpenTotal.WithLabelValues(result).Add(0)
		}
	})
}

// PrimePlayerStore 把一个 op 的全部 result 以 0 预置,告警的 increase 才抓得到第一次。
func PrimePlayerStore(op string, results []string) {
	register()
	for _, result := range results {
		playerStoreRequestsTotal.WithLabelValues(op, result).Add(0)
	}
}

// ObservePlayerStore 记一次 PlayerStore RPC 的结局和耗时。
func ObservePlayerStore(op, result string, d time.Duration) {
	register()
	playerStoreRequestsTotal.WithLabelValues(op, result).Inc()
	playerStoreSeconds.WithLabelValues(op).Observe(d.Seconds())
}

// ObservePlayerStoreRowBytes 记一行已提交 payload 的大小。
func ObservePlayerStoreRowBytes(kind string, bytes int) {
	register()
	playerStoreRowBytes.WithLabelValues(kind).Observe(float64(bytes))
}

// PlayerStore RPC 在拿到判定结果之前就结束时的 result 取值。
const (
	PlayerStoreDenied      = "denied"
	PlayerStoreUnavailable = "unavailable"
	PlayerStoreUnknown     = "unknown_outcome"
)

// storeCooldownCollector 在每次抓取时现算 db_store_cooldown_stores:此刻仍在库级冷却期内(探活确认整库不可达)的库个数,
// 按连接池类别分。不用 Gauge 在进出冷却时设值:冷却到期后要等下一次选库探活才删记录,某个库到期后再也没有流量时
// 设值式的指标会一直挂着,告警跟着常亮。
type storeCooldownCollector struct {
	source func() map[string]int
	desc   *prometheus.Desc
}

func (c *storeCooldownCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *storeCooldownCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.source()
	for _, pool := range []string{PoolHome, PoolExtraZone, PoolExtraNonZone} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[pool]), pool)
	}
}

// NewStoreCooldownCollector 只构造不注册(单测用自己的注册表)。
func NewStoreCooldownCollector(source func() map[string]int) prometheus.Collector {
	desc := prometheus.NewDesc(prometheus.BuildFQName("", subsystem, "store_cooldown_stores"),
		"Databases currently cooling down after a probe confirmed they are unreachable. pool: home | extra_zone | extra_non_zone.",
		[]string{"pool"}, nil)
	return &storeCooldownCollector{source: source, desc: desc}
}

// RegisterStoreCooldown 把落点库注册表的冷却计数接到默认注册表。进程内调一次;注册失败只记日志,不挡落库。
func RegisterStoreCooldown(source func() map[string]int) {
	register()
	if err := prometheus.Register(NewStoreCooldownCollector(source)); err != nil {
		logx.Errorf("[metrics] register store cooldown collector failed (db_store_cooldown_stores unavailable): %v", err)
	}
}

// 按需打开落点库的 result label 取值。
const (
	StoreOpenOK       = "ok"
	StoreOpenError    = "error"
	StoreOpenRejected = "rejected"
)

// 连接池类别(mmorpg_sql_pool_*{sql_client="db",pool=...});封闭集合,不含库名(落点库数不设上限)。
// deploy/k8s/mysql-connection-alerts.yaml 的 DbExtraStorePoolsBeyondPlan 按 extra_zone / extra_non_zone 两个取值写死,
// 改常量必须同改告警。
const (
	PoolHome         = "home"           // 本 zone 库(InitDB 打开,Database.MaxOpenConn)
	PoolExtraZone    = "extra_zone"     // 额外 zone 家族库 zone_{1..999999}_db(本 zone 库除外)
	PoolExtraNonZone = "extra_non_zone" // 非 zone 落点库 player_store_{>=1000000}_db
)

// NewSQLPoolCollector 只构造不注册(单测用自己的注册表)。sql_client 取本包的 subsystem("db")。
func NewSQLPoolCollector(source sqlpoolstats.Source) prometheus.Collector {
	return sqlpoolstats.NewCollector(subsystem, []string{PoolHome, PoolExtraZone, PoolExtraNonZone}, source)
}

// RegisterSQLPools 把连接池快照接到默认注册表(Start 暴露的那个)。进程内调一次;注册失败只记日志:
// 指标是观测面,不能挡住落库。MetricsListenAddr 留空时调用也无害(没人抓)。
func RegisterSQLPools(source sqlpoolstats.Source) {
	register()
	if err := prometheus.Register(NewSQLPoolCollector(source)); err != nil {
		logx.Errorf("[metrics] register sql pool collector failed (pool metrics unavailable): %v", err)
	}
}

// CountPlacementStoreOpen 记一次按需打开落点库的结果,result 用 StoreOpen* 常量。
func CountPlacementStoreOpen(result string) {
	register()
	placementStoreOpenTotal.WithLabelValues(result).Inc()
}

// SetPlacementOpenStores 更新常驻落点库个数。
func SetPlacementOpenStores(n int) {
	register()
	placementOpenStores.Set(float64(n))
}

// Start exposes /metrics over HTTP. Failures log but do not block service
// startup — metrics are observability, not critical path. Empty addr
// disables the endpoint (production default; opt-in via config).
func Start(addr string) {
	if addr == "" {
		logx.Info("metrics endpoint disabled (MetricsListenAddr empty)")
		return
	}
	register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("db /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("db /metrics ListenAndServe failed: %v", err)
		}
	}()
}
