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
	// ErrUnsafeCrossNodeHandoff: 当前请求需要跨节点交接，或已有位置记录的
	// 玩家需要跨区重定向，但系统尚未实现旧节点落盘完成到新节点加载之间的
	// epoch/持久化屏障。默认拒绝，避免加载陈旧状态造成进度回档。
	ErrUnsafeCrossNodeHandoff uint32 = 14
	// ErrEnterSceneInProgress: 相同 request_id 的首个请求仍在执行，尚未产生
	// 可安全重放的完整成功响应。调用方应稍后用同一 request_id 重试。
	ErrEnterSceneInProgress uint32 = 15
	// ErrEnterSceneIdempotencyConflict: 同一玩家复用了 request_id，但请求内容
	// 与首个请求不同。不得重放旧路由/redirect，也不得覆盖正在执行的 owner。
	ErrEnterSceneIdempotencyConflict uint32 = 16
)
