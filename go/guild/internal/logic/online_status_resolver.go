package logic

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	plpb "proto/player_locator"
)

const playerSessionKeyPrefix = "player:session:"

// playerSessionKey 拼 player_locator 维护的会话键(契约 key,只读不写)。
// 抽成函数是为了让"在线判定"与"推送路由"读的是同一把键 —— 两处各拼一次
// 迟早有一处漏改前缀。
func playerSessionKey(playerID uint64) string {
	return fmt.Sprintf("%s%d", playerSessionKeyPrefix, playerID)
}

// onlineSessionFrom 解一条 MGET 结果。
//
// 第二个返回值只有 SESSION_STATE_ONLINE 才为 true(与 match push.go 同判据):
// 断线等待重连期间的会话虽然还在,但 gate 上已经没有连接,推过去只会被丢弃。
// 解不开、类型不对、键不存在(value 为 nil,落到 default)一律按"不在线"处理 ——
// 这是推送路径,拿不准就不推,比推给错误的会话安全。
func onlineSessionFrom(value any) (*plpb.PlayerSession, bool) {
	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return nil, false
	}

	session := &plpb.PlayerSession{}
	if err := proto.Unmarshal(raw, session); err != nil {
		return nil, false
	}
	return session, session.GetState() == plpb.PlayerSessionState_SESSION_STATE_ONLINE
}

// OnlineStatusResolver resolves player online state from player_locator session store.
type OnlineStatusResolver struct {
	rdb *redis.Client
}

func NewOnlineStatusResolver(rdb *redis.Client) *OnlineStatusResolver {
	return &OnlineStatusResolver{rdb: rdb}
}

func (r *OnlineStatusResolver) BatchResolve(ctx context.Context, playerIDs []uint64) map[uint64]bool {
	onlineMap := make(map[uint64]bool, len(playerIDs))
	if r == nil || r.rdb == nil || len(playerIDs) == 0 {
		return onlineMap
	}

	keys := make([]string, 0, len(playerIDs))
	ids := make([]uint64, 0, len(playerIDs))
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if _, ok := seen[playerID]; ok {
			continue
		}
		seen[playerID] = struct{}{}
		ids = append(ids, playerID)
		keys = append(keys, playerSessionKey(playerID))
	}

	values, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		logx.Errorf("batch get player sessions failed: %v", err)
		return onlineMap
	}
	// MGET 返回值与 keys 一一对应;长度对不上就不能按下标回填,
	// 否则会把 A 的在线状态记到 B 头上(展示成员列表时表现为随机的在线标记)。
	if len(values) != len(ids) {
		logx.Errorf("batch get player sessions: MGET returned %d values for %d keys", len(values), len(ids))
		return onlineMap
	}

	for i, value := range values {
		if _, online := onlineSessionFrom(value); online {
			onlineMap[ids[i]] = true
		}
	}

	return onlineMap
}
