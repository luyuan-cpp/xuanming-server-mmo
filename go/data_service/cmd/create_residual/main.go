// create_residual 核对并修复多集群时间点恢复之后的建角残留:ref 缺失、名字缺失、名字未绑定(提交之后还是预留态)、初始存档行缺失,
// 以及名字登记表上的两种:删角的名字还登记着(名字片恢复点早于删角释放)、孤儿名字(建角回滚的释放没做成,比如玩家片冻结时)
// (手册 docs/ops/mysql-backup-pitr-runbook.md §4.1a,判据和修复见 internal/store/create_residual.go)。-repair name 修名字的五种。
//
// 默认 -mode plan 只报告,一笔都不写。修复要 -mode apply 并用 -repair 列出要修的种类;每一项修之前重读角色行复核,
// 写在一个事务里(名字登记是建角同一套两步幂等写),可以重放。修完再跑一遍 plan,退出码 0 才算干净。
// 按片扫:-shard 指定一片(多个进程各扫一片可以并行),不指定扫全部片;-target 选扫角色行、名字行或两样都扫(默认);
// -after / -after-name / -limit 分段,每片的 shard 行给出下一段的起点。
//
// 修名字要删 data_service 的名字读缓存(NameCacheRedis 里的 player:name:{id},正、负缓存同一个键),修完马上查得到:
// -repair 带 name 时必须给 -name-cache-addr,口令从 -name-cache-pass-env 指的环境变量读。
//
// 连接用维护身份(MMORPG_SQL_OPERATOR_PRINCIPAL),不过写者闸门,每笔写在事务里自己锁本库闸门行;库被搬库或维护冻结时那一项拒绝。
// 库在哪按映射(MMORPG_DB_PLACEMENT)逐库解析,没有映射就拒绝,不连连接串里的地址。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"data_service/internal/routing"
	"data_service/internal/store"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"shared/authoritysql"
	"shared/dbplacement"
)

type options struct {
	dsnEnv           string
	schema           string
	stores           string
	mode             string
	repair           string
	target           string
	logicalShards    int
	maxRoles         int
	shard            int
	limit            int
	batch            int
	after            uint64
	afterName        string
	timeout          time.Duration
	nameCacheAddr    string
	nameCacheType    string
	nameCachePassEnv string
}

type auditor interface {
	Shards() int
	Run(ctx context.Context, scan store.CreateResidualScan) (store.CreateResidualReport, error)
}

// opener 打开核账。回的错误只带固定文案:底层错误可能含连接细节。
type opener func(ctx context.Context, o options) (auditor, func(), error)

func main() {
	var o options
	flag.StringVar(&o.dsnEnv, "dsn-env", "CREATE_RESIDUAL_DSN", "全局库 DSN 所在的环境变量名(维护身份)")
	flag.StringVar(&o.schema, "schema", "", "目录基名(SnapshotMySQL.DBName),DSN 的库名要写它")
	flag.IntVar(&o.logicalShards, "logical-shards", 1, "目录分片数(Directory.LogicalShards)")
	flag.StringVar(&o.stores, "storage-ids", "", "要核的存档库 storage_id,逗号分隔;落点不在里面的角色只报 storage_not_listed")
	flag.IntVar(&o.maxRoles, "max-roles", 5, "每个账号的角色上限(AccountDirectory.MaxPlayersPerAccount),只用来标补回 ref 之后超上限")
	flag.IntVar(&o.shard, "shard", -1, "只扫这一片(0..K-1);-1 扫全部片")
	flag.StringVar(&o.target, "target", "all", "扫什么:roles 角色行(含墓碑);names 名字行(孤儿名字);all 两样都扫")
	flag.Uint64Var(&o.after, "after", 0, "角色行从这个 player_id 之后接着扫,要和 -shard、-target roles 一起用")
	flag.StringVar(&o.afterName, "after-name", "", "名字行从这个判重键之后接着扫,要和 -shard、-target names 一起用")
	flag.IntVar(&o.limit, "limit", 0, "每片这一次最多扫多少行;0 扫到片尾")
	flag.IntVar(&o.batch, "batch", store.CreateResidualDefaultBatch, "每页行数")
	flag.StringVar(&o.mode, "mode", "plan", "plan 只报告;apply 才写")
	flag.StringVar(&o.repair, "repair", "", "apply 要修的种类,逗号分隔:ref,name,initial_rows")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Minute, "整次运行的时限")
	flag.StringVar(&o.nameCacheAddr, "name-cache-addr", "", "data_service 名字读缓存的地址(NameCacheRedis.Host);-repair 带 name 时必填")
	flag.StringVar(&o.nameCacheType, "name-cache-type", redis.NodeType, "名字读缓存的类型(NameCacheRedis.Type):node 或 cluster")
	flag.StringVar(&o.nameCachePassEnv, "name-cache-pass-env", "", "名字读缓存口令所在的环境变量名(K8s 上是 SHARED_REDIS_PASSWORD);不给就不带口令")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, o, os.Stdout, os.Stderr, openAuditor)
	stop()
	os.Exit(code)
}

// run 的退出码:0 扫到的范围里没有要人看的项;1 有要人看的项(含补回 ref 之后超过角色上限、名字缓存没删掉),或者扫到一半出错;
// 2 参数错误或打不开库。
func run(ctx context.Context, o options, out, errOut io.Writer, open opener) int {
	repairs, err := parseRepairs(o.mode, o.repair)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	targets, err := parseTargets(o.target)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	resuming := o.after != 0 || o.afterName != ""
	if o.shard < -1 || (resuming && (o.shard < 0 || len(targets) != 1)) || (o.after != 0 && targets[0] != store.ResidualTargetRoles) ||
		(o.afterName != "" && targets[0] != store.ResidualTargetNames) ||
		o.limit < 0 || o.batch < 1 || o.batch > store.CreateResidualMaxBatch || o.timeout <= 0 || o.maxRoles < 1 {
		fmt.Fprintf(errOut, "invalid arguments: -shard is -1 or a shard number, -after needs -shard and -target roles, -after-name needs -shard and -target names, "+
			"-limit >= 0, -batch in [1,%d], -timeout > 0, -max-roles >= 1\n", store.CreateResidualMaxBatch)
		return 2
	}
	if repairs.Names && o.nameCacheAddr == "" {
		fmt.Fprintln(errOut, "-repair name needs -name-cache-addr (data_service NameCacheRedis.Host): a repaired name must be readable at once, "+
			"so its cache entry (negative entries included) is deleted right after the repair")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	a, closeAll, err := open(ctx, o)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	defer closeAll()
	var slots []int
	switch {
	case o.shard >= a.Shards():
		fmt.Fprintf(errOut, "shard %d out of range: the directory has %d shards\n", o.shard, a.Shards())
		return 2
	case o.shard >= 0:
		slots = []int{o.shard}
	default:
		for slot := 0; slot < a.Shards(); slot++ {
			slots = append(slots, slot)
		}
	}
	scanned, findings, attention, done := 0, 0, 0, true
	for _, slot := range slots {
		for _, target := range targets {
			report, err := a.Run(ctx, store.CreateResidualScan{Slot: slot, Target: target, After: o.after, AfterName: o.afterName,
				Limit: o.limit, Batch: o.batch, Repair: repairs})
			for _, f := range report.Findings {
				printFinding(out, f)
				if f.NeedsAttention() {
					attention++
				}
			}
			scanned += report.Scanned
			findings += len(report.Findings)
			printShard(out, slot, target, report)
			if err != nil {
				// 底层错误可能带连接细节,只输出固定文案和接着扫的位置。
				fmt.Fprintf(errOut, "scan stopped on shard %d (%s): rows after the resume point were not checked; rerun with %s\n",
					slot, target, resumeFlags(slot, target, report))
				return 1
			}
			done = done && report.Done
		}
	}
	fmt.Fprintf(out, "summary mode=%s target=%s shards=%d scanned=%d findings=%d attention=%d done=%t\n", o.mode, o.target, len(slots), scanned, findings, attention, done)
	if attention > 0 {
		return 1
	}
	return 0
}

func printShard(out io.Writer, slot int, target store.CreateResidualTarget, report store.CreateResidualReport) {
	switch target {
	case store.ResidualTargetNames:
		fmt.Fprintf(out, "shard shard=%d target=%s scanned=%d findings=%d next_after_name=%q done=%t\n",
			slot, target, report.Scanned, len(report.Findings), report.NextAfterName, report.Done)
	default:
		fmt.Fprintf(out, "shard shard=%d target=%s scanned=%d findings=%d next_after=%d done=%t\n",
			slot, target, report.Scanned, len(report.Findings), report.NextAfter, report.Done)
	}
}

// resumeFlags 是一片扫到一半出错之后接着扫要带的参数。
func resumeFlags(slot int, target store.CreateResidualTarget, report store.CreateResidualReport) string {
	if target == store.ResidualTargetNames {
		return fmt.Sprintf("-shard %d -target names -after-name %q", slot, report.NextAfterName)
	}
	return fmt.Sprintf("-shard %d -target roles -after %d", slot, report.NextAfter)
}

func printFinding(out io.Writer, f store.CreateResidualFinding) {
	line := fmt.Sprintf("finding shard=%d player=%d kind=%s result=%s", f.Slot, f.PlayerID, f.Kind, f.Result)
	if f.Reason != "" {
		line += " reason=" + f.Reason
	}
	switch {
	case f.Kind == store.ResidualNameTaken:
		line += fmt.Sprintf(" holder=%d holder_live=%t keeper=%d", f.Holder, f.HolderLive, f.Keeper)
	case f.Holder != 0:
		line += fmt.Sprintf(" holder=%d", f.Holder)
	}
	if f.NeedsAttention() {
		line += " attention=true"
	}
	fmt.Fprintln(out, line)
}

func parseRepairs(mode, raw string) (store.CreateResidualRepairs, error) {
	var repairs store.CreateResidualRepairs
	switch mode {
	case "plan":
		if raw != "" {
			return repairs, errors.New("-repair only applies to -mode apply")
		}
		return repairs, nil
	case "apply":
	default:
		return repairs, errors.New("-mode must be plan or apply")
	}
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "ref":
			repairs.Refs = true
		case "name":
			repairs.Names = true
		case "initial_rows":
			repairs.InitialRows = true
		default:
			return store.CreateResidualRepairs{}, errors.New("-mode apply needs -repair with one or more of ref,name,initial_rows")
		}
	}
	return repairs, nil
}

// parseTargets 把 -target 换成要扫的对象,按扫描顺序:先角色行,再名字行。
func parseTargets(raw string) ([]store.CreateResidualTarget, error) {
	switch raw {
	case "all":
		return []store.CreateResidualTarget{store.ResidualTargetRoles, store.ResidualTargetNames}, nil
	case "roles":
		return []store.CreateResidualTarget{store.ResidualTargetRoles}, nil
	case "names":
		return []store.CreateResidualTarget{store.ResidualTargetNames}, nil
	default:
		return nil, errors.New("-target must be roles, names or all")
	}
}

func openAuditor(ctx context.Context, o options) (auditor, func(), error) {
	ids, err := parseStorageIDs(o.stores)
	if err != nil {
		return nil, nil, errors.New("invalid -storage-ids")
	}
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		return nil, nil, errors.New("invalid " + authoritysql.BackendEnv)
	}
	permission, err := authoritysql.LoadOperatorPermission(os.Getenv)
	if err != nil {
		return nil, nil, errors.New("missing SQL operator identity")
	}
	// 从环境读 DSN 后只取连接配置,错误里不回显原 DSN 或密码。
	cfg, err := mysql.ParseDSN(os.Getenv(o.dsnEnv))
	if err != nil || cfg.DBName != o.schema || cfg.Net != "tcp" {
		return nil, nil, errors.New("invalid SQL connection parameters")
	}
	placement, err := dbplacement.ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, nil, errors.New("invalid database placement")
	}
	// 没有映射就连连接串里的地址:库搬走之后那是冻结的旧库,不能猜。
	if !placement.Enabled() {
		return nil, nil, errors.New(dbplacement.EnvConfig + " is required: without it this tool cannot tell where each database lives")
	}
	// 名字读缓存先连:连不上就不碰库(修名字的 apply 才要它,run 已经核过参数)。
	var cache store.NameCacheInvalidator
	if o.nameCacheAddr != "" {
		conf := redis.RedisConf{Host: o.nameCacheAddr, Type: o.nameCacheType}
		if o.nameCachePassEnv != "" {
			conf.Pass = os.Getenv(o.nameCachePassEnv)
		}
		opened, err := routing.OpenPlayerNameCache(conf)
		if err != nil {
			return nil, nil, errors.New("cannot connect the name cache Redis (-name-cache-addr / -name-cache-type / -name-cache-pass-env)")
		}
		cache = opened
	}
	conf := store.MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: o.schema, Backend: backend, Permission: permission,
		LogicalShards: o.logicalShards, MaxOpenConn: 2, MaxIdleConn: 1,
		// 目录片和存档库可能不在一个集群,逐库读一次映射。
		PoolOptionsFor: func(schema string) ([]authoritysql.OpenOption, error) {
			return dbplacement.ForOneShot(ctx, placement, schema)
		},
	}
	// 目录每片一个池:扫描、补 ref、名字修复锁闸门行、孤儿名字的证明都走它,名字写走名字库自己的池,所以每片至少两个连接。
	directory, err := store.NewAccountDirectoryStore(conf, backend, o.maxRoles, 4)
	if err != nil {
		return nil, nil, errors.New("cannot open the account directory")
	}
	if err := directory.ConfigurePlayerStores(ids, store.PlayerStorePoolOptions{MaxOpenConn: 2, MaxIdleConn: 1}); err != nil {
		_ = directory.Close()
		return nil, nil, errors.New("invalid -storage-ids")
	}
	names, err := store.NewPlayerNameStore(conf)
	if err != nil {
		_ = directory.Close()
		return nil, nil, errors.New("cannot open the player name registry")
	}
	a, err := store.NewCreateResidualAuditor(directory, names, cache, time.Now)
	if err != nil {
		_ = names.Close()
		_ = directory.Close()
		return nil, nil, errors.New("the account directory and the name registry are not the same databases")
	}
	return a, func() {
		_ = names.Close()
		_ = directory.Close()
	}, nil
}

func parseStorageIDs(raw string) ([]uint32, error) {
	var ids []uint32
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil || id == 0 {
			return nil, errors.New("invalid storage id")
		}
		ids = append(ids, uint32(id))
	}
	return ids, nil
}
