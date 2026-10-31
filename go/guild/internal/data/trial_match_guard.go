package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"shared/authoritysql"
	"shared/matchentry"
)

// 显式跨schema、同主连接池；只读核列，不从DSN推断，不在业务进程补建Match表。
func (r *GuildRepo) ConfigureMatchAuthority(schema string, backend authoritysql.Backend) error {
	if !roleAuthoritySchema.MatchString(schema) {
		return errors.New("历练Match权威schema未配置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := authoritysql.AssertCurrentSession(ctx, r.db, backend); err != nil {
		return err
	}
	for _, query := range []string{
		"SELECT player_id,state,ticket,request_id,formation_id,mode,config_id,team_size,enqueued_ms,not_before_ms,zone_id,rating_centi,team_id,network_profile,expires_ms,selected_region,preferred_region FROM `" + schema + "`.match_authority_ticket LIMIT 0",
		"SELECT actor_id,request_high,request_low,player_id,state FROM `" + schema + "`.team_launch_role LIMIT 0",
		"SELECT actor_id,request_high,request_low,state FROM `" + schema + "`.team_launch LIMIT 0",
		"SELECT battle_id,raw_payload,decision FROM `" + schema + "`.match_battle_completion LIMIT 0",
	} {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	r.matchAuthority = schema
	return nil
}

// 已持全部原角色P，跨域写者也必须同P；TTL、缺父行、未知状态都不是终局证据。
// 观战沿Match首次direct意图自动捕获并驱逐原op，不能由Guild另复制一份watch真相。
func (r *GuildRepo) requireTrialEntryFree(ctx context.Context, tx *sql.Tx, players []uint64, ownLobby uint64) error {
	if r.matchAuthority == "" {
		return ErrWriteConflict
	}
	for _, p := range players {
		queries := []struct {
			query string
			args  []any
		}{
			{"SELECT player_id FROM `" + r.matchAuthority + "`.match_authority_ticket WHERE player_id=? AND " + matchentry.TicketOccupiedSQL + " LIMIT 1 FOR UPDATE", []any{p}},
			{"SELECT r.player_id FROM `" + r.matchAuthority + "`.team_launch_role r LEFT JOIN `" + r.matchAuthority + "`.team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low WHERE r.player_id=? AND (l.state IS NULL OR l.state<>5 OR r.state IS NULL OR r.state<>5) LIMIT 1 FOR UPDATE", []any{p}},
			{"SELECT r.player_id FROM guild_trial_launch_role r LEFT JOIN guild_trial_launch l ON l.request_id=r.request_id WHERE r.player_id=? AND (r.state IS NULL OR r.state<>2 OR l.state IS NULL OR l.state<>3) LIMIT 1 FOR UPDATE", []any{p}},
		}
		for _, q := range queries {
			var occupied uint64
			err := tx.QueryRowContext(ctx, q.query, q.args...).Scan(&occupied)
			if err == nil {
				return ErrWriteConflict
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var id uint64
		err := tx.QueryRowContext(ctx, "SELECT lobby_id FROM guild_trial_lobby_slot WHERE player_id=? FOR UPDATE", p).Scan(&id)
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
