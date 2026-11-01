package rolefence

import (
	"context"
	"database/sql"
	"errors"

	rfpb "proto/role_fence"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PrepareOutcome 是协调者问完全部参与者的结论。零值是 PrepareRetry:没问到就不算通过。
type PrepareOutcome int

const (
	// PrepareRetry:有参与者不可达或版本过时。不改状态,调用方重放。
	PrepareRetry PrepareOutcome = iota
	// PrepareClean:全部参与者已冻结,而且没有挂起。试探时只表示这一刻没有挂起。
	PrepareClean
	// PrepareBlocked:有参与者明确有挂起。
	PrepareBlocked
)

// Methods 是参与者服务的全部方法,给各服务的方法清单用。
func Methods() []string {
	return []string{rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName}
}

// PendingFunc 在 PrepareRoleFreeze 的事务里 FOR UPDATE 读本库的挂起全集。此时已持有副本行锁,新的业务写入排在后面。
// dry_run 时没有副本行锁,结论只是试探,事务回滚。
//
// blocker 非空表示有挂起,是低基数枚举串,进指标 label,不含玩家信息。查询失败或未知状态一律不算通过:
// 查询失败回 err,未知状态回 blocker。state.freeze_token 是这次的令牌,trade 用它放行本 listing 自己的 rmb_freezing。
type PendingFunc func(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (blocker string, err error)

// NoPending 给没有挂起全集的参与者用(friend):只冻结。
func NoPending(context.Context, *sql.Tx, *rfpb.RoleFenceState) (string, error) { return "", nil }

// ParticipantServer 是 RoleFenceParticipant 的实现,每个参与者注册一份。
//
// 两个方法幂等、有版本保护,没有结局未知,内部失败一律带内回 retry。回 Unavailable 的话 PublicationAdmission 会把排空证明打成 Hold。
type ParticipantServer struct {
	rfpb.UnimplementedRoleFenceParticipantServer
	db      *sql.DB
	pending PendingFunc
	auth    *serviceauth.Verifier
}

// NewParticipantServer 装配参与者。db 是本服务的逻辑库,secret 是 CoordinatorSecretEnv 的值。
func NewParticipantServer(db *sql.DB, pending PendingFunc, secret string) (*ParticipantServer, error) {
	if db == nil || pending == nil {
		return nil, errors.New("角色护栏参与者缺连接或挂起判定")
	}
	auth, err := serviceauth.New(map[string]string{CoordinatorCaller: secret})
	if err != nil {
		return nil, err
	}
	return &ParticipantServer{db: db, pending: pending, auth: auth}, nil
}

func (s *ParticipantServer) admit(ctx context.Context, method string, in proto.Message) error {
	caller, err := s.auth.Verify(ctx, method, in)
	if err != nil || caller != CoordinatorCaller {
		return status.Error(codes.PermissionDenied, "角色护栏调用证明无效")
	}
	return nil
}

// PrepareRoleFreeze 一个事务:写入副本,再读本库的挂起全集。有挂起就回滚(本库不冻结),没有就提交。
// 和业务写入的串行点是副本行锁:票先赢,这里读得到它的挂起;冻结先赢,业务在 Lock 上被拒。
//
// dry_run 只读挂起全集,不写副本,事务回滚。它不是判定:提交点仍是真正的 Prepare。
func (s *ParticipantServer) PrepareRoleFreeze(ctx context.Context, in *rfpb.PrepareRoleFreezeRequest) (*rfpb.PrepareRoleFreezeResponse, error) {
	if err := s.admit(ctx, rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in); err != nil {
		return nil, err
	}
	state := in.GetState()
	// Prepare 只冻结还没到最终的令牌:最终冻结是目录在全部通过之后才写的,再问一遍参与者说明协调者错了。
	if !ValidState(state) || state.FreezeToken == 0 || state.FreezeFinal {
		return nil, status.Error(codes.InvalidArgument, "角色护栏状态非法")
	}
	out, err := s.prepare(ctx, state, in.GetDryRun())
	if err != nil {
		logx.Errorf("[role-fence] prepare failed: player=%d version=%d dry_run=%t err=%v", state.PlayerId, state.Version, in.GetDryRun(), err)
		return &rfpb.PrepareRoleFreezeResponse{Retry: true}, nil
	}
	return out, nil
}

func (s *ParticipantServer) prepare(ctx context.Context, state *rfpb.RoleFenceState, dryRun bool) (*rfpb.PrepareRoleFreezeResponse, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if dryRun {
		blocker, err := s.pending(ctx, tx, state)
		if err != nil {
			return nil, err
		}
		return &rfpb.PrepareRoleFreezeResponse{Clean: blocker == "", Blocker: blocker}, nil
	}
	if _, err = Apply(ctx, tx, state); err != nil {
		return nil, err
	}
	fence, err := Lock(ctx, tx, state.PlayerId)
	if err != nil {
		return nil, err
	}
	// 延迟到达的旧 Prepare 版本小,Apply 不生效。版本相同就是同一份状态,令牌不同只可能是数据错乱,同样不算通过。
	if fence.Version != state.Version || fence.FreezeToken != state.FreezeToken {
		return &rfpb.PrepareRoleFreezeResponse{Stale: true}, nil
	}
	blocker, err := s.pending(ctx, tx, state)
	if err != nil {
		return nil, err
	}
	if blocker != "" {
		return &rfpb.PrepareRoleFreezeResponse{Blocker: blocker}, nil
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &rfpb.PrepareRoleFreezeResponse{Clean: true}, nil
}

// ApplyRoleFence 写入副本,版本更大才覆盖。写失败带内回 retry:推送丢了不要紧,副本下一次访问自愈。
func (s *ParticipantServer) ApplyRoleFence(ctx context.Context, in *rfpb.ApplyRoleFenceRequest) (*rfpb.ApplyRoleFenceResponse, error) {
	if err := s.admit(ctx, rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName, in); err != nil {
		return nil, err
	}
	if !ValidState(in.GetState()) {
		return nil, status.Error(codes.InvalidArgument, "角色护栏状态非法")
	}
	if _, err := Apply(ctx, s.db, in.GetState()); err != nil {
		logx.Errorf("[role-fence] apply failed: player=%d version=%d err=%v", in.GetState().PlayerId, in.GetState().Version, err)
		return &rfpb.ApplyRoleFenceResponse{Retry: true}, nil
	}
	return &rfpb.ApplyRoleFenceResponse{}, nil
}
