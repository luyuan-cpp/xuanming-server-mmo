package team

import "fmt"

// SharedRedis 上的组队 key 契约(设计文档 docs/design/team-system.md §C.1)。
//
// 这是 cross-zone-matchmaking.md D2"match 对 SharedRedis 只读"的唯一例外(DV-2):
// 只允许本包经 Lua 写下面四类 key。match 其余代码仍然只读 SharedRedis。
// 四类 key 在同一条 S_COMMIT 里跨 key 原子写,依赖 SharedRedis 是单实例
// (cross-zone-matchmaking.md §10.0 共享库禁止集群化清单)。
//
//	team:rec:<team_id>      hash   ver(十进制,每次提交严格 +1)、pb(TeamRecord 字节)  权威记录
//	team:<team_id>          string TeamInfo pb(proto/common/component/team_comp.proto) scene 读的投影
//	team:player:<player_id> hash   tid(十进制,无队伍 "0")、epoch(十进制)             玩家索引
//	team:invite:<player_id> zset   member=team_id,score=expire_at_ms                   邀请反查索引
//
// team:<team_id> 与 team:player:<player_id> 的格式是 C++ scene 的既有/新增契约
// (player_scene.cpp 的 GET team:%llu、PlayerTeamSystem 的 HMGET team:player:%llu tid epoch),
// 不带 hash tag,改格式必须同步 C++。
//
// 另有两处镜像(独立 module / 独立运行时,无法 import 本包,改格式时必须同步):
//   - cpp/libs/services/scene/player/system/player_team.cpp 的 kTeam*KeyFmt(scene 读 team:player / team:<tid> / team:rec);
//   - tools/merge_zone/preflight.go 的 teamPlayerIndexKey / teamRecordKey(合服 preflight P7)。

// team:rec 的 hash 字段。
const (
	recFieldVer = "ver"
	recFieldPb  = "pb"
)

// team:player 的 hash 字段(C++ scene HMGET 按这两个名字读)。
const (
	indexFieldTid   = "tid"
	indexFieldEpoch = "epoch"
)

// recordKey 队伍权威记录。
func recordKey(teamId uint64) string {
	return fmt.Sprintf("team:rec:%d", teamId)
}

// projectionKey 队伍投影(scene 读)。
func projectionKey(teamId uint64) string {
	return fmt.Sprintf("team:%d", teamId)
}

// playerIndexKey 玩家 → 队伍索引。
func playerIndexKey(playerId uint64) string {
	return fmt.Sprintf("team:player:%d", playerId)
}

// inviteIndexKey 被邀请人 → 队伍的反查索引。
func inviteIndexKey(playerId uint64) string {
	return fmt.Sprintf("team:invite:%d", playerId)
}
