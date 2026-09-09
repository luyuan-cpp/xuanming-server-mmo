package main

// 并发围栏(fence):合服跑的这段时间里,别人不许往被合的两个 zone 里塞新对象。
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
//  2. guild_rank:maintenance_lock —— **互斥锁**,写在 guild 服自己的 Redis(DB 2)。
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
	ttl := runTimeout + 30*time.Minute
	if ttl < time.Hour {
		ttl = time.Hour
	}
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
