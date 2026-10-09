package main

// channel-smoke 场景:单机器人对「玩家主动切线」做端到端冒烟
// (docs/design/world-channel-switch.md §3 协议 / §4.4 选线预检 / §10 验证清单):
//
//	C1 登录进场,记下当前 scene_id / scene_config_id(登录时 scene_manager 自动分到的那条线,下称"原线");
//	C2 列线:SceneInfoC2S(43)→ NotifySceneInfo(31)必须带 channel_directory。目录由 scene_manager 领导者
//	   每 2s 发布一次,不带就隔 3s 重试、最多试到 15s。目录形状:scene_config_id == 当前地图;channels 非空;
//	   channel_no 严格递增且 ≥ 1;恰有一条的 scene_id == 当前 scene_id;switch_enabled == true;
//	C3 选目标线:第一条 scene_id != 当前、state 为 SMOOTH / BUSY 的线。该地图只有 1 条线 → CHANNEL_SMOKE_SKIP;
//	C4 切线:EnterScene(63){scene_config_id = 当前地图, scene_id = 目标线},同步应答必须无错(= 已受理);
//	   到达 = 随后收到 NotifyEnterScene(79)且 scene_id == 目标线;
//	C5 再列线:恰有一条的 scene_id == 目标线,且两次目录里都出现的线 channel_no 不变(线号在线的生命周期内不变);
//	C6 冷却(目录 switch_cooldown_seconds > 0 才做):立刻切回原线 → 同步应答无错(已受理),5s 内收到
//	   SendTipToClient(23)且 id == kEnterSceneFailed,期间没有 scene_id == 原线的 NotifyEnterScene;
//	   等冷却过期(从 C4 到达起算 switch_cooldown_seconds + 1s)再切回原线 → 成功;
//	C7 负例:请求切到当前所在的线 → 同步应答 tip == kEnterSceneYouInCurrentScene。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `CHANNEL_SMOKE_OK player_id=… zone=… scene_config_id=… …`,退出码 0;
//	该地图只有 1 条线 → `CHANNEL_SMOKE_SKIP reason=single_channel …`,退出码 0。前置条件不满足不算失败,
//	  先例是帮会经济段:今日次数用完时走 degraded 分支并以 0 退出(guild_economy_smoke.go);
//	任一步失败 → `CHANNEL_SMOKE_FAIL step=… reason=…`,退出码 1。
//	每步通过另打一行 `[channel-smoke] PASS step=…`;某一步内被跳过的子断言打 `[channel-smoke] SKIP step=…`。
//
// 与其它冒烟不一样、不照做就会假绿或误报的三件事:
//   - 43 的应答类型是 Empty(scene 不回包),数据走 31 推送。31 由通用 handler 记到 gameobject.Player
//     (SetSceneInfoPush,带自增序号),本场景按序号等"这次请求带回来的那一份";其余消息(63 的回包、79、23、34,
//     以及 gate 对 43 的信封拒绝)按到达顺序留底,每步在发请求**之前**记下标 mark、之后从 mark 起扫描 ——
//     上一步的推送不会被这一步误认;
//   - 切线受理后的失败没有细分码:scene_manager 的任何拒绝(满 / 回收中 / 冷却)在 scene 侧一律变成
//     kEnterSceneFailed(设计文档 §3.3)。所以 C6 只能断言"被拒",分不出是不是因为冷却;C4 被拒时若目录有冷却,
//     按"撞上了上一轮留下的冷却"等它过期后重试一次(同账号连着跑两轮必然遇到),重试仍被拒才判失败;
//   - 限流:43 / 63 都不在 MessageLimiter 表里,吃 gate 默认档(任意连续两个整秒内最多 3 次,超了回
//     kRateLimitExceeded 并累计非法包计数)。同一机器人相邻请求至少隔 channelSmokeRequestSpacing,
//     两次 43 之间至少隔 channelSmokeListSpacing。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
// 98xx 与 battle-smoke(9001-9004)、chat-smoke(9005/9006)、属性(9101)、宠物(9102)、guild-smoke(92xx)、
// team-smoke(93xx)、trade-smoke(94xx)、资产通道预留(95xx)、travel-smoke(9601)、friend-smoke(97xx)错开,
// 避免并行冒烟互相顶号。账号必须**不在任何队伍里**:队员切线后会被同节点的队长拉回去(设计文档 §6)。
const channelSmokeAccount = "robot_9801"

const (
	// 单次请求等同步应答 / 等 NotifySceneInfo 的预算。列线与同步应答都是本地毫秒级;10s 覆盖 gate→scene 首次路由建立。
	channelSmokeRpcTimeout = 10 * time.Second
	// 首登等进场的预算,与 prepareBehaviorClient / team-smoke 同值。
	channelSmokeSceneReadyTimeout = 15 * time.Second
	// 同一机器人相邻两个请求的最小间隔。1.1s 间隔下任一请求与前两次的秒差都 ≥ 2,gate 默认限流档
	// (3 次 / 窗口,推导见 chat_smoke_scenario.go 的 chatSmokeRequestSpacing)的缓冲区至多 2 条。
	channelSmokeRequestSpacing = 1100 * time.Millisecond
	// 两次 SceneInfoC2S 之间的最小间隔,与客户端单一发送出口的 2.5s 同值(设计文档 §7)。
	channelSmokeListSpacing = 2500 * time.Millisecond
	// 目录缺失时的重试节奏与总预算:scene_manager 每 2s 发布一次,3s 后重试必然跨过一个发布周期;
	// 15s 覆盖领导者切换后的第一轮发布。
	channelSmokeDirectoryRetryInterval = 3 * time.Second
	channelSmokeDirectoryRetryBudget   = 15 * time.Second
	// C6:冷却期内切回被受理之后,等失败 tip 的窗口。拒绝发生在 scene_manager 的预检(不走交接),本地毫秒级。
	channelSmokeRejectTimeout = 5 * time.Second
	// C6:等冷却过期时在 switch_cooldown_seconds 之上多等的余量(覆盖往返延迟与过期判定的时钟误差)。
	channelSmokeCooldownMargin = time.Second
	// C6:做"冷却期内被拒"断言所需的最小剩余冷却。剩得更少时请求可能在冷却过期后才到 scene_manager、
	// 被正常放行,断言会误报;此时跳过这条子断言(打 SKIP),只验"过期后能切回"。
	channelSmokeCooldownMinRemaining = 2 * time.Second
	// 留底扫描的轮询粒度。
	channelSmokePollInterval = 50 * time.Millisecond
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipChannelEnterSceneFailed  = uint32(tiptable.SceneError_kEnterSceneFailed)
	tipChannelYouInCurrentScene = uint32(tiptable.SceneError_kEnterSceneYouInCurrentScene)
	tipChannelChangingScene     = uint32(tiptable.SceneError_kEnterSceneChangingScene)
	tipChannelRateLimited       = uint32(tiptable.CommonError_kRateLimitExceeded)
)

// errChannelSmokeNoOutcome:一次已受理的切线请求在等待窗口内既没有到达也没有失败 tip。
// 单列成哨兵是因为两处调用方要补的说明不同(等到达 vs 等冷却的拒绝)。
var errChannelSmokeNoOutcome = errors.New("在等待窗口内既没有收到 NotifyEnterScene 也没有收到失败 tip")

// channelSmokeRecord 是一条留底消息。
type channelSmokeRecord struct {
	messageId uint32
	// gate 级拒绝(限流 / 不认识的消息号)走信封 MessageContent.error_message,此时 body 为空;推送恒为 0。
	envelopeTip uint32
	body        []byte
}

// channelSmokeBot 是本场景唯一的机器人会话。
type channelSmokeBot struct {
	account string
	gate    string // assign-gate 分配到的 gate 地址(host:port),只用于日志
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// RecvLoop goroutine 写、主流程读,mu 保护。records 只追加。
	mu      sync.Mutex
	records []channelSmokeRecord

	// 以下只由主流程读写。
	lastRequestAt time.Time // 上一个请求(任意消息号)的发出时刻
	lastListAt    time.Time // 上一次 SceneInfoC2S 的发出时刻
}

// RunChannelSmoke 是 main.go `mode: channel-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过或因只有 1 条线而跳过则正常返回(退出码 0)。
func RunChannelSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	arriveTimeout := time.Duration(cfg.ChannelSmoke.ArriveTimeoutSeconds) * time.Second
	maxCooldownWait := time.Duration(cfg.ChannelSmoke.MaxCooldownWaitSeconds) * time.Second

	var bot *channelSmokeBot
	cleanup := func() {
		if bot == nil {
			return
		}
		_ = leaveGame(bot.gc, stats)
		sendDisconnectBestEffort(bot.gc)
		gameobject.PlayerList.Delete(bot.gc.PlayerId)
		bot.gc.Close()
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("CHANNEL_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[channel-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}
	skip := func(step, format string, args ...any) {
		zap.L().Warn(fmt.Sprintf("[channel-smoke] SKIP step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- C1:登录进场 ----
	var err error
	bot, err = channelSmokeLogin(cfg, channelSmokeAccount, stats)
	if err != nil {
		fail("C1-login", "account=%s zone=%d err=%v", channelSmokeAccount, cfg.ZoneID, err)
	}
	// WaitSceneReady 返回时 NotifyEnterScene 的 handler 已经先写完场景信息(先 SetSceneInfo 后 Signal),读到的是进场那一条。
	originScene := bot.player.GetSceneID()
	sceneConfigId := bot.player.GetSceneConfigID()
	if originScene == 0 || sceneConfigId == 0 {
		fail("C1-login", "进场后 scene_id=%d scene_config_id=%d,两者都必须非 0", originScene, sceneConfigId)
	}
	pass("C1-login", "player_id=%d gate=%s scene_config_id=%d scene_id=%d",
		bot.gc.PlayerId, bot.gate, sceneConfigId, originScene)

	// ---- C2:列线 ----
	firstPush, err := bot.fetchDirectory()
	if err != nil {
		fail("C2-list", "%v", err)
	}
	if err := channelSmokeCheckDirectory(firstPush, cfg.ZoneID, sceneConfigId, originScene); err != nil {
		fail("C2-directory", "%v", err)
	}
	firstDir := firstPush.GetChannelDirectory()
	originNo := channelSmokeFindChannel(firstDir, originScene).GetChannelNo()
	pass("C2-list", "zone=%d channels=%d origin_channel=%d switch_cooldown_seconds=%d max_players_per_channel=%d updated_at_ms=%d 目录:%s",
		firstDir.GetZoneId(), len(firstDir.GetChannels()), originNo, firstDir.GetSwitchCooldownSeconds(),
		firstDir.GetMaxPlayersPerChannel(), firstDir.GetUpdatedAtMs(), channelSmokeDescribe(firstDir))

	// ---- C3:选目标线 ----
	if len(firstDir.GetChannels()) == 1 {
		zap.L().Warn(fmt.Sprintf("CHANNEL_SMOKE_SKIP reason=single_channel player_id=%d zone=%d scene_config_id=%d scene_id=%d "+
			"该地图只有 1 条线,无法验证切线;把 scene_manager 的 WorldChannelCountByConfId 里这张图配成 ≥2 再跑(列线已验证通过)",
			bot.gc.PlayerId, firstDir.GetZoneId(), sceneConfigId, originScene))
		cleanup()
		_ = zap.L().Sync()
		return
	}
	target := channelSmokePickTarget(firstDir, originScene)
	if target == nil {
		// 不按 SKIP 处理:单机器人的环境里别的线全都不可选,要么环境坏了(节点判死 / 人数计数没对账),
		// 要么目录的状态判定有缺陷 —— 后者正是本冒烟要抓的。
		fail("C3-pick", "该地图有 %d 条线,但除当前线外没有 SMOOTH / BUSY 的线可选:%s。"+
			"UNAVAILABLE = 线所在的 scene 节点不存活或 scene:{id}:node 映射缺失;FULL = instance:{id}:player_count 达到每线上限 %d"+
			"(节点死亡后旧计数不清零是已知问题,设计文档 §9 第 3 条);CLOSING = 线在回收中",
			len(firstDir.GetChannels()), channelSmokeDescribe(firstDir), firstDir.GetMaxPlayersPerChannel())
	}
	targetScene, targetNo := target.GetSceneId(), target.GetChannelNo()
	pass("C3-pick", "target_scene=%d target_channel=%d state=%s player_count=%d",
		targetScene, targetNo, target.GetState(), target.GetPlayerCount())

	// ---- C4:切到目标线 ----
	cooldown := time.Duration(firstDir.GetSwitchCooldownSeconds()) * time.Second
	switchStart := time.Now()
	retriedAfterCooldown := false
	for {
		rejectTip, err := bot.switchTo(sceneConfigId, targetScene, arriveTimeout)
		if err != nil {
			fail("C4-switch", "%v", err)
		}
		if rejectTip == 0 {
			break
		}
		// 受理后被拒。唯一值得等一等再试的是"上一轮冒烟最后一次切线留下的冷却还没过"(失败码不细分,只能按条件推断)。
		leftoverCooldown := !retriedAfterCooldown && rejectTip == tipChannelEnterSceneFailed &&
			cooldown > 0 && cooldown+channelSmokeCooldownMargin <= maxCooldownWait
		if !leftoverCooldown {
			fail("C4-switch", "切到目标线 scene_id=%d(%d线)受理后被拒 tip=%d%s retried_after_cooldown=%t。"+
				"scene_manager 的拒绝原因看它的日志(channel_closing / channel_conf_mismatch / channel_switch_disabled / "+
				"channel_full / channel_switch_cooldown)",
				targetScene, targetNo, rejectTip, channelSmokeTipHint(rejectTip), retriedAfterCooldown)
		}
		retriedAfterCooldown = true
		wait := cooldown + channelSmokeCooldownMargin
		zap.L().Warn("[channel-smoke] C4 受理后被拒,按「上一轮留下的切线冷却未过」等它过期后重试一次",
			zap.Uint32("tip", rejectTip), zap.Duration("wait", wait))
		time.Sleep(wait)
	}
	// 冷却从 scene_manager 应答成功(或跨节点第一跳的 18)那一刻起算,必然早于机器人看到 NotifyEnterScene 的此刻;
	// 以此刻为起点等 switch_cooldown_seconds,只会多等、不会少等。
	switchedAt := time.Now()
	pass("C4-switch", "target_scene=%d target_channel=%d elapsed=%s retried_after_cooldown=%t",
		targetScene, targetNo, switchedAt.Sub(switchStart).Round(time.Millisecond), retriedAfterCooldown)

	// ---- C5:再列线 ----
	secondPush, err := bot.fetchDirectory()
	if err != nil {
		fail("C5-list", "%v", err)
	}
	if err := channelSmokeCheckDirectory(secondPush, cfg.ZoneID, sceneConfigId, targetScene); err != nil {
		fail("C5-directory", "切线后:%v", err)
	}
	secondDir := secondPush.GetChannelDirectory()
	shared, err := channelSmokeCheckChannelNosStable(firstDir, secondDir)
	if err != nil {
		fail("C5-channel-no", "%v;第一次:%s;第二次:%s", err, channelSmokeDescribe(firstDir), channelSmokeDescribe(secondDir))
	}
	pass("C5-list", "current_scene=%d current_channel=%d channels=%d 线号与第一次一致的线=%d",
		targetScene, targetNo, len(secondDir.GetChannels()), shared)

	// ---- C6:冷却 ----
	currentScene := targetScene
	cooldown = time.Duration(secondDir.GetSwitchCooldownSeconds()) * time.Second
	cooldownReject, cooldownSwitchBack := "skipped", "skipped"
	if cooldown == 0 {
		cooldownReject, cooldownSwitchBack = "no_cooldown", "no_cooldown"
		skip("C6-cooldown", "目录 switch_cooldown_seconds=0(scene_manager 的 ChannelSwitch.CooldownSeconds 为负 = 不设冷却),不做冷却验证")
	} else {
		origin := channelSmokeFindChannel(secondDir, originScene)
		if origin == nil || !channelSmokeSelectable(origin.GetState()) {
			fail("C6-origin", "原线 scene_id=%d(%d线)在第二次目录里已不可选(%s),切回它被拒就分不清是不是冷却;"+
				"自动伸缩在回收这条线?稍后重跑", originScene, originNo, channelSmokeDescribe(secondDir))
		}

		// 6a:冷却期内立刻切回原线,必须"已受理 → 随后失败 tip"。
		rejectMark := -1
		if remaining := cooldown - time.Since(switchedAt); remaining < channelSmokeCooldownMinRemaining {
			skip("C6-cooldown-reject", "切线后已过 %s,冷却 %s 只剩 %s(< %s):请求可能在冷却过期后才到 scene_manager,"+
				"跳过「冷却期内被拒」这条子断言(把 ChannelSwitch.CooldownSeconds 配回缺省 10 可完整验证)",
				time.Since(switchedAt).Round(time.Millisecond), cooldown, remaining.Round(time.Millisecond), channelSmokeCooldownMinRemaining)
		} else {
			since, syncTip, err := bot.requestEnterScene(sceneConfigId, originScene)
			if err != nil {
				fail("C6-cooldown-reject", "%v", err)
			}
			if syncTip != 0 {
				fail("C6-cooldown-reject", "冷却期内切回原线 scene_id=%d,同步应答 tip=%d%s;期望无错(已受理),冷却的拒绝应走随后的 SendTipToClient",
					originScene, syncTip, channelSmokeTipHint(syncTip))
			}
			arrived, asyncTip, err := bot.waitSwitchOutcome(since, originScene, channelSmokeRejectTimeout)
			if errors.Is(err, errChannelSmokeNoOutcome) {
				fail("C6-cooldown-reject", "冷却期内切回原线 scene_id=%d 已受理,但 %s 内既没有失败 tip 也没有到达。"+
					"冷却的拒绝发生在 scene_manager 的预检(不走交接),不该这么慢;原线在别的 scene 节点时,"+
					"没被拒的请求要走交接、到达会晚于这个窗口 —— 那同样说明冷却没生效", originScene, channelSmokeRejectTimeout)
			}
			if err != nil {
				fail("C6-cooldown-reject", "冷却期内切回原线 scene_id=%d 已受理,等结局时出错:%v", originScene, err)
			}
			if arrived {
				fail("C6-cooldown-reject", "冷却没生效:切线 %s 后立刻切回原线 scene_id=%d 成功了(目录 switch_cooldown_seconds=%d,期望被拒)",
					time.Since(switchedAt).Round(time.Millisecond), originScene, secondDir.GetSwitchCooldownSeconds())
			}
			if asyncTip != tipChannelEnterSceneFailed {
				fail("C6-cooldown-reject", "冷却期内切回原线,受理后收到 tip=%d%s,期望 kEnterSceneFailed(%d)",
					asyncTip, channelSmokeTipHint(asyncTip), tipChannelEnterSceneFailed)
			}
			rejectMark = since
			cooldownReject = "checked"
			pass("C6-cooldown-reject", "origin_scene=%d tip=%d(kEnterSceneFailed)", originScene, asyncTip)
		}

		// 6b:等冷却过期后切回原线,必须成功。
		if cooldown+channelSmokeCooldownMargin > maxCooldownWait {
			skip("C6-cooldown-switch-back", "冷却 %s + 余量 %s 超过 channel_smoke.max_cooldown_wait_seconds=%s,不等它过期;留在目标线上做 C7",
				cooldown, channelSmokeCooldownMargin, maxCooldownWait)
		} else {
			// 起点仍是 C4 的到达时刻而不是 6a 被拒的时刻:预检只读、被拒不该续冷却(设计文档 §4.4)。
			// 若实现在被拒时也写了冷却键,这里的切回会被拒 —— 正是要抓的缺陷。
			if wait := time.Until(switchedAt.Add(cooldown + channelSmokeCooldownMargin)); wait > 0 {
				zap.L().Info("[channel-smoke] C6 等切线冷却过期", zap.Duration("wait", wait.Round(time.Millisecond)))
				time.Sleep(wait)
			}
			// 6a 的"没有切回成功"要覆盖整个等待期:被拒之后迟到的 NotifyEnterScene 同样说明冷却没拦住。
			if rejectMark >= 0 && bot.enteredSince(rejectMark, originScene) {
				fail("C6-cooldown-reject", "冷却期内那次切回先收到了失败 tip,之后又收到 scene_id=%d 的 NotifyEnterScene:拒绝与放行同时发生", originScene)
			}
			rejectTip, err := bot.switchTo(sceneConfigId, originScene, arriveTimeout)
			if err != nil {
				fail("C6-cooldown-switch-back", "%v", err)
			}
			if rejectTip != 0 {
				fail("C6-cooldown-switch-back", "冷却过期后切回原线 scene_id=%d 仍被拒 tip=%d%s:距 C4 到达已过 %s(冷却 %s)。"+
					"冷却键 TTL 不对,或 6a 被拒的那次预检续了冷却(设计文档 §4.4:预检只读,不占冷却)",
					originScene, rejectTip, channelSmokeTipHint(rejectTip), time.Since(switchedAt).Round(time.Millisecond), cooldown)
			}
			currentScene = originScene
			cooldownSwitchBack = "checked"
			pass("C6-cooldown-switch-back", "origin_scene=%d origin_channel=%d waited_since_switch=%s",
				originScene, originNo, time.Since(switchedAt).Round(time.Millisecond))
		}
	}

	// ---- C7:负例 —— 切到当前所在的线 ----
	_, syncTip, err := bot.requestEnterScene(sceneConfigId, currentScene)
	if err != nil {
		fail("C7-same-channel", "%v", err)
	}
	if syncTip != tipChannelYouInCurrentScene {
		fail("C7-same-channel", "请求切到当前所在的线 scene_id=%d,同步应答 tip=%d%s,期望 kEnterSceneYouInCurrentScene(%d)",
			currentScene, syncTip, channelSmokeTipHint(syncTip), tipChannelYouInCurrentScene)
	}
	pass("C7-same-channel", "current_scene=%d tip=%d(kEnterSceneYouInCurrentScene)", currentScene, syncTip)

	zap.L().Info(fmt.Sprintf("CHANNEL_SMOKE_OK player_id=%d zone=%d scene_config_id=%d channels=%d origin_scene=%d origin_channel=%d "+
		"target_scene=%d target_channel=%d final_scene=%d switch_cooldown_seconds=%d cooldown_reject=%s cooldown_switch_back=%s "+
		"retried_after_cooldown=%t",
		bot.gc.PlayerId, firstDir.GetZoneId(), sceneConfigId, len(firstDir.GetChannels()), originScene, originNo,
		targetScene, targetNo, currentScene, secondDir.GetSwitchCooldownSeconds(), cooldownReject, cooldownSwitchBack,
		retriedAfterCooldown))
	cleanup()
	_ = zap.L().Sync()
}

// channelSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 teamSmokeLogin 同形;不走 prepareBehaviorClient:它的回调不留底,本场景要按到达顺序扫描回包与推送。
func channelSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*channelSmokeBot, error) {
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

	bot := &channelSmokeBot{
		account: account,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), channelSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready(account=%s): %w", account, err)
	}
	return bot, nil
}

// channelSmokeIsRecorded:本场景按到达顺序留底的消息号。
// NotifySceneInfo(31)不在其中:它由通用 handler 记到 Player 上,按序号等(见 requestSceneInfo)。
func channelSmokeIsRecorded(messageId uint32) bool {
	switch messageId {
	case game.SceneSceneClientPlayerSceneInfoC2SMessageId, // 应答类型是 Empty、scene 不回包;留底只为看见 gate 的信封拒绝(限流)
		game.SceneSceneClientPlayerEnterSceneMessageId,
		game.SceneSceneClientPlayerNotifyEnterSceneMessageId,
		game.SceneClientPlayerCommonSendTipToClientMessageId,
		game.SceneClientPlayerCommonKickPlayerMessageId:
		return true
	}
	return false
}

// onMessage:先留底,再转发给通用分发 —— NotifyEnterScene 更新 Player 的当前场景、
// NotifySceneInfo 把目录记到 Player 上,都靠通用 handler。
//
// 唯一不转发的是 EnterScene(63)的同步应答。它的通用 handler(logic/handler/scene_scene_client_player_enter_scene.go)
// 只打日志,判据是「error_message != nil」;而 scene 每次应答都会把 error_message 物化成一条空消息
// (return_define.h 的 TRANSFER_ERROR_MESSAGE 无条件 mutable_error_message()),于是每一次**被受理**的切线都会被
// 打成带堆栈的 WARN "enter scene rejected",紧挨着本场景的 PASS 行,排障时会被误读成切线被拒;gate 信封拒绝时
// body 为空,它反而打 "enter scene accepted"。本场景按 error_message.id 自己判(requestEnterScene),
// 不转发不丢任何状态。
func (b *channelSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	messageId := msg.GetMessageId()
	if channelSmokeIsRecorded(messageId) {
		b.mu.Lock()
		b.records = append(b.records, channelSmokeRecord{
			messageId:   messageId,
			envelopeTip: msg.GetErrorMessage().GetId(),
			body:        msg.GetSerializedMessage(),
		})
		b.mu.Unlock()
	}
	if messageId == game.SceneSceneClientPlayerEnterSceneMessageId {
		return
	}
	handler.MessageBodyHandler(client, msg)
}

// mark 返回当前留底条数,作为"从这之后到达的消息"的扫描起点。必须在发请求之前调用。
func (b *channelSmokeBot) mark() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records)
}

// snapshot 返回 since 之后的留底副本。
func (b *channelSmokeBot) snapshot(since int) []channelSmokeRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	if since > len(b.records) {
		since = len(b.records)
	}
	return append([]channelSmokeRecord(nil), b.records[since:]...)
}

// send 按节奏(channelSmokeRequestSpacing)发一个请求。
func (b *channelSmokeBot) send(messageId uint32, request proto.Message) error {
	if wait := channelSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	if err := b.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d account=%s: %w", messageId, b.account, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()
	return nil
}

// requestSceneInfo 发一次 SceneInfoC2S(43)并等它带回的 NotifySceneInfo(31)。
//
// 43 的应答类型是 Empty,scene 对这类方法不回包(scene_handler.cpp ProcessClientPlayerMessage),所以没有
// "回包到了"可等;等的是 Player 上的推送序号变大。本会话只有主流程会发 43,而 31 只在处理 43 时才发
// (scene 侧没有别的发送点),所以序号变大的那一份就是这次请求带回来的(上一次请求超时后迟到的推送也可能
// 被认领 —— 它同样是一份合法的目录快照,只是略旧)。
func (b *channelSmokeBot) requestSceneInfo() (*scene.SceneInfoS2C, error) {
	if wait := channelSmokeListSpacing - time.Since(b.lastListAt); wait > 0 {
		time.Sleep(wait)
	}
	since := b.mark()
	_, seqBefore := b.player.GetSceneInfoPush()
	if err := b.send(game.SceneSceneClientPlayerSceneInfoC2SMessageId, &scene.SceneInfoRequest{WithChannelDirectory: true}); err != nil {
		return nil, err
	}
	b.lastListAt = b.lastRequestAt

	deadline := time.Now().Add(channelSmokeRpcTimeout)
	for {
		if push, seq := b.player.GetSceneInfoPush(); push != nil && seq > seqBefore {
			return push, nil
		}
		for _, r := range b.snapshot(since) {
			switch r.messageId {
			case game.SceneSceneClientPlayerSceneInfoC2SMessageId:
				if r.envelopeTip != 0 {
					return nil, fmt.Errorf("SceneInfoC2S 被 gate 信封拒绝 tip=%d%s:请求没到 scene",
						r.envelopeTip, channelSmokeTipHint(r.envelopeTip))
				}
			case game.SceneClientPlayerCommonKickPlayerMessageId:
				return nil, errors.New("等 NotifySceneInfo 途中收到 KickPlayer(账号被别处顶号?)")
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("SceneInfoC2S 发出后 %s 内没有收到 NotifySceneInfo"+
				"(scene 侧「玩家不在任何场景」时只记日志不回;或 gate / scene 没在跑)", channelSmokeRpcTimeout)
		}
		time.Sleep(channelSmokePollInterval)
	}
}

// fetchDirectory 列线,直到拿到带 channel_directory 的 NotifySceneInfo。
// 不带目录时每 channelSmokeDirectoryRetryInterval 重试一次,最多试到 channelSmokeDirectoryRetryBudget。
func (b *channelSmokeBot) fetchDirectory() (*scene.SceneInfoS2C, error) {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		push, err := b.requestSceneInfo()
		if err != nil {
			return nil, err
		}
		if push.GetChannelDirectory() != nil {
			return push, nil
		}
		// 副本 / 镜像里本来就不带目录(设计文档 §5 第 4 步),重试没有意义。
		for _, info := range push.GetSceneInfo() {
			if info.GetMirrorConfigId() != 0 || info.GetDungeonConfigId() != 0 {
				return nil, fmt.Errorf("当前场景 scene_id=%d 是副本 / 镜像(mirror_config_id=%d dungeon_config_id=%d),"+
					"这类场景不带分线目录;本冒烟要求账号停在大世界地图上",
					info.GetSceneId(), info.GetMirrorConfigId(), info.GetDungeonConfigId())
			}
		}
		if elapsed := time.Since(start); elapsed >= channelSmokeDirectoryRetryBudget {
			return nil, fmt.Errorf("列线 %d 次、历时 %s,NotifySceneInfo 始终不带 channel_directory。依次检查:"+
				"scene_manager 是否有实例当上领导者(只有领导者发布目录);ChannelSwitch.DirectoryRefreshSeconds 是否被配成负数(= 不发布);"+
				"scene_manager 与 scene 节点是否都换上了带切线的新二进制;scene 节点的 zone Redis 是否已连接"+
				"(与 scene_manager 是否同一个 Redis)", attempt, elapsed.Round(time.Millisecond))
		}
		zap.L().Info("[channel-smoke] NotifySceneInfo 不带 channel_directory,稍后重试",
			zap.Int("attempt", attempt), zap.Duration("retry_in", channelSmokeDirectoryRetryInterval))
		time.Sleep(channelSmokeDirectoryRetryInterval)
	}
}

// requestEnterScene 发 EnterScene(63){scene_config_id, scene_id} 并等同步应答,返回(本次请求的扫描起点, 同步应答的 tip)。
// tip == 0 只表示"已受理",不代表已到达(设计文档 §3.1)。
// gate 信封拒绝与等不到应答一律 error:它们说明请求没到 scene 的业务逻辑,不能当成 scene 给的码去断言。
func (b *channelSmokeBot) requestEnterScene(sceneConfigId uint32, sceneId uint64) (int, uint32, error) {
	since := b.mark()
	if err := b.send(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: sceneConfigId, SceneId: sceneId},
	}); err != nil {
		return since, 0, err
	}

	deadline := time.Now().Add(channelSmokeRpcTimeout)
	for {
		for _, r := range b.snapshot(since) {
			if r.messageId != game.SceneSceneClientPlayerEnterSceneMessageId {
				continue
			}
			if r.envelopeTip != 0 {
				return since, 0, fmt.Errorf("EnterScene{scene_config_id=%d scene_id=%d} 被 gate 信封拒绝 tip=%d%s:请求没到 scene",
					sceneConfigId, sceneId, r.envelopeTip, channelSmokeTipHint(r.envelopeTip))
			}
			var response scene.EnterSceneC2SResponse
			if err := proto.Unmarshal(r.body, &response); err != nil {
				return since, 0, fmt.Errorf("decode EnterSceneC2SResponse: %w", err)
			}
			return since, response.GetErrorMessage().GetId(), nil
		}
		if !time.Now().Before(deadline) {
			return since, 0, fmt.Errorf("EnterScene{scene_config_id=%d scene_id=%d} 发出后 %s 内没有同步应答(gate / scene 没在跑?)",
				sceneConfigId, sceneId, channelSmokeRpcTimeout)
		}
		time.Sleep(channelSmokePollInterval)
	}
}

// waitSwitchOutcome 从 since 起按到达顺序扫描,返回一次**已受理**的切线请求的结局:
//
//	arrived == true              收到 scene_id == target 的 NotifyEnterScene(79);
//	arrived == false, tip != 0   收到 SendTipToClient(23):受理后失败,玩家留在原线。
//
// 进了别的场景、被踢都返回 error;窗口内两者都没等到返回包裹 errChannelSmokeNoOutcome 的 error。
// 本场景的机器人不在队伍里、不发别的请求,窗口内出现的 tip 只可能属于这次切线。
func (b *channelSmokeBot) waitSwitchOutcome(since int, target uint64, timeout time.Duration) (arrived bool, tip uint32, err error) {
	deadline := time.Now().Add(timeout)
	for {
		for _, r := range b.snapshot(since) {
			switch r.messageId {
			case game.SceneSceneClientPlayerNotifyEnterSceneMessageId:
				var entered scene.EnterSceneS2C
				if err := proto.Unmarshal(r.body, &entered); err != nil {
					return false, 0, fmt.Errorf("decode NotifyEnterScene: %w", err)
				}
				if got := entered.GetSceneInfo().GetSceneId(); got != target {
					return false, 0, fmt.Errorf("请求切到 scene_id=%d,却收到 scene_id=%d scene_config_id=%d 的 NotifyEnterScene"+
						"(账号在队伍里被队长拉走?目标线在回收中被改派到别处?)",
						target, got, entered.GetSceneInfo().GetSceneConfigId())
				}
				return true, 0, nil
			case game.SceneClientPlayerCommonSendTipToClientMessageId:
				var pushed base.TipInfoMessage
				if err := proto.Unmarshal(r.body, &pushed); err != nil {
					return false, 0, fmt.Errorf("decode SendTipToClient: %w", err)
				}
				if pushed.GetId() != 0 {
					return false, pushed.GetId(), nil
				}
			case game.SceneClientPlayerCommonKickPlayerMessageId:
				return false, 0, errors.New("切线途中收到 KickPlayer(gate 向 scene 转发进场超出预算会踢线;或账号被别处顶号)")
			}
		}
		if !time.Now().Before(deadline) {
			return false, 0, fmt.Errorf("scene_id=%d %w(%s)", target, errChannelSmokeNoOutcome, timeout)
		}
		time.Sleep(channelSmokePollInterval)
	}
}

// switchTo 请求切到 target 并等结局。返回 (0, nil) = 已到达;(tip, nil) = 受理后被拒(玩家留在原线);
// 同步应答带 tip、信封拒绝、进了别的场景、被踢、等不到结局都返回 error。
func (b *channelSmokeBot) switchTo(sceneConfigId uint32, target uint64, timeout time.Duration) (uint32, error) {
	since, syncTip, err := b.requestEnterScene(sceneConfigId, target)
	if err != nil {
		return 0, err
	}
	if syncTip != 0 {
		return 0, fmt.Errorf("EnterScene{scene_config_id=%d scene_id=%d} 同步应答 tip=%d%s,期望无错(已受理)",
			sceneConfigId, target, syncTip, channelSmokeTipHint(syncTip))
	}
	arrived, rejectTip, err := b.waitSwitchOutcome(since, target, timeout)
	if errors.Is(err, errChannelSmokeNoOutcome) {
		return 0, fmt.Errorf("切线已受理但没有结局:%w。目标线在别的 scene 节点时要走「冻结 → 存盘 → 写标记 → 重发」的交接链路,"+
			"可调大 channel_smoke.arrive_timeout_seconds;同节点仍超时就看 scene_manager 的 EnterScene 日志与 gate 的进场转发日志", err)
	}
	if err != nil {
		return 0, err
	}
	if arrived {
		return 0, nil
	}
	return rejectTip, nil
}

// enteredSince 报告 since 之后是否收到过 scene_id == sceneId 的 NotifyEnterScene(不等待)。
func (b *channelSmokeBot) enteredSince(since int, sceneId uint64) bool {
	for _, r := range b.snapshot(since) {
		if r.messageId != game.SceneSceneClientPlayerNotifyEnterSceneMessageId {
			continue
		}
		var entered scene.EnterSceneS2C
		if err := proto.Unmarshal(r.body, &entered); err == nil && entered.GetSceneInfo().GetSceneId() == sceneId {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// channelSmokeCheckDirectory 校验一份 NotifySceneInfo 里的分线目录(设计文档 §3.1)。
// wantZone 为 0(登录时由 server-list 自动选区)时不校验 zone_id。
func channelSmokeCheckDirectory(push *scene.SceneInfoS2C, wantZone, wantSceneConfigId uint32, currentScene uint64) error {
	dir := push.GetChannelDirectory()
	if dir == nil {
		return errors.New("NotifySceneInfo 不带 channel_directory")
	}
	if wantZone != 0 && dir.GetZoneId() != wantZone {
		return fmt.Errorf("目录 zone_id=%d,期望登录的 zone %d(scene 节点读了别的 zone 的目录键?)", dir.GetZoneId(), wantZone)
	}
	if dir.GetSceneConfigId() != wantSceneConfigId {
		return fmt.Errorf("目录 scene_config_id=%d,期望当前地图 %d", dir.GetSceneConfigId(), wantSceneConfigId)
	}
	channels := dir.GetChannels()
	if len(channels) == 0 {
		return errors.New("目录 channels 为空(在役与回收中的线都没有时 scene_manager 不该发布目录)")
	}
	seenScene := make(map[uint64]struct{}, len(channels))
	foundCurrent := false
	var previousNo uint32
	for i, channel := range channels {
		no, sceneId := channel.GetChannelNo(), channel.GetSceneId()
		if no < 1 {
			return fmt.Errorf("第 %d 条 scene_id=%d 的 channel_no=%d,线号必须 ≥ 1;目录:%s", i+1, sceneId, no, channelSmokeDescribe(dir))
		}
		if i > 0 && no <= previousNo {
			return fmt.Errorf("channel_no 不是严格递增:第 %d 条是 %d、前一条是 %d(目录必须按线号升序且线号不重复);目录:%s",
				i+1, no, previousNo, channelSmokeDescribe(dir))
		}
		previousNo = no
		if sceneId == 0 {
			return fmt.Errorf("第 %d 条(%d线)的 scene_id=0;目录:%s", i+1, no, channelSmokeDescribe(dir))
		}
		if _, duplicated := seenScene[sceneId]; duplicated {
			return fmt.Errorf("scene_id=%d 在目录里出现多次;目录:%s", sceneId, channelSmokeDescribe(dir))
		}
		seenScene[sceneId] = struct{}{}
		if sceneId == currentScene {
			foundCurrent = true
		}
	}
	// scene_id 已保证不重复,所以"恰有一条等于当前线"只剩"有没有"。
	if !foundCurrent {
		return fmt.Errorf("目录里没有当前所在的线 scene_id=%d(期望恰有一条);目录:%s", currentScene, channelSmokeDescribe(dir))
	}
	if !dir.GetSwitchEnabled() {
		return errors.New("目录 switch_enabled=false:scene_manager 的 ChannelSwitch.Disabled 开着,玩家主动切线被关闭;把它关掉再跑")
	}
	// scene_info 是服务端取的"玩家当前场景"(每次恰好一条)。它与机器人按 NotifyEnterScene 记下的当前线不一致,
	// 说明 31 推的不是这名玩家此刻所在的场景。没带 scene_info 时不判(目录是本冒烟的断言对象,scene_info 只是旁证)。
	if infos := push.GetSceneInfo(); len(infos) > 0 && infos[0].GetSceneId() != currentScene {
		return fmt.Errorf("NotifySceneInfo.scene_info[0].scene_id=%d,但机器人当前在 scene_id=%d", infos[0].GetSceneId(), currentScene)
	}
	return nil
}

// channelSmokeCheckChannelNosStable 断言两份目录里都出现的线,线号没有变(设计文档 §4.2:线在役或回收中期间线号不变)。
// 只比交集:两次列线之间自动伸缩可能新建或销毁了别的线。返回交集大小。
func channelSmokeCheckChannelNosStable(before, after *scene.SceneChannelDirectory) (int, error) {
	beforeNo := make(map[uint64]uint32, len(before.GetChannels()))
	for _, channel := range before.GetChannels() {
		beforeNo[channel.GetSceneId()] = channel.GetChannelNo()
	}
	shared := 0
	for _, channel := range after.GetChannels() {
		no, ok := beforeNo[channel.GetSceneId()]
		if !ok {
			continue
		}
		shared++
		if no != channel.GetChannelNo() {
			return shared, fmt.Errorf("scene_id=%d 的线号从 %d 变成了 %d(同一条线的线号在它的生命周期内不得改变)",
				channel.GetSceneId(), no, channel.GetChannelNo())
		}
	}
	return shared, nil
}

// channelSmokeSelectable:只有 SMOOTH / BUSY 可以被玩家主动选中(设计文档 §3.1)。
func channelSmokeSelectable(state scene.SceneChannelState) bool {
	return state == scene.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH ||
		state == scene.SceneChannelState_SCENE_CHANNEL_STATE_BUSY
}

// channelSmokePickTarget 按目录顺序(线号升序)返回第一条不是当前线、且可选的线;没有返回 nil。
func channelSmokePickTarget(dir *scene.SceneChannelDirectory, currentScene uint64) *scene.SceneChannelInfo {
	for _, channel := range dir.GetChannels() {
		if channel.GetSceneId() != currentScene && channelSmokeSelectable(channel.GetState()) {
			return channel
		}
	}
	return nil
}

// channelSmokeFindChannel 返回目录里 scene_id == sceneId 的线;没有返回 nil(生成的 getter 对 nil 接收者安全)。
func channelSmokeFindChannel(dir *scene.SceneChannelDirectory, sceneId uint64) *scene.SceneChannelInfo {
	for _, channel := range dir.GetChannels() {
		if channel.GetSceneId() == sceneId {
			return channel
		}
	}
	return nil
}

// channelSmokeDescribe 把目录写成一行,供日志与失败原因使用。
func channelSmokeDescribe(dir *scene.SceneChannelDirectory) string {
	parts := make([]string, 0, len(dir.GetChannels()))
	for _, channel := range dir.GetChannels() {
		parts = append(parts, fmt.Sprintf("%d线{scene_id=%d %s 人数=%d}",
			channel.GetChannelNo(), channel.GetSceneId(), channel.GetState(), channel.GetPlayerCount()))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// channelSmokeTipHint 给本场景会遇到的几个 tip 配一句人话(拼在 "tip=<数字>" 后面);不认识的返回空串。
func channelSmokeTipHint(tip uint32) string {
	switch tip {
	case tipChannelEnterSceneFailed:
		return "(kEnterSceneFailed:同步 = 战斗在途;异步 = scene_manager 拒绝,原因不细分)"
	case tipChannelYouInCurrentScene:
		return "(kEnterSceneYouInCurrentScene:已经在这条线)"
	case tipChannelChangingScene:
		return "(kEnterSceneChangingScene:上一次换场景还没结束,scene 侧的在途记录没摘)"
	case tipChannelRateLimited:
		return "(kRateLimitExceeded:gate 限流,请求间隔见 channelSmokeRequestSpacing / channelSmokeListSpacing)"
	}
	return ""
}
