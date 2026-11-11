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
//
// 2026-10-16 起每节点 gauge(node_player_count / node_scene_count / node_load_score)只由领导者导出:
// 负载分刷新只在领导者上做(internal/logic/load_refresh.go),跟随者每拍 ResetNodeSeries。切主时这些序列
// 从一个 pod 换到另一个 pod,中间最多空缺约一个锁 TTL 加一拍。nodes_by_role 由领导者发布全部 zone、
// 跟随者只发布本进程 ZoneId(见 SetNodesByRole)。
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

	// nodesByRole:补 0 规则与发布者语义见 SetNodesByRole 的注释(告警 SceneManager*PoolEmpty 依赖它)。
	nodesByRole = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "nodes_by_role",
		Help:      "Count of live scene nodes per zone and declared role. The leader publishes every zone in its etcd mirror; followers publish only their configured ZoneId. Every declared role of each published zone, plus this instance's configured ZoneId, is published (0 when empty).",
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
		Help:      "Instance/mirror destroys by kind (instance|mirror) and reason (idle|explicit|cascade|node_death|source_migrated|source_scaled_in).",
	}, []string{"zone_id", "kind", "reason"})

	// enterSceneRejectedTotal 统计 EnterScene 在「归属交接 / 归属查询 / 跨 zone 传送目标地图 /
	// 进入途中场景被回收」这几类判定点上的拒绝,reason 取值见下面的 Help,各自的含义与基线见
	// internal/logic/enterscenelogic.go 文件头的 rejectReason* 常量说明(home_zone_* 两种在
	// internal/logic/home_zone.go)。
	//
	// 它**不是**"所有被拒的 EnterScene":场景解析失败(resolveSceneForEnter 出错)、再入屏障
	// 未到、Redis 读写失败、Kafka 路由失败等快速失败路径只回错误码,不记这条指标。
	// scene_gone = destroy-while-entering(AtomicAddSceneMemberIfSceneExists 返回 <0,场景在解析
	// 之后、预占之前被回收;只有请求指定 scene_id 的进入走这条预占路径)。这个发射点曾在 a0152a5b8
	// 被删、d41219ca7 加回分支时漏带,2026-09-20 补回。(2026-10-12 历史改写后对应的新号是 0e26844b5
	// 与 f3f08e2ad;10-21 起旧号经 6c7f507cc 也作为祖先保留在 main 里,引用以新号为准。)
	//
	// 读数注意:handoff_pending_no_marker 在生产配置(AllowUnsafeCrossNodeHandoff=false)下是
	// 跨节点换图的常规第一跳,恒非 0,不能拿来告警;告警口径见 deploy/k8s/scene-manager-alerts.yaml。
	// zone_id 对 home_zone_unavailable / home_zone_unmapped_travel 是 gate zone
	// (home_zone.go),对其余 reason 是目标 zone。
	// channel_* 五种 = 玩家主动选线的预检拒绝(internal/logic/channel_pick.go,world-channel-switch.md §4.4),
	// 只有带 client_channel_pick 的请求会发出,zone_id 是目标 zone。它们是玩家操作触发的业务拒绝,
	// 有稳定的低速率属正常(channel_switch_cooldown / channel_full 尤其如此),不要按「> 0」告警。
	// session_stale / session_conflict = 落点会话身份裁决拒绝(私有码 25,scene-enter-chain-residuals.md §5.2):
	// 请求代表的登录已被 location 记的更新登录取代 / 代次相等而会话不同(违反 G2)。判定发生在目标 zone 解析
	// 完成之前,zone_id 取请求已知的目标 zone,未知时取 gate zone。session_stale 在顶号竞态下偶发属正常;
	// session_conflict 应恒 0。
	// epoch_conflict 除并发推进 epoch 之外,还包括落点 Lua 发现 location 原文在观察之后被改写或删除。
	enterSceneRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_rejected_total",
		// reason 取值必须与 enterscenelogic.go / home_zone.go / channel_pick.go / enter_hosted_placement.go 里实际传入的字面量一致;
		// 旧的 unsafe_handoff / handoff_pending 已无调用点(换手门拒绝细分成了
		// no_marker / stale_marker / withdrawn 三种),按旧名配的告警会恒为空而不报错。
		// hosted_placement_*(托管落点,docs/design/player-connection-lost-hold.md §4.2):malformed / cross_zone 按构造应恒 0;
		// no_instance 是本副本镜像里拿不到目标节点唯一实例号,节点刚起或镜像重同步时偶发,scene 侧下一次跟随检查会重试。
		Help: "EnterScene rejections by reason (handoff_pending_no_marker|handoff_pending_stale_marker|handoff_pending_withdrawn|epoch_conflict|home_zone_unavailable|home_zone_unmapped_travel|travel_map_unavailable|pending_map_fallback|scene_gone|channel_closing|channel_conf_mismatch|channel_switch_disabled|channel_full|channel_switch_cooldown|session_stale|session_conflict|hosted_placement_malformed|hosted_placement_cross_zone|hosted_placement_no_instance).",
	}, []string{"zone_id", "reason"})

	// placementIdentityTotal 统计落点会话身份的每一次裁决(docs/design/scene-enter-chain-residuals.md §3.3):
	//   site=enter  EnterScene 入口的 decidePlacementIdentity,verdict ∈ fresh|same_session|legacy|newer|stale|conflict;
	//               跨节点换图的第一跳拿 18 之后重发,同一次换图会裁决两次。
	//   site=leave  LeaveScene 的 decideLeaveIdentity,verdict ∈ legacy|keep_newer|keep_conflict|deleted。
	// 读数:
	//   - legacy 在全部组件升级之后应趋近 0,持续非 0 = 还有旧版 login / scene / scene_manager 副本在发无身份的请求;
	//   - leave/keep_newer 是 b1–b4 的合法拦截(迟到的 LeaveScene 遇到更新的登录),有低速率属正常;按**速率阈值**
	//     告警,不按出现即告警。速率异常升高说明「会话记录代次 ≥ 本会话写进 location 的代次」这个前提被打破
	//     (违反发布顺序、混版 player_locator 副本),location 可能永久泄漏(§3.8)。告警表达式取
	//     rate(scene_manager_placement_identity_total{site="leave",verdict="keep_newer"}[5m]) 超过基线阈值,
	//     规则落在 deploy/k8s/scene-manager-alerts.yaml(阈值需要上线后的基线,尚未配置);
	//   - keep_conflict / enter conflict 应恒 0,出现即 G2 被破坏,看同期的 Error 日志。
	// 只按 site 与 verdict 分,player_id / session_id / 代次一律只进日志。
	placementIdentityTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "placement_identity_total",
		Help:      "Placement session-identity verdicts by site (enter|leave) and verdict (enter: fresh|same_session|legacy|newer|stale|conflict; leave: legacy|keep_newer|keep_conflict|deleted).",
	}, []string{"site", "verdict"})

	// leaveHolderReleaseTotal 统计 LeaveScene 在离场身份判为可删之后、删 location 之前的「持有者已释放」判定
	// (docs/design/location-release-binding.md §3.2)。verdict 取 internal/logic/leave_release.go 的名表:
	//   released / owner_dead / no_holder        拿到释放证据,删 location
	//   legacy_epoch0 / disabled                 例外,按旧行为删(epoch 未铸造 / 功能关闭或 dev 旁路)
	//   pending / pending_unproven               没拿到证据,保留 location、回可重试错误;登出 / 断线后的正常短暂态
	//   forced                                   等满上限仍没拿到证据,按旧行为删除(接受回档风险)。应恒 0,出现即查
	//   revoked_unqualified                      pending 过了宽限,资格原件证明没有任何进程在当前代登记过:推进 owner_epoch 并删 location
	//                                            (§17,logic/unqualified_holder_revoke.go)。只在真正撤销时计;正常应接近 0,出现说明持有节点的
	//                                            入场登记没做成、location 卡过了宽限,按 [UnqualifiedRevoke] ERROR 日志逐条看
	// 只按 verdict 分,player_id / 节点号 / epoch 一律只进日志。
	leaveHolderReleaseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "leave_holder_release_total",
		Help:      "LeaveScene holder-release verdicts before deleting a player location (released|owner_dead|no_holder|legacy_epoch0|disabled|pending|pending_unproven|forced|revoked_unqualified).",
	}, []string{"verdict"})

	// deadHolderReleaseTotal 统计 LeaveScene 判出 owner_dead 之后、删 location 之前,释放判死持有者存档写权的结果
	// (logic/dead_holder_release.go,docs/design/player-save-direct-commit.md §5.5)。result 取下面的 DeadHolderRelease* 常量:
	//   released               比较并写成功,写权已释放
	//   no_row / newer_owner   没有东西要释放:没有写权行 / 代次已比 location 新(新持有者认领过了)
	//   holder_registered      持有实例在 etcd 在册,不释放。交接源仍在册时会长期停在这里(规格 §9 第 8 条已接受的残余),
	//                          持续增长就按 LeaveScene 的 Info 日志 holder_release_pending 逐条看
	//   row_changed            写权行在复核之后变了,不释放,下一次重试重读
	//   registry_unavailable   etcd 复核做不成,不释放
	//   error                  读写权行或比较并写失败(data_service 不可用等),持续非 0 要查
	// 除 released / no_row / newer_owner 外都保留 location,由 player_locator 重试。只按 result 分,player_id 只进日志。
	deadHolderReleaseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "dead_holder_release_total",
		Help:      "Player store claim releases for dead holders during LeaveScene, by result (released|no_row|newer_owner|holder_registered|row_changed|registry_unavailable|error).",
	}, []string{"result"})

	// enterHolderReleaseTotal 统计 EnterScene 同 zone 跨节点换手门以 18 拒绝之后,要不要替这次登录请持有节点释放
	// (docs/design/location-release-binding.md §12)。verdict 取 internal/logic/enter_holder_release.go 的名表:
	//   delivered / failed                        登录撞上未释放的持有者,已同步请它释放(持有节点应答 / RPC 失败或超时)
	//   not_login                                 scene 自发的请求(换图第一跳等),18 的常规来源,不请
	//   identity_not_newer                        登录请求但落点身份不是严格更新(同会话 / 任一侧无代次),只等不请
	//   legacy_epoch0 / holder_unaddressable      epoch 未铸造 / location 寻址不到唯一持有节点,不请
	//   chain_gone                                发起的登录链已结束(超时 / 丢锁),不请
	//   disabled                                  持有者释放协议关着(与 LeaveScene 同一开关)
	//   revoked_unqualified                       请过释放之后过了宽限,资格原件证明没有任何进程在当前代登记过:推进 owner_epoch 并删 location
	//                                             (§17;与同一次请求的 delivered / failed 各计一次)。含义同 LeaveScene 一侧
	// failed 持续非 0 = 持有节点不可达或过慢;只按 verdict 分,player_id / 节点号 / epoch 一律只进日志。
	enterHolderReleaseTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_holder_release_total",
		Help:      "EnterScene holder-release requests after a cross-node handoff-gate rejection (delivered|failed|not_login|identity_not_newer|legacy_epoch0|holder_unaddressable|chain_gone|disabled|revoked_unqualified).",
	}, []string{"verdict"})

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
		Help:      "EnterScene RPC sub-stage latency (zone_id, stage). Stage values: dedup|scene_resolve|reserve|update_loc|route_gate|release_dispatch|cross_zone|home_zone|role_lookup.",
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

	// 名字沿用 rooms 还是 Counter 时的叫法;2026-11-10 起 rooms 是按条目的 List(docs/design/agones-scene-node-high-density.md §13),
	// 一次归还是删一个条目,幂等,失败的可以重试,不再重试的由对账收(agonesRoomsReconciledTotal)。
	agonesCounterRollbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_counter_rollback_total",
		Help:      "Agones room releases (removing one entry from a GameServer's rooms list) by outcome (ok|failed). Releases are idempotent; a failed one is retried by its caller or reclaimed by the rooms reconciler.",
	}, []string{"outcome"})

	agonesMappingFailureTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_mapping_failure_total",
		Help:      "Allocations whose PodIP could not be mapped to a registered scene node, by reason (no_pod_ip|unknown_pod_ip|zone_mismatch|role_mismatch).",
	}, []string{"zone", "reason"})

	// agonesCounterDrift 是对账(agones_reconcile.go)一轮结束时仍没收敛的条目与场景数:读写失败跳过的、登记在
	// 别的 zone 的、条目格式认不出的、GameServer 满了补不上或转不过来的。只由本 zone 的领导者发布,降级的副本写回 0。
	// 持续大于 0 就是对账收不回来,见告警 SceneManagerAgonesRoomsNotConverging。名字沿用 rooms 还是 Counter 时的叫法。
	agonesCounterDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "agones_counter_drift",
		Help:      "Per-zone count of Agones room entries and scenes the rooms reconciler could not bring into agreement with the Redis room records in its last round (published by the zone leader; followers report 0).",
	}, []string{"zone"})

	// agonesRoomsReconciledTotal 是对账每次动手改一方的次数,action:
	//   orphan_removed        删掉没有记录、拆除任务、在途占用指向的条目
	//   hold_expired_removed  在途占用过期:同一步取消它,删掉条目
	//   hold_canceled         过期的在途占用,条目不在任何一台 GameServer 上:取消
	//   missing_added         记录指向的条目不在它的 GameServer 上:补加
	//   missing_no_capacity   同上,GameServer 满了补不上
	//   rehomed               场景在这个节点、名额记在别的 GameServer 上:转过来
	//   rehome_no_capacity    同上,这个节点的 GameServer 满了转不过来
	// 健康时只有零星的 hold_canceled / orphan_removed(进程在占名额之后崩溃),其余恒 0。
	agonesRoomsReconciledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "agones_rooms_reconciled_total",
		Help:      "Agones rooms reconciler fixes by action (orphan_removed|hold_expired_removed|hold_canceled|missing_added|missing_no_capacity|rehomed|rehome_no_capacity).",
	}, []string{"zone", "action"})

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

	// enterSceneRollbackTotal 统计 EnterScene 推路由 / 重定向失败之后,撤掉本次落点的结果
	// (internal/logic/owner_epoch.go rollbackPlayerPlacement;GO-2 根治后是单调回滚,
	// docs/design/cross-zone-scene-travel.md §12.8):
	//   rolled_back          本次回滚成功:location 已恢复;铸造过的 epoch 再前进一格(bump,凭标记时
	//                        同时写回执、转写标记),没铸造的 / 同节点 epoch 0 的铸造不动 epoch(keep)
	//   already_rolled_back  go-redis 重发 EVAL 时首发其实已回滚(只对 bump 识别)
	//   marker_gone          凭标记铸造,回滚时所凭标记已不在(目标节点 A2′ 已消费令牌,或源 scene 已取证 /
	//                        撤回):一个字节不动,保留本次落点。与 Kafka「报错但已投递」、源端提前取证的竞态
	//                        同频,偶发属正常;无基线,暂不配告警。分辨两种来源的方法见 runbook
	//   target_registered    不凭标记铸造(首次落点 / 第二条腿 / 死节点接管),回滚时交接资格原件已属于铸出的代次
	//                        (目标节点已登记,Kafka「报错但已投递」):一个字节不动,保留本次落点。偶发属正常,不告警
	//   fence_not_proof      不凭标记铸造,资格原件比铸出值新或写坏了(协议写者写不出,Redis 被回退 / 手工改过):
	//                        一个字节不动;应恒 0,与 redis_error 同一条告警
	//   superseded           位置或 epoch 已被并发请求推进,什么都没改(偶发 = 顶号等并发)
	//   redis_error          go-redis 重试耗尽仍出错(或返回值无法解读):Redis 里可能留着本次落点。跨 zone
	//                        第一条腿上源 scene 会重置客户端(tip + 踢线 34);同 zone 路由失败上玩家会挂在哑连接上
	//   plan_error           回滚计划构造失败(按构造不可达),没有回滚,终态同 redis_error;应恒 0,出现即代码缺陷
	// 分母是「推路由 / 重定向失败」的次数,本身就该很少;redis_error 应恒 0,告警见
	// deploy/k8s/scene-manager-alerts.yaml SceneManagerEnterSceneRollbackRedisError。
	// 只按 outcome 分,不带 zone_id / player_id。
	enterSceneRollbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_rollback_total",
		Help:      "EnterScene placement rollbacks after a failed gate route/redirect push, by outcome (rolled_back|already_rolled_back|marker_gone|target_registered|fence_not_proof|superseded|redis_error|plan_error). rolled_back = location restored; a minted owner_epoch moves forward one more step, never back.",
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

	// ownerEpochMintClockBehindTotal 统计墙钟地板没起作用的铸造(docs/design/zone-redis-regression-guard-20261022.md §4.3.1 G3a):
	// 铸造目标值 = max(观察值 + 1, 当前毫秒),当前毫秒 ≤ 观察值时只能取观察值 + 1。稳态下观察值是上一次铸造取的墙钟,
	// 所以它非 0 多半说明铸造的 scene_manager 副本之间、或与上一次铸造的副本之间有时钟偏差(或同一玩家在同一毫秒内连续换手;
	// 推路由失败回滚(铸出值 + 1)之后同一毫秒内的重试也会命中,属正常)。只对真的写进 Redis 的铸造计(logic changesceneutil.go
	// placePlayerLocationCtx)。持续增长时按 §14 第 13 条的主机时钟偏差监控排查。不带任何 label(不带 player_id)。
	ownerEpochMintClockBehindTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_mint_clock_behind_total",
		Help:      "owner_epoch mints whose wall-clock floor did not apply (current ms <= stored epoch), so the mint fell back to stored+1; a host clock skew signal.",
	})

	// ownerEpochMintDurableTotal / Seconds 是铸造之后 WAITAOF 落盘确认的结果与耗时(§4.3.2 G3b)。只对真的写进 Redis 的
	// 铸造计(首发生效或重放识别命中);CAS 落败、标记已撤回的铸造什么都没写,不计。两项份数都配 0(不发 WAITAOF)时也不计。
	//   confirmed    份数满足,随后才推路由 / 重定向
	//   not_durable  WAITAOF 正常返回但份数不足(含服务端等满超时):本次落点按「推送失败」单调回滚,客户端回可重试错误
	//   error        WAITAOF 被拒 / 回复形状不对 / 传输失败:同 not_durable 处置
	// 耗时是整条 pipeline(铸造 EVAL + WAITAOF)的往返,everysec 下预期均值约 0.5s、上界约 1s(未实测,以 §8.3 R-b 为准)。
	// 起服预置全部 result 为 0(告警的 increase() 看不到序列从「不存在」到 1 的第一次跳变)。
	ownerEpochMintDurableTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_mint_durable_total",
		Help:      "WAITAOF confirmations after an owner_epoch mint, by result (confirmed|not_durable|error). not_durable/error placements are rolled back as a failed route push.",
	}, []string{"result"})

	ownerEpochMintDurableSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_mint_durable_seconds",
		Help:      "Round trip of the owner_epoch mint pipeline (EVAL + WAITAOF on one connection), by result (confirmed|not_durable|error).",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2},
	}, []string{"result"})

	// ownerEpochRollbackReceiptTotal 是两段回执的结果(docs/design/zone-redis-regression-guard-20261022.md §4.3.4)。
	// site:
	//   rollback  凭标记的 bump 回滚之后的落盘确认(推路由失败或铸造没确认落盘,logic rollbackPlacementAfterPushFailure)
	//   enter     EnterScene 要下发一条带待确认回执的 location 记的 epoch 之前的确认(logic confirmPendingRollbackLocation)
	// result:
	//   confirmed   回滚(或观察到的待确认回滚)已确认落盘,回执已改成正式回执:源端可凭它采纳,epoch 可以下发
	//   pending     落盘没确认:回执留在待确认形态。rollback 处源端按"标记已发出"收口(踢线、不存盘销毁);enter 处回 7 不下发
	//   not_sent    确认一条都没发出去(剩余预算不够发 WAITAOF / 铸造通道取不到连接 / 没装铸造通道)。rollback 处回滚已经经共享
	//               连接池生效,回执留在待确认形态;enter 处一个字节都没写,回 3
	//   superseded  复核或改写时 epoch / location 已被并发推进,什么都没写(enter 处回 19)
	//   error       Redis 出错或回复无法解读:rollback 处回执停在待确认形态或回滚结果未知;enter 处回 ErrRedis
	// 健康时只有 rollback/confirmed 随 not_durable 与推路由失败一起增长;pending 持续增长 = 刷盘或副本持续落后;
	// rollback/not_sent 成批出现多半是 Kafka 写满超时、留给确认的预算不够。起服预置全部组合为 0。
	ownerEpochRollbackReceiptTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_epoch_rollback_receipt_total",
		Help:      "Two-phase rollback receipts after an unconfirmed owner_epoch mint, by site (rollback|enter) and result (confirmed|pending|not_sent|superseded|error).",
	}, []string{"site", "result"})

	// reentryBarrierBlockedTotal 统计因「老属主节点刚判死、再入屏障未到」而被
	// 拒绝的改派/销毁/清理次数。site 是**代码里写死的常量点位名**
	// (resolve_scene|rebalance|reassign|world_channel_lazy|orphan_cleanup|
	// dead_node_cleanup|autoscale_drain|stale_location|dead_owner_takeover|
	// replaced_owner_takeover|replaced_owner_leave_release,定义在
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

	// ownershipReadUnknownTotal:改派 / 销毁 / 清理 / 伸缩决策要用的 Redis 读出错(超时、断连、熔断),
	// 拿不到「有值」还是「确实不存在」,该路径不按读错的结果行事(2026-10-22)。绝大多数 site 是本轮放弃、
	// 什么都不写,等下一拍或上游重试;mirror_cascade 是例外:销毁已提交,退回提交前快照级联并保留索引。
	// 每个 site 的具体做法见 internal/logic/read_unknown.go 的 readUnknownSite* 常量注释。
	// go-zero 的 Get 把 redis.Nil 吞成 ("", nil),以前 `x, _ :=` 的写法会把这种「不知道」当成「不存在」,
	// 进而改写归属、删映射、不还 Agones 名额。
	// site 只取那些常量,绝不拼进 scene_id / node_id。
	// 稳态应接近 0;持续非 0 说明 Redis 在抖动,对应路径的自愈 / 回收会因此停住(方向安全,但要人看)。
	ownershipReadUnknownTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "ownership_read_unknown_total",
		Help:      "Redis reads needed for an ownership/destroy/cleanup/autoscale decision that failed (state unknown); the path did not act on the unknown value (mostly: round abandoned, nothing written; mirror_cascade: fell back to the pre-commit snapshot and kept the index), by site.",
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

	// enterSceneOwnerReplacedTakeoverTotal:节点号还在册,但在册的已是重启后复用该号的新进程,持有这名玩家的旧进程实例
	// 已从注册表消失且再入屏障已过,EnterScene 把位置当作无持有者落点(owner_replaced.go playerLocationOwnerReplaced,GO-3)。
	// 与上一条分开计,口径相同:稳态恒 0,scene 进程崩溃并以原号重启后约等于「当时挂在它上面、随后回来的玩家数」。
	enterSceneOwnerReplacedTakeoverTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_owner_replaced_takeover_total",
		Help:      "EnterScene placements that treated a player's location as ownerless because the process instance holding the player was gone from the registry (its node id reused by a new process) and the re-entry barrier had elapsed.",
	}, []string{"zone_id"})

	// enterSceneHandoffAdmittedTotal:凭源 scene 的落盘标记放行、且落点已提交的跨节点 / 跨 zone 交接(同 zone 换节点的落点,
	// 与跨 zone 第一条腿的等待落点)。它是 enter_scene_rejected_total{reason="handoff_pending_no_marker"} 比值告警的分母
	// (cross-zone-scene-travel.md §13.9「口径 B」):健康时每次放行之前约有一次 no_marker 拒绝(第一跳),只拒不放才是异常。
	// 告警阈值等首轮压测基线,本计数先落。
	enterSceneHandoffAdmittedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "enter_scene_handoff_admitted_total",
		Help:      "Cross-node / cross-zone handoffs admitted on the source scene's persisted handoff marker whose placement was committed.",
	}, []string{"zone_id"})

	// ownerReplacedRecheckTotal:GO-3 判「持有进程已被顶替」时向 etcd 做的线性一致复核(owner_replaced.go),按结果分。
	// result 取值见 OwnerReplacedRecheck* 常量。absent 在节点崩溃并以原号重启后约等于名下回来的玩家数;
	// present 非 0 = 本副本镜像滞后;error 持续非 0 = etcd 不可用,名下玩家一直回 17。
	ownerReplacedRecheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "owner_replaced_recheck_total",
		Help:      "etcd re-checks made while deciding whether a player's holding scene process was replaced by a new process reusing its node id, by result.",
	}, []string{"zone_id", "result"})

	// deadNodeReconcilePending 是本 zone 还没收完的死节点收尾任务数(Redis 任务表 scene_manager:dead_node_reconcile:zone:{z},
	// 含屏障未过的与有成员没结论、等下一拍重试的),只由本 zone 的领导者发布,降级的副本把自己发过的 zone 写回 0。
	// 它应该在一个屏障时长内回到 0;长期不为 0 = 收尾链路卡住了,看 dead_node_reconcile_total 的 retry / abandoned。
	deadNodeReconcilePending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "dead_node_reconcile_pending",
		Help:      "Dead scene nodes whose orphan-scene reconciliation has not finished yet (waiting for the re-entry barrier, or retrying scenes without a verdict). Published by the leader.",
	}, []string{"zone_id"})

	// deadNodeReconcileTotal:死节点收尾任务每一轮的结局(internal/logic/reentry_barrier.go「被屏障推迟的死节点收尾」)。
	// outcome 取 DeadNodeReconcileOutcome* 常量:
	//   completed      收尾范围已空,计数已删,任务出队(正常)
	//   retry          这一轮有场景没结论(Redis 出错),任务留着,下一拍只重试剩下的
	//   abandoned      本进程从第一次没做完起满 10 分钟仍做不完,放弃:计数与任务删掉,没结论的场景留在反向索引里
	//   instance_back  判死的那一代进程(同一个 node_uuid)已按原 node_id 重新注册:它名下的实例都还活着,任务撤掉,
	//                  一个场景都不拆、计数不删(节点失租后重新拿租约,etcd_service.cpp OnKeepAliveResponse)
	// retry / abandoned 健康时恒 0;instance_back 只在节点失租又回来时出现。只有本 zone 的领导者会发出。
	deadNodeReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "dead_node_reconcile_total",
		Help:      "Dead scene node reconcile passes by outcome (completed|retry|abandoned|instance_back).",
	}, []string{"zone_id", "outcome"})

	// sceneMemberReconcileMembersTotal:成员集合对账读过的成员(internal/logic/scene_member_reconcile.go),按结局分。
	// outcome 取 SceneMemberReconcile* 常量:
	//   ok         location 指向这里,成员身份成立
	//   removed    location 不指向这里,已摘除(含摘除后复核又加回去的,那是对账与进场恰好交错)
	//   skipped    location 写坏了,或还在宽限内(可能是在途预占),这一轮不动它
	//   malformed  成员不是合法的 player_id,已摘除
	//   error      读 location 或摘除失败,下一遍再看
	// removed 长期不为 0 说明 scene_manager 经常在进场中途崩溃,或有摘成员的路径漏了。只有领导者会发出。
	sceneMemberReconcileMembersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "scene_member_reconcile_members_total",
		Help:      "Members checked by the scene member reconcile against player location, by outcome (ok|removed|skipped|malformed|error).",
	}, []string{"outcome"})

	// sceneMemberReconcilePassesTotal:走完一遍全部成员集合的次数。它长时间不涨说明预算跟不上成员总量,或对账循环停了。
	sceneMemberReconcilePassesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "scene_member_reconcile_passes_total",
		Help:      "Completed passes of the scene member reconcile over all live member sets.",
	})

	// nodeDetachDeferredTotal:已判死的 scene 节点因为 death_at 写不进、或摘负载集(ZREM)失败,
	// 被**留在负载集里**推迟摘除(GO-6,internal/logic/reentry_barrier.go 的「推迟的摘除」一段)。
	// outcome 取 NodeDetachOutcome* 常量:
	//   deferred   进入推迟队列(每个节点每次死亡只记一次)
	//   recovered  补写 death_at 成功后摘除(正常自愈)
	//   expired    自首次尝试起满一个再入屏障仍写不进 death_at,不带标记摘除(安全,但说明 Redis 在拒 SET)
	//   abandoned  自首次尝试起满 10 分钟仍摘不出负载集,放弃;死节点留在负载集里,等下一次 fullSync 重扫
	//   dropped    推迟期间节点重新注册,或本副本失去领导权,任务丢弃
	// 健康时恒 0;告警见 deploy/k8s/scene-manager-alerts.yaml SceneManagerNodeDetachWithoutDeathMark。
	nodeDetachDeferredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "node_detach_deferred_total",
		Help:      "Dead scene nodes kept in the load set because writing death_at or the load-set ZREM failed, by outcome (deferred|recovered|expired|abandoned|dropped).",
	}, []string{"zone_id", "outcome"})

	// worldChannelDirectoryPublishTotal:分线目录发布循环(internal/logic/world_channel_directory.go,
	// world-channel-switch.md §4.3)对**每张大世界地图、每一轮**的结果。只有领导者会发出。
	// outcome 取 ChannelDirectoryPublish* 常量:
	//   ok     目录已写进 Redis
	//   empty  这张图在该 zone 一条在役 / 回收中的线都没有,本轮不发布(旧目录自然过期)
	//   error  读线集合 / 分配线号 / 读节点映射 / 写目录失败,本轮不发布
	// 读数:ok 的速率 ≈ 地图数 ÷ 发布周期;ok 掉到 0 而领导者在位 = 发布停摆,客户端的线路列表
	// 会在一个目录 TTL 后变成「暂不可用」。error 持续非 0 多半是 Redis 故障。
	// 只按 zone 与 outcome 分;地图 id、scene_id、线号都不进 label。
	worldChannelDirectoryPublishTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "world_channel_directory_publish_total",
		Help:      "World channel directory publish attempts (one per world map per round) by outcome (ok|empty|error).",
	}, []string{"zone_id", "outcome"})

	// loadRefreshTotal:领导者负载分刷新(internal/logic/load_refresh.go)每拍一次的结果,outcome 取
	// LoadRefreshOutcome* 常量:
	//   ok               全部节点读写成功
	//   partial          有节点计数读不到,但只是个别键的问题(逐条回复错误如 WRONGTYPE、值是空串或非数字):
	//                    在负载集里的保留旧分,不在的以哨兵分加入,其余照常写;不论批多大、节点多少都记这个。
	//                    读失败的节点数见 load_refresh_node_errors_total
	//   read_failed      至少一批读遇到传输层失败(熔断拒绝 / 取连接 / 网络 / 超时),受影响的节点本拍不更新分数。
	//                    只按错误类型判,与批怎么切无关(internal/logic/load_refresh.go pipelineHitTransportError)
	//   write_failed     写批(类型镜像 / ZADD)有命令失败,或整批没执行
	//   lost_leadership  刷新途中失去领导权,剩余写入放弃(新领导者下一拍接上)
	// 同时出现多种时按 write_failed > read_failed > lost_leadership > partial 取一个。
	// (registry_failed 2026-11-05 随 zone 登记集合删除,docs/design/control-plane-zone-scoping-20261105.md §4.3。)
	// 只有领导者发出;watch PUT 触发的单节点刷新不计入。所有 outcome 在注册时预置为 0:increase() 看不到
	// 序列从「不存在」到 1 的第一次跳变,「刷新停摆」告警也要靠预置的 0 序列区分「没人在刷」与「指标不存在」。
	// 告警见 deploy/k8s/scene-manager-alerts.yaml SceneManagerLoadRefreshStalled / Failing / Slow / NodeErrors。
	// 只按 outcome 分,不带 zone_id / node_id。
	loadRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "load_refresh_total",
		Help:      "Leader load-score refresh ticks by outcome (ok|partial|read_failed|write_failed|lost_leadership).",
	}, []string{"outcome"})

	// loadRefreshSeconds:一拍领导者刷新的墙钟耗时(登记 SADD + 分批 pipeline 读 + 分批 pipeline 写)。
	// 不带 label。拍长是 5s,接近它说明刷新开始挤占 load reporter 的 goroutine(watch / 推迟摘除同在其上)。
	loadRefreshSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "load_refresh_duration_seconds",
		Help:      "Wall time of one leader load-score refresh tick (zone registry SADD + pipelined counter reads + pipelined writes).",
		Buckets:   []float64{.01, .05, .1, .25, .5, 1, 2, 5},
	})

	// loadRefreshNodeErrorsTotal:每拍刷新里计数读失败的节点数之和(不带 label,不按节点分)。
	// 读不到的节点不更新每节点 gauge(停在上一次成功的值),不在负载集里的还一直挂着哨兵分、选点排最后;
	// 个别键坏掉只记 outcome=partial,所以「持续读不到」靠告警 SceneManagerLoadRefreshNodeErrors 盯本计数器。
	loadRefreshNodeErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "load_refresh_node_errors_total",
		Help:      "Scene nodes whose load counters could not be read during a leader load refresh (score kept, or added with the unknown-load sentinel).",
	})
	// mysteryMerchantScheduleTotal:神秘商人调度循环(internal/logic/mystery_merchant.go,
	// mystery-merchant.md §4)对**每个 zone、每一拍**的结果。只有领导者会发出。
	// outcome 取 MerchantSchedule* 常量:
	//   created        进入新一轮,已挑线、掷货架并写入记录(每轮一次)
	//   relocated      同一轮内商人所在的线被回收 / 销毁,已换到另一条线
	//   kept           本轮已排好且线还在,什么都没写(稳态下绝大多数拍是它)
	//   idle           本轮的停留期已过,等下一轮
	//   no_channel     没有任何可落脚的线(目录未发布 / 线全在回收或不可用),下一拍再试
	//   lost_race      别的副本已写下同一版本的记录(领导权交接窗口),本拍让出
	//   invalid_table  MysteryMerchantRule / Goods / Spot 表校验不过,本拍不调度
	//   error          读写 Redis 失败,本拍不调度
	// 读数:created 每个 zone 每个间隔应当恰好 +1;一个间隔过去而 created 没动 = 这一轮没有商人,
	// 此时看 no_channel / invalid_table / error 哪个在涨。只按 zone 与 outcome 分,轮次号、scene_id 都不进 label。
	mysteryMerchantScheduleTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "mystery_merchant_schedule_total",
		Help:      "Mystery merchant scheduling ticks by outcome (created|relocated|kept|idle|no_channel|lost_race|invalid_table|error).",
	}, []string{"zone_id", "outcome"})

	// sceneRegistrationTotal:新场景登记脚本(internal/logic/scene_registration.go)的结局。
	// kind 取 SceneRegistrationKind* 常量;outcome:
	//   registered  全部登记写入已落地(稳态下几乎只有它)
	//   conflict    scene_id 已被占用(或登记落地后场景已被销毁或改派),这一次一个键都没写,名额不还 —— 任何一次都该查发号器
	//   aborted     EVAL 回错,用同一 token 作废成功:登记没落地,预占的 Agones 名额已归还
	//   unknown     作废也失败,结局未知:名额没还(可能多占一份),看 agones_counter_drift
	sceneRegistrationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "scene_registration_total",
		Help:      "New scene registration script outcomes by kind (instance|world_channel) and outcome (registered|conflict|aborted|fenced|unknown). fenced: the Agones room hold expired and was reclaimed before the registration ran.",
	}, []string{"zone_id", "kind", "outcome"})

	// sceneTeardownJobsTotal:拆除任务(internal/logic/scene_teardown_job.go)的结局。
	//   completed  认领方当场做完、删掉了任务
	//   recovered  认领方没做完,扫描接手做完 —— 进程退出、回包丢失、某一步失败过的次数
	//   deferred   这一次有步骤没做成(或删任务结局未知),任务留给扫描;同一任务每重试失败一次记一次
	// kind 是 instance | world_drain | world_orphan | rollback。
	sceneTeardownJobsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "scene_teardown_jobs_total",
		Help:      "Scene teardown jobs by kind (instance|world_drain|world_orphan|rollback) and outcome (completed|recovered|deferred).",
	}, []string{"zone_id", "kind", "outcome"})

	// sceneTeardownOldestPendingSeconds:本 zone 最老一条还没做完的拆除任务距认领的秒数,没有待办为 0。
	// 只有领导者的扫描循环发布。持续高于几个租期(90s)说明某一步一直做不成,见告警 SceneManagerSceneTeardownStuck。
	sceneTeardownOldestPendingSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: subsystem,
		Name:      "scene_teardown_oldest_pending_seconds",
		Help:      "Age in seconds of the oldest unfinished scene teardown job in the zone (0 when none); published by the leader's sweep.",
	}, []string{"zone_id"})

	// gateRedirectExcludedTotal / gateRedirectFallbackTotal:跨 zone 重定向挑 gate 时按 shared/gatepick.Eligible 分档
	// 剔除的 gate 与回落(internal/logic/gate_redirect.go eligibleTargets;2026-11-05 起,11-06 排空并入分档)。zone_id 是目标 zone;
	// reason 只取 gatepick.Reason* 常量:excluded 取 stale / draining / full,fallback 取 stale / draining / full / mixed。
	// excluded 只计结果里真被剔掉的:回落时被用上的那一档不计。excluded{reason="stale"} 持续增长说明目标 zone 有 gate 卡住
	// (按 ERROR 日志里的 node_id 去查);fallback 任何一次增长都值得告警:stale = 全部停写,票多半落在卡住的 gate 上;
	// draining / full / mixed = 没有完全合格的 gate,在连得上的里挑(整区标了排空、或准入放进来的人超过 gate 上限之和)。
	gateRedirectExcludedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gate_redirect_excluded_total",
		Help:      "Gate candidates left out of a cross-zone redirect pick, by target zone and reason: 'stale' = the gate's etcd record kept the same ModRevision for three report periods as seen by this process (likely hung while its lease lives on); 'draining' = marked draining for a scale-in; 'full' = player_count at 90% of the gate's self-reported max_connections. Gates used by a fallback tier are not counted. Counted once per redirect per excluded gate.",
	}, []string{"zone_id", "reason"})
	gateRedirectFallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gate_redirect_fallback_total",
		Help:      "Cross-zone redirects where no gate of the target zone was fully eligible, by target zone and reason: 'stale' = every gate is stale, so the pick fell back to all of them; 'draining' / 'full' / 'mixed' = the pick used the reachable gates, all of which are draining / at 90% of max_connections / either. Any increase is worth an alert.",
	}, []string{"zone_id", "reason"})

	// foreignZoneRegistryReadsTotal:判断别的 zone 的场景节点时对那个 zone 的 etcd 注册表的直读
	// (internal/logic/foreign_zone_registry.go,docs/design/control-plane-zone-scoping-20261105.md §3.5)。
	// zone_id 是被读的 zone;outcome 取 ForeignZoneRegistryRead* 常量。error 持续增长说明 etcd 不可达或那个 zone 有坏注册,
	// 位置落在那个 zone 的玩家会一直被可重试地拒绝。
	foreignZoneRegistryReadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "foreign_zone_registry_reads_total",
		Help:      "Direct etcd reads of another zone's scene node registry, used to judge holders of locations in that zone, by zone and outcome (ok|error).",
	}, []string{"zone_id", "outcome"})

	// foreignZoneSkipsTotal:数据面上遇到别的 zone 的控制域写入、按不变量跳过的次数(§4.3)。site 取 ForeignZoneSkipSite* 常量。
	// 跳过的成员由那个 zone 的成员对账收敛;本计数只用来看这类请求有多频繁,不是故障信号。
	foreignZoneSkipsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "foreign_zone_skips_total",
		Help:      "Control-domain writes for another zone skipped by this scene_manager (that zone's own reconciler converges them), by site.",
	}, []string{"site"})

	registerOnce sync.Once
)

// 别的 zone 注册表直读的结局,见 foreignZoneRegistryReadsTotal。
const (
	ForeignZoneRegistryReadOK    = "ok"
	ForeignZoneRegistryReadError = "error"
)

// 按不变量跳过别的 zone 控制域写入的点位,见 foreignZoneSkipsTotal。
const (
	ForeignZoneSkipSiteMemberAdd    = "member_add"
	ForeignZoneSkipSiteMemberRemove = "member_remove"
	ForeignZoneSkipSiteScenePlayer  = "scene_player_remove"
)

// ObserveForeignZoneRegistryRead 记一次别的 zone 注册表直读,outcome 取 ForeignZoneRegistryRead* 常量。
func ObserveForeignZoneRegistryRead(zoneID uint32, outcome string) {
	register()
	foreignZoneRegistryReadsTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), outcome).Inc()
}

// ObserveForeignZoneSkip 记一次按不变量跳过的别 zone 控制域写入,site 取 ForeignZoneSkipSite* 常量。
func ObserveForeignZoneSkip(site string) {
	register()
	foreignZoneSkipsTotal.WithLabelValues(site).Inc()
}

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
	// EnterSceneStageHomeZone 只包住 resolveHomeZone 里实际发出的 data_service GetPlayerHomeZone(没配 DataServiceRpc 的
	// 旁路不计),zone_id 取 gate zone(同 home_zone_lookup_total)。
	EnterSceneStageHomeZone = "home_zone"
	// EnterSceneStageRoleLookup 只包住 lookupCurrentRole 里的 data_service LookupAccountRole(全局 TiDB 点查)。
	// lookupCurrentRole 在 zone 解析之前跑、签名里没有 zone,zone_id 恒为 "0";LeaveScene 走同一函数,样本数 = EnterScene + LeaveScene
	// 的调用次数 —— 压测要的是这条依赖的单次耗时与总调用量,两者都正好。
	EnterSceneStageRoleLookup = "role_lookup"
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
			agonesCounterDrift, agonesRoomsReconciledTotal, worldAutoscaleTotal,
			kafkaDeliveryTotal, enterSceneRollbackTotal, enterSceneMintReplayRecognizedTotal,
			ownerEpochMintClockBehindTotal, ownerEpochMintDurableTotal, ownerEpochMintDurableSeconds,
			ownerEpochRollbackReceiptTotal,
			reentryBarrierBlockedTotal, deadNodeReconcilePending, deadNodeReconcileTotal,
			ownershipReadUnknownTotal,
			enterSceneOwnerDeadTakeoverTotal, enterSceneOwnerReplacedTakeoverTotal, enterSceneHandoffAdmittedTotal,
			ownerReplacedRecheckTotal,
			homeZoneLookupTotal,
			nodeDetachDeferredTotal,
			worldChannelDirectoryPublishTotal,
			placementIdentityTotal,
			leaveHolderReleaseTotal,
			deadHolderReleaseTotal,
			enterHolderReleaseTotal,
			loadRefreshTotal, loadRefreshSeconds, loadRefreshNodeErrorsTotal,
			mysteryMerchantScheduleTotal,
			sceneMemberReconcileMembersTotal, sceneMemberReconcilePassesTotal,
			sceneRegistrationTotal,
			sceneTeardownJobsTotal, sceneTeardownOldestPendingSeconds,
			nodeViewHolding, nodeViewReleasedTotal, // node_view.go
			etcdRevisionRegressionsTotal,           // node_holder.go
			gateRedirectExcludedTotal, gateRedirectFallbackTotal,
			foreignZoneRegistryReadsTotal, foreignZoneSkipsTotal,
		)
		for _, outcome := range loadRefreshOutcomes {
			loadRefreshTotal.WithLabelValues(outcome).Add(0)
		}
		for _, outcome := range sceneMemberReconcileOutcomes {
			sceneMemberReconcileMembersTotal.WithLabelValues(outcome).Add(0)
		}
		for _, result := range ownerEpochMintDurableResults {
			ownerEpochMintDurableTotal.WithLabelValues(result).Add(0)
			ownerEpochMintDurableSeconds.WithLabelValues(result)
		}
		for _, site := range ownerEpochRollbackReceiptSites {
			for _, result := range ownerEpochRollbackReceiptResults {
				ownerEpochRollbackReceiptTotal.WithLabelValues(site, result).Add(0)
			}
		}
		for _, result := range deadHolderReleaseResults {
			deadHolderReleaseTotal.WithLabelValues(result).Add(0)
		}
	})
}

// owner_epoch_mint_durable_total / _seconds 的 result 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	OwnerEpochMintDurableConfirmed  = "confirmed"
	OwnerEpochMintDurableNotDurable = "not_durable"
	OwnerEpochMintDurableError      = "error"
)

// ownerEpochMintDurableResults 是注册时预置为 0 的全部 result,新增取值时同步加在这里。
var ownerEpochMintDurableResults = [...]string{
	OwnerEpochMintDurableConfirmed,
	OwnerEpochMintDurableNotDurable,
	OwnerEpochMintDurableError,
}

// ObserveOwnerEpochMintClockBehind 记一次墙钟地板没起作用的铸造,见 ownerEpochMintClockBehindTotal。
func ObserveOwnerEpochMintClockBehind() {
	register()
	ownerEpochMintClockBehindTotal.Inc()
}

// ObserveOwnerEpochMintDurable 记一次铸造之后的落盘确认结果(result 取 OwnerEpochMintDurable*)与 pipeline 往返耗时。
func ObserveOwnerEpochMintDurable(result string, d time.Duration) {
	register()
	ownerEpochMintDurableTotal.WithLabelValues(result).Inc()
	ownerEpochMintDurableSeconds.WithLabelValues(result).Observe(d.Seconds())
}

// owner_epoch_rollback_receipt_total 的 site / result 标签取值(含义见 ownerEpochRollbackReceiptTotal)。**必须是常量**。
const (
	OwnerEpochRollbackReceiptSiteRollback = "rollback"
	OwnerEpochRollbackReceiptSiteEnter    = "enter"

	OwnerEpochRollbackReceiptConfirmed  = "confirmed"
	OwnerEpochRollbackReceiptPending    = "pending"
	OwnerEpochRollbackReceiptNotSent    = "not_sent"
	OwnerEpochRollbackReceiptSuperseded = "superseded"
	OwnerEpochRollbackReceiptError      = "error"
)

// ownerEpochRollbackReceiptSites / Results 是注册时预置为 0 的全部组合,新增取值时同步加在这里。
var (
	ownerEpochRollbackReceiptSites = [...]string{
		OwnerEpochRollbackReceiptSiteRollback,
		OwnerEpochRollbackReceiptSiteEnter,
	}
	ownerEpochRollbackReceiptResults = [...]string{
		OwnerEpochRollbackReceiptConfirmed,
		OwnerEpochRollbackReceiptPending,
		OwnerEpochRollbackReceiptNotSent,
		OwnerEpochRollbackReceiptSuperseded,
		OwnerEpochRollbackReceiptError,
	}
)

// ObserveOwnerEpochRollbackReceipt 记一次两段回执的结果(site 取 OwnerEpochRollbackReceiptSite*,result 取
// OwnerEpochRollbackReceipt* 的其余常量)。
func ObserveOwnerEpochRollbackReceipt(site, result string) {
	register()
	ownerEpochRollbackReceiptTotal.WithLabelValues(site, result).Inc()
}

// load_refresh_total 的 outcome 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	LoadRefreshOutcomeOK             = "ok"
	LoadRefreshOutcomePartial        = "partial"
	LoadRefreshOutcomeReadFailed     = "read_failed"
	LoadRefreshOutcomeWriteFailed    = "write_failed"
	LoadRefreshOutcomeLostLeadership = "lost_leadership"
)

// loadRefreshOutcomes 是注册时预置为 0 的全部 outcome,新增 outcome 时同步加在这里。
var loadRefreshOutcomes = [...]string{
	LoadRefreshOutcomeOK,
	LoadRefreshOutcomePartial,
	LoadRefreshOutcomeReadFailed,
	LoadRefreshOutcomeWriteFailed,
	LoadRefreshOutcomeLostLeadership,
}

// ObserveLoadRefresh 记一拍领导者负载分刷新:结果(outcome 取 LoadRefreshOutcome*)、耗时与读失败的节点数。
func ObserveLoadRefresh(outcome string, d time.Duration, nodeErrors int) {
	register()
	loadRefreshTotal.WithLabelValues(outcome).Inc()
	loadRefreshSeconds.Observe(d.Seconds())
	if nodeErrors > 0 {
		loadRefreshNodeErrorsTotal.Add(float64(nodeErrors))
	}
}

// ResetNodeSeries 清掉全部每节点 gauge 序列(node_player_count / node_scene_count / node_load_score)。
// 这三个只由领导者导出:跟随者每拍调一次(internal/logic/load_reporter.go refreshLoadScores),降级时
// ResetLeaderGauges 也调。两处都调,是因为 OnStateChange 与 load reporter 的刷新跑在不同 goroutine 上,
// 降级那一刻正在跑的刷新可能在 Reset 之后又写进一批,由跟随者的下一拍清掉。
func ResetNodeSeries() {
	register()
	playerCount.Reset()
	sceneCount.Reset()
	loadScore.Reset()
}

// world_channel_directory_publish_total 的 outcome 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	ChannelDirectoryPublishOK    = "ok"
	ChannelDirectoryPublishEmpty = "empty"
	ChannelDirectoryPublishError = "error"
)

// ObserveWorldChannelDirectoryPublish 记一次「对一张图发布分线目录」的结果,outcome 取
// ChannelDirectoryPublish*,含义见 worldChannelDirectoryPublishTotal 的注释。
func ObserveWorldChannelDirectoryPublish(zoneID uint32, outcome string) {
	register()
	worldChannelDirectoryPublishTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome,
	).Inc()
}

// mystery_merchant_schedule_total 的 outcome 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	MerchantScheduleCreated      = "created"
	MerchantScheduleRelocated    = "relocated"
	MerchantScheduleKept         = "kept"
	MerchantScheduleIdle         = "idle"
	MerchantScheduleNoChannel    = "no_channel"
	MerchantScheduleLostRace     = "lost_race"
	MerchantScheduleInvalidTable = "invalid_table"
	MerchantScheduleError        = "error"
)

// ObserveMysteryMerchantSchedule 记一拍神秘商人调度的结果,outcome 取 MerchantSchedule*,
// 含义见 mysteryMerchantScheduleTotal 的注释。
func ObserveMysteryMerchantSchedule(zoneID uint32, outcome string) {
	register()
	mysteryMerchantScheduleTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), outcome,
	).Inc()
}

// node_detach_deferred_total 的 outcome 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	NodeDetachOutcomeDeferred  = "deferred"
	NodeDetachOutcomeRecovered = "recovered"
	NodeDetachOutcomeExpired   = "expired"
	NodeDetachOutcomeAbandoned = "abandoned"
	NodeDetachOutcomeDropped   = "dropped"
)

// ObserveNodeDetachDeferred 记一次推迟摘除(GO-6)的状态变化,outcome 取 NodeDetachOutcome*。
//
// 进入推迟(deferred)时先给同一 zone 的 expired / abandoned 预建值为 0 的序列:告警用
// increase(...[10m]) > 0,而 increase() 看不到序列从「不存在」到 1 的第一次跳变 —— 不预建的话,
// 进程生命周期里第一次 expired / abandoned 会被静默漏掉。残余:抓取间隔 > 一个屏障(默认 20s)时,
// 预建的 0 可能还没被抓到就已经变成 1,第一次 expired 仍可能漏报;abandoned 要 10 分钟后才发生,不受影响。
func ObserveNodeDetachDeferred(zoneID uint32, outcome string) {
	register()
	zoneStr := strconv.FormatUint(uint64(zoneID), 10)
	if outcome == NodeDetachOutcomeDeferred {
		nodeDetachDeferredTotal.WithLabelValues(zoneStr, NodeDetachOutcomeExpired).Add(0)
		nodeDetachDeferredTotal.WithLabelValues(zoneStr, NodeDetachOutcomeAbandoned).Add(0)
	}
	nodeDetachDeferredTotal.WithLabelValues(zoneStr, outcome).Inc()
}

// dead_node_reconcile_total 的 outcome 标签取值。**必须是常量**:它们直接变成 Prometheus label。
// 成员集合对账的 outcome 标签取值,见 sceneMemberReconcileMembersTotal 的注释。**必须是常量**:它们直接变成 Prometheus label。
const (
	SceneMemberReconcileOK        = "ok"
	SceneMemberReconcileRemoved   = "removed"
	SceneMemberReconcileSkipped   = "skipped"
	SceneMemberReconcileMalformed = "malformed"
	SceneMemberReconcileError     = "error"
)

// sceneMemberReconcileOutcomes 是注册时预置为 0 的全部 outcome,新增取值时同步加在这里。
var sceneMemberReconcileOutcomes = [...]string{
	SceneMemberReconcileOK,
	SceneMemberReconcileRemoved,
	SceneMemberReconcileSkipped,
	SceneMemberReconcileMalformed,
	SceneMemberReconcileError,
}

// ObserveSceneMemberReconcile 记 count 个成员的对账结局。count 为 0 时什么都不做。
func ObserveSceneMemberReconcile(outcome string, count int) {
	if count <= 0 {
		return
	}
	register()
	sceneMemberReconcileMembersTotal.WithLabelValues(outcome).Add(float64(count))
}

// ObserveSceneMemberReconcilePass 记一次走完全部成员集合。
func ObserveSceneMemberReconcilePass() {
	register()
	sceneMemberReconcilePassesTotal.Inc()
}

const (
	DeadNodeReconcileOutcomeCompleted    = "completed"
	DeadNodeReconcileOutcomeRetry        = "retry"
	DeadNodeReconcileOutcomeAbandoned    = "abandoned"
	DeadNodeReconcileOutcomeInstanceBack = "instance_back"
)

// ObserveDeadNodeReconcile 记一轮死节点收尾的结局,outcome 取 DeadNodeReconcileOutcome*。
// 第一次 retry 时给同一 zone 的 abandoned 预建值为 0 的序列,理由同 ObserveNodeDetachDeferred:increase() 看不到
// 序列从「不存在」到 1 的第一次跳变;abandoned 要 10 分钟后才发生,预建的 0 来得及被抓到。
func ObserveDeadNodeReconcile(zoneID uint32, outcome string) {
	register()
	zoneStr := strconv.FormatUint(uint64(zoneID), 10)
	if outcome == DeadNodeReconcileOutcomeRetry {
		deadNodeReconcileTotal.WithLabelValues(zoneStr, DeadNodeReconcileOutcomeAbandoned).Add(0)
	}
	deadNodeReconcileTotal.WithLabelValues(zoneStr, outcome).Inc()
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
	ResetNodeSeries()
}

// ObserveEnterSceneOwnerDeadTakeover 记一次「属主节点已死、按无持有者落点」。
func ObserveEnterSceneOwnerDeadTakeover(zoneID uint32) {
	register()
	enterSceneOwnerDeadTakeoverTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10)).Inc()
}

// ObserveEnterSceneOwnerReplacedTakeover 记一次「持有进程已被同号新进程顶替、按无持有者落点」(GO-3)。
func ObserveEnterSceneOwnerReplacedTakeover(zoneID uint32) {
	register()
	enterSceneOwnerReplacedTakeoverTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10)).Inc()
}

// ObserveEnterSceneHandoffAdmitted 记一次凭落盘标记放行、且落点已提交的交接。
func ObserveEnterSceneHandoffAdmitted(zoneID uint32) {
	register()
	enterSceneHandoffAdmittedTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10)).Inc()
}

// owner_replaced_recheck_total 的 result 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	OwnerReplacedRecheckAbsent  = "absent"
	OwnerReplacedRecheckPresent = "present"
	OwnerReplacedRecheckError   = "error"
)

// ObserveOwnerReplacedRecheck 记一次 GO-3 的 etcd 复核结果,result 取 OwnerReplacedRecheck* 常量。
func ObserveOwnerReplacedRecheck(zoneID uint32, result string) {
	register()
	ownerReplacedRecheckTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), result).Inc()
}

// ObserveReentryBarrierBlocked 记一次被再入屏障挡下的所有权变更尝试。
// site 必须取常量点位名,见 reentryBarrierBlockedTotal 的注释。
func ObserveReentryBarrierBlocked(zoneID uint32, site string) {
	register()
	reentryBarrierBlockedTotal.WithLabelValues(
		strconv.FormatUint(uint64(zoneID), 10), site).Inc()
}

// ObserveOwnershipReadUnknown 记一次因 Redis 读失败而放弃的改派 / 销毁 / 清理 / 伸缩决策。
// site 必须取常量点位名,见 ownershipReadUnknownTotal 的注释。
func ObserveOwnershipReadUnknown(zoneID uint32, site string) {
	register()
	ownershipReadUnknownTotal.WithLabelValues(
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

// placement_identity_total 的 site 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	PlacementIdentitySiteEnter = "enter"
	PlacementIdentitySiteLeave = "leave"
)

// ObservePlacementIdentity 记一次落点会话身份裁决,见 placementIdentityTotal。site 取 PlacementIdentitySite*;
// verdict 取 internal/logic/placement_identity.go 两张判定表的名表(固定字符串,低基数)。
func ObservePlacementIdentity(site, verdict string) {
	register()
	placementIdentityTotal.WithLabelValues(site, verdict).Inc()
}

// ObserveLeaveHolderRelease 记一次 LeaveScene 的持有者释放判定,见 leaveHolderReleaseTotal。verdict 取
// internal/logic/leave_release.go 的名表(固定字符串,低基数)。
func ObserveLeaveHolderRelease(verdict string) {
	register()
	leaveHolderReleaseTotal.WithLabelValues(verdict).Inc()
}

// dead_holder_release_total 的 result 标签取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	DeadHolderReleaseReleased            = "released"
	DeadHolderReleaseNoRow               = "no_row"
	DeadHolderReleaseNewerOwner          = "newer_owner"
	DeadHolderReleaseHolderRegistered    = "holder_registered"
	DeadHolderReleaseRowChanged          = "row_changed"
	DeadHolderReleaseRegistryUnavailable = "registry_unavailable"
	DeadHolderReleaseError               = "error"
)

// deadHolderReleaseResults 是注册时预置为 0 的全部 result,新增取值时同步加在这里。
var deadHolderReleaseResults = [...]string{
	DeadHolderReleaseReleased,
	DeadHolderReleaseNoRow,
	DeadHolderReleaseNewerOwner,
	DeadHolderReleaseHolderRegistered,
	DeadHolderReleaseRowChanged,
	DeadHolderReleaseRegistryUnavailable,
	DeadHolderReleaseError,
}

// ObserveDeadHolderRelease 记一次判死持有者写权释放的结果,见 deadHolderReleaseTotal。result 取 DeadHolderRelease* 常量。
func ObserveDeadHolderRelease(result string) {
	register()
	deadHolderReleaseTotal.WithLabelValues(result).Inc()
}

// ObserveEnterHolderRelease 记一次 EnterScene 换手门拒绝之后的持有者释放判定或结果,见 enterHolderReleaseTotal。
// verdict 取 internal/logic/enter_holder_release.go 的名表与 delivered / failed(固定字符串,低基数)。
func ObserveEnterHolderRelease(verdict string) {
	register()
	enterHolderReleaseTotal.WithLabelValues(verdict).Inc()
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

// ObserveAgonesCounterRollback 记一次 rooms 名额归还(删一个条目)的结果。
// outcome=="failed" 时调用方可以重试,不重试的由对账收回。
func ObserveAgonesCounterRollback(outcome string) {
	register()
	agonesCounterRollbackTotal.WithLabelValues(outcome).Inc()
}

// Agones rooms 对账的动作,取值见 agonesRoomsReconciledTotal。
const (
	AgonesRoomsOrphanRemoved      = "orphan_removed"
	AgonesRoomsHoldExpiredRemoved = "hold_expired_removed"
	AgonesRoomsHoldCanceled       = "hold_canceled"
	AgonesRoomsMissingAdded       = "missing_added"
	AgonesRoomsMissingNoCapacity  = "missing_no_capacity"
	AgonesRoomsRehomed            = "rehomed"
	AgonesRoomsRehomeNoCapacity   = "rehome_no_capacity"
)

// ObserveAgonesRoomsReconciled 记一次对账动作,action 取上面的常量。
func ObserveAgonesRoomsReconciled(zone, action string) {
	register()
	agonesRoomsReconciledTotal.WithLabelValues(zone, action).Inc()
}

// ObserveAgonesMappingFailure 记一次 "GSA 成功但映射不回本地节点" 的失败。
func ObserveAgonesMappingFailure(zone, reason string) {
	register()
	agonesMappingFailureTotal.WithLabelValues(zone, reason).Inc()
}

// SetAgonesCounterDrift 设置某 zone 上一轮对账结束时仍没收敛的条目与场景数,见 agonesCounterDrift。
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
// pending_map_fallback / scene_gone /
// channel_closing / channel_conf_mismatch / channel_switch_disabled / channel_full / channel_switch_cooldown
// (发射点:internal/logic/enterscenelogic.go、home_zone.go、channel_pick.go)。
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

// 新场景登记的 kind 标签,见 sceneRegistrationTotal。
const (
	SceneRegistrationKindInstance     = "instance"
	SceneRegistrationKindWorldChannel = "world_channel"
)

// sceneRegistrationAlertOutcomes 是告警盯的两个结局。increase() 看不到序列从「不存在」到 1 的第一次跳变,
// 所以每次登记都给同 zone、同 kind 预建这两条值为 0 的序列(Add(0) 不改值)。
var sceneRegistrationAlertOutcomes = []string{"conflict", "unknown"}

// ObserveSceneRegistration 记一次新场景登记的结局,取值见 sceneRegistrationTotal。
func ObserveSceneRegistration(zoneID uint32, kind, outcome string) {
	register()
	zone := strconv.FormatUint(uint64(zoneID), 10)
	for _, alertOutcome := range sceneRegistrationAlertOutcomes {
		sceneRegistrationTotal.WithLabelValues(zone, kind, alertOutcome).Add(0)
	}
	sceneRegistrationTotal.WithLabelValues(zone, kind, outcome).Inc()
}

// ObserveSceneTeardownJob 记一次拆除任务的结局,取值见 sceneTeardownJobsTotal。
func ObserveSceneTeardownJob(zoneID uint32, kind, outcome string) {
	register()
	sceneTeardownJobsTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), kind, outcome).Inc()
}

// SetSceneTeardownOldestPendingSeconds 发布本 zone 最老一条拆除任务的年龄,见 sceneTeardownOldestPendingSeconds。
func SetSceneTeardownOldestPendingSeconds(zoneID uint32, seconds int64) {
	register()
	sceneTeardownOldestPendingSeconds.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10)).Set(float64(seconds))
}

// ObserveGateRedirectExcluded 记一次跨 zone 重定向挑 gate 时剔除的 gate 数,reason 取 gatepick.ReasonStale /
// ReasonDraining / ReasonFull,见 gateRedirectExcludedTotal。count 不大于 0 时不记。
func ObserveGateRedirectExcluded(zoneID uint32, reason string, count int) {
	if count <= 0 {
		return
	}
	register()
	gateRedirectExcludedTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), reason).Add(float64(count))
}

// ObserveGateRedirectFallback 记一次目标 zone 没有完全合格 gate、退到下一档的跨 zone 重定向,
// reason 取 gatepick.Exclusion.FallbackReason()(stale / draining / full / mixed),见 gateRedirectFallbackTotal。
func ObserveGateRedirectFallback(zoneID uint32, reason string) {
	register()
	gateRedirectFallbackTotal.WithLabelValues(strconv.FormatUint(uint64(zoneID), 10), reason).Inc()
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
// 2026-10-16 起只在领导者上、且只对本拍计数读成功、所在 zone 的分数 ZADD 确认写成功的节点调用
// (internal/logic/load_refresh.go);其余节点的序列停在上一次成功的值,不删。
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

// declaredSceneNodeRoles 是 SetNodesByRole 补 0 时覆盖的四种声明角色。不含 unknown:它只在
// 配置出错时出现,由告警 SceneManagerUnknownSceneNodeType 按 > 0 盯着,补 0 没有意义。
var declaredSceneNodeRoles = [...]uint32{
	constants.SceneNodeTypeMainWorld,
	constants.SceneNodeTypeInstance,
	constants.SceneNodeTypeMainWorldCross,
	constants.SceneNodeTypeInstanceCross,
}

// SetNodesByRole publishes the live count of nodes per (zone, role). Called
// on the LoadReporter tick after all per-node observations are complete so
// the snapshot is internally consistent.
//
// 补 0 规则:Reset 之后,对「counts 里出现过的 zone」与「expectedZones 里非 0 的 zone」两者的并集,
// 每个 zone 的四种声明角色先写 0,再用 counts 覆盖实际值。
//   - 为什么补 0(cross-zone-scene-travel.md §12.3 告警盲区):以前只写 counts 里出现过的组合,
//     某个池子空了 = 序列消失而不是变成 0;整个 zone 的 scene 节点全灭时该 zone 一条序列都不剩,
//     两条 PoolEmpty 告警无从按 zone 触发。expectedZones 由调用方传本进程配置的 ZoneId:部署来服务
//     zone Z 的 scene_manager 期待 Z 里有 scene 节点,这是现成的事实,不需要新配置项。
//   - 为什么不做 sticky zone(见过一次就永远补 0):合服下线的 zone 会让 critical 永远误报 pager,
//     而且进程重启后记忆清空,又回到盲区 —— 两头都不对。
//   - zone 0 跳过:不是合法 zone,单测的 config.Config{} 里 ZoneId 就是 0,补出来只会污染序列。
//
// 发布者(2026-10-16 起):领导者传入 etcd 镜像里全部 zone 的计数;跟随者只传本进程 ZoneId 的计数。
// 两者都把本进程 ZoneId 作为 expectedZones 补 0。所以每个 zone 有「领导者 + 该 zone 自己的
// scene_manager 副本」几个发布者,各自的值都来自 etcd 快照、彼此一致;看板按 max 聚合,PoolEmpty
// 告警只做 > 0 / == 0 判定,不受发布者个数影响。
func SetNodesByRole(counts map[struct {
	ZoneID uint32
	Role   uint32
}]int, expectedZones []uint32) {
	register()
	nodesByRole.Reset()

	zones := make(map[uint32]struct{}, len(counts)+len(expectedZones))
	for k := range counts {
		zones[k.ZoneID] = struct{}{}
	}
	for _, z := range expectedZones {
		if z != 0 {
			zones[z] = struct{}{}
		}
	}
	for z := range zones {
		zoneStr := strconv.FormatUint(uint64(z), 10)
		for _, role := range declaredSceneNodeRoles {
			nodesByRole.WithLabelValues(zoneStr, RoleLabel(role)).Set(0)
		}
	}

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
