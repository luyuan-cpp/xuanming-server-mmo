// Package config 是 trade 服务的配置结构与启动期校验。
//
// 键名与 etc/trade.yaml、tools/scripts/k8s_deploy.ps1 的 trade ConfigMap 三处逐字一致;
// 改名必须三处同改,否则 ConfigMap 里的值会被 go-zero 当未知键静默忽略。
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"trade/internal/data"

	tradepb "proto/trade"

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

	// DataServiceRpc:查 home_zone 与领号段的 data_service 客户端。不标 optional:
	// P1 的浏览 / 详情 / 收藏 / 种子都依赖它,缺这段就该在加载期失败。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc"`

	// IdSegment:listing_id 号段(biz_tag=trade_listing)。标 optional 只为让"整段没写"落到
	// Validate 给出可读的错误,而不是 go-zero 的缺键报错;Enabled 必须显式 true。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`

	// Market:市场范围、分页与收藏上限。
	Market MarketConf `json:"Market"`

	// SharedRedis:**共享单库** Redis,只读一类跨运行时契约键 —— player:{id}:location
	// (scene_manager 写,值是 PlayerLocation protobuf)。资产通道靠它找到玩家当前所在的
	// scene 节点(go/shared/scenenode.Locator),没有它就发不出 AssetDebit / AssetCredit。
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

	// AssetOp:通用资产通道的开关与密钥来源。**整段可缺失,缺失即关闭**(见 AssetOpConf)。
	AssetOp AssetOpConf `json:"AssetOp,optional"`
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
// **本批只落开关与密钥来源,没有落循环参数。** 规格 90-consistency.md 的 Y-06 要求 AssetOpConf
// 另带 8 个重投循环字段(ReconcileIntervalMs 2000 / ReconcileBatch 100 / Workers 8 / LeaseMs 10000 /
// OpBudgetMs 2500 / MaxBackoffMs 60000 / PoisonDelayMs 3600000 / LedgerReadMinAttempts 3,
// Validate 与 NewLoop 同约束)。它们的**唯一消费者**是 internal/reconcile 建 assetop.LoopConfig 的那处,
// 不在本工作包的归属文件里。先加字段、等别人接线,会得到一份"改了没任何反应"的配置 ——
// 那比没有配置更危险:运维以为调小了 Workers,实际还是 8,且全程零报错。
// 接线时连同 assetop.DefaultLoopConfig() 一起落,并同步 etc/trade.yaml 与 k8s ConfigMap 三处。
// 在那之前,这些值由 internal/reconcile 的常量固定(规格 §4.37),与 scene 侧 1024 的 seq 窗口
// 是同一条正确性证明。
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
}

// RedisConf 是 go-redis 客户端的连接参数。
//
// 刻意不用 go-zero 的 redis.RedisConf:那份结构没有 DB 字段(填了会被静默忽略),
// 而位置键在共享库的 DB 0。形状与 go/guild/internal/config.RedisConf 逐字一致。
type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password,optional"`
	DB       int    `json:"DB,optional"`
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
}

// SchemaConf 建表策略(D-14 第 4 条)。
type SchemaConf struct {
	// AutoMigrate 为 nil(没写)或 true:启动时跑 schemamigrate.Up;false:启动时只跑只读 Plan,
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

// ShouldAutoMigrate 是启动路径跑 Up 还是只跑 Plan 的唯一判据:没写 = Up。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
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
//     "开着却签不出名"由装配层再拦一道(svc.NewServiceContext 读不到密钥即拒启)。
func (c *Config) Validate() error {
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
	if c.IdSegment.FallbackToSnowflake {
		return errors.New("IdSegment.FallbackToSnowflake 必须为 false:trade 没有 snowflake 节点可回退")
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
	return nil
}

// HasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target),
// 与 go/guild/internal/svc.hasRpcTarget 同一判据。
func HasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
