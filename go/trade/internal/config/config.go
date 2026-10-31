// Package config 是 trade 服务的配置结构与启动期校验。
//
// 键名与 etc/trade.yaml 逐字一致;tools/scripts/k8s_deploy.ps1 的 trade ConfigMap 里**写出来的键**也与这里逐字一致。
// 改名必须几处同改,否则 ConfigMap 里的值会被 go-zero 当未知键静默忽略。
//
// K8s 已镜像完整 AssetOp 恢复、唤醒与清理参数，以及 Commerce/Mall 参数。
// 生产渲染要求显式 TiDB 地址，并启用 global/TiDB/资产通道；本地 dev 可保留旧只读目录配置，
// 不能据此认定全球交易已经启用。新增配置仍须同步部署渲染，不能依赖未知键被静默忽略。
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shared/playerredis"
	"trade/internal/data"

	tradepb "proto/trade"

	"shared/assetopwake"
	"shared/authoritysql"
	"shared/currencytype"
	"shared/idsegment"

	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
)

// MaxRpcTimeoutMs 是 zrpc 服务端整体超时的上限(毫秒)。
// 契约 §3:业务服务 Timeout ≤ 路由服 ForwardTimeoutMs(默认 5000)− 1000,
// trade 必须先于路由服超时把 in-band 结果回去,否则客户端看到的是信封级 kServiceUnavailable。
const MaxRpcTimeoutMs int64 = 4000

// InBandReplyReserve 是整请求业务预算相对 zrpc 服务端 Timeout 预留的余量。
//
// go-zero 的 UnaryTimeoutInterceptor 挂在本服务拦截器链外层,到 Timeout 就直接回 gRPC DeadlineExceeded,
// handler 之后产出的 in-band kServiceUnavailable 会被丢掉(违反 P1-9)。所以一次请求里的全部 I/O
// 共用一个更早的截止时间 = Timeout − 本余量:依赖变慢时 I/O 先因预算到期失败,handler 走故障分支
// in-band 返回,赶在拦截器之前。500ms 覆盖拦截器链开销、驱动响应取消、组包与日志。
const InBandReplyReserve = 500 * time.Millisecond

// MinRpcTimeoutMs 是 Timeout 的下限:扣掉 InBandReplyReserve 后至少还有 500ms 给 I/O。
const MinRpcTimeoutMs int64 = 1000

// Market.Scope 的两个合法取值。
const (
	ScopeZone   = "zone"
	ScopeGlobal = "global"
)

// AssetOpSecretEnvPrefix 是资产通道签名密钥环境变量的固定前缀(§4.32:
// `MMORPG_ASSET_OP_SECRET_<CALLER>`,每个调用方一把)。本包只认前缀与形状,
// 完整变量名(trade 那一把)是装配层的事实源 svc.AssetOpSecretEnv,由它断言两者一致。
const AssetOpSecretEnvPrefix = "MMORPG_ASSET_OP_SECRET_"

// assetOpSecretEnvPattern 约束 AssetOp.SecretEnv 只能是"一个环境变量名",
// 顺带挡住"把密钥值贴进 SecretEnv"这一类手滑:真密钥含小写 / 连字符,过不了这个形状。
var assetOpSecretEnvPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(AssetOpSecretEnvPrefix) + `[A-Z0-9_]+$`)

// Config 是 trade 服务的全部配置。
//
// 不声明名为 Redis 的字段:嵌入的 zrpc.RpcServerConf 已带 `Redis`,同名会在加载期报
// "conflict key Redis"。trade P1 没有 Redis 数据(P1-10),yaml 不写 Redis 段。
type Config struct {
	zrpc.RpcServerConf

	// ZoneId:只影响 etcd 注册路径 <Prefix>/zone/<z>/...。必填且拒 0(契约 §2);
	// 刻意不给 default:漏配就在 Validate 里 fail-fast,而不是悄悄注册进 zone 1。
	// 业务代码禁止读它做分支(契约 §0),市场分区一律来自 data_service 的 home_zone。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL:etcd 节点注册租约 TTL(秒),即崩溃场景的切换窗口。
	LeaseTTL int64 `json:",default=60"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。trade 用 :9230。
	MetricsListenAddr string `json:",optional"`

	// KillSwitchPrefix:RPC 级热关停规则在 etcd 里的前缀,留空用 /mmorpg/killswitch/。
	KillSwitchPrefix string `json:",optional"`

	// MySQL:独占库 mmorpg_trade 的连接参数(D-14)。结构化字段而不是 DSN 串:
	// K8s 的用户名与密码分开注入,DSN 由 svc.BuildDSN 统一拼(带严格模式)。
	MySQL MySQLConf `json:"MySQL"`

	// Schema:建表策略,见 SchemaConf.AutoMigrate。整段可缺失。
	Schema SchemaConf `json:",optional"`

	// DataServiceRpc:查 home_zone、读合服映射与领号段的 data_service 客户端。不标 optional:
	// P1 的浏览 / 详情 / 收藏 / 种子都依赖它,缺这段就该在加载期失败。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc"`

	// IdSegment:listing_id 号段(biz_tag=trade_listing)。标 optional 只为让"整段没写"落到
	// Validate 给出可读的错误,而不是 go-zero 的缺键报错;Enabled 必须显式 true。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`

	// Market:市场范围、分页与收藏上限。
	Market MarketConf `json:"Market"`

	// SharedRedis:**共享单库** Redis。资产通道在这一实例上:
	//   - 没写 PlayerRedis.Addrs 时,玩家域的 player:{id}:location 也在这里(见下面的 PlayerRedis);
	//   - 订阅 scene 发布的落地唤醒频道 assetop:wake,另写两类本服务自己的键:唤醒的跨副本去重键
	//     assetop:wake:trade:{player_id}(shared/assetopwake)与维护选主锁 trade:assetop:maintenance:leader(shared/leader)。
	//
	// 字段名**不能**叫 Redis:嵌入的 zrpc.RpcServerConf 已经有一个 Redis 字段
	// (go-zero 的 RedisKeyConf,给 Auth 用),同名会在 conf.MustLoad 阶段直接报
	// "conflict key Redis" 而不是被忽略。match 的同一份契约键读取也叫 SharedRedis
	// (cross-zone-matchmaking D2),这里沿用同一个名字。
	//
	// K8s 上必须指向与 C++ scene 相同的那一份共享 Redis(不是 trade 自己的库):
	// 位置键的写者是 scene / scene_manager,读错实例 = 永远找不到人 = 资产指令永远 NOT_HERE。
	//
	// 只有 AssetOp.Enabled=true 时才会真的去拨它(装配层判);关着时这一段不被使用,
	// 但仍要求填写:留着它才能一眼看出"打开开关要连哪份 Redis",也避免放松一条既有守卫。
	SharedRedis RedisConf `json:"SharedRedis"`

	// PlayerRedis:玩家域 Redis(docs/design/redis-player-store-cluster.md §5.8):资产通道读 player:{id}:location。
	// SharedRedis 留给控制域(唤醒频道与去重键、选主锁)。读取走 EffectivePlayerRedis():没写 Addrs 时指向 SharedRedis 那一个实例,
	// 开发环境不用改。与 SharedRedis 一样,只有 AssetOp.Enabled=true 时才会真的去拨它。
	PlayerRedis playerredis.Config `json:",optional"`

	// AssetOp:通用资产通道的开关与密钥来源。**整段可缺失,缺失即关闭**(见 AssetOpConf)。
	AssetOp AssetOpConf `json:"AssetOp,optional"`
	// 服务端准入白名单，默认空；实例config只信scene扣出快照，不信玩家声明。
	Commerce CommerceConf `json:"Commerce,optional"`
	Mall     MallConf     `json:"Mall,optional"`
	// 与data_service相同TiDB集群的显式全局目录schema；资产合同提交先锁角色。
	AccountRoleSchema string  `json:",optional"`
	Rmb               RmbConf `json:",optional"`
}

type RmbConf struct {
	Enabled bool `json:",optional"`
	// 必须由所有参与zone的login共用，不从SharedRedis/ZoneId推测或回落。
	GlobalLoginSessions bool                          `json:",optional"`
	LoginRedis          playerredis.LoginReaderConfig `json:",optional"`
	CurrencyTypes       []uint32                      `json:",optional"`
}
type MallConf struct {
	URL               string `json:",optional"`
	GatewaySecretEnv  string `json:",optional"`
	DeliverySecretEnv string `json:",optional"`
}

type CommerceConf struct {
	ItemConfigs    []uint32          `json:",optional"`
	ItemTitles     map[string]string `json:",optional"`
	PetConfigs     []uint32          `json:",optional"`
	CurrencyTypes  []uint32          `json:",optional"`
	AllowInstances bool              `json:",optional"`
}

// AssetOpConf 是通用资产通道(guild-phase2/04-asset-channel.md §S4)的开关与密钥来源。
//
// **默认关闭,而且"整段缺失 = 关闭"**:go-zero 不会下钻一个整段 optional 且未出现的嵌套结构,
// 所以没写 AssetOp 段时 Enabled 就是 false(这正是 SchemaConf.AutoMigrate 用 *bool 绕开的那个坑,
// 在这里反过来成了我们要的默认值)。这是**代码级的默认拒绝**:一份配置被抄到预发 / 生产环境时,
// 少抄或没抄这一段的结果是"通道关着",而不是"通道悄悄开着"。文档约定拦不住抄配置,这个默认值能。
//
// 本段**不含密钥值**:密钥只从 SecretEnv 指定的环境变量读(§4.32),由部署侧注入,
// 绝不写进 yaml / ConfigMap / 仓库。
//
// **Y-06 的 8 个重投循环字段仍未落。** 规格 90-consistency.md 的 Y-06 要求 AssetOpConf
// 另带 8 个重投循环字段(ReconcileIntervalMs 2000 / ReconcileBatch 100 / Workers 8 / LeaseMs 10000 /
// OpBudgetMs 2500 / MaxBackoffMs 60000 / PoisonDelayMs 3600000 / LedgerReadMinAttempts 3,
// Validate 与 NewLoop 同约束)。先加字段、等别人接线,会得到一份"改了没任何反应"的配置 ——
// 那比没有配置更危险:运维以为调小了 Workers,实际还是 8,且全程零报错。
// 接线时连同 assetop.DefaultLoopConfig() 一起落,并同步 etc/trade.yaml 与 k8s ConfigMap 三处。
// 在那之前,这些值由 internal/reconcile 的常量固定(规格 §4.37),与 scene 侧 1024 的 seq 窗口
// 是同一条正确性证明。
//
// **落地恢复的参数已落且已接线**(docs/design/landing-recovery-sync-digest.md §6.4,2026-10-15):
// 停放档 / 自动放弃 / 日对账 / 落地唤醒 / 终态行清理,键名与默认值与 guild 的 AssetOpConf 同名同值,
// 默认值的唯一事实源是 assetop.DefaultLoopConfig() 与 shared/assetopwake 的 Default*(config_test.go 守住相等)。
// 消费者:svc 的 recoveryFrom / wakeConfigFrom / cleanupConfFrom(换算成 reconcile.Recovery、assetopwake.Config、
// data.CleanupConf)。这些键**刻意不镜像**进 k8s_deploy.ps1 的 trade ConfigMap(理由与打开通道时要补的内容见包头注释)。
type AssetOpConf struct {
	// Enabled 打开资产通道:拨共享 Redis、建 scene 节点镜像、起 outbox 重投循环。
	//
	// false(默认)时装配层**根本不建**这些依赖 —— 不是"建好了但不用"。后者总会在某处漏判,
	// 而"没装配"这一种关法只有一个判点。此时浏览 / 详情 / 收藏照常,上架托管与交付一律以
	// 错误返回(reconcile.ErrSignerMissing),不会写进 outbox 之后永远投不出去。
	Enabled bool `json:",optional"`

	// SecretEnv 是签名密钥所在的**环境变量名**,不是密钥值。
	//
	// 写进配置只为让运维在 ConfigMap 上一眼看到该注入哪个变量;装配层会断言它等于代码里读的
	// svc.AssetOpSecretEnv,指到别处即拒启 —— 否则配置说一套、代码读一套,表现是"明明注入了
	// 密钥,scene 还是一律回 27008(AssetAuthFailed)"。Enabled=false 时不校验(整段可缺失)。
	SecretEnv string `json:",optional"`

	// Secret 是**保留键,永远必须留空**。
	//
	// 它存在的唯一目的:有人把密钥值贴进 yaml / ConfigMap 时**当场拒启**,而不是被 go-zero
	// 当未知键静默忽略 —— 静默忽略的代价是密钥从此留在仓库与 etcd 里,且没人知道它没生效。
	// 值不进日志、不进错误文本(AGENTS §11.3)。
	Secret string `json:",optional"`

	// ── 落地恢复(docs/design/landing-recovery-sync-digest.md §6;只在 Enabled 时校验,见 validateRecovery)──
	//
	// 段在、键缺时 go-zero 按下面的 default 回填;整段缺失时全是 0,而那时 Enabled 也是 false,没有消费者。

	// OfflineAfterAttempts 连续投递几次之后,"玩家确实没有位置"(位置键不存在)的行进入离线停放档(§6.1)。
	// 0 = 关闭停放档。默认 5:刚下线、正在过图的玩家几分钟内就回来,前几次仍按普通退避。
	OfflineAfterAttempts int `json:",default=5"`
	// OfflineBackoffMs 停放档的间隔中值(±20% 抖动)。停放档开启时必须不小于重投退避封顶 60000
	// (reconcile 的 loopMaxBackoff,否则"停放"比普通退避还勤),且不超过 24 小时(唤醒丢失时的最坏延迟)。
	OfflineBackoffMs int `json:",default=1800000"`
	// AbandonAfterDays 自动放弃的年龄门槛(§6.3):PENDING 且创建早于 now − 本值才是候选。0 = 关闭;非 0 时 ≥ 30。
	// **滚动升级**:新二进制全量上线之前必须写 0(混版窗口内旧实例的发送不推进 last_send_ms),全量之后再改回 180。
	// trade 的对侧账尚未实现(P3),放弃只终结并留痕,且只处理 TRADE_DEBIT 行(CREDIT 由存储层挡在候选之外),
	// 见 data.AssetOpRepo 的 abandonableStream 与 Abandon 的 TODO。
	AbandonAfterDays int `json:",default=180"`
	// AbandonSafetyGapMs 距该行最后一次**可能**的发送必须超过的安全间隔;自动放弃开启时 ≥ 600000(10 分钟,
	// 且远大于 trade 的租约 10s)。
	AbandonSafetyGapMs int `json:",default=3600000"`
	// AbandonBatch 一轮放弃最多处理多少行,[1, 1000]。
	AbandonBatch int `json:",default=200"`
	// AbandonIntervalMs 两轮放弃之间的间隔。
	AbandonIntervalMs int `json:",default=3600000"`
	// AuditIntervalMs 两轮日对账之间的间隔(§6.2,只读,出按类 gauge 与汇总日志)。0 = 不跑对账。
	AuditIntervalMs int `json:",default=86400000"`
	// Wake 落地唤醒订阅(shared/assetopwake,§5.3)的参数。整段可缺失,缺失或填 0 的项取 assetopwake 的默认值。
	Wake AssetOpWakeConf `json:"Wake,optional"`

	// ── 终态行清理(对齐 guild 的同名三键;data.AssetOpRepo.CleanupTerminalOps)──

	// CleanupEnabled 打开终态行清理(每个副本都跑,删除幂等,启动有随机初始延迟)。与 Enabled 独立(同 guild):
	// 通道关着时历史终态行照样要清。PENDING 与 APPLIED_PARTIAL 永不删。
	CleanupEnabled bool `json:",optional"`
	// CleanupIntervalMinutes 两轮清理的间隔。
	CleanupIntervalMinutes int `json:",default=10"`
	// TerminalRetentionDays 终态行(APPLIED / REJECTED / ABORTED / ABANDONED)保留天数。
	TerminalRetentionDays int `json:",default=30"`
}

// AssetOpWakeConf 是落地唤醒订阅的参数(shared/assetopwake.Config),与 guild 的同名结构同键同默认。
//
// 三个键都是 optional、默认值走方法(理由同 guild):整段可选的嵌套结构缺失时 go-zero 不下钻、不回填 default,
// 要做到"Wake 段没写 = 开、并发 4、超时 10s",默认值只能由读取方法给出;Enabled 用 *bool 区分"没写"(nil = 开)
// 与"显式写 false"(关)。默认值的唯一来源是 shared/assetopwake 的 DefaultConcurrency / DefaultWakeTimeout,
// 这里不另抄数字。跨副本去重窗口不开放配置(固定 assetopwake.DefaultDedupTTL,对齐 scene 侧 5 秒合并窗口,同 guild)。
// 订阅只在 AssetOp.Enabled 时装配(svc.newAssetChannel),订的是 SharedRedis 那一个实例(scene 发布在同一实例)。
type AssetOpWakeConf struct {
	// Enabled:nil(没写)或 true = 订阅;显式 false = 不订阅,PENDING 行只按原退避 / 停放档重投(只慢不错)。
	Enabled *bool `json:",optional"`
	// Concurrency 本副本同时在跑的唤醒数上限,满了直接丢(唤醒只是提示);0(没写)= assetopwake.DefaultConcurrency(4),
	// 否则 [1, 64]。
	Concurrency int `json:",optional"`
	// TimeoutMs 每次唤醒的独立超时;0(没写)= assetopwake.DefaultWakeTimeout(10s),否则 [5000, 60000]。
	// 下限必须大于单行预算 OpBudget(2500ms,reconcile 常量):剩余预算不够一整个 OpBudget 时唤醒不再开始新行,
	// 配得比它小则每次唤醒都回错误、静默退化成"从不唤醒";上限是关停时最坏要等的时长。
	TimeoutMs int `json:",optional"`
}

// On 报告是否订阅落地唤醒:没写 = 开。
func (w AssetOpWakeConf) On() bool {
	return w.Enabled == nil || *w.Enabled
}

// EffectiveConcurrency 是生效的并发上限;0(没写)取 assetopwake 的默认值。
func (w AssetOpWakeConf) EffectiveConcurrency() int {
	if w.Concurrency == 0 {
		return assetopwake.DefaultConcurrency
	}
	return w.Concurrency
}

// EffectiveTimeout 是生效的单次唤醒预算;0(没写)取 assetopwake 的默认值。
func (w AssetOpWakeConf) EffectiveTimeout() time.Duration {
	if w.TimeoutMs == 0 {
		return assetopwake.DefaultWakeTimeout
	}
	return time.Duration(w.TimeoutMs) * time.Millisecond
}

// 落地恢复与清理参数的合法区间。来历:
//   - 停放间隔下限 60000 = reconcile 的 loopMaxBackoff,上限 24h = assetop 的 maxOfflineBackoff;
//   - 放弃门槛下限 30 天、安全间隔下限 10 分钟、单轮上限 1000 与 assetop(abandon.go)同值 —— 这里先拦一道给出
//     指名的报错,assetop.NewLoop 在装配期兜底;上限只为挡住"毫秒 / 秒 / 天"配错量级;
//   - 清理的三项区间与 guild 的 validateCleanup 同值(保留期下限 7 天:太短会让运维追查拿不到近期记录)。
const (
	maxOfflineAfterAttempts   = 100
	minOfflineBackoffMs       = 60000
	maxOfflineBackoffMs       = 86400000
	minAbandonAfterDays       = 30
	maxAbandonAfterDays       = 3650
	minAbandonSafetyGapMs     = 600000
	maxAbandonSafetyGapMs     = 7 * 86400000
	maxAbandonBatch           = 1000
	minAbandonIntervalMs      = 60000
	maxAbandonIntervalMs      = 86400000
	minAuditIntervalMs        = 60000
	maxAuditIntervalMs        = 7 * 86400000
	maxWakeConcurrency        = 64
	minWakeTimeoutMs          = 5000
	maxWakeTimeoutMs          = 60000
	minCleanupIntervalMinutes = 1
	maxCleanupIntervalMinutes = 1440
	minRetentionDays          = 7
	maxRetentionDays          = 365
)

// validateRecovery 校验落地恢复参数;只在 Enabled 时调用(关着时整段可能缺失、全是 0,且没有消费者)。
// 各功能关着(OfflineAfterAttempts / AbandonAfterDays 为 0)时,它的从属参数不校验,与 assetop 的口径一致。
func (a AssetOpConf) validateRecovery() error {
	if a.OfflineAfterAttempts < 0 || a.OfflineAfterAttempts > maxOfflineAfterAttempts {
		return fmt.Errorf("AssetOp.OfflineAfterAttempts(%d)必须在 [0, %d] 内(0 = 关闭停放档)",
			a.OfflineAfterAttempts, maxOfflineAfterAttempts)
	}
	if a.OfflineAfterAttempts > 0 && (a.OfflineBackoffMs < minOfflineBackoffMs || a.OfflineBackoffMs > maxOfflineBackoffMs) {
		return fmt.Errorf("AssetOp.OfflineBackoffMs(%d)必须在 [%d, %d] 内:不得小于重投退避封顶,也不得超过 24 小时",
			a.OfflineBackoffMs, minOfflineBackoffMs, maxOfflineBackoffMs)
	}
	if a.AbandonAfterDays != 0 {
		if a.AbandonAfterDays < minAbandonAfterDays || a.AbandonAfterDays > maxAbandonAfterDays {
			return fmt.Errorf("AssetOp.AbandonAfterDays(%d)必须为 0(关闭)或在 [%d, %d] 内:放弃不可逆",
				a.AbandonAfterDays, minAbandonAfterDays, maxAbandonAfterDays)
		}
		if a.AbandonSafetyGapMs < minAbandonSafetyGapMs || a.AbandonSafetyGapMs > maxAbandonSafetyGapMs {
			return fmt.Errorf("AssetOp.AbandonSafetyGapMs(%d)必须在 [%d, %d] 内",
				a.AbandonSafetyGapMs, minAbandonSafetyGapMs, maxAbandonSafetyGapMs)
		}
		if a.AbandonBatch < 1 || a.AbandonBatch > maxAbandonBatch {
			return fmt.Errorf("AssetOp.AbandonBatch(%d)必须在 [1, %d] 内", a.AbandonBatch, maxAbandonBatch)
		}
		if a.AbandonIntervalMs < minAbandonIntervalMs || a.AbandonIntervalMs > maxAbandonIntervalMs {
			return fmt.Errorf("AssetOp.AbandonIntervalMs(%d)必须在 [%d, %d] 内",
				a.AbandonIntervalMs, minAbandonIntervalMs, maxAbandonIntervalMs)
		}
	}
	if a.AuditIntervalMs != 0 && (a.AuditIntervalMs < minAuditIntervalMs || a.AuditIntervalMs > maxAuditIntervalMs) {
		return fmt.Errorf("AssetOp.AuditIntervalMs(%d)必须为 0(不跑对账)或在 [%d, %d] 内",
			a.AuditIntervalMs, minAuditIntervalMs, maxAuditIntervalMs)
	}
	w := a.Wake
	if !w.On() {
		return nil // 不订阅时其余两项没有消费者,不校验(同 guild)
	}
	if w.Concurrency < 0 || w.Concurrency > maxWakeConcurrency {
		return fmt.Errorf("AssetOp.Wake.Concurrency(%d)必须为 0(默认 %d)或在 [1, %d] 内",
			w.Concurrency, assetopwake.DefaultConcurrency, maxWakeConcurrency)
	}
	if w.TimeoutMs != 0 && (w.TimeoutMs < minWakeTimeoutMs || w.TimeoutMs > maxWakeTimeoutMs) {
		return fmt.Errorf("AssetOp.Wake.TimeoutMs(%d)必须为 0(默认)或在 [%d, %d] 内:必须大于单行预算 OpBudget",
			w.TimeoutMs, minWakeTimeoutMs, maxWakeTimeoutMs)
	}
	return nil
}

// validateCleanup 校验终态行清理参数;只在 CleanupEnabled 时调用(与 guild 同口径)。
func (a AssetOpConf) validateCleanup() error {
	if a.CleanupIntervalMinutes < minCleanupIntervalMinutes || a.CleanupIntervalMinutes > maxCleanupIntervalMinutes {
		return fmt.Errorf("AssetOp.CleanupIntervalMinutes(%d)必须在 [%d, %d] 内",
			a.CleanupIntervalMinutes, minCleanupIntervalMinutes, maxCleanupIntervalMinutes)
	}
	if a.TerminalRetentionDays < minRetentionDays || a.TerminalRetentionDays > maxRetentionDays {
		return fmt.Errorf("AssetOp.TerminalRetentionDays(%d)必须在 [%d, %d] 内",
			a.TerminalRetentionDays, minRetentionDays, maxRetentionDays)
	}
	return nil
}

// RedisConf 是 go-redis 客户端的连接参数。
//
// 刻意不用 go-zero 的 redis.RedisConf:那份结构没有 DB 字段(填了会被静默忽略),也设不了连接池上限,
// 而位置键在共享库的 DB 0。字段与 go/guild/internal/config.RedisConf 一一对应(guild 那份的 Password / DB 不是 optional)。
type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password,optional"`
	DB       int    `json:"DB,optional"`
	// PoolSize 是 go-redis 连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,
	// 随 CPU limit 漂移;负数由 Validate 拒绝。K8s 上 tools/scripts/k8s_deploy.ps1 从 etc/trade.yaml 镜像
	// SharedRedis.PoolSize 并要求正整数,Redis 连接总账读同一个键。落地唤醒的 Pub/Sub 连接不占池名额。
	PoolSize int `json:"PoolSize,optional"`
}

// playerRedisFallbackPoolSize / IOTimeoutMs:没写 PlayerRedis.Addrs 且 SharedRedis.PoolSize 也没写时的连接池,以及读写超时。
// 与换句柄之前的 go-redis 缺省同量级(K8s 上 GOMAXPROCS 按 2 算),写成常量,不随 CPU limit 漂移。
const (
	playerRedisFallbackPoolSize    = 20
	playerRedisFallbackIOTimeoutMs = 3000
)

// EffectivePlayerRedis 返回生效的玩家域配置:写了 PlayerRedis.Addrs 就用它,否则指向 SharedRedis 那一个实例。
func (c *Config) EffectivePlayerRedis() playerredis.Config {
	pool := c.SharedRedis.PoolSize
	if pool == 0 {
		pool = playerRedisFallbackPoolSize
	}
	return c.PlayerRedis.OrDefault(playerredis.Config{
		Addrs:           []string{c.SharedRedis.Host},
		Password:        c.SharedRedis.Password,
		PoolSizePerNode: min(pool, playerredis.MaxPoolSizePerNode),
		ReadTimeoutMs:   playerRedisFallbackIOTimeoutMs,
		WriteTimeoutMs:  playerRedisFallbackIOTimeoutMs,
	})
}

// MySQLConf 是 mmorpg_trade 的连接参数。
type MySQLConf struct {
	Host string `json:"Host"`
	User string `json:"User"`
	// Password 可以为空(本地免密账号);值只进 DSN,绝不打日志、不进横幅。
	Password    string `json:"Password,optional"`
	DBName      string `json:"DBName"`
	MaxOpenConn int    `json:"MaxOpenConn,default=20"`
	MaxIdleConn int    `json:"MaxIdleConn,default=5"`
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);非法值拒启。
	Backend string `json:",default=tidb"`
}

// SchemaConf 建表策略(D-14 第 4 条)。
type SchemaConf struct {
	// AutoMigrate 为 nil(没写)或 false 时常驻启动仅核对；显式迁移才跑 schemamigrate.Up;false:启动时只跑只读 Plan,
	// 发现待执行语句或需人工项即拒绝启动。
	//
	// 用 *bool 而不是 `bool json:",default=true"`:go-zero 不下钻一个整段 optional 且未出现的
	// 嵌套结构,default 标签不回填,裸 bool 会让"没写 Schema 段"变成 false(与 data_service 同一个坑)。
	// K8s 非 dev 档由 ConfigMap 显式写 false。
	AutoMigrate *bool `json:",optional"`
}

// MarketConf 是聚宝斋市场参数。
type MarketConf struct {
	// Scope ∈ {zone, global},见 ScopeZone / ScopeGlobal。不给 default:范围决定玩家能看到哪些
	// 商品,写错或漏写都必须在启动时暴露。
	Scope string `json:"Scope,optional"`
	// DefaultPageSize:请求 page_size=0 时的页长。
	DefaultPageSize uint32 `json:"DefaultPageSize,default=20"`
	// MaxPageSize:page_size 的上限,超出钳到它。
	MaxPageSize uint32 `json:"MaxPageSize,default=20"`
	// MaxPage:页码上限,防深分页扫表(OFFSET ≤ (MaxPage-1)×MaxPageSize)。
	MaxPage uint32 `json:"MaxPage,default=100"`
	// MaxFavoritesPerPlayer:每玩家收藏条数软上限。
	MaxFavoritesPerPlayer uint32 `json:"MaxFavoritesPerPlayer,default=100"`
}

// ScopeEnum 把配置里的范围字符串转成协议枚举。Validate 已保证只会是两个合法值之一。
func (m MarketConf) ScopeEnum() tradepb.MarketScope {
	switch m.Scope {
	case ScopeGlobal:
		return tradepb.MarketScope_MARKET_SCOPE_GLOBAL
	case ScopeZone:
		return tradepb.MarketScope_MARKET_SCOPE_ZONE
	default:
		return tradepb.MarketScope_MARKET_SCOPE_UNSPECIFIED
	}
}

// RequestBudget 返回一次请求内全部 I/O(home_zone 查询、MySQL、发号)共用的业务预算。
// 单次调用另有 HomeZoneLookupTimeout / StoreOpTimeout 上限,两者取先到者;串行多次调用的总和
// 由本预算封顶。Validate 保证 Timeout ≥ MinRpcTimeoutMs,结果恒为正。
func (c Config) RequestBudget() time.Duration {
	return time.Duration(c.Timeout)*time.Millisecond - InBandReplyReserve
}

// ShouldAutoMigrate 识别显式自动迁移请求；常驻入口会拒绝true，没写默认只读核对。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate != nil && *c.Schema.AutoMigrate
}

// IsRelaxedMode 报告 go-zero Mode 是否为 dev / test(与 go/login/internal/config/secrets.go 同一判据)。
// TradeAdmin.SeedListing 只在这两档可用(P1-5)。
func IsRelaxedMode(mode string) bool {
	return mode == service.DevMode || mode == service.TestMode
}

// ListenHostPort 把 ListenOn 拆成 host 与端口号。用 net.SplitHostPort 以正确处理 "[::]:50800"。
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

// Validate 做启动期整体校验(fail-fast)。conf.MustLoad / conf.Load 会自动调用它(go-zero v1.10.0)。
// 每一条拒绝的都是"进程能起来、但行为静默出错"的配置:
//   - ZoneId=0 / ListenOn 非法 / Timeout 越界 / Etcd.Hosts 为空 / Etcd.Key 非空:同 chat(契约 §2/§3、D-13);
//     Timeout 另有下限 MinRpcTimeoutMs,否则整请求预算(RequestBudget)为非正,所有 I/O 立即超时;
//   - MySQL 缺 Host/User/DBName,或 DBName 不是 mmorpg_trade:表会建进别的库(D-14 第 3 条的第一道防线);
//   - DataServiceRpc 没有目标:home_zone 查不到,浏览 / 详情 / 种子全部失败;
//   - IdSegment 未启用或开了 snowflake 回退:trade 没有 snowflake,listing_id 无号可发;
//   - Market.Scope 不在 {zone, global}:玩家可见范围不确定;
//   - 页长 / 页数 / 收藏上限 ≤ 0,或默认页长大于上限;
//   - AssetOp.Secret 非空:密钥写进了配置文件(泄漏),无论开关如何都拒;
//   - AssetOp.Enabled=true 但 SecretEnv 不是合法环境变量名:密钥来源说不清,scene 会一律回 27008。
//     "开着却签不出名"由装配层再拦一道(svc.NewServiceContext 读不到密钥即拒启);
//   - AssetOp.Enabled=true 时落地恢复参数越界(停放间隔、放弃门槛 / 安全间隔 / 批量 / 间隔、对账间隔、唤醒参数);
//   - AssetOp.CleanupEnabled=true 时清理间隔或保留期越界(与 Enabled 独立,同 guild)。
func (c *Config) Validate() error {
	if _, err := authoritysql.ParseBackend(c.MySQL.Backend); err != nil {
		return fmt.Errorf("MySQL.Backend: %w", err)
	}
	if c.ZoneId == 0 {
		return errors.New("ZoneId 必须非 0(K8s 全局服务用 ConfigMap 注入的 CurrentZoneId)")
	}
	if c.ListenOn == "" {
		return errors.New("ListenOn 不能为空")
	}
	if _, _, err := c.ListenHostPort(); err != nil {
		return err
	}
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 [%d, %d] 内:上限是路由服 ForwardTimeoutMs − 1000(契约 §3),"+
			"下限保证扣掉 %v 回包余量后仍有 I/O 预算", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs, InBandReplyReserve)
	}
	if len(c.Etcd.Hosts) == 0 {
		return errors.New("Etcd.Hosts 不能为空(节点注册与 killswitch 都依赖 etcd)")
	}
	if c.Etcd.Key != "" {
		return fmt.Errorf("Etcd.Key 必须留空(得到 %q):全局服务不注册 go-zero key,契约 D-13", c.Etcd.Key)
	}
	if c.LeaseTTL <= 0 {
		return fmt.Errorf("LeaseTTL 必须为正数,得到 %d", c.LeaseTTL)
	}

	if c.MySQL.Host == "" || c.MySQL.User == "" || c.MySQL.DBName == "" {
		return errors.New("MySQL.Host / MySQL.User / MySQL.DBName 都不能为空")
	}
	if c.MySQL.DBName != data.DatabaseName {
		return fmt.Errorf("MySQL.DBName 必须是 %q(得到 %q):trade 独占该库,表不许建进别的库(D-14)",
			data.DatabaseName, c.MySQL.DBName)
	}
	if c.MySQL.MaxOpenConn <= 0 || c.MySQL.MaxIdleConn <= 0 {
		return fmt.Errorf("MySQL.MaxOpenConn(%d)与 MySQL.MaxIdleConn(%d)必须为正数",
			c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn)
	}

	if !HasRpcTarget(c.DataServiceRpc) {
		return errors.New("DataServiceRpc 必须配置 Etcd.Key / Endpoints / Target 之一:home_zone 查询与 listing_id 发号都依赖 data_service")
	}
	if !c.IdSegment.Enabled {
		return errors.New("IdSegment.Enabled 必须为 true:listing_id 只由号段发(biz_tag=trade_listing),trade 没有 snowflake 回退")
	}

	if c.Market.Scope != ScopeZone && c.Market.Scope != ScopeGlobal {
		return fmt.Errorf("Market.Scope 必须是 %q 或 %q,得到 %q", ScopeZone, ScopeGlobal, c.Market.Scope)
	}
	positives := []struct {
		name  string
		value uint32
	}{
		{"Market.DefaultPageSize", c.Market.DefaultPageSize},
		{"Market.MaxPageSize", c.Market.MaxPageSize},
		{"Market.MaxPage", c.Market.MaxPage},
		{"Market.MaxFavoritesPerPlayer", c.Market.MaxFavoritesPerPlayer},
	}
	for _, p := range positives {
		if p.value == 0 {
			return fmt.Errorf("%s 必须为正数", p.name)
		}
	}
	if c.Market.DefaultPageSize > c.Market.MaxPageSize {
		return fmt.Errorf("Market.DefaultPageSize(%d)不能大于 Market.MaxPageSize(%d)",
			c.Market.DefaultPageSize, c.Market.MaxPageSize)
	}

	// SharedRedis 必填:缺它 = 资产通道找不到玩家 = 托管 / 交付永远悬挂。玩家资产路径
	// fail-closed(AGENTS §11.3),宁可在启动期点名缺哪一段,也不要起一个"浏览能用、
	// 一上架就永远卡住"的 trade。
	if c.SharedRedis.Host == "" {
		return errors.New("SharedRedis.Host 不能为空:资产通道按 player:{id}:location 定位玩家所在 scene," +
			"该键在与 C++ scene 相同的共享 Redis 上")
	}
	if c.SharedRedis.PoolSize < 0 {
		return fmt.Errorf("SharedRedis.PoolSize(%d)不能为负:0 = go-redis 缺省 10 × GOMAXPROCS", c.SharedRedis.PoolSize)
	}
	if err := c.PlayerRedis.ValidateSection(); err != nil {
		return fmt.Errorf("PlayerRedis: %w", err)
	}

	// 资产通道开关(§S4)。整段缺失 = 关闭,是刻意的默认拒绝,见 AssetOpConf。
	//
	// Secret 无论开关如何都必须为空:配置文件里的密钥是泄漏,不是配置。
	// 错误文本只说"必须留空",不回显任何片段。
	if c.AssetOp.Secret != "" {
		return fmt.Errorf("AssetOp.Secret 必须留空:签名密钥只从环境变量 %s<CALLER> 读(§4.32),"+
			"绝不写进 yaml / ConfigMap / 仓库。删掉这一行,改由部署侧注入该环境变量", AssetOpSecretEnvPrefix)
	}
	if c.AssetOp.Enabled && !assetOpSecretEnvPattern.MatchString(c.AssetOp.SecretEnv) {
		// **不回显这个值**。这条分支最常见的触发原因恰恰是"手滑把密钥值贴进了 SecretEnv",
		// 回显就等于把密钥抄进启动日志 —— 上面 Secret 那条特地不回显,这里回显就自相矛盾了
		// (AGENTS §11.3:真实密钥不得进日志 / 指标 / 错误响应)。只说长度与形状不符,
		// 足够定位手滑,又不泄漏内容。
		return fmt.Errorf("AssetOp.Enabled=true 时 AssetOp.SecretEnv 必须是形如 %sTRADE 的环境变量名"+
			"(得到一个长度 %d 的值,形状不符;此处刻意不回显内容,因为它可能就是被误贴进来的密钥):"+
			"它是密钥的来源声明,不是密钥值", AssetOpSecretEnvPrefix, len(c.AssetOp.SecretEnv))
	}
	// 落地恢复参数只在通道开着时有消费者(停放 / 放弃 / 对账 / 唤醒都挂在重投循环上),关着时一概不查。
	if c.AssetOp.Enabled {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(c.AccountRoleSchema) || !c.Rmb.GlobalLoginSessions || !c.Rmb.LoginRedis.HasAddr() {
			return errors.New("交易写面必须显式配置全局角色schema与全局login会话Redis，不允许从SharedRedis推测")
		}
		if c.Rmb.LoginRedis.DB < 0 || c.Rmb.LoginRedis.PoolSizePerNode < 0 {
			return errors.New("登录态Redis库号/连接池非法")
		}
		if c.Market.Scope != ScopeGlobal {
			return errors.New("交易写面必须使用全球市场，禁止按区域独立撮合")
		}
		for _, id := range c.Commerce.ItemConfigs {
			if id == 0 || strings.TrimSpace(c.Commerce.ItemTitles[strconv.FormatUint(uint64(id), 10)]) == "" {
				return errors.New("可交易道具必须配置服务端名称与合法配置号")
			}
		}
		for _, currency := range c.Commerce.CurrencyTypes {
			if !isTradableCurrency(currency) {
				return errors.New("交易货币只允许金币0与显式启用钻石1，绑定币禁售")
			}
		}
		if err := c.AssetOp.validateRecovery(); err != nil {
			return err
		}
	}
	if c.Rmb.Enabled {
		if !c.AssetOp.Enabled || c.Mall.URL == "" {
			return errors.New("人民币寄售必须开启商城与SQL资产通道")
		}
		for _, currency := range c.Rmb.CurrencyTypes {
			if !isTradableCurrency(currency) {
				return errors.New("人民币寄售不能交易绑定币")
			}
		}
	}
	// 清理只依赖 MySQL,与通道开关独立(同 guild):关着通道时历史终态行照样要清。
	if c.AssetOp.CleanupEnabled {
		if err := c.AssetOp.validateCleanup(); err != nil {
			return err
		}
	}
	return nil
}

// HasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target),
// 与 go/guild/internal/svc.hasRpcTarget 同一判据。
func HasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}

// isTradableCurrency:交易与人民币寄售可用的币种,是取值表(shared/currencytype)的子集。只点名常量、
// 不写数字:扩白名单时不可能写出表外的值,否则 scene 会把它判成版本错位,截止为 0 的交付流将永久 RETRY。
// 绑定灵石禁售。
func isTradableCurrency(currency uint32) bool {
	return currency == currencytype.Gold || currency == currencytype.Diamond
}
