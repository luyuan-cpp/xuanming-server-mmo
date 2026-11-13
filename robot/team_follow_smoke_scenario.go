package main

// team-follow-smoke 场景:两个机器人对「组队跟随 v1.1」的服务端部分做端到端冒烟
// (docs/design/team-system.md「组队跟随 v1.1」V.2;走路跟随本身在客户端,不在本场景范围):
//
//	F0 预备:A、B 各自离开残留队伍;从自己的 ActorCreate(guid == 自己)读出实体号与进场坐标;
//	F1 拉开距离:A 朝若干方向之一上报移动(服务器按导航裁决,撞墙会回 MoveAck),直到与 B 水平相距 >= 12;
//	F2 入队即归队:A 建队 → B 申请 → A 同意。B 在 15s 内收到 TeleportS2C(reason=5),落点在 A 的权威位置 1 以内
//	   (两人不同线时 B 会先收到换线的 EnterSceneS2C,这一步不单独断言,只记进日志);
//	   随后 B 像真实客户端那样在落点上报一次位置,服务器不应把它拉走;
//	F3 可见性重建:B 收到 guid == A 的 ActorCreate,A 收到 guid == B 的 ActorCreate;
//	F4 移动广播:A MoveStart(带速度)→ B 收到 entity == A、move_state=1、速度非零的 ActorMoveS2C;
//	   A MoveStop → B 收到 move_state=0。反过来 B 动一次,A 同样收到;
//	F6 请求归队(GetMyTeam{regroup},V.9):B 在 A 身边请求 → 2s 内不应瞬移;B 自己走开 >= 12 → 不请求就没有瞬移;
//	   B 请求归队 → 15s 内收到 TeleportS2C(reason=5)、落点在 A 的位置 1 以内,在落点上报不被拉走;
//	F5 A 解散。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `TEAM_FOLLOW_SMOKE_OK team_id=… player_a=… player_b=…`,退出码 0;
//	任一步失败 → `TEAM_FOLLOW_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// `team_follow_smoke.client_follower: true` 时改跑「真实客户端跟随」验收(走路跟随在客户端,只有真客户端能验):
//
//	C1 A 建队、机器人 B 入队;C2 机器人 B 下线(离线队员留在队里),A 走开 16,然后打印一行
//	   `TEAM_FOLLOW_CLIENT_READY account=robot_9306 …` —— 外层脚本看到后用 DevAutoPilot 让真实客户端登录该账号;
//	C3 A 等客户端出现在视野里,且出现的位置就在 A 身边(登录进场 → 被带到 A 的线 → 归队瞬移);
//	C4 A 按脚本走 6 段,从客户端的移动广播里断言:第一条上报来自 A 身边(客户端处理了 TeleportS2C),
//	   每段都跟着走、A 停下后它也停下并停在身后约 1.4 处;
//	C5 A 解散。全过 → `TEAM_FOLLOW_CLIENT_OK … leg0=ok(…) …`;失败同样是 `TEAM_FOLLOW_SMOKE_FAIL step=C… reason=…`。
//
// 组队请求 / 回包复用 team-smoke 的 teamSmokeBot(同一套经 smokeCaller 按请求号认领回包、信封拒绝与业务 tip 分开的口径);
// 本场景另把**所有**下行消息按到达顺序留底(records),因为要断言的 ActorCreate / ActorMove / Teleport / MoveAck
// 不在 team-smoke 的留底名单里。每步在触发动作**之前**记下标,之后从下标起扫描。

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/common/component"
	"proto/scene"
	teampb "proto/team"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

// 账号落在 team-smoke 预留的 93xx 段里(9301-9304 已被 team-smoke 占用),与其它冒烟错开,可并行跑。
const (
	teamFollowSmokeAccountA = "robot_9305" // 队长
	teamFollowSmokeAccountB = "robot_9306" // 队员(入队后被带到队长身边)
)

const (
	// 与服务端 kTeleportReasonTeamRegroup 一致(cpp/libs/services/scene/spatial/system/movement.h)。
	teamFollowSmokeRegroupReason = 5
	// F1 要拉开的最小水平距离。服务端归队阈值 kTeamRegroupDistance = 8,留出余量。
	teamFollowSmokeSeparation = 12.0
	// F1 每次尝试朝目标方向走出的距离。
	teamFollowSmokeStride = 16.0
	// 落点 / 复位位置与期望点的允许误差。
	teamFollowSmokeArriveTolerance = 1.0
	// 上报位置后等服务器裁决(MoveAck)的窗口:有 MoveAck 以最后一条为准,没有则上报点被原样接受。
	teamFollowSmokeAckWindow = 800 * time.Millisecond
	// 等自己的 ActorCreate。
	teamFollowSmokeSelfTimeout = 5 * time.Second
	// F2 等归队瞬移:覆盖「刷新事件经 Kafka 到 scene → 三跳 Redis 回调 → (不同线时)换线进场 → 再判一次归队」。
	teamFollowSmokeTeleportTimeout = 15 * time.Second
	// F3 等可见性重建:瞬移后下一次 AoiSystem::Update 即建好。
	teamFollowSmokeVisibleTimeout = 5 * time.Second
	// F4 等移动广播。
	teamFollowSmokeMoveTimeout = 3 * time.Second
	// F4 上报的速度(只为让 move_state = 1,远低于服务端信任上限 10)。
	teamFollowSmokeProbeSpeed = 2.0
	// F6 断言「不该瞬移」时观察的窗口:同一条刷新链(Kafka → scene 三跳 Redis)平时在 1 秒内走完。
	teamFollowSmokeQuietWindow = 2 * time.Second

	// ---- client_follower=true(真实客户端跟随验收)----
	// C3 等客户端出现的缺省预算:播放器冷启动 + 登录 + 被带到队长的线。
	teamFollowSmokeClientWait = 240 * time.Second
	// C1 组队后等机器人 B 自己的归队瞬移(只在两人起步相距较远 / 不同线时才会有)。
	teamFollowSmokeFormSettle = 3 * time.Second
	// C4 队员第一条移动上报离队长出发点的上限:落在瞬移位置上的客户端从 1.4 以内起步,留在旧位置的会从 16 之外来。
	teamFollowSmokeRegroupReach = 5.0
	// C3 等待期间的保活间隔。
	teamFollowSmokeKeepAlive = 10 * time.Second
	// 客户端出现后到开始走路之间的等待(本地角色、队伍视图、跟随驱动就绪)。
	teamFollowSmokeClientSettle = 4 * time.Second
	// 队长的移速与上报节拍,与客户端一致(基础移速 9,移动中每 250ms 一条 MoveSync)。
	teamFollowSmokeWalkSpeed    = 9.0
	teamFollowSmokeSyncInterval = 250 * time.Millisecond
	// 每段的 MoveSync 条数:6 × 9 × 0.25 = 13.5。
	teamFollowSmokeLegSteps = 6
	// 段末等队员走到位并停下。
	teamFollowSmokeLegSettle = 2500 * time.Millisecond
	// 实际走出不到这么远的段(撞墙)不计入断言。
	teamFollowSmokeMinLeg = 4.0
	// 队员停下后离队长的允许范围:客户端一号槽位 1.4,起停滞回 0.15 / 0.6,再留出网络与裁决误差。
	teamFollowSmokeGapMin = 0.5
	teamFollowSmokeGapMax = 3.0
	// 走满这么远的段才判「在身后」;沿行进方向至少落后这么多。
	teamFollowSmokeFullLeg   = 13.0
	teamFollowSmokeBehindMin = 0.5
)

// teamFollowSmokeRecord 是一条留底的下行消息。
type teamFollowSmokeRecord struct {
	messageId uint32
	body      []byte
}

// teamFollowSmokeBot 在 teamSmokeBot 之上加一份「全部下行消息」留底。
type teamFollowSmokeBot struct {
	*teamSmokeBot

	// RecvLoop goroutine 写、场景流程读,recMu 保护。只追加。
	recMu   sync.Mutex
	records []teamFollowSmokeRecord

	// 只由主流程读写。
	inputSeq uint32
}

// RunTeamFollowSmoke 是 main.go `mode: team-follow-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunTeamFollowSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var bots []*teamFollowSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("TEAM_FOLLOW_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录(并发) ----
	accounts := []struct{ name, account string }{{"A", teamFollowSmokeAccountA}, {"B", teamFollowSmokeAccountB}}
	loggedIn := make([]*teamFollowSmokeBot, len(accounts))
	loginErrs := make([]error, len(accounts))
	var wg sync.WaitGroup
	for i, spec := range accounts {
		wg.Add(1)
		go func(idx int, name, account string) {
			defer wg.Done()
			loggedIn[idx], loginErrs[idx] = teamFollowSmokeLogin(cfg, name, account, stats)
		}(i, spec.name, spec.account)
	}
	wg.Wait()
	// 先收齐已登录的会话再判错:否则一人失败时另一人的会话不会被 cleanup 收尾。
	for _, bot := range loggedIn {
		if bot != nil {
			bots = append(bots, bot)
		}
	}
	for i, err := range loginErrs {
		if err != nil {
			fail("login", "account=%s zone=%d err=%v", accounts[i].account, cfg.ZoneID, err)
		}
	}
	a, b := loggedIn[0], loggedIn[1]

	// ---- F0:离开残留队伍;读出自己的实体号与进场坐标 ----
	for _, bot := range bots {
		if err := bot.leaveAnyTeam(); err != nil {
			fail("F0-prepare", "account=%s: %v", bot.account, err)
		}
	}
	entityA, posA, err := a.waitSelfActor()
	if err != nil {
		fail("F0-self-actor", "A: %v", err)
	}
	entityB, posB, err := b.waitSelfActor()
	if err != nil {
		fail("F0-self-actor", "B: %v", err)
	}
	sceneA, sceneB := a.lastEnterScene(), b.lastEnterScene()
	if sceneA.GetSceneConfigId() != sceneB.GetSceneConfigId() {
		fail("F0-same-map", "A 在地图 %d、B 在地图 %d:本场景要求两人起步在同一张地图(新号默认同图;别的冒烟动过这两个账号时换一对账号)",
			sceneA.GetSceneConfigId(), sceneB.GetSceneConfigId())
	}
	sameLineBefore := sceneA.GetSceneId() == sceneB.GetSceneId()
	zap.L().Info("[team-follow-smoke] F0: robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId),
		zap.Uint64("entity_a", entityA), zap.Uint64("entity_b", entityB),
		zap.String("pos_a", teamFollowSmokeFormat(posA)), zap.String("pos_b", teamFollowSmokeFormat(posB)),
		zap.Uint32("scene_config_id", sceneA.GetSceneConfigId()), zap.Bool("same_line", sameLineBefore))

	// ---- client_follower=true:改跑「真实客户端跟随」验收(C1..C5),不走下面的 F1..F5 ----
	if cfg.TeamFollowSmoke.ClientFollower {
		// C1:组队。两人相距较远 / 不同线时机器人 B 会先被带到 A 身边,以它收到的落点为准。
		bFormMark := b.recordMark()
		tid, err := teamFollowSmokeFormTeam(a, b)
		if err != nil {
			fail("C1-team", "%v", err)
		}
		if regroup, err := teamFollowSmokeWait(b, bFormMark, game.SceneMovementClientPlayerNotifyTeleportMessageId, teamFollowSmokeFormSettle,
			func() *scene.TeleportS2C { return &scene.TeleportS2C{} },
			func(t *scene.TeleportS2C) bool { return t.GetReason() == teamFollowSmokeRegroupReason }); err == nil {
			posB = teamFollowSmokeFromVector(regroup.GetTransform().GetLocation())
		}
		// C2:机器人 B 下线(不离队:离线队员留在队里),把账号让给真实客户端;
		// 然后队长走开 16:客户端登录时落在 B 的存档位置,必须经过「归队瞬移」才能到队长身边(客户端要处理 TeleportS2C)。
		followerId := b.gc.PlayerId
		leaderMark := a.recordMark()
		_ = leaveGame(b.gc, stats)
		gameobject.PlayerList.Delete(b.gc.PlayerId)
		b.gc.Close()
		bots = []*teamFollowSmokeBot{a}
		posA, err = teamFollowSmokeWalkAway(a, posA, posB)
		if err != nil {
			fail("C2-walk-away", "%v", err)
		}
		zap.L().Info("[team-follow-smoke] C2: robot B went offline and the leader walked away",
			zap.String("pos_a", teamFollowSmokeFormat(posA)), zap.String("saved_pos_b", teamFollowSmokeFormat(posB)),
			zap.Float64("distance", teamFollowSmokeDistance(posA, posB)))
		// 外层脚本看到这一行再启动客户端(同一账号不能同时在线)。
		zap.L().Info(fmt.Sprintf("TEAM_FOLLOW_CLIENT_READY account=%s player_id=%d team_id=%d", teamFollowSmokeAccountB, followerId, tid))
		_ = zap.L().Sync()

		summary, step, checkErr := teamFollowSmokeClientCheck(cfg, a, followerId, posA, leaderMark)
		// C5:无论成败都解散,不把队伍留给下一轮
		disbandErr := a.expect(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0)
		if checkErr != nil {
			fail(step, "%v", checkErr)
		}
		if disbandErr != nil {
			fail("C5-disband", "%v", disbandErr)
		}
		zap.L().Info(fmt.Sprintf("TEAM_FOLLOW_CLIENT_OK team_id=%d player_a=%d player_b=%d %s", tid, a.gc.PlayerId, followerId, summary))
		cleanup()
		return
	}

	// ---- F1:A 走开,直到与 B 水平相距 >= teamFollowSmokeSeparation ----
	posA, err = teamFollowSmokeWalkAway(a, posA, posB)
	if err != nil {
		fail("F1-walk-away", "%v", err)
	}
	zap.L().Info("[team-follow-smoke] F1: A walked away",
		zap.String("pos_a", teamFollowSmokeFormat(posA)), zap.Float64("distance", teamFollowSmokeDistance(posA, posB)))

	// ---- F2:A 建队 → B 申请 → A 同意;B 被瞬移到 A 身边 ----
	// 下标取在建队之前:B 入队之前不会有归队瞬移,两人此刻也互相不可见,之后到达的 Teleport / ActorCreate 才是要断言的。
	aJoinMark, bJoinMark := a.recordMark(), b.recordMark()
	tid, err := teamFollowSmokeFormTeam(a, b)
	if err != nil {
		fail("F2-team", "%v", err)
	}

	teleport, err := teamFollowSmokeWait(b, bJoinMark, game.SceneMovementClientPlayerNotifyTeleportMessageId, teamFollowSmokeTeleportTimeout,
		func() *scene.TeleportS2C { return &scene.TeleportS2C{} },
		func(t *scene.TeleportS2C) bool { return t.GetReason() == teamFollowSmokeRegroupReason })
	lineSwitched := b.countSince(bJoinMark, game.SceneSceneClientPlayerNotifyEnterSceneMessageId) > 0
	if err != nil {
		fail("F2-regroup-teleport", "B 入队后没有收到 TeleportS2C(reason=%d):%v;期间 B 收到换线 EnterSceneS2C=%v、任意 TeleportS2C=%d 条"+
			"(scene 是否换上了含组队跟随 v1.1 的新二进制?scene 日志搜 PlayerTeam / team_regroup)",
			teamFollowSmokeRegroupReason, err, lineSwitched, b.countSince(bJoinMark, game.SceneMovementClientPlayerNotifyTeleportMessageId))
	}
	landing := teamFollowSmokeFromVector(teleport.GetTransform().GetLocation())
	if d := teamFollowSmokeDistance(landing, posA); d > teamFollowSmokeArriveTolerance {
		fail("F2-regroup-landing", "B 的落点 %s 离 A 的位置 %s 有 %.2f,期望 <= %.1f",
			teamFollowSmokeFormat(landing), teamFollowSmokeFormat(posA), d, teamFollowSmokeArriveTolerance)
	}
	// 真实客户端落位后会从新位置上报;这条上报落在瞬移目标附近,服务器应原样接受(不回拉)。
	posB, err = b.reportPosition(landing)
	if err != nil {
		fail("F2-report-after-teleport", "%v", err)
	}
	if d := teamFollowSmokeDistance(posB, landing); d > teamFollowSmokeArriveTolerance {
		fail("F2-report-after-teleport", "B 在落点 %s 上报后被服务器改到 %s(差 %.2f)",
			teamFollowSmokeFormat(landing), teamFollowSmokeFormat(posB), d)
	}
	zap.L().Info("[team-follow-smoke] F2: B was regrouped to the leader",
		zap.Uint64("team_id", tid), zap.Bool("line_switched", lineSwitched),
		zap.String("landing", teamFollowSmokeFormat(landing)), zap.Uint64("teleport_entity", teleport.GetEntity()))

	// ---- F3:可见性重建(互相收到对方的 ActorCreate) ----
	seenA, err := b.waitActorCreate(bJoinMark, a.gc.PlayerId, teamFollowSmokeVisibleTimeout)
	if err != nil {
		fail("F3-visibility", "B 没有收到队长 A 的 ActorCreate:%v(归队瞬移后可见性没有重建)", err)
	}
	seenB, err := a.waitActorCreate(aJoinMark, b.gc.PlayerId, teamFollowSmokeVisibleTimeout)
	if err != nil {
		fail("F3-visibility", "A 没有收到队员 B 的 ActorCreate:%v(归队瞬移后可见性没有重建)", err)
	}
	entityA, entityB = seenA.GetEntity(), seenB.GetEntity()
	zap.L().Info("[team-follow-smoke] F3: mutual visibility rebuilt",
		zap.Uint64("entity_a_seen_by_b", entityA), zap.Uint64("entity_b_seen_by_a", entityB))

	// ---- F4:移动广播(双向) ----
	if err := teamFollowSmokeBroadcast(a, b, posA, entityA); err != nil {
		fail("F4-broadcast-leader", "%v", err)
	}
	if err := teamFollowSmokeBroadcast(b, a, posB, entityB); err != nil {
		fail("F4-broadcast-member", "%v", err)
	}
	zap.L().Info("[team-follow-smoke] F4: movement is broadcast both ways")

	// ---- F6:请求归队(GetMyTeam{regroup},组队跟随 v1.1 V.9) ----
	// 近处:两人互相可见、相距不到 8,请求归队不应瞬移。
	nearMark := b.recordMark()
	if err := b.expect(game.ClientPlayerTeamGetMyTeamMessageId, &teampb.GetMyTeamRequest{Regroup: true}, &teampb.TeamResponse{}, 0); err != nil {
		fail("F6-regroup-near", "%v", err)
	}
	if _, err := teamFollowSmokeWait(b, nearMark, game.SceneMovementClientPlayerNotifyTeleportMessageId, teamFollowSmokeQuietWindow,
		func() *scene.TeleportS2C { return &scene.TeleportS2C{} }, nil); err == nil {
		fail("F6-regroup-near", "B 就在 A 身边(%.2f)请求归队却被瞬移了", teamFollowSmokeDistance(posB, posA))
	}
	// 走远:B 自己走开 12 以上;不请求归队就没人拉他(这正是要补的缺口)。
	posB, err = teamFollowSmokeWalkAway(b, posB, posA)
	if err != nil {
		fail("F6-walk-away", "%v", err)
	}
	awayMark := b.recordMark()
	if _, err := teamFollowSmokeWait(b, awayMark, game.SceneMovementClientPlayerNotifyTeleportMessageId, teamFollowSmokeQuietWindow,
		func() *scene.TeleportS2C { return &scene.TeleportS2C{} }, nil); err == nil {
		fail("F6-walk-away", "B 走开后没请求归队就被瞬移了:本步要证明瞬移是归队请求触发的")
	}
	// 请求归队:B 在 15s 内收到 TeleportS2C(reason=5),落点在 A 的位置 1 以内,在落点上报不被拉走。
	if err := b.expect(game.ClientPlayerTeamGetMyTeamMessageId, &teampb.GetMyTeamRequest{Regroup: true}, &teampb.TeamResponse{}, 0); err != nil {
		fail("F6-regroup-request", "%v", err)
	}
	rejoin, err := teamFollowSmokeWait(b, awayMark, game.SceneMovementClientPlayerNotifyTeleportMessageId, teamFollowSmokeTeleportTimeout,
		func() *scene.TeleportS2C { return &scene.TeleportS2C{} },
		func(t *scene.TeleportS2C) bool { return t.GetReason() == teamFollowSmokeRegroupReason })
	if err != nil {
		fail("F6-regroup-teleport", "B 在 %s(离 A %.2f)请求归队后没有收到 TeleportS2C(reason=%d):%v"+
			"(match 是否换上了认识 GetMyTeamRequest.regroup 的新二进制?match 日志搜 Regroup requested,scene 日志搜 team_regroup)",
			teamFollowSmokeFormat(posB), teamFollowSmokeDistance(posB, posA), teamFollowSmokeRegroupReason, err)
	}
	rejoinAt := teamFollowSmokeFromVector(rejoin.GetTransform().GetLocation())
	if d := teamFollowSmokeDistance(rejoinAt, posA); d > teamFollowSmokeArriveTolerance {
		fail("F6-regroup-landing", "B 的落点 %s 离 A 的位置 %s 有 %.2f,期望 <= %.1f",
			teamFollowSmokeFormat(rejoinAt), teamFollowSmokeFormat(posA), d, teamFollowSmokeArriveTolerance)
	}
	walkedAway := teamFollowSmokeDistance(posB, posA)
	posB, err = b.reportPosition(rejoinAt)
	if err != nil {
		fail("F6-report-after-teleport", "%v", err)
	}
	if d := teamFollowSmokeDistance(posB, rejoinAt); d > teamFollowSmokeArriveTolerance {
		fail("F6-report-after-teleport", "B 在落点 %s 上报后被服务器改到 %s(差 %.2f)",
			teamFollowSmokeFormat(rejoinAt), teamFollowSmokeFormat(posB), d)
	}
	zap.L().Info("[team-follow-smoke] F6: regroup request brought B back to the leader",
		zap.Float64("walked_away", walkedAway), zap.String("landing", teamFollowSmokeFormat(rejoinAt)))

	// ---- F5:A 解散 ----
	if err := a.expect(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		fail("F5-disband", "%v", err)
	}

	zap.L().Info(fmt.Sprintf("TEAM_FOLLOW_SMOKE_OK team_id=%d player_a=%d player_b=%d", tid, a.gc.PlayerId, b.gc.PlayerId),
		zap.Bool("same_line_before", sameLineBefore), zap.Bool("line_switched", lineSwitched))
	cleanup()
}

// teamFollowSmokeWalkAway 让 walker 离开 anchor 至少 teamFollowSmokeSeparation:目标点按 anchor 的位置算
// (anchor 那个人全程不动),八个方向各试一次,以服务器裁决后的位置为准。返回 walker 的新位置。
func teamFollowSmokeWalkAway(walker *teamFollowSmokeBot, from, anchor *component.Location) (*component.Location, error) {
	directions := [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {0.7071, 0.7071}, {-0.7071, 0.7071}, {0.7071, -0.7071}, {-0.7071, -0.7071}}
	pos := from
	for _, dir := range directions {
		target := &component.Location{X: anchor.GetX() + dir[0]*teamFollowSmokeStride, Y: anchor.GetY() + dir[1]*teamFollowSmokeStride, Z: pos.GetZ()}
		accepted, err := walker.reportPosition(target)
		if err != nil {
			return nil, err
		}
		pos = accepted
		if teamFollowSmokeDistance(pos, anchor) >= teamFollowSmokeSeparation {
			return pos, nil
		}
	}
	return nil, fmt.Errorf("8 个方向都没能让 %s 离开 %s %.0f 以上(停在 %s):四周被导航阻挡?",
		walker.name, teamFollowSmokeFormat(anchor), teamFollowSmokeSeparation, teamFollowSmokeFormat(pos))
}

// teamFollowSmokeFormTeam:leader 建队 → member 申请 → leader 同意,返回 team_id。
func teamFollowSmokeFormTeam(leader, member *teamFollowSmokeBot) (uint64, error) {
	createResp := &teampb.TeamResponse{}
	if err := leader.expect(game.ClientPlayerTeamCreateTeamMessageId, &teampb.CreateTeamRequest{}, createResp, 0); err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}
	tid := createResp.GetTeam().GetTeamId()
	if tid == 0 {
		return 0, fmt.Errorf("create: 建队被受理但视图 team_id=0")
	}
	if err := member.expect(game.ClientPlayerTeamApplyJoinTeamMessageId,
		&teampb.ApplyJoinTeamRequest{TargetPlayerId: leader.gc.PlayerId}, &teampb.TeamResponse{}, 0); err != nil {
		return 0, fmt.Errorf("apply: %w", err)
	}
	approveReq := &teampb.HandleApplicationRequest{ApplicantId: member.gc.PlayerId, Approve: true, ExpectedTeamId: tid}
	if err := leader.expect(game.ClientPlayerTeamHandleApplicationMessageId, approveReq, &teampb.TeamResponse{}, 0); err != nil {
		return 0, fmt.Errorf("approve: %w", err)
	}
	return tid, nil
}

// teamFollowSmokeClientCheck 是 client_follower=true 的主体(C3、C4):
//
//	C3 等真实客户端(DevAutoPilot 登录 followerId 的账号)出现在队长视野里,出现的位置必须在队长(start)1 以内;
//	C4 队长按脚本走 6 段(东西南北来回,每段约 13.5,客户端基础移速 9,每 250ms 一条 MoveSync,段末停 2.5s)。
//	   走得开的每一段都要求:期间收到队员的起步广播(客户端真的在走);队长停下后队员也停下(最后一条 move_state=0),
//	   且停在离队长 0.5~3.0 处(客户端一号槽位在队长身后 1.4),走满全程的段还要求它在队长身后(沿行进方向落后 >= 0.5)。
//	   第一段走得开的另外要求队员的第一条上报来自队长出发点 5 以内。
//	   撞墙走不开的段只记录不断言,至少要有 2 段走得开。
//
// 返回逐段摘要;失败时返回步骤名与原因。
func teamFollowSmokeClientCheck(cfg *config.Config, leader *teamFollowSmokeBot, followerId uint64,
	start *component.Location, since int) (string, string, error) {
	wait := time.Duration(cfg.TeamFollowSmoke.ClientWaitSeconds) * time.Second
	if wait <= 0 {
		wait = teamFollowSmokeClientWait
	}
	// 机器人 B 下线时队长先收到它的 ActorDestroy;since 之后到达的 guid == followerId 的 ActorCreate 才是客户端。
	// 等待期间每 10s 发一次 GetMyTeam,连接上保持有上行。
	var seen *scene.ActorCreateS2C
	deadline := time.Now().Add(wait)
	for seen == nil {
		created, err := leader.waitActorCreate(since, followerId, teamFollowSmokeKeepAlive)
		if err == nil {
			seen = created
			break
		}
		if !time.Now().Before(deadline) {
			return "", "C3-client-visible", fmt.Errorf("真实客户端(账号 %s)没有在 %s 内出现在队长视野里:客户端没启动 / 没登录成功,"+
				"或登录后没有被带到队长所在的线", teamFollowSmokeAccountB, wait)
		}
		if _, err := leader.myTeam(); err != nil {
			return "", "C3-client-visible", fmt.Errorf("等待客户端期间队长连接异常:%w", err)
		}
	}
	follower := seen.GetEntity()
	// 队长在 16 之外,客户端只有被归队瞬移之后才会进入队长视野,所以它出现时的位置就是瞬移落点。
	arrive := teamFollowSmokeFromVector(seen.GetTransform().GetLocation())
	if d := teamFollowSmokeDistance(arrive, start); d > teamFollowSmokeArriveTolerance {
		return "", "C3-regroup", fmt.Errorf("客户端出现在 %s,离队长 %s 有 %.2f(期望 <= %.1f):登录进场后没有被归队瞬移到队长身边",
			teamFollowSmokeFormat(arrive), teamFollowSmokeFormat(start), d, teamFollowSmokeArriveTolerance)
	}
	zap.L().Info("[team-follow-smoke] C3: real client was regrouped next to the leader",
		zap.Uint64("follower_entity", follower), zap.String("follower_pos", teamFollowSmokeFormat(arrive)))
	// 客户端建好本地角色、拉到队伍视图、跟随驱动接管都在这之后的一两秒内。
	time.Sleep(teamFollowSmokeClientSettle)

	legs := [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 0}, {-1, 0}}
	pos := start
	valid := 0
	var parts []string
	for i, dir := range legs {
		legMark := leader.recordMark()
		legStart := pos
		end, err := leader.walkLeg(pos, dir)
		if err != nil {
			return "", "C4-walk", err
		}
		moved := teamFollowSmokeDistance(end, pos)
		pos = end
		time.Sleep(teamFollowSmokeLegSettle)
		walking, first, last := leader.followerMoves(legMark, follower)
		if moved < teamFollowSmokeMinLeg {
			parts = append(parts, fmt.Sprintf("leg%d=blocked(moved=%.1f)", i, moved))
			continue
		}
		if walking == 0 || last == nil {
			return "", "C4-follow", fmt.Errorf("第 %d 段队长走了 %.1f(停在 %s),期间没有收到队员的任何起步广播:客户端没有跟着走",
				i, moved, teamFollowSmokeFormat(pos))
		}
		if valid == 0 {
			// 第一段走得开的:队员的第一条上报必须来自队长出发点附近 —— 客户端确实落在了归队瞬移的位置,
			// 而不是还站在 16 之外的旧位置上(那样它的上报会从远处来)。
			firstPos := teamFollowSmokeFromVector(first.GetTransform().GetLocation())
			if d := teamFollowSmokeDistance(firstPos, legStart); d > teamFollowSmokeRegroupReach {
				return "", "C4-client-snap", fmt.Errorf("队员的第一条移动上报来自 %s,离队长出发点 %s 有 %.2f(期望 <= %.1f):客户端没有落到归队瞬移的位置",
					teamFollowSmokeFormat(firstPos), teamFollowSmokeFormat(legStart), d, teamFollowSmokeRegroupReach)
			}
		}
		lastPos := teamFollowSmokeFromVector(last.GetTransform().GetLocation())
		gap := teamFollowSmokeDistance(lastPos, pos)
		if last.GetMoveState() != 0 {
			// 多等一个窗口再取证:是慢(之后到位了)还是卡住(再也没有上报)。
			time.Sleep(teamFollowSmokeLegSettle)
			return "", "C4-follow", fmt.Errorf("第 %d 段队长 %s → %s 停下 %s 后,队员仍在移动(最后一条 move_state=%d,位置 %s,离队长 %.2f)。"+
				"本段及其后 %s 内队员的全部上报:%s",
				i, teamFollowSmokeFormat(legStart), teamFollowSmokeFormat(pos), teamFollowSmokeLegSettle, last.GetMoveState(),
				teamFollowSmokeFormat(lastPos), gap, teamFollowSmokeLegSettle, leader.followerTrace(legMark, follower))
		}
		if gap < teamFollowSmokeGapMin || gap > teamFollowSmokeGapMax {
			return "", "C4-follow", fmt.Errorf("第 %d 段结束后队员停在 %s,离队长 %s 有 %.2f,期望 %.1f~%.1f(客户端一号槽位在队长身后 1.4)。本段队员的全部上报:%s",
				i, teamFollowSmokeFormat(lastPos), teamFollowSmokeFormat(pos), gap, teamFollowSmokeGapMin, teamFollowSmokeGapMax,
				leader.followerTrace(legMark, follower))
		}
		// 队员必须停在队长**身后**(队长走来的那一侧):沿本段行进方向量,队员相对队长的位移应为负。
		// 只对没撞墙、走满全程的段判(撞墙段的实际行进方向不一定是 dir)。
		along := (lastPos.GetX()-pos.GetX())*dir[0] + (lastPos.GetY()-pos.GetY())*dir[1]
		if moved >= teamFollowSmokeFullLeg && along > -teamFollowSmokeBehindMin {
			return "", "C4-follow", fmt.Errorf("第 %d 段结束后队员停在 %s、队长在 %s:沿行进方向队员相对队长 %+.2f,期望 <= -%.1f(应当跟在身后,不能并排或跑到前面)",
				i, teamFollowSmokeFormat(lastPos), teamFollowSmokeFormat(pos), along, teamFollowSmokeBehindMin)
		}
		valid++
		parts = append(parts, fmt.Sprintf("leg%d=ok(moved=%.1f,gap=%.2f,along=%+.2f,reports=%d)", i, moved, gap, along, walking))
		zap.L().Info("[team-follow-smoke] C4: follower kept up",
			zap.Int("leg", i), zap.Float64("leader_moved", moved), zap.Float64("gap", gap), zap.Float64("along", along), zap.Int("walking_reports", walking),
			zap.String("leader", teamFollowSmokeFormat(pos)), zap.String("follower", teamFollowSmokeFormat(lastPos)))
	}
	if valid < 2 {
		return "", "C4-walk", fmt.Errorf("6 段里只有 %d 段走得开(%s),不足以下结论", valid, strings.Join(parts, " "))
	}
	return strings.Join(parts, " "), "", nil
}

// teamFollowSmokeLogin 与 teamSmokeLogin 同流程,区别只在 RecvLoop 的回调:先留底全部下行消息,再交给 teamSmokeBot。
func teamFollowSmokeLogin(cfg *config.Config, name, account string, stats *metrics.Stats) (*teamFollowSmokeBot, error) {
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

	bot := &teamFollowSmokeBot{teamSmokeBot: &teamSmokeBot{
		name:    name,
		account: account,
		zone:    cfg.ZoneID,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
		caller:  newSmokeCaller(gc, stats, account, teamSmokeRpcTimeout, errTeamSmokeTimeout),
	}}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), teamSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

func (f *teamFollowSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	f.recMu.Lock()
	f.records = append(f.records, teamFollowSmokeRecord{messageId: msg.GetMessageId(), body: msg.GetSerializedMessage()})
	f.recMu.Unlock()
	f.teamSmokeBot.onMessage(client, msg)
}

// recordMark 返回当前留底条数,作为「从这之后到达的消息」的扫描起点。必须在触发动作之前调用。
func (f *teamFollowSmokeBot) recordMark() int {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	return len(f.records)
}

// recordsSince 返回下标 since 起的留底副本。
func (f *teamFollowSmokeBot) recordsSince(since int) []teamFollowSmokeRecord {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	if since > len(f.records) {
		since = len(f.records)
	}
	return append([]teamFollowSmokeRecord(nil), f.records[since:]...)
}

func (f *teamFollowSmokeBot) countSince(since int, messageId uint32) int {
	count := 0
	for _, r := range f.recordsSince(since) {
		if r.messageId == messageId {
			count++
		}
	}
	return count
}

// lastEnterScene 返回最近一次 EnterSceneS2C 的场景信息(登录进场那条一定在留底里:进场就绪信号由它触发)。
func (f *teamFollowSmokeBot) lastEnterScene() *scene.SceneInfoComp {
	var last *scene.SceneInfoComp
	for _, r := range f.recordsSince(0) {
		if r.messageId != game.SceneSceneClientPlayerNotifyEnterSceneMessageId {
			continue
		}
		msg := &scene.EnterSceneS2C{}
		if proto.Unmarshal(r.body, msg) == nil {
			last = msg.GetSceneInfo()
		}
	}
	return last
}

// waitSelfActor 等自己的 ActorCreate(进场时服务器补发,guid == 自己的 player_id),返回实体号与进场坐标。
func (f *teamFollowSmokeBot) waitSelfActor() (uint64, *component.Location, error) {
	self, err := f.waitActorCreate(0, f.gc.PlayerId, teamFollowSmokeSelfTimeout)
	if err != nil {
		return 0, nil, fmt.Errorf("没有收到自己的 ActorCreate(guid=%d):%w", f.gc.PlayerId, err)
	}
	return self.GetEntity(), teamFollowSmokeFromVector(self.GetTransform().GetLocation()), nil
}

// waitActorCreate 从下标 since 起找 guid 对应的 ActorCreate(单条或列表里的一条),返回最先到达的那条。
func (f *teamFollowSmokeBot) waitActorCreate(since int, guid uint64, timeout time.Duration) (*scene.ActorCreateS2C, error) {
	next := since
	deadline := time.Now().Add(timeout)
	for {
		batch := f.recordsSince(next)
		next += len(batch)
		for _, r := range batch {
			switch r.messageId {
			case game.SceneSceneClientPlayerNotifyActorCreateMessageId:
				msg := &scene.ActorCreateS2C{}
				if proto.Unmarshal(r.body, msg) == nil && msg.GetGuid() == guid {
					return msg, nil
				}
			case game.SceneSceneClientPlayerNotifyActorListCreateMessageId:
				list := &scene.ActorListCreateS2C{}
				if proto.Unmarshal(r.body, list) != nil {
					continue
				}
				for _, msg := range list.GetActorList() {
					if msg.GetGuid() == guid {
						return msg, nil
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("account=%s 等 guid=%d 的 ActorCreate %w(%s)", f.account, guid, errTeamSmokeTimeout, timeout)
		}
		time.Sleep(teamSmokePollInterval)
	}
}

// reportPosition 把自己「走到」target:MoveStart + MoveStop 各上报一次 target,返回服务器裁决后的位置。
// 服务器从当前位置向上报点做导航射线:畅通则原样接受(不回包),撞墙则截在阻挡点并回 MoveAck。
func (f *teamFollowSmokeBot) reportPosition(target *component.Location) (*component.Location, error) {
	since := f.recordMark()
	f.inputSeq++
	if err := f.send(game.SceneMovementClientPlayerMoveStartMessageId, &scene.MoveStartC2S{
		StartLocation: target,
		ClientTimeMs:  uint64(time.Now().UnixMilli()),
		InputSeq:      f.inputSeq,
	}); err != nil {
		return nil, err
	}
	f.inputSeq++
	if err := f.send(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
		EndLocation:  target,
		ClientTimeMs: uint64(time.Now().UnixMilli()),
		InputSeq:     f.inputSeq,
	}); err != nil {
		return nil, err
	}
	time.Sleep(teamFollowSmokeAckWindow)

	accepted := target
	for _, r := range f.recordsSince(since) {
		if r.messageId != game.SceneMovementClientPlayerNotifyMoveAckMessageId {
			continue
		}
		ack := &scene.MoveAckS2C{}
		if err := proto.Unmarshal(r.body, ack); err != nil {
			return nil, fmt.Errorf("decode MoveAck account=%s: %w", f.account, err)
		}
		accepted = ack.GetServerLocation()
	}
	return accepted, nil
}

// sendMove 发一条移动上报,不走 teamSmokeBot.send 的 400ms 请求间隔(MoveSync 要按 250ms 的节拍发;
// gate 对移动消息的限流是每窗口 15~20 条,本场景每秒 4 条)。
func (f *teamFollowSmokeBot) sendMove(messageId uint32, request proto.Message) error {
	if err := f.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d account=%s: %w", messageId, f.account, err)
	}
	f.lastRequestAt = time.Now()
	f.stats.MsgSent()
	return nil
}

// lastMoveAck 返回下标 since 之后最后一条 MoveAck 的裁决位置;没有则 nil(上报都被原样接受)。
func (f *teamFollowSmokeBot) lastMoveAck(since int) *component.Location {
	var last *component.Location
	for _, r := range f.recordsSince(since) {
		if r.messageId != game.SceneMovementClientPlayerNotifyMoveAckMessageId {
			continue
		}
		ack := &scene.MoveAckS2C{}
		if proto.Unmarshal(r.body, ack) == nil {
			last = ack.GetServerLocation()
		}
	}
	return last
}

// walkLeg 让自己从 from 朝 dir(单位向量)按客户端基础移速走一段:MoveStart → 每 250ms 一条 MoveSync → MoveStop,
// 与真实客户端的上报节拍一致。服务器回了 MoveAck(撞墙)就停在裁决位置。返回停下时的权威位置。
func (f *teamFollowSmokeBot) walkLeg(from *component.Location, dir [2]float64) (*component.Location, error) {
	since := f.recordMark()
	velocity := &component.Velocity{X: dir[0] * teamFollowSmokeWalkSpeed, Y: dir[1] * teamFollowSmokeWalkSpeed}
	stride := teamFollowSmokeWalkSpeed * teamFollowSmokeSyncInterval.Seconds()
	pos := from

	f.inputSeq++
	if err := f.sendMove(game.SceneMovementClientPlayerMoveStartMessageId, &scene.MoveStartC2S{
		StartLocation: pos,
		Velocity:      velocity,
		ClientTimeMs:  uint64(time.Now().UnixMilli()),
		InputSeq:      f.inputSeq,
	}); err != nil {
		return nil, err
	}
	// 前 N-1 拍发 MoveSync,最后一拍直接发 MoveStop:真实客户端停步时立刻在当前位置上报停步。
	// 停步前不能空拍 —— 观察者按最后一次上报的速度外推,空出来的时间里它们看到的是「冲过头再被拉回」,
	// 跟随的队员会照着那段不存在的路走过去(本场景第一版就这样把队员带到了队长前面)。
	for step := 1; step <= teamFollowSmokeLegSteps; step++ {
		time.Sleep(teamFollowSmokeSyncInterval)
		if ack := f.lastMoveAck(since); ack != nil {
			// 撞墙:服务器已把位置截在阻挡点,就地停步。
			pos = ack
			f.inputSeq++
			if err := f.sendMove(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
				EndLocation:  pos,
				ClientTimeMs: uint64(time.Now().UnixMilli()),
				InputSeq:     f.inputSeq,
			}); err != nil {
				return nil, err
			}
			return pos, nil
		}
		pos = &component.Location{X: pos.GetX() + dir[0]*stride, Y: pos.GetY() + dir[1]*stride, Z: pos.GetZ()}
		f.inputSeq++
		var err error
		if step < teamFollowSmokeLegSteps {
			err = f.sendMove(game.SceneMovementClientPlayerMoveSyncMessageId, &scene.MoveSyncC2S{
				Location:     pos,
				Velocity:     velocity,
				ClientTimeMs: uint64(time.Now().UnixMilli()),
				InputSeq:     f.inputSeq,
			})
		} else {
			err = f.sendMove(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
				EndLocation:  pos,
				ClientTimeMs: uint64(time.Now().UnixMilli()),
				InputSeq:     f.inputSeq,
			})
		}
		if err != nil {
			return nil, err
		}
	}
	// 最后几拍的裁决可能还在路上:停步上报撞墙时服务器截在阻挡点并回 MoveAck(速度已由 MoveStop 清零,不用再发)。
	time.Sleep(teamFollowSmokeAckWindow)
	if ack := f.lastMoveAck(since); ack != nil {
		pos = ack
	}
	return pos, nil
}

// actorMovesOf 把一条下行消息展开成 ActorMoveS2C:单条移动广播是一条;同一 tick 合并发出的移动列表
// (ActorMoveListS2C,例如同一队形的几名队员同时起步、停步)是多条;其它消息或解不开时返回 nil。
func actorMovesOf(r teamFollowSmokeRecord) []*scene.ActorMoveS2C {
	switch r.messageId {
	case game.SceneMovementClientPlayerNotifyActorMoveMessageId:
		msg := &scene.ActorMoveS2C{}
		if proto.Unmarshal(r.body, msg) != nil {
			return nil
		}
		return []*scene.ActorMoveS2C{msg}
	case game.SceneMovementClientPlayerNotifyActorMoveListMessageId:
		list := &scene.ActorMoveListS2C{}
		if proto.Unmarshal(r.body, list) != nil {
			return nil
		}
		return list.GetMoves()
	}
	return nil
}

// followerMoves 统计下标 since 之后 entity 的移动广播(含合并列表里的):返回 move_state != 0 的条数,以及第一条与最后一条。
func (f *teamFollowSmokeBot) followerMoves(since int, entity uint64) (int, *scene.ActorMoveS2C, *scene.ActorMoveS2C) {
	walking := 0
	var first, last *scene.ActorMoveS2C
	for _, r := range f.recordsSince(since) {
		for _, msg := range actorMovesOf(r) {
			if msg.GetEntity() != entity {
				continue
			}
			if msg.GetMoveState() != 0 {
				walking++
			}
			if first == nil {
				first = msg
			}
			last = msg
		}
	}
	return walking, first, last
}

// followerTrace 把下标 since 之后 entity 的移动广播排成一行(失败取证用):`state@(x,y) v=(vx,vy)`,
// 另附期间收到的 ActorDestroy 里有没有它(可见性丢了的话后面的上报就收不到了)。
func (f *teamFollowSmokeBot) followerTrace(since int, entity uint64) string {
	var parts []string
	for _, r := range f.recordsSince(since) {
		if r.messageId == game.SceneSceneClientPlayerNotifyActorDestroyMessageId {
			msg := &scene.ActorDestroyS2C{}
			if proto.Unmarshal(r.body, msg) == nil && msg.GetEntity() == entity {
				parts = append(parts, "DESTROYED(可见性丢失)")
			}
			continue
		}
		for _, msg := range actorMovesOf(r) {
			if msg.GetEntity() != entity {
				continue
			}
			location := msg.GetTransform().GetLocation()
			parts = append(parts, fmt.Sprintf("%d@(%.2f,%.2f) v=(%.1f,%.1f)", msg.GetMoveState(),
				location.GetX(), location.GetY(), msg.GetVelocity().GetX(), msg.GetVelocity().GetY()))
		}
	}
	if len(parts) == 0 {
		return "(无)"
	}
	return strings.Join(parts, " | ")
}

// teamFollowSmokeBroadcast:mover 在 position 原地上报「起步(带速度)→ 停步」,observer 必须按顺序收到
// entity == moverEntity 的 ActorMoveS2C:先 move_state=1 且速度非零,再 move_state=0。
func teamFollowSmokeBroadcast(mover, observer *teamFollowSmokeBot, position *component.Location, moverEntity uint64) error {
	since := observer.recordMark()
	mover.inputSeq++
	if err := mover.send(game.SceneMovementClientPlayerMoveStartMessageId, &scene.MoveStartC2S{
		StartLocation: position,
		Velocity:      &component.Velocity{X: teamFollowSmokeProbeSpeed},
		ClientTimeMs:  uint64(time.Now().UnixMilli()),
		InputSeq:      mover.inputSeq,
	}); err != nil {
		return err
	}
	walking, err := teamFollowSmokeWait(observer, since, game.SceneMovementClientPlayerNotifyActorMoveMessageId, teamFollowSmokeMoveTimeout,
		func() *scene.ActorMoveS2C { return &scene.ActorMoveS2C{} },
		func(m *scene.ActorMoveS2C) bool { return m.GetEntity() == moverEntity && m.GetMoveState() == 1 })
	if err != nil {
		return fmt.Errorf("%s 起步后 %s 没有收到 entity=%d、move_state=1 的 ActorMoveS2C:%w(期间收到任意 ActorMoveS2C %d 条)",
			mover.name, observer.name, moverEntity, err,
			observer.countSince(since, game.SceneMovementClientPlayerNotifyActorMoveMessageId))
	}
	velocity := walking.GetVelocity()
	if math.Abs(velocity.GetX())+math.Abs(velocity.GetY()) == 0 {
		return fmt.Errorf("%s 收到 %s 的起步广播,但速度为零(move_state=%d)", observer.name, mover.name, walking.GetMoveState())
	}

	stopMark := observer.recordMark()
	mover.inputSeq++
	if err := mover.send(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
		EndLocation:  position,
		ClientTimeMs: uint64(time.Now().UnixMilli()),
		InputSeq:     mover.inputSeq,
	}); err != nil {
		return err
	}
	if _, err := teamFollowSmokeWait(observer, stopMark, game.SceneMovementClientPlayerNotifyActorMoveMessageId, teamFollowSmokeMoveTimeout,
		func() *scene.ActorMoveS2C { return &scene.ActorMoveS2C{} },
		func(m *scene.ActorMoveS2C) bool { return m.GetEntity() == moverEntity && m.GetMoveState() == 0 }); err != nil {
		return fmt.Errorf("%s 停步后 %s 没有收到 entity=%d、move_state=0 的 ActorMoveS2C:%w", mover.name, observer.name, moverEntity, err)
	}
	return nil
}

// teamFollowSmokeWait 从下标 since 起按到达顺序扫描留底,返回第一条消息号为 messageId 且满足 pred(nil = 任意)的消息。
// 超时返回包裹 errTeamSmokeTimeout 的 error。
func teamFollowSmokeWait[T proto.Message](bot *teamFollowSmokeBot, since int, messageId uint32, timeout time.Duration,
	newMsg func() T, pred func(T) bool) (T, error) {
	var zero T
	next := since
	deadline := time.Now().Add(timeout)
	for {
		batch := bot.recordsSince(next)
		next += len(batch)
		for _, r := range batch {
			if r.messageId != messageId {
				continue
			}
			msg := newMsg()
			if err := proto.Unmarshal(r.body, msg); err != nil {
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

// Transform.location 是 Vector3,移动协议用结构等价的 Location;在这里换壳。
func teamFollowSmokeFromVector(v *component.Vector3) *component.Location {
	return &component.Location{X: v.GetX(), Y: v.GetY(), Z: v.GetZ()}
}

// teamFollowSmokeDistance 是水平距离(与服务端 HorizontalDistance 同口径:只看 x / y)。
func teamFollowSmokeDistance(p, q *component.Location) float64 {
	return math.Hypot(p.GetX()-q.GetX(), p.GetY()-q.GetY())
}

func teamFollowSmokeFormat(p *component.Location) string {
	return fmt.Sprintf("(%.2f,%.2f,%.2f)", p.GetX(), p.GetY(), p.GetZ())
}
