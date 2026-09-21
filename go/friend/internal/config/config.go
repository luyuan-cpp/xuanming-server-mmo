// Package config 是 friend 服务的配置结构与启动期校验。
//
// 键名与 etc/friend.yaml、tools/scripts/k8s_deploy.ps1 的 friend ConfigMap 三处逐字一致;
// 改名必须三处同改,否则 ConfigMap 里的值会被 go-zero 当未知键静默忽略 —— 这类事故没有任何
// 报错,只会表现为"线上跑的还是默认值"。
//
// friend 是全局服务(全服一份、多副本、进程无状态,契约 §2):ZoneId 只影响 etcd 注册路径,
// 业务代码禁止读它做分支。
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"friend/internal/data"

	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// MaxRpcTimeoutMs 是 zrpc 服务端整体超时的上限(毫秒)。
//
// 契约 §3 超时预算:业务服务 Timeout ≤ 路由服 ForwardTimeoutMs(默认 5000)− 1000。
// 为什么必须留这 1000ms:friend 要先于路由服超时把 in-band 结果回给路由服。反过来路由服先超时,
// 会把"friend 的正常慢响应"判成信封级 kServiceUnavailable,而 friend 这边的副作用(好友边已落库、
// 申请已写入)仍然生效 —— 客户端看到失败再点一次,就是重复申请 / 重复关系。
const MaxRpcTimeoutMs int64 = 4000

// MinRpcTimeoutMs 是 Timeout 的下限(毫秒):扣掉 InBandReplyReserve 后至少还要剩 500ms 给 I/O。
// 没有下限时 Timeout=100 这种配置能加载成功,但 RequestBudget 直接为负,所有 MySQL / Redis 调用
// 在进 handler 的第一行就已经过期,整个服务"能起来、全部请求失败"。
const MinRpcTimeoutMs int64 = 1000

// InBandReplyReserve 是整请求业务预算相对 zrpc 服务端 Timeout 预留的余量。
//
// go-zero 的 UnaryTimeoutInterceptor 挂在本服务拦截器链**外层**,到 Timeout 就直接回 gRPC
// DeadlineExceeded,handler 之后产出的 in-band kServiceUnavailable 会被丢掉。所以一次请求里的
// 全部 I/O 共用一个更早的截止时间 = Timeout − 本余量:依赖变慢时 I/O 先因预算到期失败,handler
// 走故障分支 in-band 返回,赶在拦截器之前。500ms 覆盖拦截器链开销、驱动响应取消、组包与日志。
const InBandReplyReserve = 500 * time.Millisecond

// Sweep.Mode 的两个合法取值,见 SweepConf.Mode。
const (
	// SweepModeReportOnly:只统计待清理行数(指标 friend_sweep_pending_rows),不删数据。
	SweepModeReportOnly = "report_only"
	// SweepModeDelete:真删过期的 rejected / accepted 申请行。
	SweepModeDelete = "delete"
)

// Config 是 friend 服务的全部配置。
//
// 共享 Redis 库**不在这里重新声明**:嵌入的 zrpc.RpcServerConf 已经带了
// `Redis redis.RedisKeyConf`(yaml 顶层 `Redis:`),再声明一个同名字段会让 go-zero conf 在加载期
// 报 "conflict key Redis"(core/conf/config.go addOrMergeFields)。共享库一律用 c.Redis.RedisConf
// —— 与 chat / match / trade 同一写法。
type Config struct {
	zrpc.RpcServerConf

	// ZoneId:只影响 etcd 注册路径 <Prefix>/zone/<z>/...(路由服按 NodeInfo 发现本服务)。
	// 必填且拒 0(契约 §2)。刻意不给 default:漏配就在 Validate 里 fail-fast,而不是悄悄注册进
	// zone 1 —— 那会让 zone 2 的玩家请求被路由到 zone 1 的注册项上,排障时看不出任何异常。
	// friend 是全局服务,业务代码**禁止**读它做分支(契约 §0)。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL:etcd 节点注册租约 TTL(秒)。它就是崩溃场景的切换窗口 —— 路由服 PickRandom
	// 不看连接状态,进程崩掉后最多这么久还会被选中(契约 §2)。
	LeaseTTL int64 `json:",default=60"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。friend 用 :9180(契约 §7)。
	// 必须是 yaml 顶层单行:go_services.ps1 -Zone 的端口位移正则只认这个形状。
	MetricsListenAddr string `json:",optional"`

	// KillSwitchPrefix:RPC 级热关停规则(shared/killswitch)在 etcd 里的前缀,
	// 留空用默认前缀 /mmorpg/killswitch/,且前缀下没有 key 时全部放行(fail-open)。
	KillSwitchPrefix string `json:",optional"`

	// MySQL:独占库 mmorpg_friend 的连接参数(D-14)。用结构化字段而不是一整串 DSN:
	// K8s 的用户名与密码分开注入,DSN 由 svc.BuildDSN 统一拼(带严格模式,见那边注释)。
	MySQL MySQLConf `json:"MySQL"`

	// Schema:建表策略,见 SchemaConf.AutoMigrate。整段可缺失。
	Schema SchemaConf `json:",optional"`

	// FriendRedis:friend 私有 key 的存储,可配 Type: cluster(所有私有 key 都是单 key 操作,
	// 集群安全,契约 §4)。留空则回落到共享库 c.Redis.RedisConf,启动时打 WARN 并在横幅打印落点。
	// ⚠ staging/prod 必须指向独立实例且 maxmemory-policy=noeviction:共享库是 allkeys-lfu,
	// 好友列表缓存被淘汰只是多打一次 MySQL,但配额 / 限流 key 被淘汰等于限流静默失效。
	//
	// 类型用 go-zero 的 core/stores/redis.RedisConf(不是 redis/go-redis/v9 的 Options):
	// 全仓 Go 服务统一走 go-zero 句柄,回落逻辑才能与 chat / match 复用同一套写法。
	FriendRedis redis.RedisConf `json:",optional"`

	// Kafka:S2C 推送(kafkautil.PushToPlayer → gate-cmd_g<N>)的 broker 地址。
	//
	// ⚠ 本字段不在 F1 规格 §3.1 的结构清单里,但 §3.6 要求 svc 继续按现有写法建
	// kafkago.Writer(`kafkago.TCP(c.Kafka.Brokers...)`),没有它装配不出来。保留现有段名与形状,
	// 与 guild 的 KafkaConf 逐字一致。
	// Brokers 为空是合法配置:svc 会让 KafkaWriter 保持 nil,推送路径降级为"不推"
	// (推送本身是 at-most-once,契约 §5),所以 Validate 不拒绝空 Brokers。
	Kafka KafkaConf `json:"Kafka"`

	// Friend:好友业务的各项阈值与 sweep 参数。刻意不标 optional。
	//
	// go-zero 对"非 optional 但整段缺失"的嵌套结构的处理是:内层字段全都带 default / optional 时
	// 按空 map 下钻并回填 default(v1.10.0 mapping/unmarshaler.go processNamedFieldWithoutValue)。
	// 所以漏写整个 Friend 段**不会**报错、也不会全变 0,而是悄悄套用一整套默认阈值 ——
	// 唯一露出来的破口是内层 Sweep(它标了 optional,不被下钻,Mode 留空串),再由 Validate 拒掉。
	// 结论:etc/friend.yaml 与 ConfigMap 必须写全本段,别指望 go-zero 报缺段。
	Friend FriendConf `json:"Friend"`
}

// MySQLConf 是 mmorpg_friend 的连接参数。
type MySQLConf struct {
	Host string `json:"Host"`
	User string `json:"User"`
	// Password 可以为空(本地免密账号);值只进 DSN,绝不打日志、不进启动横幅。
	Password    string `json:"Password,optional"`
	DBName      string `json:"DBName"`
	MaxOpenConn int    `json:"MaxOpenConn,default=20"`
	MaxIdleConn int    `json:"MaxIdleConn,default=5"`
}

// KafkaConf 是控制面 / 推送 topic 的 broker 地址。形状与 go/guild/internal/config 的同名结构一致。
type KafkaConf struct {
	Brokers []string `json:"Brokers,optional"`
}

// SchemaConf 建表策略(D-14 第 4 条)。
type SchemaConf struct {
	// AutoMigrate 为 nil(没写)或 true:启动时跑 schemamigrate.Up;false:启动时只跑只读 Plan,
	// 发现待执行语句或需人工项即拒绝启动并打印补救命令。
	//
	// 用 *bool 而不是 `bool json:",default=true"`:go-zero 不下钻一个整段 optional 且未出现的
	// 嵌套结构,default 标签不回填,裸 bool 会让"没写 Schema 段"变成 false(data_service 踩过
	// 这个坑 —— 表一张没建,服务却按"人工迁移模式"拒启)。K8s 非 dev 档由 ConfigMap 显式写 false。
	AutoMigrate *bool `json:",optional"`
}

// FriendConf 是好友业务的阈值。
//
// 这些数字全是**硬上限与限流**,不是调优旋钮:写大了会让单个玩家的好友列表 / 拉黑表无界增长
// (一次 GetFriendList 要回几万行),写小了玩家加不上好友。改动必须同步 k8s ConfigMap。
type FriendConf struct {
	// MaxFriends:单玩家好友数硬上限(friend_capacity.friend_count 的封顶值)。
	MaxFriends uint32 `json:",default=200"`

	// MaxPendingRequests:单玩家**出站**(自己发出、对方未处理)的 pending 申请上限。
	// 它挡的是"一个人给全服发好友申请"。
	MaxPendingRequests uint32 `json:",default=50"`

	// MaxIncomingRequests:单玩家**入站**(别人发给自己、自己未处理)的 pending 申请上限。
	// 与出站分开两个值:入站量不由本人控制,共用一个上限会让被骚扰者先被自己的收件箱打满,
	// 反而挡住正常好友的申请。
	MaxIncomingRequests uint32 `json:",default=200"`

	// MaxBlocks:单玩家黑名单条数上限。拉黑表进 ListBlocks 的全量返回,必须有封顶。
	MaxBlocks uint32 `json:",default=200"`

	// RecommendDefaultLimit:RecommendFriends 请求 limit=0 时返回多少个候选。
	RecommendDefaultLimit uint32 `json:",default=10"`

	// RecommendMaxLimit:RecommendFriends 的 limit 上限,超出钳到它。
	// 推荐要算二度关系,每个候选都是额外的图查询 —— 上限就是单请求最坏开销的封顶。
	RecommendMaxLimit uint32 `json:",default=20"`

	// RecommendMaxExclude:RecommendFriends 请求里 exclude_player_ids 的条数上限。
	// 客户端可控的数组必须封顶,否则一次请求能塞十万个 id 让服务端做十万次集合运算。
	RecommendMaxExclude uint32 `json:",default=64"`

	// RequestQuotaPerMinute:单玩家每分钟最多发起几次好友申请(滑窗配额)。
	// 与 MaxPendingRequests 互补:后者限"同时挂着多少",前者限"多快发"——
	// 只有后者时,刷子可以用"发一条、立刻撤一条"绕过去。
	RequestQuotaPerMinute uint32 `json:",default=10"`

	// ListReadHardLimit:列表类读(好友列表 / 申请列表 / 黑名单)SQL 上的 LIMIT 硬上限。
	// 即使容量表被写坏、真实行数超过上限,单次读也不会拖垮 MySQL 与网关包体。
	ListReadHardLimit uint32 `json:",default=1000"`

	// CacheTTL:friend 私有 Redis 缓存(好友列表等)的过期时间。
	// 必须为正:go-redis / go-zero 的 Setex 语义下 TTL≤0 等于"永不过期",缓存一旦与 MySQL
	// 不一致就再也不会自愈(玩家删了好友仍在列表里,只能靠重启 Redis 修)。
	CacheTTL time.Duration `json:",default=30m"`

	// Sweep:过期申请行的后台清理参数,见 SweepConf。
	Sweep SweepConf `json:",optional"`
}

// SweepConf 是 friend_request 表过期行的后台清理参数(F2 批实现循环,F1 只定契约)。
//
// ⚠ 整段标 optional 只为让"没写 Sweep 段"落到 Validate 给出可读错误。go-zero 不下钻一个
// 整段 optional 且未出现的嵌套结构,default 标签**不会**回填(同 SchemaConf.AutoMigrate 的坑),
// 所以 etc/friend.yaml 与 K8s ConfigMap 都必须显式写全这四个键。
//
// **同一份参数也用于 friend_capacity 零好友行的回收**(为什么要回收见 logic/sweep.go 文件头):
// Mode 决定那一段删不删,RetentionDays 是容量行建出后至少保留多久,BatchLimit 是单轮回收行数上限。
// 刻意不为它新增配置键(KISS):两段是同一个后台任务、同一个"先观察再真删"的开关,分成两套键
// 只会出现"一段 delete、一段忘了改"的半开状态;而且 K8s ConfigMap 的 22 个契约值也不用跟着改。
type SweepConf struct {
	// Mode:report_only 只出指标不删数据;delete 真删。默认 report_only ——
	// 清理的是权威数据,新环境先跑观察模式确认"待清理行数"符合预期,再改 delete。
	// 对 friend_capacity 零好友行的回收同样生效(指标 friend_sweep_idle_capacity_rows)。
	Mode string `json:",default=report_only,options=report_only|delete"`

	// Interval:两轮 sweep 之间的间隔。
	Interval time.Duration `json:",default=5m"`

	// RetentionDays:已处理(rejected / accepted)的申请行保留天数。
	// 保留期存在的意义是让玩家还能看到"最近谁拒了我";删太早等于这段历史凭空消失。
	// 同一个值也是 friend_capacity 零好友行的保留天数(按 created_ms 算):留够这么久再回收,
	// 是为了不去删一条刚建出来、正要被写事务拿去当守卫的行。
	RetentionDays int `json:",default=7"`

	// BatchLimit:单轮 sweep 一次 DELETE 的行数上限。分批是为了不让一条长事务
	// 长时间持锁 friend_request(它同时在好友申请写路径上)。
	// 同一个值也是单轮回收 friend_capacity 零好友行的行数上限(那一段是逐行按主键删,
	// 不是一条批量 DELETE,理由见 data/sweep_repo.go)。
	BatchLimit int `json:",default=1000"`
}

// RequestBudget 返回一次请求内全部 I/O(MySQL、Redis、Kafka 投递)共用的业务预算。
// 单次调用另有各自的上限,两者取先到者;串行多次调用的总和由本预算封顶。
// Validate 保证 Timeout ≥ MinRpcTimeoutMs,所以结果恒为正。
func (c Config) RequestBudget() time.Duration {
	return time.Duration(c.Timeout)*time.Millisecond - InBandReplyReserve
}

// ShouldAutoMigrate 是启动路径跑 Up 还是只跑 Plan 的唯一判据:没写 = Up。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// ListenHostPort 把 ListenOn 拆成 host 与端口号。用 net.SplitHostPort 以正确处理 "[::]:50400"。
// noderegistry 通告 IP 时要拿这里的 host 判断是不是 0.0.0.0 / ::(见 friend.go 的 advertisedHost)。
func (c *Config) ListenHostPort() (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(c.ListenOn)
	if err != nil {
		return "", 0, fmt.Errorf("ListenOn 必须是 host:port,得到 %q: %w", c.ListenOn, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("ListenOn 端口非法: %q", c.ListenOn)
	}
	return host, uint32(port), nil
}

// IsRelaxedMode 报告 go-zero Mode 是否为 dev / test(与 go/login/internal/config/secrets.go、
// go/trade/internal/config 同一判据)。go-zero 的 Mode 默认是 pro,只在这两档放开的调试入口
// (GM / 冒烟用的构造数据接口)必须用它判,不能各自比较字符串。
func IsRelaxedMode(mode string) bool {
	return mode == service.DevMode || mode == service.TestMode
}

// Validate 做启动期整体校验(fail-fast)。conf.MustLoad / conf.Load 会自动调用它(go-zero v1.10.0
// core/conf/config.go 里的 validate(v))。
//
// 每一条拒绝的都是"进程能起来、但行为静默出错"的配置 —— 逐条理由写在各分支旁边。
func (c *Config) Validate() error {
	// ZoneId=0:注册路径变成 <Prefix>/zone/0/...,路由服按 zone 过滤时看不见本节点,
	// 表现为"服务起来了、客户端却说好友功能不可用",日志里一个错都没有。
	if c.ZoneId == 0 {
		return errors.New("ZoneId 必须非 0(K8s 全局服务用 ConfigMap 注入的 CurrentZoneId)")
	}
	// ListenOn 为空 / 非 host:port:注册进 etcd 的 Endpoint 拨不通,路由服每次转发都失败。
	if c.ListenOn == "" {
		return errors.New("ListenOn 不能为空")
	}
	if _, _, err := c.ListenHostPort(); err != nil {
		return err
	}
	// Timeout 越界:上限见 MaxRpcTimeoutMs(路由服先超时 = 副作用生效但客户端看到失败);
	// 下限见 MinRpcTimeoutMs(RequestBudget 非正 = 所有 I/O 一进 handler 就过期)。
	// 注意 go-zero 里 Timeout=0 表示"不限时",在这里同样是错的,所以用 < 下限一并拒掉。
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 [%d, %d] 内:上限是路由服 ForwardTimeoutMs − 1000(契约 §3),"+
			"下限保证扣掉 %v 回包余量后仍有 I/O 预算", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs, InBandReplyReserve)
	}
	// Etcd.Hosts 为空:节点注册与 killswitch 都没有 etcd 可用。killswitch 是 fail-open 的,
	// 所以这种情况下"热关停开关全部失效"也不会报错。
	if len(c.Etcd.Hosts) == 0 {
		return errors.New("Etcd.Hosts 不能为空(节点注册与 killswitch 都依赖 etcd)")
	}
	// Etcd.Key 非空:go-zero 会**额外**把本服务注册成 friend.rpc 这个 key,并被
	// go_services.ps1 -Zone 按 .z<N> 切分 —— 全局服务被切成每 zone 一套负载均衡集,
	// 与 noderegistry 的全服一份语义直接冲突(契约 D-13)。
	// ⚠ yaml 里必须显式写 `Key: ""` 而不是整行省略:go-zero 的 discov.EtcdConf.Key 不是
	// optional,Etcd 段存在而缺 Key 会在 conf 加载期直接 Fatal、K8s Pod CrashLoop。
	if c.Etcd.Key != "" {
		return fmt.Errorf("Etcd.Key 必须留空(得到 %q):全局服务不注册 go-zero key,契约 D-13", c.Etcd.Key)
	}
	// LeaseTTL ≤ 0:租约建不出来(etcd 拒绝非正 TTL),注册直接失败;友好地在加载期报出来。
	if c.LeaseTTL <= 0 {
		return fmt.Errorf("LeaseTTL 必须为正数,得到 %d", c.LeaseTTL)
	}

	// MySQL 缺 Host/User/DBName:sql.Open 不做连接,错误要到第一次 Ping / 查询才出现。
	if c.MySQL.Host == "" || c.MySQL.User == "" || c.MySQL.DBName == "" {
		return errors.New("MySQL.Host / MySQL.User / MySQL.DBName 都不能为空")
	}
	// DBName 不是 mmorpg_friend:schemamigrate 会把 friend 的四张表建进别的服务的库,
	// 而建表本身会成功 —— 这是"每服务一库"(D-14 第 3 条)的第一道防线。
	// 常量取自 internal/data,单向依赖:data 包**不能**反向 import config,否则循环依赖。
	if c.MySQL.DBName != data.DatabaseName {
		return fmt.Errorf("MySQL.DBName 必须是 %q(得到 %q):friend 独占该库,表不许建进别的库(D-14)",
			data.DatabaseName, c.MySQL.DBName)
	}
	// 连接池上限 ≤ 0:database/sql 里 MaxOpenConn=0 是"不限连接数",高并发下能把 MySQL
	// 的 max_connections 打满,把同一实例上其它服务一起拖死;MaxIdleConn=0 则每次查询都新建连接。
	if c.MySQL.MaxOpenConn <= 0 || c.MySQL.MaxIdleConn <= 0 {
		return fmt.Errorf("MySQL.MaxOpenConn(%d)与 MySQL.MaxIdleConn(%d)必须为正数",
			c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn)
	}

	// 共享库 Host 为空:它既是 FriendRedis 未配置时的回落目标,也是读跨运行时契约 key
	// player:session:{id}(判好友在不在线)的唯一句柄(契约 §4)。缺它 = 回落落到空地址,
	// 所有私有 key 操作与在线判定都失败。
	if c.Redis.Host == "" {
		return errors.New("Redis.Host(共享库)不能为空:FriendRedis 未配置时私有 key 回落到它," +
			"且它是读契约 key player:session:{id} 的唯一句柄")
	}

	// Friend 各阈值为 0 都是"功能静默变形"而不是报错:MaxFriends=0 让谁都加不上好友;
	// RequestQuotaPerMinute=0 让所有申请被限流挡掉;ListReadHardLimit=0 让列表读恒返回空;
	// RecommendMaxExclude=0 让客户端传任何 exclude 都被拒。
	positives := []struct {
		name  string
		value uint32
	}{
		{"Friend.MaxFriends", c.Friend.MaxFriends},
		{"Friend.MaxPendingRequests", c.Friend.MaxPendingRequests},
		{"Friend.MaxIncomingRequests", c.Friend.MaxIncomingRequests},
		{"Friend.MaxBlocks", c.Friend.MaxBlocks},
		{"Friend.RecommendDefaultLimit", c.Friend.RecommendDefaultLimit},
		{"Friend.RecommendMaxLimit", c.Friend.RecommendMaxLimit},
		{"Friend.RecommendMaxExclude", c.Friend.RecommendMaxExclude},
		{"Friend.RequestQuotaPerMinute", c.Friend.RequestQuotaPerMinute},
		{"Friend.ListReadHardLimit", c.Friend.ListReadHardLimit},
	}
	for _, p := range positives {
		if p.value == 0 {
			return fmt.Errorf("%s 必须为正数", p.name)
		}
	}
	// 默认值大于上限:limit=0 的请求会拿到一个超过上限的页长,钳制逻辑形同虚设。
	if c.Friend.RecommendDefaultLimit > c.Friend.RecommendMaxLimit {
		return fmt.Errorf("Friend.RecommendDefaultLimit(%d)不能大于 Friend.RecommendMaxLimit(%d)",
			c.Friend.RecommendDefaultLimit, c.Friend.RecommendMaxLimit)
	}
	// 推荐上限本身还有一层硬天花板:单次推荐要算二度关系,20 是压测确认能在
	// RequestBudget 内跑完的最大值。配成 200 不会报错,只会让推荐请求集体超时。
	if c.Friend.RecommendMaxLimit > recommendMaxLimitCeiling {
		return fmt.Errorf("Friend.RecommendMaxLimit(%d)不能大于 %d:单次推荐的二度关系计算开销上限",
			c.Friend.RecommendMaxLimit, recommendMaxLimitCeiling)
	}
	// CacheTTL ≤ 0 在 Redis 的 setex 语义下等于"永不过期":缓存与 MySQL 一旦不一致
	// 就再也不会自愈(删了的好友一直留在列表里),只能靠清 Redis 修。
	if c.Friend.CacheTTL <= 0 {
		return fmt.Errorf("Friend.CacheTTL 必须为正数,得到 %v:TTL≤0 等于缓存永不过期,不一致无法自愈",
			c.Friend.CacheTTL)
	}

	// Sweep.Mode 不在枚举里:最危险的是空串 —— 那说明整个 Sweep 段没写、default 没回填
	// (见 SweepConf 的包内说明),此时 Interval / RetentionDays / BatchLimit 也全是 0。
	// 把它当配置错误拒掉,比让 sweep 循环按 0 参数跑(每 0 秒删 0 行)安全得多。
	if c.Friend.Sweep.Mode != SweepModeReportOnly && c.Friend.Sweep.Mode != SweepModeDelete {
		return fmt.Errorf("Friend.Sweep.Mode 必须是 %q 或 %q,得到 %q(整段 Sweep 未写时 go-zero 不回填 default)",
			SweepModeReportOnly, SweepModeDelete, c.Friend.Sweep.Mode)
	}
	// Interval ≤ 0:time.NewTicker 会 panic;就算改成 select 轮询也变成忙循环烧 CPU。
	if c.Friend.Sweep.Interval <= 0 {
		return fmt.Errorf("Friend.Sweep.Interval 必须为正数,得到 %v", c.Friend.Sweep.Interval)
	}
	// RetentionDays ≤ 0:delete 模式下会把**刚刚**被拒 / 被接受的申请立刻删掉,
	// 玩家再也看不到"谁拒了我",且可能与正在进行的接受流程竞争同一行。
	if c.Friend.Sweep.RetentionDays <= 0 {
		return fmt.Errorf("Friend.Sweep.RetentionDays 必须为正数,得到 %d", c.Friend.Sweep.RetentionDays)
	}
	// BatchLimit ≤ 0:DELETE 不带 LIMIT,一条长事务锁住整张 friend_request,
	// 把好友申请写路径一起卡住。
	if c.Friend.Sweep.BatchLimit <= 0 {
		return fmt.Errorf("Friend.Sweep.BatchLimit 必须为正数,得到 %d", c.Friend.Sweep.BatchLimit)
	}
	return nil
}

// recommendMaxLimitCeiling 是 Friend.RecommendMaxLimit 的硬天花板(契约 F1 §3.1)。
// 不做成配置项:它不是运维旋钮,而是"单次请求最坏开销必须能装进 RequestBudget"的结论。
const recommendMaxLimitCeiling uint32 = 20
