package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"match/internal/config"
	"math"
	"sort"
	"strings"

	pb "proto/match"
)

// 与公开探测入口共用同一上限；一张票只投影已经验签的至多16个不同机房。
const MaxProbeRegions = 16

// 既有表缺普通索引时迁移器只给计划/警告；候选扫描不能带着全表排序接流。
func (s *Store) CheckCandidateIndexes(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT table_name,index_name,column_name FROM information_schema.statistics
		WHERE table_schema=DATABASE() AND table_name IN ('match_authority_ticket','match_ticket_region','match_challenge_invitation','match_direct_intent','match_join_intent','team_authority','team_invite_index','team_application_index','team_launch','team_launch_region','team_launch_role','match_spectator')
		ORDER BY table_name,index_name,seq_in_index`)
	if err != nil {
		return err
	}
	indexes := map[string][]string{}
	for rows.Next() {
		var table, index, column string
		if err = rows.Scan(&table, &index, &column); err != nil {
			rows.Close()
			return err
		}
		key := table + "/" + index
		indexes[key] = append(indexes[key], column)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for table, required := range map[string][]string{
		"match_authority_ticket":     {"state,enqueued_ms,player_id", "state,mode,team_id,enqueued_ms,player_id", "state,mode,config_id,team_size,team_id,enqueued_ms,player_id"},
		"match_ticket_region":        {"physical_region,mode,config_id,team_size,preferred_region,enqueued_ms,player_id", "physical_region,mode,config_id,team_size,enqueued_ms,player_id", "physical_region,mode,config_id,team_size,home_zone,preferred_region,enqueued_ms,player_id", "physical_region,mode,config_id,team_size,home_zone,enqueued_ms,player_id", "expires_ms,player_id,physical_region"},
		"match_challenge_invitation": {"target_player_id,state,expires_ms,challenge_id", "challenger_player_id,state,expires_ms,challenge_id"},
		"match_direct_intent":        {"exit_prepared,cancelled,battle_id,kind,owner_id,request_high,request_low"},
		"match_join_intent":          {"state,player_id,request_high,request_low"},
		"match_spectator":            {"state,player_id", "updated_ms,player_id"},
		"team_authority":             {"state,idle_expires_ms,team_id"},
		"team_invite_index":          {"invitee_id,expires_ms,team_id"},
		"team_application_index":     {"applicant_id,expires_ms,team_id"},
		"team_launch":                {"state,enqueued_ms,actor_id,request_high,request_low", "team_id,state,enqueued_ms,actor_id,request_high,request_low", "state,mode,config_id,target_size,enqueued_ms,actor_id,request_high,request_low"},
		"team_launch_region":         {"physical_region,mode,config_id,target_size,member_count,local_preferred,enqueued_ms,actor_id,request_high,request_low", "physical_region,mode,config_id,target_size,member_count,enqueued_ms,actor_id,request_high,request_low", "physical_region,mode,config_id,target_size,member_count,home_zone,local_preferred,enqueued_ms,actor_id,request_high,request_low", "physical_region,mode,config_id,target_size,member_count,home_zone,enqueued_ms,actor_id,request_high,request_low", "expires_ms,actor_id,request_high,request_low,physical_region"},
		"team_launch_role":           {"player_id,state,actor_id,request_high,request_low"},
	} {
		for _, columns := range required {
			found := false
			for key, actual := range indexes {
				if strings.HasPrefix(key, table+"/") && strings.Join(actual, ",") == columns {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%s缺候选索引(%s)，先执行迁移计划再接流", table, columns)
			}
		}
	}
	return nil
}

// PreferredRegion 由完整签名探测决定，不随某个副本的节点发现抖动或硬阈值变化。
// 最低RTT都超过硬阈值时不存在合格机房；因此保存全部测量可直接适应阈值调整。
func PreferredRegion(profile *pb.MatchNetworkProfile, now uint64) uint32 {
	var preferred, best uint32
	for region, rtt := range Latencies(profile, now, math.MaxUint32) {
		if preferred == 0 || rtt < best || (rtt == best && region < preferred) {
			preferred, best = region, rtt
		}
	}
	return preferred
}

func ticketRegionRows(ticket *pb.MatchAuthorityTicketRecord, now uint64) ([]*pb.MatchTicketRegionRecord, error) {
	if ticket.State != 1 || ticket.ExpiresMs <= now {
		return nil, nil
	}
	profile, err := DecodeProfile(ticket.NetworkProfile)
	if err != nil {
		return nil, err
	}
	if len(profile.Receipts) > MaxProbeRegions {
		return nil, errors.New("候选原探测超过已约定的机房上限")
	}
	if ticket.TeamId != 0 {
		return nil, nil
	} // 完整队伍只进入team_launch_region，单排查询不扫描整队成员。
	latencies := Latencies(profile, now, math.MaxUint32)
	preferred := PreferredRegion(profile, now)
	regions := make([]uint32, 0, len(latencies))
	for region := range latencies {
		regions = append(regions, region)
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	rows := make([]*pb.MatchTicketRegionRecord, 0, len(regions))
	for _, region := range regions {
		rows = append(rows, &pb.MatchTicketRegionRecord{PlayerId: ticket.PlayerId, PhysicalRegion: region, Ticket: ticket.Ticket, Mode: ticket.Mode, ConfigId: ticket.ConfigId, TeamSize: ticket.TeamSize, PreferredRegion: preferred, EnqueuedMs: ticket.EnqueuedMs, RatingCenti: ticket.RatingCenti, RttMs: latencies[region], ExpiresMs: ticket.ExpiresMs, HomeZone: ticket.ZoneId})
	}
	return rows, nil
}

func deleteTicketRegions(ctx context.Context, tx *sql.Tx, player uint64) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM match_ticket_region WHERE player_id=?", player)
	return err
}

// 调用者已经按P→ticket顺序持有原票行锁。投影与票原件同事务提交，永不反向授权。
func (s *Store) replaceTicketRegions(ctx context.Context, tx *sql.Tx, ticket *pb.MatchAuthorityTicketRecord, now uint64) error {
	rows, err := ticketRegionRows(ticket, now)
	if err != nil {
		return err
	}
	if err = deleteTicketRegions(ctx, tx, ticket.PlayerId); err != nil {
		return err
	}
	var preferred uint32
	if len(rows) > 0 {
		preferred = rows[0].PreferredRegion
	} else if ticket.TeamId != 0 && ticket.State == 1 && ticket.ExpiresMs > now {
		profile, decodeErr := DecodeProfile(ticket.NetworkProfile)
		if decodeErr != nil {
			return decodeErr
		}
		preferred = PreferredRegion(profile, now)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET preferred_region=? WHERE player_id=? AND ticket=?", preferred, ticket.PlayerId, ticket.Ticket); err != nil {
		return err
	}
	ticket.PreferredRegion = preferred
	for _, row := range rows {
		if _, err = tx.ExecContext(ctx, `INSERT INTO match_ticket_region
			(player_id,physical_region,ticket,mode,config_id,team_size,preferred_region,enqueued_ms,rating_centi,rtt_ms,expires_ms,home_zone)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.PlayerId, row.PhysicalRegion, row.Ticket, row.Mode, row.ConfigId, row.TeamSize, row.PreferredRegion, row.EnqueuedMs, row.RatingCenti, row.RttMs, row.ExpiresMs, row.HomeZone); err != nil {
			return err
		}
	}
	return nil
}

// CandidatePeers 在LIMIT之前完成机房、lane、原代次、等待、容差与时效过滤。
// 与锚点在全局扫描相距多少页无关；返回不足才能说明该次SQL观察中人数不足。
func (s *Store) CandidatePeers(ctx context.Context, anchor *pb.MatchAuthorityTicketRecord, region uint32, local bool, now uint64, toleranceCenti float64) ([]*pb.MatchAuthorityTicketRecord, error) {
	return s.candidatePeers(ctx, anchor, region, local, now, toleranceCenti, 0)
}

// CandidateTeamPeers 将最终整队的home资格放在LIMIT之前；网络机房与home是独立维度。
func (s *Store) CandidateTeamPeers(ctx context.Context, anchor *pb.MatchAuthorityTicketRecord, region uint32, local bool, now uint64) ([]*pb.MatchAuthorityTicketRecord, error) {
	if anchor == nil || !validTeamQueueMode(anchor.Mode) || anchor.TeamSize > config.TeamCapacity {
		return nil, ErrDirectConflict
	}
	var home uint32
	if !s.TeamConfig.AllowCrossZone {
		if anchor.ZoneId == 0 {
			return nil, ErrTeamRoute
		}
		home = anchor.ZoneId
	}
	return s.candidatePeers(ctx, anchor, region, local, now, math.Inf(1), home)
}

func (s *Store) candidatePeers(ctx context.Context, anchor *pb.MatchAuthorityTicketRecord, region uint32, local bool, now uint64, toleranceCenti float64, home uint32) ([]*pb.MatchAuthorityTicketRecord, error) {
	if anchor == nil || anchor.TeamSize == 0 || anchor.TeamSize > 10 || region == 0 {
		return nil, errors.New("候选组人数或目标机房无效")
	}
	if anchor.TeamSize == 1 && anchor.Mode != uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		return nil, nil
	}
	q := `SELECT t.` + strings.ReplaceAll(ticketColumns, ",", ",t.") + ` FROM match_ticket_region r
		JOIN match_authority_ticket t ON t.player_id=r.player_id AND t.ticket=r.ticket AND t.state=1
		WHERE r.physical_region=? AND r.mode=? AND r.config_id=? AND r.team_size=? AND r.player_id<>?
		AND r.rtt_ms>0 AND r.rtt_ms<=? AND r.expires_ms>? AND t.expires_ms=r.expires_ms
		AND t.not_before_ms<=? AND t.mode=r.mode AND t.config_id=r.config_id AND t.team_size=r.team_size
		AND t.team_id=0
		AND t.preferred_region=r.preferred_region AND t.enqueued_ms=r.enqueued_ms AND t.rating_centi=r.rating_centi`
	args := []any{region, anchor.Mode, anchor.ConfigId, anchor.TeamSize, anchor.PlayerId, s.Config.HardLatencyMs, now, now}
	if home != 0 {
		q += " AND r.home_zone=? AND t.zone_id=r.home_zone"
		args = append(args, home)
	}
	if local {
		q += " AND r.preferred_region=?"
		args = append(args, region)
	} else {
		wait := uint64(s.Config.CrossRegionWaitSeconds) * 1000
		if now < wait || anchor.EnqueuedMs > now-wait {
			return nil, nil
		}
		q += " AND r.enqueued_ms<=?"
		args = append(args, now-wait)
	}
	if math.IsNaN(toleranceCenti) || toleranceCenti < 0 {
		return nil, errors.New("评分容差无效")
	}
	if !math.IsInf(toleranceCenti, 1) && toleranceCenti < float64(math.MaxInt64) {
		spread := int64(toleranceCenti)
		low, high := int64(math.MinInt64), int64(math.MaxInt64)
		if anchor.RatingCenti >= math.MinInt64+spread {
			low = anchor.RatingCenti - spread
		}
		if anchor.RatingCenti <= math.MaxInt64-spread {
			high = anchor.RatingCenti + spread
		}
		q += " AND r.rating_centi BETWEEN ? AND ?"
		args = append(args, low, high)
	}
	q += " ORDER BY r.enqueued_ms,r.player_id LIMIT ?"
	limit := anchor.TeamSize - 1
	if anchor.Mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		limit = anchor.TeamSize*2 - 1
	}
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		candidate, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// 每轮至多修复32张原票、删除64个过期投影。游标只用于调度，重启从头修复仍幂等。
// 不凭当前目录补造历史owner；缺原件的旧票由重新探测/入队恢复。
func (s *Store) RepairCandidateRegions(ctx context.Context, afterMs, afterPlayer uint64) (uint64, uint64, error) {
	return s.RepairCandidateRegionsThrough(ctx, afterMs, afterPlayer, ^uint64(0), ^uint64(0))
}

// CandidateRegionRepairHighWater 固定一轮修复的尾界。新Join已同tx写投影，
// 不能让持续入队把旧票首轮修复无限延长并永久禁用跨区。
func (s *Store) CandidateRegionRepairHighWater(ctx context.Context) (uint64, uint64, error) {
	var stamp, player uint64
	err := s.DB.QueryRowContext(ctx, "SELECT enqueued_ms,player_id FROM match_authority_ticket WHERE state=1 ORDER BY enqueued_ms DESC,player_id DESC LIMIT 1").Scan(&stamp, &player)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return stamp, player, err
}

func (s *Store) RepairCandidateRegionsThrough(ctx context.Context, afterMs, afterPlayer, throughMs, throughPlayer uint64) (uint64, uint64, error) {
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM match_ticket_region WHERE expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) LIMIT 64"); err != nil {
		return afterMs, afterPlayer, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=1 AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) AND (enqueued_ms<? OR (enqueued_ms=? AND player_id<=?)) ORDER BY enqueued_ms,player_id LIMIT 32", afterMs, afterMs, afterPlayer, throughMs, throughMs, throughPlayer)
	if err != nil {
		return afterMs, afterPlayer, err
	}
	var tickets []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			rows.Close()
			return afterMs, afterPlayer, err
		}
		tickets = append(tickets, ticket)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return afterMs, afterPlayer, err
	}
	if len(tickets) == 0 {
		return 0, 0, nil
	}
	for _, ticket := range tickets {
		if err = s.repairCandidateRegions(ctx, ticket); err != nil {
			return afterMs, afterPlayer, err
		}
		afterMs, afterPlayer = ticket.EnqueuedMs, ticket.PlayerId
	}
	return afterMs, afterPlayer, nil
}

func (s *Store) repairCandidateRegions(ctx context.Context, observed *pb.MatchAuthorityTicketRecord) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{observed}); err != nil {
		// 目录已换主/冻结是旧票失效，不能阻塞后续有效票的有界维护。
		// 原票仍由提交点拒绝；不把维护错误伪装成可成局候选。
		if errors.Is(err, ErrOriginalPlayer) {
			return nil
		}
		return err
	}
	current, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", observed.PlayerId))
	if err != nil {
		return err
	}
	if current.Ticket != observed.Ticket || current.State != 1 {
		return nil
	}
	n, err := now(ctx, tx)
	if err != nil {
		return err
	}
	wanted, err := ticketRegionRows(current, n)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT physical_region,ticket,mode,config_id,team_size,preferred_region,enqueued_ms,rating_centi,rtt_ms,expires_ms,home_zone FROM match_ticket_region WHERE player_id=? ORDER BY physical_region", current.PlayerId)
	if err != nil {
		return err
	}
	matched := 0
	expectedPreferred := uint32(0)
	if len(wanted) > 0 {
		expectedPreferred = wanted[0].PreferredRegion
	} else if current.TeamId != 0 && current.ExpiresMs > n {
		profile, decodeErr := DecodeProfile(current.NetworkProfile)
		if decodeErr != nil {
			return decodeErr
		}
		expectedPreferred = PreferredRegion(profile, n)
	}
	equal := current.PreferredRegion == expectedPreferred
	for rows.Next() {
		actual := &pb.MatchTicketRegionRecord{PlayerId: current.PlayerId}
		if err = rows.Scan(&actual.PhysicalRegion, &actual.Ticket, &actual.Mode, &actual.ConfigId, &actual.TeamSize, &actual.PreferredRegion, &actual.EnqueuedMs, &actual.RatingCenti, &actual.RttMs, &actual.ExpiresMs, &actual.HomeZone); err != nil {
			rows.Close()
			return err
		}
		if matched >= len(wanted) || !sameTicketRegion(actual, wanted[matched]) {
			equal = false
		}
		matched++
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if equal && matched == len(wanted) {
		return nil
	}
	if err = s.replaceTicketRegions(ctx, tx, current, n); err != nil {
		return err
	}
	return tx.Commit()
}

func sameTicketRegion(a, b *pb.MatchTicketRegionRecord) bool {
	return a.PlayerId == b.PlayerId && a.PhysicalRegion == b.PhysicalRegion && a.Ticket == b.Ticket && a.Mode == b.Mode && a.ConfigId == b.ConfigId && a.TeamSize == b.TeamSize && a.PreferredRegion == b.PreferredRegion && a.EnqueuedMs == b.EnqueuedMs && a.RatingCenti == b.RatingCenti && a.RttMs == b.RttMs && a.ExpiresMs == b.ExpiresMs && a.HomeZone == b.HomeZone
}
