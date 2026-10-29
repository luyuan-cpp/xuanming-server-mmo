package main

// sync-smoke 场景:对「落地恢复」的按域版本摘要做端到端冒烟
// (docs/design/landing-recovery-sync-digest.md §4 落地取摘要 / §10 第 6 步):
//
//	S1 A、B 并发登录进场;A 立即取一次摘要:GetSyncDigest(253)→ NotifySyncDigest(254),status 必须 OK,
//	   记下 g 与各域计数(从未被 bump 过的新号 g=0、没有 entries,这是合法的);
//	S2 预清理并播种:A 删好友 B、B 解除拉黑 A、A 拒掉 B 发来的待处理申请(都幂等,业务结果码不断言;
//	   但传输层失败 —— 信封拒绝、gate 推 1003、超时 —— 即失败;GetPendingRequests 读失败(含业务 tip≠0)同样判失败,
//	   读不到就无从确认残留;拒完后复查 A 的收件箱仍有 B 的 PENDING 申请也失败),
//	   最后 A 解除拉黑 B 必须受理 —— friend 的 Unblock 无条件给本人 bump FRIEND_BLOCK(设计 §3.3),
//	   所以这一步之后 A 的摘要 hash 一定存在。再取摘要作为基线:g ≠ 0、FRIEND_BLOCK 比 S1 大,
//	   且 S1 的 g 非 0 时 g 不变(hash 已存在时 bump 不改写 g);
//	S3 B 向 A 发好友申请(必须受理)→ A 再取摘要:只有 FRIEND_REQUEST 恰好 +1、g 不变;
//	   按客户端的比对规则(§4.3 第 3 步)算出的待拉取域恰好是 {FRIEND_REQUEST}。
//	   受理回的 request_id 若是预清理见过的 PENDING 申请即失败:friend 对已有 pending 做同代次重放(Changed=false)、不 bump;
//	S4 短重连:A 不发 LeaveGame 直接断开,syncSmokeReconnectDelay(50ms)后新建连接,固定再等 200ms(connectAndVerify)
//	   验票后按 player_id 严格重登(Login 比 Close 晚约 ≥250ms + RTT)→ 再取摘要:g 与各域计数与 S3 完全一致(待拉取域为空);
//	S5 跨 zone 落地(sync_smoke.cross_zone=true 才做,缺省关):TravelToZone{visit_zone} → 跟随 msg 124 严格重登
//	   → 落地取摘要与 S4 一致 → TravelToZone{zone_id} 回家 → 再取一次仍一致;
//	S6 负向:A 正常下线、断开、等 scene 销毁实体后重登,在 EnterGame 回包之后、RecvLoop 启动(处理 NotifyEnterScene)
//	   之前立刻发 GetSyncDigest。设计 §4.2:实体还在异步载入时 scene 丢弃请求且不回包。探针落在哪个结局取决于时序
//	   (dropped / replied_ok / replied_unavailable / envelope_rejected),这些**只记录不断言**;与时序无关的几条硬断言
//	   (syncSmokeJudgeProbe):探针之后会话没被踢;254 解不开即失败(契约破坏);信封 1008 即失败(冒烟自己的节流失效);
//	   replied_ok 时回的摘要必须与之前一致。最后进场完成后再取摘要必须 OK 且与之前一致 —— 证明探针没把 scene 打崩、会话仍可用;
//	S7 收尾:A 拒掉 B 的申请(尽力而为,失败或复查有残留只打 WARN;下一轮的 S2 会再清一次)。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `SYNC_SMOKE_OK player_a=… player_b=… g=… friend_request=… probe=… cross_zone=…`,退出码 0;
//	任一步失败 → `SYNC_SMOKE_FAIL step=… reason=…`,退出码 1。
//	每步通过另打一行 `[sync-smoke] PASS step=…`;S6 的探针行为打一行 `[sync-smoke] OBSERVE step=S6-probe …`。
//
// 与其它冒烟不一样、不照做就会假绿或误报的几件事:
//   - 253 的应答类型是 Empty(scene 不回包),数据走 254 推送。本会话只有主流程会发 253,254 也只在处理 253 时才发
//     (scene 侧没有别的发送点),所以每次请求前记下留底下标 mark,之后等到的第一条 254 就是这次请求带回来的
//     (上一次请求超时后迟到的推送也可能被认领 —— 它同样是一份合法摘要,只是略旧;S6 探针的迟到回包同理);
//   - 摘要比较一律用「不相等」,g 只在 hash 被重建时变化(§3.2)。S3 要求「恰好 +1」:B 的一次 AddFriend 只给 A bump 一次。
//     预清理里会给 A bump 的操作全部放在 S2 基线之前;friend 的 bump 在 RPC 回包之前同步完成
//     (go/friend/internal/logic/friend_logic.go bumpSyncDigest),回包到了 bump 就已落地;
//   - 限流:253 不在 MessageLimiter 表里,负责人 2026-10-16 拍板接受默认档、不加行(设计 §4.1 原写「与 SceneInfoC2S 同档」,
//     而 43 本身也不在表里,同档即默认档)。gate 默认档按会话、按消息号计数,时间取整秒、按整秒淘汰:
//     同一消息号在当前与上一整秒内合计最多 3 次(真实时间 1~2 秒,持续速率约 1.5 次/秒,不是「每秒 3 次」)。
//     同一机器人相邻请求至少隔 syncSmokeRequestSpacing,两次 253 之间至少隔 syncSmokeDigestSpacing(> 1 秒:每个整秒至多 1 次,
//     判定时缓冲区至多 2 条,永不触发),节奏跨重连保留,S6 探针也走同一节流;
//   - 好友请求与 TravelToZone 走 gc.Call,回包按请求号认领(docs/design/client-rpc-callback.md §6),不进留底、
//     不经 onMessage;好友消息号上 id = 0 的消息仍须在通用分发之前全部拦下(friendSmokeIsFriendMessage):
//     信封拒绝落进通用分发会被解成全零响应,看上去像 tip=0 受理;
//   - 留底与重定向进度直接复用 travel-smoke 的类型(travelSmokeRecord / travelSmokeProgress),跨 zone 腿用
//     travelSmokeStartTravel 发请求、travelSmokeScanHop 判传送窗口、travelSmokeStrictRelogin 严格重登 ——
//     传送链路的完整断言归 travel-smoke,这里只做落地判据(player_id 不变、票据 zone == 目标、gate 换了)。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	friendpb "proto/friend"
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
// 99xx 段内与 ordering-smoke(9901)错开,取 9911 / 9912;与 battle 9001-9004、chat 9005/9006、属性 9101、宠物 9102、
// 装备 9103/9104、guild 92xx、team 93xx、trade 94xx、资产通道 95xx、travel 9601、friend 97xx、channel 9801 都不撞,
// 避免并行冒烟互相顶号或互相改好友关系(S3/S4 要求期间没有别的业务给 A bump)。
const (
	syncSmokeAccountA = "robot_9911" // 被观测方:取摘要、收好友申请、短重连、(可选)跨 zone、负向探针
	syncSmokeAccountB = "robot_9912" // 触发方:向 A 发一条好友申请,让 A 的 FRIEND_REQUEST 域 +1
)

const (
	// 等一次同步回包 / 一条 NotifySyncDigest 的预算。摘要是一次本地 HGETALL,好友 RPC 经路由服;
	// 10s 大于路由服 ForwardTimeoutMs(5s)与 friend zrpc Timeout(4000ms),先看到信封拒绝而不是本地超时。
	syncSmokeRpcTimeout = 10 * time.Second
	// 等进场的预算,与 channel-smoke / friend-smoke 同值。
	syncSmokeSceneReadyTimeout = 15 * time.Second
	// 同一机器人相邻两个请求(任意消息号)的最小间隔。1.1s 下 gate 默认限流档(推导见 chat_smoke_scenario.go)的缓冲区至多 2 条。
	syncSmokeRequestSpacing = 1100 * time.Millisecond
	// 同一机器人两次 GetSyncDigest(253)之间的最小间隔:同一消息号两次发送至少隔 1 秒(限流)。
	syncSmokeDigestSpacing = 1100 * time.Millisecond
	// S3:B 的申请受理后,等 A 的摘要出现变化的总预算。friend 的 bump 在回包前同步完成,第一次取就该看到;
	// 留一点余量只为容忍将来改成异步 bump,不是为了吞掉"没 bump"。
	syncSmokeBumpWaitBudget = 5 * time.Second
	// S4:断开旧连接后多久发起新连接。connectAndVerify 建连后还要固定等 200ms 再验票、登录,
	// 所以 Login 比 Close 晚约 ≥250ms + RTT(不是 50ms)。
	syncSmokeReconnectDelay = 50 * time.Millisecond
	// S6:LeaveGame 之后、关连接之前的等待,与 login_test_scenarios.go testLeaveAndReLogin 同值(让 LeaveGame 先送达)。
	syncSmokeLeaveFlush = 300 * time.Millisecond
	// S6:关连接之后等 scene 销毁实体再重登,让探针尽量落进「实体异步载入」窗口。等不够只会让探针拿到正常回包(记为 replied_ok),
	// 不影响通过判定。
	syncSmokeLeaveSettle = time.Second
	// S6:探针发出后收集其结局的窗口。scene 回包是本地毫秒级;窗口内既没回包也没信封拒绝 = dropped。
	syncSmokeProbeSettle = 3 * time.Second
	// S5:一跳传送的总预算,与 travel-smoke 的 travelSmokeRedirectTimeout 同值(推导见那里)。
	syncSmokeRedirectTimeout = 60 * time.Second
	// 留底扫描的轮询粒度。
	syncSmokePollInterval = 50 * time.Millisecond
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipSyncRateLimited        = uint32(tiptable.CommonError_kRateLimitExceeded)
	tipSyncServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
)

// errSyncSmokeNotBumpedYet:摘要相对基线没有任何变化(g 也没变)。S3 据此在预算内重取,而不是立刻判失败。
var errSyncSmokeNotBumpedYet = errors.New("摘要相对基线没有任何变化")

// errSyncSmokeTimeout 是好友请求等回包超时的哨兵(smokeCaller 的 timeoutErr),拼进失败原因。
var errSyncSmokeTimeout = errors.New("got no reply within")

// S6 探针的结局。取决于时序的只记录;与时序无关的硬判定见 syncSmokeJudgeProbe。
const (
	syncSmokeProbeDropped            = "dropped"             // 窗口内既没有 254 也没有信封拒绝:scene 丢弃了请求(设计 §4.2 的预期)
	syncSmokeProbeRepliedOK          = "replied_ok"          // 收到 status=OK 的 254:实体已就绪(或被复用)
	syncSmokeProbeRepliedUnavailable = "replied_unavailable" // 收到 status=UNAVAILABLE 的 254
	syncSmokeProbeEnvelopeRejected   = "envelope_rejected"   // gate 对 253 回了信封拒绝(会话还没路由到 scene / 限流)
	syncSmokeProbeUndecodable        = "undecodable"         // 收到 254 但解不开
)

// syncSmokeDigest 是一份解析后的 NotifySyncDigest。versions 只含推送里出现过的域,缺席的域按 0 处理(proto 注释)。
type syncSmokeDigest struct {
	status     scene.SyncDigestStatus
	generation uint64
	versions   map[scene.SyncDomain]uint64
}

// syncSmokeDomainChange 是一个域在两份摘要之间的计数变化。
type syncSmokeDomainChange struct {
	domain        scene.SyncDomain
	before, after uint64
}

// syncSmokeProbe 是 S6 负向探针观察到的行为。
type syncSmokeProbe struct {
	outcome     string
	envelopeTip uint32 // outcome == envelope_rejected 时的信封 tip
	// replyBeforeEnterScene:254 排在 NotifyEnterScene 之前到达(登录握手期间暂存的 NotifyEnterScene 在 RecvLoop
	// 入口补投,也算"之前到达")。只在收到 254 时有意义。
	replyBeforeEnterScene bool
	// kicked:探针之后收到过 KickPlayer。
	kicked bool
	// digest:outcome 为 replied_ok / replied_unavailable 时解码出的那条 254(proto 解码,未过契约校验),其余结局为 nil。
	// 指针相等没有意义,比较两个 syncSmokeProbe 要逐字段比。
	digest *scene.SyncDigestS2C
}

// syncSmokeBot 是一个已登录进场的机器人会话。
type syncSmokeBot struct {
	account  string
	password string
	gc       *pkg.GameClient
	player   *gameobject.Player
	stats    *metrics.Stats
	// homePlayer 是首登拿到的 player_id;重连与跨 zone 重登都只认它。RecvLoop 启动前写入,之后只读。
	homePlayer uint64

	// RecvLoop goroutine 写、主流程读,mu 保护。records 只追加,跨重连保留:扫描一律从请求前记下的 mark 开始,
	// 旧连接的留底不会被新请求误认。
	mu       sync.Mutex
	records  []travelSmokeRecord
	progress travelSmokeProgress
	// caller 发好友请求并只等本次请求的回包(smoke_rpc_call.go)。它绑定一个 GameClient:重定向只换内部连接、
	// gc 不变,caller 照用;reconnect 换了新的 GameClient,caller 随之重建。主流程是唯一写者,写时持 mu;
	// onMessage(RecvLoop)持 mu 读;主流程自己读不加锁。
	caller *smokeCaller

	// 以下只由主流程读写。跨重连保留:保守起见不假设换连接就换了限流窗口。
	lastRequestAt time.Time
	lastDigestAt  time.Time
}

// RunSyncSmoke 是 main.go `mode: sync-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunSyncSmoke(cfg *config.Config) {
	// loginAndEnterScenario / buildScenarioLoginRequest 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.SyncSmoke
	registered := syncSmokeRegisteredDomains()

	var subject, peer *syncSmokeBot
	cleanup := func() {
		for _, bot := range []*syncSmokeBot{subject, peer} {
			if bot != nil {
				bot.close()
			}
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("SYNC_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[sync-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- S1:登录(并发)+ 首次取摘要 ----
	type loginSpec struct {
		account string
		// strictRelogin:是否把重定向后的重登换成「按 player_id 严格重登」。只有要走跨 zone 腿的 A 需要。
		strictRelogin bool
	}
	specs := []loginSpec{{syncSmokeAccountA, sc.CrossZone}, {syncSmokeAccountB, false}}
	loggedIn := make([]*syncSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			loggedIn[idx], loginErrs[idx] = syncSmokeLogin(cfg, spec.account, spec.strictRelogin, stats)
		}(i, spec)
	}
	wg.Wait()
	// 先收齐已登录的会话再判错:否则某人失败时另一人的会话不会被 cleanup 收尾。
	subject, peer = loggedIn[0], loggedIn[1]
	for i, err := range loginErrs {
		if err != nil {
			fail("S1-login", "account=%s zone=%d err=%v", specs[i].account, cfg.ZoneID, err)
		}
	}
	d1, err := subject.fetchDigest()
	if err != nil {
		fail("S1-digest", "%v", err)
	}
	pass("S1-login", "player_a=%d gate_a=%s player_b=%d gate_b=%s 首登摘要=%s 首登待拉取域=%d 个(无本地缓存 = 全部已知域)",
		subject.homePlayer, subject.gateAddr(), peer.homePlayer, peer.gateAddr(), syncSmokeDescribe(d1),
		len(syncSmokeDirtyDomains(nil, d1, registered)))

	// ---- S2:预清理 + 播种 + 基线 ----
	// precleanSeenPending:预清理第一次拉取时见到的、B 发来且仍为 PENDING 的 request_id,S3 据此识别同代次重放。
	precleanSeenPending, err := syncSmokePreclean(subject, peer)
	if err != nil {
		fail("S2-preclean", "%v", err)
	}
	seedTip, err := subject.friendRpc(game.ClientPlayerFriendUnblockMessageId,
		&friendpb.UnblockRequest{TargetPlayerId: peer.homePlayer}, &friendpb.UnblockResponse{})
	if err != nil {
		fail("S2-seed", "A Unblock(B): %v", err)
	}
	if seedTip != 0 {
		fail("S2-seed", "A Unblock(B) was rejected with tip=%d%s; expected accepted (Unblock is idempotent and succeeds even when B was never blocked)",
			seedTip, syncSmokeTipHint(game.ClientPlayerFriendUnblockMessageId, seedTip))
	}
	d2, err := subject.fetchDigest()
	if err != nil {
		fail("S2-baseline", "%v", err)
	}
	if d2.generation == 0 {
		fail("S2-baseline", "A Unblock(B) was accepted, but A's digest still does not exist (g=0). friend's Unblock always bumps FRIEND_BLOCK for the caller: "+
			"friend is not running a binary with sync-digest bumps; friend's SharedRedis is not the same Redis/DB the scene reads digests from; or the bump failed "+
			"(friend log [playersync] WARN / metric player_sync_bump_total{service=\"friend\",result=\"error\"}). Check manually: redis-cli HGETALL player:%d:sync_digest",
			subject.homePlayer)
	}
	if err := syncSmokeCheckGenerationStable(d1, d2); err != nil {
		fail("S2-baseline", "S1 -> S2: %v; S1 %s, S2 %s", err, syncSmokeDescribe(d1), syncSmokeDescribe(d2))
	}
	blockDomain := scene.SyncDomain_SYNC_DOMAIN_FRIEND_BLOCK
	if d2.versions[blockDomain] <= d1.versions[blockDomain] {
		fail("S2-baseline", "FRIEND_BLOCK went %d -> %d, expected an increase (A just unblocked B and Unblock always bumps this domain for the caller); S1 %s, S2 %s",
			d1.versions[blockDomain], d2.versions[blockDomain], syncSmokeDescribe(d1), syncSmokeDescribe(d2))
	}
	pass("S2-baseline", "基线=%s 相对 S1 的待拉取域=%s", syncSmokeDescribe(d2),
		syncSmokeDomainList(syncSmokeDirtyDomains(&d1, d2, registered)))

	// ---- S3:B 向 A 发好友申请 → A 的摘要只有 FRIEND_REQUEST +1 ----
	added := &friendpb.AddFriendResponse{}
	addTip, err := peer.friendRpc(game.ClientPlayerFriendAddFriendMessageId,
		&friendpb.AddFriendRequest{TargetPlayerId: subject.homePlayer}, added)
	if err != nil {
		fail("S3-add-friend", "B AddFriend(A): %v", err)
	}
	if addTip != 0 {
		fail("S3-add-friend", "B AddFriend(A) was rejected with tip=%d%s; expected accepted. Already being friends, or either side blocking the other, is rejected "+
			"(did the S2 preclean leave A and B as friends or blocked? see the [sync-smoke] preclean DEBUG lines); an existing pending request from B "+
			"is not rejected but accepted as a replay without a bump (checked right below via the request_id)",
			addTip, syncSmokeTipHint(game.ClientPlayerFriendAddFriendMessageId, addTip))
	}
	if added.GetRequestId() == 0 {
		fail("S3-add-friend", "B AddFriend(A) was accepted but returned request_id=0")
	}
	if syncSmokeIsReplayedRequest(precleanSeenPending, added.GetRequestId()) {
		fail("S3-add-friend", "B AddFriend(A) was accepted with request_id=%d, which the S2 preclean saw PENDING in A's inbox: friend replays an existing "+
			"pending request with the same id (Changed=false) and does not bump, so the preclean did not actually clear the residual request "+
			"(even though its re-check found none; see the [sync-smoke] preclean DEBUG / RejectFriend WARN lines and the friend log)",
			added.GetRequestId())
	}
	friendRequest := scene.SyncDomain_SYNC_DOMAIN_FRIEND_REQUEST
	bumpDeadline := time.Now().Add(syncSmokeBumpWaitBudget)
	var d3 syncSmokeDigest
	for attempt := 1; ; attempt++ {
		if d3, err = subject.fetchDigest(); err != nil {
			fail("S3-digest", "%v", err)
		}
		err = syncSmokeCheckSingleBump(d2, d3, friendRequest)
		if err == nil {
			break
		}
		if !errors.Is(err, errSyncSmokeNotBumpedYet) {
			fail("S3-only-friend-request", "%v; S2 %s, S3 %s", err, syncSmokeDescribe(d2), syncSmokeDescribe(d3))
		}
		if !time.Now().Before(bumpDeadline) {
			fail("S3-only-friend-request", "B's friend request was accepted (request_id=%d), but A's digest did not change within %s (%d fetches): %s. "+
				"friend bumps the receiver's FRIEND_REQUEST only when the request actually changed (change.Changed); check the friend log for [playersync] WARN "+
				"and player_sync_bump_total{service=\"friend\",result=\"error\"}",
				added.GetRequestId(), syncSmokeBumpWaitBudget, attempt, syncSmokeDescribe(d3))
		}
		zap.L().Info("[sync-smoke] S3 digest has not changed yet; retrying", zap.Int("attempt", attempt))
	}
	if dirty := syncSmokeDirtyDomains(&d2, d3, registered); !slices.Equal(dirty, []scene.SyncDomain{friendRequest}) {
		fail("S3-dirty-domains", "Domains to pull under the client comparison rule (design 4.3 step 3) = %s, expected only FRIEND_REQUEST; S2 %s, S3 %s",
			syncSmokeDomainList(dirty), syncSmokeDescribe(d2), syncSmokeDescribe(d3))
	}
	pass("S3-only-friend-request", "request_id=%d FRIEND_REQUEST %d→%d g=%d 待拉取域=%s",
		added.GetRequestId(), d2.versions[friendRequest], d3.versions[friendRequest], d3.generation,
		syncSmokeDomainList(syncSmokeDirtyDomains(&d2, d3, registered)))

	// ---- S4:短重连 → 摘要原样延续 ----
	if err := subject.reconnect(cfg, false, nil); err != nil {
		fail("S4-reconnect", "%v", err)
	}
	d4, err := subject.fetchDigest()
	if err != nil {
		fail("S4-digest", "%v", err)
	}
	if err := syncSmokeCheckUnchanged(d3, d4); err != nil {
		fail("S4-unchanged", "After a short reconnect: %v; S3 %s, S4 %s", err, syncSmokeDescribe(d3), syncSmokeDescribe(d4))
	}
	pass("S4-reconnect", "gate=%s 摘要=%s 待拉取域=%s", subject.gateAddr(), syncSmokeDescribe(d4),
		syncSmokeDomainList(syncSmokeDirtyDomains(&d3, d4, registered)))
	last := d4

	// ---- S5:跨 zone 落地(可选) ----
	crossZone := "off"
	if sc.CrossZone {
		homeZone := cfg.ZoneID
		if err := subject.travelTo(sc.VisitZone, sc.SceneConfigId); err != nil {
			fail("S5-travel-out", "%v", err)
		}
		visit, err := subject.fetchDigest()
		if err != nil {
			fail("S5-visit-digest", "%v", err)
		}
		if err := syncSmokeCheckUnchanged(last, visit); err != nil {
			fail("S5-visit-unchanged", "After landing in zone %d: %v. The scenes of both zones must read the same core Redis that holds player:{id}:sync_digest; "+
				"before %s, after %s", sc.VisitZone, err, syncSmokeDescribe(last), syncSmokeDescribe(visit))
		}
		pass("S5-visit", "visit_zone=%d gate=%s 摘要=%s", sc.VisitZone, subject.gateAddr(), syncSmokeDescribe(visit))

		if err := subject.travelTo(homeZone, sc.SceneConfigId); err != nil {
			fail("S5-travel-home", "%v", err)
		}
		back, err := subject.fetchDigest()
		if err != nil {
			fail("S5-home-digest", "%v", err)
		}
		if err := syncSmokeCheckUnchanged(visit, back); err != nil {
			fail("S5-home-unchanged", "After returning to zone %d: %v; before %s, after %s",
				homeZone, err, syncSmokeDescribe(visit), syncSmokeDescribe(back))
		}
		pass("S5-home", "home_zone=%d gate=%s 摘要=%s", homeZone, subject.gateAddr(), syncSmokeDescribe(back))
		last = back
		crossZone = "checked"
	}

	// ---- S6:负向 —— 实体就绪前发请求 ----
	probeSince := -1
	var probeSentAt time.Time
	var probeSentAfterEnterGame time.Duration
	probeHook := func(enteredGameAt time.Time) error {
		since, err := subject.sendDigestRequest()
		if err != nil {
			return fmt.Errorf("send the pre-ready GetSyncDigest probe: %w", err)
		}
		probeSince = since
		probeSentAt = time.Now()
		probeSentAfterEnterGame = probeSentAt.Sub(enteredGameAt)
		return nil
	}
	if err := subject.reconnect(cfg, true, probeHook); err != nil {
		fail("S6-relogin", "%v", err)
	}
	if wait := time.Until(probeSentAt.Add(syncSmokeProbeSettle)); wait > 0 {
		time.Sleep(wait)
	}
	probeRecords, _ := subject.snapshot(probeSince)
	probe := syncSmokeClassifyProbe(probeRecords)
	zap.L().Info(fmt.Sprintf("[sync-smoke] OBSERVE step=S6-probe outcome=%s envelope_tip=%d reply_before_enter_scene=%t kicked=%t sent_after_enter_game_ms=%d",
		probe.outcome, probe.envelopeTip, probe.replyBeforeEnterScene, probe.kicked, probeSentAfterEnterGame.Milliseconds()))
	if err := syncSmokeJudgeProbe(probe, last); err != nil {
		fail("S6-probe", "%v (check gate / scene / login logs around player_id=%d)", err, subject.homePlayer)
	}
	d6, err := subject.fetchDigest()
	if err != nil {
		fail("S6-liveness", "After the pre-ready probe the session or scene no longer answers digests (did the scene process crash? check its log and process state): %v", err)
	}
	if err := syncSmokeCheckUnchanged(last, d6); err != nil {
		fail("S6-liveness", "After leave + relogin: %v; before %s, after %s", err, syncSmokeDescribe(last), syncSmokeDescribe(d6))
	}
	pass("S6-probe", "probe=%s 之后摘要=%s", probe.outcome, syncSmokeDescribe(d6))

	// ---- S7:收尾(尽力而为:失败与残留只打 WARN;下一轮的 S2 预清理会再清一次,清不掉才判失败) ----
	_, rejected, residual, err := subject.rejectPendingFrom(peer.homePlayer)
	if err != nil || len(residual) > 0 {
		zap.L().Warn("[sync-smoke] S7 tidy-up may have left B's request(s) pending in A's inbox; the next run's S2 preclean clears them again",
			zap.Int("rejected", rejected), zap.Uint64s("residual_request_ids", residual), zap.Error(err))
	} else {
		zap.L().Info("[sync-smoke] S7 tidy-up: rejected B's pending request(s) in A's inbox", zap.Int("rejected", rejected))
	}

	zap.L().Info(fmt.Sprintf("SYNC_SMOKE_OK player_a=%d player_b=%d gate_a=%s g=%d friend_request=%d->%d probe=%s cross_zone=%s",
		subject.homePlayer, peer.homePlayer, subject.gateAddr(), d6.generation,
		d2.versions[friendRequest], d3.versions[friendRequest], probe.outcome, crossZone))
	cleanup()
	_ = zap.L().Sync()
}

// syncSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 首登走通用登录(允许建角:冒烟账号第一次跑时要在 zone_id 建角);之后的重连与跨 zone 重登一律按 player_id 严格重登。
// strictRelogin 与 pkg.Clients 登记都必须早于 RecvLoop 启动:登录握手期间暂存的推送(可能就有一条 msg 124)会在 RecvLoop 入口补投。
func syncSmokeLogin(cfg *config.Config, account string, strictRelogin bool, stats *metrics.Stats) (*syncSmokeBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, err := syncSmokeConnect(host, port, account, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("connect gate: %w", err)
	}
	if err := loginAndEnterScenario(gc, cfg.Password, stats); err != nil {
		gc.Close()
		return nil, fmt.Errorf("login and enter: %w", err)
	}

	bot := &syncSmokeBot{
		account:    account,
		password:   cfg.Password,
		gc:         gc,
		player:     gameobject.NewPlayer(gc.PlayerId),
		stats:      stats,
		homePlayer: gc.PlayerId,
		caller:     newSmokeCaller(gc, stats, account, syncSmokeRpcTimeout, errSyncSmokeTimeout),
	}
	if strictRelogin {
		// 覆盖 main.go 注册的通用重登(它在角色列表为空时会自动建角),理由同 travel-smoke。
		// SetRedirectRelogin 是进程全局的,本模式独占进程,覆盖是安全的。
		pkg.SetRedirectRelogin(bot.relogin)
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	// 不登记的话 msg 124 的 handler 反查不到连接,只打一行 WARN 就返回,跨 zone 腿表现为静默超时。
	pkg.Clients.Register(gc.PlayerId, gc)
	go gc.RecvLoop(bot.onMessage)

	if err := bot.waitSceneReady(); err != nil {
		sendDisconnectBestEffort(gc)
		pkg.Clients.Unregister(gc.PlayerId, gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, err
	}
	return bot, nil
}

// syncSmokeConnect 给共享的 connectAndVerify 套一个上界(不改共享 helper,别的场景依赖它):
// 它里面的 VerifyGateToken 阻塞在没有截止时间的 Recv 上,而 muduo 是异步拨号、拨不通就每秒重拨永不放弃,
// gate 不可达时冒烟会永久挂起,既不打 SYNC_SMOKE_FAIL 也不退出。
// 超时(syncSmokeSceneReadyTimeout:建连 + 验票只是一次本地往返,与进场同档的预算足够宽)返回 error;
// 后台 goroutine 继续等迟到的结果并 Close 拿到的连接。真挂死时那个 goroutine 等不到结果,随进程退出一起结束。
func syncSmokeConnect(host string, port int, account string, tokenPayload, tokenSig []byte) (*pkg.GameClient, error) {
	type connectResult struct {
		gc  *pkg.GameClient
		err error
	}
	done := make(chan connectResult, 1) // 带缓冲:超时后迟到的结果写进来不会阻塞
	go func() {
		gc, err := connectAndVerify(host, port, account, tokenPayload, tokenSig)
		done <- connectResult{gc: gc, err: err}
	}()
	select {
	case r := <-done:
		return r.gc, r.err
	case <-time.After(syncSmokeSceneReadyTimeout):
		go func() {
			if late := <-done; late.gc != nil {
				late.gc.Close()
			}
		}()
		return nil, fmt.Errorf("connect + gate token verify at %s did not finish within %s (gate unreachable, or no reply to ClientTokenVerifyRequest)",
			net.JoinHostPort(host, strconv.Itoa(port)), syncSmokeSceneReadyTimeout)
	}
}

// waitSceneReady 等本次进场的 NotifyEnterScene(通用 handler 收到后 Signal)。每次重连都换了一个新 Player,
// 所以它等的一定是这次进场,不会被上一次的 sync.Once 提前放行。
func (b *syncSmokeBot) waitSceneReady() error {
	ctx, cancel := context.WithTimeout(context.Background(), syncSmokeSceneReadyTimeout)
	defer cancel()
	if err := b.player.WaitSceneReady(ctx); err != nil {
		return fmt.Errorf("wait scene ready(account=%s): %w", b.account, err)
	}
	return nil
}

// close 尽力收尾一个会话:LeaveGame + 断线通知 + 摘掉登记 + 关连接。连接可能已断(重连失败、搬迁途中失败),失败一律忽略。
func (b *syncSmokeBot) close() {
	_ = leaveGame(b.gc, b.stats)
	sendDisconnectBestEffort(b.gc)
	pkg.Clients.Unregister(b.homePlayer, b.gc)
	gameobject.PlayerList.Delete(b.homePlayer)
	b.gc.Close()
}

// reconnect 让本机器人换一条新连接、按 player_id 严格重登并等进场完成:
//
//	graceful=false:短重连(S4)—— 不发 LeaveGame,直接断开旧连接,syncSmokeReconnectDelay 后发起新连接
//	                (connectAndVerify 再固定等 200ms 验票,Login 比 Close 晚约 ≥250ms + RTT);
//	graceful=true :正常下线(S6)—— LeaveGame、断开、等 syncSmokeLeaveSettle 让 scene 销毁实体,再重登。
//
// afterEnterGame 在 EnterGame 回包之后、RecvLoop 启动之前调用(S6 在这里发负向探针),入参是 EnterGame 回包到达的时刻;可为 nil。
// 此刻还没有人读新连接,探针之后到达的消息都会在 RecvLoop 启动后按到达顺序进留底。
//
// 已知并接受:旧连接的 RecvLoop goroutine 会永久阻塞(泄漏一个),原因见 pkg/client.go SwapConn 的注释
// (vendored muduo 的 Close 只关 outgoing、不关 incoming)。冒烟进程寿命很短,每轮只泄漏两个;根治要改 vendor,不在本场景范围。
func (b *syncSmokeBot) reconnect(cfg *config.Config, graceful bool, afterEnterGame func(enteredGameAt time.Time) error) error {
	// 先拿新票据再断旧连接:短重连要在断开后 syncSmokeReconnectDelay 内发起新连接,不能把 assign-gate 的 HTTP 往返算进去。
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return fmt.Errorf("resolve gate address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("bad gate port %q: %w", portStr, err)
	}

	old := b.gc
	if graceful {
		_ = leaveGame(old, b.stats)
		time.Sleep(syncSmokeLeaveFlush)
	}
	pkg.Clients.Unregister(b.homePlayer, old)
	old.Close()
	if graceful {
		time.Sleep(syncSmokeLeaveSettle)
	} else {
		time.Sleep(syncSmokeReconnectDelay)
	}

	gc, err := syncSmokeConnect(host, port, b.account, tokenPayload, tokenSig)
	if err != nil {
		return fmt.Errorf("reconnect gate %s:%s: %w", host, portStr, err)
	}
	// 严格重登:只认首登的 player_id,绝不自动建角(理由见 travelSmokeStrictRelogin)。
	if err := travelSmokeStrictRelogin(gc, b.password, b.stats, b.homePlayer); err != nil {
		gc.Close()
		return fmt.Errorf("relogin on %s:%s: %w", host, portStr, err)
	}
	enteredGameAt := time.Now()

	b.gc = gc
	b.mu.Lock()
	b.caller = newSmokeCaller(gc, b.stats, b.account, syncSmokeRpcTimeout, errSyncSmokeTimeout)
	b.mu.Unlock()
	// WaitSceneReady 由 sync.Once 关闭,每次进场都要一个新的 Player,否则会被上一次进场提前放行。
	b.player = gameobject.NewPlayer(b.homePlayer)
	gameobject.PlayerList.Set(b.homePlayer, b.player)
	pkg.Clients.Register(b.homePlayer, gc)
	if afterEnterGame != nil {
		if err := afterEnterGame(enteredGameAt); err != nil {
			return err
		}
	}
	go gc.RecvLoop(b.onMessage)
	return b.waitSceneReady()
}

// relogin 是注册给 pkg.SetRedirectRelogin 的重登实现:严格重登 + 把过程与结果记给主流程(同 travel-smoke)。
// 跑在 RecvLoop goroutine 上(pkg.FollowRedirect 调用),此刻 onMessage 不持有 b.mu。
func (b *syncSmokeBot) relogin(gc *pkg.GameClient) error {
	b.mu.Lock()
	b.progress.reloginStarted++
	b.mu.Unlock()

	err := travelSmokeStrictRelogin(gc, b.password, b.stats, b.homePlayer)

	b.mu.Lock()
	b.progress.reloginFinished++
	b.progress.reloginErr = err
	b.mu.Unlock()
	return err
}

// syncSmokeIsRecorded:本场景按到达顺序留底的消息号:推送,以及单向请求 253 的信封拒绝。
// 请求-响应调用(好友请求、TravelToZone)的回包按请求号交给各自的 Call,不进留底。
func syncSmokeIsRecorded(messageId uint32) bool {
	switch messageId {
	case game.SceneSceneClientPlayerGetSyncDigestMessageId, // 应答类型是 Empty、scene 不回包;留底只为看见 gate 的信封拒绝
		game.SceneSceneClientPlayerNotifySyncDigestMessageId,
		game.SceneSceneClientPlayerNotifyEnterSceneMessageId,
		game.SceneClientPlayerCommonSendTipToClientMessageId,
		game.SceneClientPlayerCommonKickPlayerMessageId,
		game.SceneClientPlayerCommonRedirectToGateMessageId:
		return true
	}
	return false
}

// onMessage:先留底,再分发。
//   - 好友请求与 TravelToZone 的回包带请求号,由 GameClient 交给 caller / 在途的那次 TravelToZone 调用,不会到这里。
//     这些消息号上 id = 0 的消息说明服务端没回显请求号,交给 smokeDropUncorrelatedReply 丢弃(TravelToZone 的
//     丢弃前持 b.mu 记一条诊断,含回包体里的同步拒绝码,travelTo 的超时文案据此说出真实原因,判定不变);好友推送
//     NotifyFriendEvent 本场景不消费,直接丢掉。两者都不进通用分发(见 friendSmokeIsFriendMessage 的注释);
//   - gate 的 SendTipToClient 交给 caller:kServiceUnavailable 让正在等的好友请求立刻失败;
//   - 其余无条件转发:NotifyEnterScene 靠通用 handler 放行 WaitSceneReady;msg 124 在转发里同步跑完整个搬迁
//     (pkg.FollowRedirect);
//   - 转发时**绝不能持有 b.mu**:严格重登(b.relogin)要拿 b.mu,FollowRedirect 末尾的 ReplayDeferred 还会在同一个
//     goroutine 里重入本函数。
func (b *syncSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	messageId := msg.GetMessageId()
	if messageId == game.ClientPlayerFriendNotifyFriendEventMessageId {
		return
	}
	if friendSmokeIsFriendMessage(messageId) || messageId == travelToZoneMessageId {
		if messageId == travelToZoneMessageId {
			b.mu.Lock()
			b.progress.uncorrelatedTravel.note(msg)
			b.mu.Unlock()
		}
		smokeDropUncorrelatedReply(b.account, msg)
		return
	}
	if syncSmokeIsRecorded(messageId) {
		b.mu.Lock()
		b.records = append(b.records, travelSmokeRecord{
			messageId:   messageId,
			envelopeTip: msg.GetErrorMessage().GetId(),
			body:        msg.GetSerializedMessage(),
		})
		b.mu.Unlock()
	}
	if messageId == game.SceneClientPlayerCommonSendTipToClientMessageId {
		b.mu.Lock()
		caller := b.caller
		b.mu.Unlock()
		caller.onGateTipMessage(msg)
	}

	handler.MessageBodyHandler(client, msg)

	if messageId == game.SceneClientPlayerCommonRedirectToGateMessageId {
		// FollowRedirect 失败只打日志、没有返回值。记下"124 的处理已经返回",主流程据此区分"还在搬"与"没走到重登就失败了"。
		b.mu.Lock()
		b.progress.redirectsHandled++
		b.mu.Unlock()
	}
}

// mark 返回当前留底条数与进度快照,作为"从这之后"的扫描起点。必须在发请求之前调用。
func (b *syncSmokeBot) mark() (int, travelSmokeProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.records), b.progress
}

// snapshot 返回 since 之后的留底副本与当前进度(同一把锁下读出)。since 为负时从头返回。
func (b *syncSmokeBot) snapshot(since int) ([]travelSmokeRecord, travelSmokeProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	since = max(0, min(since, len(b.records)))
	return append([]travelSmokeRecord(nil), b.records[since:]...), b.progress
}

// gateAddr 返回当前连着的 gate("ip:port";重连或重定向后是新的 gate)。
func (b *syncSmokeBot) gateAddr() string {
	ip, port := b.gc.GateAddr()
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

// pace 让出与上一个请求(任意消息号)的最小间隔 syncSmokeRequestSpacing。调用方在发出后更新 lastRequestAt。
func (b *syncSmokeBot) pace() {
	if wait := syncSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
}

// send 按节奏发一个不关联回包的请求(ClientRequest.id = 0)。只用于单向的 GetSyncDigest(253):
// 应答类型是 Empty,scene TCP 路径上不回包,用 Call 必然超时;请求-响应调用走 friendRpc / travelSmokeStartTravel。
func (b *syncSmokeBot) send(messageId uint32, request proto.Message) error {
	b.pace()
	if err := b.gc.SendRequest(messageId, request); err != nil {
		return fmt.Errorf("send message_id=%d account=%s: %w", messageId, b.account, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()
	return nil
}

// sendDigestRequest 按节奏发一次 GetSyncDigest(253),返回本次请求的留底扫描起点(发之前记下)。
func (b *syncSmokeBot) sendDigestRequest() (int, error) {
	if wait := syncSmokeDigestSpacing - time.Since(b.lastDigestAt); wait > 0 {
		time.Sleep(wait)
	}
	since, _ := b.mark()
	if err := b.send(game.SceneSceneClientPlayerGetSyncDigestMessageId, &scene.GetSyncDigestRequest{}); err != nil {
		return since, err
	}
	b.lastDigestAt = b.lastRequestAt
	return since, nil
}

// waitDigest 从 since 起等第一条 NotifySyncDigest(254)。gate 对 253 的信封拒绝、被踢、超时一律 error。
func (b *syncSmokeBot) waitDigest(since int) (*scene.SyncDigestS2C, error) {
	deadline := time.Now().Add(syncSmokeRpcTimeout)
	for {
		records, _ := b.snapshot(since)
		for _, r := range records {
			switch r.messageId {
			case game.SceneSceneClientPlayerGetSyncDigestMessageId:
				if r.envelopeTip != 0 {
					// 不提"gate 不认识这个消息号":未知消息号 gate 不回信封,只会表现为下面的超时。
					return nil, fmt.Errorf("GetSyncDigest was rejected by the gate envelope tip=%d%s: the request never reached the scene "+
						"(the gate session is not bound to a scene node yet (Scene not ready) or the scene RpcClient is not attached; "+
						"or the client was rate limited)",
						r.envelopeTip, syncSmokeTipHint(game.SceneSceneClientPlayerGetSyncDigestMessageId, r.envelopeTip))
				}
			case game.SceneSceneClientPlayerNotifySyncDigestMessageId:
				push := &scene.SyncDigestS2C{}
				if err := proto.Unmarshal(r.body, push); err != nil {
					return nil, fmt.Errorf("decode NotifySyncDigest: %w", err)
				}
				return push, nil
			case game.SceneClientPlayerCommonKickPlayerMessageId:
				return nil, errors.New("received KickPlayer while waiting for NotifySyncDigest (account taken over elsewhere?)")
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("no NotifySyncDigest within %s after GetSyncDigest. Check in order: the scene runs a binary with PlayerLandingSystem "+
				"(an old scene does not know this method and drops it); the gate runs a binary that knows message_id %d; the scene drops requests "+
				"while the entity is still loading asynchronously (this smoke only sends after NotifyEnterScene, except the S6 probe); "+
				"the scene callback also drops the reply when the entity was destroyed or replaced",
				syncSmokeRpcTimeout, game.SceneSceneClientPlayerGetSyncDigestMessageId)
		}
		time.Sleep(syncSmokePollInterval)
	}
}

// fetchDigest 发一次 GetSyncDigest 并等回推送,解析并要求 status == OK。
func (b *syncSmokeBot) fetchDigest() (syncSmokeDigest, error) {
	since, err := b.sendDigestRequest()
	if err != nil {
		return syncSmokeDigest{}, err
	}
	push, err := b.waitDigest(since)
	if err != nil {
		return syncSmokeDigest{}, err
	}
	digest, err := syncSmokeParseDigest(push)
	if err != nil {
		return digest, fmt.Errorf("NotifySyncDigest violates the contract: %w", err)
	}
	if digest.status != scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK {
		return digest, fmt.Errorf("NotifySyncDigest status=%s, expected OK: the scene could not read the digest (zone Redis not connected, HGETALL not sent, "+
			"or a malformed reply such as a non-decimal field value). Check the scene log [PlayerLanding]; inspect manually: redis-cli HGETALL player:%d:sync_digest",
			digest.status, b.homePlayer)
	}
	return digest, nil
}

// friendRpc 按节奏发一个好友请求并只等它自己的回包(smokeCaller.call,按请求号认领),
// 返回业务 tip(响应体 error_message.id,0 = 受理)。
// 信封拒绝、gate 推 kServiceUnavailable、超时一律 error:它们说明请求没到 friend 业务逻辑,不能当成 friend 给的码去断言。
// 只由主流程调用(CallSync 禁止在 RecvLoop goroutine 上调用)。
func (b *syncSmokeBot) friendRpc(messageId uint32, request proto.Message, response friendResponse) (uint32, error) {
	b.pace()
	b.lastRequestAt = time.Now()
	envelopeTip, err := b.caller.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("message_id=%d account=%s was rejected by the gate / router envelope tip=%d%s: the request never reached friend "+
			"(is the gate started with GATE_CLIENT_RPC_ROUTER=1, and are client_rpc_router and friend running?)",
			messageId, b.account, envelopeTip, syncSmokeTipHint(messageId, envelopeTip))
	}
	return response.GetErrorMessage().GetId(), nil
}

// syncSmokePreclean 把 A、B 之间的好友关系恢复成「不是好友、B 没拉黑 A、A 的收件箱里没有 B 的待处理申请」,
// 返回第一次拉取时见到的、B 发来且仍为 PENDING 的 request_id(S3 据此识别 friend 的同代次重放)。
// 业务结果码刻意**不断言**(上一轮可能停在任何一步,每条都幂等);但传输层失败(信封拒绝、gate 推 1003、超时)、
// GetPendingRequests 读失败(含业务 tip≠0:读不到就无从确认残留)与拒完后复查仍有残留一律返回 error
// (判定见 syncSmokePrecleanVerdict),由调用方判 S2-preclean 失败 —— 吞掉它们的话,残留的 pending 会让 S3 的
// AddFriend 被受理成一次不 bump 的重放,失败被错归到 S3-only-friend-request,而且每轮复现、不能自愈。
// 会给 A bump 的操作全部在 S2 基线之前做完。A 解除拉黑 B 不在这里:它要断言受理(S2 播种),由调用方单独发。
func syncSmokePreclean(subject, peer *syncSmokeBot) ([]uint64, error) {
	type op struct {
		name      string
		bot       *syncSmokeBot
		messageId uint32
		request   proto.Message
		response  friendResponse
	}
	for _, o := range []op{
		{"A RemoveFriend(B)", subject, game.ClientPlayerFriendRemoveFriendMessageId,
			&friendpb.RemoveFriendRequest{TargetPlayerId: peer.homePlayer}, &friendpb.RemoveFriendResponse{}},
		{"B Unblock(A)", peer, game.ClientPlayerFriendUnblockMessageId,
			&friendpb.UnblockRequest{TargetPlayerId: subject.homePlayer}, &friendpb.UnblockResponse{}},
	} {
		tip, err := o.bot.friendRpc(o.messageId, o.request, o.response)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.name, err)
		}
		zap.L().Debug("[sync-smoke] preclean",
			zap.String("op", o.name), zap.String("account", o.bot.account), zap.Uint32("message_id", o.messageId), zap.Uint32("tip", tip))
	}
	seen, rejected, residual, err := subject.rejectPendingFrom(peer.homePlayer)
	if verdict := syncSmokePrecleanVerdict(seen, residual, err); verdict != nil {
		return seen, verdict
	}
	zap.L().Debug("[sync-smoke] preclean rejected pending requests",
		zap.Uint64s("seen_request_ids", seen), zap.Int("rejected", rejected))
	return seen, nil
}

// rejectPendingFrom 拉本人的待处理申请,逐条拒掉 from 发来、仍为 PENDING 的那些,拒完再拉一次复查。
// 返回第一次拉取所见的 request_id、RejectFriend 受理(tip=0)的条数、复查时仍为 PENDING 的 request_id。
// GetPendingRequests 失败(传输层失败,或业务 tip ≠ 0 —— 读不到就无从判断有没有残留)与 RejectFriend 的传输层失败返回 error;
// RejectFriend 的业务 tip 只打 WARN(那条申请可能刚被别处处理掉),是不是真有残留交给复查判定。
// 怎么处置 error 与残留由调用方决定:S2 预清理判失败,S7 收尾只打日志。
func (b *syncSmokeBot) rejectPendingFrom(from uint64) (seenIDs []uint64, rejected int, residualIDs []uint64, err error) {
	seenIDs, err = b.pendingRequestIDsFrom(from)
	if err != nil {
		return nil, 0, nil, err
	}
	for _, requestID := range seenIDs {
		tip, rpcErr := b.friendRpc(game.ClientPlayerFriendRejectFriendMessageId,
			&friendpb.RejectFriendRequest{RequestId: requestID}, &friendpb.RejectFriendResponse{})
		if rpcErr != nil {
			return seenIDs, rejected, nil, fmt.Errorf("RejectFriend request_id=%d: %w", requestID, rpcErr)
		}
		if tip != 0 {
			zap.L().Warn("[sync-smoke] RejectFriend was not accepted",
				zap.String("account", b.account), zap.Uint64("request_id", requestID), zap.Uint32("tip", tip))
			continue
		}
		rejected++
	}
	if len(seenIDs) == 0 {
		// 第一次拉取就没有 from 的 pending:那一次就是复查,不再多花一个 1.1s 的请求间隔。
		return seenIDs, 0, nil, nil
	}
	residualIDs, err = b.pendingRequestIDsFrom(from)
	if err != nil {
		return seenIDs, rejected, nil, fmt.Errorf("re-check after rejecting: %w", err)
	}
	return seenIDs, rejected, residualIDs, nil
}

// pendingRequestIDsFrom 拉本人的待处理申请(friend 的 GetPendingRequests 直接回源 MySQL,是权威读),
// 返回 from 发来、仍为 PENDING 的 request_id。传输层失败与业务 tip ≠ 0 都返回 error。
func (b *syncSmokeBot) pendingRequestIDsFrom(from uint64) ([]uint64, error) {
	pending := &friendpb.GetPendingRequestsResponse{}
	tip, err := b.friendRpc(game.ClientPlayerFriendGetPendingRequestsMessageId, &friendpb.GetPendingRequestsRequest{}, pending)
	if err != nil {
		return nil, fmt.Errorf("GetPendingRequests: %w", err)
	}
	if tip != 0 {
		return nil, fmt.Errorf("GetPendingRequests was rejected with tip=%d%s; cannot tell whether a pending request is left",
			tip, syncSmokeTipHint(game.ClientPlayerFriendGetPendingRequestsMessageId, tip))
	}
	return syncSmokePendingIDsFrom(pending.GetRequests(), from), nil
}

// travelTo 发起一次跨 zone 传送并等到落地:msg 124 → 严格重登成功 → 排在 124 之后的 NotifyEnterScene;
// 再做最小落地断言(verifyHop)。传送链路的完整断言归 travel-smoke。
// TravelToZone 经 travelSmokeStartTravel 用 gc.Call 异步发出,认领窗口与整跳预算相同;回包可有可无,
// 每圈连同留底交给 travelSmokeScanHop(回包带拒绝码立即失败;124 之后以 ErrRPCDisconnected 收场属正常)。
// 轮询里不能调 gc.RedirectHops():它与 FollowRedirect 抢同一把 redirectMu,搬迁进行中会一直阻塞到结束。
func (b *syncSmokeBot) travelTo(targetZone, sceneConfigId uint32) error {
	prevGate := b.gateAddr()
	b.pace()
	since, before := b.mark()
	call, err := travelSmokeStartTravel(b.gc, b.stats, targetZone, sceneConfigId, syncSmokeRedirectTimeout)
	if err != nil {
		return fmt.Errorf("TravelToZone{zone=%d}: send message_id=%d account=%s: %w", targetZone, travelToZoneMessageId, b.account, err)
	}
	b.lastRequestAt = time.Now()

	deadline := time.Now().Add(syncSmokeRedirectTimeout)
	for {
		records, progress := b.snapshot(since)
		scan, err := travelSmokeScanHop(call.snapshot(), records)
		if err != nil {
			return fmt.Errorf("TravelToZone{zone=%d}: %w", targetZone, err)
		}
		reloginStarted := progress.reloginStarted > before.reloginStarted
		reloginDone := progress.reloginFinished > before.reloginFinished
		if scan.hop != nil {
			if reloginDone && progress.reloginErr != nil {
				return fmt.Errorf("TravelToZone{zone=%d}: followed msg 124 to %s, but the strict relogin failed (old connection closed, session unusable): %w",
					targetZone, scan.hop.targetAddr, progress.reloginErr)
			}
			if progress.redirectsHandled > before.redirectsHandled && !reloginStarted {
				return fmt.Errorf("TravelToZone{zone=%d}: handling of msg 124 (target %s) returned without reaching relogin; see the log line "+
					"\"follow gate redirect failed\" above (bad address / expired ticket / target gate unreachable / signature rejected / loop breaker)",
					targetZone, scan.hop.targetAddr)
			}
			if scan.arrived && reloginDone {
				return b.verifyHop(scan.hop, targetZone, prevGate)
			}
		}
		if !time.Now().Before(deadline) {
			// 回包与 124 都没到、窗口内却收到过没回显请求号的回包时,优先报它(理由见 travelSmokeUncorrelatedCause)。
			uncorrelated := progress.uncorrelatedTravel.since(before.uncorrelatedTravel)
			hint := "Diagnose the travel chain itself with travel-smoke (etc/travel_smoke.yaml)"
			if cause := travelSmokeUncorrelatedCause(scan, uncorrelated); cause != "" {
				hint = cause
			}
			return fmt.Errorf("TravelToZone{zone=%d} did not land within %s: reply_seen=%t uncorrelated_replies=%d redirect_seen=%t relogin_started=%t "+
				"relogin_done=%t arrived=%t late_tips=%v. %s",
				targetZone, syncSmokeRedirectTimeout, scan.replySeen, uncorrelated.count, scan.hop != nil, reloginStarted,
				reloginDone, scan.arrived, scan.lateTips, hint)
		}
		time.Sleep(syncSmokePollInterval)
	}
}

// verifyHop 是一跳落地的最小断言:还是同一个角色、票据指向目标 zone、确实换到了 124 指向的 gate。
// 读 gc.PlayerId 是安全的:travelTo 是在锁下看到 124 之后的 NotifyEnterScene 才调用本函数的,
// 那条留底由 RecvLoop goroutine 在重登(写 PlayerId)返回之后才追加。
func (b *syncSmokeBot) verifyHop(hop *travelSmokeHop, wantZone uint32, prevGate string) error {
	if b.gc.PlayerId != b.homePlayer {
		return fmt.Errorf("player_id changed %d -> %d (relogin entered another character)", b.homePlayer, b.gc.PlayerId)
	}
	if hop.tokenZone != wantZone {
		return fmt.Errorf("ticket zone=%d, expected %d (target %s): sent to another zone, or msg 124 carried no ticket", hop.tokenZone, wantZone, hop.targetAddr)
	}
	gate := b.gateAddr()
	if gate == prevGate {
		return fmt.Errorf("gate address unchanged (%s): did not actually move to another zone's gate", gate)
	}
	if gate != hop.targetAddr {
		return fmt.Errorf("connected to %s, but msg 124 pointed to %s (redirected again after landing?)", gate, hop.targetAddr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 纯函数(sync_smoke_scenario_test.go 覆盖)
// ---------------------------------------------------------------------------

// syncSmokeRegisteredDomains 返回全部已知域(生成枚举里除 UNSPECIFIED 以外的值),按域号升序。
// 客户端只对注册了消费者的域算待拉取;机器人没有真消费者,按全部已知域算(即客户端注册集合的上界)。
func syncSmokeRegisteredDomains() []scene.SyncDomain {
	domains := make([]scene.SyncDomain, 0, len(scene.SyncDomain_name))
	for value := range scene.SyncDomain_name {
		if domain := scene.SyncDomain(value); domain != scene.SyncDomain_SYNC_DOMAIN_UNSPECIFIED {
			domains = append(domains, domain)
		}
	}
	slices.Sort(domains)
	return domains
}

// syncSmokeParseDigest 把 NotifySyncDigest 解成 syncSmokeDigest,并校验契约形状(设计 §3.1 / §4.1):
// entries 的域号必须为正、同一个域至多一条(hash 字段名唯一);有域计数就必有 g(bump 脚本建 hash 时必写 g)。
// status 不是 OK 时 generation / entries 没有意义(客户端按全量拉处理),不再校验形状,由调用方按 status 判定。
// 不认识的正域号原样保留:新服务端先上、机器人的生成枚举还没跟上时会出现,它们不参与待拉取计算(不在注册集合里)。
func syncSmokeParseDigest(push *scene.SyncDigestS2C) (syncSmokeDigest, error) {
	digest := syncSmokeDigest{
		status:     push.GetStatus(),
		generation: push.GetGeneration(),
		versions:   make(map[scene.SyncDomain]uint64, len(push.GetEntries())),
	}
	if digest.status != scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK {
		return digest, nil
	}
	for i, entry := range push.GetEntries() {
		domain := entry.GetDomain()
		if domain <= scene.SyncDomain_SYNC_DOMAIN_UNSPECIFIED {
			return digest, fmt.Errorf("entry #%d has domain=%d; the scene must only send fields that parse as known positive domain numbers", i+1, int32(domain))
		}
		if _, duplicated := digest.versions[domain]; duplicated {
			return digest, fmt.Errorf("domain %s appears more than once in entries (hash field names are unique, at most one entry per domain)",
				syncSmokeDomainName(domain))
		}
		digest.versions[domain] = entry.GetVersion()
	}
	if digest.generation == 0 && len(digest.versions) > 0 {
		return digest, fmt.Errorf("g=0 but %d entries present: the bump script always writes g when it creates the hash", len(digest.versions))
	}
	return digest, nil
}

// syncSmokeDiff 返回两份摘要之间计数不同的域,按域号升序。缺席的域按 0 比较(与显式的 0 等价)。
func syncSmokeDiff(before, after syncSmokeDigest) []syncSmokeDomainChange {
	domains := make([]scene.SyncDomain, 0, len(before.versions)+len(after.versions))
	for domain := range before.versions {
		domains = append(domains, domain)
	}
	for domain := range after.versions {
		if _, seen := before.versions[domain]; !seen {
			domains = append(domains, domain)
		}
	}
	slices.Sort(domains)
	var changes []syncSmokeDomainChange
	for _, domain := range domains {
		if was, now := before.versions[domain], after.versions[domain]; was != now {
			changes = append(changes, syncSmokeDomainChange{domain: domain, before: was, after: now})
		}
	}
	return changes
}

// syncSmokeCheckGenerationStable 断言 before → after 之间摘要 hash 没有被重建(设计 §3.2:g 只在 hash 不存在时写入)。
// before 的 g 为 0(摘要还不存在)时不约束 after:之后第一次 bump 建 hash、写入新的 g 是合法的。
func syncSmokeCheckGenerationStable(before, after syncSmokeDigest) error {
	if before.generation == 0 || before.generation == after.generation {
		return nil
	}
	if after.generation == 0 {
		return fmt.Errorf("the digest disappeared: g went %d -> 0 (hash deleted / expired / FLUSHALL, or the scene reads a different Redis than the writers)",
			before.generation)
	}
	return fmt.Errorf("g went %d -> %d: the digest hash was rebuilt (deleted / expired / FLUSHALL / rolled back), or the bump script rewrote g on an existing hash",
		before.generation, after.generation)
}

// syncSmokeCheckUnchanged 断言两份摘要完全一致:g 相同、每个域计数相同(缺席按 0)。
// 用于期间没有任何业务操作的两次落地(短重连、跨 zone、下线重登)。
func syncSmokeCheckUnchanged(before, after syncSmokeDigest) error {
	if err := syncSmokeCheckGenerationStable(before, after); err != nil {
		return err
	}
	changes := syncSmokeDiff(before, after)
	if before.generation != after.generation {
		// 只剩 before 为 0、after 非 0 这一种:期间有业务给这名玩家建了摘要。
		return fmt.Errorf("the digest was created in between (g 0 -> %d) with %s: some service bumped this player",
			after.generation, syncSmokeDescribeChanges(changes))
	}
	if len(changes) > 0 {
		return fmt.Errorf("domain counts changed %s: some service bumped this player in between (is the smoke account shared with another smoke or a client?)",
			syncSmokeDescribeChanges(changes))
	}
	return nil
}

// syncSmokeCheckSingleBump 断言 before → after 之间恰好发生了一次对 domain 的 bump:g 没被重建,
// 只有 domain 变了且恰好 +1。完全没有变化时返回 errSyncSmokeNotBumpedYet(调用方可在预算内重取)。
func syncSmokeCheckSingleBump(before, after syncSmokeDigest, domain scene.SyncDomain) error {
	if err := syncSmokeCheckGenerationStable(before, after); err != nil {
		return err
	}
	changes := syncSmokeDiff(before, after)
	if len(changes) == 0 {
		return errSyncSmokeNotBumpedYet
	}
	var target *syncSmokeDomainChange
	var others []syncSmokeDomainChange
	for i := range changes {
		if changes[i].domain == domain {
			target = &changes[i]
		} else {
			others = append(others, changes[i])
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("domains other than %s changed %s: the operation should only bump %s for this player",
			syncSmokeDomainName(domain), syncSmokeDescribeChanges(others), syncSmokeDomainName(domain))
	}
	// 走到这里 changes 非空且全是 domain,target 必然非空。
	if target.after < target.before {
		return fmt.Errorf("%s went down %d -> %d: the digest was rolled back while exactly one bump was expected",
			syncSmokeDomainName(domain), target.before, target.after)
	}
	if delta := target.after - target.before; delta != 1 {
		return fmt.Errorf("%s went %d -> %d (+%d), expected exactly +1: one accepted operation bumps the receiver once",
			syncSmokeDomainName(domain), target.before, target.after, delta)
	}
	return nil
}

// syncSmokeDirtyDomains 按客户端的落地比对规则(设计 §4.3 第 3 步)算待拉取的域,结果按 registered 的顺序:
// 没有本地缓存、摘要不是 OK、g 与缓存不同 → registered 全部;否则逐域比较,计数不等即待拉取(缺席按 0)。
// cached 必须来自一份 OK 的摘要:客户端只在拉取成功后推进缓存版本,而拉取成功后写下的版本对缺席的域就是 0,
// 所以「本地没有记录」与「记录为 0」在这里等价。
func syncSmokeDirtyDomains(cached *syncSmokeDigest, fresh syncSmokeDigest, registered []scene.SyncDomain) []scene.SyncDomain {
	if cached == nil || fresh.status != scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK || cached.generation != fresh.generation {
		return slices.Clone(registered)
	}
	var dirty []scene.SyncDomain
	for _, domain := range registered {
		if cached.versions[domain] != fresh.versions[domain] {
			dirty = append(dirty, domain)
		}
	}
	return dirty
}

// syncSmokeClassifyProbe 按到达顺序扫 S6 探针之后的留底,给出探针的结局。第一个决定性的结局(254 或 253 的信封拒绝)为准;
// kicked 扫完全部留底。
func syncSmokeClassifyProbe(records []travelSmokeRecord) syncSmokeProbe {
	probe := syncSmokeProbe{outcome: syncSmokeProbeDropped}
	decided := false
	enteredScene := false
	for _, r := range records {
		switch r.messageId {
		case game.SceneSceneClientPlayerNotifyEnterSceneMessageId:
			enteredScene = true
		case game.SceneClientPlayerCommonKickPlayerMessageId:
			probe.kicked = true
		case game.SceneSceneClientPlayerGetSyncDigestMessageId:
			if !decided && r.envelopeTip != 0 {
				probe.outcome, probe.envelopeTip, decided = syncSmokeProbeEnvelopeRejected, r.envelopeTip, true
			}
		case game.SceneSceneClientPlayerNotifySyncDigestMessageId:
			if decided {
				continue
			}
			decided = true
			probe.replyBeforeEnterScene = !enteredScene
			push := &scene.SyncDigestS2C{}
			switch {
			case proto.Unmarshal(r.body, push) != nil:
				probe.outcome = syncSmokeProbeUndecodable
			case push.GetStatus() == scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK:
				probe.outcome, probe.digest = syncSmokeProbeRepliedOK, push
			default:
				probe.outcome, probe.digest = syncSmokeProbeRepliedUnavailable, push
			}
		}
	}
	return probe
}

// syncSmokeJudgeProbe 是 S6 探针里与时序无关的硬判定;取决于时序的结局(dropped / replied_unavailable /
// 1003 等其余信封拒绝 / 与 last 一致的 replied_ok)放行,只由 OBSERVE 行记录。last 是探针之前最后一份 OK 摘要。
//   - 被踢:实体就绪前的请求只能被丢弃或应答,不能弄断会话;但也可能来自 login 按 session_id 踢线的竞态;
//   - 254 解不开:契约破坏,与时机无关;
//   - 信封 1008:冒烟自己的 253 节流失效(syncSmokeDigestSpacing 下默认档永不触发);
//   - replied_ok:回的摘要必须过契约校验,且与 last 一致(期间只有下线重登,没有任何业务 bump)。
func syncSmokeJudgeProbe(probe syncSmokeProbe, last syncSmokeDigest) error {
	if probe.kicked {
		return errors.New("received KickPlayer after the pre-ready GetSyncDigest probe: a request before the entity is ready must be dropped or answered, " +
			"never break the session. It may also come from the login race that kicks by session_id only " +
			"(docs/handoff/handoff-landing-recovery-and-fencing-20261016.md §3.3 a); compare the gate / login logs")
	}
	switch probe.outcome {
	case syncSmokeProbeUndecodable:
		return errors.New("the probe's NotifySyncDigest could not be decoded as SyncDigestS2C: the contract is broken regardless of timing " +
			"(scene and robot proto out of sync?)")
	case syncSmokeProbeEnvelopeRejected:
		if probe.envelopeTip == tipSyncRateLimited {
			return fmt.Errorf("the gate rejected the probe with envelope tip=%d%s: the smoke's own GetSyncDigest throttle is broken "+
				"(two 253s must be at least syncSmokeDigestSpacing apart, which never trips the default limit)",
				probe.envelopeTip, syncSmokeTipHint(game.SceneSceneClientPlayerGetSyncDigestMessageId, probe.envelopeTip))
		}
	case syncSmokeProbeRepliedOK:
		if probe.digest == nil {
			return errors.New("probe outcome is replied_ok but carries no decoded NotifySyncDigest (syncSmokeClassifyProbe bug)")
		}
		digest, err := syncSmokeParseDigest(probe.digest)
		if err != nil {
			return fmt.Errorf("the probe's NotifySyncDigest violates the contract: %w", err)
		}
		if err := syncSmokeCheckUnchanged(last, digest); err != nil {
			return fmt.Errorf("the probe was answered with a digest that differs from the one before leave + relogin: %v; before %s, probe %s",
				err, syncSmokeDescribe(last), syncSmokeDescribe(digest))
		}
	}
	return nil
}

// syncSmokePrecleanVerdict 是 S2 预清理收尾的判定,入参是 rejectPendingFrom 的结果:
//   - err 非 nil 即失败:GetPendingRequests 读失败(传输层失败,或业务 tip≠0 —— 读不到就无从确认残留)
//     与 RejectFriend 的传输层失败都在这里;
//   - 拒完复查仍有 B 的 PENDING 申请(residual 非空)即失败:留着它,S3 的 AddFriend 会被受理成一次不 bump 的重放。
//
// seen(第一次拉取所见)只进失败原因,不参与判定:见过的申请都拒掉、复查为空就是清干净了。
func syncSmokePrecleanVerdict(seen, residual []uint64, err error) error {
	if err != nil {
		return fmt.Errorf("A rejecting B's pending requests (seen %v): %w", seen, err)
	}
	if len(residual) > 0 {
		return fmt.Errorf("B's request(s) %v are still PENDING in A's inbox after RejectFriend (first pull saw %v; see the [sync-smoke] RejectFriend WARN lines "+
			"for the tips); left in place, S3's AddFriend would be accepted as a replay of the same request_id without a bump", residual, seen)
	}
	return nil
}

// syncSmokeIsReplayedRequest:S3 受理回的 request_id 是否是预清理第一次拉取时见过的 PENDING 申请。
// 是 = friend 对已有 pending 做了同代次重放(Changed=false)、不 bump,预清理实际没清掉残留。
// seen 由 syncSmokePendingIDsFrom 产出,不含 0;request_id=0 由调用方先行判失败。
func syncSmokeIsReplayedRequest(seen []uint64, id uint64) bool {
	return slices.Contains(seen, id)
}

// syncSmokePendingIDsFrom 从待处理申请列表里挑出 from 发来、仍为 PENDING、request_id 非 0 的 request_id,保持原顺序。
// request_id=0 的旧行按号拒不掉;AddFriend 遇到它会补发新号并 bump(Changed=true),不会造成 S3 的重放,所以不计入。
func syncSmokePendingIDsFrom(requests []*friendpb.FriendRequest, from uint64) []uint64 {
	var ids []uint64
	for _, request := range requests {
		if request.GetFromPlayerId() != from || request.GetRequestId() == 0 ||
			request.GetStatus() != friendpb.FriendRequestStatus_FRIEND_REQUEST_PENDING {
			continue
		}
		ids = append(ids, request.GetRequestId())
	}
	return ids
}

// syncSmokeDomainName 返回去掉 SYNC_DOMAIN_ 前缀的域名;不认识的域号返回数字。
func syncSmokeDomainName(domain scene.SyncDomain) string {
	return strings.TrimPrefix(domain.String(), "SYNC_DOMAIN_")
}

// syncSmokeDomainList 把域列表写成 "[A B]",空列表写 "[]"。
func syncSmokeDomainList(domains []scene.SyncDomain) string {
	names := make([]string, 0, len(domains))
	for _, domain := range domains {
		names = append(names, syncSmokeDomainName(domain))
	}
	return "[" + strings.Join(names, " ") + "]"
}

// syncSmokeDescribe 把一份摘要写成一行(域按域号升序),供日志与失败原因使用。
func syncSmokeDescribe(digest syncSmokeDigest) string {
	if digest.status != scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK {
		return fmt.Sprintf("{status=%s}", digest.status)
	}
	domains := make([]scene.SyncDomain, 0, len(digest.versions))
	for domain := range digest.versions {
		domains = append(domains, domain)
	}
	slices.Sort(domains)
	parts := make([]string, 0, len(domains)+1)
	parts = append(parts, fmt.Sprintf("g=%d", digest.generation))
	for _, domain := range domains {
		parts = append(parts, fmt.Sprintf("%s=%d", syncSmokeDomainName(domain), digest.versions[domain]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// syncSmokeDescribeChanges 把域变化写成 "[A 1->2 B 0->1]"。
func syncSmokeDescribeChanges(changes []syncSmokeDomainChange) string {
	parts := make([]string, 0, len(changes))
	for _, change := range changes {
		parts = append(parts, fmt.Sprintf("%s %d->%d", syncSmokeDomainName(change.domain), change.before, change.after))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// syncSmokeTipHint 给本场景会遇到的几个 tip 配一句说明(拼在 "tip=<数字>" 后面);不认识的返回空串。
// 同一个码在不同链路上原因不同,所以按消息号区分:253 由 gate 直接转 scene(不经路由服),其余是好友消息(经路由服到 friend)。
func syncSmokeTipHint(messageId, tip uint32) string {
	digestRequest := messageId == game.SceneSceneClientPlayerGetSyncDigestMessageId
	switch tip {
	case tipSyncRateLimited:
		if digestRequest {
			return " (kRateLimitExceeded: gate rate limit on message_id 253, see syncSmokeDigestSpacing / syncSmokeRequestSpacing)"
		}
		return " (kRateLimitExceeded: gate rate limit, see syncSmokeRequestSpacing; or the gate's router lane is full (router mode); " +
			"or friend's Friend.RequestQuotaPerMinute, default 10 per minute)"
	case tipSyncServiceUnavailable:
		if digestRequest {
			return " (kServiceUnavailable: the gate session is not bound to a scene node yet (Scene not ready) or the scene RpcClient is not attached; " +
				"see the gate's [TCP Node] WARN)"
		}
		return " (kServiceUnavailable: the router has no friend instance, or a friend-side storage fault)"
	}
	return ""
}
