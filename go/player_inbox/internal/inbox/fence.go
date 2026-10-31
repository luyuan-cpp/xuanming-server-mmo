package inbox

import (
	inboxpb "proto/player_inbox"
)

// owner_epoch 围栏(设计 §7.8、IB-8)。
//
// req == 0 时拒绝(收件箱没有历史包袱,fail-closed,与资产通道一致);有核心 Redis 上界 R。
//
//	| 条件                       | 结局           | 动作                                   |
//	| req == 0                   | EPOCH_REQUIRED | 拒绝,不改任何状态                      |
//	| req < fence                | STALE          | 拒绝,不投递、不终结(另按 R 分类)      |
//	| req == fence               | MATCH          | 放行                                   |
//	| req > fence 且 req > R     | EPOCH_AHEAD    | 拒绝并告警,不改状态(只会来自缺陷或伪造)|
//	| fence == 0 且 req ≤ R      | FIRST          | fence = req                            |
//	| fence < req ≤ R            | ADVANCE        | fence = req                            |
//
// R 的读取约定(调用方负责,见 Store):只在快照读到的 req > fence 时,才在**事务外**读一次 R。
// epoch 只增不减,所以「先读 R、后在锁内判定并写」做上界判定没有竞态:锁内看到的 fence 只会更大,
// R 在读之后也只会更大。判定与写入在同一个事务、持有流行行锁期间完成,不依赖 RowsAffected。

// NeedsEpochBound 报告判定是否需要核心 Redis 上界 R:只有 req > fence(且 req 非 0)时才需要。
func NeedsEpochBound(req, fence uint64) bool {
	return req != 0 && req > fence
}

// DecideFence 是围栏判定纯函数。r 只在 NeedsEpochBound(req, fence) 为真时有意义;
// 调用方在不需要 R 时可传任意值(此时结果与 r 无关)。
func DecideFence(req, fence, r uint64) inboxpb.PlayerInboxFenceVerdict {
	switch {
	case req == 0:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED
	case req < fence:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE
	case req == fence:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_MATCH
	case req > r:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_AHEAD
	case fence == 0:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST
	default:
		return inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_ADVANCE
	}
}

// FenceAllows 报告围栏是否放行(MATCH / FIRST / ADVANCE)。
func FenceAllows(v inboxpb.PlayerInboxFenceVerdict) bool {
	switch v {
	case inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_MATCH,
		inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST,
		inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_ADVANCE:
		return true
	default:
		return false
	}
}

// FenceWrites 报告放行结局是否要把 fence_epoch 写成 req(FIRST / ADVANCE)。
func FenceWrites(v inboxpb.PlayerInboxFenceVerdict) bool {
	return v == inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST ||
		v == inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_ADVANCE
}

// StaleKind 是 STALE 的分类(设计 §7.8、§8 #14),只进日志与指标,不改变 STALE 本身的处置。
type StaleKind uint8

const (
	// StaleUnclassified:分类用的那次 R 读失败,无法区分。
	StaleUnclassified StaleKind = iota
	// StaleZombie:R > req,请求来自已失去归属的旧 owner(僵尸 scene),属正常收尾路径上的迟到请求。
	StaleZombie
	// StaleFenceRegressed:R ≤ req < fence,核心 Redis 里的 owner_epoch 倒退了(备份恢复 / AOF 丢尾 / 键被删)。
	// critical:按 db-write-fencing-mysql.md §9.1 定真源、踢下线、epoch-raise(收件箱的 max_fence_epoch 是取最大值的来源之一),
	// **不归零**(设计 §7.8)。
	StaleFenceRegressed
)

// String 返回分类的日志 / 指标标签。
func (k StaleKind) String() string {
	switch k {
	case StaleZombie:
		return "zombie"
	case StaleFenceRegressed:
		return "fence_regressed"
	default:
		return "unclassified"
	}
}

// ClassifyStale 按返回前再读一次的 R 给 STALE 分类。
//
// 设计 §7.8 写的是「R == req 记 fence_regressed,否则 zombie」;这里把 R < req 也归入 fence_regressed:
// 那表示核心 Redis 的值比一个仍在请求的旧 owner 还小,同样只可能是计数器倒退,归进 zombie 会把一条
// critical 信号藏进正常收尾的计数里。
func ClassifyStale(req, r uint64) StaleKind {
	if r <= req {
		return StaleFenceRegressed
	}
	return StaleZombie
}
