package main

// friend-smoke 场景:三个机器人经 gate → client_rpc_router 对 go/friend 做端到端冒烟
// (移植设计 docs/design/friend-port-20260918.md;冻结规格 F3 §5)。
//
//	0. A / C 登 zone_a,B 登 zone_b(cross_zone=false 时 B 也登 zone_a);
//	   跨区时断言 A 与 B 的 gate 地址不同;随后做幂等预清理让冒烟可重复跑;
//	1. A AddFriend(B) 受理,10s 内 B 收到 NotifyFriendEvent{REQUEST_RECEIVED, by=A}
//	   —— 这一步验的是**跨区推送三步**:A 在 zone_a 发起 → friend 读 B 的 player:session
//	   → gate-cmd_g<N> → zone_b 的 gate → B 的连接;
//	2. B GetPendingRequests 看到 A 的非零申请代次；A 重发成功并返回同一代次，原时间不变；
//	3. B 按 request_id 接受申请，A 10s 内收到 NotifyFriendEvent{REQUEST_ACCEPTED, by=B};
//	   双方 GetFriendList 互见,且 A 看到的 B 是 is_online=true / last_active_ms≠0
//	   (验跨区读共享库的 player:session:{id});
//	4. C Block(A) 受理、C ListBlocks 含 A;A→C 与 C→A 的 AddFriend **两个方向都**回 FriendBlocked;
//	5. A RecommendFriends(limit=50):条数被服务端钳到 ≤20,且不含自己 / 已是好友的 B / 已拉黑的 C(弱断言);
//	6. D-9 反向断言:A 经 gate 发 NotifyFriendEvent 的消息号 → 必须拿不到业务回包,
//	   期望信封 tip = kServiceUnavailable(friend 的会话拦截器回 PermissionDenied,
//	   路由服把上游 gRPC 错误统一翻成这个信封码,见 go/client_rpc_router 的 forwardlogic);
//	7. 清理:A RemoveFriend(B)、C Unblock(A)。
//
// 结果约定(供外层脚本消费):
//	全过 → 日志一行 `FRIEND_SMOKE_OK player_a=… player_b=… player_c=… gate_a=… gate_b=…`,退出码 0;
//	任一步失败 → `FRIEND_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// ⚠ 消息号常量名(game.ClientPlayerFriend*MessageId)在 proto-gen 之前**不存在**。
// 下面这些名字是按 robot/generated/pb/game/message_id.go 的现有规律
// (`<服务名><方法名>MessageId`,现有例 ClientPlayerJubaozhaiBrowseListingsMessageId /
// MatchServiceNotifyChallengeInviteMessageId)推导出来的,服务名取 F1 改名后的
// ClientPlayerFriend。**待 proto-gen 后逐个核对常量名**,对不上就改这里,别改 proto。
// 同理,go/friend/internal/logic/push.go 里推送用的是同一个常量名,两处必须一致。
//
// ⚠ 编译前置:proto-gen 之后还要在 robot/ 下 `go mod tidy && go mod vendor`
// —— 当前 robot/vendor/proto/ 下**没有** friend 这个包(只有 battle/chat/.../trade),
// 不补 vendor 就编不过。tip 侧同理:下面引用的三个 kFriendBlocked 段枚举要等
// data/tip/Tip.xlsx 加行 + 导表器跑完才存在(F2 规格 §1.2,与 guild 的 4 个新码同一流程)。

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

	"proto/common/base"
	friendpb "proto/friend"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
//
// 用 97xx 而不是 F3 规格候选的 95xx:落地前按规格要求 grep 了两个仓
// (E:\work\xuanming-server-mmo 与本 worktree)的 robot/ tools/ docs/,结果是
//   - 9001-9006 battle-smoke / 跨 zone 匹配 / chat-smoke;9101-9102 属性 / 宠物;
//   - 9201-9203 guild-smoke,9211-9219 帮会二期(guild-phase2/00-contract.md §6 已划段);
//   - 9301-9304 team-smoke;9401-9403 trade-smoke;
//   - **9501 已被帮会二期的资产通道冒烟占用**(guild-phase2/04-asset-channel.md 第 11 部分,
//     且该文建议把整个 95xx 段划给它);9601 是 travel_smoke。
//
// 所以候选 9501/9502/9503 会与帮会二期撞号,整体后移到 97xx(两仓都未使用)。
//
// 归属区要求:A、C 必须**首次在 zone_a 建角**,B 必须**首次在 zone_b 建角**
// (归属区在建角时登记、之后不随登录区变化)。第 1 步的跨区推送要 B 的 player:session
// 落在 zone_b 的 gate 上,才证明"friend 跨区推得到";B 若其实是 zone_a 的角色,
// 这一步会通过,但证明不了任何跨区结论。
const (
	friendSmokeAccountA = "robot_9701" // 发起方:加好友 / 被拉黑 / 收推荐
	friendSmokeAccountB = "robot_9702" // 别区接收方:收推送 / 同意申请
	friendSmokeAccountC = "robot_9703" // 同区拉黑方
)

// 单次好友 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s)与 friend zrpc Timeout(4000ms),
// 这样先看到路由服的信封拒绝而不是本地超时。第 6 步的反向断言也按这个预算判"超时"。
const friendSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const friendSmokeSceneReadyTimeout = 15 * time.Second

// 等一条 S2C 推送的兜底预算(friend_smoke.push_timeout_ms 未配时用它)。推送链路是
// friend → Kafka gate-cmd_g<N> → gate → TCP,比一次同步 RPC 多一跳 Kafka,
// 所以给得比 RPC 预算宽松。**推送是 at-most-once**(go/friend/internal/logic/push.go):
// 正常路径不丢,但它没有重试也没有回执,所以这条断言超时的含义是
// "推送链路断了",不是"偶尔丢一条属正常"。
const friendSmokeDefaultPushTimeout = 10 * time.Second

// 第 5 步请求的 limit:故意大于 friend.yaml 的 RecommendMaxLimit(默认 20),
// 用来验"超上限被截断而不是报错"(proto/friend/friend.proto RecommendFriendsRequest 的约定)。
const friendSmokeRecommendLimit uint32 = 50

// friend.yaml Friend.RecommendMaxLimit。第 5 步断言返回条数不超过它;改了 yaml 这里要同步。
const friendSmokeServerMaxRecommend = 20

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipFriendBlocked            = uint32(tiptable.FriendError_kFriendBlocked)
	tipFriendNoPendingRequest   = uint32(tiptable.FriendError_kFriendNoPendingRequest)
	tipFriendInvalidParameter   = uint32(tiptable.CommonError_kInvalidParameter)
	tipFriendRateLimited        = uint32(tiptable.CommonError_kRateLimitExceeded)
	tipFriendServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
)

var errFriendSmokeTimeout = errors.New("等回包超时")

// friendResponse 是所有好友客户端响应的共同形状:业务拒绝码在响应体 error_message 里。
type friendResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// friendSmokeBot 是一个已登录进场的机器人会话。
type friendSmokeBot struct {
	account string
	zone    uint32
	gate    string
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// 相邻请求的最小间隔(friend_smoke.request_interval_ms)。好友消息号在 proto-gen 之前
	// 还进不了 data/MessageLimiter.xlsx(那张表按**消息号**配档位,而号要生成后才确定),
	// 所以现在只能吃 gate 的默认档 3 次 / 窗口,靠间隔兜住:窗口按秒粒度、
	// `now - oldest > window` 才淘汰,实际是 1~2 秒,1.1s 间隔下缓冲区至多 2 条
	// (完整推导见 chat_smoke_scenario.go)。
	requestSpacing time.Duration

	// pushTimeout 是等一条 S2C 好友事件的预算(friend_smoke.push_timeout_ms)。
	pushTimeout time.Duration

	// caller 发请求并只等它自己的回包(按请求号认领,见 smoke_rpc_call.go)。
	caller *smokeCaller

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu sync.Mutex
	// pushes:收到的 S2C 好友事件,按到达顺序。断言前用 resetPushes 清空。
	pushes []*friendpb.FriendEventS2C

	lastRequestAt time.Time // 仅主流程读写
}

// RunFriendSmoke 是 main.go `mode: friend-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunFriendSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.FriendSmoke

	var bots []*friendSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("FRIEND_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	spacing := time.Duration(sc.RequestIntervalMs) * time.Millisecond
	pushTimeout := time.Duration(sc.PushTimeoutMs) * time.Millisecond
	if pushTimeout <= 0 {
		// config.validate 已拦 <=0;这里兜底,避免 waitPush 变成"立刻超时"。
		pushTimeout = friendSmokeDefaultPushTimeout
	}
	zoneB := sc.ZoneA
	if sc.CrossZone {
		zoneB = sc.ZoneB
	}

	// ---- 步骤 0a:登录(并发) ----
	type loginSpec struct {
		account string
		zone    uint32
	}
	specs := []loginSpec{{friendSmokeAccountA, sc.ZoneA}, {friendSmokeAccountB, zoneB}, {friendSmokeAccountC, sc.ZoneA}}
	loggedIn := make([]*friendSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = spec.zone
			loggedIn[idx], loginErrs[idx] = friendSmokeLogin(&zoneCfg, spec.account, spacing, pushTimeout, stats)
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
	a, b, c := loggedIn[0], loggedIn[1], loggedIn[2]

	if sc.CrossZone {
		// 配置校验已拦,这里兜底:zone 相同的话"跨区推送"这个结论根本不成立。
		if sc.ZoneA == sc.ZoneB {
			fail("zone-placement", "cross_zone=true but zone_a=zone_b=%d", sc.ZoneA)
		}
		// 本地各 zone 的 gate 端口不同;地址相同说明 B 其实进了 zone_a,
		// 第 1 / 3 步的推送就只是同一个 gate 内部的巧合,证明不了跨区可达。
		if a.gate == b.gate {
			fail("zone-placement", "A and B were assigned the same gate %s (zone_a=%d zone_b=%d); this is not a cross-zone run",
				a.gate, sc.ZoneA, sc.ZoneB)
		}
	}
	zap.L().Info("[friend-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Uint64("c", c.gc.PlayerId),
		zap.String("gate_a", a.gate), zap.String("gate_b", b.gate), zap.Bool("cross_zone", sc.CrossZone))

	// ---- 步骤 0b:预清理 ----
	// 全部幂等、**一律不断言结果码**:上一轮可能停在任何一步,这里只负责把三人之间的
	// 关系恢复成"互不相识、互不拉黑"。每一条的常见拒绝码都是良性的:
	//   RemoveFriend 不是好友 → kFriendNoPendingRequest 家族的业务拒绝;
	//   RejectFriend 没有待处理申请 → kFriendNoPendingRequest;
	//   Unblock 本来就没拉黑 → 受理(幂等 DELETE)。
	// 连接层面的错误(超时 / 信封拒绝)也吞掉:那类问题会在第 1 步立刻以更准确的信息暴露。
	//
	// 为什么需要它:friend 的数据全在 MySQL 里、跨轮次存活,不清理的话第 1 步会直接
	// 撞 kFriendAlreadyFriends 或 kFriendRequestAlreadySent。
	friendSmokePreclean(a, b, c)
	zap.L().Info("[friend-smoke] step 0: preconditions reset (idempotent, result codes ignored)")

	// ---- 步骤 1:A 加 B 为好友,B 必须收到推送 ----
	b.resetPushes()
	added := &friendpb.AddFriendResponse{}
	if err := a.expect(game.ClientPlayerFriendAddFriendMessageId,
		&friendpb.AddFriendRequest{TargetPlayerId: b.gc.PlayerId}, added, 0); err != nil {
		fail("add-friend", "%v", err)
	}
	if added.GetRequestId() == 0 {
		fail("add-friend", "申请成功却未返回非零 request_id")
	}
	evt, err := b.waitPush(friendpb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, a.gc.PlayerId)
	if err != nil {
		fail("push-request-received", "%v (this step verifies three cross-zone push stages: friend reads B player:session:%d -> "+
			"gate-cmd_g<N> -> gate in zone %d. Check friend push metrics in its logs and whether this zone gate consumes the command topic; "+
			"KAFKA_COMMAND_TOPIC_PARTITIONS / _GENERATION mismatches with match will silently lose pushes)",
			err, b.gc.PlayerId, b.zone)
	}
	if evt.GetTsMs() == 0 {
		fail("push-request-received", "Push ts_ms=0 (the server did not set the event timestamp)")
	}
	zap.L().Info("[friend-smoke] step 1: request sent and pushed cross-zone",
		zap.Uint64("by", evt.GetByPlayerId()), zap.Int64("ts_ms", evt.GetTsMs()))

	// ---- 步骤 2:B 拉到权威申请列表；重复 pending 保留原代次与时间 ----
	pending := &friendpb.GetPendingRequestsResponse{}
	if err := b.expect(game.ClientPlayerFriendGetPendingRequestsMessageId,
		&friendpb.GetPendingRequestsRequest{}, pending, 0); err != nil {
		fail("pending-list", "%v", err)
	}
	found := false
	var requestTime int64
	for _, req := range pending.GetRequests() {
		if req.GetFromPlayerId() == a.gc.PlayerId {
			if req.GetStatus() != friendpb.FriendRequestStatus_FRIEND_REQUEST_PENDING {
				fail("pending-list", "A(%d) request in B pending list has status=%s, expected PENDING",
					a.gc.PlayerId, req.GetStatus())
			}
			if req.GetRequestId() != added.GetRequestId() {
				fail("pending-list", "待审列表代次=%d，与申请响应=%d 不一致", req.GetRequestId(), added.GetRequestId())
			}
			requestTime = req.GetRequestTimeMs()
			found = true
			break
		}
	}
	if !found {
		fail("pending-list", "B(%d) pending requests contain no A(%d): the push arrived but the authoritative fetch path has no entry; "+
			"the push only triggers a notification badge; this fetch is authoritative", b.gc.PlayerId, a.gc.PlayerId)
	}
	duplicate := &friendpb.AddFriendResponse{}
	if err := a.expect(game.ClientPlayerFriendAddFriendMessageId,
		&friendpb.AddFriendRequest{TargetPlayerId: b.gc.PlayerId}, duplicate, 0); err != nil {
		fail("add-friend-duplicate", "%v", err)
	}
	if duplicate.GetRequestId() != added.GetRequestId() {
		fail("add-friend-duplicate", "重复 pending 更换了申请代次")
	}
	replayed := &friendpb.GetPendingRequestsResponse{}
	if err := b.expect(game.ClientPlayerFriendGetPendingRequestsMessageId,
		&friendpb.GetPendingRequestsRequest{}, replayed, 0); err != nil {
		fail("pending-replay", "%v", err)
	}
	matches := 0
	for _, request := range replayed.GetRequests() {
		if request.GetFromPlayerId() == a.gc.PlayerId {
			matches++
			if request.GetRequestId() != added.GetRequestId() || request.GetRequestTimeMs() != requestTime {
				fail("pending-replay", "重复 pending 改变了申请代次或原始时间")
			}
		}
	}
	if matches != 1 {
		fail("pending-replay", "同一玩家对的待审申请数量=%d，期望1", matches)
	}
	zap.L().Info("[friend-smoke] step 2: authoritative pending list + stable request id",
		zap.Int("pending_count", len(pending.GetRequests())))

	// ---- 步骤 3:B 同意,A 收到推送,双方互见 ----
	a.resetPushes()
	if err := b.expect(game.ClientPlayerFriendAcceptFriendMessageId,
		&friendpb.AcceptFriendRequest{RequestId: added.GetRequestId(), FromPlayerId: a.gc.PlayerId}, &friendpb.AcceptFriendResponse{}, 0); err != nil {
		fail("accept-friend", "%v", err)
	}
	if _, err := a.waitPush(friendpb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED, b.gc.PlayerId); err != nil {
		fail("push-request-accepted", "%v (the direction is opposite to step 1: friend in zone %d pushes to A in zone %d)", err, b.zone, a.zone)
	}

	aList := &friendpb.GetFriendListResponse{}
	if err := a.expect(game.ClientPlayerFriendGetFriendListMessageId,
		&friendpb.GetFriendListRequest{}, aList, 0); err != nil {
		fail("friend-list-a", "%v", err)
	}
	entryB := friendSmokeFindFriend(aList.GetFriends(), b.gc.PlayerId)
	if entryB == nil {
		fail("friend-list-a", "A friend list contains no B(%d): AcceptFriend was accepted but the bidirectional INSERT was not persisted", b.gc.PlayerId)
	}
	// 在线状态与活跃时刻都读共享库的 player:session:{id}(契约 §4)。B 此刻确实在线,
	// 所以 is_online=false 只有两种可能:friend 连错了 Redis(私有库而不是共享库),
	// 或者会话读失败被降级成"全部离线"(F2 的 fail-open)。两种都要人看,不能放过。
	if !entryB.GetIsOnline() {
		fail("friend-online", "A sees B(%d) as offline, but B is logged into zone %d; "+
			"does friend SharedRedis point to the database that stores player:session? Read failures fail open to all offline; check friend error logs",
			b.gc.PlayerId, b.zone)
	}
	if entryB.GetLastActiveMs() == 0 {
		fail("friend-online", "A sees B(%d) with last_active_ms=0: the session has no activity timestamp, or it was not projected into the list", b.gc.PlayerId)
	}
	bList := &friendpb.GetFriendListResponse{}
	if err := b.expect(game.ClientPlayerFriendGetFriendListMessageId,
		&friendpb.GetFriendListRequest{}, bList, 0); err != nil {
		fail("friend-list-b", "%v", err)
	}
	if friendSmokeFindFriend(bList.GetFriends(), a.gc.PlayerId) == nil {
		fail("friend-list-b", "B friend list contains no A(%d): the friendship was persisted in only one direction", a.gc.PlayerId)
	}
	zap.L().Info("[friend-smoke] step 3: accepted, pushed back, both lists agree",
		zap.Bool("b_online_seen_by_a", entryB.GetIsOnline()), zap.Int64("b_last_active_ms", entryB.GetLastActiveMs()))

	// ---- 步骤 4:黑名单双向拦截 ----
	if err := c.expect(game.ClientPlayerFriendBlockMessageId,
		&friendpb.BlockRequest{TargetPlayerId: a.gc.PlayerId}, &friendpb.BlockResponse{}, 0); err != nil {
		fail("block", "%v", err)
	}
	blocks := &friendpb.ListBlocksResponse{}
	if err := c.expect(game.ClientPlayerFriendListBlocksMessageId,
		&friendpb.ListBlocksRequest{}, blocks, 0); err != nil {
		fail("list-blocks", "%v", err)
	}
	if !friendSmokeHasBlock(blocks.GetBlocks(), a.gc.PlayerId) {
		fail("list-blocks", "C blocklist contains no A(%d): Block was accepted but not persisted in friend_block", a.gc.PlayerId)
	}
	// **两个方向都要拦**,而且回的是同一个中性码:回"对方把你拉黑了"等于把别人的黑名单
	// 状态变成一个可探测的接口(挨个发申请就能反查谁拉黑了自己)。
	if err := a.expect(game.ClientPlayerFriendAddFriendMessageId,
		&friendpb.AddFriendRequest{TargetPlayerId: c.gc.PlayerId}, &friendpb.AddFriendResponse{},
		tipFriendBlocked); err != nil {
		fail("blocked-forward", "The blocked party A->C request was not rejected: %v", err)
	}
	if err := c.expect(game.ClientPlayerFriendAddFriendMessageId,
		&friendpb.AddFriendRequest{TargetPlayerId: a.gc.PlayerId}, &friendpb.AddFriendResponse{},
		tipFriendBlocked); err != nil {
		fail("blocked-reverse", "The blocking party C->A request was not rejected (blocking only one direction allows bypassing the blocklist): %v", err)
	}
	zap.L().Info("[friend-smoke] step 4: block list enforced in both directions")

	// ---- 步骤 5:推荐(弱断言) ----
	// 推荐的**内容**取决于全库数据,断言不了。本步实际能证明的只有两条:
	//   - **不推自己**;
	//   - **不推已是好友的 B**(A↔B 的 friend 行在第 3 步已经写出来了,去掉 SQL 的排除条件 B 就会被返回)。
	// 另外两条断言在本冒烟的三账号数据集下够不着,**它们绿了不代表对应逻辑是对的**:
	//   - **不含已拉黑的 C**:两条召回路径的候选来源都是 friend 表本身(RecommendByMutual 的
	//     `FROM friend f1 JOIN friend f2`、recommendAnchor 的 `FROM friend WHERE player_id >= ?`),
	//     而 C 在本冒烟里自始至终没有任何好友边、在 friend 表里零行 —— 无论 friend_block 的双向排除
	//     是否失效,C 都进不了候选集。这条断言在本数据集下结构性地不可能失败。
	//   - **条数 ≤20**:三个账号在干净库上凑不出 20 个候选,截断路径根本不会被触发。
	//     注意 recommendAnchor 扫的是整张 friend 表,本机库里若留着别的账号的好友边,候选数是可能上到
	//     20 的 —— 所以这条只是"在干净库上不可达",不是"永远不可能"。
	// 结论:friend_block 的双向排除与 RecommendMaxLimit 的截断只能靠 go/friend 的单测或人工造数据覆盖,
	// 不要把本步的绿当成它们的端到端证据。
	rec := &friendpb.RecommendFriendsResponse{}
	if err := a.expect(game.ClientPlayerFriendRecommendFriendsMessageId,
		&friendpb.RecommendFriendsRequest{Limit: friendSmokeRecommendLimit}, rec, 0); err != nil {
		fail("recommend", "%v", err)
	}
	if n := len(rec.GetCandidates()); n > friendSmokeServerMaxRecommend {
		fail("recommend", "A limit=%d request returned %d entries, exceeding the server limit %d (RecommendMaxLimit did not clamp it)",
			friendSmokeRecommendLimit, n, friendSmokeServerMaxRecommend)
	}
	for _, cand := range rec.GetCandidates() {
		switch cand.GetCandidatePlayerId() {
		case a.gc.PlayerId:
			fail("recommend", "Recommendations include A itself (%d)", a.gc.PlayerId)
		case b.gc.PlayerId:
			fail("recommend", "Recommendations include existing friend B(%d)", b.gc.PlayerId)
		case c.gc.PlayerId:
			fail("recommend", "Recommendations include C(%d), which has a blocking relationship with A", c.gc.PlayerId)
		}
	}
	zap.L().Info("[friend-smoke] step 5: recommendations clamped and filtered",
		zap.Int("candidates", len(rec.GetCandidates())))

	// ---- 步骤 6:D-9 反向断言 —— 客户端不能调 S2C 方法 ----
	// NotifyFriendEvent 与 10 个 C2S 方法同处 ClientPlayerFriend 这一个服务里
	// (客户端协议服务只看 OptionIsClientProtocolService,推送方法标 CLIENT_RPC_PUSH),
	// 所以 gate 的客户端白名单**会**收这个消息号、请求**会**进到 friend ——
	// 与 trade 第 2 步那种"gate 直接丢弃、表现为超时"不是同一回事,这里期望的是信封拒绝。
	// 挡它的是 friend 的会话拦截器(方法级白名单),不是"服务端恰好没实现"。
	envelopeTip, err := a.call(game.ClientPlayerFriendNotifyFriendEventMessageId,
		&friendpb.FriendEventS2C{
			Reason:     friendpb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED,
			ByPlayerId: b.gc.PlayerId,
			TsMs:       time.Now().UnixMilli(),
		}, &base.Empty{})
	switch {
	case errors.Is(err, errFriendSmokeTimeout):
		fail("notify-via-gate", "Expected envelope tip=%d, but timed out: gate dropped the NotifyFriendEvent message ID (not included in the allowlist), "+
			"or the request never reached the routing service. This assertion checks rejection by the friend session layer and proves nothing when the transport path is broken",
			tipFriendServiceUnavailable)
	case err != nil:
		fail("notify-via-gate", "%v", err)
	case envelopeTip == tipFriendServiceUnavailable:
		// 期望:friend 回 PermissionDenied,路由服统一翻成 kServiceUnavailable 的信封拒绝。
		// 同时去 friend 日志里核对一行"拒绝客户端调用非客户端方法 /friendpb.ClientPlayerFriend/NotifyFriendEvent"
		// —— 信封码是被各种上游错误共用的,只有那行日志能证明拒绝来自会话层的白名单。
	case envelopeTip != 0:
		fail("notify-via-gate", "Envelope tip=%d, expected %d (kServiceUnavailable)", envelopeTip, tipFriendServiceUnavailable)
	default:
		fail("notify-via-gate", "A client calling NotifyFriendEvent through gate received a business response: "+
			"the S2C push method is exposed to clients, allowing anyone to forge a friend event (D-9)")
	}
	zap.L().Info("[friend-smoke] step 6: NotifyFriendEvent refused for client callers",
		zap.Uint32("envelope_tip", envelopeTip),
		zap.String("expect_friend_log", "Reject client calls to the non-client method .../NotifyFriendEvent"))

	// ---- 步骤 7:清理 ----
	// 这里**断言**结果码(与步骤 0b 的预清理不同):清理失败说明 RemoveFriend / Unblock
	// 本身坏了,而不是"上一轮留下的状态不确定"。
	if err := a.expect(game.ClientPlayerFriendRemoveFriendMessageId,
		&friendpb.RemoveFriendRequest{TargetPlayerId: b.gc.PlayerId}, &friendpb.RemoveFriendResponse{}, 0); err != nil {
		fail("cleanup-remove", "%v", err)
	}
	if err := c.expect(game.ClientPlayerFriendUnblockMessageId,
		&friendpb.UnblockRequest{TargetPlayerId: a.gc.PlayerId}, &friendpb.UnblockResponse{}, 0); err != nil {
		fail("cleanup-unblock", "%v", err)
	}
	zap.L().Info("[friend-smoke] step 7: cleaned up")

	zap.L().Info(fmt.Sprintf("FRIEND_SMOKE_OK player_a=%d player_b=%d player_c=%d gate_a=%s gate_b=%s",
		a.gc.PlayerId, b.gc.PlayerId, c.gc.PlayerId, a.gate, b.gate),
		zap.Bool("cross_zone", sc.CrossZone), zap.Uint32("zone_a", sc.ZoneA), zap.Uint32("zone_b", zoneB))
	cleanup()
	_ = zap.L().Sync()
}

// friendSmokePreclean 把三人之间的关系恢复成"互不相识、互不拉黑"。
//
// 刻意**不返回 error、不断言结果码**:它要在"上一轮停在任意一步"的前提下都能跑完。
// 每一条都是幂等操作,做没做成都不影响后续断言的前提 —— 真正的前提由第 1 步自己验。
func friendSmokePreclean(a, b, c *friendSmokeBot) {
	type op struct {
		bot       *friendSmokeBot
		messageID uint32
		request   proto.Message
		response  friendResponse
	}
	// 先断好友并解除两方向拉黑，再按权威列表的当前代次清理残留申请。
	// RemoveFriend / RejectFriend 不受拉黑影响；第1步 AddFriend 前必须已经解黑。
	for _, o := range []op{
		{a, game.ClientPlayerFriendRemoveFriendMessageId,
			&friendpb.RemoveFriendRequest{TargetPlayerId: b.gc.PlayerId}, &friendpb.RemoveFriendResponse{}},
		{c, game.ClientPlayerFriendUnblockMessageId,
			&friendpb.UnblockRequest{TargetPlayerId: a.gc.PlayerId}, &friendpb.UnblockResponse{}},
		{a, game.ClientPlayerFriendUnblockMessageId,
			&friendpb.UnblockRequest{TargetPlayerId: c.gc.PlayerId}, &friendpb.UnblockResponse{}},
	} {
		tip, err := o.bot.rpc(o.messageID, o.request, o.response)
		zap.L().Debug("[friend-smoke] preclean",
			zap.String("account", o.bot.account), zap.Uint32("message_id", o.messageID),
			zap.Uint32("tip", tip), zap.Error(err))
	}
	// 清理同样先拉权威代次，旧的按玩家对拒绝请求已经不再合法。
	pending := &friendpb.GetPendingRequestsResponse{}
	code, err := b.rpc(game.ClientPlayerFriendGetPendingRequestsMessageId, &friendpb.GetPendingRequestsRequest{}, pending)
	if err != nil || code != 0 {
		zap.L().Debug("[friend-smoke] 预清理拉取待审失败", zap.Uint32("tip", code), zap.Error(err))
		return
	}
	for _, request := range pending.GetRequests() {
		if request.GetFromPlayerId() != a.gc.PlayerId || request.GetRequestId() == 0 {
			continue
		}
		code, err = b.rpc(game.ClientPlayerFriendRejectFriendMessageId,
			&friendpb.RejectFriendRequest{RequestId: request.GetRequestId()}, &friendpb.RejectFriendResponse{})
		zap.L().Debug("[friend-smoke] 按申请代次预清理", zap.Uint32("tip", code), zap.Error(err))
	}
}

// friendSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 tradeSmokeLogin 同形。
func friendSmokeLogin(cfg *config.Config, account string, spacing, pushTimeout time.Duration, stats *metrics.Stats) (*friendSmokeBot, error) {
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

	bot := &friendSmokeBot{
		account:        account,
		zone:           cfg.ZoneID,
		gate:           host + ":" + portStr,
		gc:             gc,
		player:         gameobject.NewPlayer(gc.PlayerId),
		stats:          stats,
		requestSpacing: spacing,
		pushTimeout:    pushTimeout,
		caller:         newSmokeCaller(gc, stats, account, friendSmokeRpcTimeout, errFriendSmokeTimeout),
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), friendSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// onMessage:好友推送留底;好友请求的回包带请求号,由 GameClient 交给 caller,不会到这里;
// gate 的 SendTipToClient 交给 caller 供快速失败;其余交给通用分发。
//
// ⚠ 必须在分发前拦下 ClientPlayerFriend 的**全部 11 个**消息号:生成器给它产出了 robot handler stub,
// 信封拒绝(error_message 非空、body 为空)一旦落进 MessageBodyHandler 会被解成全零响应,看上去像 tip=0 受理。
// 第 6 步对推送号本身发请求:它的回包带请求号、推送 id = 0,按 id 即可分开,不再需要 awaiting 标记。
func (b *friendSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	id := msg.GetMessageId()
	if id == game.ClientPlayerFriendNotifyFriendEventMessageId {
		evt := &friendpb.FriendEventS2C{}
		if err := proto.Unmarshal(msg.GetSerializedMessage(), evt); err == nil {
			b.mu.Lock()
			b.pushes = append(b.pushes, evt)
			b.mu.Unlock()
		}
		return
	}
	if friendSmokeIsFriendMessage(id) {
		smokeDropUncorrelatedReply(b.account, msg)
		return
	}
	if id == game.SceneClientPlayerCommonSendTipToClientMessageId {
		b.caller.onGateTipMessage(msg)
	}
	handler.MessageBodyHandler(client, msg)
}

// resetPushes 清空已收到的推送。在**触发推送的那次请求发出之前**调用:
// 清晚了会把本次要断言的那条一起清掉。
func (b *friendSmokeBot) resetPushes() {
	b.mu.Lock()
	b.pushes = nil
	b.mu.Unlock()
}

// waitPush 等一条 reason + by_player_id 都匹配的好友事件。
//
// 为什么要同时匹配两者:同一轮里可能先后来两种事件(例如并行跑别的冒烟时),
// 只按 reason 匹配会把别人的事件当成自己的。
func (b *friendSmokeBot) waitPush(reason friendpb.FriendEventReason, byPlayerID uint64) (*friendpb.FriendEventS2C, error) {
	deadline := time.Now().Add(b.pushTimeout)
	for {
		b.mu.Lock()
		for _, evt := range b.pushes {
			if evt.GetReason() == reason && evt.GetByPlayerId() == byPlayerID {
				b.mu.Unlock()
				return evt, nil
			}
		}
		seen := len(b.pushes)
		b.mu.Unlock()
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("account=%s 在 %s 内没等到 NotifyFriendEvent{reason=%s, by=%d}(期间收到 %d 条其它好友事件)",
				b.account, b.pushTimeout, reason, byPlayerID, seen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// call 发一个好友请求并只等它自己的回包(smokeCaller.call)。
//
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable 返回 error;超时返回包裹 errFriendSmokeTimeout 的 error。
func (b *friendSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	if wait := b.requestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	b.lastRequestAt = time.Now()
	return b.caller.call(messageId, request, response)
}

// rpc 发请求并返回业务 tip(响应体 error_message.id,0 = 受理)。
// 信封拒绝与超时一律 error:它们说明请求没到 friend 业务逻辑,不能当成 friend 给的码去断言。
func (b *friendSmokeBot) rpc(messageId uint32, request proto.Message, response friendResponse) (uint32, error) {
	envelopeTip, err := b.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("gate/路由服信封拒绝 message_id=%d tip=%d(请求未到达 friend 业务逻辑:tip=%d 多为路由服没有 friend 实例、"+
			"路由表未更新,或 friend 返回了 gRPC 错误(会话拦截器拒绝等);其它常见为 gate 限流,可调大 request_interval_ms)",
			messageId, envelopeTip, tipFriendServiceUnavailable)
	}
	return response.GetErrorMessage().GetId(), nil
}

// expect 发请求并断言业务 tip 等于 want;常见的环境类拒绝码翻译成可读原因。
func (b *friendSmokeBot) expect(messageId uint32, request proto.Message, response friendResponse, want uint32) error {
	got, err := b.rpc(messageId, request, response)
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	switch got {
	case tipFriendInvalidParameter:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(请求校验不通过,或 friend 在逻辑层拿不到会话身份 —— "+
			"D-9 下 \"我是谁\" 只从 gate 注入的会话 metadata 取,检查路由服是否透传 x-session-detail-bin),期望 tip=%d",
			messageId, b.account, got, want)
	case tipFriendRateLimited:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(撞上 Friend.RequestQuotaPerMinute,默认 10 次/分钟。"+
			"一轮冒烟里 A 要发 3~4 次好友申请,同一分钟内连跑多轮会撞 —— 等一分钟再跑,或调大该配置),期望 tip=%d",
			messageId, b.account, got, want)
	case tipFriendServiceUnavailable:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(friend 侧 MySQL / Redis 真故障,看 friend 错误日志;"+
			"常见为 mmorpg_friend 库没建或表没迁移)", messageId, b.account, got)
	case tipFriendNoPendingRequest:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(没有待处理的申请 —— 上一步的申请没落库,"+
			"或被别的会话抢先处理了),期望 tip=%d", messageId, b.account, got, want)
	}
	return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d", messageId, b.account, want, got)
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// friendSmokeIsFriendMessage 列出 ClientPlayerFriend 的全部 11 个消息号。
// 新增 rpc 必须同步加进来 —— 漏一个就是"该号的回包被通用分发解成全零响应"。
func friendSmokeIsFriendMessage(messageId uint32) bool {
	switch messageId {
	case game.ClientPlayerFriendAddFriendMessageId,
		game.ClientPlayerFriendAcceptFriendMessageId,
		game.ClientPlayerFriendRejectFriendMessageId,
		game.ClientPlayerFriendRemoveFriendMessageId,
		game.ClientPlayerFriendGetFriendListMessageId,
		game.ClientPlayerFriendGetPendingRequestsMessageId,
		game.ClientPlayerFriendBlockMessageId,
		game.ClientPlayerFriendUnblockMessageId,
		game.ClientPlayerFriendListBlocksMessageId,
		game.ClientPlayerFriendRecommendFriendsMessageId,
		game.ClientPlayerFriendNotifyFriendEventMessageId:
		return true
	}
	return false
}

func friendSmokeFindFriend(entries []*friendpb.FriendEntry, playerID uint64) *friendpb.FriendEntry {
	for _, e := range entries {
		if e.GetFriendPlayerId() == playerID {
			return e
		}
	}
	return nil
}

func friendSmokeHasBlock(entries []*friendpb.BlockEntry, playerID uint64) bool {
	for _, e := range entries {
		if e.GetBlockedPlayerId() == playerID {
			return true
		}
	}
	return false
}
