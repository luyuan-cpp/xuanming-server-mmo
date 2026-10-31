package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"

	"guild/internal/data"
	"shared/assetopwake"
	"shared/authoritysql"
	"shared/idsegment"
)

// MaxRpcTimeoutMs:帮会二期契约 §2,Timeout ≤ 路由服 ForwardTimeoutMs(5000)− 1000。
const MaxRpcTimeoutMs int64 = 4000

// MaxApplicationQuotaPerMinute 防止误配置实质关闭入帮申请频率保护。
const MaxApplicationQuotaPerMinute uint32 = 600

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

// SchemaConf 建表策略(port-decisions D-14 §4)。AutoMigrate 为 nil(没写)或 false 时常驻启动仅核对；显式迁移才跑
// schemamigrate.Up;false:只跑只读 Plan,不干净就拒绝启动。用 *bool 的理由同 go/trade/internal/config.SchemaConf:
// 要把"没写"与"显式写 false"区分开。
type SchemaConf struct {
	AutoMigrate *bool `json:",optional"`
}

type Config struct {
	// 全局目录与帮会必须位于同一TiDB集群；成员新增事务跨schema锁同一角色行。
	AccountRoleSchema    string    `json:",optional"`
	MatchAuthoritySchema string    `json:",optional"`
	GlobalLoginSessions  bool      `json:",optional"`
	LoginRedis           RedisConf `json:",optional"`
	zrpc.RpcServerConf
	RedisClient        RedisConf    `json:"RedisClient"`
	PlayerLocatorRedis RedisConf    `json:"PlayerLocatorRedis"`
	MySQL              MySQLConf    `json:"MySQL"`
	Node               NodeConf     `json:"Node"`
	Registry           RegistryConf `json:"Registry"`
	Cache              CacheConf    `json:"Cache"`
	Kafka              KafkaConf    `json:"Kafka"`
	TableDir           string       `json:",default=../../generated/tables"`
	// 入帮申请尝试的每玩家固定 60 秒配额，包含重复刷新与仓储拒绝；不能用待审总量代替。
	ApplicationQuotaPerMinute uint32 `json:"ApplicationQuotaPerMinute,default=10"`

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

	// MatchRpc 是 match 内部服务(MatchInternal.StartActivityBattle)的 gRPC 客户端,帮会同道历练在全员同意后
	// 用它确认开战(docs/design/guild-phase2/06-activities.md §6.22 / §6.26)。
	// **整段缺失或没有目标 = 历练未开放**(视图显示未开放,两个历练写 RPC 回 kGuildActivityNotOpen);
	// 灯会、团圆不受影响。match 是弱依赖:svc 建客户端时强制 NonBlock,match 没起时 guild 照常起服,
	// 开战回 tip kGuildTrialServiceBusy(不是 gRPC 错误,否则客户端进重连隔离)。
	MatchRpc zrpc.RpcClientConf `json:"MatchRpc,optional"`

	// Activity 是帮会活动后台任务的节律与历练结果消费配置,见 ActivityConf。
	Activity ActivityConf `json:"Activity,optional"`
}

// ActivityConf 是同道历练(B6b)的后台节律与结果消费配置(06-activities.md §6.22)。
//
// # 整段缺失时的取值
//
// go-zero 不下钻一个整段 optional 且未出现的嵌套结构(同 AssetOpConf、idsegment.Conf 的说明),没写 Activity 段时
// 四个秒数全是 0。它们**只经下面的四个方法读取**,0 一律回落到设计默认值:待入队循环与巡检器在"历练此刻关着"时
// 也要跑(关之前结算留下的待入队物品、在途对局行仍要处理完),不能因为少抄一段配置就起不来或不跑。
// TrialResult 相反:整段缺失 = Enabled 为 false = 历练未开放(见 TrialResultConf)。
type ActivityConf struct {
	// TrialResultOverdueSeconds:对局登记多久之后仍没有结果,巡检器就去读 battle 落下的结果记录兜底。
	// 默认 420 = match 的 BattleMaxDurationSeconds(300)+ 120。
	TrialResultOverdueSeconds uint32 `json:",default=420"`
	// TrialAbandonSeconds:登记多久之后仍没有结果、也没有结果记录,就判 EXPIRED(迟到的结果仍可结算)。
	// 必须 ≥ TrialResultOverdueSeconds + 600:battle 的重发窗口(10s × 30 次 = 300s)之外再留余量。
	TrialAbandonSeconds uint32 `json:",default=3600"`
	// TrialSweepIntervalSeconds:巡检器两轮的间隔。多副本靠 guild 全局 Redis 上的租约互斥,租约略短于它。
	TrialSweepIntervalSeconds uint32 `json:",default=60"`
	// OwedLoopIntervalSeconds:待入队物品循环两轮的间隔(结算时未决指令已满者的物品,等窗口有空位再入队)。
	OwedLoopIntervalSeconds uint32 `json:",default=10"`
	// TrialResult 是历练对局结果(Kafka match-results)的消费配置。
	TrialResult TrialResultConf `json:",optional"`
}

// TrialResultConf 是历练对局结果的 Kafka 消费配置。battle 把带活动上下文的 BattleResultEvent 发到
// match 的 ResultTopic(key = battle_id),guild 以独立消费组消费并结算。
//
// # 为什么 Enabled 没有 default=true(与 06 §6.22 原稿不同)
//
// 理由同 idsegment.Conf:optional 的段整块缺失时 go-zero 不回填内层 default。写成 default=true 就会出现
// "段写了但漏了 Enabled → 开;段整个没写 → 关"这种看配置猜不出来的分叉。规则收成一条:
// **只有显式写了 Enabled: true 才消费**;没写 = 不消费 = 历练未开放(没有结算就不许开战)。
type TrialResultConf struct {
	// Enabled 打开结果消费。它与 Kafka.Brokers 一起决定历练是否开放,见 Config.TrialResultActive。
	Enabled bool `json:",optional"`
	// Topic 必须与 match 的 ResultTopic、battle 的 kMatchResultsTopic 一致。
	Topic string `json:",default=match-results"`
	// Partitions 必须与 match 的 ResultTopicPartitions 相同:topic 由先起的一方建出来,分区数建后不可变。
	Partitions int32 `json:",default=3"`
	// GroupID 是消费组。guild 是全局服务、topic 也是全局的:本地多开的 guild 必须同组分摊分区,
	// 所以 etc/guild.yaml 里这个值**刻意不加引号**(go_services.ps1 -Zone 只给带引号的 GroupID 加 _zN 后缀)。
	GroupID string `json:",default=guild-trial"`
}

// 同道历练配置的默认值与合法区间(06 §6.22)。默认值同时写在上面的 struct tag 里(tag 只能是字面量),
// config_test.go 的 TestActivityConfDefaultsMatchTags 守住两处相等。
const (
	// MaxMatchRpcTimeoutMs:调 match 的单次上限。开战那次请求在它之前还要做房间原子判定、在线复核与成员复核,
	// 之后还要登记对局与回视图,全部挤在 RequestBudget(Timeout − 500)里;给 match 的不能超过 2s。
	MaxMatchRpcTimeoutMs int64 = 2000
	// MinMatchRpcTimeoutMs:MatchRpc.Timeout 的下限,镜像 logic 的 minMatchBudget(config 不能 import logic;
	// logic/activity_trial_test.go 的 TestTrialMinMatchBudgetMirrorsConfig 守住两者相等)。
	// logic 给 match 的预算 = min(MatchRpc.Timeout, 请求剩余 − 500ms),不足 minMatchBudget 就**不调 match**、
	// 直接回 kGuildTrialServiceBusy。Timeout 配得比它小,预算恒不够:进程能起、历练显示开放、能建房能同意,
	// 而每一次开战都必回"服务繁忙"—— 这种"半开"必须炸在启动期(同下面 AssetOp 对号段的要求)。
	MinMatchRpcTimeoutMs int64 = 300

	defaultTrialResultOverdueSeconds uint32 = 420
	defaultTrialAbandonSeconds       uint32 = 3600
	defaultTrialSweepIntervalSeconds uint32 = 60
	defaultOwedLoopIntervalSeconds   uint32 = 10

	// 下限 60:比它更短,正常打着的对局也会被当成"结果超时"去读记录,每轮白读一遍 Redis。
	minTrialResultOverdueSeconds uint32 = 60
	// battle 重发窗口 300s 之外再留 300s 余量(06 §6.22)。
	trialAbandonHeadroomSeconds uint32 = 600
	// 上限 7 天 = battle 结果记录与 Kafka match-results 的保留期:过了它结果不可能再来,再等没有意义,
	// 而行一直停在 STARTED 会占着巡检器每轮的候选名额。
	maxTrialAbandonSeconds uint32 = 7 * 24 * 3600
	// 巡检间隔下限 10:租约取"间隔 − 5s",再短租约就没有意义;上限 600:结果丢失后最多多等这么久才兜底。
	minTrialSweepIntervalSeconds uint32 = 10
	maxTrialSweepIntervalSeconds uint32 = 600
	minOwedLoopIntervalSeconds   uint32 = 1
	maxOwedLoopIntervalSeconds   uint32 = 60
)

// secondsOrDefault:0 = 没写(整段缺失时 default 不生效),取默认值。
func secondsOrDefault(v, def uint32) time.Duration {
	if v == 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

// TrialResultOverdue 是"结果超时"的判定时长;0(没写)取默认 420s。
func (a ActivityConf) TrialResultOverdue() time.Duration {
	return secondsOrDefault(a.TrialResultOverdueSeconds, defaultTrialResultOverdueSeconds)
}

// TrialAbandon 是判 EXPIRED 的时长;0(没写)取默认 3600s。
func (a ActivityConf) TrialAbandon() time.Duration {
	return secondsOrDefault(a.TrialAbandonSeconds, defaultTrialAbandonSeconds)
}

// TrialSweepInterval 是巡检间隔;0(没写)取默认 60s。
func (a ActivityConf) TrialSweepInterval() time.Duration {
	return secondsOrDefault(a.TrialSweepIntervalSeconds, defaultTrialSweepIntervalSeconds)
}

// OwedLoopInterval 是待入队物品循环的间隔;0(没写)取默认 10s。
func (a ActivityConf) OwedLoopInterval() time.Duration {
	return secondsOrDefault(a.OwedLoopIntervalSeconds, defaultOwedLoopIntervalSeconds)
}

// validate 校验生效值(0 已按默认值折算),所以整段缺失也能通过。
func (a ActivityConf) validate() error {
	overdue, abandon := a.TrialResultOverdue(), a.TrialAbandon()
	if overdue < time.Duration(minTrialResultOverdueSeconds)*time.Second {
		return fmt.Errorf("Activity.TrialResultOverdueSeconds(%d)必须 ≥ %d:更短会把正常进行中的对局当成结果超时",
			a.TrialResultOverdueSeconds, minTrialResultOverdueSeconds)
	}
	headroom := time.Duration(trialAbandonHeadroomSeconds) * time.Second
	if abandon < overdue+headroom || abandon > time.Duration(maxTrialAbandonSeconds)*time.Second {
		return fmt.Errorf("Activity.TrialAbandonSeconds(生效值 %d)必须在 [TrialResultOverdueSeconds + %d = %d, %d] 内:"+
			"下限留出 battle 的重发窗口,上限是结果记录与 Kafka 的 7 天保留期",
			int64(abandon/time.Second), trialAbandonHeadroomSeconds, int64((overdue+headroom)/time.Second), maxTrialAbandonSeconds)
	}
	if v := a.TrialSweepInterval(); v < time.Duration(minTrialSweepIntervalSeconds)*time.Second ||
		v > time.Duration(maxTrialSweepIntervalSeconds)*time.Second {
		return fmt.Errorf("Activity.TrialSweepIntervalSeconds(%d)必须在 [%d, %d] 内",
			a.TrialSweepIntervalSeconds, minTrialSweepIntervalSeconds, maxTrialSweepIntervalSeconds)
	}
	if v := a.OwedLoopInterval(); v < time.Duration(minOwedLoopIntervalSeconds)*time.Second ||
		v > time.Duration(maxOwedLoopIntervalSeconds)*time.Second {
		return fmt.Errorf("Activity.OwedLoopIntervalSeconds(%d)必须在 [%d, %d] 内",
			a.OwedLoopIntervalSeconds, minOwedLoopIntervalSeconds, maxOwedLoopIntervalSeconds)
	}
	if a.TrialResult.Enabled {
		if a.TrialResult.Topic == "" || a.TrialResult.GroupID == "" {
			return errors.New("Activity.TrialResult.Enabled=true 要求 Topic 与 GroupID 非空")
		}
		if a.TrialResult.Partitions < 1 {
			return fmt.Errorf("Activity.TrialResult.Partitions(%d)必须 ≥ 1,且与 match 的 ResultTopicPartitions 相同",
				a.TrialResult.Partitions)
		}
	}
	return nil
}

// MatchRpcConfigured 报告 MatchRpc 是否指向了任何目标(etcd key / 直连端点 / target)。
// svc 据此决定建不建 MatchInternal 客户端;没有目标 = 历练未开放。
func (c Config) MatchRpcConfigured() bool {
	return hasRpcTarget(c.MatchRpc)
}

// TrialResultActive 报告历练结果消费是否会真的跑起来:显式 Enabled 且配了 Kafka Brokers。
// 为假时历练必须视为未开放 —— **没有结算就不许开战**:否则玩家打完一局,帮贡、资金、物品都无人发放。
// Brokers 为空不拒绝启动(本地不起 Kafka 是正常形态),由 guild.go 在 Enabled 而不 Active 时打一条告警。
func (c Config) TrialResultActive() bool {
	return c.Activity.TrialResult.Enabled && len(c.Kafka.Brokers) > 0
}

// TrialResultRequiresDurability 报告历练结果 topic 是否要核验 RF3 严格策略。结算要不要耐久和 SQL 用哪一档无关:
// 只有本地 dev/test 的 mysql 档放行(本地 Kafka 通常单 broker);生产 mysql 档同样严格,省钱档不能顺带丢结算。
func (c Config) TrialResultRequiresDurability() bool {
	if authoritysql.Backend(c.MySQL.Backend) != authoritysql.BackendMySQL {
		return true
	}
	return c.Mode != service.DevMode && c.Mode != service.TestMode
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
	// Workers 是**每个副本**同时在途的资产 RPC 上限。scene 的资产 RPC 在 scene 的 EventLoop 上同步执行,
	// 调大会挤占逻辑帧,把 scene 的进场 / 开战请求拖在后面(assetop.LoopConfig.Workers 注释)。
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
	// LedgerReadMinAttempts 连续几次投不出去之后才读已落盘账本(B5d-1 起由 guild.go 的 AttachPersistedLedger 接上;没配 DataServiceRpc 时不读)。
	LedgerReadMinAttempts int `json:",default=3"`

	// ── 清理(goroutine guild.asset_op_cleanup,§5.22)──

	// CleanupEnabled 打开终态行 / 过期计数行清理。与 Enabled 独立:通道关着时仍可以清历史行。
	CleanupEnabled bool `json:",optional"`
	// CleanupIntervalMinutes 两轮清理的间隔(每个副本都跑,删除幂等,启动有随机初始延迟)。
	CleanupIntervalMinutes int `json:",default=10"`
	// TerminalRetentionDays 终态行保留天数。它同时是 B5d 回档检查可追溯窗口的下界
	// (07-rollback-fail-closed.md §7.4.2),调小之前先确认那边的窗口。
	// 默认值同时写在 DefaultTerminalRetentionDays(tag 只能是字面量;config_test.go 守住两处相等)。
	TerminalRetentionDays int `json:",default=30"`
	// CounterRetentionDays 每日 / 每周计数行保留天数(只影响历史周期,当期行永不被删)。
	CounterRetentionDays int `json:",default=30"`

	// ── 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6;映射进 assetop.LoopConfig 的同名项)──
	// 与上面的循环参数一样只在 Enabled 时校验;段出现而没写这些键时按 default 回填。

	// OfflineAfterAttempts 进入离线停放档的已投递次数门槛:本次结局是本地 NOT_HERE 且位置键不存在、attempts 达到它,
	// 下一次投递推到约 OfflineBackoffMs 之后(玩家上线由落地唤醒兜住)。0 = 关闭停放档。
	OfflineAfterAttempts int `json:",default=5"`
	// OfflineBackoffMs 停放档的间隔中值(±20% 抖动)。停放档开启时必须 ≥ MaxBackoffMs、≤ 24 小时。
	OfflineBackoffMs int `json:",default=1800000"`
	// AbandonAfterDays 自动放弃的年龄门槛(天):PENDING 且创建早于它的行,在玩家无位置、账本确认未见时终结为 ABANDONED,
	// 对侧账同 REJECTED / ABORTED。0 = 关闭;非 0 时 ≥ 30(放弃不可逆)。
	// **滚动升级**:旧版实例的发送不推进 last_send_ms,新二进制全量上线之前必须写 0,全量之后再设 180(设计 §6.3)。
	AbandonAfterDays int `json:",default=180"`
	// AbandonSafetyGapMs 放弃条件 4 的安全间隔:距该行最后一次可能的发送不足它就不放弃。开启放弃时 ≥ 600000 且 ≥ LeaseMs。
	AbandonSafetyGapMs int `json:",default=3600000"`
	// AbandonBatch 一轮放弃最多处理多少行(候选分页大小),开启放弃时取 [1, 1000]。
	AbandonBatch int `json:",default=200"`
	// AbandonIntervalMs 两轮放弃的间隔,开启放弃时取 [60000, 86400000]。
	AbandonIntervalMs int `json:",default=3600000"`
	// AuditIntervalMs 日对账(只读,刷新 assetop_pending_rows / assetop_pending_class_oldest_age_seconds)的间隔。
	// 0 = 不跑对账;非 0 时取 [60000, 604800000]。对账与放弃只在选主当选的一个副本上跑(svc.AssetPipeline)。
	AuditIntervalMs int `json:",default=86400000"`
	// Wake 是落地唤醒订阅(核心 Redis 频道 assetop:wake),见 AssetOpWakeConf。
	Wake AssetOpWakeConf `json:",optional"`
}

// AssetOpWakeConf 是落地唤醒订阅的参数(shared/assetopwake,设计 §5.3)。
//
// # 为什么三个键都是 optional、默认值走方法
//
// 理由同 ActivityConf:整段可选的嵌套结构缺失时 go-zero 不下钻、不回填 default。要做到"Wake 段没写 = 开、并发 4、超时 10s",
// 默认值只能由读取方法给出;Enabled 用 *bool 区分"没写"(nil = 开)与"显式写 false"(关)。
// 默认值的唯一来源是 shared/assetopwake 的 DefaultConcurrency / DefaultWakeTimeout,这里不另抄数字。
type AssetOpWakeConf struct {
	// Enabled:nil(没写)或 true = 订阅;显式 false = 不订阅,PENDING 行只按原退避 / 停放档重投(只慢不错)。
	Enabled *bool `json:",optional"`
	// Concurrency 本副本同时在跑的唤醒数上限,满了直接丢;0(没写)= assetopwake.DefaultConcurrency(4),否则 [1, 64]。
	Concurrency int `json:",optional"`
	// TimeoutMs 每次唤醒的总预算;0(没写)= assetopwake.DefaultWakeTimeout(10s)。生效值必须 ≥ 2 × OpBudgetMs
	// (先列出该玩家的未决行,之后剩余预算够一整个 OpBudget 才领下一行;理由见 validateLanding),且 ≤ 60000(关停最坏要等它)。
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

// DefaultTerminalRetentionDays:TerminalRetentionDays 的默认值(与 struct tag 同值)。
// 单独导出是因为 AssetOp 整段缺失时 go-zero 不回填 default、字段是 0,而回档检查(GuildInternal)在那种形态下
// 仍要有一个保留期可用 —— 见 svc.RollbackProofRetention。
const DefaultTerminalRetentionDays = 30

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

// 落地恢复配置的合法区间。与 assetop.LoopConfig.validate / validateAbandon 同形(停放上限 24h、放弃门槛 ≥ 30 天、
// 安全间隔 ≥ 10 分钟且 ≥ 租约、批 [1, 1000]),这里放过的配置不该在 assetop.NewLoop 里才炸;
// 另加的上下界(次数 / 天数 / 间隔的上限、间隔下限 1 分钟)只为挡住毫秒 / 秒写错一个量级。
const (
	maxOfflineAfterAttempts = 1000
	maxOfflineBackoffMs     = 86400000
	minAbandonAfterDays     = 30
	maxAbandonAfterDays     = 3650
	minAbandonSafetyGapMs   = 600000
	maxAbandonBatch         = 1000
	minMaintenanceTickMs    = 60000
	maxAbandonIntervalMs    = 86400000
	maxAuditIntervalMs      = 7 * 86400000
	maxWakeConcurrency      = 64
	maxWakeTimeoutMs        = 60000
	// minWakeTimeoutOpBudgets:唤醒超时下界 = 它 × OpBudgetMs,理由见 validateLanding。
	minWakeTimeoutOpBudgets = 2
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
	return a.validateLanding()
}

// validateLanding 校验落地恢复的停放档 / 自动放弃 / 日对账 / 唤醒参数;由 validateLoop 在最后调用(只在 Enabled 时)。
// 每一项都是"0 = 关闭"时其余从属参数不校验,与 assetop 的口径一致。
func (a AssetOpConf) validateLanding() error {
	if a.OfflineAfterAttempts < 0 || a.OfflineAfterAttempts > maxOfflineAfterAttempts {
		return fmt.Errorf("AssetOp.OfflineAfterAttempts(%d)必须在 [0, %d] 内(0 = 关闭停放档)",
			a.OfflineAfterAttempts, maxOfflineAfterAttempts)
	}
	if a.OfflineAfterAttempts > 0 && (a.OfflineBackoffMs < a.MaxBackoffMs || a.OfflineBackoffMs > maxOfflineBackoffMs) {
		return fmt.Errorf("AssetOp.OfflineBackoffMs(%d)必须在 [MaxBackoffMs(%d), %d] 内:停放比普通退避还勤等于没有停放,"+
			"超过 24 小时则唤醒丢失时玩家要等太久", a.OfflineBackoffMs, a.MaxBackoffMs, maxOfflineBackoffMs)
	}

	if a.AbandonAfterDays != 0 {
		if a.AbandonAfterDays < minAbandonAfterDays || a.AbandonAfterDays > maxAbandonAfterDays {
			return fmt.Errorf("AssetOp.AbandonAfterDays(%d)必须为 0(关闭)或在 [%d, %d] 内:放弃不可逆",
				a.AbandonAfterDays, minAbandonAfterDays, maxAbandonAfterDays)
		}
		if a.AbandonSafetyGapMs < minAbandonSafetyGapMs || a.AbandonSafetyGapMs < a.LeaseMs {
			return fmt.Errorf("AssetOp.AbandonSafetyGapMs(%d)必须 ≥ %d 且 ≥ LeaseMs(%d):间隔不足会让放弃撞上仍在途的请求",
				a.AbandonSafetyGapMs, minAbandonSafetyGapMs, a.LeaseMs)
		}
		if a.AbandonBatch < 1 || a.AbandonBatch > maxAbandonBatch {
			return fmt.Errorf("AssetOp.AbandonBatch(%d)必须在 [1, %d] 内", a.AbandonBatch, maxAbandonBatch)
		}
		if a.AbandonIntervalMs < minMaintenanceTickMs || a.AbandonIntervalMs > maxAbandonIntervalMs {
			return fmt.Errorf("AssetOp.AbandonIntervalMs(%d)必须在 [%d, %d] 内",
				a.AbandonIntervalMs, minMaintenanceTickMs, maxAbandonIntervalMs)
		}
	} else if a.AbandonAfterDays < 0 {
		return fmt.Errorf("AssetOp.AbandonAfterDays(%d)不得为负(0 = 关闭)", a.AbandonAfterDays)
	}

	if a.AuditIntervalMs != 0 && (a.AuditIntervalMs < minMaintenanceTickMs || a.AuditIntervalMs > maxAuditIntervalMs) {
		return fmt.Errorf("AssetOp.AuditIntervalMs(%d)必须为 0(不跑对账)或在 [%d, %d] 内",
			a.AuditIntervalMs, minMaintenanceTickMs, maxAuditIntervalMs)
	}

	if !a.Wake.On() {
		return nil
	}
	if c := a.Wake.EffectiveConcurrency(); c < 1 || c > maxWakeConcurrency {
		return fmt.Errorf("AssetOp.Wake.Concurrency(%d)必须为 0(默认 %d)或在 [1, %d] 内",
			a.Wake.Concurrency, assetopwake.DefaultConcurrency, maxWakeConcurrency)
	}
	// 唤醒超时下界取 2 × OpBudgetMs。Loop.WakePlayer 先用一次 Store 读列出该玩家的 PENDING 行
	// (GuildAssetStore.ListPendingByPlayer,子预算 storeReadBudget = 1000ms),之后每开始一行都要求剩余预算
	// ≥ 一整个 OpBudget。旧下界"> OpBudgetMs"只保证进门那一次预算检查能过:超时只比 OpBudget 多一点时,
	// 列完行剩下的不足一个 OpBudget,一行都不领、回 (0, nil) 计 none —— 每次唤醒都静默退化成"什么也没做"。
	// 取 2 × OpBudgetMs 而不是 OpBudgetMs + 1000 + 余量:OpBudgetMs ≥ minOpBudgetMs(1000)≥ 列行子预算,所以它总装得下
	// "一次列行 + 一整个 OpBudget",多出的 OpBudgetMs − 1000 留给等 Workers 名额与调度;也不必在这里另抄一份 data 层的子预算数字。
	timeout := a.Wake.EffectiveTimeout()
	minTimeout := minWakeTimeoutOpBudgets * time.Duration(a.OpBudgetMs) * time.Millisecond
	if timeout < minTimeout || timeout > time.Duration(maxWakeTimeoutMs)*time.Millisecond {
		return fmt.Errorf("AssetOp.Wake.TimeoutMs(生效值 %d)必须在 [%d × OpBudgetMs = %d, %d] 内:"+
			"唤醒先列出该玩家的未决行,剩余预算不够一整个 OpBudget 时一行也不领,唤醒静默退化",
			timeout.Milliseconds(), minWakeTimeoutOpBudgets, minTimeout.Milliseconds(), maxWakeTimeoutMs)
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

// ShouldAutoMigrate 识别显式自动迁移请求；常驻入口会拒绝true，没写默认只读核对。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate != nil && *c.Schema.AutoMigrate
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
	if c.MySQL.DataSourceEnv != "" {
		value, present := os.LookupEnv(c.MySQL.DataSourceEnv)
		if !present || strings.TrimSpace(value) == "" {
			return errors.New("MySQL.DataSourceEnv 指定的SQL连接串未注入")
		}
		// 环境变量配置代表唯一来源；缺失时禁止退回本地样例连接串。
		c.MySQL.DataSource = value
	}
	if _, err := authoritysql.ParseBackend(c.MySQL.Backend); err != nil {
		return fmt.Errorf("MySQL.Backend: %w", err)
	}
	if c.AccountRoleSchema != "" && strings.TrimSpace(c.MatchAuthoritySchema) == "" {
		return errors.New("MatchAuthoritySchema必须显式指向同事务域的Match/Team权威库")
	}
	if c.AccountRoleSchema != "" && (!c.GlobalLoginSessions || strings.TrimSpace(c.LoginRedis.Host) == "" || c.LoginRedis.DB < 0 || c.LoginRedis.PoolSize < 0) {
		return errors.New("角色归属互锁须显式声明全局LoginRedis，同库登录态缺失拒绝")
	}
	if maxOpen, _ := c.MySQL.EffectivePool(); maxOpen < 1 || maxOpen > 128 {
		return errors.New("MySQL.MaxOpenConn 必须在 [1, 128] 内")
	}
	// 连接池上限:负数没有合法含义(0 = go-redis 缺省 10 × GOMAXPROCS),见 RedisConf.PoolSize。
	for _, pool := range []struct {
		name string
		size int
	}{
		{"RedisClient", c.RedisClient.PoolSize},
		{"PlayerLocatorRedis", c.PlayerLocatorRedis.PoolSize},
	} {
		if pool.size < 0 {
			return fmt.Errorf("%s.PoolSize(%d)不能为负:0 = go-redis 缺省 10 × GOMAXPROCS", pool.name, pool.size)
		}
	}
	if c.ApplicationQuotaPerMinute < 1 || c.ApplicationQuotaPerMinute > MaxApplicationQuotaPerMinute {
		return fmt.Errorf("ApplicationQuotaPerMinute(%d)必须在 [1, %d] 内", c.ApplicationQuotaPerMinute, MaxApplicationQuotaPerMinute)
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
	// 帮会活动(B6a)与经济的每日计数都是"带上限的 upsert",靠 RowsAffected 区分新插(1)/ 累加(2)/ 已达上限(0)。
	// clientFoundRows=true 会把"值没变"也报成匹配行数 1:达上限后的再一次参与被当成成功,帮贡与物品多发。
	// data.WithLockWaitTimeout 建池时已强制关掉它(90-consistency Y-20 的防御);这里仍在启动期拒绝显式打开的配置,
	// 让"配置写的"与"实际生效的"不一致暴露在部署时,而不是被静默改写、日后有人照着配置去排障。
	if dsn.ClientFoundRows {
		return errors.New("MySQL.DataSource 不得设置 clientFoundRows=true:帮会计数 upsert 以 RowsAffected=0 判定已达上限,开启后会多发奖励(原文含口令,不打印)")
	}
	if c.MySQL.MaxOpenConn < 0 || c.MySQL.MaxIdleConn < 0 {
		return fmt.Errorf("MySQL.MaxOpenConn(%d)/ MaxIdleConn(%d)不得为负(0 = 取缺省值 %d / %d)",
			c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn, DefaultMySQLMaxOpenConn, DefaultMySQLMaxIdleConn)
	}
	if maxOpen, maxIdle := c.MySQL.EffectivePool(); maxIdle > maxOpen {
		return fmt.Errorf("MySQL.MaxIdleConn(生效值 %d)不得大于 MaxOpenConn(生效值 %d)", maxIdle, maxOpen)
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

	// 同道历练(B6b)。MatchRpc 只在配了目标时校验:整段缺失时 Timeout 是 0(go-zero 不回填 default),
	// 无条件校验会让所有还没配这段的环境起不来,而那时根本不会建客户端。
	// 下限不是"大于 0"而是 MinMatchRpcTimeoutMs:比它小的值能过"非 0"这一关,却让每一次开战都判预算不足(见该常量)。
	if c.MatchRpcConfigured() && (c.MatchRpc.Timeout < MinMatchRpcTimeoutMs || c.MatchRpc.Timeout > MaxMatchRpcTimeoutMs) {
		return fmt.Errorf("MatchRpc.Timeout(%d ms)必须在 [%d, %d] 内:低于下限时给 match 的预算恒不够,logic 一次都不会调它"+
			"(每次开战必回服务繁忙;0 = 不设客户端超时,同样不允许);上限见 MaxMatchRpcTimeoutMs 的说明",
			c.MatchRpc.Timeout, MinMatchRpcTimeoutMs, MaxMatchRpcTimeoutMs)
	}
	return c.Activity.validate()
}

type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password"`
	DB       int    `json:"DB"`
	// PoolSize 是 go-redis 连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,
	// 随 CPU limit 漂移;负数由 Validate 拒绝。guild 现在没有 K8s 清单,不进 Redis 连接总账;接入 K8s 时由
	// tools/scripts/k8s_deploy.ps1 镜像并记账。PlayerLocatorRedis 没写 Host 时回落 RedisClient(连同本字段)。
	// 落地恢复摘要专用客户端与申请配额池各自固定 4 条,不读本字段。
	PoolSize int `json:"PoolSize,optional"`
}

type MySQLConf struct {
	DataSource    string `json:"DataSource,optional"`
	DataSourceEnv string `json:",optional"`
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);非法值拒启。
	Backend     string `json:",default=tidb"`
	MaxOpenConn int    `json:",default=20"`
	MaxIdleConn int    `json:",optional"`
}

// 连接池预算与部署总账共用，强会话初始化仍由 authoritysql 负责。
const (
	DefaultMySQLMaxOpenConn = 20
	DefaultMySQLMaxIdleConn = 5
)

func (m MySQLConf) EffectivePool() (maxOpen, maxIdle int) {
	maxOpen, maxIdle = m.MaxOpenConn, m.MaxIdleConn
	if maxOpen == 0 {
		maxOpen = DefaultMySQLMaxOpenConn
	}
	if maxIdle == 0 {
		maxIdle = DefaultMySQLMaxIdleConn
	}
	return
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
