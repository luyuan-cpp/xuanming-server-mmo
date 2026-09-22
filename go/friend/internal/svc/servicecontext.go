// Package svc 是 friend 服务的依赖装配:etcd 客户端、mmorpg_friend 连接池、
// FriendRedis / SharedRedis 双句柄,以及 S2C 推送用的 Kafka writer。
//
// 与 trade / chat 的 svc 包刻意不同的一点:本包**不含** Prometheus 指标。
// friend 的指标在 friend/internal/metrics 里(懒注册 + Start(addr),由 main 调用),
// 本包一行都不 import 它 —— 否则 svc 与 metrics 双向牵引,将来 metrics 想记
// "Redis 回落"这类装配期事件就会成环。
package svc

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"friend/internal/config"
	friendkafka "friend/internal/kafka"

	"shared/kafkacmd"
	"shared/kafkautil"

	// database/sql 只认注册过的驱动名;这个空导入就是注册 "mysql" 的唯一动作,删了它
	// sql.Open("mysql", ...) 会在运行期报 unknown driver,而编译期一声不响。
	_ "github.com/go-sql-driver/mysql"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
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
	Config config.Config

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

	// 双存储(契约 §4):
	//   - SharedRedis:跨运行时契约 key(`player:session:{id}`)的**唯一句柄**,禁 Cluster。
	//     这条 key 的写者是 C++ / login,C++ 侧没有集群客户端,所以它永远指向既有共享库;
	//     friend 靠它判在线(F2 批的推送路径)。
	//   - FriendRedis:friend 私有 key(好友列表缓存、申请配额……),可指向 Redis Cluster。
	// FriendRedis 未配置时两者是同一个句柄(本地单库形态),见 NewRedisHandles。
	//
	// 两个句柄都是 go-zero core/stores/redis 的 *redis.Redis(照 chat / match / trade),
	// 不再直接用 github.com/redis/go-redis/v9:同进程里两套客户端 = 两套连接池、
	// 两套超时与断路器语义,排障时对不上账。
	SharedRedis *redis.Redis
	FriendRedis *redis.Redis

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

	stopOnce sync.Once
}

// NewServiceContext 建全部依赖。任何一项建不出来都 panic(启动致命,照 trade):
// 没有 etcd 的 friend 注册不了、被路由服发现不到;没有 mmorpg_friend 的 friend
// 每个请求都只能回 kServiceUnavailable。这种进程活着但全量失败的形态比起不来更难查,
// 所以在启动期就把它变成崩溃 + 日志。
func NewServiceContext(c config.Config) *ServiceContext {
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: etcdDialTimeout,
	})
	if err != nil {
		panic("friend: failed to create etcd client: " + err.Error())
	}

	db, err := OpenMySQL(c.MySQL)
	if err != nil {
		panic(fmt.Errorf("friend: %w", err))
	}

	friendRds, sharedRds, target := NewRedisHandles(c)

	return &ServiceContext{
		Config:             c,
		Etcd:               etcdCli,
		DB:                 db,
		SharedRedis:        sharedRds,
		FriendRedis:        friendRds,
		FriendRedisTarget:  target,
		KafkaWriter:        newKafkaWriter(c),
		GateCommandBuilder: friendkafka.NewGateCommandBuilder(),
	}
}

// NewRedisHandles 按配置建立 (FriendRedis, SharedRedis) 两个句柄,并返回 FriendRedis 落点描述。
// FriendRedis.Host 为空即未配置,私有 key 回落到共享库,两者返回**同一个**句柄。
// 拆成独立函数是为了不依赖 etcd / MySQL 就能测回落逻辑(与 chat / match 的同名函数同形)。
func NewRedisHandles(c config.Config) (friendRds, sharedRds *redis.Redis, target string) {
	// 共享库无条件建:它是 `player:session:{id}` 的唯一句柄,也是 FriendRedis 的回落目标,
	// 所以 config.Validate 把 `Redis.Host == ""` 当配置错误拒掉,这里不用再判空。
	sharedRds = redis.MustNewRedis(c.Redis.RedisConf)
	if c.FriendRedis.Host == "" {
		// go-zero 的 logx 没有 Warn 级别;用 Error 级打出来,保证在默认日志级别下一定可见
		// (Info 级在 staging/prod 常被调掉,这条正是那里最需要看到的)。
		// 回落本身不是故障(本地单库形态就是这样),但在 staging/prod 出现就是部署缺陷:
		// 共享库是 allkeys-lfu,好友缓存会被当缓存淘汰,还要和契约 key 抢内存 ——
		// 而契约 key 被淘汰的后果是"玩家明明在线却判成离线",推送静默丢。
		logx.Errorf("[friend] WARN FriendRedis 未配置,friend 私有 key 回落到共享库 host=%s type=%s —— "+
			"仅限本地单库形态;staging/prod 必须配置独立且 maxmemory-policy=noeviction 的 FriendRedis(契约 §4)",
			c.Redis.Host, c.Redis.Type)
		return sharedRds, sharedRds, fmt.Sprintf("shared-fallback host=%s type=%s", c.Redis.Host, c.Redis.Type)
	}
	friendRds = redis.MustNewRedis(c.FriendRedis)
	logx.Infof("[friend] FriendRedis 独立配置生效 host=%s type=%s(共享库 host=%s 仅作契约 key 句柄)",
		c.FriendRedis.Host, c.FriendRedis.Type, c.Redis.Host)
	return friendRds, sharedRds, fmt.Sprintf("FriendRedis host=%s type=%s", c.FriendRedis.Host, c.FriendRedis.Type)
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
// **事务之外**的自动提交语句 —— Unblock 的 DELETE、sweep / 回收的逐行 DELETE、ensure 的 INSERT ——
// 它们原先落在服务器全局默认的 REPEATABLE-READ 上,会拿间隙锁 / next-key 锁:
//   - 排队中的间隙锁请求会挡住别人的插入意向锁,是 RR 下经典的插入死锁源;
//   - ensure 的"主键重复 → ODKU 取 X"在 RR 下拿的是 next-key,两个并发 ensure 撞上刚被回收的行时
//     仍可能互等(2026-09-21 死锁事故的后续,docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md)。
// RC 下只剩记录锁,friend 全部锁定语句又都是完整主键点查 / 点更新,于是不存在可成环的间隙。
// 自动提交语句每条各取新快照,读语义与 RR 下相同,不影响任何业务判定。
//
// 返回值**含密码**,只能交给 sql.Open,绝不打日志、绝不进错误信息(横幅用 MySQLTarget)。
func BuildDSN(c config.MySQLConf) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&sql_mode=%%27STRICT_TRANS_TABLES%%27"+
		"&transaction_isolation=%%27READ-COMMITTED%%27",
		c.User, c.Password, c.Host, c.DBName)
}

// MySQLTarget 是连接目标的可读描述(**不含密码**),供启动横幅与错误信息使用。
func MySQLTarget(c config.MySQLConf) string {
	return fmt.Sprintf("%s/%s (user=%s)", c.Host, c.DBName, c.User)
}

// OpenMySQL 建池、设上限、ping。返回错误时池已关闭(否则 sql.DB 会带着后台
// connectionOpener goroutine 泄漏在一个注定要退出的进程里)。
// 错误信息点名库名:友好度不是重点,重点是 `-migrate` 以 1 退出时运维能直接看出
// 是"库不存在"而不是"密码错"(D-14 第 5 条)。
func OpenMySQL(c config.MySQLConf) (*sql.DB, error) {
	db, err := sql.Open("mysql", BuildDSN(c))
	if err != nil {
		return nil, fmt.Errorf("open MySQL %s: %w", MySQLTarget(c), err)
	}
	// 上限必须显式设:database/sql 的 MaxOpenConns 零值是"不限",
	// 一次慢查询风暴就能把 MySQL 的 max_connections 吃光、连带打死同库的别人。
	db.SetMaxOpenConns(c.MaxOpenConn)
	db.SetMaxIdleConns(c.MaxIdleConn)

	ctx, cancel := context.WithTimeout(context.Background(), mysqlPingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping MySQL %s 失败(库 %s 不存在时先按 deploy/mysql-init/00_init_zone_dbs.sql 建库并授权): %w",
			MySQLTarget(c), c.DBName, err)
	}
	return db, nil
}

// newKafkaWriter 建 S2C 推送用的 writer;没配 broker 返回 nil(见 ServiceContext.KafkaWriter 注释)。
//
// ⚠ 这里是本文件唯一读 Kafka 配置的地方:冻结规格 §3.1 列的 Config 结构里没有 Kafka 段,
// 但 §3.6 要求 ServiceContext 带 KafkaWriter。规格显式点名要删的只有 TableDir,
// 没说删 Kafka,所以按仓内三处先例(friend 原实现 / guild / match)保留 `Kafka.Brokers`。
// 若 config 包最终没有这个字段,改这一处即可。
func newKafkaWriter(c config.Config) *kafkago.Writer {
	if len(c.Kafka.Brokers) == 0 {
		logx.Info("[friend] Kafka.Brokers 为空:S2C 推送出口关闭(好友申请/同意只落库,不通知在线对端)")
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

// Stop 依次关 Kafka writer(flush 在途批次)、DB、etcd。幂等,正常 defer 与
// 异常强退路径可安全共用。
//
// **不关 Redis 句柄**,两条理由:
//  1. go-zero 的 redis.Redis 没有 Close —— 连接池由框架持有并复用,本来就不该由业务关;
//  2. FriendRedis 回落时它与 SharedRedis 是**同一个实例**,即使有 Close 也会被关两次
//     (二次释放)。
//
// 顺序不能改:Kafka 先关才能 flush 掉在途推送;etcd 必须最后关,因为
// noderegistry 与 killswitch 共用这条连接。调用方须保证 noderegistry.Close()
// (注销)与 killswitch 的 ctx 取消都**先于**本函数 —— 先关连接会让注销 Txn 失败
// (节点键留在 etcd 里等租约过期,这段时间路由服还在往死进程拨号),
// 并让 watch 撞上已关闭的 client。
func (sc *ServiceContext) Stop() {
	sc.stopOnce.Do(func() {
		if sc.KafkaWriter != nil {
			if err := sc.KafkaWriter.Close(); err != nil {
				logx.Errorf("[friend] Kafka writer close/flush failed: %v", err)
			}
		}
		if sc.DB != nil {
			if err := sc.DB.Close(); err != nil {
				logx.Errorf("[friend] MySQL close failed: %v", err)
			}
		}
		if sc.Etcd != nil {
			if err := sc.Etcd.Close(); err != nil {
				logx.Errorf("[friend] etcd client close failed: %v", err)
			}
		}
	})
}
