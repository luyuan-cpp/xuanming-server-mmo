package logic

import (
	"context"
	"errors"
	"sync"
	"time"

	"match/internal/authority"
	"match/internal/svc"

	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

type globalReadyCursor struct{ enqueuedMs, player uint64 }

// 独立于撮合循环的有界维护：一次最多8张原票、8个并发、每次3秒。
// 本地游标不代表已完成；UNKNOWN也推进，扫到末尾重新开始，避免未知队首永久挡住离线玩家。
//
// 巡检尊重个人结算的等待退避(2026-10-30,评审 B-6):本地 ReleaseCompletedReady 放不了的票,只有它的等待行
// next_retry_ms 已到(Store.SettlementLookupDue)才向 Inbox Lookup。每 500ms 一轮,不看退避就会对每个离线玩家每轮查一次
// Inbox,绕过发件箱给等待行定的 5 秒到 5 分钟退避;到期确认本来就由终局发件箱负责,跳过不影响释放。
// JoinQueue / GetQueueStatus 的按需路径照旧立即 Lookup。
func (cursor *globalReadyCursor) round(ctx context.Context, s *svc.ServiceContext) {
	ready, err := s.Authority.ReadyAfter(ctx, cursor.enqueuedMs, cursor.player)
	if err != nil {
		logx.Error("全球匹配已建房票巡检读取失败")
		return
	}
	if len(ready) == 0 {
		cursor.enqueuedMs, cursor.player = 0, 0
		return
	}
	var workers sync.WaitGroup
	for _, ticket := range ready {
		if ctx.Err() != nil {
			break
		}
		// 只推进真正调度的条目；故障/UNKNOWN不改变原票，后续完整轮次会重访。
		cursor.enqueuedMs, cursor.player = ticket.EnqueuedMs, ticket.PlayerId
		workers.Add(1)
		original := ticket
		safego.Go("match.global.ready.ticket", func() {
			defer workers.Done()
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			released, err := s.Authority.ReleaseCompletedReady(call, original)
			if released || errors.Is(err, authority.ErrOriginalPlayer) {
				return
			}
			if err == nil {
				var due bool
				if due, err = s.Authority.SettlementLookupDue(call, original); err == nil && due {
					_, err = releaseGlobalFinished(call, s, original)
				}
			}
			if err != nil {
				s.HoldRelay(err)
				logx.Error("全球匹配已建房票巡检失败，保留原票")
			}
		})
	}
	workers.Wait()
}
