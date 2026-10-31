package logic

import (
	battlepb "proto/battle"
	matchpb "proto/match"

	"google.golang.org/protobuf/proto"
)

// 观战可见性(docs/design/battle-spectate-tiered-fanout.md §10 第 6 条):「这局能不能被观战」只在本文件决定。
// gather 建房时调用一次 spectateVisibilityFor,结果写进 CreateBattleRequest.spectate_visibility 随原件冻结
// (全局模式随 formation 的 create_request 落 TiDB),观战记录 SpectateBattleRecord.visibility 取同一个值。
// 之后各执法点只读冻结值、不再按模式重算:
//   - 登记与公开:私密局照写观战记录(丢票补签靠它定位 battle 节点),但不进活跃集合(addSpectateActive 拒收);
//   - WatchBattle:Redis 路径指定场在任何副作用之前判定、随机模式跳过私密成员;全局路径在落原件之前判定
//     (globalFormationWatchable),不满足按 16018 关闭意图;
//   - 观战列表:过滤非公开记录,只摘活跃集合成员;
//   - battle AddObserver:房间可见性非 PUBLIC 一律拒绝(第二道闸,C++ battle 侧)。
// 默认拒绝:UNSPECIFIED(漏填)与 PRIVATE 一样不可观战,只有 PUBLIC 放行。

// spectateVisibilityFor 是「这局能不能被观战」的唯一权威,建房时调用一次、随 CreateBattleRequest 冻结。
// 活动局一律私密、不看模式;其余按 spectateVisibilityOfMode 的显式决策表,表外的值按私密处理。
func spectateVisibilityFor(mode matchpb.MatchMode, activity *battlepb.BattleActivityContext) battlepb.ESpectateVisibility {
	if activity != nil && activity.GetKind() != battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE {
		// 活动局(帮会同道历练等)的名单与 battle_id 归活动方:历练在开局前就把 battle_id 回给了 guild,
		// 不是公开对局。未知的活动类型同样落在这里(fail-closed)。
		return battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE
	}
	if visibility, decided := spectateVisibilityOfMode(mode); decided {
		return visibility
	}
	// 表外的值(proto 新增了模式却没进表,或收到未知枚举值):判不了就不放行。
	return battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE
}

// spectateVisibilityOfMode 是按对局模式的显式决策表:每个 MatchMode 枚举值各占一行、各写理由。decided=false
// 只给表外的值(此时同样返回 PRIVATE)。TestSpectateVisibilityDecidesEveryMatchMode 遍历 MatchMode_name 守住
// 「每个枚举值都在表里」:proto 新增模式而这里没加一行,测试即红,不会被 default 静默归到哪一边。
func spectateVisibilityOfMode(mode matchpb.MatchMode) (visibility battlepb.ESpectateVisibility, decided bool) {
	const (
		public  = battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC
		private = battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE
	)
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		return public, true // 单人 PVE:对手是怪物,任何在线玩家都可观战
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		return public, true // 组队 PVE(队列凑单与整队开战);活动局在 spectateVisibilityFor 已判私密
	case matchpb.MatchMode_MATCH_MODE_1V1:
		return private, true // 计分局禁止观战(global-match-hierarchical-pools.md D7 / §8)
	case matchpb.MatchMode_MATCH_MODE_5V5:
		return private, true // 计分局禁止观战(同上)
	case matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		return private, true // 点名切磋:没有公开依据,默认拒绝
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		return private, true // 真人人数档 PVP:没有公开依据,默认拒绝
	case matchpb.MatchMode_MATCH_MODE_3V3:
		return private, true // 未开放
	case matchpb.MatchMode_MATCH_MODE_UNSPECIFIED:
		return private, true // 漏填模式
	default:
		return private, false
	}
}

// isSpectatePublic 只认 PUBLIC;UNSPECIFIED(漏填)与 PRIVATE 一样不可观战。
func isSpectatePublic(visibility battlepb.ESpectateVisibility) bool {
	return visibility == battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC
}

// formationSpectatePublic 读全局成局持久的建房原件(match_formation.create_request)判定能否观战。原件在成局
// 1→2 时写入一次、之后只允许逐字节相同的重写(authority saveIntent),所以判定可重放。空字节(原件尚未落库)
// 与坏字节一律按私密(fail-closed):判不了就不放行。
func formationSpectatePublic(createRequest []byte) bool {
	if len(createRequest) == 0 {
		return false
	}
	create := &battlepb.CreateBattleRequest{}
	if err := proto.Unmarshal(createRequest, create); err != nil {
		return false
	}
	return isSpectatePublic(create.GetSpectateVisibility())
}
