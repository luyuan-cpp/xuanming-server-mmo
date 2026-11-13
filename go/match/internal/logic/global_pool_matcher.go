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
	"match/internal/ratingrule"
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
//   - 池 = (模式, 怪物战斗模板, 人数档)，来自模板目录（PlayMode 表）。每个池切成撮合段（§5.2.1）：每组 1 人的池一个整池段；
//     其余的池每个机房一个机房段（热门模板再按玩家拆子池段）、外加一个跨机房段。副本之间按 match 实例注册表对段的归属键
//     做最高随机权重哈希分工（poolowner），一个热池不再只压在一个副本上。
//   - 每轮对负责的每个段读一页排队单位（单排票 + 整队），交给纯函数 poolgroup.Form 一次切出多组，
//     再并发提交；提交点（authority.claimTx / ClaimTeamUnits）在锁内用原票和数据库时钟复核全部规则。
//   - 一轮里所有提交都在本轮结束前完成，下一轮读到的已是提交后的状态，同一张票不会被本副本选两次；
//     两个段（跨机房段与机房段、或两个副本）同时选中同一张票时，只有一方过得了提交点的票行复核（ErrClaimStale）。

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
	reportUnusedSubPools(s)
	m := &poolMatcher{s: s, cursors: map[poolowner.Segment]authority.PoolCursor{}, pageLabels: map[poolLabel]bool{}}
	s.StartSourceLoop(ctx, "match.global.pool", interval, m.round)
}

type poolMatcher struct {
	s *svc.ServiceContext
	// cursors 是各撮合段的翻页位置，只由撮合循环这一个 goroutine 访问；只用于调度，不参与提交决策。
	cursors map[poolowner.Segment]authority.PoolCursor
	// pageLabels 是上一轮写过 match_pool_page_units 的标签，本轮没读到的置 0。只由撮合循环访问。
	pageLabels map[poolLabel]bool
}

// poolLabel 是 match_pool_page_units 的 (模式, 人数档) 标签。
type poolLabel struct{ mode, teamSize string }

// poolUnit 是一个不可拆的撮合单位的原件：单排票（launch 为 nil）或一支排队中的整队。
type poolUnit struct {
	tickets []*pb.MatchAuthorityTicketRecord
	launch  *teampb.TeamLaunchRecord
}

// poolClaim 是一个待提交的候选组，units 与 group.Units 一一对应。
type poolClaim struct {
	pool  catalog.Pool
	role  poolowner.Role // 切出这一组的撮合段角色，只用于指标；入队即时成局不经撮合段，为零值
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
	// battle 节点按选点状态计数(match_battle_nodes{state},R6):放在"本副本不负责任何段"的提前返回之前,
	// 不撮合的副本也照常报。同一份快照也给撮合段枚举机房列表。
	nodes := s.BattleNodes.Snapshot()
	for state, count := range s.Placement.Census(nodes) {
		metrics.SetBattleNodes(state, count)
	}
	var owned []poolowner.Segment
	for _, segment := range poolowner.Segments(s.Catalog.Pools(), registeredRegions(nodes), func(pool catalog.Pool) uint32 {
		return s.Config.GlobalMatch.SubPoolCount(uint32(pool.Mode), pool.ConfigId)
	}) {
		if s.OwnsWork(segment.Key()) {
			owned = append(owned, segment)
		}
	}
	setSegmentsOwned(owned)
	m.keepCursorsOf(owned)
	if len(owned) == 0 {
		m.publishPageUnits(nil)
		return
	}
	now, err := s.Authority.Now(ctx)
	if err != nil {
		logx.Errorf("[match.pool] Failed to read database time: %v", err)
		return
	}
	regions := s.Placement.Regions(nodes)
	var claims []poolClaim
	pageUnits := map[poolLabel]int{}
	for _, segment := range owned {
		if ctx.Err() != nil {
			break
		}
		formed, units := m.formSegment(ctx, segment, now, regions)
		claims = append(claims, formed...)
		// 跨机房段的页与机房段重叠(老票两边都读),不计入排队深度。
		if segment.Role != poolowner.RoleCross {
			pageUnits[pageLabelOf(segment.Pool)] += units
		}
	}
	m.publishPageUnits(pageUnits)
	m.claimAll(ctx, claims, nodes)
}

// formSegment 读一个撮合段的一页排队单位并切出候选组;同时终止页里已过期的单排票。返回候选组与本页可撮合的单位数。
func (m *poolMatcher) formSegment(ctx context.Context, segment poolowner.Segment, now uint64, regions map[uint32]bool) ([]poolClaim, int) {
	s := m.s
	pool := segment.Pool
	slice, ok := segmentSlice(segment, now, s.Config.GlobalMatch.SubPoolMergeMs(), uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds)*1000)
	if !ok {
		return nil, 0
	}
	pageSize := s.Config.GlobalMatch.PoolPage()
	tickets, err := s.Authority.SliceTickets(ctx, slice, m.cursors[segment], pageSize)
	if err != nil {
		logx.Errorf("[match.pool] Failed to read pool tickets segment=%s: %v", segment.Key(), err)
		return nil, 0
	}
	var launches []*teampb.TeamLaunchRecord
	if pool.Mode == pb.MatchMode_MATCH_MODE_PVE_TEAM || pool.Mode == pb.MatchMode_MATCH_MODE_PVP_TEAM {
		if launches, err = s.Authority.SliceTeamLaunches(ctx, slice, poolTeamLaunchLimit); err != nil {
			logx.Errorf("[match.pool] Failed to read pool team launches segment=%s: %v", segment.Key(), err)
			return nil, 0
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
	groups := poolgroup.Form(inputs, regions, now, segmentPolicy(s, segment))
	m.advanceCursor(segment, tickets, pageSize, len(groups))
	claims := make([]poolClaim, 0, len(groups))
	for _, group := range groups {
		claim := poolClaim{pool: pool, role: segment.Role, group: group, now: now}
		for _, index := range group.Units {
			claim.units = append(claim.units, units[index])
		}
		claims = append(claims, claim)
	}
	return claims, len(units)
}

// advanceCursor：读到这一段的尾或本页成了组，下一轮从段首重读；整页都凑不出组时翻到下一页，
// 防止一页卡住的票（例如机房暂时没有节点、整队装不进人数档）挡住后面能成局的人。
// 跨机房段的区间上界每轮随时钟前移，新等满阈值的票接在尾部，游标照用（§5.2.1 第 4 条）。
func (m *poolMatcher) advanceCursor(segment poolowner.Segment, tickets []*pb.MatchAuthorityTicketRecord, pageSize, formed int) {
	if len(tickets) < pageSize || formed > 0 {
		delete(m.cursors, segment)
		return
	}
	last := tickets[len(tickets)-1]
	m.cursors[segment] = authority.PoolCursor{EnqueuedMs: last.EnqueuedMs, PlayerId: last.PlayerId}
}

// keepCursorsOf 丢掉本轮不再负责的段的游标:段换了主或机房下线后,旧位置不该在它回来时接着用。
func (m *poolMatcher) keepCursorsOf(owned []poolowner.Segment) {
	keep := make(map[poolowner.Segment]bool, len(owned))
	for _, segment := range owned {
		keep[segment] = true
	}
	for segment := range m.cursors {
		if !keep[segment] {
			delete(m.cursors, segment)
		}
	}
}

// publishPageUnits 写本轮各本机房段读到的可撮合单位数之和;上一轮写过、本轮没读到的标签置 0,
// 不再负责某个池的副本不留着旧读数。
func (m *poolMatcher) publishPageUnits(units map[poolLabel]int) {
	for label := range m.pageLabels {
		if _, ok := units[label]; !ok {
			metrics.SetPoolPageUnits(label.mode, label.teamSize, 0)
			delete(m.pageLabels, label)
		}
	}
	for label, count := range units {
		metrics.SetPoolPageUnits(label.mode, label.teamSize, count)
		m.pageLabels[label] = true
	}
}

func pageLabelOf(pool catalog.Pool) poolLabel {
	return poolLabel{mode: pool.Mode.String(), teamSize: strconv.FormatUint(uint64(pool.TeamSize), 10)}
}

// setSegmentsOwned 按角色写本副本负责的撮合段数,每种角色都写(没有的写 0)。
func setSegmentsOwned(owned []poolowner.Segment) {
	counts := map[poolowner.Role]int{}
	for _, segment := range owned {
		counts[segment.Role]++
	}
	for _, role := range poolowner.Roles {
		metrics.SetPoolOwned(role.String(), counts[role])
	}
}

// registeredRegions 是撮合段枚举用的机房列表:stable battle 节点镜像里出现的 physical_region,不看可分配状态、不扣本地熔断。
// 各副本 watch 同一个 etcd 前缀,看到同一份(§5.2.1 第 2 条);能不能往某个机房放只在成组时看(Placement.Regions)。
// 0 与重复由 poolowner.Segments 去掉。
func registeredRegions(nodes []discovery.NodeEntry) []uint32 {
	out := make([]uint32, 0, len(nodes))
	for _, node := range nodes {
		if node.PhysicalRegion != 0 {
			out = append(out, node.PhysicalRegion)
		}
	}
	return out
}

// segmentSlice 把撮合段换成这一轮要读的那一段票(§5.2.1 第 3 条):机房段、子池段按首选机房读,子池段再按 id 取模、
// 只读入队不满合并时长的;机房段配了子池时、跨机房段只读等满各自阈值的。ok 为 false:时钟还没走过阈值,本轮跳过这一段。
func segmentSlice(segment poolowner.Segment, now, mergeMs, crossMs uint64) (authority.PoolSlice, bool) {
	after, until, ok := segment.Window(now, mergeMs, crossMs)
	if !ok {
		return authority.PoolSlice{}, false
	}
	slice := authority.PoolSlice{Pool: segment.Pool, Region: segment.Region, AfterMs: after, UntilMs: until}
	if segment.Role == poolowner.RoleSub {
		slice.SubPools, slice.Sub = segment.SubPools, segment.Sub
	}
	return slice, true
}

// segmentPolicy 是撮合段的成组规则:与整池同一份 poolPolicy,只按段角色选跑哪些机房阶段。
func segmentPolicy(s *svc.ServiceContext, segment poolowner.Segment) poolgroup.Policy {
	policy := poolPolicy(s, segment.Pool)
	policy.Passes = segmentPasses(segment.Role)
	return policy
}

// segmentPasses:机房段、子池段只跑本机房阶段;跨机房段先跑本机房阶段占位、只提交跨机房阶段的组;整池段两个阶段都跑。
func segmentPasses(role poolowner.Role) poolgroup.Passes {
	switch role {
	case poolowner.RoleRegion, poolowner.RoleSub:
		return poolgroup.PassesLocal
	case poolowner.RoleCross:
		return poolgroup.PassesCross
	}
	return poolgroup.PassesBoth
}

// reportUnusedSubPools 在启动时核子池配置里的模板:目录里没有这个模板的多人池,那一行不起作用,打一条 ERROR,不拒启
// (配置结构的越界已在加载时拒启,§5.2.1 第 5 条)。
func reportUnusedSubPools(s *svc.ServiceContext) {
	for _, entry := range s.Config.GlobalMatch.SubPools {
		used := false
		for _, pool := range s.Catalog.Pools() {
			if uint32(pool.Mode) == entry.Mode && pool.ConfigId == entry.ConfigId && poolowner.Split(pool) {
				used = true
				break
			}
		}
		if !used {
			logx.Errorf("[match.pool] GlobalMatch.SubPools entry mode=%d config=%d matches no open multi-player pool in the PlayMode catalog; it has no effect", entry.Mode, entry.ConfigId)
		}
	}
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
				metrics.ObservePoolSegmentClaim(skipped.role.String(), metrics.PoolClaimNoSlot)
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
			metrics.ObservePoolSegmentClaim(claim.role.String(), outcome)
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
	// 战斗按整局选轨道(发布轨道设计 §2.6):组里全员都是金丝雀会话才先在 canary battle 节点里选,机房仍是撮合按 stable 节点定的。
	node, ok := pickBattleNode(s, poolClaimTrack(claim), nodes, claim.group.Region)
	if !ok {
		return metrics.PoolClaimNoNode
	}
	id, err := s.BattleIDGen.Generate()
	if err != nil {
		return metrics.PoolClaimIDError
	}
	f := &pb.MatchFormationRecord{BattleId: id, OperationId: uuid.NewString(), PhysicalRegion: claim.group.Region,
		TargetNodeId: node.NodeId, TargetInstanceId: node.NodeUuid, TargetEndpoint: node.Endpoint, ReleaseTrack: node.Track.String(),
		Mode: uint32(claim.pool.Mode), ConfigId: claim.pool.ConfigId, RequeueOnFailure: true}
	var tickets []*pb.MatchAuthorityTicketRecord
	var launches []*teampb.TeamLaunchRecord
	for i, unit := range claim.units {
		tickets = append(tickets, unit.tickets...)
		if unit.launch != nil {
			launches = append(launches, unit.launch)
		}
		side := uint32(0)
		if claim.group.Sides != nil {
			side = claim.group.Sides[i]
		}
		for _, ticket := range unit.tickets {
			// 整队开局的发起人就是该队队长:建房时这一方的战斗剧本按队长的选用(battle-script.md §5.2)。
			// 没有分边的模式 side 恒为 0,由 gather 的 teamAssignment 另行分边,这里只是把队长标记带进成局原件
			captain := unit.launch != nil && ticket.PlayerId == unit.launch.GetActorId()
			f.Members = append(f.Members, &pb.MatchFormationMember{PlayerId: ticket.PlayerId, Ticket: ticket.Ticket, TeamIndex: side, Captain: captain})
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
		if errors.Is(err, authority.ErrRatingSpread) {
			// 提交点复核的是撮合用过的同一批原票，按同一份 ratingrule 判，正常不会触发；触发说明两边的规则或配置不一致。
			logx.Errorf("[match.pool] Rating spread rejected at commit; matcher and commit rules disagree mode=%s config=%d battle=%d", claim.pool.Mode, claim.pool.ConfigId, id)
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

// poolPolicy 是一个池的成组规则；提交点用同一组配置复核（车队差是质量规则，提交点不复核）。
func poolPolicy(s *svc.ServiceContext, pool catalog.Pool) poolgroup.Policy {
	rated := isRatedMode(pool.Mode)
	return poolgroup.Policy{
		Target:             pool.Players(),
		Scope:              s.Authority.Schedule(),
		CrossRegionAfterMs: uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds) * 1000,
		Rated:              rated,
		Sides:              pool.Mode == pb.MatchMode_MATCH_MODE_PVP_TEAM,
		PremadeGapStepMs:   s.Config.GlobalMatch.PremadeGapStepMs(),
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
	// 队伍分：单排是本人评分，整队是成员平均分加组队加成；提交点按同一公式复核组内极差（authority.requireRatedSpread）。
	ratings := make([]int64, len(unit.tickets))
	for i, ticket := range unit.tickets {
		ratings[i] = ticket.RatingCenti
	}
	input.RatingCenti = ratingrule.UnitCenti(ratings, s.Config.GlobalMatch.PartyBonusCenti())
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
// 票被取消或已被别的撮合段、别的副本先提交（ErrClaimStale，拆写者之后跨机房段与机房段撞车是常态）、整队名单变化、
// 范围、入口或评分规则在提交点不满足、home 迁移。这些都在提交之前回滚，结局已知，不记排空 HOLD。
func expectedClaimRejection(err error) bool {
	for _, expected := range []error{authority.ErrClaimStale, authority.ErrScopeWait, authority.ErrTemplateClosed, authority.ErrDirectConflict, authority.ErrTeamLaunchActive, authority.ErrOriginalPlayer, authority.ErrTeamRoute, authority.ErrRatingSpread} {
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
