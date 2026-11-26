package logic

import (
	"context"
	"sync"
	"testing"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	chatpb "proto/chat"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	friendpb "proto/friend"
	mailpb "proto/mail"
	matchpb "proto/match"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeJianghuStore struct {
	mu            sync.Mutex
	teamMu        sync.Mutex
	invites       map[uint64]*matchpb.MatchJianghuDuelInvitationRecord
	cooldownUntil map[[2]uint64]uint64
	dailyCount    map[uint64]struct {
		day   uint32
		count uint32
	}
	refunds map[uint64]*matchpb.MatchJianghuRefundRecord
	teams   map[uint64]struct {
		teamID   uint64
		leaderID uint64
		members  []uint64
	}
	grudges       map[[3]uint64]*matchpb.JianghuGrudgeEntry
	dismissed     map[[3]uint64]bool
	appliedBattle map[uint64]bool
}

func newFakeJianghuStore() *fakeJianghuStore {
	return &fakeJianghuStore{
		invites:       make(map[uint64]*matchpb.MatchJianghuDuelInvitationRecord),
		cooldownUntil: make(map[[2]uint64]uint64),
		dailyCount: make(map[uint64]struct {
			day   uint32
			count uint32
		}),
		refunds: make(map[uint64]*matchpb.MatchJianghuRefundRecord),
		teams: make(map[uint64]struct {
			teamID   uint64
			leaderID uint64
			members  []uint64
		}),
		grudges:       make(map[[3]uint64]*matchpb.JianghuGrudgeEntry),
		dismissed:     make(map[[3]uint64]bool),
		appliedBattle: make(map[uint64]bool),
	}
}

func (f *fakeJianghuStore) setTeam(teamID uint64, members ...uint64) {
	f.teamMu.Lock()
	defer f.teamMu.Unlock()
	cp := append([]uint64(nil), members...)
	var leader uint64
	if len(cp) > 0 {
		leader = cp[0]
	}
	for _, pid := range cp {
		f.teams[pid] = struct {
			teamID   uint64
			leaderID uint64
			members  []uint64
		}{teamID: teamID, leaderID: leader, members: cp}
	}
}

func (f *fakeJianghuStore) CreateJianghuDuelInvite(_ context.Context, rec *matchpb.MatchJianghuDuelInvitationRecord, sameTargetCooldownMs uint64, dailyUnmatchedLimit uint32, nowMs uint64) (*matchpb.MatchJianghuDuelInvitationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cdKey := [2]uint64{rec.ChallengerCaptainId, rec.TargetCaptainId}
	if until := f.cooldownUntil[cdKey]; until > nowMs {
		return nil, authority.ErrJianghuSameTargetCooldown
	}
	day := authority.JianghuGameDayKey(nowMs)
	dc := f.dailyCount[rec.ChallengerCaptainId]
	if dc.day == day && dailyUnmatchedLimit > 0 && dc.count >= dailyUnmatchedLimit {
		return nil, authority.ErrJianghuDailyInviteLimitReached
	}
	for _, existing := range f.invites {
		if existing.TargetCaptainId == rec.TargetCaptainId &&
			(existing.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET) ||
				existing.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM)) &&
			existing.ExpiresMs > nowMs {
			return nil, authority.ErrJianghuInvitePending
		}
	}
	cp := proto.Clone(rec).(*matchpb.MatchJianghuDuelInvitationRecord)
	f.invites[rec.DuelInviteId] = cp
	f.cooldownUntil[cdKey] = nowMs + sameTargetCooldownMs
	if dc.day == day {
		dc.count++
	} else {
		dc.day = day
		dc.count = 1
	}
	f.dailyCount[rec.ChallengerCaptainId] = dc
	return proto.Clone(cp).(*matchpb.MatchJianghuDuelInvitationRecord), nil
}

func (f *fakeJianghuStore) GetJianghuDuelInvite(_ context.Context, duelInviteID uint64) (*matchpb.MatchJianghuDuelInvitationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.invites[duelInviteID]
	if !ok {
		return nil, authority.ErrJianghuInviteNotFound
	}
	return proto.Clone(rec).(*matchpb.MatchJianghuDuelInvitationRecord), nil
}

func (f *fakeJianghuStore) UpdateJianghuDuelInvite(_ context.Context, rec *matchpb.MatchJianghuDuelInvitationRecord, expectedState uint32) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.invites[rec.DuelInviteId]
	if !ok || cur.State != expectedState {
		return false, nil
	}
	f.invites[rec.DuelInviteId] = proto.Clone(rec).(*matchpb.MatchJianghuDuelInvitationRecord)
	return true, nil
}

func (f *fakeJianghuStore) MutateJianghuDuelInvite(_ context.Context, duelInviteID uint64, mutate func(*matchpb.MatchJianghuDuelInvitationRecord, func(uint64, uint64) (bool, error)) (bool, error)) (*matchpb.MatchJianghuDuelInvitationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.invites[duelInviteID]
	if !ok {
		return nil, authority.ErrJianghuInviteNotFound
	}
	cp := proto.Clone(cur).(*matchpb.MatchJianghuDuelInvitationRecord)
	checkBusy := func(targetCaptainID, nowMs uint64) (bool, error) {
		for _, ex := range f.invites {
			if ex.DuelInviteId != duelInviteID && ex.TargetCaptainId == targetCaptainID &&
				(ex.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET) ||
					ex.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM)) &&
				ex.ExpiresMs > nowMs {
				return true, nil
			}
		}
		return false, nil
	}
	commit, err := mutate(cp, checkBusy)
	if err != nil {
		return proto.Clone(cp).(*matchpb.MatchJianghuDuelInvitationRecord), err
	}
	if commit {
		f.invites[duelInviteID] = proto.Clone(cp).(*matchpb.MatchJianghuDuelInvitationRecord)
	}
	return proto.Clone(cp).(*matchpb.MatchJianghuDuelInvitationRecord), nil
}

func (f *fakeJianghuStore) SetJianghuDuelInviteBattleID(_ context.Context, duelInviteID, battleID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.invites[duelInviteID]; ok && cur.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED) {
		cur.BattleId = battleID
	}
	return nil
}

func (f *fakeJianghuStore) AbortMatchedJianghuDuelInvite(_ context.Context, duelInviteID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.invites[duelInviteID]; ok && cur.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED) && cur.BattleId == 0 {
		cur.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
	}
	return nil
}

func (f *fakeJianghuStore) DecrementDailyUnmatchedOnMatch(_ context.Context, challengerID uint64, nowMs uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	day := authority.JianghuGameDayKey(nowMs)
	dc := f.dailyCount[challengerID]
	if dc.day == day && dc.count > 0 {
		dc.count--
		f.dailyCount[challengerID] = dc
	}
	return nil
}

func (f *fakeJianghuStore) ListActiveJianghuDuelInvitesForPlayer(_ context.Context, playerID uint64, nowMs uint64) ([]*matchpb.MatchJianghuDuelInvitationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*matchpb.MatchJianghuDuelInvitationRecord
	for _, r := range f.invites {
		if (r.ChallengerCaptainId == playerID || r.TargetCaptainId == playerID) &&
			(r.State == 1 || r.State == 2 || r.State == 3) && r.ExpiresMs > nowMs {
			out = append(out, proto.Clone(r).(*matchpb.MatchJianghuDuelInvitationRecord))
		}
	}
	return out, nil
}

func (f *fakeJianghuStore) RecordJianghuRefundOnce(_ context.Context, rec *matchpb.MatchJianghuRefundRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.refunds[rec.DuelInviteId]; exists {
		return false, nil
	}
	f.refunds[rec.DuelInviteId] = proto.Clone(rec).(*matchpb.MatchJianghuRefundRecord)
	return true, nil
}

func (f *fakeJianghuStore) UpdateJianghuRefundMailID(_ context.Context, duelInviteID, mailID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.refunds[duelInviteID]; ok {
		r.RefundMailId = mailID
	}
	return nil
}

func (f *fakeJianghuStore) DeleteJianghuRefundMarker(_ context.Context, duelInviteID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.refunds[duelInviteID]; ok && r.RefundMailId == 0 {
		delete(f.refunds, duelInviteID)
	}
	return nil
}

func (f *fakeJianghuStore) ApplyJianghuDuelBattleResult(_ context.Context, event *kafkapb.BattleResultEvent, ownerRevs map[uint64]uint64) (authority.JianghuBattleGrudgeSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var summary authority.JianghuBattleGrudgeSummary
	if f.appliedBattle[event.GetBattleId()] {
		return summary, false, nil
	}
	ctxDuel := event.GetJianghuDuelContext()
	if ctxDuel == nil || event.GetTotalRounds() == 0 || event.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID {
		return summary, false, nil
	}
	f.appliedBattle[event.GetBattleId()] = true
	capA, capB := ctxDuel.GetChallengerCaptainId(), ctxDuel.GetTargetCaptainId()
	var outA, outB uint32
	switch event.GetOutcome() {
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		outA, outB = 1, 2
		summary.WinnerPlayerID, summary.LoserPlayerID = capA, capB
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		outA, outB = 2, 1
		summary.WinnerPlayerID, summary.LoserPlayerID = capB, capA
	case battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		outA, outB = 3, 3
		summary.IsDraw = true
	default:
		return summary, false, nil
	}
	updateSide := func(pid, peer uint64, peerName string, outcome uint32) {
		key := [3]uint64{pid, ownerRevs[pid], peer}
		e, ok := f.grudges[key]
		if !ok {
			e = &matchpb.JianghuGrudgeEntry{PeerPlayerId: peer, PeerName: peerName}
			f.grudges[key] = e
		}
		e.PeerName = peerName
		e.LastBattleMs = event.GetFinishedAtMs()
		f.dismissed[key] = false
		prevWin := e.WinStreak
		switch outcome {
		case 1:
			e.Wins++
			e.WinStreak++
			e.LoseStreak = 0
			if pid == summary.WinnerPlayerID {
				summary.WinnerNewWinStreak = e.WinStreak
			}
		case 2:
			e.Losses++
			e.WinStreak = 0
			e.LoseStreak++
			if pid == summary.LoserPlayerID {
				summary.LoserPrevWinStreak = prevWin
			}
		case 3:
			e.Draws++
		}
		e.RecentLogs = append([]*matchpb.JianghuGrudgeBattleLogEntry{{
			BattleId:       event.GetBattleId(),
			OutcomeForSelf: outcome,
			TotalRounds:    event.GetTotalRounds(),
			WritTier:       matchpb.JianghuWritTier(ctxDuel.GetWritTier()),
			TeamSize:       ctxDuel.GetTeamSize(),
			FinishedAtMs:   event.GetFinishedAtMs(),
		}}, e.RecentLogs...)
	}
	updateSide(capA, capB, ctxDuel.GetTargetCaptainName(), outA)
	updateSide(capB, capA, ctxDuel.GetChallengerCaptainName(), outB)
	return summary, true, nil
}

func (f *fakeJianghuStore) ListJianghuGrudges(_ context.Context, playerID, ownerRevision uint64) ([]*matchpb.JianghuGrudgeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*matchpb.JianghuGrudgeEntry
	for k, v := range f.grudges {
		if k[0] == playerID && k[1] == ownerRevision && !f.dismissed[k] {
			out = append(out, proto.Clone(v).(*matchpb.JianghuGrudgeEntry))
		}
	}
	return out, nil
}

func (f *fakeJianghuStore) DismissJianghuGrudge(_ context.Context, playerID, ownerRevision, peerPlayerID uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := [3]uint64{playerID, ownerRevision, peerPlayerID}
	if _, ok := f.grudges[key]; !ok || f.dismissed[key] {
		return false, nil
	}
	f.dismissed[key] = true
	return true, nil
}

func (f *fakeJianghuStore) LoadTeamMemberIDs(_ context.Context, captainID uint64) (uint64, uint64, []uint64, error) {
	f.teamMu.Lock()
	defer f.teamMu.Unlock()
	if t, ok := f.teams[captainID]; ok {
		return t.teamID, t.leaderID, append([]uint64(nil), t.members...), nil
	}
	return 0, captainID, []uint64{captainID}, nil
}

type fakeFriendClient struct {
	friendpb.ClientPlayerFriendClient
	hasBlock   bool
	allFriends bool
}

func (f *fakeFriendClient) BatchCheckSocialRelations(_ context.Context, _ *friendpb.BatchCheckSocialRelationsRequest, _ ...grpc.CallOption) (*friendpb.BatchCheckSocialRelationsResponse, error) {
	return &friendpb.BatchCheckSocialRelationsResponse{
		HasAnyBlock:   f.hasBlock,
		AllAreFriends: f.allFriends,
	}, nil
}

type fakeChatClient struct {
	chatpb.ClientPlayerChatClient
	notices []*chatpb.PublishSystemWorldNoticeRequest
}

func (f *fakeChatClient) PublishSystemWorldNotice(_ context.Context, in *chatpb.PublishSystemWorldNoticeRequest, _ ...grpc.CallOption) (*chatpb.PublishSystemWorldNoticeResponse, error) {
	f.notices = append(f.notices, proto.Clone(in).(*chatpb.PublishSystemWorldNoticeRequest))
	return &chatpb.PublishSystemWorldNoticeResponse{}, nil
}

type fakeMailAdminClient struct {
	mailpb.MailAdminClient
	sent    []*mailpb.SendMailRequest
	failErr error
}

func (f *fakeMailAdminClient) SendPersonalMail(_ context.Context, in *mailpb.SendMailRequest, _ ...grpc.CallOption) (*mailpb.SendMailResponse, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	f.sent = append(f.sent, proto.Clone(in).(*mailpb.SendMailRequest))
	return &mailpb.SendMailResponse{MailId: uint64(9000 + len(f.sent))}, nil
}

type seqIDGen struct {
	next uint64
}

func (g *seqIDGen) Generate() (uint64, error) {
	g.next++
	return g.next, nil
}

func withPlayerSession(playerID uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{
		PlayerId: playerID,
	})
}

func TestJianghuDuel_1v1InviteAcceptAndCooldown(t *testing.T) {
	origNow := jianghuNowMsFn
	origAvail := checkJianghuPlayersAvailableFn
	origPush := pushJianghuToPlayerFn
	origLaunch := launchJianghuBattleFn
	defer func() {
		jianghuNowMsFn = origNow
		checkJianghuPlayersAvailableFn = origAvail
		pushJianghuToPlayerFn = origPush
		launchJianghuBattleFn = origLaunch
	}()

	now := uint64(1_730_000_000_000)
	jianghuNowMsFn = func() uint64 { return now }
	checkJianghuPlayersAvailableFn = func(*svc.ServiceContext, []uint64, []uint64) (uint32, string) { return 0, "" }
	var pushed []uint64
	pushJianghuToPlayerFn = func(_ context.Context, _ *svc.ServiceContext, pid uint64, _ uint32, _ proto.Message) error {
		pushed = append(pushed, pid)
		return nil
	}
	var launchedBattle uint64
	launchJianghuBattleFn = func(_ context.Context, _ *svc.ServiceContext, _ *matchpb.MatchJianghuDuelInvitationRecord, sideA, sideB []uint64, duelCtx *battlepb.JianghuDuelBattleContext) (uint64, error) {
		if len(sideA) != 1 || len(sideB) != 1 || duelCtx.GetRoundLimitOverride() != 30 {
			t.Fatalf("unexpected launch sides/context: %v vs %v ctx=%+v", sideA, sideB, duelCtx)
		}
		launchedBattle = 88001
		return launchedBattle, nil
	}

	store := newFakeJianghuStore()
	svcCtx := &svc.ServiceContext{
		JianghuStore: store,
		FriendClient: &fakeFriendClient{},
		BattleIDGen:  &seqIDGen{next: 100},
	}

	// 1. 发起 1V1 金帖战书
	createResp, err := NewJianghuLogic(withPlayerSession(1001), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_GOLD_ITEM,
	})
	if err != nil {
		t.Fatalf("CreateJianghuDuelInvite err: %v", err)
	}
	if createResp.GetErrorMessage().GetId() != 0 {
		t.Fatalf("unexpected error: %v", createResp.GetErrorMessage())
	}
	if createResp.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET {
		t.Fatalf("expected PENDING_TARGET, got %v", createResp.GetInvite().GetState())
	}

	// 2. 同目标 5 分钟冷却拦截二次下帖
	cdResp, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER,
	})
	if cdResp.GetErrorMessage().GetId() != constants.ErrJianghuSameTargetCooldown {
		t.Fatalf("expected cooldown error %d, got %d", constants.ErrJianghuSameTargetCooldown, cdResp.GetErrorMessage().GetId())
	}

	// 3. 目标应战 -> 直接成局并释放未成局计数
	respResp, err := NewJianghuLogic(withPlayerSession(2001), svcCtx).RespondJianghuDuelInvite(&matchpb.RespondJianghuDuelInviteRequest{
		DuelInviteId: createResp.GetInvite().GetDuelInviteId(),
		Accept:       true,
	})
	if err != nil || respResp.GetErrorMessage().GetId() != 0 {
		t.Fatalf("RespondJianghuDuelInvite failed: err=%v msg=%v", err, respResp.GetErrorMessage())
	}
	if respResp.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED || respResp.GetInvite().GetBattleId() != 88001 {
		t.Fatalf("expected MATCHED battle=88001, got state=%v battle=%d", respResp.GetInvite().GetState(), respResp.GetInvite().GetBattleId())
	}
	if store.dailyCount[1001].count != 0 {
		t.Fatalf("matched invite should decrement unmatched daily count to 0, got %d", store.dailyCount[1001].count)
	}
	if len(pushed) < 3 {
		t.Fatalf("expected invite + matched pushes, got %v", pushed)
	}
}

func TestJianghuDuel_3v3TwoStageTeamConfirmAndDeclineAbort(t *testing.T) {
	origNow := jianghuNowMsFn
	origAvail := checkJianghuPlayersAvailableFn
	origPush := pushJianghuToPlayerFn
	origLaunch := launchJianghuBattleFn
	defer func() {
		jianghuNowMsFn = origNow
		checkJianghuPlayersAvailableFn = origAvail
		pushJianghuToPlayerFn = origPush
		launchJianghuBattleFn = origLaunch
	}()

	now := uint64(1_730_000_000_000)
	jianghuNowMsFn = func() uint64 { return now }
	checkJianghuPlayersAvailableFn = func(*svc.ServiceContext, []uint64, []uint64) (uint32, string) { return 0, "" }
	pushJianghuToPlayerFn = func(context.Context, *svc.ServiceContext, uint64, uint32, proto.Message) error { return nil }
	launchCount := 0
	launchJianghuBattleFn = func(context.Context, *svc.ServiceContext, *matchpb.MatchJianghuDuelInvitationRecord, []uint64, []uint64, *battlepb.JianghuDuelBattleContext) (uint64, error) {
		launchCount++
		return 99001, nil
	}

	store := newFakeJianghuStore()
	store.setTeam(10, 1001, 1002, 1003)
	store.setTeam(20, 2001, 2002, 2003)
	svcCtx := &svc.ServiceContext{
		JianghuStore: store,
		FriendClient: &fakeFriendClient{},
		BattleIDGen:  &seqIDGen{next: 200},
	}

	// 1. 发起 3V3 -> 先进入发起方本队确认 AWAITING_CHALLENGER_TEAM
	cResp, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER,
	})
	if cResp.GetErrorMessage().GetId() != 0 || cResp.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_CHALLENGER_TEAM {
		t.Fatalf("expected AWAITING_CHALLENGER_TEAM, got err=%v state=%v", cResp.GetErrorMessage(), cResp.GetInvite().GetState())
	}
	invID := cResp.GetInvite().GetDuelInviteId()

	// 2. 1002 & 1003 确认 -> 自动流转至 PENDING_TARGET
	r1, _ := NewJianghuLogic(withPlayerSession(1002), svcCtx).ConfirmJianghuDuelMember(&matchpb.ConfirmJianghuDuelMemberRequest{DuelInviteId: invID, Agree: true})
	if r1.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_CHALLENGER_TEAM {
		t.Fatalf("should still wait for 1003, got %v", r1.GetInvite().GetState())
	}
	r2, _ := NewJianghuLogic(withPlayerSession(1003), svcCtx).ConfirmJianghuDuelMember(&matchpb.ConfirmJianghuDuelMemberRequest{DuelInviteId: invID, Agree: true})
	if r2.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET {
		t.Fatalf("expected PENDING_TARGET after side A confirms, got %v", r2.GetInvite().GetState())
	}

	// 3. 目标队长 2001 应战 -> 进入目标队 30s 确认 AWAITING_TARGET_TEAM（绝不直接开打）
	aResp, _ := NewJianghuLogic(withPlayerSession(2001), svcCtx).RespondJianghuDuelInvite(&matchpb.RespondJianghuDuelInviteRequest{DuelInviteId: invID, Accept: true})
	if aResp.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM || launchCount != 0 {
		t.Fatalf("expected AWAITING_TARGET_TEAM with 0 launches, got state=%v launches=%d", aResp.GetInvite().GetState(), launchCount)
	}

	// 4. 目标队 2002 拒绝 -> 整单 ABORTED（0 开局、0 扣费）
	dResp, _ := NewJianghuLogic(withPlayerSession(2002), svcCtx).ConfirmJianghuDuelMember(&matchpb.ConfirmJianghuDuelMemberRequest{DuelInviteId: invID, Agree: false})
	if dResp.GetInvite().GetState() != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED || launchCount != 0 {
		t.Fatalf("expected ABORTED on teammate decline, got state=%v launches=%d", dResp.GetInvite().GetState(), launchCount)
	}
}

func TestJianghuDuel_BlockRejectedAndStreakNoticesAndVoidSingleRefund(t *testing.T) {
	origNow := jianghuNowMsFn
	origAvail := checkJianghuPlayersAvailableFn
	defer func() {
		jianghuNowMsFn = origNow
		checkJianghuPlayersAvailableFn = origAvail
	}()
	jianghuNowMsFn = func() uint64 { return 1_730_000_000_000 }
	checkJianghuPlayersAvailableFn = func(*svc.ServiceContext, []uint64, []uint64) (uint32, string) { return 0, "" }

	store := newFakeJianghuStore()
	chat := &fakeChatClient{}
	mail := &fakeMailAdminClient{}
	svcCtx := &svc.ServiceContext{
		JianghuStore: store,
		FriendClient: &fakeFriendClient{hasBlock: true},
		ChatClient:   chat,
		MailAdmin:    mail,
		MailSecret:   "0123456789abcdef0123456789abcdef",
		BattleIDGen:  &seqIDGen{next: 300},
	}

	// 1. 拉黑直接拒接
	blkResp, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER,
	})
	if blkResp.GetErrorMessage().GetId() != constants.ErrJianghuBlockedByPeer {
		t.Fatalf("expected blocked error %d, got %d", constants.ErrJianghuBlockedByPeer, blkResp.GetErrorMessage().GetId())
	}

	// 2. 连胜 3 场触发高光播报，第 4 场对手翻盘触发终结连胜播报 + 一笔勾销后再交手自动重现
	makeResult := func(battleID uint64, outcome battlepb.EBattleOutcome, rounds uint32) *kafkapb.BattleResultEvent {
		return &kafkapb.BattleResultEvent{
			BattleId:     battleID,
			MatchMode:    uint32(matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL),
			Outcome:      outcome,
			TotalRounds:  rounds,
			FinishedAtMs: 1_730_000_000_000 + battleID,
			JianghuDuelContext: &battlepb.JianghuDuelBattleContext{
				DuelInviteId:          500 + battleID,
				WritTier:              uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER),
				ChallengerCaptainId:   1001,
				ChallengerCaptainName: "QingFeng",
				TargetCaptainId:       2001,
				TargetCaptainName:     "MoYuan",
				TeamSize:              1,
			},
		}
	}
	for i := uint64(1); i <= 3; i++ {
		out, err := ApplyBattleResult(svcCtx, makeResult(i, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, 6))
		if err != nil || out != ratingOutcomeApplied {
			t.Fatalf("battle %d apply failed: out=%s err=%v", i, out, err)
		}
	}
	// 第 3 胜触发 WIN_SILVER + STREAK_MILESTONE
	foundMilestone := false
	for _, n := range chat.notices {
		if n.GetTemplateId() == chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_MILESTONE && n.GetStreakCount() == 3 {
			foundMilestone = true
		}
	}
	if !foundMilestone {
		t.Fatalf("expected streak milestone notice at 3 wins, got %+v", chat.notices)
	}

	// 一笔勾销后 1001 看不到 2001，但 2001 仍看到 1001；再次交手(第 4 场 2001 胜) 1001 侧自动重现并触发 STREAK_BROKEN
	dResp, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).DismissJianghuGrudge(&matchpb.DismissJianghuGrudgeRequest{PeerPlayerId: 2001})
	if dResp.GetErrorMessage().GetId() != 0 {
		t.Fatalf("dismiss failed: %v", dResp.GetErrorMessage())
	}
	l1, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).ListJianghuGrudges(&matchpb.ListJianghuGrudgesRequest{})
	l2, _ := NewJianghuLogic(withPlayerSession(2001), svcCtx).ListJianghuGrudges(&matchpb.ListJianghuGrudgesRequest{})
	if len(l1.GetGrudges()) != 0 || len(l2.GetGrudges()) != 1 {
		t.Fatalf("one-way dismiss broken: l1=%d l2=%d", len(l1.GetGrudges()), len(l2.GetGrudges()))
	}

	out4, err := ApplyBattleResult(svcCtx, makeResult(4, battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN, 8))
	if err != nil || out4 != ratingOutcomeApplied {
		t.Fatalf("battle 4 apply failed: %s %v", out4, err)
	}
	l1After, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).ListJianghuGrudges(&matchpb.ListJianghuGrudgesRequest{})
	if len(l1After.GetGrudges()) != 1 || l1After.GetGrudges()[0].GetWins() != 3 || l1After.GetGrudges()[0].GetLosses() != 1 {
		t.Fatalf("expected rematch to revive grudge (3-1), got %+v", l1After.GetGrudges())
	}
	foundBreak := false
	for _, n := range chat.notices {
		if n.GetTemplateId() == chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_BROKEN && n.GetStreakCount() == 3 {
			foundBreak = true
		}
	}
	if !foundBreak {
		t.Fatalf("expected streak broken notice, got %+v", chat.notices)
	}

	// 3. 离线备战中止单次幂等邮件退 500 银两（CURRENCY_TYPE_GOLD=0）：发信失败清 marker 返回 false，重试成功仅发 1 封且不双退
	mail.failErr = context.DeadlineExceeded
	if ok := refundJianghuSilverOnce(context.Background(), svcCtx, 599, 99, 1001, uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER), 500, "gather_failed"); ok {
		t.Fatalf("failed mail send must return false and not block retry or scene online refund")
	}
	mail.failErr = nil
	if ok := refundJianghuSilverOnce(context.Background(), svcCtx, 599, 99, 1001, uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER), 500, "gather_failed"); !ok {
		t.Fatalf("expected retry refund mail to succeed")
	}
	if ok := refundJianghuSilverOnce(context.Background(), svcCtx, 599, 99, 1001, uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER), 500, "gather_failed"); !ok {
		t.Fatalf("duplicate refund should return true (already handled)")
	}
	if len(mail.sent) != 1 {
		t.Fatalf("expected exactly 1 refund mail, got %d", len(mail.sent))
	}
	cur := mail.sent[0].GetAttachments().GetCurrencies()
	if len(cur) != 1 || cur[0].GetCurrencyType() != uint32(base.CurrencyType_CURRENCY_TYPE_GOLD) || cur[0].GetAmount() != 500 {
		t.Fatalf("unexpected refund mail currency: %+v", cur)
	}
}

func TestJianghuDuel_Concurrent3v3ConfirmNoLostUpdateAndNonLeaderRejected(t *testing.T) {
	origNow := jianghuNowMsFn
	origAvail := checkJianghuPlayersAvailableFn
	origPush := pushJianghuToPlayerFn
	defer func() {
		jianghuNowMsFn = origNow
		checkJianghuPlayersAvailableFn = origAvail
		pushJianghuToPlayerFn = origPush
	}()
	jianghuNowMsFn = func() uint64 { return 1_730_000_000_000 }
	checkJianghuPlayersAvailableFn = func(*svc.ServiceContext, []uint64, []uint64) (uint32, string) { return 0, "" }
	pushJianghuToPlayerFn = func(context.Context, *svc.ServiceContext, uint64, uint32, proto.Message) error { return nil }

	store := newFakeJianghuStore()
	store.setTeam(10, 1001, 1002, 1003) // leader=1001
	store.setTeam(20, 2001, 2002, 2003) // leader=2001
	svcCtx := &svc.ServiceContext{
		JianghuStore: store,
		FriendClient: &fakeFriendClient{},
		BattleIDGen:  &seqIDGen{next: 400},
	}

	// 非队长 1002 发起小队战书 -> 拒绝
	nonLeaderResp, _ := NewJianghuLogic(withPlayerSession(1002), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER,
	})
	if nonLeaderResp.GetErrorMessage().GetId() != constants.ErrJianghuNotInviteParticipant {
		t.Fatalf("expected non-leader rejection %d, got %d", constants.ErrJianghuNotInviteParticipant, nonLeaderResp.GetErrorMessage().GetId())
	}

	// 队长 1001 发起 3V3 -> 1002 与 1003 并发确认，不得丢更新，最终进入 PENDING_TARGET
	cResp, _ := NewJianghuLogic(withPlayerSession(1001), svcCtx).CreateJianghuDuelInvite(&matchpb.CreateJianghuDuelInviteRequest{
		TargetPlayerId: 2001,
		WritTier:       matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER,
	})
	if cResp.GetErrorMessage().GetId() != 0 {
		t.Fatalf("create failed: %v", cResp.GetErrorMessage())
	}
	invID := cResp.GetInvite().GetDuelInviteId()

	var wg sync.WaitGroup
	wg.Add(2)
	for _, memberID := range []uint64{1002, 1003} {
		pid := memberID
		go func() {
			defer wg.Done()
			_, _ = NewJianghuLogic(withPlayerSession(pid), svcCtx).ConfirmJianghuDuelMember(&matchpb.ConfirmJianghuDuelMemberRequest{
				DuelInviteId: invID,
				Agree:        true,
			})
		}()
	}
	wg.Wait()

	after, err := store.GetJianghuDuelInvite(context.Background(), invID)
	if err != nil {
		t.Fatalf("get invite err: %v", err)
	}
	if after.State != uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET) {
		t.Fatalf("expected PENDING_TARGET after concurrent 3v3 confirms (no lost update), got state=%d", after.State)
	}
}
