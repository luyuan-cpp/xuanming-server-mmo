package handler

import (
	"go.uber.org/zap"

	"proto/scene"
	"robot/logic/gameobject"
	"robot/pkg"
)

// SceneClientPlayerCommonRedirectToGateHandler 跟随跨区重定向(msg 124)。
// 这是 login 的 HomeZone.RedirectOnEnterEnabled 开关所要求的**参考客户端实现**:
// Unity 端照着这里实现 msg 124 之前,那个开关必须保持 false。
//
// ── 完整流程 ────────────────────────────────────────────────────────────────
//
//	① 玩家在 zone A 的 gate 上登录 → login.EnterGame 发现他的 player:zone 归属是
//	   zone B(合服后的真归属),于是让 scene_manager 走跨区重定向,并**照常回成功**、
//	   清掉登录会话 —— A 这边从此不会再有任何东西推进这个玩家;
//	② A 的 gate 收到 RedirectToGateEvent,把 target_ip / target_port /
//	   token_payload / token_signature / token_deadline 打进 RedirectToGateNotify
//	   推给客户端就完事了(cpp/nodes/gate/handler/event/gate_event_handler.cpp
//	   RedirectToGateEventHandler),搬迁动作**全在客户端**;
//	③ 客户端(这里)断开 A 的 gate,连到 target_ip:target_port;
//	④ 首包发 ClientTokenVerifyRequest,payload / signature 原样来自 notify。B 的 gate
//	   重算 HMAC-SHA256(gate_token_secret, payload) 与 signature 做常数时间比较,再解出
//	   GateTokenPayload 校验 gate_node_id == 本 gate 且 expire_timestamp > now
//	   (cpp/nodes/gate/handler/rpc/client_message_processor.cpp);
//	⑤ 在新连接上**重新跑一遍 Login + EnterGame**。
//
// ── 为什么第 ⑤ 步不能省 ─────────────────────────────────────────────────────
// 票据只认证**这一条 TCP**:验过之后 B 的 gate 只是把连接标成 verified,
// B 的 login 那边没有任何会话、player_locator 里没有条目、scene 里没有实体。
// **没有跨区的登录会话转移这回事**,所以必须完整重登录;跳过它的客户端会连上
// 一个哑连接,表现为"重定向后卡死"。
//
// ── 失败时会发生什么 ───────────────────────────────────────────────────────
// 换连接**之前**失败(地址非法 / 票据已过期 / 目标不可达 / 环路熔断):老连接原封不动,
// 本次会话就地作废,外层重连兜底。换连接**之后**失败:老连接已经关了,这条会话就是死的,
// 只能等外层重来 —— 所以真正的判据在换连接之前尽量做完(见 pkg.FollowRedirect)。
func SceneClientPlayerCommonRedirectToGateHandler(player *gameobject.Player, notify *scene.RedirectToGateNotify) {
	if player == nil || notify == nil {
		return
	}
	gc := pkg.Clients.Get(player.ID)
	if gc == nil {
		// 会话正在拆:注册表里已经没有这条连接了,没什么可搬的。
		zap.L().Warn("redirect ignored: no live connection for player",
			zap.Uint64("player_id", player.ID),
			zap.String("target_ip", notify.GetTargetIp()),
			zap.Uint32("target_port", notify.GetTargetPort()))
		return
	}

	target := pkg.RedirectTarget{
		IP:        notify.GetTargetIp(),
		Port:      int(notify.GetTargetPort()),
		Payload:   notify.GetTokenPayload(),
		Signature: notify.GetTokenSignature(),
		Deadline:  notify.GetTokenDeadline(),
	}
	oldID := gc.PlayerId
	if err := pkg.FollowRedirect(gc, target); err != nil {
		zap.L().Error("follow gate redirect failed",
			zap.Uint64("player_id", player.ID),
			zap.String("target", target.Addr()),
			zap.Error(err))
		return
	}

	// 正常情况下重登录拿到的是同一个角色,player_id 不变,两张注册表都不用动。
	// 万一变了(账号首个角色换了),把同一个 Player 对象在新 id 下也登记一份:
	// MessageBodyHandler 按 client.PlayerId 反查 PlayerList,不补这一条的话
	// 重定向之后每条推送都只会打一行 "Player not found"。
	// 刻意不去改 player.ID —— 它是裸字段,别的 goroutine 正在读。
	if gc.PlayerId != oldID {
		zap.L().Warn("redirect changed player_id",
			zap.Uint64("old_player_id", oldID),
			zap.Uint64("new_player_id", gc.PlayerId))
		gameobject.PlayerList.Set(gc.PlayerId, player)
		pkg.Clients.Register(gc.PlayerId, gc)
	}

	ip, port := gc.GateAddr()
	zap.L().Info("followed gate redirect",
		zap.Uint64("player_id", gc.PlayerId),
		zap.String("gate_ip", ip),
		zap.Int("gate_port", port),
		zap.Int("hops", gc.RedirectHops()))
}
