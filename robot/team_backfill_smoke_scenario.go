package main

// team-backfill-smoke 场景:队伍补位匹配(docs/design/team-backfill-matching.md)的端到端冒烟 ——
// 三个机器人经 gate → 路由服 → match(进程内的组队服务 + 匹配)走完设计文档 §7.3 的联机验收:
//
//	P0 预备:三人各自离队 / 解散,并撤掉可能残留的排队(同账号可反复跑);
//	P1 A 建队 → B 申请 → A 同意;视图必须带能力位 match_fill_supported(否则 match 不是补位版本,或开关已关);
//	P2 队员取消 = 整队退出:A、B 先各自地域探测(network_probe.go)→ A 选「凑满 3 人」开战 → 回包 QUEUED(目标 3 人、入队时刻非 0);
//	   B 收到 MATCH_QUEUED;两人 GetQueueStatus 都是 QUEUED → B 带查到的票号取消 → A 收到 MATCH_QUEUE_CANCELED(actor=B)→ 两人都 NOT_QUEUED;
//	P3 名单变化 = 整队退出:A 再发起 → B 离队 → A 收到 MATCH_QUEUE_CANCELED(原因:队伍状态已变化)→ A NOT_QUEUED;
//	   B 重新入队;
//	P4 补位成组:A 再发起 → C 单排同一副本的 3 人档 → 三人收到同一 battle_id 的 BattleStart,直连自动战斗到终局;
//	   A、B 收到 MATCH_ENDED(队伍面板从「匹配中」归位);
//	P5 A 解散,B 收到 DISBANDED。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `TEAM_BACKFILL_SMOKE_OK team_id=… battle_id=… player_a=… player_b=… player_c=…`,退出码 0;
//	任一步失败 → `TEAM_BACKFILL_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// 复用 team_smoke_scenario.go 的 teamSmokeBot:登录进场、经 smokeCaller 按请求号认领组队回包、推送按到达顺序留底(每步在触发动作
// 之前 mark,之后从 mark 起扫描)、战斗直连与自动战斗。排队请求(JoinQueue / CancelQueue / GetQueueStatus)
// 同样经 smokeCaller 按请求号认领回包(teamSmokeBot.matchCall),不进留底。

import (
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/common/base"
	"proto/match"
	teampb "proto/team"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth;93xx 里 9301–9306 已被 team-smoke / team-follow-smoke 占用。
const (
	backfillSmokeAccountA = "robot_9321" // 队长
	backfillSmokeAccountB = "robot_9322" // 队员
	backfillSmokeAccountC = "robot_9323" // 路人(单排补位)
)

const (
	// 目标人数固定 3:2 人队 + 1 个路人。PVE 人数档队列按 battle_config_id 的人数上限分档,上限必须 ≥ 3。
	backfillSmokeTargetSize = 3
	// 名单变化的撤单是提交之后异步执行的(cancelPartyOnRosterChange),队长看到「已不在排」最多再等这么久。
	backfillSmokeAsyncCancelTimeout = 10 * time.Second
)

var tipTeamStateChanged = uint32(tiptable.TeamError_kTeamStateChanged)

// RunTeamBackfillSmoke 是 main.go `mode: team-backfill-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunTeamBackfillSmoke(cfg *config.Config) {
	loginTestCfg = cfg // loginAndEnterScenario 从这个包级变量读认证方式

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.TeamBackfillSmoke

	var bots []*teamSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("TEAM_BACKFILL_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录(并发) ----
	specs := []struct{ name, account string }{
		{"A", backfillSmokeAccountA}, {"B", backfillSmokeAccountB}, {"C", backfillSmokeAccountC},
	}
	loggedIn := make([]*teamSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, name, account string) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = sc.Zone
			loggedIn[idx], loginErrs[idx] = teamSmokeLogin(&zoneCfg, name, account, stats)
		}(i, spec.name, spec.account)
	}
	wg.Wait()
	for _, bot := range loggedIn {
		if bot != nil {
			bots = append(bots, bot)
		}
	}
	for i, err := range loginErrs {
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", specs[i].account, sc.Zone, err)
		}
	}
	a, b, c := loggedIn[0], loggedIn[1], loggedIn[2]
	zap.L().Info("[team-backfill-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Uint64("c", c.gc.PlayerId),
		zap.Uint32("zone", sc.Zone), zap.Uint32("battle_config_id", sc.BattleConfigId))

	// ---- P0:不在任何队伍、不在任何队列 ----
	for _, bot := range bots {
		if err := bot.leaveAnyTeam(); err != nil {
			fail("P0-leave-team", "account=%s: %v", bot.account, err)
		}
		if err := backfillCancelQueue(bot); err != nil {
			fail("P0-cancel-queue", "account=%s: %v", bot.account, err)
		}
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, 0); err != nil {
			fail("P0-not-queued", "%v(上一轮可能停在开局途中,票据短 TTL 过期后再跑)", err)
		}
	}

	// ---- P1:A 建队 → B 入队;能力位 ----
	createResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamCreateTeamMessageId, &teampb.CreateTeamRequest{}, createResp, 0); err != nil {
		fail("P1-create", "%v", err)
	}
	tid := createResp.GetTeam().GetTeamId()
	if tid == 0 {
		fail("P1-create", "Team creation was accepted but the view has team_id=0")
	}
	view, err := backfillJoin(a, b, tid)
	if err != nil {
		fail("P1-join", "%v", err)
	}
	if !view.GetMatchFillSupported() {
		fail("P1-capability", "Team view has match_fill_supported=false: the running match is not the backfill build, "+
			"or Team.FillQueueDisabled is true (see team-backfill-matching.md §4.6)")
	}
	zap.L().Info("[team-backfill-smoke] P1: team ready", zap.Uint64("team_id", tid), zap.Int("members", len(view.GetMembers())))

	// ---- P2:进队列 → 队员取消 = 整队退出 ----
	party := []*teamSmokeBot{a, b}
	bMark := b.mark()
	queued, err := backfillStartQueue(a, party, tid, sc.BattleConfigId)
	if err != nil {
		fail("P2-start", "%v", err)
	}
	if _, err := b.waitSnapshot(bMark, teamSmokePushTimeout, "B MATCH_QUEUED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUED && s.GetTeam().GetTeamId() == tid &&
			s.GetTeam().GetMatchState() == teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED &&
			s.GetTeam().GetMatchQueuedAtMs() == queued.GetMatchQueuedAtMs()
	}); err != nil {
		fail("P2-queued-push", "%v", err)
	}
	for _, bot := range party {
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_QUEUED, 0); err != nil {
			fail("P2-status", "%v", err)
		}
	}
	aMark := a.mark()
	if err := backfillCancelQueue(b); err != nil {
		fail("P2-cancel", "%v", err)
	}
	canceled, err := a.waitSnapshot(aMark, teamSmokePushTimeout, "A MATCH_QUEUE_CANCELED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUE_CANCELED && s.GetTeam().GetTeamId() == tid
	})
	if err != nil {
		fail("P2-canceled-push", "%v", err)
	}
	if canceled.GetActorId() != b.gc.PlayerId {
		fail("P2-canceled-push", "MATCH_QUEUE_CANCELED actor_id=%d, expected the canceller B=%d", canceled.GetActorId(), b.gc.PlayerId)
	}
	if canceled.GetTeam().GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE {
		fail("P2-canceled-push", "MATCH_QUEUE_CANCELED view match_state=%s, expected IDLE", canceled.GetTeam().GetMatchState())
	}
	for _, bot := range party {
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, 0); err != nil {
			fail("P2-not-queued", "%v", err)
		}
	}
	zap.L().Info("[team-backfill-smoke] P2: member cancel withdrew the whole party")

	// ---- P3:进队列 → 名单变化 = 整队退出 ----
	if _, err := backfillStartQueue(a, party, tid, sc.BattleConfigId); err != nil {
		fail("P3-start", "%v", err)
	}
	aMark = a.mark()
	if err := b.expect(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		fail("P3-leave", "%v", err)
	}
	rosterCanceled, err := a.waitSnapshot(aMark, teamSmokePushTimeout, "A MATCH_QUEUE_CANCELED after roster change", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUE_CANCELED && s.GetTeam().GetTeamId() == tid
	})
	if err != nil {
		fail("P3-canceled-push", "%v", err)
	}
	if got := rosterCanceled.GetTip().GetId(); got != tipTeamStateChanged {
		fail("P3-canceled-push", "MATCH_QUEUE_CANCELED tip=%d, expected TeamStateChanged=%d", got, tipTeamStateChanged)
	}
	for _, bot := range party {
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, backfillSmokeAsyncCancelTimeout); err != nil {
			fail("P3-not-queued", "%v", err)
		}
	}
	if _, err := backfillJoin(a, b, tid); err != nil {
		fail("P3-rejoin", "%v", err)
	}
	zap.L().Info("[team-backfill-smoke] P3: roster change withdrew the whole party")

	// ---- P4:补位成组、同场开战、打到终局 ----
	fighters := []*teamSmokeBot{a, b, c}
	marks := make([]int, len(fighters))
	for i, bot := range fighters {
		marks[i] = bot.mark()
	}
	// C 的探测放在 A 发起之前:A 一进队列,补位的票就该跟上,不让探测夹在两者之间。
	cPlanId, err := c.networkProbe()
	if err != nil {
		fail("P4-network-probe", "%v", err)
	}
	if _, err := backfillStartQueue(a, party, tid, sc.BattleConfigId); err != nil {
		fail("P4-start", "%v", err)
	}
	// C 的推送扫描起点 marks[2] 已在 A 发起之前记下:成组后的 BattleAssigned / BattleStart 可能先于 JoinQueue 回包到达。
	joinResp := &match.JoinQueueResponse{}
	if err := c.matchCall(game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
		PlayerId:       c.gc.PlayerId,
		Mode:           match.MatchMode_MATCH_MODE_PVE_TEAM,
		BattleConfigId: sc.BattleConfigId,
		TeamSize:       backfillSmokeTargetSize,
		ZoneId:         sc.Zone,
		RequestId:      chatSmokeRequestId(),
		NetworkPlanId:  cPlanId,
	}, joinResp); err != nil {
		fail("P4-solo-join", "%v", err)
	}
	if joinResp.GetErrorCode() != 0 || joinResp.GetErrorMessage().GetId() != 0 {
		fail("P4-solo-join", "C JoinQueue(PVE_TEAM, team_size=%d) rejected error_code=%d tip=%d",
			backfillSmokeTargetSize, joinResp.GetErrorCode(), joinResp.GetErrorMessage().GetId())
	}

	type fightResult struct {
		battleId uint64
		turns    int
		err      error
	}
	results := make([]fightResult, len(fighters))
	for i, bot := range fighters {
		wg.Add(1)
		go func(idx int, bot *teamSmokeBot) {
			defer wg.Done()
			start, err := bot.waitBattleStart(marks[idx])
			if err != nil {
				results[idx].err = err
				return
			}
			results[idx].battleId = start.GetBattleId()
			direct, err := bot.connectBattle(marks[idx], start.GetBattleId())
			if err != nil {
				results[idx].err = err
				return
			}
			defer direct.Close()
			results[idx].turns, results[idx].err = bot.finishBattleOn(direct, marks[idx], start.GetBattleId())
		}(i, bot)
	}
	wg.Wait()
	battleId := uint64(0)
	for i, result := range results {
		if result.err != nil {
			fail("P4-battle", "%v", result.err)
		}
		if i == 0 {
			battleId = result.battleId
			continue
		}
		if result.battleId != battleId {
			fail("P4-same-battle", "%s battle_id=%d differs from A battle_id=%d: the party and the stranger did not enter the same battle",
				fighters[i].name, result.battleId, battleId)
		}
	}
	for i, bot := range party {
		if _, err := bot.waitSnapshot(marks[i], teamSmokePushTimeout, bot.name+" MATCH_ENDED", func(s *teampb.TeamSnapshotS2C) bool {
			return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED && s.GetTeam().GetTeamId() == tid
		}); err != nil {
			fail("P4-match-ended", "%v", err)
		}
	}
	zap.L().Info("[team-backfill-smoke] P4: party + stranger filled the queue and fought one battle",
		zap.Uint64("battle_id", battleId), zap.Int("turns_a", results[0].turns))

	// ---- P5:解散 ----
	bMark = b.mark()
	if err := a.expect(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		fail("P5-disband", "%v", err)
	}
	if _, err := b.waitSnapshot(bMark, teamSmokePushTimeout, "B DISBANDED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_DISBANDED && s.GetTeam().GetTeamId() == 0
	}); err != nil {
		fail("P5-disband-push", "%v", err)
	}

	zap.L().Info(fmt.Sprintf("TEAM_BACKFILL_SMOKE_OK team_id=%d battle_id=%d player_a=%d player_b=%d player_c=%d",
		tid, battleId, a.gc.PlayerId, b.gc.PlayerId, c.gc.PlayerId))
	cleanup()
	_ = zap.L().Sync()
}

// backfillJoin:member 申请加入 leader 的队伍、leader 同意,返回同意回包里的视图。
func backfillJoin(leader, member *teamSmokeBot, tid uint64) (*teampb.TeamView, error) {
	if err := member.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
		&teampb.ApplyJoinTeamRequest{TargetPlayerId: leader.gc.PlayerId}, &teampb.TeamResponse{}, 0); err != nil {
		return nil, fmt.Errorf("%s apply: %w", member.name, err)
	}
	resp := &teampb.TeamResponse{}
	if err := leader.expect(game.ClientPlayerTeamHandleApplicationMessageId,
		&teampb.HandleApplicationRequest{ApplicantId: member.gc.PlayerId, Approve: true, ExpectedTeamId: tid}, resp, 0); err != nil {
		return nil, fmt.Errorf("%s approve: %w", leader.name, err)
	}
	if _, n := teamSmokeMemberOf(resp.GetTeam(), member.gc.PlayerId); n != 1 {
		return nil, fmt.Errorf("after approval %s appears %d times in the view", member.name, n)
	}
	return resp.GetTeam(), nil
}

// backfillStartQueue:队长选「凑满 3 人」发起,断言回包是 QUEUED(目标人数、副本、入队时刻)。party 是当前队伍全员(队长在内):
// 进队列要读每名成员自己的探测结果,发起之前都要探测过。一次调用是一次发起意图,request_id 只生成一次;
// 上一场结算落地前的过渡态(队员仍持 battle:lock、ready 票据未清)在 teamSmokeSettleTimeout 内重试,重试复用同一份请求。
func backfillStartQueue(leader *teamSmokeBot, party []*teamSmokeBot, tid uint64, battleConfigId uint32) (*teampb.TeamView, error) {
	if err := teamSmokeEnsureNetworkProbes(party...); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	request := &teampb.StartTeamMatchRequest{BattleConfigId: battleConfigId, ExpectedTeamId: tid, TargetTeamSize: backfillSmokeTargetSize, RequestId: chatSmokeRequestId()}
	for {
		resp := &teampb.TeamResponse{}
		tip, err := leader.rpc(game.ClientPlayerTeamStartTeamMatchMessageId, request, resp)
		if err != nil {
			return nil, err
		}
		if tip == 0 {
			view := resp.GetTeam()
			if view.GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED {
				return nil, fmt.Errorf("StartTeamMatch(target=%d) accepted but match_state=%s, expected QUEUED "+
					"(a server that does not support backfill silently starts a direct battle)", backfillSmokeTargetSize, view.GetMatchState())
			}
			if view.GetMatchTargetSize() != backfillSmokeTargetSize || view.GetMatchBattleConfigId() != battleConfigId || view.GetMatchQueuedAtMs() == 0 {
				return nil, fmt.Errorf("QUEUED view target=%d config=%d queued_at_ms=%d, expected target=%d config=%d queued_at_ms>0",
					view.GetMatchTargetSize(), view.GetMatchBattleConfigId(), view.GetMatchQueuedAtMs(), backfillSmokeTargetSize, battleConfigId)
			}
			return view, nil
		}
		transient := tip == tipTeamMemberInBattle || tip == tipTeamMemberNotReady
		if !transient || !time.Now().Before(deadline) {
			return nil, fmt.Errorf("StartTeamMatch(battle_config_id=%d, target=%d) rejected tip=%d parameters=%v",
				battleConfigId, backfillSmokeTargetSize, tip, resp.GetErrorMessage().GetParameters())
		}
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}

// backfillCancelQueue 撤掉 bot 当前的排队:先 GetQueueStatus 拿票号,再带票号发 CancelQueue,等回包;持补位票时撤整单。
// 撤销必须带原请求编号或票号(两个都空服务端回 InvalidArgument);队员手里没有队长那次发起的 request_id,票号是他能拿到的凭据。
// 查不到票号说明不在排,什么也不发。响应是 Empty:经路由服 / 直连 gRPC 时 gate 照样回一个带请求号的包(见 matchCall 注释)。
func backfillCancelQueue(bot *teamSmokeBot) error {
	status := &match.GetQueueStatusResponse{}
	if err := bot.matchCall(game.MatchServiceGetQueueStatusMessageId, &match.GetQueueStatusRequest{PlayerId: bot.gc.PlayerId}, status); err != nil {
		return fmt.Errorf("%s GetQueueStatus before cancel: %w", bot.name, err)
	}
	if status.GetQueueTicket() == "" {
		return nil
	}
	cancel := &match.CancelQueueRequest{PlayerId: bot.gc.PlayerId, QueueTicket: status.GetQueueTicket()}
	if err := bot.matchCall(game.MatchServiceCancelQueueMessageId, cancel, &base.Empty{}); err != nil {
		return fmt.Errorf("%s CancelQueue: %w", bot.name, err)
	}
	return nil
}

// backfillExpectQueueState 用 GetQueueStatus 断言排队票据的状态;wait>0 时在这段时间内重试直到相符
// (名单变化的撤单是异步的)。
func backfillExpectQueueState(bot *teamSmokeBot, want match.QueueState, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		resp := &match.GetQueueStatusResponse{}
		if err := bot.matchCall(game.MatchServiceGetQueueStatusMessageId, &match.GetQueueStatusRequest{PlayerId: bot.gc.PlayerId}, resp); err != nil {
			return fmt.Errorf("%s GetQueueStatus: %w", bot.name, err)
		}
		if resp.GetState() == want {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s queue state=%s, expected %s", bot.name, resp.GetState(), want)
		}
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}
