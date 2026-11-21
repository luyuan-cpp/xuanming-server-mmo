package tablebundle

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	metricsOnce sync.Once
	metricsMu   sync.Mutex

	activeInfoLabels *infoLabels

	tableBundleInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "table_bundle_info",
		Help: "Currently active config table bundle identity (constant 1; one series per process).",
	}, []string{"schema_digest", "content_digest", "source"})

	tableReloadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "table_reload_total",
		Help: "Total config table hot-reload outcomes.",
	}, []string{"result"})

	tableReloadPrepareSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "table_reload_prepare_seconds",
		Help:    "Duration of a full table bundle prepare + validate cycle in seconds.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
)

type infoLabels struct {
	schemaDigest  string
	contentDigest string
	source        string
}

func registerMetrics() {
	metricsOnce.Do(func() {
		for _, c := range []prometheus.Collector{
			tableBundleInfo,
			tableReloadTotal,
			tableReloadPrepareSeconds,
		} {
			err := prometheus.DefaultRegisterer.Register(c)
			if err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					panic(err)
				}
			}
		}
	})
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// setActiveBundleMetric 确保每个进程任意时刻仅暴露一条 table_bundle_info 序列(设计 §11)。
func setActiveBundleMetric(schemaDigest, contentDigest, source string) {
	registerMetrics()
	next := infoLabels{
		schemaDigest:  schemaDigest,
		contentDigest: shortDigest(contentDigest),
		source:        source,
	}
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if activeInfoLabels != nil && *activeInfoLabels != next {
		tableBundleInfo.DeleteLabelValues(
			activeInfoLabels.schemaDigest,
			activeInfoLabels.contentDigest,
			activeInfoLabels.source,
		)
	}
	tableBundleInfo.WithLabelValues(next.schemaDigest, next.contentDigest, next.source).Set(1)
	activeInfoLabels = &next
}

func observeReloadResult(result string) {
	registerMetrics()
	tableReloadTotal.WithLabelValues(result).Inc()
}

func observePrepareSeconds(seconds float64) {
	registerMetrics()
	tableReloadPrepareSeconds.Observe(seconds)
}
