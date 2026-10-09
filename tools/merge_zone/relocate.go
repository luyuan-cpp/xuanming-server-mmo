package main

// -mode relocate:冻结式单玩家 / 批量搬库(docs/design/player-storage-placement.md §9)。
//
// 把一批玩家的主数据从落点库 S 搬到 T,不要求玩家离线:
//
//	R0  校验 S≠T、两库存在、列名对齐;检查能力标记;清单在任何写之前落盘(relocate_run.go)
//	R1  冻结:逐人一段 Lua(placementCASScript)原子确认「记录与 home 仍是刚读到的值、home 所在 zone 没有
//	    合服围栏」后写 "{S}:{v}:frozen:{run}"。有效落点 == S、记录不在冻结态由 Go 侧用同一套编解码判定
//	    (decideFreeze);Lua 确认的正是判定所依据的那两个原值没变,两者合起来等价于原子完成。不满足的人跳过
//	R2  等在途写:每人每张玩家表的 go/db 排序锁都至少一次观察到不存在;超时 → 本批解冻
//	R3  拷贝:每人一个事务,T 里该玩家的旧行(冷副本)先删后插,INSERT … SELECT 自 S,按列名对位
//	R4  逐人逐表逐字节比对 S 与 T;不一致 → 解冻该人
//	R5  切换:CAS "{S}:{v}:frozen:{run}" → "{T}:{v+1}";CAS 失败 → 报警,该人保持原状
//
// 为什么冻结之后才拷:冻结期间 go/db 把该玩家的写延后(不耗重试次数),S 不再变化,拷走的就是最后一份;
// 切换之后被延后的写按 per-key 游标全序落到 T。正确性不依赖 R2 的等待:锁过期之后才落进 S 的在途写,
// go/db 的落库后复核看到冻结 / 切换会不标记游标、稍后重投到 T(§6.3 末段)。R2 只是少一些重投与 R4 比对失败。
//
// 与合服互斥(§9 末条):R1 的 Lua 检查玩家 home 所在 zone 的合服围栏;合服在立围栏之后检查清单玩家
// 没有冻结记录(merge_run.go 的落点扫描)。两边各自「先写自己的标记、再查对方的标记」,落在同一个 Redis 上,
// 不会同时放行。
//
// 本文件是与存储无关的状态机(relocateEngine),Redis / MySQL 经三个接缝注入:生产实现在 relocate_run.go,
// 单测(relocate_test.go)注入内存替身。
//
// 缓存不删(§9):T 的内容 = S 在冻结时刻的内容,之后的写都经游标落 T;共享缓存的键不带落点,内容也没变。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"
)

// relocatePlacements 是 R1 / R5 / 解冻用到的落点读写(生产:mapping Redis,readPlacements / runPlacementCAS)。
// cas 出错时返回已拿到的那部分结果(与 ops 同序的前缀),调用方先处理它们再上抛错误。
type relocatePlacements interface {
	read(ctx context.Context, ids []uint64) ([]placementRead, error)
	cas(ctx context.Context, ops []placementCAS) ([]placementCASResult, error)
}

// relocateLocks 是 R2 的排序锁探测(生产:go/db 的 RedisClient,即 shared DB 0 上的 EXISTS)。
type relocateLocks interface {
	exists(ctx context.Context, keys []string) ([]bool, error)
}

// relocateRows 是 R3 / R4(生产:MySQL,sqlRelocateRows)。
type relocateRows interface {
	// copyPlayer 在一个事务里把 playerID 在各玩家表的行从 S 拷到 T(先删 T 里的旧行),返回删掉的旧行数。
	copyPlayer(ctx context.Context, playerID uint64) (coldRows int, err error)
	// mismatched 返回 ids 里 S 与 T 不逐字节相同的玩家(任一张表:一边有行一边没有,或有列不同)。
	mismatched(ctx context.Context, ids []uint64) ([]uint64, error)
}

// errRelocateLockWait 表示 R2 在期限内没等到排序锁全部消失(本批解冻,不是基础设施故障)。
var errRelocateLockWait = errors.New("in-flight writes did not finish in time")

// relocateEngine 驱动 R1~R5。构造后单线程使用。
type relocateEngine struct {
	m    *relocateManifest
	save func(*relocateManifest) error

	places relocatePlacements
	locks  relocateLocks
	rows   relocateRows

	// capZones:动手前确认过能力标记的 zone。home 不在其中的玩家不冻结 —— 处理他存盘的那个 go/db 没有核实过
	// 会按落点选库,切换之后它照样把写落进自己的 zone 库。
	capZones map[uint32]bool

	batchSize int
	lockWait  time.Duration // R2 的期限
	lockPoll  time.Duration // R2 两次探测之间的间隔
	sleep     func(ctx context.Context, d time.Duration) error
	now       func() time.Time
}

// newRelocateEngine 装配生产用的默认时钟与睡眠;接缝由调用方给。
func newRelocateEngine(m *relocateManifest, save func(*relocateManifest) error, places relocatePlacements,
	locks relocateLocks, rows relocateRows, capZones map[uint32]bool, batchSize int, lockWait time.Duration) *relocateEngine {
	return &relocateEngine{
		m: m, save: save, places: places, locks: locks, rows: rows, capZones: capZones,
		batchSize: batchSize, lockWait: lockWait, lockPoll: relocateLockPoll,
		sleep: sleepCtx, now: time.Now,
	}
}

// relocateLockPoll:R2 探测间隔。go/db 为被延后的写也会短暂持锁(先拿排序锁再选库),探测过密只会多看到这些
// 转瞬即逝的锁;200ms 让一批的等待在锁真的空下来之后很快结束。
const relocateLockPoll = 200 * time.Millisecond

// defaultRelocateLockWait:R2 期限的默认值。略长于 go/db 的排序锁 TTL(orderingLockTTL = 2m):持锁进程死掉时
// 锁最迟在 TTL 到期后消失;活着的持有者每 30s 续期,能撑过这个期限的是一条异常慢的写,本批解冻、稍后另起一次。
const defaultRelocateLockWait = 150 * time.Second

func (e *relocateEngine) stamp(p *relocatePlayer, state, reason string) {
	p.State = state
	p.Reason = reason
	p.At = e.now().UTC().Format(time.RFC3339)
}

// run 按清单顺序、每 batchSize 人一批,处理所有未到终态的玩家。出错即停:已冻结的人留在冻结态
// (清单与 Redis 都记着),续跑用同一份清单,放弃用 -mode relocate-abort。
func (e *relocateEngine) run(ctx context.Context) error {
	var todo []int
	for i, p := range e.m.Players {
		if !relocFinal(p.State) {
			todo = append(todo, i)
		}
	}
	size := max(e.batchSize, 1)
	for start := 0; start < len(todo); start += size {
		if err := ctx.Err(); err != nil {
			return err
		}
		idx := todo[start:min(start+size, len(todo))]
		if err := e.runBatch(ctx, idx); err != nil {
			return err
		}
		log.Printf("relocate: batch %d/%d done — %s", start/size+1, (len(todo)+size-1)/size, e.m.relocSummary())
	}
	return nil
}

// runBatch 对一批玩家跑 R1~R5,前后各落一次盘。
func (e *relocateEngine) runBatch(ctx context.Context, idx []int) error {
	// R1
	if err := e.freeze(ctx, idx); err != nil {
		return fmt.Errorf("R1 freeze: %w", err)
	}
	// 冻结结果先落盘(版本号 v 与 home),再做任何依赖它的事:崩溃之后续跑靠它认领已切换的人。
	if err := e.save(e.m); err != nil {
		return fmt.Errorf("persist the manifest after R1 (players stay frozen; resume with the same manifest): %w", err)
	}
	frozen := e.inState(idx, relocFrozen)
	if len(frozen) > 0 {
		// R2
		if err := e.awaitOrderingLocks(ctx, frozen); err != nil {
			if !errors.Is(err, errRelocateLockWait) {
				return fmt.Errorf("R2 wait for in-flight writes: %w", err)
			}
			if uerr := e.unfreeze(ctx, frozen, "R2: "+err.Error()); uerr != nil {
				return uerr
			}
			frozen = nil
		}
		// R3
		var copied []int
		for _, i := range frozen {
			p := &e.m.Players[i]
			cold, err := e.rows.copyPlayer(ctx, p.PlayerID)
			if err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return fmt.Errorf("R3 copy of player %d interrupted: %w", p.PlayerID, errors.Join(err, cerr))
				}
				if uerr := e.unfreeze(ctx, []int{i}, "R3 copy failed (transaction rolled back): "+err.Error()); uerr != nil {
					return uerr
				}
				continue
			}
			p.ColdCopyRows = cold
			copied = append(copied, i)
		}
		// R4
		verified := copied
		if len(copied) > 0 {
			bad, err := e.rows.mismatched(ctx, e.idsOf(copied))
			switch {
			case err != nil:
				if cerr := ctx.Err(); cerr != nil {
					return fmt.Errorf("R4 compare interrupted: %w", errors.Join(err, cerr))
				}
				// 比不成就不能证明 T 是完整的一份:全部解冻,一个都不切。
				if uerr := e.unfreeze(ctx, copied, "R4 compare failed: "+err.Error()); uerr != nil {
					return uerr
				}
				verified = nil
			case len(bad) > 0:
				badSet := make(map[uint64]bool, len(bad))
				for _, id := range bad {
					badSet[id] = true
				}
				var badIdx []int
				verified = nil
				for _, i := range copied {
					if badSet[e.m.Players[i].PlayerID] {
						badIdx = append(badIdx, i)
					} else {
						verified = append(verified, i)
					}
				}
				if uerr := e.unfreeze(ctx, badIdx, "R4: source and target rows differ after the copy"); uerr != nil {
					return uerr
				}
			}
		}
		// R5
		if err := e.switchOver(ctx, verified); err != nil {
			return fmt.Errorf("R5 switch: %w", err)
		}
	}
	if err := e.save(e.m); err != nil {
		return fmt.Errorf("persist the manifest after R5: %w", err)
	}
	return nil
}

func (e *relocateEngine) inState(idx []int, state string) []int {
	var out []int
	for _, i := range idx {
		if e.m.Players[i].State == state {
			out = append(out, i)
		}
	}
	return out
}

func (e *relocateEngine) idsOf(idx []int) []uint64 {
	out := make([]uint64, len(idx))
	for k, i := range idx {
		out[k] = e.m.Players[i].PlayerID
	}
	return out
}

// ── R1 ───────────────────────────────────────────────────────────

type freezeKind int

const (
	freezeGo            freezeKind = iota // 条件满足:发冻结 CAS
	freezeSkip                            // 条件不满足:跳过,本次不碰
	freezeAdoptFrozen                     // 本次 run_id 的冻结值已在(崩溃续跑):认回冻结态
	freezeAdoptSwitched                   // 已是 "{T}:{v+1}"(R5 之后、落盘之前崩溃):认回已切换
)

type freezeDecision struct {
	kind    freezeKind
	op      placementCAS // freezeGo
	version uint64       // freezeGo / freezeAdoptFrozen
	home    uint32       // freezeGo / freezeAdoptFrozen(0 = 认领时 home 读不到)
	reason  string       // freezeSkip
}

// decideFreeze 是 R1 的纯判定。p 是清单里的这名玩家(未到终态),rd 是刚读到的记录与 home。
//
// 冻结条件(§9 R1):有效落点 == S(有记录看记录,无记录要求 home == S)、记录不在冻结态;另要求 home 可读
// (R2 按它拼 topic)且所在 zone 核实过能力标记。home 所在 zone 没有合服围栏由 CAS 脚本在写的那一刻原子确认
// (FenceZone),期望的记录 / home 原值就是这里读到的,所以判定与写入之间被改过的一律不写。
func decideFreeze(p relocatePlayer, rd placementRead, s, t uint32, runID string, capZones map[uint32]bool) freezeDecision {
	if rd.RecordPresent && rd.RecordErr == nil {
		rec := rd.Record
		if rec.Frozen && rec.RunID == runID {
			home := uint32(0)
			if rd.HomePresent {
				home = rd.Home
			}
			return freezeDecision{kind: freezeAdoptFrozen, version: rec.Version, home: home}
		}
		if !rec.Frozen && p.Version != 0 && rec.StorageID == t && rec.Version == p.Version+1 {
			return freezeDecision{kind: freezeAdoptSwitched}
		}
	}
	skip := func(format string, args ...any) freezeDecision {
		return freezeDecision{kind: freezeSkip, reason: fmt.Sprintf(format, args...)}
	}
	switch {
	case rd.HomeErr != nil:
		return skip("player:zone is malformed (%q): %v", rd.HomeRaw, rd.HomeErr)
	case !rd.HomePresent:
		return skip("no player:zone — the db_task topic of the in-flight writes cannot be derived")
	case !capZones[rd.Home]:
		return skip("home zone %d was not capability-checked (-db-capability-zones)", rd.Home)
	}
	op := placementCAS{PlayerID: p.PlayerID, ExpectHome: rd.HomeRaw, FenceZone: rd.Home}
	var v uint64
	switch {
	case rd.RecordPresent && rd.RecordErr != nil:
		return skip("malformed placement record %q", rd.RecordRaw)
	case rd.RecordPresent && rd.Record.Frozen:
		return skip("already frozen by run %s", rd.Record.RunID)
	case rd.RecordPresent:
		if rd.Record.StorageID != s {
			return skip("effective storage is %d (placement record %q), not %d", rd.Record.StorageID, rd.RecordRaw, s)
		}
		v = rd.Record.Version
		op.ExpectRecord = rd.RecordRaw
	default:
		if rd.Home != s {
			return skip("no placement record: effective storage is home zone %d, not %d", rd.Home, s)
		}
		v = 1
		op.ExpectAbsent = true
	}
	op.NewValue = frozenValue(s, v, runID)
	return freezeDecision{kind: freezeGo, op: op, version: v, home: rd.Home}
}

// freeze 是 R1:读 → 判定 → 冻结 CAS。认领回来的冻结者若 home 读不到或 zone 没核实过能力,当场解冻。
func (e *relocateEngine) freeze(ctx context.Context, idx []int) error {
	reads, err := e.places.read(ctx, e.idsOf(idx))
	if err != nil {
		return err
	}
	if len(reads) != len(idx) {
		return fmt.Errorf("placement read returned %d answers for %d players", len(reads), len(idx))
	}
	s, t := e.m.SourceStorage, e.m.TargetStorage
	var ops []placementCAS
	var opIdx []int
	var opDec []freezeDecision
	var orphans []int // 认领回来、却拼不出 topic 的冻结者
	for k, i := range idx {
		p := &e.m.Players[i]
		d := decideFreeze(*p, reads[k], s, t, e.m.RunID, e.capZones)
		switch d.kind {
		case freezeAdoptFrozen:
			p.Version, p.HomeZone = d.version, d.home
			e.stamp(p, relocFrozen, "")
			if d.home == 0 || !e.capZones[d.home] {
				orphans = append(orphans, i)
			}
		case freezeAdoptSwitched:
			p.Final = stableValue(t, p.Version+1)
			e.stamp(p, relocSwitched, "")
		case freezeSkip:
			e.stamp(p, relocSkipped, d.reason)
		case freezeGo:
			ops = append(ops, d.op)
			opIdx = append(opIdx, i)
			opDec = append(opDec, d)
		}
	}
	if len(orphans) > 0 {
		if err := e.unfreeze(ctx, orphans, "adopted this run's freeze but its home zone is unreadable or not capability-checked"); err != nil {
			return err
		}
	}
	results, err := e.places.cas(ctx, ops)
	for k, res := range results {
		if k >= len(opIdx) {
			break
		}
		p := &e.m.Players[opIdx[k]]
		d := opDec[k]
		switch res.Outcome {
		case placementCASWritten:
			p.Version, p.HomeZone = d.version, d.home
			e.stamp(p, relocFrozen, "")
		case placementCASFenced:
			e.stamp(p, relocSkipped, fmt.Sprintf("home zone %d is being merged (%s present at freeze time)", d.home, mergeFenceKey(d.home)))
		case placementCASHomeChanged:
			e.stamp(p, relocSkipped, fmt.Sprintf("player:zone changed between the read and the freeze (now %q)", res.CurrentHome))
		default:
			e.stamp(p, relocSkipped, fmt.Sprintf("placement record changed between the read and the freeze (now %q)", res.CurrentRecord))
		}
	}
	// 结果没回来的那部分 CAS 可能已经生效:他们留在 pending,续跑时按本次 run_id 的冻结值认领。
	return err
}

// ── R2 ───────────────────────────────────────────────────────────

// orderingLockKey 镜像 go/db 的跨实例排序锁键:locker.NewRedisLocker 的默认前缀 "distributed:lock:" +
// key_ordered_consumer.go 的 orderingLockKey "kafka:ordering:{topic}:{key}:{msgType}"。
// key = player_id(db_task 的 Key),msgType = proto message 全名,即玩家表名(proto/common/database 没有 package)。
// 锁在 go/db 的 RedisClient 上(shared DB 0,与 kafka:retry:queue:* 同库)。改一处必须同步另一处。
func orderingLockKey(topic string, playerID uint64, msgType string) string {
	return "distributed:lock:kafka:ordering:" + topic + ":" + strconv.FormatUint(playerID, 10) + ":" + msgType
}

// relocateLockKeys 列出一批冻结玩家在各玩家表上的排序锁键(topic 按冻结时的 home 与世代号拼)。
func relocateLockKeys(players []relocatePlayer, tables []string, generation uint32) []string {
	keys := make([]string, 0, len(players)*len(tables))
	for _, p := range players {
		topic := dbTaskTopic(p.HomeZone, generation)
		for _, t := range tables {
			keys = append(keys, orderingLockKey(topic, p.PlayerID, t))
		}
	}
	return keys
}

// awaitOrderingLocks 是 R2:每把锁都至少一次观察到不存在即可。
//
// 为什么「观察到一次不存在」就够:冻结之前已经选好库、正在往 S 写的任务,从拿锁到复核完一直持锁;一旦看到
// 它的锁不在,它就已经写完了。之后再出现的持锁者都是冻结之后才选库的,会看到冻结而延后,不会写 S。
// 期限到了仍有锁在:本批解冻(errRelocateLockWait),由调用方处理。Redis 故障 / ctx 取消原样上抛。
func (e *relocateEngine) awaitOrderingLocks(ctx context.Context, idx []int) error {
	players := make([]relocatePlayer, len(idx))
	for k, i := range idx {
		players[k] = e.m.Players[i]
	}
	remaining := relocateLockKeys(players, e.m.Tables, e.m.TopicGeneration)
	deadline := e.now().Add(e.lockWait)
	for {
		present, err := e.locks.exists(ctx, remaining)
		if err != nil {
			return err
		}
		if len(present) != len(remaining) {
			return fmt.Errorf("lock probe returned %d answers for %d keys", len(present), len(remaining))
		}
		next := make([]string, 0, len(remaining))
		for k, key := range remaining {
			if present[k] {
				next = append(next, key)
			}
		}
		remaining = next
		if len(remaining) == 0 {
			return nil
		}
		if !e.now().Before(deadline) {
			return fmt.Errorf("%w: %d go/db ordering lock(s) still held after %s (first: %v)",
				errRelocateLockWait, len(remaining), e.lockWait, remaining[:min(len(remaining), 5)])
		}
		if err := e.sleep(ctx, e.lockPoll); err != nil {
			return err
		}
	}
}

// ── 解冻 / 切换(R5)────────────────────────────────────────────

// unfreeze 把冻结者 CAS 回 "{S}:{v}"(无记录者冻结时按 v=1,解冻后留下 "{S}:1":有效落点与冻结前相同,
// 版本不变,P-2)。CAS 没写成(记录已不是本次的冻结值)的人记 cas_failed,留给人看。
func (e *relocateEngine) unfreeze(ctx context.Context, idx []int, reason string) error {
	if len(idx) == 0 {
		return nil
	}
	s := e.m.SourceStorage
	ops := make([]placementCAS, len(idx))
	for k, i := range idx {
		p := e.m.Players[i]
		ops[k] = placementCAS{PlayerID: p.PlayerID, ExpectRecord: frozenValue(s, p.Version, e.m.RunID), NewValue: stableValue(s, p.Version)}
	}
	results, err := e.places.cas(ctx, ops)
	for k, res := range results {
		if k >= len(idx) {
			break
		}
		p := &e.m.Players[idx[k]]
		if res.Outcome == placementCASWritten {
			p.Final = stableValue(s, p.Version)
			e.stamp(p, relocUnfrozen, reason)
			log.Printf("relocate: player %d unfrozen back to %s — %s", p.PlayerID, p.Final, reason)
			continue
		}
		e.stamp(p, relocCASFailed, fmt.Sprintf("%s; unfreeze found %q instead of this run's frozen value — left as is", reason, res.CurrentRecord))
		log.Printf("ERROR: relocate: player %d: %s", p.PlayerID, p.Reason)
	}
	if err != nil {
		return fmt.Errorf("unfreeze (%s): %w", reason, err)
	}
	return nil
}

// switchOver 是 R5:CAS "{S}:{v}:frozen:{run}" → "{T}:{v+1}"。CAS 失败 = 记录被人动过,报警,该人保持原状。
func (e *relocateEngine) switchOver(ctx context.Context, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	s, t := e.m.SourceStorage, e.m.TargetStorage
	ops := make([]placementCAS, len(idx))
	for k, i := range idx {
		p := e.m.Players[i]
		ops[k] = placementCAS{PlayerID: p.PlayerID, ExpectRecord: frozenValue(s, p.Version, e.m.RunID), NewValue: stableValue(t, p.Version+1)}
	}
	results, err := e.places.cas(ctx, ops)
	for k, res := range results {
		if k >= len(idx) {
			break
		}
		p := &e.m.Players[idx[k]]
		if res.Outcome == placementCASWritten {
			p.Final = stableValue(t, p.Version+1)
			e.stamp(p, relocSwitched, "")
			continue
		}
		e.stamp(p, relocCASFailed, fmt.Sprintf("R5: the record is %q instead of this run's frozen value — someone changed it; "+
			"the copy in %d is NOT in use, investigate", res.CurrentRecord, t))
		log.Printf("ERROR: relocate: player %d: %s", p.PlayerID, p.Reason)
	}
	return err
}

// ── 撤销(-mode relocate-abort)───────────────────────────────────

// relocateAbortReport 是撤销的计数。
type relocateAbortReport struct {
	Unfrozen  int // 本次 run_id 的冻结值已 CAS 回稳定态(dry-run:将会)
	Switched  int // 已是 "{T}:{v+1}":R5 已做完,认回已切换,不回退(要回退就反向 relocate)
	Untouched int // 还没冻结过(或记录已不是本次的):标 skipped,不碰
	Failed    int // 解冻 CAS 没写成
}

func (r relocateAbortReport) String() string {
	return fmt.Sprintf("unfrozen=%d already_switched=%d untouched=%d cas_failed=%d", r.Unfrozen, r.Switched, r.Untouched, r.Failed)
}

// abort 把本 run 仍处冻结的玩家 CAS 回 "{S}:{v}"(§9:工具崩溃后放弃这次搬迁)。只看清单里未到终态的人:
//   - 记录是本次 run_id 的冻结值 → 解冻(版本与库取自记录本身,不依赖清单是否来得及记下 v);
//   - 记录已是 "{T}:{v+1}" → R5 已做完,认回 switched(撤销不回退已切换的人,要回就反向 relocate);
//   - 其余(从没冻结过,或记录已被别人改掉)→ 标 skipped,不碰。
//
// 调用前必须确认搬库进程已经退出:两者同时写同一份清单,后落盘的覆盖先落盘的。
func (e *relocateEngine) abort(ctx context.Context, dryRun bool) (relocateAbortReport, error) {
	var rep relocateAbortReport
	var todo []int
	for i, p := range e.m.Players {
		if !relocFinal(p.State) {
			todo = append(todo, i)
		}
	}
	t := e.m.TargetStorage
	size := max(e.batchSize, 1)
	for start := 0; start < len(todo); start += size {
		idx := todo[start:min(start+size, len(todo))]
		reads, err := e.places.read(ctx, e.idsOf(idx))
		if err != nil {
			return rep, err
		}
		if len(reads) != len(idx) {
			return rep, fmt.Errorf("placement read returned %d answers for %d players", len(reads), len(idx))
		}
		var ops []placementCAS
		var opIdx []int
		for k, i := range idx {
			p := &e.m.Players[i]
			rd := reads[k]
			rec := rd.Record
			switch {
			case rd.RecordPresent && rd.RecordErr == nil && rec.Frozen && rec.RunID == e.m.RunID:
				ops = append(ops, placementCAS{PlayerID: p.PlayerID, ExpectRecord: rd.RecordRaw,
					NewValue: stableValue(rec.StorageID, rec.Version)})
				opIdx = append(opIdx, i)
			case rd.RecordPresent && rd.RecordErr == nil && !rec.Frozen && p.Version != 0 &&
				rec.StorageID == t && rec.Version == p.Version+1:
				rep.Switched++
				if !dryRun {
					p.Final = stableValue(t, p.Version+1)
					e.stamp(p, relocSwitched, "")
				}
			default:
				rep.Untouched++
				if !dryRun {
					e.stamp(p, relocSkipped, fmt.Sprintf("run aborted; placement record is %q (not frozen by this run)", rd.RecordRaw))
				}
			}
		}
		if dryRun {
			rep.Unfrozen += len(ops)
			continue
		}
		results, err := e.places.cas(ctx, ops)
		for k, res := range results {
			if k >= len(opIdx) {
				break
			}
			p := &e.m.Players[opIdx[k]]
			if res.Outcome == placementCASWritten {
				rep.Unfrozen++
				p.Final = ops[k].NewValue
				e.stamp(p, relocUnfrozen, "aborted by -mode relocate-abort")
				continue
			}
			rep.Failed++
			e.stamp(p, relocCASFailed, fmt.Sprintf("abort: the record is %q instead of this run's frozen value — left as is", res.CurrentRecord))
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}
