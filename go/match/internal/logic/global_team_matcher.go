package logic

import (
	"context"
	"errors"
	"sort"
	"time"

	"match/internal/authority"
	"match/internal/config"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/svc"

	pb "proto/match"
	teampb "proto/team"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

type globalTeamUnit struct {
	tickets []*pb.MatchAuthorityTicketRecord
	launch  *teampb.TeamLaunchRecord
}

type globalTeamCursor struct {
	soloMs, soloPlayer             uint64
	party                          authority.TeamLaunchCursor
	repair, through                authority.TeamLaunchCursor
	repairStarted, projectionReady bool
}

func StartGlobalTeamMatcher(ctx context.Context, s *svc.ServiceContext) {
	if s.Authority == nil {
		return
	}
	cursor := &globalTeamCursor{}
	s.StartSourceLoop(ctx, "match.global.team", 500*time.Millisecond, func(ctx context.Context) { cursor.round(ctx, s) })
}

func (cursor *globalTeamCursor) round(parent context.Context, s *svc.ServiceContext) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if !cursor.projectionReady {
		if !cursor.repairStarted {
			through, err := s.Authority.TeamLaunchRepairHighWater(ctx)
			if err != nil {
				return
			}
			cursor.through, cursor.repairStarted = through, true
		}
		next, complete, err := s.Authority.RepairTeamLaunchProjectionThrough(ctx, cursor.repair, cursor.through)
		if err != nil {
			s.HoldRelay(err)
			return
		}
		cursor.repair, cursor.projectionReady = next, complete
		// 独立固定尾界完成前不把缺投影当人数不足；新入队已同tx建好，不延长本轮。
		if !complete {
			return
		}
	}
	solos, err := s.Authority.TeamSoloAnchorsAfter(ctx, cursor.soloMs, cursor.soloPlayer)
	if err != nil {
		return
	}
	parties, err := s.Authority.TeamLaunchesAfter(ctx, 2, cursor.party)
	if err != nil {
		return
	}
	if len(solos) == 0 {
		cursor.soloMs, cursor.soloPlayer = 0, 0
	}
	if len(parties) == 0 {
		cursor.party = authority.TeamLaunchCursor{}
	}
	units := make([]globalTeamUnit, 0, len(solos)+len(parties))
	for _, ticket := range solos {
		units = append(units, globalTeamUnit{tickets: []*pb.MatchAuthorityTicketRecord{ticket}})
	}
	for _, launch := range parties {
		units = append(units, globalTeamUnit{launch: launch})
	}
	sort.SliceStable(units, func(i, j int) bool {
		leftMs, leftPlayer := teamUnitOrder(units[i])
		rightMs, rightPlayer := teamUnitOrder(units[j])
		if leftMs != rightMs {
			return leftMs < rightMs
		}
		return leftPlayer < rightPlayer
	})
	if len(units) > 32 {
		units = units[:32]
	}
	stamp, err := s.Authority.Now(ctx)
	if err != nil {
		return
	}
	nodes := teamRegionNodes(s.BattleNodes.Snapshot())
	loaded := make([]globalTeamUnit, 0, len(units))
	localCtx, localCancel := context.WithTimeout(ctx, time.Second)
	for _, unit := range units {
		if localCtx.Err() != nil {
			break
		}
		// 只推进本轮真正尝试项。单个未知/过期unit不会挡住后续锚点。
		if unit.launch == nil {
			last := unit.tickets[0]
			cursor.soloMs, cursor.soloPlayer = last.EnqueuedMs, last.PlayerId
		} else {
			last := unit.launch
			cursor.party = authority.TeamLaunchCursor{EnqueuedMs: last.EnqueuedMs, ActorId: last.ActorId, RequestHigh: last.RequestHigh, RequestLow: last.RequestLow}
			unit.tickets, err = s.Authority.TeamLaunchTickets(localCtx, last)
			if err != nil {
				continue
			}
		}
		if !validTeamUnit(unit, stamp, s.Config.GlobalMatch.HardLatencyMs) {
			continue
		}
		eligible, checkErr := checkGlobalTeamUnit(localCtx, s, unit)
		if checkErr != nil {
			localCancel()
			return
		}
		if !eligible {
			continue
		}
		group, region, err := findTeamUnitGroup(localCtx, s, unit, nodes, stamp, true)
		if err != nil {
			if localCtx.Err() != nil {
				break
			}
			localCancel()
			return
		}
		if len(group) > 0 {
			localCancel()
			claimTeamUnitGroup(ctx, s, group, region, nodes[region])
			return
		}
		loaded = append(loaded, unit)
	}
	localCancel()
	if !s.GlobalRegionProjectionReady.Load() {
		return
	}
	// 本批所有锚点本区均完整尝试后才扩区；每个unit内每人仍须等满原等待期。
	for _, unit := range loaded {
		if ctx.Err() != nil {
			return
		}
		group, region, err := findTeamUnitGroup(ctx, s, unit, nodes, stamp, false)
		if err != nil {
			return
		}
		if len(group) > 0 {
			claimTeamUnitGroup(ctx, s, group, region, nodes[region])
			return
		}
	}
}

func teamUnitOrder(unit globalTeamUnit) (uint64, uint64) {
	if unit.launch != nil {
		return unit.launch.EnqueuedMs, unit.launch.ActorId
	}
	return unit.tickets[0].EnqueuedMs, unit.tickets[0].PlayerId
}

func teamRegionNodes(nodes []discovery.NodeEntry) map[uint32]discovery.NodeEntry {
	out := map[uint32]discovery.NodeEntry{}
	for _, node := range nodes {
		if node.PhysicalRegion == 0 || node.NodeUuid == "" {
			continue
		}
		if old, ok := out[node.PhysicalRegion]; !ok || node.NodeId < old.NodeId {
			out[node.PhysicalRegion] = node
		}
	}
	return out
}

func validTeamUnit(unit globalTeamUnit, stamp uint64, hard uint32) bool {
	if len(unit.tickets) == 0 || len(unit.tickets) > config.TeamCapacity {
		return false
	}
	for _, ticket := range unit.tickets {
		if ticket.State != 1 || ticket.ExpiresMs <= stamp || ticket.NotBeforeMs > stamp || ticket.TeamSize == 0 || ticket.TeamSize > config.TeamCapacity {
			return false
		}
		profile, err := authority.DecodeProfile(ticket.NetworkProfile)
		if err != nil || len(profile.Receipts) > authority.MaxProbeRegions || len(authority.Latencies(profile, stamp, hard)) == 0 {
			return false
		}
	}
	return true
}

func teamUnitRegions(unit globalTeamUnit, stamp uint64, hard uint32, local bool, wait uint64) []uint32 {
	common := map[uint32]bool{}
	for index, ticket := range unit.tickets {
		if !local && (stamp < wait || ticket.EnqueuedMs > stamp-wait) {
			return nil
		}
		profile, err := authority.DecodeProfile(ticket.NetworkProfile)
		if err != nil {
			return nil
		}
		latencies := authority.Latencies(profile, stamp, hard)
		preferred := authority.PreferredRegion(profile, stamp)
		if index == 0 {
			for region := range latencies {
				if !local || region == preferred {
					common[region] = true
				}
			}
		} else {
			for region := range common {
				if _, ok := latencies[region]; !ok || (local && region != preferred) {
					delete(common, region)
				}
			}
		}
	}
	regions := make([]uint32, 0, len(common))
	for region := range common {
		regions = append(regions, region)
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	return regions
}

func findTeamUnitGroup(ctx context.Context, s *svc.ServiceContext, anchor globalTeamUnit, nodes map[uint32]discovery.NodeEntry, stamp uint64, local bool) ([]globalTeamUnit, uint32, error) {
	a := anchor.tickets[0]
	home, err := authority.TeamUnitHomeZone(anchor.tickets)
	if err != nil {
		return nil, 0, err
	}
	if s.Config.Team.AllowCrossZone {
		home = 0
	} else if home == 0 {
		return nil, 0, nil
	}
	wait := uint64(s.Config.GlobalMatch.CrossRegionWaitSeconds) * 1000
	for _, region := range teamUnitRegions(anchor, stamp, s.Config.GlobalMatch.HardLatencyMs, local, wait) {
		if _, ok := nodes[region]; !ok {
			continue
		}
		if uint32(len(anchor.tickets)) == a.TeamSize {
			return []globalTeamUnit{anchor}, region, nil
		}
		var peers []globalTeamUnit
		solos, err := s.Authority.CandidateTeamPeers(ctx, a, region, local, stamp)
		if err != nil {
			return nil, 0, err
		}
		for _, ticket := range solos {
			peers = append(peers, globalTeamUnit{tickets: []*pb.MatchAuthorityTicketRecord{ticket}})
		}
		remaining := a.TeamSize - uint32(len(anchor.tickets))
		for size := uint32(2); size <= remaining; size++ {
			parties, err := s.Authority.TeamLaunchPeers(ctx, a, anchor.launch, region, size, local, stamp)
			if err != nil {
				return nil, 0, err
			}
			for _, party := range parties {
				tickets, err := s.Authority.TeamLaunchTickets(ctx, party)
				if err != nil {
					return nil, 0, err
				}
				unit := globalTeamUnit{tickets: tickets, launch: party}
				eligible, checkErr := checkGlobalTeamUnit(ctx, s, unit)
				if checkErr != nil {
					return nil, 0, checkErr
				}
				if !eligible {
					return nil, 0, errors.New("team: 失效原unit已撤，须重新读取本区完整候选")
				}
				regions := teamUnitRegions(unit, stamp, s.Config.GlobalMatch.HardLatencyMs, local, wait)
				found := false
				for _, eligible := range regions {
					if eligible == region {
						found = true
					}
				}
				if !found {
					return nil, 0, errors.New("team: 候选原件已变化，必须重新读取本区集合")
				}
				peers = append(peers, unit)
			}
		}
		for _, peer := range peers {
			peerHome, err := authority.TeamUnitHomeZone(peer.tickets)
			if err != nil || (home != 0 && peerHome != home) {
				return nil, 0, errors.New("team: 候选原home已变化，须重新读取完整集合")
			}
		}
		if group := packTeamUnits(anchor, peers, int(a.TeamSize), home); len(group) > 0 {
			return group, region, nil
		}
	}
	return nil, 0, nil
}

func checkGlobalTeamUnit(ctx context.Context, s *svc.ServiceContext, unit globalTeamUnit) (bool, error) {
	if unit.launch == nil {
		return true, nil
	}
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

// 人数上限5、每size桶最多5个unit，动态规划O(25×5)，完整unit从不拆分。
func packTeamUnits(anchor globalTeamUnit, peers []globalTeamUnit, target int, home uint32) []globalTeamUnit {
	if target < 1 || target > config.TeamCapacity || len(anchor.tickets) < 1 || len(anchor.tickets) > target {
		return nil
	}
	if actual, err := authority.TeamUnitHomeZone(anchor.tickets); err != nil || (home != 0 && actual != home) {
		return nil
	}
	peers = append([]globalTeamUnit(nil), peers...)
	sort.SliceStable(peers, func(i, j int) bool {
		a, b := teamUnitOrder(peers[i])
		c, d := teamUnitOrder(peers[j])
		if a != c {
			return a < c
		}
		return b < d
	})
	solutions := make([][]globalTeamUnit, target+1)
	solutions[len(anchor.tickets)] = []globalTeamUnit{anchor}
	for _, peer := range peers {
		size := len(peer.tickets)
		if size == 0 {
			continue
		}
		if actual, err := authority.TeamUnitHomeZone(peer.tickets); err != nil || (home != 0 && actual != home) {
			continue
		}
		for count := target; count >= size; count-- {
			if solutions[count] != nil || solutions[count-size] == nil {
				continue
			}
			duplicate := false
			for _, unit := range solutions[count-size] {
				for _, a := range unit.tickets {
					for _, b := range peer.tickets {
						if a.PlayerId == b.PlayerId {
							duplicate = true
						}
					}
				}
			}
			if !duplicate {
				solutions[count] = append(append([]globalTeamUnit(nil), solutions[count-size]...), peer)
			}
		}
	}
	return solutions[target]
}

func claimTeamUnitGroup(ctx context.Context, s *svc.ServiceContext, units []globalTeamUnit, region uint32, node discovery.NodeEntry) {
	var tickets []*pb.MatchAuthorityTicketRecord
	var parties []*teampb.TeamLaunchRecord
	for _, unit := range units {
		tickets = append(tickets, unit.tickets...)
		if unit.launch != nil {
			parties = append(parties, unit.launch)
		}
	}
	id, err := s.BattleIDGen.Generate()
	if err != nil {
		return
	}
	formation := &pb.MatchFormationRecord{BattleId: id, OperationId: uuid.NewString(), PhysicalRegion: region, TargetNodeId: node.NodeId, TargetInstanceId: node.NodeUuid, TargetEndpoint: node.Endpoint, Mode: uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: tickets[0].ConfigId}
	if err = s.Authority.ClaimTeamUnits(ctx, formation, tickets, parties); err != nil {
		if !errors.Is(err, authority.ErrDirectConflict) && !errors.Is(err, authority.ErrTeamLaunchActive) && !errors.Is(err, authority.ErrOriginalPlayer) {
			s.HoldRelay(err)
		}
		logx.Error("全球整队装箱提交冲突，保留原unit")
		return
	}
	launchGlobalFormation(s, formation)
}
