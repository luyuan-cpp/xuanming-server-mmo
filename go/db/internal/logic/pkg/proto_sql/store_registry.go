package proto_sql

import (
	"context"
	"database/sql"
	"db/internal/config"
	"db/internal/dbguard"
	"db/internal/metrics"
	"db/internal/playerstore"
	"errors"
	"fmt"
	"shared/authoritysql"
	"shared/placement"
	"shared/sqlpoolstats"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/luyuancpp/proto2mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/syncx"
	"google.golang.org/protobuf/proto"
)

// ───────────────────────── 落点库注册表(player-storage-placement.md §6.1)─────────────────────────
//
// 玩家主数据落在本地域的 K 个地域存档库 player_store_<id>_db 里,和 go/db 所在的区无关(docs/design/player-data-tidb-scaling-plan.md §7.2)。
// 选库只在 go/db 一处发生(internal/playerstore 经 data_service 的 GetPlayerRoute 选),本文件负责把「落点编号」变成「一个已打开、
// 已核对过的库」:
//   - go/db 没有「本区主库」(A17c):注册表建出来是空的,每个存档库第一次被指向时按需打开(同一个库并发只打开一次),之后常驻;
//   - 按需打开**从不建库、不跑 DDL**:只 Ping + 断言连上的库 + 只读 schema 闸,库不存在就报错延后,
//     绝不会因为一条畸形的落点记录在生产实例上建出一个新库。

// ErrStoreUnavailable 表示落点库暂时拿不到:不被放行、库不存在、连不上或 schema 闸未通过。
// 调用方(消费者)对写任务一律延后重试且不耗重试次数(§6.1):写进死信等于丢盘,而这类故障修好配置 / 库之后
// 任务就能落地。
var ErrStoreUnavailable = errors.New("placement store unavailable")

// AdmitStore 的放行依据,用于日志与 cmd/migrate 判断是否需要把库名并进交给迁移 runner 的有效白名单。
const (
	StoreAdmittedByAllowlist = "allowlist"
	StoreAdmittedByFamily    = "family"
)

const (
	// defaultStoreOpenTimeout 是一次按需打开(Ping + DATABASE() 断言 + 只读 schema 闸)的总预算。
	// 打开发生在 worker 的处理路径上,期间同一子分片的任务都在等,所以远小于启动期的
	// schemaDriftCheckTimeout(30s);健康实例上整套核对是毫秒级。
	defaultStoreOpenTimeout = 10 * time.Second

	// defaultStoreOpenRetryBackoff 是打开失败后的冷却期:期间对同一个库的请求直接返回上次的错误,不再拨号。
	// 没有冷却时,一个连不上的库会让每条指向它的任务都在 worker 上等满打开预算,同一子分片里与它
	// 无关的玩家一起被拖住;有了冷却,坏库上的任务秒级转进重试队列,别的玩家照常落库。
	defaultStoreOpenRetryBackoff = 10 * time.Second
)

// AdmitStore 判定本进程能否打开名为 name 的落点库(§6.1),返回放行依据。
//
// 可打开 = 名字在外部注入的白名单里,或 familiesAllowed 且名字属于存档库家族 player_store_{>=1000000}_db
// (zone 库已经退役,不属于任何家族)。cmd/migrate 的 -storage-id 与业务服务共用这一个判定,
// 保证「迁移能动的已有库」与「服务能写的库」是同一个集合。建库不走这里:建库仍只认白名单(CreateDatabase)。
func AdmitStore(allow dbguard.Allowlist, familiesAllowed bool, name string) (string, bool) {
	if allow.Contains(name) {
		return StoreAdmittedByAllowlist, true
	}
	if familiesAllowed {
		if _, ok := placement.StorageIDFromDBName(name); ok {
			return StoreAdmittedByFamily, true
		}
	}
	return "", false
}

// StoreOpener 按库名打开一个落点库。实现必须只连接、只读核对,绝不建库、不跑 DDL,
// 返回的 GameDB.SqlModel.DBName 必须等于 dbName。
type StoreOpener func(ctx context.Context, dbName string) (*GameDB, error)

// StoreRegistryOptions 是 NewStoreRegistry 的全部依赖。时间与打开动作都显式注入,测试替身走同一条路径。
type StoreRegistryOptions struct {
	// Allow 是启动期解析好的外部白名单;AllowFamilies 对应 Placement.AllowStoreFamilies。
	Allow         dbguard.Allowlist
	AllowFamilies bool
	// Open 真正打开一个库;生产为 OpenPlacementStore。
	Open StoreOpener
	// Now 为 nil 时用 time.Now;OpenTimeout / RetryBackoff 为 0 时用默认值。
	Now          func() time.Time
	OpenTimeout  time.Duration
	RetryBackoff time.Duration

	// 以下四个是运行期失联的探活与冷却(store_health.go)。为零时用默认值;Ping 为 nil 时 PingContext 本库连接池。
	ProbeTimeout time.Duration
	Cooldown     time.Duration
	Jitter       func(time.Duration) time.Duration
	Ping         func(ctx context.Context, store *GameDB) error
}

// StoreRegistry 按落点编号给出已打开的库。并发安全:所有 worker 子分片共用一个实例。
type StoreRegistry struct {
	allow         dbguard.Allowlist
	allowFamilies bool
	open          StoreOpener
	now           func() time.Time
	openTimeout   time.Duration
	retryBackoff  time.Duration

	// flight 保证同一个库同一时刻只有一次打开在跑,其余请求等它的结果。
	// 用 go-zero 自带的实现,不为此引入 golang.org/x/sync。
	flight syncx.SingleFlight

	probeTimeout time.Duration
	cooldown     time.Duration
	jitter       func(time.Duration) time.Duration
	ping         func(ctx context.Context, store *GameDB) error

	mu       sync.RWMutex
	stores   map[uint32]*GameDB
	failures map[uint32]storeOpenFailure
	// cooling 是已打开、但被探活确认不可达、正在冷却的库(store_health.go)。
	cooling map[uint32]storeCooldown
}

type storeOpenFailure struct {
	at  time.Time
	err error
}

// NewStoreRegistry 建一个空的注册表:没有预先打开的库,每个存档库第一次被指向时按需打开(A17c)。
func NewStoreRegistry(opts StoreRegistryOptions) (*StoreRegistry, error) {
	if opts.Open == nil {
		return nil, errors.New("placement store registry needs a store opener")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	openTimeout := opts.OpenTimeout
	if openTimeout <= 0 {
		openTimeout = defaultStoreOpenTimeout
	}
	retryBackoff := opts.RetryBackoff
	if retryBackoff <= 0 {
		retryBackoff = defaultStoreOpenRetryBackoff
	}
	probeTimeout := opts.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultStoreProbeTimeout
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = defaultStoreCooldown
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = defaultCooldownJitter
	}
	ping := opts.Ping
	if ping == nil {
		ping = pingStore
	}
	metrics.SetPlacementOpenStores(0)
	return &StoreRegistry{
		allow:         opts.Allow,
		allowFamilies: opts.AllowFamilies,
		open:          opts.Open,
		now:           now,
		openTimeout:   openTimeout,
		retryBackoff:  retryBackoff,
		probeTimeout:  probeTimeout,
		cooldown:      cooldown,
		jitter:        jitter,
		ping:          ping,
		flight:        syncx.NewSingleFlight(),
		stores:        map[uint32]*GameDB{},
		failures:      map[uint32]storeOpenFailure{},
		cooling:       map[uint32]storeCooldown{},
	}, nil
}

// NewStoreRegistryFromConfig 按进程配置建注册表。必须在 InitDB 成功之后、消费者启动之前调用。
func NewStoreRegistryFromConfig() (*StoreRegistry, error) {
	allow, err := AllowlistSpec().Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve database allow list: %w", err)
	}
	// 库名清单只许列地域存档库(A17c):zone 库已经退役,清单里还有它说明部署没跟上,拒启并点名,不留一个永远打不开的名字。
	for _, name := range allow.Names {
		if _, ok := placement.StorageIDFromDBName(name); !ok {
			return nil, fmt.Errorf("database allow list entry %q is not a player store database player_store_<id>_db; zone databases are retired "+
				"(player-data-tidb-scaling-plan.md §7.2 A17c), list only the player stores of this region in MMORPG_DB_ALLOWED_DATABASES", name)
		}
	}
	pc := config.AppConfig.Placement
	pool := StorePoolOptions{
		MaxOpenConn:     pc.StoreMaxOpenConn,
		MaxIdleConn:     pc.StoreMaxIdleConn,
		ConnMaxIdleTime: time.Duration(pc.StoreConnMaxIdleSeconds) * time.Second,
	}
	registry, err := NewStoreRegistry(StoreRegistryOptions{
		Allow:         allow,
		AllowFamilies: pc.StoreFamiliesAllowed(),
		Open: func(ctx context.Context, dbName string) (*GameDB, error) {
			return OpenPlacementStore(ctx, dbName, pool)
		},
	})
	if err != nil {
		return nil, err
	}
	logx.Infof("[placement] store registry ready: zone=%d allowlist=%s storeFamilies=%v storePool=%d/%d storeIdleTimeout=%v",
		config.AppConfig.ZoneId, allow.String(), pc.StoreFamiliesAllowed(), pool.MaxOpenConn, pool.MaxIdleConn, pool.ConnMaxIdleTime)
	return registry, nil
}

// PoolSamples 返回此刻每个已打开库的连接池快照(shared/sqlpoolstats.Source,G2b)。
// 只有一类池 metrics.PoolStore(每个地域存档库一个)。读锁内只拷出 *sql.DB,
// Stats() 在锁外取(它自己加 db.mu),不挡按需打开。没有连接的库(单测的 storeNamed)跳过。
// 池常驻不删(见文件头),所以求和的 counter 单调,满足 sqlpoolstats 的前提。
func (r *StoreRegistry) PoolSamples() []sqlpoolstats.Sample {
	r.mu.RLock()
	handles := make([]*sql.DB, 0, len(r.stores))
	for _, store := range r.stores {
		if store == nil || store.DB == nil {
			continue
		}
		handles = append(handles, store.DB)
	}
	r.mu.RUnlock()

	samples := make([]sqlpoolstats.Sample, 0, len(handles))
	for _, handle := range handles {
		samples = append(samples, sqlpoolstats.Sample{Pool: metrics.PoolStore, Stats: handle.Stats()})
	}
	return samples
}

// StorePoolOptions 是每个存档库连接池的参数(Placement.StoreMaxOpenConn / StoreMaxIdleConn / StoreConnMaxIdleSeconds)。
type StorePoolOptions struct {
	MaxOpenConn     int
	MaxIdleConn     int
	ConnMaxIdleTime time.Duration
}

// applyStorePool 把池参数设到 handle 上;ConnMaxLifetime 固定 5 分钟。
// ConnMaxIdleTime ≤ 0 时不设(= database/sql 默认不按空闲时长回收)。
func applyStorePool(handle *sql.DB, pool StorePoolOptions) {
	handle.SetMaxOpenConns(pool.MaxOpenConn)
	handle.SetMaxIdleConns(pool.MaxIdleConn)
	handle.SetConnMaxLifetime(5 * time.Minute)
	if pool.ConnMaxIdleTime > 0 {
		handle.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	}
}

// Store 返回落点 storageID 对应的库,必要时按需打开。
//
// 参数里的 context 不参与打开:同一个库的打开结果被所有并发请求共享,不能随第一个调用方的取消一起失败;
// 打开用自己的有界 context(openTimeout)。返回的错误一律包着 ErrStoreUnavailable。
func (r *StoreRegistry) Store(_ context.Context, storageID uint32) (*GameDB, error) {
	r.mu.RLock()
	store := r.stores[storageID]
	cooling, isCooling := r.cooling[storageID]
	r.mu.RUnlock()
	if store != nil {
		if isCooling {
			return r.storeAfterCooldown(storageID, store, cooling)
		}
		return store, nil
	}

	v, err := r.flight.Do(strconv.FormatUint(uint64(storageID), 10), func() (any, error) {
		opened, err := r.openOnce(storageID)
		if err != nil {
			return nil, err
		}
		return opened, nil
	})
	if err != nil {
		return nil, err
	}
	store, _ = v.(*GameDB)
	if store == nil {
		// go-zero 的 SingleFlight 在打开函数 panic 时让并发等待者拿到 (nil, nil);按不可用处理,不能当成功。
		return nil, fmt.Errorf("%w: storage %d: open produced no store", ErrStoreUnavailable, storageID)
	}
	return store, nil
}

// reject 处理一次不拨号的拒绝:记失败(退避期内同一个库直接回这个错误,不刷日志)、计数、打一行日志。
// 这个进程的白名单和家族开关启动后不变,所以被拒的库要改部署再重启才能打开;其间这些玩家的写一直延后,不消耗重试。
func (r *StoreRegistry) reject(storageID uint32, err error) error {
	r.recordFailure(storageID, err)
	metrics.CountPlacementStoreOpen(metrics.StoreOpenRejected)
	logx.Errorf("[placement] store rejected (writes for its players stay deferred without consuming retries until the deployment is fixed): storage=%d err=%v",
		storageID, err)
	return err
}

// openOnce 在 singleflight 里执行:同一个库同一时刻只有一个 goroutine 走到这里。
func (r *StoreRegistry) openOnce(storageID uint32) (*GameDB, error) {
	// 复查:排在前面的一次打开可能在本请求读缓存与进入 singleflight 之间刚好完成或刚好失败。
	r.mu.RLock()
	store := r.stores[storageID]
	failure, failed := r.failures[storageID]
	r.mu.RUnlock()
	if store != nil {
		return store, nil
	}
	if failed && r.now().Sub(failure.at) < r.retryBackoff {
		return nil, failure.err
	}

	name, isStore := placement.StoreDBName(storageID)
	if !isStore {
		// 0 和区号的值域(zone 库已退役,A17c)不派生库名。登记落点时就拒了,走到这里说明路由行或调用方坏了。
		return nil, r.reject(storageID, fmt.Errorf("%w: storage %d is not a player store id (want >= %d)",
			ErrStoreUnavailable, storageID, placement.FirstStorageID))
	}
	via, admitted := AdmitStore(r.allow, r.allowFamilies, name)
	if !admitted {
		return nil, r.reject(storageID, fmt.Errorf("%w: %s is neither in the database allow list %s nor admitted by Placement.AllowStoreFamilies=%v",
			ErrStoreUnavailable, name, r.allow.String(), r.allowFamilies))
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.openTimeout)
	defer cancel()
	opened, err := r.open(ctx, name)
	if err == nil {
		switch {
		case opened == nil || opened.SqlModel == nil:
			err = errors.New("opener returned no store")
		case opened.SqlModel.DBName != name:
			err = fmt.Errorf("opener returned database %q, want %q", opened.SqlModel.DBName, name)
			// 落错库的池不能留着,也不能交出去。
			if opened.DB != nil {
				_ = opened.DB.Close()
			}
		}
	}
	if err != nil {
		wrapped := fmt.Errorf("%w: open %s: %w", ErrStoreUnavailable, name, err)
		// 闸门冻结是搬库窗口,秒级,库本身没有坏:不进冷却,窗口一过下一条任务就能打开。
		if authoritysql.IsWriterFrozen(err) {
			logx.Infof("[placement] store open hit a frozen writer gate, not backing off: storage=%d db=%s", storageID, name)
			return nil, wrapped
		}
		r.recordFailure(storageID, wrapped)
		metrics.CountPlacementStoreOpen(metrics.StoreOpenError)
		logx.Errorf("[placement] store open failed (writes for its players keep being deferred without consuming retries; next attempt in %v): storage=%d admitted_by=%s err=%v",
			r.retryBackoff, storageID, via, err)
		return nil, wrapped
	}

	r.mu.Lock()
	r.stores[storageID] = opened
	delete(r.failures, storageID)
	openStores := len(r.stores)
	r.mu.Unlock()
	metrics.CountPlacementStoreOpen(metrics.StoreOpenOK)
	metrics.SetPlacementOpenStores(openStores)
	logx.Infof("[placement] store opened on demand: storage=%d db=%s admitted_by=%s open_stores=%d",
		storageID, name, via, openStores)
	return opened, nil
}

func (r *StoreRegistry) recordFailure(storageID uint32, err error) {
	r.mu.Lock()
	r.failures[storageID] = storeOpenFailure{at: r.now(), err: err}
	r.mu.Unlock()
}

// OpenPlacementStore 按需打开一个落点库:独立 *sql.DB(DSN 默认库 = dbName)+ 独立 proto2mysql 模型。
//
// 为什么每个库必须有自己的连接池:proto2mysql 生成的 SQL 都是不带库名的裸表名
// (UPDATE `<表名>` … / INSERT INTO `<表名>` …,Save 按主键先 UPDATE、0 行再 INSERT),落到哪个库完全取决于连接的当前库;而 USE 是会话级的,
// 只切换池里当场借到的**一条**连接。f3b308f 版的 OpenDB 正是靠一次 USE「切库」,池里其余连接
// 仍落在 DSN 的默认库;proto2mysql main(83fed85 起)的 OpenDB 已改为不 USE、只校验 DSN 的库。
// 两种实现下「库由 DSN 默认库决定」都成立,所以每个落点库一个连接池,DSN 的库名就是落点库名。
// 多个库共用一个 *sql.DB 靠 USE 切换,写会随连接复用漂到别的库上。
// pool 是每个存档库连接池的参数(NewStoreRegistryFromConfig 从 Placement.Store* 取)。
func OpenPlacementStore(ctx context.Context, dbName string, pool StorePoolOptions) (*GameDB, error) {
	if err := validateDatabaseIdentifier(dbName); err != nil {
		return nil, err
	}
	tables, err := TablesFromJSON()
	if err != nil {
		return nil, err
	}
	mysqlConfig := newServiceMysqlConfig()
	mysqlConfig.DBName = dbName
	connector, err := newPlayerConnector(mysqlConfig)
	if err != nil {
		return nil, fmt.Errorf("create MySQL connector for %s: %w", dbName, err)
	}
	handle := sql.OpenDB(connector)
	applyStorePool(handle, pool)
	if err := authoritysql.AssertCurrentSession(ctx, handle, authoritysql.Backend(config.AppConfig.ServerConfig.Database.Backend)); err != nil {
		_ = handle.Close()
		return nil, err
	}
	store, err := preparePlacementStore(ctx, handle, dbName, tables)
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	if err := playerstore.VerifySchema(ctx, handle); err != nil {
		_ = handle.Close()
		return nil, fmt.Errorf("verify player store %s: %w", dbName, err)
	}
	store.Connector = connector
	return store, nil
}

// preparePlacementStore 在已建好的连接池上做按需打开的全部核对,拆出来是为了用 dbtest 假驱动单测。
//
// 顺序:Ping(库不存在 = 1049,直接失败,绝不建库)→ 断言连上的 DATABASE() 就是 dbName →
// 注册表并过表名守卫(纯内存)→ 只读 schema 闸(缺列 / 可补的缺表拒绝,与启动期同一道闸)→
// OpenDB 把模型绑到这个池上(旧版会在一条连接上 USE dbName,新版只校验;见 OpenPlacementStore)。
func preparePlacementStore(ctx context.Context, handle *sql.DB, dbName string, tables []proto.Message) (*GameDB, error) {
	if err := handle.PingContext(ctx); err != nil {
		if isUnknownDatabaseError(err) {
			return nil, fmt.Errorf("placement store %s does not exist; on-demand open never creates databases, create it in the deploy stage with `go run ./cmd/migrate -f <db 配置> -storage-id <id> -command up -create-database`: %w",
				dbName, err)
		}
		return nil, fmt.Errorf("ping placement store %s: %w", dbName, err)
	}
	// 放行(白名单 / 家族)由注册表在打开之前判定;这里只确认连接池真的落在 dbName 上,
	// 所以交给 AssertDatabase 的清单就是 dbName 本身。
	if _, err := dbguard.AssertDatabase(ctx, handle, dbguard.AssertOptions{
		Expected: dbName,
		Allow:    dbguard.Allowlist{Names: []string{dbName}, Source: "placement-admitted"},
	}); err != nil {
		return nil, err
	}
	model := proto2mysql.NewDB()
	if err := registerTables(model, tables); err != nil {
		return nil, err
	}
	if err := assertSchemaUpToDate(ctx, handle, model, dbName, tables); err != nil {
		// 闸内的补救命令只写了占位的 -storage-id,这里把这个库的编号点名出来,照抄就是迁这个库。
		storageID, _ := placement.StorageIDFromDBName(dbName)
		return nil, fmt.Errorf("placement store %s failed the read-only schema gate (run the migrate command below with `-storage-id %d`): %w",
			dbName, storageID, err)
	}
	if err := model.OpenDB(handle, dbName); err != nil {
		return nil, fmt.Errorf("select placement store %s: %w", dbName, err)
	}
	return &GameDB{DB: handle, SqlModel: model}, nil
}

// RelayStorageIDs 返回审计发件箱转发要覆盖的存档库编号,升序、去重(docs/design/player-audit-outbox.md §7.1)。
//
// 外部白名单不空时就是白名单里的全部存档库:部署保证它恰好是本地域的 K 个库,所以转发不依赖本进程有没有打开过它们
// (进程重启之后,没人认领过的库里也可能有上一个进程写下、还没投出去的行)。白名单为空只出现在本机 dev 的宽松档,
// 这时退到本进程已经打开过的库:那里的存档库都是按需打开的,没打开过的库在这个进程里也没写过发件箱。
func (r *StoreRegistry) RelayStorageIDs() []uint32 {
	ids := make([]uint32, 0, len(r.allow.Names))
	for _, name := range r.allow.Names {
		if id, ok := placement.StorageIDFromDBName(name); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		r.mu.RLock()
		for id := range r.stores {
			ids = append(ids, id)
		}
		r.mu.RUnlock()
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
