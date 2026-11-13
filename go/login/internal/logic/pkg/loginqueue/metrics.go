// metrics.go — Prometheus instrumentation for the AssignGate login queue.
//
// All metrics are exposed through go-zero's built-in /metrics endpoint
// (RpcServerConf.Prometheus in login.yaml). Naming follows the
// `login_queue_*` convention for easy Grafana filtering.
//
// The first six map 1:1 onto the runbook's "待补 Prometheus 指标" table; the two capacity gauges
// came with the gate-capacity cap (2026-11-10, capacity.go):
//
//	login_queue_size              Gauge       per zone     ZCARD snapshot
//	login_queue_admitted_inflight Gauge       per zone     in-flight placeholders (ZCARD after dropping expired)
//	login_queue_wait_seconds      Histogram   per zone     time from Enqueue to Poll-admit
//	login_queue_admit_total       Counter     per zone     dispatcher PopAdmit results
//	login_queue_expired_total     Counter     per zone+reason  EXPIRED responses (token invalid / meta gone / etc.)
//	login_queue_dispatcher_is_leader  Gauge   per pod+zone  1 = this pod leads its zone, 0 = standby
//	login_queue_capacity          Gauge       per zone     admission ceiling FreeSlots used (after the gate cap, 2026-11-10)
//	login_queue_configured_capacity Gauge     per zone     zone_config.capacity from the zone policy, 0 = policy unknown (soft cap)
//
// Why only these six (and not, say, "rank histogram" or "per-gate admit
// distribution"):
//   - rank already shows up in the periodic robot stats line; on the server
//     side ZRANK is O(log N) per call, recording it for every Poll is
//     wasteful when the queue length itself (login_queue_size) tells the
//     same story.
//   - per-gate admit distribution is interesting for capacity planning but
//     not for SLO; defer to a later PR if ops asks for it.
//
// Histogram bucket choice for wait_seconds:
//   - sub-second buckets: catches "queue effectively empty, dispatcher
//     just hadn't gotten to it" cases that should NOT show up in the tail.
//   - 1s..120s buckets: this is the "normal queue" range; a healthy server
//     under capacity should sit in 5-30s.
//   - 300s/600s outliers: anything past 5min is a red flag (dispatcher
//     stalled or AdmitTTL races).
//
// All metrics are nil-safe to call: counters/gauges from go-zero
// metric.NewXxxVec are non-nil singletons, so handlers don't need
// guard checks.
package loginqueue

import (
	"context"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/metric"
)

const metricNamespace = "login_queue"

var (
	// queueSizeGauge mirrors ZCARD queue:zone:{zone}. Updated periodically
	// by the dispatcher (every drain tick) and on Enqueue/Poll paths so
	// scrape granularity matches dispatcher granularity.
	queueSizeGauge = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "size",
		Help: "Current length of the AssignGate login queue per zone (ZCARD queue:zone:{zone}). " +
			"Healthy: drops to 0 within a few minutes after open-server burst. " +
			"Red flag: monotonically rising — dispatcher likely stalled.",
		Labels: []string{"zone"},
	})

	// admittedInflightGauge 是清掉过期成员之后 admitted:zset:zone:{zone} 的成员数:
	// 已放行还没来取的,加上票已交出去、还在交票后宽限(20s)之内的(排队取走与快速通道)。
	// 容量记账用它(FreeSlots 减去这一项)。
	admittedInflightGauge = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "admitted_inflight",
		Help: "In-flight admission placeholders per zone: admits not yet picked up, plus gate " +
			"tokens handed out within the last 20s handoff grace (queue consume and fast path). " +
			"ZCARD admitted:zset:zone:{zone} after dropping expired members. Sitting at " +
			"(capacity - online) while login_queue_size > 0 is normal back-pressure; if gate " +
			"PlayerCount does not grow at the same time, clients are not redeeming their admits.",
		Labels: []string{"zone"},
	})

	// capacityGauge 是 FreeSlots 实际用的区容量:配置值(没配时按软上限)再按本区在接客的 gate 到水位一共能接的人数封顶
	// (gatepick.AdmissionCapacity,2026-11-10 起)。每次算容量都写:快速通道每次 AssignGate、dispatcher 每个非空拍。
	// 排队关着时不算容量,也就没有这条序列。
	capacityGauge = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "capacity",
		Help: "Admission ceiling FreeSlots used for the zone at its last computation: zone_config.capacity from the " +
			"zone policy (or the soft cap while the policy is unknown), capped at what the zone's serving gates hold at 90% of their " +
			"self-reported max_connections. Written on every capacity computation (AssignGate fast path, " +
			"dispatcher ticks with a non-empty queue); absent while the queue is disabled.",
		Labels: []string{"zone"},
	})

	// configuredCapacityGauge 是区服目录给本区的容量(zone_config.capacity,经 zonepolicy 读),0 = 读不到区服策略(按软上限)。
	// 与 capacityGauge 同时写。它大于 login_queue_capacity 时,gate 台数装不下目录的容量,排队会比目录写的早开始拦人
	// (告警 LoginQueueCapacityLimitedByGates)。
	configuredCapacityGauge = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "configured_capacity",
		Help: "zone_config.capacity for the zone as read from the zone policy in etcd; 0 = policy unknown (soft cap in use). " +
			"Above login_queue_capacity means the zone's gates cannot hold the directory capacity and the queue " +
			"starts holding players earlier than configured: add gate replicas or lower zone_config.capacity.",
		Labels: []string{"zone"},
	})

	// waitSecondsHistogram measures end-to-end queue wait per admitted
	// entry. Recorded in Poll() when status flips QUEUEING → ADMITTED.
	waitSecondsHistogram = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "wait_seconds",
		Help: "End-to-end wait time for queue entries that were admitted, " +
			"measured from Enqueue (queue:meta:{queueId} createdAt) to the " +
			"Poll call that consumed admit:{queueId}. p99 > 300s is a red " +
			"flag — dispatcher can't keep up with EnterGame.",
		Labels:  []string{"zone"},
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
	})

	// admitTotal counts dispatcher PopAdmit successes. The increase rate
	// equals the dispatcher's effective drain throughput.
	admitTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "admit_total",
		Help: "Cumulative count of queue entries admitted by the dispatcher, " +
			"per zone. rate(login_queue_admit_total[1m]) == dispatcher drain QPS.",
		Labels: []string{"zone"},
	})

	// expiredTotal counts EXPIRED responses. The reason label distinguishes
	// the failure modes that share the same status code on the wire:
	//   "bad_signature" — HMAC mismatch / malformed token
	//   "ttl"           — body.ExpireTS in the past
	//   "meta_gone"     — Poll found token but meta TTL'd before admit
	//   "queue_disabled" — Queue.Enabled=false but client polled QueryQueueStatus
	//   "admit_window_passed" — 放行过(meta 有 admitted_at),放行槽却已被取走或已按 AdmitTTL 过期(2026-10-15 起;
	//                     此前这两种情况回「排队中、名次 0」,不计数)。持续升高 = 客户端轮询慢于 AdmitTTL,
	//                     或取走的回包在丢、客户端在重试。
	//
	// (2026-10-18 补注)各原因的新语义:
	//   - "meta_gone" 含空闲超过 QueueEntryTTL 的条目(token 索引或 meta 已按空闲 TTL 过期;此前这类通常记 ttl,
	//     因为令牌与条目同为 1h),也含 meta 类型损坏、meta 的 zone 与令牌不符;zone 一律取自令牌(此前 token 索引
	//     不在时记 unknown;令牌里的 zone 为 0 时仍记 unknown)。
	//   - "ttl" 只表示排队令牌超过 24h 的绝对过期(loginqueue.queueTokenMaxAge)。
	//   - "queue_gone":meta 在、没有 admitted_at、又不在队列(来源见 Queue.lookupScript:整个 zone 空闲超过
	//     QueueEntryTTL 后队列键先到期、人工 ZREM / DEL 队列键、排队开着时混跑新旧版)。持续非零说明有人在排队开着时
	//     混跑新旧版,或手工动了队列键。与其它原因一样不预置标签(go-zero 指标开关打开前写的样本会被丢弃),面板用
	//     `or vector(0)`。
	expiredTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "expired_total",
		Help: "Cumulative count of EXPIRED responses to AssignGate/QueryQueueStatus, per zone " +
			"(taken from the queue token; 'unknown' when the token could not be parsed) and reason: " +
			"'bad_signature' = forged or malformed queue token; " +
			"'ttl' = queue token past its 24h absolute lifetime; " +
			"'meta_gone' = entry idle longer than QueueEntryTTL (token index or meta expired), corrupt meta, or zone mismatch; " +
			"'queue_gone' = meta present but neither queued nor admitted (whole zone idle past QueueEntryTTL, " +
			"manual ZREM/DEL of the queue key, or old and new login versions mixed while the queue was enabled); " +
			"'admit_window_passed' = admitted, but the admit slot was already taken or expired; " +
			"'queue_disabled' = polled while Queue.Enabled=false.",
		Labels: []string{"zone", "reason"},
	})

	// dispatcherIsLeaderGauge:每个副本在自己所在 zone 发布 1 或 0。稳态下
	// sum by (zone) 恰好为 1;短暂为 0 表示没有 leader,短暂为 2 表示 Redis 抖动
	// 或滚动升级窗口里出现了双 leader(见 login-queue-2026-05.md §7.1)。
	// pod 标签区分此刻是哪个副本在出队;zone 是低基数标签(zone 数),不含 player_id。
	dispatcherIsLeaderGauge = metric.NewGaugeVec(&metric.GaugeVecOpts{
		Namespace: metricNamespace,
		Subsystem: "",
		Name:      "dispatcher_is_leader",
		Help: "Whether this login pod currently holds its zone's dispatcher leader lock " +
			"(1=leader, 0=standby). sum by (zone) (login_queue_dispatcher_is_leader) " +
			"should be 1 in steady state.",
		Labels: []string{"pod", "zone"},
	})
)

// Reason strings for expiredTotal — exported as constants so the call
// sites self-document why they're recording an EXPIRED.
const (
	ExpireReasonBadSignature  = "bad_signature"
	ExpireReasonTTL           = "ttl"
	ExpireReasonMetaGone      = "meta_gone"
	ExpireReasonQueueDisabled = "queue_disabled"

	// ExpireReasonAdmitWindowPassed:放行槽已被取走或已过期之后再查(见 Queue.Poll)。
	ExpireReasonAdmitWindowPassed = "admit_window_passed"

	// ExpireReasonQueueGone:meta 在、没有 admitted_at、又不在队列(2026-10-18 起,见 Queue.Poll 与 lookupScript)。
	// 此前这种形状回「排队中、名次 0」的在途窗口。
	ExpireReasonQueueGone = "queue_gone"
)

// labelZone formats a uint32 zone id for use as a Prometheus label.
// Centralized so a future label-strategy change (e.g. zone groups) only
// touches one spot.
func labelZone(zoneID uint32) string {
	return strconv.FormatUint(uint64(zoneID), 10)
}

// recordExpired is a small wrapper so callers don't repeat the labelZone
// formatting and so unknown-zone (parse-failed token) gets a consistent
// "unknown" label instead of "0", which would alias zone 0.
func recordExpired(zoneID uint32, reason string) {
	z := "unknown"
	if zoneID != 0 {
		z = labelZone(zoneID)
	}
	expiredTotal.Inc(z, reason)
}

// RecordExpiredQueueDisabled is an exported helper for handlers that need
// to surface "queue disabled" without going through ParseAndVerifyQueueToken.
// Lives in the package's public API because the handler in
// loginpregate/queryqueuestatuslogic.go records it directly.
func RecordExpiredQueueDisabled() {
	recordExpired(0, ExpireReasonQueueDisabled)
}

// recordAdmit is called by the dispatcher after each successful PopAdmit
// batch, with the count of entries that were actually admitted (which may
// be less than the requested batch if signFn failed for some entries).
// (2026-10-15 补注)meta 已过期的条目(在队里待满 QueueEntryTTL)与写入出错的条目都不计入,
// 此前前者照样计入、虚增 admit_total;两类都由 PopAdmit 每批汇总一行日志(见 Queue.PopAdmit)。
// (2026-10-18 补注)PopAdmit 中途出错时返回的已放行数照样计入(dispatchOnce 先记账再判错误);「写入出错的条目」
// 现在留在队列、下一拍再放,不再丢失。go-redis 重发、第一次已写成的放行(REPLAYED)也计入。
func recordAdmit(zoneID uint32, count int) {
	if count <= 0 {
		return
	}
	admitTotal.Add(float64(count), labelZone(zoneID))
}

// recordWait records one queue wait observation in the histogram.
// Negative durations (clock skew across pods) are clamped to 0 — better
// than polluting the histogram with bogus data.
//
// We use ObserveFloat (not Observe, which truncates to int64 seconds)
// because the histogram buckets start at 0.5s — int64 truncation would
// collapse all sub-1s observations into the 0 bucket and hide the
// "queue is healthy, drained instantly" case from the dashboard.
func recordWait(zoneID uint32, d time.Duration) {
	secs := d.Seconds()
	if secs < 0 {
		secs = 0
	}
	waitSecondsHistogram.ObserveFloat(secs, labelZone(zoneID))
}

// publishGaugeSnapshot updates queue_size and admitted_inflight gauges
// for the given zone by reading Redis directly. Called by the dispatcher
// once per drain tick — that cadence (1s) is the right granularity for
// these metrics; doing it inline on every Enqueue/Poll would drown
// Redis in extra ZCARD / EVAL calls for marginal accuracy gain.
//
// The dispatcher invokes this even for zones where it admits nothing,
// so the gauges stay fresh during periods of capacity exhaustion.
func publishGaugeSnapshot(ctx context.Context, q *Queue, zoneID uint32) {
	if size, err := q.QueueLen(ctx, zoneID); err == nil {
		queueSizeGauge.Set(float64(size), labelZone(zoneID))
	}
	if inflight, err := q.AdmittedCount(ctx, zoneID); err == nil {
		admittedInflightGauge.Set(float64(inflight), labelZone(zoneID))
	}
}
