package logic

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/svc"

	matchpb "proto/match"
)

func TestTrialChallengeCardRejectsMissingSessionAndZeroIds(t *testing.T) {
	noSession := NewTrialChallengeCardLogic(context.Background(), &svc.ServiceContext{})

	cResp, err := noSession.CreateTrialChallengeCard(&matchpb.CreateTrialChallengeCardRequest{BattleId: 1001})
	if err != nil || cResp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("CreateTrialChallengeCard missing session got resp=%+v err=%v", cResp, err)
	}
	gResp, err := noSession.GetTrialChallengeCard(&matchpb.GetTrialChallengeCardRequest{CardId: 2001})
	if err != nil || gResp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("GetTrialChallengeCard missing session got resp=%+v err=%v", gResp, err)
	}
	rResp, err := noSession.GetSharedTrialReplay(&matchpb.GetSharedTrialReplayRequest{CardId: 2001})
	if err != nil || rResp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("GetSharedTrialReplay missing session got resp=%+v err=%v", rResp, err)
	}

	authed := NewTrialChallengeCardLogic(boundSessionCtx(ownPlayerID), &svc.ServiceContext{})
	cZero, err := authed.CreateTrialChallengeCard(&matchpb.CreateTrialChallengeCardRequest{BattleId: 0})
	if err != nil || cZero.GetErrorMessage().GetId() != constants.ErrBattleReportNotFound {
		t.Fatalf("CreateTrialChallengeCard zero battle got resp=%+v err=%v", cZero, err)
	}
	gZero, err := authed.GetTrialChallengeCard(&matchpb.GetTrialChallengeCardRequest{CardId: 0})
	if err != nil || gZero.GetErrorMessage().GetId() != constants.ErrBattleReportNotFound {
		t.Fatalf("GetTrialChallengeCard zero card got resp=%+v err=%v", gZero, err)
	}
	rZero, err := authed.GetSharedTrialReplay(&matchpb.GetSharedTrialReplayRequest{CardId: 0})
	if err != nil || rZero.GetErrorMessage().GetId() != constants.ErrBattleReportNotFound {
		t.Fatalf("GetSharedTrialReplay zero card got resp=%+v err=%v", rZero, err)
	}
}
