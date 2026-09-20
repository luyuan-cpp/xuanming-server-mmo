package logic

import (
	"context"
	"time"

	"match/internal/metrics"
	"match/internal/playercontract"
	"match/internal/svc"

	plpb "proto/player_locator"

	"google.golang.org/protobuf/proto"
)

// 会话读取、在线判定与推送的实现收口在 playercontract(team-system.md §A.2),
// 本文件只保留 logic 内部的转调,调用点与语义不变。match_kafka_push_total 只在这里记:
// playercontract 本身不记指标,team 的推送不进这个挑战链路指标。

// pushTimeout 覆盖一次推送的整体预算(读会话 + 序列化 + WriteMessages);writer
// 自身另有 WriteTimeout 有界重试(见 servicecontext)。
const pushTimeout = 5 * time.Second

// loadPlayerSession 读 player_locator 维护的 player:session:{id}
// (契约 key,只经 SharedRedis 读)。返回 (nil, nil) 表示键不存在。
func loadPlayerSession(svcCtx *svc.ServiceContext, playerId uint64) (*plpb.PlayerSession, error) {
	return playercontract.LoadSession(context.Background(), svcCtx, playerId)
}

// isSessionOnline 判定会话在线(仅 ONLINE;断线等待重连期间不弹挑战窗)。
func isSessionOnline(session *plpb.PlayerSession) bool {
	return playercontract.IsSessionOnline(session)
}

// errPlayerOffline 与 playercontract.ErrPlayerOffline 是同一个值,errors.Is 判定不变。
var errPlayerOffline = playercontract.ErrPlayerOffline

// pushToPlayer 把 S2C 消息经 Kafka gate-cmd_g<N> 的 gate_node_id % P 号分区推给玩家客户端
// (设计决策 D6 下行路径;共享收口 kafkautil.PushToPlayer 会 fail-closed
// 拒绝空 gate_instance_id,防僵尸不变量)。预算固定 pushTimeout,不继承请求 ctx。
// 指标口径与改造前一致:成功记 ok,其余(读会话失败 / 不在线 / 序列化失败 / Kafka 失败)记 error。
func pushToPlayer(svcCtx *svc.ServiceContext, playerId uint64, messageId uint32, msg proto.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	if err := playercontract.PushToPlayer(ctx, svcCtx, playerId, messageId, msg); err != nil {
		metrics.ObserveKafkaPush("error")
		return err
	}
	metrics.ObserveKafkaPush("ok")
	return nil
}
