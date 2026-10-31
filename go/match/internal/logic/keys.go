package logic

import (
	"fmt"
	"strconv"
	"strings"

	"match/internal/playercontract"
)

// Redis key 契约(设计文档 cross-zone-matchmaking.md §4;修订 turn-based-battle-server.md §5.4 / §6)。
//
// ---- MatchRedis:match 独占,可集群 ----
//
//	match:{mq}:index                      set   活跃队列 key 注册集(SCAN 的替代)
//	match:{mq}:queue:{mode}:{config}      list  排队玩家 id,等待序(Rpush 入队尾;队首最久)
//	match:{mq}:rank:{mode}:{config}       zset  同一队列的评分镜像(member=player_id,score=rating,§11)
//	match:{mq}:lock:{mode}:{config}       锁    matcher 凑单临界区(多实例 SETNX)
//	match:{mq}:size{N}:queue|rank|lock:{mode}:{config}
//	                                      PVE 人数档(pve-team-size-matching.md §3):同一副本按目标人数 N
//	                                      各有一套 queue / rank / lock,人数段在 hash tag 之后,仍与上面同 slot
//	match:ticket:{player_id}              hash  排队票据(状态机:queued/matched/ready),按玩家分布
//	challenge:{id}                        hash  切磋记录(TTL 60s)
//	challenge:target:{player_id}          锁    同一目标同时只挂一个待应答挑战
//	spectate:*                            观战索引与互斥标记(§10.4),key 组装见 spectate.go
//
// 队列、评分、版本、全局写入序列、注册集与凑单锁共用 hash tag {mq} 落同一 slot(设计决策 D3 / §11):入队
// SADD+RPUSH+ZADD 用一条 Lua 原子完成,主从异步复制的故障切换要丢一起丢,
// "活跃队列 ⊆ 注册集"与"list 成员集 == zset 成员集"两条不变量不会被打破。
// 队列 QPS 极低,单 slot 不是瓶颈(§6)。
//
// 评分不在 Redis(全球权威库的 match_rating):JoinQueue 读出评分后作为 ARGV 传进入队 Lua。
//
// ---- SharedRedis:既有共享库,match 原有代码只读 ----
//
//	player:{id}:location                  scene_manager 写  JoinQueue 取 zone;gather 定位 scene 节点
//	player:session:{id}                   player_locator 写 挑战推送 / 观战路由取 gate
//	battle:lock:{player_id}               C++ scene 写      咨询性"是否在战斗中"(权威在 InBattleComp)
//
// 这三类契约 key 的拼法、读取与推送收口在 match/internal/playercontract
// (team-system.md §A.2),本包文件末尾的同名函数只是一行转调。
//
// **D2 例外(cross-zone-matchmaking.md D2,2026-09-15)**:同进程的 team 模块
// (match/internal/team)写 SharedRedis 的 team:rec:* / team:<id> / team:player:* /
// team:invite:* 四类 key。理由:C++ scene 只能用 hiredis 读 SharedRedis 上的 team:<id>
// 投影,而权威记录、投影、玩家索引、邀请反查必须在同一条 Lua 里原子提交,四类 key
// 只能同库(team-system.md §C.1 / §C.2)。写入范围只限这四类前缀;本包(logic)
// 不写任何 team:* key,对上面三类契约 key 只读。
//
// 本包不写 SharedRedis。原先唯一的例外是旧格式队列 match:queue:* 的搬迁
// (migrateLegacyQueues),项目未上线、没有存量旧 key,2026-10-20 已整体删除
// (cross-zone-matchmaking.md D2 的 2026-10-20 更正)。

// matchQueueHashTag 队列相关 key 的 hash tag,所有队列 Lua 的 key 必须共用。
const matchQueueHashTag = "{mq}"

// matchQueueIndexKey 活跃队列 key 注册集:JoinQueue 入队时 SADD,matcher 用
// SMEMBERS 遍历替代 SCAN(集群下 SCAN 只扫一个分片)。空队列由 matcher 懒剔除。
const matchQueueIndexKey = "match:" + matchQueueHashTag + ":index"

// matchQueueVersionSequenceKey 给每次队列写入分配新版本。它与所有队列同 {mq} slot,
// 全库只保留这一个无 TTL 的递增计数器;清空某条队列时不能删除它,否则旧撤单快照会遇到版本重用。
// 该保护以 Redis 不丢失已确认的序列写入为前提,不覆盖故障切换导致计数器回退。
const matchQueueVersionSequenceKey = "match:" + matchQueueHashTag + ":write_sequence"

// queueVersionsKey 的 hash 记录 player_id → 最近一次 push 的版本;入队/回队与版本写入同一条 Lua。
// 队列项摘出时删除对应字段,空队列回收时删除整张 hash。直接追加后缀保持原 {mq} slot。
func queueVersionsKey(queueKey string) string { return queueKey + ":versions" }

func matchQueueKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:%s:queue:%d:%d", matchQueueHashTag, mode, battleConfigId)
}

func matcherLockKey(mode int32, battleConfigId uint32) string {
	return fmt.Sprintf("match:%s:lock:%d:%d", matchQueueHashTag, mode, battleConfigId)
}

// queueSizeSegmentPrefix PVE 人数档队列 key 里人数段的前缀("size2" = 2 人档)。
const queueSizeSegmentPrefix = "size"

// matchSizedQueueKey PVE 人数档的队列 key(pve-team-size-matching.md §3):同一个
// (mode, battle_config_id) 按目标人数分成互不相干的队列,2 人档的人不会被 5 人档弹走。
// 人数段放在 hash tag 之后、"queue" 之前:parseQueueKey / rankKeyForQueue / lockKeyForQueue
// 只认尾部三段,所以 (mode, config) 的解析与同 slot 的 rank / lock key 推导都不用改;
// 凑满人数由 queueTeamSize 从这一段读回,不再去问配置(配置只管上限)。
func matchSizedQueueKey(mode int32, battleConfigId uint32, teamSize uint32) string {
	return fmt.Sprintf("match:%s:%s%d:queue:%d:%d", matchQueueHashTag, queueSizeSegmentPrefix, teamSize, mode, battleConfigId)
}

// queueTeamSize 读队列 key 里的人数段。不带人数段的队列(PVP 各模式、加人数档之前写下的
// PVE_TEAM 队列)返回 0,调用方按原来的口径(requiredPlayers)取凑满人数。
// 只看紧挨 "queue" 前面的那一段:hash tag 段 "{mq}" 不以 size 开头,不会被误认。
func queueTeamSize(queueKey string) uint32 {
	parts := strings.Split(queueKey, ":")
	if len(parts) < 5 || parts[len(parts)-3] != "queue" {
		return 0
	}
	digits, found := strings.CutPrefix(parts[len(parts)-4], queueSizeSegmentPrefix)
	if !found {
		return 0
	}
	size, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(size)
}

// lockKeyForQueue 由队列 key 推出它的凑单锁 key:与 rankKeyForQueue 同一种换段写法
// (倒数第三段 "queue" → "lock")。不带人数段的队列得到的就是 matcherLockKey(mode, config),
// 所以新旧实例混跑时抢的还是同一把锁;人数档队列各有各的锁,互不挡道。非队列 key 形态报错。
func lockKeyForQueue(queueKey string) (string, error) {
	if _, _, err := parseQueueKey(queueKey); err != nil {
		return "", err
	}
	parts := strings.Split(queueKey, ":")
	parts[len(parts)-3] = "lock"
	return strings.Join(parts, ":"), nil
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

// matchPartyEndReasonKey / matchPartyEndNotifiedKey 一支队伍补位单「撤出队列」的两个短期标记
// (SET NX + 短 TTL,见 party.go claimPartyEndReason / markPartyEndNotified):前者记下最先认定的撤单原因
// (主动取消者是谁),后者保证全队只收到一条通知。单 key、按 party_id 分布,不进任何多 key Lua,集群安全。
func matchPartyEndReasonKey(partyId string) string {
	return "match:party:end:reason:" + partyId
}

func matchPartyEndNotifiedKey(partyId string) string {
	return "match:party:end:notified:" + partyId
}

func challengeKey(challengeId uint64) string {
	return fmt.Sprintf("challenge:%d", challengeId)
}

func challengeTargetKey(playerId uint64) string {
	return fmt.Sprintf("challenge:target:%d", playerId)
}

// ---- 以下是契约 key:只能经 svcCtx.SharedRedis 读,logic 不写 ----
// 拼法的唯一真相在 playercontract,这里保留 logic 内部名字做一行转调。

// battleLockKey 由 C++ scene 写;match 只做咨询性 Exists。
func battleLockKey(playerId uint64) string {
	return playercontract.BattleLockKey(playerId)
}

// getPlayerLocationKey 是 scene_manager 维护的玩家位置权威键
// (共享契约记录见设计文档 §5.4;写者:go/scene_manager changesceneutil.go)。
func getPlayerLocationKey(playerId uint64) string {
	return playercontract.LocationKey(playerId)
}

// playerSessionKey 是 player_locator 维护的会话键(PlayerSession proto),
// 挑战推送按它定位目标玩家的 gate(照 guild online_status_resolver 的读法)。
func playerSessionKey(playerId uint64) string {
	return playercontract.SessionKey(playerId)
}
