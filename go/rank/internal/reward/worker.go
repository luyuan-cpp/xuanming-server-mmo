// Package reward只把已封榜且持久的奖励意图交给邮箱，不直接更改玩家资产。
package reward

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"shared/authoritysql"
	"shared/drainproof"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"rank/internal/data"
	"rank/internal/metrics"
	"shared/serviceauth"
)

// Store 是工作器对奖励表的全部依赖。生产装配 *data.Repo,单测用内存替身走同一接口。
type Store interface {
	DueRewards(ctx context.Context, now uint64, limit int) ([]uint64, error)
	ClaimReward(ctx context.Context, id, now, until, token uint64) (*rankpb.RankMailRewardRecord, bool, error)
	SettleReward(ctx context.Context, p *rankpb.RankMailRewardRecord, mailID, next, now uint64) error
	AbandonReward(ctx context.Context, p *rankpb.RankMailRewardRecord, now uint64) error
	// DeferReward 不发件、不计尝试次数,只把下次尝试推迟到 next(剩余可领时长不足时用)。
	DeferReward(ctx context.Context, p *rankpb.RankMailRewardRecord, next, now uint64) error
}

var _ Store = (*data.Repo)(nil)

type Worker struct {
	// Relay 由进程在启动 worker 前注入，同步RPC返回不能代替邮件终局。
	Relay  *drainproof.Session
	Repo   Store
	Mail   mailpb.MailAdminClient
	Secret string
	Now    func() time.Time
}

// 标题与正文进载荷摘要:改了它们,已提交但回包丢失的首投在重投时会被邮件判成同请求号不同载荷。
const (
	rewardMailTitle = "排行榜结算奖励"
	rewardMailBody  = "本期排行榜奖励已结算，请领取附件。"
)

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
				logx.Errorf("[rank] 邮件奖励投递失败: %v", err)
			}
		}
	}
}
func (w *Worker) Tick(ctx context.Context) error {
	if w.Repo == nil || w.Mail == nil {
		return errors.New("排行榜奖励工作器未装配")
	}
	now := uint64(w.now().UnixMilli())
	scan, cancel := context.WithTimeout(ctx, time.Second)
	ids, err := w.Repo.DueRewards(scan, now, 100)
	cancel()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = w.process(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (w *Worker) process(ctx context.Context, id uint64) (err error) {
	if w.Relay != nil {
		finish, enterErr := w.Relay.Enter()
		if enterErr != nil {
			return enterErr
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				w.Relay.Hold()
				panic(recovered)
			}
			// 闸门冻结是确定没写,下一轮再领再发。
			if err != nil && !authoritysql.IsWriterFrozen(err) {
				w.Relay.Hold()
			}
		}()
	}
	outcome := metrics.RewardBusy
	defer func() {
		if err != nil {
			outcome = metrics.RewardError
		}
		metrics.ObserveDispatch(outcome)
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2500*time.Millisecond)
	defer cancel()
	var tokenBytes [8]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return err
	}
	token := binary.LittleEndian.Uint64(tokenBytes[:])
	if token == 0 {
		token = 1
	}
	now := uint64(w.now().UnixMilli())
	p, ok, err := w.Repo.ClaimReward(ctx, id, now, now+10000, token)
	if err != nil || !ok {
		return err
	}
	outcome = metrics.RewardUnknown
	// record 按 nextStep 落本次结果;转放弃终态时改记 abandoned 并打错误日志——不会再自动投,要人工核对处理。
	record := func(result attempt, mailID, stamp uint64) error {
		status, next := nextStep(result, p.Attempts, stamp, p.ExpireMs)
		if status != data.RewardAbandoned {
			return w.Repo.SettleReward(ctx, p, mailID, next, stamp)
		}
		if err := w.Repo.AbandonReward(ctx, p, stamp); err != nil {
			return err
		}
		logx.Errorf("[rank] mail reward abandoned, needs manual follow-up: reward_id=%d settlement_id=%d attempts=%d expire_ms=%d last_outcome=%s",
			p.RewardId, p.SettlementId, p.Attempts+1, p.ExpireMs, outcome)
		outcome = metrics.RewardAbandoned
		return nil
	}
	atts := &mailpb.MailAttachments{}
	if proto.Unmarshal(p.Attachments, atts) != nil || p.ExpireMs == 0 {
		// 封榜原件损坏(附件解不开,或没有写定的过期时刻):不发件,按明确拒收处理——
		// 截止点之前按拒收退避等受控修复,之后推迟到探询时刻再判,次数到顶或探询时仍坏转放弃终态。
		outcome = metrics.RewardInvalid
		return record(attemptRejected, 0, now)
	}
	// 剩余可领时长不足 data.MinRewardMailClaimWindow(24 小时):不发件——这时发出的邮件玩家多半领不到,却会被记成已投。也不转终态:之前的尝试可能已经写了邮件
	// (结果未知,或上一个领租约的副本发出后崩溃),推迟到过期后的判重探询再定(policy.go 的 sendDeferredTo)。不计尝试次数。
	if probeMs, deferred := sendDeferredTo(now, p.ExpireMs); deferred {
		outcome = metrics.RewardRejected
		if err = w.Repo.DeferReward(ctx, p, probeMs, now); err != nil {
			return err
		}
		logx.Errorf("[rank] mail reward has less than the minimum claim window left, not sending; duplicate check after expiry: reward_id=%d settlement_id=%d attempts=%d expire_ms=%d check_at_ms=%d",
			p.RewardId, p.SettlementId, p.Attempts, p.ExpireMs, probeMs)
		return nil
	}
	// 载荷每次重投完全相同:请求号是 reward_id,过期时刻是封榜时写定的 expire_ms,标题正文是常量。
	request := &mailpb.SendMailRequest{SenderKind: 3, RequestId: p.RewardId, RecipientPlayerId: p.PlayerId, Title: rewardMailTitle, Body: rewardMailBody, Attachments: atts, ExpireMs: p.ExpireMs}
	if err = serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, request, "rank", w.Secret, now); err != nil {
		return err
	}
	call, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	response, callErr := w.Mail.SendPersonalMail(call, request)
	stop()
	result := classify(response, callErr)
	mailID := uint64(0)
	switch result {
	case attemptDelivered:
		outcome = metrics.RewardDelivered
		mailID = response.GetMailId()
	case attemptRejected:
		outcome = metrics.RewardRejected
	default:
		outcome = metrics.RewardUnknown
		// 结果未知:传输失败、成功码却没有 mail_id、邮件回故障码(serverbase.TipVerdict 判 fault)。三种都可能已经写了邮件,
		// 排空证明不能据同步返回宣称终局,一律挂起(邮件服务注释引的规格 §3.6:回故障 tip 即 Hold)。
		if w.Relay != nil {
			w.Relay.Hold()
		}
	}
	return record(result, mailID, uint64(w.now().UnixMilli()))
}
