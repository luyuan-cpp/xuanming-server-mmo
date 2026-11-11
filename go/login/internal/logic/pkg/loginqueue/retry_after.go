package loginqueue

import "time"

// 排队轮询间隔按名次拉长(缺口清单第 14 条,2026-11-11 定案;规则与 redis-cluster-all-domains-20261110.md §4.3 相同,
// 原排在第 34 条第 4 批,提前在这里落)。
//
// 10 万人排队时,排队主节点的压力来自轮询次数,不来自算名次:每人每 2 秒一次约 5 万段 lookupScript/s,
// 一个 zone 的排队键又只能在一个主节点上。离队头远的人名次变化慢,没必要 2 秒问一次。
// 标准做法是服务端按名次下发更长的 RetryAfterMs(客户端本来就按它轮询),名次与总数的定义都不变:
//
//   - 0 基名次小于 RetryAfterFullRateRanks 的人按配置的 DefaultRetryAfterMs(缺省 2000ms);
//   - 之后与名次成正比:base × (rank+1) / RetryAfterFullRateRanks。放行速率不变时,预计等待时间与名次成正比;
//   - 上限 min(MaxRetryAfterMs, AdmitTTL/2)。轮到之后放行槽只等 AdmitTTL(缺省 60s),客户端最坏在放行之后
//     隔一个间隔才来问,上限取 AdmitTTL 的一半,保证还剩一半时间去取;
//   - 任何情况下都不短于 base。
//
// 估算(10 万人、base 2s):前 2000 名约 1000 次/s,2000~3 万名约 1000 × ln(15) ≈ 2700 次/s,其余 7 万人约 2300 次/s,
// 合计约 6000 次/s,约为固定 2 秒的八分之一。客户端不遵守 RetryAfterMs 时退回原来的固定间隔,不影响正确性:
// 放行槽、续期与过期的判定都不看轮询间隔。
//
// 名次改用「入队序号差」(只省一条 ZRANK,却把客户端看到的名次从精确值改成上界)不做,理由见
// docs/design/login-queue-2026-05.md §14 第 10 条。
const (
	// RetryAfterFullRateRanks:排在这之前的人按 DefaultRetryAfterMs 轮询。
	RetryAfterFullRateRanks = 2000
	// MaxRetryAfterMs:按名次拉长的上限。
	MaxRetryAfterMs = 30000
)

// RetryAfterForRank 回给排队中的客户端的轮询间隔(毫秒)。rank 是 0 基名次(EnqueueResult.Rank、QueueState.Rank),
// baseMs 是配置的 DefaultRetryAfterMs,admitTTL 是配置的 AdmitTTL。baseMs 为 0 时原样回 0(客户端按自己的缺省间隔)。
func RetryAfterForRank(baseMs uint32, rank uint32, admitTTL time.Duration) uint32 {
	if baseMs == 0 || rank < RetryAfterFullRateRanks {
		return baseMs
	}
	limit := uint64(MaxRetryAfterMs)
	if half := uint64(admitTTL/time.Millisecond) / 2; half < limit {
		limit = half
	}
	scaled := uint64(baseMs) * (uint64(rank) + 1) / RetryAfterFullRateRanks
	if scaled > limit {
		scaled = limit
	}
	if scaled < uint64(baseMs) {
		scaled = uint64(baseMs)
	}
	return uint32(scaled)
}
