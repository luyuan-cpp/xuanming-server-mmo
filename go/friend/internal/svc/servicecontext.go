// Package svc 是 friend 服务的依赖装配:etcd 客户端、mmorpg_friend 连接池、
// FriendRedis / PlayerRedis 双句柄,以及 S2C 推送用的 Kafka writer。
//
// 与 trade / chat 的 svc 包刻意不同的一点:本包**不含** Prometheus 指标。
// friend 的指标在 friend/internal/metrics 里(懒注册 + Start(addr),由 main 调用),
// 本包一行都不 import 它 —— 否则 svc 与 metrics 双向牵引,将来 metrics 想记
// "Redis 回落"这类装配期事件就会成环。
package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"friend/internal/config"
	friendkafka "friend/internal/kafka"
	"friend/internal/sqldialect"

	redv9 "github.com/redis/go-redis/v9"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/idsegment"
	"shared/kafkacmd"
	"shared/kafkautil"
	"shared/playerredis"
	"shared/playersync"

	// database/sql 只认注册过的驱动名;这个空导入就是注册 "mysql" 的唯一动作,删了它
	// sql.Open("mysql", ...) 会在运行期报 unknown driver,而编译期一声不响。
	_ "github.com/go-sql-driver/mysql"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	dspb "proto/data_service"
)

const (
	// etcdDialTimeout 与 chat / trade 同值:这里只是建客户端(clientv3.New 默认不阻塞等连通),
	// 真正的连通性由 noderegistry 注册与 killswitch watch 在各自的 ctx 超时里暴露。
	etcdDialTimeout = 5 * time.Second

	// mysqlPingTimeout 启动期探库上限,与 trade / data_service store.openMySQL 同值。
	// 探库必须有上限:MySQL 不可达时 TCP 连接可能挂到内核超时(分钟级),
	// 那样 K8s 的 startupProbe 会先判失败并重启 Pod,日志里看不到"库连不上"这句因果。
	mysqlPingTimeout = 5 * time.Second

	// kafkaWriteTimeout 与 go/match/internal/svc/servicecontext.go 的默认值同口径(5s)。
	// friend 不做成配置项:推送是 at-most-once 的旁路,唯一需要的是"别无上限地挂住"——
	// kafka-go 的 Writer 零值 WriteTimeout 是 10s,比 RPC 预算(Timeout 4000ms 减
	// 在带回包预留)还长,一次 broker 抖动就能把好友申请的响应拖成客户端超时。
	kafkaWriteTimeout = 5 * time.Second
)

type ServiceContext struct {
	// LoginSessions 只读显式全局 login 登录态，与好友缓存/在线投影句柄分离。
	LoginSessions redv9.UniversalClient
	Config        config.Config

	// Etcd 供 shared/noderegistry 注册与 shared/killswitch 监听共用 —— 同一个集群不开第二条连接。
	//
	// 它与 go-zero 自己的 etcd 注册无关:zrpc.RpcServerConf.HasEtcd() 判的是
	// `len(Etcd.Hosts) > 0 && len(Etcd.Key) > 0`(go-zero v1.10.0 zrpc/config.go),
	// friend.yaml 按契约 D-13 显式写 `Key: ""`(只能写空串、不能整行省略,省略时
	// conf.MustLoad 直接 Fatal),所以 zrpc.NewServer 走 internal.NewRpcServer 而不是
	// NewRpcPubServer —— etcd 里只有 noderegistry 按 C++ NodeInfo 约定写的
	// FriendNodeService.rpc/... 的键,没有 go-zero 自己那套 friend.rpc。
	Etcd *clientv3.Client

	// DB 是 mmorpg_friend 的连接池(friend 独占逻辑库,D-14)。表以
	// proto/friend/friend_table.proto 为唯一事实源、由 go/schemamigrate 建
	// (启动期 AutoMigrate 或 `-migrate`),本包一行 DDL 都不发。
	DB *sql.DB

	// SQLDialect 是 DB 背后那台服务器的方言(MySQL / TiDB),启动期探测一次。
	// 好友推荐两条查询的优化器提示按它分写(data/recommend_sql.go);手工构造的 ServiceContext
	// (logic 包的单测)不填它,零值即 MySQL。
	// 类型来自只依赖标准库的叶子包 sqldialect:装配层不为一个枚举去直接 import 数据层。
	SQLDialect sqldialect.Dialect

	// 双存储(契约 §4):
	//   - PlayerRedis:玩家域句柄(shared/playerredis,生产可以是 Redis Cluster):跨运行时契约 key
	//     `player:session:{id}` 与 `player:{id}:sync_digest`。键一律带 {player_id},发送前经守卫核对。
	//   - FriendRedis:friend 私有 key(好友列表缓存、申请配额……),go-zero 句柄,可指向 Redis Cluster。
	//     未配置时回落到 Redis 段的共享库(本地单库形态),见 NewRedisHandles。
	PlayerRedis redv9.UniversalClient
	FriendRedis *redis.Redis

	// SyncDigest 在好友写提交之后给受影响玩家的落地摘要 bump 好友三个域
	// (docs/design/landing-recovery-sync-digest.md §3.3 friend 行)。
	// 句柄必须是 PlayerRedis:player:{id}:sync_digest 是 C++ scene 读的玩家域键,FriendRedis 它读不到。
	// 指标 player_sync_bump_total 由 shared/playersync 自己注册在默认 registry 上(叶子包,不与本包成环)。
	// 手工构造的 ServiceContext(logic 单测)不填时为 nil,bump 全部是无操作。
	SyncDigest *playersync.Bumper

	// 在线组队目录按权威 home zone 过滤；映射库不一定与 PlayerRedis 同库。
	DataService   dspb.DataServiceClient
	RequestIDs    *idsegment.Client
	directoryConn *grpc.ClientConn

	// FriendRedisTarget 是 FriendRedis 实际落点的可读描述,启动横幅打印用
	// (排障第一问永远是"好友缓存到底写到哪个库了")。
	FriendRedisTarget string

	// KafkaWriter 是 S2C 推送的出口(kafkautil.PushToPlayer → gate-cmd_g<N> 控制面命令)。
	// **没配 broker 时是 nil**:推送是 at-most-once 的旁路,缺它不该拒启(本地起个 friend
	// 调好友列表不需要 Kafka),但调用方必须自己判空 —— nil Writer 上调 WriteMessages 会 panic。
	KafkaWriter *kafkago.Writer

	// GateCommandBuilder 把消息号 + 序列化体包成 GateCommand。它用的是本服务自己的
	// friend/generated/pb/game 常量,所以只能留在 friend 里、不能上提到 shared。
	GateCommandBuilder kafkautil.GateCommandBuilder

	// 同一 boot 的 SQL 身份与两个独立角色；P1 不关闭 RELAY 或 SQL 池。
	WriterConfig     authoritysql.WriterConfig
	RootAdmission    *drainproof.Session
	RelayAdmission   *drainproof.Session
	RelayIngress     *drainproof.Session
	DrainRuntime     *drainproof.Runtime
	lifecycleMu      sync.Mutex
	sourceStop       func(context.Context) error
	sourceClosed     bool
	resourcesClosed  bool
	resourceCloseErr error
}

// NewServiceContext 建全部依赖。任何一项建不出来都 panic(启动致命,照 trade):
// 没有 etcd 的 friend 注册不了、被路由服发现不到;没有 mmorpg_friend 的 friend
// 每个请求都只能回 kServiceUnavailable。这种进程活着但全量失败的形态比起不来更难查,
// 所以在启动期就把它变成崩溃 + 日志。
func NewServiceContext(c config.Config) *ServiceContext {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(fmt.Errorf("friend SQL boot 装配失败: %w", err))
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New(), drainproof.New())
}

// NewServiceContextWithAdmission 由实际进程 owner 显式传入同一 boot 的身份与角色门禁。
// 新 boot 的 prepared 门禁也在这里传入，不能在 handler 已持有门禁后替换它。
func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root, relay *drainproof.Session) *ServiceContext {
	if len(writer.InstanceID()) != 16 || len(writer.WriterKey()) != 16 || root == nil || relay == nil || root == relay {
		panic("friend SQL boot 与 SOURCE/RELAY 装配缺失或角色复用")
	}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: etcdDialTimeout,
	})
	if err != nil {
		panic("friend: failed to create etcd client: " + err.Error())
	}

	backend := authoritysql.Backend(c.Backend)
	db, err := openMySQL(c.MySQL, backend, authoritysql.Writer(writer))
	if err != nil {
		panic(fmt.Errorf("friend: %w", err))
	}
	dialect, err := detectSQLDialect(db, c.MySQL)
	if err != nil {
		_ = db.Close()
		panic(fmt.Errorf("friend: %w", err))
	}
	// 档位与方言必须一致:推荐查询的提示按方言渲染,两边对不上时提示全部失效而不报错。
	wantDialect := sqldialect.TiDB
	if backend == authoritysql.BackendMySQL {
		wantDialect = sqldialect.MySQL
	}
	if dialect != wantDialect {
		_ = db.Close()
		_ = etcdCli.Close()
		panic(fmt.Sprintf("friend: Backend=%q, but the authority database dialect is %s; refusing to start", c.Backend, dialect))
	}

	friendRds, target := NewRedisHandles(c)
	playerRedisConf := c.EffectivePlayerRedis()
	playerRedis, err := playerredis.New(playerRedisConf)
	if err != nil {
		_ = db.Close()
		_ = etcdCli.Close()
		panic(fmt.Errorf("friend: invalid PlayerRedis config: %w", err))
	}
	logx.Infof("[friend] player redis %s", playerRedisConf.Describe())
	directoryClient, directoryConn := newDirectoryDataService(c)
	requestIDs, err := NewRequestIDSegment(directoryClient)
	if err != nil {
		_ = directoryConn.Close()
		_ = db.Close()
		_ = etcdCli.Close()
		panic(fmt.Errorf("friend 申请号源装配失败: %w", err))
	}
	loginSessions, err := NewLoginSessionClient(c)
	if err != nil {
		requestIDs.Close()
		_ = directoryConn.Close()
		_ = db.Close()
		_ = etcdCli.Close()
		panic("好友显式全局 LoginRedis 不可用")
	}

	kafkaWriter := newKafkaWriter(c)
	if kafkaWriter != nil {
		kafkaWriter.Completion = func(_ []kafkago.Message, err error) {
			if err != nil {
				relay.Hold()
			}
		}
	}
	return &ServiceContext{
		WriterConfig:       writer,
		RootAdmission:      root,
		RelayAdmission:     relay,
		RelayIngress:       drainproof.New(),
		LoginSessions:      loginSessions,
		Config:             c,
		Etcd:               etcdCli,
		DB:                 db,
		SQLDialect:         dialect,
		PlayerRedis:        playerRedis,
		FriendRedis:        friendRds,
		SyncDigest:         playersync.NewBumper(relayDigestEvaler{next: playersync.NewGoRedisEvaler(playerRedis), relay: relay}, "friend"),
		DataService:        directoryClient,
		RequestIDs:         requestIDs,
		directoryConn:      directoryConn,
		FriendRedisTarget:  target,
		KafkaWriter:        kafkaWriter,
		GateCommandBuilder: friendkafka.NewGateCommandBuilder(),
	}
}

func NewLoginSessionClient(c config.Config) (redv9.UniversalClient, error) {
	if !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() || c.LoginRedis.DB < 0 || c.LoginRedis.PoolSizePerNode <= 0 {
		return nil, errors.New("好友全局 LoginRedis 未显式配置")
	}
	client, err := playerredis.New(c.LoginRedis.ClientConfig(playerredis.Config{PoolTimeoutMs: 1000, DialTimeoutMs: 2000,
		ReadTimeoutMs: 2000, WriteTimeoutMs: 2000}))
	if err != nil {
		return nil, errors.New("好友全局 LoginRedis 配置非法")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, errors.New("好友全局登录态实例不可用")
	}
	return client, nil
}

// 沿用全局 dataservice.rpc 注册键与本服务 etcd 集群；非阻塞连接保证好友功能
// 不因目录的请求期依赖不可达而无法启动。归属区查询使用调用者的整体请求预算。
func newDirectoryDataService(c config.Config) (dspb.DataServiceClient, *grpc.ClientConn) {
	rpcConf := zrpc.RpcClientConf{Etcd: c.Etcd, Timeout: 1500, NonBlock: true}
	rpcConf.Etcd.Key = "dataservice.rpc"
	conn := zrpc.MustNewClient(rpcConf).Conn()
	return dspb.NewDataServiceClient(conn), conn
}

// NewRedisHandles 按配置建立 FriendRedis 句柄,并返回它的落点描述。
// FriendRedis.Host 为空即未配置,私有 key 回落到共享库(config.Validate 把 `Redis.Host == ""` 当配置错误拒掉,这里不用再判空)。
// 拆成独立函数是为了不依赖 etcd / MySQL 就能测回落逻辑(与 chat / match 的同名函数同形)。
func NewRedisHandles(c config.Config) (friendRds *redis.Redis, target string) {
	if c.FriendRedis.Host == "" {
		// go-zero 的 logx 没有 Warn 级别;用 Error 级打出来,保证在默认日志级别下一定可见
		// (Info 级在 staging/prod 常被调掉,这条正是那里最需要看到的)。
		// 回落本身不是故障(本地单库形态就是这样),但在 staging/prod 出现就是部署缺陷:
		// 好友私有 key 要和同实例的其他键抢内存。2026-10-12 起本地 compose 也改为 noeviction(K8s 一直是
		// noeviction),共享库设了 maxmemory 时写满会连带拒写契约 key;共享库一旦被配成淘汰型,
		// 契约 key 也可能被淘汰,后果是"玩家明明在线却判成离线",推送静默丢。
		logx.Errorf("[friend] WARN FriendRedis not configured; friend private keys falling back to shared database host=%s type=%s -- "+
			"local single-database deployments only; staging/prod require separate FriendRedis with maxmemory-policy=noeviction (contract section 4)",
			c.Redis.Host, c.Redis.Type)
		return redis.MustNewRedis(c.Redis.RedisConf), fmt.Sprintf("shared-fallback host=%s type=%s", c.Redis.Host, c.Redis.Type)
	}
	logx.Infof("[friend] Separate FriendRedis configuration active host=%s type=%s", c.FriendRedis.Host, c.FriendRedis.Type)
	return redis.MustNewRedis(c.FriendRedis), fmt.Sprintf("FriendRedis host=%s type=%s", c.FriendRedis.Host, c.FriendRedis.Type)
}

// BuildDSN 拼 mmorpg_friend 的 DSN,与 go/trade/internal/svc BuildDSN、
// go/data_service/internal/store/mysql.go buildDSN 同口径:
// sql_mode=%27STRICT_TRANS_TABLES%27 强制会话级严格模式(%27 是转义的单引号)。
// friend 的列现在全是整数,但非严格模式下越界写入会被**静默夹到边界值**
// (friend_count 溢出写成 65535、负数写成 0),而且一个错都不报 —— 容量计数一旦偏了,
// 后面所有"好友列表满"的判定全是错的。在连接层兜底比在每条 SQL 上兜底可靠。
//
// transaction_isolation=%27READ-COMMITTED%27 把**整个连接池**的会话隔离级别设成 RC(驱动在建连时
// 发 SET)。显式写事务本来就用 BeginTx(RC)(data.friendWriteTxIsolation);这一项管的是
// **事务之外**的自动提交语句 —— ensure 的 INSERT(sweep / 回收的逐行 DELETE 2026-10-18 起、Unblock 的 DELETE
// 2026-10-16 起都改在 RC 写事务里,见 data.FriendRepo.deleteOneRowPointLocked / Unblock;TiDB 上自动提交走乐观事务,
// 会与守卫的点锁各持一半互等、检测不到)——
// 它们原先落在服务器全局默认的 REPEATABLE-READ 上,会拿间隙锁 / next-key 锁:
//   - 排队中的间隙锁请求会挡住别人的插入意向锁,是 RR 下经典的插入死锁源;
//   - ensure 的"主键重复 → ODKU 取 X"在 RR 下拿的是 next-key,两个并发 ensure 撞上刚被回收的行时
//     仍可能互等(2026-09-21 死锁事故的后续,docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md)。
//
// RC 下只剩记录锁,friend 全部锁定语句又都是完整主键点查 / 点更新,于是不存在可成环的间隙。
// 自动提交语句每条各取新快照,读语义与 RR 下相同,不影响任何业务判定。
//
// 返回值**含密码**,只能交给 sql.Open,绝不打日志、绝不进错误信息(横幅用 MySQLTarget)。
func BuildDSN(c config.MySQLConf, backend authoritysql.Backend) string {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27"+
		"&transaction_isolation=%%27READ-COMMITTED%%27",
		c.User, c.Password, c.Host, c.DBName)
	if backend != authoritysql.BackendMySQL {
		// 驱动在每条新建物理连接上初始化会话，不能只在当前Ping连接SET一次。
		// 权威列表是自动提交普通读；悲观事务模式不会关闭历史快照、缓存TSO或跳锁弱读。
		// 仅TiDB池设置这些变量，mysql 档不发送它们；禁止改服务器全局模式。
		dsn += "&tidb_txn_mode=%27pessimistic%27&tidb_read_staleness=0" +
			"&tidb_low_resolution_tso=OFF&tidb_replica_read=%27leader%27" +
			"&tidb_read_consistency=%27strict%27&tidb_snapshot=%27%27"
	}
	return dsn
}

// MySQLTarget 是连接目标的可读描述(**不含密码**),供启动横幅与错误信息使用。
func MySQLTarget(c config.MySQLConf) string {
	return fmt.Sprintf("%s/%s (user=%s)", c.Host, c.DBName, c.User)
}

// OpenMySQL 建池、设上限、ping。返回错误时池已关闭(否则 sql.DB 会带着后台
// connectionOpener goroutine 泄漏在一个注定要退出的进程里)。
// 错误信息点名库名:友好度不是重点,重点是 `-migrate` 以 1 退出时运维能直接看出
// 是"库不存在"而不是"密码错"(D-14 第 5 条)。
func OpenMySQL(c config.MySQLConf, backend authoritysql.Backend) (*sql.DB, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, backend, authoritysql.Writer(writer))
}

// OpenSchemaMySQL只属于显式迁移入口，常驻业务不得借用此能力。
func OpenSchemaMySQL(c config.MySQLConf, backend authoritysql.Backend) (*sql.DB, error) {
	permission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openMySQL(c, backend, permission)
}

func openMySQL(c config.MySQLConf, backend authoritysql.Backend, permission authoritysql.Permission) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	db, err := authoritysql.Open(ctx, BuildDSN(c, backend), c.DBName, backend, c.MaxOpenConn, permission)
	if err != nil {
		return nil, fmt.Errorf("open MySQL %s: %w", MySQLTarget(c), err)
	}
	// 上限必须显式设:database/sql 的 MaxOpenConns 零值是"不限",
	// 一次慢查询风暴就能把 MySQL 的 max_connections 吃光、连带打死同库的别人。
	db.SetMaxOpenConns(c.MaxOpenConn)
	db.SetMaxIdleConns(c.MaxIdleConn)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping MySQL %s 失败(库 %s 不存在时先按 deploy/mysql-init/00_init_zone_dbs.sql 建库并授权): %w",
			MySQLTarget(c), c.DBName, err)
	}
	if backend != authoritysql.BackendMySQL {
		dialect, _, err := sqldialect.Detect(ctx, db)
		if err != nil || dialect != sqldialect.TiDB {
			_ = db.Close()
			return nil, fmt.Errorf("好友权威池必须连接TiDB: %w", errors.Join(err, errors.New("后端校验未通过")))
		}
		var mode, isolation, replica, consistency, snapshot string
		var staleness, lowResolution int64
		err = db.QueryRowContext(ctx,
			"SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot").
			Scan(&mode, &isolation, &staleness, &lowResolution, &replica, &consistency, &snapshot)
		if err != nil || mode != "pessimistic" || isolation != "READ-COMMITTED" || staleness != 0 || lowResolution != 0 || replica != "leader" || consistency != "strict" || snapshot != "" {
			_ = db.Close()
			return nil, fmt.Errorf("TiDB好友权威池必须使用RC悲观强读会话: %w", errors.Join(err, errors.New("权威会话参数校验未通过")))
		}
	}
	return db, nil
}

// detectSQLDialect 在启动期问一次服务器版本、判出方言,并把结果打进启动日志
// (排障时"这个进程到底把库当成了 MySQL 还是 TiDB"必须一眼可查:判错的后果是推荐查询的提示全部失效)。
// 探测失败、或对端是不支持的服务器(MariaDB),都算启动致命,理由见 sqldialect.Detect / sqldialect.FromServerInfo。
// 超时与 ping 同值:两者是同一次连通性确认的两步。
func detectSQLDialect(db *sql.DB, c config.MySQLConf) (sqldialect.Dialect, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	dialect, info, err := sqldialect.Detect(ctx, db)
	if err != nil {
		return dialect, fmt.Errorf("detect SQL dialect of %s: %w", MySQLTarget(c), err)
	}
	// 日志文案写英文(提交 946ffd216d「日志文案统一为英文」起的做法);交接文档里 Codex 的通过标准引用的就是这一行的前缀。
	// 两个原始串都打出来:TiDB 配了 server-version 时 VERSION() 里没有 TiDB 字样,判据落在 @@version_comment 上,
	// 排障的人要能从这一行看出"为什么判成了 tidb"。
	logx.Infof("[friend] SQL dialect = %s (%s, %s)", dialect, MySQLTarget(c), info)
	return dialect, nil
}

// newKafkaWriter 建 S2C 推送用的 writer;没配 broker 返回 nil(见 ServiceContext.KafkaWriter 注释)。
//
// ⚠ 这里是本文件唯一读 Kafka 配置的地方:冻结规格 §3.1 列的 Config 结构里没有 Kafka 段,
// 但 §3.6 要求 ServiceContext 带 KafkaWriter。规格显式点名要删的只有 TableDir,
// 没说删 Kafka,所以按仓内三处先例(friend 原实现 / guild / match)保留 `Kafka.Brokers`。
// 若 config 包最终没有这个字段,改这一处即可。
func newKafkaWriter(c config.Config) *kafkago.Writer {
	if len(c.Kafka.Brokers) == 0 {
		logx.Info("[friend] Kafka.Brokers empty: S2C push disabled (friend requests / acceptance only persisted, online peer not notified)")
		return nil
	}
	return &kafkago.Writer{
		Addr: kafkago.TCP(c.Kafka.Brokers...),
		// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
		// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
		// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
		// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer。
		//
		// 其它 topic 仍走 Hash 按 Key(player_id/gate_id)选分区,保证同一实体的
		// 推送有序(项目不变量:kafka key = 业务实体 ID)。LeastBytes 忽略 Key。
		Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafkago.Hash{}},
		// kafka-go 的 RequiredAcks 零值是 RequireNone(fire-and-forget):
		// 写进 socket 即返回 nil,broker 端 leader 切换/落盘前崩溃全部不可见,
		// 于是 gate_push 依赖 WriteMessages 返回值的 fail-closed 语义形同虚设。
		// 与 scene_manager / player_locator 的 servicecontext 对齐,显式 RequireOne。
		RequiredAcks: kafkago.RequireOne,
		// Async=false 是上面那条 RequireOne 能生效的前提:Async 模式下 WriteMessages
		// 把消息塞进内部队列就返回 nil,错误只走 Completion 回调,调用方再怎么判
		// 返回值都永远是"成功"。显式写出来,不靠零值(照 match)。
		Async:        false,
		WriteTimeout: kafkaWriteTimeout,
	}
}

// BindDrainRuntime 将维护原件绑定到已经消费的同一 SQL boot，不重新读 grant。
// 实际 main 仍须把 RootAdmission 接进 RPC/sweep，把 RelayAdmission 接进发布链。
func (sc *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if runtime == nil || sc.DrainRuntime != nil || sc.RootAdmission == nil || sc.RelayAdmission == nil ||
		sc.RootAdmission == sc.RelayAdmission || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(sc.WriterConfig.InstanceID(), sc.WriterConfig.WriterKey()); err != nil {
		return err
	}
	sc.DrainRuntime = runtime
	return nil
}

// CloseSource 只关闭根门禁，等待已经计入该门禁的 RPC；不冒称 sweep/号段/摘要已收敛。
// StartSweep 尚无 join/frontier，RequestIDs.Close 取消 RPC 也不能证明服务器端结局。
// 缺这些真实 owner 原件时保持 HOLD，同 boot 的 RELAY 与 SQL 句柄继续保留。
func (sc *ServiceContext) BindSourceStop(stop func(context.Context) error) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if stop == nil || sc.sourceStop != nil || sc.sourceClosed {
		return drainproof.ErrHold
	}
	sc.sourceStop = stop
	return nil
}

func (sc *ServiceContext) CloseSource(ctx context.Context) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if sc.sourceClosed {
		return nil
	}
	if sc.RootAdmission == nil {
		return drainproof.ErrHold
	}
	sc.RootAdmission.CloseAdmission()
	if sc.sourceStop != nil {
		if err := sc.sourceStop(ctx); err != nil {
			sc.RootAdmission.Hold()
			return err
		}
	}
	if err := sc.RootAdmission.Wait(ctx); err != nil {
		return err
	}
	if sc.RequestIDs != nil {
		frontier, err := sc.RequestIDs.DrainAndReport(ctx)
		if err != nil {
			sc.RootAdmission.Hold()
			return err
		}
		if err = sc.RootAdmission.MergeClosedSettled(frontier); err != nil {
			return err
		}
	}
	sc.sourceClosed = true
	return nil
}

// friend 的发布与摘要调用同步包含在根 RPC 中；Kafka Completion 和 Evaler 保留吞掉的 I/O 错误。
// 根入口与 sweep 真 join 后才关闭 Kafka，再关闭 relay 与同 boot SQL writer。
func (sc *ServiceContext) StopAndReport(ctx context.Context) error {
	if err := sc.CloseSource(ctx); err != nil {
		return err
	}
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if sc.resourcesClosed {
		return sc.resourceCloseErr
	}
	if sc.RelayAdmission == nil || sc.RelayIngress == nil {
		return drainproof.ErrHold
	}
	sc.RelayIngress.CloseAdmission()
	if err := sc.RelayIngress.Wait(ctx); err != nil {
		return err
	}
	if err := sc.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	if sc.KafkaWriter != nil {
		if err := sc.KafkaWriter.Close(); err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
	}
	sc.RelayAdmission.CloseAdmission()
	if err := sc.RelayAdmission.Wait(ctx); err != nil {
		return err
	}
	sc.WriterConfig.Seal()
	var result error
	if sc.directoryConn != nil {
		result = errors.Join(result, sc.directoryConn.Close())
	}
	if sc.LoginSessions != nil {
		result = errors.Join(result, sc.LoginSessions.Close())
	}
	if sc.PlayerRedis != nil {
		result = errors.Join(result, sc.PlayerRedis.Close())
	}
	if sc.DB != nil {
		result = errors.Join(result, sc.DB.Close())
	}
	if sc.Etcd != nil {
		result = errors.Join(result, sc.Etcd.Close())
	}
	sc.resourcesClosed = true
	sc.resourceCloseErr = result
	return result
}

// relayDigestEvaler 复用 playersync 的实际 Eval 接缝；业务提示失败仍保持原返回语义，但维护不能遗忘它。
type relayDigestEvaler struct {
	next  playersync.Evaler
	relay *drainproof.Session
}

func (e relayDigestEvaler) Eval(ctx context.Context, script string, keys []string, args ...string) (resultErr error) {
	finish, err := e.relay.Enter()
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			e.relay.Hold()
			panic(recovered)
		}
		if resultErr != nil {
			e.relay.Hold()
		}
	}()
	return e.next.Eval(ctx, script, keys, args...)
}

func (sc *ServiceContext) Stop() {
	if err := sc.StopAndReport(context.Background()); err != nil {
		logx.Errorf("friend 排空 HOLD，保留同 boot SQL/RELAY 资源: %v", err)
	}
}
