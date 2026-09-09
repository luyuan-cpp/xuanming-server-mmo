package config

import (
	"strconv"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	Kafka struct {
		Brokers []string
	}

	// MatchRedis:match 私有 key(match:* / challenge:* / spectate:*)的存储,
	// 可配 Type: cluster(设计文档 cross-zone-matchmaking.md D2 双存储)。
	// 留空则回落到 Redis —— 私有 key 与契约 key 同一实例(本地单库形态)。
	// 契约 key(player:*:location / player:session:* / battle:lock:*)永远走
	// Redis:写者含 C++ scene,hiredis 无集群客户端,共享库不能集群化。
	MatchRedis redis.RedisConf `json:",optional"`

	// ZoneId:C++ 约定 etcd 注册路径用的 zone id。MatchNodeService 不是
	// zone-scoped 节点类型,gate 连的是全 zone 的 match 实例;zone 只影响
	// etcd 注册路径,不影响匹配池(匹配池全局、不分 zone,设计决策 D1)。
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL:etcd 节点注册 keepalive 的租约 TTL(秒)。
	LeaseTTL int64 `json:",default=60"`

	// ClusterId:部署级集群号,snowflake 17 位 worker 段的 [cluster5] 部分
	// (docs/design/node-id-overhaul-plan-20260908.md §5)。**运维按集群一次性设定,
	// 策划不碰**;默认 0 = 单集群 / 存量 id 布局。etcd 槽位前缀带 c<cluster>,两个集群
	// 共用一个 etcd 也不会撞号。必须 < 32。
	ClusterId uint32 `json:",default=0"`

	// SnowflakeCacheDir:snowflake 槽位的本地缓存目录(shared/snowflakealloc,设计稿 §3.5)。
	// 启动时 etcd 不可达、且缓存里的上次水位确认在 F(2h)内,就用缓存的槽起服并后台重试
	// 注册。留空关闭。默认相对服务工作目录(go/match/)指向仓库 run/(已 gitignore)。
	SnowflakeCacheDir string `json:",default=../../run/snowflake"`

	// KafkaWriteTimeoutSeconds:挑战 S2C 推送经 gate-{id} topic 的一次
	// 同步 produce 的 writer 侧超时(秒)。与 scene_manager 同口径:
	// 不用 caller context 取消,避免 WriteMessages 返回后批次仍在投递。
	KafkaWriteTimeoutSeconds int64 `json:",default=5"`

	// MatcherIntervalMs:matcher loop 扫队列的间隔(毫秒)。
	MatcherIntervalMs int64 `json:",default=500"`

	// MatcherLockTTLSeconds:每个 (mode, battle_config_id) 队列凑单临界区
	// 的 Redis SETNX 锁 TTL(秒)。多实例部署的安全性由该锁保证:
	// 拿不到锁的实例跳过本轮,不会双弹同一批玩家。
	MatcherLockTTLSeconds int64 `json:",default=10"`

	// BattleMaxDurationSeconds:战斗最长时限(秒)。gather 用
	// now + 该值 生成 deadline_ms,同时下发给 scene(InBattleComp 作废
	// 期限,reaper 依据)与 battle(强制收尾期限)。
	BattleMaxDurationSeconds int64 `json:",default=300"`

	// ChallengeTTLSeconds:切磋挑战记录的 TTL(秒),超时未应答自动作废。
	ChallengeTTLSeconds int64 `json:",default=60"`

	// TicketTTLSeconds:排队 ticket 的兜底 TTL(秒)。正常路径由
	// 取消/开战收尾,TTL 只负责清理异常残留(进程崩溃等)。
	TicketTTLSeconds int64 `json:",default=21600"`

	// MatchedTicketTTLSeconds:ticket 被 matcher 弹出进入 MATCHED 态后的 TTL(秒)。
	// gather 在弹组实例的 goroutine 里跑,实例中途崩溃时票据不能留 6 小时
	// (玩家会一直被 ErrAlreadyQueued 拒绝);短 TTL 让玩家在该窗口后可重排,
	// scene 侧冻结由 InBattleComp.deadline_ms reaper 兜底(设计决策 D5)。
	// 开局成功走 ReadyTicketTTLSeconds,失败回队首恢复 TicketTTLSeconds。
	MatchedTicketTTLSeconds int64 `json:",default=30"`

	// ReadyTicketTTLSeconds:开局成功后 ticket 停留在 READY 态的 TTL(秒)。
	// 过期后 GetQueueStatus 返回 NOT_QUEUED —— 战斗内状态由
	// battle:lock / scene 的 InBattleComp 权威表达,不再依赖 ticket。
	ReadyTicketTTLSeconds int64 `json:",default=60"`

	// TableFingerprintMode:战斗配表指纹比对策略(设计决策 D14)。scene 在
	// PrepareBattleResponse.table_fingerprint 回报本节点六张战斗表的内容指纹,
	// gather 收齐全员后比对:
	//   off     —— 不比对、不透传(CreateBattleRequest.table_fingerprint 留空);
	//   warn    —— 不一致 / 部分为空只记 Error 日志 + 指标,照常开局(默认);
	//   enforce —— 不一致视为 prepare 失败:与多数派不一致者出局删票,
	//              幸存者回队首,已冻结者逐个解冻(与 prepare_failed 同补偿路径)。
	// 全员非空且两两一致才把该值透传给 battle(battle 再与自身指纹核对)。
	TableFingerprintMode string `json:",default=warn,options=off|warn|enforce"`

	// PveTeamSizeByConfigId:PVE 组队各 battle_config_id(DungeonTable id)
	// 的凑满人数。一期临时配置 —— 人数权威来源是 DungeonTable.max_team_size,
	// Go 侧尚无表管理器,待接导表数据后改为查表。
	// yaml map 键是字符串(与 scene_manager 的 WorldChannelCountByConfId 同 workaround)。
	PveTeamSizeByConfigId map[string]uint32 `json:",optional"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。
	// 端口分工:9101=login / 9150=scene_manager / 9160=db / 9170=match。
	MetricsListenAddr string `json:",optional"`

	// ---- 评分匹配(设计文档 cross-zone-matchmaking.md §11)----

	// RatingEnabled:是否消费 Kafka 对局结果(ResultTopic)更新 Elo 评分。
	// 关闭后评分停留在默认 1500,评分匹配退化为纯等待序;队列的评分镜像
	// ZSET 仍然维护,随时可以打开。Kafka 地址复用 Kafka.Brokers。
	RatingEnabled bool `json:",default=true"`

	// ResultTopic:battle 节点发对局结果(contracts.kafka.BattleResultEvent)的
	// topic,全局无 zone 段,key=battle_id。C++ 侧 battle_room_manager.cpp 的
	// kMatchResultsTopic 必须与之一致。
	ResultTopic string `json:",default=match-results"`

	// ResultTopicPartitions:ResultTopic 的分区数。启动时用 kafkautil.EnsureTopics
	// 确保 topic 存在;分区数是不可变契约(EnsureTopics 会拒绝与 broker 不一致的值)。
	ResultTopicPartitions int32 `json:",default=3"`

	// ResultConsumerGroup:消费组名。多个 match 实例同组分摊分区,每条结果只被
	// 一个实例消费;入账幂等由 match:rating:applied:{battle_id} 兜底。
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
	// 一律按平局(0.5)结算。默认 30 = C++ kDefaultMaxRounds;0 关闭该判定。
	RatingDrawRoundCap uint32 `json:",default=30"`

	// RatingDrawRoundCapByConfigId:按 battle_config_id 覆盖回合上限(引擎按
	// DungeonTable.time_limit 换算,配了 time_limit 的配置上限不是 30)。
	// yaml map 键是字符串(与 PveTeamSizeByConfigId 同 workaround);0 视为未配置。
	RatingDrawRoundCapByConfigId map[string]uint32 `json:",optional"`
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

// PveTeamSizeFor 返回 battle_config_id 对应的 PVE 组队凑满人数;
// 未配置返回 0(调用方按"该副本未开放组队"拒绝)。
func (c *Config) PveTeamSizeFor(configId uint32) uint32 {
	if c.PveTeamSizeByConfigId == nil {
		return 0
	}
	for key, value := range c.PveTeamSizeByConfigId {
		if value == 0 {
			continue
		}
		parsed, err := strconv.ParseUint(key, 10, 32)
		if err == nil && uint32(parsed) == configId {
			return value
		}
	}
	return 0
}
