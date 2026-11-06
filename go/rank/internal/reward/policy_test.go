package reward

import (
	"errors"
	"testing"

	base "proto/common/base"
	mailpb "proto/mail"
	"rank/internal/data"
	table "shared/generated/pb/table"
)

func tipReply(code uint32) *mailpb.SendMailResponse {
	return &mailpb.SendMailResponse{ErrorMessage: &base.TipInfoMessage{Id: code}}
}

// 只有邮件的业务拒绝码才算明确拒收。故障码、码表外的码、成功码却没有 mail_id 都按结果未知:
// 邮件提交结果未知时回的就是故障码,据此放弃会把可能已到账的奖励记成没发。
func TestClassifyTreatsOnlyBusinessRejectionsAsRejected(t *testing.T) {
	cases := []struct {
		name     string
		response *mailpb.SendMailResponse
		err      error
		want     attempt
	}{
		{"传输失败", nil, errors.New("deadline exceeded"), attemptUnknown},
		{"没有回包", nil, nil, attemptUnknown},
		{"投到", &mailpb.SendMailResponse{MailId: 9}, nil, attemptDelivered},
		{"判重回原邮件", &mailpb.SendMailResponse{MailId: 9, Duplicated: true}, nil, attemptDelivered},
		{"成功码却没有邮件号", &mailpb.SendMailResponse{}, nil, attemptUnknown},
		{"收件箱满", tipReply(uint32(table.MailError_kMailRecipientBoxFull)), nil, attemptRejected},
		{"参数非法(含已过期、超出有效期)", tipReply(uint32(table.CommonError_kInvalidParameter)), nil, attemptRejected},
		{"同请求号不同载荷", tipReply(uint32(table.MailError_kMailRequestConflict)), nil, attemptRejected},
		{"邮件故障码", tipReply(uint32(table.CommonError_kServiceUnavailable)), nil, attemptUnknown},
		{"码表外的码", tipReply(999999), nil, attemptUnknown},
	}
	for _, c := range cases {
		if got := classify(c.response, c.err); got != c.want {
			t.Fatalf("%s: classify=%d want %d", c.name, got, c.want)
		}
	}
}

// 去向只由本次结果、此前尝试次数与封榜时写定的过期时刻决定。截止点 = 过期前 24 小时;探询 = 过期后 1 小时。
func TestNextStepBoundsRejectionRetriesByExpiryAndAttempts(t *testing.T) {
	const minute = uint64(60 * 1000)
	const now = uint64(1_000_000_000_000)
	expire := now + 30*24*60*minute
	cutoff, probe := expire-minClaimMs, expire+expiredProbeDelayMs
	cases := []struct {
		name            string
		result          attempt
		attempts        uint32
		nowMs, expireMs uint64
		wantStatus      uint32
		wantNext        uint64
	}{
		{"投到即已投", attemptDelivered, 3, now, expire, data.RewardDelivered, now},
		{"未知首次1秒后重投", attemptUnknown, 0, now, expire, data.RewardPending, now + 1000},
		{"未知翻倍", attemptUnknown, 3, now, expire, data.RewardPending, now + 8000},
		{"未知封顶60秒", attemptUnknown, 10, now, expire, data.RewardPending, now + 60000},
		{"未知的下次恰在截止点照常", attemptUnknown, 10, cutoff - 60000, expire, data.RewardPending, cutoff},
		{"未知的下次越过截止点挪到探询时刻", attemptUnknown, 10, cutoff - 59999, expire, data.RewardPending, probe},
		{"未知在截止点之后只等探询", attemptUnknown, 0, cutoff + 1, expire, data.RewardPending, probe},
		{"探询结果未知仍待投", attemptUnknown, 5, probe, expire, data.RewardPending, probe + 32000},
		{"未知超过次数上限仍待投", attemptUnknown, MaxAttempts + 5, probe + 1, expire, data.RewardPending, probe + 1 + 60000},
		{"拒收首次1分钟后重投", attemptRejected, 0, now, expire, data.RewardPending, now + minute},
		{"拒收翻倍", attemptRejected, 5, now, expire, data.RewardPending, now + 32*minute},
		{"拒收封顶1小时", attemptRejected, 6, now, expire, data.RewardPending, now + 60*minute},
		{"拒收的下次尝试不晚于截止点", attemptRejected, 20, cutoff - 1000, expire, data.RewardPending, cutoff},
		{"截止点那一投被拒推迟到探询时刻", attemptRejected, 20, cutoff, expire, data.RewardPending, probe},
		{"过期后探询之前被拒仍等探询", attemptRejected, 0, expire + 5, expire, data.RewardPending, probe},
		{"探询被拒即放弃", attemptRejected, 20, probe, expire, data.RewardAbandoned, probe},
		{"拒收且本次是第MaxAttempts次即放弃", attemptRejected, MaxAttempts - 1, now, expire, data.RewardAbandoned, now},
		{"拒收差一次到上限仍重投", attemptRejected, MaxAttempts - 2, now, expire, data.RewardPending, now + 60*minute},
		{"缺写定过期时刻的原件直接放弃", attemptRejected, 0, now, 0, data.RewardAbandoned, now},
	}
	for _, c := range cases {
		status, next := nextStep(c.result, c.attempts, c.nowMs, c.expireMs)
		if status != c.wantStatus || next != c.wantNext {
			t.Fatalf("%s: status=%d next=%d, want status=%d next=%d", c.name, status, next, c.wantStatus, c.wantNext)
		}
	}
}

// 次数上限不得先于截止点到达:邮件长期结果未知(按 60 秒一次重投)之后的第一次拒收,只要还没到截止点就必须继续重投,
// 不能把仍可领取的奖励转成放弃。按工作器自己的退避算出发件期(封榜到截止点,含)里最多能有几次尝试(全程结果未知是最快的节奏),
// 发件期里最后一次尝试被拒时仍应待投(推迟到过期后的判重探询),探询再被拒才转放弃。上限曾是 2000,约 33 小时的结果未知就会触发提前放弃。
func TestAttemptCapNeverPreemptsExpiryWithinRewardWindow(t *testing.T) {
	window := uint64(data.MaxRewardMailWindow.Milliseconds())
	span := window - minClaimMs
	// 第 k 次尝试(从 0 起)的开始时刻 = 前 k 次退避之和;数出开始时刻不晚于截止点的尝试次数。
	var within uint32
	for start := uint64(0); start <= span; within++ {
		start += backoff(within, unknownRetryBaseMs, unknownRetryCeilingMs)
	}
	if within+1 >= MaxAttempts {
		t.Fatalf("发件期里最多 %d 次尝试, 加上过期后的探询会撞上 MaxAttempts=%d", within, MaxAttempts)
	}
	const sealed = uint64(1_000_000_000_000)
	expire := sealed + window
	cutoff, probe := expire-minClaimMs, expire+expiredProbeDelayMs
	// 发件期里最后一次尝试之前已完成 within-1 次。
	if status, next := nextStep(attemptRejected, within-1, cutoff-1, expire); status != data.RewardPending || next != cutoff {
		t.Fatalf("发件期内第 %d 次尝试被拒时尚未到截止点, 应待投到截止点: status=%d next=%d (MaxAttempts=%d)", within, status, next, MaxAttempts)
	}
	if status, next := nextStep(attemptRejected, within-1, cutoff, expire); status != data.RewardPending || next != probe {
		t.Fatalf("截止点那一投被拒应推迟到探询时刻: status=%d next=%d", status, next)
	}
	if status, _ := nextStep(attemptRejected, within, probe, expire); status != data.RewardAbandoned {
		t.Fatalf("过期后的探询被拒才转放弃: status=%d", status)
	}
}

// 截止点(含)之前照常发;之后到探询时刻之前不发、推迟到探询时刻;探询时刻起照常发(邮件只会回原邮件或明确拒收)。
func TestSendDeferredToBlocksOnlyBetweenCutoffAndExpiryCheck(t *testing.T) {
	if minClaimMs != 24*60*60*1000 {
		t.Fatalf("最短可领时长应为 24 小时, 实为 %d ms", minClaimMs)
	}
	const sealed = uint64(1_000_000_000_000)
	expire := sealed + 30*24*60*60*1000
	cutoff, probe := expire-minClaimMs, expire+expiredProbeDelayMs
	for _, c := range []struct {
		name     string
		at       uint64
		deferred bool
	}{
		{"封榜后照常发", sealed, false},
		{"截止点那一刻剩余恰好 24 小时仍发", cutoff, false},
		{"过了截止点 1 毫秒不发", cutoff + 1, true},
		{"过期那一刻不发", expire, true},
		{"探询前 1 毫秒不发", probe - 1, true},
		{"探询时刻起照常发", probe, false},
		{"探询之后照常发", probe + 1, false},
	} {
		got, deferred := sendDeferredTo(c.at, expire)
		if deferred != c.deferred || got != probe {
			t.Fatalf("%s: deferred=%v to=%d, want deferred=%v to=%d", c.name, deferred, got, c.deferred, probe)
		}
	}
	// 窗口比最短可领时长还短的原件(只会是坏数据,配置下限是 2 天):封榜即过截止点,一律等探询。
	if _, deferred := sendDeferredTo(sealed, sealed+60*1000); !deferred {
		t.Fatal("剩余不足最短可领时长的原件不得发件")
	}
}
