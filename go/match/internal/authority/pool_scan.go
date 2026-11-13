package authority

import (
	"context"
	"strings"

	"match/internal/catalog"

	pb "proto/match"
	teampb "proto/team"
)

// 按池读候选（docs/design/global-match-hierarchical-pools.md §5.3）。只读、不加锁：
// 读到的是撮合的输入，成局仍由 claimTx / ClaimTeamUnits 在锁内复核原票。
//
// 热池拆分（同一文档 §5.2.1）之后一个池按撮合段分给多个副本，每段只读自己那一段（PoolSlice）：
// 机房段与子池段按首选机房列 pool_region 读，子池段再按 id 取模、按入队时刻截取；跨机房段按入队时刻截取、不分机房。

// PoolCursor 是池内的排队序位置（原始入队时刻, player_id），只用于翻页调度，不参与任何提交决策。
type PoolCursor struct{ EnqueuedMs, PlayerId uint64 }

// PoolSlice 是一个撮合段要读的那一段排队单位。只决定读哪些行，不参与任何提交决策。
// 各条件按非零与否叠加，零值 = 整个池（WholePool）。
type PoolSlice struct {
	Pool catalog.Pool
	// Region 非 0：只读首选机房列 pool_region 等于它的单位（机房段、子池段）；0 = 不按机房过滤（整池段、跨机房段）。
	// 不读 pool_region=0 的单位（整队成员首选机房不一致），那些只有不分机房的段读。
	Region uint32
	// SubPools > 1：只读子池号为 Sub 的单位，子池号 = 单排的 player_id（整队取发起者 actor_id）对 SubPools 取模。
	// 子池号不落列：它是主键的函数，在索引项上过滤（§5.2.1 第 5 条）。
	SubPools, Sub uint32
	// AfterMs 非 0：只读 enqueued_ms > AfterMs（子池段）；UntilMs 非 0：只读 enqueued_ms ≤ UntilMs（机房段配了子池时、跨机房段）。
	AfterMs, UntilMs uint64
}

// WholePool 是不分机房、不分子池、不限入队时刻的整个池。
func WholePool(pool catalog.Pool) PoolSlice { return PoolSlice{Pool: pool} }

// filter 把切片条件拼成接在 WHERE 后面的 SQL 片段与绑定参数。列名是写死的常量，值只走绑定参数。
// id 是子池号取模用的列：单排票是 player_id，整队是 actor_id（都是索引列，取模在索引项上过滤，回表只取命中的行）。
func (p PoolSlice) filter(id string) (string, []any) {
	var where strings.Builder
	var args []any
	if p.Region != 0 {
		where.WriteString(" AND pool_region=?")
		args = append(args, p.Region)
	}
	if p.SubPools > 1 {
		where.WriteString(" AND MOD(" + id + ",?)=?")
		args = append(args, p.SubPools, p.Sub)
	}
	if p.AfterMs != 0 {
		where.WriteString(" AND enqueued_ms>?")
		args = append(args, p.AfterMs)
	}
	if p.UntilMs != 0 {
		where.WriteString(" AND enqueued_ms<=?")
		args = append(args, p.UntilMs)
	}
	return where.String(), args
}

// PoolTickets 按排队序读整个池的一页单排票，等于 SliceTickets(WholePool(pool), …)。
func (s *Store) PoolTickets(ctx context.Context, pool catalog.Pool, after PoolCursor, limit int) ([]*pb.MatchAuthorityTicketRecord, error) {
	return s.SliceTickets(ctx, WholePool(pool), after, limit)
}

// SliceTickets 按排队序读一个撮合段的一页单排票（team_id=0，整队成员由 SliceTeamLaunches 带出）。
// 不过滤过期票：过期票集中在队首，由调用方收集后交给 ExpireTickets 终止。
// 按机房读走索引 (state,mode,config_id,team_size,team_id,pool_region,enqueued_ms,player_id)，
// 不分机房走 (state,mode,config_id,team_size,team_id,enqueued_ms,player_id)；两条都按 (enqueued_ms, player_id) 顺着索引读。
func (s *Store) SliceTickets(ctx context.Context, slice PoolSlice, after PoolCursor, limit int) ([]*pb.MatchAuthorityTicketRecord, error) {
	pool := slice.Pool
	extra, extraArgs := slice.filter("player_id")
	args := append([]any{uint32(pool.Mode), pool.ConfigId, pool.TeamSize}, extraArgs...)
	args = append(args, after.EnqueuedMs, after.EnqueuedMs, after.PlayerId, limit)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=1 AND mode=? AND config_id=? AND team_size=? AND team_id=0"+extra+" AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) ORDER BY enqueued_ms,player_id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ticket)
	}
	return out, rows.Err()
}

// PoolTeamLaunches 按排队序读整个池排队中的整队，等于 SliceTeamLaunches(WholePool(pool), …)。
func (s *Store) PoolTeamLaunches(ctx context.Context, pool catalog.Pool, limit int) ([]*teampb.TeamLaunchRecord, error) {
	return s.SliceTeamLaunches(ctx, WholePool(pool), limit)
}

// SliceTeamLaunches 按排队序读一个撮合段排队中（state=2）的整队，从队首读至多 limit 支。只有 PVE_TEAM / PVP_TEAM 池有整队。
// 按机房读走索引 (state,mode,config_id,target_size,pool_region,enqueued_ms,actor_id,request_high,request_low)，
// 不分机房走 (state,mode,config_id,target_size,enqueued_ms,actor_id,request_high,request_low)；mode 是等值条件，
// 两条都顺着索引按入队序读、读够 limit 就停。成员票与原件同一个入队时刻（EnqueueTeamLaunchTx），所以按原件的 enqueued_ms
// 截取就是按单位的等待截取。
// 只认显式写入的模式：开战原件由 normalizeTeamMatchMode 写定（PVE_TEAM 恒为 5）。旧原件 mode=0 按 PVE_TEAM 解释的读法
// （TeamLaunchMode）不进读页：写成 mode IN(5,0) 会让读页变成两段范围、TiDB 只能读完再排序（§5.2.1 第 6 条），
// 项目未上线、清库重建，库里没有 mode=0 的原件。
func (s *Store) SliceTeamLaunches(ctx context.Context, slice PoolSlice, limit int) ([]*teampb.TeamLaunchRecord, error) {
	pool := slice.Pool
	if pool.Mode != pb.MatchMode_MATCH_MODE_PVE_TEAM && pool.Mode != pb.MatchMode_MATCH_MODE_PVP_TEAM {
		return nil, nil
	}
	extra, extraArgs := slice.filter("actor_id")
	args := append([]any{uint32(pool.Mode), pool.ConfigId, pool.TeamSize}, extraArgs...)
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE state=2 AND mode=? AND config_id=? AND target_size=?"+extra+" ORDER BY enqueued_ms,actor_id,request_high,request_low LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*teampb.TeamLaunchRecord
	for rows.Next() {
		launch, err := scanTeamLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, launch)
	}
	return out, rows.Err()
}

// preferredRegionOf 是单排票入队时写进 pool_region 的值：按刚落库的探测原件现算，与撮合、提交点同一个 PreferredRegion。
func preferredRegionOf(profile []byte, now uint64) (uint32, error) {
	p, err := DecodeProfile(profile)
	if err != nil {
		return 0, err
	}
	return PreferredRegion(p, now), nil
}

// unitPoolRegion 是整队原件 pool_region 的值：全体成员的首选机房一致时取它，不一致（或没有成员）为 0。
// 与撮合输入（logic.poolUnitInput）里 Unit.Preferred 同一条规则：首选机房不一致的单位只能参加跨机房阶段，
// 只有不分机房的段（跨机房段）会读到它。
func unitPoolRegion(members []uint32) uint32 {
	if len(members) == 0 {
		return 0
	}
	for _, region := range members[1:] {
		if region != members[0] {
			return 0
		}
	}
	return members[0]
}

// ExpireTickets 终止已过期的单排票（state 1 → 4），按票号 CAS，只命中仍过期、仍在排队的那一张。
// 与 Ticket() 里本人读票时的惰性过期同一语义，不需要玩家锁：过期票不会再被续命（续命要先持锁并
// 校验原票），也不会被提交（claimTx 在锁内复核 expires_ms）。返回实际终止的张数。
// 两个撮合段读到同一张过期票时只有一边命中，另一边是 0 行。
func (s *Store) ExpireTickets(ctx context.Context, tickets []*pb.MatchAuthorityTicketRecord) (int, error) {
	expired := 0
	for _, ticket := range tickets {
		r, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND state=1 AND team_id=0 AND expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)", ticket.PlayerId, ticket.Ticket)
		if err != nil {
			return expired, err
		}
		if n, _ := r.RowsAffected(); n == 1 {
			expired++
		}
	}
	return expired, nil
}
