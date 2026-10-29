package main

// 并发围栏(fence):合服跑的这段时间里,别人不许往被合的两个 zone 里塞新对象。
//
// -route-source=db 时第 1 把的真源改为全局库 zone_merge_fence,merge:in_progress:{zone} 降为镜像(无「按 run_id 子串删」,
// 改按完整值删),见本文件后半「-route-source=db:库围栏」一节;下面的契约描述的是 redis 源(逐字不变)。
//
// 两把锁,职责不同:
//
//  1. merge:in_progress:{zone} —— **广播型**围栏,写在 mapping Redis(data_service
//     的 MappingRedis,dev/prod 都是 DB 0:go-zero RedisConf 没有 DB 字段)。它不互斥任何东西,是给别的服务
//     看的一面旗:data_service 的 RegisterPlayerZone 与 guild 的 CreateGuild
//     在写之前 EXISTS 这把键,存在就拒绝本次创建。
//
//     为什么必须有:合服工具在 T 时刻扫出「源区有 N 个玩家」,之后逐面搬。
//     如果这中间有人建了个新号并 RegisterPlayerZone(src),他既不在清单里
//     (不会被搬),又留着一个指向已下线 zone 的映射 —— 合服结束后这个号
//     永远登不上,而且没人知道他存在。公会同理。
//
//     契约(与并发实现该逻辑的另一位保持一致):
//       key    : merge:in_progress:{zone_id}
//       where  : data_service MappingRedis(data_service.yaml 的 MappingRedis;go-zero 无 DB 字段 ⇒ 恒 DB 0)
//       value  : JSON,见 mergeInProgressValue
//       TTL    : 有(见 acquireMergeFence),绝不是永久键 —— 工具被 kill 时
//                必须自己过期,否则整个服会被一把忘了删的键钉死
//       reader : 只做 EXISTS 判断即可;value 是给人看的排障信息,不参与判定
//       writer : 只有 tools/merge_zone。释放时按 run_id 校验后再删(Lua),
//                不会误删别人的围栏
//     两个 zone 都会被打标(src 与 dst):目标区在合服窗口里同样不能进新对象,
//     否则 guild_rank ZSET 合并与玩家行拷贝的计数都对不上。读者按**自己要写的
//     那个 zone** 查 merge:in_progress:{那个 zone},src/dst 都覆盖到。
//
//  2. guild_rank:maintenance_lock —— **互斥锁**,写在 guild 服自己的 Redis(RedisClient,2026-10-23 起是 DB 0)。
//     语义直接抄 go/guild/internal/data/guild_repo.go 的 acquireRankLock:
//     SETNX + TTL 5min,值是随机 token,释放走 releaseRankLockScript(比对 token
//     再 DEL)。guild 服的榜单重建 / 分数回填在同一把锁下跑,合服的 ZSET 合并
//     必须和它们互斥,否则一边在 ZADD 一边在重建,结果不可预期。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// mergeFenceKey 是广播围栏的键。zone 是**被保护的那个 zone**。
func mergeFenceKey(zone uint32) string {
	return "merge:in_progress:" + strconv.FormatUint(uint64(zone), 10)
}

// mergeInProgressValue 是围栏键的 JSON 值。字段只增不改名:别的服务可能已经
// 在日志里打它了。判定用 EXISTS,不解析这个结构 —— 这里的内容纯为排障。
type mergeInProgressValue struct {
	Tool            string `json:"tool"`               // 固定 "tools/merge_zone"
	RunID           string `json:"run_id"`             // 与 manifest.RunID 相同
	SourceZone      uint32 `json:"source_zone"`        //
	TargetZone      uint32 `json:"target_zone"`        //
	StartedAt       string `json:"started_at"`         // RFC3339 UTC
	StartedAtUnixMs int64  `json:"started_at_unix_ms"` //
	ExpiresAtUnixMs int64  `json:"expires_at_unix_ms"` // 与 TTL 一致,给人看的
	Operator        string `json:"operator"`           // host/pid
	ManifestPath    string `json:"manifest_path"`      // 出事时去哪找清单
}

// releaseFenceScript:只删「还是我写的那一把」。合服跑超时被别人接手时,
// 后来者写的围栏不能被前一个进程的 defer 删掉。
var releaseFenceScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur == false then return 0 end
if string.find(cur, ARGV[1], 1, true) == nil then return 0 end
return redis.call("DEL", KEYS[1])
`)

// refreshFenceScript:同样的所有权校验后续期。
var refreshFenceScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur == false then return 0 end
if string.find(cur, ARGV[1], 1, true) == nil then return 0 end
return redis.call("PEXPIRE", KEYS[1], ARGV[2])
`)

// mergeFence 是已获取的围栏句柄。
type mergeFence struct {
	rdb   *redis.Client
	keys  []string
	runID string
	ttl   time.Duration
	stop  chan struct{}
}

// acquireMergeFence 给 src 与 dst 各打一把围栏。
//
// TTL 的取法:必须**比整个 run 长**,否则跑到一半围栏过期,别人就能在合服
// 中途插入新对象。但也不能是永久键 —— 进程被 kill 时得自己消失。取法是
// runTimeout + 一个下限,并且起一个后台 goroutine 每 ttl/3 续期一次。
// 续期失败(键被人删了 / Redis 抖动)只记日志:真正的保护是 TTL 本身,
// 而 TTL 已经覆盖了整个预算内的 run。
func acquireMergeFence(
	ctx context.Context,
	rdb *redis.Client,
	src, dst uint32,
	runID, manifestPath string,
	runTimeout time.Duration,
	dryRun bool,
) (*mergeFence, error) {
	ttl := mergeFenceTTL(runTimeout)
	now := time.Now()
	val := mergeInProgressValue{
		Tool:            "tools/merge_zone",
		RunID:           runID,
		SourceZone:      src,
		TargetZone:      dst,
		StartedAt:       now.UTC().Format(time.RFC3339),
		StartedAtUnixMs: now.UnixMilli(),
		ExpiresAtUnixMs: now.Add(ttl).UnixMilli(),
		Operator:        operatorTag(),
		ManifestPath:    manifestPath,
	}
	payload, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("marshal merge fence value: %w", err)
	}

	f := &mergeFence{rdb: rdb, runID: runID, ttl: ttl, stop: make(chan struct{})}
	for _, zone := range []uint32{src, dst} {
		key := mergeFenceKey(zone)
		if dryRun {
			// dry-run 不写围栏,但要报告它会不会撞上别人的。
			existing, gerr := rdb.Get(ctx, key).Result()
			if gerr == nil {
				return nil, fmt.Errorf("another merge is already fencing zone %d: %s", zone, existing)
			}
			if gerr != nil && !errors.Is(gerr, redis.Nil) {
				return nil, fmt.Errorf("get %s: %w", key, gerr)
			}
			log.Printf("[DRY-RUN] would SET %s (ttl=%s)", key, ttl)
			continue
		}
		ok, serr := rdb.SetNX(ctx, key, string(payload), ttl).Result()
		if serr != nil {
			f.release(context.Background())
			return nil, fmt.Errorf("set %s: %w", key, serr)
		}
		if !ok {
			existing, _ := rdb.Get(ctx, key).Result()
			f.release(context.Background())
			return nil, fmt.Errorf("another merge is already fencing zone %d: %s "+
				"(if that run is dead, DEL %s manually after confirming no other merge_zone process is alive)",
				zone, existing, key)
		}
		f.keys = append(f.keys, key)
		log.Printf("merge fence acquired: %s ttl=%s run_id=%s", key, ttl, runID)
	}
	if !dryRun && len(f.keys) > 0 {
		go f.keepAlive()
	}
	return f, nil
}

// mergeFenceTTL 是围栏的 TTL:max(-timeout + 30m, 1h)。Redis 围栏与库围栏同一口径(规格 §6.3 W12)。
func mergeFenceTTL(runTimeout time.Duration) time.Duration {
	ttl := runTimeout + 30*time.Minute
	if ttl < time.Hour {
		ttl = time.Hour
	}
	return ttl
}

func (f *mergeFence) keepAlive() {
	t := time.NewTicker(f.ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			for _, k := range f.keys {
				if _, err := refreshFenceScript.Run(context.Background(), f.rdb,
					[]string{k}, f.runID, strconv.FormatInt(f.ttl.Milliseconds(), 10)).Int(); err != nil {
					log.Printf("WARN: refresh merge fence %s: %v (TTL still covers the budgeted run)", k, err)
				}
			}
		}
	}
}

// release 删掉自己写的围栏。可重复调用。
func (f *mergeFence) release(ctx context.Context) {
	if f == nil {
		return
	}
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	for _, k := range f.keys {
		n, err := releaseFenceScript.Run(ctx, f.rdb, []string{k}, f.runID).Int()
		switch {
		case err != nil:
			log.Printf("WARN: release merge fence %s: %v (it will expire on its own within %s)", k, err, f.ttl)
		case n == 0:
			// 键已经不在,或者挂在别人的 run_id 上(我们超时后有人接手了)。
			// 两种都不该悄悄过去:后者意味着这个进程的写与接手者的写交错过。
			log.Printf("WARN: merge fence %s was not ours to release (expired, or another run took it over)", k)
		default:
			log.Printf("merge fence released: %s", k)
		}
	}
	f.keys = nil
}

// refuseAndRelease 是「围栏已立、还一个字节都没写」时的拒绝出口(合服:首跑时清单落盘之前;撤销:首次撤销时
// 第一步写之前):打出原因,正常释放围栏,非零退出(2026-09-28,player-storage-placement.md §12 A2)。
// 续跑时(合服:清单已存在且步骤 7 未标记完成;撤销:已有清单玩家被改回源区)上一次运行可能已写到一半,
// 「这次什么都没写」不等于「两个 zone 没有半截状态」,调用方改走 abortKeepingFence 保留围栏(见 runMerge /
// runUnmerge 的 refuse),不走这里。
//
// 不能用 log.Fatalf:它不跑 defer,围栏会一直挂到 TTL(-timeout+30m,下限 1h)。这段时间里两个 zone 的
// 建号 / 建帮全被拒,而此刻什么都没写、没有任何需要保护的半截状态 —— 保留围栏只有代价。
// 写过之后的失败走各自的 abortKeepingFence(fenceKeptAbortMessage),那里保留围栏才是对的。
// dry-run 的句柄没有键,release 是空操作。
func (f *mergeFence) refuseAndRelease(format string, args ...any) {
	log.Printf("ERROR: "+format, args...)
	held := f != nil && len(f.keys) > 0
	f.release(context.Background())
	if held {
		log.Printf("Refused before this run's first write: this run wrote nothing and its merge fence was released. " +
			"Fix the cause above and re-run the same command.")
	} else {
		log.Printf("Refused before this run's first write: this run wrote nothing. Fix the cause above and re-run the same command.")
	}
	os.Exit(1)
}

// ── -route-source=db:库围栏(W12,规格 §6.3、§13.1 第 3 条、§13.3 第 1、4 条)──────────────
//
// 真源是全局库的 zone_merge_fence(每个 zone 64 片,route_store.go);merge:in_progress:{zone} 降为镜像:值 = 库里的
// holder_json(带 fence_epoch),PEXPIREAT = 库 expires_at_ms + 10 分钟,按 fence_epoch set-if-newer 写(routeFenceMirrorSetScript)。
// 读方(data_service / guild / scene_manager)的判据不变:镜像存在即封锁;db_first 的 W1 另在库内复核围栏分片。
//
//   - 获取:一个库事务写满两区全部分片 → 提交 → 两区镜像各一次 EVAL + WAITAOF(同一连接)→ **镜像确认之后**才返回,调用方
//     才收集清单(库已封锁而镜像未写的那一刻,W1 已在库内拦住,但 scene_manager / guild 还看不见)。镜像写不成 → 先清库、
//     再按完整值删已写的镜像,返回错误(本次什么都没写)。-take-over-fence 接手与合服续跑(keepOnFailure:清单已存在且步骤 7
//     未标记完成)例外:获取之后的任何失败都不释放(见下)。
//   - 续期:routeFenceGuard 每 min(ttl/3, 30s) 一次(routeFenceRenewMaxInterval),受影响行数不足 = 确定失权。每个写步骤每批之前查 check():失权、本地截止时刻
//     (最后一次成功续期 + ttl − 60s)已过、或镜像里出现更大的 fence_epoch,即中止、退出非 0。writeContext 派生的 ctx 同时
//     被看门狗取消,批内与本文件改不到的步骤(拷行、热状态、公告打标)的下一条命令随之失败 —— 失权之后不再写任何东西
//     (Redis 围栏的续期失败只打 WARN)。
//   - 释放:先清库(只清本次 run_id 的分片),再按完整值 compare-and-delete 镜像;删镜像失败由对账 leader 按库删(多留只会多封锁)。
//   - 续跑接手(-take-over-fence <run_id>):写到一半中止时库围栏故意保留(routeFenceKeptAbortMessage);续跑在一个库事务里把它
//     从旧 run_id 接到本次(TakeOverFence),两区一刻不解封;镜像按更大的 fence_epoch 覆盖旧值。接手的前提是上一次已写到一半,
//     所以接手事务结果不明、接手之后镜像没确认,都**不释放**库围栏(释放 = 在半合服 / 半撤销状态下解封),只报出围栏此刻归谁、
//     怎么续跑;接手事务在库里核对 zone 对、清单路径,且每个 zone 要么 64 片全接、要么一片不接(route_store.go acquireFence)。
//     不带 -take-over-fence 的合服续跑(上一次的围栏已过期)同一口径:清单已存在说明两区可能处在半截状态,获取之后失败也不释放。

// runFence 是一次持围栏运行(合服 / 撤销)手里的围栏:redis 源是 *mergeFence(今天的 Redis 围栏,逐字不变),
// db 源是 *routeMergeFence(库围栏 + 镜像)。
type runFence interface {
	// release 释放本次运行的围栏,可重复调用。
	release(ctx context.Context)
	// refuseAndRelease 是「围栏已立、还一个字节都没写」时的拒绝出口:打原因、释放围栏、退出 1。
	refuseAndRelease(format string, args ...any)
	// check 是每个写步骤每批之前的判据:nil = 仍持有。redis 源恒为 nil(Redis 围栏没有失权判据,行为与今天相同)。
	check() error
	// writeContext 派生写步骤用的 ctx(只调一次):db 源在 check() 变成非 nil 时以该错误取消它(context.Cause 可取回);
	// redis 源原样返回。
	writeContext(ctx context.Context) context.Context
}

// check:Redis 围栏的续期失败只打 WARN(TTL 已覆盖整个预算内的运行),没有失权判据。
func (f *mergeFence) check() error { return nil }

func (f *mergeFence) writeContext(ctx context.Context) context.Context { return ctx }

// routeFenceStore 是库围栏的原语(生产 *mergeRouteStore,route_store.go;单测注入假实现)。
type routeFenceStore interface {
	routeFenceRenewer
	AcquireFence(ctx context.Context, req routeFenceAcquire) (routeFenceGrant, error)
	TakeOverFence(ctx context.Context, req routeFenceAcquire, previousRunID string) (routeFenceGrant, error)
	ReleaseFence(ctx context.Context, zones []uint32, runID string) (map[uint32]int64, error)
	ReadFence(ctx context.Context, zone uint32) (routeFenceState, error)
}

// routeFenceMirror 是围栏镜像 merge:in_progress:{zone} 的写入(生产 *routePublisher,route_publish.go)。
type routeFenceMirror interface {
	setFenceMirror(ctx context.Context, zone uint32, holderJSON string, epoch, expiresAtMs uint64) error
	deleteFenceMirror(ctx context.Context, zone uint32, holderJSON string) (bool, error)
}

// routeFenceWatchInterval 是写 ctx 看门狗查 check() 的间隔。每批之前另有显式 check;看门狗只兜「批内」与改不到的步骤,
// 本地截止时刻本身比库里的过期时刻提前 60s,1s 的轮询延迟落在这段余量里。
const routeFenceWatchInterval = time.Second

// routeFenceReleaseTimeout 是释放库围栏(与获取失败后的清理)的时限。路由库的 DSN 没有读写超时(routeDBDSN 只钉隔离级别、锁等待与
// sql_mode,全局钉超时会改变所有路由库操作),库网络黑洞时释放会卡到内核放弃重传(十几分钟);超时走释放的 WARN 分支 ——
// 围栏按库时钟到期,镜像随后由对账 leader 按库删。
const routeFenceReleaseTimeout = 30 * time.Second

// routeFenceParams 是获取一次运行围栏的入参(merge / unmerge 共用)。
type routeFenceParams struct {
	src, dst     uint32
	runID        string
	purpose      string // routeFencePurposeMerge / routeFencePurposeUnmerge
	manifestPath string
	runTimeout   time.Duration
	dryRun       bool
	// takeOverRunID 非空 = 续跑接手该 run_id 留在库里的围栏(-take-over-fence;运维已确认那次运行的进程不在了)。
	takeOverRunID string
	// keepOnFailure = 获取之后的失败(镜像确认不了、获取事务结果不明)也不释放库围栏:调用方已知两区可能处在上一次留下的
	// 半截状态(合服续跑:清单已存在且步骤 7 未标记完成,由 runMerge 置位),此时释放 = 在半截状态下解封。
	// -take-over-fence 恒按此处理,不必再置;撤销在立围栏之前判断不了半撤销,不置。redis 源不看它。
	keepOnFailure bool
}

// acquireRunFence 按 -route-source 获取一次运行的围栏:rb == nil(redis 源)→ 今天的 acquireMergeFence,逐字不变;
// 否则 → acquireRouteFence(库围栏 + 镜像)。
func acquireRunFence(ctx context.Context, rb *routeBackend, rdb *redis.Client, p routeFenceParams) (runFence, error) {
	if rb == nil {
		if p.takeOverRunID != "" {
			return nil, errors.New("-take-over-fence only applies to -route-source=db")
		}
		f, err := acquireMergeFence(ctx, rdb, p.src, p.dst, p.runID, p.manifestPath, p.runTimeout, p.dryRun)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	f, err := acquireRouteFence(ctx, rb.db, rb.pub, p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// routeMergeFence 是 db 源的围栏句柄。release / check 可能与续期 goroutine、写 ctx 看门狗并发,可变状态由 mu 保护。
type routeMergeFence struct {
	store  routeFenceStore
	mirror routeFenceMirror
	zones  []uint32 // 升序的 src、dst
	runID  string
	grant  routeFenceGrant
	// watchInterval / sleep 是写 ctx 看门狗的轮询间隔与睡眠接缝(单测注入,不真睡)。
	watchInterval time.Duration
	sleep         func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// guard 只在 -apply 获取成功之后非 nil;dry-run 句柄没有它(check 恒 nil)。
	guard     *routeFenceGuard
	held      bool               // 库里持有、尚未释放
	lost      error              // 续期后刷新镜像时发现更大的 fence_epoch(另一个持有者接手过):当作失权
	stopWatch context.CancelFunc // 停写 ctx 看门狗
}

// acquireRouteFence 获取 db 源的围栏(见本节说明)。dry-run 只读库预判会不会撞上别人的围栏,不写库、不写镜像。
func acquireRouteFence(ctx context.Context, store routeFenceStore, mirror routeFenceMirror, p routeFenceParams) (*routeMergeFence, error) {
	zones, err := routeZonePair(p.src, p.dst)
	if err != nil {
		return nil, err
	}
	f := &routeMergeFence{store: store, mirror: mirror, zones: zones, runID: p.runID,
		watchInterval: routeFenceWatchInterval, sleep: sleepCtx}
	ttl := mergeFenceTTL(p.runTimeout)
	if p.dryRun {
		if err := f.dryRunCheck(ctx, p, ttl); err != nil {
			return nil, err
		}
		return f, nil
	}
	// 获取之前取本地时刻(带单调读数):本地截止时刻 = 它 + ttl − 60s,只会比库里的过期时刻更早。
	acquiredAt := time.Now()
	req := routeFenceAcquire{
		Zones: zones, RunID: p.runID, Purpose: p.purpose, Src: p.src, Dst: p.dst, TTL: ttl,
		Holder: mergeInProgressValue{
			Tool:            "tools/merge_zone",
			RunID:           p.runID,
			SourceZone:      p.src,
			TargetZone:      p.dst,
			StartedAt:       acquiredAt.UTC().Format(time.RFC3339),
			StartedAtUnixMs: acquiredAt.UnixMilli(),
			Operator:        operatorTag(),
			ManifestPath:    p.manifestPath,
		},
	}
	var grant routeFenceGrant
	if p.takeOverRunID != "" {
		grant, err = store.TakeOverFence(ctx, req, p.takeOverRunID)
	} else {
		grant, err = store.AcquireFence(ctx, req)
	}
	if err != nil {
		// Held / InvalidArgument:事务没写,围栏原样(别人的,或 -take-over-fence 点名的那一次的)。
		if errors.Is(err, errRouteFenceHeld) || errors.Is(err, errRouteInvalidArgument) {
			return nil, err
		}
		// 接手:唯一的例外「COMMIT 已生效而回包丢了」意味着围栏已归本次,而两区正处在上一次留下的半截状态 —— 绝不按本次 run_id
		// 清理(清掉 = 在半合服 / 半撤销状态下解封)。围栏此刻归谁,由运维查库确认后再接手。
		if p.takeOverRunID != "" {
			return nil, fmt.Errorf("接手事务结果不确定:zone %v 的 zone_merge_fence(shard=63)此刻归 run_id=%s(接手没提交)或 run_id=%s"+
				"(提交已生效、回包丢了);围栏未释放,两区在库里仍封锁。查 zone_merge_fence(shard=63)的 run_id 后,用原命令(-manifest-path %s)"+
				"加 -take-over-fence <库里那个 run_id> 重跑: %w", zones, p.takeOverRunID, p.runID, p.manifestPath, err)
		}
		// 续跑(keepOnFailure)同理:提交若已生效,围栏归本次、按库时钟到期,清掉就是在半截状态下解封。
		if p.keepOnFailure {
			return nil, fmt.Errorf("获取事务结果不确定(续跑:清单已存在,两区可能处在上一次运行留下的半截状态,不按本次 run_id 清理):"+
				"zone %v 的 zone_merge_fence(shard=63)此刻可能已归 run_id=%s(提交已生效、回包丢了)。查 zone_merge_fence(shard=63)的 run_id:"+
				"是 %s 就用原命令(-manifest-path %s)加 -take-over-fence %s 重跑;没有生效的围栏就用原命令直接重跑: %w",
				zones, p.runID, p.runID, p.manifestPath, p.runID, err)
		}
		// 获取是一个库事务,失败时通常什么都没写;唯一的例外是 COMMIT 已生效而回包丢了 —— 按本次 run_id 清一次
		// (只碰本次的分片,别人的围栏不受影响)。清理有时限(routeFenceReleaseTimeout):库网络黑洞时不卡住退出。
		cctx, cancel := context.WithTimeout(context.Background(), routeFenceReleaseTimeout)
		_, rerr := store.ReleaseFence(cctx, zones, p.runID)
		cancel()
		if rerr != nil {
			log.Printf("WARN: merge fence acquisition failed and the cleanup release of run_id=%s failed too (%v); "+
				"if the acquisition did commit, the fence expires by the DB clock", p.runID, rerr)
		}
		return nil, err
	}
	f.mu.Lock()
	f.grant, f.held = grant, true
	f.mu.Unlock()
	// keepTakenOver 是接手(或续跑,keepOnFailure)之后失败的出口:库围栏已在本次 run_id 名下,两区处在上一次留下的半截状态 ——
	// 不释放、保留在库里(held 置 false:这个句柄不再释放它;调用方随后非零退出,不走 defer)。镜像没确认的那一区,scene_manager /
	// guild 可能还看着旧值或看不见,W1 已在库内拦住,对账 leader 按库补镜像。首跑的获取照旧先清库、再删已写的镜像(本次什么都没写)。
	keep := p.takeOverRunID != "" || p.keepOnFailure
	keepTakenOver := func(what string, cause error) error {
		f.mu.Lock()
		f.held = false
		f.mu.Unlock()
		how := fmt.Sprintf("从 run_id=%s 接手", p.takeOverRunID)
		if p.takeOverRunID == "" {
			how = "获取(续跑:清单已存在,两区可能处在上一次运行留下的半截状态)"
		}
		fix := "对账 leader 会按库补镜像。排除原因(镜像:恢复 mapping Redis 及其副本的连通)后"
		if errors.Is(cause, errRouteFenceMirrorForeign) {
			// 旧格式值不是库围栏的镜像,leader 不覆盖它(规格 §13.1 第 3 条 foreign_fence):补镜像这条路走不通,要人先处理这把键。
			fix = "镜像键里是没有 fence_epoch 的旧值(旧版 merge_zone / redis 源 / 手工写的),对账 leader 不覆盖它:GET 它查清属于哪次运行、" +
				"确认那次运行已不在之后只删这一把键(它不是库围栏的镜像),然后"
		}
		return fmt.Errorf("库围栏已由 run_id=%s %s并保留(zone_merge_fence zones=%v fence_epoch=%d expires_at_ms=%d),但%s;"+
			"两区在库里仍封锁。%s,用原命令(-manifest-path %s)加 -take-over-fence %s 重跑: %w",
			p.runID, how, zones, grant.Epoch, grant.ExpiresAtMs, what, fix, p.manifestPath, p.runID, cause)
	}
	for _, zone := range zones {
		if err := mirror.setFenceMirror(ctx, zone, grant.HolderJSON, grant.Epoch, grant.ExpiresAtMs); err != nil {
			if keep {
				return nil, keepTakenOver(fmt.Sprintf("镜像 %s 未确认", mergeFenceKey(zone)), err)
			}
			f.release(context.Background())
			return nil, fmt.Errorf("merge fence mirror %s not confirmed, so the fence was released again in zone_merge_fence "+
				"(this run wrote nothing): %w", mergeFenceKey(zone), err)
		}
	}
	guard, err := newRouteFenceGuard(store, zones, p.runID, ttl, acquiredAt)
	if err != nil {
		if keep {
			return nil, keepTakenOver("续期守卫装配失败", err)
		}
		f.release(context.Background())
		return nil, err
	}
	guard.onRenewed = f.refreshMirrors
	f.mu.Lock()
	f.guard = guard
	f.mu.Unlock()
	guard.Start(ctx)
	log.Printf("merge fence acquired (route-source=db): zones=%v run_id=%s fence_epoch=%d ttl=%s; mirrors %s and %s confirmed by WAITAOF",
		zones, p.runID, grant.Epoch, ttl, mergeFenceKey(zones[0]), mergeFenceKey(zones[1]))
	return f, nil
}

// dryRunCheck 只读库(63 号片)预判:任一区的围栏正被别的 run_id 持有且未过期(-take-over-fence 点名的那一个除外)即拒绝。
func (f *routeMergeFence) dryRunCheck(ctx context.Context, p routeFenceParams, ttl time.Duration) error {
	for _, zone := range f.zones {
		st, err := f.store.ReadFence(ctx, zone)
		if err != nil {
			return err
		}
		switch {
		case st.Active && st.RunID != p.runID && p.takeOverRunID != "" && st.RunID == p.takeOverRunID:
			log.Printf("[DRY-RUN] would take over the merge fence of zone %d from run_id=%s (-take-over-fence) and mirror it to %s",
				zone, st.RunID, mergeFenceKey(zone))
		case st.Active && st.RunID != p.runID:
			return fmt.Errorf("another merge is already fencing zone %d in zone_merge_fence: run_id=%s holder=%s (if run %s is no longer "+
				"running, re-run the same command — same -manifest-path — with -take-over-fence %s; otherwise wait for it to finish or "+
				"for its expires_at_ms)", zone, st.RunID, st.HolderJSON, st.RunID, st.RunID)
		default:
			log.Printf("[DRY-RUN] would acquire the merge fence of zone %d in zone_merge_fence (%d shards, ttl=%s) and mirror it to %s",
				zone, routeFenceShards, ttl, mergeFenceKey(zone))
		}
	}
	return nil
}

// refreshMirrors 是续期成功后的回调(routeFenceGuard.onRenewed):按库里新的 expires_at_ms 刷新两区镜像的 PEXPIREAT。
// 镜像里已是更大的 fence_epoch(errRouteFenceMirrorStale)= 另一个持有者接手过:当作失权(规格 §13.1 第 3 条)。
// 其余错误只由 guard 打 WARN:镜像在获取时已按库过期时刻 + 10 分钟设过 TTL,leader 也会按库补镜像。
func (f *routeMergeFence) refreshMirrors(ctx context.Context, expiries map[uint32]uint64) error {
	var errs []error
	for _, zone := range f.zones {
		exp, ok := expiries[zone]
		if !ok {
			errs = append(errs, fmt.Errorf("merge fence renewal returned no expiry for zone %d", zone))
			continue
		}
		err := f.mirror.setFenceMirror(ctx, zone, f.grant.HolderJSON, f.grant.Epoch, exp)
		if errors.Is(err, errRouteFenceMirrorStale) {
			f.mu.Lock()
			if f.lost == nil {
				f.lost = fmt.Errorf("%w: %w", errRouteFenceLost, err)
			}
			f.mu.Unlock()
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// check 见 runFence:镜像里出现更大的 fence_epoch,或 guard 判失权(续期确定失权 / 本地截止时刻已过)即非 nil。
func (f *routeMergeFence) check() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	guard, lost := f.guard, f.lost
	f.mu.Unlock()
	if lost != nil {
		return lost
	}
	if guard == nil {
		return nil // dry-run:什么都不写
	}
	return guard.Check()
}

// writeContext 见 runFence。看门狗在 release 时停下。
func (f *routeMergeFence) writeContext(parent context.Context) context.Context {
	if f == nil {
		return parent
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guard == nil || !f.held || f.stopWatch != nil {
		return parent
	}
	ctx, cancel := context.WithCancelCause(parent)
	watchCtx, stop := context.WithCancel(ctx)
	f.stopWatch = stop
	go f.watch(watchCtx, cancel)
	return ctx
}

// watch 每 watchInterval 查一次 check(),非 nil 即以它取消写 ctx 并退出;watchCtx 结束(release / 写 ctx 结束)即退出。
func (f *routeMergeFence) watch(watchCtx context.Context, cancel context.CancelCauseFunc) {
	for {
		if err := f.sleep(watchCtx, f.watchInterval); err != nil {
			return
		}
		if err := f.check(); err != nil {
			log.Printf("ERROR: %v — cancelling every in-flight write of this run", err)
			cancel(err)
			return
		}
	}
}

// release 先停看门狗与续期,再清库(只清本次 run_id 的分片),最后对清掉了全部分片的 zone 按完整值删镜像。可重复调用。
// 清库与删镜像合计不超过 routeFenceReleaseTimeout(调用方多传 context.Background()):超时走下面的 WARN 分支,围栏按库时钟到期。
func (f *routeMergeFence) release(ctx context.Context) {
	if f == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, routeFenceReleaseTimeout)
	defer cancel()
	f.mu.Lock()
	held, guard, stop := f.held, f.guard, f.stopWatch
	f.held, f.stopWatch = false, nil
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
	if guard != nil {
		guard.Stop()
	}
	if !held {
		return
	}
	released, err := f.store.ReleaseFence(ctx, f.zones, f.runID)
	if err != nil {
		log.Printf("WARN: release merge fence in zone_merge_fence (zones=%v run_id=%s): %v — it expires by the DB clock at "+
			"expires_at_ms=%d; its mirrors stay until shortly after (more blocking, never less)", f.zones, f.runID, err, f.grant.ExpiresAtMs)
		return
	}
	for _, zone := range f.zones {
		key := mergeFenceKey(zone)
		switch n := released[zone]; {
		case n == int64(routeFenceShards):
			deleted, derr := f.mirror.deleteFenceMirror(ctx, zone, f.grant.HolderJSON)
			switch {
			case derr != nil:
				log.Printf("WARN: merge fence of zone %d released in zone_merge_fence, but deleting its mirror %s failed: %v "+
					"(the reconcile leader removes it by the DB)", zone, key, derr)
			case !deleted:
				log.Printf("WARN: merge fence of zone %d released in zone_merge_fence; its mirror %s no longer held this run's value "+
					"(expired or rewritten) and was left alone", zone, key)
			default:
				log.Printf("merge fence released: zone %d (zone_merge_fence and mirror %s)", zone, key)
			}
		case n == 0:
			// 已过期被人接手,或早已释放。不能悄悄过去:前者意味着这个进程的写与接手者的写可能交错过。
			log.Printf("WARN: merge fence of zone %d was not ours to release in zone_merge_fence (expired by the DB clock, or another "+
				"run took it over); mirror %s left alone", zone, key)
		default:
			log.Printf("WARN: merge fence of zone %d: only %d of %d shards still carried run_id=%s at release — the shard rows are "+
				"inconsistent; mirror %s left alone, check zone_merge_fence by hand", zone, n, routeFenceShards, f.runID, key)
		}
	}
}

// refuseAndRelease 同 mergeFence.refuseAndRelease(首跑在第一次写之前的拒绝出口):打原因、释放、退出 1。
func (f *routeMergeFence) refuseAndRelease(format string, args ...any) {
	log.Printf("ERROR: "+format, args...)
	held := false
	if f != nil {
		f.mu.Lock()
		held = f.held
		f.mu.Unlock()
	}
	f.release(context.Background())
	if held {
		log.Printf("Refused before this run's first write: this run wrote nothing and its merge fence was released " +
			"(zone_merge_fence and its mirrors). Fix the cause above and re-run the same command.")
	} else {
		log.Printf("Refused before this run's first write: this run wrote nothing. Fix the cause above and re-run the same command.")
	}
	os.Exit(1)
}

// routeFenceKeptAbortMessage 是 db 源「写到一半中止、围栏故意留着」的中止文案(fenceKeptAbortMessage 的库围栏版。那一份的
// 「GET / DEL 镜像再重跑」在 db 源下是错的:库里的围栏才是真源,删镜像只会让 scene_manager / guild 提前放行,重跑照样被库拒绝)。
// lost 非 nil = 本次运行已失去围栏(续期确定失权 / 本地截止时刻已过 / 镜像里出现更大的 fence_epoch / 批事务内的 fencing token
// 复核已判失权,见 routeFenceAbortLost):围栏可能已归别人,文案改成「先查清是谁接手」,不给接手指引。
// manifestPath 是本次实际用的清单路径(合服没给 -manifest-path 时是带时间戳的缺省路径):续跑必须指回同一份清单,文案原样打出来 ——
// 落到新路径的续跑会被当成首跑(接手事务按 holder_json 的 manifest_path 也会拒绝它)。
func routeFenceKeptAbortMessage(cause string, op fencedOp, src, dst uint32, runID, manifestPath string, lost error) string {
	if lost != nil {
		return fmt.Sprintf("%s。本次运行已失去合服围栏(run_id=%s):%v。已停止一切写入(未完成的步骤没有标记完成)。"+
			"zone %d / %d 的围栏此刻可能已归另一次运行:先查 zone_merge_fence(shard=63)的 run_id 与 holder_json 是谁、"+
			"那个进程是否还在跑,再决定怎么续跑;不要对一个还在运行的 run_id 用 -take-over-fence",
			cause, runID, lost, src, dst)
	}
	why, _, rerun := fencedOpText(op)
	return fmt.Sprintf("%s。合服围栏(全局库 zone_merge_fence 中 zone %d 与 %d 的全部分片,镜像 %s / %s)故意保留(LEFT IN PLACE),"+
		"仍归 run_id=%s:%s。续跑步骤:"+
		"(1) 按上面的原因排除故障(锁冲突:查 SHOW ENGINE INNODB STATUS 与 information_schema.innodb_trx 里的长事务;"+
		"Redis 失败:先恢复 mapping Redis 的连通);"+
		"(2) 确认没有存活的 merge_zone 进程;"+
		"(3) 立即用原命令(-manifest-path %s)加 -take-over-fence %s 重跑(%s)—— 工具在一个库事务里把围栏从 %s 接到新的 run_id 上,两区一刻不解封;"+
		"旧进程若其实还活着,它的下一批映射写入会被库拒绝,但它不碰映射的写(拷行、blob、帮会、交易、榜单、热状态、公告、清单文件)"+
		"要到它下一次续期失败(最长约 %s)才停,与新进程交错 —— 所以 (2) 必须先确认。"+
		"不加 -take-over-fence 直接重跑会被拒(\"merge fence is held by another run\"),直到库时钟过了 expires_at_ms;"+
		"不要手工 DEL 镜像:库里的围栏才是真源,删镜像只会让 scene_manager / guild 提前放行",
		cause, src, dst, mergeFenceKey(src), mergeFenceKey(dst), runID, why, manifestPath, runID, rerun, runID, routeFenceRenewMaxInterval)
}

// routeFenceAbortLost 选出 db 源中止文案的 lost 参数(纯函数):本地判据 check(续期确定失权 / 本地截止时刻 / 镜像更大 epoch)
// 非 nil 就用它;本地仍判持有时,取本步骤带回的错误里第一个 errRouteFenceLost —— 批事务内的 fencing token 复核(requireFenceHeld)
// 比续期更早发现「围栏已归别人」(被接手而本地下一次续期还没到),这时文案不能再说「仍归本次、对自己 -take-over-fence」。
func routeFenceAbortLost(check error, errs ...error) error {
	if check != nil {
		return check
	}
	for _, err := range errs {
		if err != nil && errors.Is(err, errRouteFenceLost) {
			return err
		}
	}
	return nil
}

// checkTakeOverFenceFlag 是 -take-over-fence 的前置校验(纯函数):只用于 -route-source=db 的 -mode merge / unmerge
// (持围栏的两种运行);值必须是合法的 run_id。
func checkTakeOverFenceFlag(o options) error {
	if o.takeOverFenceRunID == "" {
		return nil
	}
	if o.backfill || (o.mode != "merge" && o.mode != "unmerge") {
		return errors.New("-take-over-fence only applies to -mode merge and -mode unmerge (the runs that hold a merge fence)")
	}
	if o.routeSource != routeSourceDB {
		return errors.New("-take-over-fence only applies to -route-source=db; with -route-source=redis follow the abort " +
			"message (GET / DEL merge:in_progress:*)")
	}
	if !validRunID(o.takeOverFenceRunID) {
		return fmt.Errorf("-take-over-fence %q is not a run_id (1..64 characters of [A-Za-z0-9_-])", o.takeOverFenceRunID)
	}
	return nil
}

// ── guild_rank:maintenance_lock ───────────────────────────────

const (
	// 与 go/guild/internal/data/guild_repo.go 的常量保持一致。
	guildRankLockKey = "guild_rank:maintenance_lock"
	guildRankLockTTL = 5 * time.Minute
	// 合服的 ZSET 合并是毫秒级的一条 TxPipeline,拿不到锁多等一会儿就好;
	// guild 服自己的 acquireRankLock 用 5s,这里放宽到 30s(维护窗口不缺这点)。
	guildRankLockWait = 30 * time.Second
)

// releaseRankLockScript 逐字抄 guild_repo.go —— 释放必须校验 token,
// 否则会误删 guild 服自己拿到的那把锁。
var releaseRankLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

// acquireGuildRankLock 与 guild 服互斥。返回释放函数。
func acquireGuildRankLock(ctx context.Context, rdb *redis.Client, dryRun bool) (func(), error) {
	if dryRun {
		held, err := rdb.Exists(ctx, guildRankLockKey).Result()
		if err != nil {
			return nil, fmt.Errorf("exists %s: %w", guildRankLockKey, err)
		}
		if held > 0 {
			log.Printf("WARN: %s is currently held by the guild service; a real -apply run would wait up to %s",
				guildRankLockKey, guildRankLockWait)
		}
		return func() {}, nil
	}
	token := randomToken()
	deadline := time.Now().Add(guildRankLockWait)
	for {
		ok, err := rdb.SetNX(ctx, guildRankLockKey, token, guildRankLockTTL).Result()
		if err != nil {
			return nil, fmt.Errorf("acquire %s: %w", guildRankLockKey, err)
		}
		if ok {
			log.Printf("acquired %s (ttl=%s)", guildRankLockKey, guildRankLockTTL)
			return func() {
				if _, err := releaseRankLockScript.Run(context.Background(), rdb,
					[]string{guildRankLockKey}, token).Int(); err != nil {
					log.Printf("WARN: release %s: %v", guildRankLockKey, err)
				}
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for %s — the guild service is running a rank "+
				"rebuild/backfill; wait for it to finish", guildRankLockWait, guildRankLockKey)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ── 小工具 ────────────────────────────────────────────────────

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实践中等于系统坏了;退化成时间戳仍然唯一到毫秒 + pid。
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func operatorTag() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/pid=%d", host, os.Getpid())
}

// newRunID 生成本次运行的身份。清单与围栏共用它。
func newRunID(src, dst uint32, now time.Time) string {
	return fmt.Sprintf("%d-%d-%s-%s", src, dst, now.UTC().Format("20060102T150405Z"), randomToken()[:8])
}

// newToolRunID 生成不持围栏的写模式(-backfill-home-zone / -mode pin-placement)的运行标识,只进写者标识 updated_by
// (newMergeRouteWriter),排障时对得上是哪一次运行写的。
func newToolRunID(tag string, zone uint32, now time.Time) string {
	return fmt.Sprintf("%s-%d-%s-%s", tag, zone, now.UTC().Format("20060102T150405Z"), randomToken()[:8])
}
