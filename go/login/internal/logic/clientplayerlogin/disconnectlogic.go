package clientplayerloginlogic

import (
	"context"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
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
	// 照常按会话头收尾(docs/design/client-request-ordering.md §6.2)。不能整条拒绝(2026-11-20 评审改):这条通知由 gate
	// 在断线时合成、只发一次、没人重发,拒绝就等于这条连接上的玩家永久停在 ONLINE(markPlayerSessionDisconnecting
	// 是唯一入口,gate 还活着时死 gate 清扫也不会补);会话头验过签,按它收尾不会被请求体带偏。
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
	bound, err := sessionfence.Close(l.ctx, l.svcCtx.LoginRedis, sessionRef.Fence(), sessionFenceTTL)
	if err != nil {
		// 栅栏写失败不阻断清理(fail-open,与 LeaveGame 的 fail-closed 相反,2026-11-10 复核理由仍成立):
		// Disconnect 由 gate 在断线时合成、只发一次,回包被丢弃(会话已 erase),没有任何人会重发
		// (client_session_handler.cpp 断线通知)。这里回错误什么也换不来,只会连会话头那个玩家的断线租约也挂不上 ——
		// 那是它唯一的清理入口,丢了就永久停在 ONLINE。所以照旧按会话里的身份清,只失去两层兜底:
		// 栅栏里绑定、会话头没有的玩家(BindSession 还没到 gate)不补挂租约;closed 没写上,晚到的后台链绑定点
		// 看不到关闭。两者都要 LoginRedis 恰好在这一刻失败。残留的玩家按 release-checklist 附录 F.B-1「定向补救」
		// 逐个补投断线(gate 还活着时会话对账看不到它们);不做全键 SCAN。
		logx.Errorf("Disconnect session fence close failed: sessionId=%d err=%v", sessionDetails.SessionId, err)
		bound = 0
	}

	cleanupLoginSessionState(l.ctx, l.svcCtx, sessionRef, "disconnect")

	// 把会话转成 DISCONNECTING 并挂 30s 断线租约(取代 Centre 的 DelayedCleanupTimer)。会话键本身没有 TTL:
	// 清理靠 player_locator 的 LeaseMonitor 在租约到期后发踢人与 LeaveScene,再删会话。
	// 带上本连接所在的 gate 实例号:同号跨实例时,旧实例迟到的断线由 player_locator 按实例号忽略
	// (docs/design/scene-enter-chain-residuals.md §6)。栅栏绑定的玩家来自同一个含实例号的栅栏键,用同一个实例号。
	for _, playerID := range sessionPlayersToSettle(sessionDetails.PlayerId, bound) {
		markPlayerSessionDisconnecting(l.ctx, l.svcCtx, playerID, sessionID, sessionDetails.GetGateInstanceId())
	}

	return &login_proto.LoginEmptyResponse{}, nil
}

// disconnectSessionID 返回断线收尾用的会话号,恒为会话头里的 session_id;consistent 报告请求体是否与它一致。
// gate 合成这条通知时两处填的是同一个值(client_session_handler.cpp 断线通知);0 是 proto3 的「没填」,算一致,
// 留出 gate 以后不再填这个字段的余地。不一致时调用方只记 CRITICAL,照常按会话头收尾。
func disconnectSessionID(header *login_proto_common.SessionDetails, in *login_proto.LoginNodeDisconnectRequest) (sessionID uint32, consistent bool) {
	body := in.GetSessionId()
	return header.GetSessionId(), body == 0 || body == header.GetSessionId()
}
