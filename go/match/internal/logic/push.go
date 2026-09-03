package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	plpb "proto/player_locator"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"shared/kafkautil"
)

// pushTimeout 覆盖 build + WriteMessages 的整体预算;writer 自身另有
// WriteTimeout 有界重试(见 servicecontext)。
const pushTimeout = 5 * time.Second

// loadPlayerSession 读 player_locator 维护的 player:session:{id}
// (照 guild online_status_resolver 的读法,只是这里走 go-zero redis;
// 契约 key,只经 SharedRedis 读)。返回 (nil, nil) 表示键不存在。
func loadPlayerSession(svcCtx *svc.ServiceContext, playerId uint64) (*plpb.PlayerSession, error) {
	raw, err := svcCtx.SharedRedis.Get(playerSessionKey(playerId))
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	session := &plpb.PlayerSession{}
	if err := proto.Unmarshal([]byte(raw), session); err != nil {
		return nil, fmt.Errorf("PlayerSession 反序列化失败: %w", err)
	}
	return session, nil
}

// isSessionOnline 判定会话在线(仅 ONLINE;断线等待重连期间不弹挑战窗)。
func isSessionOnline(session *plpb.PlayerSession) bool {
	return session != nil && session.State == plpb.PlayerSessionState_SESSION_STATE_ONLINE
}

var errPlayerOffline = errors.New("玩家不在线")

// pushToPlayer 把 S2C 消息经 Kafka gate-{id} 推给玩家客户端
// (设计决策 D6 下行路径;共享收口 kafkautil.PushToPlayer 会 fail-closed
// 拒绝空 gate_instance_id,防僵尸不变量)。
func pushToPlayer(svcCtx *svc.ServiceContext, playerId uint64, messageId uint32, msg proto.Message) error {
	session, err := loadPlayerSession(svcCtx, playerId)
	if err != nil {
		metrics.ObserveKafkaPush("error")
		return fmt.Errorf("读玩家会话失败: %w", err)
	}
	if !isSessionOnline(session) {
		metrics.ObserveKafkaPush("error")
		return errPlayerOffline
	}

	body, err := proto.Marshal(msg)
	if err != nil {
		metrics.ObserveKafkaPush("error")
		return fmt.Errorf("序列化 S2C 失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	err = kafkautil.PushToPlayer(ctx, svcCtx.Kafka, svcCtx.GateCommandBuilder, kafkautil.PlayerGateInfo{
		PlayerID:       playerId,
		SessionID:      session.SessionId,
		GateID:         session.GateId,
		GateInstanceID: session.GateInstanceId,
	}, messageId, body)
	if err != nil {
		metrics.ObserveKafkaPush("error")
		logx.Errorf("[match] 推送失败 player=%d message_id=%d: %v", playerId, messageId, err)
		return err
	}
	metrics.ObserveKafkaPush("ok")
	return nil
}
