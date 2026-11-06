package reward

import (
	mailpb "proto/mail"
	"rank/internal/data"
	"shared/serverbase"
	"time"
)

// attempt 是一次投递的结果类别。
type attempt uint8

const (
	// attemptDelivered 邮件回了 mail_id(含判重回原邮件)。
	attemptDelivered attempt = iota + 1
	// attemptUnknown 结果未知:传输失败、没有回包、成功码却没有 mail_id,或邮件回故障码/码表外的码。
	// 邮件可能已经提交,不能据此放弃。
	attemptUnknown
	// attemptRejected 邮件明确拒收(业务拒绝码),或本地原件损坏没有发出。
	attemptRejected
)

// MaxAttempts 是一条奖励的总尝试次数上限(含结果未知),只在明确拒收时裁决,只兜时钟或数据异常。
// 必须大于发件期(封榜到截止点)里可能出现的最多次数,否则会先于截止点放弃一条仍可领取的奖励:发件期最长 29 天
// (data.MaxRewardMailWindow 减 data.MinRewardMailClaimWindow),即使全程按结果未知最快的节奏重投(1 秒起翻倍、60 秒封顶)
// 也只有约 41765 次(TestAttemptCapNeverPreemptsExpiryWithinRewardWindow 按同一退避算)。2026-11-06 评审前是 2000:
// 邮件结果未知连续约 33 小时后的第一次拒收(如收件箱满)就会把还有几十天可领的奖励转成放弃。
const MaxAttempts = 50000

// 结果未知按秒级退避:多是传输或邮件短暂故障,恢复要快。拒收按分钟级退避:收件箱满要等玩家腾地方。
const (
	unknownRetryBaseMs    = 1000
	unknownRetryCeilingMs = 60 * 1000
	rejectRetryBaseMs     = 60 * 1000
	rejectRetryCeilingMs  = 60 * 60 * 1000
)

// 截止点与过期后的判重探询(2026-11-06 评审跟进):
//   - 邮件只拒 expire_ms <= now:过期前几分钟那一投会生成一封马上过期、玩家领不到的邮件,却被记成已投,奖励静默丢失。
//     所以发件时剩余可领时长不得少于 minClaimMs(data.MinRewardMailClaimWindow,24 小时);截止点 = 过期时刻 − 24 小时,
//     截止点那一刻(含)还能发,过了就不再发。
//   - 过了截止点不能直接转终态:之前的尝试可能已经写了邮件(结果未知;或上一个领租约的副本发出请求后崩溃,行上不留任何痕迹),
//     这时记成放弃,人工补发就会重复发奖。行上记不下「上一次是不是结果未知」,所以过了截止点一律推迟到过期后判重探询:
//     原样再发一次,过期以后邮件对同一请求只会回原邮件(判重先于一切校验;那封信发出时剩余不少于 24 小时)或明确拒收
//     (确认没有这封信),不会新建。探询被拒才转放弃,所以「放弃」一定是邮件确认过没有这封信(次数到顶除外)。
//   - 探询比过期晚 expiredProbeDelayMs(1 小时):邮件按自己的时钟判过期,这一小时盖住两边的时钟偏差;
//     奖励此时已不可领,晚一点探询不影响结局。
const (
	minClaimMs          = uint64(data.MinRewardMailClaimWindow / time.Millisecond)
	expiredProbeDelayMs = 60 * 60 * 1000
)

// sendCutoffMs 是还能发件的最后时刻(含):此刻发出的邮件恰好剩 minClaimMs 可领。
func sendCutoffMs(expireMs uint64) uint64 {
	if expireMs < minClaimMs {
		return 0
	}
	return expireMs - minClaimMs
}

// expiredProbeMs 是过期后判重探询的时刻。
func expiredProbeMs(expireMs uint64) uint64 { return expireMs + expiredProbeDelayMs }

// sendDeferredTo 判断此刻能否发件:截止点(含)之前照常发;截止点之后、探询时刻之前不发,返回推迟到的探询时刻;
// 探询时刻起照常发(邮件只会回原邮件或明确拒收)。
func sendDeferredTo(nowMs, expireMs uint64) (probeMs uint64, deferred bool) {
	probeMs = expiredProbeMs(expireMs)
	return probeMs, nowMs > sendCutoffMs(expireMs) && nowMs < probeMs
}

// classify 把一次 SendPersonalMail 的返回归类。按 tip 码表定性:只有业务拒绝码算明确拒收,
// 故障码与码表外的码按结果未知处理——邮件提交结果未知时回的就是故障码。
func classify(response *mailpb.SendMailResponse, callErr error) attempt {
	if callErr != nil || response == nil {
		return attemptUnknown
	}
	switch serverbase.TipVerdict(response.GetErrorMessage().GetId()) {
	case serverbase.VerdictOK:
		if response.GetMailId() != 0 {
			return attemptDelivered
		}
		return attemptUnknown
	case serverbase.VerdictBizReject:
		return attemptRejected
	default:
		return attemptUnknown
	}
}

// nextStep 依据本次结果、此前已完成的尝试次数与封榜时写定的过期时刻,给出奖励行的新状态和下次尝试时刻。
// 只计算,不读时钟、不做 I/O。截止点与判重探询见上。
//   - 投到:已投。
//   - 结果未知:留在待投,1 秒起翻倍、60 秒封顶;下次尝试落在截止点与探询时刻之间的,挪到探询时刻。不因过期或次数转终态:
//     放弃会把可能已到账的奖励记成没发;探询时邮件对同一请求只会回原邮件或明确拒收,重投必然收敛。
//   - 明确拒收:本次已到探询时刻(过期后的判重探询被拒,邮件确认没有这封信),或本次已是第 MaxAttempts 次,转放弃终态;
//     已过截止点(截止点那一刻发出的一投,或回包晚于截止点)推迟到探询时刻;否则 1 分钟起翻倍、1 小时封顶,下次尝试不晚于截止点。
//     没有写定过期时刻(expireMs 为 0)的原件没有可投窗口,直接放弃。
func nextStep(result attempt, attempts uint32, nowMs, expireMs uint64) (status uint32, nextMs uint64) {
	cutoffMs, probeMs := sendCutoffMs(expireMs), expiredProbeMs(expireMs)
	switch result {
	case attemptDelivered:
		return data.RewardDelivered, nowMs
	case attemptUnknown:
		nextMs = nowMs + backoff(attempts, unknownRetryBaseMs, unknownRetryCeilingMs)
		if nextMs > cutoffMs && nextMs < probeMs {
			nextMs = probeMs
		}
		return data.RewardPending, nextMs
	}
	if nowMs >= probeMs || attempts+1 >= MaxAttempts {
		return data.RewardAbandoned, nowMs
	}
	if nowMs >= cutoffMs {
		return data.RewardPending, probeMs
	}
	return data.RewardPending, min(nowMs+backoff(attempts, rejectRetryBaseMs, rejectRetryCeilingMs), cutoffMs)
}

// backoff 按已完成次数翻倍,最多翻 6 次再封顶。
func backoff(attempts uint32, baseMs, ceilingMs uint64) uint64 {
	return min(baseMs<<min(attempts, 6), ceilingMs)
}
