// Package playercontract 是 match 进程内"跨运行时玩家契约 key"的唯一实现:
// key 拼法、读取与解析、在线判定、经 gate Kafka 的下行推送。
//
// 为什么单独成包(docs/design/team-system.md §A.2):logic(排队 / 切磋 / 观战)与
// team(组队)都要读会话、位置、战斗锁并推送。这些原本是 logic 的未导出函数,
// team 引用不到,反向 import 又会成环;在 team 里手抄 key 拼法则让契约出现第二份
// 真相(AGENTS.md §11.2 DRY)。
//
// 依赖方向:logic → playercontract、team → playercontract;本包只依赖 svc / proto / shared,
// **不得** import logic 或 team。本包不记任何指标:推送结果由调用方记在各自的指标上
// (logic 记 match_kafka_push_total,team 记 team_push_total),互不串口径。
//
// 契约 key(写者都不是 match;match 只读,并且只经 svcCtx.SharedRedis 读):
//
//	player:session:{id}    player_locator 写  PlayerSession protobuf
//	player:{id}:location   scene_manager 写   PlayerLocation protobuf
//	battle:lock:{id}       C++ scene 写       咨询性"是否在战斗中"(权威在 scene 的 InBattleComp)
//
// key 字面量是跨进程契约(C++ scene / scene_manager / player_locator 各自拼同一串),
// 改这里等于单方面改协议;playercontract_test.go 钉住了字面量。
//
// 所有读函数都收 ctx:调用方的超时预算(team 的请求预算、推送批次预算)必须能截断
// Redis 往返。不关心预算的旧调用点传 context.Background(),行为与 go-zero 无 ctx 版本
// 完全一致(go-zero 的 Get(key) 就是 GetCtx(context.Background(), key))。
package playercontract

import (
	"context"
	"errors"
	"fmt"

	"match/internal/svc"

	plpb "proto/player_locator"
	smpb "proto/scene_manager"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"shared/kafkautil"
)

// SessionKey 返回 player_locator 维护的会话键(值为 PlayerSession protobuf)。
func SessionKey(playerId uint64) string {
	return fmt.Sprintf("player:session:%d", playerId)
}

// LocationKey 返回 scene_manager 维护的玩家位置权威键(值为 PlayerLocation protobuf;
// 写者 go/scene_manager changesceneutil.go,契约记录见 cross-zone-matchmaking.md §5.4)。
func LocationKey(playerId uint64) string {
	return fmt.Sprintf("player:%d:location", playerId)
}

// BattleLockKey 返回 C++ scene 写的战斗锁键;match 只做咨询性 Exists。
func BattleLockKey(playerId uint64) string {
	return fmt.Sprintf("battle:lock:%d", playerId)
}

// ErrPlayerOffline 表示推送目标没有 ONLINE 会话(键不存在,或处于断线 / 离线态)。
// 调用方用 errors.Is 区分"不在线"(至多一次投递下的正常情况)与 Redis / Kafka 故障。
var ErrPlayerOffline = errors.New("玩家不在线")

// ParseSession 解析一条 player:session:{id} 的原始值。raw 为空串(键不存在,
// 包括 MGET 的缺失项)返回 (nil, nil);反序列化失败返回错误。
// 批量读(MGET)的调用方用它逐项解析,与 LoadSession 共用同一份解析。
func ParseSession(raw string) (*plpb.PlayerSession, error) {
	if raw == "" {
		return nil, nil
	}
	session := &plpb.PlayerSession{}
	if err := proto.Unmarshal([]byte(raw), session); err != nil {
		return nil, fmt.Errorf("PlayerSession 反序列化失败: %w", err)
	}
	return session, nil
}

// LoadSession 读 player:session:{id}(照 guild online_status_resolver 的读法,
// 这里走 go-zero redis)。键不存在返回 (nil, nil);Redis 错误原样返回、不包装,
// 由调用方各自补上下文;反序列化失败返回错误。
func LoadSession(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64) (*plpb.PlayerSession, error) {
	raw, err := svcCtx.SharedRedis.GetCtx(ctx, SessionKey(playerId))
	if err != nil {
		return nil, err
	}
	return ParseSession(raw)
}

// IsSessionOnline 判定会话在线:只有 SESSION_STATE_ONLINE 算在线。断线等重连
// (DISCONNECTING,player_locator 的 30s 宽限)期间 key 仍在但不算在线 ——
// 不弹挑战窗、不推组队邀请。nil 视为不在线。
func IsSessionOnline(session *plpb.PlayerSession) bool {
	return session != nil && session.State == plpb.PlayerSessionState_SESSION_STATE_ONLINE
}

// LoadLocation 读 player:{id}:location。键不存在返回 (nil, nil):玩家不在线或还没进场。
// Redis 错误与反序列化失败都包装后返回。JoinQueue 取 zone、gather 定位 scene 节点、
// 观战取观众 zone、组队开战预检共用这一份解析。
func LoadLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64) (*smpb.PlayerLocation, error) {
	raw, err := svcCtx.SharedRedis.GetCtx(ctx, LocationKey(playerId))
	if err != nil {
		return nil, fmt.Errorf("读玩家位置失败: %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), loc); err != nil {
		return nil, fmt.Errorf("玩家位置反序列化失败: %w", err)
	}
	return loc, nil
}

// IsBattleLocked 咨询性检查 battle:lock:{player_id}。权威判定仍在 scene 的 InBattleComp,
// 这里只是提前挡住明显不合法的请求。Redis 出错时返回 (true, err):fail-closed,
// 宁可拒绝排队 / 开战,也不能放进双战斗。
func IsBattleLocked(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64) (bool, error) {
	ok, err := svcCtx.SharedRedis.ExistsCtx(ctx, BattleLockKey(playerId))
	if err != nil {
		return true, fmt.Errorf("查询 battle:lock 失败: %w", err)
	}
	return ok, nil
}

// PushToPlayer 把 S2C 消息经 Kafka gate-cmd_g<N> 的 gate_node_id % P 号分区推给玩家客户端
// (cross-zone-matchmaking.md D6 下行路径)。共享收口 kafkautil.PushToPlayer 会 fail-closed
// 拒绝空 gate_instance_id(AGENTS.md §7 #2 防僵尸不变量)。
//
// 契约:
//   - 投递语义至多一次:目标没有 ONLINE 会话时不推,返回 ErrPlayerOffline;
//   - 读会话失败、序列化失败、Kafka 写失败返回错误(非 ErrPlayerOffline);
//   - ctx 同时约束读会话与 Kafka 写入,本函数**不另加超时**,预算由调用方给;
//     补偿 / 异步推送不要直接继承请求 ctx(请求结束即取消);
//   - 不记指标:match_kafka_push_total 由 logic 的 pushToPlayer 包装记(与改造前同口径),
//     team 只记 team_push_total(离线记 offline,不算 error);
//   - 线程安全:只读 svcCtx 上的共享句柄。
func PushToPlayer(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64, messageId uint32, msg proto.Message) error {
	session, err := LoadSession(ctx, svcCtx, playerId)
	if err != nil {
		return fmt.Errorf("读玩家会话失败: %w", err)
	}
	if !IsSessionOnline(session) {
		return ErrPlayerOffline
	}

	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化 S2C 失败: %w", err)
	}

	err = kafkautil.PushToPlayer(ctx, svcCtx.Kafka, svcCtx.GateCommandBuilder, kafkautil.PlayerGateInfo{
		PlayerID:       playerId,
		SessionID:      session.SessionId,
		GateID:         session.GateId,
		GateInstanceID: session.GateInstanceId,
	}, messageId, body)
	if err != nil {
		logx.Errorf("[match] 推送失败 player=%d message_id=%d: %v", playerId, messageId, err)
		return err
	}
	return nil
}
