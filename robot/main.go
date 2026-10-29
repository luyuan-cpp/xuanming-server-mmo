package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/login"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/ai"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	// Blank-import proto packages so they register with protoregistry.
	_ "proto/chat"
)

// robotStatsRef is a process-wide handle to the metrics aggregator. It's
// populated in main() and read by lower-level helpers that don't (yet) take
// a Stats pointer in their signature — notably the queue-aware AssignGate
// loop in assignGateHTTPLocal, which is reached from resolveGateAddrLocal
// at multiple call sites across login modes (login-test, data-stress, stress).
//
// Plumbing Stats through every caller would touch ~10 functions for marginal
// benefit. The trade-off is "one global, set once at startup" vs "always-
// explicit-arg, never-nil". We pick the global because (a) Stats is a
// singleton by construction, (b) the queue helper has graceful nil-handling
// for unit tests, and (c) the alternative was littering signatures with a
// parameter that almost no caller cares about.
var robotStatsRef *metrics.Stats

func main() {
	cfgPath := flag.String("c", "etc/robot.yaml", "config file path")
	flag.Parse()

	initLogger()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		zap.L().Fatal("load config", zap.Error(err))
	}

	// Load tables (Skill, Class) and auto-resolve skill_ids if not configured.
	cfg.LoadTables()

	stats := metrics.NewStats()
	robotStatsRef = stats

	if cfg.Mode == "asset-arrival-prepare" {
		if err := RunAssetArrivalPrepare(cfg, stats); err != nil {
			zap.L().Error("ASSET_ARRIVAL_PREPARE_FAIL", zap.Error(err))
			_ = zap.L().Sync()
			os.Exit(1)
		}
		return
	}

	// 跟随跨区重定向(msg 124)后要在新 gate 上重跑 Login + EnterGame,而"用哪种认证"
	// 只有 main 知道(cfg.AuthType / 密码 / SA-Token / access_token)。pkg 层不能反向
	// import main,所以把实现注册进去。见 pkg/redirect.go 与
	// logic/handler/scene_client_player_common_redirect_to_gate.go。
	// 传当前会话已有的 access_token:重定向发生在登录之后,能省掉一次主认证往返;
	// 拿不到(空)时 loginAndEnterWithAuth 自己会回落到主认证。
	pkg.SetRedirectRelogin(func(gc *pkg.GameClient) error {
		access, _, accessExpire, _ := gc.SnapshotTokens()
		return loginAndEnterWithAuth(gc, cfg, stats, access, accessExpire)
	})

	// Features-smoke never invokes the common auto-create login/relogin path.
	if cfg.Mode == "features-smoke" {
		if err := RunFeaturesSmoke(cfg, stats); err != nil {
			zap.L().Error("FEATURES_SMOKE_FAIL", zap.Error(err))
			_ = zap.L().Sync()
			os.Exit(1)
		}
		return
	}
	// Login-test mode: run the scenario suite and exit.
	if cfg.Mode == "login-test" {
		host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
		if err != nil {
			zap.L().Fatal("resolve gate address", zap.Error(err))
		}
		port, _ := strconv.Atoi(portStr)
		runLoginTests(host, port, cfg, stats, tokenPayload, tokenSig)
		_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
		_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
		return
	}

	// Data-stress mode: drive repeated login → play → logout cycles per robot
	// and publish per-player expectations to Redis for the db-side verifier
	// (see go/db/cmd/verifier).
	if cfg.Mode == "data-stress" {
		stop := make(chan struct{})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			zap.L().Info("data-stress shutdown signal received")
			close(stop)
		}()
		runDataStressMode(cfg, stats, stop)
		_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
		_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
		return
	}

	// Currency-crash-snapshot mode: single login → optional GmAddCurrency →
	// readback → write snapshot JSON → exit. Driven by an external script
	// that runs the robot twice per case (pre/post kill) and diffs the JSON.
	// See docs/notes/currency-crash-window-verification.md.
	if cfg.Mode == "currency-crash-snapshot" {
		host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
		if err != nil {
			zap.L().Fatal("resolve gate address", zap.Error(err))
		}
		port, _ := strconv.Atoi(portStr)
		runCurrencyCrashSnapshotMode(host, port, cfg, stats, tokenPayload, tokenSig)
		return
	}

	// Battle-smoke 模式:两个机器人做"回合制战斗 + 观战"端到端冒烟
	// (A 排队 PVE_SOLO 自动战斗,B 观战匹配跟看),断言全过退出码 0,
	// 任一步失败退出码 1。见 battle_smoke_scenario.go 与
	// docs/design/turn-based-battle-server.md §9 的 robot battle 动作预留。
	if cfg.Mode == "battle-smoke" {
		RunBattleSmoke(cfg)
		return
	}

	// Attribute-smoke 模式:单机器人做「角色属性加点」端到端冒烟
	// (拉面板 → 提级发点 → 自动加点预览 → 确认 → 各类拒绝守卫 → 方案隔离 → 洗点返还)。
	// 见 attribute_smoke_scenario.go 与 docs/design/player-attribute-allocation.md。
	if cfg.Mode == "attribute-smoke" {
		RunAttributeSmoke(cfg)
		return
	}

	// Pet-smoke 模式:单机器人做「宝宝(宠物)系统」端到端冒烟
	// (发宝宝 → 池隔离 → 等级跟随主人 → 自动加点预览 → 确认 → 各类拒绝守卫 → 出战 → 重登往返 → 收回)。
	// 见 pet_smoke_scenario.go 与 docs/design/player-pet.md。
	if cfg.Mode == "pet-smoke" {
		RunPetSmoke(cfg)
		return
	}

	// Equip-smoke 模式:单机器人做「装备属性(问道式随机属性 + 穿脱)」端到端冒烟
	// (GM 发装备 → tooltip 字段与随机属性形状 → 等级不足被拒 → 穿上 / 替换 / 再穿一件 → 面板加成
	//  → 重登往返 → 卸下后面板恰好还原 → 各类拒绝守卫)。
	// 见 equip_smoke_scenario.go 与 docs/design/equipment-attributes.md §7;前置条件写在 etc/equip_smoke.yaml 文件头。
	if cfg.Mode == "equip-smoke" {
		RunEquipSmoke(cfg)
		return
	}

	// Equip-forge-smoke 模式:单机器人做「装备强化 + 宝石镶嵌 / 取下」端到端冒烟
	// (预备一把还能强化的武器 → 强化预览 / 孔位 / 宝石的显示字段 → 各类拒绝守卫 → 背包里强化 → 穿上后再强化
	//  → 镶嵌 + 面板加成 → 重登往返 → 取下后面板恰好还原)。用独立账号,不与 equip-smoke 共用(强化不可逆)。
	// 见 equip_forge_smoke_scenario.go 与 docs/design/equipment-enhance-inlay.md §7;前置条件写在 etc/equip_forge_smoke.yaml 文件头。
	if cfg.Mode == "equip-forge-smoke" {
		RunEquipForgeSmoke(cfg)
		return
	}

	// Equip-forge-topup 模式:给一个开发账号补强化石 / 宝石 / 银两后退出,好在真实客户端里点强化与镶嵌
	// (游戏里没有这些东西的产出,客户端也没有 GM 发物入口)。见 equip_forge_topup_scenario.go 与 etc/equip_forge_topup.yaml。
	if cfg.Mode == "equip-forge-topup" {
		RunEquipForgeTopup(cfg)
		return
	}

	// Chat-smoke 模式:两个机器人做「全局聊天 go/chat v1」端到端冒烟
	// (A/B 分登两个 zone → WORLD 发 + 拉 → PRIVATE 发 + 拉 → chat 侧超长拒绝 → 同 request_id 幂等)。
	// 见 chat_smoke_scenario.go 与 zone 接入契约 v1 §9 冒烟段;前置条件写在 etc/chat_smoke.yaml 文件头。
	if cfg.Mode == "chat-smoke" {
		RunChatSmoke(cfg)
		return
	}

	// Guild-smoke 模式:三个机器人做「公会按 zone 隔离」端到端冒烟
	// (建帮 zone 由服务端决定 → 同区可见 / 可加入 → 别区不可见 / 不可加入 / 同名被拒 → 公告权限 → 身份伪造与内部方法被拒 → 解散)。
	// 见 guild_smoke_scenario.go 与 docs/design/guild-zone-client-access.md;前置条件写在 etc/guild_smoke.yaml 文件头。
	if cfg.Mode == "guild-smoke" {
		RunGuildSmoke(cfg)
		return
	}

	// Trade-smoke 模式:三个机器人做「聚宝斋 P1」端到端冒烟
	// (gRPC 直连 TradeAdmin 造种子 → TradeAdmin 经 gate 不可达 → 市场范围过滤 → 公示 / 分页 / 拍卖未开放 → 详情可见性 → 收藏 → 货架)。
	// 见 trade_smoke_scenario.go 与 docs/design/jubaozhai-market.md §9;前置条件写在 etc/trade_smoke.yaml 文件头。
	if cfg.Mode == "trade-smoke" {
		RunTradeSmoke(cfg)
		return
	}

	// Team-smoke 模式:四个机器人做「组队系统」端到端冒烟
	// (建队 / 申请 / 审批 / 邀请 / 踢人 / 转让 → 同节点跟随 → 整队开战 → 战斗中开战被拒 → 跨区两形态 → 解散)。
	// 见 team_smoke_scenario.go 与 docs/design/team-system.md §I.5;前置条件写在 etc/team_smoke.yaml 文件头。
	if cfg.Mode == "team-smoke" {
		RunTeamSmoke(cfg)
		return
	}

	// Team-follow-smoke 模式:两个机器人做「组队跟随 v1.1」服务端部分的端到端冒烟
	// (队长走开 → 入队即被瞬移到队长身边 → 可见性重建 → 双向移动广播 → 解散)。
	// 见 team_follow_smoke_scenario.go 与 docs/design/team-system.md「组队跟随 v1.1」;前置条件写在 etc/team_follow_smoke.yaml 文件头。
	if cfg.Mode == "team-follow-smoke" {
		RunTeamFollowSmoke(cfg)
		return
	}

	// 队伍补位匹配:A、B 组队选「凑满 3 人」,C 单排同一副本的 3 人档,验证队员取消 / 名单变化整队退出、
	// 补位成组同场开战。见 team_backfill_smoke_scenario.go 与 docs/design/team-backfill-matching.md §7.3。
	if cfg.Mode == "team-backfill-smoke" {
		RunTeamBackfillSmoke(cfg)
		return
	}

	// Travel-smoke 模式:单机器人做「跨 zone 场景传送」往返端到端冒烟
	// (登 home_zone → 非法目标被拒 → TravelToZone{visit_zone} → 跟随 msg 124 严格重登 → 访客区断言 → TravelToZone{home_zone} → 回家不回档)。
	// 本模式会用「按 player_id 选角、绝不自动建角」的严格重登覆盖上面注册的通用重登(本模式独占进程,覆盖是安全的)。
	// 见 travel_smoke_scenario.go 与 docs/design/cross-zone-scene-travel.md;前置条件写在 etc/travel_smoke.yaml 文件头。
	if cfg.Mode == "travel-smoke" {
		RunTravelSmoke(cfg)
		return
	}

	// Friend-smoke 模式:三个机器人做「全局好友 go/friend」端到端冒烟
	// (A/C 与 B 分登两个 zone → 加好友 + 跨区推送 → 权威拉取 + 重发被拒 → 同意 + 反向推送 + 在线状态
	//  → 黑名单双向拦截 → 推荐截断与过滤 → S2C 方法对客户端不可调 → 清理)。
	// 见 friend_smoke_scenario.go 与 docs/design/friend-port-20260918.md;前置条件写在 etc/friend_smoke.yaml 文件头。
	if cfg.Mode == "friend-smoke" {
		RunFriendSmoke(cfg)
		return
	}

	// Mail-smoke 模式:两个机器人做「全局邮件 go/mail」端到端冒烟
	// (R 与 O 分登两个 zone → 直连 MailAdmin 发系统 / 定向 / 带附件三封 + 同键重放 → 两区都看到系统邮件、只有 R 看到定向邮件
	//  → 新信推送(弱断言)→ 读信减未读 → 别人读不到 → 领取未开放 → 带附件的删不掉、无附件的可重复删 → S2C 方法与 MailAdmin 对客户端不可调)。
	// 见 mail_smoke_scenario.go 与 docs/design/mail-system.md §10 ⑦;前置条件(含 MMORPG_MAIL_ADMIN_TOKEN)写在 etc/mail_smoke.yaml 文件头。
	if cfg.Mode == "mail-smoke" {
		RunMailSmoke(cfg)
		return
	}

	// Rank-smoke 模式:三个机器人做「全服排行榜 go/rank」端到端冒烟
	// (A、N 登 zone_a、B 登 zone_b → A、B 各读一次榜 → A、B 跨区打一局 1V1 → 轮询到两人都新记一局
	//  → 分数方向 / 名次先后 / 跨区名字补全 / 页形状 / 未上榜的 self.rank = 0 →(可选)gate 限流)。
	// 见 rank_smoke_scenario.go 与 docs/design/leaderboard-system.md §8.2;前置条件(含 match 的快照发布开关)写在 etc/rank_smoke.yaml 文件头。
	if cfg.Mode == "rank-smoke" {
		RunRankSmoke(cfg)
		return
	}

	// Channel-smoke 模式:单机器人做「玩家主动切线」端到端冒烟
	// (登录 → 列线(SceneInfoC2S → NotifySceneInfo 带分线目录)→ 切到同图另一条线 → 再列线核对线号不变
	//  → 冷却期内切回被拒、冷却过后切回成功 → 切到当前所在的线被同步拒绝)。
	// 该地图只有 1 条线时打印 CHANNEL_SMOKE_SKIP 并以退出码 0 结束(前置条件不满足,不算失败)。
	// 见 channel_smoke_scenario.go 与 docs/design/world-channel-switch.md;前置条件写在 etc/channel_smoke.yaml 文件头。
	if cfg.Mode == "channel-smoke" {
		RunChannelSmoke(cfg)
		return
	}

	// Ordering-smoke 模式:单机器人验证「同一会话经路由服的请求按收包顺序执行」
	// (不等回包连发 JoinQueue → CancelQueue,再查 GetQueueStatus,最终必须不在队列里)。
	// 见 ordering_smoke_scenario.go 与 docs/design/client-request-ordering.md;前置条件写在 etc/ordering_smoke.yaml 文件头。
	if cfg.Mode == "ordering-smoke" {
		RunOrderingSmoke(cfg)
		return
	}

	// Sync-smoke 模式:机器人 A 被观测、机器人 B 触发 bump,验证「落地恢复」的按域版本摘要
	// (登录取摘要 → B 的好友申请只让 A 的 FRIEND_REQUEST +1、g 不变 → 短重连后摘要原样延续 → 可选跨 zone 落地
	//  → 实体就绪前发请求不致崩溃)。跨 zone 腿开启时会用「按 player_id 严格重登」覆盖上面注册的通用重登(本模式独占进程)。
	// 见 sync_smoke_scenario.go 与 docs/design/landing-recovery-sync-digest.md §10 第 6 步;前置条件写在 etc/sync_smoke.yaml 文件头。
	if cfg.Mode == "sync-smoke" {
		RunSyncSmoke(cfg)
		return
	}

	// merchant-smoke: 单机器人神秘商人冒烟
	// (登录 → 查询商人在哪条线 → 不在同线就切过去 → 走到站位旁 → 份数 0 / 旧轮次 / 报价对不上被拒
	//  → GM 加钱后买 1 份并核对余额、背包、已购数 → 买到限购上限后再买被拒)。
	// 商人不在场 / 不在机器人所在的地图 / 本轮已买满时打印 MERCHANT_SMOKE_SKIP 并以退出码 0 结束。
	// 见 mystery_merchant_smoke_scenario.go 与 docs/design/mystery-merchant.md;前置条件写在 etc/merchant_smoke.yaml 文件头。
	if cfg.Mode == "merchant-smoke" {
		RunMerchantSmoke(cfg)
		return
	}

	// mall-arrival-smoke: 单机器人官方商品到账冒烟(D-17)
	// (登录 → 清理上一轮没付款的单 → 下单并核对同号重发拿回同一张单 → 错金额付款被商城拒
	//  → 以 dev 模拟渠道付款 → 等订单变已发货 → 背包正好多出应得数量且不重复发 → 重登后仍在)。
	// 见 mall_arrival_smoke_scenario.go 与 commerce_mock_payer.go;前置条件写在 etc/mall_arrival_smoke.yaml 文件头。
	if cfg.Mode == "mall-arrival-smoke" {
		RunMallArrivalSmoke(cfg)
		return
	}

	stopReport := make(chan struct{})
	reportInterval := time.Duration(cfg.ReportInterval) * time.Second
	if reportInterval <= 0 {
		reportInterval = 5 * time.Second
	}
	stats.StartReporter(reportInterval, stopReport)

	zap.L().Info("starting robots",
		zap.String("gateway", cfg.GatewayAddr),
		zap.Int("count", cfg.RobotCount),
	)

	stopAll := make(chan struct{})

	var wg sync.WaitGroup
	for i := 1; i <= cfg.RobotCount; i++ {
		account := fmt.Sprintf(cfg.AccountFmt, i)
		wg.Add(1)
		go func(account string) {
			defer func() {
				if r := recover(); r != nil {
					zap.L().Error("robot panic", zap.String("account", account), zap.Any("panic", r))
				}
				wg.Done()
			}()
			runRobot(account, cfg, stats, stopAll)
		}(account)
		time.Sleep(50 * time.Millisecond) // stagger to avoid thundering herd
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	zap.L().Info("shutting down...")
	close(stopAll)
	close(stopReport)
	wg.Wait()
	_ = stats.ExportBehaviorCSV("behavior_test_results.csv")
	_ = stats.ExportBehaviorJSONL("behavior_test_results.jsonl")
	zap.L().Info("all robots stopped")
}

// runRobot is the full lifecycle for one robot (one goroutine).
func runRobot(account string, cfg *config.Config, stats *metrics.Stats, stop <-chan struct{}) {
	const maxRetries = 5
	// Token-expiry refetches deliberately do NOT consume the normal retry
	// budget (see the tokenExpired branch below), but they MUST be bounded:
	// otherwise a permanently-unreachable / mis-signing gate would make a robot
	// spin forever (attempt-- never advances), turning a real outage into an
	// invisible hang instead of a counted failure. The cap is generous —
	// transient expiry during the 78k-concurrency ramp only needs a handful of
	// refetches. Round 19 hardening.
	const maxTokenExpiredRetries = 20
	backoff := 3 * time.Second
	tokenExpiredRetries := 0

	// access_token reconnect cache: carried across retry attempts so a failed
	// session (e.g. scene-ready timeout) doesn't pay the full primary-auth
	// cost again. Cleared implicitly when the server rotates the token.
	var cachedAccessToken string
	var cachedAccessExpire int64

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-stop:
			return
		default:
		}

		if attempt > 1 {
			zap.L().Info("retrying login", zap.String("account", account), zap.Int("attempt", attempt), zap.Duration("backoff", backoff))
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}

		ok, tok, exp, tokenExpired := runRobotOnce(account, cfg, stats, stop, cachedAccessToken, cachedAccessExpire)
		cachedAccessToken, cachedAccessExpire = tok, exp
		if ok {
			return // played successfully (or stop signal)
		}
		// R17 R2 收尾(Round 19): VerifyGateToken 失败 = gate token 已过期,
		// 这是 dispatcher 签名时间 → robot 拨上来的链路延迟问题,不是 robot
		// 自己的错。下一次 attempt 会重新走 resolveGateAddrLocal 拿全新 token,
		// 所以这一轮不应该消耗 retry 预算,也不算 login_fail。
		// 抵消本次 attempt 自增,backoff 用一个非常短的固定值(token 拿新的就 OK)。
		// 但有上限:连续 token 过期超过 maxTokenExpiredRetries 说明 gate 真的有
		// 问题(宕机 / 时钟漂移 / 签名错),此时给一次 LoginFail 让它显式可见,
		// 而不是无限自旋假装"没失败"。
		if tokenExpired {
			tokenExpiredRetries++
			if tokenExpiredRetries > maxTokenExpiredRetries {
				zap.L().Error("robot gave up after repeated gate-token expiry",
					zap.String("account", account),
					zap.Int("tokenExpiredRetries", tokenExpiredRetries))
				stats.LoginFail()
				return
			}
			attempt--
			backoff = 200 * time.Millisecond
		}
	}
	zap.L().Error("robot gave up after retries", zap.String("account", account), zap.Int("maxRetries", maxRetries))
}

// runRobotOnce attempts a single connect→login→play cycle.
// Returns (ok, newAccessToken, newAccessExpire, tokenExpired):
//   - ok=true: session ended gracefully, caller should stop retrying.
//   - ok=false: caller should back off and retry.
//   - tokenExpired=true: VerifyGateToken 失败,外层应该 attempt-- 抵消本次 retry,
//     直接重新走 AssignGate 拿新 token(R17 R2 收尾,Round 19)。
//
// On login failure the previous cached token is preserved (maybe the issue was
// transient). On successful login we hand back whatever the server rotated to,
// falling back to the incoming token if access_token reconnect took the path
// where the server doesn't rotate.
func runRobotOnce(account string, cfg *config.Config, stats *metrics.Stats, stop <-chan struct{}, prevAccessToken string, prevAccessExpire int64) (ok bool, newAccessToken string, newAccessExpire int64, tokenExpired bool) {
	newAccessToken, newAccessExpire = prevAccessToken, prevAccessExpire
	// Fetch a fresh gate token for each connection attempt so the gate token
	// TTL (10min since Round 19, see login assigngatelogic.gateTokenTTL) is
	// never stale — even under high robot counts or retry backoff.
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		zap.L().Error("resolve gate address failed", zap.String("account", account), zap.Error(err))
		stats.LoginFail()
		return
	}
	port, _ := strconv.Atoi(portStr)

	gc, err := pkg.NewGameClient(host, port)
	if err != nil {
		zap.L().Error("connect failed", zap.String("account", account), zap.Error(err))
		stats.LoginFail()
		return
	}
	// Order matters: Disconnect must go out BEFORE we close the TCP, otherwise
	// gate sees an EOF on a verified session, drops it, and login never finds
	// out. Once login receives Disconnect it marks the player_locator session
	// as Disconnecting (with a 30s reconnect lease) — without this, repeated
	// dev runs leave residual ONLINE sessions that flip the next EnterGame
	// into ReplaceLogin against a now-dead gate. See docs/design/
	// stress-test-2026-05-http-login.md §四 #B-1 for the diagnosis.
	defer gc.Close()
	defer sendDisconnectBestEffort(gc)
	stats.Connected()
	defer stats.Disconnected()

	gc.Account = account
	time.Sleep(500 * time.Millisecond)

	if len(tokenPayload) > 0 {
		if err := gc.VerifyGateToken(tokenPayload, tokenSig); err != nil {
			// R17 R2 收尾(Round 19): gate token 过期不算 robot 的错,也不消耗
			// retry 预算 —— 让外层重新走 AssignGate 拿一个全新 token。计入
			// gate_token_retry 计数器以便复盘观测。详见
			// docs/stress/stress-1zone-45k-2026-06-04-round18.md §R2。
			zap.L().Warn("gate token verify failed, will refetch", zap.String("account", account), zap.Error(err))
			stats.GateTokenRetry()
			tokenExpired = true
			return
		}
	}

	loginStart := time.Now()
	if err := loginAndEnterWithAuth(gc, cfg, stats, prevAccessToken, prevAccessExpire); err != nil {
		zap.L().Error("login flow failed", zap.String("account", account), zap.Error(err))
		return
	}
	// Capture whatever the server rotated to (password/satoken paths) or keep
	// the prev token (access_token reconnect path doesn't rotate). Read
	// through SnapshotTokens so we don't race the RefreshToken handler.
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}
	stats.LoginOK(time.Since(loginStart))
	stats.EnterOK()

	zap.L().Info("entered game",
		zap.String("account", account),
		zap.Uint64("player_id", gc.PlayerId),
	)

	// Register player in global list so message handlers can find it.
	player := gameobject.NewPlayer(gc.PlayerId)
	gameobject.PlayerList.Set(gc.PlayerId, player)
	defer gameobject.PlayerList.Delete(gc.PlayerId)

	// Register this connection so the RefreshToken handler (which only has
	// the Player, not the GameClient) can patch our token state in place.
	pkg.Clients.Register(gc.PlayerId, gc)
	defer pkg.Clients.Unregister(gc.PlayerId, gc)

	// Start RecvLoop early so we can receive NotifyEnterScene.
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
			stats.MsgRecv()
			// 这里只收推送与 id = 0 的回包。ListSkills 只经 startListOwnedSkills 带请求号发出,它的 id = 0 回包说明服务端
			// 没回显请求号,丢弃(那次调用将以超时收场);AI 循环仍以 SendRequest 发的请求(ReleaseSkill / EnterScene 等)
			// 回包照旧交 handler。
			if msg.GetMessageId() == game.SceneSkillClientPlayerListSkillsMessageId {
				smokeDropUncorrelatedReply(client.Account, msg)
				return
			}
			handler.MessageBodyHandler(client, msg)
		})
	}()

	// Proactively refresh access_token when it's close to expiry so a very
	// long-running session doesn't force the next reconnect into the
	// primary-auth fallback. Exits when the recv loop or stop closes.
	go runTokenRefresher(gc, cfg.GatewayAddr, stats, stop, recvDone)

	// Wait for scene node to be bound (NotifyEnterScene) before sending scene-targeted messages.
	// Round 20: 15s -> 60s. EnterGame already succeeded here (we have a player_id);
	// a real UE client does NOT tear down its verified session and re-auth just
	// because the first NotifyEnterScene is slow. Under 45k concentrated players the
	// scene EventLoop backlog can delay NotifyEnterScene past 15s, and the old short
	// timeout turned every slow load into disconnect -> reconnect -> re-EnterScene,
	// a self-amplifying retry storm (R19: ~36k give-up leaves churning the scene).
	// A patient wait lets the saturated-but-draining scene deliver the notify, which
	// collapses the churn and lets the backlog drain for everyone.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer waitCancel()
	if err := player.WaitSceneReady(waitCtx); err != nil {
		zap.L().Error("timed out waiting for scene ready", zap.String("account", gc.Account), zap.Error(err))
		return
	}

	// Now safe to send scene-targeted requests.
	// Wait for the ListSkills response so the AI loop only casts skills the player actually owns.
	// 只等 listSkillsWait:等不到只告警、AI 照常起跑(此时没有已拥有技能,施法动作记失败),与迁移前等 WaitSkillsReady
	// 超时的语义一致;之后才到的回包仍由 handler 写进 Player,AI 从那时起按已拥有技能施法。
	// RecvLoop 已在上面启动,满足 startListOwnedSkills 的前提。
	skillsResult := startListOwnedSkills(gc, stats, listSkillsReplyTimeout)
	skillsWait := time.NewTimer(listSkillsWait)
	select {
	case err := <-skillsResult:
		skillsWait.Stop()
		if err != nil {
			zap.L().Warn("skill list unavailable", zap.String("account", gc.Account), zap.Error(err))
		}
	case <-skillsWait.C:
		zap.L().Warn("timed out waiting for skill list", zap.String("account", gc.Account),
			zap.Duration("waited", listSkillsWait))
	}

	robotAI := ai.NewRobotAI(gc, stats)
	robotAI.SetPlayer(player)
	if len(cfg.SkillIDs) > 0 {
		robotAI.SetSkillIDs(cfg.SkillIDs)
	}
	if cfg.ActionInterval > 0 {
		robotAI.SetInterval(time.Duration(cfg.ActionInterval) * time.Second)
	}
	profile := cfg.ResolveProfile()
	if profile != nil {
		robotAI.SetProfile(profile)
	}
	if cfg.LLM.Enabled {
		robotAI.SetLLM(ai.NewLLMAdvisor(cfg.LLM.Endpoint, cfg.LLM.APIKey, cfg.LLM.Model))
	}
	go robotAI.RunLoop(stop)

	// Block until connection closes or stop signal.
	select {
	case <-recvDone:
	case <-stop:
	}

	// Graceful shutdown: send LeaveGame so server cleans up session.
	_ = gc.SendRequest(game.ClientPlayerLoginLeaveGameMessageId, &login.LeaveGameRequest{})
	if gc.SaToken != "" {
		revokeSaToken(cfg.SaTokenAddr, gc.SaToken)
	}
	// Re-snapshot: an in-session refresh may have rotated the token after
	// the post-login capture above; don't hand a stale value back to runRobot.
	if access, _, accessExp, _ := gc.SnapshotTokens(); access != "" {
		newAccessToken = access
		newAccessExpire = accessExp
	}
	ok = true
	return
}

const (
	// listSkillsWait 是压测主循环起 AI 前等 ListSkills 结局的时长(迁移前等 WaitSkillsReady 的 5s)。
	listSkillsWait = 5 * time.Second
	// listSkillsReplyTimeout 是 ListSkills 这次调用本身的回包预算,刻意长于各处的等待时长:迁移前回包到得再晚,
	// 也会被 handler 写进 Player,AI 从那以后就按已拥有技能施法;满载时 scene 积压能让回包晚于 5s,若按等待时长
	// 设预算,它会被当成迟到回包丢弃,整场会话都不再施法,压测的请求构成随之改变。
	// 取大厅客户端的 15s(docs/design/client-rpc-callback.md §2.3):gate 等 scene 回包满 12s 即回带请求号的 1003
	// (cpp/nodes/gate/handler/rpc/client_session_handler.cpp kGateToSceneClientCallTimeout),gate 正常时
	// 回包或信封总在预算内到达。
	listSkillsReplyTimeout = 15 * time.Second
)

// startListOwnedSkills 发出一次 ListSkills 并立刻返回,不等回包(压测主循环与 prepareBehaviorClient 共用)。
//
// 回包带请求号,不进 RecvLoop 的 onMessage,生成 handler 不会自己跑;而 AI 循环(RobotAI.castSkill)与登录测试读的是
// Player 上那份已拥有技能(GetOwnedSkillIDs),ListSkills 的 handler 还负责剔除职业被动(服务端拒绝施放被动,
// docs/design/class-skills.md §4)。所以 done 在派发回包的 RecvLoop goroutine 上把成功回包交给
// handler.MessageBodyHandler:与迁移前同一 goroutine、同一收包顺序,不会与同连接上的推送处理器交错。
// 信封错误不交 handler(body 为空,会被解成一份全零响应)。
//
// 返回的通道恰好收到一条结局,容量 1:done 写入从不阻塞,调用方不读也不泄漏。nil = 列表已由 handler 写进 Player;
// 否则是信封拒绝(*pkg.ServerTipError,Player 保持原样)、业务拒绝(回包 error_message,handler 照常执行、
// 已拥有技能被写成空)、超时(pkg.ErrRPCTimeout)、本地未发出(pkg.ErrRPCNotSent)或连接已断(pkg.ErrRPCDisconnected)。
// 调用方可以只等其中一段:之后才到的成功回包仍由 handler 写进 Player,与迁移前一致。
//
// 前提:gc 的 RecvLoop 已经启动(回包只能由它派发)。不阻塞,在任何 goroutine 上调用都安全。
// 发出时计 MsgSent,收到回包或信封时计 MsgRecv(stats 可为 nil)。成功路径不打日志:压测一次上万机器人。
func startListOwnedSkills(gc *pkg.GameClient, stats *metrics.Stats, timeout time.Duration) <-chan error {
	result := make(chan error, 1)
	gc.Call(game.SceneSkillClientPlayerListSkillsMessageId, &scene.ListSkillsRequest{}, timeout,
		func(reply *base.MessageContent, err error) {
			if reply != nil && stats != nil {
				stats.MsgRecv() // 回包与信封不再经过 onMessage,在这里计数
			}
			if err == nil {
				handler.MessageBodyHandler(gc, reply)
				var rsp scene.ListSkillsResponse
				if decodeErr := proto.Unmarshal(reply.GetSerializedMessage(), &rsp); decodeErr != nil {
					err = fmt.Errorf("decode list skills reply: %w", decodeErr)
				} else if rsp.GetErrorMessage() != nil {
					err = fmt.Errorf("list skills rejected: %s", rsp.GetErrorMessage().String())
				}
			}
			result <- err
		})
	// 本地没发出(已 Close / 无连接)在 Call 返回前同步完成,不计发送。其余结局也可能已抢先写入:
	// 取出后原样放回(done 只写一次,此刻槽是空的,不会阻塞),照 ordering 冒烟的 callAll。
	select {
	case err := <-result:
		result <- err
		if errors.Is(err, pkg.ErrRPCNotSent) {
			return result
		}
	default:
	}
	if stats != nil {
		stats.MsgSent()
	}
	return result
}

func initLogger() {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	cfg.Level.SetLevel(zap.InfoLevel)
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	zap.ReplaceGlobals(logger)
}

type gateAssignmentLocal struct {
	addr      string
	payload   []byte
	signature []byte
}

// resolveGateAddrLocal keeps main.go runnable as a single-file command.
func resolveGateAddrLocal(cfg *config.Config) (host, port string, payload, signature []byte, err error) {
	return resolveGateViaHTTPLocal(cfg)
}

// resolveGateViaHTTPLocal calls POST /api/assign-gate (HTTP mode) with retry.
//
// Queue-aware behavior (added 2026-05): when /api/assign-gate returns
// code=100 (queueing) we delegate to AssignGateWithQueue so the robot
// transparently polls /api/queue-status until admitted, expired, or the
// outer retry budget runs out. This makes a stress run with N > zoneCapacity
// robots actually exercise the queue path instead of treating it as an error.
func resolveGateViaHTTPLocal(cfg *config.Config) (host, port string, payload, signature []byte, err error) {
	zoneID := cfg.ZoneID
	if zoneID == 0 {
		zoneID, err = resolveZoneIDLocal(cfg.GatewayAddr)
		if err != nil {
			zoneID = 0
		}
	}

	const maxRetries = 30
	backoff := 2 * time.Second
	for attempt := 1; attempt <= maxRetries; attempt++ {
		result, assignErr := assignGateHTTPLocal(cfg, zoneID)
		if assignErr == nil {
			h, p, splitErr := net.SplitHostPort(result.addr)
			return h, p, result.payload, result.signature, splitErr
		}
		if attempt < maxRetries {
			time.Sleep(backoff)
			if backoff < 10*time.Second {
				backoff = backoff * 3 / 2
			}
		}
	}
	return "", "", nil, nil, fmt.Errorf("AssignGate failed after retries")
}

func resolveZoneIDLocal(gatewayAddr string) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayAddr+"/api/server-list", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Zones []struct {
			ZoneID      uint32 `json:"zone_id"`
			Recommended bool   `json:"recommended"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if len(result.Zones) == 0 {
		return 0, fmt.Errorf("no zones")
	}
	for _, z := range result.Zones {
		if z.Recommended {
			return z.ZoneID, nil
		}
	}
	return result.Zones[0].ZoneID, nil
}

// assignGateHTTPLocal drives one queue-aware AssignGate flow.
//
// 排队状态机在 AssignGateWithQueue(http_assign_gate.go),按 2026-10-15 的客户端契约推进:持有 queue_token
// 时轮询 /api/queue-status(间隔下限 2s 加抖动,queue-status 连续失败时指数退避封顶 30s),恢复用的
// assign-gate 重入被限流时回到轮询,zone 不可进(503 zone_* / 404)按错误返回,410 丢 token 重来。网关放行并给出
// gate 票时返回结果;排队预算用完、网络错误或不可恢复的回包码都以错误返回,由外层
// resolveGateViaHTTPLocal 的重试循环处理。
//
// 每个 robot 的排队统计(entered / admitted-after-wait / expired / 等待时长 / 最大名次)由状态机写进
// Stats,周期报表据此区分「排队正常但慢」与「dispatcher 卡住」。
func assignGateHTTPLocal(cfg *config.Config, zoneId uint32) (*gateAssignmentLocal, error) {
	// Total budget for queue-aware polling. Generous on purpose: a real
	// queue under load can take minutes to drain. The outer
	// resolveGateViaHTTPLocal retry loop adds another layer on top.
	const queueBudget = 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), queueBudget)
	defer cancel()

	req := &httpAssignGateRequest{
		ZoneID:  zoneId,
		Account: "", // robot doesn't have a stable account at this stage
	}
	api := httpGatewayQueueAPI{gatewayAddr: cfg.GatewayAddr, perCallTimeout: 5 * time.Second}

	// robotStatsRef 在 main() 里赋值;为 nil 时状态机不记统计。等待与抖动用生产实现(sleepCtx / uniformJitter)。
	rsp, err := AssignGateWithQueue(ctx, api, req, robotStatsRef, sleepCtx, uniformJitter)
	if err != nil {
		return nil, err
	}
	if rsp.GateIP == "" || rsp.GatePort == 0 {
		return nil, fmt.Errorf("AssignGate admitted but returned empty address")
	}
	return &gateAssignmentLocal{
		addr:      fmt.Sprintf("%s:%d", rsp.GateIP, rsp.GatePort),
		payload:   rsp.TokenPayload,
		signature: rsp.TokenSignature,
	}, nil
}

const defaultLoginTimeoutLocal = 15 * time.Second

// loginAndEnterLocal performs: login -> create player (if needed) -> enter game.
func loginAndEnterLocal(gc *pkg.GameClient, password string, stats *metrics.Stats) error {
	var lr login.LoginResponse
	if err := sendAndRecvLocal(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{Account: gc.Account, Password: password},
		&lr,
	); err != nil {
		stats.LoginFail()
		return fmt.Errorf("login: %w", err)
	}
	if lr.ErrorMessage != nil {
		stats.LoginFail()
		return fmt.Errorf("login: server error %v", lr.ErrorMessage)
	}

	if lr.AccessToken != "" {
		gc.SetTokens(lr.AccessToken, lr.RefreshToken, lr.AccessTokenExpire, lr.RefreshTokenExpire)
	}

	if len(lr.Players) == 0 {
		var cr login.CreatePlayerResponse
		if err := sendAndRecvLocal(gc, stats,
			game.ClientPlayerLoginCreatePlayerMessageId,
			&login.CreatePlayerRequest{},
			&cr,
		); err != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: %w", err)
		}
		if cr.ErrorMessage != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: server error %v", cr.ErrorMessage)
		}
		lr.Players = cr.Players
	}
	if len(lr.Players) == 0 {
		stats.LoginFail()
		return fmt.Errorf("no players after create")
	}

	playerID := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecvLocal(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerID},
		&er,
	); err != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: %w", err)
	}
	if er.ErrorMessage != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: server error %v", er.ErrorMessage)
	}

	gc.PlayerId = er.PlayerId
	return nil
}

// loginAndEnterWithAuth routes to the correct login implementation based on cfg.AuthType.
// If prevAccessToken is non-empty and not near expiry, it is tried first; on
// failure we transparently fall back to the configured primary auth provider.
//
// 例外:前一种方式因握手超时失败(errLoginStuck)时不回落,直接返回该错误——超时后连接上留着阻塞的读者,
// 这条连接已不能再用。返回之后怎样取决于调用方:压测主路径(runRobotOnce / data_stress)登录失败即结束本次会话,
// 外层退避后换新连接重来。重定向重登录(pkg.SetRedirectRelogin)也走本函数,同样不回落,但那里没有外层重连:
// FollowRedirect 返回的错误目前只被 logic/handler 的重定向处理器记一行日志,RecvLoop 继续读这条未认证的新连接
// (muduo 的 Close 也唤不醒它),会话就此空转到 stop,仍按已连接计数。这是「确定失败、不做有竞态的回落」的代价;
// 让重定向失败结束会话要改 logic/handler 与 pkg,不在本文件范围。
func loginAndEnterWithAuth(gc *pkg.GameClient, cfg *config.Config, stats *metrics.Stats, prevAccessToken string, prevAccessExpire int64) error {
	// Use access_token reconnect when available and with at least 60s of TTL left.
	if prevAccessToken != "" && (prevAccessExpire == 0 || time.Now().Unix() < prevAccessExpire-60) {
		if err := loginAndEnterAccessToken(gc, prevAccessToken, stats); err == nil {
			stats.AccessReconnectOK()
			zap.L().Debug("access_token reconnect ok", zap.String("account", gc.Account))
			return nil
		} else if errors.Is(err, errLoginStuck) {
			// 握手超时后这条连接上还留着一个阻塞在 RecvOne 的读者,在同一连接上换认证方式重试会和它抢回包
			// (回落发的同样是 Login,同号回包可能被它吞掉),所以不回落,直接返回;返回后主路径换新连接重来、
			// 重定向路径会话空转,见函数注释。
			return err
		} else {
			stats.AccessReconnectFallback()
			zap.L().Info("access_token reconnect failed, falling back",
				zap.String("account", gc.Account), zap.Error(err))
		}
	}
	// New path: do the heavy auth via Java Gateway HTTP, then use the
	// returned access_token to do a lightweight Login on the gate TCP
	// channel just to bind the session. Falls back to legacy on any
	// non-2xx so a half-rolled gateway doesn't break stress runs.
	if cfg.UseHttpLogin {
		if err := loginAndEnterViaHttpGateway(gc, cfg, stats); err == nil {
			return nil
		} else if errors.Is(err, errLoginStuck) {
			// 同上:gate 上的握手已超时,这条连接不能再用(errLoginStuck)。
			return err
		} else {
			zap.L().Warn("http-login path failed, falling back to legacy gate Login RPC",
				zap.String("account", gc.Account), zap.Error(err))
		}
	}
	if cfg.AuthType == "satoken" {
		return loginAndEnterSaToken(gc, cfg.SaTokenAddr, stats)
	}
	return loginAndEnterLocal(gc, cfg.Password, stats)
}

// loginAndEnterViaHttpGateway exercises the new Java Gateway /api/login path.
//
// Why the gate TCP still runs ClientPlayerLogin.Login afterwards:
//
//	The gate-side Login RPC's job is twofold — it (a) verifies credentials
//	and (b) binds the {session_id, account} pair via loginsession.Save +
//	the device-set so that EnterGame can find the account. (a) was already
//	done over HTTP, so we use auth_type="access_token" here to skip the
//	provider round-trip; (b) still has to happen on the actual TCP socket.
//
// This is exactly the static-shape we want for production clients: one
// HTTP round trip moves the OAuth/3rd-party heavy lifting off the gate, and
// the gate Login RPC degenerates into a Redis lookup + a Kafka BindSession.
func loginAndEnterViaHttpGateway(gc *pkg.GameClient, cfg *config.Config, stats *metrics.Stats) error {
	loginReq := &httpLoginRequest{
		ZoneID:   cfg.ZoneID,
		Account:  gc.Account,
		Password: cfg.Password,
		AuthType: cfg.AuthType,
	}
	// SA-Token path: behave exactly like the legacy SA-Token flow, only
	// the credential exchange happens over HTTP instead of via the gate.
	if cfg.AuthType == "satoken" {
		tok, err := fetchSaToken(cfg.SaTokenAddr, gc.Account)
		if err != nil {
			stats.LoginFail()
			return fmt.Errorf("fetch satoken: %w", err)
		}
		gc.SaToken = tok
		loginReq.AuthToken = tok
	}

	rsp, err := httpLogin(cfg.GatewayAddr, loginReq, 10*time.Second)
	if err != nil {
		stats.LoginFail()
		return fmt.Errorf("/api/login: %w", err)
	}
	switch rsp.Code {
	case 0:
		// fall through
	case 100, 101, 429:
		// queue / rate-limit — surface as "fail" so the outer retry loop
		// honours the backoff. retry_after_ms hint is logged but the
		// outer loop owns the actual sleep.
		stats.LoginFail()
		return fmt.Errorf("/api/login queue (code=%d retry_after_ms=%d)", rsp.Code, rsp.RetryAfterMs)
	default:
		stats.LoginFail()
		return fmt.Errorf("/api/login error code=%d msg=%s", rsp.Code, rsp.Message)
	}
	if rsp.AccessToken == "" {
		stats.LoginFail()
		return fmt.Errorf("/api/login returned empty access_token")
	}
	gc.SetTokens(rsp.AccessToken, rsp.RefreshToken, rsp.AccessTokenExpire, rsp.RefreshTokenExpire)

	// Now run the cheap path on the gate TCP channel: server sees
	// auth_type="access_token" and skips provider validation entirely.
	return loginAndEnterAccessToken(gc, rsp.AccessToken, stats)
}

// errLoginStuck 标记同步握手(sendAndRecvLocal 与 login.go 的 sendAndRecvTimeout)在预算内没等到回包。
// 包裹它的错误文案仍以「STUCK: 」开头:登录测试与压测统计按 strings.Contains(err, "STUCK") 归类卡死。
//
// 见到它的调用方必须丢弃这条连接,不能在同一个 gc 上继续握手或起 RecvLoop:等回包的读者 goroutine
// 仍阻塞在 RecvOne 上(vendored muduo 的 Recv 读的是永不关闭的 incoming channel,Close 也唤不醒它),
// 之后同一连接上的任何读者都会和它抢收包——同号回包可能被它吞掉,其余消息被它塞进暂存队列。
// loginAndEnterWithAuth 因此在它之后不再换认证方式回落,直接把它返回给调用方(各调用方的后果见该函数注释)。
var errLoginStuck = errors.New("STUCK")

// sendAndRecvLocal 是 loginAndEnterLocal 的同步握手,形状与 login.go 的 sendAndRecvTimeout 相同:跑在 RecvLoop 之前,
// 或在重定向重登录时跑在 RecvLoop 之上,只能 SendRequest + RecvOne,不迁到 Call。
// 信封错误返回 *envelopeError;超时返回包裹 errLoginStuck 的错误,此时读者 goroutine 仍阻塞在 RecvOne 上,
// 调用方必须丢弃这条连接,不得再在同一个 gc 上握手或起 RecvLoop(见 errLoginStuck)。
func sendAndRecvLocal(gc *pkg.GameClient, stats *metrics.Stats, msgID uint32, req, resp proto.Message) error {
	if err := gc.SendRequest(msgID, req); err != nil {
		return err
	}
	stats.MsgSent()

	type recvResult struct {
		msg *base.MessageContent
		err error
	}
	ch := make(chan recvResult, 1)

	go func() {
		for {
			raw, err := gc.RecvOne()
			if err != nil {
				ch <- recvResult{err: err}
				return
			}
			stats.MsgRecv()
			if raw.MessageId == msgID {
				ch <- recvResult{msg: raw}
				return
			}
			gc.DeferMessage(raw)
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		// gate / 路由服级失败回的是信封错误,body 为空;只解 body 会把它当成「成功、字段全默认」——
		// Login 看成角色列表为空去建角,EnterGame 看成已进游戏。口径与 sendAndRecvTimeout 相同。
		if tipID := r.msg.GetErrorMessage().GetId(); tipID != 0 {
			return &envelopeError{msgID: msgID, tipID: tipID}
		}
		return proto.Unmarshal(r.msg.SerializedMessage, resp)
	case <-time.After(defaultLoginTimeoutLocal):
		stats.LoginStuck()
		return fmt.Errorf("%w: no response for msg_id=%d within %s", errLoginStuck, msgID, defaultLoginTimeoutLocal)
	}
}
