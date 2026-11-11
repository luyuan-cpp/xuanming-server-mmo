package authority

import (
	"strconv"
	"strings"
)

// 后台工作按片分工(docs/design/global-match-hierarchical-pools.md §5.2,缺口清单第 24 条):
// 成局恢复(Pending)、整队维护(TeamLaunchesAfter)、闲置队伍到期、投榜(PublishRankOnce),以及终局发件箱
// (PublishBattleOutcomes,缺口清单第 25 条,docs/design/assetop-redelivery-sharding-20261110.md §2.5)这几项,
// 每个副本只做自己负责的片,加副本就分摊,不再每个副本把全部工作各做一遍。
//
// 归属判定由装配层绑定(BindWorkOwner,生产是 svc.OwnsWork:etcd 注册的 match 实例上的最高随机权重哈希)。
// 它只管分工、不承担正确性:这些工作的互斥都在各自的 SQL 提交点(工作租约、行锁、CAS)。成员表变化的瞬间
// 某一片可能两个副本都做(多几次冲突)或暂时没人做(下一拍补上),都不影响结果。
// 没绑(单测)= 全部片都归本副本,行为与分工前相同。

// WorkShards 是按片分工的片数。片号 = 业务实体 id 对它取模:发号器发的 id 已经打散(AGENTS §7 不变量 1),
// 取模足够均匀。它只决定分工,不进任何主键或持久数据,改它不用迁移。
const WorkShards = 64

// 各项工作的归属键前缀。键 = 前缀 + ":" + 片号;投榜按 (模式, 来源片) 再细分,见 rankWorkKey。
const (
	workFormationRecovery = "formation"   // 成局恢复:片号 = battle_id % WorkShards
	workTeamLaunch        = "team-launch" // 整队维护:片号 = 发起者 actor_id % WorkShards
	workTeamIdle          = "team-idle"   // 闲置队伍到期:片号 = team_id % WorkShards
	workRankSource        = "rank-source" // 投榜:每个 (计分模式, 来源片) 一个键
	// workReadyTicket 已建房票巡检(ReadyAfter):片号 = player_id % WorkShards,条件在 ready_recovery.go ReadyAfter 的
	// ORDER BY 之前接入(2026-11-10,数据覆盖审计批 B5)。
	workReadyTicket = "ready-ticket"
	// 终局发件箱(outcome_outbox.go,缺口清单第 25 条)。整局结果表 match_battle_completion 一局一行、没有 player_id 列,
	// 按 battle_id 分;个人结算按玩家分,首投与等待两页共用一个键,同一玩家的首投和之后的 Lookup 归同一个副本。
	workOutcomeResult     = "outcome-result"     // 整局结果发 Kafka:片号 = battle_id % WorkShards
	workOutcomeSettlement = "outcome-settlement" // 个人结算入 Inbox / 等 APPLIED:片号 = player_id % WorkShards
)

// BindWorkOwner 接入按片分工的归属判定。启动时写一次,在 Store 交给任何后台循环之前;之后只读。
func (s *Store) BindWorkOwner(owns func(key string) bool) { s.workOwner = owns }

// ownsWork 报告本副本是否负责 key 这份工作。没绑归属判定时全部归本副本。
func (s *Store) ownsWork(key string) bool { return s == nil || s.workOwner == nil || s.workOwner(key) }

func workShardKey(kind string, shard int) string { return kind + ":" + strconv.Itoa(shard) }

func rankWorkKey(mode, shard uint32) string {
	return workRankSource + ":" + strconv.FormatUint(uint64(mode), 10) + ":" + strconv.FormatUint(uint64(shard), 10)
}

// WorkShardFilter 返回把 column 限定在本副本负责的 kind 片里的 SQL 条件与绑定参数,接在 WHERE 里用 AND 拼上。
// column 必须是调用方写死的列名(不是外部输入),片号只走绑定参数。条件放在 LIMIT 之前:各副本只看自己的片,
// 一个副本积压的工作占不住别的副本的名额。全部片都归本副本时回 "1=1"(不过滤);一片都不归时回 "1=0"。
func (s *Store) WorkShardFilter(kind, column string) (string, []any) {
	var shards []any
	for shard := 0; shard < WorkShards; shard++ {
		if s.ownsWork(workShardKey(kind, shard)) {
			shards = append(shards, shard)
		}
	}
	switch len(shards) {
	case WorkShards:
		return "1=1", nil
	case 0:
		return "1=0", nil
	}
	return "MOD(" + column + "," + strconv.Itoa(WorkShards) + ") IN(" + strings.TrimSuffix(strings.Repeat("?,", len(shards)), ",") + ")", shards
}

// TeamIdleWorkFilter 是闲置队伍到期扫描的分片条件(team 包的到期循环用;片号 = team_id)。
func (s *Store) TeamIdleWorkFilter() (string, []any) {
	return s.WorkShardFilter(workTeamIdle, "team_id")
}
