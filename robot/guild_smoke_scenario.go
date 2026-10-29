package main

// guild-smoke 场景:五个同区机器人(A / B / D / E / F)加可选的别区 C,经 gate → client_rpc_router
// 对 go/guild 做「按 zone 隔离」+「管理与审批 + 推送」的端到端冒烟
// (docs/design/guild-zone-client-access.md §6、docs/design/guild-phase2/02-management.md §24):
//
//	0. 预备:每个机器人各自撤回遗留的待审申请,再 GetPlayerGuild,还在帮会里就解散(帮主)或退出(成员)—— 同账号可反复跑;
//	1. A 建帮,请求体 zone_id 故意填错 → 受理,且帮会 zone_id == zone_a(zone 由服务端按归属映射决定);
//	2. A 再建一个 → kGuildAlreadyInGuild;
//	3. B(同区)查该帮在本区榜上的名次 → 上榜;
//	4. C(zone_b,cross_zone=true 时):GetGuild → 不存在;查名次(请求体伪造 zone_a)→ 未上榜;
//	   本区榜里没有它;申请入帮 → 不存在;用同一个帮名建帮 → kGuildNameTaken(帮名全局唯一);
//	5. B 未入帮时改公告 → kGuildNoPermission;
//	6. B 申请入帮 → 受理,B 与 A 各自列得到这条申请;A 审批通过 → 响应快照里 B 在册,B 的 GetPlayerGuild 看到自己是成员;
//	   member-names:这份快照里每个成员的 name 与帮会的 leader_name 都非空(名字来自 data_service 名字注册表);
//	7. A 改公告 → B 读到;700 字节公告 → kGuildAnnouncementTooLong(guild 侧拒绝,整包仍 < gate 1KB);
//	8. 身份伪造:B 发 LeaveGuild 但请求体 player_id 填 A → 服务端按会话身份让 B 退出,A 仍是帮主;
//	9. 内部方法:A 发 UpdateGuildScore → 收到 kServiceUnavailable 信封拒绝,随后查榜成功且积分仍是 0;
//	   确切拒绝原因需结合路由服 PermissionDenied 日志与 session 单测确认(信封不保留 gRPC code);
//
// 管理段 M1–M10(设计 docs/design/guild-phase2/02-management.md §24.2),跑在第 9 步之后、解散之前。
// 进入这一段时帮会里只剩帮主 A(B 已在第 8 步退帮),同区的 D / E / F 三人全程在场:
//
//	M1. D、E、F 各申请入帮,F 再申请一次 → 都受理,且 F 的待审列表仍只有 1 条(同帮重复申请是刷新有效期,不新增行);
//	M2. 未入帮的 E 列帮会待审名单 → kGuildNotInGuild(待审名单是管理侧信息);
//	M3. A 审批 D、E 通过 → D 收到 MEMBER_JOINED 推送;
//	M4. A 拒绝 F → F 收到 APPLICATION_REJECTED、待审列表清空,A 再审同一人 → kGuildApplicationNotFound;
//	M5. A 任命 D 为长老(连做两次,第二次幂等) → 快照里 D role=1、officer_count=1、max_officers=2,D 收到 ROLE_CHANGED;
//	M6. D(长老)踢 E → E 收到 MEMBER_KICKED 且已不在帮;D 踢帮主 A / 踢自己 / 任免 A / 再踢已走的 E
//	    → kGuildRankTooLow / kGuildCannotTargetSelf / kGuildRankTooLow / kGuildTargetNotMember;
//	M7. A 转让帮主给 D → D 在册为帮主、A 降为长老,D 收到 LEADER_TRANSFERRED;
//	M8. D 转让回 A → A 在册为帮主、D 降为长老(把帮会恢复成可解散的状态);
//	M9. D 退帮 → 留守的 A 收到 MEMBER_LEFT;
//	M10. F 重新申请(APPLICATION_RECEIVED 有 60s 冷却,可能被抑制,**不**断言),解散之后列表必须空;
//
//	10. A 解散 → 自己不在帮会,帮会从本区榜上消失,并连带删掉 F 在 M10 留下的待审申请。
//	11. 过期的 expected_guild_id(docs/design/client-request-ordering.md §6.4,批次 E2):A 另建临时帮 H、B 申请并获批;
//	    B 发 LeaveGuild{expected_guild_id = 第 1 步的帮(B 第 8 步已退出、第 10 步已解散)} → kGuildNotInGuild,
//	    B 仍是 H 的普通成员且 H 仍是 2 人;对照:B 带 H 的 id 退帮 → 受理且已不在帮会;A 解散 H 收尾。
//
// 结果约定(供外层脚本消费):
//	管理段全过 → `GUILD_MGMT_OK guild_id=… leader=… officer=… kicked=… rejected=…`;
//	全过 → 再加一行 `GUILD_SMOKE_OK guild_id=… zone_a=… player_a=… player_b=… player_c=…`,退出码 0;
//	任一步失败 → `GUILD_SMOKE_FAIL step=… reason=…`,退出码 1(管理段的 step 形如 `mgmt-M6`)。
//
// 经济段(B5c,guild_smoke.economy=true 时):GUILD_SMOKE_OK 之后由 robot_9214 / 9215 另建一个帮会跑
// 捐献 / 升级 / 兑换,通过打印 `GUILD_ECONOMY_SMOKE_OK mode=full|degraded …`,失败的 step 形如 `economy-7-silver`。
// 步骤与两种模式见 guild_economy_smoke.go 文件头。
//
// 活动段(B6a-cli + B6b-cli,guild_smoke.activities=true 时):经济段之后由 robot_9216–9219 另建一个帮会跑
// 元宵灯会 / 中秋团圆(06-activities.md §6.44 的 S1–S8);guild_smoke.trial=true 时接着跑同道历练
// (S9–S15:邀请房间 → 全员同意 → 开战 → 结算发奖 → 拒绝 / 离线 / 名单不合法)。通过打印
// `GUILD_SMOKE_ACTIVITIES_OK guild_id=…`,失败打印 `GUILD_SMOKE_ACTIVITIES_FAIL step=Sx reason=…`(退出码同样是 1)。
// 步骤见 guild_activity_smoke.go 文件头。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	guildpb "proto/guild"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。
// 92xx 与 battle-smoke(9001/9002)、跨 zone 匹配(9003/9004)、chat-smoke(9005/9006)、属性(9101)错开。
// 必须是**首次在各自 zone 建角**的账号:A/B/D/E/F 的归属区要是 zone_a、C 的要是 zone_b,第 1 步才能断言帮会落区。
const (
	guildSmokeAccountA = "robot_9201" // 帮主
	guildSmokeAccountB = "robot_9202" // 同区成员
	guildSmokeAccountC = "robot_9203" // 别区玩家(cross_zone=true 才登录)
	// 管理段(M1–M10)的三个同区账号。号段由 90-consistency Y-09 分配:B2c 用 9211–9213,
	// 9214–9219 留给 B5 / B6,不要在本场景占用 —— 两批同时跑时账号撞车的表现是
	// "另一个人的帮会状态莫名其妙地变了",比编译错误难查得多。
	// 和 A / B 一样必须是**首次在 zone_a 建角**的账号,否则归属区映射对不上。
	guildSmokeAccountD = "robot_9211" // 被任命为长老,M7 短暂当帮主
	guildSmokeAccountE = "robot_9212" // 被长老请离
	guildSmokeAccountF = "robot_9213" // 申请被拒绝
)

// 单次公会 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s),先看到路由服的信封拒绝而不是本地超时。
const guildSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const guildSmokeSceneReadyTimeout = 15 * time.Second

// 相邻请求的最小间隔。公会消息号在 MessageLimiter 表里是读 10 次 / 秒、写 5 次 / 秒
//(申请制与管理段的 8 个新号在 B2c 一并补了行),UpdateGuildScore 不在表里吃默认 3 次 / 窗口;
// 冒烟不追求速度,300ms 远离各档上限。
// 注意 300ms 只挡得住"连发 3 次"(跨度 600ms);同一个消息号要连发 4 次以上时,
// 必须另外插 guildSmokeSameIdCooldown,别指望这个间隔。
const guildSmokeRequestSpacing = 300 * time.Millisecond

// 等一条推送的预算。推送走 guild → Kafka → gate → 客户端,比同步 RPC 多一跳 Kafka,
// 5s 覆盖本机 Kafka 的正常投递抖动。超时一律判失败:推送丢了是缺陷,不是"慢"。
const guildSmokePushTimeout = 5 * time.Second

// 同一个消息号连发多次时的退火间隔,取 > 限流窗口(1 秒),让前面几次彻底滑出窗口。
// 管理段里 D 连发 4 次 KickGuildMember、A 连发 4 次 ReviewGuildApplication:按 300ms 间隔算,
// 四次会落在同一秒内,离写类消息号 5 次 / 秒的上限只剩一次余量,而中间夹的"等推送"耗时又不可预测。
// 被限流的表现是**回包永远不来**(gate 直接丢),排查成本远高于这里多等 1 秒,所以宁可退火。
const guildSmokeSameIdCooldown = 1100 * time.Millisecond

// 超长公告:> guild MaxAnnouncementBytes(600),又让整个 ClientRequest < gate 的 1KB,拒绝才来自 guild 而不是 gate。
const guildSmokeOversizeAnnouncementBytes = 700

// 查本区榜的页长,等于 guild 对客户端的上限 MaxRankPageSize。
const guildSmokeRankPageSize uint32 = 50

// 成员身份编码是持久化契约(go/guild/internal/constants:0=member / 1=officer / 3=leader;2 是刻意跳过的空号)。
const (
	guildSmokeRoleMember  uint32 = 0
	guildSmokeRoleOfficer uint32 = 1
	guildSmokeRoleLeader  uint32 = 3
)

// GuildLevel 第 1 级的长老位上限。这里**故意**写死而不是跟着配表走:
// 冒烟要同时盯住"配表被人改了"和"服务端没按配表算"两件事,跟着表走就只剩后者。
// 配表真要改第 1 级的上限,改这个常量即可(设计 02-management.md §3.2)。
const guildSmokeLevel1MaxOfficers uint32 = 2

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipGuildAlreadyInGuild     = uint32(tiptable.GuildError_kGuildAlreadyInGuild)
	tipGuildNotFound           = uint32(tiptable.GuildError_kGuildNotFound)
	tipGuildNotInGuild         = uint32(tiptable.GuildError_kGuildNotInGuild)
	tipGuildLeaderCantLeave    = uint32(tiptable.GuildError_kGuildLeaderCantLeave)
	tipGuildNoPermission       = uint32(tiptable.GuildError_kGuildNoPermission)
	tipGuildNotRanked          = uint32(tiptable.GuildError_kGuildNotRanked)
	tipGuildNameTaken          = uint32(tiptable.GuildError_kGuildNameTaken)
	tipGuildAnnouncementLong   = uint32(tiptable.GuildError_kGuildAnnouncementTooLong)
	tipGuildHomeZoneUnknown    = uint32(tiptable.GuildError_kGuildHomeZoneUnknown)
	tipGuildServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
	// 管理段(B2)新增的四个码。
	tipGuildTargetNotMember     = uint32(tiptable.GuildError_kGuildTargetNotMember)
	tipGuildCannotTargetSelf    = uint32(tiptable.GuildError_kGuildCannotTargetSelf)
	tipGuildRankTooLow          = uint32(tiptable.GuildError_kGuildRankTooLow)
	tipGuildApplicationNotFound = uint32(tiptable.GuildError_kGuildApplicationNotFound)
)

var errGuildSmokeTimeout = errors.New("等回包超时")

// guildResponse 是所有公会响应的共同形状:业务拒绝码在响应体 error_message 里。
type guildResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// guildSmokeBot 是一个已登录进场的机器人会话。
type guildSmokeBot struct {
	account string
	zone    uint32
	gate    string
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// caller 发请求并只等它自己的回包(按请求号认领,见 smoke_rpc_call.go)。
	caller *smokeCaller

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu sync.Mutex
	// 收到的 NotifyGuildChanged,按到达顺序追加。推送是服务端主动下行(id = 0),与请求不成对;
	// 由 clearPushes / waitPush 消费。
	pushes []*guildpb.GuildChangedS2C

	lastRequestAt time.Time // 仅主流程读写
}

// RunGuildSmoke 是 main.go `mode: guild-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunGuildSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.GuildSmoke

	type loginSpec struct {
		account string
		zone    uint32
	}
	// 同区五人的下标固定在前五位,别区的 C 排最后:C 是可选的,把它排在中间会让
	// 后面的人随 cross_zone 开关换下标,读代码时极易看错是谁。
	specs := []loginSpec{
		{guildSmokeAccountA, sc.ZoneA},
		{guildSmokeAccountB, sc.ZoneA},
		{guildSmokeAccountD, sc.ZoneA},
		{guildSmokeAccountE, sc.ZoneA},
		{guildSmokeAccountF, sc.ZoneA},
	}
	const guildSmokeCrossZoneIndex = 5 // C 的下标,仅 cross_zone=true 时存在
	if sc.CrossZone {
		specs = append(specs, loginSpec{guildSmokeAccountC, sc.ZoneB})
	}

	var bots []*guildSmokeBot
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("GUILD_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录(并发) ----
	loggedIn := make([]*guildSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = spec.zone
			loggedIn[idx], loginErrs[idx] = guildSmokeLogin(&zoneCfg, spec.account, stats)
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
	a, b := loggedIn[0], loggedIn[1]
	d, e, f := loggedIn[2], loggedIn[3], loggedIn[4] // 管理段的长老 / 被踢者 / 被拒者
	var c *guildSmokeBot
	if sc.CrossZone {
		c = loggedIn[guildSmokeCrossZoneIndex]
		// 本地各 zone 的 gate 端口不同;地址相同说明 C 其实进了 zone_a,后面"别区不可见"的结论就不成立。
		if c.gate == a.gate {
			fail("zone-placement", "C and A were assigned the same gate %s (zone_a=%d zone_b=%d); this is not a cross-zone run", a.gate, sc.ZoneA, sc.ZoneB)
		}
	}
	zap.L().Info("[guild-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId),
		zap.Uint64("d", d.gc.PlayerId), zap.Uint64("e", e.gc.PlayerId), zap.Uint64("f", f.gc.PlayerId),
		zap.Bool("cross_zone", sc.CrossZone))

	// ---- 步骤 0:回到"不在任何帮会、也没有待审申请" ----
	// 先撤申请再退帮:上一轮中途失败会留下待审申请,它占着"每人最多 3 个待审"的名额,
	// 下一轮第 6 步的申请就会被拒。在帮会里的玩家列不出自己的申请(服务端直接回空表),
	// 不过"在册"与"有待审申请"本就互斥,顺序不影响清理效果。
	for _, bot := range bots {
		if err := bot.cancelAllApplications(); err != nil {
			fail("prepare", "account=%s: %v", bot.account, err)
		}
		if err := bot.leaveAnyGuild(); err != nil {
			fail("prepare", "account=%s: %v", bot.account, err)
		}
	}

	// ---- 步骤 1:A 建帮,zone 由服务端决定 ----
	guildName := "烟测" + guildSmokeNonce()
	spoofedZone := sc.ZoneA + 1000
	created := &guildpb.CreateGuildResponse{}
	if err := a.expect(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: guildName, ZoneId: spoofedZone}, created, 0); err != nil {
		fail("create", "%v", err)
	}
	guild := created.GetGuild()
	guildID := guild.GetGuildId()
	if guildID == 0 {
		fail("create", "Guild creation was accepted but the response contains no guild")
	}
	if guild.GetZoneId() != sc.ZoneA {
		fail("create-zone", "Guild zone_id=%d, expected A home zone %d (the server must ignore request-body zone_id=%d)",
			guild.GetZoneId(), sc.ZoneA, spoofedZone)
	}
	if role, ok := guildSmokeRoleOf(guild, a.gc.PlayerId); guild.GetLeaderId() != a.gc.PlayerId || !ok || role != guildSmokeRoleLeader {
		fail("create-leader", "leader_id=%d A=%d A role=%d (member=%v), expected A to be guild leader", guild.GetLeaderId(), a.gc.PlayerId, role, ok)
	}
	zap.L().Info("[guild-smoke] step 1: guild created in home zone",
		zap.Uint64("guild_id", guildID), zap.String("name", guildName), zap.Uint32("zone", guild.GetZoneId()))

	// ---- 步骤 2:已在帮会里不能再建 ----
	if err := a.expect(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: guildName + "二"}, &guildpb.CreateGuildResponse{}, tipGuildAlreadyInGuild); err != nil {
		fail("create-again", "%v", err)
	}

	// ---- 步骤 3:同区 B 在本区榜上看得到 ----
	sameZoneRank := &guildpb.GetGuildRankByGuildResponse{}
	if err := b.expect(game.GuildServiceGetGuildRankByGuildMessageId,
		&guildpb.GetGuildRankByGuildRequest{GuildId: guildID}, sameZoneRank, 0); err != nil {
		fail("rank-same-zone", "%v", err)
	}
	if sameZoneRank.GetEntry().GetRank() == 0 || sameZoneRank.GetEntry().GetName() != guildName {
		fail("rank-same-zone", "Local-zone ranking entry rank=%d name=%q, expected ranked with name %q",
			sameZoneRank.GetEntry().GetRank(), sameZoneRank.GetEntry().GetName(), guildName)
	}
	zap.L().Info("[guild-smoke] step 3: same-zone player sees the guild on zone rank", zap.Uint32("rank", sameZoneRank.GetEntry().GetRank()))

	// ---- 步骤 4:别区 C 看不见、加不进、同名被拒 ----
	if c != nil {
		if err := c.expect(game.GuildServiceGetGuildMessageId,
			&guildpb.GetGuildRequest{GuildId: guildID}, &guildpb.GetGuildResponse{}, tipGuildNotFound); err != nil {
			fail("other-zone-get", "%v", err)
		}
		if err := c.expect(game.GuildServiceGetGuildRankByGuildMessageId,
			&guildpb.GetGuildRankByGuildRequest{GuildId: guildID, ZoneId: sc.ZoneA}, &guildpb.GetGuildRankByGuildResponse{}, tipGuildNotRanked); err != nil {
			fail("other-zone-rank-entry", "%v", err)
		}
		page := &guildpb.GetGuildRankResponse{}
		if err := c.expect(game.GuildServiceGetGuildRankMessageId,
			&guildpb.GetGuildRankRequest{Page: 1, PageSize: guildSmokeRankPageSize, ZoneId: sc.ZoneA}, page, 0); err != nil {
			fail("other-zone-rank-page", "%v", err)
		}
		for _, entry := range page.GetEntries() {
			if entry.GetGuildId() == guildID {
				fail("other-zone-rank-page", "A player in zone %d saw a guild from zone %d with id %d in the ranking", sc.ZoneB, sc.ZoneA, guildID)
			}
		}
		if err := c.expect(game.GuildServiceApplyJoinGuildMessageId,
			&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, tipGuildNotFound); err != nil {
			fail("other-zone-apply", "%v", err)
		}
		if err := c.expect(game.GuildServiceCreateGuildMessageId,
			&guildpb.CreateGuildRequest{Name: guildName}, &guildpb.CreateGuildResponse{}, tipGuildNameTaken); err != nil {
			fail("other-zone-same-name", "%v", err)
		}
		zap.L().Info("[guild-smoke] step 4: other-zone player cannot see or join; name is globally unique")
	}

	// ---- 步骤 5:非成员不能改公告 ----
	if err := b.expect(game.GuildServiceSetAnnouncementMessageId,
		&guildpb.SetAnnouncementRequest{GuildId: guildID, Announcement: "非成员不该能改"}, &guildpb.SetAnnouncementResponse{}, tipGuildNoPermission); err != nil {
		fail("announcement-outsider", "%v", err)
	}

	// ---- 步骤 6:B 申请入帮,A 审批通过 ----
	// 入帮从"自己点一下就进"改成申请 + 审批两段(B2),所以这一步要同时验:
	// 申请落库(两侧列表各看得到一条)、审批受理、审批响应的快照里 B 已在册、B 自己查到的也是在册。
	if err := b.expect(game.GuildServiceApplyJoinGuildMessageId,
		&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
		fail("apply", "%v", err)
	}
	myApplications := &guildpb.ListMyGuildApplicationsResponse{}
	if err := b.expect(game.GuildServiceListMyGuildApplicationsMessageId,
		&guildpb.ListMyGuildApplicationsRequest{}, myApplications, 0); err != nil {
		fail("apply-list-mine", "%v", err)
	}
	if !guildSmokeHasApplication(myApplications.GetApplications(), guildID) {
		fail("apply-list-mine", "B pending applications contain no guild %d (%d entries total)", guildID, len(myApplications.GetApplications()))
	}
	applicants := &guildpb.ListGuildApplicationsResponse{}
	if err := a.expect(game.GuildServiceListGuildApplicationsMessageId,
		&guildpb.ListGuildApplicationsRequest{}, applicants, 0); err != nil {
		fail("apply-list-applicants", "%v", err)
	}
	if !guildSmokeHasApplicant(applicants.GetApplicants(), b.gc.PlayerId) {
		fail("apply-list-applicants", "A pending applicant list contains no B=%d (%d entries total)", b.gc.PlayerId, len(applicants.GetApplicants()))
	}
	reviewed := &guildpb.ReviewGuildApplicationResponse{}
	// 清空留底必须在 A 发审批之前:审批一提交服务端就推,晚清一步会把这条推送一起丢掉。
	b.clearPushes()
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: b.gc.PlayerId, Approve: true}, reviewed, 0); err != nil {
		fail("review", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(reviewed.GetGuild(), b.gc.PlayerId); !ok || role != guildSmokeRoleMember {
		fail("review", "In approval-response snapshot B=%d role=%d (member=%v), expected an ordinary member", b.gc.PlayerId, role, ok)
	}
	// 新成员自己必须收到 MEMBER_JOINED:客户端靠它把"申请中"翻成"已入帮",没有推送就只能等玩家手动刷新。
	if err := b.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_JOINED, guildID, guildSmokePushTimeout); err != nil {
		fail("review-push", "B did not receive the guild-join push: %v", err)
	}
	bGuild, err := b.myGuild()
	if err != nil {
		fail("join-verify", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(bGuild, b.gc.PlayerId); bGuild.GetGuildId() != guildID || !ok || role != guildSmokeRoleMember {
		fail("join-verify", "B guild=%d role=%d (member=%v), expected membership in guild %d as an ordinary member", bGuild.GetGuildId(), role, ok, guildID)
	}
	// member-names(B3b,设计 docs/design/guild-phase2/03-names.md §3.21):成员名与帮主名由 guild 向
	// data_service 批量取,是展示字段、服务端 fail-open —— 取不到时 RPC 照样成功、名字留空,
	// 所以只有在这里断言"非空"才看得见取名链路断了。冒烟账号理应都是 B3a 之后建的角色、人人有名;
	// 读到空名先按文案清账号重跑,仍然为空再查 guild 日志的 "player name lookup failed"
	// 与指标 guild_player_name_lookup_failed_total(guild → data_service 不通)。
	// 清理范围按 90-consistency.md Y-09 含 9211–9219(帮会各批冒烟登记的账号段):旧角色要清就一次清全。
	memberNames := make([]string, 0, len(bGuild.GetMembers()))
	for _, member := range bGuild.GetMembers() {
		if member.GetName() == "" {
			fail("member-names", "role %d has no name: legacy role created before B3a — DEL account:robot_9201/9202/9203 and account:robot_9211–9219 then rerun",
				member.GetPlayerId())
		}
		memberNames = append(memberNames, fmt.Sprintf("%d=%s", member.GetPlayerId(), member.GetName()))
	}
	if bGuild.GetLeaderName() == "" {
		fail("member-names", "leader role %d has no leader_name: legacy role created before B3a — DEL account:robot_9201/9202/9203 and account:robot_9211–9219 then rerun",
			bGuild.GetLeaderId())
	}
	zap.L().Info("[guild-smoke] member-names: every member and the leader carry a display name",
		zap.Strings("members", memberNames), zap.String("leader_name", bGuild.GetLeaderName()))
	zap.L().Info("[guild-smoke] step 6: B applied and was approved", zap.Int("members", len(bGuild.GetMembers())))

	// ---- 步骤 7:公告 ----
	announcement := "烟测公告 " + guildSmokeNonce()
	if err := a.expect(game.GuildServiceSetAnnouncementMessageId,
		&guildpb.SetAnnouncementRequest{GuildId: guildID, Announcement: announcement}, &guildpb.SetAnnouncementResponse{}, 0); err != nil {
		fail("announcement", "%v", err)
	}
	if bGuild, err = b.myGuild(); err != nil {
		fail("announcement-read", "%v", err)
	}
	if bGuild.GetAnnouncement() != announcement {
		fail("announcement-read", "B read announcement %q, expected %q", bGuild.GetAnnouncement(), announcement)
	}
	if err := a.expect(game.GuildServiceSetAnnouncementMessageId,
		&guildpb.SetAnnouncementRequest{GuildId: guildID, Announcement: strings.Repeat("x", guildSmokeOversizeAnnouncementBytes)},
		&guildpb.SetAnnouncementResponse{}, tipGuildAnnouncementLong); err != nil {
		fail("announcement-oversize", "%v", err)
	}
	zap.L().Info("[guild-smoke] step 7: announcement published and oversize rejected")

	// ---- 步骤 8:请求体伪造 player_id 不生效 ----
	leaveTip, err := b.rpc(game.GuildServiceLeaveGuildMessageId,
		&guildpb.LeaveGuildRequest{PlayerId: a.gc.PlayerId}, &guildpb.LeaveGuildResponse{})
	if err != nil {
		fail("spoof-leave", "%v", err)
	}
	if leaveTip == tipGuildLeaderCantLeave {
		fail("spoof-leave", "The server processed B leave request as forged player_id=%d (leader A) from the request body; identity was not taken from the session", a.gc.PlayerId)
	}
	if leaveTip != 0 {
		fail("spoof-leave", "B leaving the guild should be accepted (tip=0), got tip=%d", leaveTip)
	}
	if err := b.expect(game.GuildServiceGetPlayerGuildMessageId,
		&guildpb.GetPlayerGuildRequest{}, &guildpb.GetPlayerGuildResponse{}, tipGuildNotInGuild); err != nil {
		fail("spoof-leave-verify-b", "%v", err)
	}
	aGuild, err := a.myGuild()
	if err != nil {
		fail("spoof-leave-verify-a", "%v", err)
	}
	if aGuild.GetLeaderId() != a.gc.PlayerId || len(aGuild.GetMembers()) != 1 {
		fail("spoof-leave-verify-a", "A guild leader=%d member count=%d, expected A still as leader with only 1 member", aGuild.GetLeaderId(), len(aGuild.GetMembers()))
	}
	zap.L().Info("[guild-smoke] step 8: spoofed player_id ignored, session identity used")

	// ---- 步骤 9:内部方法 UpdateGuildScore 对客户端关闭 ----
	scoreEnvelopeTip, err := a.call(game.GuildServiceUpdateGuildScoreMessageId,
		&guildpb.UpdateGuildScoreRequest{GuildId: guildID, Score: 999999, ZoneId: sc.ZoneA}, &guildpb.UpdateGuildScoreResponse{})
	if err != nil {
		fail("internal-method", "%v", err)
	}
	// 路由服将上游 PermissionDenied 映射为 kServiceUnavailable。超时、成功业务回包、
	// 其它信封码(如路由表未更新)都不能当作方法白名单生效的证据。
	if scoreEnvelopeTip != tipGuildServiceUnavailable {
		fail("internal-method", "UpdateGuildScore expected envelope tip=%d, got tip=%d (0 means a business response was received)",
			tipGuildServiceUnavailable, scoreEnvelopeTip)
	}
	scoreAfter := &guildpb.GetGuildRankByGuildResponse{}
	if err := a.expect(game.GuildServiceGetGuildRankByGuildMessageId,
		&guildpb.GetGuildRankByGuildRequest{GuildId: guildID}, scoreAfter, 0); err != nil {
		fail("internal-method-verify", "%v", err)
	}
	if scoreAfter.GetEntry().GetScore() != 0 {
		fail("internal-method-verify", "The client changed guild score: score=%d, expected 0", scoreAfter.GetEntry().GetScore())
	}
	// 不扩展线协议就不能区分 PermissionDenied 与其它上游故障:这里只声明已观察到的拒绝
	// 与积分不变,验收时还须核对路由服该方法的 code=PermissionDenied 日志与 session 单测。
	zap.L().Info("[guild-smoke] step 9: received envelope rejection, then successfully queried the ranking with unchanged score; verify allowlist rejection through routing-service code=PermissionDenied logs and session unit tests",
		zap.Uint32("envelope_tip", scoreEnvelopeTip),
		zap.String("method", guildpb.GuildService_UpdateGuildScore_FullMethodName))

	// ---- 管理段 M1–M10(02-management.md §24.2)----
	// 此刻帮会里只有帮主 A(B 已在第 8 步退帮),D / E / F 都不在任何帮会(步骤 0 清理过)。

	// M1:D、E、F 各申请一次;F 再申请一次。同帮重复申请按契约是"刷新有效期"而不是新增行,
	// 所以 F 的待审列表必须仍然只有 1 条 —— 这条断言守住的是"申请表按 (帮, 人) 唯一"。
	for _, applicant := range []*guildSmokeBot{d, e, f} {
		if err := applicant.expect(game.GuildServiceApplyJoinGuildMessageId,
			&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
			fail("mgmt-M1", "%s applying to join the guild: %v", applicant.account, err)
		}
	}
	if err := f.expect(game.GuildServiceApplyJoinGuildMessageId,
		&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
		fail("mgmt-M1", "F reapplying to the same guild should be accepted (refreshing expiry): %v", err)
	}
	fApplications := &guildpb.ListMyGuildApplicationsResponse{}
	if err := f.expect(game.GuildServiceListMyGuildApplicationsMessageId,
		&guildpb.ListMyGuildApplicationsRequest{}, fApplications, 0); err != nil {
		fail("mgmt-M1", "F listing personal applications: %v", err)
	}
	if len(fApplications.GetApplications()) != 1 || !guildSmokeHasApplication(fApplications.GetApplications(), guildID) {
		fail("mgmt-M1", "F has %d pending applications after applying twice (expected exactly 1 pointing to guild %d); the duplicate application was treated as a new row",
			len(fApplications.GetApplications()), guildID)
	}

	// M2:待审名单是管理侧信息,不在帮里的人连"谁在申请这个帮"都不该看到。
	if err := e.expect(game.GuildServiceListGuildApplicationsMessageId,
		&guildpb.ListGuildApplicationsRequest{}, &guildpb.ListGuildApplicationsResponse{}, tipGuildNotInGuild); err != nil {
		fail("mgmt-M2", "A nonmember listing guild pending applicants: %v", err)
	}

	// M3:A 审批 D、E 通过。推送的收件人是"快照里的全体成员除审批人",新成员自己也在里面。
	d.clearPushes()
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: d.gc.PlayerId, Approve: true},
		&guildpb.ReviewGuildApplicationResponse{}, 0); err != nil {
		fail("mgmt-M3", "Approving D: %v", err)
	}
	if err := d.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_JOINED, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M3", "D did not receive the guild-join push: %v", err)
	}
	approvedE := &guildpb.ReviewGuildApplicationResponse{}
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: e.gc.PlayerId, Approve: true}, approvedE, 0); err != nil {
		fail("mgmt-M3", "Approving E: %v", err)
	}
	if role, ok := guildSmokeRoleOf(approvedE.GetGuild(), e.gc.PlayerId); !ok || role != guildSmokeRoleMember {
		fail("mgmt-M3", "In approval-response snapshot E=%d role=%d (member=%v), expected an ordinary member", e.gc.PlayerId, role, ok)
	}

	// M4:拒绝只推给申请人本人(告诉全帮"谁被拒了"只有社交伤害)。拒绝会删掉申请行,
	// 所以 F 的列表清空、同一条申请再审无行可审。
	f.clearPushes()
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: f.gc.PlayerId, Approve: false},
		&guildpb.ReviewGuildApplicationResponse{}, 0); err != nil {
		fail("mgmt-M4", "Rejecting F: %v", err)
	}
	if err := f.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_APPLICATION_REJECTED, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M4", "F did not receive the rejection push: %v", err)
	}
	fApplications = &guildpb.ListMyGuildApplicationsResponse{}
	if err := f.expect(game.GuildServiceListMyGuildApplicationsMessageId,
		&guildpb.ListMyGuildApplicationsRequest{}, fApplications, 0); err != nil {
		fail("mgmt-M4", "F listing personal applications: %v", err)
	}
	if len(fApplications.GetApplications()) != 0 {
		fail("mgmt-M4", "F still has %d pending applications after rejection, expected 0 (rejection must delete the application row)", len(fApplications.GetApplications()))
	}
	// 这是 A 的第 4 次 ReviewGuildApplication,先退火再发(见 guildSmokeSameIdCooldown)。
	// 重复审批用"再拒一次"而不是"改判通过":通过分支会多走一次申请人归属 zone 查询,
	// 而这里要验的是"申请行已经不在了",不该被那条链路的故障搅进来。
	time.Sleep(guildSmokeSameIdCooldown)
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: f.gc.PlayerId, Approve: false},
		&guildpb.ReviewGuildApplicationResponse{}, tipGuildApplicationNotFound); err != nil {
		fail("mgmt-M4", "Approving the same application again: %v", err)
	}

	// M5:任命 D 为长老。第二次是同职位任免,服务端判"一个字节都没变"→ 幂等受理且**不推送**,
	// 所以推送只在第一次之后等;第二次若也推,只会让全帮白拉一次。
	d.clearPushes()
	promoted := &guildpb.SetGuildMemberRoleResponse{}
	if err := a.expect(game.GuildServiceSetGuildMemberRoleMessageId,
		&guildpb.SetGuildMemberRoleRequest{TargetPlayerId: d.gc.PlayerId, Role: guildSmokeRoleOfficer}, promoted, 0); err != nil {
		fail("mgmt-M5", "Appointing D as elder: %v", err)
	}
	if err := guildSmokeCheckOfficer(promoted.GetGuild(), d.gc.PlayerId); err != nil {
		fail("mgmt-M5", "First appointment: %v", err)
	}
	if err := d.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_ROLE_CHANGED, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M5", "D did not receive the role-change push: %v", err)
	}
	promotedAgain := &guildpb.SetGuildMemberRoleResponse{}
	if err := a.expect(game.GuildServiceSetGuildMemberRoleMessageId,
		&guildpb.SetGuildMemberRoleRequest{TargetPlayerId: d.gc.PlayerId, Role: guildSmokeRoleOfficer}, promotedAgain, 0); err != nil {
		fail("mgmt-M5", "Repeating the same appointment should be accepted idempotently: %v", err)
	}
	if err := guildSmokeCheckOfficer(promotedAgain.GetGuild(), d.gc.PlayerId); err != nil {
		fail("mgmt-M5", "Idempotent appointment: %v", err)
	}

	// M6:长老的权限边界。D 在这一段里连发 4 次 KickGuildMember,最后一次前退火。
	e.clearPushes()
	kicked := &guildpb.KickGuildMemberResponse{}
	if err := d.expect(game.GuildServiceKickGuildMemberMessageId,
		&guildpb.KickGuildMemberRequest{TargetPlayerId: e.gc.PlayerId}, kicked, 0); err != nil {
		fail("mgmt-M6", "Elder kicking an ordinary member: %v", err)
	}
	if _, stillIn := guildSmokeRoleOf(kicked.GetGuild(), e.gc.PlayerId); stillIn {
		fail("mgmt-M6", "E=%d is still a member in the kick-response snapshot", e.gc.PlayerId)
	}
	// 被踢的人必须自己收到推送,否则他的界面会一直停在"我还在帮里",直到下次手动刷新。
	if err := e.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_KICKED, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M6", "E did not receive the removal push: %v", err)
	}
	if err := e.expect(game.GuildServiceGetPlayerGuildMessageId,
		&guildpb.GetPlayerGuildRequest{}, &guildpb.GetPlayerGuildResponse{}, tipGuildNotInGuild); err != nil {
		fail("mgmt-M6", "E can still query its guild after being kicked: %v", err)
	}
	if err := d.expect(game.GuildServiceKickGuildMemberMessageId,
		&guildpb.KickGuildMemberRequest{TargetPlayerId: a.gc.PlayerId}, &guildpb.KickGuildMemberResponse{}, tipGuildRankTooLow); err != nil {
		fail("mgmt-M6", "Elder kicking the leader: %v", err)
	}
	if err := d.expect(game.GuildServiceKickGuildMemberMessageId,
		&guildpb.KickGuildMemberRequest{TargetPlayerId: d.gc.PlayerId}, &guildpb.KickGuildMemberResponse{}, tipGuildCannotTargetSelf); err != nil {
		fail("mgmt-M6", "Elder kicking itself: %v", err)
	}
	if err := d.expect(game.GuildServiceSetGuildMemberRoleMessageId,
		&guildpb.SetGuildMemberRoleRequest{TargetPlayerId: a.gc.PlayerId, Role: guildSmokeRoleOfficer},
		&guildpb.SetGuildMemberRoleResponse{}, tipGuildRankTooLow); err != nil {
		fail("mgmt-M6", "Elder changing the leader role: %v", err)
	}
	time.Sleep(guildSmokeSameIdCooldown)
	if err := d.expect(game.GuildServiceKickGuildMemberMessageId,
		&guildpb.KickGuildMemberRequest{TargetPlayerId: e.gc.PlayerId}, &guildpb.KickGuildMemberResponse{}, tipGuildTargetNotMember); err != nil {
		fail("mgmt-M6", "Kicking E again after it left the guild: %v", err)
	}

	// M7:转让帮主给 D。D 让出长老位后长老只剩 0 人,所以原帮主 A 降为长老而不是成员。
	d.clearPushes()
	transferred := &guildpb.TransferGuildLeaderResponse{}
	if err := a.expect(game.GuildServiceTransferGuildLeaderMessageId,
		&guildpb.TransferGuildLeaderRequest{TargetPlayerId: d.gc.PlayerId}, transferred, 0); err != nil {
		fail("mgmt-M7", "A transferring guild leadership to D: %v", err)
	}
	if err := guildSmokeCheckLeadership(transferred.GetGuild(), d.gc.PlayerId, a.gc.PlayerId); err != nil {
		fail("mgmt-M7", "%v", err)
	}
	if err := d.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_LEADER_TRANSFERRED, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M7", "D did not receive the leadership-transfer push: %v", err)
	}

	// M8:再转回 A。这一步既验"新帮主转得动",也把帮会恢复成"A 是帮主",后面第 10 步才解散得了。
	backToA := &guildpb.TransferGuildLeaderResponse{}
	if err := d.expect(game.GuildServiceTransferGuildLeaderMessageId,
		&guildpb.TransferGuildLeaderRequest{TargetPlayerId: a.gc.PlayerId}, backToA, 0); err != nil {
		fail("mgmt-M8", "D transferring guild leadership back to A: %v", err)
	}
	if err := guildSmokeCheckLeadership(backToA.GetGuild(), a.gc.PlayerId, d.gc.PlayerId); err != nil {
		fail("mgmt-M8", "%v", err)
	}

	// M9:D 主动退帮,留守的 A 收到 MEMBER_LEFT(收件人是退帮**之后**的在册成员)。
	a.clearPushes()
	if err := d.expect(game.GuildServiceLeaveGuildMessageId,
		&guildpb.LeaveGuildRequest{}, &guildpb.LeaveGuildResponse{}, 0); err != nil {
		fail("mgmt-M9", "D leaving the guild: %v", err)
	}
	if err := a.waitPush(guildpb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_LEFT, guildID, guildSmokePushTimeout); err != nil {
		fail("mgmt-M9", "A did not receive the guild-leave push: %v", err)
	}

	// M10 前半:F 重新申请,给第 10 步的解散留一条待审行去清理。
	// 这里**不**断言 APPLICATION_RECEIVED:同一 (帮, 申请人) 60 秒内至多推一次,M1 那次刚推过,
	// 这次多半被冷却闸门挡下;断言它会让冒烟随运行快慢随机红,那是最难排查的一类不稳定。
	if err := f.expect(game.GuildServiceApplyJoinGuildMessageId,
		&guildpb.ApplyJoinGuildRequest{GuildId: guildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
		fail("mgmt-M10", "F applying again: %v", err)
	}

	// ---- 步骤 10:解散 ----
	if err := a.expect(game.GuildServiceDisbandGuildMessageId,
		&guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0); err != nil {
		fail("disband", "%v", err)
	}
	if err := a.expect(game.GuildServiceGetPlayerGuildMessageId,
		&guildpb.GetPlayerGuildRequest{}, &guildpb.GetPlayerGuildResponse{}, tipGuildNotInGuild); err != nil {
		fail("disband-verify", "%v", err)
	}
	if err := a.expect(game.GuildServiceGetGuildRankByGuildMessageId,
		&guildpb.GetGuildRankByGuildRequest{GuildId: guildID}, &guildpb.GetGuildRankByGuildResponse{}, tipGuildNotRanked); err != nil {
		fail("disband-rank", "%v", err)
	}

	// M10 后半:解散会连带删掉该帮的全部待审申请。漏删的话这些行会一直占着 F 的
	// "每人最多 3 个待审"名额,而且指向一个已经不存在的帮会 —— 客户端列表里就是一条永远点不动的脏数据。
	fApplications = &guildpb.ListMyGuildApplicationsResponse{}
	if err := f.expect(game.GuildServiceListMyGuildApplicationsMessageId,
		&guildpb.ListMyGuildApplicationsRequest{}, fApplications, 0); err != nil {
		fail("mgmt-M10", "F listing personal applications after disbanding: %v", err)
	}
	if len(fApplications.GetApplications()) != 0 {
		fail("mgmt-M10", "F still has %d pending applications after guild disbanding, expected 0", len(fApplications.GetApplications()))
	}
	zap.L().Info(fmt.Sprintf("GUILD_MGMT_OK guild_id=%d leader=%d officer=%d kicked=%d rejected=%d",
		guildID, a.gc.PlayerId, d.gc.PlayerId, e.gc.PlayerId, f.gc.PlayerId))

	// ---- 步骤 11:迟到的退帮带着过期的 expected_guild_id,不得作用到玩家的新帮会 ----
	// 要钉住的窗口(client-request-ordering.md §6.4):路由服回超时、gate 放行下一条之后,上一条退帮仍可能在 guild 里跑完,
	// 玩家这时若已换到新帮,按「此刻所在的帮」执行就会把他从新帮移出。现有流程里没人「离开旧帮、身在新帮」,所以另建临时帮来造;
	// 旧帮已在第 10 步解散,这也是服务端列明要拒的一种(预期帮已不存在),不能按旧客户端路径退回 operatorGuild。
	// 「旧帮仍在」的那一种不另造:缓存新鲜时两种在 guild 里走同一条预检分支(本人映射 ≠ 预期即拒),已由 go/guild 的
	// TestExpectedGuildMismatchRejectedWithoutWrite(integration)覆盖;这里要钉的是字段经 gate → 路由服真实带到、部署的二进制确有栅栏。
	// B 必须是普通成员:帮主本来就退不了帮,「仍在 H」会变成帮主规则的功劳,栅栏坏了也看不出来。
	// 末尾带 H 的 id 再退一次是对照:服务端若把一切非 0 的 expected_guild_id 都拒掉,前面的拒绝断言照样会过。
	// 中途失败留下的 H 由下一轮步骤 0 解散(A 是帮主,B 若还在册会随解散离开),可重复运行。
	tempGuildName := "烟测栅栏" + guildSmokeNonce()
	tempCreated := &guildpb.CreateGuildResponse{}
	if err := a.expect(game.GuildServiceCreateGuildMessageId,
		&guildpb.CreateGuildRequest{Name: tempGuildName}, tempCreated, 0); err != nil {
		fail("stale-expected-create", "%v", err)
	}
	tempGuildID := tempCreated.GetGuild().GetGuildId()
	if tempGuildID == 0 || tempGuildID == guildID {
		fail("stale-expected-create", "Temporary guild id=%d, expected a new guild different from the disbanded guild %d", tempGuildID, guildID)
	}
	if err := b.expect(game.GuildServiceApplyJoinGuildMessageId,
		&guildpb.ApplyJoinGuildRequest{GuildId: tempGuildID}, &guildpb.ApplyJoinGuildResponse{}, 0); err != nil {
		fail("stale-expected-apply", "%v", err)
	}
	tempReviewed := &guildpb.ReviewGuildApplicationResponse{}
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: b.gc.PlayerId, Approve: true}, tempReviewed, 0); err != nil {
		fail("stale-expected-review", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(tempReviewed.GetGuild(), b.gc.PlayerId); !ok || role != guildSmokeRoleMember {
		fail("stale-expected-review", "In approval-response snapshot B=%d role=%d (member=%v), expected an ordinary member of temporary guild %d",
			b.gc.PlayerId, role, ok, tempGuildID)
	}
	staleTip, err := b.rpc(game.GuildServiceLeaveGuildMessageId,
		&guildpb.LeaveGuildRequest{ExpectedGuildId: guildID}, &guildpb.LeaveGuildResponse{})
	if err != nil {
		fail("stale-expected-leave", "%v", err)
	}
	if staleTip != tipGuildNotInGuild {
		fail("stale-expected-leave", "B LeaveGuild with stale expected_guild_id=%d (B is now in guild %d) expected tip=%d, got tip=%d "+
			"(tip=0 means the server ignored expected_guild_id, i.e. the guild binary predates E2 or the fence is broken, and B was removed from the new guild)",
			guildID, tempGuildID, tipGuildNotInGuild, staleTip)
	}
	bTemp, err := b.myGuild()
	if err != nil {
		fail("stale-expected-verify", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(bTemp, b.gc.PlayerId); bTemp.GetGuildId() != tempGuildID || !ok || role != guildSmokeRoleMember || len(bTemp.GetMembers()) != 2 {
		fail("stale-expected-verify", "After the refused stale leave B guild=%d role=%d (member=%v) member count=%d, expected B still an ordinary member of guild %d with 2 members",
			bTemp.GetGuildId(), role, ok, len(bTemp.GetMembers()), tempGuildID)
	}
	if err := b.expect(game.GuildServiceLeaveGuildMessageId,
		&guildpb.LeaveGuildRequest{ExpectedGuildId: tempGuildID}, &guildpb.LeaveGuildResponse{}, 0); err != nil {
		fail("stale-expected-control", "B LeaveGuild with matching expected_guild_id=%d should be accepted: %v", tempGuildID, err)
	}
	if err := b.expect(game.GuildServiceGetPlayerGuildMessageId,
		&guildpb.GetPlayerGuildRequest{}, &guildpb.GetPlayerGuildResponse{}, tipGuildNotInGuild); err != nil {
		fail("stale-expected-control", "B is still in a guild after leaving with matching expected_guild_id=%d: %v", tempGuildID, err)
	}
	if err := a.expect(game.GuildServiceDisbandGuildMessageId,
		&guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0); err != nil {
		fail("stale-expected-cleanup", "Disbanding temporary guild %d: %v", tempGuildID, err)
	}
	zap.L().Info("[guild-smoke] step 11: late LeaveGuild with a stale expected_guild_id refused and the player stayed in the new guild; matching expected_guild_id accepted",
		zap.Uint64("stale_guild_id", guildID), zap.Uint64("new_guild_id", tempGuildID))

	playerC := uint64(0)
	if c != nil {
		playerC = c.gc.PlayerId
	}
	zap.L().Info(fmt.Sprintf("GUILD_SMOKE_OK guild_id=%d zone_a=%d player_a=%d player_b=%d player_c=%d",
		guildID, sc.ZoneA, a.gc.PlayerId, b.gc.PlayerId, playerC), zap.Bool("cross_zone", sc.CrossZone))
	cleanup()
	// 经济段用自己的两个账号另开会话,放在管理段的会话全部收尾之后:两段互不共享帮会与限流窗口。
	if sc.Economy {
		runGuildEconomySmoke(cfg, stats)
	}
	// 活动段同理:自己的四个账号、自己的帮会,排在经济段的会话收尾之后。
	if sc.Activities {
		runGuildActivitySmoke(cfg, stats)
	}
	_ = zap.L().Sync()
}

// guildSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 chatSmokeLogin 同形:公会请求的回包带请求号,经 caller(smokeCaller)认领;同号却 id = 0 的消息在 onMessage 里丢弃,
// 不交给通用分发(生成的分发表不含 GuildService)。
func guildSmokeLogin(cfg *config.Config, account string, stats *metrics.Stats) (*guildSmokeBot, error) {
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

	bot := &guildSmokeBot{
		account: account,
		zone:    cfg.ZoneID,
		gate:    host + ":" + portStr,
		gc:      gc,
		player:  gameobject.NewPlayer(gc.PlayerId),
		stats:   stats,
		caller:  newSmokeCaller(gc, stats, account, guildSmokeRpcTimeout, errGuildSmokeTimeout),
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), guildSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// onMessage:公会请求的回包(含信封拒绝)带请求号,由 GameClient 交给 caller,不会到这里;
// NotifyGuildChanged 解码留底;gate 的 SendTipToClient 交给 caller 供快速失败;其余交给通用分发。
//
// 战斗推送(大厅连接上的 NotifyBattleAssigned / NotifyBattleStart / NotifyBattleEnd)属于"其余":
// 通用分发把它们转成 gameobject.Player 上的开战 / 落点 / 终局信号,活动段的同道历练(S10)就等在这些信号上,
// 与 battle-smoke 同一套。所以这里**不要**把战斗消息号加进认领名单 —— 认领了,Player 就再也收不到信号。
func (b *guildSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	// NotifyGuildChanged 是服务端主动下行,不是任何请求的回包:通用分发里登记的只是生成的空桩,不消费它。
	// 本场景是它唯一的消费者,留底后即返回。
	if msg.GetMessageId() == game.GuildServiceNotifyGuildChangedMessageId {
		change := &guildpb.GuildChangedS2C{}
		if err := proto.Unmarshal(msg.GetSerializedMessage(), change); err != nil {
			// 解不开不能当"没收到":那会让后面的 waitPush 超时,报成"推送没到",
			// 把一个协议不一致的问题伪装成投递问题。记下来让日志里有据可查。
			zap.L().Error("[guild-smoke] Cannot decode NotifyGuildChanged",
				zap.String("account", b.account), zap.Error(err))
			return
		}
		b.mu.Lock()
		b.pushes = append(b.pushes, change)
		b.mu.Unlock()
		return
	}
	// 公会请求与经济段自己发的 scene 请求(读背包、GM 货币,见 guildEconomyIsSceneMessage)的回包都带请求号;
	// 同号却 id = 0 的消息不交给通用分发(信封拒绝会被解成全零响应),记一行后丢弃。
	if guildSmokeIsGuildMessage(msg.GetMessageId()) || guildEconomyIsSceneMessage(msg.GetMessageId()) {
		smokeDropUncorrelatedReply(b.account, msg)
		return
	}
	if msg.GetMessageId() == game.SceneClientPlayerCommonSendTipToClientMessageId {
		b.caller.onGateTipMessage(msg)
	}
	handler.MessageBodyHandler(client, msg)
}

// call 发一个公会请求并只等它自己的回包(smokeCaller.call)。
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable 或超时返回 error。
func (b *guildSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	if wait := guildSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	b.lastRequestAt = time.Now()
	return b.caller.call(messageId, request, response)
}

// rpc 发请求并返回业务 tip(响应体 error_message.id,0 = 受理)。
// 信封拒绝与超时一律 error:它们说明请求没到 guild 业务逻辑,不能当成 guild 给的码去断言。
func (b *guildSmokeBot) rpc(messageId uint32, request proto.Message, response guildResponse) (uint32, error) {
	envelopeTip, err := b.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("gate/路由服信封拒绝 message_id=%d tip=%d(请求未到达 guild 业务逻辑:tip=%d 多为路由服没有 guild 实例,"+
			"或 guild 返回了 gRPC 错误,如 data_service 不可达;其它常见为 gate 限流)", messageId, envelopeTip, tipGuildServiceUnavailable)
	}
	return response.GetErrorMessage().GetId(), nil
}

// expect 发请求并断言业务 tip 等于 want。
func (b *guildSmokeBot) expect(messageId uint32, request proto.Message, response guildResponse, want uint32) error {
	got, err := b.rpc(messageId, request, response)
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	if got == tipGuildHomeZoneUnknown {
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(data_service 里没有该角色的归属区映射;"+
			"老账号请跑 tools/merge_zone -backfill-home-zone,或换一个首次登录的 robot_ 账号)", messageId, b.account, got)
	}
	return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d", messageId, b.account, want, got)
}

// clearPushes 丢掉已经留底的推送。必须在**触发推送的那个动作之前**调用:
// 之后 waitPush 命中的就一定是这次动作引起的,而不是上一步遗留的同类推送
//(例如 M3 连批两人,两次都会给 D 推 MEMBER_JOINED)。
func (b *guildSmokeBot) clearPushes() {
	b.mu.Lock()
	b.pushes = nil
	b.mu.Unlock()
}

// waitPush 等一条 (kind, guildID) 的推送,命中即从留底里移除(同类推送可以按顺序等多条)。
// 超时返回 error,并把此刻留底的全部推送列进错误里 —— 排查时最想知道的就是
// "到底收到了什么":收到别的 kind 说明推送矩阵错了,一条都没收到才是投递链路的问题。
func (b *guildSmokeBot) waitPush(kind guildpb.GuildChangeKind, guildID uint64, timeout time.Duration) error {
	return b.waitPushMatching(fmt.Sprintf("%s(guild=%d)", kind, guildID), timeout,
		func(push *guildpb.GuildChangedS2C) bool {
			return push.GetKind() == kind && push.GetGuildId() == guildID
		})
}

// waitPushTo 在 waitPush 的条件上再要求 target_player_id == target,其余语义相同。
// 同道历练的邀请(活动段 S9)用它:邀请是逐人推的,target 是被邀请人自己,客户端靠这一位决定弹不弹邀请框;
// 只按 kind 等,会被同一个帮会里别的 ACTIVITY_CHANGED(target = 0,只表示"去拉活动页")顶替。
func (b *guildSmokeBot) waitPushTo(kind guildpb.GuildChangeKind, guildID, target uint64, timeout time.Duration) error {
	return b.waitPushMatching(fmt.Sprintf("%s(guild=%d target=%d)", kind, guildID, target), timeout,
		func(push *guildpb.GuildChangedS2C) bool {
			return push.GetKind() == kind && push.GetGuildId() == guildID && push.GetTargetPlayerId() == target
		})
}

// waitPushMatching 是 waitPush / waitPushTo 共用的等待循环:等第一条让 match 为真的推送,命中即从留底里移除。
// what 是"在等什么"的描述,只用于超时信息。match 在持有 b.mu 时被调用,只能读传进来的那条推送。
func (b *guildSmokeBot) waitPushMatching(what string, timeout time.Duration, match func(*guildpb.GuildChangedS2C) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		b.mu.Lock()
		for i, push := range b.pushes {
			if match(push) {
				b.pushes = append(b.pushes[:i], b.pushes[i+1:]...)
				b.mu.Unlock()
				return nil
			}
		}
		// 只在真要报错时才拼这行:轮询每 50ms 一次,顺手拼字符串等于白烧 100 次分配。
		timedOut := !time.Now().Before(deadline)
		pending := ""
		if timedOut {
			pending = guildSmokeDescribePushes(b.pushes)
		}
		b.mu.Unlock()

		if timedOut {
			return fmt.Errorf("account=%s 等 %s推送超时(%s),此刻留底的推送:[%s]",
				b.account, what, timeout, pending)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// myGuild 读自己的帮会;不在帮会里也算错误。
func (b *guildSmokeBot) myGuild() (*guildpb.GuildInfo, error) {
	resp := &guildpb.GetPlayerGuildResponse{}
	if err := b.expect(game.GuildServiceGetPlayerGuildMessageId, &guildpb.GetPlayerGuildRequest{}, resp, 0); err != nil {
		return nil, err
	}
	return resp.GetGuild(), nil
}

// cancelAllApplications 撤回本人全部待审申请,把机器人带回「没有任何待审申请」。
// 只撤刚刚列出来的那几条,所以每条都必须受理(tip=0):撤不掉说明列表与服务端状态对不上,
// 这时直接失败,而不是把错误吞掉继续跑 —— 后面第 6 步再被「待审数已满」拒绝会更难定位。
func (b *guildSmokeBot) cancelAllApplications() error {
	resp := &guildpb.ListMyGuildApplicationsResponse{}
	if err := b.expect(game.GuildServiceListMyGuildApplicationsMessageId,
		&guildpb.ListMyGuildApplicationsRequest{}, resp, 0); err != nil {
		return err
	}
	for _, application := range resp.GetApplications() {
		if err := b.expect(game.GuildServiceCancelGuildApplicationMessageId,
			&guildpb.CancelGuildApplicationRequest{GuildId: application.GetGuildId()},
			&guildpb.CancelGuildApplicationResponse{}, 0); err != nil {
			return fmt.Errorf("撤回遗留申请(帮会 %d): %w", application.GetGuildId(), err)
		}
	}
	return nil
}

// leaveAnyGuild 把机器人带回「不在任何帮会」:上一轮失败可能留下帮会或成员关系。
func (b *guildSmokeBot) leaveAnyGuild() error {
	for attempt := 0; attempt < 3; attempt++ {
		resp := &guildpb.GetPlayerGuildResponse{}
		tip, err := b.rpc(game.GuildServiceGetPlayerGuildMessageId, &guildpb.GetPlayerGuildRequest{}, resp)
		if err != nil {
			return err
		}
		switch {
		case tip == tipGuildNotInGuild:
			return nil
		case tip != 0:
			return fmt.Errorf("GetPlayerGuild tip=%d", tip)
		case resp.GetGuild().GetLeaderId() == b.gc.PlayerId:
			err = b.expect(game.GuildServiceDisbandGuildMessageId, &guildpb.DisbandGuildRequest{}, &guildpb.DisbandGuildResponse{}, 0)
		default:
			err = b.expect(game.GuildServiceLeaveGuildMessageId, &guildpb.LeaveGuildRequest{}, &guildpb.LeaveGuildResponse{}, 0)
		}
		if err != nil {
			return fmt.Errorf("leave leftover guild %d: %w", resp.GetGuild().GetGuildId(), err)
		}
	}
	return errors.New("连续 3 次退出 / 解散后仍在帮会里")
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// guildSmokeIsGuildMessage 列的是"请求 / 回包"成对的消息号,GuildService 的每个方法都要在这里。
// 漏一个的表现是:那条回包掉进通用分发,只在日志里留一句"未知消息号",而发请求的地方
// 一路等到超时才报错 —— 看起来像服务端没回,实际是回了没人认领。
// NotifyGuildChanged 不在这里 —— 它只有下行、不成对,由 onMessage 单独解码留底。
func guildSmokeIsGuildMessage(messageId uint32) bool {
	switch messageId {
	case game.GuildServiceCreateGuildMessageId,
		game.GuildServiceGetGuildMessageId,
		game.GuildServiceGetPlayerGuildMessageId,
		game.GuildServiceLeaveGuildMessageId,
		game.GuildServiceDisbandGuildMessageId,
		game.GuildServiceSetAnnouncementMessageId,
		game.GuildServiceSetGuildMemberRoleMessageId,
		game.GuildServiceKickGuildMemberMessageId,
		game.GuildServiceTransferGuildLeaderMessageId,
		game.GuildServiceApplyJoinGuildMessageId,
		game.GuildServiceCancelGuildApplicationMessageId,
		game.GuildServiceListMyGuildApplicationsMessageId,
		game.GuildServiceListGuildApplicationsMessageId,
		game.GuildServiceReviewGuildApplicationMessageId,
		game.GuildServiceUpdateGuildScoreMessageId,
		game.GuildServiceGetGuildRankMessageId,
		game.GuildServiceGetGuildRankByGuildMessageId,
		// 经济(B5):捐献 / 升级 / 商店。
		game.GuildServiceGetGuildDonateOptionsMessageId,
		game.GuildServiceDonateToGuildMessageId,
		game.GuildServiceUpgradeGuildMessageId,
		game.GuildServiceGetGuildShopMessageId,
		game.GuildServiceBuyGuildShopGoodsMessageId,
		// 活动(B6):五个号在 B6a 一次占齐;历练两个由活动段的 S9–S15 使用(guild 没配历练依赖时恒回 kGuildActivityNotOpen)。
		game.GuildServiceGetGuildActivitiesMessageId,
		game.GuildServiceLightGuildLanternMessageId,
		game.GuildServiceClaimGuildReunionMessageId,
		game.GuildServiceStartGuildTrialMessageId,
		game.GuildServiceRespondGuildTrialInviteMessageId:
		return true
	}
	return false
}

// guildSmokeHasApplication 判断申请人视角的列表里有没有指向该帮会的一条。
func guildSmokeHasApplication(applications []*guildpb.GuildApplicationView, guildID uint64) bool {
	for _, application := range applications {
		if application.GetGuildId() == guildID {
			return true
		}
	}
	return false
}

// guildSmokeHasApplicant 判断审批人视角的待审名单里有没有这个玩家。
func guildSmokeHasApplicant(applicants []*guildpb.GuildApplicantView, playerID uint64) bool {
	for _, applicant := range applicants {
		if applicant.GetPlayerId() == playerID {
			return true
		}
	}
	return false
}

// guildSmokeDescribePushes 把留底的推送压成一行,只用于失败信息。
// 调用方必须持有 b.mu(它直接读切片)。
func guildSmokeDescribePushes(pushes []*guildpb.GuildChangedS2C) string {
	if len(pushes) == 0 {
		return "空"
	}
	parts := make([]string, 0, len(pushes))
	for _, push := range pushes {
		parts = append(parts, fmt.Sprintf("%s(guild=%d target=%d)", push.GetKind(), push.GetGuildId(), push.GetTargetPlayerId()))
	}
	return strings.Join(parts, " ")
}

// guildSmokeCheckOfficer 断言快照里 target 是长老,且长老数与长老位上限都对得上。
// 三条一起查:只查 role 的话,"服务端把 officer_count 算错"这种会让客户端任免按钮
// 灰错的缺陷完全看不出来。
func guildSmokeCheckOfficer(guild *guildpb.GuildInfo, target uint64) error {
	role, ok := guildSmokeRoleOf(guild, target)
	if !ok || role != guildSmokeRoleOfficer {
		return fmt.Errorf("快照里 %d 的 role=%d(在册=%v),期望长老(%d)", target, role, ok, guildSmokeRoleOfficer)
	}
	if guild.GetOfficerCount() != 1 {
		return fmt.Errorf("officer_count=%d,期望 1", guild.GetOfficerCount())
	}
	if guild.GetMaxOfficers() != guildSmokeLevel1MaxOfficers {
		return fmt.Errorf("max_officers=%d,期望 %d(GuildLevel 第 1 级)", guild.GetMaxOfficers(), guildSmokeLevel1MaxOfficers)
	}
	return nil
}

// guildSmokeCheckLeadership 断言转让之后:newLeader 既是 leader_id 也在成员表里是帮主
//(双存储必须一致,只查一边等于没查),旧帮主 demoted 降为长老。
// 降为长老而不是成员,是因为转让时长老位还有余量(第 1 级 2 个,受让人腾出了自己那个)。
func guildSmokeCheckLeadership(guild *guildpb.GuildInfo, newLeader, demoted uint64) error {
	if guild.GetLeaderId() != newLeader {
		return fmt.Errorf("leader_id=%d,期望 %d", guild.GetLeaderId(), newLeader)
	}
	if role, ok := guildSmokeRoleOf(guild, newLeader); !ok || role != guildSmokeRoleLeader {
		return fmt.Errorf("新帮主 %d 在册 role=%d(在册=%v),期望 %d", newLeader, role, ok, guildSmokeRoleLeader)
	}
	if role, ok := guildSmokeRoleOf(guild, demoted); !ok || role != guildSmokeRoleOfficer {
		return fmt.Errorf("原帮主 %d 在册 role=%d(在册=%v),期望降为长老(%d)", demoted, role, ok, guildSmokeRoleOfficer)
	}
	return nil
}

func guildSmokeRoleOf(guild *guildpb.GuildInfo, playerID uint64) (uint32, bool) {
	for _, member := range guild.GetMembers() {
		if member.GetPlayerId() == playerID {
			return member.GetRole(), true
		}
	}
	return 0, false
}

// guildSmokeNonce 返回 8 个十六进制字符:帮名全局唯一且解散后才释放,每次运行必须用新名字。
func guildSmokeNonce() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%0xffffffff, 16)
	}
	return hex.EncodeToString(buf)
}
