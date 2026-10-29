package authority

import (
	pb "proto/match"
	"testing"
)

func TestPvpTeamFormationSameSizeWholeParties(t *testing.T) {
	for size := uint32(1); size <= 5; size++ {
		formation := &pb.MatchFormationRecord{Mode: uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM), ConfigId: 1}
		var group []*pb.MatchAuthorityTicketRecord
		for side := uint32(0); side < 2; side++ {
			for i := uint32(0); i < size; i++ {
				id := uint64(side*10 + i + 1)
				formation.Members = append(formation.Members, &pb.MatchFormationMember{PlayerId: id, TeamIndex: side})
				group = append(group, &pb.MatchAuthorityTicketRecord{PlayerId: id, TeamId: uint64(side + 100), Mode: formation.Mode, ConfigId: 1, TeamSize: size})
			}
		}
		if err := validatePvpTeamFormation(formation, group); err != nil {
			t.Fatalf("%dv%d合法整队被拒绝: %v", size, size, err)
		}
		if size > 1 {
			formation.Members[0].TeamIndex = 1
			formation.Members[len(formation.Members)-1].TeamIndex = 0
			if err := validatePvpTeamFormation(formation, group); err == nil {
				t.Fatalf("%dv%d不能按评分拆散原队伍", size, size)
			}
		}
	}
}

func TestPvpTeamFormationRejectsMissingUnevenAndDuplicateSides(t *testing.T) {
	base := func() (*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord) {
		f := &pb.MatchFormationRecord{Mode: uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM), ConfigId: 1, Members: []*pb.MatchFormationMember{{PlayerId: 1}, {PlayerId: 2, TeamIndex: 1}}}
		return f, []*pb.MatchAuthorityTicketRecord{{PlayerId: 1, Mode: f.Mode, ConfigId: 1, TeamSize: 1}, {PlayerId: 2, Mode: f.Mode, ConfigId: 1, TeamSize: 1}}
	}
	for _, mutate := range []func(*pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord){
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { f.Members = nil },
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { f.Members[1].TeamIndex = 0 },
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { f.Members[1].TeamIndex = 2 },
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { f.Members[1].PlayerId = 1 },
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) {
			g[1].Mode = uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM)
		},
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { g[1].TeamSize = 2 },
		func(f *pb.MatchFormationRecord, g []*pb.MatchAuthorityTicketRecord) { g[1].PlayerId = 1 },
	} {
		f, g := base()
		mutate(f, g)
		if err := validatePvpTeamFormation(f, g); err == nil {
			t.Fatal("损坏的PVP分边或原票必须拒绝")
		}
	}
}
