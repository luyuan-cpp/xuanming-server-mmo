package team

import (
	"context"
	"fmt"

	"match/internal/playercontract"

	dbpb "proto/common/database"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

// 在线态与展示信息(设计文档 docs/design/team-system.md §G.1 / §G.2)。
//
// 这些都是显示层数据:每次构建视图或做惰性转让判定时按需批量读,绝不写进 TeamRecord
// (AGENTS §11.6)。全部只读 SharedRedis,写者都不是 match:
//
//	player:session:<id>      player_locator  在线态唯一真值(只有 SESSION_STATE_ONLINE 算在线)
//	battle:lock:<id>         C++ scene       咨询性"战斗中"(值是 battle_id,非空)
//	<PlayerAllData 全名>:<id> login / db      等级、职业(尽力而为,缺失或解析失败填 0)
//
// 失败语义:任何一次 MGET 失败只让对应字段按"离线 / 不在战斗 / 0"处理并记日志,
// 不让 RPC 失败(照 guild online_status_resolver)。惰性转让用的 SessionLoader 例外:
// 读失败给 SessionUnknown,规则层据此既不转让队长、也不把人当在线(fail-closed)。

// memberDisplay 视图里一名玩家的运行时展示信息。
type memberDisplay struct {
	Online   bool
	InBattle bool
	Level    uint32
	ClassId  uint32
}

// displayCache player_id → 展示信息。一次提交 / 一次读只构建一份,所有接收者复用(§G.2)。
type displayCache map[uint64]memberDisplay

// presenceReader 批量读取在线态与展示信息。无可变状态,可并发使用。
type presenceReader struct {
	rds *redis.Redis // svcCtx.SharedRedis
}

// playerAllDataKeyPrefix PlayerAllData 缓存 key 前缀取 message 全名,与 login 的拼法同源
// (player_class_backfill.go:%s:%d),不手写字面量。
var playerAllDataKeyPrefix = string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())

func playerAllDataKey(playerId uint64) string {
	return fmt.Sprintf("%s:%d", playerAllDataKeyPrefix, playerId)
}

// loadSessions 实现 SessionLoader:key 不存在 → SessionAbsent;ONLINE → SessionOnline;
// 其余状态 → SessionPresent;MGET 整批失败或单项解析失败 → 不填(SessionUnknown)。
func (p presenceReader) loadSessions(ctx context.Context, playerIds []uint64) Sessions {
	out := Sessions{}
	ids := uniqueIds(playerIds)
	values := p.mget(ctx, ids, playercontract.SessionKey, "player:session")
	if values == nil {
		return out
	}
	for i, raw := range values {
		if raw == "" {
			out[ids[i]] = SessionAbsent
			continue
		}
		session, err := playercontract.ParseSession(raw)
		if err != nil {
			logx.WithContext(ctx).Errorf("[team] 会话解析失败 player=%d: %v", ids[i], err)
			continue
		}
		if playercontract.IsSessionOnline(session) {
			out[ids[i]] = SessionOnline
		} else {
			out[ids[i]] = SessionPresent
		}
	}
	return out
}

// loadDisplay 批量读 playerIds 的在线态、战斗锁与等级职业。
func (p presenceReader) loadDisplay(ctx context.Context, playerIds []uint64) displayCache {
	ids := uniqueIds(playerIds)
	dc := make(displayCache, len(ids))
	if len(ids) == 0 {
		return dc
	}
	sessions := p.loadSessions(ctx, ids)
	locks := p.mget(ctx, ids, playercontract.BattleLockKey, "battle:lock")
	blobs := p.mget(ctx, ids, playerAllDataKey, "PlayerAllData")
	for i, id := range ids {
		d := memberDisplay{Online: sessions[id] == SessionOnline}
		if locks != nil {
			d.InBattle = locks[i] != ""
		}
		if blobs != nil {
			d.Level, d.ClassId = parsePlayerBrief(ctx, id, blobs[i])
		}
		dc[id] = d
	}
	return dc
}

// mget 按 keyOf 拼 key 批量读;失败或返回长度不符时记日志返回 nil(调用方按"全部缺失"处理)。
func (p presenceReader) mget(ctx context.Context, ids []uint64, keyOf func(uint64) string, what string) []string {
	if len(ids) == 0 || p.rds == nil {
		return nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, keyOf(id))
	}
	values, err := p.rds.MgetCtx(ctx, keys...)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] 批量读 %s 失败(按缺失处理) n=%d: %v", what, len(ids), err)
		return nil
	}
	if len(values) != len(ids) {
		logx.WithContext(ctx).Errorf("[team] 批量读 %s 返回长度 %d,期望 %d(按缺失处理)", what, len(values), len(ids))
		return nil
	}
	return values
}

// parsePlayerBrief 从 PlayerAllData 取等级与职业(读法照 login player_class_backfill.go)。
// 缺失、解析失败、player_id 不匹配都返回 0(尽力而为,§G.2 / J-11)。
func parsePlayerBrief(ctx context.Context, playerId uint64, raw string) (level, classId uint32) {
	if raw == "" {
		return 0, 0
	}
	data := &dbpb.PlayerAllData{}
	if err := proto.Unmarshal([]byte(raw), data); err != nil {
		logx.WithContext(ctx).Infof("[team] PlayerAllData 解析失败,展示字段填 0 player=%d: %v", playerId, err)
		return 0, 0
	}
	player := data.GetPlayerDatabaseData()
	if player.GetPlayerId() != playerId {
		return 0, 0
	}
	return player.GetLevelComponent().GetLevel(), player.GetUint32PbComponent().GetClass()
}

// uniqueIds 去掉 0 与重复,保持首次出现顺序。
func uniqueIds(ids []uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
