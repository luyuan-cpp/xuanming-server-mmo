package gameobject

import (
	"proto/scene"
	"testing"
)

func TestFeatureResponsesRequireFreshSequenceAndMatchingSource(t *testing.T) {
	p := NewPlayer(1)
	p.SetFeatureSnapshot(193, &scene.GetMissionListResponse{StatePersistent: true}, 0, "")
	cursor := p.FeatureSequence()
	if _, ok := p.FeatureResponse(193, cursor); ok {
		t.Fatal("stale response satisfied next request")
	}
	p.SetFeatureSnapshot(190, &scene.GetActivityListResponse{ServerTimeMs: 5}, 0, "")
	if _, ok := p.FeatureResponse(193, cursor); ok {
		t.Fatal("other RPC satisfied mission request")
	}
	p.SetFeatureSnapshot(193, nil, 7, "server rejected")
	got, ok := p.FeatureResponse(193, cursor)
	if !ok || got.MessageID != 193 || got.TipID != 7 || got.Response != nil || got.Failure == "" {
		t.Fatal("error did not replace the prior success with a fresh failure")
	}
}

func TestFeatureSnapshotsOwnCopiesAcrossReadersAndWriters(t *testing.T) {
	p := NewPlayer(1)
	response := &scene.GetMissionListResponse{Missions: []*scene.PlayerMissionInfo{{MissionId: 15}}}
	p.SetFeatureSnapshot(193, response, 0, "")
	response.Missions[0].MissionId = 99
	first, _ := p.FeatureResponse(193, 0)
	if first.Response.(*scene.GetMissionListResponse).Missions[0].MissionId != 15 {
		t.Fatal("writer mutated stored snapshot")
	}
	first.Response.(*scene.GetMissionListResponse).Missions[0].MissionId = 88
	second, _ := p.FeatureResponse(193, 0)
	if second.Response.(*scene.GetMissionListResponse).Missions[0].MissionId != 15 {
		t.Fatal("reader mutated stored snapshot")
	}
}
