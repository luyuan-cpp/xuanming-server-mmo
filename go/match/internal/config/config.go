package config

import (
	"strconv"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	Kafka struct {
		Brokers []string
	}

	// ZoneId:C++ 约定 etcd 注册用的 zone id,gate 按
	// MatchNodeService.rpc/zone/{zone} 前缀发现本服务。
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL:etcd 节点注册 keepalive 的租约 TTL(秒)。
	LeaseTTL int64 `json:",default=60"`

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

	// ReadyTicketTTLSeconds:开局成功后 ticket 停留在 READY 态的 TTL(秒)。
	// 过期后 GetQueueStatus 返回 NOT_QUEUED —— 战斗内状态由
	// battle:lock / scene 的 InBattleComp 权威表达,不再依赖 ticket。
	ReadyTicketTTLSeconds int64 `json:",default=60"`

	// PveTeamSizeByConfigId:PVE 组队各 battle_config_id(DungeonTable id)
	// 的凑满人数。一期临时配置 —— 人数权威来源是 DungeonTable.max_team_size,
	// Go 侧尚无表管理器,待接导表数据后改为查表。
	// yaml map 键是字符串(与 scene_manager 的 WorldChannelCountByConfId 同 workaround)。
	PveTeamSizeByConfigId map[string]uint32 `json:",optional"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。
	// 端口分工:9101=login / 9150=scene_manager / 9160=db / 9170=match。
	MetricsListenAddr string `json:",optional"`
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
