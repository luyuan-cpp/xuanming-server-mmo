package main

// 审计的两组新检查:
//
//  A. 合服**前**的真门禁(pre-merge)。旧的 auditOnlineKeys 名义上是 block 级,
//     实际上永远拦不住任何东西:
//       - 它在 **mapping Redis(DB 0)** 上查 `friend:online:{pid}`,而这把键
//         由 go/friend 写在**它自己的 Redis(DB 3,go/friend/etc 的 RedisClient)**。
//         查错库 ⇒ 恒 0 ⇒ 恒「全部离线 ✅」。
//       - pipeline 的错误被 `_, _ = pipe.Exec(ctx)` 吞掉,超时 / WRONGTYPE
//         同样落到「0 在线」。
//       - 文档写了「exit 2 = 基础设施错误」,但 runAuditEntry 从来没返回过 2:
//         连不上 Redis 只打一行 WARN,然后审计「通过」。
//     修法:分开 friend Redis(DB 3)与共享 DB 0 两个句柄;扫描失败一律
//     Severity=block;句柄缺失让整个审计以 exit 2 结束(见 auditFatal)。
//
//     2026-09-18 后记(friend 移植 F3):`friend:online:{pid}` 这把键本身已经退役,
//     上面这段只剩史料价值。真相比「查错库」还要糟一层 —— 它在全仓**从来没有写者**
//     (F1 移植期 grep 确认),所以哪怕当年查对了 DB 3 也一样恒 0。在线状态统一
//     改读契约键 `player:session:{pid}`(go/friend 的读者在 F2 删掉)。本文件因此
//     只留 player:session 一个证据面,friend Redis 句柄与 `-friend-redis-*` 参数一并删除。
//
//  B. 合服**后**的验证(-verify-merged)。此前 -verify-merged 只是把标题里的
//     "pre-merge" 换成 "POST-MERGE VERIFICATION",一条断言都没有。这里把
//     runbook §5 Step 5 的那张表逐条实现成 block 级 auditor。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// auditFatal 由 auditor 设置,表示「这不是数据问题,是我们根本没查成」。
// runAuditEntry 见到它就以 exit 2 结束 —— 与 exit 1(查到了真问题)区分开,
// ops 脚本才能分辨「不许合服」和「审计本身坏了,结论不可信」。
const auditNoteInfraPrefix = "INFRA: "

func infraAudit(name, format string, args ...any) ResourceAudit {
	return ResourceAudit{
		Name:     name,
		Severity: "block",
		Notes:    auditNoteInfraPrefix + fmt.Sprintf(format, args...),
	}
}

func isInfraAudit(r ResourceAudit) bool {
	return len(r.Notes) >= len(auditNoteInfraPrefix) && r.Notes[:len(auditNoteInfraPrefix)] == auditNoteInfraPrefix
}

// ── A. 合服前门禁 ─────────────────────────────────────────────

// auditOnlinePresence 查源区玩家还在不在线。证据面只有一个:
//   - player:session:{pid} —— player_locator 写,**DB 0**
//
// 非零 = zone-down 没做完 / 还有进程在 ACK,block。
// 扫描失败也是 block(而且是 INFRA 级):查不到就不能说「没人在线」。
//
// # 为什么 friend:online 不在这里了(2026-09-18,friend 移植 F3)
//
// `friend:online:{pid}` 全仓没有写者、go/friend 的读者也在 F2 删了,在线状态统一读
// 契约键 player:session。继续查它只会得到两种结果,两种都有害:
//   - 句柄配齐时恒 0 —— 一个永远不可能 block 的 block 级门禁,正是本文件开头列的那种假门禁;
//   - 句柄缺失时返回 INFRA、整个审计 exit 2 —— 拦住合服的理由却是一把已经没人写的键。
//
// 证据面从两个减到一个**不降低门禁强度**:player:session 由 player_locator 在登录 /
// 断线时维护,是「这个玩家还连着」的权威来源;friend:online 当年即便写了也只是它的派生物。
func auditOnlinePresence(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "online_presence", UniqueScope: "per_zone"}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v — cannot enumerate source-zone players", err)
	}
	pids, err := routes.playersWithHome(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan (%s) failed: %v", routes.source(), err)
	}
	r.SourceCount = int64(len(pids))

	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — cannot check player:session:{pid}")
	}
	sessions, err := countExistingKeys(ctx, cfg.sharedRDB, pids, "player:session:")
	if err != nil {
		return infraAudit(r.Name, "player:session scan failed: %v", err)
	}

	r.TargetCount = int64(sessions)
	r.ConflictCount = int64(sessions)
	if sessions > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d players still have player:session (DB %d). "+
			"Investigate — zone-down is incomplete.", sessions, cfg.sharedDBIndex)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("0 player:session (DB %d) for %d source players", cfg.sharedDBIndex, len(pids))
	return r
}

// auditPlayerLocks 查源区玩家有没有在持的分布式锁。
//   - lock:player:{id}     —— data_service router.go,**mapping Redis**
//   - player:{id}:__lock   —— data_service router.go,**data Redis**(可选句柄)
//
// 非零 = 有人正在改这些玩家的数据,合服搬走的会是半截状态。
func auditPlayerLocks(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "player_locks", UniqueScope: "global"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v — cannot enumerate source-zone players", err)
	}
	pids, err := routes.playersWithHome(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan (%s) failed: %v", routes.source(), err)
	}
	r.SourceCount = int64(len(pids))

	mappingLocks, err := countExistingKeys(ctx, cfg.mappingDB, pids, "lock:player:")
	if err != nil {
		return infraAudit(r.Name, "lock:player scan failed: %v", err)
	}
	dataLocks := 0
	dataNote := "data-Redis player:{id}:__lock not checked (no -source-data-redis)"
	if cfg.dataRDB != nil {
		for _, pid := range pids {
			n, derr := cfg.dataRDB.Exists(ctx, fmt.Sprintf("player:{%d}:__lock", pid)).Result()
			if derr != nil {
				return infraAudit(r.Name, "player:{id}:__lock scan failed: %v", derr)
			}
			if n > 0 {
				dataLocks++
			}
		}
		dataNote = fmt.Sprintf("%d player:{id}:__lock held in the data Redis", dataLocks)
	}
	r.ConflictCount = int64(mappingLocks + dataLocks)
	if r.ConflictCount > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d lock:player:* held in the mapping Redis; %s. Someone is mid-write.", mappingLocks, dataNote)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("0 lock:player:* held; %s", dataNote)
	return r
}

// auditKafkaQueues 查 go/db 的重试 / 死信 / 处理中队列。
// 键名镜像 go/db/internal/kafka/key_ordered_consumer.go,住在 go/db 的
// RedisClient(db.yaml: DB 0)。非空 = 有存盘任务**没有**进 zone 库。
func auditKafkaQueues(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "kafka_db_task_queues", UniqueScope: "per_zone"}
	if cfg.sharedRDB == nil {
		return infraAudit(r.Name, "no shared (DB 0) Redis handle — cannot inspect kafka:retry/dead queues")
	}
	topic := dbTaskTopic(cfg.src, cfg.topicGeneration)
	var total int64
	// 含每个 go/db 实例各自的 processing 列表(见 preflight.go 的 dbTaskQueueKeys)。
	queueKeys, err := dbTaskQueueKeys(ctx, cfg.sharedRDB, topic)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	details := make([]string, 0, len(queueKeys))
	for _, k := range queueKeys {
		n, err := cfg.sharedRDB.LLen(ctx, k).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return infraAudit(r.Name, "llen %s: %v", k, err)
		}
		total += n
		details = append(details, fmt.Sprintf("%s=%d", k, n))
	}
	r.SourceCount = total
	if total > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d db_task payload(s) never reached zone_%d_db (%v). Drain or triage before merging.",
			total, cfg.src, details)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("all empty (%v)", details)
	return r
}

// auditSourceZoneNodes 查源区场景节点:etcd 注册表(正面证据)与负载集。与 merge 的 preflight P2 同一判据
// (assertZoneDown),放进审计是为了让 T-1 彩排就能发现「zone-down 其实没做干净」。
func auditSourceZoneNodes(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "source_scene_nodes", UniqueScope: "per_zone"}
	if cfg.sceneRDB == nil {
		return infraAudit(r.Name, "no scene_manager Redis handle (-scene-redis-addr/-scene-redis-db)")
	}
	if cfg.sceneNodes == nil {
		return infraAudit(r.Name, "no etcd scene-node registry (-etcd-endpoints): an empty load set alone does not prove the zone is down")
	}
	registered, err := cfg.sceneNodes.SceneNodesInZone(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "read etcd scene-node registry: %v", err)
	}
	if registered > 0 {
		r.SourceCount = int64(registered)
		r.Severity = "block"
		r.Notes = fmt.Sprintf("etcd still registers %d scene node(s) of zone %d — zone-down is not complete", registered, cfg.src)
		return r
	}
	key := fmt.Sprintf(sceneNodeLoadKeyFmt, cfg.src)
	n, err := cfg.sceneRDB.ZCard(ctx, key).Result()
	if err != nil {
		return infraAudit(r.Name, "zcard %s: %v", key, err)
	}
	r.SourceCount = n
	if n > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s still has %d live scene node(s) — zone-down is not complete", key, n)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("no scene node of zone %d registered in etcd; %s is empty", cfg.src, key)
	return r
}

// ── B. 合服后验证(-verify-merged)─────────────────────────────

// verifyMappingDrained: mapping 里 home_zone==src 的玩家数必须是 0。
//
// 它兜的是清单之外的人:改映射只按清单(A2),清单外仍指向源区的玩家不会被改,合服后留在已下线的 zone 里。
// 旧版还有一条下界「home_zone==dst 的人数 >= -expected-src-players」,那个数里本来就有目标区原住民,
// 漏改几个照样过线 —— 已由 verifyManifestMapping 逐 id 核对取代(A6)。
func verifyMappingDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:mapping_src", UniqueScope: "global"}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	pids, err := routes.playersWithHome(ctx, cfg.src)
	if err != nil {
		return infraAudit(r.Name, "mapping scan (%s) failed: %v", routes.source(), err)
	}
	dstIDs, err := routes.playersWithHome(ctx, cfg.dst)
	if err != nil {
		return infraAudit(r.Name, "mapping scan (%s, dst) failed: %v", routes.source(), err)
	}
	r.SourceCount = int64(len(pids))
	r.TargetCount = int64(len(dstIDs))
	if len(pids) != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d players still map to home_zone=%d (first ids: %v) — remap did not complete, or they are "+
			"outside the manifest and were never remapped", len(pids), cfg.src, sampleUint64(pids))
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("home_zone=%d drained to 0; home_zone=%d now holds %d players", cfg.src, cfg.dst, len(dstIDs))
	return r
}

// verifyManifestMapping(A6):清单里的每一个玩家,player:zone 都必须已经是 dst。
//
// 逐 id 核对才回答得了「清单里的人是不是都过来了」;键丢了、指向第三个 zone、还停在源区,都算没过来。
// 顺带把清单人数与 -expected-src-players 对一遍:T-0 的 -apply 在首跑时已按它守过人数,对不上多半是拿错了清单。
func verifyManifestMapping(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:manifest_mapping", UniqueScope: "global"}
	m, err := cfg.verifyManifest()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount = int64(len(m.PlayerIDs))
	// 人数核对只看清单本身,放在任何查询之前:拿错清单是真问题(block),不因为某个库连不上被说成 INFRA。
	if cfg.expectedSrcPlayers >= 0 && r.SourceCount != cfg.expectedSrcPlayers {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("the manifest lists %d players but -expected-src-players is %d (recorded at T-1) — "+
			"is this the manifest of the merge being verified?", r.SourceCount, cfg.expectedSrcPlayers)
		return r
	}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	// db 源读库里的 home_zone(真源),redis 源读 player:zone。按原值逐字节比(与以前的 readPlayerZones 同一口径):
	// 缺席、非规范写法("0902")、指向别的 zone 都算没过来。库里的值由 homeValue 规范成十进制,两种来源可比。
	reads, err := routes.placements(ctx, m.PlayerIDs)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	if len(reads) != len(m.PlayerIDs) {
		return infraAudit(r.Name, "placement read returned %d answers for %d players", len(reads), len(m.PlayerIDs))
	}
	want := strconv.FormatUint(uint64(cfg.dst), 10)
	var wrong []uint64
	for i, id := range m.PlayerIDs {
		if reads[i].HomeRaw != want {
			wrong = append(wrong, id)
		}
	}
	r.TargetCount = r.SourceCount - int64(len(wrong))
	r.ConflictCount = int64(len(wrong))
	if len(wrong) > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d of the %d manifest players do not map to home_zone=%d (missing, still %d, or a third zone; "+
			"first ids: %v)", len(wrong), len(m.PlayerIDs), cfg.dst, cfg.src, sampleUint64(wrong))
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("all %d manifest players map to home_zone=%d", len(m.PlayerIDs), cfg.dst)
	return r
}

// playerMainRowCheck 是「清单玩家的主数据行合服后应当在哪」的判据(A6)。落点记录经 routes 读:
// -route-source=db 读全局库(真源),redis 读 mapping Redis(规格 §13.3 第 6 条)。
type playerMainRowCheck struct {
	where   string // 进 Notes:查的是哪里
	missing func(ctx context.Context, db *sql.DB, routes routeReadSide, ids []uint64) ([]uint64, error)
}

// expectedMainRowStore 是纯判定:一名清单玩家的主数据行合服后应在的落点库(player-storage-placement.md §10)。
//
//	有落点记录 → 记录指向的库:pin 模式钉住的 "{src}:1"、copy 模式因已有记录而没拷的人、合服前就在别处的人;
//	没有记录   → 目标区 zone 库:copy 模式步骤 1 把他的行拷了过去。
//
// 设计原文是「有效落点按 记录 ?? home 算」;无记录时这里用 dst 而不是 home —— verify:manifest_mapping 已逐人断言
// home == dst,两者在它通过时完全等价;它不通过时按 home 去查一个第三区的库只会得出一条难懂的 INFRA,不如照
// 合服本该落到的地方查。记录畸形 → ok=false:go/db 对他 fail-closed,算作「行不在该在的地方」。
func expectedMainRowStore(rd placementRead, dst uint32) (uint32, bool) {
	switch {
	case rd.RecordPresent && rd.RecordErr != nil:
		return 0, false
	case rd.RecordPresent:
		return rd.Record.StorageID, true
	default:
		return dst, true
	}
}

// mainRowCheckFor 给出 -verify-merged 的主数据行判据:每个清单玩家在 expectedMainRowStore 指向的库的
// player_database 有行(每个玩家必有这一行;_1 / centre 允许缺行,不作判据)。copy / pin 两种模式同一条判据,
// 差别全在落点记录上:pin 模式无记录者都被钉成了 "{src}:1",漏钉的人会落到「无记录 → dst」而在目标库查不到。
func mainRowCheckFor(_ *mergeManifest, dst uint32) playerMainRowCheck {
	return playerMainRowCheck{
		where: fmt.Sprintf("player_database of each player's placement store (player:placement record, else %s)", zoneDBName(dst)),
		missing: func(ctx context.Context, db *sql.DB, routes routeReadSide, ids []uint64) ([]uint64, error) {
			reads, err := routes.placements(ctx, ids)
			if err != nil {
				return nil, err
			}
			if len(reads) != len(ids) {
				return nil, fmt.Errorf("placement read returned %d answers for %d players", len(reads), len(ids))
			}
			var missing []uint64
			byStore := map[uint32][]uint64{}
			for i, id := range ids {
				store, ok := expectedMainRowStore(reads[i], dst)
				if !ok {
					missing = append(missing, id)
					continue
				}
				byStore[store] = append(byStore[store], id)
			}
			for _, store := range sortedZones(storeSet(byStore)) {
				name, _ := storeDBName(store)
				lost, err := idsWithoutRow(ctx, db, name+".player_database", byStore[store])
				if err != nil {
					return nil, err
				}
				missing = append(missing, lost...)
			}
			return sortedUint64(missing), nil
		},
	}
}

// storeSet 返回 byStore 的键集合(给 sortedZones 排序,查询顺序稳定)。
func storeSet(byStore map[uint32][]uint64) map[uint32]bool {
	out := make(map[uint32]bool, len(byStore))
	for s := range byStore {
		out[s] = true
	}
	return out
}

// verifyManifestRows(A6):清单里的每一个玩家,主数据行都必须在合服后该在的库里。
//
// 取代旧的 verify:target_zone_rows(目标库 player_database 总行数 >= -expected-src-players):总行数里有
// 目标区原住民,漏拷几个照样过线。清单没记「玩家行这一步」完成时报 NOT VERIFIED(warn):copy 模式是步骤 1
// (-skip-player-rows,或合服没走到那里,行本来就不该在目标库),pin 模式是钉落点(没钉完就按有效落点查只会
// 得出错误结论)。
func verifyManifestRows(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:manifest_rows", UniqueScope: "per_zone"}
	m, err := cfg.verifyManifest()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount = int64(len(m.PlayerIDs))
	mode := m.rowsMode()
	if gate := rowsStepFor(mode); !m.stepDone(gate) {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("NOT VERIFIED: step %s (-player-rows-mode %s) is not recorded as done in the manifest "+
			"(merged with -skip-player-rows, or the run did not get that far) — main-data rows were not checked", gate, mode)
		return r
	}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	routes, err := cfg.routeReads()
	if err != nil {
		return infraAudit(r.Name, "%v (placement records decide where each player's rows live)", err)
	}
	check := mainRowCheckFor(m, cfg.dst)
	missing, err := check.missing(ctx, cfg.db, routes, m.PlayerIDs)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.TargetCount = r.SourceCount - int64(len(missing))
	r.ConflictCount = int64(len(missing))
	if len(missing) > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d of the %d manifest players have no row in %s (first ids: %v) — "+
			"their main data did not arrive", len(missing), len(m.PlayerIDs), check.where, sampleUint64(missing))
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("all %d manifest players have a row in %s", len(m.PlayerIDs), check.where)
	return r
}

// verifyGuildZoneDrained: guild WHERE zone_id=src 必须是 0。
func verifyGuildZoneDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:guild_zone", UniqueScope: "global"}
	// 与合服 P1 同一口径:-skip-guild-mysql 与 -skip-guild-rank 同时给出 = 这里没有部署帮会服务,合服没有改写
	// guild.zone_id。报 SKIPPED(warn),不伪装成通过,也不因为查不到表而恒报 INFRA(exit 2)。
	// 只给其中一个开关时 P1 照样要求帮会表存在,这里照常断言(源区残留即 block,是正确结论)。
	if cfg.skipGuild {
		r.Severity = "warn"
		r.Notes = "SKIPPED (-skip-guild-mysql -skip-guild-rank): the guild service is declared absent here — " +
			"guild.zone_id was not rewritten by this merge and is NOT VERIFIED"
		return r
	}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := validateGuildSchemaName(cfg.guildSchema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	guildTableName := guildQualified(cfg.guildSchema, guildTable)
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildTableName+" WHERE zone_id = ?", cfg.src).Scan(&r.SourceCount); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildTableName, cfg.src, err)
	}
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildTableName+" WHERE zone_id = ?", cfg.dst).Scan(&r.TargetCount); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildTableName, cfg.dst, err)
	}
	if r.SourceCount != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d %s rows still carry zone_id=%d", r.SourceCount, guildTableName, cfg.src)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("zone_id=%d drained; zone_id=%d holds %d guilds", cfg.src, cfg.dst, r.TargetCount)
	return r
}

// verifyGuildRankZSets: 源区 ZSET 不存在,且目标区 ZCARD == COUNT(guild WHERE zone_id=dst)。
//
// 后半条是真正有价值的那条:ZSET 是可重建缓存,合完之后它与 MySQL 的公会数
// 必须一致 —— 不一致意味着 ZADD 漏了成员(旧的非事务 pipeline 正是这么漏的),
// 表现为公会在榜单上凭空消失。
func verifyGuildRankZSets(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:guild_rank", UniqueScope: "per_zone"}
	// 口径同 verifyGuildZoneDrained:两个帮会跳过开关都给 = 帮会服务不存在,榜单既没合并也无从核对。
	if cfg.skipGuild {
		r.Severity = "warn"
		r.Notes = "SKIPPED (-skip-guild-mysql -skip-guild-rank): the guild service is declared absent here — " +
			"the guild_rank ZSETs were not merged by this merge and are NOT VERIFIED"
		return r
	}
	if cfg.rankRDB == nil {
		return infraAudit(r.Name, "no guild (rank) Redis handle")
	}
	srcKey, dstKey := guildZoneRankKey(cfg.src), guildZoneRankKey(cfg.dst)
	exists, err := cfg.rankRDB.Exists(ctx, srcKey).Result()
	if err != nil {
		return infraAudit(r.Name, "exists %s: %v", srcKey, err)
	}
	r.SourceCount = exists
	dstCard, err := cfg.rankRDB.ZCard(ctx, dstKey).Result()
	if err != nil {
		return infraAudit(r.Name, "zcard %s: %v", dstKey, err)
	}
	r.TargetCount = dstCard
	if exists != 0 {
		r.Severity = "block"
		r.Notes = srcKey + " still exists — the rank merge did not delete the source ZSET"
		return r
	}
	if cfg.db == nil {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%s removed; %s has %d members (no MySQL handle to cross-check against guild rows)", srcKey, dstKey, dstCard)
		return r
	}
	if err := validateGuildSchemaName(cfg.guildSchema); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	var guildRows int64
	if err := cfg.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+guildQualified(cfg.guildSchema, guildTable)+" WHERE zone_id = ?",
		cfg.dst).Scan(&guildRows); err != nil {
		return infraAudit(r.Name, "count %s zone_id=%d: %v", guildQualified(cfg.guildSchema, guildTable), cfg.dst, err)
	}
	r.ConflictCount = dstCard - guildRows
	if dstCard != guildRows {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%s has %d members but MySQL has %d guilds with zone_id=%d — "+
			"the ZSET and the authoritative table disagree", dstKey, dstCard, guildRows, cfg.dst)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("%s gone; %s has %d members == %d guild rows", srcKey, dstKey, dstCard, guildRows)
	return r
}

// verifySourceHotStateGone: 源区 scene_manager 热状态应当已经清掉。
// 只在给了 scene Redis 句柄时跑;没清也只是 warn —— 它不影响数据正确性,
// 只影响 scene_manager 的孤儿清理与登录时的三次判定开销。
func verifySourceHotStateGone(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:source_hot_state", UniqueScope: "per_zone"}
	if cfg.sceneRDB == nil {
		r.Severity = "warn"
		r.Notes = "no scene_manager Redis handle — source hot state not verified"
		return r
	}
	var leftovers int64
	for _, pat := range append(sourceZoneScanPatterns(cfg.src), fmt.Sprintf(sceneNodeLoadKeyFmt, cfg.src)) {
		var cur uint64
		for {
			keys, next, err := cfg.sceneRDB.Scan(ctx, cur, pat, 500).Result()
			if err != nil {
				return infraAudit(r.Name, "scan %s: %v", pat, err)
			}
			leftovers += int64(len(keys))
			cur = next
			if cur == 0 {
				break
			}
		}
	}
	r.SourceCount = leftovers
	if leftovers > 0 {
		r.Severity = "warn"
		r.Notes = fmt.Sprintf("%d source-zone scene_manager keys remain — re-run merge with -clear-source-hot-state", leftovers)
		return r
	}
	r.Severity = "info"
	r.Notes = "source-zone scene_manager keys are gone"
	return r
}

// verifyFenceReleased: merge:in_progress:{src|dst} 必须已经释放。
// 忘了释放 = data_service / guild 会永久拒绝新建玩家与公会。
//
// -route-source=db(B2-4 / R8-3):围栏的真源是库(W1 在库内复核它),镜像只是给 scene_manager / guild 读的副本 —— 两边都要已释放。
// 库读方没装成时这一行报 INFRA(没查真源就不能说「已释放」);库围栏仍生效时处置是让持有它的那次运行释放(或等库时钟过期),
// **不能** DEL 镜像:对账 leader 会按库里生效的围栏把镜像补回来,删掉只会让 guild / scene_manager 在补回之前提前放行。
// redis 源的判定与文案逐字不变。
func verifyFenceReleased(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:merge_fence", UniqueScope: "global"}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	dbSource := cfg.routeSource == routeSourceDB
	if dbSource && cfg.routeDB == nil {
		if cfg.routeErr != nil {
			return infraAudit(r.Name, "%v", cfg.routeErr)
		}
		return infraAudit(r.Name, "no route DB handle (-route-source=db): the authoritative zone_merge_fence was not checked")
	}
	var held []string
	dbHeld := 0
	for _, z := range []uint32{cfg.src, cfg.dst} {
		key := mergeFenceKey(z)
		n, err := cfg.mappingDB.Exists(ctx, key).Result()
		if err != nil {
			return infraAudit(r.Name, "exists %s: %v", key, err)
		}
		if n > 0 {
			held = append(held, key)
		}
		if cfg.routeDB != nil {
			st, err := cfg.routeDB.ReadFence(ctx, z)
			if err != nil {
				return infraAudit(r.Name, "read zone_merge_fence(%d): %v", z, err)
			}
			if st.Active {
				held = append(held, fmt.Sprintf("zone_merge_fence(zone=%d, run_id=%s)", z, st.RunID))
				dbHeld++
			}
		}
	}
	r.SourceCount = int64(len(held))
	switch {
	case dbHeld > 0:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("merge fence still set: %v — data_service RegisterPlayerZone and guild CreateGuild "+
			"are refusing new objects in those zones. The DB fence (zone_merge_fence) is authoritative and still active: "+
			"resume / re-run the run that holds it with the same command (add -take-over-fence <run_id> if that run aborted "+
			"and kept its fence) so it releases it, or wait until the DB clock passes its expires_at_ms (runbook §15.4). "+
			"Do NOT DEL merge:in_progress:* by hand: the reconcile leader re-mirrors an active DB fence, and guild / "+
			"scene_manager would unfence early until it does.", held)
		return r
	case len(held) > 0 && dbSource:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("merge fence mirror still set: %v, but zone_merge_fence is already released — the data_service "+
			"reconcile leader compare-and-deletes such stale mirrors on its next tick: wait for it and verify again. A mirror "+
			"without fence_epoch (old merge_zone binary or a manual write) is never deleted by the leader: DEL it once no "+
			"merge_zone process is alive.", held)
		return r
	case len(held) > 0:
		r.Severity = "block"
		r.Notes = fmt.Sprintf("merge fence still set: %v — data_service RegisterPlayerZone and guild CreateGuild "+
			"are refusing new objects in those zones. DEL them once no merge_zone process is alive.", held)
		return r
	}
	r.Severity = "info"
	r.Notes = "no merge:in_progress:* fence left behind"
	if dbSource {
		r.Notes = "no merge fence left behind (zone_merge_fence released in the route DB, no merge:in_progress:* mirror)"
	}
	return r
}

// ── verify:route_cache(D1 第 2 批,规格 §6.3 R8、P-6)──────────────────────────

// verifyRouteCache(只在 -route-source=db 时跑):清单里每一个玩家在 mapping Redis 里的那一份必须与库逐字节一致 ——
// player:maprev 等于库 rev,player:zone / player:placement 等于库值的编码(库里为 NULL 的,缓存里也必须不存在)。
//
// 为什么开 dst 之前必须跑:合服每批只在「本批全员发布确认」之后推进,但那是发布那一刻的确认;之后 Redis 切主、从旧快照恢复、
// 或有绕过脚本的写者(旧版 merge_zone、手工 redis-cli),缓存都可能与库分叉。第 3 批读方门禁打开之前,go/db 选库与
// data_service 路由只读缓存:开服之前必须证明它们看到的就是库里的结论。不一致一律 block,修法是让对账 leader 补推
// (或重跑写这批的那一步,发布是幂等的),再验一遍;不要手工改 Redis。
func verifyRouteCache(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:route_cache", UniqueScope: "global"}
	m, err := cfg.verifyManifest()
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.SourceCount = int64(len(m.PlayerIDs))
	if cfg.routeDB == nil {
		if cfg.routeErr != nil {
			return infraAudit(r.Name, "%v", cfg.routeErr)
		}
		return infraAudit(r.Name, "no route DB handle (-route-source=db)")
	}
	if cfg.mappingDB == nil {
		return infraAudit(r.Name, "no mapping Redis handle")
	}
	bad, sample, err := routeCacheMismatches(ctx, cfg.routeDB, cfg.mappingDB, m.PlayerIDs)
	if err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	r.TargetCount = r.SourceCount - int64(bad)
	r.ConflictCount = int64(bad)
	if bad > 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d of the %d manifest players have a mapping-Redis copy that differs from player_route (first: %v) — "+
			"do NOT open zone %d: let the data_service reconcile leader push them (or re-run the merge step that wrote them; "+
			"publishing is idempotent) and verify again. If a sample says AHEAD and still does when you verify again (a writer "+
			"that committed between this check's DB read and its MGET shows up as AHEAD once), the route DB lost committed writes "+
			"(rollback signature): neither the leader nor a re-run can lower a cached rev — the leader freezes the route writers "+
			"(route_meta.writable=0); follow the global-DB rollback recovery (runbook §9.4) instead", bad, len(m.PlayerIDs), sample, cfg.dst)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("all %d manifest players: player:maprev == player_route.rev and the cached values equal the DB", len(m.PlayerIDs))
	return r
}

// routeCacheMismatches 逐人比对库行与缓存三键(库:一次 ReadRoutes;缓存:每 routeCatchUpReadBatch 人一次 MGET),
// 返回不一致的人数与样本。库读或 MGET 失败、MGET 回复形状不对 = 查不成(error)。
func routeCacheMismatches(ctx context.Context, db routeDBReader, rdb *redis.Client, ids []uint64) (int, []string, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	rows, err := db.ReadRoutes(ctx, ids)
	if err != nil {
		return 0, nil, err
	}
	bad := 0
	var sample []string
	for _, chunk := range chunkUint64(ids, routeCatchUpReadBatch) {
		keys := make([]string, 0, 3*len(chunk))
		for _, id := range chunk {
			keys = append(keys, playerZoneKey(id), placementKey(id), mapRevKey(id))
		}
		vals, err := rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return 0, nil, fmt.Errorf("route cache mget (%d keys): %w", len(keys), err)
		}
		if len(vals) != len(keys) {
			return 0, nil, fmt.Errorf("%w: route cache mget returned %d values for %d keys", errUnexpectedRouteScriptReply, len(vals), len(keys))
		}
		for i, id := range chunk {
			row, present := rows[id]
			same, why, err := routeCacheMatches(row, present, vals[3*i], vals[3*i+1], vals[3*i+2])
			if err != nil {
				return 0, nil, fmt.Errorf("player %d: %w", id, err)
			}
			if !same {
				bad++
				if len(sample) < unmappedSampleSize {
					sample = append(sample, fmt.Sprintf("%d: %s", id, why))
				}
			}
		}
	}
	return bad, sample, nil
}

// routeCacheMatches 是 verify:route_cache 对一名玩家的纯判定。cacheHome / cachePlc / cacheRev 是 MGET 的元素(nil = 键不存在)。
// 一致 ⇔ 库里有这一行、行自洽、maprev 存在且与库 rev 逐字节相同、player:zone 与 player:placement 分别等于库值的编码
// (库里为 NULL 的缓存里也必须不存在)。缓存 rev 比库新同样算不一致(库回档的征兆,不是「更新了」)。
// 不一致时 why 说明哪一键不对;err 只用于 MGET 元素类型不对(查不成,不是数据问题)。
func routeCacheMatches(row mergeRouteRow, present bool, cacheHome, cachePlc, cacheRev any) (bool, string, error) {
	var vals [3]string
	var has [3]bool
	for i, v := range []any{cacheHome, cachePlc, cacheRev} {
		switch s := v.(type) {
		case nil:
		case string:
			vals[i], has[i] = s, true
		default:
			return false, "", fmt.Errorf("%w: MGET element %d is %T", errUnexpectedRouteScriptReply, i, v)
		}
	}
	if !present {
		return false, fmt.Sprintf("no player_route row (cache: player:zone=%s player:maprev=%s)",
			routeCacheValueDesc(vals[0], has[0]), routeCacheValueDesc(vals[2], has[2])), nil
	}
	plc, err := row.placementValue()
	if err != nil {
		return false, fmt.Sprintf("player_route row is malformed: %v", err), nil
	}
	wantRev := formatRouteRev(row.Rev)
	home := row.homeValue()
	// 缓存 rev 领先库单独点名(B2-5):这是库回档的特征,leader 推送与重跑都压不低缓存 rev,处置不同(runbook §9.4)。
	// maprev 不规范时比不出先后,落到下面的 != 分支。
	cacheAhead := false
	if has[2] {
		if cmp, err := compareRouteRev(vals[2], wantRev); err == nil && cmp > 0 {
			cacheAhead = true
		}
	}
	switch {
	case !has[2]:
		return false, fmt.Sprintf("player:maprev is absent (DB rev %s)", wantRev), nil
	case cacheAhead:
		return false, fmt.Sprintf("player:maprev %q is AHEAD of DB rev %s (route DB rolled back?)", vals[2], wantRev), nil
	case vals[2] != wantRev:
		return false, fmt.Sprintf("player:maprev %q != DB rev %s", vals[2], wantRev), nil
	case has[0] != (home != "") || vals[0] != home:
		return false, fmt.Sprintf("player:zone %s != DB home_zone %s", routeCacheValueDesc(vals[0], has[0]),
			routeCacheValueDesc(home, home != "")), nil
	case has[1] != (plc != "") || vals[1] != plc:
		return false, fmt.Sprintf("player:placement %s != DB placement %s", routeCacheValueDesc(vals[1], has[1]),
			routeCacheValueDesc(plc, plc != "")), nil
	}
	return true, "", nil
}

// routeCacheValueDesc 给文案用:不存在写 <absent>,否则带引号的原值。
func routeCacheValueDesc(v string, present bool) string {
	if !present {
		return "<absent>"
	}
	return strconv.Quote(v)
}
