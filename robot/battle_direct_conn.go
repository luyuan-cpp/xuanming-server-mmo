package main

// 战斗直连(docs/design/turn-based-battle-server.md §18):机器人收到 NotifyBattleAssigned
// 后,凭票据向 battle 节点客户端面建第二条 TCP 连接。线协议与 gate 完全一致,所以直接
// 复用 pkg.GameClient(同一个 muduo codec):握手用 VerifyBattleToken,之后 SendRequest /
// RecvLoop 照常,收到的 S2C 按消息号计数后交给同一份 handler.MessageBodyHandler ——
// 参战 / 观战 handler 不感知消息来自哪条连接,这正是"客户端两套连接共用一套分发"的契约。
//
// 计数器用于冒烟断言"回合结果 / 终局包确实从直连到达"(而不是经 gate 回落):
// 直连建立之后 battle 节点对该玩家的 S2C 必须直发,gate 那条连接上不该再出现它们。

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	"robot/generated/pb/game"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
)

// battleDirectConnTimeout 是"等落点分配 + 建连 + 握手"的总预算,三段共用同一个 ctx。
// 落点分配先于开战包下发(D26),正常情况下机器人等到 BattleStart 时它早已到达;10s 是给
// Kafka→gate 回落链路的余量。
//
// 建连 + 握手必须受它约束:底层 muduo 客户端 NewClient 永不报错、拨不通每 1s 无限重拨,
// VerifyBattleToken 里的 Recv 阻塞在一条永不 close 的通道上 —— battle 端口从 robot 所在
// 机器不可达时(NODE_IP 注册地址 / K8s 无 hostPort,§18.6 明写的场景),没有超时就是整个
// 冒烟进程挂死、既不打 OK 也不打 FAIL。§18.2 把"直连建不起来"列为正常回落情形,参考
// 客户端必须能在这条路径上**失败退出**。
const battleDirectConnTimeout = 10 * time.Second

// battleDirectConn 是一条已完成票据握手的战斗直连。
type battleDirectConn struct {
	gc       *pkg.GameClient
	battleId uint64
	role     string

	// 从直连收到的各类 S2C 条数(参战 / 观战两套消息号分开计)
	turnResults   atomic.Int64
	battleEnds    atomic.Int64
	spectateTurns atomic.Int64
	spectateEnds  atomic.Int64
	total         atomic.Int64
}

// openBattleDirectConn 等待该机器人的 NotifyBattleAssigned,连到 battle 节点并完成握手,
// 然后起 RecvLoop 把直连 S2C 分发给既有 handler。stats 只做收发计数。
func openBattleDirectConn(bot *battleSmokeBot, stats *metrics.Stats) (*battleDirectConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), battleDirectConnTimeout)
	defer cancel()
	assigned, err := bot.player.WaitBattleAssigned(ctx)
	if err != nil {
		return nil, fmt.Errorf("no BattleAssigned within %s: %w", battleDirectConnTimeout, err)
	}
	if assigned.GetHost() == "" || assigned.GetPort() == 0 {
		return nil, fmt.Errorf("BattleAssigned has empty endpoint: host=%q port=%d",
			assigned.GetHost(), assigned.GetPort())
	}

	// 建连 + 握手放进 goroutine,用同一个 ctx 兜底(见 battleDirectConnTimeout 注释)。
	type handshakeResult struct {
		gc       *pkg.GameClient
		battleId uint64
		err      error
	}
	done := make(chan handshakeResult, 1)
	go func() {
		gc, err := pkg.NewGameClient(assigned.GetHost(), int(assigned.GetPort()))
		if err != nil {
			done <- handshakeResult{err: fmt.Errorf("connect battle node %s:%d: %w",
				assigned.GetHost(), assigned.GetPort(), err)}
			return
		}
		// 同一个玩家:handler.MessageBodyHandler 按 client.PlayerId 找 gameobject.Player,
		// 直连上收到的 S2C 才能落到与大厅连接同一个 Player 的状态机上。
		gc.Account = bot.account
		gc.PlayerId = bot.gc.PlayerId

		battleId, err := gc.VerifyBattleToken(assigned.GetTokenPayload(), assigned.GetTokenSignature())
		if err != nil {
			gc.Close()
			done <- handshakeResult{err: fmt.Errorf("battle ticket handshake: %w", err)}
			return
		}
		done <- handshakeResult{gc: gc, battleId: battleId}
	}()

	var gc *pkg.GameClient
	var battleId uint64
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		gc, battleId = r.gc, r.battleId
	case <-ctx.Done():
		// 超时后 goroutine 若还在 Recv 里阻塞会泄漏(muduo 客户端不 close incoming 通道);
		// 冒烟进程随后就退出,可接受。等它真的返回时把连接关掉,避免继续重拨。
		go func() {
			if r := <-done; r.gc != nil {
				r.gc.Close()
			}
		}()
		return nil, fmt.Errorf("battle direct handshake to %s:%d timed out after %s: %w",
			assigned.GetHost(), assigned.GetPort(), battleDirectConnTimeout, ctx.Err())
	}
	if battleId != assigned.GetBattleId() {
		gc.Close()
		return nil, fmt.Errorf("handshake battle_id=%d != assigned battle_id=%d", battleId, assigned.GetBattleId())
	}

	conn := &battleDirectConn{gc: gc, battleId: battleId, role: assigned.GetRole().String()}
	go gc.RecvLoop(func(client *pkg.GameClient, msg *base.MessageContent) {
		stats.MsgRecv()
		conn.total.Add(1)
		switch msg.MessageId {
		case game.BattleClientPlayerNotifyTurnResultMessageId:
			conn.turnResults.Add(1)
		case game.BattleClientPlayerNotifyBattleEndMessageId:
			conn.battleEnds.Add(1)
		case game.BattleClientPlayerNotifySpectateTurnResultMessageId:
			conn.spectateTurns.Add(1)
		case game.BattleClientPlayerNotifySpectateEndMessageId:
			conn.spectateEnds.Add(1)
		}
		if msg.ErrorMessage != nil && msg.ErrorMessage.Id != 0 {
			// 直连面的信封错误(越权消息号 / 超长包 / 解析失败),body 为空,不再分发
			zap.L().Error("battle direct-connect envelope error",
				zap.Uint32("message_id", msg.MessageId), zap.Uint32("tip", msg.ErrorMessage.Id))
			return
		}
		handler.MessageBodyHandler(client, msg)
	})

	zap.L().Info("[battle-direct] handshake ok",
		zap.String("account", bot.account),
		zap.Uint64("battle_id", battleId),
		zap.String("role", conn.role),
		zap.String("endpoint", fmt.Sprintf("%s:%d", assigned.GetHost(), assigned.GetPort())),
		zap.Bool("signed", len(assigned.GetTokenSignature()) > 0))
	return conn, nil
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
	return fmt.Sprintf("total=%d turn_results=%d battle_ends=%d spectate_turns=%d spectate_ends=%d",
		c.total.Load(), c.turnResults.Load(), c.battleEnds.Load(), c.spectateTurns.Load(), c.spectateEnds.Load())
}
