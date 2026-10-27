package main

// -mode pin-placement -zone N(docs/design/player-storage-placement.md §10.3):给 home==N 且没有落点记录的玩家
// 钉 "{N}:1"。
//
// 为什么要有:go/db 的 Placement.Required=true(没有记录一律 fail-closed)之前,存量玩家必须都有记录;新号由
// login 的 Placement.PinOnCreate 在建角时钉。钉的值就是他此刻的有效落点(无记录 → home = N),所以**在线执行
// 安全**:go/db 选到的库前后不变;而一旦钉住,之后合服改 home 也不再牵动他的数据落在哪。
//
// 并发(逐人一段 Lua,placementCASScript,判断与写入原子完成):
//   - 记录已存在(建角时钉的、合服钉的、搬库冻结 / 切换的)→ 不动;
//   - home 已不是 N(扫描之后被合服改走)→ 不钉:此刻他的有效落点已经不是 N;
//   - merge:in_progress:{N} 出现(N 开始合服)→ 整轮停下。合服的清单阶段按「那一刻有哪些记录」分流
//     (copy 跳过有记录者、pin 记下已有记录),围栏之下再冒出来的记录会让它中止;不和它抢,合完再钉。
//
// 前置:N 正在合服(围栏在)或已被合走(merge:merged_into:{N})就拒绝。合走之后 home 仍是 N 的只剩没被合过去的
// 漏网之鱼 —— 先处理他们;把他们钉在一个已下线的 zone 库上,只会让「这批人没过来」更难被看见。
//
// 不要求能力标记:钉的值等于 home,旧版 go/db(按本进程 zone 选库)对这些人选到的仍是同一个库。
// 扫描期间新建的号可能漏掉:先打开 login 的 PinOnCreate 再跑本模式;漏掉的重跑一次即可(幂等)。
//
// -route-source=db(W10,规格 §6.3、§13.3 第 5 条):每批 ≤ 200 人一个库事务(PinZonePlacementBatch:zone 的 63 号片围栏未生效 →
// 库里没有该 zone 的合走标记 → home = N 且无落点的行按键集游标 `route_shard > ? OR (route_shard = ? AND player_id > ?)` 加锁 →
// 钉)→ 提交 → 本批发布 + WAITAOF → 下一批;最后对 home = N 且记录已是 "{N}:1" 的行做一遍 catch-up(库 rev 领先缓存的先发布确认),
// 上一次运行提交了却没发布成的钉,重跑时由它补上(加锁扫描只挑还没钉的行,看不见它们)。缓存里已是这一版的不再发布、也不补
// 确认(catchUpRoutes 的口径,B1-8):「EVAL 执行了、WAITAOF 份数不足」的那一批由对账 leader 兜底 —— Redis 切主后它撤就绪标记、
// 按 RepairLookbackMs 回看修补,期间读方门禁 fail-closed。钉的值等于 home,早晚可见都不会把人路由到别的库。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// pinPlacementEntryParams 是 -mode pin-placement 的纯数据输入(main 管 flag,本文件管语义)。
type pinPlacementEntryParams struct {
	parent      context.Context // main装配的同boot NORMAL owner；dry-run可为空。
	zone        uint32
	mappingAddr string
	mappingPwd  string
	mappingDB   int
	dryRun      bool
	timeout     time.Duration
	// route 是 -route-source 与路由库参数(main 装配,routeEntryOptionsOf)。
	route routeEntryOptions
}

func runPinPlacementEntry(p pinPlacementEntryParams) {
	parent := p.parent
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	if !p.dryRun {
		if err := checkMaintenanceTool(ctx); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("=== Pin placement \"%d:1\" for players with home_zone=%d and no record (dry-run=%v, mapping=%s db=%d, route-source=%s) ===",
		p.zone, p.zone, p.dryRun, p.mappingAddr, p.mappingDB, p.route.source)
	mapRdb := mustDial(ctx, "mapping", p.mappingAddr, p.mappingPwd, p.mappingDB)
	defer closeUnownedMaintenanceResource(ctx, mapRdb)
	rb, err := openRunRouteBackend(ctx, p.route, mapRdb, newToolRunID("pin", p.zone, time.Now()))
	if err != nil {
		log.Fatalf("pin-placement: route source: %v", err)
	}
	if maintenanceFromContext(ctx) == nil {
		defer rb.Close()
	}

	rep, err := stepPinZonePlacements(ctx, rb, mapRdb, p.zone, p.dryRun)
	if err != nil {
		log.Fatalf("pin-placement: %v (progress: %s)", err, rep)
	}
	log.Printf("=== Pin placement done (%s): zone %d: %s ===", verbWrite(p.dryRun), p.zone, rep)
	if rep.HomeMoved > 0 {
		log.Printf("NOTE: %d players left home_zone=%d between the scan and the pin (merged away?) and were not pinned — "+
			"their effective storage is no longer %d", rep.HomeMoved, p.zone, p.zone)
	}
}

// pinZonePlacements 是本模式的本体:前置检查 → 扫 home==zone 的玩家 → 逐批 CAS(dry-run 只读分类)。
func pinZonePlacements(ctx context.Context, rdb *redis.Client, zone uint32, dryRun bool) (pinReport, error) {
	var rep pinReport
	if n, err := rdb.Exists(ctx, mergeFenceKey(zone)).Result(); err != nil {
		return rep, fmt.Errorf("exists %s: %w", mergeFenceKey(zone), err)
	} else if n > 0 {
		return rep, fmt.Errorf("zone %d is being merged (%s exists): pin after the merge finishes", zone, mergeFenceKey(zone))
	}
	if cur, raw, err := readMergedInto(ctx, rdb, zone); err != nil || cur != nil {
		if err != nil && raw == "" {
			return rep, fmt.Errorf("cannot prove zone %d was not merged away: %w", zone, err)
		}
		return rep, fmt.Errorf("zone %d was merged away (%s = %s): players still mapped to it are stragglers of that merge — "+
			"handle them first instead of pinning them to a retired zone database", zone, mergedIntoKey(zone), raw)
	}

	ids, err := collectPlayerIDsWithHomeZone(ctx, rdb, zone)
	if err != nil {
		return rep, err
	}
	log.Printf("pin-placement: %d players map to home_zone=%d", len(ids), zone)
	zoneVal := strconv.FormatUint(uint64(zone), 10)
	pinValue := stableValue(zone, 1)
	for _, batch := range chunkUint64(ids, mappingScanCount) {
		if dryRun {
			reads, err := readPlacements(ctx, rdb, batch)
			if err != nil {
				return rep, err
			}
			for i := range batch {
				rep.Candidates++
				countPinVerdict(&rep, decidePin(reads[i].RecordRaw, reads[i].HomeRaw, zone))
			}
			continue
		}
		ops := make([]placementCAS, len(batch))
		for i, id := range batch {
			ops[i] = placementCAS{PlayerID: id, ExpectAbsent: true, ExpectHome: zoneVal, FenceZone: zone, NewValue: pinValue}
		}
		results, err := runPlacementCAS(ctx, rdb, ops)
		if err != nil {
			return rep, err
		}
		for i, id := range batch {
			rep.Candidates++
			switch res := results[i]; res.Outcome {
			case placementCASWritten:
				rep.Pinned++
			case placementCASFenced:
				return rep, fmt.Errorf("zone %d entered a merge while pinning (%s appeared at player %d): stopped — "+
					"re-run after the merge (pinning is idempotent)", zone, mergeFenceKey(zone), id)
			default:
				v := decidePin(res.CurrentRecord, res.CurrentHome, zone)
				if v == pinVerdictPin {
					// 判成「该钉」却没写成:脚本看到的状态与判定对不上(例如值为空串的畸形键),不能计成已钉。
					rep.anomaly("%d: not pinned although no record was visible (record %q, player:zone %q)", id, res.CurrentRecord, res.CurrentHome)
					continue
				}
				countPinVerdict(&rep, v)
			}
		}
	}
	if rep.AnomalyCount > 0 {
		return rep, fmt.Errorf("%d players could not be pinned (first: %v)", rep.AnomalyCount, rep.Anomalies)
	}
	return rep, nil
}

// stepPinZonePlacements 按源分派 -mode pin-placement:rb == nil → pinZonePlacements(Redis,逐字不变);否则 pinZonePlacementsDB。
func stepPinZonePlacements(ctx context.Context, rb *routeBackend, rdb *redis.Client, zone uint32, dryRun bool) (pinReport, error) {
	if rb == nil {
		return pinZonePlacements(ctx, rdb, zone, dryRun)
	}
	return pinZonePlacementsDB(ctx, rb, zone, routePinZoneBatchLimit, dryRun)
}

// pinZonePlacementsDB 是 pinZonePlacements 的库版(W10,见文件头)。limit 是每批人数(生产 routePinZoneBatchLimit;
// 集成用例传小值证键集翻页跨片)。
//
// 前置(只读,先于任何写):zone 的围栏生效或库里有该 zone 的合走标记即拒绝;每个批事务在库里再复核一次,那才是权威判据 ——
// 中途有合服立起围栏,下一批回 errRouteZoneFenced,整轮停下(已钉的不回退,合完再重跑,幂等)。
func pinZonePlacementsDB(ctx context.Context, rb *routeBackend, zone uint32, limit int, dryRun bool) (pinReport, error) {
	var rep pinReport
	fs, err := rb.db.ReadFence(ctx, zone)
	if err != nil {
		return rep, fmt.Errorf("cannot prove zone %d is not being merged: %w", zone, err)
	}
	if fs.Active {
		return rep, fmt.Errorf("zone %d is being merged (zone_merge_fence run_id=%s): pin after the merge finishes", zone, fs.RunID)
	}
	if dst, run, found, err := rb.db.ReadMergedInto(ctx, zone); err != nil {
		return rep, fmt.Errorf("cannot prove zone %d was not merged away: %w", zone, err)
	} else if found {
		return rep, fmt.Errorf("zone %d was merged away (zone_merged_into: dst %d run_id %s): players still mapped to it are stragglers of "+
			"that merge — handle them first instead of pinning them to a retired zone database", zone, dst, run)
	}
	pinValue := stableValue(zone, 1)
	if dryRun {
		err := scanZoneHomeRows(ctx, rb.db, zone, func(page []mergeRouteRow) error {
			for _, row := range page {
				rep.Candidates++
				rec, err := row.placementValue()
				if err != nil {
					return err
				}
				countPinVerdict(&rep, decidePin(rec, row.homeValue(), zone))
			}
			return nil
		})
		return rep, err
	}
	after := routeScanCursor{}
	for {
		res, next, done, err := rb.db.PinZonePlacementBatch(ctx, zone, after, limit)
		if err != nil {
			if errors.Is(err, errRouteZoneFenced) {
				return rep, fmt.Errorf("zone %d entered a merge while pinning: stopped — re-run after the merge (pinning is idempotent): %w", zone, err)
			}
			return rep, err
		}
		rep.Candidates += len(res.Target)
		rep.Pinned += res.Changed
		if _, err := rb.pub.publishRoutes(ctx, res.Target); err != nil {
			// 文案不承诺「重跑补确认」(B1-8):收尾 catch-up 只补发缓存还没有这一版的行;已执行、没确认落盘的由对账 leader 兜底。
			return rep, fmt.Errorf("a pin batch of %d players committed in player_route but its publish was not confirmed — re-run: "+
				"the final catch-up republishes pinned rows the mapping Redis does not show yet; a pin that already reached the "+
				"cache but was not confirmed durable is left to the data_service reconcile leader (after a Redis failover it "+
				"withdraws the ready marker and re-pushes recent rows); the pinned value equals home either way: %w", len(res.Target), err)
		}
		if done {
			break
		}
		after = next
	}
	// 收尾 catch-up:home = zone 且记录已是 "{zone}:1" 的行(本次与以前钉的;建角 PinOnCreate 钉的也在其中),缓存落后的先发布确认。
	var pinned []mergeRouteRow
	if err := scanZoneHomeRows(ctx, rb.db, zone, func(page []mergeRouteRow) error {
		for _, row := range page {
			rec, err := row.placementValue()
			if err != nil {
				return err
			}
			if rec == pinValue {
				pinned = append(pinned, row)
			}
		}
		return nil
	}); err != nil {
		return rep, fmt.Errorf("catch-up scan of zone %d: %w", zone, err)
	}
	cu, err := rb.pub.catchUpRoutes(ctx, pinned)
	if err != nil {
		return rep, fmt.Errorf("catch-up publish of %d rows pinned to %q: %w", len(pinned), pinValue, err)
	}
	log.Printf("pin-placement: catch-up over %d rows pinned to %q: %d already current in the mapping Redis, %s",
		len(pinned), pinValue, cu.Current, cu.Published)
	return rep, nil
}

// countPinVerdict 把 -mode pin-placement 的一个判定计进报告(这里「已有别的记录」与「home 已走」都是正常情形)。
func countPinVerdict(rep *pinReport, v pinVerdict) {
	switch v {
	case pinVerdictPin:
		rep.Pinned++
	case pinVerdictAlreadyPinned:
		rep.AlreadyPinned++
	case pinVerdictKept:
		rep.Kept++
	default:
		rep.HomeMoved++
	}
}
