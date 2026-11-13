// Package authoritysql 为全局领域的权威SQL物理连接固定当前强读会话。
package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// tidb_constraint_check_in_place_pessimistic:同键并发首发靠语句当场撞 1062 转读赢家(聊天、邮件、评级);
// 关掉它 1062 拖到 COMMIT,写者闸门就只能判结果未知。TiDB 6.3 起才有这个变量,低于它的版本建连失败。
var tidbSettings = []string{
	"SET SESSION tidb_snapshot=''",
	"SET SESSION tidb_read_consistency='strict'",
	"SET SESSION tidb_txn_mode='pessimistic'",
	"SET SESSION tidb_read_staleness=0",
	"SET SESSION tidb_low_resolution_tso=OFF",
	"SET SESSION tidb_replica_read='leader'",
	"SET SESSION tidb_constraint_check_in_place_pessimistic=ON",
}

type connector struct {
	inner        driver.Connector
	backend      Backend // 构造处已 ParseBackend,这里只会是两个合法值之一
	keepLockWait bool
	permission   Permission
	placed       *placedDialer // nil = 固定连 inner;有值时 inner 不用,每条连接按当前映射拨号
}

func (c connector) Driver() driver.Driver { return opaqueDriver{} }
func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	var gate writerGate = passGate{}
	if c.permission.kind == permissionWriter {
		gate = runtimeGate{config: c.permission.writer}
	}
	inner := c.inner
	var stale func() bool
	if c.placed != nil {
		var err error
		if inner, stale, err = c.placed.dial(); err != nil {
			return nil, err
		}
	}
	guarded, err := newGuardedConnector(inner, gate, c, mysqlStatementPolicy{}, c.permission)
	if err != nil {
		return nil, err
	}
	guarded.stale = stale
	return guarded.Connect(ctx)
}

// PlacementResolver 给出逻辑库当前的物理落点。shared/dbplacement.Watcher 实现它,本包不碰 etcd。
type PlacementResolver interface {
	// ResolvePlacement 返回逻辑库当前所在集群的地址和映射版本。建连、连接复用、连接归还都会调,必须便宜、不阻塞。
	// 错误表示此刻不知道该连哪里:建连失败,已有连接按过期丢弃。
	ResolvePlacement(logicalDB string) (addr string, version uint64, err error)
}

// OpenOption 是 Open / OpenForMigration 的可选项。
type OpenOption func(*openOptions)

type openOptions struct {
	placement PlacementResolver
}

// WithPlacement 让连接池按映射拨号:DSN 里的地址不用,其余连接参数不变。*sql.DB 句柄全程不变,业务代码不用改。
// 不传这个选项时行为和没有映射时完全一样。设计见 docs/design/horizontal-scale-logical-db.md §8.1。
func WithPlacement(resolver PlacementResolver) OpenOption {
	return func(o *openOptions) { o.placement = resolver }
}

// placedDialer 按映射选集群建连,并给每条连接一个「映射是否已变」的判定。
type placedDialer struct {
	template  *mysql.Config
	logicalDB string
	resolver  PlacementResolver
	// newInner 是真正拨号的连接器,单测换成假驱动。
	newInner func(*mysql.Config) (driver.Connector, error)
}

func newPlacedDialer(template *mysql.Config, resolver PlacementResolver) *placedDialer {
	return &placedDialer{template: template, logicalDB: template.DBName, resolver: resolver, newInner: func(cfg *mysql.Config) (driver.Connector, error) {
		return mysql.NewConnector(cfg)
	}}
}

// dial 先读映射再拨号:拨号途中翻转时,这条连接记的是旧版本,下一次归还或复用就被丢弃,不会留在连接池里。
// 判定按版本比,不比地址:同一集群上的版本变化(反向搬回来)也要换连接。
func (d *placedDialer) dial() (driver.Connector, func() bool, error) {
	addr, version, err := d.resolver.ResolvePlacement(d.logicalDB)
	if err != nil {
		return nil, nil, fmt.Errorf("权威SQL逻辑库落点未知: %w", err)
	}
	cfg := d.template.Clone()
	cfg.Addr = addr
	inner, err := d.newInner(cfg)
	if err != nil {
		return nil, nil, errors.New("权威SQL连接器无效")
	}
	stale := func() bool {
		_, current, err := d.resolver.ResolvePlacement(d.logicalDB)
		return err != nil || current != version
	}
	return inner, stale, nil
}

// pin 在新建物理连接时执行一次:固定会话变量,核对主体、实际库,写者池再核世代与本 boot 的注册全文。
// 连接复用时不重放它,见 revalidate。
func (c connector) pin(ctx context.Context, conn driver.Conn) error {
	if c.permission.kind == permissionWriter {
		defer observeWriterGate(c.permission.writer.AuthoritySchema(), gatePhasePin, time.Now())
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		return errors.New("SQL驱动不支持会话初始化")
	}
	// mysql 档先核身份再动会话。逐物理连接都核,回池复用时 revalidate 再核:主从切换后同一地址可能已经是从库,只在启动时核一次拦不住。
	if c.backend == BackendMySQL {
		if err := verifyMySQLConnection(ctx, conn); err != nil {
			return err
		}
	}
	statements := []string{"SET SESSION autocommit=1", "SET SESSION transaction_isolation='READ-COMMITTED'", "SET SESSION sql_mode='STRICT_TRANS_TABLES'"}
	if !c.keepLockWait {
		statements = append(statements, "SET SESSION innodb_lock_wait_timeout=1")
	}
	if c.backend == BackendTiDB {
		statements = append(statements, tidbSettings...)
	}
	for _, statement := range statements {
		if _, err := exec.ExecContext(ctx, statement, nil); err != nil {
			return errors.New("权威SQL会话初始化失败")
		}
	}
	values, err := rawOne(ctx, conn, "SELECT CURRENT_USER(),DATABASE()", nil, 2)
	if err != nil {
		return errors.New("权威SQL主体或实际库观测失败")
	}
	principal, database := stringValue(values[0]), stringValue(values[1])
	if c.permission.kind == permissionWriter {
		grant := c.permission.writer.boot.grant
		// 闸门和业务写必须在同一个逻辑库:提交前锁的 slot 和业务行才在一个集群、一个事务里。
		if principal != string(grant.SqlPrincipal) || database != grant.AuthoritySchema {
			return ErrWriterDenied
		}
		meta, err := readRawMeta(ctx, conn, grant.AuthoritySchema)
		if err != nil || meta.Generation != grant.Generation || meta.WriterEpoch == 0 {
			return ErrWriterDenied
		}
		if err := verifyBootRegistration(ctx, conn, grant); err != nil {
			return err
		}
	} else if c.permission.kind == permissionSchema || c.permission.kind == permissionOperator {
		if principal != c.permission.principal {
			return errors.New("权威SQL受限主体不匹配")
		}
	}
	return nil
}

// idleProbeAfter 是「刚用过的连接不用探测」的窗口:连接归还之后空闲不到这么久又被借出,复用时一条语句都不发;
// 空闲更久的发一条读(verifyTiDBSession)。取值与做法照 HikariCP 的 aliveBypassWindow(500ms):窗口之内对端刚应答过,
// 再探一次得不到新信息;窗口之外才值得花一次往返。存盘高峰时连接几毫秒就被再次借出,热路径上不多发语句。
const idleProbeAfter = 500 * time.Millisecond

// revalidate 是连接复用时的核对(database/sql 每次从池里取出连接都调 ResetSession)。idle 是这条连接归还之后空闲了多久。
//
// 2026-11-12 之前这里把 pin 整个重放:十几条 SET、读主体与库名,写者池再读 route_meta 和两条注册核对,每次取连接约 14 次往返,
// 一笔存盘事务的往返大半花在这里(docs/design/player-save-direct-commit.md §9 第 1 条)。现在只核「不经过这条连接也会变」的事实:
//   - 会话变量、默认库、登录主体只有语句能改。运行池(Reader、Writer)的语句包络只放行单条数据语句,不放行 SET、USE、
//     事务控制语句和 SET_VAR 提示(statement.go);驱动的 ResetSession 只做本地存活检查,不向服务端发重置。
//     所以建连时固定之后不会再变,不重放。TestRuntimeEnvelopeKeepsPinnedSessionFixed 钉住这条前提:
//     放宽包络的人会先在那里变红。带隔离级别的 BEGIN 留下的一次性隔离级别由 guardedConn.beginLocked 处理。
//   - 对端还在不在:重放 pin 的时候,第一条 SET 顺带就是一次主动探测。对端没发 FIN 就消失的连接(节点宕机、网络分区),
//     驱动的本地存活检查看不出来;不探测的话,池里每条这样的连接都要让一次业务调用失败,Reader 池上那条读还会被记成结局未知
//     (docs/design/infra-reconnect-overview.md §4.1 R2)。所以空闲超过 idleProbeAfter 的连接复用时发一条读:
//     读的就是建连时固定的那组会话变量,一次往返同时确认对端还在、会话没被改过(也就守住了上一条里
//     「驱动复用时不重置会话」这个只靠读驱动源码确认的前提)。失败或不符都由 ResetSession 回 ErrBadConn,
//     这时还没发业务语句,database/sql 换一条连接,业务调用照常成功。
//   - 写者的世代、冻结、slot 与注册行:每笔写事务在 BEGIN 前后各核一次(runtimeGate.beforeBegin、runtimeAttempt.enter,
//     后者在 slot 行锁之下),事务外的读过读闸门(admitRead)。复用时既不持锁也不在事务里,再读一遍判不出更多,
//     判完到事务开始之间照样会变。REGISTER 收据是只追加的永久原件、按 registration_digest 取,
//     注册行的摘要每笔写事务都核,全文在建连时核过一次就够。
//   - mysql 档:主从切换之后,同一条连接的对端可能已经降成只读从库,读它不再是当前读。这一条照旧逐次核对
//     (它本身就是一次往返,也起探测的作用)。
//   - Schema、Operator 权限的池(迁移、注册、维护工具)不在热路径上,而且 Schema 的包络放行一条改锁等待的 SET
//     (schemaLockWaitStatement):它们照旧整个重放 pin,行为不变。
func (c connector) revalidate(ctx context.Context, conn driver.Conn, idle time.Duration) error {
	if c.permission.kind != permissionReader && c.permission.kind != permissionWriter {
		return c.pin(ctx, conn)
	}
	if c.backend == BackendTiDB && idle < idleProbeAfter {
		return nil
	}
	if c.permission.kind == permissionWriter {
		defer observeWriterGate(c.permission.writer.AuthoritySchema(), gatePhaseRevalidate, time.Now())
	}
	if c.backend == BackendMySQL {
		return verifyMySQLConnection(ctx, conn)
	}
	return verifyTiDBSession(ctx, conn)
}

// tidbSessionSQL 读回 tidb 档建连时固定的强读会话变量(tidbSettings 加隔离级别)。
const tidbSessionSQL = "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot"

// tidbSessionPinned 判断 tidbSessionSQL 读回的值是不是建连时固定的那一组。AssertCurrentSession(启动核对)与
// verifyTiDBSession(空闲连接复用)共用这一条规则。
func tidbSessionPinned(mode, isolation string, staleness, lowResolutionTSO int64, replicaRead, consistency, snapshot string) bool {
	return mode == "pessimistic" && isolation == "READ-COMMITTED" && staleness == 0 && lowResolutionTSO == 0 &&
		replicaRead == "leader" && consistency == "strict" && snapshot == ""
}

// verifyTiDBSession 在物理连接上读一次固定的会话变量:读不回来(对端没了)或和建连时固定的不一样都回错误。
// 不碰任何表,不过闸门,不改会话。
func verifyTiDBSession(ctx context.Context, conn driver.Conn) error {
	values, err := rawOne(ctx, conn, tidbSessionSQL, nil, 7)
	if err != nil {
		return err
	}
	// 这两列固定的值都是 0。驱动按列类型给值:布尔开关是整数,整数开关是文本(TiDB 把它当字符串列回),unsignedValue 两种都认。
	// 读回负数(tidb_read_staleness 生效时是负的秒数)或解析不了,和读回非 0 一样算不符。
	staleness, e1 := unsignedValue(values[2])
	lowResolutionTSO, e2 := unsignedValue(values[3])
	if e1 != nil || e2 != nil || !tidbSessionPinned(stringValue(values[0]), stringValue(values[1]), int64(staleness), int64(lowResolutionTSO),
		stringValue(values[4]), stringValue(values[5]), stringValue(values[6])) {
		return errors.New("权威SQL强读会话不符")
	}
	return nil
}

// PinCurrentSession 复用既有权威 SQL 会话初始化；调用方保留自己的连接池与锁等待预算。
// tidb 档的专用 SET 在每条物理连接建立时执行，接到 MySQL 必须失败；mysql 档逐连接核对对端是 MySQL 主库，接到 TiDB 或从库必须失败。
func PinCurrentSession(inner driver.Connector, backend Backend, permission Permission) (driver.Connector, error) {
	if inner == nil {
		return nil, errors.New("缺少SQL连接器")
	}
	if err := permission.validate(); err != nil {
		return nil, err
	}
	parsed, err := ParseBackend(string(backend))
	if err != nil {
		return nil, err
	}
	return connector{inner: inner, backend: parsed, keepLockWait: true, permission: permission}, nil
}

// PinPlacedSession 是 PinCurrentSession 的按映射拨号版本:连接参数取自 template,地址在每次建连时由 resolver 给出。
// 逻辑库就是 template.DBName。给自己管连接池和超时参数的调用方用(go/db)。
func PinPlacedSession(template *mysql.Config, resolver PlacementResolver, backend Backend, permission Permission) (driver.Connector, error) {
	if template == nil || resolver == nil || template.DBName == "" {
		return nil, errors.New("缺少SQL连接模板或放置解析器")
	}
	if err := permission.validate(); err != nil {
		return nil, err
	}
	parsed, err := ParseBackend(string(backend))
	if err != nil {
		return nil, err
	}
	return connector{backend: parsed, keepLockWait: true, permission: permission, placed: newPlacedDialer(template.Clone(), resolver)}, nil
}

// AssertCurrentSession 在业务池投入使用前确认强读口径。tidb 档读回全部强读变量；
// mysql 档读回 RC 并确认对端是 MySQL 主库(读主库就是 MySQL 上的当前读，没有 TiDB 那组变量可核)。
func AssertCurrentSession(ctx context.Context, db *sql.DB, backend Backend) error {
	parsed, err := ParseBackend(string(backend))
	if err != nil {
		return err
	}
	if parsed == BackendMySQL {
		return assertMySQLSession(ctx, db)
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT tidb_version()").Scan(&version); err != nil {
		return errors.New("权威SQL必须连接TiDB")
	}
	var mode, isolation, replica, consistency, snapshot string
	var stale, low int64
	if err := db.QueryRowContext(ctx, tidbSessionSQL).Scan(&mode, &isolation, &stale, &low, &replica, &consistency, &snapshot); err != nil || !tidbSessionPinned(mode, isolation, stale, low, replica, consistency, snapshot) {
		return errors.New("权威SQL强读会话不符")
	}
	return nil
}

// Open 不记录DSN；新物理连接逐条清历史快照后再关闭staleness，禁止URL绕过。
// 生产期望库名由领域代码常量给出；测试只能为本次随机库传入对应名字。
func Open(ctx context.Context, dsn, expectedDatabase string, backend Backend, maxOpen int, permission Permission, options ...OpenOption) (*sql.DB, error) {
	return open(ctx, dsn, expectedDatabase, backend, maxOpen, 5*time.Second, permission, options)
}

// OpenForMigration 与运行池复用相同的强会话约束，但DDL由迁移器的
// statement context限制。独立池的socket预算容纳TiDB预切Region，
// 不改变任何业务请求、锁等待或资产投递预算。
func OpenForMigration(ctx context.Context, dsn, expectedDatabase string, backend Backend, permission Permission, options ...OpenOption) (*sql.DB, error) {
	if permission.kind != permissionSchema {
		return nil, errors.New("迁移池必须显式Schema权限")
	}
	// 迁移器占住一条连接持有GET_LOCK；取消DDL时需第二条连接发KILL QUERY。
	return open(ctx, dsn, expectedDatabase, backend, 2, 2*time.Minute, permission, options)
}

// DSNForSchema 把控制账号的连接串指到某个逻辑库。同一集群上的逻辑库共用注册、维护账号，
// 库名由已固定摘要的 Request / Plan 决定，不为每个库各配一条连接串。
// 库搬到别的集群之后地址由 WithPlacement 按映射给(账号在目标集群提前建好),这条连接串里的地址不用。
func DSNForSchema(dsn, schema string) (string, error) {
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", errors.New("权威SQL连接串无效")
	}
	if !validIdentifier(schema) {
		return "", errors.New("权威SQL目标库名无效")
	}
	parsed.DBName = schema
	return parsed.FormatDSN(), nil
}

// connMaxIdle 是空闲连接最多留多久。要短于链路上中间设备断开空闲 TCP 的时间(云负载均衡常见 4~6 分钟,
// IPVS 缺省 15 分钟):被静默丢弃的连接收不到 FIN,复用前的存活检查看不出来,第一条语句要等满读超时才失败,
// 而且那时语句已经发出,结局未知(docs/design/infra-reconnect-overview.md §4 R2)。
const connMaxIdle = 3 * time.Minute

func open(ctx context.Context, dsn, expectedDatabase string, backend Backend, maxOpen int, ioTimeout time.Duration, permission Permission, options []OpenOption) (*sql.DB, error) {
	var opts openOptions
	for _, option := range options {
		option(&opts)
	}
	if err := permission.validate(); err != nil {
		return nil, err
	}
	parsedBackend, err := ParseBackend(string(backend))
	if err != nil {
		return nil, err
	}
	if expectedDatabase == "" || maxOpen < 1 || maxOpen > 128 {
		return nil, errors.New("权威SQL配置无效")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("权威SQL连接串无效")
	}
	if parsed.DBName != expectedDatabase {
		return nil, errors.New("权威SQL目标库不匹配")
	}
	// 不让驱动在有序初始化之前设置这些会话变量。
	for key := range parsed.Params {
		if strings.HasPrefix(strings.ToLower(key), "tidb_") || strings.EqualFold(key, "transaction_isolation") || strings.EqualFold(key, "sql_mode") {
			delete(parsed.Params, key)
		}
	}
	parsed.ParseTime = true
	parsed.MultiStatements = false
	parsed.Timeout = time.Second
	parsed.ReadTimeout = ioTimeout
	parsed.WriteTimeout = ioTimeout
	inner, err := mysql.NewConnector(parsed)
	if err != nil {
		return nil, errors.New("权威SQL连接器无效")
	}
	conn := connector{inner: inner, backend: parsedBackend, permission: permission}
	if opts.placement != nil {
		conn.placed = newPlacedDialer(parsed, opts.placement)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(15 * time.Minute)
	db.SetConnMaxIdleTime(connMaxIdle)
	ready := false
	defer func() {
		if !ready {
			_ = db.Close()
		}
	}()
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(check); err != nil {
		return nil, fmt.Errorf("权威SQL启动失败: %w", err)
	}
	// 写资格是一次性的:这几条核对撞上冻结的闸门就报错退出的话,这个 Pod 的 grant 就烧掉了。它们不碰业务数据,不过闸门。
	check = startupProbe(check)
	if err := verifyPoolSession(check, db, expectedDatabase, parsedBackend); err != nil {
		return nil, err
	}
	if permission.kind == permissionWriter {
		if err := VerifyCatalogSchema(check, db, permission.writer.AuthoritySchema()); err != nil {
			return nil, err
		}
	}
	ready = true
	return db, nil
}

// verifyPoolSession 核连接池确实连着期望的库,而且是强读会话。错误不含连接串。
func verifyPoolSession(ctx context.Context, db *sql.DB, expectedDatabase string, backend Backend) error {
	var actual string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&actual); err != nil {
		return fmt.Errorf("权威SQL实际库读取失败: %w", err)
	}
	if actual != expectedDatabase {
		return errors.New("权威SQL实际库不匹配")
	}
	return AssertCurrentSession(ctx, db, backend)
}
