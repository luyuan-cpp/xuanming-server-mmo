package main

// dbmove —— 不停服把一个逻辑库从物理集群 A 搬到集群 B(设计 docs/design/horizontal-scale-logical-db.md §8,操作 docs/ops/db-move-runbook.md)。
//
//	dbmove plan     -db <逻辑库> -target-cluster <集群> [-run-id <id>] [-roster <名册>[,<名册>]]
//	dbmove prepare  -db <逻辑库> -target-cluster <集群> -run-id <id> -roster <名册>[,<名册>]
//	dbmove verify   -db <逻辑库> -run-id <id> -sync <复制>
//	dbmove cutover  -db <逻辑库> -run-id <id> -sync <复制> -roster <名册>[,<名册>] -metrics-targets host:port[,host:port] [-budget 30s] [-max-lag 3s] [-verify-max-age 6h]
//	dbmove rollback -db <逻辑库> -run-id <id> [-target-lost]
//	dbmove audit    -db <逻辑库> -run-id <id> -metrics-targets host:port[,host:port]
//	dbmove cleanup  -db <逻辑库> -run-id <id> -confirm-drop [-retention 168h]
//
// 退出码:
//
//	0  完成(含幂等:这一步已经做过)
//	1  拒绝或核对不一致:这一步什么都没改
//	2  没做成:状态是干净的,重跑同一条命令是安全的
//	3  结局未知:源库可能还冻着,或者翻转结局未知。按输出重跑 cutover 或 rollback,读到的映射决定结局
//
// 连接串和主体都从环境变量读,不走命令行(进程列表看得见),任何输出都不回显:
//
//	MMORPG_DB_PLACEMENT                     和服务 yaml 的 DbPlacement 同形状的 JSON:etcd、默认集群、集群地址
//	MMORPG_SQL_BACKEND                      tidb(缺省)或 mysql
//	MMORPG_SQL_OPERATOR_PRINCIPAL           维护身份,这个账号 SELECT CURRENT_USER() 的结果
//	MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256    物理身份摘要,按部署一份,搬库不换
//	DBMOVE_SOURCE_DSN、DBMOVE_TARGET_DSN    两侧的维护身份:闸门和哨兵
//	DBMOVE_SOURCE_ADMIN_DSN、DBMOVE_TARGET_ADMIN_DSN
//	                                        两侧的管理员:建库、删库、校验和、读复制进度、账号(权限见手册 §2)
//	DBMOVE_REGISTRAR_PRINCIPAL、MMORPG_SQL_SCHEMA_PRINCIPAL
//	                                        注册和 schema 主体,cutover 必填:翻转之前核它们在目标上存在、授权齐全
//	DBMOVE_VERIFY_SOURCE_DSN                mysql 档 verify 用的源库副本
//	DBMOVE_AUDIT_TARGET_DSN                 mysql 档 audit 用的目标库副本

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/dbplacement"
)

const (
	cmdPlan     = "plan"
	cmdPrepare  = "prepare"
	cmdVerify   = "verify"
	cmdCutover  = "cutover"
	cmdRollback = "rollback"
	cmdAudit    = "audit"
	cmdCleanup  = "cleanup"

	envSourceDSN       = "DBMOVE_SOURCE_DSN"
	envTargetDSN       = "DBMOVE_TARGET_DSN"
	envSourceAdminDSN  = "DBMOVE_SOURCE_ADMIN_DSN"
	envTargetAdminDSN  = "DBMOVE_TARGET_ADMIN_DSN"
	envVerifySourceDSN = "DBMOVE_VERIFY_SOURCE_DSN"
	envAuditTargetDSN  = "DBMOVE_AUDIT_TARGET_DSN"
	envPhysical        = "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"
	envOperator        = "MMORPG_SQL_OPERATOR_PRINCIPAL"
	envRegistrar       = "DBMOVE_REGISTRAR_PRINCIPAL"
	envSchema          = "MMORPG_SQL_SCHEMA_PRINCIPAL"

	etcdCallTimeout = 5 * time.Second
	openTimeout     = 30 * time.Second
	scrapeTimeout   = 5 * time.Second
	ticdcTimeout    = 3 * time.Second
	scrapeLimit     = 64 << 20
	rosterFormat    = "SQL_WRITER_BOOT_ROSTER_V2"
)

const usage = `usage: dbmove <plan|prepare|verify|cutover|rollback|audit|cleanup> -db <logical db> [flags]
see docs/ops/db-move-runbook.md`

type options struct {
	command             string
	db                  string
	targetCluster       string
	runID               string
	sync                string
	timeout             time.Duration
	maxLag              time.Duration
	budget              time.Duration
	rollbackBudget      time.Duration
	verifyMaxAge        time.Duration
	retention           time.Duration
	confirmDrop         bool
	acknowledgeNoAudit  bool
	allowAddrMismatch   bool
	allowSharedAccounts bool
	targetLost          bool
	metricsTargets      []string
	rosterFiles         []string
	logDir              string
	syncSpec            syncSpec
}

// commandTimeout 是一条命令的总时限。校验和按表跑,大库要很久;cutover 这里是预检的时限,切换窗口有自己的 -budget。
func commandTimeout(command string) time.Duration {
	switch command {
	case cmdVerify, cmdAudit:
		return 12 * time.Hour
	case cmdCleanup:
		return 30 * time.Minute
	default:
		return 2 * time.Minute
	}
}

// parseOptions 解析并校验命令行。纯函数(不读环境、不连库、不连 etcd):校验失败的命令不碰任何东西。
func parseOptions(args []string, errOut io.Writer) (options, error) {
	if len(args) == 0 {
		return options{}, errors.New(usage)
	}
	var o options
	o.command = args[0]
	switch o.command {
	case cmdPlan, cmdPrepare, cmdVerify, cmdCutover, cmdRollback, cmdAudit, cmdCleanup:
	case "-h", "-help", "--help":
		fmt.Fprintln(errOut, usage)
		return options{}, flag.ErrHelp
	default:
		return options{}, fmt.Errorf("unknown command %q\n%s", o.command, usage)
	}
	fs := flag.NewFlagSet("dbmove "+o.command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&o.db, "db", "", "logical database to move (required)")
	fs.StringVar(&o.targetCluster, "target-cluster", "", "plan, prepare: cluster name in SqlClusters to move to")
	fs.StringVar(&o.runID, "run-id", "", "32 hex digits, the same for every step of one move (plan prints one)")
	fs.StringVar(&o.sync, "sync", "", "verify, cutover: ticdc:<openapi address>:<changefeed> or mysql-gtid[:<channel>]")
	fs.DurationVar(&o.timeout, "timeout", commandTimeout(o.command), "time limit of the command; for cutover, of the checks before the freeze")
	fs.DurationVar(&o.maxLag, "max-lag", 3*time.Second, "cutover: refuse to freeze when replication lags more than this")
	fs.DurationVar(&o.budget, "budget", 30*time.Second, "cutover: longest the source stays frozen before the tool rolls back")
	fs.DurationVar(&o.rollbackBudget, "rollback-budget", 15*time.Second, "cutover: when the window aborts, time to invalidate the flip, and again to unfreeze the source")
	fs.DurationVar(&o.verifyMaxAge, "verify-max-age", 6*time.Hour, "cutover: refuse when the last successful verify started longer ago than this")
	fs.DurationVar(&o.retention, "retention", 7*24*time.Hour, "cleanup: refuse to drop the source until this long after the flip")
	fs.BoolVar(&o.confirmDrop, "confirm-drop", false, "cleanup: really drop the source database")
	fs.BoolVar(&o.acknowledgeNoAudit, "acknowledge-no-audit", false, "cleanup: drop even though audit never succeeded for this run")
	fs.BoolVar(&o.allowAddrMismatch, "allow-addr-mismatch", false, "accept DSNs whose address differs from SqlClusters (a tunnel to the same cluster)")
	fs.BoolVar(&o.allowSharedAccounts, "allow-shared-writer-accounts", false, "plan, prepare: leave writer accounts that also write other databases of the target cluster unlocked")
	fs.BoolVar(&o.targetLost, "target-lost", false, "rollback: the target cannot be reached and never took writes: skip the flip marker check and do not lock its accounts")
	metrics := fs.String("metrics-targets", "", "audit, cutover: host:port of every process that uses this database, comma separated")
	roster := fs.String("roster", "", "plan, prepare, cutover: boot roster file of every service that writes this database, comma separated")
	fs.StringVar(&o.logDir, "log-dir", filepath.Join("run", "dbmove"), "directory of the local step log")
	if err := fs.Parse(args[1:]); err != nil {
		return options{}, err
	}
	// 不回显内容:多出来的参数可能是写错位置的口令。
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("%d unexpected positional argument(s); their values are not echoed", fs.NArg())
	}
	if !identifierRe.MatchString(o.db) {
		return options{}, errors.New("-db must name a logical database (letters, digits, underscore)")
	}
	if o.command != cmdPlan || o.runID != "" {
		raw, err := hex.DecodeString(o.runID)
		if err != nil || len(raw) != 16 {
			return options{}, errors.New("-run-id must be 32 hex digits (dbmove plan prints one)")
		}
		o.runID = strings.ToLower(o.runID)
	}
	o.metricsTargets = splitList(*metrics)
	o.rosterFiles = splitList(*roster)
	switch o.command {
	case cmdPlan, cmdPrepare:
		if o.targetCluster == "" {
			return options{}, errors.New("-target-cluster is required")
		}
		if o.command == cmdPrepare && len(o.rosterFiles) == 0 {
			return options{}, errors.New("-roster is required: prepare locks every writer account of the rosters on the target")
		}
	case cmdVerify, cmdCutover:
		spec, err := parseSync(o.sync)
		if err != nil {
			return options{}, err
		}
		o.syncSpec = spec
		if o.command == cmdCutover && len(o.rosterFiles) == 0 {
			return options{}, errors.New("-roster is required: cutover checks every writer account of the rosters on the target")
		}
		if o.command == cmdCutover && len(o.metricsTargets) == 0 {
			return options{}, errors.New("-metrics-targets is required: the host:port of every process that uses this database, cutover checks they know the target cluster")
		}
	case cmdAudit:
		if len(o.metricsTargets) == 0 {
			return options{}, errors.New("-metrics-targets is required: the host:port of every process that uses this database")
		}
	}
	if o.budget <= 0 || o.rollbackBudget <= 0 || o.timeout <= 0 || o.maxLag <= 0 || o.verifyMaxAge <= 0 || o.retention < 0 {
		return options{}, errors.New("durations must be positive")
	}
	return o, nil
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
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
	// Ctrl-C 和 SIGTERM 取消 ctx:切换窗口里等于超预算,走同一条回滚。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, closeAll, err := wire(ctx, o, getenv, out)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		return exitCodeOf(err)
	}
	defer closeAll()
	err = execute(ctx, m, o)
	if err != nil {
		fmt.Fprintf(errOut, "ERROR: %v\n", err)
		m.log.record(o.command, "ERROR: "+err.Error())
	}
	return exitCodeOf(err)
}

func execute(ctx context.Context, m *mover, o options) error {
	if o.command == cmdCutover {
		return m.cutover(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	switch o.command {
	case cmdPlan:
		return m.plan(ctx)
	case cmdPrepare:
		return m.prepare(ctx)
	case cmdVerify:
		return m.verify(ctx)
	case cmdRollback:
		return m.rollback(ctx)
	case cmdAudit:
		return m.audit(ctx)
	case cmdCleanup:
		return m.cleanup(ctx)
	}
	return refusedf("unknown command %q", o.command)
}

// timedStore 给每次 etcd 调用加时限:etcd 不通时不把工具挂住。结局由调用方按错误处理。
type timedStore struct {
	inner *dbplacement.Store
}

func (s timedStore) Get(ctx context.Context, logicalDB string) (dbplacement.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, etcdCallTimeout)
	defer cancel()
	return s.inner.Get(ctx, logicalDB)
}

func (s timedStore) List(ctx context.Context) (map[string]dbplacement.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, etcdCallTimeout)
	defer cancel()
	return s.inner.List(ctx)
}

func (s timedStore) CompareAndPut(ctx context.Context, logicalDB string, expected int64, p dbplacement.Placement) error {
	ctx, cancel := context.WithTimeout(ctx, etcdCallTimeout)
	defer cancel()
	return s.inner.CompareAndPut(ctx, logicalDB, expected, p)
}

func (s timedStore) DefaultCluster(ctx context.Context) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, etcdCallTimeout)
	defer cancel()
	return s.inner.DefaultCluster(ctx)
}

// dsnAddr 取连接串的地址,不回显连接串。
func dsnAddr(name, dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("cannot parse %s (its value is not echoed)", name)
	}
	return cfg.Addr, nil
}

// loadRosterPrincipals 从槽位名册取写这个逻辑库的全部主体:每个槽位一个账号,同槽位各代共用(writer-roster-exhaustion-20261106.md §5.1),
// 还没起过的槽位(扩容时才用到的序号)也算在内。
// 一个逻辑库可能有几个服务在写(存档库:go/db 和 data_service),所以可以给多个文件。文件里没有这个库,多半是给错了文件。
func loadRosterPrincipals(paths []string, db string) ([]string, error) {
	var out []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the roster: %w", err)
		}
		roster := &databasepb.SqlWriterBootRoster{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, roster); err != nil || roster.Format != rosterFormat {
			return nil, fmt.Errorf("%s is not a %s file", path, rosterFormat)
		}
		found := false
		for _, entry := range roster.Entries {
			for _, request := range entry.GetRequests() {
				if request.GetAuthoritySchema() != db {
					continue
				}
				principal := string(request.GetWriter().GetSqlPrincipal())
				if principal == "" {
					return nil, fmt.Errorf("roster %s has a request of %s without a principal", path, db)
				}
				found = true
				out = append(out, principal)
			}
		}
		if !found {
			return nil, fmt.Errorf("roster %s has no request for %s: wrong file?", path, db)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// wire 按命令打开它要用的东西。一条命令只碰它真正要的连接:plan 不写任何库,audit 不碰闸门。
func wire(ctx context.Context, o options, getenv func(string) string, out io.Writer) (*mover, func(), error) {
	config, err := dbplacement.ConfigFromEnv(getenv)
	if err != nil {
		return nil, nil, refusedf("%v", err)
	}
	if !config.Enabled() {
		return nil, nil, refusedf("%s is not set: the tool needs the same DbPlacement config as the services", dbplacement.EnvConfig)
	}
	backend, err := authoritysql.LoadBackend(getenv)
	if err != nil {
		return nil, nil, refusedf("%v", err)
	}
	if o.syncSpec.backend != "" && o.syncSpec.backend != backend {
		return nil, nil, refusedf("-sync does not fit %s=%s", authoritysql.BackendEnv, backend)
	}
	physical, err := hex.DecodeString(getenv(envPhysical))
	if err != nil || len(physical) != 32 {
		return nil, nil, refusedf("%s must be the 64 hex digit physical authority digest", envPhysical)
	}
	permission, err := authoritysql.LoadOperatorPermission(getenv)
	if err != nil {
		return nil, nil, refusedf("set %s to the CURRENT_USER() of the maintenance account: %v", envOperator, err)
	}
	principal := getenv(envOperator)
	rosterPrincipals, err := loadRosterPrincipals(o.rosterFiles, o.db)
	if err != nil {
		return nil, nil, refusedf("%v", err)
	}
	var controlPrincipals []string
	for _, env := range []string{envRegistrar, envSchema} {
		if value := getenv(env); value != "" {
			controlPrincipals = append(controlPrincipals, value)
		} else if o.command == cmdCutover {
			return nil, nil, refusedf("%s is not set: cutover checks this account on the target before the flip", env)
		}
	}
	runIDBytes, _ := hex.DecodeString(o.runID)
	journal, err := openJournal(o.logDir, o.db, o.runID, out, time.Now)
	if err != nil {
		return nil, nil, fmt.Errorf("open the step log: %w", err)
	}
	var closers []func() error
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if err := closers[i](); err != nil {
				journal.write("close", "WARN: "+err.Error())
			}
		}
	}
	closers = append(closers, journal.Close)
	fail := func(err error) (*mover, func(), error) {
		closeAll()
		return nil, nil, err
	}

	client, err := dbplacement.Dial(config)
	if err != nil {
		return fail(fmt.Errorf("connect etcd: %w", err))
	}
	closers = append(closers, client.Close)
	m := &mover{
		o: moverOptions{
			db: o.db, targetCluster: o.targetCluster, runID: o.runID, runIDBytes: runIDBytes, operator: principal,
			config: config, allowAddrMismatch: o.allowAddrMismatch, backend: string(backend),
			maxLag: o.maxLag, budget: o.budget, rollbackBudget: o.rollbackBudget, preflight: o.timeout,
			verifyMaxAge: o.verifyMaxAge, retention: o.retention, pollInterval: 200 * time.Millisecond,
			confirmDrop: o.confirmDrop, acknowledgeNoAudit: o.acknowledgeNoAudit, metricsTargets: o.metricsTargets,
			rosterPrincipals: rosterPrincipals, controlPrincipals: controlPrincipals, allowSharedAccounts: o.allowSharedAccounts, targetLost: o.targetLost,
		},
		store:        timedStore{inner: dbplacement.NewStore(client)},
		scrape:       scrapeVersion,
		knownCluster: scrapeKnownCluster,
		now:          time.Now,
		sleep:        sleepContext,
		log:          journal,
	}

	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	openGateFrom := func(env string) (*sqlGate, error) {
		dsn := getenv(env)
		if dsn == "" {
			return nil, refusedf("%s is not set", env)
		}
		gate, err := openGate(openCtx, dsn, o.db, backend, principal, physical, permission)
		if err != nil {
			return nil, fmt.Errorf("open %s through %s: %w", o.db, env, err)
		}
		closers = append(closers, gate.Close)
		return gate, nil
	}
	openAdminFrom := func(env string) (*sqlAdmin, error) {
		dsn := getenv(env)
		if dsn == "" {
			return nil, refusedf("%s is not set", env)
		}
		admin, err := openAdmin(dsn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		closers = append(closers, admin.Close)
		return admin, nil
	}
	addrOf := func(target *string, env string) error {
		if getenv(env) == "" {
			return refusedf("%s is not set", env)
		}
		addr, err := dsnAddr(env, getenv(env))
		if err != nil {
			return refusedf("%v", err)
		}
		*target = addr
		return nil
	}

	switch o.command {
	case cmdPlan, cmdPrepare:
		if m.source, err = gateSideFrom(openGateFrom(envSourceDSN)); err != nil {
			return fail(err)
		}
		if m.targetAdmin, err = adminSideFrom(openAdminFrom(envTargetAdminDSN)); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.sourceAddr, envSourceDSN); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.targetAddr, envTargetAdminDSN); err != nil {
			return fail(err)
		}
	case cmdVerify:
		repl, err := openReplication(o, getenv, backend, &closers)
		if err != nil {
			return fail(err)
		}
		m.repl = repl
		if err := addrOf(&m.o.sourceAdminAddr, envSourceAdminDSN); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.targetAdminAddr, envTargetAdminDSN); err != nil {
			return fail(err)
		}
		sourceDSN := getenv(envSourceAdminDSN)
		if backend == authoritysql.BackendMySQL {
			if sourceDSN = getenv(envVerifySourceDSN); sourceDSN == "" {
				return fail(refusedf("the mysql backend verifies on a replica of the source, CHECKSUM TABLE takes a read lock: set %s", envVerifySourceDSN))
			}
		}
		m.openView = viewOpener(o.db, backend, sourceDSN, getenv(envTargetAdminDSN))
	case cmdCutover:
		if m.source, err = gateSideFrom(openGateFrom(envSourceDSN)); err != nil {
			return fail(err)
		}
		if m.target, err = gateSideFrom(openGateFrom(envTargetDSN)); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.sourceAddr, envSourceDSN); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.targetAddr, envTargetDSN); err != nil {
			return fail(err)
		}
		if m.sourceAdmin, err = adminSideFrom(openAdminFrom(envSourceAdminDSN)); err != nil {
			return fail(err)
		}
		if m.targetAdmin, err = adminSideFrom(openAdminFrom(envTargetAdminDSN)); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.sourceAdminAddr, envSourceAdminDSN); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.targetAdminAddr, envTargetAdminDSN); err != nil {
			return fail(err)
		}
		repl, err := openReplication(o, getenv, backend, &closers)
		if err != nil {
			return fail(err)
		}
		m.repl = repl
	case cmdRollback:
		if m.source, err = gateSideFrom(openGateFrom(envSourceDSN)); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.sourceAddr, envSourceDSN); err != nil {
			return fail(err)
		}
		if !o.targetLost {
			if m.targetAdmin, err = adminSideFrom(openAdminFrom(envTargetAdminDSN)); err != nil {
				return fail(err)
			}
			if err := addrOf(&m.o.targetAdminAddr, envTargetAdminDSN); err != nil {
				return fail(err)
			}
		}
	case cmdAudit:
		if err := addrOf(&m.o.sourceAdminAddr, envSourceAdminDSN); err != nil {
			return fail(err)
		}
		targetDSN := getenv(envTargetAdminDSN)
		if backend == authoritysql.BackendMySQL {
			if targetDSN = getenv(envAuditTargetDSN); targetDSN == "" {
				return fail(refusedf("the target takes writes after the flip, the mysql backend audits on a replica stopped at the recorded point: set %s", envAuditTargetDSN))
			}
		} else if err := addrOf(&m.o.targetAdminAddr, envTargetAdminDSN); err != nil {
			return fail(err)
		}
		m.openView = viewOpener(o.db, backend, getenv(envSourceAdminDSN), targetDSN)
	case cmdCleanup:
		if m.sourceAdmin, err = adminSideFrom(openAdminFrom(envSourceAdminDSN)); err != nil {
			return fail(err)
		}
		if err := addrOf(&m.o.sourceAddr, envSourceAdminDSN); err != nil {
			return fail(err)
		}
	}
	return m, closeAll, nil
}

// gateSideFrom、adminSideFrom:不能把 nil 的具体指针包进接口,判空会拦不住。
func gateSideFrom(gate *sqlGate, err error) (gateSide, error) {
	if err != nil {
		return nil, err
	}
	return gate, nil
}

func adminSideFrom(admin *sqlAdmin, err error) (adminSide, error) {
	if err != nil {
		return nil, err
	}
	return admin, nil
}

func viewOpener(schema string, backend authoritysql.Backend, sourceDSN, targetDSN string) func(context.Context, string, string) (checksumView, error) {
	return func(ctx context.Context, role, snapshot string) (checksumView, error) {
		dsn := sourceDSN
		if role == roleTarget {
			dsn = targetDSN
		}
		if dsn == "" {
			return nil, refusedf("no DSN for the %s side", role)
		}
		view, err := openChecksumView(ctx, dsn, schema, backend, snapshot)
		if err != nil {
			return nil, err
		}
		return view, nil
	}
}

// openReplication 建复制进度的读取。用管理员连接串,只读:维护身份读不了 tidb_cdc 和 performance_schema(手册 §2)。
func openReplication(o options, getenv func(string) string, backend authoritysql.Backend, closers *[]func() error) (replication, error) {
	openFrom := func(env string) (*sql.DB, error) {
		dsn := getenv(env)
		if dsn == "" {
			return nil, refusedf("%s is not set", env)
		}
		db, err := openRaw(dsn, 2)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		*closers = append(*closers, db.Close)
		return db, nil
	}
	source, err := openFrom(envSourceAdminDSN)
	if err != nil {
		return nil, err
	}
	target, err := openFrom(envTargetAdminDSN)
	if err != nil {
		return nil, err
	}
	if backend == authoritysql.BackendTiDB {
		return &ticdcReplication{api: o.syncSpec.api, changefeed: o.syncSpec.changefeed, client: &http.Client{Timeout: ticdcTimeout}, source: source, target: target, schema: o.db}, nil
	}
	repl := &gtidReplication{channel: o.syncSpec.channel, source: source, target: target, schema: o.db}
	if getenv(envVerifySourceDSN) != "" {
		if repl.verifySource, err = openFrom(envVerifySourceDSN); err != nil {
			return nil, err
		}
	}
	return repl, nil
}

// fetchMetrics 读一个进程的 /metrics。
func fetchMetrics(ctx context.Context, target string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	url := target
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/metrics", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, scrapeLimit))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// scrapeVersion 读一个进程 /metrics 里的 db_placement_version{logical_db="..."}。
func scrapeVersion(ctx context.Context, target, logicalDB string) (uint64, bool, error) {
	body, err := fetchMetrics(ctx, target)
	if err != nil {
		return 0, false, err
	}
	return parseVersionMetric(body, logicalDB)
}

// scrapeKnownCluster 读一个进程 /metrics 里的 db_placement_known_cluster{cluster="..."}。
func scrapeKnownCluster(ctx context.Context, target, cluster string) (bool, error) {
	body, err := fetchMetrics(ctx, target)
	if err != nil {
		return false, err
	}
	return parseKnownClusterMetric(body, cluster), nil
}

// parseKnownClusterMetric 从 Prometheus 文本里找这个集群。标签只有 cluster 一个,值恒为 1。
func parseKnownClusterMetric(body, cluster string) bool {
	prefix := `db_placement_known_cluster{cluster="` + cluster + `"} `
	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && number > 0
	}
	return false
}

// parseVersionMetric 从 Prometheus 文本里取版本。标签只有 logical_db 一个,直接比整段前缀。
func parseVersionMetric(body, logicalDB string) (uint64, bool, error) {
	prefix := `db_placement_version{logical_db="` + logicalDB + `"} `
	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || number < 0 {
			return 0, false, fmt.Errorf("bad metric value %q", value)
		}
		return uint64(number), true, nil
	}
	return 0, false, nil
}
