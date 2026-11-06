package data

import (
	"context"
	"database/sql"
	"fmt"
	rankpb "proto/rank"
	"rank/internal/metrics"
	"time"
)

// rank_mail_reward.status 的取值。
const (
	RewardPending   uint32 = 1 // 待投:工作器按 next_attempt_ms 领租约投递
	RewardDelivered uint32 = 2 // 已投:邮件回了 mail_id
	// RewardAbandoned 放弃(终态):过期后的判重探询仍被邮件明确拒收(邮件确认没有这封信),或尝试次数到顶。工作器不再投,
	// 行原样保留,人工核对后处理(docs/design/leaderboard-system.md「2026-11-05 奖励邮件过期写定、放弃终态与人工重投」节)。
	RewardAbandoned uint32 = 3
)

// MaxRewardMailWindow 是奖励邮件可领时长的产品上限:封榜后最长 30 天。
const MaxRewardMailWindow = 30 * 24 * time.Hour

// MinRewardMailClaimWindow 是奖励邮件发出时至少要剩的可领时长:24 小时,一个日常登录周期,每天上线一次的玩家也来得及领。
// 剩余不足就不再发新邮件(reward/policy.go):邮件只拒 expire_ms <= now,过期前几分钟发出的那封玩家领不到,却会被记成已投。
const MinRewardMailClaimWindow = 24 * time.Hour

// minRewardMailWindow 是可投窗口的下限:截止点(过期前 24 小时)之前至少留出与最短可领时长等长的发件期,
// 邮件故障一天之内恢复仍投得出。邮件个人邮件有效期配成 1 天时一封都发不出,拒启。
const minRewardMailWindow = 2 * MinRewardMailClaimWindow

// 邮件个人邮件最大有效期的允许范围,与 go/mail/internal/data/policy.go 的 Limits.Validate 同口径。
const mailTtlDaysMin, mailTtlDaysMax = 1, 365

// RewardMailWindow 给出封榜到奖励邮件过期的时长:min(30 天, 邮件个人邮件最大有效期),且不短于 2 天(minRewardMailWindow)。
// mailMaxTtlDays 必须与 mail 的 Mail.MaxPersonalMailTtlDays 同值:邮件按「过期时刻 - 发件时刻 ≤ 有效期」校验,
// 发件时刻不早于封榜,所以过期时刻写成 封榜 + 窗口 就永远落在邮件允许的范围内,不会被判超长。
func RewardMailWindow(mailMaxTtlDays uint32) (time.Duration, error) {
	if mailMaxTtlDays < mailTtlDaysMin || mailMaxTtlDays > mailTtlDaysMax {
		return 0, fmt.Errorf("rank: MailMaxPersonalMailTtlDays=%d outside mail's allowed range %d..%d", mailMaxTtlDays, mailTtlDaysMin, mailTtlDaysMax)
	}
	window := min(time.Duration(mailMaxTtlDays)*24*time.Hour, MaxRewardMailWindow)
	if window < minRewardMailWindow {
		return 0, fmt.Errorf("rank: MailMaxPersonalMailTtlDays=%d gives a reward mail window of %v, shorter than %v (a reward needs %v of claim time left when sent, plus as long a delivery span)",
			mailMaxTtlDays, window, minRewardMailWindow, MinRewardMailClaimWindow)
	}
	return window, nil
}

// ConfigureRewardMailWindow 只在暴露服务之前调用(同 mail 的 ConfigureLimits);运行期不可并发改。
// 只影响此后封榜写出的奖励行,已写定的过期时刻不变。没调用时取 MaxRewardMailWindow。
func (r *Repo) ConfigureRewardMailWindow(window time.Duration) error {
	if window < minRewardMailWindow || window > MaxRewardMailWindow {
		return fmt.Errorf("rank: reward mail window %v outside [%v, %v]", window, minRewardMailWindow, MaxRewardMailWindow)
	}
	r.rewardMailWindow = window
	return nil
}

func (r *Repo) DueRewards(ctx context.Context, now uint64, limit int) (ids []uint64, err error) {
	var oldest uint64
	hasDue := false
	defer func() { metrics.ObserveScan(now, oldest, hasDue, err) }()
	if limit <= 0 || limit > 1000 {
		return nil, ErrInvalid
	}
	rows, err := r.db.QueryContext(ctx, "SELECT reward_id,next_attempt_ms FROM rank_mail_reward WHERE status=? AND next_attempt_ms<=? AND lease_until_ms<? ORDER BY next_attempt_ms,reward_id LIMIT ?", RewardPending, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, due uint64
		if err = rows.Scan(&id, &due); err != nil {
			return nil, err
		}
		if !hasDue {
			oldest, hasDue = due, true
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (r *Repo) ClaimReward(ctx context.Context, id, now, until, token uint64) (*rankpb.RankMailRewardRecord, bool, error) {
	res, err := r.db.ExecContext(ctx, "UPDATE rank_mail_reward SET lease_until_ms=?,lease_token=? WHERE reward_id=? AND status=? AND lease_until_ms<?", until, token, id, RewardPending, now)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return nil, false, err
	}
	p := &rankpb.RankMailRewardRecord{RewardId: id, LeaseToken: token}
	err = r.db.QueryRowContext(ctx, "SELECT settlement_id,player_id,`rank`,attachments,attempts,created_ms,expire_ms FROM rank_mail_reward WHERE reward_id=?", id).Scan(&p.SettlementId, &p.PlayerId, &p.Rank, &p.Attachments, &p.Attempts, &p.CreatedMs, &p.ExpireMs)
	return p, err == nil, err
}

// SettleReward 记一次投递结果:mailID 非 0 记已投;否则留在待投,next 是下次尝试时刻。
// 响应未知保留原意图，下一次同reward_id发信会回放。
func (r *Repo) SettleReward(ctx context.Context, p *rankpb.RankMailRewardRecord, mailID, next, now uint64) error {
	state := RewardPending
	if mailID != 0 {
		state = RewardDelivered
	}
	return r.settle(ctx, p, state, mailID, next, now, 1)
}

// AbandonReward 把仍持本租约的待投奖励转为放弃终态。只由工作器在过期后的判重探询仍被明确拒收、或次数到顶时调用;
// 行与载荷原样保留,人工核对后处理。
func (r *Repo) AbandonReward(ctx context.Context, p *rankpb.RankMailRewardRecord, now uint64) error {
	return r.settle(ctx, p, RewardAbandoned, 0, now, now, 1)
}

// DeferReward 把仍持本租约的待投奖励推迟到 next 再领,不计尝试次数:这一轮没有发件。只由工作器在剩余可领时长不足、
// 还没到过期后判重探询的时刻时调用(reward/policy.go 的 sendDeferredTo)。
func (r *Repo) DeferReward(ctx context.Context, p *rankpb.RankMailRewardRecord, next, now uint64) error {
	return r.settle(ctx, p, RewardPending, 0, next, now, 0)
}

// 先主键点锁再更新带索引字段;只有仍是待投且持本次租约的行可落结果,旧租约与终态之后的迟到写一律 ErrConflict。
// attempts 加 countedAttempts:发过件的结局记 1,没发件的推迟记 0。
func (r *Repo) settle(ctx context.Context, p *rankpb.RankMailRewardRecord, state uint32, mailID, next, now uint64, countedAttempts uint32) error {
	return r.tx(ctx, func(tx *sql.Tx) error {
		var current uint32
		var token uint64
		if err := tx.QueryRowContext(ctx, "SELECT status,lease_token FROM rank_mail_reward WHERE reward_id=? FOR UPDATE", p.RewardId).Scan(&current, &token); err != nil {
			return err
		}
		if current != RewardPending || token != p.LeaseToken {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, "UPDATE rank_mail_reward SET status=?,mail_id=?,next_attempt_ms=?,lease_until_ms=0,attempts=attempts+?,updated_ms=? WHERE reward_id=?", state, mailID, next, countedAttempts, now, p.RewardId)
		return err
	})
}
