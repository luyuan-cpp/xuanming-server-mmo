package config

import (
	"errors"
	"fmt"
	"time"

	"shared/placement"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// 仅隔离的历史MySQL/Redis工具夹具可显式开启；dev也默认全球SQL，不因Mode=dev自动降级。
	LegacyNonGlobal  bool `json:",optional"`
	MappingRedis     redis.RedisConf
	Regions          []RegionConfig
	DevRedis         DevRedisConfig `json:",optional"`
	PlayerLockTTLSec int            `json:",default=3"`

	// Snapshot MySQL (for rollback persistence)
	SnapshotMySQL SnapshotMySQLConfig `json:",optional"`
	// 账号目录全局规则；各 zone 的 login 不能抬高这里的角色上限。
	AccountDirectory AccountDirectoryConfig `json:",optional"`

	// Login admin gRPC client (for orphan account cleanup during rollback)
	LoginAdminRpc zrpc.RpcClientConf `json:",optional"`

	// AdminToken 是运维专用 RPC 的共享口令,调用方放在 gRPC metadata 的 `x-admin-token` 里。
	//
	// **2026-10-12 起 RemapHomeZoneForMerge 已停用**(docs/design/player-route-mapping-durability-proposal.md §4.3):
	// 不论带不带 token、token 对不对,一律回 FailedPrecondition + ErrCodeNotImplemented,改映射只走 tools/merge_zone。
	// 所以这个 token 现在只守三个回档 RPC 与 ReleasePlayerName 的运维路径(不限登记时间的释放)。
	// 下面关于 remap 的几段保留为历史理由 —— 它们同样适用于回档:
	//
	// **空 = 该 RPC 整体停用**(fail closed),不是"免鉴权"。理由:data_service 在集群
	// 内网是无鉴权可达的,任何一个能拨到它的进程都能调 remap;默认敞开意味着一次误配
	// 或一个跑错环境的脚本就能把全服玩家送进别的 zone,而这个操作没有反向操作
	// (改回去要先知道谁原本在哪个 zone,而那份信息刚被覆盖掉)。所以默认值只能是
	// "不能用",启用是一次显式的运维动作:合服维护窗口里临时下发,窗口结束就撤掉。
	//
	// 不放进 yaml 明文的话可以用 k8s Secret 挂成环境变量再由 ConfigMap 引用。
	//
	// 2026-09-28 起三个回档 RPC(RollbackPlayer / RollbackZone / RollbackAll)也一律要求它
	// (docs/design/guild-phase2/07-rollback-fail-closed.md §7.6.3 Q2 = ②):没配 = 回档 RPC 同样整体停用。
	// 回档还有自己的后续闸(跨服务离线栅栏、帮会资产闸),token 泄露不等于能静默回档。
	AdminToken string `json:",optional"`

	// Prometheus /metrics HTTP listen address (host:port). Empty = disabled.
	// Scraped by Prometheus alongside scene_manager. Exposes per-RPC outcome /
	// latency, per-player lock contention, version mismatch, rollback counts.
	// See go/data_service/internal/metrics/metrics.go for the metric list.
	MetricsListenAddr string `json:",optional"`

	// Schema 控制全局库(SnapshotMySQL)各表的建表/补列路径:proto 驱动的五张(其中 id_segment、player_name
	// 走手写 bootstrap DDL)+ 同样走手写 DDL、proto 只作列漂移守卫的路由表(player_route 等),见 internal/store/schema.go。
	// 整段可缺失,缺失语义见 SchemaConfig.AutoMigrate 与 ShouldAutoMigrate。
	Schema SchemaConfig `json:",optional"`

	// Store 见 StoreConfig。
	Store StoreConfig `json:",optional"`

	// RouteStore 是玩家归属 / 落点映射的提交点(redis | db_first),见 RouteStoreConfig。
	// 整段可缺失:缺失 = redis 模式,与 2026-10-12 之前逐字相同。
	RouteStore RouteStoreConfig `json:",optional"`

	// Kafka 是 C++ scene 产出的 transaction_log_topic / player_snapshot_topic 的落库消费者
	// 配置。Brokers 为空 = 不消费(本地无 Kafka 时的合法形态);消费者启动失败不会拖死
	// data_service(Load/Save 是热路径),只记日志并每 30s 后台重试。
	Kafka KafkaConfig `json:",optional"`

	// IdSegment 控制号段表 id_segment 的**行**生命周期(表结构归 Schema 管),
	// 见 internal/store/id_segment_store.go 与设计 §7.5 第 7 条。整段可缺失,缺失 = 生产安全值。
	IdSegment IdSegmentConfig `json:",optional"`

	// PlayerName 是玩家名字注册表(全服唯一)的运行参数,见 internal/store/player_name_store.go
	// 与 internal/logic/player_name_logic.go。整段可缺失:每个键的零值都当"没配",
	// 由 PlayerNameConfig.Normalize() 填上文档化的默认值(装配在 svc.NewServiceContext)。
	PlayerName PlayerNameConfig `json:",optional"`

	// ZoneId 只用于 C++ 约定的 etcd 注册路径
	// (DataServiceNodeService.rpc/zone/<ZoneId>/node_type/26/node_id/<N>,见 internal/noderegistry)。
	// data_service 按 zone namespace 各部署一份,但 DataServiceNodeService **不是** zone-scoped
	// 节点类型(C++ 发现侧只有 Gate/Scene/Login/PlayerLocator 按 zone 过滤),所以填哪个 zone
	// 都能被任何 scene 发现;写成 Pod 所在的 zone 只是为了排障时一眼对得上。
	// optional、默认 0:整段缺失也能注册(与 scene_manager / match 的 default=1 不同 —— 那两个
	// 靠它区分本地 dev-start-zones 每 zone 起的一份)。k8s ConfigMap 写部署 zone(${CurrentZoneId})。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL 是 C++ 约定注册那把 etcd 租约的 TTL(秒),keepalive 按它续租;
	// 与 scene_manager / match 同形(顶层键、默认 60)。租约丢失 = C++ 侧看到 data_service
	// 消失,scene 的号段续段会失败(手里两段能顶 10~20 分钟),noderegistry 会 CAS 重夺 / 重分配 node_id。
	LeaseTTL int64 `json:",default=60"`

	// ── 回档的帮会资产闸(docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.5)──────
	// 三项都只影响三个 Rollback*,配错 / 没配的后果一律是"回档被拒"(fail-closed),不拖累 Load/Save。

	// GuildInternalRpc 是回档前问 guild "这些玩家自快照以来有没有已应用的资产操作"的客户端
	// (guildpb.GuildInternal/ListAppliedAssetOpsSince)。
	//
	// **不配 = 回档一律被拒**(ErrCodeRollbackGuildCheckFailed),不是"跳过检查"。
	//
	// 只认 Endpoints / Target 直连(07 §7.11-U10 选 (c)),见 HasGuildInternalRpc:
	//   - guild 刻意不写 go-zero 的 Etcd.Key(port-decisions D-13),按 etcd 发现永远找不到它;
	//   - RpcClientConf.Etcd 带 `inherit`:块在但没写 Etcd(或只写了 Hosts)时会继承顶层 Etcd 的
	//     Key=dataservice.rpc —— 拨到 data_service 自己身上。所以只有 Etcd 的配置按"没配"处理。
	//
	// 单次调用上限就是这里的 Timeout(ValidateGuildCheck 限 [500, 3500] ms),**不另设旋钮**:
	// zrpc 客户端自带超时拦截器,再立一个 CallTimeoutMs 只会得到两个旋钮、小的那个悄悄生效。
	GuildInternalRpc zrpc.RpcClientConf `json:",optional"`

	// GuildClockSkewMarginMs:每个玩家查询的起点 since_ms = 快照时刻(秒)×1000 − 本值(07 §7.2)。
	// 它要盖住两件事:data_service 与 guild 两台机器的墙钟差,**加上** guild 记下的终结时刻其实是
	// ProcessOne 的**开始**时刻、可早于 scene 实际落盘最多一个 OpBudget(2500ms)。所以即使同机部署、
	// 时钟差为零,它也**不得为 0**:小于"快照时刻 − 终结时刻"的余量会漏掉"快照不含这笔扣款、guild 却记在
	// 快照之前"的行 = 回档复制资产(fail-open)。
	// 合法区间 [MinGuildClockSkewMarginMs, MaxGuildClockSkewMarginMs];默认 300000(5 分钟,与 04 §4.32
	// 的 ±300s 服务间时钟承诺同口径)。代价是多拒:快照前 5 分钟内终结、其实已含在快照里的行也会被列出。
	GuildClockSkewMarginMs int64 `json:",default=300000"`

	// GuildCheckBudgetSeconds:帮会检查阶段(沉降等待之后、第一笔写之前)的总预算,单调时钟。
	// **只罩检查阶段**,不罩写阶段。耗尽 = CheckFailed、零写入,
	// 不重试:重试只会把"guild 正在抖"伪装成"慢"。玩家很多的全服回档检查不完时调大它,并同步调大
	// yaml 里对应方法的 MethodTimeouts。
	// 写后复查的预算跟着它走(GuildRecheckBudget):复查与检查走同一套分块翻页、范围约等于全部写过的玩家,
	// 所以这里每调大 1 秒,MethodTimeouts 要调大 2 秒。
	GuildCheckBudgetSeconds int64 `json:",default=120"`
}

type AccountDirectoryConfig struct {
	MaxPlayersPerAccount      int    `json:",optional"`
	RoleTradeRequireGuildExit *bool  `json:",optional"`
	RoleTradeGuildSchema      string `json:",optional"`
	RoleTradeTradeSchema      string `json:",optional"`
	RoleTradeMailSchema       string `json:",optional"`
	RoleTradeMatchSchema      string `json:",optional"`
	RoleTradeInboxSchema      string `json:",optional"`
	// 必须显式配置；使用同 TiDB 权威池跨 schema 强读，不新配另一份资产源。
	RoleTradeSQLProof RoleTradeSQLProofConfig `json:",optional"`
	// MaxOpenConn 是账号目录独立 SQL 池(store.NewAccountDirectoryStore → authoritysql.Open)的连接上限,经 PoolMaxOpenConn 取用。
	// 0 / 缺省 = DefaultAccountDirectoryMaxOpenConn;合法区间 [1, MaxAccountDirectoryMaxOpenConn]。越界不纠正成默认值而是不装配目录
	// (理由同 PlayerNameConfig.Validate):MissingStores 带上 account_directory,全球服务恒为 Store.Required=true,main 拒启。
	// 每次登录的 ReadAccountDirectory 都在这个池上开一个悲观事务,登录并发上来时它是第一个排队点,所以做成旋钮。
	// 它与 SnapshotMySQL / PlayerName / RouteStore 的池一样连同一台全局库、计入连接总账(每个副本多占 MaxOpenConn 条):
	// 调大前按 tools/scripts/k8s_deploy.ps1 的连接总账核对 max_connections,部署脚本从本键读取。
	MaxOpenConn int `json:",optional"`
}

// 账号目录池上限的缺省与边界(见 AccountDirectoryConfig.MaxOpenConn)。
const (
	// DefaultAccountDirectoryMaxOpenConn 沿用改成可配置之前写死在构造点的值。
	DefaultAccountDirectoryMaxOpenConn = 8
	// MaxAccountDirectoryMaxOpenConn = shared/authoritysql.Open 对单池的硬上限:更大的值那边一律拒绝开池,
	// 在这里先拒,启动日志能直接指到配置键。那边放宽时这里同步。
	MaxAccountDirectoryMaxOpenConn = 128
)

// PoolMaxOpenConn 返回账号目录池的实际上限:0 = DefaultAccountDirectoryMaxOpenConn,负数或超过
// MaxAccountDirectoryMaxOpenConn 返回错误(调用方不装配目录)。
func (c AccountDirectoryConfig) PoolMaxOpenConn() (int, error) {
	if c.MaxOpenConn == 0 {
		return DefaultAccountDirectoryMaxOpenConn, nil
	}
	if c.MaxOpenConn < 1 || c.MaxOpenConn > MaxAccountDirectoryMaxOpenConn {
		return 0, fmt.Errorf("AccountDirectory.MaxOpenConn must be in [1, %d] (0 = default %d), got %d",
			MaxAccountDirectoryMaxOpenConn, DefaultAccountDirectoryMaxOpenConn, c.MaxOpenConn)
	}
	return c.MaxOpenConn, nil
}

func (c AccountDirectoryConfig) MustLeaveGuild() bool {
	return c.RoleTradeRequireGuildExit == nil || *c.RoleTradeRequireGuildExit
}

type RoleTradeSQLProofConfig struct {
	Enabled           bool     `json:",optional"`
	AllowedStorageIDs []uint32 `json:",optional"`
	// 与go/db已有独立库白名单共源；空值使用DB_ALLOWED_DATABASES。没有配置绝不按Regions/当前Zone推导。
	AllowedDatabasesEnv string `json:",optional"`
}

func (c AccountDirectoryConfig) MaxRoles() int {
	if c.MaxPlayersPerAccount == 0 {
		return 5
	}
	return c.MaxPlayersPerAccount
}

// GuildRecheckBudget:写后复查调用本身的预算(不含复查前的等待)= max(MinGuildRecheckBudgetSeconds, 检查预算)。
// 不单独做成配置项:复查的工作量由检查阶段决定(同一个 checker、同样的玩家与 since),它的预算没有独立于检查预算的取值依据;
// 两个旋钮只会被拧成不一致 —— 检查预算调大而复查仍是 120s 时,凡是"调大了才过得了闸"的回档,数据写完后复查必然超时,
// 每次都误报紧急的 post_write_recheck_failed,真分歧反而查不出(2026-10-08 评审)。
func (c Config) GuildRecheckBudget() time.Duration {
	seconds := c.GuildCheckBudgetSeconds
	if seconds < MinGuildRecheckBudgetSeconds {
		seconds = MinGuildRecheckBudgetSeconds
	}
	return time.Duration(seconds) * time.Second
}

// 回档帮会闸的配置边界(07 §7.2、§7.5.3-3)。
const (
	// MinGuildClockSkewMarginMs = OpBudget(2500ms)× 2:终结时刻可早于落盘一个 OpBudget,再留一倍给时钟差。
	// 0 与 4999 一律拒绝,理由见 Config.GuildClockSkewMarginMs。
	MinGuildClockSkewMarginMs int64 = 5000
	// MaxGuildClockSkewMarginMs:1 小时。再大就不是"时钟差"了,而是把整段历史都当成分歧,放行日志失去照单补偿的价值。
	MaxGuildClockSkewMarginMs int64 = 3_600_000

	// MinGuildInternalRpcTimeoutMs:下限防止写 0(go-zero 把 0 当成"不设客户端超时")。
	MinGuildInternalRpcTimeoutMs int64 = 500
	// MaxGuildInternalRpcTimeoutMs = guild 服务端 Timeout 4000(go/guild/etc/guild.yaml)− 500 回包余量:
	// 客户端比服务端等得久,拿到的只会是服务端的 DeadlineExceeded,多等的那段没有意义。
	MaxGuildInternalRpcTimeoutMs int64 = 3500

	// MaxGuildCheckBudgetSeconds:检查阶段总预算的上限。一小时仍检查不完的回档应当缩小范围分批做;
	// 也防止把一个秒数误填成毫秒后 Duration 乘法溢出。
	MaxGuildCheckBudgetSeconds int64 = 3600

	// MinGuildRecheckBudgetSeconds:写后复查预算的下限(见 Config.GuildRecheckBudget)。检查预算被调得很小时
	// 复查仍至少有这么久:数据已经写了,复查做不成的代价(紧急告警 + 人工核对)远大于多等两分钟。
	MinGuildRecheckBudgetSeconds int64 = 120
)

// HasGuildInternalRpc:是否配置了回档帮会检查的 guild 客户端。只认 Endpoints / Target,
// 不认 Etcd(理由见 Config.GuildInternalRpc:guild 不注册 go-zero key,且 Etcd 会继承顶层 dataservice.rpc)。
func (c Config) HasGuildInternalRpc() bool {
	return len(c.GuildInternalRpc.Endpoints) > 0 || c.GuildInternalRpc.Target != ""
}

// ValidateGuildCheck 检查回档帮会闸的三项配置。返回非 nil = 回档闸不装配(svc.NewServiceContext),
// 三个 Rollback* 一律 ErrCodeRollbackGuildCheckFailed;logic 每次回档前还会再调一次(纵深防御:
// 单测与未来的装配点绕过 NewServiceContext 时,也不会拿一个非法余量去做检查)。
//
// 配错**拒绝而不是纠正**,理由同 PlayerNameConfig.Validate:余量是安全边界,被悄悄改成默认值
// 谁都不知道;拒绝的症状是回档当场全部被拒,启动日志里写清是哪个键。
// 不拒启整个进程:这三项只关系到回档,不能为它们让 Load/Save 这条玩家数据热路径停摆。
func (c Config) ValidateGuildCheck() error {
	if c.GuildClockSkewMarginMs < MinGuildClockSkewMarginMs || c.GuildClockSkewMarginMs > MaxGuildClockSkewMarginMs {
		return fmt.Errorf("GuildClockSkewMarginMs(%d)必须在 [%d, %d] 内:终结时刻是 ProcessOne 的开始时刻,可早于落盘一个 OpBudget,余量为 0 会漏行(回档复制资产)",
			c.GuildClockSkewMarginMs, MinGuildClockSkewMarginMs, MaxGuildClockSkewMarginMs)
	}
	if c.GuildCheckBudgetSeconds <= 0 || c.GuildCheckBudgetSeconds > MaxGuildCheckBudgetSeconds {
		return fmt.Errorf("GuildCheckBudgetSeconds(%d)必须在 [1, %d] 内", c.GuildCheckBudgetSeconds, MaxGuildCheckBudgetSeconds)
	}
	if c.HasGuildInternalRpc() {
		t := c.GuildInternalRpc.Timeout
		if t < MinGuildInternalRpcTimeoutMs || t > MaxGuildInternalRpcTimeoutMs {
			return fmt.Errorf("GuildInternalRpc.Timeout(%d ms)必须在 [%d, %d] 内:0 = 不设客户端超时;上限 = guild 服务端 Timeout 4000 − 500 回包余量",
				t, MinGuildInternalRpcTimeoutMs, MaxGuildInternalRpcTimeoutMs)
		}
	}
	return nil
}

// IdSegmentConfig 号段行的种植策略。
//
// 背景(设计 §7.5 第 7 条):id_segment 里每一行都是一个永久身份的发号水位。行没了
// (全局库被 drop 重建、或从旧备份恢复)而消费表(player_database / guild / 玩家 blob 里的
// 物品)还留着已发出的号,下一次发号就会从 1 重来,login 的 INSERT ... ON DUPLICATE KEY
// UPDATE 会**静默覆盖**别人的角色行。所以行只能由迁移显式创建,运行期"缺行就补种"仅限 dev。
type IdSegmentConfig struct {
	// AllowAutoSeed=true:AllocateIdSegment 遇到表里没有的 biz_tag 时自动种一行从 1 起
	// (今天的行为,只给 dev 用);false(生产):返回 ErrCodeIdSegmentUnknownTag,不写任何行,
	// 行必须由 `data_service -migrate` / AutoMigrate 按 BootstrapTags 创建。
	//
	// 这里可以放心用裸 bool:go-zero 不下钻一个"整段 optional 且未出现"的嵌套结构,
	// 里面的 default 标签一个都不会回填 —— 但本键的安全值恰好就是零值 false,块缺失
	// 得到的正是生产语义(与 SchemaConfig.AutoMigrate 需要 *bool 的原因相反)。
	AllowAutoSeed bool `json:",optional"`

	// BootstrapTags 迁移时用 INSERT IGNORE 预建的 biz_tag 清单(max_id=1, step=100,
	// version=0),幂等、绝不降低已有行的 max_id。为空 = DefaultIdSegmentBootstrapTags
	// (原因同上:块缺失时 default 标签不生效,只能在 EffectiveBootstrapTags 里兜)。
	// 新增一种永久身份 = 在这里加一个 tag,而不是靠运行期补种。
	BootstrapTags []string `json:",optional"`
}

// DefaultIdSegmentBootstrapTags 是走号段的永久身份清单:设计 §6.4 / §7.5 第 1 条的
// player / guild(Go login / guild)与 item / txlog / snapshot(C++ scene),
// 以及聚宝斋 trade_listing(Go trade 的 listing_id,docs/design/jubaozhai-market.md)。
// 与 store.DefaultIdSegmentBootstrapTags **必须一致**(config 不能 import store,只能两处各写一份),
// 还要与 etc/data_service.yaml 的 IdSegment.BootstrapTags、k8s_deploy.ps1 data-service ConfigMap 同步。
// trade_listing 的消费表在独占库 mmorpg_trade,不在本服务全局库里,所以不进 schema.go 的水位地板校验。
// guild_asset_op:帮会资产指令的 op_id(Go guild,同时作资产通道的 correlation_id;
// docs/design/guild-phase2/05-economy.md §5.8)。消费表在独占库 mmorpg_guild,同理不做地板校验。
// friend_request:好友申请的代次号 request_id(Go friend,docs/design/friend-port-20260918.md)。
// 消费表在独占库 mmorpg_friend,同理不做地板校验。
// mail:邮件的 mail_id / sys_mail_id(Go mail,两种 id 共用这一个 tag,所以全局不撞号;
// docs/design/mail-system.md §3.7)。消费表在独占库 mmorpg_mail,同理不做地板校验。
var DefaultIdSegmentBootstrapTags = []string{"player", "guild", "item", "txlog", "snapshot", "trade_listing", "guild_asset_op", "friend_request", "trade_asset_op", "mail", "mail_asset_op", "rank_board", "rank_settlement", "rank_reward"}

// EffectiveBootstrapTags 是迁移路径实际预建的 tag 清单:没配 = 默认清单。
// 返回副本,免得调用方改到包级默认值。
func (c IdSegmentConfig) EffectiveBootstrapTags() []string {
	src := c.BootstrapTags
	if len(src) == 0 {
		src = DefaultIdSegmentBootstrapTags
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// StoreConfig 决定 MySQL 侧的 store(快照 / 流水 / 号段 / 名字注册表)装配不起来时进程怎么办。
type StoreConfig struct {
	// Required=true:四个 store 任一没装配起来(MySQL 启动瞬间不可达、迁移失败、表结构不对)就**拒绝启动**,
	// 而且发生在任何 etcd 注册之前。缺省 false = 沿用旧行为:降级运行,依赖它的 RPC 各自返回错误。
	//
	// 为什么多副本必须开:store 为 nil 的实例 gRPC 健康检查照样是 SERVING、两条 etcd 注册照常进行。
	// 单实例时这是"全挂",很显眼;多实例时是**按比例失败**(p2c / 随机挑到它的请求才失败),隐蔽得多;
	// 滚动更新还会把健康的旧 Pod 换成一个带病的新 Pod。K8s 的 ConfigMap 所有档位都写 true(k8s_deploy.ps1)。
	//
	// 用裸 bool:安全值恰好不是零值,但零值 = 旧语义,整段 `Store:` 缺失(本地 yaml)时行为不变
	// (与下面 SchemaConfig 用 *bool 的理由相反:那里零值是危险值)。
	Required bool `json:",optional"`
}

// ── 玩家归属 / 落点映射的提交点(docs/design/player-route-mapping-durability-proposal.md §4、§11.2,第 1a 批)──

// RouteStoreModeRedis:与 2026-10-12 之前逐字相同,mapping Redis 里那段 SETNX Lua 就是提交点。缺省值。
const RouteStoreModeRedis = "redis"

// RouteStoreModeDBFirst:先在全局库 player_route 提交(store.RouteStore.RegisterHome),再按 maprev
// set-if-newer 发布到 mapping Redis 并等 WAITAOF 确认落盘(routing/route_publish.go)。
//
// 切换顺序见 proposal §13.4(P1 冻结窗口 + 导入 + 审计 → P2 切 db_first → P3 Arm → P4 新版 merge_zone → P5 读方)。
// 第 2 批起 merge_zone 以 -route-source=db 库先写,并在 zone_merge_fence 持有围栏;W1 在库内锁围栏分片复核,发布脚本
// KEYS[4] 的 merge:in_progress 是围栏的镜像(merge_zone 写、对账 leader 按库对账),作为发布时的第二道复核。
// 旧版 merge_zone 二进制直写 Redis、不写库,P4 起禁用。
const RouteStoreModeDBFirst = "db_first"

// RouteStore 段各键的默认值(Normalize 照抄这一份;etc/data_service.yaml 写的就是它们)。
// FenceShards 的默认值与上限在 shared/placement(DefaultRouteFenceShards / MaxRouteFenceShards),那里同时是围栏持有者的契约;
// 它是唯一的例外:etc/data_service.yaml 写的是 MySQL 档的 16,不是默认值 8(2026-10-13,见该文件的注释)。
const (
	DefaultRouteStoreMaxOpenConn        = 20
	DefaultRouteStoreMaxIdleConn        = 5
	DefaultRouteStorePublishAttempts    = 3
	DefaultRouteStoreWaitAOFNumLocal    = 1
	DefaultRouteStoreWaitAOFNumReplicas = 0
	DefaultRouteStoreWaitAOFTimeoutMs   = 1500
)

// RouteStore 段的上界(Validate 用)。WaitAOFTimeoutMs 的上界 = 服务端默认超时 2000ms 减去库这一段与回包的余量
// (yaml 没配 Timeout,go-zero 默认 2000ms;go-redis 默认 ReadTimeout 3s,更大的值会先被客户端读超时截断)。
const (
	MaxRouteStorePublishAttempts    = 5
	MaxRouteStoreWaitAOFNumReplicas = 16
	MaxRouteStoreWaitAOFTimeoutMs   = 1500
)

// RouteStore.PublishRedisPoolSize 的缺省值与上界(A2-1 / LR-4,2026-10-20;见该字段的注释)。
//
// 缺省 20 的论证(W1 并发):每次 W1 的发布 + WAITAOF 从取到连接起占住它,直到 WAITAOF 返回;appendfsync everysec 下
// 平均约 0.5s、最长约 1s 多(上限 WaitAOFTimeoutMs)。单副本 W1 吞吐上限 ≈ 池大小 / 平均占用 ≈ 2 × 池大小 次/s。
// 改动前发布与读共用 go-zero 的 mapping 池(K8s 上 GOMAXPROCS=2 时 10 × 2 = 20 条),W1 的上限约 40 次/s/副本,
// 代价是建角尖峰把读饿死。缺省取同一个 20:W1 的吞吐上限不低于改动前,而读路径的 20 条从此不再被 W1 占用。
// 更大的建角峰值(容量模型的开服尖峰)靠加池补不上(要几百条),属于 WAITAOF 组提交 / 拓扑的设计问题,以压测为准再定。
// 上界 128:单副本 128 条已是共享 Redis 连接总账里最大的一项,再大应先改设计而不是加连接。
const (
	DefaultRouteStorePublishRedisPoolSize = 20
	MaxRouteStorePublishRedisPoolSize     = 128
)

// RouteStoreConfig 是映射提交点的运行参数。整段可缺失。
//
// 【为什么零值 = "没配"、默认值由 Normalize 填】理由同 PlayerNameConfig:go-zero 不下钻一个未出现的
// optional 结构,default 标签一个都不会回填。WaitAOF 两项用 *int 而不是 int,理由同 SchemaConfig.AutoMigrate:
// 它们的安全默认值(本地落盘 1)不是零值,而"显式写 0"又有合法语义(不发 WAITAOF,只给没开 AOF 的环境 / 单测),
// 必须把"没写"与"写了 0"分开。
//
// 【为什么配错拒启而不是纠正】理由同 PlayerNameConfig.Validate:Mode 拼成 `DB_FIRST` / `db-first` 被悄悄当成
// redis,运维以为切了提交点而实际没切,这种偏差不报错也没有日志。Mode 不论取哪个值,任何一项非法都拒启。
type RouteStoreConfig struct {
	// Mode:redis(缺省)| db_first,大小写敏感。
	Mode string `json:",optional"`
	// FenceShards:zone_merge_fence 每个 zone 的分片数,[1, placement.MaxRouteFenceShards];0 = 默认 8。
	// 建角只锁 player_id % N 那一片;各副本取值可以不同(围栏持有者按上限写满全部分片)。
	FenceShards int `json:",optional"`
	// MaxOpenConn / MaxIdleConn:route store **独立**连接池(会话级 RC + 锁等待 1s,不能与别的 store 共用池)。
	MaxOpenConn int `json:",optional"`
	MaxIdleConn int `json:",optional"`
	// PublishAttempts:一次发布(脚本 + WAITAOF)的尝试上限,[1, 5];只有网络错误与"落盘副本数不足"才重试。
	PublishAttempts int `json:",optional"`
	// WaitAOFNumLocal / WaitAOFNumReplicas:WAITAOF 的两个参数。nil = 默认 1 / 0;两者都显式写 0 = 不发 WAITAOF。
	// redis-ha 档(有副本)NumReplicas 要写 1。
	WaitAOFNumLocal    *int `json:",optional"`
	WaitAOFNumReplicas *int `json:",optional"`
	// WaitAOFTimeoutMs:WAITAOF 的等待上限(毫秒),[1, 1500];0 = 默认 1500。实际取 min(本值, ctx 剩余 − 100ms)。
	// 默认不是 1000:appendfsync everysec 下本地确认最长要等到下一次 fsync(约 1s)再加事件循环延迟,
	// 1000ms 正压在边界上,会系统性地抬高"已提交未确认"的比例(规格 d 节 M2)。
	WaitAOFTimeoutMs int `json:",optional"`
	// PublishRedisPoolSize:db_first 下 W1 发布 + WAITAOF 专用 go-redis 客户端的连接池上限,[1, 128];0 = 缺省 20
	// (论证见 DefaultRouteStorePublishRedisPoolSize)。连接参数取 MappingRedis(同一个实例),但池与 go-zero 的 mapping 客户端
	// (GetPlayerHomeZone / BatchGet / 玩家锁 / 读脚本)分开、不经 go-zero 熔断器,建角尖峰不再饿住读路径(routing/publish_redis.go)。
	// redis 模式不建这个客户端、不开连接;db_first 下是每个副本额外占 mapping Redis 的连接上限,要进共享 Redis 连接总账。
	PublishRedisPoolSize int `json:",optional"`
	// ReadRequiresReady:第 3 批读方门禁(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.2)。
	// true 时 GetPlayerHomeZone / BatchGetPlayerHomeZone / 内部路由的 home 查询只在就绪标记绑定的就是本次读所在的
	// mapping Redis 实例(placement.VerifyRouteReady = Trusted)时信任缓存,否则回源全局库 player_route(库故障回 Unavailable,
	// 绝不 NotFound)。只许 Mode=db_first(redis 模式下库不是真源,没有可回源的东西);启动期试跑一次读脚本,跑不起来(或 route
	// store 因别的原因没装配起来)不论 Store.Required 取值都拒启(M1-5,svc.ServiceContext.RouteReadGateUnassembled)。缺省 false = 与今天逐字相同。所有副本应取同一值;打开前提是对账 leader 已挂标记(规格 §8 P5),
	// 回退时先关它、再关 ArmReadyMarker、再切回 redis(规格 §13.5 第 1 条)。打开时还要求 Reconcile.Enabled 不是 false
	// (没有 leader 就没人撤 / 维护就绪标记,见 RouteReconcileConfig.Enabled;Validate 拒启)。
	ReadRequiresReady bool `json:",optional"`
	// Reconcile 是 db_first 下对账 leader 的运行参数(第 1b-2 批,见 RouteReconcileConfig)。整段可缺失,缺失 = 全部默认值;
	// redis 模式下不构造 leader,但非法值照样拒启(与本段其它键同口径)。
	Reconcile RouteReconcileConfig `json:",optional"`
}

// Normalize 返回一份把"没配"(零值 / nil)换成默认值的副本。负数不纠正,交给 Validate 拒绝。
// 刻意返回值而不是原地改(同 PlayerNameConfig.Normalize);指针字段只在 nil 时换成新指针,不改调用方指向的值。
func (r RouteStoreConfig) Normalize() RouteStoreConfig {
	r.Reconcile = r.Reconcile.Normalize()
	if r.Mode == "" {
		r.Mode = RouteStoreModeRedis
	}
	if r.FenceShards == 0 {
		r.FenceShards = int(placement.DefaultRouteFenceShards)
	}
	if r.MaxOpenConn == 0 {
		r.MaxOpenConn = DefaultRouteStoreMaxOpenConn
	}
	if r.MaxIdleConn == 0 {
		r.MaxIdleConn = DefaultRouteStoreMaxIdleConn
	}
	if r.PublishAttempts == 0 {
		r.PublishAttempts = DefaultRouteStorePublishAttempts
	}
	if r.WaitAOFNumLocal == nil {
		v := DefaultRouteStoreWaitAOFNumLocal
		r.WaitAOFNumLocal = &v
	}
	if r.WaitAOFNumReplicas == nil {
		v := DefaultRouteStoreWaitAOFNumReplicas
		r.WaitAOFNumReplicas = &v
	}
	if r.WaitAOFTimeoutMs == 0 {
		r.WaitAOFTimeoutMs = DefaultRouteStoreWaitAOFTimeoutMs
	}
	if r.PublishRedisPoolSize == 0 {
		r.PublishRedisPoolSize = DefaultRouteStorePublishRedisPoolSize
	}
	return r
}

// Validate 检查(通常是 Normalize 之后的)取值。任何一项非法都返回 error,与 Mode 无关:
// redis 模式下这些键暂时不生效,但一份"切到 db_first 才会炸"的配置不该等到切换那一刻才被发现。
func (r RouteStoreConfig) Validate() error {
	if r.Mode != RouteStoreModeRedis && r.Mode != RouteStoreModeDBFirst {
		return fmt.Errorf("RouteStore.Mode %q must be exactly %q or %q (case-sensitive; a typo must not silently fall back to redis)",
			r.Mode, RouteStoreModeRedis, RouteStoreModeDBFirst)
	}
	if r.FenceShards < 1 || r.FenceShards > int(placement.MaxRouteFenceShards) {
		return fmt.Errorf("RouteStore.FenceShards must be in [1, %d], got %d", placement.MaxRouteFenceShards, r.FenceShards)
	}
	if r.MaxOpenConn <= 0 {
		return fmt.Errorf("RouteStore.MaxOpenConn must be positive, got %d", r.MaxOpenConn)
	}
	if r.MaxIdleConn < 0 || r.MaxIdleConn > r.MaxOpenConn {
		// database/sql 会**静默**把 idle 压到 open,配置与实际行为从此对不上(同 PlayerNameConfig)。
		return fmt.Errorf("RouteStore.MaxIdleConn must be in [0, MaxOpenConn=%d], got %d", r.MaxOpenConn, r.MaxIdleConn)
	}
	if r.PublishAttempts < 1 || r.PublishAttempts > MaxRouteStorePublishAttempts {
		return fmt.Errorf("RouteStore.PublishAttempts must be in [1, %d], got %d", MaxRouteStorePublishAttempts, r.PublishAttempts)
	}
	if local := r.NumLocal(); local != 0 && local != 1 {
		return fmt.Errorf("RouteStore.WaitAOFNumLocal must be 0 or 1, got %d", local)
	}
	if replicas := r.NumReplicas(); replicas < 0 || replicas > MaxRouteStoreWaitAOFNumReplicas {
		return fmt.Errorf("RouteStore.WaitAOFNumReplicas must be in [0, %d], got %d", MaxRouteStoreWaitAOFNumReplicas, replicas)
	}
	if r.WaitAOFTimeoutMs < 1 || r.WaitAOFTimeoutMs > MaxRouteStoreWaitAOFTimeoutMs {
		return fmt.Errorf("RouteStore.WaitAOFTimeoutMs must be in [1, %d] (WAITAOF timeout 0 blocks forever; the cap leaves room inside the 2000ms server budget), got %d",
			MaxRouteStoreWaitAOFTimeoutMs, r.WaitAOFTimeoutMs)
	}
	if r.ReadRequiresReady && r.Mode != RouteStoreModeDBFirst {
		// 回退顺序写反(先切回 redis、读方开关还开着)时在这里拒启,而不是让读方在没有真源可回源的模式下静默跑(规格 §13.5)。
		return fmt.Errorf("RouteStore.ReadRequiresReady=true requires Mode=%q (in %q mode the mapping Redis is the source of truth and there is no player_route to fall back to; turn ReadRequiresReady off first), got Mode=%q",
			RouteStoreModeDBFirst, RouteStoreModeRedis, r.Mode)
	}
	if r.ReadRequiresReady && !r.Reconcile.On() {
		// A1-6 / A2-2 / M1-1 / LR-6(2026-10-20):Enabled=false 时本副本不构造 leader。就绪标记没有 TTL、读方只核身份不看新鲜度,
		// 全服都这样配时现存标记无人撤销(M4)、无人维护(孤儿行补发、M3 回档检测、淘汰 / DEBUG 自检全停),读方却继续判 Trusted,
		// 缺键被当成权威缺席(login 退回建角 zone、go/db 写进 legacy 库)。所以读方开着时不许急停 leader;顺序见报错文案。
		// go/db 的 Placement.RequireRouteReady 与 guild 的 MergeMarkerRedis.RequireRouteReady 本服务校验不到,只能靠 runbook 约束。
		return errors.New("RouteStore.ReadRequiresReady=true requires RouteStore.Reconcile.Enabled not false: without the reconcile leader " +
			"nobody withdraws or maintains mapping:route:ready (no disarm withdrawal, no orphan-row republish, no rollback detection, " +
			"no eviction / debug self-check) while readers keep trusting it. Before an emergency stop of the leader, turn off every reader switch first " +
			"(RouteStore.ReadRequiresReady, go/db Placement.RequireRouteReady, guild MergeMarkerRedis.RequireRouteReady), then set " +
			"Reconcile.ArmReadyMarker=false and wait for data_service_route_reconcile_event_total{event=\"marker_dropped_disarmed\"}, and only then set Enabled=false")
	}
	if r.PublishRedisPoolSize < 1 || r.PublishRedisPoolSize > MaxRouteStorePublishRedisPoolSize {
		return fmt.Errorf("RouteStore.PublishRedisPoolSize must be in [1, %d], got %d", MaxRouteStorePublishRedisPoolSize, r.PublishRedisPoolSize)
	}
	if err := r.Reconcile.validate(r.Mode); err != nil {
		return err
	}
	return nil
}

// ReconcileEnabled 报告是否要构造对账 leader:Mode=db_first 且 Reconcile.Enabled 不是 false(nil = true)。
// redis 模式下 Redis 才是提交点、库只是 P1 时刻的快照,往缓存推库值会回填缓存缺的键,所以一律不构造(规格 scout D1)。
func (r RouteStoreConfig) ReconcileEnabled() bool {
	return r.DBFirst() && r.Reconcile.On()
}

// DBFirst 报告提交点是否在全局库(db_first)。
func (r RouteStoreConfig) DBFirst() bool {
	return r.Mode == RouteStoreModeDBFirst
}

// WaitsForAOF 报告发布之后要不要发 WAITAOF:两项都是 0(显式写的)才不发。
func (r RouteStoreConfig) WaitsForAOF() bool {
	return r.NumLocal() > 0 || r.NumReplicas() > 0
}

// NumLocal 是 WAITAOF 的 numlocal;没配(nil)= 默认 1。
func (r RouteStoreConfig) NumLocal() int {
	if r.WaitAOFNumLocal == nil {
		return DefaultRouteStoreWaitAOFNumLocal
	}
	return *r.WaitAOFNumLocal
}

// NumReplicas 是 WAITAOF 的 numreplicas;没配(nil)= 默认 0。
func (r RouteStoreConfig) NumReplicas() int {
	if r.WaitAOFNumReplicas == nil {
		return DefaultRouteStoreWaitAOFNumReplicas
	}
	return *r.WaitAOFNumReplicas
}

// ── 对账 leader(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §4、§13.1,第 1b-2 批)──

// RouteStore.Reconcile 段各键的默认值(Normalize 照抄这一份;etc/data_service.yaml 写的就是它们)。
const (
	DefaultRouteReconcileLeaseMs                  = 10000
	DefaultRouteReconcileRenewIntervalMs          = 3000
	DefaultRouteReconcileTickIntervalMs           = 1000
	DefaultRouteReconcileIncrementalIntervalMs    = 30000
	DefaultRouteReconcileIncrementalOverlapMs     = 15000
	DefaultRouteReconcileRepairLookbackMs         = 60000
	DefaultRouteReconcileRollbackToleranceMs      = 0
	DefaultRouteReconcilePushBatch                = 500
	DefaultRouteReconcilePushMaxRowsPerSec        = 20000
	DefaultRouteReconcileRedisPoolSize            = 4
	DefaultRouteReconcileFreezeTimeoutMs          = 5000
	DefaultRouteReconcileArmingBlockedReportLimit = 1000
)

// RouteStore.Reconcile 段的取值范围(Validate 用,闭区间)。
const (
	MinRouteReconcileLeaseMs, MaxRouteReconcileLeaseMs                             = 3000, 60000
	MinRouteReconcileTickIntervalMs, MaxRouteReconcileTickIntervalMs               = 200, 10000
	MinRouteReconcileIncrementalIntervalMs, MaxRouteReconcileIncrementalIntervalMs = 1000, 600000
	MinRouteReconcileIncrementalOverlapMs, MaxRouteReconcileIncrementalOverlapMs   = 5000, 600000
	MinRouteReconcileRepairLookbackMs, MaxRouteReconcileRepairLookbackMs           = 15000, 86400000
	MaxRouteReconcileRollbackToleranceMs                                           = 60000
	// MaxRouteReconcilePushBatch = store.RouteBatchLimit(1000;config 不 import store,两处各写一份,store 端超限直接拒绝)。
	MaxRouteReconcilePushBatch                                             = 1000
	MinRouteReconcilePushMaxRowsPerSec, MaxRouteReconcilePushMaxRowsPerSec = 100, 1000000
	MaxRouteReconcileRedisPoolSize                                         = 32
	// MinRouteReconcileFreezeTimeoutMs = store.RouteFreezeTimeoutMin(5s):TiDB 把 route_meta 设成缓存表后,冻结写要等读租约。
	MinRouteReconcileFreezeTimeoutMs, MaxRouteReconcileFreezeTimeoutMs = 5000, 60000
	MaxRouteReconcileRedisFailureReleaseMs                             = 600000
	MaxRouteReconcileArmingBlockedReportLimit                          = 100000
)

// RouteReconcileConfig 是对账 leader 的运行参数(RouteStore.Reconcile)。零值 / nil = 没配,由 Normalize 填默认值
// (理由同 RouteStoreConfig:go-zero 不下钻未出现的 optional 结构)。
//
// 运行前提与上线纪律(规格 §8、§13.1 第 6 条):
//   - 只在 Mode=db_first 时构造(ReconcileEnabled);redis 模式下不建客户端、不抢租约、不碰任何控制键;
//   - ArmReadyMarker 是**全服单值**:所有共享同一全局库的 data_service 副本取同一个值;P3 门禁(二次导入、audit -require-maprev = 0)
//     满足之后才改 true;回退时先关全部读方开关,再改 false(false = leader 每拍撤掉现存标记,撤销信任);
//   - 共享同一全局库的副本必须指向同一个 MappingRedis(不变量 I4;指错时另一实例上的标记身份不符,读方不信任,安全由 M1 兜住)。
type RouteReconcileConfig struct {
	// Enabled:nil = true。写 false 是急停:本副本不构造 leader —— 不再补发孤儿行、不再做回档检测、不做淘汰 / DEBUG 自检,
	// **也不撤现存的就绪标记**(撤标记是 leader 在 ArmReadyMarker=false 时每拍做的事,leader 不在就没人做)。
	// 所以只许在读方全部关闭之后急停(2026-10-20,Validate 拒绝 ReadRequiresReady=true 时写 false):顺序是先关全部读方开关
	// (RouteStore.ReadRequiresReady、go/db Placement.RequireRouteReady、guild MergeMarkerRedis.RequireRouteReady)→ Arm=false 并看到
	// marker_dropped_disarmed → 最后才 Enabled=false。go/db 与 guild 的开关本服务校验不到,靠 runbook 约束。
	Enabled *bool `json:",optional"`
	// ArmReadyMarker:true 才挂就绪标记(mapping:route:ready)。只许 Mode=db_first 且 Enabled 不是 false。
	ArmReadyMarker bool `json:",optional"`
	// LeaseMs:租约时长,[3000, 60000]。本地在「续约发出时刻 + LeaseMs − LeaseMs/5」之后自行降级。
	LeaseMs int `json:",optional"`
	// RenewIntervalMs:leader 续约间隔,≤ (LeaseMs − LeaseMs/5)/2,且 ≥ TickIntervalMs(续约在拍里做)。
	RenewIntervalMs int `json:",optional"`
	// TickIntervalMs:一拍的间隔,[200, 10000]。
	TickIntervalMs int `json:",optional"`
	// IncrementalIntervalMs:两次增量推送之间至少隔多久,[1000, 600000]。
	IncrementalIntervalMs int `json:",optional"`
	// IncrementalOverlapMs:增量起点 = cache_verified_ms − 本值(饱和到 0),[5000, 600000]。
	// 不变量 I1:任何映射写事务从写 updated_at_ms 到提交小于本值。
	IncrementalOverlapMs int `json:",optional"`
	// RepairLookbackMs:身份变化后的回看修补起点 = min(标记水位, cache_verified_ms) − 本值(饱和到 0),[15000, 86400000],且 ≥ IncrementalOverlapMs。
	RepairLookbackMs int `json:",optional"`
	// RollbackToleranceMs:水位键的水位比库水位大多少才判回档,nil = 0(负责人 10-17 拍板),[0, 60000]。
	RollbackToleranceMs *int `json:",optional"`
	// PushBatch:每批扫 / 推的行数,[1, 1000](= store.RouteBatchLimit)。
	PushBatch int `json:",optional"`
	// PushMaxRowsPerSec:推送限速(行 / 秒),[100, 1000000]。
	PushMaxRowsPerSec int `json:",optional"`
	// RedisPoolSize:leader 独立 go-redis 连接池上限,[1, 32](不经 go-zero 熔断器;只有当选 leader 才真正建连接)。
	// 共享 Redis 连接总账要把它算进去(第 1b-3 批 k8s_deploy)。
	RedisPoolSize int `json:",optional"`
	// AssumeNoEviction:mapping Redis 禁用了 CONFIG(托管 Redis)时按 noeviction 处理。只影响淘汰策略这一项,
	// enable-debug-command 读不到时仍拒绝挂标记(规格 §13.1 第 5 条:不是 no 就拒绝)。
	AssumeNoEviction bool `json:",optional"`
	// FreezeTimeoutMs:含冻结写(leader 冻结映射写者)的每拍事务时限,[5000, 60000],且 ≤ LeaseMs − 2·(LeaseMs/5)
	// (冻结前本地剩余租约不够 FreezeTimeout + Safety 就先续约,租约要撑得住一次冻结)。
	FreezeTimeoutMs int `json:",optional"`
	// RedisFailureReleaseMs:连续读不到 mapping Redis(身份 / 控制键)超过它就主动让出租约,让连得上的副本接手;
	// 0 = 默认等于 LeaseMs。[TickIntervalMs, 600000]。
	RedisFailureReleaseMs int `json:",optional"`
	// ArmingBlockedReportLimit:挂标记被拒(armingBlocked)时记下的未决 player_id 上限,[1, 100000];超出只计数,
	// 解除之后要重新全量预热(记不全就不能只复读记下的那些)。
	ArmingBlockedReportLimit int `json:",optional"`
}

// Normalize 返回一份把没配的键换成默认值的副本(负数不纠正,交给 Validate)。
func (rc RouteReconcileConfig) Normalize() RouteReconcileConfig {
	if rc.Enabled == nil {
		v := true
		rc.Enabled = &v
	}
	if rc.LeaseMs == 0 {
		rc.LeaseMs = DefaultRouteReconcileLeaseMs
	}
	if rc.RenewIntervalMs == 0 {
		rc.RenewIntervalMs = DefaultRouteReconcileRenewIntervalMs
	}
	if rc.TickIntervalMs == 0 {
		rc.TickIntervalMs = DefaultRouteReconcileTickIntervalMs
	}
	if rc.IncrementalIntervalMs == 0 {
		rc.IncrementalIntervalMs = DefaultRouteReconcileIncrementalIntervalMs
	}
	if rc.IncrementalOverlapMs == 0 {
		rc.IncrementalOverlapMs = DefaultRouteReconcileIncrementalOverlapMs
	}
	if rc.RepairLookbackMs == 0 {
		rc.RepairLookbackMs = DefaultRouteReconcileRepairLookbackMs
	}
	if rc.RollbackToleranceMs == nil {
		v := DefaultRouteReconcileRollbackToleranceMs
		rc.RollbackToleranceMs = &v
	}
	if rc.PushBatch == 0 {
		rc.PushBatch = DefaultRouteReconcilePushBatch
	}
	if rc.PushMaxRowsPerSec == 0 {
		rc.PushMaxRowsPerSec = DefaultRouteReconcilePushMaxRowsPerSec
	}
	if rc.RedisPoolSize == 0 {
		rc.RedisPoolSize = DefaultRouteReconcileRedisPoolSize
	}
	if rc.FreezeTimeoutMs == 0 {
		rc.FreezeTimeoutMs = DefaultRouteReconcileFreezeTimeoutMs
	}
	if rc.RedisFailureReleaseMs == 0 {
		rc.RedisFailureReleaseMs = rc.LeaseMs
	}
	if rc.ArmingBlockedReportLimit == 0 {
		rc.ArmingBlockedReportLimit = DefaultRouteReconcileArmingBlockedReportLimit
	}
	return rc
}

// On 报告 Enabled 是不是开着(nil = true)。只看本段;是否真的构造 leader 要看 RouteStoreConfig.ReconcileEnabled(还要 db_first)。
func (rc RouteReconcileConfig) On() bool {
	return rc.Enabled == nil || *rc.Enabled
}

// ToleranceMs 是回档检测容差(RollbackToleranceMs,nil = 0)。
func (rc RouteReconcileConfig) ToleranceMs() uint64 {
	if rc.RollbackToleranceMs == nil || *rc.RollbackToleranceMs < 0 {
		return 0
	}
	return uint64(*rc.RollbackToleranceMs)
}

// LeaseSafetyMs 是本地自行降级的提前量 = LeaseMs/5(scout D6)。
func (rc RouteReconcileConfig) LeaseSafetyMs() int {
	return rc.LeaseMs / 5
}

// validate 检查(通常是 Normalize 之后的)取值;mode 是 RouteStore.Mode。与 Mode 无关一律校验范围,
// 只有 ArmReadyMarker=true 额外要求 db_first 且 Enabled 不是 false(否则回退顺序写反时会以为开着挂标记)。
func (rc RouteReconcileConfig) validate(mode string) error {
	type bound struct {
		name     string
		got      int
		min, max int
	}
	for _, b := range []bound{
		{"LeaseMs", rc.LeaseMs, MinRouteReconcileLeaseMs, MaxRouteReconcileLeaseMs},
		{"TickIntervalMs", rc.TickIntervalMs, MinRouteReconcileTickIntervalMs, MaxRouteReconcileTickIntervalMs},
		{"IncrementalIntervalMs", rc.IncrementalIntervalMs, MinRouteReconcileIncrementalIntervalMs, MaxRouteReconcileIncrementalIntervalMs},
		{"IncrementalOverlapMs", rc.IncrementalOverlapMs, MinRouteReconcileIncrementalOverlapMs, MaxRouteReconcileIncrementalOverlapMs},
		{"RepairLookbackMs", rc.RepairLookbackMs, MinRouteReconcileRepairLookbackMs, MaxRouteReconcileRepairLookbackMs},
		{"PushBatch", rc.PushBatch, 1, MaxRouteReconcilePushBatch},
		{"PushMaxRowsPerSec", rc.PushMaxRowsPerSec, MinRouteReconcilePushMaxRowsPerSec, MaxRouteReconcilePushMaxRowsPerSec},
		{"RedisPoolSize", rc.RedisPoolSize, 1, MaxRouteReconcileRedisPoolSize},
		{"FreezeTimeoutMs", rc.FreezeTimeoutMs, MinRouteReconcileFreezeTimeoutMs, MaxRouteReconcileFreezeTimeoutMs},
		{"ArmingBlockedReportLimit", rc.ArmingBlockedReportLimit, 1, MaxRouteReconcileArmingBlockedReportLimit},
	} {
		if b.got < b.min || b.got > b.max {
			return fmt.Errorf("RouteStore.Reconcile.%s must be in [%d, %d], got %d", b.name, b.min, b.max, b.got)
		}
	}
	if rc.RollbackToleranceMs == nil {
		return fmt.Errorf("RouteStore.Reconcile.RollbackToleranceMs is unset (call Normalize first)")
	}
	if tol := *rc.RollbackToleranceMs; tol < 0 || tol > MaxRouteReconcileRollbackToleranceMs {
		return fmt.Errorf("RouteStore.Reconcile.RollbackToleranceMs must be in [0, %d], got %d", MaxRouteReconcileRollbackToleranceMs, tol)
	}
	safety := rc.LeaseSafetyMs()
	if maxRenew := (rc.LeaseMs - safety) / 2; rc.RenewIntervalMs < rc.TickIntervalMs || rc.RenewIntervalMs > maxRenew {
		return fmt.Errorf("RouteStore.Reconcile.RenewIntervalMs must be in [TickIntervalMs=%d, (LeaseMs - LeaseMs/5)/2=%d] "+
			"(two renewals fit in the locally trusted part of the lease; renewals happen inside ticks), got %d",
			rc.TickIntervalMs, maxRenew, rc.RenewIntervalMs)
	}
	if maxFreeze := rc.LeaseMs - 2*safety; rc.FreezeTimeoutMs > maxFreeze {
		return fmt.Errorf("RouteStore.Reconcile.FreezeTimeoutMs=%d must be <= LeaseMs - 2*(LeaseMs/5) = %d "+
			"(the lease must outlive one freeze transaction after a renewal); raise LeaseMs", rc.FreezeTimeoutMs, maxFreeze)
	}
	if rc.RepairLookbackMs < rc.IncrementalOverlapMs {
		return fmt.Errorf("RouteStore.Reconcile.RepairLookbackMs=%d must be >= IncrementalOverlapMs=%d (the repair lookback covers at least the incremental overlap)",
			rc.RepairLookbackMs, rc.IncrementalOverlapMs)
	}
	if rc.RedisFailureReleaseMs < rc.TickIntervalMs || rc.RedisFailureReleaseMs > MaxRouteReconcileRedisFailureReleaseMs {
		return fmt.Errorf("RouteStore.Reconcile.RedisFailureReleaseMs must be in [TickIntervalMs=%d, %d], got %d",
			rc.TickIntervalMs, MaxRouteReconcileRedisFailureReleaseMs, rc.RedisFailureReleaseMs)
	}
	if rc.ArmReadyMarker && (mode != RouteStoreModeDBFirst || !rc.On()) {
		return fmt.Errorf("RouteStore.Reconcile.ArmReadyMarker=true requires Mode=%q and Reconcile.Enabled not false (got Mode=%q, Enabled=%t); "+
			"when rolling back, set ArmReadyMarker=false first", RouteStoreModeDBFirst, mode, rc.On())
	}
	return nil
}

// SchemaConfig 建表策略。
type SchemaConfig struct {
	// AutoMigrate 缺省(nil)= true:启动时用 proto2mysql 按 proto 定义对五张表逐张
	// CreateOrUpdateTable(建表 / 补列 / 补主键),并预建 id_segment 的号段行。
	// 其中 id_segment 与 player_name 两张走 schema.go 的手写 bootstrap DDL
	// (proto2mysql 把 string 渲染成 MEDIUMTEXT,TEXT 列上建不了唯一键),
	// 迁移对它们只做列漂移校验。
	// 另有路由表(player_route / zone_merge_fence / zone_merged_into / route_meta / route_reconcile,
	// 2026-10-12 映射持久化第 1a 批与 G1a 热点修复)同样走手写 DDL,迁移按各自的 proto 只守列名与主键,
	// 并种 route_meta / route_reconcile(id=1)。
	// **redis 模式也依赖它们**:这一步失败时快照 / 流水 / 号段 / 名字四个 store 一并不装配(规格 g.3 第 8 条)。
	//
	// 生产必须显式设 false:存量大表上的 ALTER 会持有 MDL,阻塞期间所有 GM 回滚/流水查询停摆,
	// 这种事不该发生在某个副本的启动路径上;改为部署阶段显式跑一次 `data_service -f <yaml> -migrate`
	// (同一段代码、单进程、跑完即退)。设 false 时启动路径不碰任何 DDL。
	// 多副本同时启动本身是安全的:迁移由库级命名锁串行(store.acquireMigrateLock),后到的等先到的跑完。
	//
	// 为什么是 *bool 而不是 `bool json:",default=true"`:go-zero 的 mapping **不下钻**
	// 一个"整段 optional 且未出现"的嵌套结构,里面的 default 标签一个都不会回填
	// (与 go/db 的 BlobGuardConfig、login 的 IdSegment.Enabled 同一个坑)。用裸 bool
	// 时,一份没有 `Schema:` 段的 yaml 会得到 AutoMigrate=false —— 启动路径一条 DDL 都
	// 不跑,三个 store 打在一个没有表的库上,rollback / txlog / AllocateIdSegment 每一次
	// 调用都在运行期炸;而在收口手写 DDL 之前,store 自带的 CREATE TABLE IF NOT EXISTS
	// 还兜着这一层。指针把"没写"与"显式写了 false"区分开:nil = 用安全默认。
	//
	// 键名保持 AutoMigrate(不改成反向的 SkipAutoMigrate):
	// tools/scripts/k8s_deploy.ps1 的 data-service ConfigMap 会写死
	// `Schema: AutoMigrate: <dev 取服务 yaml / staging|prod 固定 false>`,改名会让那份
	// 生产配置变成一个被静默忽略的未知键,反而把生产切回启动期 DDL。
	AutoMigrate *bool `json:",optional"`
}

// ShouldAutoMigrate 是启动路径是否跑 DDL 的唯一判据:没配 = 建表(安全默认)。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// KafkaConfig 两条落库消费者的参数。分区数是不可变契约(kafkautil.EnsureTopics
// 会拒绝与 broker 现状不一致的值):transaction_log 6、snapshot 3。要改只能离线换代
// (TopicGeneration +1,停写 → 排空 → 三方同改),步骤见 docs/ops/audit-topic-generation-bump-runbook.md。
//
// 末尾五个键(Snapshot* 与 LagProbeIntervalMs,G2a)是 player_snapshot 消费者的批量 / 并发旋钮与 lag 探针。
// 它们没有 default 标签、零值 = "没配",由 Normalize 填默认、ValidateConsumers 拒绝越界值 ——
// 理由同 RouteStoreConfig:go-zero 不下钻一个未出现的 optional 结构,default 标签在整段 Kafka 缺失时一个都不回填。
type KafkaConfig struct {
	Brokers []string `json:",optional"`

	// TopicGeneration 是分区契约的**代号**,进 topic 名(见 EffectiveTransactionLogTopic)。
	// 与 go/db、go/login 的 Kafka.TopicGeneration 同一个机制,但这里**连 1 也带后缀**。
	//
	// 为什么必须带:broker 是 auto.create.topics.enable=true + num.partitions=1
	// (deploy/docker-compose.yml KAFKA_NUM_PARTITIONS=1),而 C++ scene 一发消息就把
	// 裸名字的 topic 自动建成 1 分区。EnsureTopics 拿 6/3 的契约去比,永远得到
	// "partition contract mismatch",两条消费者一条都起不来,唯一症状是每 30s 一条
	// Error 日志 —— 流水/快照全被保留期吃掉。带代号的名字与自动建出来的那两个裸名
	// topic 互不相干,第一次就能按契约建出 6/3 分区。
	//
	// 分区数要改 = 把这个数 +1(换一批新 topic、老 topic 排空后删),**绝不**原地扩分区:
	// 扩分区会重映射 key=player_id 的哈希,同一玩家的流水顺序就断了。
	TopicGeneration uint32 `json:",default=1"`

	// TransactionLogTopic 是**基名**;实际使用的名字是 EffectiveTransactionLogTopic()。
	// 基名必须与 C++ 生产者的 topic 常量一致(全局单 topic,key=player_id)。
	TransactionLogTopic         string `json:",default=transaction_log_topic"`
	TransactionLogPartitions    int32  `json:",default=6"`
	TransactionLogConsumerGroup string `json:",default=data_service-transaction-log"`

	// SnapshotTopic 同上。单条消息(整份 PlayerAllData)代码注释估几百 KB、未实测;是否攒批由下面的
	// SnapshotBatchMaxRows 决定,默认 1 = 逐条落、逐条提交(2026-10-12 之前的行为)。
	SnapshotTopic         string `json:",default=player_snapshot_topic"`
	SnapshotPartitions    int32  `json:",default=3"`
	SnapshotConsumerGroup string `json:",default=data_service-player-snapshot"`

	// RetentionMs 两个 topic 的保留期,默认 30 天。审计数据宁可积压不许丢:
	// 消费者停下(DB 长时间不可用)期间的消息要在这个窗口内被补消费掉。
	// 注意它不是先到的上限:broker 的每分区 log.retention.bytes(prod 512MiB)在开服速率下几十秒到几分钟就能到,
	// 积压超过它的未消费快照会被删(本服务只检测,见 data_service_kafka_consumer_retention_gap_messages)。
	RetentionMs int64 `json:",default=2592000000"`

	// SnapshotBatchMaxRows:player_snapshot 每个分区一批最多多少条消息(含被跳过的坏消息),[1, MaxSnapshotBatchMaxRows]。
	// 0 = 没配 = 1(逐条,现状)。>1 时预读剔除已存在的 guid、按 guid 升序一条普通多行 INSERT 落库
	// (撞 1062 转逐行,见 store.InsertSnapshotsIfGuidAbsent),成功后提交本批最高 offset;批量失败自动逐条兜底。
	SnapshotBatchMaxRows int `json:",optional"`
	// SnapshotBatchMaxBytes:一批 data 字节的**软上限**,[MinSnapshotBatchMaxBytes, MaxSnapshotBatchMaxBytes];
	// 0 = 没配 = DefaultSnapshotBatchMaxBytes。每加入一条后检查,累计 >= 它即落库,所以一批最多超出「一条消息」的大小。
	SnapshotBatchMaxBytes int `json:",optional"`
	// SnapshotFlushIntervalMs:批非空后最多再等多久凑批,[0, MaxSnapshotFlushIntervalMs];0 = 不等,只拼已排队的消息。
	SnapshotFlushIntervalMs int `json:",optional"`
	// SnapshotMaxInflightFlushes:本实例同时落库的分区数上限(信号量),[1, max(1, SnapshotMySQL.MaxOpenConn−1)];0 = 没配 = 1。
	// 上限留一条连接:快照池同时服务 GM 快照 / 回档读,积压时不能被消费者占满;本旋钮也不新增任何 MySQL 连接。
	SnapshotMaxInflightFlushes int `json:",optional"`
	// LagProbeIntervalMs:消费组 lag / 保留期丢失量探针的周期,0 = 关,否则 [MinLagProbeIntervalMs, MaxLagProbeIntervalMs]。
	// 只读 broker 元数据、不碰消费与落库;关掉时积压 / 保留期告警全部是哑的。
	LagProbeIntervalMs int `json:",optional"`
}

// Kafka 段 G2a 五个键的默认值与边界(Normalize / ValidateConsumers 照抄这一份;etc/data_service.yaml 与 runbook 引用它)。
const (
	DefaultSnapshotBatchMaxBytes = 4 << 20
	// MaxSnapshotBatchMaxRows 与 store.MaxSnapshotBatchRows 相等(单测钉住):超过它的批会被 store 拒绝,
	// 每一批都退回逐条兜底,静默变慢。
	MaxSnapshotBatchMaxRows  = 256
	MinSnapshotBatchMaxBytes = 64 << 10
	// MaxSnapshotBatchMaxBytes 远低于 MySQL 8.0 默认 max_allowed_packet 64MiB,也低于 TiDB txn-total-size-limit 100MB;
	// 但**高于** TiKV raft-entry-max-size 默认 8MB —— 今天 TiDB 上建不出唯一键、批量不生效(退路逐行);将来 TiDB 形态的
	// 去重让批量在 TiDB 上生效时,批字节须 <= 4MiB(超了由消费者的逐条兜底接住,不会卡死,只是变慢)。
	// 调大前还要核对目标库的 innodb_redo_log_capacity:单事务 BLOB 不得超过其 10%(默认 100MB → 10MB)。
	MaxSnapshotBatchMaxBytes   = 32 << 20
	MaxSnapshotFlushIntervalMs = 5000
	MinLagProbeIntervalMs      = 5000
	MaxLagProbeIntervalMs      = 600000

	// snapshotPoolFallbackMaxOpen 是 SnapshotMySQL.MaxOpenConn <= 0 时快照池的实际上限,
	// 与 store/mysql.go openMySQLWithDSN 的回落值相同(那里写死 5)。
	snapshotPoolFallbackMaxOpen = 5
)

// Normalize 返回一份把 G2a 五个键的"没配"(零值)换成默认值的副本;已有键(TopicGeneration 等)的语义不在这里动。
// 负数不纠正,交给 ValidateConsumers 拒绝(同 RouteStoreConfig.Normalize)。
func (k KafkaConfig) Normalize() KafkaConfig {
	if k.SnapshotBatchMaxRows == 0 {
		k.SnapshotBatchMaxRows = 1
	}
	if k.SnapshotBatchMaxBytes == 0 {
		k.SnapshotBatchMaxBytes = DefaultSnapshotBatchMaxBytes
	}
	if k.SnapshotMaxInflightFlushes == 0 {
		k.SnapshotMaxInflightFlushes = 1
	}
	// SnapshotFlushIntervalMs / LagProbeIntervalMs 的零值本身就是合法语义(不等待 / 不探),不改。
	return k
}

// ValidateConsumers 检查(通常是 Normalize 之后的)G2a 五个键。snapshotPoolMaxOpen 传 SnapshotMySQL.MaxOpenConn
// (<= 0 时按快照池的实际回落值 5 算)。任何一项越界都返回 error,与 Brokers 是否为空无关:
// 一份"开 Kafka 才炸"的配置不该等到开的那一刻才被发现(同 RouteStoreConfig.Validate)。
func (k KafkaConfig) ValidateConsumers(snapshotPoolMaxOpen int) error {
	if k.SnapshotBatchMaxRows < 1 || k.SnapshotBatchMaxRows > MaxSnapshotBatchMaxRows {
		return fmt.Errorf("Kafka.SnapshotBatchMaxRows must be in [1, %d], got %d", MaxSnapshotBatchMaxRows, k.SnapshotBatchMaxRows)
	}
	if k.SnapshotBatchMaxBytes < MinSnapshotBatchMaxBytes || k.SnapshotBatchMaxBytes > MaxSnapshotBatchMaxBytes {
		return fmt.Errorf("Kafka.SnapshotBatchMaxBytes must be in [%d, %d], got %d",
			MinSnapshotBatchMaxBytes, MaxSnapshotBatchMaxBytes, k.SnapshotBatchMaxBytes)
	}
	if k.SnapshotFlushIntervalMs < 0 || k.SnapshotFlushIntervalMs > MaxSnapshotFlushIntervalMs {
		return fmt.Errorf("Kafka.SnapshotFlushIntervalMs must be in [0, %d], got %d", MaxSnapshotFlushIntervalMs, k.SnapshotFlushIntervalMs)
	}
	pool := snapshotPoolMaxOpen
	if pool <= 0 {
		pool = snapshotPoolFallbackMaxOpen
	}
	maxInflight := max(1, pool-1)
	if k.SnapshotMaxInflightFlushes < 1 || k.SnapshotMaxInflightFlushes > maxInflight {
		return fmt.Errorf("Kafka.SnapshotMaxInflightFlushes must be in [1, %d] (SnapshotMySQL pool %d minus one connection kept for GM snapshot / rollback reads), got %d",
			maxInflight, pool, k.SnapshotMaxInflightFlushes)
	}
	if k.LagProbeIntervalMs != 0 && (k.LagProbeIntervalMs < MinLagProbeIntervalMs || k.LagProbeIntervalMs > MaxLagProbeIntervalMs) {
		return fmt.Errorf("Kafka.LagProbeIntervalMs must be 0 (off) or in [%d, %d], got %d",
			MinLagProbeIntervalMs, MaxLagProbeIntervalMs, k.LagProbeIntervalMs)
	}
	return nil
}

// EffectiveTransactionLogTopic 是消费者与 EnsureTopics 真正使用的 topic 名。
func (k KafkaConfig) EffectiveTransactionLogTopic() string {
	return topicForGeneration(k.TransactionLogTopic, k.TopicGeneration)
}

// EffectiveSnapshotTopic 同上。
func (k KafkaConfig) EffectiveSnapshotTopic() string {
	return topicForGeneration(k.SnapshotTopic, k.TopicGeneration)
}

// topicForGeneration 拼 `<base>_g<N>`。base 为空时返回空,交给 kafka.Start 的 validate
// 报错,而不是拼出一个只有后缀的名字。generation=0 只可能来自"整段 Kafka 未出现"
// (go-zero 不回填未出现结构里的 default),此时按第一代 1 处理:名字保持规范。
func topicForGeneration(base string, generation uint32) string {
	if base == "" {
		return ""
	}
	if generation == 0 {
		generation = 1
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

type DevRedisConfig struct {
	Host     string `json:",optional"`
	Password string `json:",optional"`
	DB       int    `json:",default=0"`
	// PoolSize 是 go-redis 连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,
	// 随 CPU limit 漂移;负数 routing.NewRouter 拒绝。K8s 上 tools/scripts/k8s_deploy.ps1 从 etc/data_service.yaml
	// 镜像 DevRedis.PoolSize 并要求正整数,Redis 连接总账读同一个键。
	// MappingRedis 是 go-zero 的 redis.RedisConf,没有池参数(go-zero 固定 go-redis 缺省 10 × GOMAXPROCS、MinIdleConns 8),
	// 总账仍按缺省池记它。
	PoolSize int `json:",optional"`
}

// RegionConfig 是 Regions 生效时(DevRedis.Host 为空)的一段 zone → data Redis 集群表。
//
// 这张表只在 routing.NewRouter 启动时建一次,不热更新、不带版本:把已有 zone 改到另一个 Region 等于搬数据,
// 必须停写、搬完该 zone 玩家的 player:{id}:*、所有副本同时换配置后再起。滚动发布期间新旧副本会把同一玩家
// 路由到不同集群,所以不要滚动改它。
type RegionConfig struct {
	Id    uint32
	Zones []uint32
	Redis RedisClusterConfig
}

// ValidateRegions 校验 Regions 生效时的 zone → 集群表,routing.NewRouter 在建任何集群客户端之前调用;
// 配了 DevRedis 时 Regions 整段被忽略,不校验。
//   - 至少一个 Region:否则每个玩家都路由失败,服务起来也只能回 ErrCodeRedis;
//   - 每个 Region 至少一个地址、至少一个 zone;
//   - 同一个 zone 不能出现在两个 Region 里:旧实现后写的静默覆盖先写的,而 cmd/debug_fetch 取第一个命中,
//     排障工具会报出与运行时不同的集群。
func ValidateRegions(regions []RegionConfig) error {
	if len(regions) == 0 {
		return fmt.Errorf("Regions is empty and DevRedis.Host is not set: no zone can be routed to a data Redis")
	}
	owner := make(map[uint32]uint32)
	for _, region := range regions {
		if len(region.Redis.Addrs) == 0 {
			return fmt.Errorf("Regions[id=%d]: Redis.Addrs is empty", region.Id)
		}
		if len(region.Zones) == 0 {
			return fmt.Errorf("Regions[id=%d]: Zones is empty", region.Id)
		}
		for _, zone := range region.Zones {
			if prev, dup := owner[zone]; dup {
				return fmt.Errorf("zone %d is listed in both Regions[id=%d] and Regions[id=%d]: a zone must map to exactly one data Redis cluster",
					zone, prev, region.Id)
			}
			owner[zone] = region.Id
		}
	}
	return nil
}

type RedisClusterConfig struct {
	Addrs    []string
	Password string `json:",optional"`
}

type SnapshotMySQLConfig struct {
	RequireTiDB bool   `json:",optional"`
	Host        string `json:",default=127.0.0.1:3306"`
	User        string `json:",default=appuser"`
	Password    string `json:",default=apppass123"`
	DBName      string `json:",default=testdb"`
	MaxOpenConn int    `json:",default=5"`
	MaxIdleConn int    `json:",default=2"`
}

// ── 玩家名字注册表(docs/design/guild-phase2/03-names.md §3.6a)──────────────

// PlayerNameConfig 是名字注册表的运行参数:连接池 + 三个时间窗。
//
// 【为什么不用 `json:",default=..."` 标签】go-zero 的 mapping **不下钻**一个"整段
// optional 且未出现"的嵌套结构,里面的 default 一个都不会回填(同 SchemaConfig.AutoMigrate
// 与 IdSegmentConfig 注释里的坑)。而本段的零值全部是**危险值**,不是安全值:
// CacheTTL=0 会让每次回填都被 SetPlayerNames 当成"不写无过期键"而报错,
// ReleaseWindow=0 会让建角失败后的条件释放什么都删不掉(留孤儿)。
// 所以默认值由 Normalize() 在代码里填,零值 = "没配",与"显式配了 0"不作区分:
// 这三个时间窗都没有"配成 0"的合法语义,不需要那一层区分。
//
// 数值本身**不是玩法数值**(玩法侧的 2–12 字在配表 RoleNameRule 里,由 login 把关),
// 而是运维旋钮:压测时调连接池、事故时调缓存时长,所以留在 yaml 而不是配表。
type PlayerNameConfig struct {
	// MaxOpenConn / MaxIdleConn 是名字 store **独立**连接池的大小。
	// 独立的理由:建角路径上每次都有一条同步 INSERT,与快照/号段共用 SnapshotMySQL
	// 那 5 条连接时,一次 GM 批量回滚就能把建角卡住。
	MaxOpenConn int `json:",optional"`
	MaxIdleConn int `json:",optional"`

	// ReleaseWindow 是**不带 x-admin-token** 的 ReleasePlayerName 能触碰的最大登记年龄:
	// 只有 created_ms >= now-ReleaseWindow 的行才允许删。
	//
	// 这道窗是安全边界,不是性能旋钮:这个 RPC 在集群内网无鉴权可达,没有窗口的话,
	// 任何能拨到 data_service 的进程都能把一个在役角色的名字释放掉,而名字一释放就可能
	// 立刻被别人占走,不可回滚。窗口只需覆盖 login 建角失败后的补偿(立即一次 + 约 10s
	// 后一次),10 分钟已经极其宽裕。运维清孤儿走 admin token 那条路,不限时间。
	ReleaseWindow time.Duration `json:",optional"`

	// CacheTTL 是 player:name:{id} 正缓存的存活时间。v1 不支持改名,名字**内容**不会变,
	// 所以可以给很长;上限的意义只在"名字被释放且 DEL 缓存失败"时封顶脏值。
	CacheTTL time.Duration `json:",optional"`

	// NegativeCacheTTL 是"库里确实没有这个 id 的名字"这一事实的缓存时间。
	// 它挡的是帮会成员列表里那些早于本功能建的老角色 —— 没有负缓存,每次开帮会面板
	// 都会把它们全部回源查库一遍。给得短是因为它会被"刚建好的角色"打脸:虽然 Reserve
	// 成功后会用 SET 覆盖掉负缓存,但覆盖发生在 data_service 这一侧,别的路径建的号
	// (回档恢复)只能等它过期。
	NegativeCacheTTL time.Duration `json:",optional"`
}

// 默认值集中在这里,yaml 与文档照抄这一份(设计 §3.6a)。
const (
	DefaultPlayerNameMaxOpenConn      = 20
	DefaultPlayerNameMaxIdleConn      = 5
	DefaultPlayerNameReleaseWindow    = 10 * time.Minute
	DefaultPlayerNameCacheTTL         = 24 * time.Hour
	DefaultPlayerNameNegativeCacheTTL = 60 * time.Second

	// MaxPlayerNameReleaseWindow 是 ReleaseWindow 的硬上限。超过它一律拒绝装配
	// (见 Validate),而不是"配多大算多大":这个窗口越大,无鉴权释放能够到的在役
	// 角色就越多。24 小时已经远超任何一次建角补偿的时间尺度,写得比它还大只可能是
	// 把它当成了缓存时长之类的东西填错了地方。
	MaxPlayerNameReleaseWindow = 24 * time.Hour
)

// Normalize 返回一份把"没配"(零值)换成默认值的副本。
//
// 刻意返回值而不是原地改:调用方(svc.NewServiceContext)拿到的 Config 是值拷贝,
// 显式赋值一次比藏在指针方法里的副作用好追。负数不在这里纠正 —— 那是配错了,
// 该由 Validate 拒掉,而不是被悄悄改成默认值(见 Validate 的理由)。
func (p PlayerNameConfig) Normalize() PlayerNameConfig {
	if p.MaxOpenConn == 0 {
		p.MaxOpenConn = DefaultPlayerNameMaxOpenConn
	}
	if p.MaxIdleConn == 0 {
		p.MaxIdleConn = DefaultPlayerNameMaxIdleConn
	}
	if p.ReleaseWindow == 0 {
		p.ReleaseWindow = DefaultPlayerNameReleaseWindow
	}
	if p.CacheTTL == 0 {
		p.CacheTTL = DefaultPlayerNameCacheTTL
	}
	if p.NegativeCacheTTL == 0 {
		p.NegativeCacheTTL = DefaultPlayerNameNegativeCacheTTL
	}
	return p
}

// Validate 检查(通常是 Normalize 之后的)取值是否可用。
//
// 【为什么配错要拒绝而不是纠正】纠正意味着运维看到的 yaml 与进程实际行为不一致,
// 而这几个键里有一个是安全边界(ReleaseWindow)。拒绝的代价是名字 store 不装配 →
// 建角被拒(fail-closed,症状明显、当场能看出来);纠正的代价是一个悄悄放宽了的
// 释放窗口在事故发生前谁都不知道。
func (p PlayerNameConfig) Validate() error {
	if p.MaxOpenConn <= 0 {
		return fmt.Errorf("PlayerName.MaxOpenConn must be positive, got %d", p.MaxOpenConn)
	}
	if p.MaxIdleConn < 0 {
		return fmt.Errorf("PlayerName.MaxIdleConn must not be negative, got %d", p.MaxIdleConn)
	}
	if p.MaxIdleConn > p.MaxOpenConn {
		// database/sql 会**静默**把 idle 压到 open,配置与实际行为从此对不上。
		return fmt.Errorf("PlayerName.MaxIdleConn (%d) must not exceed MaxOpenConn (%d): database/sql silently clamps it",
			p.MaxIdleConn, p.MaxOpenConn)
	}
	if p.ReleaseWindow <= 0 {
		return fmt.Errorf("PlayerName.ReleaseWindow must be positive, got %v", p.ReleaseWindow)
	}
	if p.ReleaseWindow > MaxPlayerNameReleaseWindow {
		return fmt.Errorf("PlayerName.ReleaseWindow %v exceeds the hard cap %v: an unauthenticated release must not reach names older than a create-player compensation",
			p.ReleaseWindow, MaxPlayerNameReleaseWindow)
	}
	if p.CacheTTL <= 0 {
		return fmt.Errorf("PlayerName.CacheTTL must be positive, got %v", p.CacheTTL)
	}
	if p.NegativeCacheTTL <= 0 {
		return fmt.Errorf("PlayerName.NegativeCacheTTL must be positive, got %v", p.NegativeCacheTTL)
	}
	return nil
}
