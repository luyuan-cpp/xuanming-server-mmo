package team

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"match/internal/authority"

	teampb "proto/team"

	"google.golang.org/protobuf/proto"
)

// sqlTeamStore 是组队的持久化实现；业务规则由Apply/LockMatch统一定义。
// 成员离队与队伍解散保留永久墓碑，旧副本只能读到SQL新版本。
type sqlTeamStore struct{ authority *authority.Store }

func NewSQLStore(owner *authority.Store, sessions SessionLoader, cfg RuleConfig) *Store {
	return &Store{sql: &sqlTeamStore{authority: owner}, sessions: sessions, cfg: cfg}
}

type sqlTeamRow struct {
	version, idleExpires uint64
	state                uint32
	data                 *teampb.TeamAuthorityData
}

type teamSQLReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func teamSQLNow(ctx context.Context, reader teamSQLReader) (uint64, error) {
	var stamp uint64
	err := reader.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&stamp)
	return stamp, err
}

func loadSQLTeam(ctx context.Context, reader teamSQLReader, teamID uint64, lock bool) (*sqlTeamRow, error) {
	query := "SELECT version,payload,state,idle_expires_ms FROM team_authority WHERE team_id=?"
	if lock {
		query += " FOR UPDATE"
	}
	row := &sqlTeamRow{data: &teampb.TeamAuthorityData{}}
	var payload []byte
	err := reader.QueryRowContext(ctx, query, teamID).Scan(&row.version, &payload, &row.state, &row.idleExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil
	}
	if err != nil {
		return nil, err
	}
	if err = proto.Unmarshal(payload, row.data); err != nil {
		return nil, fmt.Errorf("team: SQL原记录损坏: %w", err)
	}
	if row.version == 0 || row.state < 1 || row.state > 2 || (row.state == 1 && row.data.GetRecord().GetTeamId() != teamID) {
		return nil, errors.New("team: SQL原记录版本/身份不一致")
	}
	if row.state == 2 {
		row.data = &teampb.TeamAuthorityData{}
	}
	return row, nil
}

func loadSQLMembership(ctx context.Context, reader teamSQLReader, player uint64, lock bool) (IndexEntry, error) {
	query := "SELECT team_id,epoch FROM team_membership WHERE player_id=?"
	if lock {
		query += " FOR UPDATE"
	}
	var index IndexEntry
	err := reader.QueryRowContext(ctx, query, player).Scan(&index.TeamId, &index.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexEntry{}, nil
	}
	return index, err
}

func sqlTeamSnapshot(player, teamID, stamp uint64, index IndexEntry, row *sqlTeamRow) *Snapshot {
	snap := &Snapshot{PlayerId: player, PlayerTeamId: index.TeamId, PlayerEpoch: index.Epoch, TeamId: teamID, Version: row.version, NowMs: stamp, RecordTTLSeconds: -2}
	if row.state == 1 {
		snap.Record = row.data.Record
		snap.RecordTTLSeconds = 0
		if row.idleExpires > stamp {
			snap.RecordTTLSeconds = int64((row.idleExpires - stamp) / 1000)
		}
	}
	return snap
}

func (s *sqlTeamStore) read(ctx context.Context, player, teamID uint64, free bool) (*Snapshot, error) {
	// 不设 ReadOnly:写者资格门要在同一事务里锁 writer_slot,MySQL 的 READ ONLY 事务里 FOR UPDATE / UPDATE 都是 1792。
	tx, err := s.authority.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if free {
		// GetMyTeam及错误回包自由视图也须核原actor，不能在写围栏拒绝后再无P读取私有名单。
		if err = s.authority.LockTeamReadActor(ctx, tx, player); err != nil {
			return nil, err
		}
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	index, err := loadSQLMembership(ctx, tx, player, false)
	if err != nil {
		return nil, err
	}
	if free {
		teamID = index.TeamId
	}
	row, err := loadSQLTeam(ctx, tx, teamID, false)
	if err != nil {
		return nil, err
	}
	snap := sqlTeamSnapshot(player, teamID, stamp, index, row)
	if index.TeamId == teamID && teamID != 0 && (snap.Record == nil || FindMember(snap.Record, player) == nil) {
		return nil, errors.New("team: SQL成员索引与原记录不一致，拒绝缓存修复")
	}
	if free && teamID != 0 {
		actor, actorErr := teamActor(ctx, player)
		if actorErr != nil {
			return nil, actorErr
		}
		if !proto.Equal(originalByPlayer(row.data.Members, player), actor) {
			return nil, errors.New("team: SQL原成员身份与自由读actor不一致")
		}
	}
	return snap, tx.Commit()
}

func (s *sqlTeamStore) readMembers(ctx context.Context, teamID uint64) (*MembersSnapshot, error) {
	// 不设 ReadOnly,理由同 read。
	tx, err := s.authority.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	row, err := loadSQLTeam(ctx, tx, teamID, false)
	if err != nil {
		return nil, err
	}
	out := &MembersSnapshot{TeamId: teamID, Version: row.version, NowMs: stamp, Indexes: make(map[uint64]IndexEntry)}
	if row.state == 1 {
		out.Record = row.data.Record
		for _, player := range MemberIds(out.Record) {
			index, err := loadSQLMembership(ctx, tx, player, false)
			if err != nil {
				return nil, err
			}
			if index.TeamId != teamID {
				return nil, errors.New("team: SQL成员索引与原记录不一致")
			}
			out.Indexes[player] = index
		}
	}
	return out, tx.Commit()
}

func (s *sqlTeamStore) listInvites(ctx context.Context, player uint64) (uint64, []InviteIndexEntry, error) {
	tx, err := s.authority.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	actor, err := teamActor(ctx, player)
	if err != nil {
		return 0, nil, err
	}
	if err = s.authority.LockTeamReadActor(ctx, tx, player); err != nil {
		return 0, nil, err
	}
	stamp, err := teamSQLNow(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT i.team_id,i.expires_ms FROM team_invite_index i JOIN team_authority t ON t.team_id=i.team_id AND t.state=1 WHERE i.invitee_id=? AND i.expires_ms>? ORDER BY i.expires_ms,i.team_id LIMIT 10", player, stamp)
	if err != nil {
		return 0, nil, err
	}
	var entries []InviteIndexEntry
	for rows.Next() {
		var entry InviteIndexEntry
		if err := rows.Scan(&entry.TeamId, &entry.ExpireAtMs); err != nil {
			rows.Close()
			return 0, nil, err
		}
		entry.Score = strconv.FormatUint(entry.ExpireAtMs, 10)
		entries = append(entries, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		row, readErr := loadSQLTeam(ctx, tx, entry.TeamId, false)
		if readErr != nil {
			return 0, nil, readErr
		}
		consent := consentByPlayer(row.data.Invites, player)
		if consent != nil && proto.Equal(consent.Recipient, actor) {
			filtered = append(filtered, entry)
		}
	}
	return stamp, filtered, tx.Commit()
}
