package authority

import (
	"match/internal/config"

	pb "proto/match"
	teampb "proto/team"
)

// TeamLaunchMode 的零值只兼容旧的PVE原件，不能把未知模式降级成PVE。
func TeamLaunchMode(launch *teampb.TeamLaunchRecord) uint32 {
	if launch.Mode == 0 {
		return uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
	}
	return launch.Mode
}

func validTeamQueueMode(mode uint32) bool {
	return mode == uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM) || mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
}

// 提交点只信显式分边：人数、原票和整队同边一起复核，恢复也读取同一份原件。
func validatePvpTeamFormation(formation *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) error {
	if formation.Mode != uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		return nil
	}
	if len(group) < 2 || len(group) > 2*config.TeamCapacity || len(formation.Members) != len(group) {
		return ErrDirectConflict
	}
	sides := make(map[uint64]uint32, len(group))
	tickets := make(map[uint64]string,len(group))
	var counts [2]uint32
	for _, member := range formation.Members {
		if member == nil || member.PlayerId == 0 || member.TeamIndex > 1 {
			return ErrDirectConflict
		}
		if _, duplicate := sides[member.PlayerId]; duplicate {
			return ErrDirectConflict
		}
		sides[member.PlayerId] = member.TeamIndex
		tickets[member.PlayerId] = member.Ticket
		counts[member.TeamIndex]++
	}
	if counts[0] == 0 || counts[0] > config.TeamCapacity || counts[0] != counts[1] {
		return ErrDirectConflict
	}
	teamSides := map[uint64]uint32{}
	seen := map[uint64]bool{}
	for _, ticket := range group {
		if ticket == nil || seen[ticket.PlayerId] || ticket.Mode != formation.Mode || ticket.ConfigId != formation.ConfigId || ticket.TeamSize != counts[0] {
			return ErrDirectConflict
		}
		side, present := sides[ticket.PlayerId]
		if !present || tickets[ticket.PlayerId]!=ticket.Ticket {
			return ErrDirectConflict
		}
		seen[ticket.PlayerId] = true
		if ticket.TeamId != 0 {
			if previous, found := teamSides[ticket.TeamId]; found && previous != side {
				return ErrDirectConflict
			}
			teamSides[ticket.TeamId] = side
		}
	}
	return nil
}
