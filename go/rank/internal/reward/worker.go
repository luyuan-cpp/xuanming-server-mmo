// Package reward只把已封榜且持久的奖励意图交给邮箱，不直接更改玩家资产。
package reward

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"shared/drainproof"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	"rank/internal/data"
	"rank/internal/metrics"
	"shared/serviceauth"
)

type Worker struct {
	// Relay 由进程在启动 worker 前注入，同步RPC返回不能代替邮件终局。
	Relay  *drainproof.Session
	Repo   *data.Repo
	Mail   mailpb.MailAdminClient
	Secret string
	Now    func() time.Time
}

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
			if err != nil {
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
	atts := &mailpb.MailAttachments{}
	if err = proto.Unmarshal(p.Attachments, atts); err != nil {
		outcome = metrics.RewardInvalid
		return w.Repo.SettleReward(ctx, p, 0, now+3600000, now)
	}
	// 到期时间从封榜时固定，所有重投载荷完全相同；不能每次now+TTL制造幂等冲突。
	request := &mailpb.SendMailRequest{SenderKind: 3, RequestId: p.RewardId, RecipientPlayerId: p.PlayerId, Title: "排行榜结算奖励", Body: "本期排行榜奖励已结算，请领取附件。", Attachments: atts, ExpireMs: p.CreatedMs + 30*86400000}
	if err = serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, request, "rank", w.Secret, now); err != nil {
		return err
	}
	call, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	response, callErr := w.Mail.SendPersonalMail(call, request)
	stop()
	mailID := uint64(0)
	if callErr == nil && response.GetErrorMessage().GetId() == 0 {
		mailID = response.GetMailId()
	}
	switch {
	case callErr != nil || response == nil || (response.GetErrorMessage().GetId() == 0 && mailID == 0):
		if w.Relay != nil {
			w.Relay.Hold()
		}
		outcome = metrics.RewardUnknown
	case mailID != 0:
		outcome = metrics.RewardDelivered
	default:
		outcome = metrics.RewardRejected
	}
	attempts := p.Attempts
	if attempts > 6 {
		attempts = 6
	}
	delay := uint64(1000) << attempts
	if delay > 60000 {
		delay = 60000
	}
	stamp := uint64(w.now().UnixMilli())
	return w.Repo.SettleReward(ctx, p, mailID, stamp+delay, stamp)
}
