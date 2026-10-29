package logic

import (
	"context"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"match/internal/authority"
	"match/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	pb "proto/match"
	"shared/roleidentity"
)

func admissionWarningLimiter() func(string) {
	var mu sync.Mutex
	var last time.Time
	return func(message string) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) >= 10*time.Second {
			last = time.Now()
			logx.Error(message)
		}
	}
}

func resumeGlobalDirectAdmission(ctx context.Context, s *svc.ServiceContext, record *pb.MatchDirectIntentRecord) (*pb.MatchFormationRecord, error) {
	if record.BattleId != 0 {
		return s.Authority.Formation(ctx, record.BattleId)
	}
	for _, exit := range record.SpectatorExits {
		if err := closeCapturedSpectatorExit(ctx, s, exit); err != nil {
			return nil, err
		}
	}
	return s.Authority.CompletePreparedDirect(ctx, record)
}

// Direct和Solo使用独立扫描预算/工作许可；持续输入以固定高水位分轮，UNKNOWN不挡后项。
func startGlobalDirectAdmissionRecovery(ctx context.Context, s *svc.ServiceContext) {
	s.LaunchRelayTask("match.direct.admission.recovery", func() {
		warnRead, warnOperation := admissionWarningLimiter(), admissionWarningLimiter()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		var after, through authority.DirectCursor
		active := false
		slots := make(chan struct{}, 2)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if len(slots) == cap(slots) {
				continue
			}
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			if !active {
				var err error
				through, err = s.Authority.DirectAdmissionHighWater(call)
				if err != nil {
					cancel()
					warnRead("直接开战待退出高水位读取失败")
					continue
				}
				after = authority.DirectCursor{}
				active = true
			}
			records, err := s.Authority.DirectAdmissionsThrough(call, after, through)
			cancel()
			if err != nil {
				warnRead("直接开战待退出原件读取失败")
				continue
			}
			if len(records) == 0 {
				active = false
				continue
			}
			for _, record := range records {
				if len(slots) == cap(slots) {
					break
				}
				slots <- struct{}{}
				after = authority.DirectCursor{Kind: record.Kind, Owner: record.OwnerId, High: record.RequestHigh, Low: record.RequestLow}
				original := record
				s.LaunchRelayTask("match.direct.admission.operation", func() {
					defer func() { <-slots }()
					call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					defer cancel()
					f, err := resumeGlobalDirectAdmission(call, s, original)
					if err != nil {
						s.HoldRelay(err)
						warnOperation("直接开战原退出仍未确认")
						return
					}
					if f != nil && f.State < 4 {
						launchGlobalFormation(s, f)
					}
				})
			}
		}
	})
}
func resumeGlobalJoinAdmission(ctx context.Context, s *svc.ServiceContext, intent *pb.MatchJoinIntentRecord) error {
	ticket, request, exit, err := authority.DecodePendingJoin(intent)
	if err != nil {
		return err
	}
	// 唯一内部actor来源是已持久原票，不反查当前目录制造新owner。
	original, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision, Role: &base.AccountSimplePlayer{PlayerId: ticket.PlayerId}})
	if err != nil {
		return err
	}
	if exit != nil {
		if err = closeCapturedSpectatorExit(original, s, exit); err != nil {
			return err
		}
	}
	stamp, err := s.Authority.Now(original)
	if err != nil {
		return err
	}
	if ticket.ExpiresMs <= stamp {
		return s.Authority.CancelIntent(original, ticket.PlayerId, ticket.Ticket, ticket.RequestId)
	}
	profile, err := authority.DecodeProfile(ticket.NetworkProfile)
	if err != nil {
		return err
	}
	_, _, err = s.Authority.Join(original, ticket, request.NetworkPlanId, profile.SessionBinding)
	return err
}
func startGlobalJoinAdmissionRecovery(ctx context.Context, s *svc.ServiceContext) {
	s.LaunchRelayTask("match.join.admission.recovery", func() {
		warnRead, warnOperation := admissionWarningLimiter(), admissionWarningLimiter()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		var after, through authority.JoinCursor
		active := false
		slots := make(chan struct{}, 2)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if len(slots) == cap(slots) {
				continue
			}
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			if !active {
				var err error
				through, err = s.Authority.PendingJoinHighWater(call)
				if err != nil {
					cancel()
					warnRead("单排待退出高水位读取失败")
					continue
				}
				after = authority.JoinCursor{}
				active = true
			}
			records, err := s.Authority.PendingJoinsThrough(call, after, through)
			cancel()
			if err != nil {
				warnRead("单排待退出原件读取失败")
				continue
			}
			if len(records) == 0 {
				active = false
				continue
			}
			for _, record := range records {
				if len(slots) == cap(slots) {
					break
				}
				slots <- struct{}{}
				after = authority.JoinCursor{Player: record.PlayerId, High: record.RequestHigh, Low: record.RequestLow}
				original := record
				s.LaunchRelayTask("match.join.admission.operation", func() {
					defer func() { <-slots }()
					call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					defer cancel()
					if err := resumeGlobalJoinAdmission(call, s, original); err != nil {
						s.HoldRelay(err)
						warnOperation("单排原退出仍未确认")
					}
				})
			}
		}
	})
}
