// Package routetoolcli 是 route_tool 的命令行入口:解析参数、装配(配置 → mapping Redis → 全局库 RouteStore)、把结果映射成退出码
// (docs/design/player-route-mapping-durability-proposal.md §7、§11.2;运维步骤见 docs/ops/merge-zone-runbook.md §9.3 / §9.4)。
// 逻辑全在 internal/routetool。
//
// 2026-10-18(第 1b-1 批)从 cmd/route_tool/main.go 原样迁来,为的是并进 data_service 主程序:K8s 镜像只构建单一入口
// /app/service(deploy/k8s/Dockerfile.go-svc),生产上的运行方式是
//
//	data_service -f <yaml> -route-tool <子命令> [子命令参数]
//
// (data_service.go 的 -route-tool;一次性 Job / Pod 跑,见 runbook §9.4)。cmd/route_tool 只留一个薄包装,给 dev 的 `go run`
// 与既有 runbook 命令用,命令行逐字兼容。
//
// 子命令与退出码(0 干净 / 1 需要人工处理 / 2 失败 / 3 有生效的合服围栏):
//   - import-from-redis、audit、tidb-prepare:第 1a 批,语义不变;audit 新增 -frozen-recovery(冻结期恢复口径);
//   - freeze / reconcile-from-cache / unfreeze:第 1b-1 批,全局库回档后的恢复流程(freeze → reconcile-from-cache →
//     audit -frozen-recovery → unfreeze),只碰映射写者的冻结开关与反向修库,不改服务的运行行为;
//   - rewarm 与 audit -require-maprev、unfreeze 的水位键出口:第 1b-2 批收尾(2026-10-19,规格 spec_D1_batch1b2_2_3.md §4.6
//     第 2 / 4 / 5 项、§13.1 第 4 / 6 条)。rewarm 一条 DEL 撤掉就绪标记、预热令牌与水位键,-wait > 0 时等 db_first 的 leader 重挂。
//
// 与 data_service 读同一份 yaml:mapping Redis 永远是 DB 0(go-zero RedisConf 没有 DB 字段;连接带 RedisConf 的 User / Pass / Tls,
// 由 go-zero redis.NewRedis 透传),全局库取 SnapshotMySQL 段,连接池大小取 RouteStore 段;整段仍按服务的口径校验(非法即退出码 2)。
//
// RouteStore.Mode 的门禁(在连任何东西之前判):
//   - import-from-redis:yaml 是 db_first 时真导入默认拒绝,要显式 -allow-db-first(只给提案 §7 P3 那一次);
//   - reconcile-from-cache / audit -frozen-recovery:只在 db_first 下有意义,redis 模式退出码 2 —— 那时 Redis 才是
//     提交点、库只是 P1 时刻的快照,「从缓存修库」就是 import-from-redis 的事;
//   - freeze:redis 模式也允许,但 W1 在 redis 模式下不读 writable,只打 WARN;
//   - unfreeze:redis 模式下也允许(撤销误跑 / 演练的 freeze,或从 db_first 回退时带过来的冻结 —— 否则 import-from-redis 的
//     requireRouteWritable 与模式无关,会一直拒写,工具里没有别的解冻路径),但必须显式 -cache-was-not-verified-acknowledged
//     (redis 模式没有就绪标记可核对,也跑不了 audit -frozen-recovery),不连 mapping Redis、最低水位取 0,打 WARN;
//   - audit -require-maprev:只在 db_first 下有意义(redis 模式没有写者写 maprev,每一行都会判落后),redis 模式退出码 2;
//   - rewarm:-wait=0(只撤)两种模式都允许(回退 redis 模式时就用它清控制键);-wait > 0 只许 db_first(redis 模式没有 leader 重挂)。
//     rewarm 与 db_first 的 unfreeze 一样要求 MappingRedis 是单实例(三把控制键一条 DEL、身份按单实例定义)。
package routetoolcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"data_service/internal/config"
	"data_service/internal/routetool"
	"data_service/internal/store"
	"data_service/internal/svc"
	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	subImport      = "import-from-redis"
	subAudit       = "audit"
	subTiDBPrepare = "tidb-prepare"
	subFreeze      = "freeze"
	subReconcile   = "reconcile-from-cache"
	subUnfreeze    = "unfreeze"
	subRewarm      = "rewarm"

	// defaultRewarmPoll 是 rewarm -wait 期间轮询就绪标记的间隔。
	defaultRewarmPoll = 5 * time.Second

	// redisPingTimeout 是装配时 PING mapping Redis 的上限:地址不可达要快速失败成退出码 2,
	// 不能等到第一条 SCAN 才发现(那时已经连上了全局库)。
	redisPingTimeout = 5 * time.Second

	// defaultFreezeGrace 是 reconcile-from-cache 开始扫描前的等待:冻结之前已经过了开关检查的 W1 仍会提交并发布,
	// 服务端 W1 的预算是 2s(其中加锁等待最多 1s),10s 留足余量。
	defaultFreezeGrace = time.Duration(0)
)

const usage = `用法:
  route_tool -f <data_service.yaml> import-from-redis [-dry-run=true] [-allow-db-first] [-batch=500] [-timeout=30m] [-max-report=200]
  route_tool -f <data_service.yaml> audit [-frozen-recovery] [-require-maprev] [-batch=500] [-timeout=30m] [-max-report=200] [-recheck-delay=2s] [-max-recheck=10000]
  route_tool -f <data_service.yaml> tidb-prepare -zones=1,2[,…] [-storages=1000000[,…]] [-dry-run=true] [-timeout=30m]
  route_tool -f <data_service.yaml> freeze -maintenance-request=<绝对路径> -maintenance-sha256=<摘要> -ticket-id=<票据ID> [-reason=<文本>] [-freeze-timeout=5s] [-timeout=30m]
  route_tool -f <data_service.yaml> reconcile-from-cache -maintenance-request=<FROZEN原件> -maintenance-sha256=<摘要> [-dry-run=true] [-resolve-ties=report|cache] [-freeze-grace=10s] [-batch=500] [-timeout=30m] [-max-report=200]
  route_tool -f <data_service.yaml> unfreeze -maintenance-request=<RELEASE请求> -maintenance-sha256=<摘要> -ticket-id=<票据ID> -confirm-audited [-cache-was-not-verified-acknowledged] [-freeze-timeout=5s] [-timeout=30m]
  route_tool -f <data_service.yaml> rewarm [-wait=0] [-poll=5s] [-timeout=30m]
  实际reconcile及P6 rewarm另需-ticket-id、-drain-runtime-plan、-drain-runtime-directory、-drain-runtime-sha256。
  P6 rewarm同时给正常FROZEN原件且只许-wait=0；无冻结原件的rewarm与实际import使用单次NORMAL boot。
  NORMAL命令可用同组三个runtime参数绑定同boot SOURCE/RELAY；不指定时不产排空原件；SQL_RELEASED拒绝，有限工具不能绕过P9。
  P7独立导出：audit -frozen-recovery -maintenance-request=<正常FROZEN> -maintenance-sha256=<SHA>
    -maintenance-publication=<完整闭合原件> -maintenance-publication-sha256=<SHA> -recovery-audit-output=<新私有原件>。
  (K8s 上同一套子命令走主程序:data_service -f <yaml> -route-tool <子命令> [参数])

退出码:0 干净;1 完成但需要人工处理(unfreeze:就绪标记在途中被改过、或解冻之后水位键没改成,重跑;audit -require-maprev:
redis_rev_behind 非 0;rewarm:-wait 内没等到 leader 重挂);2 失败(参数 / 配置 / 连接 / 查询 / 写入 / 超时,
db_first 下真导入没加 -allow-db-first,redis 模式下跑 reconcile-from-cache / audit -frozen-recovery / audit -require-maprev / rewarm -wait、
或 unfreeze 没加 -cache-was-not-verified-acknowledged,没冻结就修库,unfreeze 没加 -confirm-audited 或确认不了缓存、
水位键版本更新或不是 string,rewarm -wait 时映射写者是冻结的);
3 有生效的合服围栏、或导入期间 merge:merged_into 变过(import-from-redis / reconcile-from-cache)。
tidb-prepare:0 完成或 MySQL 上跳过(MySQL 没有 Region);2 失败。
恢复流程(全局库回档之后,runbook §9.4):freeze → reconcile-from-cache(先 dry-run)→ audit -frozen-recovery(退出码 0)→ unfreeze -confirm-audited。
redis 模式下撤销 freeze:unfreeze -confirm-audited -cache-was-not-verified-acknowledged(不连 mapping Redis,最低水位 0)。
rewarm:一条 DEL 撤掉 mapping:route:ready / warm / watermark;-wait > 0(只许 db_first)时等 leader 全量预热后重挂(要求 -wait < -timeout)。`

// cliArgs 是解析后的命令行。
type cliArgs struct {
	runtimeFile, runtimeDirectory, runtimeSHA            string
	recoveryAuditOutput, publicationFile, publicationSHA string
	writer                                               authoritysql.WriterConfig
	maintenanceFile                                      string
	maintenanceSHA                                       string
	ticketHex                                            string
	configPath                                           string
	sub                                                  string
	dryRun                                               bool
	allowDBFirst                                         bool
	batch                                                int
	timeout                                              time.Duration
	maxReport                                            int
	recheckDelay                                         time.Duration
	maxRecheck                                           int
	// zones / storages 只给 tidb-prepare。
	zones    []uint32
	storages []uint32
	// 第 1b-1 批:audit 的冻结期恢复口径;freeze / unfreeze / reconcile-from-cache 的参数。
	frozenRecovery     bool
	resolveTies        bool
	freezeGrace        time.Duration
	freezeTimeout      time.Duration
	reason             string
	confirmAudited     bool
	cacheUnverifiedAck bool
	// 第 1b-2 批收尾:audit 的严格 maprev 口径;rewarm 的等待与轮询。
	requireMapRev bool
	wait          time.Duration
	poll          time.Duration
}

// routeStore 是本入口对全局库 RouteStore 的全部依赖(*store.RouteStore 天然满足;单测注入假实现)。
type routeStore interface {
	ImportRoutes(ctx context.Context, rows []store.RouteImportRow, dryRun bool) (store.RouteImportStats, error)
	ImportMergedInto(ctx context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error)
	ScanRoutes(ctx context.Context, after store.RouteScanCursor, limit int) ([]store.RouteRow, error)
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
	ListMergedInto(ctx context.Context) ([]store.MergedIntoRow, error)
	PrepareTiDB(ctx context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error)
	ReadRouteMeta(ctx context.Context) (store.RouteMeta, error)
	RepairRoutesFromCache(ctx context.Context, rows []store.RouteImportRow, opts store.RouteRepairOptions) (store.RouteRepairStats, error)
	RepairMergedIntoFromCache(ctx context.Context, rows []store.MergedIntoRow, dryRun bool) (store.MergedIntoImportStats, error)
	FreezeRouteWriters(ctx context.Context) (bool, error)
	UnfreezeRouteWriters(ctx context.Context, minWatermarkMs uint64) (bool, error)
	// ListActiveMergeFences 只给 rewarm -wait 取库时钟(见 routetool.Rewarm)。
	ListActiveMergeFences(ctx context.Context) (store.ActiveMergeFences, error)
	Close() error
}

var _ routeStore = (*store.RouteStore)(nil)

// deps 是装配用到的外部依赖(显式注入,单测替换;生产取 defaultDeps)。
type deps struct {
	resolvePermission func(*cliArgs) (authoritysql.Permission, error)
	openMaintenance   func(context.Context, config.Config, cliArgs) (*maintenanceInput, error)
	openCommand       func(context.Context, config.Config, cliArgs, *maintenanceInput) (*routeCommand, error)
	connectRedis      func(ctx context.Context, rc redis.RedisConf) (routeRedis, error)
	openRouteStore    func(c config.Config, permission authoritysql.Permission, frozen *pb.SqlWriterMaintenanceReceipt) (routeStore, error)
	redisIdentity     func(ctx context.Context, rds routeRedis) (routetool.RedisIdentity, error)
}

func defaultDeps() deps {
	return deps{resolvePermission: permissionForCommand, openMaintenance: openMaintenance, openCommand: openRouteCommand, connectRedis: connectMappingRedis, openRouteStore: openRouteStore, redisIdentity: readRedisIdentity}
}

// Run 是 route_tool 的入口:args 不含程序名(`-f <yaml> <子命令> [参数]`),返回退出码,不调 os.Exit。
// stdout 只写一行统计(给脚本解析),其余全部进 stderr(含 go-zero logx 的输出)。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, defaultDeps())
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) (code int) {
	// 正式入口默认真实解析器；测试只能在同一显式装配接口注入外部系统替身。
	if d.resolvePermission == nil {
		d.resolvePermission = permissionForCommand
	}
	if d.openMaintenance == nil {
		d.openMaintenance = openMaintenance
	}
	if d.openCommand == nil {
		d.openCommand = openRouteCommand
	}
	a, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(stderr, usage)
		return routetool.ExitClean
	}
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n\n%s\n", err, usage)
		return routetool.ExitFailed
	}

	// stdout 只许有一行统计(给脚本解析),而 go-zero logx 默认把 Info 写到 stdout:整体改到 stderr,
	// 并只留 Error 以上 —— import 的 stderr 第一行要留给 NOTE,连接目标由 Target 行给出(只有 host 与库名)。
	logx.SetWriter(logx.NewWriter(stderr))
	logx.SetLevel(logx.ErrorLevel)
	logx.DisableStat()

	// 用 conf.Load 而不是 conf.MustLoad:后者出错时 log.Fatalf 直接退出 1,退出码就不归本工具管了。
	var c config.Config
	if err := conf.Load(a.configPath, &c); err != nil {
		fmt.Fprintf(stderr, "route_tool: load %s: %v\n", a.configPath, err)
		return routetool.ExitFailed
	}
	c.RouteStore = c.RouteStore.Normalize()
	if err := c.RouteStore.Validate(); err != nil {
		fmt.Fprintf(stderr, "route_tool: invalid RouteStore config in %s: %v\n", a.configPath, err)
		return routetool.ExitFailed
	}
	if code, refused := refuseByMode(a, c, stderr); refused {
		return code
	}

	if a.recoveryAuditOutput != "" {
		return runRouteRecoveryAudit(ctx, a, c, d, stdout, stderr)
	}

	permission, err := d.resolvePermission(&a)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	defer a.writer.Seal()
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	var maintenance *maintenanceInput
	if a.sub == subFreeze || a.sub == subUnfreeze || a.sub == subReconcile || a.frozenRecovery || (a.sub == subRewarm && a.maintenanceFile != "") {
		maintenance, err = d.openMaintenance(ctx, c, a)
		if err != nil {
			fmt.Fprintf(stderr, "route_tool: %v\n", err)
			return routetool.ExitFailed
		}
	}
	command, err := d.openCommand(ctx, c, a, maintenance)
	if err != nil {
		if maintenance != nil {
			err = errors.Join(err, maintenance.db.Close())
		}
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	ctx = context.WithValue(ctx, routeCommandKey{}, command)
	done, err := command.session.Enter()
	if err != nil {
		command.hold()
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), redisPingTimeout)
		err = errors.Join(err, command.finish(finishCtx))
		finishCancel()
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	defer func() {
		panicValue := recover()
		if panicValue != nil || code == routetool.ExitFailed {
			command.hold()
		}
		done()
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), redisPingTimeout)
		defer finishCancel()
		if err := command.finish(finishCtx); err != nil {
			fmt.Fprintf(stderr, "route_tool: resources/closure HOLD: %v\n", err)
			code = routetool.ExitFailed
		}
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	// 所有业务调用同步返回；资源只由同一装配owner在最终关闭线性化点释放。
	openStore := d.openRouteStore
	d.openRouteStore = func(c config.Config, p authoritysql.Permission, f *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
		st, err := openStore(c, p, f)
		if err == nil {
			command.resources = append(command.resources, st)
		}
		return st, err
	}
	connectRedis := d.connectRedis
	d.connectRedis = func(ctx context.Context, rc redis.RedisConf) (routeRedis, error) {
		rd, err := connectRedis(ctx, rc)
		if err == nil {
			command.resources = append(command.resources, rd)
		}
		return rd, err
	}
	d.openMaintenance = func(context.Context, config.Config, cliArgs) (*maintenanceInput, error) { return maintenance, nil }
	// 此后所有返回都会进入上面的有界收尾；panic同样留下UNKNOWN而不能发成功原件。

	// tidb-prepare 与 freeze 只碰全局库:在连 mapping Redis 之前分流(Redis 不可达不该挡住预切与冻结 —— 冻结是止血动作)。
	// redis 模式的 unfreeze 同样只碰全局库:那时没有就绪标记可读(refuseByMode 已要求显式确认)。
	// rewarm 先连 mapping Redis,只有 -wait > 0 才开全局库(-wait=0 用于回退 redis 模式,不该被全局库挡住)。
	switch {
	case a.sub == subTiDBPrepare:
		return runTiDBPrepare(ctx, a, c, d, permission, stdout, stderr)
	case a.sub == subFreeze:
		return runFreeze(ctx, a, c, d, stdout, stderr)
	case a.sub == subUnfreeze && !c.RouteStore.DBFirst():
		return runUnfreezeRedisMode(ctx, a, c, d, stdout, stderr)
	case a.sub == subRewarm:
		return runRewarm(ctx, a, c, d, permission, stdout, stderr)
	}

	rds, err := d.connectRedis(ctx, c.MappingRedis)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	var frozen *pb.SqlWriterMaintenanceReceipt
	if a.sub == subReconcile {
		frozen = maintenance.request
	}
	st, err := d.openRouteStore(c, permission, frozen)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: cannot open the route tables on the global DB (%s): %v\n", store.OpenRouteStoreHint(a.configPath, err), err)
		return routetool.ExitFailed
	}

	// 只打 host 与库名,不打 DSN / 口令。
	target := fmt.Sprintf("route_tool %s: mapping_redis=%s global_db=%s/%s", a.sub, c.MappingRedis.Host, c.SnapshotMySQL.Host, c.SnapshotMySQL.DBName)

	switch a.sub {
	case subImport:
		rep, err := routetool.Import(ctx, rds, st, routetool.ImportOptions{
			DryRun:    a.dryRun,
			Batch:     a.batch,
			MaxReport: a.maxReport,
			Target:    target,
			Stdout:    stdout,
			Stderr:    stderr,
		})
		if err != nil {
			fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
		}
		return routetool.ImportExitCode(rep, err)
	case subReconcile:
		rep, err := routetool.ReconcileFromCache(ctx, rds, st, routetool.RepairOptions{
			Maintenance:   command.session,
			Catalog:       maintenance.catalog,
			FrozenReceipt: maintenance.request,
			DryRun:        a.dryRun,
			ResolveTies:   a.resolveTies,
			Batch:         a.batch,
			MaxReport:     a.maxReport,
			FreezeGrace:   a.freezeGrace,
			Target:        target,
			Stdout:        stdout,
			Stderr:        stderr,
		})
		if err != nil {
			fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
		}
		return routetool.RepairExitCode(rep, err)
	case subUnfreeze:
		rep, err := routetool.Unfreeze(ctx, rds, st, routetool.UnfreezeOptions{
			Catalog: maintenance.catalog, TicketID: maintenance.ticketID, Request: maintenance.request,
			CacheUnverifiedAcknowledged: a.cacheUnverifiedAck,
			Timeout:                     a.freezeTimeout,
			Identity:                    func(ctx context.Context) (routetool.RedisIdentity, error) { return d.redisIdentity(ctx, rds) },
			Target:                      target,
			Stdout:                      stdout,
			Stderr:                      stderr,
		})
		if err != nil {
			fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
		}
		return routetool.UnfreezeExitCode(rep, err)
	}

	// audit。冻结期恢复口径只在映射写者真的冻结时成立:可写时库独有的行与「Redis 丢了已确认的发布」分不开,
	// 放过 db_only_unpublished 就是放过真丢失。
	if a.frozenRecovery {
		meta, err := st.ReadRouteMeta(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "route_tool %s: read route_meta: %v\n", a.sub, err)
			return routetool.ExitFailed
		}
		if !meta.Frozen() {
			fmt.Fprintf(stderr, "route_tool %s: -frozen-recovery only applies while the route writers are frozen "+
				"(route_meta.writable=%d); run `freeze` first or drop the flag\n", a.sub, meta.Writable)
			return routetool.ExitFailed
		}
	}
	rep, err := routetool.Audit(ctx, rds, st, routetool.AuditOptions{
		Batch:          a.batch,
		MaxReport:      a.maxReport,
		RecheckDelay:   a.recheckDelay,
		MaxRecheck:     a.maxRecheck,
		FrozenRecovery: a.frozenRecovery,
		RequireMapRev:  a.requireMapRev,
		Target:         target,
		Stdout:         stdout,
		Stderr:         stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
	}
	return routetool.AuditExitCode(rep, err)
}

// refuseByMode 是 RouteStore.Mode 的门禁(见包注释),在连任何东西之前判。refused=true 时按 code 退出。
func refuseByMode(a cliArgs, c config.Config, stderr io.Writer) (int, bool) {
	if a.sub == subRewarm && a.maintenanceFile != "" && a.wait > 0 {
		fmt.Fprintln(stderr, "route_tool rewarm: P6冻结期只允许-wait=0；leader重挂属于释放后的NORMAL流程")
		return routetool.ExitFailed, true
	}
	// db_first 下真导入是反方向的回填,只有提案 §7 P3 那一次是合法的。
	if a.sub == subImport && !a.dryRun && c.RouteStore.DBFirst() && !a.allowDBFirst {
		fmt.Fprintf(stderr, "route_tool %s: %s has RouteStore.Mode=%s — the global DB is the source of truth, "+
			"so filling it from the cache runs the wrong way; a real import is refused. Only the proposal §7 P3 re-import "+
			"(after every replica switched) may pass -allow-db-first (merge-zone-runbook §9.3). Dry-run and audit are unaffected.\n",
			a.sub, a.configPath, c.RouteStore.Mode)
		return routetool.ExitFailed, true
	}
	needsDBFirst := a.sub == subReconcile || (a.sub == subAudit && a.frozenRecovery)
	if needsDBFirst && !c.RouteStore.DBFirst() {
		what := a.sub
		if a.sub == subAudit {
			what = "audit -frozen-recovery"
		}
		fmt.Fprintf(stderr, "route_tool %s: %s has RouteStore.Mode=%s — in redis mode the cache is the commit point and the DB is only "+
			"the P1 snapshot, so there is nothing to repair the DB from and no frozen recovery; use import-from-redis (merge-zone-runbook §9.3)\n",
			what, a.configPath, c.RouteStore.Mode)
		return routetool.ExitFailed, true
	}
	// audit -require-maprev 是 P3「挂标记之前」的严格口径:redis 模式下没有任何写者写 player:maprev,每一行都会判落后,结论没有意义。
	if a.sub == subAudit && a.requireMapRev && !c.RouteStore.DBFirst() {
		fmt.Fprintf(stderr, "route_tool audit -require-maprev: %s has RouteStore.Mode=%s — in redis mode no writer maintains player:maprev, "+
			"so every row would count as redis_rev_behind; the strict maprev audit is the proposal §7 P3 gate under db_first (merge-zone-runbook §9.3)\n",
			a.configPath, c.RouteStore.Mode)
		return routetool.ExitFailed, true
	}
	// rewarm -wait 等的是对账 leader 重挂标记,redis 模式没有 leader(routetool.Rewarm 再兜一次)。
	if a.sub == subRewarm && a.wait > 0 && !c.RouteStore.DBFirst() {
		fmt.Fprintf(stderr, "route_tool rewarm: %s has RouteStore.Mode=%s — no reconcile leader runs in redis mode, so nothing re-arms "+
			"mapping:route:ready; rerun with -wait=0 to only drop the control keys (merge-zone-runbook §9.4)\n",
			a.configPath, c.RouteStore.Mode)
		return routetool.ExitFailed, true
	}
	// 三把控制键一条 DEL(多键命令)、身份按单实例定义:cluster 下 DEL 会 CROSSSLOT,控制键也从来不会写在 cluster 上。
	if a.sub == subRewarm && c.MappingRedis.Type != "" && c.MappingRedis.Type != redis.NodeType {
		fmt.Fprintf(stderr, "route_tool rewarm: MappingRedis.Type=%s; the mapping control keys live on a single instance (Type=node)\n",
			c.MappingRedis.Type)
		return routetool.ExitFailed, true
	}
	// redis 模式的 unfreeze 是撤销冻结的唯一工具路径(见包注释),但核对不了任何东西:yaml 若是旧的、服务其实已在 db_first,
	// 这里放行就绕过了就绪标记与恢复口径审计。所以要运维显式确认,而且在连任何东西之前判。
	if a.sub == subUnfreeze && !c.RouteStore.DBFirst() && !a.cacheUnverifiedAck {
		fmt.Fprintf(stderr, "route_tool %s: %s has RouteStore.Mode=%s — in redis mode there is no ready marker to verify and no "+
			"audit -frozen-recovery; unfreezing only re-enables import-from-redis (and the db_first writers after a later switch). "+
			"If every data_service replica really runs RouteStore.Mode=redis (undoing a freeze run by mistake or in a drill, or one carried "+
			"over from db_first after falling back), rerun with -cache-was-not-verified-acknowledged; if any replica runs db_first, "+
			"use that yaml instead (merge-zone-runbook §9.4)\n",
			a.sub, a.configPath, c.RouteStore.Mode)
		return routetool.ExitFailed, true
	}
	// 就绪标记与 INFO 身份都按单实例定义(多键脚本、run_id);cluster 下 INFO 落到哪个节点不确定,确认不了身份。
	// redis 模式的 unfreeze 不读标记、不连 mapping Redis,不受这条限制。
	if a.sub == subUnfreeze && c.RouteStore.DBFirst() && c.MappingRedis.Type != "" && c.MappingRedis.Type != redis.NodeType {
		fmt.Fprintf(stderr, "route_tool %s: MappingRedis.Type=%s; the ready marker and the Redis identity check need a single instance (Type=node)\n",
			a.sub, c.MappingRedis.Type)
		return routetool.ExitFailed, true
	}
	return 0, false
}

// runTiDBPrepare 是 tidb-prepare 的装配:只开全局库的 RouteStore(探针照跑:TiDB 上同时证明悲观事务与
// chk_route_shard 已就绪、模数与本二进制一致),不连 mapping Redis。失败原因由 routetool.TiDBPrepare 打在 stderr(FAILED 行)。
func runTiDBPrepare(ctx context.Context, a cliArgs, c config.Config, d deps, permission authoritysql.Permission, stdout, stderr io.Writer) int {
	st, err := d.openRouteStore(c, permission, nil)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: cannot open the route tables on the global DB (%s): %v\n", store.OpenRouteStoreHint(a.configPath, err), err)
		return routetool.ExitFailed
	}

	// 只打 host 与库名,不打 DSN / 口令。
	target := fmt.Sprintf("route_tool %s: global_db=%s/%s", a.sub, c.SnapshotMySQL.Host, c.SnapshotMySQL.DBName)
	rep, err := routetool.TiDBPrepare(ctx, st, routetool.TiDBPrepareOptions{
		Zones:    a.zones,
		Storages: a.storages,
		DryRun:   a.dryRun,
		Target:   target,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	return routetool.TiDBPrepareExitCode(rep, err)
}

// runFreeze 是 freeze 的装配:只开全局库,不连 mapping Redis。
func runFreeze(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) int {
	maintenance, err := d.openMaintenance(ctx, c, a)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	rep, err := routetool.Freeze(ctx, nil, routetool.FreezeOptions{Catalog: maintenance.catalog, TicketID: maintenance.ticketID, Request: maintenance.request, Reason: a.reason, Timeout: a.freezeTimeout, RedisMode: !c.RouteStore.DBFirst(), Stdout: stdout, Stderr: stderr})
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
	}
	return routetool.FreezeExitCode(rep, err)
}

// runUnfreezeRedisMode 是 redis 模式下 unfreeze 的装配:只开全局库,不连 mapping Redis(那时没有就绪标记语义,
// 规格「redis 模式不碰标记」)。显式确认已由 refuseByMode 要求过。
func runUnfreezeRedisMode(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) int {
	maintenance, err := d.openMaintenance(ctx, c, a)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	st, err := d.openRouteStore(c, authoritysql.Reader(), nil)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	rep, err := routetool.Unfreeze(ctx, nil, st, routetool.UnfreezeOptions{Catalog: maintenance.catalog, TicketID: maintenance.ticketID, Request: maintenance.request, CacheUnverifiedAcknowledged: a.cacheUnverifiedAck, RedisMode: true, Timeout: a.freezeTimeout, Stdout: stdout, Stderr: stderr})
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
	}
	return routetool.UnfreezeExitCode(rep, err)
}

// runRewarm 是 rewarm 的装配:连 mapping Redis;只有 -wait > 0 才开全局库(读 route_meta 与库时钟)。
// 模式与 MappingRedis.Type 的门禁已由 refuseByMode 判过(-wait > 0 只许 db_first)。
func runRewarm(ctx context.Context, a cliArgs, c config.Config, d deps, permission authoritysql.Permission, stdout, stderr io.Writer) int {
	rds, err := d.connectRedis(ctx, c.MappingRedis)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	opts := routetool.RewarmOptions{
		Wait:      a.wait,
		Poll:      a.poll,
		RedisMode: !c.RouteStore.DBFirst(),
		Target:    fmt.Sprintf("route_tool %s: mapping_redis=%s", a.sub, c.MappingRedis.Host),
		Stdout:    stdout,
		Stderr:    stderr,
	}
	var rep routetool.RewarmReport
	if a.wait == 0 {
		rep, err = routetool.Rewarm(ctx, rds, nil, opts)
	} else {
		st, openErr := d.openRouteStore(c, permission, nil)
		if openErr != nil {
			fmt.Fprintf(stderr, "route_tool: cannot open the route tables on the global DB (%s): %v\n", store.OpenRouteStoreHint(a.configPath, openErr), openErr)
			return routetool.ExitFailed
		}
		// 只打 host 与库名,不打 DSN / 口令。
		opts.Target += fmt.Sprintf(" global_db=%s/%s", c.SnapshotMySQL.Host, c.SnapshotMySQL.DBName)
		rep, err = routetool.Rewarm(ctx, rds, st, opts)
	}
	if err != nil {
		fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
	}
	return routetool.RewarmExitCode(rep, err)
}

// parseArgs 解析 `-f <yaml> <子命令> [子命令参数]`。子命令只认七个,参数按子命令登记(别的子命令的参数一律
// 「flag provided but not defined」);参数越界、多余参数一律报错(退出码 2)。
func parseArgs(args []string, stderr io.Writer) (cliArgs, error) {
	a := cliArgs{
		dryRun:        true,
		batch:         500,
		timeout:       30 * time.Minute,
		maxReport:     200,
		recheckDelay:  2 * time.Second,
		maxRecheck:    10000,
		freezeGrace:   defaultFreezeGrace,
		freezeTimeout: store.RouteFreezeTimeoutMin,
		poll:          defaultRewarmPoll,
	}

	global := flag.NewFlagSet("route_tool", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.StringVar(&a.configPath, "f", "", "data_service 的 yaml(与服务读同一份)")
	if err := global.Parse(args); err != nil {
		return a, err
	}
	if a.configPath == "" {
		return a, errors.New("missing -f <data_service.yaml>")
	}
	rest := global.Args()
	allSubs := strings.Join([]string{subImport, subAudit, subTiDBPrepare, subFreeze, subReconcile, subUnfreeze, subRewarm}, " | ")
	if len(rest) == 0 {
		return a, fmt.Errorf("missing subcommand (%s)", allSubs)
	}
	a.sub = rest[0]

	sub := flag.NewFlagSet("route_tool "+a.sub, flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.DurationVar(&a.timeout, "timeout", a.timeout, "整次运行的时限;超时退出码 2(导入与修库幂等,重跑即可)")
	if a.sub == subImport || a.sub == subReconcile || a.sub == subRewarm {
		sub.StringVar(&a.runtimeFile, "drain-runtime-plan", "", "P0预列单boot runtime原件绝对路径")
		sub.StringVar(&a.runtimeDirectory, "drain-runtime-directory", "", "同boot私有控制目录")
		sub.StringVar(&a.runtimeSHA, "drain-runtime-sha256", "", "runtime原件固定SHA-256")
	}
	if a.sub == subFreeze || a.sub == subUnfreeze || a.sub == subReconcile || a.sub == subAudit || a.sub == subRewarm {
		sub.StringVar(&a.maintenanceFile, "maintenance-request", "", "绝对路径的正式维护原件（protojson）")
		sub.StringVar(&a.maintenanceSHA, "maintenance-sha256", "", "原件文件固定 SHA-256")
	}
	var zonesRaw, storagesRaw string
	resolveTies := "report"
	switch a.sub {
	case subImport, subAudit, subReconcile:
		sub.IntVar(&a.batch, "batch", a.batch, fmt.Sprintf("一批的玩家数(SCAN COUNT / MGET / 库批量),[1, %d]", store.RouteBatchLimit))
		sub.IntVar(&a.maxReport, "max-report", a.maxReport, "stderr 明细行上限;stdout 的计数始终精确")
	}
	switch a.sub {
	case subFreeze, subUnfreeze:
		sub.StringVar(&a.ticketHex, "ticket-id", "", "正式票据 ID（32 位十六进制）")
		sub.DurationVar(&a.freezeTimeout, "freeze-timeout", a.freezeTimeout,
			fmt.Sprintf("冻结 / 解冻写本身的时限,至少 %s;TiDB 把 route_meta 设成缓存表后要 >= tidb_table_cache_lease + 2s", store.RouteFreezeTimeoutMin))
	}
	if a.sub == subReconcile || a.sub == subRewarm {
		sub.StringVar(&a.ticketHex, "ticket-id", "", "同NORMAL已消费冻结票据ID（P6必填）")
	}
	switch a.sub {
	case subImport:
		sub.BoolVar(&a.dryRun, "dry-run", a.dryRun, "只判定不写库;真导入用 -dry-run=false")
		sub.BoolVar(&a.allowDBFirst, "allow-db-first", a.allowDBFirst,
			"yaml 是 RouteStore.Mode=db_first 时仍做真导入(只用于提案 §7 P3 那一次;默认拒绝)")
	case subAudit:
		sub.StringVar(&a.recoveryAuditOutput, "recovery-audit-output", "", "显式P7：实际只读审核完成并关闭资源后CreateNew正式原件")
		sub.StringVar(&a.publicationFile, "maintenance-publication", "", "同P0维护工具全集实际闭合原件绝对路径")
		sub.StringVar(&a.publicationSHA, "maintenance-publication-sha256", "", "维护闭合原件固定SHA-256")
		sub.DurationVar(&a.recheckDelay, "recheck-delay", a.recheckDelay, "复核前的等待(给途中的 W1 走完)")
		sub.IntVar(&a.maxRecheck, "max-recheck", a.maxRecheck, "复核的玩家数上限;超过则不复核、全部计入")
		sub.BoolVar(&a.frozenRecovery, "frozen-recovery", a.frozenRecovery,
			"冻结期恢复口径:db_only_unpublished 不计入退出码,其余类别必须为 0(只在 db_first 且已冻结时可用;runbook §9.4)")
		sub.BoolVar(&a.requireMapRev, "require-maprev", a.requireMapRev,
			"严格 maprev 口径:缓存 maprev 缺席或小于库 rev 计 redis_rev_behind 并计入退出码(只在 db_first 下可用;提案 §7 P3 挂标记之前;runbook §9.3)")
	case subTiDBPrepare:
		sub.StringVar(&zonesRaw, "zones", "", fmt.Sprintf("要预切的 zone,逗号分隔(必填),每个在 [1, %d]", store.RouteHomeZoneMax))
		sub.StringVar(&storagesRaw, "storages", "", "idx_route_storage 要预切的落点值,逗号分隔(可空;Phase 2 PinOnCreate 指向 1000000 时传 1000000)")
		sub.BoolVar(&a.dryRun, "dry-run", a.dryRun, "只判引擎、核对聚簇、列出语句;真执行用 -dry-run=false")
	case subFreeze:
		sub.StringVar(&a.reason, "reason", "", "冻结原因,只打到 stderr(库里没有记原因的列)")
	case subReconcile:
		sub.BoolVar(&a.dryRun, "dry-run", a.dryRun, "只判定不写库;真修库用 -dry-run=false")
		sub.StringVar(&resolveTies, "resolve-ties", resolveTies, "同 rev 而值不同时:report 只报告(默认),cache 用缓存覆盖")
		sub.DurationVar(&a.freezeGrace, "freeze-grace", a.freezeGrace, "兼容参数；不参与正式排空判断，必须提供维护原件")
	case subUnfreeze:
		sub.BoolVar(&a.confirmAudited, "confirm-audited", a.confirmAudited,
			"确认已在冻结状态下跑过 `audit -frozen-recovery` 且退出码为 0(必填)")
		sub.BoolVar(&a.cacheUnverifiedAck, "cache-was-not-verified-acknowledged", a.cacheUnverifiedAck,
			"没有可信的就绪标记时仍解冻:先按 merge-zone-runbook §9.2 重放清单,再显式加它;redis 模式下撤销 freeze 也必须加它(runbook §9.4)")
	case subRewarm:
		sub.DurationVar(&a.wait, "wait", a.wait,
			"撤键之后等 leader 重挂就绪标记的时长;0 = 只撤不等(回退 redis 模式时用);> 0 只许 db_first,且必须小于 -timeout")
		sub.DurationVar(&a.poll, "poll", a.poll, "-wait 期间轮询就绪标记的间隔(> 0)")
	default:
		return a, fmt.Errorf("unknown subcommand %q (want %s)", a.sub, allSubs)
	}
	if err := sub.Parse(rest[1:]); err != nil {
		return a, err
	}
	if sub.NArg() > 0 {
		return a, fmt.Errorf("unexpected arguments after %s: %v", a.sub, sub.Args())
	}
	switch a.sub {
	case subTiDBPrepare:
		if zonesRaw == "" {
			return a, errors.New("tidb-prepare: missing -zones=<zone>[,<zone>…]")
		}
		var err error
		if a.zones, err = parseIDList("zones", zonesRaw, 1, uint64(store.RouteHomeZoneMax)); err != nil {
			return a, err
		}
		if a.storages, err = parseIDList("storages", storagesRaw, 1, math.MaxUint32); err != nil {
			return a, err
		}
	case subReconcile:
		switch resolveTies {
		case "report":
		case "cache":
			a.resolveTies = true
		default:
			return a, fmt.Errorf("-resolve-ties=%s: want report or cache", resolveTies)
		}
	case subUnfreeze:
		if !a.confirmAudited {
			return a, errors.New("unfreeze: missing -confirm-audited (run `audit -frozen-recovery` while frozen first; it must exit 0)")
		}
	}

	switch {
	case a.batch < 1 || a.batch > store.RouteBatchLimit:
		return a, fmt.Errorf("-batch=%d not in [1, %d]", a.batch, store.RouteBatchLimit)
	case a.timeout <= 0:
		return a, fmt.Errorf("-timeout=%s must be positive", a.timeout)
	case a.maxReport < 0:
		return a, fmt.Errorf("-max-report=%d must not be negative", a.maxReport)
	case a.recheckDelay < 0:
		return a, fmt.Errorf("-recheck-delay=%s must not be negative", a.recheckDelay)
	case a.maxRecheck < 0:
		return a, fmt.Errorf("-max-recheck=%d must not be negative", a.maxRecheck)
	case a.freezeGrace < 0:
		return a, fmt.Errorf("-freeze-grace=%s must not be negative", a.freezeGrace)
	case a.freezeTimeout < store.RouteFreezeTimeoutMin:
		return a, fmt.Errorf("-freeze-timeout=%s must be at least %s", a.freezeTimeout, store.RouteFreezeTimeoutMin)
	case a.wait < 0:
		return a, fmt.Errorf("-wait=%s must not be negative", a.wait)
	case a.wait > 0 && a.poll <= 0:
		return a, fmt.Errorf("-poll=%s must be positive when -wait is set", a.poll)
	case a.wait > 0 && a.wait >= a.timeout:
		// -timeout 先到就是退出码 2(整次运行超时),看不出是「没等到重挂」:要求 -wait 留出余量。
		return a, fmt.Errorf("-wait=%s must be shorter than -timeout=%s (raise -timeout for a long prewarm)", a.wait, a.timeout)
	}
	if a.recoveryAuditOutput != "" || a.publicationFile != "" || a.publicationSHA != "" {
		if a.sub != subAudit || !a.frozenRecovery || a.requireMapRev || !filepath.IsAbs(a.recoveryAuditOutput) || !filepath.IsAbs(a.publicationFile) || a.timeout > 30*time.Minute {
			return a, errors.New("P7导出仅支持audit -frozen-recovery、非require-maprev，且需绝对输出/闭合原件与至多30m预算")
		}
		if value, err := hex.DecodeString(a.publicationSHA); err != nil || len(value) != 32 {
			return a, errors.New("P7维护闭合原件SHA-256无效")
		}
	}
	if a.runtimeFile != "" || a.runtimeDirectory != "" || a.runtimeSHA != "" {
		if !filepath.IsAbs(a.runtimeFile) || !filepath.IsAbs(a.runtimeDirectory) {
			return a, errors.New("runtime必须同时指定绝对路径原件、私有目录与固定SHA-256")
		}
		if value, err := hex.DecodeString(a.runtimeSHA); err != nil || len(value) != 32 {
			return a, errors.New("runtime SHA-256无效")
		}
	}
	if a.sub == subRewarm && (a.maintenanceFile != "" || a.maintenanceSHA != "" || a.ticketHex != "") && a.maintenanceFile == "" {
		return a, errors.New("P6 rewarm必须同时指定正常冻结原件、摘要和同票据")
	}
	p6 := (a.sub == subReconcile && !a.dryRun) || (a.sub == subRewarm && a.maintenanceFile != "")
	if p6 && a.runtimeFile == "" {
		return a, errors.New("P6恢复必须绑定P0预列runtime及同NORMAL票据")
	}
	if a.sub == subFreeze || a.sub == subUnfreeze || a.sub == subReconcile || a.frozenRecovery || (a.sub == subRewarm && a.maintenanceFile != "") {
		if !filepath.IsAbs(a.maintenanceFile) || len(a.maintenanceSHA) != 64 {
			return a, errors.New("维护操作必须指定原件绝对路径与固定 SHA-256")
		}
	}
	if a.sub == subFreeze || a.sub == subUnfreeze || (a.sub == subReconcile && !a.dryRun) || (a.sub == subRewarm && a.maintenanceFile != "") {
		if value, err := hex.DecodeString(a.ticketHex); err != nil || len(value) != 16 {
			return a, errors.New("维护操作缺少正式票据 ID")
		}
	}
	return a, nil
}

// parseIDList 解析逗号分隔的十进制 id 列表(-zones / -storages):空串 = 空列表;拒空项("1,"、",1")、
// 非数字、越界(每项必须在 [lo, hi])。去重排序不在这里做 —— 那是 store.PrepareTiDB 的规范化(一处口径)。
func parseIDList(name, raw string, lo, hi uint64) ([]uint32, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]uint32, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("-%s=%s has an empty item", name, raw)
		}
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("-%s=%s: %q is not a decimal number: %v", name, raw, p, err)
		}
		if v < lo || v > hi {
			return nil, fmt.Errorf("-%s=%s: %d not in [%d, %d]", name, raw, v, lo, hi)
		}
		out = append(out, uint32(v))
	}
	return out, nil
}

// connectMappingRedis 连 mapping Redis 并 PING 一次。用 redis.NewRedis 而不是 MustNewRedis:
// 后者出错时 logx.Must 直接退出进程,退出码就不归本工具管了。RedisConf 的 User(ACL 用户名)/ Pass / Tls 由 NewRedis 透传。
func connectMappingRedis(ctx context.Context, rc redis.RedisConf) (routeRedis, error) {
	if rc.Host == "" || (rc.Type != "" && rc.Type != redis.NodeType && rc.Type != redis.ClusterType) {
		return nil, errors.New("mapping Redis目标或类型无效")
	}
	opts := &goredis.UniversalOptions{Addrs: strings.Split(rc.Host, ","), Username: rc.User, Password: rc.Pass, DB: 0, Protocol: 2, MaxRetries: -1, MaxRedirects: -1, DialerRetries: 1, DialTimeout: redisPingTimeout, ReadTimeout: redisPingTimeout, WriteTimeout: redisPingTimeout, PoolTimeout: redisPingTimeout, ContextTimeoutEnabled: true, DisableIdentity: true, MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}}
	if rc.Tls {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	var client goredis.UniversalClient
	command := routeCommandFrom(ctx)
	if rc.Type == redis.ClusterType {
		clusterOpts := opts.Cluster()
		// 拓扑重读和每个节点的实际dial也绑同一owner，不能只统计cluster最外层命令。
		clusterOpts.NewClient = func(opts *goredis.Options) *goredis.Client {
			node := goredis.NewClient(opts)
			if command != nil {
				node.AddHook(routeRedisHook{command: command})
			}
			return node
		}
		client = goredis.NewClusterClient(clusterOpts)
	} else {
		client = goredis.NewClient(opts.Simple())
	}
	if command != nil {
		client.AddHook(routeRedisHook{command: command})
	}
	rds := &ownedRouteRedis{client: client}
	pingCtx, cancel := context.WithTimeout(ctx, redisPingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("mapping redis %s unreachable (PING failed within %s): %w", rc.Host, redisPingTimeout, err), client.Close())
	}
	return rds, nil
}

// openRouteStore沿正式只读启动核验开池：会话、表形状、CHECK和writer authority原件不发DML。
// audit/dry-run使用Reader；tidb-prepare实际DDL仅用Schema，恢复写另绑定永久正常冻结。
func openRouteStore(c config.Config, permission authoritysql.Permission, frozen *pb.SqlWriterMaintenanceReceipt) (routeStore, error) {
	cfg := svc.MySQLConfigOf(c)
	cfg.Permission = permission
	cfg.MaxOpenConn = c.RouteStore.MaxOpenConn
	cfg.MaxIdleConn = c.RouteStore.MaxIdleConn
	host, _ := os.Hostname() // 取不到时 NewRouteWriterID 用 "unknown"
	st, err := store.NewRouteStore(cfg, store.RouteStoreOptions{
		FenceShards:       uint32(c.RouteStore.FenceShards),
		Writer:            store.NewRouteWriterID("route_tool", host),
		MaintenanceFreeze: frozen,
	})
	if err != nil {
		// 不能把 nil 的 *store.RouteStore 包进接口返回:那是「非 nil 接口包着 nil 指针」,调用方的判空拦不住。
		return nil, err
	}
	return st, nil
}

// readRedisIdentity 用 INFO server / INFO replication 读 mapping Redis 的身份(go-zero 没有 Info 方法,只能借 pipeline)。
// pipeline 返回任何错误都算读不到:包括 go-zero 熔断打开时整批没执行(命令的 Err() 为 nil、值为空,只有返回值里有
// breaker.ErrServiceUnavailable)—— 身份读不准时宁可确认不了(fail-closed)。
func readRedisIdentity(ctx context.Context, rds routeRedis) (routetool.RedisIdentity, error) {
	var server, replication *goredis.StringCmd
	pipeErr := rds.PipelinedCtx(ctx, func(p redis.Pipeliner) error {
		server = p.Info(ctx, "server")
		replication = p.Info(ctx, "replication")
		return nil
	})
	if pipeErr != nil {
		return routetool.RedisIdentity{}, fmt.Errorf("INFO server / replication: %w", pipeErr)
	}
	if server == nil || replication == nil {
		return routetool.RedisIdentity{}, errors.New("INFO server / replication: pipeline callback was not invoked")
	}
	s, err := server.Result()
	if err != nil {
		return routetool.RedisIdentity{}, fmt.Errorf("INFO server: %w", err)
	}
	r, err := replication.Result()
	if err != nil {
		return routetool.RedisIdentity{}, fmt.Errorf("INFO replication: %w", err)
	}
	return routetool.ParseRedisIdentity(s, r)
}

// permissionForCommand 只在装配入口消费一次 boot；只读命令不消费 writer 身份。
func permissionForCommand(a *cliArgs) (authoritysql.Permission, error) {
	if a.sub == subTiDBPrepare && !a.dryRun {
		return authoritysql.LoadSchemaPermission(os.Getenv)
	}
	if a.sub == subReconcile && !a.dryRun {
		return authoritysql.LoadOperatorPermission(os.Getenv)
	}
	if (a.sub == subImport && !a.dryRun) || (a.sub == subRewarm && a.maintenanceFile == "") {
		writer, err := authoritysql.LoadWriterConfig(os.Getenv)
		if err != nil {
			return authoritysql.Permission{}, err
		}
		a.writer = writer
		return authoritysql.Writer(writer), nil
	}
	return authoritysql.Reader(), nil
}

// maintenanceInput 只保存本次命令的正式原件与连接，不持久化另一份维护协议。
type maintenanceInput struct {
	db              io.Closer
	catalog         routetool.MaintenanceCatalog
	runtimeCatalog  *authoritysql.Catalog
	recoveryCatalog routeRecoveryCatalog
	request         *pb.SqlWriterMaintenanceReceipt
	ticketID        []byte
}

func openMaintenance(ctx context.Context, c config.Config, a cliArgs) (*maintenanceInput, error) {
	bad := func() (*maintenanceInput, error) {
		return nil, errors.New("维护原件、部署身份或当前目录状态不匹配")
	}
	if !filepath.IsAbs(a.maintenanceFile) {
		return bad()
	}
	info, err := os.Lstat(a.maintenanceFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 4<<20 {
		return bad()
	}
	file, err := os.Open(a.maintenanceFile)
	if err != nil {
		return bad()
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 4<<20 {
		return bad()
	}
	wanted, err := hex.DecodeString(a.maintenanceSHA)
	sum := sha256.Sum256(raw)
	if err != nil || len(wanted) != 32 || !bytes.Equal(wanted, sum[:]) {
		return bad()
	}
	request := &pb.SqlWriterMaintenanceReceipt{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, request); err != nil {
		return bad()
	}
	sealed, err := authoritysql.SealMaintenanceRequest(request)
	if err != nil || !bytes.Equal(sealed.RequestDigest, request.RequestDigest) {
		return bad()
	}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if err = proto.Unmarshal(request.PlanProto, plan); err != nil || len(plan.ProtoReflect().GetUnknown()) != 0 {
		return bad()
	}
	manifestSum := sha256.Sum256(request.PlanProto)
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 || !bytes.Equal(physical, plan.PhysicalClusterIdentity) || plan.AuthoritySchema != c.SnapshotMySQL.DBName || !bytes.Equal(plan.RunId, request.RunId) || request.Evidence == nil || !bytes.Equal(manifestSum[:], request.Evidence.ManifestDigest) {
		return bad()
	}
	if a.sub == subFreeze && request.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE && request.Kind != pb.SqlWriterMaintenanceReceipt_EMERGENCY_FREEZE {
		return bad()
	}
	if a.sub == subUnfreeze && request.Kind != pb.SqlWriterMaintenanceReceipt_RELEASE {
		return bad()
	}
	observingFreeze := a.sub == subReconcile || a.frozenRecovery || a.sub == subRewarm
	if observingFreeze && (request.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || request.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || request.CommittedMs == 0 || len(request.ReceiptDigest) != 32) {
		return bad()
	}
	if !observingFreeze && (request.CommittedMs != 0 || len(request.ReceiptDigest) != 0) {
		return bad()
	}
	permission := authoritysql.Reader()
	if !observingFreeze {
		permission, err = authoritysql.LoadOperatorPermission(os.Getenv)
		if err != nil {
			return nil, err
		}
	}
	cfg := svc.MySQLConfigOf(c)
	dsn := mysql.NewConfig()
	dsn.User = cfg.User
	dsn.Passwd = cfg.Password
	dsn.Net = "tcp"
	dsn.Addr = cfg.Host
	dsn.DBName = plan.AuthoritySchema
	dsn.ParseTime = true
	db, err := authoritysql.Open(ctx, dsn.FormatDSN(), plan.AuthoritySchema, true, 2, permission)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*maintenanceInput, error) { _ = db.Close(); return nil, err }
	var principal string
	if err = db.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		return fail(err)
	}
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: plan.AuthoritySchema, Generation: request.Generation, ProtectedSchemas: plan.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: principal, RequireReconcile: true})
	if err != nil {
		return fail(err)
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		return fail(err)
	}
	if meta.CatalogRevision != request.CatalogRevision {
		return fail(errors.New("目录修订已变化，固定请求不能重封"))
	}
	if observingFreeze {
		stored, err := catalog.MaintenanceReceipt(ctx, request.RequestId)
		if err != nil {
			return fail(err)
		}
		if !proto.Equal(stored, request) || meta.Writable != 0 || meta.WriterEpoch != request.AfterEpoch || !bytes.Equal(meta.FreezeRunId, request.RunId) {
			return fail(errors.New("当前冻结与永久正常排空收据不一致"))
		}
		run, err := catalog.DrainRun(ctx, request.RunId)
		if err != nil {
			return fail(err)
		}
		if run.Phase != pb.SqlWriterDrainRun_SQL_FROZEN && !(a.frozenRecovery && run.Phase == pb.SqlWriterDrainRun_RELEASE_READY) {
			return fail(errors.New("当前维护阶段不允许恢复操作"))
		}
	} else if meta.WriterEpoch != request.BeforeEpoch {
		// 仅同一请求的永久成功原件允许幂等重读，不能把新请求伪装成旧 epoch 的重试。
		stored, err := catalog.MaintenanceReceipt(ctx, request.RequestId)
		if err != nil || !bytes.Equal(stored.RequestDigest, request.RequestDigest) || meta.WriterEpoch != request.AfterEpoch {
			return fail(errors.New("epoch 已变化且没有匹配的永久请求结果"))
		}
	}
	ticketID, err := hex.DecodeString(a.ticketHex)
	if !observingFreeze && (err != nil || len(ticketID) != 16) {
		return fail(errors.New("票据 ID 无效"))
	}
	if !observingFreeze {
		ticket, err := catalog.FreezeTicket(ctx, ticketID)
		if err != nil {
			return fail(err)
		}
		if !bytes.Equal(ticket.OperationId, request.RunId) || ticket.Generation != request.Generation || ticket.AttemptRevision != request.AttemptRevision || !bytes.Equal(ticket.ManifestDigest, request.Evidence.ManifestDigest) {
			return fail(errors.New("票据与固定请求不属于同一次维护"))
		}
		if a.sub == subFreeze {
			if !bytes.Equal(ticket.FreezeRequestId, request.RequestId) || !bytes.Equal(ticket.FreezeRequestDigest, request.RequestDigest) {
				return fail(errors.New("票据未绑定本次冻结请求"))
			}
		} else {
			proof := request.Evidence.GetReleaseReadiness()
			if proof == nil || ticket.State != pb.SqlWriterFreezeTicket_CONSUMED || !bytes.Equal(proof.OriginalFreezeReceiptId, ticket.FreezeRequestId) || !bytes.Equal(proof.OriginalFreezeReceiptDigest, ticket.ConsumedReceiptDigest) {
				return fail(errors.New("解冻原件与已消费票据不匹配"))
			}
		}
	}
	return &maintenanceInput{db: db, catalog: catalog, runtimeCatalog: catalog, recoveryCatalog: catalog, request: request, ticketID: ticketID}, nil
}

// routeRedis保留域逻辑的同步调用形状，客户端与连接池只归本次CLI所有。
// go-zero Redis使用进程全局池且没有Close，不能作为正式有限发行者的资源owner。
type routeRedis interface {
	ScanCtx(context.Context, uint64, string, int64) ([]string, uint64, error)
	MgetCtx(context.Context, ...string) ([]string, error)
	GetCtx(context.Context, string) (string, error)
	EvalCtx(context.Context, string, []string, ...any) (any, error)
	SetnxCtx(context.Context, string, string) (bool, error)
	DelCtx(context.Context, ...string) (int, error)
	PipelinedCtx(context.Context, func(redis.Pipeliner) error) error
	Close() error
}
type ownedRouteRedis struct{ client goredis.UniversalClient }

func (r *ownedRouteRedis) ScanCtx(ctx context.Context, cursor uint64, match string, count int64) ([]string, uint64, error) {
	return r.client.Scan(ctx, cursor, match, count).Result()
}
func (r *ownedRouteRedis) MgetCtx(ctx context.Context, keys ...string) ([]string, error) {
	values, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(values))
	for i, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("MGET返回非字符串")
		}
		out[i] = text
	}
	return out, nil
}
func (r *ownedRouteRedis) GetCtx(ctx context.Context, key string) (string, error) {
	value, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	return value, err
}
func (r *ownedRouteRedis) EvalCtx(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.client.Eval(ctx, script, keys, args...).Result()
}
func (r *ownedRouteRedis) SetnxCtx(ctx context.Context, key, value string) (bool, error) {
	return r.client.SetNX(ctx, key, value, 0).Result()
}
func (r *ownedRouteRedis) DelCtx(ctx context.Context, keys ...string) (int, error) {
	n, err := r.client.Del(ctx, keys...).Result()
	return int(n), err
}
func (r *ownedRouteRedis) PipelinedCtx(ctx context.Context, fn func(redis.Pipeliner) error) error {
	_, err := r.client.Pipelined(ctx, fn)
	return err
}
func (r *ownedRouteRedis) Close() error { return r.client.Close() }

type routeCommandKey struct{}
type routeCommand struct {
	session, redisSession *drainproof.Session
	runtime               *drainproof.Runtime
	maintenance           bool
	writer                authoritysql.WriterConfig
	resources             []io.Closer
	catalog               io.Closer
	check                 func(context.Context) error
	// write只隔离正式原件落盘；生产固定Runtime.Write，测试不伪造P0/票据/队列。
	write func(string, *drainproof.Session) error
}

func routeCommandFrom(ctx context.Context) *routeCommand {
	command, _ := ctx.Value(routeCommandKey{}).(*routeCommand)
	return command
}
func (c *routeCommand) hold() { c.session.Hold() }

func openRouteCommand(ctx context.Context, config config.Config, a cliArgs, input *maintenanceInput) (*routeCommand, error) {
	c := &routeCommand{session: drainproof.New(), redisSession: drainproof.New(), writer: a.writer}
	if input != nil {
		c.catalog = input.db
	}
	p6 := (a.sub == subReconcile && !a.dryRun) || (a.sub == subRewarm && a.maintenanceFile != "")
	normal := (a.sub == subImport && !a.dryRun) || (a.sub == subRewarm && !p6)
	if !normal && !p6 {
		return c, nil
	} // Reader、Schema和Operator控制动作不冒充P6发行者。
	if normal && a.writer.AuthoritySchema() != config.SnapshotMySQL.DBName {
		return nil, drainproof.ErrHold
	}
	if a.runtimeFile == "" {
		if p6 {
			return nil, errors.New("P6恢复必须绑定P0预列runtime及同NORMAL票据")
		}
		return c, nil
	}
	var runtime *drainproof.Runtime
	var err error
	if p6 {
		runtime, err = drainproof.LoadMaintenanceRuntime(a.runtimeFile, a.runtimeDirectory, a.runtimeSHA)
	} else {
		runtime, err = drainproof.LoadRuntime(a.runtimeFile, a.runtimeDirectory, a.runtimeSHA)
	}
	if err != nil {
		return nil, err
	}
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 || !bytes.Equal(physical, runtime.Plan.PhysicalClusterIdentity) || runtime.Plan.AuthoritySchema != config.SnapshotMySQL.DBName {
		return nil, drainproof.ErrHold
	}
	if p6 {
		if input == nil || input.runtimeCatalog == nil {
			return nil, drainproof.ErrHold
		}
		c.check = func(ctx context.Context) error {
			frozen, err := runtime.CheckMaintenance(ctx, input.runtimeCatalog, input.ticketID)
			if err != nil {
				return err
			}
			if !proto.Equal(frozen, input.request) {
				return drainproof.ErrHold
			}
			return nil
		}
		if err = c.check(ctx); err != nil {
			return nil, err
		}
		c.session, err = runtime.BindMaintenance(ctx, input.runtimeCatalog, input.ticketID)
		if err != nil {
			return nil, err
		}
		c.maintenance = true
	} else {
		if err = bindNormalRouteRuntime(runtime, func() error { return runtime.BindBoot(a.writer.InstanceID(), a.writer.WriterKey()) }); err != nil {
			return nil, err
		}
	}
	c.runtime = runtime
	c.write = runtime.Write
	return c, nil
}

// 本工具没有自己派生的后台任务：业务函数已返回才关闭入口；每个Redis实际回包另由hook记账。
// 本机join不清UNKNOWN，closure仅说明有限发行者已闭合，不能替代P7业务audit。
func (c *routeCommand) finish(ctx context.Context) error {
	c.session.CloseAdmission()
	c.redisSession.CloseAdmission()
	if err := errors.Join(c.session.WaitIdle(ctx), c.redisSession.WaitIdle(ctx)); err != nil {
		c.hold()
		return err
	}
	var closeErr error
	for i := len(c.resources) - 1; i >= 0; i-- {
		closeErr = errors.Join(closeErr, c.resources[i].Close())
	}
	c.writer.Seal()
	if closeErr != nil {
		c.hold()
	}
	if c.check != nil {
		if err := c.check(ctx); err != nil {
			c.hold()
			closeErr = errors.Join(closeErr, err)
		}
	}
	if c.catalog != nil {
		if err := c.catalog.Close(); err != nil {
			c.hold()
			closeErr = errors.Join(closeErr, err)
		}
	}
	redisDigest, err := c.redisSession.SettledDigest(ctx)
	if err != nil {
		c.hold()
		closeErr = errors.Join(closeErr, err)
	} else if err = c.session.MergeClosedSettled(redisDigest); err != nil {
		c.hold()
		closeErr = errors.Join(closeErr, err)
	}
	if err = errors.Join(closeErr, c.session.Wait(ctx)); err != nil {
		return err
	}
	if c.write == nil {
		return nil
	}
	if c.maintenance {
		if err = c.write("MAINTENANCE", c.session); err != nil {
			c.hold()
		}
		return err
	}
	if c.runtime.Seed("SOURCE") != nil {
		if err = c.write("SOURCE", c.session); err != nil {
			c.hold()
			return err
		}
	}
	if c.runtime.Seed("RELAY") != nil {
		if err = c.write("RELAY", c.redisSession); err != nil {
			c.redisSession.Hold()
		}
		return err
	}
	return nil
}

// hook捕获本次固定owner，Background调用也不能绕过关闭；重试关闭，任何未知逐项保留。
type routeRedisHook struct{ command *routeCommand }

func (h routeRedisHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		done, err := h.command.redisSession.Enter()
		if err != nil {
			return nil, err
		}
		returned := false
		defer func() {
			if !returned || err != nil {
				h.command.redisSession.Hold()
			}
			done()
		}()
		conn, err = next(ctx, network, addr)
		returned = true
		return
	}
}
func (h routeRedisHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) (err error) {
		done, err := h.command.redisSession.Enter()
		if err != nil {
			return err
		}
		returned := false
		defer func() {
			if !returned || routeRedisUnknown(err) || routeRedisUnknown(cmd.Err()) {
				h.command.redisSession.Hold()
			}
			done()
		}()
		if h.command.check != nil {
			if err = h.command.check(ctx); err != nil {
				return err
			}
		}
		err = next(ctx, cmd)
		returned = true
		return
	}
}
func routeRedisUnknown(err error) bool { return err != nil && !errors.Is(err, goredis.Nil) }
func (h routeRedisHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) (err error) {
		done, err := h.command.redisSession.Enter()
		if err != nil {
			return err
		}
		returned := false
		defer func() {
			unknown := !returned || routeRedisUnknown(err)
			for _, cmd := range cmds {
				unknown = unknown || routeRedisUnknown(cmd.Err())
			}
			if unknown {
				h.command.redisSession.Hold()
			}
			done()
		}()
		if h.command.check != nil {
			if err = h.command.check(ctx); err != nil {
				return err
			}
		}
		err = next(ctx, cmds)
		returned = true
		return
	}
}

// 有限NORMAL工具没有prepared/open owner；SQL_RELEASED属于P8/P9，不能BindBoot后直接写。
func bindNormalRouteRuntime(runtime *drainproof.Runtime, bind func() error) error {
	if runtime == nil || runtime.Request == nil || runtime.Request.RunPhase == pb.SqlWriterDrainRun_SQL_FROZEN || runtime.Request.RunPhase == pb.SqlWriterDrainRun_SQL_RELEASED || bind == nil {
		return drainproof.ErrHold
	}
	// bind是生产Runtime.BindBoot的文件副作用接缝；阶段拒绝必须发生在调用它之前。
	return bind()
}

// routeRecoveryCatalog只暴露P7需要的永久只读事实；与P6写能力分开。
type routeRecoveryCatalog interface {
	MaintenanceReceipt(context.Context, []byte) (*pb.SqlWriterMaintenanceReceipt, error)
	CurrentMeta(context.Context) (*pb.RouteMeta, error)
	DrainRun(context.Context, []byte) (*pb.SqlWriterDrainRun, error)
}

func runRouteRecoveryAudit(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) int {
	if err := exportRouteRecoveryAudit(ctx, a, c, d, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "route_tool P7 audit: %v\n", err)
		return routetool.ExitFailed
	}
	return routetool.ExitClean
}

// P7独立Reader过程：不BindBoot、不重新开启已闭合的维护owner，也不把closure当作业务审核。
func exportRouteRecoveryAudit(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) (err error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if !c.RouteStore.DBFirst() || c.MappingRedis.Type == redis.ClusterType {
		return errors.New("P7审核要求db_first及P0具名单一mapping实例")
	}
	if err := authoritysql.CheckPrivateControlDirectory(filepath.Dir(a.recoveryAuditOutput)); err != nil {
		return err
	}
	var resources []io.Closer
	defer func() {
		for i := len(resources) - 1; i >= 0; i-- {
			err = errors.Join(err, resources[i].Close())
		}
	}()
	input, err := d.openMaintenance(ctx, c, a)
	if err != nil {
		return err
	}
	if input != nil && input.db != nil {
		resources = append(resources, input.db)
	}
	if input == nil || input.db == nil || input.recoveryCatalog == nil || input.request == nil {
		return drainproof.ErrHold
	}
	frozen := input.request
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if err := proto.Unmarshal(frozen.PlanProto, plan); err != nil {
		return err
	}
	beforeSQL, err := readRouteRecoveryFrozen(ctx, input.recoveryCatalog, plan, frozen)
	if err != nil {
		return err
	}
	publication, err := readRouteMaintenancePublication(a.publicationFile, a.publicationSHA)
	if err != nil {
		return err
	}
	if err := verifyRouteMaintenancePublication(plan, frozen, publication); err != nil {
		return err
	}
	sources, err := routeRecoveryQueryPins(plan)
	if err != nil {
		return err
	}
	target, err := routeRecoveryMappingTarget(plan, c.MappingRedis)
	if err != nil {
		return err
	}
	rd, err := d.connectRedis(ctx, c.MappingRedis)
	if err != nil {
		return err
	}
	resources = append(resources, rd)
	st, err := d.openRouteStore(c, authoritysql.Reader(), nil)
	if err != nil {
		return err
	}
	resources = append(resources, st)
	started := uint64(time.Now().UnixMilli())
	for _, closed := range publication.LogicalRoles {
		if closed.AcceptedSettledMs > started {
			return errors.New("P7审核早于实际维护工具闭合")
		}
	}
	before, err := captureRouteRecoveryRedis(ctx, rd, target, frozen.RunId, frozen.Generation)
	if err != nil {
		return err
	}
	report, err := routetool.Audit(ctx, rd, st, routetool.AuditOptions{Batch: a.batch, MaxReport: a.maxReport, RecheckDelay: a.recheckDelay, MaxRecheck: a.maxRecheck, FrozenRecovery: true, RequireMapRev: false, Target: fmt.Sprintf("route_tool P7 audit: mapping_redis=%s global_db=%s/%s", c.MappingRedis.Host, c.SnapshotMySQL.Host, c.SnapshotMySQL.DBName), Stdout: stdout, Stderr: stderr})
	if err != nil {
		return err
	}
	stats, err := routeRecoveryStats(report)
	if err != nil {
		return err
	}
	after, err := captureRouteRecoveryRedis(ctx, rd, target, frozen.RunId, frozen.Generation)
	if err != nil {
		return err
	}
	if !sameRouteRecoveryRedis(before, after) {
		return errors.New("P7审核期间mapping身份/nonce/完整缓存标记/水位发生变化")
	}
	if err := beforeSQL.verifyCurrent(ctx, input.recoveryCatalog, plan, frozen); err != nil {
		return err
	}
	again, err := readRouteMaintenancePublication(a.publicationFile, a.publicationSHA)
	if err != nil || !proto.Equal(again, publication) {
		return errors.Join(drainproof.ErrHold, err)
	}
	if err := verifyRouteMaintenancePublication(plan, frozen, publication); err != nil {
		return err
	}
	if _, err := routeRecoveryQueryPins(plan); err != nil {
		return err
	}
	var instance [16]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return err
	}
	digest := sha256.Sum256(frozen.PlanProto)
	audit := &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{AuditInstanceId: instance[:], RunId: bytes.Clone(frozen.RunId), Generation: frozen.Generation, AttemptRevision: frozen.AttemptRevision, PlanDigest: digest[:], OriginalFreezeReceiptId: bytes.Clone(frozen.RequestId), OriginalFreezeReceiptDigest: bytes.Clone(frozen.ReceiptDigest), MaintenancePublication: publication, AuthoritySchema: plan.AuthoritySchema, MappingTarget: before.MappingTarget, ObservedStartMs: started, ObservedEndMs: uint64(time.Now().UnixMilli()), Stats: stats, ReadyMarkerRaw: before.ReadyMarkerRaw, WatermarkRaw: before.WatermarkRaw, InfoServerRaw: before.InfoServerRaw, InfoReplicationRaw: before.InfoReplicationRaw, RouteWatermarkFloorMs: before.RouteWatermarkFloorMs, AuditSourcePins: sources}
	if audit.ObservedEndMs < audit.ObservedStartMs || audit.ObservedEndMs-audit.ObservedStartMs > 1800000 {
		return errors.New("P7审核窗口无效或超过30m")
	}
	// 当前独立审核已结束；关闭失败不会生成Completed原件，且不会重关其它owner资源。
	closing := resources
	resources = nil
	return publishRouteRecoveryAudit(ctx, a.recoveryAuditOutput, audit, closing, authoritysql.WritePrivateControlFile)
}

// 只聚合正式SQL原件，不重写协议字段；clone阻止同进程替身/owner后续修改首读快照。
type routeRecoveryFrozenSnapshot struct {
	frozen *pb.SqlWriterMaintenanceReceipt
	run    *pb.SqlWriterDrainRun
	meta   *pb.RouteMeta
}

func readRouteRecoveryFrozen(ctx context.Context, catalog routeRecoveryCatalog, plan *pb.SqlWriterMaintenanceReceipt_Plan, frozen *pb.SqlWriterMaintenanceReceipt) (*routeRecoveryFrozenSnapshot, error) {
	if catalog == nil || plan == nil || frozen == nil || frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || frozen.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || frozen.CommittedMs == 0 || !bytes.Equal(plan.RunId, frozen.RunId) {
		return nil, drainproof.ErrHold
	}
	stored, err := catalog.MaintenanceReceipt(ctx, frozen.RequestId)
	if err != nil {
		return nil, err
	}
	if !proto.Equal(stored, frozen) {
		return nil, errors.New("P7冻结原件不是永久正常FROZEN全文")
	}
	run, err := catalog.DrainRun(ctx, frozen.RunId)
	if err != nil {
		return nil, err
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(frozen.PlanProto)
	if run == nil || meta == nil || (run.Phase != pb.SqlWriterDrainRun_SQL_FROZEN && run.Phase != pb.SqlWriterDrainRun_RELEASE_READY) || !bytes.Equal(run.ManifestProto, frozen.PlanProto) || !bytes.Equal(run.ManifestDigest, sum[:]) || !bytes.Equal(run.FrozenRequestId, frozen.RequestId) || run.Generation != frozen.Generation || run.AttemptRevision != frozen.AttemptRevision || meta.Generation != frozen.Generation || meta.Writable != 0 || meta.WriterEpoch != frozen.AfterEpoch || meta.CatalogRevision != frozen.CatalogRevision || !bytes.Equal(meta.FreezeRunId, frozen.RunId) {
		return nil, errors.New("P7审核必须保持同run/attempt/epoch/catalog的真实冻结")
	}
	return &routeRecoveryFrozenSnapshot{frozen: proto.Clone(stored).(*pb.SqlWriterMaintenanceReceipt), run: proto.Clone(run).(*pb.SqlWriterDrainRun), meta: proto.Clone(meta).(*pb.RouteMeta)}, nil
}

func (s *routeRecoveryFrozenSnapshot) verifyCurrent(ctx context.Context, catalog routeRecoveryCatalog, plan *pb.SqlWriterMaintenanceReceipt_Plan, frozen *pb.SqlWriterMaintenanceReceipt) error {
	latest, err := readRouteRecoveryFrozen(ctx, catalog, plan, frozen)
	if err != nil {
		return err
	}
	if s == nil || !proto.Equal(s.frozen, latest.frozen) || !proto.Equal(s.run, latest.run) || !proto.Equal(s.meta, latest.meta) {
		return errors.New("P7审核窗口内永久freeze/run/meta全文发生变化，不能发布Completed原件")
	}
	return nil
}

func readRouteMaintenancePublication(file, wanted string) (*pb.SqlWriterMaintenanceReceipt_PublicationEvidence, error) {
	raw, err := authoritysql.ReadPrivateControlFile(file, 16<<20)
	if err != nil {
		return nil, err
	}
	expected, err := hex.DecodeString(wanted)
	sum := sha256.Sum256(raw)
	if err != nil || len(expected) != 32 || !bytes.Equal(sum[:], expected) {
		return nil, errors.New("P7维护闭合原件SHA不匹配")
	}
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, publication); err != nil {
		return nil, err
	}
	return publication, nil
}

func verifyRouteMaintenancePublication(plan *pb.SqlWriterMaintenanceReceipt_Plan, frozen *pb.SqlWriterMaintenanceReceipt, publication *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) error {
	if plan == nil || frozen == nil || publication == nil || len(plan.MaintenanceIssuers) == 0 || len(publication.LogicalRoles) != len(plan.MaintenanceIssuers) || len(publication.SourceEvidence) != len(plan.MaintenanceIssuers) || len(publication.ProtoReflect().GetUnknown()) != 0 {
		return drainproof.ErrHold
	}
	inventory, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(inventory)
	if !bytes.Equal(digest[:], publication.IssuerInventoryDigest) {
		return errors.New("P7维护闭合目录不是P0全集")
	}
	expected := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, seed := range plan.MaintenanceIssuers {
		if seed == nil || seed.LogicalRole != "MAINTENANCE" || len(seed.PhysicalWriterKey) != 0 || expected[string(seed.IssuerId)] != nil {
			return drainproof.ErrHold
		}
		expected[string(seed.IssuerId)] = seed
	}
	actual := map[string]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, closed := range publication.LogicalRoles {
		if closed == nil || actual[string(closed.IssuerId)] != nil || closed.AcceptingClosedMs < frozen.CommittedMs {
			return drainproof.ErrHold
		}
		if err := drainproof.VerifyClosure(closed); err != nil {
			return err
		}
		identity := proto.Clone(closed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
		identity.AcceptingClosedMs = 0
		identity.AcceptedSettledMs = 0
		identity.AcceptedFrontierDigest = nil
		identity.UnresolvedPublications = 0
		if !proto.Equal(identity, expected[string(closed.IssuerId)]) {
			return errors.New("P7闭合不是同P0维护boot完整身份")
		}
		actual[string(closed.IssuerId)] = closed
	}
	seen := map[string]bool{}
	for _, pin := range publication.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		closed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, closed) != nil || seen[string(closed.IssuerId)] || !proto.Equal(actual[string(closed.IssuerId)], closed) {
			return errors.New("P7缺实际维护boot闭合原文件或正文不符")
		}
		seen[string(closed.IssuerId)] = true
	}
	if len(seen) != len(expected) {
		return drainproof.ErrHold
	}
	return nil
}

func routeRecoveryQueryPins(plan *pb.SqlWriterMaintenanceReceipt_Plan) ([]*pb.SqlWriterMaintenanceReceipt_SourcePin, error) {
	if plan == nil {
		return nil, drainproof.ErrHold
	}
	required := []string{"go/data_service/internal/routetoolcli/cli.go", "go/data_service/internal/routetool/auditor.go", "go/data_service/internal/routetool/redisread.go", "go/data_service/internal/store/route_store.go"}
	result := make([]*pb.SqlWriterMaintenanceReceipt_SourcePin, 0, len(required))
	for _, name := range required {
		var selected *pb.SqlWriterMaintenanceReceipt_SourcePin
		for _, pin := range plan.SourcePins {
			if pin != nil && strings.HasSuffix(strings.ReplaceAll(pin.AbsolutePath, "\\", "/"), "/"+name) {
				if selected != nil {
					return nil, drainproof.ErrHold
				}
				selected = pin
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("P0缺实际路由审核查询来源: %s", name)
		}
		if _, err := drainproof.ReadPin(selected, 16<<20); err != nil {
			return nil, err
		}
		result = append(result, selected)
	}
	return result, nil
}

func routeRecoveryMappingTarget(plan *pb.SqlWriterMaintenanceReceipt_Plan, config redis.RedisConf) (*pb.SqlWriterMaintenanceReceipt_RedisTargetClosure, error) {
	if plan == nil || config.Host == "" || config.Type == redis.ClusterType {
		return nil, drainproof.ErrHold
	}
	var target *pb.SqlWriterMaintenanceReceipt_RedisTargetClosure
	for _, row := range plan.RedisTargets {
		if row != nil && row.Address == config.Host && row.Db == 0 {
			if target != nil {
				return nil, errors.New("P0 mapping目标重复")
			}
			target = row
		}
	}
	if target == nil || len(target.ServerRunId) != 40 || len(target.MaintenanceNonce) != 16 {
		return nil, errors.New("mapping端点/DB或维护nonce未绑定P0")
	}
	return target, nil
}

// 同一次Lua取得身份、nonce、标记、水位；前后两份原件仍必须保持一致。
func captureRouteRecoveryRedis(ctx context.Context, rd routeRedis, target *pb.SqlWriterMaintenanceReceipt_RedisTargetClosure, runID []byte, generation string) (*pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit, error) {
	key, err := drainproof.RedisMaintenanceNonceKey(runID)
	if err != nil {
		return nil, err
	}
	reply, err := rd.EvalCtx(ctx, drainproof.RouteRecoveryReadScript, []string{key, placement.RouteReadyKey, placement.RouteWatermarkKey})
	if err != nil {
		return nil, err
	}
	observed, err := drainproof.ParseRouteRecoverySnapshot(reply, runID, target)
	if err != nil {
		return nil, err
	}
	if len(observed.ReadyMarkerRaw) > 16384 || len(observed.WatermarkRaw) > 16384 {
		return nil, errors.New("P7缓存完整性标记/水位超预算")
	}
	marker, err := placement.DecodeRouteReadyMarker(string(observed.ReadyMarkerRaw))
	if err != nil {
		return nil, err
	}
	observed.RouteWatermarkFloorMs = marker.WatermarkMs
	if len(observed.WatermarkRaw) != 0 {
		watermark, err := placement.DecodeRouteWatermark(string(observed.WatermarkRaw))
		if err != nil {
			return nil, err
		}
		if floor := watermark.FloorFor(generation); floor > observed.RouteWatermarkFloorMs {
			observed.RouteWatermarkFloorMs = floor
		}
	}
	return observed, nil
}
func sameRouteRecoveryRedis(first, last *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) bool {
	if first == nil || last == nil || !proto.Equal(first.MappingTarget, last.MappingTarget) || !bytes.Equal(first.ReadyMarkerRaw, last.ReadyMarkerRaw) || !bytes.Equal(first.WatermarkRaw, last.WatermarkRaw) {
		return false
	}
	before, err := placement.ParseRedisInstanceIdentity(string(first.InfoServerRaw), string(first.InfoReplicationRaw))
	if err != nil {
		return false
	}
	after, err := placement.ParseRedisInstanceIdentity(string(last.InfoServerRaw), string(last.InfoReplicationRaw))
	return err == nil && before == after
}

func routeRecoveryStats(report routetool.AuditReport) (*pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats, error) {
	for _, n := range []int{report.DBRows, report.RedisPlayers, report.DBOnlyUnpublished, report.MissingInRedis, report.MissingInDB, report.ValueMismatch, report.RedisMalformed, report.DBInvalid, report.RedisRevAhead, report.RedisRevBehind, report.MergedIntoMissingInRedis, report.MergedIntoMissingInDB, report.MergedIntoMismatch, report.RedisFencesActive, report.TransientCleared} {
		if n < 0 {
			return nil, errors.New("P7实际审核返回负计数")
		}
	}
	if !report.FrozenRecovery || report.RequireMapRev || report.DBOnlyUnpublished > report.DBRows || report.MissingInRedis != 0 || report.MissingInDB != 0 || report.ValueMismatch != 0 || report.RedisMalformed != 0 || report.DBInvalid != 0 || report.RedisRevAhead != 0 || report.RedisRevBehind != 0 || report.MergedIntoMissingInRedis != 0 || report.MergedIntoMissingInDB != 0 || report.MergedIntoMismatch != 0 || report.RedisFencesActive != 0 || report.TransientCleared != 0 {
		return nil, errors.New("P7实际审核仍有阻断差异、围栏或并发清除")
	}
	return &pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats{DbRows: uint64(report.DBRows), RedisPlayers: uint64(report.RedisPlayers), DbOnlyUnpublished: uint64(report.DBOnlyUnpublished), MissingInRedis: uint64(report.MissingInRedis), MissingInDb: uint64(report.MissingInDB), ValueMismatch: uint64(report.ValueMismatch), RedisMalformed: uint64(report.RedisMalformed), DbInvalid: uint64(report.DBInvalid), RedisRevAhead: uint64(report.RedisRevAhead), RedisRevBehind: uint64(report.RedisRevBehind), MergedIntoMissingInRedis: uint64(report.MergedIntoMissingInRedis), MergedIntoMissingInDb: uint64(report.MergedIntoMissingInDB), MergedIntoMismatch: uint64(report.MergedIntoMismatch), RedisFencesActive: uint64(report.RedisFencesActive), TransientCleared: uint64(report.TransientCleared), FrozenRecovery: report.FrozenRecovery, RequireMapRev: report.RequireMapRev}, nil
}

func publishRouteRecoveryAudit(ctx context.Context, file string, audit *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit, resources []io.Closer, write func(string, []byte) error) error {
	var err error
	for i := len(resources) - 1; i >= 0; i-- {
		err = errors.Join(err, resources[i].Close())
	}
	if err = errors.Join(err, ctx.Err()); err != nil {
		return err
	}
	if audit == nil || audit.Stats == nil || write == nil {
		return drainproof.ErrHold
	}
	audit.Stats.Completed = true
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(audit)
	if err != nil {
		return err
	}
	return write(file, raw)
}
