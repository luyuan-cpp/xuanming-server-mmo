package config

import (
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/zrpc"

	"guild/internal/data"
	"shared/idsegment"
)

// MaxRpcTimeoutMs:帮会二期契约 §2,Timeout ≤ 路由服 ForwardTimeoutMs(5000)− 1000。
const MaxRpcTimeoutMs int64 = 4000

// InBandReplyReserve:整请求业务预算相对 Timeout 预留的回包余量(同 go/trade)。
const InBandReplyReserve = 500 * time.Millisecond

// HomeZoneLookupBudgetMs 镜像 logic.DefaultHomeZoneLookupTimeout(config 不能 import logic);
// guild_test.go 的 TestHomeZoneBudgetMirrorsLogic 守住两者相等。
const HomeZoneLookupBudgetMs int64 = 1500

// DataServiceRpc.Timeout 的合法区间:下限防止写 0(go-zero 把 0 当成不设客户端超时),
// 上限为契约 §2 规定的同步跨服务调用 ≤ 3000ms。
const (
	MinDataServiceRpcTimeoutMs int64 = 500
	MaxDataServiceRpcTimeoutMs int64 = 3000
)

// SchemaConf 建表策略(port-decisions D-14 §4)。AutoMigrate 为 nil(没写)或 true:启动时跑
// schemamigrate.Up;false:只跑只读 Plan,不干净就拒绝启动。用 *bool 的理由同 go/trade/internal/config.SchemaConf:
// 要把"没写"与"显式写 false"区分开。
type SchemaConf struct {
	AutoMigrate *bool `json:",optional"`
}

type Config struct {
	zrpc.RpcServerConf
	RedisClient        RedisConf    `json:"RedisClient"`
	PlayerLocatorRedis RedisConf    `json:"PlayerLocatorRedis"`
	MySQL              MySQLConf    `json:"MySQL"`
	Node               NodeConf     `json:"Node"`
	Registry           RegistryConf `json:"Registry"`
	Cache              CacheConf    `json:"Cache"`
	Kafka              KafkaConf    `json:"Kafka"`
	TableDir           string       `json:",default=../../generated/tables"`

	// MergeMarkerRedis 指向 data_service 的 **mapping Redis**(player:zone:{id} 所在的
	// 那个实例/DB),guild 只从里面读一把键:merge:in_progress:{zone}。
	// 契约见 go/data_service/internal/routing/router.go 顶部的「合服闸门」注释:
	// 键存在即该 zone 正在合服,值(JSON)与 TTL 都不解析,由 tools/merge_zone 写入与删除。
	//
	// **整段可缺失 = 闸门不生效**(CreateGuild 不做这项检查),启动时会打一条
	// "MergeMarkerRedis 未配置" 的 Info 日志。这是刻意的:guild 与 data_service 目前没有
	// 任何连线,强制它连一个新 Redis 会让所有还没配这段的环境直接起不来;而闸门本身
	// 是合服维护窗口里的加固,不是常态运行的必需品。真要在合服里用,就必须配上 ——
	// 没配的后果是维护窗口内玩家仍能在源 zone 建帮,那个公会不会被 merge_zone 搬走。
	MergeMarkerRedis MergeMarkerRedisConf `json:"MergeMarkerRedis,optional"`

	// DataServiceRpc 是 data_service 的 gRPC 客户端(etcd 发现,Key=dataservice.rpc),
	// 与 login 的 PlayerLocatorRpc / SceneManagerRpc 同一套 zrpc+etcd 写法。
	// 用于查询角色归属区(GetPlayerHomeZone)及领取 guild_id 号段(AllocateIdSegment)。
	// 配置目标即拨号,不受 IdSegment.Enabled 控制;缺失时客户端归属区查询不可用,
	// 且 IdSegment.Enabled=true 会在启动时明确拒绝。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc,optional"`
	// IdSegment 控制 guild_id 的号段发号(docs/design/node-id-overhaul-plan-20260908.md §6)。
	// 字段语义见 shared/idsegment.Conf。**没写这块 = 号段关闭 = 纯 snowflake 老路径**
	// (设计稿 §6.4「保留一个版本做回滚开关」)。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`

	// KillSwitchPrefix 是 RPC 级热关停规则在 etcd 里的前缀(shared/killswitch)。
	// 留空即用 killswitch.DefaultPrefix(/mmorpg/killswitch/)—— 这是安全默认值:
	// 前缀下没有任何 key 就是全部放行。只有多套环境共用一个 etcd 集群、需要各自
	// 独立的止血阀时才需要改它。
	KillSwitchPrefix string `json:"KillSwitchPrefix,optional"`

	// ClusterId 是部署级集群号:snowflake 17 位 worker 段的 [cluster5] 部分
	// (docs/design/node-id-overhaul-plan-20260908.md §5)。**运维按集群一次性设定,
	// 策划不碰**;默认 0 = 单集群 / 存量 id 布局。etcd 槽位前缀带 c<cluster>,两个集群
	// 共用一个 etcd 也不会撞号。必须 < 32。
	ClusterId uint32 `json:"ClusterId,default=0"`

	// SnowflakeCacheDir 是 snowflake 槽位的本地缓存目录(shared/snowflakealloc,设计稿 §3.5):
	// 启动时 etcd 不可达、且缓存里的上次水位确认在 F(2h)内,就用缓存的槽起服并后台重试
	// 注册。留空关闭。默认相对服务工作目录(go/guild/)指向仓库 run/(已 gitignore);
	// k8s 想要"etcd 不通也能起"就挂一个持久卷进来,写失败只告警不影响正确性。
	SnowflakeCacheDir string `json:"SnowflakeCacheDir,default=../../run/snowflake"`

	// Schema 决定启动期是建表(Up)还是只核对(Plan)。整段不写 = Up(dev 默认)。
	Schema SchemaConf `json:",optional"`

	// AssetOp 是帮会经济(B5b)的资产通道开关、重投循环参数与清理参数,见 AssetOpConf。
	// **整段缺失 = 资产通道关闭**(fail-closed,不是"用默认值打开")。
	AssetOp AssetOpConf `json:"AssetOp,optional"`
}

// AssetOpConf 是帮会资产通道(docs/design/guild-phase2/05-economy.md §5.29,
// 循环参数按 90-consistency.md Y-06)的配置。
//
// # 为什么 Enabled 默认 false 且"整段缺失 = 关"
//
// go-zero 不下钻一个整段 optional 且未出现的嵌套结构(同 idsegment.Conf 的说明),所以没写 AssetOp 段时
// 所有字段都是零值、Enabled=false —— 这正是想要的**代码级默认拒绝**:08-save-owner-fence.md §8.3
// 规定"门禁未满足的环境不得开启帮会资产操作",一份配置被抄到预发 / 生产时少抄这一段,结果应当是
// "通道关着"而不是"通道悄悄开着"。关闭时捐献 / 兑换在发号与建行之前就回 kGuildAssetPending,
// 升级与两个读接口照常(05 顶部裁决 D;05:923"只停重投、同步照常"已作废 —— 没有签名器就无法同步投递)。
//
// # 为什么循环参数只在 Enabled 时校验、清理参数只在 CleanupEnabled 时校验
//
// 整段缺失时这些字段全是 0,若无条件校验,关着通道的环境反而起不来;而关着时它们根本没有消费者。
// 打开开关的同时 go-zero 会按 default 回填没写的键,所以"只写 Enabled: true"得到的是 Y-06 的默认值。
//
// **本段不含密钥**:签名密钥只从环境变量 MMORPG_ASSET_OP_SECRET_GUILD 读(svc.AssetOpSecretEnv),
// 缺失或短于 32 字节即拒启。
type AssetOpConf struct {
	// Enabled 打开资产通道:建签名器、scene 节点镜像、重投循环,捐献 / 兑换才会生成新指令。
	Enabled bool `json:",optional"`

	// ── 重投循环(assetop.LoopConfig,Y-06;Validate 与 assetop.NewLoop 的约束同形)──

	// ReconcileIntervalMs 两次 Tick 的间隔。
	ReconcileIntervalMs int `json:",default=2000"`
	// ReconcileBatch 一次 Tick 最多领多少行;必须 ≥ Workers,否则多出来的 worker 永远闲着。
	ReconcileBatch int `json:",default=100"`
	// Workers 是**每个副本**同时在途的资产 RPC 上限。scene 的资产 RPC 是同步 gRPC,整个 scene 进程的
	// sync poller 默认只有 8 条,调大会把 scene 的进场 / 开战请求挤在后面(assetop.LoopConfig.Workers 注释)。
	// 副本数 × Workers 才是 scene 看到的并发,扩副本时连带复核。
	Workers int `json:",default=8"`
	// LeaseMs 行级租约;必须 ≥ OpBudgetMs + 2000,否则一行还在处理就被别的副本领走。
	// 捐献 / 兑换插行时也用它当"提交后同步投递期间循环看不见这一行"的时长(裁决 G,不另写常量)。
	// 另有一条跨配表的约束在这里判不了:LeaseMs + ReconcileIntervalMs 必须小于 GuildRule.asset_op_deadline_seconds,
	// 否则同步投递被跳过的捐献会未扣款即被中止 —— 由 logic.ValidateAssetOpTiming 在启动期判。
	LeaseMs int `json:",default=10000"`
	// OpBudgetMs 单行处理预算,含 assetop 固定切走的 700ms 落库预留;下限 1000 保证投递至少有 300ms。
	OpBudgetMs int `json:",default=2500"`
	// MaxBackoffMs 退避封顶,也是 UNKNOWN(Alert)行的重排间隔。退避基数来自配表
	// GuildRule.asset_op_retry_base_ms,基数大于本值时启动期拒启(logic.ValidateAssetOpTiming 指名报错,
	// assetop.NewLoop 兜底)。
	MaxBackoffMs int `json:",default=60000"`
	// PoisonDelayMs payload 解不开的毒行推迟多久再看(循环算好绝对时刻传给 Store.Claim)。
	PoisonDelayMs int `json:",default=3600000"`
	// LedgerReadMinAttempts 连续几次投不出去之后才读已落盘账本(B5d 接 Ledger 之前不生效)。
	LedgerReadMinAttempts int `json:",default=3"`

	// ── 清理(goroutine guild.asset_op_cleanup,§5.22)──

	// CleanupEnabled 打开终态行 / 过期计数行清理。与 Enabled 独立:通道关着时仍可以清历史行。
	CleanupEnabled bool `json:",optional"`
	// CleanupIntervalMinutes 两轮清理的间隔(每个副本都跑,删除幂等,启动有随机初始延迟)。
	CleanupIntervalMinutes int `json:",default=10"`
	// TerminalRetentionDays 终态行保留天数。它同时是 B5d 回档检查可追溯窗口的下界
	// (07-rollback-fail-closed.md §7.4.2),调小之前先确认那边的窗口。
	TerminalRetentionDays int `json:",default=30"`
	// CounterRetentionDays 每日 / 每周计数行保留天数(只影响历史周期,当期行永不被删)。
	CounterRetentionDays int `json:",default=30"`
}

// 资产通道配置的合法区间。上下界的来历:
//   - Workers 上限 64 与 assetop 的 maxWorkers 同值;OpBudget 下限 1000 = assetop settleBudget(700)+ 300ms 投递;
//   - LeaseMs 相对 OpBudgetMs 的 2000ms 余量与 assetop 的 leaseHeadroom 同值 —— 两边分叉时
//     这里放过的配置会在 assetop.NewLoop 里拒启(svc 的 LoopConfigFrom 测试守住两者一致);
//   - 保留天数下限 7:清理删的是终态行,太短会让 B5d 回档检查、运维追查拿不到近期记录。
const (
	minReconcileIntervalMs    = 200
	maxReconcileIntervalMs    = 60000
	maxAssetOpWorkers         = 64
	maxReconcileBatch         = 1000
	minOpBudgetMs             = 1000
	maxOpBudgetMs             = 10000
	assetOpLeaseHeadroomMs    = 2000
	maxLeaseMs                = 600000
	minMaxBackoffMs           = 1000
	maxMaxBackoffMs           = 600000
	minPoisonDelayMs          = 60000
	maxPoisonDelayMs          = 86400000
	minCleanupIntervalMinutes = 1
	maxCleanupIntervalMinutes = 1440
	minRetentionDays          = 7
	maxRetentionDays          = 365
)

// validateLoop 校验重投循环参数;只在 Enabled 时调用。
func (a AssetOpConf) validateLoop() error {
	if a.ReconcileIntervalMs < minReconcileIntervalMs || a.ReconcileIntervalMs > maxReconcileIntervalMs {
		return fmt.Errorf("AssetOp.ReconcileIntervalMs(%d)必须在 [%d, %d] 内",
			a.ReconcileIntervalMs, minReconcileIntervalMs, maxReconcileIntervalMs)
	}
	if a.Workers < 1 || a.Workers > maxAssetOpWorkers {
		return fmt.Errorf("AssetOp.Workers(%d)必须在 [1, %d] 内:它是每个副本同时压向 scene 的资产 RPC 上限",
			a.Workers, maxAssetOpWorkers)
	}
	if a.ReconcileBatch < a.Workers || a.ReconcileBatch > maxReconcileBatch {
		return fmt.Errorf("AssetOp.ReconcileBatch(%d)必须在 [Workers(%d), %d] 内",
			a.ReconcileBatch, a.Workers, maxReconcileBatch)
	}
	if a.OpBudgetMs < minOpBudgetMs || a.OpBudgetMs > maxOpBudgetMs {
		return fmt.Errorf("AssetOp.OpBudgetMs(%d)必须在 [%d, %d] 内:其中 700ms 固定留给落库",
			a.OpBudgetMs, minOpBudgetMs, maxOpBudgetMs)
	}
	if a.LeaseMs < a.OpBudgetMs+assetOpLeaseHeadroomMs || a.LeaseMs > maxLeaseMs {
		return fmt.Errorf("AssetOp.LeaseMs(%d)必须在 [OpBudgetMs + %d = %d, %d] 内:租约短于单行预算会让同一行被两个副本同时投递",
			a.LeaseMs, assetOpLeaseHeadroomMs, a.OpBudgetMs+assetOpLeaseHeadroomMs, maxLeaseMs)
	}
	if a.MaxBackoffMs < minMaxBackoffMs || a.MaxBackoffMs > maxMaxBackoffMs {
		return fmt.Errorf("AssetOp.MaxBackoffMs(%d)必须在 [%d, %d] 内",
			a.MaxBackoffMs, minMaxBackoffMs, maxMaxBackoffMs)
	}
	if a.PoisonDelayMs < minPoisonDelayMs || a.PoisonDelayMs > maxPoisonDelayMs {
		return fmt.Errorf("AssetOp.PoisonDelayMs(%d)必须在 [%d, %d] 内",
			a.PoisonDelayMs, minPoisonDelayMs, maxPoisonDelayMs)
	}
	if a.LedgerReadMinAttempts < 1 {
		return fmt.Errorf("AssetOp.LedgerReadMinAttempts(%d)必须 ≥ 1", a.LedgerReadMinAttempts)
	}
	return nil
}

// validateCleanup 校验清理参数;只在 CleanupEnabled 时调用。
func (a AssetOpConf) validateCleanup() error {
	if a.CleanupIntervalMinutes < minCleanupIntervalMinutes || a.CleanupIntervalMinutes > maxCleanupIntervalMinutes {
		return fmt.Errorf("AssetOp.CleanupIntervalMinutes(%d)必须在 [%d, %d] 内",
			a.CleanupIntervalMinutes, minCleanupIntervalMinutes, maxCleanupIntervalMinutes)
	}
	if a.TerminalRetentionDays < minRetentionDays || a.TerminalRetentionDays > maxRetentionDays {
		return fmt.Errorf("AssetOp.TerminalRetentionDays(%d)必须在 [%d, %d] 内",
			a.TerminalRetentionDays, minRetentionDays, maxRetentionDays)
	}
	if a.CounterRetentionDays < minRetentionDays || a.CounterRetentionDays > maxRetentionDays {
		return fmt.Errorf("AssetOp.CounterRetentionDays(%d)必须在 [%d, %d] 内",
			a.CounterRetentionDays, minRetentionDays, maxRetentionDays)
	}
	return nil
}

// hasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标。与 svc.hasRpcTarget 同一判据
// (config 不能 import svc:svc 已经 import 本包)。
func hasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}

// ShouldAutoMigrate 是启动路径跑 Up 还是只跑 Plan 的唯一判据:没写 = Up。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// RequestBudget 是一次请求内全部 I/O(归属区查询、发号、Redis、MySQL)共用的业务预算,
// 由 guild.go 的 requestBudgetInterceptor 套到每个 handler 的 ctx 上。Validate 保证它为正,
// 且放得下一次 data_service 调用加一次归属区查询。
func (c Config) RequestBudget() time.Duration {
	return time.Duration(c.Timeout)*time.Millisecond - InBandReplyReserve
}

// Validate 由 go-zero conf.Load / MustLoad 自动调用(v1.9.2 core/conf/config.go:69,97)。
// 本方法会遮蔽嵌入的 zrpc.RpcServerConf.Validate,所以第一步必须显式调用它(Auth=true 时校验 Redis)。
// 错误文案不得包含 DataSource 原文(含口令)。
func (c *Config) Validate() error {
	if err := c.RpcServerConf.Validate(); err != nil {
		return err
	}
	if c.Timeout <= 0 || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 (0, %d] 内:上限 = 路由服 ForwardTimeoutMs(5000)− 1000",
			c.Timeout, MaxRpcTimeoutMs)
	}
	ds := c.DataServiceRpc.Timeout
	if ds < MinDataServiceRpcTimeoutMs || ds > MaxDataServiceRpcTimeoutMs {
		return fmt.Errorf("DataServiceRpc.Timeout(%d ms)必须在 [%d, %d] 内:0 = 不设客户端超时,上限见帮会二期契约 §2",
			ds, MinDataServiceRpcTimeoutMs, MaxDataServiceRpcTimeoutMs)
	}
	reserveMs := InBandReplyReserve.Milliseconds()
	if ds+HomeZoneLookupBudgetMs+reserveMs > c.Timeout {
		return fmt.Errorf("超时预算不足:DataServiceRpc.Timeout(%d)+ 归属区查询(%d)+ 回包余量(%d)= %d ms > Timeout(%d ms);"+
			"建帮最坏要串行做归属区查询和同步领号段,超出会变成 DeadlineExceeded 而不是业务 tip",
			ds, HomeZoneLookupBudgetMs, reserveMs, ds+HomeZoneLookupBudgetMs+reserveMs, c.Timeout)
	}
	// ParseDSN 的错误里可能带 DSN 片段(含口令),所以不 %w 包装原错误。
	dsn, err := mysqldriver.ParseDSN(c.MySQL.DataSource)
	if err != nil {
		return errors.New("MySQL.DataSource 无法解析(原文含口令,不打印)")
	}
	if dsn.DBName != data.DatabaseName {
		return fmt.Errorf("MySQL.DataSource 的库名必须是 %q(得到 %q):帮会表只建在独占库(port-decisions D-14)",
			data.DatabaseName, dsn.DBName)
	}

	// 资产通道(B5b)。关着时一概不查,见 AssetOpConf 的说明。
	if c.AssetOp.Enabled {
		if err := c.AssetOp.validateLoop(); err != nil {
			return err
		}
		// op_id 只由号段发(biz_tag=guild_asset_op,不设 snowflake 回退)。开着通道却没有号段,
		// 进程能起来、每一次捐献 / 兑换都回 kGuildIdGenUnavailable —— 这种"半开"必须炸在启动期。
		if !c.IdSegment.Enabled {
			return errors.New("AssetOp.Enabled=true 要求 IdSegment.Enabled=true:资产指令的 op_id 只由号段发(biz_tag=guild_asset_op)")
		}
		if !hasRpcTarget(c.DataServiceRpc) {
			return errors.New("AssetOp.Enabled=true 要求配置 DataServiceRpc(Etcd.Key / Endpoints / Target 之一):op_id 号段由 data_service.AllocateIdSegment 发")
		}
	}
	if c.AssetOp.CleanupEnabled {
		if err := c.AssetOp.validateCleanup(); err != nil {
			return err
		}
	}
	return nil
}

type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password"`
	DB       int    `json:"DB"`
}

// MergeMarkerRedisConf 是合服标记所在 Redis 的连接参数。
//
// 字段单独定义而不复用上面的 RedisConf,只为把 optional 语义钉死在类型上:
// 这三个键**全部** optional,Host 为空就是"没配",而 RedisConf 的 Host 是必填。
// 密码字段叫 Pass(与 go-zero redis.RedisConf 同名),因为运维填这段时对着的是
// data_service 的 MappingRedis 那份 yaml。
type MergeMarkerRedisConf struct {
	Host string `json:"Host,optional"`
	Pass string `json:"Pass,optional"`
	DB   int    `json:"DB,optional"`
}

// Enabled 报告合服闸门是否配置了。Host 是唯一判据:没有地址就连不上,
// 其余两项都有合法的零值(无密码、DB 0)。
func (c MergeMarkerRedisConf) Enabled() bool {
	return c.Host != ""
}

type MySQLConf struct {
	DataSource string `json:"DataSource"`
}

type NodeConf struct {
	ZoneId   uint32 `json:"ZoneId"`
	LeaseTTL int64  `json:"LeaseTTL"`
}

type RegistryConf struct {
	Etcd EtcdConf `json:"Etcd"`
}

type EtcdConf struct {
	Hosts       []string      `json:"Hosts"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

type CacheConf struct {
	DefaultTTL time.Duration `json:"DefaultTTL"`
	MaxMembers uint32        `json:"MaxMembers"`
}

type KafkaConf struct {
	Brokers []string `json:"Brokers"`
}

var AppConfig Config
