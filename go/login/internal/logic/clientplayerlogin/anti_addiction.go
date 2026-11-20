package clientplayerloginlogic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/nppa"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// readAccountDirectoryWithRealName 与 readAccountDirectory 是同一次目录读,另带账号的实名 pi
// (docs/design/real-name-anti-addiction.md §5.1)与注销状态(docs/design/account-deletion.md §4.3)。
// 目录实现不支持(测试替身)时 pi 为空、注销状态为空(0)。lifecycle 恒非 nil。
func readAccountDirectoryWithRealName(ctx context.Context, sc *svc.ServiceContext, account string, create bool) (*dbpb.UserAccounts, string, *dspb.AccountLifecycle, error) {
	realNames, ok := sc.RealNames()
	if !ok {
		userAccount, err := readAccountDirectory(ctx, sc, account, create)
		return userAccount, "", &dspb.AccountLifecycle{}, err
	}
	out, err := realNames.ReadWithLifecycle(ctx, account, create)
	if err != nil {
		return nil, "", nil, err
	}
	lifecycle := out.GetLifecycle()
	if lifecycle == nil {
		lifecycle = &dspb.AccountLifecycle{}
	}
	return &dbpb.UserAccounts{Account: account, SimplePlayers: out.GetPlayers()}, out.GetRealNamePi(), lifecycle, nil
}

// httpLoginAntiAddiction 是 /api/login 应答里的防沉迷状态,只给客户端决定下一步,不拦(拦在 gate 上的 Login)。
// 认证中的实名提交在这里收尾(§5.2)。部署没开防沉迷时回 nil。
func httpLoginAntiAddiction(ctx context.Context, sc *svc.ServiceContext, account, channel, pi string) *login_proto.AntiAddictionStatus {
	aa := sc.AntiAddiction
	if aa == nil {
		return nil
	}
	if pi == "" && aa.SelfManaged(channel) {
		pi = aa.RealName.ResolvePending(ctx, account)
	}
	return aa.Evaluate(channel, pi, time.Now()).Status
}

// antiAddictionSession 是一条 gate 连接上的防沉迷记录(redisslot.LoginAntiAddictionKey):gate 上的 Login 绑定会话时写,
// 续租时读(到点踢线)并续期,会话收尾时取走(报下线)。
type antiAddictionSession struct {
	PI         string `json:"pi,omitempty"`
	SI         string `json:"si,omitempty"`
	DeadlineMs int64  `json:"deadline_ms,omitempty"`
	Report     bool   `json:"report,omitempty"`
}

func antiAddictionKey(ref loginsession.Ref) string {
	return redisslot.LoginAntiAddictionKey(ref.Fence().String())
}

// antiAddictionTTL 让记录至少活到截止时刻之后一分钟,也不短于登录态寿命。
func antiAddictionTTL(deadlineMs int64, now time.Time) time.Duration {
	ttl := loginsession.TTL()
	if deadlineMs > 0 {
		if untilDeadline := time.UnixMilli(deadlineMs).Sub(now) + time.Minute; untilDeadline > ttl {
			ttl = untilDeadline
		}
	}
	return ttl
}

func writeAntiAddictionSession(ctx context.Context, rdb redis.UniversalClient, ref loginsession.Ref, record antiAddictionSession, now time.Time) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, antiAddictionKey(ref), raw, antiAddictionTTL(record.DeadlineMs, now)).Err()
}

// sessionSI 是上报用的会话标识:连接身份加绑定时刻做 SHA-256,取前 32 位十六进制。同一连接 LeaveGame 之后重新绑定是新的会话。
func sessionSI(ref loginsession.Ref, now time.Time) string {
	sum := sha256.Sum256([]byte(ref.Fence().String() + ":" + strconv.FormatInt(now.UnixNano(), 10)))
	return hex.EncodeToString(sum[:16])
}

// holdMinorDeadline 在写登录态之前落下未成年人的截止时刻:写不上就拒绝这次绑定,到点踢线不能没有依据(§5.4)。
func holdMinorDeadline(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref, v svc.Verdict, now time.Time) error {
	if v.Deadline.IsZero() {
		return nil
	}
	return writeAntiAddictionSession(ctx, sc.LoginRedis, ref, antiAddictionSession{DeadlineMs: v.Deadline.UnixMilli()}, now)
}

// startAntiAddictionReport 在绑定成功之后为我们自己实名的会话写上报记录并排一条上线(§5.5)。写不上只记日志:
// 上报本来就是尽力而为;写不上时也不排上线,免得只有上线没有下线。截止时刻已由 holdMinorDeadline 落下,不受影响。
func startAntiAddictionReport(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref, v svc.Verdict, now time.Time) {
	if sc.AntiAddiction == nil || v.ReportPI == "" {
		return
	}
	record := antiAddictionSession{PI: v.ReportPI, SI: sessionSI(ref, now), Report: true}
	if !v.Deadline.IsZero() {
		record.DeadlineMs = v.Deadline.UnixMilli()
	}
	if err := writeAntiAddictionSession(ctx, sc.LoginRedis, ref, record, now); err != nil {
		logx.Errorf("[anti-addiction] session record write failed, this login is not reported: session=%s err=%v", ref.DeviceMember(), err)
		return
	}
	sc.AntiAddiction.Reporter.Report(nppa.Behavior{SI: record.SI, BT: nppa.BehaviorLogin, OT: now.Unix(), PI: record.PI})
}

// endAntiAddictionSession 在会话收尾(Disconnect、LeaveGame)时取走本连接的防沉迷记录,我们自己实名的会话排一条下线。
func endAntiAddictionSession(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref) {
	if sc.AntiAddiction == nil {
		return
	}
	raw, err := sc.LoginRedis.GetDel(ctx, antiAddictionKey(ref)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.Errorf("[anti-addiction] take session record failed, logout not reported: session=%s err=%v", ref.DeviceMember(), err)
		}
		return
	}
	var record antiAddictionSession
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		logx.Errorf("[anti-addiction] corrupt session record dropped: session=%s err=%v", ref.DeviceMember(), err)
		return
	}
	if record.Report && record.SI != "" && record.PI != "" {
		sc.AntiAddiction.Reporter.Report(nppa.Behavior{SI: record.SI, BT: nppa.BehaviorLogout, OT: time.Now().Unix(), PI: record.PI})
	}
}

// minorRenewRetry 是读不到记录或踢线没发出去时下一次续租的间隔:不能等一个正常续租周期(登录态寿命的三分之一)。
const minorRenewRetry = 30 * time.Second

// enforceMinorDeadline 在 gate 代发的续租里执行未成年人的截止时刻(§5.4):没到点就把下一次续租提前到截止时刻,
// 到点了就踢线。顺带给防沉迷记录续期。gate 把间隔夹到至少 5 秒,所以最晚在截止之后约 5 秒踢下线。
func enforceMinorDeadline(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref,
	details *login_proto_common.SessionDetails, resp *login_proto.RenewSessionResponse, now time.Time) {
	if sc.AntiAddiction == nil {
		return
	}
	key := antiAddictionKey(ref)
	raw, err := sc.LoginRedis.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return
	}
	if err != nil {
		logx.Errorf("[anti-addiction] read session record failed, retrying soon: session=%s err=%v", ref.DeviceMember(), err)
		shortenRenewal(resp, minorRenewRetry)
		return
	}
	var record antiAddictionSession
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		logx.Errorf("[anti-addiction] corrupt session record: session=%s err=%v", ref.DeviceMember(), err)
		return
	}
	if err := sc.LoginRedis.PExpire(ctx, key, antiAddictionTTL(record.DeadlineMs, now)).Err(); err != nil {
		logx.Errorf("[anti-addiction] renew session record failed: session=%s err=%v", ref.DeviceMember(), err)
	}
	if record.DeadlineMs == 0 {
		return
	}
	if remaining := time.UnixMilli(record.DeadlineMs).Sub(now); remaining > 0 {
		shortenRenewal(resp, remaining)
		return
	}
	gateID := strconv.FormatUint(uint64(details.GetGateNodeId()), 10)
	if err := sc.KickSessionWithReason(ctx, gateID, details.GetGateInstanceId(), details.GetSessionId(), details.GetPlayerId(),
		uint32(table.LoginError_kLoginMinorPlayTimeOver)); err != nil {
		logx.Errorf("[anti-addiction] kick at minor play deadline failed, retrying soon: session_id=%d err=%v", details.GetSessionId(), err)
	} else {
		logx.Infof("[anti-addiction] minor play time over, kicked: session_id=%d gate_instance=%q", details.GetSessionId(), details.GetGateInstanceId())
	}
	// 连接还在的话,下一次续租再踢一次。
	shortenRenewal(resp, minorRenewRetry)
}

func shortenRenewal(resp *login_proto.RenewSessionResponse, after time.Duration) {
	seconds := uint32((after + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if resp.RenewAfterSeconds == 0 || seconds < resp.RenewAfterSeconds {
		resp.RenewAfterSeconds = seconds
	}
}
