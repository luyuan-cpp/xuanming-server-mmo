// Package metrics exposes Prometheus gauges for SceneManager's per-node load
// surface. The intent is to mirror the Redis-backed state (which is authoritative
// for scheduling) into Prometheus so operators can dashboard and alert without
// shelling into redis-cli.
//
// All gauges are labelled {node_id, zone_id, role}. role is the string form of
// the declared scene_node_type: "main_world", "instance", "main_world_cross",
// "instance_cross", or "unknown".
//
// Reset() is invoked when a node leaves the cluster so stale label sets do not
// linger forever.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/constants"
	"shared/safego"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/core/logx"
)

const subsystem = "scene_manager"

// safePointMetricsHTTP 是 /metrics + /debug HTTP 服务的 safego 点位名。
// 它会变成 safego_panic_total{point="..."} 的 label,必须是常量。
const safePointMetricsHTTP = "scene_manager.metrics_http"

var (
	playerCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "node_player_count",
		Help:      "Online players reported by SceneManager for each scene node.",
	}, []string{"node_id", "zone_id", "role"})

	sceneCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "node_scene_count",
		Help:      "Number of scene entities currently allocated to each scene node.",
	}, []string{"node_id", "zone_id", "role"})

	loadScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "node_load_score",
		Help:      "Composite load score used for scheduling (lower = more attractive).",
	}, []string{"node_id", "zone_id", "role"})

	nodesByRole = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "nodes_by_role",
		Help:      "Count of live scene nodes per zone and declared role.",
	}, []string{"zone_id", "role"})

	// isLeaderGauge:1 = 本副本是变更类后台循环的领导者,0 = 跟随者。
	// 多副本部署时全体副本之和应恒为 1;和为 0 的时间窗超过锁 TTL 说明
	// 选主卡住(Redis 不可达),需要告警。
	isLeaderGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "is_leader",
		Help:      "Whether this replica leads the mutating background loops (1 leader / 0 follower).",
	})

	rebalanceMigrationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "rebalance_migrations_total",
		Help:      "World channel migrations by outcome (migrated|failed) and reason.",
	}, []string{"zone_id", "reason", "outcome"})

	rebalancePending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "rebalance_pending",
		Help:      "Channels currently queued for migration, broken down by reason.",
	}, []string{"zone_id", "reason"})

	// mirrorColocateTotal counts mirror placement outcomes. "hit" = the
	// mirror was co-located with its source scene's node; "fallback" = the
	// source node wasn't viable (dead, over load cap, no mapping) and we
	// used GetBestNode instead. Dashboards should chart hit / (hit+fallback)
	// as the mirror co-location effectiveness rate.
	mirrorColocateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "mirror_colocate_total",
		Help:      "Mirror placement outcomes: hit (co-located) vs fallback (picked best node).",
	}, []string{"zone_id", "outcome", "reason"})

	// instanceDestroyedTotal breaks destroys down by kind (mirror vs
	// normal instance) and reason (idle auto-destroy, explicit RPC,
	// cascade from source destroy, node-death reconciliation). Operators
	// use this to sanity-check that instance lifecycles are short and
	// that mirrors are actually getting reclaimed by the shorter timeout.
	instanceDestroyedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "instance_destroyed_total",
		Help:      "Instance/mirror destroys by kind (instance|mirror) and reason (idle|explicit|cascade|node_death|source_migrated).",
	}, []string{"zone_id", "kind", "reason"})

	// enterSceneRejectedTotal 统计 EnterScene 在「归属交接 / 归属查询 / 跨 zone 传送目标地图 /
	// 进入途中场景被回收」这几类判定点上的拒绝,reason 取值见下面的 Help,各自的含义与基线见
	// internal/logic/enterscenelogic.go 文件头的 rejectReason* 常量说明(home_zone_* 两种在
	// internal/logic/home_zone.go)。
	//
	// 它**不是**"所有被拒的 EnterScene":场景解析失败(resolveSceneForEnter 出错)、再入屏障
	// 未到、Redis 读写失败、Kafka 路由失败等快速失败路径只回错误码,不记这条指标。
	// scene_gone = destroy-while-entering(AtomicIncrPlayerCountIfSceneExists 返回 <0,场景在解析
	// 之后、预占之前被回收;只有请求指定 scene_id 的进入走这条预占路径)。这个发射点曾在 a0152a5b8
	// 被删、d41219ca7 加回分支时漏带,2026-09-20 补回。
	//
	// 读数注意:handoff_pending_no_marker 在生产配置(AllowUnsafeCrossNodeHandoff=false)下是
	// 跨节点换图的常规第一跳,恒非 0,不能拿来告警;告警口径见 deploy/k8s/scene-manager-alerts.yaml。
	// zone_id 对 home_zone_unavailable / home_zone_unmapped_travel 是 gate zone(home_zone.go),
	// 对其余 reason 是目标 zone。
	enterSceneRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_rejected_total",
		// reason 取值必须与 enterscenelogic.go / home_zone.go 里实际传入的字面量一致;
		// 旧的 unsafe_handoff / handoff_pending 已无调用点(换手门拒绝细分成了
		// no_marker / stale_marker / withdrawn 三种),按旧名配的告警会恒为空而不报错。
		Help: "EnterScene rejections by reason (handoff_pending_no_marker|handoff_pending_stale_marker|handoff_pending_withdrawn|epoch_conflict|home_zone_unavailable|home_zone_unmapped_travel|travel_map_unavailable|pending_map_fallback|scene_gone).",
	}, []string{"zone_id", "reason"})

	// homeZoneLookupTotal 统计 EnterScene 里每一次归属 zone 查询的结果:
	//   mapped        data_service 给出了归属 zone
	//   unmapped      映射里没有这名玩家(首次落点 / 同 zone 换图按 gate zone 处理;跨 zone 传送的
	//                 两条腿拒绝,另记 enter_scene_rejected_total{reason="home_zone_unmapped_travel"})
	//   unconfigured  本进程没配 DataServiceRpc(按 gate zone 处理;多 zone 部署
	//                 里持续非 0 = 漏配,访客存盘会落错库)
	//   error         超时 / 不可用,请求被拒绝让上游重试
	// 稳态下 unmapped 应只在建号高峰出现,error 应恒 0。
	homeZoneLookupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "home_zone_lookup_total",
		Help:      "EnterScene home-zone lookups by outcome (mapped|unmapped|unconfigured|error).",
	}, []string{"zone_id", "outcome"})

	// sceneOrphansReconciledTotal tracks scenes destroyed by the
	// node-death reconciliation loop. A spike here correlates with a
	// node crash / rollout and should match (approximately) the count
	// of instances that were hosted on the departed node.
	sceneOrphansReconciledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "scene_orphans_reconciled_total",
		Help:      "Orphan scenes cleaned up after a node death, broken down by zone.",
	}, []string{"zone_id"})

	// mirrorSourceMissingTotal counts mirror create requests rejected
	// because their source_scene_id no longer exists. A nonzero rate
	// after a feature ships usually means the caller (gameplay code)
	// is racing the source scene's destroy path; consider holding a
	// soft "keep-alive" on the source while a mirror is in flight.
	mirrorSourceMissingTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "mirror_source_missing_total",
		Help:      "CreateScene mirror requests rejected because source_scene_id is gone.",
	}, []string{"zone_id"})

	// mirrorDedupTotal tracks the MirrorDedupBySource code path.
	//   outcome=hit   -> request returned an existing mirror
	//   outcome=miss  -> no mirror existed, fell through to fresh allocate
	//   outcome=stale -> mirrors set had a dangling id; cleaned + fell through
	// Only emitted when the config flag is on. Operators dashboard
	// hit-rate to confirm dedup is actually saving allocations.
	mirrorDedupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "mirror_dedup_total",
		Help:      "Mirror dedup attempts by outcome (hit|miss|stale). Only emitted when MirrorDedupBySource=true.",
	}, []string{"zone_id", "outcome"})

	// releasePlayerTotal counts cross-node ReleasePlayer notifications
	// dispatched to the previous scene node when a player switches
	// between scene nodes. outcome:
	//   ok        -> RPC succeeded on the first try
	//   retry_ok  -> RPC succeeded after one or more retries
	//   timeout   -> all attempts exceeded the per-call deadline
	//   error     -> dial / RPC failure on every attempt (non-timeout)
	// Operators dashboard error+timeout rates to spot scene nodes that
	// are dropping ReleasePlayer notifications and accumulating residual
	// player entities.
	releasePlayerTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "release_player_total",
		Help:      "Cross-node ReleasePlayer outcomes (ok|retry_ok|timeout|error).",
	}, []string{"zone_id", "outcome"})

	// enterSceneStageSeconds breaks the EnterScene RPC end-to-end latency
	// into deterministic sub-stages so we can see which Redis op / Kafka
	// send dominates under load. Round 15 (45k) saw the parent
	// `entergame_apply_enter_scene_seconds` avg climb 26.6ms → 35.6ms while
	// every other apply sub-stage (get_session / persist / bind_gate)
	// stayed flat sub-3ms. This split tells us whether the regression sits
	// in dedup / zone-resolve / scene-resolve / reserve / update-loc /
	// route-gate. stage values: see EnterSceneStage* constants below.
	enterSceneStageSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_stage_seconds",
		Help:      "EnterScene RPC sub-stage latency (zone_id, stage). Stage values: dedup|scene_resolve|reserve|update_loc|route_gate|release_dispatch|cross_zone.",
		Buckets:   []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"zone_id", "stage"})

	// ── Agones 高密度容量预占 ────────────────────────────────────────
	// 标签一律只用低基数维度。scene_id / player_id 绝不能进 label。
	agonesAllocationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_allocation_total",
		Help:      "GameServerAllocation attempts by outcome (ok|no_capacity|error|mapping_failed).",
	}, []string{"zone", "role", "outcome"})

	agonesAllocationLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "agones_allocation_latency_seconds",
		Help:      "GameServerAllocation round-trip latency.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"zone", "role"})

	agonesCounterRollbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_counter_rollback_total",
		Help:      "rooms counter rollback attempts by outcome (ok|failed). failed means the counter is now drifted and needs reconcile.",
	}, []string{"outcome"})

	agonesMappingFailureTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_mapping_failure_total",
		Help:      "Allocations whose PodIP could not be mapped to a registered scene node, by reason (no_pod_ip|unknown_pod_ip|zone_mismatch|role_mismatch).",
	}, []string{"zone", "reason"})

	agonesCounterDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "agones_counter_drift",
		Help:      "Per-zone count of GameServers whose Agones rooms counter disagrees with the Redis scene mapping.",
	}, []string{"zone"})

	worldAutoscaleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "world_autoscale_total",
		Help:      "World-channel autoscale actions by action (scale_out|scale_in) and outcome (ok|drained|error|max_reached).",
	}, []string{"zone_id", "action", "outcome"})

	// kafkaDeliveryTotal 只由 kafka.Writer.Completion 更新，不看
	// WriteMessages：Async 模式只有回调能拿到 broker 终态。failed 表示
	// kafka-go 已耗尽内部投递尝试且 RPC 调用方早已返回，告警必须盯此指标。
	kafkaDeliveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "kafka_delivery_total",
		Help:      "Terminal async Kafka message delivery outcomes (acked|failed).",
	}, []string{"outcome"})

	// enterSceneRollbackTotal 统计 EnterScene 推路由 / 重定向失败之后,把本次落点(location +
	// owner_epoch)精确值回滚的结果(internal/logic/owner_epoch.go rollbackPlayerPlacement):
	//   rolled_back          本次回滚成功
	//   already_rolled_back  go-redis 重发 EVAL 时首发其实已回滚(只对铸造过的落点识别)
	//   superseded           位置或 epoch 已被并发请求推进,什么都没改(偶发 = 顶号等并发)
	//   redis_error          go-redis 重试耗尽仍出错:Redis 里可能留着本次落点。跨 zone 第一条腿上
	//                        源 scene 会重置客户端(tip + 踢线 34);同 zone 路由失败上玩家会挂在哑连接上
	// 分母是「推路由 / 重定向失败」的次数,本身就该很少;redis_error 应恒 0,告警见
	// deploy/k8s/scene-manager-alerts.yaml SceneManagerEnterSceneRollbackRedisError。
	// 只按 outcome 分,不带 zone_id / player_id。
	enterSceneRollbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_rollback_total",
		Help:      "EnterScene placement rollbacks after a failed gate route/redirect push, by outcome (rolled_back|already_rolled_back|superseded|redis_error).",
	}, []string{"outcome"})

	// enterSceneMintReplayRecognizedTotal 统计铸造 owner_epoch 的 EVAL 被 go-redis 原样重发
	// (首发已生效、应答丢了)而被脚本认成「本请求已生效」的次数(只对凭 handoff 标记放行的铸造做识别,
	// 见 internal/logic/owner_epoch.go luaMintEpochAndSetLocation)。以前这种情况回 19,状态已推进
	// 却既不发重定向 / 路由也不回滚。应恒近 0;持续增长说明 zone Redis 读超时频繁。不带任何 label。
	enterSceneMintReplayRecognizedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_mint_replay_recognized_total",
		Help:      "owner_epoch mint EVALs replayed by the redis client after the first execution already applied, recognised as the request's own write (handoff-marker mints only).",
	})

	// reentryBarrierBlockedTotal 统计因「老属主节点刚判死、再入屏障未到」而被
	// 拒绝的改派/销毁/清理次数。site 是**代码里写死的常量点位名**
	// (resolve_scene|rebalance|reassign|world_channel_lazy|orphan_cleanup|
	// dead_node_cleanup|stale_location|dead_owner_takeover,定义在
	// internal/logic/reentry_barrier.go 的 barrierSite* 常量),
	// 绝不能拼进 scene_id / node_id 之类运行期值。
	//
	// 稳态应该恒 0;非 0 只在节点刚死后的一个屏障窗口内出现,持续非 0 说明
	// 有节点在反复丢租约,或者 death_at 标记被写坏了。
	reentryBarrierBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "reentry_barrier_blocked_total",
		Help:      "Ownership changes refused because the dead node's re-entry barrier had not elapsed, by site.",
	}, []string{"zone_id", "site"})

	// enterSceneOwnerDeadTakeoverTotal:玩家位置记录指向的节点已确认死亡且再入屏障已过,
	// EnterScene 把它当作无持有者直接落点(enterscenelogic.go playerLocationOwnerDead)。
	// 只按 zone 分;稳态恒 0,节点崩溃后约等于「当时挂在那个节点上、随后回来的玩家数」。
	// 若没有任何节点死亡它却在涨,说明判死(IsNodeAlive / death_at)出了问题。
	enterSceneOwnerDeadTakeoverTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_owner_dead_takeover_total",
		Help:      "EnterScene placements that treated a player's location as ownerless because its scene node was confirmed dead and the re-entry barrier had elapsed.",
	}, []string{"zone_id"})

	// deadNodeReconcilePending 是「已判死、但收尾动作还被屏障压着」的节点数。
	// 它应该在一个屏障时长内回到 0;长期不为 0 = 收尾链路卡住了。
	deadNodeReconcilePending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "dead_node_reconcile_pending",
		Help:      "Dead scene nodes whose orphan-scene reconciliation is deferred by the re-entry barrier.",
	}, []string{"zone_id"})

	registerOnce sync.Once
)

// EnterScene sub-stage labels. Keep these in sync with the call sites in
// internal/logic/enterscenelogic.go; new stages must be both added here and
// documented in enterSceneStageSeconds.Help.
const (
	EnterSceneStageDedup           = "dedup"
	EnterSceneStageSceneResolve    = "scene_resolve"
	EnterSceneStageReserve         = "reserve"
	EnterSceneStageUpdateLoc       = "update_loc"
	EnterSceneStageRouteGate       = "route_gate"
	EnterSceneStageReleaseDispatch = "release_dispatch"
	EnterSceneStageCrossZone       = "cross_zone"
)

func register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			playerCount, sceneCount, loadScore, nodesByRole,
			isLeaderGauge,
			rebalanceMigrationsTotal, rebalancePending,
			mirrorColocateTotal, instanceDestroyedTotal,
			enterSceneRejectedTotal, sceneOrphansReconciledTotal,
			mirrorSourceMissingTotal, mirrorDedupTotal,
			releasePlayerTotal, enterSceneStageSeconds,
			agonesAllocationTotal, agonesAllocationLatency,
			agonesCounterRollbackTotal, agonesMappingFailureTotal,
			agonesCounterDrift, worldAutoscaleTotal,
			kafkaDeliveryTotal, enterSceneRollbackTotal, enterSceneMintReplayRecognizedTotal,
			reentryBarrierBlockedTotal, deadNodeReconcilePending,
			enterSceneOwnerDeadTakeoverTotal,
			homeZoneLookupTotal,
		)
	})
}

// 归属 zone 查询结果标签取值,见 homeZoneLookupTotal 的注释。
const (
	HomeZoneLookupMapped       = "mapped"
	HomeZoneLookupUnmapped     = "unmapped"
	HomeZoneLookupUnconfigured = "unconfigured"
	HomeZoneLookupError        = "error"
)

// ObserveHomeZoneLookup 记一次 EnterScene 里的归属 zone 查询结果。
// zoneID 是请求的 gate zone(查询发生时目标 zone 已解析,但归属查询的语义是
// "这名玩家从哪个 zone 进来的",用 gate zone 更贴合排障时的提问)。
func ObserveHomeZoneLookup(zoneID uint32, outcome string) {
	register()
	homeZoneLookupTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome,
	).Inc()
}

// SetLeader 发布本副本的领导权状态。选主接线见 scene_manager_service.go,
// 领导权语义见 internal/logic/leader_gate.go。
func SetLeader(leading bool) {
	register()
	if leading {
		isLeaderGauge.Set(1)
	} else {
		isLeaderGauge.Set(0)
	}
}

// ResetLeaderGauges 清掉只有领导者才刷新的 gauge 序列。降级后不清的话,
// 该副本会永远导出最后一次在任时的旧值,sum()/max() 聚合读到脏数据。
// Reset 直接移除 child 序列(而不是置 0),对聚合最友好。
// 在失去领导权时调用(scene_manager_service.go 的 OnStateChange)。
func ResetLeaderGauges() {
	register()
	agonesCounterDrift.Reset()
	rebalancePending.Reset()
}

// ObserveEnterSceneOwnerDeadTakeover 记一次「属主节点已死、按无持有者落点」。
func ObserveEnterSceneOwnerDeadTakeover(zoneID uint32) {
	register()
	enterSceneOwnerDeadTakeoverTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10)).Inc()
}

// ObserveReentryBarrierBlocked 记一次被再入屏障挡下的所有权变更尝试。
// site 必须取常量点位名,见 reentryBarrierBlockedTotal 的注释。
func ObserveReentryBarrierBlocked(zoneID uint32, site string) {
	register()
	reentryBarrierBlockedTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), site).Inc()
}

// SetDeadNodeReconcilePending 发布某 zone 里被屏障推迟的死节点收尾任务数。
func SetDeadNodeReconcilePending(zoneID uint32, count int) {
	register()
	deadNodeReconcilePending.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10)).Set(float64(count))
}

// ObserveKafkaDelivery 记录异步 writer 回报的终态；count 是批内消息数，
// 零消息回调直接忽略。
func ObserveKafkaDelivery(outcome string, count int) {
	if count <= 0 {
		return
	}
	register()
	kafkaDeliveryTotal.WithLabelValues(outcome).Add(float64(count))
}

// ObserveEnterSceneRollback 记一次落点回滚结果。outcome 取值见 enterSceneRollbackTotal 的注释
// (常量在 internal/logic/owner_epoch.go 的 rollbackOutcome*)。
func ObserveEnterSceneRollback(outcome string) {
	register()
	enterSceneRollbackTotal.WithLabelValues(outcome).Inc()
}

// ObserveEnterSceneMintReplayRecognized 记一次铸造 EVAL 的重放识别命中,见 enterSceneMintReplayRecognizedTotal。
func ObserveEnterSceneMintReplayRecognized() {
	register()
	enterSceneMintReplayRecognizedTotal.Inc()
}

// ObserveWorldAutoscale 记一次大世界频道扩缩容动作。
// action: scale_out|scale_in;outcome: ok|drained|error|max_reached。
func ObserveWorldAutoscale(zoneID uint32, action, outcome string) {
	register()
	worldAutoscaleTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), action, outcome,
	).Inc()
}

// Agones allocation outcome 标签取值。
const (
	AgonesOutcomeOK            = "ok"
	AgonesOutcomeNoCapacity    = "no_capacity"
	AgonesOutcomeError         = "error"
	AgonesOutcomeMappingFailed = "mapping_failed"
)

// ObserveAgonesAllocation 记一次 GSA 结果及其耗时。
func ObserveAgonesAllocation(zone, role, outcome string, d time.Duration) {
	register()
	agonesAllocationTotal.WithLabelValues(zone, role, outcome).Inc()
	agonesAllocationLatency.WithLabelValues(zone, role).Observe(d.Seconds())
}

// ObserveAgonesCounterRollback 记一次 rooms 计数回滚结果。
// outcome=="failed" 表示计数已经漂移,需要 reconcile 才能发现,
// 应当配一条告警而不是只留日志。
func ObserveAgonesCounterRollback(outcome string) {
	register()
	agonesCounterRollbackTotal.WithLabelValues(outcome).Inc()
}

// ObserveAgonesMappingFailure 记一次 "GSA 成功但映射不回本地节点" 的失败。
func ObserveAgonesMappingFailure(zone, reason string) {
	register()
	agonesMappingFailureTotal.WithLabelValues(zone, reason).Inc()
}

// SetAgonesCounterDrift 设置某 zone 当前的计数漂移条数(reconcile 写入)。
func SetAgonesCounterDrift(zone string, count int) {
	register()
	agonesCounterDrift.WithLabelValues(zone).Set(float64(count))
}

// ObserveEnterSceneStage records one sub-stage latency for the EnterScene
// RPC. Use the EnterSceneStage* constants for the stage label. zoneID may
// be 0 when the stage runs before the zone is resolved (dedup).
func ObserveEnterSceneStage(zoneID uint32, stage string, d time.Duration) {
	register()
	enterSceneStageSeconds.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), stage,
	).Observe(d.Seconds())
}

// ObserveMirrorColocate records one mirror placement outcome. outcome is
// "hit" or "fallback". reason is only meaningful on fallbacks
// ("no_mapping" | "zone_mismatch" | "node_dead" | "overloaded", 即
// createscenelogic.go resolveMirrorSourceNode 的返回值); pass "ok"
// for hit rows so the label set stays well-formed.
func ObserveMirrorColocate(zoneID uint32, outcome, reason string) {
	register()
	mirrorColocateTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome, reason,
	).Inc()
}

// ObserveInstanceDestroyed records one scene destroy. kind is
// "instance" or "mirror"; reason is "idle" | "explicit" | "cascade" |
// "node_death" | "source_migrated". The last one fires when a world
// channel is migrated by the rebalancer and its co-located mirrors are
// proactively cleaned up so the old node can drain.
func ObserveInstanceDestroyed(zoneID uint32, kind, reason string) {
	register()
	instanceDestroyedTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), kind, reason,
	).Inc()
}

// ObserveEnterSceneRejected 记一次 EnterScene 拒绝。reason 必须是固定字面量(低基数),
// 现有取值与 enterSceneRejectedTotal 的 Help 一致:handoff_pending_no_marker /
// handoff_pending_stale_marker / handoff_pending_withdrawn / epoch_conflict /
// home_zone_unavailable / home_zone_unmapped_travel / travel_map_unavailable /
// pending_map_fallback / scene_gone
// (发射点:internal/logic/enterscenelogic.go、home_zone.go)。
// 其它快速失败路径(场景解析失败、再入屏障未到、Kafka 路由失败等)只回各自的错误码,不记本指标。
// 新增 reason 时同步改 Help 与 deploy/k8s/scene-manager-alerts.yaml 的分组说明。
func ObserveEnterSceneRejected(zoneID uint32, reason string) {
	register()
	enterSceneRejectedTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), reason,
	).Inc()
}

// ObserveSceneOrphansReconciled records the count of orphan scenes
// cleaned up in a single node-death reconciliation sweep. Zero-count
// sweeps are silently dropped so we don't fabricate a counter tick
// every time a healthy node exits.
func ObserveSceneOrphansReconciled(zoneID uint32, count int) {
	if count <= 0 {
		return
	}
	register()
	sceneOrphansReconciledTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10),
	).Add(float64(count))
}

// ObserveMirrorSourceMissing records one mirror create rejected because
// its source scene no longer exists. See ErrSourceSceneGone.
func ObserveMirrorSourceMissing(zoneID uint32) {
	register()
	mirrorSourceMissingTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10),
	).Inc()
}

// ObserveMirrorDedup records one outcome of the MirrorDedupBySource code
// path. outcome must be one of "hit" | "miss" | "stale".
func ObserveMirrorDedup(zoneID uint32, outcome string) {
	register()
	mirrorDedupTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome,
	).Inc()
}

// ObserveReleasePlayer records one outcome of a cross-node ReleasePlayer
// RPC dispatched by EnterScene. outcome must be one of
// "ok" | "retry_ok" | "timeout" | "error". A persistently nonzero
// timeout/error rate means the previous scene node is unreachable and
// some player entities will linger until AFK cleanup.
func ObserveReleasePlayer(zoneID uint32, outcome string) {
	register()
	releasePlayerTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome,
	).Inc()
}

// ObserveRebalanceMigration records one completed migration attempt.
// outcome is either "migrated" or "failed". reason echoes rebalanceReason
// ("node_gone" | "better_home").
func ObserveRebalanceMigration(zoneID uint32, reason, outcome string) {
	register()
	rebalanceMigrationsTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), reason, outcome,
	).Inc()
}

// SetRebalancePending publishes the queue depth per (zone, reason).
// Called by the rebalance planner so dashboards can distinguish
// transient spikes ("a world pod just restarted") from chronic drift
// ("budget too low; urgent queue keeps growing").
func SetRebalancePending(zoneID uint32, reason string, count int) {
	register()
	rebalancePending.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), reason,
	).Set(float64(count))
}

// RoleLabel returns the string label for a scene_node_type. Unknown values
// fall through to "unknown" so the metric stays well-formed even during
// misconfiguration (which will already have been logged by LoadReporter).
func RoleLabel(t uint32) string {
	switch t {
	case constants.SceneNodeTypeMainWorld:
		return "main_world"
	case constants.SceneNodeTypeInstance:
		return "instance"
	case constants.SceneNodeTypeMainWorldCross:
		return "main_world_cross"
	case constants.SceneNodeTypeInstanceCross:
		return "instance_cross"
	}
	return "unknown"
}

// ObserveNode updates the per-node gauges. Called by LoadReporter every
// scrape interval for every known node.
func ObserveNode(nodeID string, zoneID uint32, role uint32, sceneCnt, playerCnt int64, score float64) {
	register()
	zoneStr := strconv.FormatUint(uint64(zoneID), 10)
	roleStr := RoleLabel(role)
	playerCount.WithLabelValues(nodeID, zoneStr, roleStr).Set(float64(playerCnt))
	sceneCount.WithLabelValues(nodeID, zoneStr, roleStr).Set(float64(sceneCnt))
	loadScore.WithLabelValues(nodeID, zoneStr, roleStr).Set(score)
}

// ForgetNode drops per-node label sets when a node is removed. Keeping them
// around after removal would make rate()/increase() queries misleading and
// would leak memory in long-running SceneManagers that churn nodes.
func ForgetNode(nodeID string, zoneID uint32, role uint32) {
	register()
	zoneStr := strconv.FormatUint(uint64(zoneID), 10)
	roleStr := RoleLabel(role)
	playerCount.DeleteLabelValues(nodeID, zoneStr, roleStr)
	sceneCount.DeleteLabelValues(nodeID, zoneStr, roleStr)
	loadScore.DeleteLabelValues(nodeID, zoneStr, roleStr)
}

// SetNodesByRole publishes the live count of nodes per (zone, role). Called
// on the LoadReporter tick after all per-node observations are complete so
// the snapshot is internally consistent.
func SetNodesByRole(counts map[struct {
	ZoneID uint32
	Role   uint32
}]int) {
	register()
	nodesByRole.Reset()
	for k, v := range counts {
		nodesByRole.WithLabelValues(
			strconv.FormatUint(uint64(k.ZoneID), 10),
			RoleLabel(k.Role),
		).Set(float64(v))
	}
}

// debugMux holds handlers registered via RegisterDebugHandler. Mutating it
// after Start() returns is safe because the HTTP server reads it through
// the mux lock. Registration before Start is the expected pattern.
var (
	debugMuxOnce sync.Once
	debugMux     *http.ServeMux
)

func ensureDebugMux() *http.ServeMux {
	debugMuxOnce.Do(func() { debugMux = http.NewServeMux() })
	return debugMux
}

// RegisterDebugHandler attaches a handler under /debug/<path>. Use for
// operational introspection (rebalance plan, dead-node probe, etc). Do
// NOT use for business logic — these endpoints sit next to /metrics on
// an internal port and must never authenticate users.
//
// path should start with a leading "/"; the "/debug" prefix is added.
func RegisterDebugHandler(path string, handler http.Handler) {
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	ensureDebugMux().Handle("/debug"+path, handler)
}

// Start exposes /metrics over HTTP alongside any /debug/* handlers that
// were registered before Start returned. Failures log but do not block
// service startup — metrics are observability, not critical path.
// Empty addr is a no-op (disables the endpoint).
func Start(addr string) {
	if addr == "" {
		logx.Info("[metrics] MetricsListenAddr empty; Prometheus /metrics endpoint disabled")
		return
	}
	register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// Mount the debug sub-mux if any handlers registered. Using a dedicated
	// sub-mux means late registrations (rare, but possible in tests) still
	// route correctly without racing on mux setup.
	mux.Handle("/debug/", ensureDebugMux())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// safego.Go:观测端口炸掉不能连累业务进程。裸 go func 里一次 panic
	// (比如 debug handler 里的空指针)会直接打死 SceneManager —— 用观测代码
	// 换掉整个服务是最不划算的交易。
	safego.Go(safePointMetricsHTTP, func() {
		logx.Infof("[metrics] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[metrics] HTTP server exited: %v", err)
		}
	})
}
