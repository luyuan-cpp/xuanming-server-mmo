package main

// guild-smoke 场景:三个机器人经 gate → client_rpc_router 对 go/guild 做「按 zone 隔离」的端到端冒烟
// (docs/design/guild-zone-client-access.md §6):
//
//	0. 预备:三人各自撤回遗留的待审申请,再 GetPlayerGuild,还在帮会里就解散(帮主)或退出(成员)—— 同账号可反复跑;
//	1. A 建帮,请求体 zone_id 故意填错 → 受理,且帮会 zone_id == zone_a(zone 由服务端按归属映射决定);
//	2. A 再建一个 → kGuildAlreadyInGuild;
//	3. B(同区)查该帮在本区榜上的名次 → 上榜;
//	4. C(zone_b,cross_zone=true 时):GetGuild → 不存在;查名次(请求体伪造 zone_a)→ 未上榜;
//	   本区榜里没有它;申请入帮 → 不存在;用同一个帮名建帮 → kGuildNameTaken(帮名全局唯一);
//	5. B 未入帮时改公告 → kGuildNoPermission;
//	6. B 申请入帮 → 受理,B 与 A 各自列得到这条申请;A 审批通过 → 响应快照里 B 在册,B 的 GetPlayerGuild 看到自己是成员;
//	7. A 改公告 → B 读到;700 字节公告 → kGuildAnnouncementTooLong(guild 侧拒绝,整包仍 < gate 1KB);
//	8. 身份伪造:B 发 LeaveGuild 但请求体 player_id 填 A → 服务端按会话身份让 B 退出,A 仍是帮主;
//	9. 内部方法:A 发 UpdateGuildScore → 收到 kServiceUnavailable 信封拒绝,随后查榜成功且积分仍是 0;
//	   确切拒绝原因需结合路由服 PermissionDenied 日志与 session 单测确认(信封不保留 gRPC code);
//	10. A 解散 → 自己不在帮会,帮会从本区榜上消失。
//
// 结果约定(供外层脚本消费):
//	全过 → 日志一行 `GUILD_SMOKE_OK guild_id=… zone_a=… player_a=… player_b=… player_c=…`,退出码 0;
//	任一步失败 → `GUILD_SMOKE_FAIL step=… reason=…`,退出码 1。

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
// 必须是**首次在各自 zone 建角**的账号:A/B 的归属区要是 zone_a、C 的要是 zone_b,第 1 步才能断言帮会落区。
const (
	guildSmokeAccountA = "robot_9201" // 帮主
	guildSmokeAccountB = "robot_9202" // 同区成员
	guildSmokeAccountC = "robot_9203" // 别区玩家(cross_zone=true 才登录)
)

// 单次公会 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s),先看到路由服的信封拒绝而不是本地超时。
const guildSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const guildSmokeSceneReadyTimeout = 15 * time.Second

// 相邻请求的最小间隔。公会消息号在 MessageLimiter 表里是 5~10 次 / 秒,UpdateGuildScore 不在表里吃默认 3 次 / 窗口;
// 冒烟不追求速度,300ms 远离两档上限。
// 申请制的 8 个新消息号要到 B2c 才进 MessageLimiter 表,在那之前同样吃默认档(3 次 / 秒,按消息号各算一份)。
// 本场景对同一个新消息号最多连发 3 次(撤回遗留申请,GuildRule 每人 ≤3 个待审),跨度 600ms 恰好压在默认档上限上;
// 再加一次就会被 gate 限流,所以别在 B2s 阶段往同号上加请求。
const guildSmokeRequestSpacing = 300 * time.Millisecond

// 超长公告:> guild MaxAnnouncementBytes(600),又让整个 ClientRequest < gate 的 1KB,拒绝才来自 guild 而不是 gate。
const guildSmokeOversizeAnnouncementBytes = 700

// 查本区榜的页长,等于 guild 对客户端的上限 MaxRankPageSize。
const guildSmokeRankPageSize uint32 = 50

// 成员身份编码是持久化契约(go/guild/internal/constants:0=member / 1=officer / 3=leader)。
const (
	guildSmokeRoleMember uint32 = 0
	guildSmokeRoleLeader uint32 = 3
)

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

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu       sync.Mutex
	replies  map[uint32]*base.MessageContent // 公会消息号 → 本次请求发出后收到的第一个回包
	gateTips []uint32                        // 本次请求期间收到的 SendTipToClient

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
	specs := []loginSpec{{guildSmokeAccountA, sc.ZoneA}, {guildSmokeAccountB, sc.ZoneA}}
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
	var c *guildSmokeBot
	if sc.CrossZone {
		c = loggedIn[2]
		// 本地各 zone 的 gate 端口不同;地址相同说明 C 其实进了 zone_a,后面"别区不可见"的结论就不成立。
		if c.gate == a.gate {
			fail("zone-placement", "C 与 A 分到同一个 gate %s(zone_a=%d zone_b=%d),不是跨 zone 运行", a.gate, sc.ZoneA, sc.ZoneB)
		}
	}
	zap.L().Info("[guild-smoke] robots in scene",
		zap.Uint64("a", a.gc.PlayerId), zap.Uint64("b", b.gc.PlayerId), zap.Bool("cross_zone", sc.CrossZone))

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
		fail("create", "建帮受理但响应里没有帮会")
	}
	if guild.GetZoneId() != sc.ZoneA {
		fail("create-zone", "帮会 zone_id=%d,期望 A 的归属区 %d(服务端必须忽略请求体 zone_id=%d)",
			guild.GetZoneId(), sc.ZoneA, spoofedZone)
	}
	if role, ok := guildSmokeRoleOf(guild, a.gc.PlayerId); guild.GetLeaderId() != a.gc.PlayerId || !ok || role != guildSmokeRoleLeader {
		fail("create-leader", "leader_id=%d A=%d A 的 role=%d(在册=%v),期望 A 是帮主", guild.GetLeaderId(), a.gc.PlayerId, role, ok)
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
		fail("rank-same-zone", "本区榜条目 rank=%d name=%q,期望上榜且名为 %q",
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
				fail("other-zone-rank-page", "zone %d 的玩家在榜单里看到了 zone %d 的帮会 %d", sc.ZoneB, sc.ZoneA, guildID)
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
		fail("apply-list-mine", "B 的待审申请里没有帮会 %d(共 %d 条)", guildID, len(myApplications.GetApplications()))
	}
	applicants := &guildpb.ListGuildApplicationsResponse{}
	if err := a.expect(game.GuildServiceListGuildApplicationsMessageId,
		&guildpb.ListGuildApplicationsRequest{}, applicants, 0); err != nil {
		fail("apply-list-applicants", "%v", err)
	}
	if !guildSmokeHasApplicant(applicants.GetApplicants(), b.gc.PlayerId) {
		fail("apply-list-applicants", "A 的待审名单里没有 B=%d(共 %d 条)", b.gc.PlayerId, len(applicants.GetApplicants()))
	}
	reviewed := &guildpb.ReviewGuildApplicationResponse{}
	if err := a.expect(game.GuildServiceReviewGuildApplicationMessageId,
		&guildpb.ReviewGuildApplicationRequest{ApplicantPlayerId: b.gc.PlayerId, Approve: true}, reviewed, 0); err != nil {
		fail("review", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(reviewed.GetGuild(), b.gc.PlayerId); !ok || role != guildSmokeRoleMember {
		fail("review", "审批响应的快照里 B=%d role=%d(在册=%v),期望已是普通成员", b.gc.PlayerId, role, ok)
	}
	bGuild, err := b.myGuild()
	if err != nil {
		fail("join-verify", "%v", err)
	}
	if role, ok := guildSmokeRoleOf(bGuild, b.gc.PlayerId); bGuild.GetGuildId() != guildID || !ok || role != guildSmokeRoleMember {
		fail("join-verify", "B 的帮会=%d role=%d(在册=%v),期望在帮会 %d 且为成员", bGuild.GetGuildId(), role, ok, guildID)
	}
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
		fail("announcement-read", "B 读到公告 %q,期望 %q", bGuild.GetAnnouncement(), announcement)
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
		fail("spoof-leave", "服务端按请求体里伪造的 player_id=%d(帮主 A)处理了 B 的退帮请求:身份没有取会话", a.gc.PlayerId)
	}
	if leaveTip != 0 {
		fail("spoof-leave", "B 退帮应受理(tip=0),实得 tip=%d", leaveTip)
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
		fail("spoof-leave-verify-a", "A 的帮会 leader=%d 成员数=%d,期望 A 仍是帮主且只剩 1 人", aGuild.GetLeaderId(), len(aGuild.GetMembers()))
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
		fail("internal-method", "UpdateGuildScore 期望信封 tip=%d,实得 tip=%d(0 表示拿到了业务回包)",
			tipGuildServiceUnavailable, scoreEnvelopeTip)
	}
	scoreAfter := &guildpb.GetGuildRankByGuildResponse{}
	if err := a.expect(game.GuildServiceGetGuildRankByGuildMessageId,
		&guildpb.GetGuildRankByGuildRequest{GuildId: guildID}, scoreAfter, 0); err != nil {
		fail("internal-method-verify", "%v", err)
	}
	if scoreAfter.GetEntry().GetScore() != 0 {
		fail("internal-method-verify", "客户端改动了帮会积分:score=%d,期望 0", scoreAfter.GetEntry().GetScore())
	}
	// 不扩展线协议就不能区分 PermissionDenied 与其它上游故障:这里只声明已观察到的拒绝
	// 与积分不变,验收时还须核对路由服该方法的 code=PermissionDenied 日志与 session 单测。
	zap.L().Info("[guild-smoke] step 9: 收到信封拒绝,随后查榜成功且积分未变;白名单拒绝原因需核对路由服 code=PermissionDenied 日志与 session 单测",
		zap.Uint32("envelope_tip", scoreEnvelopeTip),
		zap.String("method", guildpb.GuildService_UpdateGuildScore_FullMethodName))

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

	playerC := uint64(0)
	if c != nil {
		playerC = c.gc.PlayerId
	}
	zap.L().Info(fmt.Sprintf("GUILD_SMOKE_OK guild_id=%d zone_a=%d player_a=%d player_b=%d player_c=%d",
		guildID, sc.ZoneA, a.gc.PlayerId, b.gc.PlayerId, playerC), zap.Bool("cross_zone", sc.CrossZone))
	cleanup()
	_ = zap.L().Sync()
}

// guildSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调。
// 与 chatSmokeLogin 同形:公会回包由本场景按消息号认领,不交给通用分发(生成的分发表不含 GuildService)。
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
		replies: make(map[uint32]*base.MessageContent),
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

// onMessage:公会消息号的回包(含信封拒绝)由本场景认领;gate 的 SendTipToClient 记下来供快速失败;其余交给通用分发。
func (b *guildSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	// NotifyGuildChanged 是服务端主动下行,不是任何请求的回包:放进 replies 会被下一次同号等待
	// 误当成结果,交给通用分发又只会得到一条"未知消息号"。B2s 只要求原冒烟跑通,推送的收集与
	// 断言属于 B2c,这里直接丢弃。
	if msg.GetMessageId() == game.GuildServiceNotifyGuildChangedMessageId {
		return
	}
	if guildSmokeIsGuildMessage(msg.GetMessageId()) {
		b.mu.Lock()
		if _, seen := b.replies[msg.GetMessageId()]; !seen {
			b.replies[msg.GetMessageId()] = msg
		}
		b.mu.Unlock()
		return
	}
	if msg.GetMessageId() == game.SceneClientPlayerCommonSendTipToClientMessageId {
		var tipInfo base.TipInfoMessage
		if err := proto.Unmarshal(msg.GetSerializedMessage(), &tipInfo); err == nil && tipInfo.GetId() != 0 {
			b.mu.Lock()
			b.gateTips = append(b.gateTips, tipInfo.GetId())
			b.mu.Unlock()
		}
	}
	handler.MessageBodyHandler(client, msg)
}

// call 发一个公会请求并等同消息号的第一个回包。
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable 或超时返回 error。
func (b *guildSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	if wait := guildSmokeRequestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	b.mu.Lock()
	delete(b.replies, messageId)
	b.gateTips = nil
	b.mu.Unlock()

	if err := b.gc.SendRequest(messageId, request); err != nil {
		return 0, fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	b.lastRequestAt = time.Now()
	b.stats.MsgSent()

	deadline := time.Now().Add(guildSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		reply := b.replies[messageId]
		gateUnavailable := false
		for _, tip := range b.gateTips {
			gateUnavailable = gateUnavailable || tip == tipGuildServiceUnavailable
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
				tipGuildServiceUnavailable)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("message_id=%d account=%s %w(%s)", messageId, b.account, errGuildSmokeTimeout, guildSmokeRpcTimeout)
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

// guildSmokeIsGuildMessage 列的是"请求 / 回包"成对的消息号。
// 管理段的三个号(SetGuildMemberRole / KickGuildMember / TransferGuildLeader)本场景还没用到,
// 一并列全:少一个就会让 B2c 的回包掉进通用分发,查起来很费劲。
// NotifyGuildChanged 不在这里 —— 它只有下行,由 onMessage 单独丢弃。
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
		game.GuildServiceGetGuildRankByGuildMessageId:
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
