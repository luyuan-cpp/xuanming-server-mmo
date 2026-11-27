package clientplayerloginlogic

import (
	"context"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type DisconnectLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDisconnectLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DisconnectLogic {
	return &DisconnectLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DisconnectLogic) Disconnect(in *login_proto.LoginNodeDisconnectRequest) (*login_proto.LoginEmptyResponse, error) {
	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "disconnect")
	if !ok {
		return &login_proto.LoginEmptyResponse{}, nil
	}

	// 会话号以 gate 签名、callerauth 验过的会话头为准(ctx 里的 SessionDetails 只可能来自验签,见 pkg/callerauth)。
	// 请求体的 session_id 不在签名范围内,只做一致性核对:填了又与会话头不同,说明发送方有缺陷,记 CRITICAL 留痕,
	// 照常按会话头收尾(docs/design/client-request-ordering.md §6.2)。不能整条拒绝(2026-11-19 评审改):不一致是发送方的
	// 确定缺陷,回错误的话 gate 会原样重发同一条通知、每次都被拒,这条连接上的玩家永远等不到断线租约,停在 ONLINE
	// (markPlayerSessionDisconnecting 是唯一入口,gate 还活着时死 gate 清扫也不会补);会话头验过签,按它收尾不会被请求体带偏。
	sessionID, consistent := disconnectSessionID(sessionDetails, in)
	if !consistent {
		logx.Severef("CRITICAL: Disconnect body session_id=%d differs from the verified session header session_id=%d gate_instance=%q incarnation=%d; settling by the header",
			in.GetSessionId(), sessionDetails.GetSessionId(), sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation())
	}

	// 没有完整连接身份的连接过不了 Login,也进不了游戏,没有任何状态要收尾。
	sessionRef, hasIdentity := loginsession.RefOf(sessionDetails)
	if !hasIdentity {
		logMissingConnectionIdentity("Disconnect", sessionDetails)
		return &login_proto.LoginEmptyResponse{}, nil
	}

	// gate 连接会话栅栏(docs/design/client-request-ordering.md §6.2):先标记这条连接已关闭,再清理登录态。
	// 之后晚到的 Login 不再写 login_session;EnterGame 后台链在绑定点看到关闭会自己补挂断线租约。
	// 反过来,后台链已经绑定了玩家、而 gate 会话还没收到 BindSession 时,从栅栏读回绑定的玩家补挂租约:
	// 这时会话头里的 player_id 是 0(首次进游戏),或是换角前的旧角色(gate 在 LeaveGame 后不清 playerId),
	// 只认会话头的话新角色会永久停在 ONLINE。两者都收尾,见 sessionPlayersToSettle。
	//
	// 只有栅栏关上、每个要收尾的玩家都挂上了断线租约才回成功(2026-11-19 改,原先任何一步失败都照回成功)。gate 持有这条通知,
	// 回成功之前一直退避重发(cpp/nodes/gate/handler/rpc/login_session_renewal.h);这里自己不重试,重试只有 gate 一层
	// (docs/design/infra-reconnect-overview.md §1.3)。所以任何一步失败都回 Unavailable,由重发补上,原样重放是安全的:
	//   - 栅栏 Close 可重放:每次都写 closed、把记录续到 sessionFenceTTL,并返回同一个绑定的玩家(Close 不删 player)。
	//     gate 的重发间隔封顶 30 秒出头,远短于 sessionFenceTTL(6 分钟),绑定在重发期间一直读得到;只有一台 gate 积压到
	//     每拍预算排不过来(约 7 万条以上)时重发才可能晚于它,那时这条通知只剩会话头带来的玩家能收尾;
	//   - 清登录态按连接身份(gate 实例 + 会话代次)删键,删过再删是空操作;防沉迷账本的 End 在一段脚本里删登记并排一条下线,
	//     登记已不在时什么都不做,所以下线只报一次;End 失败只记日志,由账本清扫在租约到期后补报(nppa/ledger.go);
	//   - SetDisconnecting 对已是 DISCONNECTING 的会话直接回成功、不延长租约;会话号、gate 实例或会话代次不符时忽略。
	bound, err := sessionfence.Close(l.ctx, l.svcCtx.LoginRedis, sessionRef.Fence(), sessionFenceTTL)
	if err != nil {
		// 栅栏没关上就什么都不收尾(fail-closed,与 LeaveGame 一致):照旧收尾的话,栅栏里绑定、会话头没有的玩家
		// (BindSession 还没到 gate)补挂不上租约,closed 没写上,晚到的后台链绑定点也看不到关闭。gate 会重发。
		logx.Errorf("Disconnect aborted: session fence close failed, nothing cleaned up, gate retries: "+
			"session_id=%d gate_instance=%q incarnation=%d err=%v",
			sessionDetails.GetSessionId(), sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation(), err)
		return nil, status.Error(codes.Unavailable, "login session fence unavailable, gate retries Disconnect")
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionRef, "disconnect")

	// 把会话转成 DISCONNECTING 并挂 30s 断线租约(取代 Centre 的 DelayedCleanupTimer)。会话键本身没有 TTL:
	// 清理靠 player_locator 的 LeaseMonitor 在租约到期后发踢人与 LeaveScene,再删会话。
	// 带上本连接所在的 gate 实例号:同号跨实例时,旧实例迟到的断线由 player_locator 按实例号忽略
	// (docs/design/scene-enter-chain-residuals.md §6)。再带本连接的会话代次:同一 gate 进程里会话号回绕后,同号旧连接迟到的断线
	// 由 player_locator 按代次忽略(docs/design/gate-online-identity.md §10.4「断线通知」)。
	// 栅栏绑定的玩家来自同一条连接(栅栏键就是实例号 + 代次),用同一个实例号与代次。
	// 每个玩家都试一次再决定回什么:一个失败不耽误另一个,重发时已挂上的那个是空操作。
	var settleErr error
	for _, playerID := range sessionPlayersToSettle(sessionDetails.PlayerId, bound) {
		if err := markPlayerSessionDisconnecting(l.ctx, l.svcCtx, playerID, sessionID,
			sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation()); err != nil {
			logx.Errorf("Disconnect incomplete: SetSessionDisconnecting failed, gate retries: player=%d session=%d gate_instance=%q incarnation=%d err=%v",
				playerID, sessionID, sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation(), err)
			settleErr = err
		}
	}
	if settleErr != nil {
		return nil, status.Error(codes.Unavailable, "player_locator SetDisconnecting failed, gate retries Disconnect")
	}

	return &login_proto.LoginEmptyResponse{}, nil
}

// disconnectSessionID 返回断线收尾用的会话号,恒为会话头里的 session_id;consistent 报告请求体是否与它一致。
// gate 合成这条通知时两处填的是同一个值(client_session_handler.cpp SendLoginDisconnectNotice);0 是 proto3 的「没填」,算一致,
// 留出 gate 以后不再填这个字段的余地。不一致时调用方只记 CRITICAL,照常按会话头收尾。
func disconnectSessionID(header *login_proto_common.SessionDetails, in *login_proto.LoginNodeDisconnectRequest) (sessionID uint32, consistent bool) {
	body := in.GetSessionId()
	return header.GetSessionId(), body == 0 || body == header.GetSessionId()
}
