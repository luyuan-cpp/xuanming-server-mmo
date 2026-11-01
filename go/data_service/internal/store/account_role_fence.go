package store

import (
	"context"
	"database/sql"
	"errors"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/rolefence"

	"google.golang.org/protobuf/proto"
)

// 角色护栏的协调者一侧,规格见 docs/design/horizontal-scale-logical-db.md §3.5。
//
// 目录是唯一权威。account_role.fence_version 在每个改 account、owner_revision、trade_listing_id、prepare_token 的事务里加 1,
// 副本按它合并,大的赢。过户和删角都是两阶段:目录事务先写准备令牌,事务外让全部参与者冻结,全通过之后再推进。
// 准备令牌不挡入场也不算最终冻结:Register、Lookup 只看 trade_listing_id,过户在全通过的事务里才写它。
// 目录事务里不调 RPC。

// FenceCoordinator 是目录事务之外对全部参与者的调用,实现在 internal/rolefencecoord。
type FenceCoordinator interface {
	// DryRun 并行问全部参与者有没有挂起,有限时。不写副本,结论只用来挡住注定被拒的 Begin,提交点仍是 Prepare。
	DryRun(ctx context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome
	// Prepare 并行调全部参与者 PrepareRoleFreeze,有限时。
	Prepare(ctx context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome
	// Push 尽力推送 ApplyRoleFence,调用之后不再读写 state。丢了不要紧,副本下一次访问自愈。
	Push(ctx context.Context, state *rfpb.RoleFenceState)
}

// ConfigureFenceCoordinator 在装配完成、对外服务之前调用。不配的话 Begin、Transfer、Remove 一律拒绝,不能跳过参与者。
func (s *AccountDirectoryStore) ConfigureFenceCoordinator(coordinator FenceCoordinator) {
	s.fences = coordinator
}

const fencedOwnershipColumns = ownershipColumns + ",fence_version"

func scanFencedOwnership(row interface{ Scan(...any) error }, id uint64) (*dspb.AccountRoleOwnership, uint64, error) {
	var account string
	var raw []byte
	var rev, listing, prepare, version uint64
	if err := row.Scan(&account, &raw, &rev, &listing, &prepare, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, accountdirectory.ErrMissing
		}
		return nil, 0, err
	}
	if version == 0 {
		return nil, 0, errors.New("角色护栏版本损坏")
	}
	owned, err := decodeOwnership(id, account, raw, rev, listing, prepare)
	return owned, version, err
}

// lockFencedOwnership 锁角色行,连同护栏版本一起读出来。
func lockFencedOwnership(ctx context.Context, tx *sql.Tx, id uint64) (*dspb.AccountRoleOwnership, uint64, error) {
	return scanFencedOwnership(tx.QueryRowContext(ctx, "SELECT "+fencedOwnershipColumns+" FROM account_role WHERE player_id=? FOR UPDATE", id), id)
}

// fenceStateOf 算护栏状态。freeze_token 是最终令牌,没有最终令牌时是准备令牌;freeze_final 表示它是最终令牌。
func fenceStateOf(id uint64, owned *dspb.AccountRoleOwnership, version uint64) *rfpb.RoleFenceState {
	token, final := owned.TradeListingId, owned.TradeListingId != 0
	if !final {
		token = owned.PrepareToken
	}
	return &rfpb.RoleFenceState{PlayerId: id, Account: owned.Account, OwnerRevision: owned.OwnerRevision, FreezeToken: token, FreezeFinal: final, Version: version, HomeZone: owned.Role.GetZoneId()}
}

// readFenceState 普通读一行的护栏状态。事务里用它读本事务刚改完的值。
func readFenceState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uint64) (*rfpb.RoleFenceState, error) {
	owned, version, err := scanFencedOwnership(q.QueryRowContext(ctx, "SELECT "+fencedOwnershipColumns+" FROM account_role WHERE player_id=?", id), id)
	if err != nil {
		return nil, err
	}
	return fenceStateOf(id, owned, version), nil
}

// RoleFenceState 读权威的护栏状态。已删除的角色回 account 为空的状态,目录里没有这个角色回 ErrMissing。
func (s *AccountDirectoryStore) RoleFenceState(ctx context.Context, id uint64) (*rfpb.RoleFenceState, error) {
	if id == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	return s.steps.readFence(ctx, id)
}

// VerifyRoleAdmission 强读一份入场登记是否仍然有效:就是该实例当前最高世代的原件,而且没有关闭(规格 §3.7)。
// 只读,不取角色行锁。世代和关闭水位都只增不减,调用方据此判断「到这一刻为止一直有效」。
func (s *AccountDirectoryStore) VerifyRoleAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) (bool, error) {
	if !accountdirectory.ValidAdmission(admission) {
		return false, accountdirectory.ErrInvalid
	}
	var highest, closed uint64
	var raw []byte
	err := s.shards.forPlayer(admission.PlayerId).QueryRowContext(ctx, "SELECT highest_generation,closed_through,identity_proto FROM account_role_admission WHERE player_id=? AND scene_instance_id=?", admission.PlayerId, []byte(admission.SceneInstanceId)).Scan(&highest, &closed, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	registered := &base.RoleAdmissionIdentity{}
	if proto.Unmarshal(raw, registered) != nil || highest != admission.Generation || closed >= admission.Generation || !proto.Equal(registered, admission) {
		return false, nil
	}
	return true, nil
}

// pushFence 在事务提交之后推送。没装配协调者时不推,副本照样会自愈。
func (s *AccountDirectoryStore) pushFence(ctx context.Context, state *rfpb.RoleFenceState) {
	if s.fences != nil && state != nil {
		s.fences.Push(ctx, state)
	}
}
