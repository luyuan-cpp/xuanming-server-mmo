package logic

import (
	"context"
	"fmt"
	"time"

	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"

	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

// battle 节点补签 RPC 超时(与观战 AddObserver 同量级:battle 侧只是查表 + HMAC,毫秒级)。
const issueBattleTicketTimeout = 3 * time.Second

// 补签走的 battle 节点 RPC 可替换缝(单测用 fake 顶替真 gRPC;与 gather.go 的
// prepareBattleFn 等、spectate.go 的 addObserverFn 同一模式)。生产实现只做
// "建连 + 发一次请求",身份 / 索引 / 节点定位判定全留在 logic 里,单测覆盖判定不起 battle 节点。
var issueBattleTicketFn = issueBattleTicketRPC

// issueBattleTicketRPC 是 issueBattleTicketFn 的生产实现。
func issueBattleTicketRPC(endpoint string, req *battlepb.IssueBattleTicketRequest) (*battlepb.IssueBattleTicketResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), issueBattleTicketTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).IssueBattleTicket(ctx, req)
}

type RequestBattleTicketLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRequestBattleTicketLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RequestBattleTicketLogic {
	return &RequestBattleTicketLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RequestBattleTicket 丢票补签(turn-based §18 D25;改道见 client-rpc-router.md D33):
//   - player_id 只取 gate 注入的会话身份(x-session-detail-bin → ctxkeys);请求体没有也不该有
//     player_id。没有会话身份即不可信来源,fail-closed;
//   - 按 battle_id 读观战索引 spectate:battle:{battle_id}(gather 成功登记,TTL = 战斗时限 + 60s)
//     取 battle_node_id;索引不存在 = 房间已结束 / 作废,回 kInvalidParameter —— 与 battle 侧
//     "房间不存在"同一 tip,客户端据此丢弃本地战斗 UI;
//   - 定位 battle 节点 gRPC 地址(EndpointOfNode;未发现 / 身份歧义 = kServiceUnavailable),
//     调 BattleNode.IssueBattleTicket;battle 核对名单并自签,match 不持票据密钥、不复制名单;
//   - battle 的 error_message / assignment 原样搬进响应,不改写语义(不在名单 = kInvalidParameter,
//     签不出票 = kServiceUnavailable,均由 battle 决定)。
//
// 为什么走 match 而不是 gate→battle 直达:路由服不转发 battle 消息(D33),而票据发放必须由已鉴权的
// 大厅会话背书 —— match 是客户端协议里唯一同时掌握"会话身份 + 房间所在节点"的服务。
func (l *RequestBattleTicketLogic) RequestBattleTicket(in *battlepb.RequestBattleTicketRequest) (*battlepb.RequestBattleTicketResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[ticket] RequestBattleTicket 缺少会话身份 battle=%d", in.GetBattleId())
		metrics.ObserveRequestBattleTicket("no_session")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	playerId := detail.GetPlayerId()

	record, err := loadSpectateBattle(l.svcCtx, in.GetBattleId())
	if err != nil {
		l.Errorf("[ticket] RequestBattleTicket 读观战索引失败 player=%d battle=%d: %v",
			playerId, in.GetBattleId(), err)
		metrics.ObserveRequestBattleTicket("internal")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if record == nil {
		l.Infof("[ticket] RequestBattleTicket 房间已不存在 player=%d battle=%d", playerId, in.GetBattleId())
		metrics.ObserveRequestBattleTicket("not_found")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "该战斗不存在或已结束"),
		}, nil
	}

	endpoint, err := l.svcCtx.BattleNodes.EndpointOfNode(record.GetBattleNodeId())
	if err != nil {
		l.Errorf("[ticket] RequestBattleTicket 定位 battle 节点失败 player=%d battle=%d node=%d: %v",
			playerId, in.GetBattleId(), record.GetBattleNodeId(), err)
		metrics.ObserveRequestBattleTicket("no_node")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kServiceUnavailable), "战斗服务暂不可用"),
		}, nil
	}

	resp, err := issueBattleTicketFn(endpoint, &battlepb.IssueBattleTicketRequest{
		BattleId: in.GetBattleId(),
		PlayerId: playerId,
	})
	if err != nil {
		l.Errorf("[ticket] IssueBattleTicket RPC 失败 player=%d battle=%d node=%d(%s): %v",
			playerId, in.GetBattleId(), record.GetBattleNodeId(), endpoint, err)
		metrics.ObserveRequestBattleTicket("rpc_error")
		return &battlepb.RequestBattleTicketResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kServiceUnavailable), "战斗服务暂不可用"),
		}, nil
	}

	if tipId := resp.GetErrorMessage().GetId(); tipId != 0 {
		l.Infof("[ticket] battle 节点拒签 player=%d battle=%d node=%d tip_id=%d",
			playerId, in.GetBattleId(), record.GetBattleNodeId(), tipId)
		metrics.ObserveRequestBattleTicket("rejected")
	} else {
		l.Infof("[ticket] 补签成功 player=%d battle=%d node=%d role=%s",
			playerId, in.GetBattleId(), record.GetBattleNodeId(), resp.GetAssignment().GetRole().String())
		metrics.ObserveRequestBattleTicket("ok")
	}
	// battle 的裁决原样透传:tip 与 assignment 两字段都由 battle 决定,match 不改写。
	return &battlepb.RequestBattleTicketResponse{
		ErrorMessage: resp.GetErrorMessage(),
		Assignment:   resp.GetAssignment(),
	}, nil
}
