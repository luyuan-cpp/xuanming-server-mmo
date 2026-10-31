package logic

import "shared/redisslot"

// 玩家域的键(docs/design/redis-player-store-cluster.md §5.2、§5.9):全部带 hash tag,经 ServiceContext.PlayerRedis 访问。
//
// 会话键带玩家 tag {<pid>};租约与 gate 索引按「会话 pid 所在 slot 的 slot-tag」分桶,与该玩家的 {<pid>} 键必然同槽,
// 所以一次会话 CAS 能在同一段 Lua 里原子维护本 slot 的租约和索引(不变量 I1)。这些键只在本服务内访问,
// 别的服务只读会话键(redisslot.SessionKey)。
const (
	leaseKeyPrefix     = "player:leases:"
	gateIndexKeyPrefix = "player:gate_index:"
)

// leaseKeys 是一个 slot 的四把租约键,带同一个 slot-tag。
type leaseKeys struct {
	ready         string // ZSET:成员是十进制 player_id,分数是到期 unix 秒
	processing    string // ZSET:已被认领的成员,分数是认领截止 unix 秒
	claimTokens   string // HASH:player_id → 认领令牌
	claimPayloads string // HASH:player_id → 清理时要用的会话字节
}

func leaseKeysForSlot(slot uint16) leaseKeys {
	ready := leaseKeyPrefix + redisslot.SlotIndexTag(slot)
	return leaseKeys{
		ready:         ready,
		processing:    ready + ":processing",
		claimTokens:   ready + ":claim_tokens",
		claimPayloads: ready + ":claim_payloads",
	}
}

func leaseKeysForPlayer(playerID uint64) leaseKeys {
	return leaseKeysForSlot(redisslot.PlayerSlot(playerID))
}

// gateRegistryKey 是一个 slot 的 gate 登记集:SET,成员是 gate 实例 uuid。
// 不变量 I2:本 slot 的索引非空 ⇒ gate uuid 在本 slot 的登记集里。
func gateRegistryKey(slot uint16) string {
	return gateIndexKeyPrefix + redisslot.SlotIndexTag(slot) + ":gates"
}

// gateSessionIndexKey 是一个 slot 上 gate 实例的会话索引:SET,成员是十进制 player_id,这些玩家都在这个 slot 上。
// 不变量 I1:会话 ONLINE 且 gate_instance_id 非空 ⇔ player_id 是「它的 slot + 它的 gate」这把索引的成员。
func gateSessionIndexKey(slot uint16, gateUUID string) string {
	return gateIndexKeyPrefix + redisslot.SlotIndexTag(slot) + ":sessions:" + gateUUID
}

func sessionKey(playerID uint64) string {
	return redisslot.SessionKey(playerID)
}

// sceneManagerLocationKey 是 SceneManager 的位置权威键。
func sceneManagerLocationKey(playerID uint64) string {
	return redisslot.LocationKey(playerID)
}

// 分片用的成员登记与段租约(slot_shard.go),role 区分 LeaseMonitor 与死 gate 清扫各自的一套。
// 它们也是玩家域的普通键,随所在 slot 迁移。成员登记固定在 slot 0 的 tag 上,段 c 的租约带第 64c 个 slot 的 tag。
func shardMembersKey(role string) string {
	return "player:locator_shard:" + role + ":members:" + redisslot.SlotIndexTag(0)
}

func shardSegmentKey(role string, segment int) string {
	return "player:locator_shard:" + role + ":segment:" + redisslot.SlotIndexTag(uint16(segment*slotsPerSegment))
}

// 控制域的键(ServiceContext.RedisClient,不带 hash tag,不和玩家键进同一段脚本):
// 就绪、代次、审计时间与选主锁都是全服一份,只由对账 leader 读写。
const (
	// gateIndexReadyKey:就绪标记(值 v2:<设置时刻 unix 秒>,带 PX 租约)。存在 = 最近一次完整补建开始之后
	// 没有观察到旧写者,且此后每拍都有 leader 在成功的 etcd 读里确认零旧写者并续期。只由 leader 写。
	gateIndexReadyKey = "player:gate_index:ready"
	// gateIndexReadyValuePrefix 随索引形状走:v1 是全局索引,v2 是按 slot 分桶。
	gateIndexReadyValuePrefix = "v2:"
	// gateIndexGenKey:代次计数器。leader 在一次成功的 etcd 读里看到旧写者就 INCR;补建结束时
	// 「代次未变才设就绪标记」,跨 leader 守住「补建期间出现过旧写者」这件事。
	gateIndexGenKey = "player:gate_index:gen"
	// gateIndexLastAuditKey:上一次完整补建 / 审计完成的 unix 秒。审计时机以它为准,换主不重置。
	gateIndexLastAuditKey = "player:gate_index:last_audit"
	// gateIndexAuditCooldownKey:审计失败后的冷却键(值 1,带 EX)。存在时审计不到期,换主不重置。
	gateIndexAuditCooldownKey = "player:gate_index:audit_cooldown"
	// reconcilerLeaderKey:写者检查与补建 / 审计的选主锁(shared/leader,带 TTL)。全服一个 leader。
	reconcilerLeaderKey = "player_locator:session_reconciler:leader"
)
