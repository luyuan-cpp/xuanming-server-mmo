package commerce

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/rolefence"
)

// 明确的目录替身仅隔离原commerce状态机夹具，不作为跨域权限或原生冻结运行证据。
// 判定走真的 rolefence.Check。替身的状态本来就是权威,所以 RunTx 的自愈什么也不改,第二次还判不了就是拒绝。
type fixtureRoles struct {
	Values map[uint64]*dspb.AccountRoleOwnership
}

func (f fixtureRoles) RunTx(_ context.Context, action func() error) error {
	err := action()
	var stale *rolefence.StaleError
	if !errors.As(err, &stale) {
		return err
	}
	err = action()
	if errors.As(err, &stale) {
		return errors.Join(stale.Reject, rolefence.ErrRejected)
	}
	return err
}
func (f fixtureRoles) Ownership(_ context.Context, _ *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	if value := f.Values[player]; value != nil {
		return value, nil
	}
	return &dspb.AccountRoleOwnership{Account: "account-" + strconv.FormatUint(player, 10), OwnerRevision: 1, Role: &base.AccountSimplePlayer{PlayerId: player}}, nil
}
func (f fixtureRoles) Lock(ctx context.Context, tx *sql.Tx, expects ...rolefence.Expect) error {
	for _, expect := range expects {
		v, _ := f.Ownership(ctx, tx, expect.PlayerID)
		fence := rolefence.Fence{PlayerID: expect.PlayerID, Account: v.Account, OwnerRevision: v.OwnerRevision, FreezeToken: v.TradeListingId, FreezeFinal: v.TradeListingId != 0, Version: 1, HomeZone: 1}
		if !fence.FreezeFinal {
			fence.FreezeToken = v.PrepareToken
		}
		if err := rolefence.Check(fence, nil, expect); err != nil {
			return err
		}
	}
	return nil
}

// lockedPlayers 把 Lock 的条件还原成玩家列表,给要按「锁了谁」触发的替身用。
func lockedPlayers(expects []rolefence.Expect) []uint64 {
	players := make([]uint64, 0, len(expects))
	for _, expect := range expects {
		players = append(players, expect.PlayerID)
	}
	return players
}
