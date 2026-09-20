package main

// team-smoke 场景:四个机器人经 gate → client_rpc_router → TeamNodeService(go/match 进程内 internal/team)
// 对组队系统做端到端冒烟(docs/design/team-system.md §I.5):
//
//	S0 预备:四人各自 GetMyTeam,在队就解散(队长)或离队(队员)—— 同账号可反复跑;
//	S1 A 建队(队伍 zone == zone_a)→ B 申请 → A 拉取看到申请 → A 同意;B 收到 MEMBER_JOINED 推送,
//	   A 的回包与 B 的推送 version 递增;
//	S2 A 重放同一条"同意"→ 受理,成员仍是 2 人、B 只出现一次且 join_seq 不变(§D.6 重试安全);
//	S3 A 邀请 D → D 的 ListMyInvites 能看到 → D 拒绝(A 收 INVITE_CHANGED)→ A 再邀请 → D 接受;
//	S4 A 踢 D(D 收到 team_id=0 的 MEMBER_KICKED 视图,membership_epoch 变大)→ A 转让给 B → B 转回 A;
//	S5 D 申请 → A 拒绝 → D 收到 NotifyTeamEvent(APPLICATION_REJECTED);
//	S6 跟随:A 换到同节点另一张地图(EnterSceneC2S,见 teamSmokeSwitchScene 注释)→ B 在 10s 内收到
//	   scene_id 与 A 相同的 EnterSceneS2C;
//	S7 整队开战:A 调 StartTeamMatch(回包 match_state=STARTING)→ B 收到 MATCH_STARTED(发起人 A 不收,
//	   以回包为准)→ A、B 收到同一 battle_id 的 BattleStartS2C,自动战斗到终局;A、B 都收到 MATCH_ENDED
//	   (gather 开局成功、开战锁释放后即推,通常早于战斗结束,按 mark 扫描不会漏);
//	S8 确定性拒绝:B 单人 PVE 开战、尚未出招(持 battle:lock)时 A 发起开战 → TeamMemberInBattle,parameters[0] == B;
//	X1(cross_zone=true 且 expect_cross_zone_allowed=false)C 申请 A 的队伍、A 邀请 C → 都回 TeamCrossZoneDenied;
//	X2(cross_zone=true 且 expect_cross_zone_allowed=true)C 申请 → A 同意,视图里 C 的 zone_id == zone_b;
//	   A 换图时 C 不跟随;B 离队后 A+C 跨 zone 整队开战成功;
//	S9 A 解散,仍在队的其他成员收到 team_id=0 的 DISBANDED 视图。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `TEAM_SMOKE_OK team_id=… zone_a=… player_a=… player_b=… player_d=… player_c=…`,退出码 0;
//	任一步失败 → `TEAM_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// 与 guild-smoke 的同与不同:
//   - 请求/回包:同 guild_smoke_scenario.go,按消息号在本场景的 RecvLoop 回调里认领回包,信封拒绝与业务 tip 分开断言;
//   - 推送:组队、换场景、战斗推送都按到达顺序留底(pushes),每步在触发动作**之前**记下标 mark,
//     之后从 mark 起扫描 —— 推送先于等待到达也不会漏,上一步的推送也不会被这一步误认;
//   - 战斗:不复用 gameobject.Player 的 WaitBattleStart/WaitBattleEnd。那两组信号是 sync.Once 一次性广播,
//     而本场景同一会话要打多场(S7、S8、X2),第二场起会直接读到上一场的旧信号。也因此不开战斗直连
//     (openBattleDirectConn 依赖同一组一次性信号),战斗消息全程经 gate 中继(D23 回落路径,
//     battle-smoke 的 skip_direct_connect=true 已覆盖其完整性)。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	"proto/common/base"
	"proto/match"
	"proto/scene"
	teampb "proto/team"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
// 93xx 与 battle-smoke(9001/9002)、跨 zone 匹配(9003/9004)、chat-smoke(9005/9006)、属性(9101)、
// 宠物(9102)、guild-smoke(9201-9203)、trade-smoke(9401-9403)错开,避免并行冒烟互相顶号。
// 必须是**首次在各自 zone 建角**的账号:A/B/D 的 home zone 要是 zone_a、C 的要是 zone_b。
const (
	teamSmokeAccountA = "robot_9301" // 队长
	teamSmokeAccountB = "robot_9302" // 同区队员(申请入队、被跟随、参战)
	teamSmokeAccountD = "robot_9303" // 同区玩家(被邀请、被踢、申请被拒)
	teamSmokeAccountC = "robot_9304" // 别区玩家(cross_zone=true 才登录)
)

const (
	// 单次组队 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s),先看到路由服的信封拒绝而不是本地超时。
	teamSmokeRpcTimeout = 10 * time.Second
	// 等进场的预算,与 prepareBehaviorClient 同值。
	teamSmokeSceneReadyTimeout = 15 * time.Second
	// 等组队推送的预算。推送是提交后异步经 Kafka gate-cmd 下发(§G.3,整批预算 3s),10s 足够本地环境。
	teamSmokePushTimeout = 10 * time.Second
	// S6 跟随判定窗口(§I.5 写死 10s);也用作换图自身等 EnterSceneS2C 的预算。
	teamSmokeFollowTimeout = 10 * time.Second
	// X2 "C 不跟随"的观察窗口:同节点跟随在本地通常 1s 内完成,8s 内没有 EnterSceneS2C 视为未跟随。
	teamSmokeNoFollowWindow = 8 * time.Second
	// 开战预算:匹配 gather + battle 建房,与 battle-smoke 同值。
	teamSmokeBattleStartTimeout = 30 * time.Second
	// 终局预算:覆盖自动战斗打满回合上限的最坏情况,与 battle-smoke 同值。
	teamSmokeBattleEndTimeout = 120 * time.Second
	// 上一场战斗收尾的过渡态(结算落地前 battle:lock 仍在、ready 票据未清)的等待上限。
	// 过渡态有界:结算经 battle → Kafka → scene 应用,本地秒级;超过这个时间按真实故障失败。
	teamSmokeSettleTimeout = 20 * time.Second
	// 过渡态重试间隔。
	teamSmokeSettleRetryInterval = time.Second
	// 同一机器人相邻请求的最小间隔。team 消息号在 MessageLimiter 表里是每秒 3~5 次(J-19);
	// 冒烟不追求速度,400ms 远离上限。表没导出时吃默认 3 次 / 窗口,可能被信封拒绝(错误里会写明)。
	teamSmokeRequestSpacing = 400 * time.Millisecond
	// 推送扫描的轮询粒度。
	teamSmokePollInterval = 50 * time.Millisecond
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipTeamHomeZoneUnknown    = uint32(tiptable.TeamError_kTeamHomeZoneUnknown)
	tipTeamCrossZoneDenied    = uint32(tiptable.TeamError_kTeamCrossZoneDenied)
	tipTeamMemberInBattle     = uint32(tiptable.TeamError_kTeamMemberInBattle)
	tipTeamMemberNotReady     = uint32(tiptable.TeamError_kTeamMemberNotReady)
	tipTeamInMatch            = uint32(tiptable.TeamError_kTeamInMatch)
	tipTeamServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
)

var errTeamSmokeTimeout = errors.New("等待超时")

// teamSmokeResponse 是组队响应的共同形状:业务拒绝码在响应体 error_message 里。
type teamSmokeResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// teamSmokePush 是一条留底的推送(或非组队请求的回包,如 JoinQueue / EnterSceneC2S)。
type teamSmokePush struct {
	messageId   uint32
	envelopeTip uint32 // gate / 路由服信封拒绝(body 为空);推送恒为 0
	body        []byte
}

// teamSmokeBot 是一个已登录进场的机器人会话。
type teamSmokeBot struct {
	name    string // A/B/C/D,只用于日志
	account string
	zone    uint32
	gate    string
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// RecvLoop goroutine 写、场景流程读,mu 保护。
	mu       sync.Mutex
	replies  map[uint32]*base.MessageContent // 组队请求消息号 → 本次请求发出后收到的第一个回包
	gateTips []uint32                        // 本次请求期间收到的 SendTipToClient
	pushes   []teamSmokePush                 // 本会话留底的推送,按到达顺序;只追加

	// 只由"当前驱动这个机器人的那一个 goroutine"读写(主流程,或战斗期间该机器人自己的参战 goroutine)。
	lastRequestAt time.Time
}

// RunTeamSmoke 是 main.go `mode: team-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunTeamSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.TeamSmoke

	type loginSpec struct {
		name    string
		account string
		zone    uint32
	}
	specs := []loginSpec{
		{"A", teamSmokeAccountA, sc.ZoneA},
		{"B", teamSmokeAccountB, sc.ZoneA},
		{"D", teamSmokeAccountD, sc.ZoneA},
	}
	if sc.CrossZone {
		specs = append(specs, loginSpec{"C", teamSmokeAccountC, sc.ZoneB})
	}

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
		zap.L().Error(fmt.Sprintf("TEAM_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录(并发) ----
	loggedIn := make([]*teamSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = spec.zone
			loggedIn[idx], loginErrs[idx] = teamSmokeLogin(&zoneCfg, spec.name, spec.account, stats)
		}(i, spec)
	}
	wg.Wait()
	// 先收齐已登录的会话再判错:否则某人失败时其他人的会话不会被 cleanup 收尾。
	for _, bot := range loggedIn {
		if bot != nil {
			bots = append(bots, bot)
		}
	}
	for i, err := range loginErrs {
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", specs[i].account, specs[i].zone, err)
		}
	}
	a, b, d := loggedIn[0], loggedIn[1], loggedIn[2]
	var c *teamSmokeBot
	if sc.CrossZone {
		c = loggedIn[3]
		// 本地各 zone 的 gate 端口不同;地址相同说明 C 其实进了 zone_a,跨区结论不成立。
		if c.gate == a.gate {
			fail("zone-placement", "C 与 A 分到同一个 gate %s(zone_a=%d zone_b=%d),不是跨 zone 运行", a.gate, sc.ZoneA, sc.ZoneB)
		}
	}
	botByPlayer := make(map[uint64]*teamSmokeBot, len(bots))
	for _, bot := range bots {
		botByPlayer[bot.gc.PlayerId] = bot
	}
	zap.L().Info("[team-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Uint64("d", d.gc.PlayerId),
		zap.Bool("cross_zone", sc.CrossZone), zap.Bool("expect_cross_zone_allowed", sc.ExpectCrossZoneAllowed))

	// ---- S0:回到"不在任何队伍" ----
	for _, bot := range bots {
		if err := bot.leaveAnyTeam(); err != nil {
			fail("S0-prepare", "account=%s: %v", bot.account, err)
		}
	}

	// ---- S1:A 建队 → B 申请 → A 同意 ----
	createResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamCreateTeamMessageId, &teampb.CreateTeamRequest{}, createResp, 0); err != nil {
		fail("S1-create", "%v", err)
	}
	created := createResp.GetTeam()
	tid := created.GetTeamId()
	if tid == 0 {
		fail("S1-create", "建队受理但视图 team_id=0")
	}
	if created.GetLeaderId() != a.gc.PlayerId || len(created.GetMembers()) != 1 {
		fail("S1-create", "leader_id=%d 成员数=%d,期望 A=%d 是唯一成员且为队长", created.GetLeaderId(), len(created.GetMembers()), a.gc.PlayerId)
	}
	if created.GetZoneId() != sc.ZoneA {
		fail("S1-create-zone", "队伍 zone_id=%d,期望建队者 A 的 home zone %d(账号须首次在 zone_a 建角)", created.GetZoneId(), sc.ZoneA)
	}

	if err := b.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
		&teampb.ApplyJoinTeamRequest{TargetPlayerId: a.gc.PlayerId}, &teampb.TeamResponse{}, 0); err != nil {
		fail("S1-apply", "%v", err)
	}
	// 申请是否落进记录用拉取断言(确定性);队长的 APPLICATION_CHANGED 推送不在本步断言范围。
	afterApply, err := a.myTeam()
	if err != nil {
		fail("S1-apply-verify", "%v", err)
	}
	if !teamSmokeHasApplication(afterApply, b.gc.PlayerId) {
		fail("S1-apply-verify", "A 的视图里没有 B=%d 的申请(applications=%d)", b.gc.PlayerId, len(afterApply.GetApplications()))
	}
	if afterApply.GetVersion() <= created.GetVersion() {
		fail("S1-apply-version", "申请后 version=%d,期望大于建队时的 %d", afterApply.GetVersion(), created.GetVersion())
	}

	bMark := b.mark()
	approveResp := &teampb.TeamResponse{}
	approveReq := &teampb.HandleApplicationRequest{ApplicantId: b.gc.PlayerId, Approve: true, ExpectedTeamId: tid}
	if err := a.expect(game.ClientPlayerTeamHandleApplicationMessageId, approveReq, approveResp, 0); err != nil {
		fail("S1-approve", "%v", err)
	}
	approved := approveResp.GetTeam()
	bMember, bCount := teamSmokeMemberOf(approved, b.gc.PlayerId)
	if bCount != 1 || len(approved.GetMembers()) != 2 {
		fail("S1-approve", "同意后成员数=%d、B 出现 %d 次,期望 2 人且 B 恰好一次", len(approved.GetMembers()), bCount)
	}
	if approved.GetVersion() <= afterApply.GetVersion() {
		fail("S1-approve-version", "同意后 version=%d,期望大于申请后的 %d", approved.GetVersion(), afterApply.GetVersion())
	}
	joined, err := b.waitSnapshot(bMark, teamSmokePushTimeout, "B MEMBER_JOINED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_JOINED && s.GetTeam().GetTeamId() == tid
	})
	if err != nil {
		fail("S1-joined-push", "%v", err)
	}
	if joined.GetTeam().GetVersion() < approved.GetVersion() {
		fail("S1-joined-push", "B 收到的视图 version=%d,低于 A 同意回包的 %d", joined.GetTeam().GetVersion(), approved.GetVersion())
	}
	if _, n := teamSmokeMemberOf(joined.GetTeam(), b.gc.PlayerId); n != 1 {
		fail("S1-joined-push", "B 收到的 MEMBER_JOINED 视图里自己出现 %d 次", n)
	}
	zap.L().Info("[team-smoke] S1: team created and B joined",
		zap.Uint64("team_id", tid), zap.Uint64("version", approved.GetVersion()),
		zap.Uint64("b_epoch", joined.GetTeam().GetMembershipEpoch()))

	// ---- S2:重放"同意",状态只变化一次 ----
	replayResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamHandleApplicationMessageId, approveReq, replayResp, 0); err != nil {
		fail("S2-replay", "%v", err)
	}
	replayed := replayResp.GetTeam()
	replayMember, replayCount := teamSmokeMemberOf(replayed, b.gc.PlayerId)
	if replayCount != 1 || len(replayed.GetMembers()) != 2 || replayMember.GetJoinSeq() != bMember.GetJoinSeq() {
		fail("S2-replay", "重放后成员数=%d、B 出现 %d 次、B join_seq=%d(首次 %d),期望与首次同意完全一致",
			len(replayed.GetMembers()), replayCount, replayMember.GetJoinSeq(), bMember.GetJoinSeq())
	}
	// version 只记日志:无变化的幂等成功是否仍产生一次提交由实现决定,"状态只变化一次"以成员集合与 join_seq 为准。
	zap.L().Info("[team-smoke] S2: replayed approve is idempotent",
		zap.Uint64("version_before", approved.GetVersion()), zap.Uint64("version_after", replayed.GetVersion()))

	// ---- S3:邀请 D → D 拒绝 → 再邀请 → D 接受 ----
	inviteReq := &teampb.InviteToTeamRequest{TargetPlayerId: d.gc.PlayerId, ExpectedTeamId: tid}
	inviteResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamInviteToTeamMessageId, inviteReq, inviteResp, 0); err != nil {
		fail("S3-invite", "%v", err)
	}
	if !teamSmokeHasPendingInvite(inviteResp.GetTeam(), d.gc.PlayerId) {
		fail("S3-invite", "A 的回包视图里没有发给 D=%d 的待处理邀请", d.gc.PlayerId)
	}
	invites := &teampb.ListMyInvitesResponse{}
	if err := d.expect(game.ClientPlayerTeamListMyInvitesMessageId, &teampb.ListMyInvitesRequest{}, invites, 0); err != nil {
		fail("S3-list-invites", "%v", err)
	}
	if !teamSmokeHasIncomingInvite(invites, tid) {
		fail("S3-list-invites", "D 的邀请列表(%d 条)里没有队伍 %d", len(invites.GetInvites()), tid)
	}
	aMark := a.mark()
	if err := d.expect(game.ClientPlayerTeamRespondInviteMessageId,
		&teampb.RespondInviteRequest{TeamId: tid, Accept: false}, &teampb.TeamResponse{}, 0); err != nil {
		fail("S3-decline", "%v", err)
	}
	if _, err := a.waitSnapshot(aMark, teamSmokePushTimeout, "A INVITE_CHANGED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_INVITE_CHANGED &&
			s.GetTeam().GetTeamId() == tid && !teamSmokeHasPendingInvite(s.GetTeam(), d.gc.PlayerId)
	}); err != nil {
		fail("S3-decline-push", "%v", err)
	}
	if err := a.expect(game.ClientPlayerTeamInviteToTeamMessageId, inviteReq, &teampb.TeamResponse{}, 0); err != nil {
		fail("S3-reinvite", "%v", err)
	}
	acceptResp := &teampb.TeamResponse{}
	if err := d.expect(game.ClientPlayerTeamRespondInviteMessageId,
		&teampb.RespondInviteRequest{TeamId: tid, Accept: true}, acceptResp, 0); err != nil {
		fail("S3-accept", "%v", err)
	}
	accepted := acceptResp.GetTeam()
	if _, n := teamSmokeMemberOf(accepted, d.gc.PlayerId); accepted.GetTeamId() != tid || n != 1 || len(accepted.GetMembers()) != 3 {
		fail("S3-accept", "D 接受后视图 team_id=%d 成员数=%d D 出现 %d 次,期望在队伍 %d 且共 3 人",
			accepted.GetTeamId(), len(accepted.GetMembers()), n, tid)
	}
	dEpochInTeam := accepted.GetMembershipEpoch()
	zap.L().Info("[team-smoke] S3: D declined then accepted the invite", zap.Uint64("d_epoch", dEpochInTeam))

	// ---- S4:踢 D;转让给 B 再转回 A ----
	dMark := d.mark()
	kickResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamKickMemberMessageId,
		&teampb.KickMemberRequest{TargetPlayerId: d.gc.PlayerId, ExpectedTeamId: tid}, kickResp, 0); err != nil {
		fail("S4-kick", "%v", err)
	}
	if _, n := teamSmokeMemberOf(kickResp.GetTeam(), d.gc.PlayerId); n != 0 {
		fail("S4-kick", "踢人受理但 A 的视图里仍有 D")
	}
	kicked, err := d.waitSnapshot(dMark, teamSmokePushTimeout, "D MEMBER_KICKED", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MEMBER_KICKED && s.GetTeam().GetTeamId() == 0
	})
	if err != nil {
		fail("S4-kick-push", "%v", err)
	}
	if kicked.GetTeam().GetMembershipEpoch() <= dEpochInTeam {
		fail("S4-kick-epoch", "D 被踢后的空视图 membership_epoch=%d,期望大于在队时的 %d(§C.4)",
			kicked.GetTeam().GetMembershipEpoch(), dEpochInTeam)
	}
	toB := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamTransferLeaderMessageId,
		&teampb.TransferLeaderRequest{TargetPlayerId: b.gc.PlayerId, ExpectedTeamId: tid}, toB, 0); err != nil {
		fail("S4-transfer-to-b", "%v", err)
	}
	if toB.GetTeam().GetLeaderId() != b.gc.PlayerId {
		fail("S4-transfer-to-b", "转让后 leader_id=%d,期望 B=%d", toB.GetTeam().GetLeaderId(), b.gc.PlayerId)
	}
	toA := &teampb.TeamResponse{}
	if err := b.expect(game.ClientPlayerTeamTransferLeaderMessageId,
		&teampb.TransferLeaderRequest{TargetPlayerId: a.gc.PlayerId, ExpectedTeamId: tid}, toA, 0); err != nil {
		fail("S4-transfer-to-a", "%v", err)
	}
	if toA.GetTeam().GetLeaderId() != a.gc.PlayerId {
		fail("S4-transfer-to-a", "转回后 leader_id=%d,期望 A=%d", toA.GetTeam().GetLeaderId(), a.gc.PlayerId)
	}
	zap.L().Info("[team-smoke] S4: D kicked (epoch advanced), leadership A→B→A",
		zap.Uint64("d_epoch_after_kick", kicked.GetTeam().GetMembershipEpoch()))

	// ---- S5:D 申请,A 拒绝 → D 收 APPLICATION_REJECTED ----
	if err := d.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
		&teampb.ApplyJoinTeamRequest{TargetPlayerId: a.gc.PlayerId}, &teampb.TeamResponse{}, 0); err != nil {
		fail("S5-apply", "%v", err)
	}
	dMark = d.mark()
	rejectResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamHandleApplicationMessageId,
		&teampb.HandleApplicationRequest{ApplicantId: d.gc.PlayerId, Approve: false, ExpectedTeamId: tid}, rejectResp, 0); err != nil {
		fail("S5-reject", "%v", err)
	}
	if teamSmokeHasApplication(rejectResp.GetTeam(), d.gc.PlayerId) {
		fail("S5-reject", "拒绝受理但 A 的视图里仍有 D 的申请")
	}
	if _, err := d.waitTeamEvent(dMark, "D APPLICATION_REJECTED", func(e *teampb.TeamEventS2C) bool {
		return e.GetType() == teampb.TeamEventType_TEAM_EVENT_TYPE_APPLICATION_REJECTED && e.GetTeamId() == tid
	}); err != nil {
		fail("S5-rejected-event", "%v", err)
	}
	zap.L().Info("[team-smoke] S5: D's application rejected and notified")

	// ---- S6:A 换图,B 跟随 ----
	followTarget := teamSmokePickSceneConfig(sc.FollowSceneConfigIds, a.player.GetSceneConfigID(), b.player.GetSceneConfigID())
	if followTarget == 0 {
		fail("S6-config", "follow_scene_config_ids=%v 里没有与 A(%d)、B(%d) 当前地图都不同的配置",
			sc.FollowSceneConfigIds, a.player.GetSceneConfigID(), b.player.GetSceneConfigID())
	}
	bMark = b.mark()
	leaderScene, err := a.switchScene(followTarget)
	if err != nil {
		fail("S6-leader-switch", "%v", err)
	}
	if _, err := b.waitEnterScene(bMark, teamSmokeFollowTimeout, func(s *scene.EnterSceneS2C) bool {
		return s.GetSceneInfo().GetSceneId() == leaderScene
	}); err != nil {
		fail("S6-follow", "B 在 %s 内没有进入 A 的新场景 scene_id=%d(前置:A、B 须在同一 scene 节点,DV-6):%v",
			teamSmokeFollowTimeout, leaderScene, err)
	}
	zap.L().Info("[team-smoke] S6: B followed A to the new scene",
		zap.Uint32("scene_config_id", followTarget), zap.Uint64("scene_id", leaderScene))

	// ---- S7:整队开战 ----
	battleId, err := teamSmokeTeamBattle(a, tid, sc.BattleConfigId, []*teamSmokeBot{a, b})
	if err != nil {
		fail("S7-team-battle", "%v", err)
	}
	zap.L().Info("[team-smoke] S7: team battle finished", zap.Uint64("battle_id", battleId))

	// ---- S8:B 在战斗中时 A 开战被拒 ----
	bSoloMark, bSoloBattle, err := b.startSoloBattle(sc.BattleConfigId, sc.ZoneA)
	if err != nil {
		fail("S8-b-solo-battle", "%v", err)
	}
	// B 尚未开自动战斗,对局停在第一回合等出招;scene 在 PrepareBattle 时已落 battle:lock(先于 CreateBattle / BattleStart)。
	if err := teamSmokeExpectMemberInBattle(a, tid, sc.BattleConfigId, b.gc.PlayerId); err != nil {
		fail("S8-in-battle-reject", "%v", err)
	}
	if _, err := b.finishBattle(bSoloMark, bSoloBattle); err != nil {
		fail("S8-b-solo-finish", "%v", err)
	}
	zap.L().Info("[team-smoke] S8: StartTeamMatch rejected while B was in battle", zap.Uint64("b_battle_id", bSoloBattle))

	// ---- X1 / X2:跨区 ----
	if c != nil && !sc.ExpectCrossZoneAllowed {
		if err := c.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
			&teampb.ApplyJoinTeamRequest{TargetPlayerId: a.gc.PlayerId}, &teampb.TeamResponse{}, tipTeamCrossZoneDenied); err != nil {
			fail("X1-apply", "%v", err)
		}
		if err := a.expect(game.ClientPlayerTeamInviteToTeamMessageId,
			&teampb.InviteToTeamRequest{TargetPlayerId: c.gc.PlayerId, ExpectedTeamId: tid}, &teampb.TeamResponse{}, tipTeamCrossZoneDenied); err != nil {
			fail("X1-invite", "%v", err)
		}
		zap.L().Info("[team-smoke] X1: cross-zone apply and invite denied")
	}
	if c != nil && sc.ExpectCrossZoneAllowed {
		if err := c.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
			&teampb.ApplyJoinTeamRequest{TargetPlayerId: a.gc.PlayerId}, &teampb.TeamResponse{}, 0); err != nil {
			fail("X2-apply", "%v", err)
		}
		approveC := &teampb.TeamResponse{}
		if err := a.expect(game.ClientPlayerTeamHandleApplicationMessageId,
			&teampb.HandleApplicationRequest{ApplicantId: c.gc.PlayerId, Approve: true, ExpectedTeamId: tid}, approveC, 0); err != nil {
			fail("X2-approve", "%v", err)
		}
		cMember, n := teamSmokeMemberOf(approveC.GetTeam(), c.gc.PlayerId)
		if n != 1 {
			fail("X2-approve", "同意后 C 在视图里出现 %d 次", n)
		}
		if cMember.GetZoneId() != sc.ZoneB {
			fail("X2-member-zone", "视图里 C 的 zone_id=%d,期望 zone_b=%d(账号须首次在 zone_b 建角)", cMember.GetZoneId(), sc.ZoneB)
		}

		// A 换图:C 在别的 zone,不得跟随(§F.1 第 4 条)。
		noFollowTarget := teamSmokePickSceneConfig(sc.FollowSceneConfigIds, a.player.GetSceneConfigID())
		if noFollowTarget == 0 {
			fail("X2-config", "follow_scene_config_ids=%v 里没有与 A 当前地图(%d)不同的配置", sc.FollowSceneConfigIds, a.player.GetSceneConfigID())
		}
		cMark := c.mark()
		if _, err := a.switchScene(noFollowTarget); err != nil {
			fail("X2-leader-switch", "%v", err)
		}
		if s, err := c.waitEnterScene(cMark, teamSmokeNoFollowWindow, nil); err == nil {
			fail("X2-no-follow", "A 换图后别区的 C 收到了 EnterSceneS2C scene_id=%d scene_config_id=%d,跨 zone 不应跟随",
				s.GetSceneInfo().GetSceneId(), s.GetSceneInfo().GetSceneConfigId())
		} else if !errors.Is(err, errTeamSmokeTimeout) {
			fail("X2-no-follow", "%v", err)
		}

		// B 先离队,只剩 A+C 开战,证明跨 zone gather。
		leaveB := &teampb.TeamResponse{}
		if err := b.expect(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, leaveB, 0); err != nil {
			fail("X2-b-leave", "%v", err)
		}
		if leaveB.GetTeam().GetTeamId() != 0 {
			fail("X2-b-leave", "B 离队受理但视图 team_id=%d", leaveB.GetTeam().GetTeamId())
		}
		crossBattle, err := teamSmokeTeamBattle(a, tid, sc.BattleConfigId, []*teamSmokeBot{a, c})
		if err != nil {
			fail("X2-team-battle", "%v", err)
		}
		zap.L().Info("[team-smoke] X2: cross-zone member joined, did not follow, and fought with A",
			zap.Uint64("battle_id", crossBattle))
	}

	// ---- S9:解散 ----
	final, err := a.myTeam()
	if err != nil {
		fail("S9-read", "%v", err)
	}
	type disbandWaiter struct {
		bot  *teamSmokeBot
		mark int
	}
	var waiters []disbandWaiter
	for _, member := range final.GetMembers() {
		if member.GetPlayerId() == a.gc.PlayerId {
			continue
		}
		bot, ok := botByPlayer[member.GetPlayerId()]
		if !ok {
			fail("S9-read", "队伍 %d 里有非本冒烟账号的成员 %d", tid, member.GetPlayerId())
		}
		waiters = append(waiters, disbandWaiter{bot: bot, mark: bot.mark()})
	}
	disbandResp := &teampb.TeamResponse{}
	if err := a.expect(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, disbandResp, 0); err != nil {
		fail("S9-disband", "%v", err)
	}
	if disbandResp.GetTeam().GetTeamId() != 0 {
		fail("S9-disband", "解散受理但 A 的视图 team_id=%d", disbandResp.GetTeam().GetTeamId())
	}
	for _, w := range waiters {
		if _, err := w.bot.waitSnapshot(w.mark, teamSmokePushTimeout, w.bot.name+" DISBANDED", func(s *teampb.TeamSnapshotS2C) bool {
			return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_DISBANDED && s.GetTeam().GetTeamId() == 0
		}); err != nil {
			fail("S9-disband-push", "%v", err)
		}
	}

	playerC := uint64(0)
	if c != nil {
		playerC = c.gc.PlayerId
	}
	zap.L().Info(fmt.Sprintf("TEAM_SMOKE_OK team_id=%d zone_a=%d player_a=%d player_b=%d player_d=%d player_c=%d",
		tid, sc.ZoneA, a.gc.PlayerId, b.gc.PlayerId, d.gc.PlayerId, playerC),
		zap.Bool("cross_zone", sc.CrossZone), zap.Bool("expect_cross_zone_allowed", sc.ExpectCrossZoneAllowed))
	cleanup()
	_ = zap.L().Sync()
}

// teamSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 guildSmokeLogin 同形;不走 prepareBehaviorClient:它的回调不认领组队回包,也不留底推送。
func teamSmokeLogin(cfg *config.Config, name, account string, stats *metrics.Stats) (*teamSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate: %w", err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &teamSmokeBot{
		name:    name,
		account: account,
		zone:    cfg.ZoneID,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
		replies: make(map[uint32]*base.MessageContent),
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), teamSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// onMessage:
//   - 组队请求的回包(含信封拒绝)由本场景按消息号认领,不再分发;
//   - 组队推送只留底,不再分发(本场景是唯一消费者);
//   - 换场景、战斗、JoinQueue 消息留底后仍交给通用分发,让 gameobject.Player 的场景信息等状态照常更新;
//   - gate 的 SendTipToClient 记下来供快速失败。
func (b *teamSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	messageId := msg.GetMessageId()
	if teamSmokeIsTeamRequest(messageId) {
		b.mu.Lock()
		if _, seen := b.replies[messageId]; !seen {
			b.replies[messageId] = msg
		}
		b.mu.Unlock()
		return
	}
	if teamSmokeIsRecordedMessage(messageId) {
		b.mu.Lock()
		b.pushes = append(b.pushes, teamSmokePush{
			messageId:   messageId,
			envelopeTip: msg.GetErrorMessage().GetId(),
			body:        msg.GetSerializedMessage(),
		})
		b.mu.Unlock()
		if teamSmokeIsTeamPush(messageId) {
			return
		}
	}
	if messageId == game.SceneClientPlayerCommonSendTipToClientMessageId {
		var tipInfo base.TipInfoMessage
		if err := proto.Unmarshal(msg.GetSerializedMessage(), &tipInfo); err == nil && tipInfo.GetId() != 0 {
			b.mu.Lock()
			b.gateTips = append(b.gateTips, tipInfo.GetId())
			b.mu.Unlock()
		}
	}
	handler.MessageBodyHandler(client, msg)
}

// pace 让出与上一个请求的最小间隔(teamSmokeRequestSpacing)。
func (b *teamSmokeBot) pace() {
	if wait := teamSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
}

// send 发一个不按消息号认领回包的请求(换场景、JoinQueue、SetAutoBattle)。
func (b *teamSmokeBot) send(messageId uint32, request proto.Message) error {
	b.pace()
	if err := b.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d account=%s: %w", messageId, b.account, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()
	return nil
}

// mark 返回当前留底推送的条数,作为"从这之后到达的推送"的扫描起点。必须在触发推送的动作之前调用。
func (b *teamSmokeBot) mark() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pushes)
}

// call 发一个组队请求并等同消息号的第一个回包。
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable 或超时返回 error。
func (b *teamSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	b.pace()
	b.mu.Lock()
	delete(b.replies, messageId)
	b.gateTips = nil
	b.mu.Unlock()

	if err := b.gc.SendRequest(messageId, request); err != nil {
		return 0, fmt.Errorf("send message_id=%d account=%s: %w", messageId, b.account, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()

	deadline := time.Now().Add(teamSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		reply := b.replies[messageId]
		gateUnavailable := false
		for _, tip := range b.gateTips {
			gateUnavailable = gateUnavailable || tip == tipTeamServiceUnavailable
		}
		b.mu.Unlock()

		if reply != nil {
			if tip := reply.GetErrorMessage().GetId(); tip != 0 {
				return tip, nil
			}
			if err := proto.Unmarshal(reply.GetSerializedMessage(), response); err != nil {
				return 0, fmt.Errorf("decode message_id=%d: %w", messageId, err)
			}
			return 0, nil
		}
		if gateUnavailable {
			return 0, fmt.Errorf("gate SendTipToClient tip=%d(gate 选不到路由服实例,或未以 GATE_CLIENT_RPC_ROUTER=1 启动)",
				tipTeamServiceUnavailable)
		}
		time.Sleep(teamSmokePollInterval)
	}
	return 0, fmt.Errorf("message_id=%d account=%s %w(%s)", messageId, b.account, errTeamSmokeTimeout, teamSmokeRpcTimeout)
}

// rpc 发请求并返回业务 tip(响应体 error_message.id,0 = 受理)。
// 信封拒绝与超时一律 error:它们说明请求没到 team 业务逻辑,不能当成 team 给的码去断言。
func (b *teamSmokeBot) rpc(messageId uint32, request proto.Message, response teamSmokeResponse) (uint32, error) {
	envelopeTip, err := b.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("gate/路由服信封拒绝 message_id=%d account=%s tip=%d(请求未到达 team 业务逻辑:tip=%d 多为路由服没有 "+
			"TeamNodeService 实例或 match 返回了 gRPC 错误;其它常见为 gate 限流(MessageLimiter 未导出 team 额度)或 gate 不认识该消息号)",
			messageId, b.account, envelopeTip, tipTeamServiceUnavailable)
	}
	return response.GetErrorMessage().GetId(), nil
}

// expect 发请求并断言业务 tip 等于 want。
func (b *teamSmokeBot) expect(messageId uint32, request proto.Message, response teamSmokeResponse, want uint32) error {
	got, err := b.rpc(messageId, request, response)
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	if got == tipTeamHomeZoneUnknown {
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(data_service 里没有该角色的 home zone 映射;"+
			"老账号请跑 tools/merge_zone -backfill-home-zone,或换一个首次登录的 robot_ 账号)", messageId, b.account, got)
	}
	return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d parameters=%v",
		messageId, b.account, want, got, response.GetErrorMessage().GetParameters())
}

// myTeam 拉自己当前的队伍视图(无队时 team_id=0)。
func (b *teamSmokeBot) myTeam() (*teampb.TeamView, error) {
	resp := &teampb.TeamResponse{}
	if err := b.expect(game.ClientPlayerTeamGetMyTeamMessageId, &teampb.GetMyTeamRequest{}, resp, 0); err != nil {
		return nil, err
	}
	return resp.GetTeam(), nil
}

// leaveAnyTeam 把机器人带回"不在任何队伍":上一轮失败可能留下队伍或成员关系。
func (b *teamSmokeBot) leaveAnyTeam() error {
	for attempt := 0; attempt < 3; attempt++ {
		view, err := b.myTeam()
		if err != nil {
			return err
		}
		tid := view.GetTeamId()
		if tid == 0 {
			return nil
		}
		resp := &teampb.TeamResponse{}
		var tip uint32
		if view.GetLeaderId() == b.gc.PlayerId {
			tip, err = b.rpc(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, resp)
		} else {
			tip, err = b.rpc(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, resp)
		}
		if err != nil {
			return fmt.Errorf("leave leftover team %d: %w", tid, err)
		}
		if tip == tipTeamInMatch {
			return fmt.Errorf("残留队伍 %d 的开战锁仍有效(tip=%d,上一轮死在开战途中;锁最长约 83s 自然过期),稍后重跑", tid, tip)
		}
		if tip != 0 {
			return fmt.Errorf("leave leftover team %d: tip=%d parameters=%v", tid, tip, resp.GetErrorMessage().GetParameters())
		}
	}
	return errors.New("连续 3 次离队 / 解散后仍在队伍里")
}

// switchScene 请求换到 configId 对应的地图,返回新场景的 scene_id。
//
// 换图消息核实(§I.5 S6 "待核实"):用客户端协议 SceneSceneClientPlayer.EnterScene(EnterSceneC2SRequest.scene_info.scene_config_id),
// 与 login_test_scenarios.go testSceneSwitch 同一条。链路:scene 的 SceneSceneClientPlayerHandler::EnterScene
// (cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp)→ SceneManager.EnterScene → RoutePlayerEvent →
// gate 同节点换图转发(commit cd918f4f8 的 gate_scene_route::ApplyRoute,LOGIN_NONE 也转发)→ scene HandleEnterScene
// → EnterSceneS2C。战斗在途时 scene 直接拒绝(kEnterSceneFailed)。
func (b *teamSmokeBot) switchScene(configId uint32) (uint64, error) {
	since := b.mark()
	if err := b.send(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: configId},
	}); err != nil {
		return 0, err
	}
	entered, err := b.waitEnterScene(since, teamSmokeFollowTimeout, func(s *scene.EnterSceneS2C) bool {
		return s.GetSceneInfo().GetSceneConfigId() == configId
	})
	if err != nil {
		if tip := b.enterSceneRejection(since); tip != 0 {
			return 0, fmt.Errorf("%s 换图到 scene_config_id=%d 被拒 tip=%d(战斗在途 / 参数错误 / 找不到 SceneManager)", b.name, configId, tip)
		}
		return 0, fmt.Errorf("%s 换图到 scene_config_id=%d: %w", b.name, configId, err)
	}
	return entered.GetSceneInfo().GetSceneId(), nil
}

// enterSceneRejection 返回 since 之后 EnterSceneC2S 回包里的拒绝码(信封或响应体),0 = 没有拒绝。只用于诊断。
func (b *teamSmokeBot) enterSceneRejection(since int) uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pushes[min(since, len(b.pushes)):] {
		if p.messageId != game.SceneSceneClientPlayerEnterSceneMessageId {
			continue
		}
		if p.envelopeTip != 0 {
			return p.envelopeTip
		}
		var resp scene.EnterSceneC2SResponse
		if err := proto.Unmarshal(p.body, &resp); err == nil && resp.GetErrorMessage().GetId() != 0 {
			return resp.GetErrorMessage().GetId()
		}
	}
	return 0
}

// startSoloBattle 让本机器人单人 PVE(PVE_SOLO)开战,返回扫描起点与对局 id;不开自动战斗。
// 上一场结算落地前 battle:lock 仍在,JoinQueue 会被拒(过渡态):在 teamSmokeSettleTimeout 内重试。
func (b *teamSmokeBot) startSoloBattle(battleConfigId, zoneId uint32) (int, uint64, error) {
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	for {
		since := b.mark()
		if err := b.send(game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
			PlayerId:       b.gc.PlayerId,
			Mode:           match.MatchMode_MATCH_MODE_PVE_SOLO,
			BattleConfigId: battleConfigId,
			ZoneId:         zoneId,
		}); err != nil {
			return 0, 0, err
		}
		resp, err := teamSmokeWaitPush(b, since, game.MatchServiceJoinQueueMessageId, teamSmokeRpcTimeout,
			func() *match.JoinQueueResponse { return &match.JoinQueueResponse{} }, nil)
		if err != nil {
			return 0, 0, fmt.Errorf("%s JoinQueue(PVE_SOLO): %w", b.name, err)
		}
		if resp.GetErrorCode() == 0 && resp.GetErrorMessage().GetId() == 0 {
			start, err := b.waitBattleStart(since)
			if err != nil {
				return 0, 0, err
			}
			return since, start.GetBattleId(), nil
		}
		if !time.Now().Before(deadline) {
			return 0, 0, fmt.Errorf("%s JoinQueue(PVE_SOLO) 在 %s 内一直被拒 error_code=%d tip=%d",
				b.name, teamSmokeSettleTimeout, resp.GetErrorCode(), resp.GetErrorMessage().GetId())
		}
		zap.L().Info("[team-smoke] solo JoinQueue rejected, retrying (previous battle may still be settling)",
			zap.String("bot", b.name), zap.Uint32("error_code", resp.GetErrorCode()), zap.Uint32("tip", resp.GetErrorMessage().GetId()))
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}

// finishBattle 对 since 之后开始的对局 battleId 开自动战斗(经 gate 中继),等同一 battle_id 的终局,返回回合结算条数。
func (b *teamSmokeBot) finishBattle(since int, battleId uint64) (int, error) {
	if err := b.send(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{
		BattleId: battleId,
		Enabled:  true,
	}); err != nil {
		return 0, err
	}
	if _, err := teamSmokeWaitPush(b, since, game.BattleClientPlayerNotifyBattleEndMessageId, teamSmokeBattleEndTimeout,
		func() *battle.BattleEndS2C { return &battle.BattleEndS2C{} },
		// 按 battle_id 过滤:登录补推的上一局结算、上一场的终局都不算(同 Player.SignalBattleEnd 的口径)。
		func(e *battle.BattleEndS2C) bool { return e.GetBattleId() == battleId }); err != nil {
		return 0, fmt.Errorf("%s 等 battle_id=%d 的 BattleEnd: %w", b.name, battleId, err)
	}
	turns := b.countTurnResults(since, battleId)
	if turns < 1 {
		return turns, fmt.Errorf("%s 在 battle_id=%d 里收到 %d 条回合结算,期望 >= 1", b.name, battleId, turns)
	}
	return turns, nil
}

// countTurnResults 统计 since 之后属于 battleId 的回合结算推送条数(不等待)。
func (b *teamSmokeBot) countTurnResults(since int, battleId uint64) int {
	b.mu.Lock()
	batch := append([]teamSmokePush(nil), b.pushes[min(since, len(b.pushes)):]...)
	b.mu.Unlock()
	count := 0
	for _, p := range batch {
		if p.messageId != game.BattleClientPlayerNotifyTurnResultMessageId {
			continue
		}
		var result battle.TurnResultS2C
		if err := proto.Unmarshal(p.body, &result); err == nil && result.GetBattleId() == battleId {
			count++
		}
	}
	return count
}

func (b *teamSmokeBot) waitSnapshot(since int, timeout time.Duration, what string, pred func(*teampb.TeamSnapshotS2C) bool) (*teampb.TeamSnapshotS2C, error) {
	snapshot, err := teamSmokeWaitPush(b, since, game.ClientPlayerTeamNotifyTeamSnapshotMessageId, timeout,
		func() *teampb.TeamSnapshotS2C { return &teampb.TeamSnapshotS2C{} }, pred)
	if err != nil {
		return nil, fmt.Errorf("等 NotifyTeamSnapshot(%s): %w", what, err)
	}
	return snapshot, nil
}

func (b *teamSmokeBot) waitTeamEvent(since int, what string, pred func(*teampb.TeamEventS2C) bool) (*teampb.TeamEventS2C, error) {
	event, err := teamSmokeWaitPush(b, since, game.ClientPlayerTeamNotifyTeamEventMessageId, teamSmokePushTimeout,
		func() *teampb.TeamEventS2C { return &teampb.TeamEventS2C{} }, pred)
	if err != nil {
		return nil, fmt.Errorf("等 NotifyTeamEvent(%s): %w", what, err)
	}
	return event, nil
}

func (b *teamSmokeBot) waitEnterScene(since int, timeout time.Duration, pred func(*scene.EnterSceneS2C) bool) (*scene.EnterSceneS2C, error) {
	return teamSmokeWaitPush(b, since, game.SceneSceneClientPlayerNotifyEnterSceneMessageId, timeout,
		func() *scene.EnterSceneS2C { return &scene.EnterSceneS2C{} }, pred)
}

func (b *teamSmokeBot) waitBattleStart(since int) (*battle.BattleStartS2C, error) {
	start, err := teamSmokeWaitPush(b, since, game.BattleClientPlayerNotifyBattleStartMessageId, teamSmokeBattleStartTimeout,
		func() *battle.BattleStartS2C { return &battle.BattleStartS2C{} },
		func(s *battle.BattleStartS2C) bool { return s.GetBattleId() != 0 })
	if err != nil {
		return nil, fmt.Errorf("%s 等 BattleStart: %w", b.name, err)
	}
	return start, nil
}

// teamSmokeWaitPush 从下标 since 起按到达顺序扫描 bot 的留底推送,返回第一条消息号为 messageId 且满足 pred
// (nil = 任意)的推送;信封拒绝直接返回 error。超时返回包裹 errTeamSmokeTimeout 的 error。
func teamSmokeWaitPush[T proto.Message](bot *teamSmokeBot, since int, messageId uint32, timeout time.Duration,
	newMsg func() T, pred func(T) bool) (T, error) {
	var zero T
	next := since
	deadline := time.Now().Add(timeout)
	for {
		bot.mu.Lock()
		if next > len(bot.pushes) {
			next = len(bot.pushes)
		}
		batch := append([]teamSmokePush(nil), bot.pushes[next:]...)
		bot.mu.Unlock()
		next += len(batch)

		for _, p := range batch {
			if p.messageId != messageId {
				continue
			}
			if p.envelopeTip != 0 {
				return zero, fmt.Errorf("message_id=%d account=%s 信封拒绝 tip=%d", messageId, bot.account, p.envelopeTip)
			}
			msg := newMsg()
			if err := proto.Unmarshal(p.body, msg); err != nil {
				return zero, fmt.Errorf("decode message_id=%d account=%s: %w", messageId, bot.account, err)
			}
			if pred == nil || pred(msg) {
				return msg, nil
			}
		}
		if !time.Now().Before(deadline) {
			return zero, fmt.Errorf("message_id=%d account=%s %w(%s)", messageId, bot.account, errTeamSmokeTimeout, timeout)
		}
		time.Sleep(teamSmokePollInterval)
	}
}

// teamSmokeStartTeamMatch 队长发起整队开战直到受理,返回受理回包里的视图。
// 上一场结算落地前队员仍持 battle:lock(TeamMemberInBattle)、上一场的 ready 票据未清(TeamMemberNotReady)
// 都是有界过渡态:在 teamSmokeSettleTimeout 内重试;其它拒绝码立即失败。被拒时服务端不加锁,重试没有副作用。
func teamSmokeStartTeamMatch(leader *teamSmokeBot, tid uint64, battleConfigId uint32) (*teampb.TeamView, error) {
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	request := &teampb.StartTeamMatchRequest{BattleConfigId: battleConfigId, ExpectedTeamId: tid}
	for {
		resp := &teampb.TeamResponse{}
		tip, err := leader.rpc(game.ClientPlayerTeamStartTeamMatchMessageId, request, resp)
		if err != nil {
			return nil, err
		}
		if tip == 0 {
			return resp.GetTeam(), nil
		}
		transient := tip == tipTeamMemberInBattle || tip == tipTeamMemberNotReady
		if !transient || !time.Now().Before(deadline) {
			return nil, fmt.Errorf("StartTeamMatch(battle_config_id=%d) 被拒 tip=%d parameters=%v",
				battleConfigId, tip, resp.GetErrorMessage().GetParameters())
		}
		zap.L().Info("[team-smoke] StartTeamMatch hit a settling member, retrying",
			zap.Uint32("tip", tip), zap.Strings("parameters", resp.GetErrorMessage().GetParameters()))
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}

// teamSmokeExpectMemberInBattle 断言队长开战被拒且码为 TeamMemberInBattle、parameters[0] 是 inBattle。
// 若先撞到的是另一名队员(上一场结算尚未落地),在 teamSmokeSettleTimeout 内重试。
func teamSmokeExpectMemberInBattle(leader *teamSmokeBot, tid uint64, battleConfigId uint32, inBattle uint64) error {
	want := strconv.FormatUint(inBattle, 10)
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	request := &teampb.StartTeamMatchRequest{BattleConfigId: battleConfigId, ExpectedTeamId: tid}
	for {
		resp := &teampb.TeamResponse{}
		tip, err := leader.rpc(game.ClientPlayerTeamStartTeamMatchMessageId, request, resp)
		if err != nil {
			return err
		}
		params := resp.GetErrorMessage().GetParameters()
		if tip == 0 {
			return fmt.Errorf("玩家 %s 正在战斗中,StartTeamMatch 却被受理(match_state=%s)", want, resp.GetTeam().GetMatchState())
		}
		if tip == tipTeamMemberInBattle && len(params) > 0 && params[0] == want {
			return nil
		}
		settling := tip == tipTeamMemberInBattle || tip == tipTeamMemberNotReady
		if !settling || !time.Now().Before(deadline) {
			return fmt.Errorf("期望 tip=%d parameters[0]=%s,实得 tip=%d parameters=%v", tipTeamMemberInBattle, want, tip, params)
		}
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}

// teamSmokeTeamBattle 队长整队开战:队长以回包 STARTING 为准,其余队员等 MATCH_STARTED;全员等同一 battle_id 的
// BattleStart、自动战斗到终局,最后全员(含队长)等 MATCH_ENDED。fighters 必须恰好是当前队伍全员(队长在内)。返回 battle_id。
func teamSmokeTeamBattle(leader *teamSmokeBot, tid uint64, battleConfigId uint32, fighters []*teamSmokeBot) (uint64, error) {
	marks := make([]int, len(fighters))
	for i, fighter := range fighters {
		marks[i] = fighter.mark()
	}
	view, err := teamSmokeStartTeamMatch(leader, tid, battleConfigId)
	if err != nil {
		return 0, err
	}
	if view.GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_STARTING {
		return 0, fmt.Errorf("StartTeamMatch 受理但 match_state=%s,期望 STARTING", view.GetMatchState())
	}
	for i, fighter := range fighters {
		// 发起人不收 MATCH_STARTED:服务端不给 RPC 调用者推快照(go/match/internal/team/notify.go pushCommit),
		// 他的回包就是同一次加锁提交构建的视图,上面已断言 STARTING。
		if fighter == leader {
			continue
		}
		if _, err := fighter.waitSnapshot(marks[i], teamSmokePushTimeout, fighter.name+" MATCH_STARTED", func(s *teampb.TeamSnapshotS2C) bool {
			return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_STARTED && s.GetTeam().GetTeamId() == tid
		}); err != nil {
			return 0, err
		}
	}

	// 每人一个 goroutine 跑"等开战 → 自动战斗 → 等终局";goroutine 内只驱动自己那个机器人。
	type fightResult struct {
		battleId uint64
		turns    int
		err      error
	}
	results := make([]fightResult, len(fighters))
	var wg sync.WaitGroup
	for i, fighter := range fighters {
		wg.Add(1)
		go func(idx int, bot *teamSmokeBot) {
			defer wg.Done()
			start, err := bot.waitBattleStart(marks[idx])
			if err != nil {
				results[idx].err = err
				return
			}
			results[idx].battleId = start.GetBattleId()
			results[idx].turns, results[idx].err = bot.finishBattle(marks[idx], start.GetBattleId())
		}(i, fighter)
	}
	wg.Wait()

	battleId := uint64(0)
	for i, result := range results {
		if result.err != nil {
			return 0, result.err
		}
		if i == 0 {
			battleId = result.battleId
			continue
		}
		if result.battleId != battleId {
			return 0, fmt.Errorf("%s battle_id=%d 与 %s battle_id=%d 不同,整队没有进同一场", fighters[i].name, result.battleId, fighters[0].name, battleId)
		}
	}

	for i, fighter := range fighters {
		ended, err := fighter.waitSnapshot(marks[i], teamSmokePushTimeout, fighter.name+" MATCH_ENDED/FAILED", func(s *teampb.TeamSnapshotS2C) bool {
			reason := s.GetReason()
			return s.GetTeam().GetTeamId() == tid && (reason == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED ||
				reason == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_FAILED)
		})
		if err != nil {
			return 0, err
		}
		if ended.GetReason() != teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_ENDED {
			return 0, fmt.Errorf("%s 收到 %s tip=%d parameters=%v,期望 MATCH_ENDED", fighter.name, ended.GetReason(),
				ended.GetTip().GetId(), ended.GetTip().GetParameters())
		}
	}
	return battleId, nil
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// teamSmokeIsTeamRequest:12 个请求 RPC 的回包按消息号认领。
func teamSmokeIsTeamRequest(messageId uint32) bool {
	switch messageId {
	case game.ClientPlayerTeamCreateTeamMessageId,
		game.ClientPlayerTeamGetMyTeamMessageId,
		game.ClientPlayerTeamApplyJoinTeamMessageId,
		game.ClientPlayerTeamHandleApplicationMessageId,
		game.ClientPlayerTeamInviteToTeamMessageId,
		game.ClientPlayerTeamRespondInviteMessageId,
		game.ClientPlayerTeamListMyInvitesMessageId,
		game.ClientPlayerTeamLeaveTeamMessageId,
		game.ClientPlayerTeamKickMemberMessageId,
		game.ClientPlayerTeamTransferLeaderMessageId,
		game.ClientPlayerTeamDisbandTeamMessageId,
		game.ClientPlayerTeamStartTeamMatchMessageId:
		return true
	}
	return false
}

// teamSmokeIsTeamPush:3 个组队推送。
func teamSmokeIsTeamPush(messageId uint32) bool {
	switch messageId {
	case game.ClientPlayerTeamNotifyTeamSnapshotMessageId,
		game.ClientPlayerTeamNotifyTeamInviteMessageId,
		game.ClientPlayerTeamNotifyTeamEventMessageId:
		return true
	}
	return false
}

// teamSmokeIsRecordedMessage:需要按到达顺序留底的消息(组队推送 + 换场景 + 战斗 + JoinQueue 回包)。
func teamSmokeIsRecordedMessage(messageId uint32) bool {
	if teamSmokeIsTeamPush(messageId) {
		return true
	}
	switch messageId {
	case game.SceneSceneClientPlayerNotifyEnterSceneMessageId,
		game.SceneSceneClientPlayerEnterSceneMessageId,
		game.BattleClientPlayerNotifyBattleStartMessageId,
		game.BattleClientPlayerNotifyTurnResultMessageId,
		game.BattleClientPlayerNotifyBattleEndMessageId,
		game.MatchServiceJoinQueueMessageId:
		return true
	}
	return false
}

// teamSmokeMemberOf 返回视图里 playerId 的成员条目与出现次数(成员集合语义,出现 2 次即服务端缺陷)。
func teamSmokeMemberOf(view *teampb.TeamView, playerId uint64) (*teampb.TeamMemberView, int) {
	var found *teampb.TeamMemberView
	count := 0
	for _, member := range view.GetMembers() {
		if member.GetPlayerId() == playerId {
			found = member
			count++
		}
	}
	return found, count
}

func teamSmokeHasApplication(view *teampb.TeamView, playerId uint64) bool {
	for _, application := range view.GetApplications() {
		if application.GetPlayer().GetPlayerId() == playerId {
			return true
		}
	}
	return false
}

func teamSmokeHasPendingInvite(view *teampb.TeamView, playerId uint64) bool {
	for _, invite := range view.GetPendingInvites() {
		if invite.GetInvitee().GetPlayerId() == playerId {
			return true
		}
	}
	return false
}

func teamSmokeHasIncomingInvite(resp *teampb.ListMyInvitesResponse, tid uint64) bool {
	for _, invite := range resp.GetInvites() {
		if invite.GetTeamId() == tid {
			return true
		}
	}
	return false
}

// teamSmokePickSceneConfig 按配置顺序返回第一个不在 excluded 里的地图配置 id;没有返回 0。
// S6 要排除 A、B 两人的当前地图:若 B 已经在目标图,A 换过去不会触发 B 的进场,跟随断言就失真。
func teamSmokePickSceneConfig(candidates []uint32, excluded ...uint32) uint32 {
	for _, candidate := range candidates {
		skip := false
		for _, e := range excluded {
			if candidate == e {
				skip = true
				break
			}
		}
		if !skip {
			return candidate
		}
	}
	return 0
}
