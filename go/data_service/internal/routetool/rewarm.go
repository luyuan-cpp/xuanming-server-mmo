package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"data_service/internal/store"

	"shared/placement"
)

// rewarm(第 1b-2 批收尾,2026-10-19):撤掉 mapping Redis 上全部三把映射控制键,让 db_first 的对账 leader 从头全量预热、重挂就绪标记
// (docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §2 M2 / M5、§4.6 第 2 项、§13.1 第 4 / 6 条;runbook §9.4)。
//
//  1. 读就绪标记的原值:只作诊断(打到 stderr),也是 -wait 的判据基线;解不开照样往下走;
//  2. **一条** `DEL mapping:route:ready mapping:route:warm mapping:route:watermark`:单条命令在 Redis 里是原子的,
//     不会留下「标记没了、令牌还在」或「标记在、水位键没了」的中间态。
//     - 撤标记:读方(第 3 批)立即不再信任缓存,fail-closed 直到 leader 重挂(S3 规模下全量预热数小时,runbook 醒目提示);
//     - 撤令牌(M2):在途的预热 / 修补任务挂不上标记(挂标记脚本要求令牌仍是本任务的),leader 只能从头预热;
//     - 撤水位键(§13.1 第 4 条):它与被撤的标记属于同一份「缓存截至」的叙述,回退 redis 模式时必须一起清掉;leader 下一拍按库值重建。
//       同时让 leader 的 armingBlocked 闩锁随之解除、按新的基线重新判淘汰(§13.1 第 6 条,那是 leader 一侧的事)。
//  3. -wait > 0:要求 yaml 是 db_first(redis 模式没有 leader,不会有人重挂),且映射写者没冻结(冻结期 leader 停止推送、不会重挂);
//     按 -poll 轮询,直到出现「可解码、且 set_at_ms 大于基线」的新标记 → 退出码 0;到 -wait 还没有 → 退出码 1,
//     提示查 Reconcile.ArmReadyMarker、leader 指标与 arming_refused_* 事件。基线 = 被撤标记的 set_at_ms;被撤时没有可解码的标记,
//     就取 DEL 之前读的库时钟。撤掉令牌之后任何在途任务都挂不上,所以 DEL 之后出现的标记必然出自 DEL 之后才开始的任务;
//     基线挡的是从旧 AOF / RDB 恢复回来的旧标记。
//  4. -wait = 0:只撤(回退 redis 模式时,在读方开关与 ArmReadyMarker 都关掉之后执行;规格 §13.5 第 1 条)。
//
// 标记只有 leader 写(单写者),rewarm 不推送、不挂标记。对全局库只读(-wait > 0 时读 route_meta 与库时钟)。

// rewarmRedis 是 rewarm 对 mapping Redis 的依赖。go-zero 的 *redis.Redis 天然满足(DelCtx 多个键是同一条 DEL)。
type rewarmRedis interface {
	GetCtx(ctx context.Context, key string) (string, error)
	DelCtx(ctx context.Context, keys ...string) (int, error)
}

// rewarmDB 是 rewarm 在 -wait > 0 时对全局库的依赖,*store.RouteStore 天然满足(编译期断言在 rewarm_test.go)。
type rewarmDB interface {
	ReadRouteMeta(ctx context.Context) (store.RouteMeta, error)
	DBNowMs(ctx context.Context) (uint64, error)
}

// rewarmKeys 是一条 DEL 里的三把控制键,顺序固定(单测按这个顺序核对命令参数)。
var rewarmKeys = []string{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey}

// 被撤标记的状态(RewarmReport.MarkerWas 的取值)。缺席 / 版本更新 / 不是 string 与 unfreeze 共用 Marker* 常量。
const (
	// RewarmMarkerDecoded:可解码(不核对身份:撤掉它不需要知道它是否可信)。
	RewarmMarkerDecoded = "decoded"
	// RewarmMarkerMalformed:解不开。
	RewarmMarkerMalformed = "malformed"
)

// ErrRewarmNeedsDBFirst 表示 -wait > 0 而 yaml 是 RouteStore.Mode=redis:redis 模式没有对账 leader,不会有人重挂标记(退出码 2,什么都不动)。
var ErrRewarmNeedsDBFirst = errors.New("route_tool rewarm: -wait needs RouteStore.Mode=db_first (in redis mode no reconcile leader re-arms the marker); use -wait=0")

// ErrRewarmWritersFrozen 表示 -wait > 0 而映射写者是冻结的:冻结期 leader 停止推送、不会重挂(退出码 2,什么都不动)。
var ErrRewarmWritersFrozen = errors.New("route_tool rewarm: the route writers are frozen (route_meta.writable != 1) and the reconcile leader " +
	"does not prewarm while frozen; finish the recovery first (runbook §9.4), or use -wait=0")

// ErrRewarmNotRearmed 表示到 -wait 还没等到新标记(退出码 1:键已经撤了,只是没看到重挂)。
var ErrRewarmNotRearmed = errors.New("route_tool rewarm: the reconcile leader has not re-armed mapping:route:ready within -wait")

// RewarmOptions 是 rewarm 的参数(routetoolcli 的命令行一一对应)。
type RewarmOptions struct {
	// Wait 是等 leader 重挂标记的时长;0 = 只撤不等。不得为负。
	Wait time.Duration
	// Poll 是轮询间隔,Wait > 0 时必须 > 0。
	Poll time.Duration
	// RedisMode:yaml 是 RouteStore.Mode=redis。此时 Wait 必须为 0。
	RedisMode bool
	// Sleep 是轮询之间的等待;nil = 真睡(可被 ctx 取消)。单测注入它,不真睡。
	Sleep func(ctx context.Context, d time.Duration) error
	// Now 是本地单调时钟,用来算 -wait 的截止时间;nil = time.Now(带单调读数)。单测注入假钟(与 Sleep 一起推进)。
	Now func() time.Time
	// Target 非空时作为 stderr 第二行(连接目标,只含 host 与库名)。
	Target string
	// Stdout 只写一行统计;Stderr 写 NOTE、诊断与提示。
	Stdout, Stderr io.Writer
}

func (o RewarmOptions) validate(db rewarmDB) error {
	switch {
	case o.Stdout == nil || o.Stderr == nil:
		return errors.New("stdout / stderr writers are required")
	case o.Wait < 0:
		return fmt.Errorf("wait %s must not be negative", o.Wait)
	case o.Wait > 0 && o.Poll <= 0:
		return fmt.Errorf("poll %s must be positive when waiting", o.Poll)
	case o.Wait > 0 && o.RedisMode:
		return ErrRewarmNeedsDBFirst
	case o.Wait > 0 && db == nil:
		return errors.New("the global DB is required to wait for the re-armed marker")
	}
	return nil
}

// RewarmReport 是一次 rewarm 的结果。
type RewarmReport struct {
	// Deleted 是 DEL 回的删除键数(0..3)。
	Deleted int
	// MarkerWas 是被撤标记的状态(MarkerAbsent / RewarmMarkerDecoded / RewarmMarkerMalformed / MarkerNewerVersion / MarkerWrongType)。
	MarkerWas string
	// Waited:本次是否等了重挂(Wait > 0)。
	Waited bool
	// Rearmed:等到了新标记。
	Rearmed bool
	// BaselineSetAtMs 是判「新标记」的基线(被撤标记的 set_at_ms,或 DEL 之前读的库时钟);不等时为 0。
	BaselineSetAtMs uint64
	// NewSetAtMs 是等到的新标记的 set_at_ms;没等到为 0。
	NewSetAtMs uint64
}

// SummaryLine 是 stdout 唯一的一行。
func (r RewarmReport) SummaryLine() string {
	return fmt.Sprintf("route_rewarm deleted=%d marker_was=%s waited=%t rearmed=%t baseline_set_at_ms=%d new_set_at_ms=%d",
		r.Deleted, r.MarkerWas, r.Waited, r.Rearmed, r.BaselineSetAtMs, r.NewSetAtMs)
}

// RewarmExitCode:等不到新标记 1;其它错误 2(参数、redis 模式下 -wait、冻结中 -wait、连接 / 读 / DEL 出错、超时);否则 0。不用 3。
func RewarmExitCode(_ RewarmReport, err error) int {
	switch {
	case errors.Is(err, ErrRewarmNotRearmed):
		return ExitAttention
	case err != nil:
		return ExitFailed
	default:
		return ExitClean
	}
}

// RewarmNote 是 rewarm 在 stderr 打的第一行。
const RewarmNote = "NOTE: rewarm drops mapping:route:ready, mapping:route:warm and mapping:route:watermark in one DEL; readers with " +
	"RouteStore.ReadRequiresReady / Placement.RequireRouteReady on fail closed until the reconcile " +
	"leader re-arms the marker after a full prewarm (hours at full scale; runbook §9.4)"

// Rewarm 跑一次 rewarm(步骤见文件头)。db 只在 Wait > 0 时用(Wait = 0 可传 nil)。
// DEL 之前的任何失败都不动 Redis;DEL 之后才出的错(轮询期间 ctx 结束)在 stderr 写明「键已撤掉」。
// 撤成功之后(含等不到新标记)才往 Stdout 写统计行。
func Rewarm(ctx context.Context, rd rewarmRedis, db rewarmDB, opts RewarmOptions) (RewarmReport, error) {
	if err := opts.validate(db); err != nil {
		return RewarmReport{}, err
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	fmt.Fprintln(opts.Stderr, RewarmNote)
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	rep := RewarmReport{Waited: opts.Wait > 0}

	if rep.Waited {
		meta, err := db.ReadRouteMeta(ctx)
		if err != nil {
			return rep, fmt.Errorf("read route_meta: %w", err)
		}
		if meta.Frozen() {
			return rep, fmt.Errorf("%w (writable=%d)", ErrRewarmWritersFrozen, meta.Writable)
		}
	}

	raw, marker, state, err := peekReadyMarker(ctx, rd)
	if err != nil {
		return rep, err
	}
	rep.MarkerWas = state
	switch state {
	case RewarmMarkerDecoded:
		fmt.Fprintf(opts.Stderr, "MARKER generation=%s redis_run_id=%s master_replid=%s set_at_ms=%d watermark_ms=%d leader=%s\n",
			marker.Generation, marker.RedisRunID, marker.MasterReplID, marker.SetAtMs, marker.WatermarkMs, marker.Leader)
	case RewarmMarkerMalformed, MarkerNewerVersion:
		fmt.Fprintf(opts.Stderr, "MARKER %s raw=%q\n", state, raw)
	default:
		fmt.Fprintf(opts.Stderr, "MARKER %s\n", state)
	}

	if rep.Waited {
		if state == MarkerNewerVersion {
			// 新版本 leader 重挂的标记本二进制也解不开,等下去必然超时;在任何写之前拒绝。
			return rep, fmt.Errorf("%s has a newer format version than this binary understands, so the re-armed marker could not be "+
				"recognised either; nothing was dropped (use the newer binary, or -wait=0)", placement.RouteReadyKey)
		}
		if state == RewarmMarkerDecoded {
			rep.BaselineSetAtMs = marker.SetAtMs
		} else {
			now, err := db.DBNowMs(ctx)
			if err != nil {
				return rep, fmt.Errorf("read the DB clock (baseline for the re-armed marker): %w", err)
			}
			rep.BaselineSetAtMs = now
		}
	}

	n, err := rd.DelCtx(ctx, rewarmKeys...)
	if err != nil {
		return rep, fmt.Errorf("DEL %v (outcome unknown: the keys may or may not be gone; rerun, it is idempotent): %w", rewarmKeys, err)
	}
	rep.Deleted = n
	fmt.Fprintf(opts.Stderr, "DROPPED %v deleted=%d\n", rewarmKeys, n)

	if !rep.Waited {
		fmt.Fprintln(opts.Stdout, rep.SummaryLine())
		return rep, nil
	}
	newSetAt, err := waitRearmed(ctx, rd, opts, rep.BaselineSetAtMs)
	switch {
	case err == nil:
		rep.Rearmed, rep.NewSetAtMs = true, newSetAt
		fmt.Fprintf(opts.Stderr, "REARMED set_at_ms=%d (baseline %d)\n", newSetAt, rep.BaselineSetAtMs)
	case errors.Is(err, ErrRewarmNotRearmed):
		fmt.Fprintf(opts.Stderr, "TIMEOUT: %v. The keys ARE dropped (readers that require the marker stay fail-closed). Check on every "+
			"data_service replica: RouteStore.Mode=db_first, RouteStore.Reconcile.Enabled and RouteStore.Reconcile.ArmReadyMarker; "+
			"data_service_route_reconcile_leader (exactly one replica at 1) and data_service_route_reconcile_arm_enabled; "+
			"data_service_route_reconcile_event_total{event=~\"arming_refused_.*\"} and {event=\"task_aborted\"}; "+
			"data_service_route_reconcile_push_total{result=~\"conflict|legacy_conflict|db_invalid|error\"}. "+
			"A full prewarm can take hours at full scale: rerun with a longer -wait (rewarm again only drops the keys again).\n", err)
	default:
		fmt.Fprintf(opts.Stderr, "ERROR while waiting: %v. The keys ARE dropped; the leader re-arms on its own, watch "+
			"data_service_route_ready_marker on the current leader\n", err)
		return rep, err
	}
	fmt.Fprintln(opts.Stdout, rep.SummaryLine())
	if !rep.Rearmed {
		return rep, err
	}
	return rep, nil
}

// peekReadyMarker 读一次就绪标记(不核对身份,不写)。返回原值、能解码时的标记与状态。
func peekReadyMarker(ctx context.Context, rd rewarmRedis) (string, placement.RouteReadyMarker, string, error) {
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
		return raw, placement.RouteReadyMarker{}, RewarmMarkerMalformed, nil
	}
	return raw, m, RewarmMarkerDecoded, nil
}

// waitRearmed 按 opts.Poll 轮询就绪标记,直到出现可解码且 set_at_ms > baseline 的标记(返回它的 set_at_ms),
// 或本地单调时钟过了 Wait 的截止时间(ErrRewarmNotRearmed)。轮询本身只读:单次 GET 出错不算失败,记下来继续到截止时间
// (第一次与最后一次的错误进 stderr / 错误信息);ctx 结束(-timeout)才按失败返回。
func waitRearmed(ctx context.Context, rd rewarmRedis, opts RewarmOptions, baseline uint64) (uint64, error) {
	deadline := opts.Now().Add(opts.Wait)
	var (
		polls, getErrors int
		lastErr          error
		staleSeen        bool
	)
	for {
		polls++
		raw, err := rd.GetCtx(ctx, placement.RouteReadyKey)
		switch {
		case err != nil && ctx.Err() != nil:
			return 0, fmt.Errorf("poll %s: %w", placement.RouteReadyKey, ctx.Err())
		case err != nil:
			getErrors++
			lastErr = err
			if getErrors == 1 {
				fmt.Fprintf(opts.Stderr, "WARN: poll GET %s failed (keeps polling until -wait): %v\n", placement.RouteReadyKey, err)
			}
		case raw != "":
			m, derr := placement.DecodeRouteReadyMarker(raw)
			switch {
			case derr != nil:
				// 版本更新 / 畸形:不是本二进制能认的新标记,继续等(leader 会按自己的规则处置畸形值)。
			case m.SetAtMs > baseline:
				return m.SetAtMs, nil
			case !staleSeen:
				staleSeen = true
				fmt.Fprintf(opts.Stderr, "WARN: %s reappeared with set_at_ms=%d <= baseline %d (an old marker restored from AOF / RDB?); "+
					"not counted as re-armed\n", placement.RouteReadyKey, m.SetAtMs, baseline)
			}
		}
		remaining := deadline.Sub(opts.Now())
		if remaining <= 0 {
			if lastErr != nil {
				return 0, fmt.Errorf("%w (%s, %d polls, %d GET errors, last: %v)", ErrRewarmNotRearmed, opts.Wait, polls, getErrors, lastErr)
			}
			return 0, fmt.Errorf("%w (%s, %d polls)", ErrRewarmNotRearmed, opts.Wait, polls)
		}
		if err := opts.Sleep(ctx, min(opts.Poll, remaining)); err != nil {
			return 0, fmt.Errorf("wait between polls: %w", err)
		}
	}
}
