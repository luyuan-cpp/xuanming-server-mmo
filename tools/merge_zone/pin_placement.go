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

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// pinPlacementEntryParams 是 -mode pin-placement 的纯数据输入(main 管 flag,本文件管语义)。
type pinPlacementEntryParams struct {
	zone        uint32
	mappingAddr string
	mappingPwd  string
	mappingDB   int
	dryRun      bool
	timeout     time.Duration
}

func runPinPlacementEntry(p pinPlacementEntryParams) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	log.Printf("=== Pin placement \"%d:1\" for players with home_zone=%d and no record (dry-run=%v, mapping=%s db=%d) ===",
		p.zone, p.zone, p.dryRun, p.mappingAddr, p.mappingDB)
	mapRdb := mustDial(ctx, "mapping", p.mappingAddr, p.mappingPwd, p.mappingDB)
	defer mapRdb.Close()

	rep, err := pinZonePlacements(ctx, mapRdb, p.zone, p.dryRun)
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
