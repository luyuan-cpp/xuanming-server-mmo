// 2026-10-19 主干适配：M2/M3 serviceauth + MailView/limit + MailChangedEvent。
// 使用 admin 生产者密钥，附件必须通过 scene 资产通道完成，不再把 M1 领取短路当成功。
package main

// mail-smoke 场景:两个机器人经 gate → client_rpc_router 对 go/mail 做端到端冒烟
// (设计 docs/design/mail-system.md §10 ⑦;前置条件写在 etc/mail_smoke.yaml 文件头)。
//
//	0. 收件人 R 登 zone_b、旁观者 O 登 zone_a(cross_zone=false 时 R 也登 zone_a);跨区时断言两人 gate 地址不同;
//	1. gRPC 直连 MailAdmin(不带会话 metadata;metadata 带 x-mail-admin-token / x-operator)发三封:
//	   系统邮件 S、给 R 的无附件定向邮件 P、给 R 的带 1 条货币附件的定向邮件 Q(dev 档才允许附件);
//	   每封用新的 request_id,expire_ms = now + 15min;S 用同一个请求再发一次,必须 duplicated=true 且 id 相同;
//	2. R 在 push_timeout_ms 内收到 NotifyMailEvent{NEW_MAIL}——**弱断言**,只打印 push_received,不算失败
//	   (推送是 at-most-once,红点真相是 ListMails 的 unread_count);
//	3. R、O 都能在 ListMails 里翻到 S(按 before_mail_id / has_more 翻页,不假定在第一页);
//	   只有 R 看到 P、Q;Q 的 claim_state = UNCLAIMED,且响应 claimable_count ≥ 1;
//	4. R ReadMail(P):P 自己的已读位 false → true;unread_count 按两次列表之间的过期 / 新到逐封核账后恰好少 1
//	   (mailSmokeCheckUnreadAfterRead);O ReadMail(P) 回 MailNotFound;
//	5. R ClaimMailAttachments([Q]) 回 MailClaimNotOpen,再 ListMails 那封仍是 UNCLAIMED;
//	6. R DeleteMails([Q]):deleted_count = 0、Q 在 protected_mail_ids;DeleteMails([P]) 两次都受理,之后列表里看不到 P;
//	   R DeleteMails([S]):deleted_count = 1(系统邮件按人删),之后 R 看不到 S、O 仍看得到;
//	7. D-9 反向断言:O 经 gate 发 NotifyMailChanged 的消息号 → 必须**超时**(推送号不在 gate 上行白名单里,
//	   client-rpc-callback.md §7 R4);两个 MailAdmin 消息号经 gate 各发一次 → 同样必须**超时**(gate 丢弃非客户端消息号)。
//
// 结果约定(供外层脚本消费):
//	全过 → 日志一行 `MAIL_SMOKE_OK recipient=… observer=… sys_mail_id=… …`,退出码 0;
//	任一步失败 → `MAIL_SMOKE_FAIL step=… reason=…`,退出码 1。
//
// 可重复运行:邮件不可撤回,所以不做预清理;每轮新的 request_id,15 分钟后全部过期(Q 删不掉,只能等它过期)。
// R 每轮删掉自己视图里的 S,邮箱只累积 Q 一封,连跑时第 4 步的未读核账一直是单页精确等式。
//
// ⚠ 消息号常量名(game.ClientPlayerMail*MessageId / game.MailAdmin*MessageId)按 robot/generated/pb/game/message_id.go 的
// 规律(`<服务名><方法名>MessageId`)书写,与 go/mail/internal/logic/push.go 用的 game.ClientPlayerMailNotifyMailChangedMessageId
// 同源;proto-gen 之后若对不上,改这里,别改 proto。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"shared/serviceauth"

	"proto/common/base"
	mailpb "proto/mail"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。98xx 段里只有 9801 被 channel-smoke 用了;
// 97xx 是 friend-smoke、99xx 是 ordering / merchant、95xx 整段划给帮会资产通道。
//
// 归属区要求:R 必须**首次在 zone_b 建角**、O 首次在 zone_a 建角(归属区在建角时登记,之后不随登录区变化)。
// R 的推送要落在 zone_b 的 gate 上才证明「mail 跨区推得到」。
const (
	mailSmokeAccountRecipient = "robot_9811" // 收件人:收定向邮件 / 推送 / 读 / 删 / 领取
	mailSmokeAccountObserver  = "robot_9812" // 旁观者:只看到系统邮件、读别人的信被拒、发 S2C 号做反向断言
)

// 单次邮件 RPC 的等待预算:大于路由服 ForwardTimeoutMs(5s)与 mail zrpc Timeout(4000ms),先看到路由服的信封拒绝而不是本地超时。
// 第 7 步的反向断言也按这个预算判"超时"。
const mailSmokeRpcTimeout = 10 * time.Second

// 等进场的预算,与 prepareBehaviorClient 同值。
const mailSmokeSceneReadyTimeout = 15 * time.Second

// 等一条 S2C 推送的兜底预算(mail_smoke.push_timeout_ms 未配时用它)。
const mailSmokeDefaultPushTimeout = 10 * time.Second

// 直连 MailAdmin 的单次调用预算:mail 的 zrpc Timeout 是 4000ms,留 1s 余量。
const mailSmokeAdminTimeout = 5 * time.Second

// 本轮发出的信的有效期:足够跑完一轮,又让连跑的数据尽快过期(有效系统邮件 / 邮箱上限都只数未过期的)。
const mailSmokeExpireAfter = 15 * time.Minute

// ListMails 翻页:每页取服务端上限 50,最多翻 10 页(邮箱上限与有效系统邮件上限默认都是 100,10 页足够覆盖)。
const (
	mailSmokePageSize uint32 = 50
	mailSmokeMaxPages        = 10
)

// 带附件那封的货币类型与数量:0 = 金币,在 go/mail/etc/mail.yaml 的 Mail.AllowedCurrencyTypes 白名单里。
const (
	mailSmokeCurrencyType   uint32 = 0
	mailSmokeCurrencyAmount uint64 = 1
)

// admin 生产者密钥只用于 serviceauth.Sign；环境变量名与 mail.yaml Producers.admin 对应。
const (
	mailSmokeAdminTokenEnv      = "MAIL_PRODUCER_SECRET_ADMIN"
	mailSmokeMinAdminTokenBytes = 32
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipMailNotFound           = uint32(tiptable.MailError_kMailNotFound)
	tipMailRecipientBoxFull   = uint32(tiptable.MailError_kMailRecipientBoxFull)
	tipMailAttachmentNotOpen  = uint32(tiptable.MailError_kMailAttachmentInvalid)
	tipMailRequestConflict    = uint32(tiptable.MailError_kMailRequestConflict)
	tipMailInvalidParameter   = uint32(tiptable.CommonError_kInvalidParameter)
	tipMailRateLimited        = uint32(tiptable.CommonError_kRateLimitExceeded)
	tipMailServiceUnavailable = uint32(tiptable.CommonError_kServiceUnavailable)
)

var errMailSmokeTimeout = errors.New("等回包超时")

// mailResponse 是所有邮件客户端响应的共同形状:业务拒绝码在响应体 error_message 里。
type mailResponse interface {
	proto.Message
	GetErrorMessage() *base.TipInfoMessage
}

// mailSmokeBot 是一个已登录进场的机器人会话(形状照 friendSmokeBot)。
type mailSmokeBot struct {
	account string
	zone    uint32
	gc      *pkg.GameClient
	player  *gameobject.Player
	stats   *metrics.Stats

	// 相邻请求的最小间隔(mail_smoke.request_interval_ms),理由同 friend-smoke。
	requestSpacing time.Duration
	// pushTimeout 是等一条 S2C 邮件事件的预算(mail_smoke.push_timeout_ms)。
	pushTimeout time.Duration

	// caller 发请求并只等它自己的回包(按请求号认领,见 smoke_rpc_call.go)。
	caller *smokeCaller

	// RecvLoop goroutine 写、场景主流程读,mu 保护。
	mu sync.Mutex
	// pushes:收到的 S2C 邮件事件,按到达顺序。断言前用 resetPushes 清空。
	pushes []*mailpb.MailChangedEvent
	// uncorrelated:按消息号累计 onMessage 丢弃的「没回显请求号的回包」。在 Call 看来它与「gate 丢弃」一样都是超时,
	// 第 7 步的反向断言靠这个计数把两者分开(改造前按消息号认领,这类回包会被当成本次结果判失败)。
	uncorrelated map[uint32]int

	lastRequestAt time.Time // 仅主流程读写
}

// RunMailSmoke 是 main.go `mode: mail-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunMailSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.MailSmoke

	var bots []*mailSmokeBot
	var adminConn *grpc.ClientConn
	cleanup := func() {
		for _, bot := range bots {
			_ = leaveGame(bot.gc, stats)
			sendDisconnectBestEffort(bot.gc)
			gameobject.PlayerList.Delete(bot.gc.PlayerId)
			bot.gc.Close()
		}
		if adminConn != nil {
			_ = adminConn.Close()
		}
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("MAIL_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// 与服务端 admin 生产者使用相同的分用途签名密钥；不记录密钥值。
	token := strings.TrimSpace(os.Getenv(mailSmokeAdminTokenEnv))
	if len([]byte(token)) < mailSmokeMinAdminTokenBytes {
		fail("admin-secret", "%s must contain the same >=32-byte producer secret as the mail service", mailSmokeAdminTokenEnv)
	}

	spacing := time.Duration(sc.RequestIntervalMs) * time.Millisecond
	pushTimeout := time.Duration(sc.PushTimeoutMs) * time.Millisecond
	if pushTimeout <= 0 {
		// config.validate 已拦 <=0;这里兜底,避免 waitPush 变成"立刻超时"。
		pushTimeout = mailSmokeDefaultPushTimeout
	}
	zoneR := sc.ZoneA
	if sc.CrossZone {
		zoneR = sc.ZoneB
	}

	// ---- 步骤 0:登录(并发) ----
	type loginSpec struct {
		account string
		zone    uint32
	}
	specs := []loginSpec{{mailSmokeAccountRecipient, zoneR}, {mailSmokeAccountObserver, sc.ZoneA}}
	loggedIn := make([]*mailSmokeBot, len(specs))
	loginErrs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, spec loginSpec) {
			defer wg.Done()
			zoneCfg := *cfg
			zoneCfg.ZoneID = spec.zone
			loggedIn[idx], loginErrs[idx] = mailSmokeLogin(&zoneCfg, spec.account, spacing, pushTimeout, stats)
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
	r, o := loggedIn[0], loggedIn[1]
	// 取**当前实际连着**的 gate,而不是 AssignGate 分到的地址:login 开了 HomeZone.RedirectOnEnterEnabled 时,
	// 归属区不是 zone_b 的 R 会被重定向回归属区的 gate(pkg.FollowRedirect 换连接),分配地址就不再代表 R 在哪。
	if gateR, gateO := r.gateAddr(), o.gateAddr(); sc.CrossZone && gateR == gateO {
		// 本地各 zone 的 gate 端口不同;地址相同说明 R 其实进了 zone_a,推送就只是同一个 gate 内部的巧合。
		fail("zone-placement", "recipient and observer are connected to the same gate %s (zone_a=%d zone_b=%d); this is not a cross-zone run",
			gateR, sc.ZoneA, sc.ZoneB)
	}
	zap.L().Info("[mail-smoke] robots in scene",
		zap.Uint64("recipient", r.gc.PlayerId), zap.Uint64("observer", o.gc.PlayerId),
		zap.String("gate_recipient", r.gateAddr()), zap.String("gate_observer", o.gateAddr()), zap.Bool("cross_zone", sc.CrossZone))

	// ---- 步骤 1:直连 MailAdmin 发三封 + 同键重放 ----
	var err error
	adminConn, err = grpc.NewClient(sc.AdminAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("admin-dial", "admin_addr=%s: %v", sc.AdminAddr, err)
	}
	admin := mailpb.NewMailAdminClient(adminConn)

	nonce := mailSmokeNonce()
	expireMs := uint64(time.Now().Add(mailSmokeExpireAfter).UnixMilli())
	requestBase := uint64(time.Now().UnixNano())
	sysReq := &mailpb.SendMailRequest{
		SenderKind: 1,
		RequestId:  requestBase,

		Title:    "smoke sys " + nonce,
		Body:     "mail-smoke system mail " + nonce,
		ExpireMs: expireMs,
	}
	plainReq := &mailpb.SendMailRequest{
		SenderKind:        1,
		RequestId:         requestBase + 1,
		RecipientPlayerId: r.gc.PlayerId,

		Title:    "smoke plain " + nonce,
		Body:     "mail-smoke personal mail " + nonce,
		ExpireMs: expireMs,
	}
	attachReq := &mailpb.SendMailRequest{
		SenderKind:        1,
		RequestId:         requestBase + 2,
		RecipientPlayerId: r.gc.PlayerId,

		Title: "smoke attach " + nonce,
		Body:  "mail-smoke attachment mail " + nonce,
		Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{
			{CurrencyType: mailSmokeCurrencyType, Amount: mailSmokeCurrencyAmount},
		}},
		ExpireMs: expireMs,
	}

	r.resetPushes()
	sysID, dup, err := mailSmokeSendSystem(admin, token, sysReq)
	if err != nil {
		fail("send-system", "%v", err)
	}
	if dup {
		fail("send-system", "first send of request_id=%d reported duplicated=true (request ids must be fresh per run)", sysReq.GetRequestId())
	}
	plainID, dup, err := mailSmokeSendPersonal(admin, token, plainReq)
	if err != nil {
		fail("send-personal", "%v", err)
	}
	if dup {
		fail("send-personal", "first send of request_id=%d reported duplicated=true", plainReq.GetRequestId())
	}
	attachID, dup, err := mailSmokeSendPersonal(admin, token, attachReq)
	if err != nil {
		fail("send-attachment", "%v", err)
	}
	if dup {
		fail("send-attachment", "first send of request_id=%d reported duplicated=true", attachReq.GetRequestId())
	}
	// 同键同内容重放:必须返回首次的 id 并标 duplicated(请求对象原样复用,expire_ms 不重算)。
	replayID, replayDup, err := mailSmokeSendSystem(admin, token, sysReq)
	if err != nil {
		fail("send-system-replay", "%v", err)
	}
	if !replayDup || replayID != sysID {
		fail("send-system-replay", "replay of request_id=%d returned sys_mail_id=%d duplicated=%v, expected sys_mail_id=%d duplicated=true",
			sysReq.GetRequestId(), replayID, replayDup, sysID)
	}
	zap.L().Info("[mail-smoke] step 1: sent via MailAdmin",
		zap.String("nonce", nonce), zap.Uint64("sys_mail_id", sysID),
		zap.Uint64("plain_mail_id", plainID), zap.Uint64("attach_mail_id", attachID))

	// ---- 步骤 2:新信推送(弱断言) ----
	pushReceived := r.waitPush(plainID)
	zap.L().Info("[mail-smoke] step 2: new-mail push", zap.Bool("push_received", pushReceived),
		zap.Duration("waited_up_to", r.pushTimeout))

	// ---- 步骤 3:两区都看到系统邮件,只有 R 看到定向邮件 ----
	rList, err := r.listAll()
	if err != nil {
		fail("list-recipient", "%v", err)
	}
	if mailSmokeFind(rList.mails, sysID) == nil {
		fail("list-recipient", "recipient cannot see system mail %d within %d pages", sysID, mailSmokeMaxPages)
	}
	if mailSmokeFind(rList.mails, plainID) == nil {
		fail("list-recipient", "recipient cannot see its personal mail %d", plainID)
	}
	attachSummary := mailSmokeFind(rList.mails, attachID)
	if attachSummary == nil {
		fail("list-recipient", "recipient cannot see its attachment mail %d", attachID)
	}
	if attachSummary.GetClaimState() != mailpb.MailClaimState_MAIL_CLAIM_UNCLAIMED {
		fail("list-recipient", "attachment mail %d claim_state=%s, expected UNCLAIMED", attachID, attachSummary.GetClaimState())
	}
	if rList.claimable == 0 {
		fail("list-recipient", "claimable_count=0 although attachment mail %d is UNCLAIMED", attachID)
	}
	oList, err := o.listAll()
	if err != nil {
		fail("list-observer", "%v", err)
	}
	if mailSmokeFind(oList.mails, sysID) == nil {
		fail("list-observer", "observer (zone %d) cannot see system mail %d within %d pages: system mail must be visible to every zone", sc.ZoneA, sysID, mailSmokeMaxPages)
	}
	if mailSmokeFind(oList.mails, plainID) != nil || mailSmokeFind(oList.mails, attachID) != nil {
		fail("list-observer", "observer can see the recipient's personal mail (%d / %d)", plainID, attachID)
	}
	zap.L().Info("[mail-smoke] step 3: listing visibility ok",
		zap.Uint32("recipient_unread", rList.unread), zap.Uint32("recipient_claimable", rList.claimable))

	// ---- 步骤 4:读信减未读;别人读不到 ----
	// 先断言 P 自己的已读位(不依赖全局计数):request_id 每轮新发,第 3 步的列表里它必须是未读。
	if mailSmokeFind(rList.mails, plainID).GetIsRead() {
		fail("read-recipient", "personal mail %d is already read before ReadMail", plainID)
	}
	readResp := &mailpb.ReadMailResponse{}
	if err := r.expect(game.ClientPlayerMailReadMailMessageId, &mailpb.ReadMailRequest{MailId: plainID}, readResp, 0); err != nil {
		fail("read-recipient", "%v", err)
	}
	if !readResp.GetMail().GetIsRead() || readResp.GetMail().GetBody() != plainReq.GetBody() {
		fail("read-recipient", "ReadMail(%d) returned read=%v body_match=%v", plainID, readResp.GetMail().GetIsRead(), readResp.GetMail().GetBody() == plainReq.GetBody())
	}
	rAfterRead, err := r.listAll()
	if err != nil {
		fail("read-recipient", "%v", err)
	}
	if s := mailSmokeFind(rAfterRead.mails, plainID); s == nil || !s.GetIsRead() {
		fail("read-recipient", "personal mail %d is not listed as read after ReadMail (listed=%v)", plainID, s != nil)
	}
	unreadExact, err := mailSmokeCheckUnreadAfterRead(rList, rAfterRead, plainID)
	if err != nil {
		fail("read-recipient", "%v", err)
	}
	if err := o.expect(game.ClientPlayerMailReadMailMessageId, &mailpb.ReadMailRequest{MailId: plainID}, &mailpb.ReadMailResponse{}, tipMailNotFound); err != nil {
		fail("read-observer", "%v", err)
	}
	zap.L().Info("[mail-smoke] step 4: read ok, observer refused",
		zap.Uint32("unread_before", rList.unread), zap.Uint32("unread_after", rAfterRead.unread), zap.Bool("unread_check_exact", unreadExact))

	// ---- 步骤 5：未领取保护与真实资产到账 ----
	protected := &mailpb.DeleteMailsResponse{}
	if err := r.expect(game.ClientPlayerMailDeleteMailsMessageId, &mailpb.DeleteMailsRequest{MailIds: []uint64{attachID}}, protected, 0); err != nil {
		fail("protect-before-claim", "%v", err)
	}
	if !mailSmokeContains(protected.GetProtectedMailIds(), attachID) {
		fail("protect-before-claim", "unclaimed attachment was not protected")
	}
	claim := &mailpb.ClaimMailAttachmentsResponse{}
	if err := r.expect(game.ClientPlayerMailClaimMailAttachmentsMessageId, &mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{attachID}}, claim, 0); err != nil {
		fail("claim", "%v", err)
	}
	if len(claim.GetResults()) != 1 || claim.GetResults()[0].GetAssetOpId() == 0 {
		fail("claim", "claim did not persist exactly one asset operation")
	}
	opID := claim.GetResults()[0].GetAssetOpId()
	claimDeadline := time.Now().Add(30 * time.Second)
	for {
		listed, err := r.listAll()
		if err != nil {
			fail("claim-poll", "%v", err)
		}
		view := mailSmokeFind(listed.mails, attachID)
		if view != nil && view.GetClaimState() == mailpb.MailClaimState_MAIL_CLAIM_CLAIMED {
			break
		}
		if view == nil || view.GetClaimState() == mailpb.MailClaimState_MAIL_CLAIM_MANUAL || !time.Now().Before(claimDeadline) {
			fail("claim-poll", "asset operation %d did not reach CLAIMED", opID)
		}
		time.Sleep(spacing)
	}
	replayClaim := &mailpb.ClaimMailAttachmentsResponse{}
	if err := r.expect(game.ClientPlayerMailClaimMailAttachmentsMessageId, &mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{attachID}}, replayClaim, 0); err != nil {
		fail("claim-replay", "%v", err)
	}
	if len(replayClaim.GetResults()) != 1 || replayClaim.GetResults()[0].GetAssetOpId() != opID {
		fail("claim-replay", "claim replay changed the asset operation")
	}

	// ---- 步骤 6:带附件的删不掉;无附件的可重复删 ----
	delAttach := &mailpb.DeleteMailsResponse{}
	if err := r.expect(game.ClientPlayerMailDeleteMailsMessageId, &mailpb.DeleteMailsRequest{MailIds: []uint64{attachID}}, delAttach, 0); err != nil {
		fail("delete-protected", "%v", err)
	}
	if !mailSmokeContains(delAttach.GetDeletedMailIds(), attachID) || mailSmokeContains(delAttach.GetProtectedMailIds(), attachID) {
		fail("delete-protected", "DeleteMails([%d]) deleted_count=%d protected=%v, expected claimed attachment to be deletable after the earlier protection check",
			attachID, len(delAttach.GetDeletedMailIds()), delAttach.GetProtectedMailIds())
	}
	for round := 1; round <= 2; round++ {
		if err := r.expect(game.ClientPlayerMailDeleteMailsMessageId, &mailpb.DeleteMailsRequest{MailIds: []uint64{plainID}}, &mailpb.DeleteMailsResponse{}, 0); err != nil {
			fail("delete-plain", "round %d: %v", round, err)
		}
	}
	// 系统邮件按人删:R 删掉自己视图里的 S,O 的视图不受影响。顺带让 R 的邮箱每轮只累积删不掉的 Q 一封(连跑时第 4 步保持单页精确核账)。
	delSys := &mailpb.DeleteMailsResponse{}
	if err := r.expect(game.ClientPlayerMailDeleteMailsMessageId, &mailpb.DeleteMailsRequest{MailIds: []uint64{sysID}}, delSys, 0); err != nil {
		fail("delete-system", "%v", err)
	}
	if len(delSys.GetDeletedMailIds()) != 1 || len(delSys.GetProtectedMailIds()) != 0 {
		fail("delete-system", "DeleteMails([%d]) deleted_count=%d protected=%v, expected 1 and nothing protected (system mail without attachments)",
			sysID, len(delSys.GetDeletedMailIds()), delSys.GetProtectedMailIds())
	}
	rAfterDelete, err := r.listAll()
	if err != nil {
		fail("delete-plain", "%v", err)
	}
	if mailSmokeFind(rAfterDelete.mails, plainID) != nil {
		fail("delete-plain", "deleted mail %d is still listed", plainID)
	}
	if mailSmokeFind(rAfterDelete.mails, sysID) != nil {
		fail("delete-system", "system mail %d is still listed for the recipient after deleting it", sysID)
	}
	oAfterDelete, err := o.listAll()
	if err != nil {
		fail("delete-system", "%v", err)
	}
	if mailSmokeFind(oAfterDelete.mails, sysID) == nil {
		fail("delete-system", "observer can no longer see system mail %d after the recipient deleted it: deleting a system mail must only hide it for the deleter", sysID)
	}
	zap.L().Info("[mail-smoke] step 6: unclaimed delete protected, claimed delete accepted, plain delete idempotent, system mail deleted for the recipient only")

	// ---- 步骤 7:D-9 反向断言 ----
	// NotifyMailChanged 与 5 个 C2S 方法同处 ClientPlayerMail(客户端协议服务),但它标了 CLIENT_RPC_PUSH:
	// gate 的上行白名单 IsClientMessageId 只收客户端能发起的方法(REQUEST / ONE_WAY,client-rpc-callback.md §7 R4),
	// 所以这个号在 gate 就按非法包丢弃、不回包,期望超时。mail 的会话拦截器(方法级白名单)仍是第二道防线,经 gate 走不到它。
	// 仍由**不收定向邮件的 O** 来发,让 R 的推送留底与本步无关。
	// 超时有三种来源要分开报:gate 回了信封但没回显请求号(onMessage 丢弃并计数)、回了不带错误也没回显请求号的
	// 业务回包(onMessage 当成推送留底,O 在本步本不该收到推送)、真没回包(期望)。前两种说明 gate 收了这个号,不是丢弃。
	// 等待预算 10s 大于路由服 ForwardTimeoutMs(5s),这类回包会落在等待期内。
	notifyDropsBefore := o.uncorrelatedReplies(game.ClientPlayerMailNotifyMailChangedMessageId)
	notifyPushesBefore := o.pushCount()
	envelopeTip, err := o.call(game.ClientPlayerMailNotifyMailChangedMessageId,
		&mailpb.MailChangedEvent{MailId: sysID}, &base.Empty{})
	switch {
	case errors.Is(err, errMailSmokeTimeout) &&
		o.uncorrelatedReplies(game.ClientPlayerMailNotifyMailChangedMessageId) > notifyDropsBefore:
		fail("notify-via-gate", "Expected gate to drop NotifyMailChanged, but a reply without request id arrived on message_id=%d (see the "+
			"\"reply without request id dropped\" warning): the gate client allowlist includes push message IDs "+
			"(regenerate with the R4 generator and rebuild gate), and the reply path does not echo ClientRequest.id",
			game.ClientPlayerMailNotifyMailChangedMessageId)
	case errors.Is(err, errMailSmokeTimeout) && o.pushCount() > notifyPushesBefore:
		fail("notify-via-gate", "Timed out, but a NotifyMailChanged message without request id and without error arrived meanwhile "+
			"and was taken as a push: likely a business response to this request whose reply path does not echo ClientRequest.id. "+
			"If so, the S2C push method is exposed to clients (D-9)")
	case errors.Is(err, errMailSmokeTimeout):
		// 期望:gate 丢弃
	case err != nil:
		fail("notify-via-gate", "%v", err)
	case envelopeTip != 0:
		fail("notify-via-gate", "Expected gate to drop NotifyMailChanged (timeout), got envelope tip=%d: the gate client allowlist "+
			"includes push message IDs (regenerate with the R4 generator and rebuild gate)", envelopeTip)
	default:
		fail("notify-via-gate", "A client calling NotifyMailChanged through gate received a business response: "+
			"the S2C push method is exposed to clients, allowing anyone to forge a mail event (D-9)")
	}
	// MailAdmin 不是客户端协议:gate 的 IsClientMessageId 不收这两个号,直接丢弃且不回包,并计一次非法包
	// (默认阈值 50 才踢线),所以每个号只发一次。信封拒绝说明白名单收了这个号、请求进了路由服,同样不合格。
	for _, adminCall := range []struct {
		name      string
		messageID uint32
		request   proto.Message
		response  proto.Message
	}{
		{"SendPersonalMail", game.MailAdminSendPersonalMailMessageId,
			&mailpb.SendMailRequest{SenderKind: 1, RequestId: requestBase + 3,
				RecipientPlayerId: o.gc.PlayerId, Title: "smoke via gate " + nonce, ExpireMs: expireMs},
			&mailpb.SendMailResponse{}},
		{"SendSystemMail", game.MailAdminSendSystemMailMessageId,
			&mailpb.SendMailRequest{SenderKind: 1, RequestId: requestBase + 4,
				Title: "smoke via gate " + nonce, ExpireMs: expireMs},
			&mailpb.SendMailResponse{}},
	} {
		// 超时前若这个号上有过被丢弃的无请求号回包,说明 gate 收了这个号并回了包(只是没回显请求号),不是丢弃。
		adminDropsBefore := o.uncorrelatedReplies(adminCall.messageID)
		envelopeTip, err := o.call(adminCall.messageID, adminCall.request, adminCall.response)
		switch {
		case errors.Is(err, errMailSmokeTimeout) && o.uncorrelatedReplies(adminCall.messageID) > adminDropsBefore:
			fail("admin-via-gate", "MailAdmin.%s: expected gate to drop the request, but a reply without request id arrived on message_id=%d "+
				"(see the \"reply without request id dropped\" warning): the gate client allowlist includes MailAdmin message IDs",
				adminCall.name, adminCall.messageID)
		case errors.Is(err, errMailSmokeTimeout):
			// 期望:gate 丢弃
		case err != nil:
			fail("admin-via-gate", "MailAdmin.%s: %v", adminCall.name, err)
		case envelopeTip != 0:
			fail("admin-via-gate", "MailAdmin.%s: expected gate to drop the request (timeout), got envelope tip=%d: the gate client allowlist includes MailAdmin message IDs",
				adminCall.name, envelopeTip)
		default:
			fail("admin-via-gate", "A client calling MailAdmin.%s through gate received a business response", adminCall.name)
		}
	}
	zap.L().Info("[mail-smoke] step 7: gate dropped client-sent NotifyMailChanged / MailAdmin")

	zap.L().Info(fmt.Sprintf("MAIL_SMOKE_OK recipient=%d observer=%d sys_mail_id=%d plain_mail_id=%d attach_mail_id=%d push_received=%v gate_recipient=%s gate_observer=%s",
		r.gc.PlayerId, o.gc.PlayerId, sysID, plainID, attachID, pushReceived, r.gateAddr(), o.gateAddr()),
		zap.Bool("cross_zone", sc.CrossZone), zap.Uint32("zone_a", sc.ZoneA), zap.Uint32("zone_recipient", zoneR))
	cleanup()
	_ = zap.L().Sync()
}

// mailSmokeLogin 走完 AssignGate → 连接 → 登录 → 进场,并挂上本场景自己的 RecvLoop 回调(与 friendSmokeLogin 同形)。
func mailSmokeLogin(cfg *config.Config, account string, spacing, pushTimeout time.Duration, stats *metrics.Stats) (*mailSmokeBot, error) {
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

	bot := &mailSmokeBot{
		account:        account,
		zone:           cfg.ZoneID,
		gc:             gc,
		player:         gameobject.NewPlayer(gc.PlayerId),
		stats:          stats,
		requestSpacing: spacing,
		pushTimeout:    pushTimeout,
		caller:         newSmokeCaller(gc, stats, account, mailSmokeRpcTimeout, errMailSmokeTimeout),
		uncorrelated:   make(map[uint32]int),
	}
	gameobject.PlayerList.Set(gc.PlayerId, bot.player)
	go gc.RecvLoop(bot.onMessage)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), mailSmokeSceneReadyTimeout)
	defer waitCancel()
	if err := bot.player.WaitSceneReady(waitCtx); err != nil {
		sendDisconnectBestEffort(gc)
		gameobject.PlayerList.Delete(gc.PlayerId)
		gc.Close()
		return nil, fmt.Errorf("wait scene ready: %w", err)
	}
	return bot, nil
}

// gateAddr 返回当前连着的 gate("ip:port";重定向后是新 gate),形状同 syncSmokeBot.gateAddr。
func (b *mailSmokeBot) gateAddr() string {
	ip, port := b.gc.GateAddr()
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

// onMessage:邮件推送留底;邮件请求的回包带请求号,由 GameClient 交给 caller,不会到这里;
// gate 的 SendTipToClient 交给 caller 供快速失败;其余交给通用分发。
//
// ⚠ 必须在分发前拦下 ClientPlayerMail 的**全部 6 个**消息号与 MailAdmin 的 2 个(mailSmokeIsMailMessage):
// 落到这里的是没回显请求号的回包,其中的信封拒绝(error_message 非空、body 为空)一旦落进 MessageBodyHandler
// 会被解成全零响应,看上去像 tip=0 受理 —— 失败会显示为通过。
// 第 7 步对推送号 NotifyMailChanged 本身发请求:它的回包带请求号、推送 id = 0,按 id 即可分开,不再需要 awaiting 标记。
// 这个号上 id = 0 却带信封错误的不是推送(推送从不带 error_message),而是没回显请求号的信封拒绝,同样丢弃,
// 不记成一条 mail_id = 0 的假推送。丢弃时按消息号计数(uncorrelated),第 7 步据此区分「gate 丢弃」与「回了包但没回显请求号」。
func (b *mailSmokeBot) onMessage(client *pkg.GameClient, msg *base.MessageContent) {
	b.stats.MsgRecv()
	id := msg.GetMessageId()
	if id == game.ClientPlayerMailNotifyMailChangedMessageId && msg.GetErrorMessage().GetId() == 0 {
		evt := &mailpb.MailChangedEvent{}
		if err := proto.Unmarshal(msg.GetSerializedMessage(), evt); err == nil {
			b.mu.Lock()
			b.pushes = append(b.pushes, evt)
			b.mu.Unlock()
		}
		return
	}
	if mailSmokeIsMailMessage(id) {
		smokeDropUncorrelatedReply(b.account, msg)
		b.mu.Lock()
		b.uncorrelated[id]++
		b.mu.Unlock()
		return
	}
	if id == game.SceneClientPlayerCommonSendTipToClientMessageId {
		b.caller.onGateTipMessage(msg)
	}
	handler.MessageBodyHandler(client, msg)
}

// resetPushes 清空已收到的推送。在**触发推送的那次请求发出之前**调用:清晚了会把本次要看的那条一起清掉。
func (b *mailSmokeBot) resetPushes() {
	b.mu.Lock()
	b.pushes = nil
	b.mu.Unlock()
}

// pushCount 返回当前留底的邮件推送条数(第 7 步诊断用:没回显请求号、也不带错误的回包会被当成推送留底)。
func (b *mailSmokeBot) pushCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pushes)
}

// uncorrelatedReplies 返回 messageId 上至今被 onMessage 丢弃的「没回显请求号的回包」个数。
func (b *mailSmokeBot) uncorrelatedReplies(messageId uint32) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.uncorrelated[messageId]
}

// waitPush 等一条指定 reason 的邮件事件,返回是否在预算内收到(弱断言,由调用方只打印不判失败)。
func (b *mailSmokeBot) waitPush(mailID uint64) bool {
	deadline := time.Now().Add(b.pushTimeout)
	for {
		b.mu.Lock()
		for _, evt := range b.pushes {
			if evt.GetMailId() == mailID {
				b.mu.Unlock()
				return true
			}
		}
		b.mu.Unlock()
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mailSmokeListing 是一次翻页列表的汇总结果。unread / claimable / serverNowMs 取第一页的值(服务端对全集算;now 以第一页为准)。
type mailSmokeListing struct {
	mails       []*mailpb.MailView
	unread      uint32
	claimable   uint32
	serverNowMs uint64
	// singlePage:第一页就拉完(has_more=false)。服务端在一次请求里用同一个 now 算列表与计数,单页列表就是计数所数的全集。
	singlePage bool
}

// listAll 按 before_mail_id 游标翻页拉列表,最多 mailSmokeMaxPages 页;has_more=false 即停。
func (b *mailSmokeBot) listAll() (*mailSmokeListing, error) {
	out := &mailSmokeListing{}
	var before uint64
	for page := 0; page < mailSmokeMaxPages; page++ {
		resp := &mailpb.ListMailsResponse{}
		if err := b.expect(game.ClientPlayerMailListMailsMessageId,
			&mailpb.ListMailsRequest{BeforeMailId: before, Limit: mailSmokePageSize}, resp, 0); err != nil {
			return nil, err
		}
		if page == 0 {
			out.unread = resp.GetUnreadCount()
			out.claimable = resp.GetClaimableCount()
			out.serverNowMs = resp.GetServerNowMs()
			out.singlePage = resp.GetNextBeforeMailId() == 0
		}
		out.mails = append(out.mails, resp.GetMails()...)
		if resp.GetNextBeforeMailId() == 0 || len(resp.GetMails()) == 0 {
			return out, nil
		}
		next := resp.GetNextBeforeMailId()
		if before != 0 && next >= before {
			return nil, fmt.Errorf("account=%s ListMails cursor did not advance (before=%d next=%d): mail_id must be strictly descending", b.account, before, next)
		}
		before = next
	}
	return out, nil
}

// mailSmokeCheckUnreadAfterRead 核对「读掉一封未读信」前后两次列表的 unread_count,返回本次是否为精确核账。
//
// 两次列表之间隔着好几个请求(每个 ≥ request_interval_ms),这段时间里别的信也会变:前几轮留下的未读信可能恰好过期
// (服务端只数 expire_ms > now 的),别人(另一轮冒烟、运维)也可能新发一封系统邮件。所以期望值逐封核账,不死板地「恰好少 1」:
//
//	expected = before.unread − 1(读掉的那封)
//	         − before 里未读、且 expire_ms ∈ (before.serverNowMs, after.serverNowMs] 的封数(两次之间过期)
//	         + after 里未读、且 mail_id 不在 before 里的封数(两次之间新到;新信 mail_id 最大,一定落在 after 的第一页)
//
// 两次都是单页时这是精确等式。多页时,翻页期间过期的信两份列表都看不见,实际值只会比 expected 更小,
// 只能断言 after.unread ≤ expected:「读信没扣未读」(实际 = expected + 1)照样抓得到,「多扣一封」抓不到 —— 返回 exact=false 由调用方打进日志。
func mailSmokeCheckUnreadAfterRead(before, after *mailSmokeListing, readID uint64) (exact bool, err error) {
	seen := make(map[uint64]struct{}, len(before.mails))
	var expired int64
	for _, m := range before.mails {
		seen[m.GetMailId()] = struct{}{}
		if m.GetMailId() == readID || m.GetIsRead() {
			continue
		}
		if m.GetExpireMs() > before.serverNowMs && m.GetExpireMs() <= after.serverNowMs {
			expired++
		}
	}
	var arrived int64
	for _, m := range after.mails {
		if _, ok := seen[m.GetMailId()]; !ok && !m.GetIsRead() {
			arrived++
		}
	}
	expected := int64(before.unread) - 1 - expired + arrived
	actual := int64(after.unread)
	exact = before.singlePage && after.singlePage
	if actual == expected || (!exact && actual < expected) {
		return exact, nil
	}
	return exact, fmt.Errorf("unread_count %d -> %d after reading mail %d, expected %d (= %d - 1 read - %d expired between the two listings + %d new unread; exact=%v)",
		before.unread, after.unread, readID, expected, before.unread, expired, arrived, exact)
}

// call 发一个邮件请求并只等它自己的回包(smokeCaller.call,按请求号认领;形状照 friendSmokeBot.call)。
// 上一次超时请求的迟到回包由 GameClient 计数后丢弃,不会被本次误认。
//
// 返回的 envelopeTip 非 0 表示 gate / 路由服级拒绝(信封 error_message,body 为空),此时 response 未填充;
// 为 0 时 response 已解码。gate 推 kServiceUnavailable(tipMailServiceUnavailable 与 smokeCaller 判的是同一个
// CommonError_kServiceUnavailable)返回 error;超时返回包裹 errMailSmokeTimeout 的 error。
func (b *mailSmokeBot) call(messageId uint32, request, response proto.Message) (envelopeTip uint32, err error) {
	if wait := b.requestSpacing - time.Since(b.lastRequestAt); wait > 0 {
		time.Sleep(wait)
	}
	b.lastRequestAt = time.Now()
	return b.caller.call(messageId, request, response)
}

// rpc 发请求并返回业务 tip(响应体 error_message.id,0 = 受理)。信封拒绝与超时一律 error:
// 它们说明请求没到 mail 业务逻辑,不能当成 mail 给的码去断言。
func (b *mailSmokeBot) rpc(messageId uint32, request proto.Message, response mailResponse) (uint32, error) {
	envelopeTip, err := b.call(messageId, request, response)
	if err != nil {
		return 0, err
	}
	if envelopeTip != 0 {
		return 0, fmt.Errorf("gate/路由服信封拒绝 message_id=%d tip=%d(请求未到达 mail 业务逻辑:tip=%d 多为路由服没有 mail 实例、"+
			"路由表未更新,或 mail 返回了 gRPC 错误(会话拦截器拒绝等);其它常见为 gate 限流,可调大 request_interval_ms)",
			messageId, envelopeTip, tipMailServiceUnavailable)
	}
	return response.GetErrorMessage().GetId(), nil
}

// expect 发请求并断言业务 tip 等于 want;常见的环境类拒绝码翻译成可读原因。
func (b *mailSmokeBot) expect(messageId uint32, request proto.Message, response mailResponse, want uint32) error {
	got, err := b.rpc(messageId, request, response)
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	switch got {
	case tipMailInvalidParameter:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(请求校验不通过,或 mail 在逻辑层拿不到会话身份 —— "+
			"\"我是谁\" 只从 gate 注入的会话 metadata 取,检查路由服是否透传 x-session-detail-bin),期望 tip=%d",
			messageId, b.account, got, want)
	case tipMailRateLimited:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(gate 限流:MessageLimiter 没按 mail 导表时吃默认档,调大 request_interval_ms),期望 tip=%d",
			messageId, b.account, got, want)
	case tipMailServiceUnavailable:
		return fmt.Errorf("message_id=%d account=%s 被拒:tip=%d(mail 侧 MySQL / Redis 真故障,看 mail 错误日志;"+
			"常见为 mmorpg_mail 库没建或表没迁移)", messageId, b.account, got)
	}
	return fmt.Errorf("message_id=%d account=%s 期望 tip=%d,实得 tip=%d", messageId, b.account, want, got)
}

// mailSmokeAdminContext 给直连 MailAdmin 的调用加上令牌与操作人 metadata(与 go/mail/cmd/mailadmin 同一写法)。
// 不带任何会话 metadata:mail 的会话拦截器把"无会话"视为内部调用放行,令牌由 MailAdmin 自己核对。
func mailSmokeAdminContext(_ string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), mailSmokeAdminTimeout)
	// 不带 gate 会话；身份由请求内的 serviceauth 证明。
	return ctx, cancel
}

// mailSmokeAdminGrpcHint 把直连 MailAdmin 的 gRPC 错误翻成可读原因(不含令牌值)。
func mailSmokeAdminGrpcHint(err error) string {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return "(令牌没配、不足 32 字节或与 mail 进程不一致 —— mail 启动横幅 mail_admin=disabled 说明 mail 那边没拿到;或调用带上了会话 metadata)"
	case codes.Unavailable, codes.DeadlineExceeded:
		return "(mail 未起或 mail_smoke.admin_addr 不可达)"
	case codes.Unimplemented:
		return "(mail 没有注册 MailAdmin 服务,二进制是否过旧)"
	}
	return ""
}

// mailSmokeAdminTipHint 把 MailAdmin 的业务拒绝码翻成可读原因。
func mailSmokeAdminTipHint(tip uint32) string {
	switch tip {
	case tipMailInvalidParameter:
		return "(参数没通过校验;Mode 不是 dev / test 时 sender_kind=ROBOT 也回这个码)"
	case tipMailAttachmentNotOpen:
		return "(Mode=pro 不允许发带附件的邮件:mail-smoke 要求 mail.yaml 的 Mode 为 dev 或 test)"
	case tipMailRecipientBoxFull:
		return "(收件人邮箱满:15 分钟内连跑太多轮,等本轮的信过期再跑)"
	case tipMailRequestConflict:
		return "(同一 request_id 带了不同内容:重放必须原样带回首次的请求,含 expire_ms)"
	case tipMailServiceUnavailable:
		return "(mail 侧存储 / data_service / 号段故障:检查 mmorpg_mail 迁移、data_service BootstrapTags 含 mail 且 id_segment 有 mail 行)"
	}
	return ""
}

// mailSmokeSendSystem 调 MailAdmin.SendSystemMail,受理且 sys_mail_id 非 0 才返回。
func mailSmokeSendSystem(client mailpb.MailAdminClient, token string, req *mailpb.SendMailRequest) (uint64, bool, error) {
	ctx, cancel := mailSmokeAdminContext(token)
	defer cancel()
	if err := serviceauth.Sign("/mailpb.MailAdmin/SendSystemMail", req, "admin", token, uint64(time.Now().UnixMilli())); err != nil {
		return 0, false, err
	}
	resp, err := client.SendSystemMail(ctx, req)
	if err != nil {
		return 0, false, fmt.Errorf("SendSystemMail request_id=%d gRPC 错误 %v%s", req.GetRequestId(), err, mailSmokeAdminGrpcHint(err))
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return 0, false, fmt.Errorf("SendSystemMail request_id=%d 业务拒绝 tip=%d%s", req.GetRequestId(), tip, mailSmokeAdminTipHint(tip))
	}
	if resp.GetMailId() == 0 {
		return 0, false, fmt.Errorf("SendSystemMail request_id=%d 受理但 sys_mail_id=0", req.GetRequestId())
	}
	return resp.GetMailId(), resp.GetDuplicated(), nil
}

// mailSmokeSendPersonal 调 MailAdmin.SendPersonalMail,受理且 mail_id 非 0 才返回。
func mailSmokeSendPersonal(client mailpb.MailAdminClient, token string, req *mailpb.SendMailRequest) (uint64, bool, error) {
	ctx, cancel := mailSmokeAdminContext(token)
	defer cancel()
	if err := serviceauth.Sign("/mailpb.MailAdmin/SendPersonalMail", req, "admin", token, uint64(time.Now().UnixMilli())); err != nil {
		return 0, false, err
	}
	resp, err := client.SendPersonalMail(ctx, req)
	if err != nil {
		return 0, false, fmt.Errorf("SendPersonalMail request_id=%d recipient=%d gRPC 错误 %v%s",
			req.GetRequestId(), req.GetRecipientPlayerId(), err, mailSmokeAdminGrpcHint(err))
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return 0, false, fmt.Errorf("SendPersonalMail request_id=%d recipient=%d 业务拒绝 tip=%d%s",
			req.GetRequestId(), req.GetRecipientPlayerId(), tip, mailSmokeAdminTipHint(tip))
	}
	if resp.GetMailId() == 0 {
		return 0, false, fmt.Errorf("SendPersonalMail request_id=%d 受理但 mail_id=0", req.GetRequestId())
	}
	return resp.GetMailId(), resp.GetDuplicated(), nil
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

// mailSmokeIsMailMessage 列出 ClientPlayerMail 的全部 6 个消息号与 MailAdmin 的 2 个。
// 新增 rpc 必须同步加进来 —— 漏一个,该号上没回显请求号的信封拒绝就会落进通用分发、被解成全零响应。
func mailSmokeIsMailMessage(messageId uint32) bool {
	switch messageId {
	case game.ClientPlayerMailListMailsMessageId,
		game.ClientPlayerMailReadMailMessageId,
		game.ClientPlayerMailDeleteMailsMessageId,
		game.ClientPlayerMailClearReadMailsMessageId,
		game.ClientPlayerMailClaimMailAttachmentsMessageId,
		game.ClientPlayerMailNotifyMailChangedMessageId,
		game.MailAdminSendPersonalMailMessageId,
		game.MailAdminSendSystemMailMessageId:
		return true
	}
	return false
}

// mailSmokeNonce 是本轮的短标识(放进标题,邮件标题上限 32 个字符):毫秒时间戳的 36 进制尾 8 位。
func mailSmokeNonce() string {
	s := strconv.FormatInt(time.Now().UnixMilli(), 36)
	if len(s) > 8 {
		s = s[len(s)-8:]
	}
	return s
}

func mailSmokeFind(mails []*mailpb.MailView, mailID uint64) *mailpb.MailView {
	for _, m := range mails {
		if m.GetMailId() == mailID {
			return m
		}
	}
	return nil
}

func mailSmokeContains(ids []uint64, id uint64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
