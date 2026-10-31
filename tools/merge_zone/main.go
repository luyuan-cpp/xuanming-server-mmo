package main

// merge_zone —— 不停服合服工具(方案 docs/design/player-data-tidb-scaling-plan.md §7.1,操作步骤 docs/ops/merge-zone-runbook.md)。
//
// 合服只写一条映射:全局库 zone_merged_into 的「源区 → 目标区」。玩家、公会、商品、榜单一行都不动,各服务读的时候
// 把记录里的原始区解析成最终区(placement.MergedZones)。没有数据搬动,就没有「搬动期间的一致性」问题,
// 所以这里没有清单、没有前置门禁、不踢人,也不碰 Redis。
//
//	-mode merge   -source-zone S -target-zone T [-dry-run]   写 S → T
//	-mode unmerge -source-zone S -target-zone T [-dry-run]   删 S → T
//	-mode audit                                              只读,列出所有映射和每个源区的最终区
//
// 退出码:
//
//	0  完成(含幂等:映射本来就是这样)
//	1  拒绝:参数或校验不过,什么都没写;审计时表示映射表本身解析不通
//	2  没做成或结局未知:连不上库、SQL 失败、提交没有回音。重跑同一条命令是安全的
//
// 连接规范与其它运维工具相同:全局库走 authoritysql(MMORPG_SQL_BACKEND 选档位),真写要操作员身份
// (MMORPG_SQL_OPERATOR_PRINCIPAL),dry-run 和审计只用只读权限。DSN 含口令,任何输出都不回显。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"shared/authoritysql"
)

const (
	modeMerge   = "merge"
	modeUnmerge = "unmerge"
	modeAudit   = "audit"

	exitOK      = 0
	exitRefused = 1
	exitFailed  = 2

	// runTimeout 是一次运行的总时限。工具只跑一个事务,锁等待封顶 1 秒(authoritysql 的会话设置),30 秒足够连库和重连。
	runTimeout = 30 * time.Second
)

type options struct {
	mode       string
	sourceZone uint32
	targetZone uint32
	routeDSN   string // 含口令:只交给 authoritysql.Open,不进日志、不进错误信息
	dryRun     bool
}

// writes 报告这次运行会不会写库。dry-run 和审计只要只读权限,不要求操作员身份。
func (o options) writes() bool {
	return o.mode != modeAudit && !o.dryRun
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, out, errOut io.Writer) int {
	o, err := parseOptions(args, errOut)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return exitRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	store, err := openStoreFor(ctx, o, getenv)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return exitFailed
	}
	defer func() {
		if err := store.Close(); err != nil {
			fmt.Fprintf(errOut, "WARN: close the global database: %v\n", err)
		}
	}()
	err = execute(ctx, store, o, time.Now(), out)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
	}
	return exitCodeOf(err)
}

// exitCodeOf:没有错误 0;校验拒绝 1;其余都是没做成或结局未知,2。
func exitCodeOf(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errRefused):
		return exitRefused
	default:
		return exitFailed
	}
}

// parseOptions 解析并校验命令行。纯函数(不读环境、不连库):校验失败的命令不碰任何东西。
func parseOptions(args []string, errOut io.Writer) (options, error) {
	fs := flag.NewFlagSet("merge_zone", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o options
	fs.StringVar(&o.mode, "mode", "", "merge | unmerge | audit (required)")
	source := fs.Uint("source-zone", 0, "zone merged away (merge / unmerge)")
	target := fs.Uint("target-zone", 0, "zone it is merged into (merge / unmerge)")
	fs.StringVar(&o.routeDSN, "route-db-dsn", "",
		"DSN of the global database that holds zone_merged_into and route_meta (dev: testdb, K8s: mmorpg_global). Required, never logged")
	fs.BoolVar(&o.dryRun, "dry-run", false, "merge / unmerge: validate and print what would happen, write nothing")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	// 不回显内容:把 DSN 写错位置时,多出来的参数就是口令。
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("%d unexpected positional argument(s); their values are not echoed", fs.NArg())
	}
	if o.routeDSN == "" {
		return options{}, errors.New("-route-db-dsn is required: the global database that holds zone_merged_into is the only source of truth for the mapping")
	}
	switch o.mode {
	case modeMerge, modeUnmerge:
		pair, err := zonePair(*source, *target)
		if err != nil {
			return options{}, err
		}
		o.sourceZone, o.targetZone = pair[0], pair[1]
	case modeAudit:
		// 审计看的是整张表;接受 zone 参数会让人以为结果按它过滤过。
		if *source != 0 || *target != 0 || o.dryRun {
			return options{}, errors.New("-mode audit reads the whole mapping and writes nothing: it takes no -source-zone, -target-zone or -dry-run")
		}
	case "":
		return options{}, errors.New("-mode is required: merge, unmerge or audit")
	default:
		return options{}, fmt.Errorf("unknown -mode %q (expected merge, unmerge or audit)", o.mode)
	}
	return o, nil
}

// zonePair 校验一对 flag 值:都在 zone 编号范围内,且不是同一个区。先比范围再收窄,不让 uint32() 把人指到另一个区。
func zonePair(source, target uint) ([2]uint32, error) {
	for _, zone := range []uint{source, target} {
		if zone < 1 || zone > uint(routeZoneMax) {
			return [2]uint32{}, fmt.Errorf("-source-zone and -target-zone must be in [1, %d] (got %d and %d)", routeZoneMax, source, target)
		}
	}
	if source == target {
		return [2]uint32{}, fmt.Errorf("-source-zone and -target-zone must differ (both %d)", source)
	}
	return [2]uint32{uint32(source), uint32(target)}, nil
}

// openStoreFor 按模式选权限开库。档位和操作员身份都在连库之前读完:配置错了不发一条语句。
func openStoreFor(ctx context.Context, o options, getenv func(string) string) (*routeStore, error) {
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", authoritysql.BackendEnv, err)
	}
	permission := authoritysql.Reader()
	if o.writes() {
		permission, err = authoritysql.LoadOperatorPermission(getenv)
		if err != nil {
			return nil, fmt.Errorf("a real %s needs the operator identity: set MMORPG_SQL_OPERATOR_PRINCIPAL to the CURRENT_USER() of the DSN account: %w", o.mode, err)
		}
	}
	return openRouteStore(ctx, o.routeDSN, backend, permission)
}
