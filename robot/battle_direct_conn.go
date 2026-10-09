package main

// 战斗直连(docs/design/turn-based-battle-server.md §18;收缩后口径见 turn-based §22 D66/D68/D69/D73):
// 直连是战斗的**唯一**通路 —— gate 两种路由模式都拒绝战斗上行(D66),battle 的战斗帧
// (TurnResult / BattleEnd / Spectate*)只走直连,没有活直连就丢帧(D68)。大厅那条连接上只剩
// 两条大厅公告 NotifyBattleAssigned / NotifyBattleStart,以及 scene 结算后推的 NotifyBattleEnd。
//
// 机器人收到 NotifyBattleAssigned 后,凭票据向 battle 节点客户端面建第二条 TCP 连接。线协议与
// gate 完全一致,所以直接复用 pkg.GameClient(同一个 muduo codec):握手用 VerifyBattleToken,
// 之后 SendRequest / RecvLoop 照常。
//
// 分两层:
//   - dialBattleDirect:按调用方给定的 BattleAssignedS2C 建连、握手、按消息号计数,再把每条 S2C
//     交给调用方的回调。team-smoke(同一会话打多场,不能用 Player 上的一次性 Assigned 信号)与
//     features-smoke(有自己的收包回调)直接用它;
//   - openBattleDirectConn:等 Player 上的 Assigned 信号再拨号,回调是通用分发
//     dispatchBattleDirectDefault —— battle-smoke / 跨 zone 冒烟用。
//
// 就绪后的第一帧(D69):观众由 battle 在握手成功时经直连推一份 NotifySpectateState(握手快照);
// 参战者没有握手快照,由客户端在直连就绪时 GetBattleState 补拉 —— dialBattleDirect 替参战者
// 发这一条,与 Unity 客户端同口径。
//
// 计数器供冒烟断言"首帧 / 回合结果 / 终局包确实从直连到达"。

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/battle"
	base "proto/common/base"
	"robot/generated/pb/game"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
)

// battleDirectConnTimeout 是"建连 + 握手"的预算;openBattleDirectConn 等落点分配另用一份同值预算。
// 落点分配先于开战包下发(D26/D70),正常情况下机器人等到 BattleStart 时它早已到达。
//
// 建连 + 握手必须受它约束:底层 muduo 客户端 NewClient 永不报错、拨不通每 1s 无限重拨,
// VerifyBattleToken 里的 Recv 阻塞在一条永不 close 的通道上 —— battle 端口从 robot 所在
// 机器不可达时(NODE_IP 注册地址 / K8s 无 hostPort,§18.6 明写的场景),没有超时就是整个
// 冒烟进程挂死、既不打 OK 也不打 FAIL。收缩后直连建不起来 = 本局无法战斗(D68:上行被 gate
// 拒、战斗帧不再经 gate 回落),参考客户端必须能在这条路径上**失败退出**。
const battleDirectConnTimeout = 10 * time.Second

// battleDirectConn 是一条已完成票据握手的战斗直连。
type battleDirectConn struct {
	gc       *pkg.GameClient
	battleId uint64
	role     battle.EBattleTicketRole

	// 从直连收到的各类 S2C 条数(参战 / 观战两套消息号分开计)
	turnResults    atomic.Int64
	battleEnds     atomic.Int64
	spectateStates atomic.Int64 // 观众握手快照(D69)与之后的观战全量快照
	spectateTurns  atomic.Int64
	spectateEnds   atomic.Int64
	stateReplies   atomic.Int64 // GetBattleState 的成功应答(参战者就绪补拉,D69)
	total          atomic.Int64
}

// openBattleDirectConn 等待该机器人的 NotifyBattleAssigned,再用 dialBattleDirect 连到 battle
// 节点并完成握手;直连 S2C 经 dispatchBattleDirectDefault 交给既有 handler。stats 只做收发计数。
//
// 只适合"一个 Player 只打一场"的场景:WaitBattleAssigned 是一次性广播,第二场起会读到上一场的
// 分配。同一会话打多场的场景(team-smoke)按 battle_id 自己找分配,直接调 dialBattleDirect。
func openBattleDirectConn(bot *battleSmokeBot, stats *metrics.Stats) (*battleDirectConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), battleDirectConnTimeout)
	assigned, err := bot.player.WaitBattleAssigned(ctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("no BattleAssigned within %s: %w", battleDirectConnTimeout, err)
	}
	return dialBattleDirect(bot.account, bot.gc.PlayerId, assigned, stats, dispatchBattleDirectDefault)
}

// dispatchBattleDirectDefault 是 openBattleDirectConn 用的直连回调:交给通用的
// handler.MessageBodyHandler。信封错误(越权消息号 / 超长包 / 限速 / 解析失败)的 body 为空,
// 交给通用分发会被当成一条"成功的空应答",所以只由 record 打日志、不再分发。
func dispatchBattleDirectDefault(client *pkg.GameClient, msg *base.MessageContent) {
	if msg.GetErrorMessage().GetId() != 0 {
		return
	}
	handler.MessageBodyHandler(client, msg)
}

// dialBattleDirect 按 assigned 给的 host:port + 票据连到 battle 节点客户端面并完成握手,
// 起 RecvLoop:每条 S2C 先按消息号计数(record),再原样交给 onMessage(信封错误也照交,
// 由调用方决定是否分发)。
//
// account / playerId 写进直连的 GameClient:handler.MessageBodyHandler 按 client.PlayerId 找
// gameobject.Player,直连上收到的 S2C 才能落到与大厅连接同一个 Player 的状态机上。
// stats 为 nil 时不计收发(调用方的 onMessage 自己计的情况,见 team-smoke)。
//
// 参战者(role=PARTICIPANT)握手成功后立即经直连发一条 GetBattleState 补拉全量(D69);
// 观众不发,首帧由 battle 的握手快照推送。
//
// 失败(分配无效 / 拨不通 / 握手被拒 / 超时 / battle_id 不符)一律返回 error 且不留连接:
// 收缩后直连就是战斗唯一通路,调用方应按失败处理,不存在回落。
func dialBattleDirect(account string, playerId uint64, assigned *battle.BattleAssignedS2C, stats *metrics.Stats,
	onMessage func(*pkg.GameClient, *base.MessageContent),
) (*battleDirectConn, error) {
	if assigned == nil {
		return nil, errors.New("no BattleAssigned to dial")
	}
	if onMessage == nil {
		return nil, errors.New("dialBattleDirect requires an onMessage callback")
	}
	if assigned.GetHost() == "" || assigned.GetPort() == 0 {
		return nil, fmt.Errorf("BattleAssigned has empty endpoint: host=%q port=%d",
			assigned.GetHost(), assigned.GetPort())
	}

	ctx, cancel := context.WithTimeout(context.Background(), battleDirectConnTimeout)
	defer cancel()

	// NewGameClient 只起 muduo 的连接管理 goroutine、立即返回(拨号在后台);握手放进 goroutine,
	// 用 ctx 兜底(见 battleDirectConnTimeout 注释)。
	gc, err := pkg.NewGameClient(assigned.GetHost(), int(assigned.GetPort()))
	if err != nil {
		return nil, fmt.Errorf("connect battle node %s:%d: %w", assigned.GetHost(), assigned.GetPort(), err)
	}
	gc.Account = account
	gc.PlayerId = playerId

	type handshakeResult struct {
		battleId uint64
		err      error
	}
	done := make(chan handshakeResult, 1)
	go func() {
		battleId, err := gc.VerifyBattleToken(assigned.GetTokenPayload(), assigned.GetTokenSignature())
		done <- handshakeResult{battleId: battleId, err: err}
	}()

	var battleId uint64
	select {
	case r := <-done:
		if r.err != nil {
			gc.Close()
			return nil, fmt.Errorf("battle ticket handshake to %s:%d: %w", assigned.GetHost(), assigned.GetPort(), r.err)
		}
		battleId = r.battleId
	case <-ctx.Done():
		// 异步 Close 停止重拨,不阻塞调用方:vendored muduo 的 Connection.Close 要 wg.Wait 等连接管理
		// goroutine 退出,而它的 net.Dial 没有超时 —— 黑洞地址下 Close 要等 Dial 的 OS 超时
		// (Windows 约 21s、Linux 约 127s)才返回,同步调用会让本函数的 10s 预算失效、*_FAIL 行被推迟。
		// Dial 返回后管理 goroutine 看到 closed 即退出,不再重拨。阻塞在 Recv 上的握手 goroutine
		// 仍会泄漏(muduo 客户端不 close incoming 通道),冒烟进程随后就以 FAIL 退出,可接受。
		// 握手被拒 / battle_id 不符两条路径的连接已建立,Close 很快返回,那两处保持同步。
		go gc.Close()
		return nil, fmt.Errorf("battle direct handshake to %s:%d timed out after %s: %w",
			assigned.GetHost(), assigned.GetPort(), battleDirectConnTimeout, ctx.Err())
	}
	if battleId != assigned.GetBattleId() {
		gc.Close()
		return nil, fmt.Errorf("handshake battle_id=%d != assigned battle_id=%d", battleId, assigned.GetBattleId())
	}

	// 握手应答之后 battle 可能立刻推观众快照(D69);它先进 muduo 的 incoming 缓冲,
	// RecvLoop 起来后按到达顺序取出,不会丢。
	conn := &battleDirectConn{gc: gc, battleId: battleId, role: assigned.GetRole()}
	go gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
		if stats != nil {
			stats.MsgRecv()
		}
		conn.record(msg)
		onMessage(client, msg)
	})

	if conn.role == battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT {
		// 参战者就绪补拉(D69):开战到直连就绪之间的战斗帧 battle 不会补发(没有活直连即丢弃),
		// 全量状态以这条 GetBattleState 的应答为准。
		if err := conn.Send(game.BattleClientPlayerGetBattleStateMessageId,
			&battle.GetBattleStateRequest{BattleId: battleId}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("catch-up GetBattleState on direct connection: %w", err)
		}
		if stats != nil {
			stats.MsgSent()
		}
	}

	zap.L().Info("[battle-direct] handshake ok",
		zap.String("account", account),
		zap.Uint64("battle_id", battleId),
		zap.String("role", conn.role.String()),
		zap.String("endpoint", fmt.Sprintf("%s:%d", assigned.GetHost(), assigned.GetPort())),
		zap.Bool("signed", len(assigned.GetTokenSignature()) > 0))
	return conn, nil
}

// record 按消息号给直连收到的 S2C 计数;信封错误(body 为空、ErrorMessage 非零)只打日志,
// 不计入任何分类 —— 它不是一条有效的应答或推送。
func (c *battleDirectConn) record(msg *base.MessageContent) {
	c.total.Add(1)
	if tip := msg.GetErrorMessage().GetId(); tip != 0 {
		zap.L().Error("battle direct-connect envelope error",
			zap.Uint64("battle_id", c.battleId),
			zap.Uint32("message_id", msg.GetMessageId()), zap.Uint32("tip", tip))
		return
	}
	switch msg.GetMessageId() {
	case game.BattleClientPlayerNotifyTurnResultMessageId:
		c.turnResults.Add(1)
	case game.BattleClientPlayerNotifyBattleEndMessageId:
		c.battleEnds.Add(1)
	case game.BattleClientPlayerNotifySpectateStateMessageId:
		c.spectateStates.Add(1)
	case game.BattleClientPlayerNotifySpectateTurnResultMessageId:
		c.spectateTurns.Add(1)
	case game.BattleClientPlayerNotifySpectateEndMessageId:
		c.spectateEnds.Add(1)
	case game.BattleClientPlayerGetBattleStateMessageId:
		c.stateReplies.Add(1)
	}
}

// Send 经直连发一条战斗客户端消息(消息号只能是 BattleClientPlayer 的四条客户端 RPC)。
func (c *battleDirectConn) Send(messageId uint32, body proto.Message) error {
	return c.gc.SendRequest(messageId, body)
}

// Close 关闭直连。battle 节点在终局后会主动关,这里是机器人侧的兜底清理。
func (c *battleDirectConn) Close() {
	if c != nil && c.gc != nil {
		c.gc.Close()
	}
}

// summary 给日志 / 断言用的一行摘要。
func (c *battleDirectConn) summary() string {
	return fmt.Sprintf("total=%d state_replies=%d turn_results=%d battle_ends=%d spectate_states=%d spectate_turns=%d spectate_ends=%d",
		c.total.Load(), c.stateReplies.Load(), c.turnResults.Load(), c.battleEnds.Load(),
		c.spectateStates.Load(), c.spectateTurns.Load(), c.spectateEnds.Load())
}
