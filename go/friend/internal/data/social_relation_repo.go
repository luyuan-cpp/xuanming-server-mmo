package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BatchCheckSocialRelations 内部只读批量关系检查(供 match 生死状发起前校验两队两两拉黑与好友关系)。
// 仅做普通主键点查，不持有容量守卫锁，不创建任何行。
func (r *FriendRepo) BatchCheckSocialRelations(ctx context.Context, sideA, sideB []uint64) (hasAnyBlock bool, allAreFriends bool, err error) {
	if r == nil || r.db == nil {
		return false, false, errors.New("friend repo database unavailable")
	}
	uniqA, err := dedupePositivePlayerIDs(sideA)
	if err != nil {
		return false, false, err
	}
	uniqB, err := dedupePositivePlayerIDs(sideB)
	if err != nil {
		return false, false, err
	}
	if len(uniqA) == 0 || len(uniqB) == 0 || len(uniqA) > 5 || len(uniqB) > 5 {
		return false, false, fmt.Errorf("invalid team sizes for social relation check: %d vs %d", len(uniqA), len(uniqB))
	}
	setA := make(map[uint64]struct{}, len(uniqA))
	for _, a := range uniqA {
		setA[a] = struct{}{}
	}
	for _, b := range uniqB {
		if _, dup := setA[b]; dup {
			return false, false, errInvalidPlayerPair(b, b)
		}
	}

	allAreFriends = true
	for _, a := range uniqA {
		for _, b := range uniqB {
			blocked, blockErr := r.pairBlockedEitherWayReadOnly(ctx, a, b)
			if blockErr != nil {
				return false, false, blockErr
			}
			if blocked {
				return true, false, nil
			}
			if allAreFriends {
				isFriend, friendErr := r.pairAreMutualFriendsReadOnly(ctx, a, b)
				if friendErr != nil {
					return false, false, friendErr
				}
				if !isFriend {
					allAreFriends = false
				}
			}
		}
	}
	return false, allAreFriends, nil
}

func dedupePositivePlayerIDs(ids []uint64) ([]uint64, error) {
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, fmt.Errorf("player id 0 not allowed")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func (r *FriendRepo) pairBlockedEitherWayReadOnly(ctx context.Context, a, b uint64) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, "SELECT 1 FROM friend_block WHERE player_id = ? AND blocked_player_id = ?", a, b).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check block (%d,%d): %w", a, b, err)
	}
	err = r.db.QueryRowContext(ctx, "SELECT 1 FROM friend_block WHERE player_id = ? AND blocked_player_id = ?", b, a).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check block (%d,%d): %w", b, a, err)
	}
	return false, nil
}

func (r *FriendRepo) pairAreMutualFriendsReadOnly(ctx context.Context, a, b uint64) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, "SELECT 1 FROM friend WHERE player_id = ? AND friend_player_id = ?", a, b).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check friend (%d,%d): %w", a, b, err)
	}
	err = r.db.QueryRowContext(ctx, "SELECT 1 FROM friend WHERE player_id = ? AND friend_player_id = ?", b, a).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check friend (%d,%d): %w", b, a, err)
	}
	return true, nil
}
