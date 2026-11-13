package main

// 搬库的流程。流程只依赖下面几个接口,单测用内存替身;SQL 在 side.go、checksum.go,复制在 replication.go。
//
// 映射值里的状态机(都写在 etcd 的映射键上,每步用 CAS):
//
//	没有键 / ACTIVE --prepare--> MOVING --cutover 翻转--> ACTIVE(cluster=目标,version+1,提交点)
//	                               |
//	                               +------rollback------> ACTIVE(cluster=源,version 不变)
//
// 闸门的状态在库里(route_meta),不在映射值里:FreezeWriters / ReleaseWriters 按 run-id 幂等,所以任何一步崩了都可以重跑。
//
// 写者账号在目标上的状态(目标的闸门表是复制过来的,writable=1,挡不住提前连上来的写者,所以复制期间靠账号):
//
//	prepare 锁定 --cutover 窗口,翻转之前--> 解锁      rollback 和窗口回滚:再锁一遍,保持锁定
//
// 每种故障的结局(手册 §6 有同一张表):
//
//	冻结之前任何一步失败          什么都没变,退出码 1(拒绝)或 2(没做成)。
//	冻结失败 / 复制断开 / 超预算   先作废翻转,再解冻源库,退出码 2。state 还是 MOVING,可以重跑。复制断开还要清掉 verify 记录。
//	静止比对不一致                 追平之后源库和目标内容不同:verify 之后复制漏了写,或者目标被写过。同上回滚,清掉 verify 记录。
//	翻转 CAS 冲突                 映射被别人改了。这个 run 已翻转算成功;还归这个 run 就回滚;都不是就不动,退出码 3。
//	翻转时 etcd 不通或没有回音     结局未知。作废不了就绝不解冻:解冻之后如果翻转其实成了,旧进程和新进程会各写一边。
//	                              源库保持冻结,退出码 3,等 etcd 恢复后重跑 cutover 或 rollback,读到的映射决定结局。
//	没发过翻转时 etcd 不通         没有人翻转得了,解冻源库恢复服务,退出码 2。
//	解冻源库失败                   源库还冻着,退出码 3,重跑 rollback(幂等)。
//	目标闸门打开过且写者连着       翻转可能已经生效(翻转落地之后 etcd 回档,或者标记还没写),绝不解冻,退出码 3,人工查。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	databasepb "proto/common/database"
	"shared/dbplacement"
)

const (
	exitOK = 0
	// exitRefused:校验拒绝或核对不一致,这一步什么都没改。
	exitRefused = 1
	// exitFailed:没做成,状态是干净的(源库可写或没动过),重跑同一条命令是安全的。
	exitFailed = 2
	// exitUnknown:源库可能还冻着,或者翻转结局未知。按输出重跑 cutover / rollback,读到的映射决定结局。
	exitUnknown = 3
)

var (
	errRefused  = errors.New("refused")
	errMismatch = errors.New("inconsistent")
	errUnknown  = errors.New("outcome unknown")

	// errStrayWriter:翻转之前有写者连着目标。闸门表是复制过去的,没冻结时它能过闸,所以它可能已经写过目标,校验记录不再算数。
	errStrayWriter = errors.New("writer accounts are connected to the target before the flip")

	// errTargetDiverged:源库冻结、复制追平之后,目标和源库的内容还是不同。verify 之后复制漏了一段写,或者目标被别人写过,校验记录不再算数。
	// 它不带 errMismatch:窗口已经冻结过又回滚了,退出码和别的回滚原因一样是 2。
	errTargetDiverged = errors.New("the target differs from the frozen source after replication caught up")
)

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRefused, fmt.Sprintf(format, args...))
}

func unknownf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnknown, fmt.Sprintf(format, args...))
}

func exitCodeOf(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUnknown):
		return exitUnknown
	case errors.Is(err, errRefused), errors.Is(err, errMismatch):
		return exitRefused
	default:
		return exitFailed
	}
}

// placementStore 是映射键的读写,生产实现 *dbplacement.Store。
type placementStore interface {
	Get(ctx context.Context, logicalDB string) (dbplacement.Record, error)
	List(ctx context.Context) (map[string]dbplacement.Record, error)
	CompareAndPut(ctx context.Context, logicalDB string, expected int64, p dbplacement.Placement) error
	// DefaultCluster 读 etcd 里记下的默认集群。工具只读不写:首次部署时由 -migrate 和常驻进程写入。
	DefaultCluster(ctx context.Context) (cluster string, found bool, err error)
}

// gateSide 是一侧逻辑库上的闸门和哨兵。账号是维护身份。
type gateSide interface {
	CurrentMeta(ctx context.Context) (*databasepb.RouteMeta, error)
	FreezeWriters(ctx context.Context, runID []byte) (*databasepb.RouteMeta, error)
	ReleaseWriters(ctx context.Context, runID []byte) (*databasepb.RouteMeta, error)
	WriteSentinel(ctx context.Context, nonce, runID []byte, epoch uint64) error
	HasSentinel(ctx context.Context, nonce []byte) (bool, error)
	// ProbeControl 在一个回滚掉的事务里试一遍切换要用的权限:锁 meta 和 slot,写哨兵。
	ProbeControl(ctx context.Context) error
	ActivePrincipals(ctx context.Context) ([]string, error)
	SchemaCharset(ctx context.Context) (charset, collation string, err error)
}

// accountConnection 是一个账号在集群上的一组连接。
type accountConnection struct {
	User     string
	Database string
	Count    int
}

// gateState 是管理员连接读到的闸门状态。rollback 没有目标的维护身份,只有管理员连接。Found=false 表示这个库还没有闸门表。
type gateState struct {
	Found       bool
	Writable    bool
	WriterEpoch uint64
}

// adminSide 是一侧集群上的管理操作:建库、删库、账号。账号是管理员,权限见手册 §2。
type adminSide interface {
	ServerBackend(ctx context.Context) (string, error)
	SchemaExists(ctx context.Context, schema string) (bool, error)
	// TablesWithoutPrimaryKey 列这个库里没有主键的表。
	TablesWithoutPrimaryKey(ctx context.Context, schema string) ([]string, error)
	CreateSchema(ctx context.Context, schema, charset, collation string) error
	DropSchema(ctx context.Context, schema string) error
	AccountExists(ctx context.Context, principal string) (bool, error)
	// GateFrozenBy 报告这个库的闸门是不是被 runID 冻结着。删库之前最后核一次。
	GateFrozenBy(ctx context.Context, schema string, runID []byte) (bool, error)
	// GateState 读这个库的闸门状态。
	GateState(ctx context.Context, schema string) (gateState, error)
	// SetAccountLocked 跑 ALTER USER ... ACCOUNT LOCK / UNLOCK,幂等。账号不随复制走,所以 TiDB 和 MySQL 都只影响这一侧。
	// 已经连着的连接不受影响,所以打开目标之前还要查 Connections。
	SetAccountLocked(ctx context.Context, principal string, locked bool) error
	// AccountLocked 读账号现在是不是锁着。账号不存在是错误。
	AccountLocked(ctx context.Context, principal string) (bool, error)
	// AccountGrants 读 SHOW GRANTS 的原文。账号不存在是错误。
	AccountGrants(ctx context.Context, principal string) ([]string, error)
	// Connections 列这个集群全部实例上的前台连接。
	Connections(ctx context.Context) ([]accountConnection, error)
	// CheckSeesAllConnections 核管理员账号看得到别的账号的连接。看不到时 Connections 不报错,只是查不到。
	CheckSeesAllConnections(ctx context.Context) error
	// HasFlipMarker 读翻转标记。这个库还不在目标上回 false,不是错误。
	HasFlipMarker(ctx context.Context, schema string, nonce []byte) (bool, error)
}

type moverOptions struct {
	db            string
	targetCluster string // plan、prepare 从命令行取,之后从映射值的 Move 取
	runID         string
	runIDBytes    []byte
	operator      string // 维护身份的主体,plan 核它在目标上存在

	config            dbplacement.Config
	sourceAddr        string // 源库连接串的地址,核它和 SqlClusters 一致;只读副本不算
	targetAddr        string
	sourceAdminAddr   string // 管理员连接串的地址,同样要核:校验和、复制进度和账号都走它
	targetAdminAddr   string
	allowAddrMismatch bool

	rosterPrincipals    []string // 名册里这个库的全部写者主体,包括还没注册的
	controlPrincipals   []string // 注册和 schema 主体。不锁,只核存在和授权
	allowSharedAccounts bool     // -allow-shared-writer-accounts
	targetLost          bool     // -target-lost

	backend            string
	maxLag             time.Duration
	budget             time.Duration
	rollbackBudget     time.Duration
	preflight          time.Duration
	verifyMaxAge       time.Duration
	retention          time.Duration
	pollInterval       time.Duration
	confirmDrop        bool
	acknowledgeNoAudit bool
	metricsTargets     []string

	feedEndpoint     string // -sync 里的 TiCDC OpenAPI 地址,MySQL 档为空。cutover 记进映射,cleanup 比
	skipFinalCompare bool   // -skip-final-compare
}

type mover struct {
	o           moverOptions
	store       placementStore
	source      gateSide
	target      gateSide
	sourceAdmin adminSide
	targetAdmin adminSide
	repl        replication
	// feeds 只有 cleanup 用:删源库之前问复制流还在不在。
	feeds feedPresence
	// openView 打开一侧的校验和视图。snapshot 为空表示当前数据。
	openView func(ctx context.Context, role, snapshot string) (checksumView, error)
	// scrape 读一个进程的 db_placement_version;没有这个序列时 found=false。
	scrape func(ctx context.Context, target, logicalDB string) (version uint64, found bool, err error)
	// knownCluster 读一个进程的 db_placement_known_cluster。
	knownCluster func(ctx context.Context, target, cluster string) (bool, error)
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error
	log          *journal
}

const (
	roleSource = "source"
	roleTarget = "target"
)

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *mover) nowMs() uint64 { return uint64(m.now().UnixMilli()) }

func (m *mover) logf(step, format string, args ...any) {
	m.log.write(step, fmt.Sprintf(format, args...))
}

func clonePlacement(p dbplacement.Placement) dbplacement.Placement {
	if p.Move != nil {
		move := *p.Move
		if move.VerifiedFeed != nil {
			feed := *move.VerifiedFeed
			move.VerifiedFeed = &feed
		}
		move.LockedAccounts = slices.Clone(move.LockedAccounts)
		move.SharedAccounts = slices.Clone(move.SharedAccounts)
		p.Move = &move
	}
	return p
}

// sourceCluster 是这个逻辑库现在所在的集群:没有键时是默认集群。
func (m *mover) sourceCluster(rec dbplacement.Record) string {
	if rec.ModRevision == 0 {
		return m.o.config.DefaultCluster
	}
	return rec.Placement.Cluster
}

func (m *mover) flippedByThisRun(p dbplacement.Placement) bool {
	return p.Move != nil && p.Move.RunID == m.o.runID && p.Move.FlippedMs != 0 && p.Cluster == p.Move.TargetCluster
}

func (m *mover) requireMoving(p dbplacement.Placement) error {
	if p.State != dbplacement.StateMoving || p.Move == nil {
		return refusedf("%s is not being moved (state=%q): run prepare first", m.o.db, p.State)
	}
	if p.Move.RunID != m.o.runID {
		return refusedf("%s is being moved by run %s, not %s", m.o.db, p.Move.RunID, m.o.runID)
	}
	return nil
}

// checkEndpoints 核连接串的地址和配置的集群对得上:工具核对过的库,必须就是翻转之后各进程要连的那个。
func (m *mover) checkEndpoints(source, target string) error {
	for _, side := range []struct{ cluster, addr string }{{source, m.o.sourceAddr}, {source, m.o.sourceAdminAddr}, {target, m.o.targetAddr}, {target, m.o.targetAdminAddr}} {
		if side.cluster == "" || side.addr == "" {
			continue
		}
		cluster, ok := m.o.config.SqlClusters[side.cluster]
		if !ok {
			return refusedf("cluster %q is not in SqlClusters of %s", side.cluster, dbplacement.EnvConfig)
		}
		if cluster.Hosts != side.addr && !m.o.allowAddrMismatch {
			return refusedf("the DSN points at %s but cluster %s is %s in SqlClusters: fix the DSN, or pass -allow-addr-mismatch when it goes through a tunnel to the same cluster",
				side.addr, side.cluster, cluster.Hosts)
		}
	}
	return nil
}

// checkNewMove 是开始一次新搬迁之前的判定,plan 和 prepare 共用。
func (m *mover) checkNewMove(ctx context.Context, rec dbplacement.Record) error {
	source := m.sourceCluster(rec)
	if _, ok := m.o.config.SqlClusters[m.o.targetCluster]; !ok {
		return refusedf("target cluster %q is not in SqlClusters of %s: every process must know the address before the flip", m.o.targetCluster, dbplacement.EnvConfig)
	}
	if m.o.targetCluster == source {
		return refusedf("%s is already on cluster %s", m.o.db, source)
	}
	if err := m.checkEndpoints(source, m.o.targetCluster); err != nil {
		return err
	}
	if rec.Placement.State == dbplacement.StateMoving {
		run := ""
		if rec.Placement.Move != nil {
			run = rec.Placement.Move.RunID
		}
		return refusedf("%s is already being moved by run %s", m.o.db, run)
	}
	if move := rec.Placement.Move; move != nil && move.FlippedMs != 0 && move.CleanedMs == 0 {
		return refusedf("the source of the previous move (run %s, cluster %s) is not cleaned up: run cleanup with that run id first", move.RunID, move.SourceCluster)
	}
	// 没有键的库在默认集群上。工具和进程对默认集群的理解不一样时,搬的就不是那个库。
	recorded, found, err := m.store.DefaultCluster(ctx)
	if err != nil {
		return fmt.Errorf("read the default cluster: %w", err)
	}
	if !found {
		return refusedf("etcd has no %s: deploy the services first, the first -migrate records it", dbplacement.DefaultClusterKey)
	}
	if recorded != m.o.config.DefaultCluster {
		return refusedf("DefaultCluster is %q in %s but %q in etcd: fix the config", m.o.config.DefaultCluster, dbplacement.EnvConfig, recorded)
	}
	all, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list the mappings: %w", err)
	}
	for name, other := range all {
		if name != m.o.db && other.Placement.State == dbplacement.StateMoving {
			return refusedf("%s is being moved: move one logical database at a time", name)
		}
	}
	exists, err := m.targetAdmin.SchemaExists(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("check the target: %w", err)
	}
	if exists {
		return refusedf("the target cluster already has a database %s: a move starts from an empty target", m.o.db)
	}
	return m.requirePrimaryKeys(ctx)
}

// requirePrimaryKeys 拒绝搬有无主键表的库。逐行摘要用 BIT_XOR 合并,完全相同的两行互相抵消,这种表比不出差异;
// TiCDC 也不同步既没有主键、又没有非空唯一键的表。verify 按同一个判据再拦一次(noPrimaryKeyError),这里是为了在起复制之前就说清楚。
func (m *mover) requirePrimaryKeys(ctx context.Context) error {
	tables, err := m.sourceAdmin.TablesWithoutPrimaryKey(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("list the tables of %s without a primary key: %w", m.o.db, err)
	}
	if len(tables) != 0 {
		return refusedf("these tables of %s have no primary key and cannot be moved, their contents cannot be verified: %s. Give each a primary key first", m.o.db, strings.Join(tables, ", "))
	}
	return nil
}

// plan 只读,不写 etcd,不改任何库。
func (m *mover) plan(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if err := m.checkNewMove(ctx, rec); err != nil {
		return err
	}
	backend, err := m.targetAdmin.ServerBackend(ctx)
	if err != nil {
		return fmt.Errorf("read the target server type: %w", err)
	}
	if backend != m.o.backend {
		return refusedf("the source is %s but the target is %s: source and target must use the same backend", m.o.backend, backend)
	}
	meta, err := m.source.CurrentMeta(ctx)
	if err != nil {
		return fmt.Errorf("read the source gate: %w", err)
	}
	if meta.Writable != 1 || len(meta.FreezeRunId) != 0 {
		return refusedf("the source gate is frozen by run %x: finish that first", meta.FreezeRunId)
	}
	if err := m.source.ProbeControl(ctx); err != nil {
		return refusedf("the maintenance account cannot control the source gate: %v", err)
	}
	writers, err := m.writerAccounts(ctx, nil)
	if err != nil {
		return err
	}
	if err := m.requireAccountsOnTarget(ctx, m.withControlAccounts(writers)); err != nil {
		return err
	}
	if err := m.targetAdmin.CheckSeesAllConnections(ctx); err != nil {
		return refusedf("the target admin account is not enough: %v", err)
	}
	locked, shared, err := m.partitionWriterAccounts(ctx, writers)
	if err != nil {
		return err
	}
	source := m.sourceCluster(rec)
	m.logf("plan", "ok: %s can move from cluster %s to cluster %s (backend %s, %d writer account(s) checked: prepare locks %d on the target, %d stay open because they also write other databases there)", m.o.db, source, m.o.targetCluster, backend, len(writers), len(locked), len(shared))
	if m.o.runID == "" {
		suggested, err := newRunID()
		if err != nil {
			return err
		}
		m.logf("plan", "use this run id for the next steps: %s", suggested)
	}
	return nil
}

func newRunID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", raw), nil
}

// writerAccounts 是这个库的全部写者账号:源库 ACTIVE 的、名册里的(滚动重启时新 Pod 用名册里下一个)、这次搬迁已经记下的。
func (m *mover) writerAccounts(ctx context.Context, move *dbplacement.Move) ([]string, error) {
	active, err := m.source.ActivePrincipals(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the source writers: %w", err)
	}
	all := slices.Concat(active, m.o.rosterPrincipals)
	if move != nil {
		all = slices.Concat(all, move.LockedAccounts, move.SharedAccounts)
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}

// withControlAccounts 在写者账号之外加上维护、注册、schema 账号。这些不锁,只核存在和授权。
func (m *mover) withControlAccounts(writers []string) []string {
	all := slices.Concat(writers, m.o.controlPrincipals, []string{m.o.operator})
	slices.Sort(all)
	return slices.Compact(all)
}

func (m *mover) requireAccountsOnTarget(ctx context.Context, principals []string) error {
	var missing []string
	for _, principal := range principals {
		ok, err := m.targetAdmin.AccountExists(ctx, principal)
		if err != nil {
			return fmt.Errorf("check accounts on the target: %w", err)
		}
		if !ok {
			missing = append(missing, principal)
		}
	}
	if len(missing) != 0 {
		return refusedf("these accounts do not exist on the target cluster, create them with the same password first: %s", strings.Join(missing, ", "))
	}
	return nil
}

// partitionWriterAccounts 把写者账号分成能锁的和不能锁的。ACCOUNT LOCK 按账号生效,不按库:
// 一个账号还在写目标集群上别的库时(go/db、data_service 的一个 boot 写全部存档库),锁了就把那些库一起断了。这种账号默认拒绝,
// 运维确认之后(-allow-shared-writer-accounts)不锁,靠翻转之前查连接挡。
func (m *mover) partitionWriterAccounts(ctx context.Context, writers []string) (locked, shared []string, err error) {
	var details []string
	for _, writer := range writers {
		grants, err := m.targetAdmin.AccountGrants(ctx, writer)
		if err != nil {
			return nil, nil, fmt.Errorf("read the grants of %s on the target: %w", writer, err)
		}
		others := otherSchemasOf(grants, m.o.db)
		if len(others) == 0 {
			locked = append(locked, writer)
			continue
		}
		shared = append(shared, writer)
		details = append(details, writer+" (also on "+strings.Join(others, ", ")+")")
	}
	if len(shared) != 0 && !m.o.allowSharedAccounts {
		return nil, nil, refusedf("these writer accounts also have grants on other databases of the target cluster, ACCOUNT LOCK would cut those off: %s. Pass -allow-shared-writer-accounts to leave them unlocked, see docs/ops/db-move-runbook.md §4", strings.Join(details, "; "))
	}
	return locked, shared, nil
}

func (m *mover) setTargetAccounts(ctx context.Context, accounts []string, locked bool) error {
	for _, account := range accounts {
		if err := m.targetAdmin.SetAccountLocked(ctx, account, locked); err != nil {
			return fmt.Errorf("change the lock of account %s on the target: %w", account, err)
		}
	}
	return nil
}

// prepare 写 state=MOVING,在目标上锁写者账号、建空库。复制由运维按手册起。
//
// 先写映射再动目标:崩在中间时重跑走 resumed 分支,锁账号和 CREATE DATABASE IF NOT EXISTS 都幂等。
// 锁账号排在建库前面:库一出现,闸门表就会复制过来(writable=1),那之前账号就要锁上。
func (m *mover) prepare(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	p := rec.Placement
	resumed := p.State == dbplacement.StateMoving && p.Move != nil && p.Move.RunID == m.o.runID && p.Move.TargetCluster == m.o.targetCluster
	source := m.sourceCluster(rec)
	var earlier *dbplacement.Move
	if resumed {
		earlier = p.Move
		// 后面要在目标上锁账号。连接串指错就锁到别的集群上了;etcd 回档过的话目标已经在收写入,锁账号就把它断了。
		if err := m.checkEndpoints(p.Move.SourceCluster, p.Move.TargetCluster); err != nil {
			return err
		}
		if err := m.checkNoFlipMarker(ctx, "Fix the connection and re-run"); err != nil {
			return err
		}
	} else if err := m.checkNewMove(ctx, rec); err != nil {
		return err
	}
	writers, err := m.writerAccounts(ctx, earlier)
	if err != nil {
		return err
	}
	if err := m.requireAccountsOnTarget(ctx, writers); err != nil {
		return err
	}
	locked, shared, err := m.partitionWriterAccounts(ctx, writers)
	if err != nil {
		return err
	}
	if !resumed {
		next := dbplacement.Placement{Cluster: source, Version: p.Version, State: dbplacement.StateMoving, Move: &dbplacement.Move{
			RunID: m.o.runID, SourceCluster: source, TargetCluster: m.o.targetCluster, PreparedMs: m.nowMs(), LockedAccounts: locked, SharedAccounts: shared,
		}}
		if err := m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next); err != nil {
			return fmt.Errorf("mark MOVING (re-run prepare, it reads the mapping again): %w", err)
		}
	} else if !slices.Equal(p.Move.LockedAccounts, locked) || !slices.Equal(p.Move.SharedAccounts, shared) {
		next := clonePlacement(p)
		next.Move.LockedAccounts, next.Move.SharedAccounts = locked, shared
		if err := m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next); err != nil {
			return fmt.Errorf("record the writer accounts (re-run prepare, it reads the mapping again): %w", err)
		}
	}
	if err := m.setTargetAccounts(ctx, locked, true); err != nil {
		return fmt.Errorf("%w (re-run prepare after fixing it)", err)
	}
	charset, collation, err := m.source.SchemaCharset(ctx)
	if err != nil {
		return fmt.Errorf("read the source charset: %w", err)
	}
	if err := m.targetAdmin.CreateSchema(ctx, m.o.db, charset, collation); err != nil {
		return fmt.Errorf("create the empty database on the target (re-run prepare after fixing it): %w", err)
	}
	m.logf("prepare", "ok: %s is MOVING from cluster %s to cluster %s, %d writer account(s) locked on the target, the empty target database exists. Start full + incremental replication as in docs/ops/db-move-runbook.md, then run verify", m.o.db, source, m.o.targetCluster, len(locked))
	return nil
}

// verify 在复制运行中用一致快照比校验和,成功时把位置的时刻和复制流的身份记进映射值。cutover 要求它足够新,而且复制流没换过。
func (m *mover) verify(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if err := m.requireMoving(rec.Placement); err != nil {
		return err
	}
	if err := m.checkEndpoints(rec.Placement.Move.SourceCluster, rec.Placement.Move.TargetCluster); err != nil {
		return err
	}
	// 比的是那个位置之前的数据,耗时不算新鲜度。TiDB 档取 primary_ts 的物理时间:syncpoint 可能比工具开始的时刻旧得多。
	startedMs := m.nowMs()
	point, err := m.repl.VerifyPoint(ctx)
	if err != nil {
		return err
	}
	verifiedMs := startedMs
	if point.AtMs != 0 {
		verifiedMs = point.AtMs
	}
	m.logf("verify", "comparing at %s", point.Detail)
	compareStarted := m.now()
	mismatches, err := m.compare(ctx, "verify", point.Source, point.Target)
	if err != nil {
		return err
	}
	compareTook := max(m.now().Sub(compareStarted), 0)
	if err := m.repl.RecheckVerifyPoint(ctx, point); err != nil {
		return err
	}
	if len(mismatches) != 0 {
		return fmt.Errorf("%w: %d table(s) differ: %s", errMismatch, len(mismatches), strings.Join(mismatches, "; "))
	}
	// 校验和要算很久,这段时间复制可能被重建、过滤可能被改。流变了,比过的数据和之后复制过来的数据之间可能有缺口。
	feed, err := m.repl.Identity(ctx)
	if err != nil {
		return fmt.Errorf("read the replication stream after comparing: %w", err)
	}
	if feed != point.Feed {
		return refusedf("the replication stream or its config changed during verify (%+v, now %+v): run verify again", point.Feed, feed)
	}
	cur, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if err := m.requireMoving(cur.Placement); err != nil {
		return err
	}
	next := clonePlacement(cur.Placement)
	next.Move.VerifiedMs = verifiedMs
	next.Move.VerifiedFeed = &feed
	next.Move.VerifiedPrimaryTs = point.PrimaryTs
	next.Move.VerifyTookMs = uint64(compareTook.Milliseconds())
	if err := m.store.CompareAndPut(ctx, m.o.db, cur.ModRevision, next); err != nil {
		return fmt.Errorf("record the verify result (re-run verify): %w", err)
	}
	// 复制流的身份也写进本地日志:映射要手工写回时(etcd 从备份恢复),cleanup 靠 move.verified_feed 核这条流撤掉了没有。
	m.logf("verify", "ok: every table matches, the comparison took %s, replication stream %+v. cutover accepts this result for %s and compares once more inside the freeze window",
		compareTook.Round(time.Millisecond), feed, m.o.verifyMaxAge)
	return nil
}

// compare 打开两侧的视图并比较。视图连的是只读连接,不进切换窗口。
func (m *mover) compare(ctx context.Context, step, sourceSnapshot, targetSnapshot string) ([]string, error) {
	source, err := m.openView(ctx, roleSource, sourceSnapshot)
	if err != nil {
		return nil, fmt.Errorf("open the source for checksums: %w", err)
	}
	defer source.Close()
	target, err := m.openView(ctx, roleTarget, targetSnapshot)
	if err != nil {
		return nil, fmt.Errorf("open the target for checksums: %w", err)
	}
	defer target.Close()
	return compareSides(ctx, source, target, func(format string, args ...any) { m.logf(step, format, args...) })
}

// compareSides 比业务表的校验和,闸门表单独比(compareGateRows)。
// verify 也这样比:窗口崩在打开目标闸门之后,目标的闸门表正好多一次 release,这不是数据差异。
func compareSides(ctx context.Context, source, target checksumView, logf func(string, ...any)) ([]string, error) {
	mismatches, err := compareViewsExcept(ctx, source, target, auditSeparateTables, logf)
	if err != nil {
		return nil, err
	}
	sourceGate, err := source.GateRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the source gate: %w", err)
	}
	targetGate, err := target.GateRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the target gate: %w", err)
	}
	return append(mismatches, compareGateRows(sourceGate, targetGate)...), nil
}

// checkNoFlipMarker 读目标上的翻转标记。映射只在 etcd 里,etcd 回档之后它说没翻转不算数:标记在就说明目标已经在收写入。
// 这时不管是 rollback,还是重跑 prepare、cutover,都不能往下走:解冻源库两边各写一半,锁账号会把目标上的写者断掉。
func (m *mover) checkNoFlipMarker(ctx context.Context, unreadableHint string) error {
	flipped, err := m.targetAdmin.HasFlipMarker(ctx, m.o.db, flipMarkerNonce(m.o.runID))
	if err != nil {
		return refusedf("cannot read the flip marker on the target, nothing was changed: %v. %s", err, unreadableHint)
	}
	if flipped {
		return refusedf("the target has the flip marker of run %s, but the mapping says it never flipped: etcd was probably restored from a backup. The target is taking writes, do not unfreeze the source. Put the mapping back as in docs/ops/db-move-runbook.md §6", m.o.runID)
	}
	return nil
}

// checkTargetNotTakingWrites 在解冻源库之前核目标没有在收写入。目标闸门比冻结的源库新,只可能是这个 run 的窗口打开过;这时还有写者连着,
// 翻转可能已经生效过:翻转落地之后 etcd 回档把映射带了回去,或者翻转落地了但标记还没写。解冻的话,留在目标上的进程和按旧映射连源库的进程各写一边。
// 比 epoch 不比 writable:复制期间目标的闸门表是源库的副本,本来就可写。
func (m *mover) checkTargetNotTakingWrites(ctx context.Context, move *dbplacement.Move) error {
	source, err := m.source.CurrentMeta(ctx)
	if err != nil {
		return fmt.Errorf("read the source gate: %w", err)
	}
	target, err := m.targetAdmin.GateState(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the target gate: %w", err)
	}
	frozenByUs := source.Writable == 0 && bytes.Equal(source.FreezeRunId, m.o.runIDBytes)
	if !frozenByUs || !target.Found || !target.Writable || target.WriterEpoch <= source.WriterEpoch {
		return nil
	}
	return m.requireNoWritersOnTarget(ctx, move)
}

// cutover 是切换窗口。翻转之后源库保持冻结,旧进程写不进去。
func (m *mover) cutover(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping, nothing was changed: %w", err)
	}
	if m.flippedByThisRun(rec.Placement) {
		// 并发跑的 rollback、prepare 可能在翻转之后把账号又锁上了:映射已经指向目标,不解锁的话新连接全部失败,还回不去。
		if err := m.setTargetAccounts(ctx, rec.Placement.Move.LockedAccounts, false); err != nil {
			return fmt.Errorf("%s is flipped, but the writer accounts on the target could not be unlocked: %w. Re-run cutover", m.o.db, err)
		}
		// 翻转和写标记之间崩过的话,这里补上。
		if err := m.writeFlipMarker(ctx, rec.Placement); err != nil {
			return err
		}
		m.logf("cutover", "already done: %s is on cluster %s, version %d. The source stays frozen", m.o.db, rec.Placement.Cluster, rec.Placement.Version)
		return nil
	}
	if err := m.requireMoving(rec.Placement); err != nil {
		return err
	}
	if err := m.checkEndpoints(rec.Placement.Move.SourceCluster, rec.Placement.Move.TargetCluster); err != nil {
		return err
	}
	if err := m.checkNoFlipMarker(ctx, "Fix the connection and re-run"); err != nil {
		return err
	}
	if _, ok := m.o.config.SqlClusters[rec.Placement.Move.TargetCluster]; !ok {
		return refusedf("target cluster %q is not in SqlClusters of %s", rec.Placement.Move.TargetCluster, dbplacement.EnvConfig)
	}
	verifiedMs := rec.Placement.Move.VerifiedMs
	if verifiedMs == 0 {
		return refusedf("no successful verify for run %s: run verify first", m.o.runID)
	}
	if age := m.now().Sub(time.UnixMilli(int64(verifiedMs))); age > m.o.verifyMaxAge {
		return refusedf("the last verify is %s old, older than -verify-max-age=%s: run verify again", age.Round(time.Second), m.o.verifyMaxAge)
	}
	if m.o.skipFinalCompare {
		m.log.warn("cutover", skipFinalCompareWarning)
	} else if err := m.checkFinalCompareFits(rec.Placement.Move); err != nil {
		return err
	}
	preflight, cancel := context.WithTimeout(ctx, m.o.preflight)
	err = m.cutoverPreflight(preflight, rec.Placement.Move)
	cancel()
	if err != nil {
		return err
	}
	// 窗口从这里开始。超预算的那一步拿到 ctx 过期,走 abortWindow。
	window, cancel := context.WithTimeout(ctx, m.o.budget)
	defer cancel()
	started := m.now()
	if err := m.runWindow(window, rec); err != nil {
		return err
	}
	windowTook := m.now().Sub(started).Round(time.Millisecond)
	flipped := clonePlacement(rec.Placement)
	flipped.Version++
	// 翻转已经落地,窗口的预算不再管这一步。
	marker, cancelMarker := context.WithTimeout(context.Background(), m.o.rollbackBudget)
	defer cancelMarker()
	if err := m.writeFlipMarker(marker, flipped); err != nil {
		return err
	}
	m.logf("cutover", "ok: %s flipped to cluster %s, version %d. The window took %s. The source stays frozen. Next: stop the replication, run audit",
		m.o.db, rec.Placement.Move.TargetCluster, flipped.Version, windowTook)
	return nil
}

// flipMarkerNonce 是翻转标记的主键。按 run id 算,重跑写的是同一行。
func flipMarkerNonce(runID string) []byte {
	sum := sha256.Sum256([]byte("dbmove-flip-marker/" + runID))
	return sum[:16]
}

// writeFlipMarker 在目标库写一条翻转标记。映射只在 etcd 里,etcd 回档之后 rollback 读到的是翻转之前的状态,解冻源库就分叉了。
// 标记和目标的数据在一起,rollback 先读它,读到就拒绝。写不成回错误(退出码 2),重跑 cutover 补上:翻转已经做完,这一步不影响翻转。
func (m *mover) writeFlipMarker(ctx context.Context, placement dbplacement.Placement) error {
	nonce := flipMarkerNonce(m.o.runID)
	seen, err := m.target.HasSentinel(ctx, nonce)
	if err == nil && !seen {
		err = m.target.WriteSentinel(ctx, nonce, m.o.runIDBytes, placement.Version)
	}
	if err != nil {
		return fmt.Errorf("%s is flipped, but the flip marker on the target could not be written, so rollback would not notice an etcd restore. Re-run cutover to write it: %w", m.o.db, err)
	}
	return nil
}

// skipFinalCompareWarning 是 -skip-final-compare 的告警,打到标准错误输出并写进本地日志。
const skipFinalCompareWarning = "-skip-final-compare is set: the frozen source and the target are NOT compared before the flip. " +
	"Writes that replication skipped after the last verify stay unnoticed until audit. Run audit right after the flip and keep the source until it passes"

// checkFinalCompareFits 在冻结之前估窗口里那次静止比对装不装得下。它和 verify 做的是同一件事(全量比一遍),所以拿 verify 的耗时估:
// 明知比不完还去冻结,业务白停一个 -budget。
func (m *mover) checkFinalCompareFits(move *dbplacement.Move) error {
	if took := time.Duration(move.VerifyTookMs) * time.Millisecond; took >= m.o.budget {
		return refusedf("the last verify needed %s to compare the tables and the freeze window compares them once more, which -budget=%s cannot hold. Nothing was changed: pass a larger -budget (the source stays frozen that long), see docs/ops/db-move-runbook.md §4",
			took.Round(time.Second), m.o.budget)
	}
	return nil
}

// feedChange 说明当前的复制流和 verify 记下的差在哪,相同时回空串。只有配置摘要不同是流没换、配置被改了,单独说清楚。
func feedChange(verified *dbplacement.FeedIdentity, now dbplacement.FeedIdentity) string {
	if verified == nil {
		return fmt.Sprintf("the mapping has no verified replication stream (now %+v)", now)
	}
	if *verified == now {
		return ""
	}
	sameStream := *verified
	sameStream.ConfigDigest = now.ConfigDigest
	if sameStream == now {
		return fmt.Sprintf("the config of the replication stream changed since the last verify (digest %s, now %s): its filter or skip rules were edited", verified.ConfigDigest, now.ConfigDigest)
	}
	return fmt.Sprintf("the replication stream changed since the last verify (%+v, now %+v)", *verified, now)
}

// checkFeedUnchanged 核复制流还是 verify 时那一条,配置也没动过。流断过之后用较新的位置重建,或者过滤被改过,
// 延迟回到 0、校验记录还在,中间那段写却没复制过去。
func (m *mover) checkFeedUnchanged(ctx context.Context, move *dbplacement.Move) error {
	feed, err := m.repl.Identity(ctx)
	if err != nil {
		return refusedf("replication is not ready, nothing was changed: %v", err)
	}
	if change := feedChange(move.VerifiedFeed, feed); change != "" {
		return refusedf("%s: writes in between may be missing on the target. Nothing was changed: run verify again", change)
	}
	return nil
}

// checkAccountsBeforeFlip 重查账号和授权:plan 之后可能滚动重启过,新 Pod 用名册里下一个主体。
// 翻转之后才发现账号缺了,这个库全部连接失败,而且回不去,所以冻结之前核完。
func (m *mover) checkAccountsBeforeFlip(ctx context.Context, move *dbplacement.Move) error {
	writers, err := m.writerAccounts(ctx, move)
	if err != nil {
		return err
	}
	var notPrepared []string
	for _, writer := range writers {
		if !slices.Contains(move.LockedAccounts, writer) && !slices.Contains(move.SharedAccounts, writer) {
			notPrepared = append(notPrepared, writer)
		}
	}
	if len(notPrepared) != 0 {
		return refusedf("these writer accounts appeared after prepare and are not locked on the target: %s. Nothing was changed: re-run prepare with the same run id", strings.Join(notPrepared, ", "))
	}
	all := m.withControlAccounts(writers)
	if err := m.requireAccountsOnTarget(ctx, all); err != nil {
		return err
	}
	var problems []string
	for _, principal := range all {
		source, err := m.sourceAdmin.AccountGrants(ctx, principal)
		if err != nil {
			return refusedf("cannot read the grants of %s on the source, nothing was changed: %v", principal, err)
		}
		target, err := m.targetAdmin.AccountGrants(ctx, principal)
		if err != nil {
			return refusedf("cannot read the grants of %s on the target, nothing was changed: %v", principal, err)
		}
		if missing := missingGrants(source, target, m.o.db); len(missing) != 0 {
			problems = append(problems, principal+": "+strings.Join(missing, ", "))
		}
	}
	if len(problems) != 0 {
		return refusedf("the target is missing grants the source has, these accounts would fail after the flip and there is no way back. Nothing was changed: %s", strings.Join(problems, "; "))
	}
	// 共享账号不在窗口的解锁名单里。它其实锁着的话(上一次搬迁回滚留下的,或者先被锁、后来才补了别的库的授权),翻转之后这个库连不上,也回不去。
	var stillLocked []string
	for _, account := range move.SharedAccounts {
		locked, err := m.targetAdmin.AccountLocked(ctx, account)
		if err != nil {
			return refusedf("cannot read the lock state of %s on the target, nothing was changed: %v", account, err)
		}
		if locked {
			stillLocked = append(stillLocked, account)
		}
	}
	if len(stillLocked) != 0 {
		return refusedf("these shared writer accounts are locked on the target and the window does not unlock them, this database would be unreachable after the flip. Nothing was changed: unlock them by hand with ALTER USER ... ACCOUNT UNLOCK, see docs/ops/db-move-runbook.md §6: %s", strings.Join(stillLocked, ", "))
	}
	return nil
}

// checkProcessesKnowCluster 核列出的进程都认识目标集群。配置改了但 Pod 用的还是旧 Secret 时,翻转之后这个库读写全停,还回不去。
func (m *mover) checkProcessesKnowCluster(ctx context.Context, cluster string) error {
	var problems []string
	for _, target := range m.o.metricsTargets {
		known, err := m.knownCluster(ctx, target, cluster)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: cannot read metrics: %v", target, err))
		case !known:
			problems = append(problems, target+": does not report db_placement_known_cluster for it")
		}
	}
	if len(problems) != 0 {
		return refusedf("these processes do not know cluster %s, nothing was changed. Add it to their SqlClusters and restart them: %s", cluster, strings.Join(problems, "; "))
	}
	return nil
}

// cutoverPreflight 在冻结之前做完能提前做的检查:一条不过就什么都没变。
func (m *mover) cutoverPreflight(ctx context.Context, move *dbplacement.Move) error {
	meta, err := m.source.CurrentMeta(ctx)
	if err != nil {
		return fmt.Errorf("read the source gate, nothing was changed: %w", err)
	}
	frozenByUs := meta.Writable == 0 && bytes.Equal(meta.FreezeRunId, m.o.runIDBytes)
	if !frozenByUs && (meta.Writable != 1 || len(meta.FreezeRunId) != 0) {
		return refusedf("the source gate is frozen by another run (%x): finish that first", meta.FreezeRunId)
	}
	if err := m.source.ProbeControl(ctx); err != nil {
		return refusedf("the maintenance account cannot control the source gate, nothing was changed: %v", err)
	}
	if err := m.target.ProbeControl(ctx); err != nil {
		return refusedf("the maintenance account cannot control the target gate, nothing was changed: %v", err)
	}
	if err := m.targetAdmin.CheckSeesAllConnections(ctx); err != nil {
		return refusedf("the target admin account is not enough, nothing was changed: %v", err)
	}
	if err := m.checkFeedUnchanged(ctx, move); err != nil {
		return err
	}
	if err := m.repl.CheckLag(ctx, m.o.maxLag); err != nil {
		return refusedf("replication is not ready, nothing was changed: %v", err)
	}
	if err := m.checkAccountsBeforeFlip(ctx, move); err != nil {
		return err
	}
	return m.checkProcessesKnowCluster(ctx, move.TargetCluster)
}

func (m *mover) runWindow(ctx context.Context, rec dbplacement.Record) error {
	// 记过目标位置,说明上一次窗口走到过翻转附近,它发出的翻转可能还没有结局。这次即使还没发翻转,etcd 不通时也不能解冻。
	earlierFlipPossible := rec.Placement.Move.TargetPoint != ""
	frozen, err := m.freezeSource(ctx)
	if err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("freeze the source: %w", err))
	}
	m.logf("cutover", "source frozen: writer_epoch=%d", frozen.WriterEpoch)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, err)
	}
	if err := m.source.WriteSentinel(ctx, nonce, m.o.runIDBytes, frozen.WriterEpoch); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("write the sentinel: %w", err))
	}
	mark, err := m.repl.Mark(ctx)
	if err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("read the freeze point: %w", err))
	}
	if err := m.waitCaughtUp(ctx, mark, nonce); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, err)
	}
	if err := m.recheckFeedInWindow(ctx, rec.Placement.Move); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, err)
	}
	m.logf("cutover", "replication caught up, the target has the sentinel")
	targetMeta, err := m.target.CurrentMeta(ctx)
	if err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("read the target gate: %w", err))
	}
	if !targetGateReady(targetMeta, frozen, m.o.runIDBytes) {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("the target gate (epoch %d, writable %d) does not match the frozen source (epoch %d): the gate tables did not replicate", targetMeta.WriterEpoch, targetMeta.Writable, frozen.WriterEpoch))
	}
	// 排在记目标位置前面:比不过时还没解锁过账号、没开过目标闸门,回滚不用碰目标。
	if err := m.finalCompare(ctx); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, err)
	}
	recorded, err := m.recordTargetPoint(ctx, rec, targetMeta)
	if err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("record the target point: %w", err))
	}
	rec = recorded
	m.logf("cutover", "target point for audit: %s", rec.Placement.Move.TargetPoint)
	// 账号锁不踢已有连接,所以先看有没有写者已经连着。解锁排在打开闸门前面:解锁之后、闸门打开之前,连上来的写者还是过不了闸。
	if err := m.requireNoWritersOnTarget(ctx, rec.Placement.Move); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, err)
	}
	if err := m.setTargetAccounts(ctx, rec.Placement.Move.LockedAccounts, false); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("unlock the writer accounts: %w", err))
	}
	if _, err := m.target.ReleaseWriters(ctx, m.o.runIDBytes); err != nil {
		return m.abortWindow(rec, earlierFlipPossible, fmt.Errorf("open the target gate: %w", err))
	}
	next := clonePlacement(rec.Placement)
	next.Cluster = next.Move.TargetCluster
	next.Version++
	next.State = dbplacement.StateActive
	next.Move.FlippedMs = m.nowMs()
	// 和翻转同一次写:cleanup 按 FeedEndpoint 找这条复制流,跳过静止比对这件事也留在映射里。
	next.Move.FeedEndpoint = m.o.feedEndpoint
	next.Move.FinalCompareSkipped = m.o.skipFinalCompare
	// 提交点。
	err = m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next)
	if err == nil {
		return nil
	}
	// 冲突时这次写确定没落地,但映射被人改过,可能是同一个 run 的另一个进程翻转了;其余错误这次写本身结局未知。两种都由 abortWindow 重读裁决。
	return m.abortWindow(rec, true, fmt.Errorf("flip the mapping: %w", err))
}

// recheckFeedInWindow 冻结之后再核一次复制流。预检之后流可能被删了重建、过滤可能被改:新流的 checkpoint 一样能越过冻结点,目标也能读到哨兵,中间那段写却不在目标上。
// 流或它的配置变了按复制断开处理,回滚时清掉 verify 记录。
func (m *mover) recheckFeedInWindow(ctx context.Context, move *dbplacement.Move) error {
	feed, err := m.repl.Identity(ctx)
	if errors.Is(err, errReplicationBroken) {
		return err
	}
	if err != nil {
		return fmt.Errorf("read the replication stream: %v", err)
	}
	if change := feedChange(move.VerifiedFeed, feed); change != "" {
		return fmt.Errorf("%w: %s", errReplicationBroken, change)
	}
	return nil
}

// finalCompare 在两边都静止的时候把全量再比一次:源库冻结着,复制追平了,目标的闸门还没开。
// verify 之后复制被跳过一段(changefeed 用 --overwrite-checkpoint-ts 恢复、往目标注入空事务跳过 GTID),位置和哨兵照样对得上,
// 只有比数据才看得出目标少了写入。两边都不动,读当前数据就行:不用 syncpoint,MySQL 档也不用副本(源库冻结着,CHECKSUM TABLE 的读锁挡不到写入)。
// 回的错误不带 errRefused、errMismatch:窗口已经冻结过,回滚之后的退出码是 2,和别的回滚原因一样。
func (m *mover) finalCompare(ctx context.Context) error {
	if m.o.skipFinalCompare {
		return nil
	}
	started := m.now()
	mismatches, err := m.compare(ctx, "cutover", "", "")
	switch {
	case err != nil && ctx.Err() != nil:
		return fmt.Errorf("comparing the frozen source with the target was cut short (-budget=%s): %v. If it ran out of time, pass a larger -budget", m.o.budget, err)
	case err != nil:
		return fmt.Errorf("compare the frozen source with the target: %v", err)
	case len(mismatches) != 0:
		return fmt.Errorf("%w: %d table(s) differ: %s. Writes that replication skipped after the last verify, or writes made on the target, would look like this", errTargetDiverged, len(mismatches), strings.Join(mismatches, "; "))
	}
	m.logf("cutover", "the frozen source and the target match, compared in %s", max(m.now().Sub(started), 0).Round(time.Millisecond))
	return nil
}

// recordTargetPoint 在打开目标闸门之前把目标位置写进映射,返回写完之后的记录。
// 目标闸门已经开着,说明上一次窗口崩在打开之后、翻转之前:复用那一次记下的位置,这时再读,位置就包含了那次打开,audit 会报不一致。
func (m *mover) recordTargetPoint(ctx context.Context, rec dbplacement.Record, target *databasepb.RouteMeta) (dbplacement.Record, error) {
	if target.Writable == 1 {
		if rec.Placement.Move.TargetPoint == "" {
			return rec, errors.New("the target gate is open but the mapping has no target point")
		}
		return rec, nil
	}
	point, err := m.repl.TargetPoint(ctx)
	if err != nil {
		return rec, err
	}
	next := clonePlacement(rec.Placement)
	next.Move.TargetPoint = point
	if err := m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next); err != nil {
		return rec, err
	}
	return m.store.Get(ctx, m.o.db)
}

// requireNoWritersOnTarget 核翻转之前没有写者连着目标。锁定的账号连着就是有进程提前配了新地址;没锁的账号还在写目标上别的库,只看连到这个库的。
func (m *mover) requireNoWritersOnTarget(ctx context.Context, move *dbplacement.Move) error {
	connections, err := m.targetAdmin.Connections(ctx)
	if err != nil {
		return fmt.Errorf("list the connections on the target: %w", err)
	}
	if stray := strayConnections(connections, move, m.o.db); len(stray) != 0 {
		return fmt.Errorf("%w, a process has the new address too early: %s", errStrayWriter, strings.Join(stray, ", "))
	}
	return nil
}

func strayConnections(connections []accountConnection, move *dbplacement.Move, db string) []string {
	var stray []string
	for _, connection := range connections {
		locked := slices.ContainsFunc(move.LockedAccounts, func(account string) bool { return accountUser(account) == connection.User })
		shared := slices.ContainsFunc(move.SharedAccounts, func(account string) bool { return accountUser(account) == connection.User })
		if locked || (shared && connection.Database == db) {
			stray = append(stray, fmt.Sprintf("%s x%d on database %q", connection.User, connection.Count, connection.Database))
		}
	}
	slices.Sort(stray)
	return stray
}

// accountUser 取 user@host 的 user:连接列表里只有用户名,host 列是客户端地址。
func accountUser(principal string) string {
	if at := strings.LastIndexByte(principal, '@'); at >= 0 {
		return principal[:at]
	}
	return principal
}

// freezeSource 在预算内重试。锁等待、连接抖动、写者注册撞车都是暂时的;提交结局未知时重放同一个 run-id 是幂等的。
func (m *mover) freezeSource(ctx context.Context) (*databasepb.RouteMeta, error) {
	return retryGate(ctx, m.sleep, func(ctx context.Context) (*databasepb.RouteMeta, error) {
		return m.source.FreezeWriters(ctx, m.o.runIDBytes)
	})
}

func (m *mover) releaseSource(ctx context.Context) (*databasepb.RouteMeta, error) {
	return retryGate(ctx, m.sleep, func(ctx context.Context) (*databasepb.RouteMeta, error) {
		return m.source.ReleaseWriters(ctx, m.o.runIDBytes)
	})
}

const gateRetryInterval = 200 * time.Millisecond

func retryGate(ctx context.Context, sleep func(context.Context, time.Duration) error, call func(context.Context) (*databasepb.RouteMeta, error)) (*databasepb.RouteMeta, error) {
	for {
		meta, err := call(ctx)
		if err == nil {
			return meta, nil
		}
		if sleepErr := sleep(ctx, gateRetryInterval); sleepErr != nil {
			return nil, fmt.Errorf("%w (last error: %v)", sleepErr, err)
		}
	}
}

func (m *mover) waitCaughtUp(ctx context.Context, mark string, nonce []byte) error {
	var last error
	for {
		caught, err := m.repl.CaughtUp(ctx, mark)
		switch {
		case errors.Is(err, errReplicationBroken):
			return err
		case err != nil:
			last = err
		case caught:
			seen, err := m.target.HasSentinel(ctx, nonce)
			if err != nil {
				last = err
			} else if seen {
				return nil
			} else {
				last = errors.New("caught up, but the target does not have the sentinel yet")
			}
		default:
			last = errors.New("the target has not reached the freeze point")
		}
		if err := m.sleep(ctx, m.o.pollInterval); err != nil {
			return fmt.Errorf("replication did not catch up within the budget: %w (last: %v)", err, last)
		}
	}
}

// targetGateReady:目标的闸门表是复制过来的冻结点,或者是上一次重跑已经打开过的那一步(epoch 正好多 1,没有写者连过)。
func targetGateReady(target, frozen *databasepb.RouteMeta, runID []byte) bool {
	if target.Generation != frozen.Generation || target.CatalogRevision != frozen.CatalogRevision {
		return false
	}
	if target.Writable == 0 {
		return target.WriterEpoch == frozen.WriterEpoch && bytes.Equal(target.FreezeRunId, runID)
	}
	return target.WriterEpoch == frozen.WriterEpoch+1 && len(target.FreezeRunId) == 0
}

type flipVerdict int

const (
	flipInvalidated flipVerdict = iota // 这个 run 不会再翻转了,解冻是安全的
	flipHappened                       // 映射已经翻到这个 run 的目标
	flipForeign                        // 映射被别的东西改了,不属于这个 run
	flipUnreachable                    // 读不到 etcd,不知道
)

// abortWindow 回滚切换窗口。顺序不能反:先让翻转失效,再解冻。
// 解冻之后翻转才落地的话,旧进程写源库、新进程写目标库,两边各丢一半。
func (m *mover) abortWindow(rec dbplacement.Record, mayHaveFlipped bool, cause error) error {
	m.logf("cutover", "aborting the window: %v", cause)
	// 作废和解冻各有一份预算,窗口的 ctx 可能已经过期:etcd 不通时作废耗满预算,解冻还要有时间。
	invalidate, cancelInvalidate := context.WithTimeout(context.Background(), m.o.rollbackBudget)
	verdict := m.invalidateFlip(invalidate, rec, cause)
	cancelInvalidate()
	switch verdict {
	case flipHappened:
		m.logf("cutover", "the mapping is already flipped by run %s: nothing to roll back", m.o.runID)
		return nil
	case flipForeign:
		return unknownf("the mapping of %s changed underneath this run, the source stays frozen: check it by hand, then run rollback or finish the other run (cause: %v)", m.o.db, cause)
	case flipUnreachable:
		if mayHaveFlipped {
			return unknownf("etcd did not answer, the flip may or may not have landed, so the source stays frozen. When etcd is back re-run cutover (it finishes if the flip landed) or rollback (cause: %v)", cause)
		}
		// 这个进程没发过翻转,etcd 又不通,没有人翻转得了。解冻恢复服务。
		m.logf("cutover", "etcd did not answer, but this run never sent the flip: unfreezing the source")
	}
	// 记过目标位置,这一次或者崩掉的上一次窗口就可能解锁过账号、打开过目标闸门。
	// 解冻之后源库的闸门表复制过去,目标又是 writable=1,账号要先锁回去。
	// 没记过位置就没有解锁过,不碰目标:回滚常常就是因为目标连不上。
	var lockErr error
	if rec.Placement.Move.TargetPoint != "" {
		relock, cancelRelock := context.WithTimeout(context.Background(), m.o.rollbackBudget)
		if err := m.checkTargetNotTakingWrites(relock, rec.Placement.Move); err != nil {
			cancelRelock()
			return unknownf("the flip may have taken effect on the target (%v), so the source stays frozen. Check as in docs/ops/db-move-runbook.md §6 before re-running rollback (cause: %v)", err, cause)
		}
		lockErr = m.setTargetAccounts(relock, rec.Placement.Move.LockedAccounts, true)
		cancelRelock()
	}
	release, cancelRelease := context.WithTimeout(context.Background(), m.o.rollbackBudget)
	defer cancelRelease()
	if _, err := m.releaseSource(release); err != nil {
		return unknownf("the source is still frozen, could not release it: %v. Re-run rollback (cause: %v)", err, cause)
	}
	if lockErr != nil {
		return unknownf("the source is writable again, but the writer accounts on the target may still be unlocked: %v. Re-run rollback (cause: %v)", lockErr, cause)
	}
	m.logf("cutover", "rolled back: the source is writable again, %s stays MOVING, cutover can be re-run", m.o.db)
	return fmt.Errorf("cutover rolled back: %w", cause)
}

// invalidateFlip 写一次映射让窗口里可能还在路上的翻转 CAS 失效(它带的是旧 ModRevision)。写成功之后这个 run 就翻转不了了。
// 回滚的原因是复制断了、有写者提前连着目标,或者静止比对不一致时,顺手清掉 verify 记录:这段时间的写有没有丢、目标有没有被写过,只有重新校验才知道。
func (m *mover) invalidateFlip(ctx context.Context, rec dbplacement.Record, cause error) flipVerdict {
	for {
		cur, err := m.store.Get(ctx, m.o.db)
		if err == nil {
			p := cur.Placement
			switch {
			case m.flippedByThisRun(p):
				return flipHappened
			case p.Move != nil && p.Move.RunID == m.o.runID && p.Move.RolledBackMs != 0:
				return flipInvalidated // rollback 命令已经作废了这个 run
			case p.State != dbplacement.StateMoving || p.Move == nil || p.Move.RunID != m.o.runID || p.Cluster != rec.Placement.Cluster:
				return flipForeign
			}
			next := clonePlacement(p)
			next.Move.AbortedMs = m.nowMs()
			if errors.Is(cause, errReplicationBroken) || errors.Is(cause, errStrayWriter) || errors.Is(cause, errTargetDiverged) {
				next.Move.VerifiedMs, next.Move.VerifiedFeed, next.Move.VerifiedPrimaryTs, next.Move.VerifyTookMs = 0, nil, 0, 0
			}
			if err = m.store.CompareAndPut(ctx, m.o.db, cur.ModRevision, next); err == nil {
				return flipInvalidated
			}
			if errors.Is(err, dbplacement.ErrConflict) {
				continue
			}
		}
		m.logf("cutover", "etcd is not answering while aborting: %v", err)
		if m.sleep(ctx, gateRetryInterval) != nil {
			return flipUnreachable
		}
	}
}

// rollback 只在翻转之前可用:先作废映射,再解冻。翻转之后目标已经在收写入,只能反向再搬一次。
//
// 映射说没翻转还不够:etcd 可能回档了。先读目标上的翻转标记,读到就拒绝,解冻的话两边各写一半。
// 账号再锁一遍(崩在解锁之后的话还开着),解冻之后源库的闸门表复制过去,目标又是 writable=1。
// 目标连不上时什么都核不了,默认拒绝;确认它没收过写入之后加 -target-lost。
func (m *mover) rollback(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping, nothing was changed: %w", err)
	}
	move := rec.Placement.Move
	if move == nil || move.RunID != m.o.runID {
		return refusedf("%s has no move with run id %s", m.o.db, m.o.runID)
	}
	if move.FlippedMs != 0 || rec.Placement.Cluster == move.TargetCluster {
		return refusedf("run %s already flipped %s to cluster %s, the target is taking writes. To go back, move it in the other direction: cleanup the source, then plan and prepare a new run", m.o.runID, m.o.db, rec.Placement.Cluster)
	}
	// 连接串指错时解冻的不是真正的源库,却会报告成功。
	if err := m.checkEndpoints(move.SourceCluster, move.TargetCluster); err != nil {
		return err
	}
	if !m.o.targetLost {
		if err := m.checkNoFlipMarker(ctx, "Fix the connection, or pass -target-lost if you are sure the target never took writes"); err != nil {
			return err
		}
		if err := m.checkTargetNotTakingWrites(ctx, move); err != nil {
			return refusedf("cannot rule out that the flip took effect on the target, nothing was changed: %v. Check as in docs/ops/db-move-runbook.md §6", err)
		}
		if err := m.setTargetAccounts(ctx, move.LockedAccounts, true); err != nil {
			return fmt.Errorf("%w. Nothing else was changed: fix it and re-run rollback, or pass -target-lost if the target cannot be reached", err)
		}
	}
	if rec.Placement.State == dbplacement.StateMoving {
		next := clonePlacement(rec.Placement)
		next.State = dbplacement.StateActive
		next.Move.RolledBackMs = m.nowMs()
		if err := m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next); err != nil {
			return unknownf("could not mark the rollback (the source gate was not touched): %v. Re-run rollback. If it says the run already flipped, re-run cutover: the accounts locked just now have to be unlocked", err)
		}
	}
	if _, err := m.releaseSource(ctx); err != nil {
		return unknownf("the mapping is rolled back but the source may still be frozen: %v. Re-run rollback", err)
	}
	m.logf("rollback", "ok: %s is ACTIVE on cluster %s and writable. Stop the replication and drop the target database by hand (runbook §7)", m.o.db, rec.Placement.Cluster)
	return nil
}

// audit 翻转之后对冻结的源库和目标库比全量校验和,再核各进程上报的映射版本。结果不一致退出码 1。
func (m *mover) audit(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if !m.flippedByThisRun(rec.Placement) {
		return refusedf("run %s has not flipped %s: audit runs after cutover", m.o.runID, m.o.db)
	}
	move := rec.Placement.Move
	if err := m.checkEndpoints(move.SourceCluster, move.TargetCluster); err != nil {
		return err
	}
	var problems []string
	for _, target := range m.o.metricsTargets {
		version, found, err := m.scrape(ctx, target, m.o.db)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("process %s: cannot read metrics: %v", target, err))
		case !found:
			problems = append(problems, fmt.Sprintf("process %s: reports no db_placement_version for %s (no DbPlacement config, or it never connected)", target, m.o.db))
		case version != rec.Placement.Version:
			problems = append(problems, fmt.Sprintf("process %s: at version %d, want %d", target, version, rec.Placement.Version))
		default:
			m.logf("audit", "process %s: version %d", target, version)
		}
	}
	mismatches, err := m.compareAudit(ctx, move.TargetPoint)
	if err != nil {
		return err
	}
	problems = append(problems, mismatches...)
	if len(problems) != 0 {
		return fmt.Errorf("%w: %s", errMismatch, strings.Join(problems, "; "))
	}
	cur, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if !m.flippedByThisRun(cur.Placement) {
		return refusedf("the mapping of %s changed during the audit", m.o.db)
	}
	next := clonePlacement(cur.Placement)
	next.Move.AuditedMs = m.nowMs()
	if err := m.store.CompareAndPut(ctx, m.o.db, cur.ModRevision, next); err != nil {
		return fmt.Errorf("record the audit result (re-run audit): %w", err)
	}
	m.logf("audit", "ok: source and target match, every listed process is at version %d", rec.Placement.Version)
	return nil
}

// compareAudit:源库已冻结,读当前数据。目标已经在收写入,要读翻转之前的位置。
func (m *mover) compareAudit(ctx context.Context, targetPoint string) ([]string, error) {
	source, err := m.openView(ctx, roleSource, "")
	if err != nil {
		return nil, fmt.Errorf("open the source for checksums: %w", err)
	}
	defer source.Close()
	target, err := m.openView(ctx, roleTarget, targetPoint)
	if err != nil {
		return nil, fmt.Errorf("open the target for checksums: %w", err)
	}
	defer target.Close()
	if err := target.CheckPoint(ctx, targetPoint); err != nil {
		return nil, err
	}
	return compareSides(ctx, source, target, func(format string, args ...any) { m.logf("audit", format, args...) })
}

// requireFeedRemoved 在删源库之前核喂过目标的那条复制流已经不存在。DROP DATABASE 也是一条会被复制的语句:
// changefeed 只是暂停、通道只是 STOP REPLICA 的话,它一恢复就把这条 DROP 送到目标,把正在收写入的库整个删掉。
// 所以流只要还在就拒绝,不管它是什么状态;查不成也拒绝,不知道不等于没有。
func (m *mover) requireFeedRemoved(ctx context.Context, move *dbplacement.Move) error {
	if move.VerifiedFeed == nil {
		return refusedf("the mapping of %s does not record the replication stream that fed the target, so nothing can prove it is gone. Nothing was dropped: put move.verified_feed back from the step log of run %s, see docs/ops/db-move-runbook.md §6", m.o.db, m.o.runID)
	}
	// 换一个 TiCDC 去问,答「没有这个 changefeed」不说明它已经撤掉:只认 cutover 读过这条流的那个地址。
	if move.FeedEndpoint != m.o.feedEndpoint && !m.o.allowAddrMismatch {
		return refusedf("cutover read the replication stream through %q but -sync points at %q: another TiCDC not knowing the changefeed proves nothing. Nothing was dropped: pass the -sync that cutover used, or -allow-addr-mismatch when this is another address of the same TiCDC cluster", move.FeedEndpoint, m.o.feedEndpoint)
	}
	exists, detail, err := m.feeds.StillExists(ctx, *move.VerifiedFeed)
	switch {
	case errors.Is(err, errRefused):
		return err
	case err != nil:
		return refusedf("cannot tell whether the replication stream of this move is gone, nothing was dropped: %v", err)
	case exists:
		return refusedf("%s: DROP DATABASE on the source would replicate to cluster %s and drop the live database there. Nothing was dropped: remove the replication first (docs/ops/db-move-runbook.md §4 step 6), then re-run cleanup", detail, move.TargetCluster)
	}
	return nil
}

// cleanup 删源库。保留期内拒绝,必须显式 -confirm-drop。闸门不是这个 run 冻结的就不删:那不是一个已搬走的库。
// 复制流没撤掉也不删(requireFeedRemoved)。
func (m *mover) cleanup(ctx context.Context) error {
	rec, err := m.store.Get(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("read the mapping: %w", err)
	}
	if !m.flippedByThisRun(rec.Placement) {
		return refusedf("run %s has not flipped %s: cleanup only drops a source that was moved away", m.o.runID, m.o.db)
	}
	move := rec.Placement.Move
	if move.CleanedMs != 0 {
		m.logf("cleanup", "already done: the source on cluster %s was dropped", move.SourceCluster)
		return nil
	}
	if age := m.now().Sub(time.UnixMilli(int64(move.FlippedMs))); age < m.o.retention {
		return refusedf("the flip was %s ago, the retention is %s: the frozen source is the way back if the target turns out bad", age.Round(time.Minute), m.o.retention)
	}
	if move.AuditedMs == 0 && !m.o.acknowledgeNoAudit {
		return refusedf("run %s has no successful audit: run audit first, or pass -acknowledge-no-audit if it cannot be run any more", m.o.runID)
	}
	if !m.o.confirmDrop {
		return refusedf("this drops database %s on cluster %s: pass -confirm-drop", m.o.db, move.SourceCluster)
	}
	// 目标的连接串只有 MySQL 档给(通道配在目标上),给了就一起核:查错了集群,「没有这个通道」不算数。
	if err := m.checkEndpoints(move.SourceCluster, move.TargetCluster); err != nil {
		return err
	}
	exists, err := m.sourceAdmin.SchemaExists(ctx, m.o.db)
	if err != nil {
		return fmt.Errorf("check the source: %w", err)
	}
	if exists {
		frozen, err := m.sourceAdmin.GateFrozenBy(ctx, m.o.db, m.o.runIDBytes)
		if err != nil {
			return fmt.Errorf("read the source gate: %w", err)
		}
		if !frozen {
			return refusedf("the gate of %s on cluster %s is not frozen by run %s: it is not safe to drop, check the DSN", m.o.db, move.SourceCluster, m.o.runID)
		}
		// 紧挨着 DROP:核完到删库之间留给别人重新起复制的时间越短越好。
		if err := m.requireFeedRemoved(ctx, move); err != nil {
			return err
		}
		if err := m.sourceAdmin.DropSchema(ctx, m.o.db); err != nil {
			return fmt.Errorf("drop the source (re-run cleanup): %w", err)
		}
	}
	next := clonePlacement(rec.Placement)
	next.Move.CleanedMs = m.nowMs()
	if err := m.store.CompareAndPut(ctx, m.o.db, rec.ModRevision, next); err != nil {
		return fmt.Errorf("record the cleanup (the source is gone, re-run cleanup): %w", err)
	}
	m.logf("cleanup", "ok: database %s dropped from cluster %s", m.o.db, move.SourceCluster)
	return nil
}
