package main

// merge_zone —— 不停服合服工具(方案 docs/design/player-data-tidb-scaling-plan.md §7.1,操作步骤 docs/ops/merge-zone-runbook.md)。
//
// 合服只写一条映射:目录第 0 片 zone_merged_into 的「源区 → 目标区」。玩家、公会、商品、榜单一行都不动,各服务读的时候
// 把记录里的原始区解析成最终区(placement.MergedZones)。没有数据搬动,就没有「搬动期间的一致性」问题,
// 所以这里没有清单、没有前置门禁、不踢人,也不碰 Redis。映射每变一次,同一个事务里往 zone_merge_history 追加一条
// 历史(谁、什么时候、合进 / 撤出哪个区),撤销合服删掉映射行之后仍查得到(提案 docs/design/player-route-mapping-durability-proposal.md §16)。
//
//	-mode merge   -source-zone S -target-zone T [-dry-run]   写 S → T
//	-mode unmerge -source-zone S -target-zone T [-dry-run]   删 S → T
//	-mode audit                                              只读,列出所有映射、每个源区的最终区和合服历史
//
// 退出码:
//
//	0  完成(含幂等:映射本来就是这样)
//	1  拒绝:参数或校验不过,什么都没写;审计时表示映射表本身解析不通,或有映射行在历史里找不到对应的合服记录
//	2  没做成或结局未知:连不上库、SQL 失败、提交没有回音。重跑同一条命令是安全的
//
// 连接规范与其它运维工具相同:全局库走 authoritysql(MMORPG_SQL_BACKEND 选档位),真写要操作员身份
// (MMORPG_SQL_OPERATOR_PRINCIPAL),dry-run 和审计只用只读权限。DSN 含口令,任何输出都不回显。
//
// 真写提交之后把映射发布进 etcd 副本 /mmorpg/merged_zone_copy/<第 0 片库名>(docs/design/merged-zone-etcd-copy.md),
// 所以真写还要 -etcd-hosts(data_service 节点注册那一套 etcd);审计给了它就比对副本,dry-run 不用它。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/authoritysql"
	"shared/etcdwatch"
	"shared/mergedzonecopy"
)

const (
	modeMerge   = "merge"
	modeUnmerge = "unmerge"
	modeAudit   = "audit"

	exitOK      = 0
	exitRefused = 1
	exitFailed  = 2

	// runTimeout 是一次运行的总时限。工具只跑一个事务,锁等待封顶 1 秒(authoritysql 的会话设置),30 秒足够连库、重连和发布副本。
	runTimeout = 30 * time.Second

	// etcdDialTimeout 与 data_service 连同一套 etcd 的客户端同值。
	etcdDialTimeout = 5 * time.Second
)

type options struct {
	mode       string
	sourceZone uint32
	targetZone uint32
	routeDSN   string // 含口令:只交给 authoritysql.Open,不进日志、不进错误信息
	dryRun     bool
	// etcdHosts 是 -etcd-hosts 的原文(逗号分隔的 host:port),parseOptions 已校验形状。存原文不存切片,options 才能直接比较。
	etcdHosts string
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
	copies, closeCopies, err := openCopiesFor(o, store.database)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return exitFailed
	}
	defer closeCopies()
	err = execute(ctx, store, copies, o, time.Now(), out)
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
		"DSN of directory shard 0, the only one that holds zone_merged_into (the base name when Directory.LogicalShards=1, otherwise <base>_00; dev: testdb, K8s: mmorpg_global_00). Required, never logged")
	fs.BoolVar(&o.dryRun, "dry-run", false, "merge / unmerge: validate and print what would happen, write nothing")
	fs.StringVar(&o.etcdHosts, "etcd-hosts", "",
		"comma-separated host:port of the etcd that data_service registers in (dev: 127.0.0.1:2379; K8s: a port-forward to etcd in the infra namespace). "+
			"Required for a real merge or unmerge, which publishes the mapping to the etcd copy after committing it; optional for audit, which then compares the copy; unused by -dry-run")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	// 不回显内容:把 DSN 写错位置时,多出来的参数就是口令。
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("%d unexpected positional argument(s); their values are not echoed", fs.NArg())
	}
	if o.routeDSN == "" {
		return options{}, errors.New("-route-db-dsn is required: directory shard 0, which holds zone_merged_into, is the only source of truth for the mapping")
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
	if o.etcdHosts != "" {
		if _, err := splitEtcdHosts(o.etcdHosts); err != nil {
			return options{}, err
		}
	} else if o.writes() {
		return options{}, fmt.Errorf("a real %s needs -etcd-hosts: after committing, it publishes the mapping to the etcd copy that data_service replicas "+
			"which cannot read directory shard 0 answer from (docs/design/merged-zone-etcd-copy.md)", o.mode)
	}
	return o, nil
}

// splitEtcdHosts 拆 -etcd-hosts:逗号分隔,每一项都是 host:port。
func splitEtcdHosts(raw string) ([]string, error) {
	var hosts []string
	for _, part := range strings.Split(raw, ",") {
		host := strings.TrimSpace(part)
		if _, _, err := net.SplitHostPort(host); err != nil || host == "" {
			return nil, fmt.Errorf("-etcd-hosts must be comma-separated host:port entries (got %q)", raw)
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// openCopiesFor 按需要连 etcd 副本:没给 -etcd-hosts 或 dry-run 时回 nil(审计不比对,dry-run 不用)。shardZeroDB 是 DSN 里的库名。
// clientv3.New 不带 WithBlock,这里不拨号,连不通要到第一次读副本才报。回接口类型的 nil,不回包着 nil 指针的接口。
func openCopiesFor(o options, shardZeroDB string) (mergedzonecopy.Copies, func(), error) {
	if o.etcdHosts == "" || o.dryRun {
		return nil, func() {}, nil
	}
	hosts, err := splitEtcdHosts(o.etcdHosts)
	if err != nil {
		return nil, nil, err
	}
	// keepalive 与 data_service、dbplacement 的 etcd 客户端同一套(etcdwatch 包注释)。
	client, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{Endpoints: hosts, DialTimeout: etcdDialTimeout}))
	if err != nil {
		return nil, nil, fmt.Errorf("connect etcd for the merged zone copy: %w", err)
	}
	return mergedzonecopy.NewEtcdCopies(client, shardZeroDB), func() { _ = client.Close() }, nil
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
