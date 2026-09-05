package logic

import (
	"fmt"
	"strconv"
	"strings"
)

// Redis key 契约(设计文档 cross-zone-matchmaking.md §4;修订 turn-based-battle-server.md §5.4 / §6)。
//
// ---- MatchRedis:match 独占,可集群 ----
//
//	match:{mq}:index                      set   活跃队列 key 注册集(SCAN 的替代)
//	match:{mq}:queue:{mode}:{config}      list  排队玩家 id,等待序(Rpush 入队尾;队首最久)
//	match:{mq}:rank:{mode}:{config}       zset  同一队列的评分镜像(member=player_id,score=rating,§11)
//	match:{mq}:lock:{mode}:{config}       锁    matcher 凑单临界区(多实例 SETNX)
//	match:ticket:{player_id}              hash  排队票据(状态机:queued/matched/ready),按玩家分布
//	match:rating:{player_id}              hash  玩家评分(rating/games/updated_at_ms,无 TTL),按玩家分布
//	match:rating:applied:{battle_id}      string 对局结果已入账标记(TTL 7d,SETNX 幂等),按对局分布
//	challenge:{id}                        hash  切磋记录(TTL 60s)
//	challenge:target:{player_id}          锁    同一目标同时只挂一个待应答挑战
//	spectate:*                            观战索引与互斥标记(§10.4),key 组装见 spectate.go
//
// 队列四类 key 共用 hash tag {mq} 落同一 slot(设计决策 D3 / §11):入队
// SADD+RPUSH+ZADD 用一条 Lua 原子完成,主从异步复制的故障切换要丢一起丢,
// "活跃队列 ⊆ 注册集"与"list 成员集 == zset 成员集"两条不变量不会被打破。
// 队列 QPS 极低,单 slot 不是瓶颈(§6)。
//
// 评分 key 按玩家分布、不带 {mq}(JoinQueue 读评分与入队 Lua 不同 slot,所以评分
// 在 Go 侧读出后作为 ARGV 传进 Lua)。**无 TTL 的前提是 MatchRedis 独立部署**:
// MatchRedis 缺省回落到共享库时,共享库若配了 allkeys-lfu 之类的淘汰策略,评分
// 可能被淘汰回落到默认 1500 —— 生产必须配独立 MatchRedis(设计文档 §4.2 / §11)。
//
// ---- SharedRedis:既有共享库,match 只读 ----
//
//	player:{id}:location                  scene_manager 写  JoinQueue 取 zone;gather 定位 scene 节点
//	player:session:{id}                   player_locator 写 挑战推送 / 观战路由取 gate
//	battle:lock:{player_id}               C++ scene 写      咨询性"是否在战斗中"(权威在 InBattleComp)

// matchQueueHashTag 队列相关 key 的 hash tag,三类 key 必须共用。
const matchQueueHashTag = "{mq}"

// matchQueueIndexKey 活跃队列 key 注册集:JoinQueue 入队时 SADD,matcher 用
// SMEMBERS 遍历替代 SCAN(集群下 SCAN 只扫一个分片)。空队列由 matcher 懒剔除。
const matchQueueIndexKey = "match:" + matchQueueHashTag + ":index"

func matchQueueKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:%s:queue:%d:%d", matchQueueHashTag, mode, battleConfigId)
}

func matcherLockKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:%s:lock:%d:%d", matchQueueHashTag, mode, battleConfigId)
}

// matchRankKey 是队列的评分镜像 ZSET(§11):与队列 list 同 slot,每条改动
// list 的 Lua 都同时改它。member=player_id,score=入队时读到的评分。
func matchRankKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:%s:rank:%d:%d", matchQueueHashTag, mode, battleConfigId)
}

// rankKeyForQueue 由队列 key 推出同 slot 的评分镜像 key:只把倒数第三段的
// "queue" 换成 "rank",前缀(含 hash tag)原样保留,所以票据里记录的任何
// 形态的 queue_key 都能得到与之同 slot 的 rank key。非队列 key 形态报错。
func rankKeyForQueue(queueKey string) (string, error) {
	if _, _, err := parseQueueKey(queueKey); err != nil {
		return "", err
	}
	parts := strings.Split(queueKey, ":")
	parts[len(parts)-3] = "rank"
	return strings.Join(parts, ":"), nil
}

// matchRatingKey 玩家评分 hash(§11):字段 rating / games / updated_at_ms,
// 无 TTL,按玩家分布(不带 {mq})。缺失即默认 defaultRating。
func matchRatingKey(playerId uint64) string {
	return fmt.Sprintf("match:rating:%d", playerId)
}

// matchRatingAppliedKey 对局结果入账标记(§11):消费 match-results 时先 SETNX
// 它(TTL 7d),已存在即重复投递,跳过 —— Kafka at-least-once 下同一局不会算两次。
func matchRatingAppliedKey(battleId uint64) string {
	return fmt.Sprintf("match:rating:applied:%d", battleId)
}

// legacyMatchQueueScanPattern / legacyMatchQueueKey 是加 hash tag 之前的队列 key
// 形态(`match:queue:<mode>:<config>`,无 tag,不在注册集里)。滚动升级的混跑
// 窗口内旧实例仍往这里 RPUSH,且旧票据没有 queue_key 字段;新 matcher 只遍历
// 注册集,永远不会再读到这些 list。两处兜底:
//   - matcher 启动与定期 SCAN 旧 key 把成员搬进新队列(migrateLegacyQueues);
//   - CancelQueue 对无 queue_key 的旧票据新旧两个 key 各 Lrem 一次。
//
// glob `match:queue:*` 不会匹配到 `match:{mq}:queue:…`,不会把新队列当旧的搬。
// 旧 key 只存在于单库形态(改造前没有集群部署),SCAN 在这里可用。
const legacyMatchQueueScanPattern = "match:queue:*"

func legacyMatchQueueKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:queue:%d:%d", mode, battleConfigId)
}

// parseQueueKey 从队列 key 尾部解析 (mode, config):只认最后两段,前缀
// (hash tag 等)演进不影响解析。倒数第三段必须是 queue,防止把注册集里
// 混入的异物当队列处理。
func parseQueueKey(queueKey string) (mode int32, config uint32, err error) {
	parts := strings.Split(queueKey, ":")
	if len(parts) < 4 || parts[len(parts)-3] != "queue" || !strings.HasPrefix(queueKey, "match:") {
		return 0, 0, fmt.Errorf("队列 key %q 不符合 match:{tag}:queue:<mode>:<config> 形态", queueKey)
	}
	m, err := strconv.ParseInt(parts[len(parts)-2], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("队列 key %q 的 mode 段非法: %w", queueKey, err)
	}
	c, err := strconv.ParseUint(parts[len(parts)-1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("队列 key %q 的 config 段非法: %w", queueKey, err)
	}
	return int32(m), uint32(c), nil
}

func matchTicketKey(playerId uint64) string {
	return fmt.Sprintf("match:ticket:%d", playerId)
}

func challengeKey(challengeId uint64) string {
	return fmt.Sprintf("challenge:%d", challengeId)
}

func challengeTargetKey(playerId uint64) string {
	return fmt.Sprintf("challenge:target:%d", playerId)
}

// ---- 以下是契约 key:只能经 svcCtx.SharedRedis 读,match 不写 ----

// battleLockKey 由 C++ scene 写;match 只做咨询性 Exists。
func battleLockKey(playerId uint64) string {
	return fmt.Sprintf("battle:lock:%d", playerId)
}

// getPlayerLocationKey 是 scene_manager 维护的玩家位置权威键
// (共享契约记录见设计文档 §5.4;写者:go/scene_manager changesceneutil.go)。
func getPlayerLocationKey(playerId uint64) string {
	return fmt.Sprintf("player:%d:location", playerId)
}

// playerSessionKey 是 player_locator 维护的会话键(PlayerSession proto),
// 挑战推送按它定位目标玩家的 gate(照 guild online_status_resolver 的读法)。
func playerSessionKey(playerId uint64) string {
	return fmt.Sprintf("player:session:%d", playerId)
}
