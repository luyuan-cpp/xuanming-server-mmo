package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"data_service/internal/store"

	pb "proto/common/database"
	"shared/authoritysql"
	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
)

// freeze / unfreeze(第 1b-1 批,2026-10-18):全局库回档恢复流程 freeze → reconcile-from-cache → audit -frozen-recovery → unfreeze
// 的首尾两步(docs/design/player-route-mapping-durability-proposal.md §11.2「第 1b 批」、runbook §9.4)。
//
// freeze 只碰全局库(route_meta.writable = 0),不连 mapping Redis。
//
// unfreeze 在翻回 writable = 1 之前要确认 mapping Redis 是**可信的超集**:冻结期间 Redis 若重启 / 切主 / 换实例(从旧备份恢复),
// 库与缓存可能同时缺同一批登记,而 audit 看不见两边都缺的行;解冻之后读方(第 3 批)会把它们当成权威缺席。确认的依据只有
// 就绪标记(placement.RouteReadyMarker):存在、可解码,且标记里的 run_id / master_replid 等于当前实例。确认不了一律退出码 2,
// 只有显式 -cache-was-not-verified-acknowledged 才放行 —— 这时要先按 runbook §9.2 的清单重放,补上两边都可能缺的合服与搬库。
// **第 1b-2 批之前没有任何写者挂标记**,所以本批的 unfreeze 一律要走这条显式确认。
//
// 标记的处置(fail-closed 的方向是「撤掉信任」):畸形 → 按原值 compare-and-delete;版本号比本二进制新 → 只记日志、不删也不信任;
// 不是 string 类型 → 读方 MGET 看到的是 nil(等同缺席),不动它;身份不符 → 不信任,显式确认放行时按原值撤掉;
// 可信但 generation 与库不等 → 先 compare-and-set 成库的 generation 再解冻(第 1b-2 批的 leader 据 generation 判库被重置,
// 不对齐的话解冻之后它会立刻再冻结),CAS 输了退出码 1、重跑即可。
//
// redis 模式(UnfreezeOptions.RedisMode)下 unfreeze 不读、不碰标记(那时没有标记语义),最低水位取 0,要求显式确认:它只用来
// 撤销误跑 / 演练的 freeze 或从 db_first 回退时带过来的冻结 —— import-from-redis 的 requireRouteWritable 与模式无关,不解冻就一直拒写。
//
// 水位键 mapping:route:watermark 的出口(2026-10-19,规格 spec_D1_batch1b2_2_3.md §2 M3、§4.6 第 4 项、§13.1 第 4 条;只在 db_first):
// 对账 leader 每拍拿水位键与库比,世代不等、或水位键的水位 > 库水位就冻结映射写者。库被回档之后水位键留着回档前的水位,
// 不处理的话解冻之后 leader 下一拍就会再冻结。所以:
//   - 解冻之前先读水位键(早于读标记,版本更新 / 不是 string 时在任何写之前拒绝,退出码 2、不动它);
//   - 最低水位取 max(可信标记的 watermark_ms, 水位键.FloorFor(库世代))——世代相同的水位键才约束本世代;
//     -cache-was-not-verified-acknowledged 放行时可信标记那一项是 0,水位键那一项照取;
//   - writable 翻回 1 之后(同一流程),把水位键改成 {库世代, 上面那个最低水位}:不在就 SET NX,在就按原值 compare-and-set
//     (RouteReadyCompareAndSetScript,脚本与键无关);最低水位是 0(没有可写的正水位)而水位键是别的世代或畸形时按原值删掉,
//     留给 leader 下一拍按库值重建。写输了(读到之后被改过)复读一次:已不在、或已是本世代的值(leader 解冻后已推进它)都算完成;
//     否则退出码 1,重跑 —— writable 已经是 1,重跑走「已可写」分支什么都不做;若 leader 因世代 / 水位又冻结了,重跑会再走一遍并重置它。
//   - 写进去的是交给解冻事务的**下限**,不是解冻后的精确库水位(store 没有读 route_reconcile.watermark_ms 的接口):解冻事务按 GREATEST
//     抬水位,解冻后的库水位 ≥ 它,所以水位键不会让 leader 判「水位键 > 库水位」;leader 之后每拍按库水位单调推进它。

// readyMarkerRedis 是 unfreeze 对 mapping Redis 的依赖(就绪标记与水位键的读、按原值改 / 删,水位键不在时 SET NX)。
// go-zero 的 *redis.Redis 天然满足。GetCtx 的约定(go-zero):键不在回 ("", nil)。
type readyMarkerRedis interface {
	GetCtx(ctx context.Context, key string) (string, error)
	EvalCtx(ctx context.Context, script string, keys []string, args ...any) (any, error)
	SetnxCtx(ctx context.Context, key, value string) (bool, error)
}

// freezeDB / unfreezeDB 是两个子命令对全局库的依赖,*store.RouteStore 天然满足。
type freezeDB interface {
	FreezeRouteWriters(ctx context.Context) (bool, error)
}

type unfreezeDB interface {
	ReadRouteMeta(ctx context.Context) (store.RouteMeta, error)
	UnfreezeRouteWriters(ctx context.Context, minWatermarkMs uint64) (bool, error)
}

// RedisIdentity 是 mapping Redis 实例的身份:INFO server 的 run_id 与 INFO replication 的 master_replid。
// 两者都是 Redis 生成的 40 个十六进制字符;任一变化都说明实例重启、切主或换了一个,此前挂的标记不再可信。
type RedisIdentity struct {
	RunID        string
	MasterReplID string
}

// ParseRedisIdentity 从 INFO server 与 INFO replication 的原文里取身份(行尾 CRLF 或 LF 都认)。
// 任一字段缺席或不是 40 个小写十六进制字符都报错:身份读不准时宁可确认不了(fail-closed)。
//
// 2026-10-19(规格 spec_D1_batch1b2_2_3.md §4.6 第 5 项):解析委托 placement.ParseRedisInstanceIdentity,删掉本包的私有副本,
// 读方脚本、对账 leader 与本工具只剩一处口径。导出名与返回类型不变(调用方不改)。唯一的收紧:replication 段缺 role 也报错
// (真 Redis 的 INFO replication 恒有 role;读不到说明回复不对,fail-closed)。TestParseRedisIdentityAgreesWithPlacement 钉住两者同结论。
func ParseRedisIdentity(infoServer, infoReplication string) (RedisIdentity, error) {
	id, err := placement.ParseRedisInstanceIdentity(infoServer, infoReplication)
	if err != nil {
		return RedisIdentity{}, fmt.Errorf("parse INFO server / replication: %w", err)
	}
	return RedisIdentity{RunID: id.RunID, MasterReplID: id.MasterReplID}, nil
}

// ── freeze ──────────────────────────────────────────────────────────────────

// FreezeCatalog 只暴露正式冻结提交；生产由独立Operator的authoritysql.Catalog实现。
type FreezeCatalog interface {
	Freeze(context.Context, []byte, *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error)
}

// ReleaseCatalog 只暴露同票据及完整ReleaseReadiness的正式释放。
type ReleaseCatalog interface {
	Release(context.Context, []byte, *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error)
}

// MaintenanceCatalog 是CLI维护连接的完整typed能力，普通业务Store不持有它。
type MaintenanceCatalog interface {
	FreezeCatalog
	ReleaseCatalog
	FrozenCatalog
}

// MaintenanceWork是同步恢复调用的接收门。CLI传真实BindMaintenance会话；SQL域用例可显式
// 注入本地会话，仅证明自己的同步SQL已返回，不据此伪造全栈P0/Redis/Kafka身份或P7审计。
type MaintenanceWork interface {
	Enter() (func(), error)
	Hold()
}

var _ MaintenanceCatalog = (*authoritysql.Catalog)(nil)

// FreezeOptions 是 freeze 的参数。
type FreezeOptions struct {
	// 仅接受已固定摘要的正式请求；调用方不能从当前状态拼造排空证据。
	Catalog  FreezeCatalog
	TicketID []byte
	Request  *pb.SqlWriterMaintenanceReceipt
	// Reason 只打到 stderr(route_meta 没有记原因的列),给事后对日志。
	Reason string
	// Timeout 是冻结写本身的时限,至少 store.RouteFreezeTimeoutMin(见它的注释)。
	Timeout time.Duration
	// RedisMode:yaml 是 RouteStore.Mode=redis 时为 true —— 那时 W1 不读 writable,冻结只拦导入与反向修库,打 WARN。
	RedisMode bool
	// Target 非空时作为 stderr 第一行。
	Target string
	// Stdout 只写一行统计;Stderr 写说明与告警。
	Stdout, Stderr io.Writer
}

// FreezeReport 是一次 freeze 的结果。
type FreezeReport struct {
	// Changed false = 本来就是冻结的。
	Changed bool
}

// Freeze 冻结映射写者。成功时 stdout 一行 `route_freeze changed=… writable=0`;出错时退出码 2、不出统计行。
func Freeze(ctx context.Context, db freezeDB, opts FreezeOptions) (FreezeReport, error) {
	if opts.Catalog == nil || len(opts.TicketID) != 16 || opts.Request == nil {
		return FreezeReport{}, errors.New("冻结缺少正式 Catalog、票据或固定请求")
	}
	if opts.Stdout == nil || opts.Stderr == nil {
		return FreezeReport{}, errors.New("stdout / stderr writers are required")
	}
	if opts.Timeout < store.RouteFreezeTimeoutMin {
		return FreezeReport{}, fmt.Errorf("freeze timeout %s is below %s", opts.Timeout, store.RouteFreezeTimeoutMin)
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	if opts.RedisMode {
		fmt.Fprintln(opts.Stderr, "WARN: RouteStore.Mode=redis: W1 (RegisterPlayerZone) does not read route_meta.writable in redis mode; "+
			"freezing only blocks route_tool import and the DB repair")
	}
	fmt.Fprintf(opts.Stderr, "FREEZE reason=%q\n", opts.Reason)
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	_, err := opts.Catalog.Freeze(wctx, opts.TicketID, opts.Request)
	if err != nil {
		return FreezeReport{}, err
	}
	// 成功原件已经包含全部角色、历史非SQL副作用和槽锁排空；不以延迟猜测完成。
	fmt.Fprintln(opts.Stdout, "route_freeze changed=true writable=0")
	return FreezeReport{Changed: true}, nil
}

// FreezeExitCode:出错 2,否则 0(含本来就冻结)。
func FreezeExitCode(_ FreezeReport, err error) int {
	if err != nil {
		return ExitFailed
	}
	return ExitClean
}

// ── unfreeze ────────────────────────────────────────────────────────────────

// 就绪标记的状态(UnfreezeReport.Marker 的取值,只进统计行与日志)。
const (
	MarkerVerified         = "verified"
	MarkerAbsent           = "absent"
	MarkerMalformedDropped = "malformed_dropped"
	MarkerNewerVersion     = "newer_version"
	MarkerWrongType        = "wrong_type"
	MarkerIdentityMismatch = "identity_mismatch"
	// MarkerNotReadRedisMode:redis 模式下的 unfreeze 不读标记。
	MarkerNotReadRedisMode = "not_read_redis_mode"
)

// ErrCacheNotVerified 表示 unfreeze 确认不了 mapping Redis 是可信的超集(没有可信的就绪标记),拒绝解冻(退出码 2)。
var ErrCacheNotVerified = errors.New("route_tool unfreeze: the mapping cache is not verified by a ready marker matching the current Redis instance")

// ErrReadyMarkerChanged 表示 unfreeze 按原值改 / 删就绪标记时 CAS 输了:读到之后有人改过它。重跑即可(退出码 1)。
var ErrReadyMarkerChanged = errors.New("route_tool unfreeze: mapping:route:ready changed while unfreezing (compare-and-swap lost); rerun")

// ErrWatermarkKeyWrongType 表示 mapping:route:watermark 不是 string 类型:解冻前就拒绝(退出码 2),不动它 ——
// 按原值改 / 删的脚本对它只会报 WRONGTYPE,leader 也维护不了它。人工确认后只 DEL 这一把键(或跑 rewarm)再重跑。
var ErrWatermarkKeyWrongType = errors.New("route_tool unfreeze: mapping:route:watermark is not a string key; DEL that key alone (or run `rewarm`) and rerun")

// ErrWatermarkKeyNotReset 表示 writable 已经翻回 1,但水位键没能改成本世代的值(按原值改输了且复读到别的世代 / 畸形,或 Redis 出错)。
// 退出码 1、重跑(见文件头「水位键的出口」)。
var ErrWatermarkKeyNotReset = errors.New("route_tool unfreeze: route writers are unfrozen but mapping:route:watermark was not reset to this generation; rerun")

// UnfreezeOptions 是 unfreeze 的参数。
type UnfreezeOptions struct {
	Catalog  ReleaseCatalog
	TicketID []byte
	Request  *pb.SqlWriterMaintenanceReceipt
	// CacheUnverifiedAcknowledged:没有可信标记时仍解冻(-cache-was-not-verified-acknowledged)。见文件头。
	CacheUnverifiedAcknowledged bool
	// RedisMode:yaml 是 RouteStore.Mode=redis。不读标记(rd 可为 nil、Identity 可不给),最低水位 0;
	// 必须同时给 CacheUnverifiedAcknowledged(调用方的门禁先判,这里再兜一次)。见文件头。
	RedisMode bool
	// Timeout 是解冻事务本身的时限,至少 store.RouteFreezeTimeoutMin。
	Timeout time.Duration
	// Identity 读当前 mapping Redis 的身份(装配层用 INFO 实现;单测注入)。只在标记可解码时才调。
	Identity func(ctx context.Context) (RedisIdentity, error)
	// Target 非空时作为 stderr 第一行。
	Target string
	// Stdout 只写一行统计;Stderr 写判定过程与告警。
	Stdout, Stderr io.Writer
}

// UnfreezeReport 是一次 unfreeze 的结果。
type UnfreezeReport struct {
	// Changed true = 本次把 writable 翻回了 1。
	Changed bool
	// AlreadyWritable:开始时就已可写,什么都没做。
	AlreadyWritable bool
	// Marker 是就绪标记的状态(Marker* 常量之一;AlreadyWritable 时为空)。
	Marker string
	// GenerationRealigned:可信标记的 generation 与库不等,已 CAS 成库的值。
	GenerationRealigned bool
	// MinWatermarkMs 是交给 UnfreezeRouteWriters 的最低水位:max(可信标记的 watermark_ms, WatermarkKeyFloorMs);都没有时 0。
	MinWatermarkMs uint64
	// WatermarkKeyFloorMs 是水位键对库世代给出的下限(世代相同且可解码时是它的 watermark_ms,否则 0)。不进统计行。
	WatermarkKeyFloorMs uint64
	// WatermarkKey 是解冻之后水位键的处置(WatermarkKey* 常量之一;AlreadyWritable 时为空)。不进统计行(统计行格式不变),
	// 打在 stderr 的 WATERMARK_KEY 行。
	WatermarkKey string
}

// 解冻之后水位键的处置(UnfreezeReport.WatermarkKey 的取值)。
const (
	// WatermarkKeySetNX:原来不在,SET NX 成 {库世代, MinWatermarkMs}。
	WatermarkKeySetNX = "set_nx"
	// WatermarkKeyCompareAndSet:按原值换成 {库世代, MinWatermarkMs}。
	WatermarkKeyCompareAndSet = "compare_and_set"
	// WatermarkKeyKept:原值已经就是 {库世代, MinWatermarkMs}。
	WatermarkKeyKept = "kept"
	// WatermarkKeyLeftAbsent:原来不在、MinWatermarkMs 又是 0(没有可写的正水位):留给 leader 下一拍 SET NX。
	WatermarkKeyLeftAbsent = "left_absent"
	// WatermarkKeyDropped:原值是别的世代或畸形、MinWatermarkMs 又是 0:按原值删掉,留给 leader 下一拍重建。
	WatermarkKeyDropped = "dropped"
	// WatermarkKeySuperseded:按原值写输了,复读到的是本世代的可解码值(leader 已推进它)或键已不在(rewarm 删了):不必再写。
	WatermarkKeySuperseded = "superseded"
	// WatermarkKeyNotReset:没能改成本世代的值,退出码 1(ErrWatermarkKeyNotReset)。
	WatermarkKeyNotReset = "not_reset"
	// WatermarkKeySkipped:没有由本次翻 writable(并发被别人解冻),或 redis 模式。
	WatermarkKeySkipped = "skipped"
)

// SummaryLine 是 stdout 唯一的一行。
func (r UnfreezeReport) SummaryLine() string {
	return fmt.Sprintf("route_unfreeze changed=%t already_writable=%t marker=%s generation_realigned=%t min_watermark_ms=%d",
		r.Changed, r.AlreadyWritable, r.Marker, r.GenerationRealigned, r.MinWatermarkMs)
}

// UnfreezeExitCode:CAS 输了 1(重跑;含解冻之后水位键没改成);其它错误 2(含确认不了缓存、水位键版本更新或不是 string);否则 0。
func UnfreezeExitCode(_ UnfreezeReport, err error) int {
	switch {
	case errors.Is(err, ErrReadyMarkerChanged), errors.Is(err, ErrWatermarkKeyNotReset):
		return ExitAttention
	case err != nil:
		return ExitFailed
	default:
		return ExitClean
	}
}

// Unfreeze 解冻映射写者(流程见文件头)。调用方(routetoolcli)负责 -confirm-audited 与 RouteStore.Mode 的门禁
// (redis 模式另要求显式确认,这里只兜底)。只有翻成功或本来就可写才往 Stdout 写统计行。
func Unfreeze(ctx context.Context, rd readyMarkerRedis, db unfreezeDB, opts UnfreezeOptions) (UnfreezeReport, error) {
	if opts.Catalog == nil || len(opts.TicketID) != 16 || opts.Request == nil || opts.Request.Evidence.GetReleaseReadiness() == nil {
		return UnfreezeReport{}, errors.New("解冻缺少正式 Catalog、票据或 P8 ReleaseReadiness 原件")
	}
	if opts.Stdout == nil || opts.Stderr == nil {
		return UnfreezeReport{}, errors.New("stdout / stderr writers are required")
	}
	if !opts.RedisMode && (rd == nil || opts.Identity == nil) {
		return UnfreezeReport{}, errors.New("the mapping Redis and the identity reader are required outside redis mode")
	}
	if opts.RedisMode && !opts.CacheUnverifiedAcknowledged {
		return UnfreezeReport{}, fmt.Errorf("%w: RouteStore.Mode=redis needs -cache-was-not-verified-acknowledged", ErrCacheNotVerified)
	}
	if opts.Timeout < store.RouteFreezeTimeoutMin {
		return UnfreezeReport{}, fmt.Errorf("unfreeze timeout %s is below %s", opts.Timeout, store.RouteFreezeTimeoutMin)
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	var rep UnfreezeReport

	meta, err := db.ReadRouteMeta(ctx)
	if err != nil {
		return rep, fmt.Errorf("read route_meta: %w", err)
	}
	if !meta.Frozen() {
		rep.AlreadyWritable = true
		fmt.Fprintln(opts.Stderr, "route writers are already writable; nothing to do")
		fmt.Fprintln(opts.Stdout, rep.SummaryLine())
		return rep, nil
	}

	if opts.RedisMode {
		rep.Marker = MarkerNotReadRedisMode
		rep.WatermarkKey = WatermarkKeySkipped
		fmt.Fprintf(opts.Stderr, "WARN: RouteStore.Mode=redis: %s is not read in redis mode and the cache is not verified; "+
			"unfreezing because -cache-was-not-verified-acknowledged was given (W1 does not read route_meta.writable in redis mode; "+
			"this re-enables route_tool import and the db_first writers after a later switch)\n", placement.RouteReadyKey)
		return unfreezeWriters(ctx, db, opts, rep, nil)
	}

	// 水位键先于标记读:版本更新 / 不是 string 时在任何写之前拒绝(连畸形标记的撤除都不做)。见文件头「水位键的出口」。
	wk, err := readWatermarkKey(ctx, rd)
	if err != nil {
		return rep, err
	}
	rep.WatermarkKey = WatermarkKeySkipped

	raw, marker, state, err := readReadyMarker(ctx, rd, opts.Identity)
	if err != nil {
		return rep, err
	}
	rep.Marker = state
	if state == MarkerMalformedDropped {
		fmt.Fprintf(opts.Stderr, "WARN: %s held a malformed value %q; dropped it by compare-and-delete\n", placement.RouteReadyKey, raw)
	}

	if state != MarkerVerified {
		reason := unverifiedReason(state)
		if !opts.CacheUnverifiedAcknowledged {
			fmt.Fprintf(opts.Stderr, "REFUSED: %s. Without a verified ready marker the cache may have lost the same entries the DB lost, "+
				"and audit cannot see entries missing on both sides. Replay the manifests per merge-zone-runbook §9.2 first, "+
				"then rerun with -cache-was-not-verified-acknowledged (runbook §9.4).\n", reason)
			return rep, fmt.Errorf("%w: %s", ErrCacheNotVerified, reason)
		}
		fmt.Fprintf(opts.Stderr, "WARN: %s; unfreezing anyway because -cache-was-not-verified-acknowledged was given "+
			"(the operator confirmed the merge-zone-runbook §9.2 replay)\n", reason)
		if state == MarkerIdentityMismatch {
			// 不可信的标记不能活过解冻:读方只看「存在」。按原值撤掉,读到之后有人改过就让运维重跑。
			if err := compareAndDeleteReady(ctx, rd, raw); err != nil {
				return rep, err
			}
			fmt.Fprintf(opts.Stderr, "dropped %s (identity mismatch) by compare-and-delete\n", placement.RouteReadyKey)
		}
	} else {
		rep.MinWatermarkMs = marker.WatermarkMs
		if marker.Generation != meta.Generation {
			aligned := marker
			aligned.Generation = meta.Generation
			next, err := placement.EncodeRouteReadyMarker(aligned)
			if err != nil {
				return rep, fmt.Errorf("re-encode %s with generation %s: %w", placement.RouteReadyKey, meta.Generation, err)
			}
			if err := compareAndSetReady(ctx, rd, raw, next); err != nil {
				return rep, err
			}
			rep.GenerationRealigned = true
			fmt.Fprintf(opts.Stderr, "%s generation %s -> %s (compare-and-set)\n", placement.RouteReadyKey, marker.Generation, meta.Generation)
		}
	}

	rep.WatermarkKeyFloorMs = wk.floorFor(meta.Generation)
	if rep.WatermarkKeyFloorMs > rep.MinWatermarkMs {
		fmt.Fprintf(opts.Stderr, "%s (generation %s) raises the DB watermark floor %d -> %d; otherwise the reconcile leader "+
			"re-freezes on its next tick (watermark key above the DB watermark)\n",
			placement.RouteWatermarkKey, meta.Generation, rep.MinWatermarkMs, rep.WatermarkKeyFloorMs)
		rep.MinWatermarkMs = rep.WatermarkKeyFloorMs
	} else if wk.state == watermarkKeyDecoded && wk.value.Generation != meta.Generation {
		fmt.Fprintf(opts.Stderr, "%s belongs to generation %s, not the DB generation %s: it does not bound this generation "+
			"and is reset after unfreezing\n", placement.RouteWatermarkKey, wk.value.Generation, meta.Generation)
	}
	generation := meta.Generation
	return unfreezeWriters(ctx, db, opts, rep, func(r *UnfreezeReport) error {
		outcome, err := resetWatermarkKey(ctx, rd, wk, generation, r.MinWatermarkMs)
		r.WatermarkKey = outcome
		fmt.Fprintf(opts.Stderr, "WATERMARK_KEY %s before=%s generation=%s watermark_ms=%d\n", outcome, wk.state, generation, r.MinWatermarkMs)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "WARN: %v. Rerun unfreeze: an already-writable rerun changes nothing; "+
				"if the reconcile leader re-froze the writers (data_service_route_reconcile_event_total{event=\"freeze_generation\"} or "+
				"{event=\"freeze_watermark\"}), the rerun resets the key\n", err)
		}
		return err
	})
}

// unfreezeWriters 是解冻的最后一步:在 opts.Timeout 内翻回 writable = 1(最低水位取 rep.MinWatermarkMs);确实由本次翻成功时
// 再调 afterFlip(可为 nil;水位键的出口)。翻成功或被并发解冻就写统计行(afterFlip 出错也写:writable 已经是 1),
// 返回 afterFlip 的错误;翻的那一步出错不写统计行。
func unfreezeWriters(ctx context.Context, db unfreezeDB, opts UnfreezeOptions, rep UnfreezeReport, afterFlip func(*UnfreezeReport) error) (UnfreezeReport, error) {
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	// 强读所得下限只能检验固定原件，不得修改、重封原件来掩盖漂移。
	proof := opts.Request.Evidence.GetReleaseReadiness()
	if proof == nil || proof.RouteWatermarkFloorMs < rep.MinWatermarkMs {
		return rep, errors.New("P8 原件的路由水位下限低于当前可信 Redis 水位，拒绝解冻")
	}
	_, err := opts.Catalog.Release(wctx, opts.TicketID, opts.Request)
	changed := err == nil
	if err != nil {
		return rep, err
	}
	rep.Changed = changed
	var afterErr error
	switch {
	case !changed:
		// 读开关与解冻之间被别人解冻了:结果一样,照实报。水位键由那次解冻处置,这里不碰。
		fmt.Fprintln(opts.Stderr, "route writers were unfrozen concurrently; nothing changed")
	case afterFlip != nil:
		afterErr = afterFlip(&rep)
	}
	fmt.Fprintln(opts.Stdout, rep.SummaryLine())
	return rep, afterErr
}

// ── 水位键(mapping:route:watermark)的读与出口 ───────────────────────────────────

// 解冻前读到的水位键状态(只进 stderr 的 WATERMARK_KEY 行)。
const (
	watermarkKeyAbsent    = "absent"
	watermarkKeyDecoded   = "decoded"
	watermarkKeyMalformed = "malformed"
)

// watermarkKeyRead 是解冻前读到的水位键。raw 是原值(按原值改 / 删用);value 只在 decoded 时有效。
type watermarkKeyRead struct {
	state string
	raw   string
	value placement.RouteWatermark
}

// floorFor 是这把水位键对库世代 generation 给出的库水位下限:可解码且世代相同时是它的 watermark_ms,否则 0。
func (w watermarkKeyRead) floorFor(generation string) uint64 {
	if w.state != watermarkKeyDecoded {
		return 0
	}
	return w.value.FloorFor(generation)
}

// readWatermarkKey 解冻前读水位键。版本比本二进制新 → 拒绝(包 placement.ErrRouteWatermarkNewerVersion,退出码 2,不动它:
// 新版本的下限规则本二进制不知道,删了又会与新版本的 leader 来回打架);不是 string 类型 → ErrWatermarkKeyWrongType;
// 畸形 → 不给下限,解冻之后按原值改掉 / 删掉。
func readWatermarkKey(ctx context.Context, rd readyMarkerRedis) (watermarkKeyRead, error) {
	raw, err := rd.GetCtx(ctx, placement.RouteWatermarkKey)
	if err != nil {
		if isWrongType(err) {
			return watermarkKeyRead{}, ErrWatermarkKeyWrongType
		}
		return watermarkKeyRead{}, fmt.Errorf("GET %s: %w", placement.RouteWatermarkKey, err)
	}
	if raw == "" {
		return watermarkKeyRead{state: watermarkKeyAbsent}, nil
	}
	w, err := placement.DecodeRouteWatermark(raw)
	switch {
	case errors.Is(err, placement.ErrRouteWatermarkNewerVersion):
		return watermarkKeyRead{}, fmt.Errorf("route_tool unfreeze: %s=%q: %w; refused before unfreezing and left untouched (use the newer binary)",
			placement.RouteWatermarkKey, raw, err)
	case err != nil:
		return watermarkKeyRead{state: watermarkKeyMalformed, raw: raw}, nil
	}
	return watermarkKeyRead{state: watermarkKeyDecoded, raw: raw, value: w}, nil
}

// resetWatermarkKey 在本次把 writable 翻回 1 之后,把水位键改成 {generation, minWatermarkMs}(见文件头「水位键的出口」)。
// 返回处置(WatermarkKey* 之一);没改成时返回包 ErrWatermarkKeyNotReset 的错误(退出码 1)。
func resetWatermarkKey(ctx context.Context, rd readyMarkerRedis, before watermarkKeyRead, generation string, minWatermarkMs uint64) (string, error) {
	if minWatermarkMs == 0 {
		// 没有可写的正水位(EncodeRouteWatermark 要求 watermark_ms > 0)。本世代的水位键必然给出正的下限,走不到这里。
		switch {
		case before.state == watermarkKeyAbsent:
			return WatermarkKeyLeftAbsent, nil
		case before.state == watermarkKeyDecoded && before.value.Generation == generation:
			return WatermarkKeyKept, nil
		}
		// 别的世代或畸形:留着它 leader 下一拍就按世代 / 水位再冻结。按原值删掉,leader 下一拍按库值 SET NX 重建。
		won, err := evalWatermarkKeyScript(ctx, rd, placement.RouteReadyCompareAndDeleteScript, "compare-and-delete", before.raw)
		return settleWatermarkKey(ctx, rd, generation, WatermarkKeyDropped, won, err)
	}
	next, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: generation, WatermarkMs: minWatermarkMs})
	if err != nil {
		return WatermarkKeyNotReset, fmt.Errorf("%w: encode {generation=%s, watermark_ms=%d}: %v", ErrWatermarkKeyNotReset, generation, minWatermarkMs, err)
	}
	if before.state == watermarkKeyAbsent {
		won, err := rd.SetnxCtx(ctx, placement.RouteWatermarkKey, next)
		if err != nil {
			err = fmt.Errorf("SET NX %s: %w", placement.RouteWatermarkKey, err)
		}
		return settleWatermarkKey(ctx, rd, generation, WatermarkKeySetNX, won, err)
	}
	if before.raw == next {
		return WatermarkKeyKept, nil
	}
	won, err := evalWatermarkKeyScript(ctx, rd, placement.RouteReadyCompareAndSetScript, "compare-and-set", before.raw, next)
	return settleWatermarkKey(ctx, rd, generation, WatermarkKeyCompareAndSet, won, err)
}

// settleWatermarkKey 收尾一次水位键写:写成了 → outcome;Redis 出错 → not_reset;写输了(读到之后被改过)→ 复读一次:
// 键已不在(rewarm 删了,leader 下一拍重建)、或是本世代的可解码值(只有 leader 与解冻会写本世代的值,两者写的都不高于库水位)
// 都算完成(superseded);别的世代 / 畸形 / 版本更新 → not_reset。
func settleWatermarkKey(ctx context.Context, rd readyMarkerRedis, generation, outcome string, won bool, err error) (string, error) {
	if err != nil {
		return WatermarkKeyNotReset, fmt.Errorf("%w (%s: %v)", ErrWatermarkKeyNotReset, outcome, err)
	}
	if won {
		return outcome, nil
	}
	raw, err := rd.GetCtx(ctx, placement.RouteWatermarkKey)
	if err != nil {
		return WatermarkKeyNotReset, fmt.Errorf("%w (%s lost the race and the re-read failed: %v)", ErrWatermarkKeyNotReset, outcome, err)
	}
	if raw == "" {
		return WatermarkKeySuperseded, nil
	}
	if w, derr := placement.DecodeRouteWatermark(raw); derr == nil && w.Generation == generation {
		return WatermarkKeySuperseded, nil
	}
	return WatermarkKeyNotReset, fmt.Errorf("%w (%s lost the race; %s is now %q)", ErrWatermarkKeyNotReset, outcome, placement.RouteWatermarkKey, raw)
}

// evalWatermarkKeyScript 对水位键跑一段按原值改 / 删的脚本(RouteReadyCompareAndSetScript / RouteReadyCompareAndDeleteScript
// 与键无关,KEYS[1] 换成水位键即可):回 1 → true;回 0(原值已变)→ false;其余 → error。
func evalWatermarkKeyScript(ctx context.Context, rd readyMarkerRedis, script, what string, args ...any) (bool, error) {
	res, err := rd.EvalCtx(ctx, script, []string{placement.RouteWatermarkKey}, args...)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", what, placement.RouteWatermarkKey, err)
	}
	n, ok := res.(int64)
	switch {
	case !ok:
		return false, fmt.Errorf("%s %s: unexpected reply %T %v", what, placement.RouteWatermarkKey, res, res)
	case n == 1:
		return true, nil
	case n == 0:
		return false, nil
	default:
		return false, fmt.Errorf("%s %s: unexpected reply %d", what, placement.RouteWatermarkKey, n)
	}
}

// readReadyMarker 读并判定就绪标记。返回原值(CAS / CAD 用)、解码后的标记(只在 verified 时有效)与状态。
// 畸形的值在这里就按原值撤掉(状态 malformed_dropped);撤的时候 CAS 输了按 ErrReadyMarkerChanged 返回。
func readReadyMarker(ctx context.Context, rd readyMarkerRedis, identity func(context.Context) (RedisIdentity, error)) (string, placement.RouteReadyMarker, string, error) {
	raw, err := rd.GetCtx(ctx, placement.RouteReadyKey)
	if err != nil {
		if isWrongType(err) {
			return "", placement.RouteReadyMarker{}, MarkerWrongType, nil
		}
		return "", placement.RouteReadyMarker{}, "", fmt.Errorf("GET %s: %w", placement.RouteReadyKey, err)
	}
	if raw == "" {
		return "", placement.RouteReadyMarker{}, MarkerAbsent, nil
	}
	m, err := placement.DecodeRouteReadyMarker(raw)
	switch {
	case errors.Is(err, placement.ErrRouteReadyMarkerNewerVersion):
		return raw, placement.RouteReadyMarker{}, MarkerNewerVersion, nil
	case err != nil:
		if cadErr := compareAndDeleteReady(ctx, rd, raw); cadErr != nil {
			return raw, placement.RouteReadyMarker{}, "", cadErr
		}
		return raw, placement.RouteReadyMarker{}, MarkerMalformedDropped, nil
	}
	id, err := identity(ctx)
	if err != nil {
		return raw, placement.RouteReadyMarker{}, "", fmt.Errorf("read the mapping Redis identity (INFO server / replication): %w", err)
	}
	if id.RunID != m.RedisRunID || id.MasterReplID != m.MasterReplID {
		return raw, placement.RouteReadyMarker{}, MarkerIdentityMismatch, nil
	}
	return raw, m, MarkerVerified, nil
}

// unverifiedReason 是确认不了缓存时给运维的一句原因。
func unverifiedReason(state string) string {
	switch state {
	case MarkerAbsent:
		return placement.RouteReadyKey + " is absent (no reconcile leader has verified the cache; there is none before batch 1b-2)"
	case MarkerMalformedDropped:
		return placement.RouteReadyKey + " was malformed"
	case MarkerNewerVersion:
		return placement.RouteReadyKey + " has a newer format version than this binary understands (left untouched; use the newer binary)"
	case MarkerWrongType:
		return placement.RouteReadyKey + " is not a string key (readers see it as absent; left untouched)"
	case MarkerIdentityMismatch:
		return placement.RouteReadyKey + " was set against another Redis instance (run_id / master_replid differ: restarted, failed over or replaced)"
	default:
		return placement.RouteReadyKey + " state " + state
	}
}

// compareAndDeleteReady 按原值撤标记;回 0(读到之后被改过)→ ErrReadyMarkerChanged。
func compareAndDeleteReady(ctx context.Context, rd readyMarkerRedis, raw string) error {
	return evalReadyScript(ctx, rd, placement.RouteReadyCompareAndDeleteScript, "compare-and-delete", raw)
}

// compareAndSetReady 按原值换标记;回 0 → ErrReadyMarkerChanged。
func compareAndSetReady(ctx context.Context, rd readyMarkerRedis, raw, next string) error {
	return evalReadyScript(ctx, rd, placement.RouteReadyCompareAndSetScript, "compare-and-set", raw, next)
}

func evalReadyScript(ctx context.Context, rd readyMarkerRedis, script, what string, args ...any) error {
	res, err := rd.EvalCtx(ctx, script, []string{placement.RouteReadyKey}, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w", what, placement.RouteReadyKey, err)
	}
	n, ok := res.(int64)
	switch {
	case !ok:
		return fmt.Errorf("%s %s: unexpected reply %T %v", what, placement.RouteReadyKey, res, res)
	case n == 1:
		return nil
	case n == 0:
		return fmt.Errorf("%w (%s)", ErrReadyMarkerChanged, what)
	default:
		return fmt.Errorf("%s %s: unexpected reply %d", what, placement.RouteReadyKey, n)
	}
}

// isWrongType 判断 err 是不是 Redis 的 WRONGTYPE 错误回复(键不是 string 类型)。
func isWrongType(err error) bool {
	var re goredis.Error
	return errors.As(err, &re) && strings.HasPrefix(re.Error(), "WRONGTYPE")
}
