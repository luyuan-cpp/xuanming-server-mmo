package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shared/authoritysql"
	"shared/idsegment"
	"shared/playerredis"

	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// 排队、占用、成局、评分与组队的权威都在同一个 SQL 事务域,开发环境与生产走同一条路径(玩家数据方案 §5.4)。
	// 不标 optional:整段缺席或缺 Host / User / 两个跨域 schema 时加载期就失败,缺配置不能起服。
	GlobalMatch GlobalMatchConf
	// GlobalLoginSessions 必须为 true,LoginRedis 必须指向 login 同一实例 / DB:客户端请求的原角色身份只从这里核(Validate)。
	GlobalLoginSessions bool                          `json:",optional"`
	LoginRedis          playerredis.LoginReaderConfig `json:",optional"`
	Kafka               struct {
		Brokers []string
	}

	// MatchRedis:match 私有协调 key(spectate:* 观战索引、档位与配额)的存储,
	// 可配 Type: cluster(设计文档 cross-zone-matchmaking.md D2 双存储)。
	// 留空则回落到 Redis 段(本地单库形态)。
	MatchRedis redis.RedisConf `json:",optional"`

	// PlayerRedis:玩家域 Redis(docs/design/redis-player-store-cluster.md §5.7、§5.8):契约 key(player:{id}:location /
	// player:session:{id} / battle:lock:{id})与 player:{id}:sync_digest。
	// 读取走 EffectivePlayerRedis():没写 Addrs 时指向 Redis 段那一个实例,开发环境不用改。
	PlayerRedis playerredis.Config `json:",optional"`

	// ZoneId:C++ 约定 etcd 注册路径用的 zone id。MatchNodeService 不是
	// zone-scoped 节点类型,gate 连的是全 zone 的 match 实例;zone 只影响
	// etcd 注册路径,不影响匹配池(匹配池全局、不分 zone,设计决策 D1)。
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL:etcd 节点注册 keepalive 的租约 TTL(秒)。
	LeaseTTL int64 `json:",default=60"`

	// IdSegment:battle_id / challenge_id / team_id 的号段配置(biz_tag = battle,字段语义见 shared/idsegment.Conf)。
	// 必须显式写 Enabled: true,没写起服即拒绝:发不出 id 的 match 什么都做不了。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`

	// KafkaWriteTimeoutSeconds:挑战 S2C 推送经 gate-{id} topic 的一次
	// 同步 produce 的 writer 侧超时(秒)。与 scene_manager 同口径:
	// 不用 caller context 取消,避免 WriteMessages 返回后批次仍在投递。
	KafkaWriteTimeoutSeconds int64 `json:",default=5"`

	// MatcherIntervalMs:按池撮合循环的间隔(毫秒)。
	MatcherIntervalMs int64 `json:",default=500"`

	// ChallengeTTLSeconds:切磋挑战记录的 TTL(秒),超时未应答自动作废。
	ChallengeTTLSeconds int64 `json:",default=60"`

	// MatchedTicketTTLSeconds:一局备战预算的下限(秒)。实际预算取 max(该值, 按组大小算的 gather 最坏链路),
	// 见 logic/queue.go matchedTicketTTLFor;它决定 scene 侧 PREPARING 的作废期限与组队开战锁时长。
	// 缺省 30s 已低于公式最小值 42s(1 人组),所以按缺省配置不起作用,
	// 只有往上调(超过对应组大小的公式值)时才生效;上调还会拉长组队开战锁与 battle 补发窗口的要求。
	MatchedTicketTTLSeconds int64 `json:",default=30"`

	// TableFingerprintMode:战斗配表指纹比对策略(设计决策 D14)。scene 在
	// PrepareBattleResponse.table_fingerprint 回报本节点六张战斗表的内容指纹,
	// gather 收齐全员后比对:
	//   off     —— 不比对、不透传(CreateBattleRequest.table_fingerprint 留空);
	//   warn    —— 不一致 / 部分为空只记 Error 日志 + 指标,照常开局(默认);
	//   enforce —— 不一致视为 prepare 失败:与多数派不一致者出局删票,
	//              幸存者回队首,已冻结者逐个解冻(与 prepare_failed 同补偿路径)。
	// 全员非空且两两一致才把该值透传给 battle(battle 再与自身指纹核对)。
	TableFingerprintMode string `json:",default=warn,options=off|warn|enforce"`

	// TableDir:导表产物目录。match 只从中读 PlayMode(匹配入口白名单与人数上限)、Dungeon(怪物战斗模板)、
	// ZoneGroup(大区)三张表,编译成模板目录(internal/catalog;docs/design/global-match-hierarchical-pools.md §7)。
	// 读表或校验失败拒绝启动。开新本、调人数只改表,不改本配置,也不需要停全部副本。
	TableDir string `json:",default=../../generated/tables"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。
	// 端口分工:9101=login / 9150=scene_manager / 9160=db / 9170=match。
	MetricsListenAddr string `json:",optional"`

	// ---- 评分匹配(设计文档 cross-zone-matchmaking.md §11)----

	// RatingEnabled:消费 Kafka 对局结果(ResultTopic)时是否更新 Elo 评分。
	// 关闭后评分停留在默认 1500,评分匹配退化为纯等待序;结果来源归档不受它影响,照常消费。
	// Kafka 地址复用 Kafka.Brokers。
	RatingEnabled bool `json:",default=true"`

	// ResultTopic:battle 节点发对局结果(contracts.kafka.BattleResultEvent)的
	// topic,全局无 zone 段,key=battle_id。C++ 侧 battle_room_manager.cpp 的
	// kMatchResultsTopic 必须与之一致。
	ResultTopic string `json:",default=match-results"`

	// ResultTopicPartitions:ResultTopic 的分区数。启动时用 kafkautil.EnsureTopics
	// 确保 topic 存在;分区数是不可变契约(EnsureTopics 会拒绝与 broker 不一致的值)。
	ResultTopicPartitions int32 `json:",default=3"`

	// ResultConsumerGroup:消费组名。多个 match 实例同组分摊分区,每条结果只被
	// 一个实例消费;入账幂等由全球权威库的 match_rating_applied 兜底。
	ResultConsumerGroup string `json:",default=match-rating"`

	// RatingTolerance*:评分容差曲线 tol = min(Max, Base + floor(wait/StepSeconds) × StepDelta)。
	// 默认 100 起、每 5s 放宽 100、上限 1000:两个新号(都 1500)第一轮就互配,
	// 分差 300 的两人等 10s、分差 900 等 40s。0 / 漏配按默认值。
	RatingToleranceBase        int64 `json:",default=100"`
	RatingToleranceStepSeconds int64 `json:",default=5"`
	RatingToleranceStepDelta   int64 `json:",default=100"`
	RatingToleranceMax         int64 `json:",default=1000"`

	// RatingToleranceMaxWaitSeconds:容差曲线的终态兜底 —— 锚点已等 ≥ 该秒数时
	// 容差 = ∞,退化为纯等待序(与 PVE_TEAM 同分支),保证"最长等待 N 秒必配"
	// (只要队列里有足够人数);分差超过 RatingToleranceMax 的两人不再永久饥饿。
	// 默认 90(曲线 45s 饱和后再等 45s);0 / 漏配按默认,不支持关闭。
	RatingToleranceMaxWaitSeconds int64 `json:",default=90"`

	// RatingDrawRoundCap:PVP 队列模式"回合打满"的判定阈值。C++ 引擎对所有模式
	// 一律 roundIndex >= maxRounds → SIDE_B_WIN(PVE"进攻方判负"规则泄漏到 PVP),
	// 而 team 0 不是随机的(1v1 是锚点、5v5 是评分最高者所在队),照胜负结算会
	// 让锚点 / 高分方系统性扣分。BattleResultEvent.total_rounds ≥ 该值的胜负结果
	// 一律按平局(0.5)结算。默认 300 = C++ kDefaultMaxRounds;0 关闭该判定。
	RatingDrawRoundCap uint32 `json:",default=300"`

	// RatingDrawRoundCapByConfigId:按 battle_config_id 覆盖回合上限(引擎按
	// DungeonTable.time_limit 换算,配了 time_limit 的配置上限可能不是 300)。
	// yaml map 键是字符串(go-zero 不支持整数键的 map);0 视为未配置。
	RatingDrawRoundCapByConfigId map[string]uint32 `json:",optional"`

	// ---- 组队(docs/design/team-system.md §D.1)----

	// Team:组队模块配置。不标 optional:整段缺席时 go-zero 按空块递归填各字段的
	// default(字段全带 default,不会报"未设置");标了 optional 反而不会填 default。
	Team TeamConf

	// DataServiceRpc:data_service 的 gRPC 客户端(etcd 发现,Key=dataservice.rpc),
	// 用来领号段、查玩家昵称与展示资料。结构照 guild config。
	// **必须 NonBlock**:客户端在 svc.NewServiceContext 里构造,早于节点注册与 s.Start();
	// 阻塞建连会让"data_service 没起 / etcd 里没有 dataservice.rpc"拖垮排队 / 切磋 / 观战。
	// svc.NewDataServiceClient 对 NonBlock=false 记错误日志并强制改为 true。
	// optional:没配任何目标时不建客户端,号段装配(svc.NewBattleIDSegment)随之拒绝启动。
	DataServiceRpc zrpc.RpcClientConf `json:",optional"`

	// ---- 观战分级(docs/design/battle-spectate-tiered-fanout.md §15.6)----

	// Spectate:观战 L1 中继、L2 全服广播与观战凭证。与 Team 一样不标 optional:整段缺席时 go-zero 按空块
	// 递归填各字段的 default(开关关闭,其余取默认值),标了 optional 反而不会填 default。
	Spectate SpectateConf
}

type GlobalMatchConf struct {
	Backend                string `json:",default=tidb"` // tidb | mysql,不写就是 tidb
	Host                   string
	User                   string
	PasswordEnv            string `json:",default=MATCH_MYSQL_PASSWORD"`
	Database               string `json:",default=mmorpg_match"`
	AccountRoleSchema      string
	GuildTrialSchema       string
	ProbeSecretEnv         string `json:",default=MATCH_PROBE_SECRET"`
	CrossRegionWaitSeconds uint32 `json:",default=30"`
	HardLatencyMs          uint32 `json:",default=180"`
	ProbeTTLSeconds        uint32 `json:",default=120"`
	// 匹配范围树逐级放宽(docs/design/global-match-hierarchical-pools.md §3.2):入队后先只和同 zone 的人凑,
	// 等满 ScopeGroupAfterSeconds 可到同大区,等满 ScopeGlobalAfterSeconds 可到全服;从原始入队时刻起算。
	// 两者都是提交点规则,进全局策略指纹,各副本必须一致。初值待按真实等待分布定标(D-16)。
	ScopeGroupAfterSeconds  uint32 `json:",default=10"`
	ScopeGlobalAfterSeconds uint32 `json:",default=20"`
	// GatherSlots:每个副本同时进行的成局备战(PrepareBattle + CreateBattle)上限。池写者先占槽再提交成局,
	// 占不到槽本轮就不再提交,票留在队列里。不是提交点规则,不进策略指纹。
	GatherSlots int `json:",default=256"`
	// PoolPageSize:池写者每轮每个池读的单排票页大小;队首一整页都凑不出组时下一轮翻到下一页。
	PoolPageSize  int    `json:",default=512"`
	RankEndpoint  string `json:",default=127.0.0.1:51100"`
	RankSecretEnv string `json:",default=RANK_PRODUCER_SECRET_MATCH"`
	// OutcomeOutbox:终局发件箱(整局结果发 Kafka、个人结算入 Inbox)的投递节拍、每轮批量与并发(2026-10-30,评审 B-6)。
	// 不是提交点规则,不进策略指纹,各副本可以不同;越界在装配时拒启(authority.OutcomeOutboxOptions.Normalized)。
	OutcomeOutbox OutcomeOutboxConf `json:",optional"`
}

// OutcomeOutboxConf 是终局发件箱的投递参数。默认值与 2026-10-30 之前写死的行为一致;0 / 漏配按默认。
type OutcomeOutboxConf struct {
	IntervalMs   int `json:",default=2000"` // 投递循环节拍
	ResultBatch  int `json:",default=8"`    // 每轮最多读出的结果行
	FirstBatch   int `json:",default=4"`    // 每轮最多读出的个人首投行(inbox_seq=0)
	WaitingBatch int `json:",default=4"`    // 每轮最多读出的个人等待 APPLIED 行(inbox_seq>0)
	Concurrency  int `json:",default=1"`    // 已认领行的并发投递上限;1 = 逐行串行
}

// OutcomeOutboxInterval 返回终局发件箱的投递节拍;0 / 负数 / 漏配(直接构造 Config 的单测)按默认 2 秒。
func (g GlobalMatchConf) OutcomeOutboxInterval() time.Duration {
	if g.OutcomeOutbox.IntervalMs <= 0 {
		return 2 * time.Second
	}
	return time.Duration(g.OutcomeOutbox.IntervalMs) * time.Millisecond
}

// GatherSlotCount 返回成局备战并发上限;0 / 漏配(直接构造 Config 的单测)按默认 256。
func (g GlobalMatchConf) GatherSlotCount() int {
	if g.GatherSlots <= 0 {
		return 256
	}
	return g.GatherSlots
}

// PoolPage 返回按池读单排票的页大小;0 / 漏配按默认 512。
func (g GlobalMatchConf) PoolPage() int {
	if g.PoolPageSize <= 0 {
		return 512
	}
	return g.PoolPageSize
}

// 没写 PlayerRedis.Addrs 时玩家域连接的口径:go-zero 对 Redis 段的缺省值(池 10 × GOMAXPROCS,K8s 上 GOMAXPROCS 按 2 算,读写 3s,
// 取连接 4s,拨号 5s)。这里写成常量,不随 CPU limit 漂移。
const (
	playerRedisFallbackPoolSize      = 20
	playerRedisFallbackIOTimeoutMs   = 3000
	playerRedisFallbackPoolTimeoutMs = 4000
	playerRedisFallbackDialTimeoutMs = 5000
)

// EffectivePlayerRedis 返回生效的玩家域配置:写了 PlayerRedis.Addrs 就用它,否则指向 Redis 段那一个实例。
func (c *Config) EffectivePlayerRedis() playerredis.Config {
	return c.PlayerRedis.OrDefault(playerredis.Config{
		Addrs:           []string{c.Redis.Host},
		Username:        c.Redis.User,
		Password:        c.Redis.Pass,
		TLS:             c.Redis.Tls,
		PoolSizePerNode: playerRedisFallbackPoolSize,
		DialTimeoutMs:   playerRedisFallbackDialTimeoutMs,
		ReadTimeoutMs:   playerRedisFallbackIOTimeoutMs,
		WriteTimeoutMs:  playerRedisFallbackIOTimeoutMs,
		PoolTimeoutMs:   playerRedisFallbackPoolTimeoutMs,
	})
}

// TeamConf 组队配置(team-system.md §D.1 / §D.3)。
type TeamConf struct {
	// AllowCrossZone:是否允许不同 home zone 的玩家同队。默认 false:申请、审批(同意)、
	// 邀请、接受邀请四个入口比对对方 home zone 与队伍 zone,不一致回 kTeamCrossZoneDenied;
	// 审批 / 接受时用申请 / 邀请记录里存的 zone 再核一次,所以中途改成 false 也能拦住。
	// 全球模式也约束补位后最终完整队伍，单排/整队unit都在SQL LIMIT前过滤home，提交重核原件与已锁route。
	// 已有跨home名册保留，但false下不得新开跨home对局；全球副本必须同SQL策略指纹，切换须停全部副本受控调整。
	// 直接构造 Config 的测试取零值 false,与缺省一致。
	AllowCrossZone bool `json:",default=false"`

	// FillQueueDisabled:关闭队伍补位匹配(team-backfill-matching.md §4.6)。默认 false = 开放:
	// StartTeamMatch 带大于当前人数的 target_team_size 时整队进人数档队列等补位。
	// 置 true 后这类请求回 kTeamDungeonNotOpen;已经在队列里的补位单不受影响(照常可取消、可成组),
	// 直接开战(不带目标人数或等于当前人数)也不受影响。用「Disabled」而不是「Enabled」是为了让
	// 直接构造 Config 的测试取到的零值与缺省一致。
	FillQueueDisabled bool `json:",default=false"`
}

// spectateSecretMinBytes 观战凭证与运营入口密钥去除首尾空白后的最小长度,口径同 battle 令牌(§15.7)。
const spectateSecretMinBytes = 32

// defaultSpectateTicketTTL 观战凭证的缺省有效期,与 TicketTTLSeconds 的 default 同值。
const defaultSpectateTicketTTL = 60 * time.Second

// SpectateOpsCaller 是 MatchInternal.SetSpectateFeatured 唯一接受的 serviceauth 调用方。
const SpectateOpsCaller = "ops"

// SpectateConf 观战分级配置(battle-spectate-tiered-fanout.md §15.1、§15.6)。每一档都能单独关掉:
// RelayEnabled=false 时观众只走 L0,直连席满了回 16021;BroadcastMaxConcurrent=0 时不再升 L2,只用 L1。
type SpectateConf struct {
	// RelayEnabled:是否开启 L1 / L2(Q6)。两档都依赖「battle 能经 Kafka 到达观众所在 gate」,所以只在全部 battle
	// 与 gate 共用一个 Kafka 集群时打开。这是部署级开关:靠运行期探测猜集群边界不可靠。
	RelayEnabled bool `json:",default=false"`

	// BroadcastMaxConcurrent:全服同时处于 L2 的场次上限 B。0 = 不开 L2。
	BroadcastMaxConcurrent int `json:",default=4"`

	// BroadcastAutoGateThreshold:本场有观众的 gate 数达到该值时自动尝试升 L2(G_min)。0 = 只认运营标记。
	BroadcastAutoGateThreshold int `json:",default=32"`

	// TicketTTLSeconds:观战凭证有效期(秒)。凭证只决定「能不能订阅」,过期后客户端用原 request_id 重放拿新凭证。
	TicketTTLSeconds int64 `json:",default=60"`

	// TicketSecret:观战凭证的 HMAC 密钥,与 gate 的 BaseDeployConfig.spectate_token_secret 同值
	// (部署注入 MMORPG_SPECTATE_TOKEN_SECRET),与 gate / battle 令牌密钥分域。签名用原值,不去空白:gate 也用原值验签。
	TicketSecret string `json:",optional"`

	// OpsCallerSecret:运营工具调用 MatchInternal.SetSpectateFeatured 的 serviceauth 密钥。空 = 运营入口关闭,一律 PermissionDenied。
	OpsCallerSecret string `json:",optional"`
}

// RelayUsable 判定本实例能否把观众放进 L1 / L2:开关打开且有凭证密钥。dev / test 允许开关开着而密钥为空
// (启动时告警),此时按关闭处理、满员回 16021,不去升档再签 gate 必然拒绝的凭证。
func (c SpectateConf) RelayUsable() bool {
	return c.RelayEnabled && strings.TrimSpace(c.TicketSecret) != ""
}

// OpsEntryEnabled 判定运营入口是否开启。纯空白的密钥等同没配。
func (c SpectateConf) OpsEntryEnabled() bool {
	return strings.TrimSpace(c.OpsCallerSecret) != ""
}

// TicketTTL 返回观战凭证有效期;0 / 负数 / 漏配(直接构造 Config 的单测)按默认 60 秒。
func (c SpectateConf) TicketTTL() time.Duration {
	if c.TicketTTLSeconds <= 0 {
		return defaultSpectateTicketTTL
	}
	return time.Duration(c.TicketTTLSeconds) * time.Second
}

// validateSpectate 校验观战配置:prod(非 dev / test)打开中继时
// 凭证密钥必须够长,否则拒启;运营密钥一旦配了就必须够长(serviceauth 的硬要求,提前到加载期报)。
func (c *Config) validateSpectate() error {
	spectate := c.Spectate
	if spectate.BroadcastMaxConcurrent < 0 {
		return fmt.Errorf("Spectate.BroadcastMaxConcurrent 不能为负数,得到 %d(0 = 不开 L2)", spectate.BroadcastMaxConcurrent)
	}
	if spectate.BroadcastAutoGateThreshold < 0 {
		return fmt.Errorf("Spectate.BroadcastAutoGateThreshold 不能为负数,得到 %d(0 = 只认运营标记)", spectate.BroadcastAutoGateThreshold)
	}
	if spectate.RelayEnabled && c.Mode != "dev" && c.Mode != "test" &&
		len(strings.TrimSpace(spectate.TicketSecret)) < spectateSecretMinBytes {
		return fmt.Errorf("Spectate.RelayEnabled=true 时 Spectate.TicketSecret 去除首尾空白后必须至少 %d 字节", spectateSecretMinBytes)
	}
	if spectate.OpsEntryEnabled() && len(strings.TrimSpace(spectate.OpsCallerSecret)) < spectateSecretMinBytes {
		return fmt.Errorf("Spectate.OpsCallerSecret 去除首尾空白后必须至少 %d 字节(留空 = 关闭运营入口)", spectateSecretMinBytes)
	}
	return nil
}

// ResultTopicRequiresDurability 报告结果 topic 是否要核验 RF3 严格策略:只有 dev / test 档放行(本地 Kafka 通常单 broker),
// 其余档一律严格。结算要不要耐久和 SQL 用哪一档无关,所以这里不看 Backend。
func (c *Config) ResultTopicRequiresDurability() bool {
	return c.Mode != service.DevMode && c.Mode != service.TestMode
}

// Validate 由 conf.MustLoad 自动调用(go-zero v1.10.0)。SQL 权威是唯一路径,它的必需配置在这里挡住,缺配置不能起服;
// 其余字段沿用各自的"0 / 漏配按默认"约定。
// 先调内嵌 RpcServerConf 的 Validate:本方法会遮蔽那个被提升的方法,不显式调用的话加载期就少了它那项校验
// (Auth=true 时要求 Redis),失败时间点会推迟到 zrpc.NewServer、而那时 matcher 与消费者都已经起来了。
func (c *Config) Validate() error {
	if err := c.RpcServerConf.Validate(); err != nil {
		return err
	}
	if _, err := authoritysql.ParseBackend(c.GlobalMatch.Backend); err != nil {
		return fmt.Errorf("GlobalMatch.Backend: %w", err)
	}
	if strings.TrimSpace(c.GlobalMatch.Host) == "" || strings.TrimSpace(c.GlobalMatch.User) == "" {
		return errors.New("GlobalMatch.Host 与 GlobalMatch.User 必须显式配置:匹配只有 SQL 权威一条路径")
	}
	if !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() {
		return errors.New("GlobalLoginSessions 必须为 true 且 LoginRedis 的地址必须显式配置:客户端请求的原角色身份只从 login 同一实例核对")
	}
	if err := c.PlayerRedis.ValidateSection(); err != nil {
		return fmt.Errorf("PlayerRedis: %w", err)
	}
	if c.GlobalMatch.ScopeGroupAfterSeconds > c.GlobalMatch.ScopeGlobalAfterSeconds {
		return fmt.Errorf("GlobalMatch.ScopeGroupAfterSeconds(%d) 不能大于 ScopeGlobalAfterSeconds(%d):先放宽到大区,再放宽到全服",
			c.GlobalMatch.ScopeGroupAfterSeconds, c.GlobalMatch.ScopeGlobalAfterSeconds)
	}
	return c.validateSpectate()
}

// RatingDrawRoundCapFor 返回 battle_config_id 对应的"回合打满按平局"阈值:
// 按配置覆盖优先,否则用 RatingDrawRoundCap;返回 0 表示不做该判定。
func (c *Config) RatingDrawRoundCapFor(configId uint32) uint32 {
	for key, value := range c.RatingDrawRoundCapByConfigId {
		if value == 0 {
			continue
		}
		parsed, err := strconv.ParseUint(key, 10, 32)
		if err == nil && uint32(parsed) == configId {
			return value
		}
	}
	return c.RatingDrawRoundCap
}
