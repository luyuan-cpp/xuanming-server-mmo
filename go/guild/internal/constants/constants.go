package constants

// Guild member roles.
const (
	RoleMember  uint32 = 0
	RoleOfficer uint32 = 1
	RoleLeader  uint32 = 3
)

// Business error IDs returned in TipInfoMessage.Id.
const (
	ErrAlreadyInGuild  uint32 = 1
	ErrGuildNotFound   uint32 = 2
	ErrNotInGuild      uint32 = 3
	ErrGuildFull       uint32 = 4
	ErrLeaderCantLeave uint32 = 5
	ErrNotLeader       uint32 = 6
	ErrNoPermission    uint32 = 7
	ErrNotRanked       uint32 = 8
	// ErrIDGenUnavailable:发号器已被 fence(worker id 的 etcd 租约丢了),
	// 本次建帮整体失败。客户端重试即可 —— 进程会退出并由编排重拉、重新拿号。
	ErrIDGenUnavailable uint32 = 9
)

// Default limits.
const (
	DefaultMaxMembers uint32 = 50
	DefaultInitLevel  uint32 = 1
)
