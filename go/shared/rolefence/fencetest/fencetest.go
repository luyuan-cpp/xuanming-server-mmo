// Package fencetest 是角色护栏的测试替身,生产不得引用。
package fencetest

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	base "proto/common/base"
	rfpb "proto/role_fence"
	"shared/rolefence"
)

// Directory 是内存里的目录,测自愈用:改它不动副本,副本要到下一次 Heal 才跟上。
type Directory struct {
	mu     sync.Mutex
	states map[uint64]*rfpb.RoleFenceState
	// Err 非 nil 时读状态都失败,模拟 data_service 不可达。
	Err error
	// Reads 记下读过的角色,测「RPC 只在重试间隙」和「不该自愈时没有 RPC」。
	Reads []uint64
}

// NewDirectory 建一个空目录。
func NewDirectory() *Directory {
	return &Directory{states: map[uint64]*rfpb.RoleFenceState{}}
}

// Set 写一名角色的权威状态,版本加 1。account 空串表示已删除,token 非 0 是最终冻结。home zone 固定 1。
func (d *Directory) Set(player uint64, account string, revision, token uint64) *rfpb.RoleFenceState {
	return d.put(player, account, revision, token, token != 0)
}

// SetPreparing 写还在投票的准备令牌,版本加 1。
func (d *Directory) SetPreparing(player uint64, account string, revision, token uint64) *rfpb.RoleFenceState {
	return d.put(player, account, revision, token, false)
}

func (d *Directory) put(player uint64, account string, revision, token uint64, final bool) *rfpb.RoleFenceState {
	d.mu.Lock()
	defer d.mu.Unlock()
	var version uint64 = 1
	if old, ok := d.states[player]; ok {
		version = old.Version + 1
	}
	state := &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, FreezeToken: token, FreezeFinal: final, Version: version, HomeZone: 1}
	d.states[player] = state
	return state
}

// GetRoleFenceState 实现 rolefence.StateReader。
func (d *Directory) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Reads = append(d.Reads, player)
	if d.Err != nil {
		return nil, d.Err
	}
	state, ok := d.states[player]
	if !ok {
		return nil, rolefence.ErrRoleNotFound
	}
	return state, nil
}

// ReplicaReader 把副本自己当目录:自愈读回副本现状,不改变任何东西。
// 用例直接改副本行来摆状态时用它,这样 stale 的路径照常走到第二次判定。副本没有这一行时回 ErrRoleNotFound。
type ReplicaReader struct{ DB *sql.DB }

// GetRoleFenceState 实现 rolefence.StateReader。
func (r ReplicaReader) GetRoleFenceState(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	fence, err := rolefence.Read(ctx, r.DB, player)
	if errors.Is(err, rolefence.ErrMissing) {
		return nil, rolefence.ErrRoleNotFound
	}
	if err != nil {
		return nil, errors.Join(rolefence.ErrUnavailable, err)
	}
	return &rfpb.RoleFenceState{PlayerId: player, Account: fence.Account, OwnerRevision: fence.OwnerRevision, FreezeToken: fence.FreezeToken, FreezeFinal: fence.FreezeFinal, Version: fence.Version, HomeZone: fence.HomeZone}, nil
}

// Admissions 是目录入场校验的替身:所有登记都回 Valid,Err 非 nil 时回错误。
type Admissions struct {
	Valid bool
	Err   error
}

// VerifyRoleAdmission 实现 rolefence.AdmissionVerifier。
func (a Admissions) VerifyRoleAdmission(context.Context, *base.RoleAdmissionIdentity) (bool, error) {
	return a.Valid, a.Err
}

// Seed 写一行未冻结的副本,home zone 固定 1,版本 1。
func Seed(t testing.TB, ctx context.Context, db *sql.DB, player uint64, account string, revision uint64) {
	t.Helper()
	if _, err := rolefence.Apply(ctx, db, &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, Version: 1, HomeZone: 1}); err != nil {
		t.Fatalf("seed role fence replica %d: %v", player, err)
	}
}

// Update 直接改副本行,版本加 1。token 非 0 就是最终冻结,account 空串就是已删除。
func Update(t testing.TB, ctx context.Context, db *sql.DB, player uint64, account string, revision, token uint64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "UPDATE role_fence SET account=?,owner_revision=?,freeze_token=?,freeze_final=?,version=version+1 WHERE player_id=?", []byte(account), revision, token, token != 0, player); err != nil {
		t.Fatalf("update role fence replica %d: %v", player, err)
	}
}
