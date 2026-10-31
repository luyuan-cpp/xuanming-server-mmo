package team

import (
	"context"

	"match/internal/playercontract"

	dspb "proto/data_service"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"shared/playerredis"
)

// 在线态与展示信息(设计文档 docs/design/team-system.md §G.1 / §G.2)。
//
// 这些都是显示层数据:每次构建视图或做惰性转让判定时按需批量读,绝不写进 TeamRecord
// (AGENTS §11.6)。只读,写者都不是 match:
//
//	player:session:{<id>}    player_locator  在线态唯一真值(只有 SESSION_STATE_ONLINE 算在线)
//	battle:lock:{<id>}       C++ scene       咨询性"战斗中"(值是 battle_id,非空)
//	data_service             player_store    等级、职业、昵称、外观、性别(尽力而为,缺失或读失败填零值)
//
// 失败语义:任何一次 MGET 失败只让对应字段按"离线 / 不在战斗 / 0"处理并记日志,
// 不让 RPC 失败(照 guild online_status_resolver)。惰性转让用的 SessionLoader 例外:
// 读失败给 SessionUnknown,规则层据此既不转让队长、也不把人当在线(fail-closed)。

// memberDisplay 视图里一名玩家的运行时展示信息。
type memberDisplay struct {
	Online       bool
	InBattle     bool
	Level        uint32
	ClassId      uint32
	Name         string
	AppearanceId string
	Gender       uint32
}

// displayCache player_id → 展示信息。一次提交 / 一次读只构建一份,所有接收者复用(§G.2)。
type displayCache map[uint64]memberDisplay

// presenceReader 批量读取在线态与展示信息。无可变状态,可并发使用。
type presenceReader struct {
	rds      redis.UniversalClient // svcCtx.PlayerRedis
	displays dspb.DataServiceClient
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
			logx.WithContext(ctx).Errorf("[team] Failed to parse session player=%d: %v", ids[i], err)
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
	displays := p.loadStoreDisplays(ctx, ids)
	for i, id := range ids {
		d := memberDisplay{Online: sessions[id] == SessionOnline}
		if locks != nil {
			d.InBattle = locks[i] != ""
		}
		if display := displays[id]; display != nil {
			d.Level, d.ClassId, d.Name, d.AppearanceId, d.Gender = display.GetLevel(), display.GetClassId(),
				display.GetName(), display.GetAppearanceId(), display.GetGender()
		}
		dc[id] = d
	}
	return dc
}

// loadStoreDisplays 经 data_service 读展示资料。失败只记日志,按全部缺失处理(同 mget 的失败语义)。
func (p presenceReader) loadStoreDisplays(ctx context.Context, ids []uint64) map[uint64]*dspb.PlayerDisplay {
	if p.displays == nil {
		return nil
	}
	resp, err := p.displays.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: ids})
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] Failed to batch-read player display (treating as missing) n=%d: %v", len(ids), err)
		return nil
	}
	return resp.GetPlayers()
}

// mget 按 keyOf 拼 key 批量读,缺键是空串;失败或返回长度不符时记日志返回 nil(调用方按"全部缺失"处理)。
// 这些键分属不同玩家、不同 slot,按 slot 分组读。
func (p presenceReader) mget(ctx context.Context, ids []uint64, keyOf func(uint64) string, what string) []string {
	if len(ids) == 0 || p.rds == nil {
		return nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, keyOf(id))
	}
	raw, err := playerredis.MGetBySlot(ctx, p.rds, keys)
	if err != nil {
		logx.WithContext(ctx).Errorf("[team] Failed to batch-read %s (treating as missing) n=%d: %v", what, len(ids), err)
		return nil
	}
	if len(raw) != len(ids) {
		logx.WithContext(ctx).Errorf("[team] Batch-read %s returned length %d, expected %d (treating as missing)", what, len(raw), len(ids))
		return nil
	}
	values := make([]string, len(raw))
	for i, value := range raw {
		values[i], _ = value.(string)
	}
	return values
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
