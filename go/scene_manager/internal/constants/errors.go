package constants

// Scene manager business error codes returned in gRPC response fields.
const (
	ErrNoAvailableNode   uint32 = 1
	ErrSceneLookupFailed uint32 = 2
	ErrUpdateLocation    uint32 = 3
	ErrInvalidNodeID     uint32 = 4
	ErrInvalidGateID     uint32 = 5
	ErrEncodeEvent       uint32 = 6
	ErrKafkaRoute        uint32 = 7
	ErrRedis             uint32 = 8
	ErrDuplicateScene    uint32 = 9
	ErrInvalidSceneType  uint32 = 10
	ErrNoSceneConfId     uint32 = 11
	// ErrNoNodeForPurpose: no scene node with a matching scene_node_type is
	// registered in the target zone. Returned when StrictNodeTypeSeparation is
	// enabled and the caller requests a world/instance scene but the zone's
	// node pool has no node able to host that purpose.
	ErrNoNodeForPurpose uint32 = 12
	// ErrSourceSceneGone: a CreateScene request specified source_scene_id > 0
	// but that source scene no longer exists (already destroyed by the
	// lifecycle manager / DestroyScene RPC). Refusing here prevents creating
	// an "orphan mirror" whose source-derived map / NPC / spawn data will
	// never resolve. Caller should pick a fresh source or fall back to a
	// non-mirror create.
	ErrSourceSceneGone uint32 = 13
	// ErrUnsafeCrossNodeHandoff: 历史码。跨节点交接 / 已有位置的跨区重定向曾经
	// 因为没有落盘屏障而一律拒绝;落盘标记 + owner_epoch 两道门落地后,同类拒绝
	// 改走 ErrHandoffPending(可重试)。保留编号只为不复用(客户端 / 日志检索
	// 里可能还有它),EnterScene 不再发出。
	ErrUnsafeCrossNodeHandoff uint32 = 14
	// ErrEnterSceneInProgress: 相同 request_id 的首个请求仍在执行，尚未产生
	// 可安全重放的完整成功响应。调用方应稍后用同一 request_id 重试。
	ErrEnterSceneInProgress uint32 = 15
	// ErrEnterSceneIdempotencyConflict: 同一玩家复用了 request_id，但请求内容
	// 与首个请求不同。不得重放旧路由/redirect，也不得覆盖正在执行的 owner。
	ErrEnterSceneIdempotencyConflict uint32 = 16
	// ErrSceneReentryBarrier: 场景当前映射的节点刚被判死,但再入屏障(见
	// reentry_barrier.go)还没走完 —— 老节点可能仍在 emergency relocate drain
	// 里 SavePlayerToRedis。此刻改派会造成同一玩家双写/回档,所以本次请求
	// **一个字节都不改**地拒绝,由上游带着同样的 scene_conf_id 退避重试。
	// 这是可重试的瞬时拒绝,不是故障。
	ErrSceneReentryBarrier uint32 = 17
	// ErrHandoffPending: 玩家已有位置记录且需要换到别的节点 / zone,但源 scene 还
	// 没有写出「已落盘」标记(player:{id}:handoff 的 epoch != 当前 owner_epoch)。
	// 源节点可能仍在 SavePlayerToRedis,此刻放行就是新节点读到旧快照。与
	// ErrSceneReentryBarrier 同款语义:**一个字节都不改**地拒绝,上游退避重试,
	// 源 scene 落地并写标记后自然放行。见 cross-zone-scene-travel.md CZ-4。
	ErrHandoffPending uint32 = 18
	// ErrOwnerEpochConflict: 本次 EnterScene 铸出 epoch N 之后、写 location 之前,
	// 有并发的 EnterScene 抢先铸了 N+1,Lua CAS 拒绝了本次写入。本次没有发出任何
	// 路由,玩家归属仍由后到者决定;上游重试即可。可重试,不是故障。
	ErrOwnerEpochConflict uint32 = 19
	// ErrHomeZoneUnavailable: data_service 的 GetPlayerHomeZone 超时 / 不可用,
	// 查不到玩家的归属 zone。归属 zone 决定存盘落哪个库,未知时**不得**静默落进程
	// zone 库(cross-zone-scene-travel.md §6 不变量 2),所以拒绝并让上游重试。
	// 「映射里确实没有这个玩家」不走这个码(那是首登,按 gate zone 处理)。
	ErrHomeZoneUnavailable uint32 = 20
)
