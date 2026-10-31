package logic

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/poolgroup"
	"match/internal/poolowner"
	"match/internal/scope"
	"match/internal/svc"

	pb "proto/match"
	teampb "proto/team"
	"shared/safego"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 按池撮合（docs/design/global-match-hierarchical-pools.md §5）。
//
//   - 池 = (模式, 怪物战斗模板, 人数档)，来自模板目录（PlayMode 表）。同一模板的各人数档由同一个副本负责，
//     副本之间按 match 实例注册表做最高随机权重哈希分工（poolowner），不再抢同一批票。
//   - 每轮对负责的每个池读一页排队单位（单排票 + 整队），交给纯函数 poolgroup.Form 一次切出多组，
//     再并发提交；提交点（authority.claimTx / ClaimTeamUnits）在锁内用原票和数据库时钟复核全部规则。
//   - 一轮里所有提交都在本轮结束前完成，下一轮读到的已是提交后的状态，同一张票不会被本副本选两次。

const (
	poolRoundBudget      = 3 * time.Second // 一轮（读页、成组、提交）的总预算
	poolTeamLaunchLimit  = 64              // 每轮每池读的排队整队上限
	poolExpireLimit      = 64              // 每轮每池终止的过期票上限
	poolClaimParallelism = 8               // 一轮里同时进行的成局提交事务数，给入队请求留出数据库连接
	immediateClaimBudget = 3 * time.Second
)

// StartGlobalPoolMatcher 启动按池撮合循环。
func StartGlobalPoolMatcher(ctx context.Context, s *svc.ServiceContext) {
	interval := time.Duration(s.Config.MatcherIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	m := &poolMatcher{s: s, cursors: map[catalog.Pool]authority.PoolCursor{}}
	s.StartSourceLoop(ctx, "match.global.pool", interval, m.round)
}

type poolMatcher struct {
	s *svc.ServiceContext
	// cursors 是各池的翻页位置，只由撮合循环这一个 goroutine 访问；只用于调度，不参与提交决策。
	cursors map[catalog.Pool]authority.PoolCursor
}

// poolUnit 是一个不可拆的撮合单位的原件：单排票（launch 为 nil）或一支排队中的整队。
type poolUnit struct {
	tickets []*pb.MatchAuthorityTicketRecord
	launch  *teampb.TeamLaunchRecord
}

// poolClaim 是一个待提交的候选组，units 与 group.Units 一一对应。
type poolClaim struct {
	pool  catalog.Pool
	group poolgroup.Group
	units []poolUnit
	now   uint64
}

func (m *poolMatcher) round(parent context.Context) {
	start := time.Now()
	defer func() { metrics.ObservePoolRound(time.Since(start)) }()
	ctx, cancel := context.WithTimeout(parent, poolRoundBudget)
	defer cancel()
	s := m.s
	members, self := s.PoolMembers(), s.PoolMember()
	var owned []catalog.Pool
	for _, pool := range s.Catalog.Pools() {
		if poolowner.Owns(self, pool.OwnerKey(), members) {
			owned = append(owned, pool)
		}
	}
	metrics.SetPoolOwned(len(owned))
	if len(owned) == 0 {
		return
	}
	now, err := s.Authority.Now(ctx)
	if err != nil {
		logx.Errorf("[match.pool] Failed to read database time: %v", err)
		return
	}
	nodes := s.BattleNodes.Snapshot()
	regions := s.Placement.Regions(nodes)
	var claims []poolClaim
	for _, pool := range owned {
		if ctx.Err() != nil {
			break
		}
		claims = append(claims, m.formPool(ctx, pool, now, regions)...)
	}
	m.claimAll(ctx, claims, nodes)
}

// formPool 读一页排队单位并切出候选组；同时终止页里已过期的单排票。
func (m *poolMatcher) formPool(ctx context.Context, pool catalog.Pool, now uint64, regions map[uint32]bool) []poolClaim {
	s := m.s
	pageSize := s.Config.GlobalMatch.PoolPage()
	tickets, err := s.Authority.PoolTickets(ctx, pool, m.cursors[pool], pageSize)
	if err != nil {
		logx.Errorf("[match.pool] Failed to read pool tickets mode=%s config=%d size=%d: %v", pool.Mode, pool.ConfigId, pool.TeamSize, err)
		return nil
	}
	var launches []*teampb.TeamLaunchRecord
	if pool.Mode == pb.MatchMode_MATCH_MODE_PVE_TEAM || pool.Mode == pb.MatchMode_MATCH_MODE_PVP_TEAM {
		if launches, err = s.Authority.PoolTeamLaunches(ctx, pool, poolTeamLaunchLimit); err != nil {
			logx.Errorf("[match.pool] Failed to read pool team launches mode=%s config=%d size=%d: %v", pool.Mode, pool.ConfigId, pool.TeamSize, err)
			return nil
		}
	}
	var units []poolUnit
	var inputs []poolgroup.Unit
	var expired []*pb.MatchAuthorityTicketRecord
	for _, ticket := range tickets {
		if ticket.ExpiresMs <= now {
			if len(expired) < poolExpireLimit {
				expired = append(expired, ticket)
			}
			continue
		}
		if !ticketReady(ticket, now) {
			continue
		}
		original := poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{ticket}}
		if input, ok := poolUnitInput(s, original, now); ok {
			units, inputs = append(units, original), append(inputs, input)
		}
	}
	for _, launch := range launches {
		members, err := s.Authority.TeamLaunchTickets(ctx, launch)
		if err != nil {
			continue // 整队原件刚变化（取消、成局、换名单），下一轮重读
		}
		original := poolUnit{tickets: members, launch: launch}
		if !unitReady(original, now) {
			continue
		}
		if input, ok := poolUnitInput(s, original, now); ok {
			units, inputs = append(units, original), append(inputs, input)
		}
	}
	m.expire(ctx, pool, expired)
	metrics.SetPoolPageUnits(pool.Mode.String(), strconv.FormatUint(uint64(pool.TeamSize), 10), len(units))
	groups := poolgroup.Form(inputs, regions, now, poolPolicy(s, pool))
	m.advanceCursor(pool, tickets, pageSize, len(groups))
	claims := make([]poolClaim, 0, len(groups))
	for _, group := range groups {
		claim := poolClaim{pool: pool, group: group, now: now}
		for _, index := range group.Units {
			claim.units = append(claim.units, units[index])
		}
		claims = append(claims, claim)
	}
	return claims
}

// advanceCursor：读到池尾或本页成了组，下一轮从队首重读；整页都凑不出组时翻到下一页，
// 防止一页卡住的票（例如机房暂时没有节点、整队装不进人数档）挡住后面能成局的人。
func (m *poolMatcher) advanceCursor(pool catalog.Pool, tickets []*pb.MatchAuthorityTicketRecord, pageSize, formed int) {
	if len(tickets) < pageSize || formed > 0 {
		delete(m.cursors, pool)
		return
	}
	last := tickets[len(tickets)-1]
	m.cursors[pool] = authority.PoolCursor{EnqueuedMs: last.EnqueuedMs, PlayerId: last.PlayerId}
}

func (m *poolMatcher) expire(ctx context.Context, pool catalog.Pool, tickets []*pb.MatchAuthorityTicketRecord) {
	if len(tickets) == 0 {
		return
	}
	count, err := m.s.Authority.ExpireTickets(ctx, tickets)
	metrics.ObservePoolExpired(pool.Mode.String(), count)
	if err != nil {
		logx.Errorf("[match.pool] Failed to expire tickets mode=%s config=%d: %v", pool.Mode, pool.ConfigId, err)
	}
}

// claimAll 并发提交本轮全部候选组：先占备战槽再提交，占不到槽的组留到下一轮。
// 等全部提交返回后本轮才结束，下一轮读到的已是提交后的票据状态。
func (m *poolMatcher) claimAll(ctx context.Context, claims []poolClaim, nodes []discovery.NodeEntry) {
	s := m.s
	var wg sync.WaitGroup
	limiter := make(chan struct{}, poolClaimParallelism)
	for index, claim := range claims {
		if ctx.Err() != nil {
			break
		}
		if !reserveGatherSlot(s) {
			for _, skipped := range claims[index:] {
				metrics.ObservePoolClaim(skipped.pool.Mode.String(), skipped.group.Level.String(), passLabel(skipped.group.Cross), metrics.PoolClaimNoSlot)
			}
			break
		}
		limiter <- struct{}{}
		wg.Add(1)
		safego.Go("match.global.pool.claim", func() {
			defer wg.Done()
			defer func() { <-limiter }()
			outcome := claimGroup(ctx, s, claim, nodes)
			metrics.ObservePoolClaim(claim.pool.Mode.String(), claim.group.Level.String(), passLabel(claim.group.Cross), outcome)
		})
	}
	wg.Wait()
}

// claimGroup 提交一个候选组。调用方已占好一个备战槽：提交成功则槽随成局推进，失败在这里释放。
func claimGroup(ctx context.Context, s *svc.ServiceContext, claim poolClaim, nodes []discovery.NodeEntry) string {
	launched := false
	defer func() {
		if !launched {
			<-s.GlobalGatherSlots
		}
	}()
	// 整队先复核（成员变化、下线）：失效的整队由队伍域撤单，本组放弃，其余单位下一轮重组。
	for _, unit := range claim.units {
		if unit.launch == nil {
			continue
		}
		if ok, err := checkGlobalTeamUnit(ctx, s, unit); err != nil || !ok {
			return metrics.PoolClaimPartyGone
		}
	}
	node, ok := s.Placement.Pick(nodes, claim.group.Region)
	if !ok {
		return metrics.PoolClaimNoNode
	}
	id, err := s.BattleIDGen.Generate()
	if err != nil {
		return metrics.PoolClaimIDError
	}
	f := &pb.MatchFormationRecord{BattleId: id, OperationId: uuid.NewString(), PhysicalRegion: claim.group.Region,
		TargetNodeId: node.NodeId, TargetInstanceId: node.NodeUuid, TargetEndpoint: node.Endpoint,
		Mode: uint32(claim.pool.Mode), ConfigId: claim.pool.ConfigId, RequeueOnFailure: true}
	var tickets []*pb.MatchAuthorityTicketRecord
	var launches []*teampb.TeamLaunchRecord
	for i, unit := range claim.units {
		tickets = append(tickets, unit.tickets...)
		if unit.launch != nil {
			launches = append(launches, unit.launch)
		}
		if claim.group.Sides != nil {
			for _, ticket := range unit.tickets {
				f.Members = append(f.Members, &pb.MatchFormationMember{PlayerId: ticket.PlayerId, Ticket: ticket.Ticket, TeamIndex: claim.group.Sides[i]})
			}
		}
	}
	switch claim.pool.Mode {
	case pb.MatchMode_MATCH_MODE_PVE_TEAM, pb.MatchMode_MATCH_MODE_PVP_TEAM:
		// 整队与组队池的单排一起提交：先锁 route 再逐人核对 home，整队整体占用。
		err = s.Authority.ClaimTeamUnits(ctx, f, tickets, launches)
	default:
		err = s.Authority.Claim(ctx, f, tickets)
	}
	if err != nil {
		if !expectedClaimRejection(err) {
			s.HoldRelay(err)
		}
		metrics.ObserveGlobalFlow("claim", "conflict")
		return metrics.PoolClaimConflict
	}
	metrics.ObserveGlobalFlow("claim", "committed")
	metrics.ObservePoolWait(claim.pool.Mode.String(), claim.group.Level.String(), float64(longestWaitMs(tickets, claim.now))/1000)
	launched = true
	launchReservedFormation(s, f)
	return metrics.PoolClaimCommitted
}

// claimImmediately 让单人档（PVE_SOLO、PVE_TEAM 1 人档）入队后马上成局，与 Java tryImmediate 同形态：
// 独立事务、独立超时，不阻塞入队回包；失败只计数，票留在排队态由负责该池的写者下一轮兜底。
func claimImmediately(s *svc.ServiceContext, ticket *pb.MatchAuthorityTicketRecord) {
	mode := pb.MatchMode(ticket.Mode).String()
	s.LaunchRelayTask("match.global.immediate", func() {
		ctx, cancel := context.WithTimeout(context.Background(), immediateClaimBudget)
		defer cancel()
		now, err := s.Authority.Now(ctx)
		if err != nil {
			return
		}
		original := poolUnit{tickets: []*pb.MatchAuthorityTicketRecord{ticket}}
		input, ok := poolUnitInput(s, original, now)
		if !ok {
			return
		}
		pool := catalog.Pool{Mode: pb.MatchMode(ticket.Mode), ConfigId: ticket.ConfigId, TeamSize: 1}
		nodes := s.BattleNodes.Snapshot()
		groups := poolgroup.Form([]poolgroup.Unit{input}, s.Placement.Regions(nodes), now, poolPolicy(s, pool))
		if len(groups) != 1 {
			metrics.ObserveImmediateClaim(mode, metrics.PoolClaimNoNode)
			return
		}
		if !reserveGatherSlot(s) {
			metrics.ObserveImmediateClaim(mode, metrics.PoolClaimNoSlot)
			return
		}
		outcome := claimGroup(ctx, s, poolClaim{pool: pool, group: groups[0], units: []poolUnit{original}, now: now}, nodes)
		metrics.ObserveImmediateClaim(mode, outcome)
	})
}

// poolPolicy 是一个池的成组规则；提交点用同一组配置复核。
func poolPolicy(s *svc.ServiceContext, pool catalog.Pool) poolgroup.Policy {
	rated := isRatedMode(pool.Mode)
	return poolgroup.Policy{
		Target:             pool.Players(),
		Scope:              s.Authority.Schedule(),
		CrossRegionAfterMs: uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds) * 1000,
		Rated:              rated,
		Sides:              pool.Mode == pb.MatchMode_MATCH_MODE_PVP_TEAM,
		ToleranceCenti: func(anchorWaitMs uint64) float64 {
			return anchorToleranceFor(s, rated, int64(anchorWaitMs/1000)) * 100
		},
	}
}

// poolUnitInput 把一个单位的原票换算成撮合输入：范围树位置取票上的 home zone 解析到的最终区（合服后两个原始区算同一个 zone），
// 可放置机房是全体成员都在硬延迟内的机房（取成员中的最大 RTT），首选机房取全员一致的那个。
func poolUnitInput(s *svc.ServiceContext, unit poolUnit, now uint64) (poolgroup.Unit, bool) {
	input := poolgroup.Unit{ID: unit.tickets[0].PlayerId, EnqueuedMs: unit.tickets[0].EnqueuedMs}
	if unit.launch != nil {
		input.ID, input.EnqueuedMs = unit.launch.ActorId, unit.launch.EnqueuedMs
	}
	zones := s.Authority.MergedZones()
	hard := s.Config.GlobalMatch.HardLatencyMs
	for index, ticket := range unit.tickets {
		profile, err := authority.DecodeProfile(ticket.NetworkProfile)
		if err != nil || len(profile.GetReceipts()) > authority.MaxProbeRegions {
			return poolgroup.Unit{}, false
		}
		latencies := authority.Latencies(profile, now, hard)
		preferred := authority.PreferredRegion(profile, now)
		if index == 0 {
			input.RTT, input.Preferred = latencies, preferred
		} else {
			for region, worst := range input.RTT {
				rtt, ok := latencies[region]
				if !ok {
					delete(input.RTT, region)
					continue
				}
				input.RTT[region] = max(worst, rtt)
			}
			if preferred != input.Preferred {
				input.Preferred = 0
			}
		}
		input.Players = append(input.Players, ticket.PlayerId)
		input.Members = append(input.Members, scope.Member{Node: s.Catalog.ZoneNode(zones.Final(ticket.ZoneId)), EnqueuedMs: ticket.EnqueuedMs})
	}
	if len(unit.tickets) == 1 {
		input.RatingCenti = unit.tickets[0].RatingCenti
	}
	return input, len(input.RTT) > 0
}

// ticketReady 报告单排票此刻能否参加撮合（仍在排队、已过失败退避）。过期由调用方单独处理。
func ticketReady(ticket *pb.MatchAuthorityTicketRecord, now uint64) bool {
	return ticket.State == 1 && ticket.NotBeforeMs <= now && ticket.ExpiresMs > now && ticket.TeamId == 0
}

// unitReady 报告整队此刻能否参加撮合：每名成员的票都在排队、未过期、已过退避。
// 整队成员票过期由队伍维护整单撤队，这里只跳过。
func unitReady(unit poolUnit, now uint64) bool {
	if len(unit.tickets) == 0 || unit.launch == nil || uint32(len(unit.tickets)) != unit.launch.MemberCount {
		return false
	}
	for _, ticket := range unit.tickets {
		if ticket.State != 1 || ticket.NotBeforeMs > now || ticket.ExpiresMs <= now {
			return false
		}
	}
	return true
}

// checkGlobalTeamUnit 在提交前复核整队仍然有效（成员集合未变、全员在线）。失效时撤掉这支整队的排队，
// 并通知队伍域；返回 false 表示本组不能提交。err 非 nil 表示读不到，不做失效判定。
func checkGlobalTeamUnit(ctx context.Context, s *svc.ServiceContext, unit poolUnit) (bool, error) {
	port := s.PartyTeam()
	if port == nil {
		return false, errors.New("team: 整队复核端口未装配")
	}
	members := make([]uint64, 0, len(unit.tickets))
	for _, ticket := range unit.tickets {
		members = append(members, ticket.PlayerId)
	}
	reason, culprit, err := port.CheckParty(ctx, unit.launch.TeamId, members)
	if err != nil {
		return false, err
	}
	if reason == constants.PartyEndNone {
		return true, nil
	}
	closed, err := s.Authority.WithdrawObservedTeamQueue(ctx, unit.launch)
	if err != nil {
		s.HoldRelay(err)
		return false, err
	}
	if closed {
		port.PartyEnded(unit.launch.TeamId, members, reason, culprit)
	}
	return false, nil
}

// reserveGatherSlot 非阻塞地占一个备战槽；占到后由提交失败路径或成局结束释放。
func reserveGatherSlot(s *svc.ServiceContext) bool {
	select {
	case s.GlobalGatherSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// expectedClaimRejection 报告提交失败是不是撮合与提交之间的正常变化（不影响排空判定）：
// 票被取消或已被别人提交、整队名单变化、范围或入口规则在提交点不满足、home 迁移。
func expectedClaimRejection(err error) bool {
	for _, expected := range []error{authority.ErrScopeWait, authority.ErrTemplateClosed, authority.ErrDirectConflict, authority.ErrTeamLaunchActive, authority.ErrOriginalPlayer, authority.ErrTeamRoute} {
		if errors.Is(err, expected) {
			return true
		}
	}
	return false
}

func passLabel(cross bool) string {
	if cross {
		return "cross"
	}
	return "local"
}

// longestWaitMs 返回组内最长等待（从原始入队时刻起算）。
func longestWaitMs(tickets []*pb.MatchAuthorityTicketRecord, now uint64) uint64 {
	var longest uint64
	for _, ticket := range tickets {
		longest = max(longest, scope.WaitMs(ticket.EnqueuedMs, now))
	}
	return longest
}
