// Package routetoolcli 是 route_tool 的命令行入口:解析参数、装配(配置 → 全局库)、把结果映射成退出码
// (docs/design/player-route-mapping-durability-proposal.md §15)。逻辑全在 internal/routetool。
//
// K8s 镜像只构建单一入口 /app/service(deploy/k8s/Dockerfile.go-svc),生产上的运行方式是
//
//	data_service -f <yaml> -route-tool <子命令> [子命令参数]
//
// (data_service.go 的 -route-tool;一次性 Job / Pod 跑)。cmd/route_tool 只留一个薄包装,给 dev 的 `go run` 用,命令行逐字相同。
//
// 子命令与退出码(0 完成 / 2 失败):
//   - tidb-prepare:对路由表设防合并并预切 Region;MySQL 上跳过(没有 Region)。
//   - freeze / unfreeze:经正式维护原件与票据冻结 / 解冻映射写者(route_meta.writable)。
//
// 与 data_service 读同一份 yaml:全局库取 SnapshotMySQL 段,连接池大小取 RouteStore 段;整段仍按服务的口径校验(非法即退出码 2)。
package routetoolcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	subTiDBPrepare = "tidb-prepare"
	subFreeze      = "freeze"
	subUnfreeze    = "unfreeze"
)

const usage = `用法:
  route_tool -f <data_service.yaml> tidb-prepare -zones=1,2[,…] [-storages=1000000[,…]] [-dry-run=true] [-timeout=30m]
  route_tool -f <data_service.yaml> freeze -maintenance-request=<绝对路径> -maintenance-sha256=<摘要> -ticket-id=<票据ID> [-reason=<文本>] [-freeze-timeout=5s] [-timeout=30m]
  route_tool -f <data_service.yaml> unfreeze -maintenance-request=<RELEASE请求> -maintenance-sha256=<摘要> -ticket-id=<票据ID> [-freeze-timeout=5s] [-timeout=30m]
  (K8s 上同一套子命令走主程序:data_service -f <yaml> -route-tool <子命令> [参数])

退出码:0 完成(tidb-prepare 在 MySQL 上跳过也是 0,MySQL 没有 Region);2 失败(参数 / 配置 / 连接 / 查询 / 写入 / 超时)。`

// cliArgs 是解析后的命令行。
type cliArgs struct {
	configPath string
	sub        string
	timeout    time.Duration
	// zones / storages / dryRun 只给 tidb-prepare。
	zones    []uint32
	storages []uint32
	dryRun   bool
	// 以下只给 freeze / unfreeze;reason 只给 freeze。
	maintenanceFile string
	maintenanceSHA  string
	ticketHex       string
	freezeTimeout   time.Duration
	reason          string
}

// routeStore 是本入口对全局库 RouteStore 的全部依赖(*store.RouteStore 天然满足;单测注入假实现)。
type routeStore interface {
	PrepareTiDB(ctx context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error)
	Close() error
}

var _ routeStore = (*store.RouteStore)(nil)

// deps 是装配用到的外部依赖(显式注入,单测替换;生产取 defaultDeps)。
type deps struct {
	openMaintenance func(context.Context, config.Config, cliArgs) (*maintenanceInput, error)
	openRouteStore  func(config.Config, authoritysql.Permission) (routeStore, error)
}

func defaultDeps() deps {
	return deps{openMaintenance: openMaintenance, openRouteStore: openRouteStore}
}

// Run 是 route_tool 的入口:args 不含程序名(`-f <yaml> <子命令> [参数]`),返回退出码,不调 os.Exit。
// stdout 只写一行统计(给脚本解析),其余全部进 stderr(含 go-zero logx 的输出)。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, defaultDeps())
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
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
	// 并只留 Error 以上 —— 连接目标由 Target 行给出(只有 host 与库名)。
	logx.SetWriter(logx.NewWriter(stderr))
	logx.SetLevel(logx.ErrorLevel)
	logx.DisableStat()

	// 用 conf.Load 而不是 conf.MustLoad:后者出错时 log.Fatalf 直接退出 1,退出码就不归本工具管了。
	var c config.Config
	if err := conf.Load(a.configPath, &c); err != nil {
		fmt.Fprintf(stderr, "route_tool: load %s: %v\n", a.configPath, err)
		return routetool.ExitFailed
	}
	if _, err := authoritysql.ParseBackend(c.SnapshotMySQL.Backend); err != nil {
		fmt.Fprintf(stderr, "route_tool: invalid SnapshotMySQL.Backend in %s: %v\n", a.configPath, err)
		return routetool.ExitFailed
	}
	c.RouteStore = c.RouteStore.Normalize()
	if err := c.RouteStore.Validate(); err != nil {
		fmt.Fprintf(stderr, "route_tool: invalid RouteStore config in %s: %v\n", a.configPath, err)
		return routetool.ExitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if a.sub == subTiDBPrepare {
		return runTiDBPrepare(ctx, a, c, d, stdout, stderr)
	}
	return runMaintenance(ctx, a, c, d, stdout, stderr)
}

// runTiDBPrepare 是 tidb-prepare 的装配:只开全局库的 RouteStore(探针照跑:TiDB 上同时证明悲观事务与
// chk_route_shard 已就绪、模数与本二进制一致)。失败原因由 routetool.TiDBPrepare 打在 stderr(FAILED 行)。
func runTiDBPrepare(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) (code int) {
	permission, err := tidbPreparePermission(a.dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	st, err := d.openRouteStore(c, permission)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: cannot open the route tables on the global DB (%s): %v\n", store.OpenRouteStoreHint(a.configPath, err), err)
		return routetool.ExitFailed
	}
	defer closeGlobalDB(&code, st, stderr)

	rep, err := routetool.TiDBPrepare(ctx, st, routetool.TiDBPrepareOptions{
		Zones:    a.zones,
		Storages: a.storages,
		DryRun:   a.dryRun,
		Target:   globalDBTarget(a, c),
		Stdout:   stdout,
		Stderr:   stderr,
	})
	return routetool.TiDBPrepareExitCode(rep, err)
}

// runMaintenance 是 freeze / unfreeze 的装配:开维护连接(核完原件、票据与当前目录状态),提交一次,关连接。
func runMaintenance(ctx context.Context, a cliArgs, c config.Config, d deps, stdout, stderr io.Writer) (code int) {
	maintenance, err := d.openMaintenance(ctx, c, a)
	if err != nil {
		fmt.Fprintf(stderr, "route_tool: %v\n", err)
		return routetool.ExitFailed
	}
	defer closeGlobalDB(&code, maintenance.db, stderr)

	if a.sub == subFreeze {
		err = routetool.Freeze(ctx, routetool.FreezeOptions{
			Catalog: maintenance.catalog, TicketID: maintenance.ticketID, Request: maintenance.request,
			Reason: a.reason, Timeout: a.freezeTimeout, Target: globalDBTarget(a, c), Stdout: stdout, Stderr: stderr,
		})
	} else {
		err = routetool.Unfreeze(ctx, routetool.UnfreezeOptions{
			Catalog: maintenance.catalog, TicketID: maintenance.ticketID, Request: maintenance.request,
			Timeout: a.freezeTimeout, Target: globalDBTarget(a, c), Stdout: stdout, Stderr: stderr,
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "route_tool %s: %v\n", a.sub, err)
	}
	return routetool.ExitCode(err)
}

// globalDBTarget 是 stderr 第一行:只打 host 与库名,不打 DSN / 口令。
func globalDBTarget(a cliArgs, c config.Config) string {
	return fmt.Sprintf("route_tool %s: global_db=%s/%s", a.sub, c.SnapshotMySQL.Host, c.SnapshotMySQL.DBName)
}

// closeGlobalDB 关闭本次命令持有的 SQL 连接。关闭失败算整次失败:不能确认连接上的语句都结束了。
func closeGlobalDB(code *int, db io.Closer, stderr io.Writer) {
	if err := db.Close(); err != nil {
		fmt.Fprintf(stderr, "route_tool: close the global DB connection: %v\n", err)
		*code = routetool.ExitFailed
	}
}

// parseArgs 解析 `-f <yaml> <子命令> [子命令参数]`。子命令只认三个,参数按子命令登记(别的子命令的参数一律
// 「flag provided but not defined」);参数越界、多余参数一律报错(退出码 2)。
func parseArgs(args []string, stderr io.Writer) (cliArgs, error) {
	a := cliArgs{
		dryRun:        true,
		timeout:       30 * time.Minute,
		freezeTimeout: store.RouteFreezeTimeoutMin,
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
	allSubs := strings.Join([]string{subTiDBPrepare, subFreeze, subUnfreeze}, " | ")
	if len(rest) == 0 {
		return a, fmt.Errorf("missing subcommand (%s)", allSubs)
	}
	a.sub = rest[0]

	sub := flag.NewFlagSet("route_tool "+a.sub, flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.DurationVar(&a.timeout, "timeout", a.timeout, "整次运行的时限;超时退出码 2(三个子命令都幂等,重跑即可)")
	var zonesRaw, storagesRaw string
	switch a.sub {
	case subTiDBPrepare:
		sub.StringVar(&zonesRaw, "zones", "", fmt.Sprintf("要预切的 zone,逗号分隔(必填),每个在 [1, %d]", store.RouteHomeZoneMax))
		sub.StringVar(&storagesRaw, "storages", "", "idx_route_storage 要预切的落点值,逗号分隔(可空;新角色落点配成 1000000 时传 1000000)")
		sub.BoolVar(&a.dryRun, "dry-run", a.dryRun, "只判引擎、核对聚簇、列出语句;真执行用 -dry-run=false")
	case subFreeze, subUnfreeze:
		sub.StringVar(&a.maintenanceFile, "maintenance-request", "", "绝对路径的正式维护原件（protojson）")
		sub.StringVar(&a.maintenanceSHA, "maintenance-sha256", "", "原件文件固定 SHA-256")
		sub.StringVar(&a.ticketHex, "ticket-id", "", "正式票据 ID（32 位十六进制）")
		sub.DurationVar(&a.freezeTimeout, "freeze-timeout", a.freezeTimeout,
			fmt.Sprintf("冻结 / 解冻写本身的时限,至少 %s;TiDB 把 route_meta 设成缓存表后要 >= tidb_table_cache_lease + 2s", store.RouteFreezeTimeoutMin))
		if a.sub == subFreeze {
			sub.StringVar(&a.reason, "reason", "", "冻结原因,只打到 stderr(库里没有记原因的列)")
		}
	default:
		return a, fmt.Errorf("unknown subcommand %q (want %s)", a.sub, allSubs)
	}
	if err := sub.Parse(rest[1:]); err != nil {
		return a, err
	}
	if sub.NArg() > 0 {
		return a, fmt.Errorf("unexpected arguments after %s: %v", a.sub, sub.Args())
	}
	if a.timeout <= 0 {
		return a, fmt.Errorf("-timeout=%s must be positive", a.timeout)
	}
	if a.sub == subTiDBPrepare {
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
		return a, nil
	}
	if a.freezeTimeout < store.RouteFreezeTimeoutMin {
		return a, fmt.Errorf("-freeze-timeout=%s must be at least %s", a.freezeTimeout, store.RouteFreezeTimeoutMin)
	}
	if !filepath.IsAbs(a.maintenanceFile) || len(a.maintenanceSHA) != 64 {
		return a, errors.New("维护操作必须指定原件绝对路径与固定 SHA-256")
	}
	if value, err := hex.DecodeString(a.ticketHex); err != nil || len(value) != 16 {
		return a, errors.New("维护操作缺少正式票据 ID")
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

// openRouteStore 沿正式只读启动核验开池:会话、表形状、CHECK 和 writer authority 原件不发 DML。
func openRouteStore(c config.Config, permission authoritysql.Permission) (routeStore, error) {
	cfg := svc.MySQLConfigOf(c)
	cfg.Permission = permission
	cfg.MaxOpenConn = c.RouteStore.MaxOpenConn
	cfg.MaxIdleConn = c.RouteStore.MaxIdleConn
	host, _ := os.Hostname() // 取不到时 NewRouteWriterID 用 "unknown"
	st, err := store.NewRouteStore(cfg, store.RouteStoreOptions{Writer: store.NewRouteWriterID("route_tool", host)})
	if err != nil {
		// 不能把 nil 的 *store.RouteStore 包进接口返回:那是「非 nil 接口包着 nil 指针」,调用方的判空拦不住。
		return nil, err
	}
	return st, nil
}

// tidbPreparePermission 选 tidb-prepare 的 SQL 主体:真执行只用独立的 Schema 主体,dry-run 只读。
// freeze / unfreeze 的 Operator 主体在 openMaintenance 里加载。
func tidbPreparePermission(dryRun bool) (authoritysql.Permission, error) {
	if dryRun {
		return authoritysql.Reader(), nil
	}
	return authoritysql.LoadSchemaPermission(os.Getenv)
}

// maintenanceInput 只保存本次命令的正式原件与连接,不持久化另一份维护协议。
type maintenanceInput struct {
	db       io.Closer
	catalog  routetool.MaintenanceCatalog
	request  *pb.SqlWriterMaintenanceReceipt
	ticketID []byte
}

// openMaintenance 读并核对维护原件(文件摘要、请求摘要、部署身份、库名),都对了才连库;连上之后再核当前目录状态与票据。
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
	if request.CommittedMs != 0 || len(request.ReceiptDigest) != 0 {
		return bad()
	}
	permission, err := authoritysql.LoadOperatorPermission(os.Getenv)
	if err != nil {
		return nil, err
	}
	cfg := svc.MySQLConfigOf(c)
	dsn := mysql.NewConfig()
	dsn.User = cfg.User
	dsn.Passwd = cfg.Password
	dsn.Net = "tcp"
	dsn.Addr = cfg.Host
	dsn.DBName = plan.AuthoritySchema
	dsn.ParseTime = true
	db, err := authoritysql.Open(ctx, dsn.FormatDSN(), plan.AuthoritySchema, cfg.Backend, 2, permission)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*maintenanceInput, error) { _ = db.Close(); return nil, err }
	var principal string
	if err = db.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		return fail(err)
	}
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{AuthoritySchema: plan.AuthoritySchema, Generation: request.Generation, ProtectedSchemas: plan.ProtectedSchemas, PhysicalAuthoritySHA256: physical, Principal: principal})
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
	if meta.WriterEpoch != request.BeforeEpoch {
		// 仅同一请求的永久成功原件允许幂等重读，不能把新请求伪装成旧 epoch 的重试。
		stored, err := catalog.MaintenanceReceipt(ctx, request.RequestId)
		if err != nil || !bytes.Equal(stored.RequestDigest, request.RequestDigest) || meta.WriterEpoch != request.AfterEpoch {
			return fail(errors.New("epoch 已变化且没有匹配的永久请求结果"))
		}
	}
	ticketID, err := hex.DecodeString(a.ticketHex)
	if err != nil || len(ticketID) != 16 {
		return fail(errors.New("票据 ID 无效"))
	}
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
	return &maintenanceInput{db: db, catalog: catalog, request: request, ticketID: ticketID}, nil
}
