package logic

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
)

func TestTacticalPuzzleLogicRejectsMissingSessionAndInvalidInput(t *testing.T) {
	noSession := NewTacticalPuzzleLogic(context.Background(), &svc.ServiceContext{})
	if resp, err := noSession.ListTacticalPuzzles(&matchpb.ListTacticalPuzzlesRequest{}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("ListTacticalPuzzles missing session got resp=%+v err=%v", resp, err)
	}
	if resp, err := noSession.SaveTacticalPuzzleDraft(&matchpb.SaveTacticalPuzzleDraftRequest{}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("SaveTacticalPuzzleDraft missing session got resp=%+v err=%v", resp, err)
	}

	// 验证会话有效但缺少底层依赖时仍先执行输入文本/配置校验（需有 Authority + IDGen 才会放行文本校验，因此用 nil Authority 时测内部错误，或造最小逻辑校验）
	authed := NewTacticalPuzzleLogic(boundSessionCtx(ownPlayerID), &svc.ServiceContext{})
	if resp, err := authed.GetTacticalPuzzle(&matchpb.GetTacticalPuzzleRequest{PuzzleId: 1}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("GetTacticalPuzzle nil authority got resp=%+v err=%v", resp, err)
	}
	if resp, err := authed.DeleteTacticalPuzzle(&matchpb.DeleteTacticalPuzzleRequest{PuzzleId: 1}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("DeleteTacticalPuzzle nil authority got resp=%+v err=%v", resp, err)
	}
	if resp, err := authed.LikeTacticalPuzzle(&matchpb.LikeTacticalPuzzleRequest{PuzzleId: 1}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("LikeTacticalPuzzle nil authority got resp=%+v err=%v", resp, err)
	}
	if resp, err := authed.GetTacticalPuzzleSolutionReplay(&matchpb.GetTacticalPuzzleSolutionReplayRequest{PuzzleId: 1}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("GetTacticalPuzzleSolutionReplay nil authority got resp=%+v err=%v", resp, err)
	}
	if resp, err := authed.StartTacticalPuzzleBattle(&matchpb.StartTacticalPuzzleBattleRequest{PuzzleId: 1}); err != nil || resp.GetErrorMessage().GetId() != constants.ErrInternal {
		t.Fatalf("StartTacticalPuzzleBattle nil authority got resp=%+v err=%v", resp, err)
	}
	_ = &battlepb.TacticalPuzzleSpec{}
}
