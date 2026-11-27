package svc

import (
	"context"
	"time"

	"chat/internal/authority"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"
	"shared/safego"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zeromicro/go-zero/core/logx"
)

// 世界频道平台补审的后台循环(docs/design/rename-and-content-safety.md §7.3)。
//
// 每个副本都跑。待办在聊天库里(authority/platform_recheck.go):发送事务写待办,这里按行认领、送平台、违规就撤回、写结局。
// 认领在库里裁决,同一条待办同一时刻只有一个副本在做;副本崩了,租期过后别的副本接着做。
//
// 每一次写(认领一批、做一条)之前先进停机准入(RootAdmission),与 RPC 同一个门:停机排空关门之后不再写聊天库,
// 已经在做的那一条做完才放行封存(CloseSource 等它)。没做完的待办留在库里,由下一代副本接着做。
const (
	platformRecheckInterval = time.Second
	platformRecheckBatch    = 16
	// 送检和撤回各用各的时限:共用一个的话,送检等满之后留给撤回落库的时间不到半秒,平台判了违规却撤不掉。
	platformRecheckCheckTimeout   = 4 * time.Second
	platformRecheckRetractTimeout = 3 * time.Second
	// PlatformRejectReason 是平台判违规的撤回原因,撤回记录与推给在线玩家的撤回通知都带它。
	PlatformRejectReason = "platform_async_reject"
)

// platformRecheckTotal 的 outcome 取值:passed / retracted / gave_up / unavailable / error。
var platformRecheckTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Subsystem: "chat",
	Name:      "world_platform_recheck_total",
	Help:      "World chat platform rechecks by outcome (passed|retracted|gave_up|unavailable|error). gave_up means a message stayed visible without a platform verdict.",
}, []string{"outcome"})

// StartPlatformRecheck 起补审循环。停机准入关门之后它在下一拍退出,ctx 取消也退出。没装平台补审(内容安全没配)时不起。
func (sc *ServiceContext) StartPlatformRecheck(ctx context.Context) {
	if sc.Authority == nil || sc.RootAdmission == nil || sc.platformChecker() == nil {
		return
	}
	safego.Go("chat.world_platform_recheck", func() {
		ticker := time.NewTicker(platformRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !sc.runPlatformRecheckOnce(ctx) {
				logx.Info("[chat] World platform recheck loop stopped: admission closed")
				return
			}
		}
	})
}

func (sc *ServiceContext) platformChecker() func(context.Context, cgpb.TextScene, uint64, string) contentguard.Result {
	if sc.PlatformCheck != nil {
		return sc.PlatformCheck
	}
	if sc.ContentGuard != nil && sc.ContentGuard.Enabled() {
		return sc.ContentGuard.CheckPlatform
	}
	return nil
}

// runPlatformRecheckOnce 认领一批并逐条做完。返回 false = 停机准入已经关了。
func (sc *ServiceContext) runPlatformRecheckOnce(ctx context.Context) bool {
	finish, err := sc.RootAdmission.Enter()
	if err != nil {
		return false
	}
	tasks, err := sc.Authority.ClaimPlatformRechecks(ctx, platformRecheckBatch)
	finish()
	if err != nil {
		platformRecheckTotal.WithLabelValues("error").Inc()
		logx.Errorf("[chat] World platform recheck claim failed: %v", err)
	}
	for _, task := range tasks {
		if !sc.recheckOne(ctx, task) {
			return false
		}
	}
	return true
}

// recheckOne 做一条认领到的待补审。返回 false = 停机准入已经关了(这一条没做,租期过后由下一代副本接着做)。
func (sc *ServiceContext) recheckOne(ctx context.Context, task authority.PlatformRecheckTask) bool {
	finish, err := sc.RootAdmission.Enter()
	if err != nil {
		return false
	}
	defer finish()
	outcome, label := sc.decidePlatformRecheck(ctx, task)
	platformRecheckTotal.WithLabelValues(label).Inc()
	if outcome == 0 {
		return true // 这一次没有结局:租期过后认领下一次
	}
	if err := sc.Authority.FinishPlatformRecheck(ctx, task.SenderPlayerID, task.AcceptedMs, outcome); err != nil {
		// 结局没记上:下一次认领时会再判一次。撤回是幂等的,不会撤两次、推两次。
		logx.Errorf("[chat] World platform recheck outcome not recorded, sender=%d send_time_ms=%d: %v", task.SenderPlayerID, task.AcceptedMs, err)
	}
	return true
}

// decidePlatformRecheck 判一条待补审的结局。返回 0 = 这一次没有结局(平台不可用,或撤回没落库),留给下一次尝试。
func (sc *ServiceContext) decidePlatformRecheck(ctx context.Context, task authority.PlatformRecheckTask) (authority.RecheckOutcome, string) {
	switch {
	case task.Exhausted:
		logx.Errorf("[chat] metric=chat_world_platform_recheck_gave_up sender=%d send_time_ms=%d attempts=%d: "+
			"the platform stayed unavailable, the message stays visible without a platform verdict",
			task.SenderPlayerID, task.AcceptedMs, task.Attempt)
		return authority.RecheckGaveUp, "gave_up"
	case task.Missing:
		logx.Errorf("[chat] metric=chat_world_platform_recheck_gave_up sender=%d send_time_ms=%d: the original is gone",
			task.SenderPlayerID, task.AcceptedMs)
		return authority.RecheckGaveUp, "gave_up"
	case task.AlreadyRetracted:
		return authority.RecheckRetracted, "retracted"
	}
	checker := sc.platformChecker()
	if checker == nil {
		return 0, "unavailable"
	}
	checkCtx, cancel := context.WithTimeout(ctx, platformRecheckCheckTimeout)
	result := checker(checkCtx, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, task.SenderPlayerID, task.Content)
	cancel()
	switch result.Verdict {
	case contentguard.Reject:
		return sc.retractPlatformRejected(ctx, task)
	case contentguard.Unavailable:
		logx.Errorf("[chat] World platform recheck unavailable, sender=%d send_time_ms=%d attempt=%d of %d",
			task.SenderPlayerID, task.AcceptedMs, task.Attempt+1, authority.PlatformRecheckMaxAttempts)
		return 0, "unavailable"
	default:
		return authority.RecheckPassed, "passed"
	}
}

// retractPlatformRejected 撤回平台判违规的那条,并推给在线玩家。撤回没落库时返回 0,下一次尝试再撤。
func (sc *ServiceContext) retractPlatformRejected(ctx context.Context, task authority.PlatformRecheckTask) (authority.RecheckOutcome, string) {
	// 撤回另起一个时限:送检可能已经把它那个时限耗得差不多了。
	retractCtx, cancelRetract := context.WithTimeout(ctx, platformRecheckRetractTimeout)
	defer cancelRetract()
	retracted, tip, err := sc.Authority.RetractWorld(retractCtx, task.SenderPlayerID, task.AcceptedMs, PlatformRejectReason)
	if err != nil {
		logx.Errorf("[chat] metric=chat_world_platform_reject_not_retracted sender=%d send_time_ms=%d: %v", task.SenderPlayerID, task.AcceptedMs, err)
		return 0, "error"
	}
	if tip != 0 {
		// 原件已经不在(保留期删掉了):没有东西可撤。
		logx.Errorf("[chat] World platform reject could not be retracted, sender=%d send_time_ms=%d tip=%d", task.SenderPlayerID, task.AcceptedMs, tip)
		return authority.RecheckGaveUp, "gave_up"
	}
	if retracted {
		sc.PublishWorldRetract(retractCtx, &chatpb.ChatRetractedNotify{
			Channel:        chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
			SenderPlayerId: task.SenderPlayerID,
			SendTimeMs:     task.AcceptedMs,
			Reason:         PlatformRejectReason,
		})
	}
	return authority.RecheckRetracted, "retracted"
}
