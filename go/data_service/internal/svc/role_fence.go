package svc

import (
	"context"
	"errors"
	"strings"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/rolefencecoord"
	"data_service/internal/store"
	base "proto/common/base"
	rfpb "proto/role_fence"
	"shared/authoritysql"
	"shared/rolefence"
	"shared/safego"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// 删角第 1 步之后超过这么久还没收尾,后台接着做。规格 §3.5。
	roleRemovalResumeAfter    = 5 * time.Minute
	roleRemovalResumeInterval = time.Minute
	roleRemovalResumeBatch    = 64
	roleRemovalResumePoint    = "data_service.role_removal.resume"

	// 超过这么久还是 PENDING 的账号角色索引才清,一轮最多这么多条。规格 §5.2。
	roleRefSweepAfter = 5 * time.Minute
	roleRefSweepBatch = 64
	roleRefSweepPoint = "data_service.role_ref.sweep"

	// 没人重放的 PREPARING 一轮最多撤销这么多条,超时口径在 store。规格 §3.5。
	rolePrepareAbortBatch = 64
	rolePrepareAbortPoint = "data_service.role_prepare.abort"
)

// RoleFenceStateReader 读角色护栏的权威状态。*store.AccountDirectoryStore 满足它。
type RoleFenceStateReader interface {
	RoleFenceState(ctx context.Context, playerID uint64) (*rfpb.RoleFenceState, error)
}

// RoleAdmissionChecker 校验入场登记是否仍然有效。*store.AccountDirectoryStore 满足它。
type RoleAdmissionChecker interface {
	VerifyRoleAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) (bool, error)
}

// RoleRemovalResumer 把超时的删角接着做。*store.AccountDirectoryStore 满足它。
type RoleRemovalResumer interface {
	ResumeRemovals(ctx context.Context, olderThan time.Duration, limit int) (store.RemovalResumeResult, error)
}

// RoleRefSweeper 清陈旧的账号角色索引。*store.AccountDirectoryStore 满足它。
type RoleRefSweeper interface {
	SweepStaleRefs(ctx context.Context, olderThan time.Duration, limit int) (store.SweepResult, error)
}

// RolePreparingAborter 撤销没人重放的超时 PREPARING。*store.AccountDirectoryStore 满足它。
type RolePreparingAborter interface {
	AbortStalePreparing(ctx context.Context, limit int) (store.PrepareAbortResult, error)
}

// configureRoleFence 装配协调者和 GetRoleFenceState 的鉴权。失败只记日志、保持缺席:
// 协调者缺席时过户和删角拒绝,鉴权缺席时读权威状态拒绝,其余功能不受影响。
func (s *ServiceContext) configureRoleFence(c config.Config, directory *store.AccountDirectoryStore, getenv func(string) string) {
	s.RoleFenceStates = directory
	s.RoleAdmissionChecks = directory
	s.RoleRefs = directory
	s.RolePreparing = directory
	if auth, err := newRoleFenceAuth(getenv); err != nil {
		logx.Errorf("[role-fence] no caller key usable; GetRoleFenceState disabled: %v", err)
	} else {
		s.RoleFenceAuth = auth
	}
	coordinator, err := rolefencecoord.New(c.RoleFence, getenv(rolefence.CoordinatorSecretEnv))
	if err != nil {
		logx.Errorf("[role-fence] coordinator not assembled; role trades and removals stay rejected: %v", err)
		return
	}
	// 流水库是第七个参与者:有未结束的 GM 回收不能过户、不能删。接不上就不装配协调者,少问一个就是漏冻结。
	participant, err := s.recallFenceParticipant(getenv(rolefence.CoordinatorSecretEnv))
	if err != nil {
		logx.Errorf("[role-fence] recall participant not assembled; role trades and removals stay rejected: %v", err)
		if closeErr := coordinator.Close(); closeErr != nil {
			logx.Errorf("[role-fence] close coordinator: %v", closeErr)
		}
		return
	}
	coordinator.AddLocalParticipant(participant)
	directory.ConfigureFenceCoordinator(coordinator)
	s.roleFenceCoordinator = coordinator
	s.RoleRemovals = directory
}

// newRoleFenceAuth 只收配了密钥的调用方。没配的调用方验不过,读不了权威状态,其余调用方照常。
func newRoleFenceAuth(getenv func(string) string) (*serviceauth.Verifier, error) {
	secrets := make(map[string]string, len(rolefence.Callers))
	for _, caller := range rolefence.Callers {
		secret := getenv(rolefence.SecretEnv(caller))
		if strings.TrimSpace(secret) == "" {
			logx.Errorf("[role-fence] %s is not set; caller %s cannot read role fence state", rolefence.SecretEnv(caller), caller)
			continue
		}
		secrets[caller] = secret
	}
	return serviceauth.New(secrets)
}

// StartRoleRemovalResumer 起目录的后台收尾:接着做超时的删角,撤销没人重放的超时 PREPARING,清陈旧的账号角色索引。
// main 通过启动自检之后调用一次;都缺席时不起。每一轮走 RELAY 门禁,关停时由 StopRoleRemovalResumer 收回。
func (s *ServiceContext) StartRoleRemovalResumer() {
	if s.RoleRemovals == nil && s.RoleRefs == nil && s.RolePreparing == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.stopRemovalResumer = func(wait context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-wait.Done():
			return wait.Err()
		}
	}
	safego.Go(roleRemovalResumePoint, func() {
		defer close(done)
		ticker := time.NewTicker(roleRemovalResumeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.resumeRoleRemovals(ctx)
				s.abortStaleRolePreparing(ctx)
				s.sweepStaleRoleRefs(ctx)
			}
		}
	})
}

// StopRoleRemovalResumer 停后台收尾并等它退出。要排在 RELAY 排空之前:它的 SQL 写在 RELAY 门禁内。没起过时直接返回。
func (s *ServiceContext) StopRoleRemovalResumer(ctx context.Context) error {
	if s.stopRemovalResumer == nil {
		return nil
	}
	return s.stopRemovalResumer(ctx)
}

func (s *ServiceContext) resumeRoleRemovals(ctx context.Context) {
	if s.RoleRemovals == nil {
		return
	}
	finish, err := s.RelayAdmission.Enter()
	if err != nil {
		return
	}
	defer finish()
	finished := safego.Run(roleRemovalResumePoint, func() {
		result, err := s.RoleRemovals.ResumeRemovals(ctx, roleRemovalResumeAfter, roleRemovalResumeBatch)
		if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
			s.RelayAdmission.Hold()
		}
		// 逐行结局都计(出错的一轮也照记):收尾没做完(unsettled)不算完成,持续出现由告警 DataServiceRoleRemovalResumeStuck 报。
		metrics.ObserveRoleRemovalResume(result.Finished, result.Abandoned, result.Unsettled, result.Failed)
		if err != nil && ctx.Err() == nil {
			logx.Errorf("[role-fence] resume removals: finished=%d abandoned=%d unsettled=%d failed=%d err=%v",
				result.Finished, result.Abandoned, result.Unsettled, result.Failed, err)
			return
		}
		if result.Unsettled != 0 {
			logx.Errorf("[role-fence] resume removals: %d removed roles still not settled (name not released or removal row not dropped), retried next round: finished=%d abandoned=%d",
				result.Unsettled, result.Finished, result.Abandoned)
			return
		}
		if result.Finished != 0 || result.Abandoned != 0 {
			logx.Infof("[role-fence] resume removals: finished=%d abandoned=%d failed=%d", result.Finished, result.Abandoned, result.Failed)
		}
	})
	if !finished {
		s.RelayAdmission.Hold()
	}
}

func (s *ServiceContext) abortStaleRolePreparing(ctx context.Context) {
	if s.RolePreparing == nil {
		return
	}
	finish, err := s.RelayAdmission.Enter()
	if err != nil {
		return
	}
	defer finish()
	finished := safego.Run(rolePrepareAbortPoint, func() {
		result, err := s.RolePreparing.AbortStalePreparing(ctx, rolePrepareAbortBatch)
		if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
			s.RelayAdmission.Hold()
		}
		if err != nil && ctx.Err() == nil {
			logx.Errorf("[role-fence] abort stale preparing: aborted=%d failed=%d err=%v", result.Aborted, result.Failed, err)
			return
		}
		if result.Aborted != 0 {
			logx.Infof("[role-fence] abort stale preparing: aborted=%d failed=%d", result.Aborted, result.Failed)
		}
	})
	if !finished {
		s.RelayAdmission.Hold()
	}
}

func (s *ServiceContext) sweepStaleRoleRefs(ctx context.Context) {
	if s.RoleRefs == nil {
		return
	}
	finish, err := s.RelayAdmission.Enter()
	if err != nil {
		return
	}
	defer finish()
	finished := safego.Run(roleRefSweepPoint, func() {
		result, err := s.RoleRefs.SweepStaleRefs(ctx, roleRefSweepAfter, roleRefSweepBatch)
		if errors.Is(err, authoritysql.ErrOutcomeUnknown) {
			s.RelayAdmission.Hold()
		}
		// 新标成保留、没写墓碑的建角 ref(角色行不在,但角色提交过):逐条的 ERROR 已在 store 里记过,这里只计数。出错的一轮也照记。
		// 还留着多少条报 gauge(告警 DataServiceRoleRefHeldForReview 按它持续报);这一轮没数成就不写,保留上一轮的值。
		metrics.ObserveRoleRefTombstoneHeld(result.Held)
		if result.HeldCounted {
			metrics.SetRoleRefHeld(result.HeldTotal)
		}
		if err != nil && ctx.Err() == nil {
			logx.Errorf("[account-role-ref] sweep: scanned=%d dropped=%d failed=%d held=%d rechecked=%d held_total=%d err=%v",
				result.Scanned, result.Dropped, result.Failed, result.Held, result.Rechecked, result.HeldTotal, err)
			return
		}
		if result.Dropped != 0 {
			logx.Infof("[account-role-ref] sweep: scanned=%d dropped=%d failed=%d", result.Scanned, result.Dropped, result.Failed)
		}
	})
	if !finished {
		s.RelayAdmission.Hold()
	}
}
