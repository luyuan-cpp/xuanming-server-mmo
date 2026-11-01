package authority

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	pb "proto/match"
	"shared/matchentry"
	"shared/rolefence"
)

// 帮会历练的战斗入口占用,规格见 docs/design/horizontal-scale-logical-db.md §4。
//
// guild 和 match 的护栏副本分在两个库,同一把 P 不再串行它们,这张表是单一仲裁:match 是战斗入口占用的唯一权威。
//   - guild 建房、同意、Prepare 之前都先 Acquire,对应旧 requireTrialEntryFree 在事务里读 match 的两条。
//   - match 自己入场读这张表,对应旧的跨库读 guild_trial_launch_role、guild_trial_lobby_slot。
//   - StartActivityBattle 是提交点:占用还由这个大厅持有、名单身份逐项相等,才写票。租约丢了就拒绝,guild 按开局失败收尾。
//
// 写者(Acquire、开局)都先持本库 role_fence 行锁,和入场、冻结同一个串行点。Release 只放开限制,不需要这把锁。
// 同一张表上的锁序:全部 P → 整队开战 → 票 → 占用。

const occupancyKindTrial uint32 = 1

const occupancyColumns = "player_id,owner_kind,owner_id,account,owner_revision,expires_ms"

const (
	// maxOccupancyTTLMs 是单次租约上限。guild 的数字写错也不能把人锁死。
	maxOccupancyTTLMs uint64 = 3600 * 1000
	// occupancyBattleGraceMs 是开局之后在战斗期限之上多留的时间:覆盖从写票到成局的间隙,再给 guild 留出结算和释放的时间。
	occupancyBattleGraceMs = WorkLeaseMs
)

// ErrOccupancyInvalid 表示占用请求的参数不合法。请求是 guild 拼的,走到这里是缺陷,不是业务拒绝。
var ErrOccupancyInvalid = errors.New("历练占用请求参数无效")

func readOccupancyTx(ctx context.Context, tx *sql.Tx, player uint64) (*pb.MatchRoleOccupancyRecord, error) {
	row := &pb.MatchRoleOccupancyRecord{}
	err := tx.QueryRowContext(ctx, "SELECT "+occupancyColumns+" FROM match_role_occupancy WHERE player_id=? FOR UPDATE", player).Scan(&row.PlayerId, &row.OwnerKind, &row.OwnerId, &row.Account, &row.OwnerRevision, &row.ExpiresMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

func heldByTrial(row *pb.MatchRoleOccupancyRecord, owner uint64) bool {
	return row.OwnerKind == occupancyKindTrial && row.OwnerId == owner
}

// requireRoleOccupancyTx 替代旧的两条跨库读:没有未到期的占用才能入场;大厅自己的活动开局只认自己的占用。
//   - source 不是活动(排队、观战、整队、切磋):占用未到期就拒绝。
//   - 活动开局的提交点:占用必须未到期、由这个大厅持有、身份相等。到期或被别人拿走就是租约丢了。
//   - 提交点之后接着成局(admitted):全员票已写,Acquire 看到票就拒绝,所以到期也不会有别的大厅写进来,这里不再要求占用还在。
func requireRoleOccupancyTx(ctx context.Context, tx *sql.Tx, member *pb.MatchAuthorityTicketRecord, source *matchEntryOriginal, stamp uint64) error {
	row, err := readOccupancyTx(ctx, tx, member.PlayerId)
	if err != nil {
		return err
	}
	held := row != nil && row.ExpiresMs > stamp
	if source == nil || source.activity == nil {
		if held {
			return ErrMatchEntryOccupied
		}
		return nil
	}
	if source.admitted {
		if held && !heldByTrial(row, source.activity.RequestId) {
			return ErrMatchEntryOccupied
		}
		return nil
	}
	if !held || !heldByTrial(row, source.activity.RequestId) {
		return ErrMatchEntryOccupied
	}
	if row.Account != member.OwnerAccount || row.OwnerRevision != member.OwnerRevision {
		return ErrOriginalPlayer
	}
	return nil
}

// requireActivityRosterTx 在开局提交点核名单两个方向都相等:每名成员都持有(上面逐人核过),这个大厅也没有名单之外的占用。
func requireActivityRosterTx(ctx context.Context, tx *sql.Tx, group []*pb.MatchAuthorityTicketRecord, source *matchEntryOriginal) error {
	if source == nil || source.activity == nil || source.admitted {
		return nil
	}
	var rows int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_role_occupancy WHERE owner_kind=? AND owner_id=?", occupancyKindTrial, source.activity.RequestId).Scan(&rows); err != nil {
		return err
	}
	if rows != len(group) {
		return ErrMatchEntryOccupied
	}
	return nil
}

// extendOccupancyForBattleTx 在开局提交时把占用延到战斗期限之后。票是 match 自己的占用,这一行留到 guild 结算之后释放:
// 战斗打完、票终局到 guild 结算之间,旧判据靠 launch 未终结拒绝新入场,这段由这一行接着拒。
// guild 一直不释放也只锁到期限,不把人永远锁住。
func (s *Store) extendOccupancyForBattleTx(ctx context.Context, tx *sql.Tx, owner uint64, configID uint32, stamp uint64) error {
	_, err := tx.ExecContext(ctx, "UPDATE match_role_occupancy SET expires_ms=GREATEST(expires_ms,?) WHERE owner_kind=? AND owner_id=?", s.battleDeadlineMs(stamp, configID)+occupancyBattleGraceMs, occupancyKindTrial, owner)
	return err
}

// AcquireTrialOccupancy 占住名单上的全部角色,全成或全不成。同一个大厅重放幂等,只延长不缩短。
//
// offender 非 0 时 err 是 ErrMatchEntryOccupied,offender 是第一个不空闲的角色。角色冻结、已删除或原身份不符回 ErrOriginalPlayer。
// 名单里身份留空(修订号 0)的角色由这里从本库副本捕获:没有冻结的副本行不会落后于目录,过户、删角之前全部参与者都先冻结了。
func (s *Store) AcquireTrialOccupancy(ctx context.Context, owner uint64, roster []*pb.TrialOccupancyRole, ttlMs uint64) (offender uint64, err error) {
	if owner == 0 || ttlMs == 0 || ttlMs > maxOccupancyTTLMs || len(roster) == 0 {
		return 0, ErrOccupancyInvalid
	}
	seen := make(map[uint64]bool, len(roster))
	expects := make([]rolefence.Expect, 0, len(roster))
	for _, role := range roster {
		if role.GetPlayerId() == 0 || seen[role.PlayerId] {
			return 0, ErrOccupancyInvalid
		}
		seen[role.PlayerId] = true
		expects = append(expects, rolefence.Expect{PlayerID: role.PlayerId, Account: role.Account, OwnerRevision: role.OwnerRevision, Reject: ErrOriginalPlayer})
	}
	err = s.Fenced(ctx, func() (e error) { offender, e = s.acquireTrialOccupancyOnce(ctx, owner, expects, ttlMs); return e })
	return offender, err
}

func (s *Store) acquireTrialOccupancyOnce(ctx context.Context, owner uint64, expects []rolefence.Expect, ttlMs uint64) (uint64, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	fences, err := rolefence.LockAll(ctx, tx, expects...)
	if err != nil {
		return 0, err
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	players := make([]uint64, 0, len(fences))
	for player := range fences {
		players = append(players, player)
	}
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	for _, player := range players {
		occupied, err := s.playerHasBattleEntryTx(ctx, tx, player)
		if err != nil {
			return 0, err
		}
		if occupied {
			return player, ErrMatchEntryOccupied
		}
		row, err := readOccupancyTx(ctx, tx, player)
		if err != nil {
			return 0, err
		}
		expires := stamp + ttlMs
		if row != nil && row.ExpiresMs > stamp {
			if !heldByTrial(row, owner) {
				return player, ErrMatchEntryOccupied
			}
			expires = max(expires, row.ExpiresMs)
		}
		fence := fences[player]
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_role_occupancy("+occupancyColumns+") VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE owner_kind=VALUES(owner_kind),owner_id=VALUES(owner_id),account=VALUES(account),owner_revision=VALUES(owner_revision),expires_ms=VALUES(expires_ms)", player, occupancyKindTrial, owner, fence.Account, fence.OwnerRevision, expires); err != nil {
			return 0, err
		}
	}
	return 0, tx.Commit()
}

// playerHasBattleEntryTx 是旧 requireTrialEntryFree 跨库读 match 的那两条,语句和判据原样:
// 未终结的整队开战、未释放的票。TTL、缺父行、未知状态都不是终局证据。观战由开局时捕获的退出原件处理,不在这里判。
func (s *Store) playerHasBattleEntryTx(ctx context.Context, tx *sql.Tx, player uint64) (bool, error) {
	for _, query := range []string{
		"SELECT r.player_id FROM team_launch_role r LEFT JOIN team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low WHERE r.player_id=? AND (l.state IS NULL OR l.state<>5 OR r.state IS NULL OR r.state<>5) LIMIT 1 FOR UPDATE",
		"SELECT player_id FROM match_authority_ticket WHERE player_id=? AND " + matchentry.TicketOccupiedSQL + " LIMIT 1 FOR UPDATE",
	} {
		var occupied uint64
		err := tx.QueryRowContext(ctx, query, player).Scan(&occupied)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// ReleaseTrialOccupancy 只删 owner 相同的行,幂等。别的大厅后来拿到的占用不受影响。
func (s *Store) ReleaseTrialOccupancy(ctx context.Context, owner uint64, players []uint64) error {
	if owner == 0 {
		return ErrOccupancyInvalid
	}
	ordered := append([]uint64(nil), players...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, player := range ordered {
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_role_occupancy WHERE player_id=? AND owner_kind=? AND owner_id=?", player, occupancyKindTrial, owner); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BattleCompletion 读一局的 SQL 终局,取代 guild 对 match_battle_completion 的跨库读。没有行回 nil。
func (s *Store) BattleCompletion(ctx context.Context, battle uint64) (*pb.MatchBattleCompletionRecord, error) {
	return readTerminal(ctx, s.DB, battle)
}
