package rolefence

import (
	"context"
	"database/sql"
	"errors"
	"time"

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
	return []string{rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName,
		rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName}
}

// PurgeFunc 在删角收尾的事务里删本库里这名已删除玩家的个人数据(docs/design/account-deletion.md §6)。调用时已持有副本行锁、
// 复核过副本是墓碑:墓碑之后本库不会再有这名玩家的业务写入,所以不用两阶段。只删只属于这名玩家的行,与其他玩家共有的记录
// (成交、对局、资产审计)保留。必须可重复执行:没有可删的行就是成功。
// 提交之后才能做的非数据库副作用(缓存失效、摘榜、推送)用 OnCommit 登记,不要在事务里做。
type PurgeFunc func(ctx context.Context, tx *sql.Tx, player uint64) error

// NoPurge 给本库没有要清的个人数据的参与者用(data_service 流水库的回收审计按 §6.2 保留)。
func NoPurge(context.Context, *sql.Tx, uint64) error { return nil }

// errPurgeNotRemoved:副本在锁内看到的不是墓碑。墓碑之后不会再有版本更大的非墓碑状态,所以这是数据错乱,不清。
var errPurgeNotRemoved = errors.New("角色护栏副本不是墓碑,不清数据")

// commitHookTimeout 是提交后动作的总预算。它们在事务之外跑,与协调者的调用截止脱钩(协调者等不及就重放清理,
// 重放时没有可删的行、也就不再登记动作),所以给一份独立的短预算。
const commitHookTimeout = 3 * time.Second

type commitHooksKey struct{}

type commitHooks struct{ fns []func(context.Context) }

// OnCommit 给 PurgeFunc 用:登记一个在这次清理事务提交成功之后才跑的动作。回滚、提交失败都不跑。
// 动作跑在事务之外,失败只能记日志,数据库的结论不受影响;所以只放可丢的副作用,丢了要有别的路径兜底
// (缓存按 TTL 过期、榜单启动时重建、客户端打开界面时拉取)。
// ctx 不是 PurgeRemovedRole 交给 PurgeFunc 的那个时回错误:清理整笔回滚,协调者下一轮再调。
func OnCommit(ctx context.Context, fn func(context.Context)) error {
	hooks, ok := ctx.Value(commitHooksKey{}).(*commitHooks)
	if !ok || fn == nil {
		return errors.New("rolefence.OnCommit used outside PurgeRemovedRole")
	}
	hooks.fns = append(hooks.fns, fn)
	return nil
}

type probingKey struct{}

// WithProbing 标出这一次挂起判定是试探:ParticipantServer 在 dry_run 时设置;单测直接调 PendingFunc 时用它模拟试探。
func WithProbing(ctx context.Context) context.Context {
	return context.WithValue(ctx, probingKey{}, true)
}

// Probing 告诉 PendingFunc 这一次是不是试探(PrepareRoleFreeze 的 dry_run:账号注销申请时的预检、寄售前的预检)。
// 只有「要玩家自己先处理、到执行时由删角收尾代为处理」的挂起看它(guild 的帮主:申请注销时拦,让玩家自己转让;
// 执行时放行,收尾自动让位,见 docs/design/account-deletion.md §6.3)。其余挂起两种情况一律同样判。
func Probing(ctx context.Context) bool {
	probing, _ := ctx.Value(probingKey{}).(bool)
	return probing
}

// PendingFunc 在 PrepareRoleFreeze 的事务里 FOR UPDATE 读本库的挂起全集。此时已持有副本行锁,新的业务写入排在后面。
// dry_run 时没有副本行锁,结论只是试探,事务回滚;ctx 带 Probing 标记。
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
	purge   PurgeFunc
	auth    *serviceauth.Verifier
}

// ValidateCoordinatorSecret 只核协调者密钥能不能用(与 NewParticipantServer 同一判据)。参与者要晚于节点注册才装配的服务
// (guild:删角收尾要用 logic 推送)在注册进 etcd 之前先核一遍,配置错的实例不注册。
func ValidateCoordinatorSecret(secret string) error {
	_, err := serviceauth.New(map[string]string{CoordinatorCaller: secret})
	return err
}

// NewParticipantServer 装配参与者。db 是本服务的逻辑库,secret 是 CoordinatorSecretEnv 的值。
// pending 与 purge 都必须给:没有挂起全集的用 NoPending,没有要清的数据的用 NoPurge,不给算装配失败。
func NewParticipantServer(db *sql.DB, pending PendingFunc, purge PurgeFunc, secret string) (*ParticipantServer, error) {
	if db == nil || pending == nil || purge == nil {
		return nil, errors.New("角色护栏参与者缺连接、挂起判定或删角清理")
	}
	auth, err := serviceauth.New(map[string]string{CoordinatorCaller: secret})
	if err != nil {
		return nil, err
	}
	return &ParticipantServer{db: db, pending: pending, purge: purge, auth: auth}, nil
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
		blocker, err := s.pending(WithProbing(ctx), tx, state)
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

// PurgeRemovedRole 删角收尾清数据(docs/design/account-deletion.md §6.1)。state 必须是墓碑(账号为空、没有令牌)。
// 一个事务:按版本合并进副本,锁副本复核是墓碑,再调本服务的 purge。失败带内回 retry,协调者下一轮整组再调。
func (s *ParticipantServer) PurgeRemovedRole(ctx context.Context, in *rfpb.PurgeRemovedRoleRequest) (*rfpb.PurgeRemovedRoleResponse, error) {
	if err := s.admit(ctx, rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in); err != nil {
		return nil, err
	}
	state := in.GetState()
	if !ValidState(state) || state.Account != "" || state.FreezeToken != 0 {
		return nil, status.Error(codes.InvalidArgument, "只清已删除的角色")
	}
	if err := s.purgeRemoved(ctx, state); err != nil {
		logx.Errorf("[role-fence] purge failed: player=%d version=%d err=%v", state.PlayerId, state.Version, err)
		return &rfpb.PurgeRemovedRoleResponse{Retry: true}, nil
	}
	return &rfpb.PurgeRemovedRoleResponse{}, nil
}

func (s *ParticipantServer) purgeRemoved(ctx context.Context, state *rfpb.RoleFenceState) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = Apply(ctx, tx, state); err != nil {
		return err
	}
	fence, err := Lock(ctx, tx, state.PlayerId)
	if err != nil {
		return err
	}
	if fence.Account != "" {
		return errPurgeNotRemoved
	}
	hooks := &commitHooks{}
	if err = s.purge(context.WithValue(ctx, commitHooksKey{}, hooks), tx, state.PlayerId); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(hooks.fns) > 0 {
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitHookTimeout)
		defer cancel()
		for _, fn := range hooks.fns {
			fn(hookCtx)
		}
	}
	return nil
}
