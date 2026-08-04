package constants

const (
	ErrCodeOK              uint32 = 0
	ErrCodeRedis           uint32 = 1 // Redis operation failed
	ErrCodeLockConflict    uint32 = 2 // Another writer holds the player lock
	ErrCodeVersionMismatch uint32 = 3 // Optimistic lock version mismatch
	ErrCodeNotFound        uint32 = 4 // Player data or mapping not found

	// Snapshot / Rollback errors
	ErrCodeSnapshotNotFound uint32 = 10 // No snapshot found matching criteria
	ErrCodeSnapshotDBError  uint32 = 11 // MySQL error during snapshot operation
	ErrCodeRollbackFailed   uint32 = 12 // Rollback execution failed
	ErrCodePlayerOnline     uint32 = 13 // Player must be offline for rollback
	ErrCodeInvalidRequest   uint32 = 14 // Missing required fields
	ErrCodeZoneNotFound     uint32 = 15 // Zone has no players

	// ErrCodeNotImplemented 表示这条请求语义上合法,但服务端**没有真正执行**。
	// 存在的理由:宁可让调用方看到一次明确失败,也不能让"什么都没做"伪装成成功
	// —— 后者会让运营以为刷金资产已被回收、事故已处置完毕。
	ErrCodeNotImplemented uint32 = 16

	// ErrCodeResultTruncated 表示筛选命中数超过本次能安全返回/处理的上限。
	// 返回该码时本次操作必须为零变更，调用方应缩小时间或玩家范围后重试。
	ErrCodeResultTruncated uint32 = 17
)
