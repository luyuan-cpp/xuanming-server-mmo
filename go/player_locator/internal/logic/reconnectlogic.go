package logic

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
)

type ReconnectLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReconnectLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReconnectLogic {
	return &ReconnectLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ReconnectLogic) Reconnect(in *pb.ReconnectRequest) (*pb.ReconnectResponse, error) {
	key := sessionKey(in.PlayerId)

	data, err := l.svcCtx.PlayerRedis.Get(l.ctx, key).Bytes()
	if err == redis.Nil {
		return &pb.ReconnectResponse{
			Success:      false,
			ErrorMessage: "no session found for player",
		}, nil
	}
	if err != nil {
		return nil, err
	}

	session := &pb.PlayerSession{}
	if err := proto.Unmarshal(data, session); err != nil {
		return nil, err
	}
	if session.State == pb.PlayerSessionState_SESSION_STATE_ONLINE &&
		session.SessionId == in.NewSessionId && session.RequestId == in.RequestId {
		// 同一幂等键 (session_id, request_id) 只有代次也相等才算 RPC response 丢失后的同请求重放
		// (docs/design/scene-enter-chain-residuals.md §3.2 处置 2)。代次不等 = 失锁时两条并发重连链复用了
		// 同一个 request_id:回成功的话,第二条链会用自己更大的铸出值发 EnterScene,location 记下 (sB, gB) 而会话
		// 记录仍是第一条链的 (sB, gA),到期清理的 LeaveScene{sB, gA} 被 scene_manager 判 keep_newer、claim 被 ack,
		// location 从此无人删除(幽灵人数)。这是 keep_newer 不漏删的前提之一。
		// 同一幂等键配不同内容按幂等的通行做法拒绝,不当成功回:不走后面的代次栅栏与 CAS,记录一个字节都不改。
		// 代价是第二条链失败、同会话收 3023,客户端走短线重连收敛,只伤活性。双方都为 0(旧版 login)照旧算重放。
		if session.LoginGeneration != in.LoginGeneration {
			l.Infof("Reconnect: replay generation mismatch rejected as stale: player=%d session=%d request_id=%s current_generation=%d incoming_generation=%d",
				in.PlayerId, session.SessionId, in.RequestId, session.LoginGeneration, in.LoginGeneration)
			return &pb.ReconnectResponse{
				Success: false,
				ErrorMessage: fmt.Sprintf("stale reconnect: replay generation mismatch current=%d incoming=%d",
					session.LoginGeneration, in.LoginGeneration),
			}, nil
		}
		// RPC response 丢失后的同请求重放。
		return &pb.ReconnectResponse{Success: true, Session: session}, nil
	}
	if session.State != pb.PlayerSessionState_SESSION_STATE_DISCONNECTING {
		return &pb.ReconnectResponse{
			Success:      false,
			ErrorMessage: "session is no longer disconnecting",
		}, nil
	}
	if in.Account != "" && session.Account != "" && in.Account != session.Account {
		return &pb.ReconnectResponse{
			Success:      false,
			ErrorMessage: "session account mismatch",
		}, nil
	}
	// 代次栅栏排在同请求重放之后:重放的代次与记录相等,先判会把丢包重试误拒。
	// 判定基于本次读到的 data,随后的整字节 CAS 以同一份 data 为 expected,判与写之间没有窗口。
	if !loginGenerationAdvances(session.LoginGeneration, in.LoginGeneration) {
		return &pb.ReconnectResponse{
			Success: false,
			ErrorMessage: fmt.Sprintf("stale login generation: current=%d incoming=%d",
				session.LoginGeneration, in.LoginGeneration),
		}, nil
	}

	// 原地改写之前留一份旧会话,只用来推导 gate 索引(这里必然是 DISCONNECTING,推导为 "",按通用规则照传)。
	previous := proto.Clone(session).(*pb.PlayerSession)

	// Update session with new connection info
	session.SessionId = in.NewSessionId
	session.GateId = in.GateId
	session.GateInstanceId = in.GateInstanceId
	session.TokenId = in.TokenId
	session.TokenExpiryMs = in.TokenExpiryMs
	session.RequestId = in.RequestId
	// 请求为 0(旧版 login)也照写 0:记录如实表示这条会话没有代次,不保留上一会话的值。
	session.LoginGeneration = in.LoginGeneration
	session.SessionVersion++
	session.State = pb.PlayerSessionState_SESSION_STATE_ONLINE
	session.LastActiveTs = time.Now().UnixMilli()

	updated, err := proto.Marshal(session)
	if err != nil {
		return nil, err
	}

	// ONLINE 写入、ready ZREM、processing claim 撤销在同一 Lua 里完成。
	// monitor 若已 claim 旧会话，其 token/payload 会在这里一起失效。
	result, err := replaceSessionAndCancelLease(l.ctx, l.svcCtx, in.PlayerId, data, updated, 0, previous, session)
	if err != nil {
		return nil, err
	}
	if result != replaceSessionApplied {
		// 不自动重放读改写；调用方重新走登录决策，避免基于陈旧 session 生成新会话。
		return &pb.ReconnectResponse{
			Success:      false,
			ErrorMessage: "session changed concurrently",
		}, nil
	}

	l.Infof("Reconnect: player=%d new_session=%d gate=%s version=%d login_generation=%d",
		in.PlayerId, in.NewSessionId, in.GateId, session.SessionVersion, session.LoginGeneration)

	return &pb.ReconnectResponse{
		Success: true,
		Session: session,
	}, nil
}
