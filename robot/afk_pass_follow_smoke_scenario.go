package main

// afk-pass-follow-smoke 场景:挂机月卡开通入口 + 离线托管跟随的端到端冒烟(docs/design/afk-monthly-card.md §8)。
//
//	P0 两人登录(A 队长、B 队员),清残留队伍;B 用 GmSetAfkPass(0) 清掉上一轮留下的月卡。
//	P1 B GmAddCurrency(灵石, 一次的价格) → GetAfkPass → BuyAfkPass(键 K):到期 = 服务器此刻 + period_days 天;
//	   同一个 K 再发一次:replayed = true,到期与余额都不变(同一个键只扣一次钱)。
//	   然后 GmSetAfkPass 把剩余缩到 20 分钟:本场景中途失败时,留在世界里的托管角色最晚 20 分钟后自然退出。
//	P2 A 建队、B 入队(两人不在一起时 B 被带到 A 身边)。
//	P3 B 关掉 TCP(不登出)。40 秒内 A 不应收到 B 的 ActorDestroy:普通断线保留 25 秒就退出,留着 = 离线托管生效。
//	P4 A 走一段:A 收到 B 的移动广播,B 停在 A 身后约 3 米(服务器替离线的 B 开启了跟随)。
//	P5 A 依次切到分线目录里的其它线,直到「同节点」「跨节点」各成功一次。每次切完:A 在新场景看到 B
//	   (ActorCreate guid == B,托管落点把没有客户端的 B 带了过来),A 再走一段 B 照样跟。
//	   同节点 / 跨节点的判法:客户端 actor 号的高 32 位是 scene 的 node_id(cpp actor_net_id.h);
//	   比较 A 切线前后自己的 actor 号。这是冒烟工具对服务端编码的白盒依赖,只在 robot 里用。
//	P6 B 重新登录:进场后在 A 身边(接回托管中的实体);GetAfkPass 的到期与 P1 设定的一致(权益随存档走)。
//	P7 清理:B GmSetAfkPass(0),A 解散。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `AFK_PASS_FOLLOW_SMOKE_OK team_id=… player_a=… player_b=… same_node=… cross_node=…`,退出码 0;
//	任一步失败 → `AFK_PASS_FOLLOW_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/component"
	"proto/scene"
	teampb "proto/team"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
)

// 账号落在 team-smoke 预留的 93xx 段里(9301-9306 已被别的冒烟占用),可与它们并行跑。
const (
	afkPassSmokeAccountA = "robot_9307" // 队长
	afkPassSmokeAccountB = "robot_9308" // 队员(开通月卡后断线,由服务器离线托管)
)

const (
	// P1:到期时刻与「服务器此刻 + 天数」比较时允许的秒差(开通与组应答之间跨过一秒)。
	afkPassSmokeExpiryTolerance = 2
	// P1 末尾把剩余缩到这么多秒:P3~P6 最多几分钟,留足余量;中途失败时托管角色最晚这么久之后退出。
	afkPassSmokeGmRemainingSeconds = 1200
	// P3:断开之后观察多久。普通保留 25 秒(connection_lost_hold::kHoldWindow)+ 扫描与退出存盘余量。
	afkPassSmokeHoldWatch = 40 * time.Second
	// P5:切线之后等 NotifyEnterScene;跨节点要走「冻结 → 存盘 → 交接」链路。
	afkPassSmokeArriveTimeout = 30 * time.Second
	// P5:切线之后等托管的 B 出现在 A 的新场景里(跨节点时是托管落点:目标节点认领、载入、入场)。
	afkPassSmokeFollowAppearTimeout = 30 * time.Second
	// P5:列线拿不到目录时的重试节奏与预算(scene_manager 刚当选领导者时目录可能还没发布)。
	afkPassSmokeDirectoryRetry  = 3 * time.Second
	afkPassSmokeDirectoryBudget = 30 * time.Second
	// P5:对目录里的线最多过几轮(切过去再切回来也算一次尝试)。
	afkPassSmokeMaxRounds = 2
	// P6:B 重登后离 A 的允许距离(跟随槽位在身后 3 米,再留出裁决与网络误差)。
	afkPassSmokeRejoinReach = teamFollowSmokeGapMax + 1.0
)

// RunAfkPassFollowSmoke 是 main.go `mode: afk-pass-follow-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunAfkPassFollowSmoke(cfg *config.Config) {
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
		bots = nil
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("AFK_PASS_FOLLOW_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- P0:登录;清残留队伍与上一轮的月卡 ----
	a, err := teamFollowSmokeLogin(cfg, "A", afkPassSmokeAccountA, stats)
	if err != nil {
		fail("P0-login", "account=%s zone=%d err=%v", afkPassSmokeAccountA, cfg.ZoneID, err)
	}
	bots = append(bots, a)
	b, err := teamFollowSmokeLogin(cfg, "B", afkPassSmokeAccountB, stats)
	if err != nil {
		fail("P0-login", "account=%s zone=%d err=%v", afkPassSmokeAccountB, cfg.ZoneID, err)
	}
	bots = append(bots, b)
	for _, bot := range bots {
		if err := bot.leaveAnyTeam(); err != nil {
			fail("P0-prepare", "account=%s: %v", bot.account, err)
		}
	}
	if _, err := afkPassSmokeGmSet(b, 0); err != nil {
		fail("P0-clear-pass", "%v(scene 要以 SCENE_RUN_MODE=dev 启动,GM 指令才放行)", err)
	}
	bId := b.gc.PlayerId

	// ---- P1:开通,同键重发只生效一次 ----
	info, err := afkPassSmokeGet(b)
	if err != nil {
		fail("P1-get", "%v", err)
	}
	if info.GetPrice() == 0 || info.GetPeriodDays() == 0 {
		fail("P1-get", "AfkPassInfo 不带售价(price=%d period_days=%d):AfkPassRule 表没导出或取值不合法,scene 日志搜 [AfkPass]",
			info.GetPrice(), info.GetPeriodDays())
	}
	if err := b.expect(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId, &scene.GmAddCurrencyRequest{
		CurrencyType: info.GetPriceCurrencyType(), Amount: int64(info.GetPrice()),
	}, &scene.GmAddCurrencyResponse{}, 0); err != nil {
		fail("P1-topup", "%v", err)
	}
	key := uint64(time.Now().UnixNano()) | 1 // 非 0;本场景每轮一个新键
	buy := &scene.BuyAfkPassRequest{
		ClientRequestId: key, ExpectedCurrencyType: info.GetPriceCurrencyType(), ExpectedPrice: info.GetPrice(),
	}
	first := &scene.BuyAfkPassResponse{}
	if err := b.expect(game.SceneAfkPassClientPlayerBuyAfkPassMessageId, buy, first, 0); err != nil {
		fail("P1-buy", "%v", err)
	}
	period := uint64(info.GetPeriodDays()) * 86400
	bought := first.GetInfo()
	if first.GetReplayed() {
		fail("P1-buy", "第一次开通就回了 replayed = true(键 %d 撞了旧账本?)", key)
	}
	if got, want := bought.GetExpiresAt(), bought.GetServerNow()+period; got+afkPassSmokeExpiryTolerance < want || got > want+afkPassSmokeExpiryTolerance {
		fail("P1-buy", "开通后到期 %d,期望约为服务器此刻 %d + %d 天 = %d(P0 已把月卡清零,应从现在起算)",
			got, bought.GetServerNow(), info.GetPeriodDays(), want)
	}
	replay := &scene.BuyAfkPassResponse{}
	if err := b.expect(game.SceneAfkPassClientPlayerBuyAfkPassMessageId, buy, replay, 0); err != nil {
		fail("P1-replay", "%v", err)
	}
	if !replay.GetReplayed() || replay.GetInfo().GetExpiresAt() != bought.GetExpiresAt() || replay.GetInfo().GetBalance() != bought.GetBalance() {
		fail("P1-replay", "同一个键重发:replayed=%v 到期 %d→%d 余额 %d→%d,期望 replayed=true 且到期、余额都不变",
			replay.GetReplayed(), bought.GetExpiresAt(), replay.GetInfo().GetExpiresAt(), bought.GetBalance(), replay.GetInfo().GetBalance())
	}
	shortened, err := afkPassSmokeGmSet(b, afkPassSmokeGmRemainingSeconds)
	if err != nil {
		fail("P1-shorten", "%v", err)
	}
	zap.L().Info("[afk-pass-follow-smoke] P1: AFK pass bought once and the replay was recognised",
		zap.Uint64("b", bId), zap.Uint64("bought_expires_at", bought.GetExpiresAt()),
		zap.Uint64("shortened_expires_at", shortened.GetExpiresAt()), zap.Uint64("price", info.GetPrice()))

	// ---- P2:组队 ----
	tid, err := teamFollowSmokeFormTeam(a, b)
	if err != nil {
		fail("P2-team", "%v", err)
	}
	// 两人不在一起时 B 被带到 A 身边(归队瞬移;不同线时先换线,可能跨节点),等到两人同线、A 看得见 B。
	selfA, seenB, err := afkPassSmokeWaitTogether(a, b, afkPassSmokeFollowAppearTimeout)
	if err != nil {
		fail("P2-together", "%v", err)
	}
	posA := teamFollowSmokeFromVector(selfA.GetTransform().GetLocation())
	entityB := seenB.GetEntity()

	// ---- P3:B 断线(关 TCP,不登出),离线托管让它留在世界里 ----
	holdMark := a.recordMark()
	gameobject.PlayerList.Delete(b.gc.PlayerId)
	b.gc.Close()
	bots = []*teamFollowSmokeBot{a}
	time.Sleep(afkPassSmokeHoldWatch)
	if afkPassSmokeDestroyedSince(a, holdMark, entityB) {
		fail("P3-hosting", "B 断线后 %s 内从 A 的视野里消失了:没有离线托管(scene 日志搜 metric=afk_hosting_started 与 [AfkPass];"+
			"玩家域 Redis 的 player:afk_pass:{%d} 是不是在提交之后写出来了)", afkPassSmokeHoldWatch, bId)
	}

	// ---- P4:同一场景里,服务器替离线的 B 跟着队长走 ----
	posA, leg, err := afkPassSmokeLegFollowed(a, posA, entityB)
	if err != nil {
		fail("P4-follow", "%v", err)
	}
	zap.L().Info("[afk-pass-follow-smoke] P4: the hosted member follows the leader", zap.String("leg", leg))

	// ---- P5:队长换线,同节点、跨节点各一次 ----
	dir, err := afkPassSmokeDirectory(a)
	if err != nil {
		fail("P5-directory", "%v", err)
	}
	if !dir.GetSwitchEnabled() {
		fail("P5-directory", "服务端关闭了玩家主动切线(switch_enabled=false),换不了线")
	}
	current := a.lastEnterScene()
	nodeA := selfA.GetEntity() >> 32
	cooldown := time.Duration(dir.GetSwitchCooldownSeconds()+1) * time.Second
	var lastSwitch time.Time
	sameNode, crossNode := "", ""
	for round := 0; round < afkPassSmokeMaxRounds && (sameNode == "" || crossNode == ""); round++ {
		for _, channel := range dir.GetChannels() {
			if sameNode != "" && crossNode != "" {
				break
			}
			if channel.GetSceneId() == current.GetSceneId() || !channelSmokeSelectable(channel.GetState()) {
				continue
			}
			if wait := cooldown - time.Since(lastSwitch); wait > 0 {
				time.Sleep(wait)
			}
			mark := a.recordMark()
			if err := afkPassSmokeSwitch(a, current.GetSceneConfigId(), channel.GetSceneId()); err != nil {
				fail("P5-switch", "切到 %d线(scene_id=%d):%v", channel.GetChannelNo(), channel.GetSceneId(), err)
			}
			lastSwitch = time.Now()
			self, err := a.waitActorCreate(mark, a.gc.PlayerId, teamFollowSmokeSelfTimeout)
			if err != nil {
				fail("P5-self", "切到 %d线后:%v", channel.GetChannelNo(), err)
			}
			node := self.GetEntity() >> 32
			kind := "same_node"
			if node != nodeA {
				kind = "cross_node"
			}
			member, err := a.waitActorCreate(mark, bId, afkPassSmokeFollowAppearTimeout)
			if err != nil {
				fail("P5-"+kind, "A 切到 %d线(node %d → %d)之后托管的 B 没有跟过来:%v(scene 日志搜 [HostedPlacement] 与 hosted_placement)",
					channel.GetChannelNo(), nodeA, node, err)
			}
			posA = teamFollowSmokeFromVector(self.GetTransform().GetLocation())
			posA, leg, err = afkPassSmokeLegFollowed(a, posA, member.GetEntity())
			if err != nil {
				fail("P5-"+kind+"-follow", "切到 %d线之后:%v", channel.GetChannelNo(), err)
			}
			summary := fmt.Sprintf("%d线(node %d→%d,%s)", channel.GetChannelNo(), nodeA, node, leg)
			zap.L().Info("[afk-pass-follow-smoke] P5: the hosted member followed the leader to another line",
				zap.String("kind", kind), zap.String("detail", summary))
			if kind == "cross_node" && crossNode == "" {
				crossNode = summary
			} else if kind == "same_node" && sameNode == "" {
				sameNode = summary
			}
			nodeA = node
			current = a.lastEnterScene()
			entityB = member.GetEntity()
		}
	}
	if sameNode == "" || crossNode == "" {
		fail("P5-coverage", "过了 %d 轮分线仍没有同时覆盖同节点与跨节点(same_node=%q cross_node=%q,目录:%s)。"+
			"要起两个 scene 节点,并给这张地图配至少 3 条分线(scene_manager WorldChannelCountByConfId),"+
			"让两个节点上都有它的线", afkPassSmokeMaxRounds, sameNode, crossNode, channelSmokeDescribe(dir))
	}

	// ---- P6:B 重登,接回托管中的实体;月卡随存档走 ----
	b, err = teamFollowSmokeLogin(cfg, "B", afkPassSmokeAccountB, stats)
	if err != nil {
		fail("P6-relogin", "%v", err)
	}
	bots = append(bots, b)
	if got, want := b.lastEnterScene().GetSceneId(), current.GetSceneId(); got != want {
		fail("P6-relogin", "B 重登进了 scene_id=%d,期望回到托管时所在的队长那条线 scene_id=%d", got, want)
	}
	_, posB, err := b.waitSelfActor()
	if err != nil {
		fail("P6-relogin", "%v", err)
	}
	if d := teamFollowSmokeDistance(posB, posA); d > afkPassSmokeRejoinReach {
		fail("P6-relogin", "B 重登后在 %s,离 A(%s)%.2f,期望 <= %.1f(接回的应是跟在 A 身后的托管实体)",
			teamFollowSmokeFormat(posB), teamFollowSmokeFormat(posA), d, afkPassSmokeRejoinReach)
	}
	after, err := afkPassSmokeGet(b)
	if err != nil {
		fail("P6-pass", "%v", err)
	}
	if after.GetExpiresAt() != shortened.GetExpiresAt() {
		fail("P6-pass", "重登后月卡到期 %d,期望与 P1 设定的 %d 相同(权益随存档提交,托管与换节点都不该改它)",
			after.GetExpiresAt(), shortened.GetExpiresAt())
	}

	// ---- P7:清理 ----
	if _, err := afkPassSmokeGmSet(b, 0); err != nil {
		fail("P7-clear-pass", "%v", err)
	}
	if err := a.expect(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		fail("P7-disband", "%v", err)
	}

	zap.L().Info(fmt.Sprintf("AFK_PASS_FOLLOW_SMOKE_OK team_id=%d player_a=%d player_b=%d same_node=%q cross_node=%q",
		tid, a.gc.PlayerId, bId, sameNode, crossNode))
	cleanup()
}

// afkPassSmokeGet 查一次月卡。
func afkPassSmokeGet(bot *teamFollowSmokeBot) (*scene.AfkPassInfo, error) {
	resp := &scene.GetAfkPassResponse{}
	if err := bot.expect(game.SceneAfkPassClientPlayerGetAfkPassMessageId, &scene.GetAfkPassRequest{}, resp, 0); err != nil {
		return nil, fmt.Errorf("GetAfkPass: %w", err)
	}
	if resp.GetInfo() == nil {
		return nil, fmt.Errorf("GetAfkPass 应答不带 info")
	}
	return resp.GetInfo(), nil
}

// afkPassSmokeGmSet 用 GM 指令把剩余时长设成 remainingSeconds(0 = 清除)。
func afkPassSmokeGmSet(bot *teamFollowSmokeBot, remainingSeconds uint32) (*scene.AfkPassInfo, error) {
	resp := &scene.GmSetAfkPassResponse{}
	if err := bot.expect(game.SceneAfkPassClientPlayerGmSetAfkPassMessageId,
		&scene.GmSetAfkPassRequest{RemainingSeconds: remainingSeconds}, resp, 0); err != nil {
		return nil, fmt.Errorf("GmSetAfkPass(%d): %w", remainingSeconds, err)
	}
	return resp.GetInfo(), nil
}

// afkPassSmokeLastActorCreate 返回留底里 guid 对应的最后一条 ActorCreate(单条或列表里的一条);没有返回 nil。
// 取最后一条:换线之后实体在新场景重新创建,actor 号可能变了(跨节点时一定变)。
func afkPassSmokeLastActorCreate(bot *teamFollowSmokeBot, guid uint64) *scene.ActorCreateS2C {
	var last *scene.ActorCreateS2C
	for _, r := range bot.recordsSince(0) {
		switch r.messageId {
		case game.SceneSceneClientPlayerNotifyActorCreateMessageId:
			msg := &scene.ActorCreateS2C{}
			if proto.Unmarshal(r.body, msg) == nil && msg.GetGuid() == guid {
				last = msg
			}
		case game.SceneSceneClientPlayerNotifyActorListCreateMessageId:
			list := &scene.ActorListCreateS2C{}
			if proto.Unmarshal(r.body, list) != nil {
				continue
			}
			for _, msg := range list.GetActorList() {
				if msg.GetGuid() == guid {
					last = msg
				}
			}
		}
	}
	return last
}

// afkPassSmokeWaitTogether 等到 leader 与 member 在同一条线(两人最近一次进场的 scene_id 相同),且 leader 留底里有
// member 的 ActorCreate;返回 leader 自己与它看到的 member 的最后一条 ActorCreate。
func afkPassSmokeWaitTogether(leader, member *teamFollowSmokeBot, timeout time.Duration) (*scene.ActorCreateS2C, *scene.ActorCreateS2C, error) {
	deadline := time.Now().Add(timeout)
	for {
		sameLine := leader.lastEnterScene().GetSceneId() == member.lastEnterScene().GetSceneId()
		self := afkPassSmokeLastActorCreate(leader, leader.gc.PlayerId)
		seen := afkPassSmokeLastActorCreate(leader, member.gc.PlayerId)
		if sameLine && self != nil && seen != nil {
			return self, seen, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil, fmt.Errorf("组队后 %s 内两人没到一起:同线=%v A 看到自己=%v A 看到 B=%v(scene 日志搜 team_regroup)",
				timeout, sameLine, self != nil, seen != nil)
		}
		time.Sleep(teamSmokePollInterval)
	}
}

// afkPassSmokeDestroyedSince 判断下标 since 之后 bot 有没有收到 entity 的销毁(单条或列表)。
func afkPassSmokeDestroyedSince(bot *teamFollowSmokeBot, since int, entity uint64) bool {
	for _, r := range bot.recordsSince(since) {
		switch r.messageId {
		case game.SceneSceneClientPlayerNotifyActorDestroyMessageId:
			msg := &scene.ActorDestroyS2C{}
			if proto.Unmarshal(r.body, msg) == nil && msg.GetEntity() == entity {
				return true
			}
		case game.SceneSceneClientPlayerNotifyActorListDestroyMessageId:
			list := &scene.ActorListDestroyS2C{}
			if proto.Unmarshal(r.body, list) != nil {
				continue
			}
			for _, id := range list.GetEntity() {
				if id == entity {
					return true
				}
			}
		}
	}
	return false
}

// afkPassSmokeLegFollowed:leader 从 from 走一段(东西南北依次试,撞墙走不开的换方向),停下后断言 follower 跟着走了:
// 期间收到它的起步广播,最后一条是停步,停在离 leader 2.0~4.6 处(一号槽位在身后 3)。返回 leader 的新位置与本段摘要。
func afkPassSmokeLegFollowed(leader *teamFollowSmokeBot, from *component.Location, follower uint64) (*component.Location, string, error) {
	directions := [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for _, dir := range directions {
		mark := leader.recordMark()
		pos, err := leader.walkLeg(from, dir)
		if err != nil {
			return nil, "", err
		}
		walked := teamFollowSmokeDistance(pos, from)
		if walked < teamFollowSmokeMinLeg {
			from = pos // 撞墙:换个方向再走
			continue
		}
		time.Sleep(teamFollowSmokeLegSettle)
		walking, _, last := leader.followerMoves(mark, follower)
		if walking == 0 || last == nil {
			return nil, "", fmt.Errorf("A 走了 %.1f,没收到托管队员(entity=%d)的起步广播:%s",
				walked, follower, leader.followerTrace(mark, follower))
		}
		if last.GetMoveState() != 0 {
			return nil, "", fmt.Errorf("A 停下 %s 之后,托管队员最后一条移动广播仍是 move_state=%d:%s",
				teamFollowSmokeLegSettle, last.GetMoveState(), leader.followerTrace(mark, follower))
		}
		stop := teamFollowSmokeFromVector(last.GetTransform().GetLocation())
		gap := teamFollowSmokeDistance(stop, pos)
		if gap < teamFollowSmokeGapMin || gap > teamFollowSmokeGapMax {
			return nil, "", fmt.Errorf("托管队员停在 %s,离 A(%s)%.2f,期望 %.1f~%.1f(跟随间距 3):%s",
				teamFollowSmokeFormat(stop), teamFollowSmokeFormat(pos), gap, teamFollowSmokeGapMin, teamFollowSmokeGapMax,
				leader.followerTrace(mark, follower))
		}
		return pos, fmt.Sprintf("walked=%.1f gap=%.2f moves=%d", walked, gap, walking), nil
	}
	return nil, "", fmt.Errorf("A 在 %s 四个方向都走不出 %.1f(四周被导航阻挡?)", teamFollowSmokeFormat(from), teamFollowSmokeMinLeg)
}

// afkPassSmokeDirectory 列线:发 SceneInfoC2S{with_channel_directory},等带目录的 NotifySceneInfo。
// 不带目录时(scene_manager 还没发布)按 afkPassSmokeDirectoryRetry 重试,最多 afkPassSmokeDirectoryBudget。
func afkPassSmokeDirectory(bot *teamFollowSmokeBot) (*scene.SceneChannelDirectory, error) {
	deadline := time.Now().Add(afkPassSmokeDirectoryBudget)
	for {
		mark := bot.recordMark()
		if err := bot.send(game.SceneSceneClientPlayerSceneInfoC2SMessageId, &scene.SceneInfoRequest{WithChannelDirectory: true}); err != nil {
			return nil, err
		}
		push, err := teamFollowSmokeWait(bot, mark, game.SceneSceneClientPlayerNotifySceneInfoMessageId, afkPassSmokeDirectoryRetry,
			func() *scene.SceneInfoS2C { return &scene.SceneInfoS2C{} },
			func(p *scene.SceneInfoS2C) bool { return p.GetChannelDirectory() != nil })
		if err == nil {
			return push.GetChannelDirectory(), nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s 内 NotifySceneInfo 始终不带 channel_directory(本冒烟要求在大世界地图上;"+
				"scene_manager 是否有领导者在发布目录):%w", afkPassSmokeDirectoryBudget, err)
		}
	}
}

// afkPassSmokeSwitch 请求切到 target 并等 scene_id == target 的 NotifyEnterScene。同步应答带 tip、受理后被拒
// (SendTipToClient)、进了别的场景、等不到结局都返回 error。
func afkPassSmokeSwitch(bot *teamFollowSmokeBot, sceneConfigId uint32, target uint64) error {
	mark := bot.recordMark()
	if err := bot.expect(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: sceneConfigId, SceneId: target},
	}, &scene.EnterSceneC2SResponse{}, 0); err != nil {
		return fmt.Errorf("EnterScene 没被受理:%w", err)
	}
	entered, err := teamFollowSmokeWait(bot, mark, game.SceneSceneClientPlayerNotifyEnterSceneMessageId, afkPassSmokeArriveTimeout,
		func() *scene.EnterSceneS2C { return &scene.EnterSceneS2C{} }, nil)
	if err != nil {
		tips := bot.countSince(mark, game.SceneClientPlayerCommonSendTipToClientMessageId)
		return fmt.Errorf("已受理但没有到达:%w(期间收到 SendTipToClient %d 条:受理后被拒时玩家留在原线)", err, tips)
	}
	if got := entered.GetSceneInfo().GetSceneId(); got != target {
		return fmt.Errorf("请求切到 scene_id=%d,却进了 scene_id=%d", target, got)
	}
	return nil
}
