package data

import (
	"context"
	"database/sql"
	"errors"
)

// 已持全部原角色副本行锁；TTL、缺父行、未知状态都不是终局证据。
// 票和整队开战不在这里读:guild 和 match 不再同一把 P,进事务之前由 match 的 AcquireTrialOccupancy 判
// (docs/design/horizontal-scale-logical-db.md §4)。这里只判 guild 自己的 launch 和大厅。
func (r *GuildRepo) requireTrialEntryFree(ctx context.Context, tx *sql.Tx, players []uint64, ownLobby uint64) error {
	for _, p := range players {
		var occupied uint64
		err := tx.QueryRowContext(ctx, "SELECT r.player_id FROM guild_trial_launch_role r LEFT JOIN guild_trial_launch l ON l.request_id=r.request_id WHERE r.player_id=? AND (r.state IS NULL OR r.state<>2 OR l.state IS NULL OR l.state<>3) LIMIT 1 FOR UPDATE", p).Scan(&occupied)
		if err == nil {
			return ErrWriteConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var id uint64
		err = tx.QueryRowContext(ctx, "SELECT lobby_id FROM guild_trial_lobby_slot WHERE player_id=? FOR UPDATE", p).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if id == 0 {
			continue
		}
		row, err := scanTrialLobby(tx.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=?", id))
		if err != nil {
			return err
		}
		_, o, _, err := lobbyView(row)
		if err != nil {
			return err
		}
		member := false
		for _, role := range o.Roster {
			if role.PlayerId == p {
				member = true
			}
		}
		if !member {
			return ErrWriteConflict
		}
		if row.State == 4 {
			continue
		}
		if ownLobby == 0 || id != ownLobby || row.State != 1 && row.State != 2 {
			return ErrWriteConflict
		}
	}
	return nil
}
