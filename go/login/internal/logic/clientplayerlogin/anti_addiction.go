package clientplayerloginlogic

import (
	"context"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	return aa.Evaluate(channel, pi, aa.Now()).Status
}

// beginWatchedSession 在写登录态之前把这条连接登记进防沉迷会话账本(nppa.Ledger,§5.4、§5.5):自有渠道实名的会话排一条上线,
// 未成年人记下截止时刻。登记不上就拒绝这次绑定:报不了上线、到点兜不住的会话不放行。走平台渠道的成年人什么都不登记。
func beginWatchedSession(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref, v svc.Verdict) error {
	if sc.AntiAddiction == nil || (v.ReportPI == "" && v.Deadline.IsZero()) {
		return nil
	}
	si := ""
	if v.ReportPI != "" {
		var err error
		if si, err = nppa.NewID(); err != nil {
			return err
		}
	}
	return sc.AntiAddiction.Ledger.Begin(ctx, ref.DeviceMember(), si, v.ReportPI, v.Deadline)
}

// endWatchedSession 在会话收尾(断线、离开游戏)或绑定半途发现连接已断时,把这条连接从账本里收尾,上报的会话排一条下线。
// 账本里没有它就什么都不做,所以 gate 重放断线通知无害。出错只记日志:登记还在账本里,租约到期后由清扫补下线。
func endWatchedSession(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref) {
	if sc.AntiAddiction == nil {
		return
	}
	if err := sc.AntiAddiction.Ledger.End(ctx, ref.DeviceMember()); err != nil {
		logx.Errorf("[anti-addiction] end session failed, the sweep closes it when its lease runs out: session=%s err=%v", ref.DeviceMember(), err)
	}
}

// watchedPlayDeadlineMs 取这条连接在账本里的截止时刻(未成年人当天能玩到的时刻,Unix 毫秒;0 = 不限)。进游戏时用:
// 它随会话记录写进 player_locator,断线后的离线托管不越过它(§5.4)。进游戏本身说明连接还活着,所以顺带续上账本的租约。
// 账本读不了时返回错误,调用方拒绝进游戏。部署没开防沉迷时恒为 0。
func watchedPlayDeadlineMs(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref) (uint64, error) {
	if sc.AntiAddiction == nil {
		return 0, nil
	}
	deadline, err := sc.AntiAddiction.Ledger.Renew(ctx, ref.DeviceMember())
	if err != nil || deadline.IsZero() {
		return 0, err
	}
	return uint64(deadline.UnixMilli()), nil
}

// backstopKickTotal 计 login 兜底踢掉的未成年人连接。到点断开由 gate 自己做,这个数不为 0 说明有 gate 没断开。
var backstopKickTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "login_anti_addiction_backstop_kick_total",
	Help: "Minor connections kicked by login because their gate did not close them at the play deadline.",
})

// renewWatchedSession 在 gate 代发的续租里给账本里的会话顺延租约,并兜底未成年人的截止时刻(§5.4)。
// 到点断开由 gate 自己做(cpp/nodes/gate/handler/rpc/minor_play_deadline.h):它在截止前几秒就关连接,不依赖 login。
// 过了截止时刻续租还能送到,说明那台 gate 没关它(没读到登录回包里的截止时刻):这里补一次踢线,并打 ERROR。
// 账本读不了时不踢:续租会重试,截止时刻下一次还在。
func renewWatchedSession(ctx context.Context, sc *svc.ServiceContext, ref loginsession.Ref,
	details *login_proto_common.SessionDetails) {
	if sc.AntiAddiction == nil {
		return
	}
	deadline, err := sc.AntiAddiction.Ledger.Renew(ctx, ref.DeviceMember())
	if err != nil {
		logx.Errorf("[anti-addiction] renew session lease failed, retried on the next renewal: session=%s err=%v", ref.DeviceMember(), err)
		return
	}
	if deadline.IsZero() || sc.AntiAddiction.Now().Before(deadline) {
		return
	}
	backstopKickTotal.Inc()
	gateID := strconv.FormatUint(uint64(details.GetGateNodeId()), 10)
	err = sc.KickSessionWithReason(ctx, gateID, details.GetGateInstanceId(), details.GetSessionId(), details.GetPlayerId(),
		uint32(table.LoginError_kLoginMinorPlayTimeOver))
	logx.Errorf("[anti-addiction] a minor is still connected past the play deadline, the gate did not close it; kicking: "+
		"session_id=%d gate_instance=%q deadline=%s kick_err=%v",
		details.GetSessionId(), details.GetGateInstanceId(), deadline.Format(time.RFC3339), err)
}
