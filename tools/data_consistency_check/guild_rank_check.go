package main

// 帮会榜孤儿检查(guild_rank:zone:* 指向不在 knownZones 里的区)与它用的 Redis 句柄。
//
// guild 的 Redis 正在上 Cluster(docs/design/redis-cluster-all-domains-20261110.md §4.8,归控制集群)。Cluster 下每个主节点
// 只存一部分键,只对一个节点 SCAN 会漏掉其余主节点上的榜键,检查却照样报「干净」。所以扫描一律走
// shared/clusterredis.ScanEachMaster:单实例就是一个节点,Cluster 逐个主节点扫完整个库;任何节点出错,或扫描前后
// 拓扑 / 节点身份不一致,整体报错,不给半截结果。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"shared/clusterredis"
	"shared/redisslot"
)

const (
	// zoneRankKeyPrefix 与 go/guild internal/data/guild_repo.go 的 zoneRankKeyPrefix 是同一个前缀,键形以那边为准:
	// guild_rank:zone:{gr:<最终区>}(2026-11-12 全部 Redis 上 Cluster 第 5a 批改形,见 zoneRankKey)。扫描仍按整个前缀匹配,
	// 改形之前留下的旧键 guild_rank:zone:<区> 与手写的键都会扫到,报成「形状不认识」,不静默跳过。
	zoneRankKeyPrefix  = "guild_rank:zone:"
	zoneRankKeyPattern = zoneRankKeyPrefix + "*"
	// rankKeyScanCount 是每条 SCAN 的 COUNT 提示,与改动前相同。
	rankKeyScanCount = 200
	// rankKeySampleLimit 是报告里列出的样例键个数上限。
	rankKeySampleLimit = 5
)

// keyScanner 把 match 命中的键分批交给 fn,扫完整个库才返回 nil。返回错误时,已经交给 fn 的批次不完整,
// 调用方不得据此下结论。生产实现是 scanEachMaster;测试注入假实现,不连 Redis。
type keyScanner func(ctx context.Context, match string, fn func(keys []string) error) error

// newRankRedis 构造帮会榜所在 Redis 的客户端,用 guild 上 Cluster 之后同一个构造器(shared/clusterredis,控制集群键策略)。
// cluster=false 时 addrs 是一个单实例地址;cluster=true 时 addrs 是逗号分隔的集群种子节点,只有 DB 0。
// 参数不合法返回错误,错误文本不含口令。
func newRankRedis(addrs string, cluster bool, password string, db int) (redis.UniversalClient, error) {
	cfg := clusterredis.Config{Addrs: splitAddrs(addrs), Cluster: cluster, Password: password, DB: db}
	return clusterredis.New(cfg, clusterredis.ControlClusterKeys)
}

// splitAddrs 按逗号切地址并去掉两端空白。空段原样留下,交给 clusterredis.Config.Validate 报错,不静默丢掉。
func splitAddrs(csv string) []string {
	parts := strings.Split(csv, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// scanEachMaster 是 keyScanner 的生产实现,语义见 clusterredis.ScanEachMaster(fail-closed,单实例核对扫描前后的 run_id,
// Cluster 核对扫描前后的拓扑指纹)。单实例客户端先确认连的不是 Cluster 节点,见 ensureNotClusterNode。
func scanEachMaster(c redis.UniversalClient) keyScanner {
	return func(ctx context.Context, match string, fn func(keys []string) error) error {
		if err := ensureNotClusterNode(ctx, c); err != nil {
			return err
		}
		return clusterredis.ScanEachMaster(ctx, c, match, rankKeyScanCount, fn)
	}
}

// errStandaloneOnClusterNode:没写 -redis-cluster,-redis-addr 却指向一个 Cluster 节点。单实例客户端对它 SCAN 只扫得到
// 这一个主节点上的键,其余主节点上的孤儿全漏掉,检查照样报干净 —— 所以整条检查判没扫完。
var errStandaloneOnClusterNode = errors.New("the Redis at -redis-addr is a Redis Cluster node: rerun with -redis-cluster " +
	"(a single-node SCAN misses the keys on the other masters)")

// ensureNotClusterNode 只对单实例客户端生效:读 INFO cluster,cluster_enabled:1 就返回 errStandaloneOnClusterNode;
// 读不到、或输出里没有 cluster_enabled(Redis 3.0 起都有,没有就说明对端不是我们认识的 Redis)同样返回错误(fail-closed)。
// Cluster 客户端本来就逐主节点扫,直接放行。
func ensureNotClusterNode(ctx context.Context, c redis.UniversalClient) error {
	node, ok := c.(*redis.Client)
	if !ok {
		return nil
	}
	info, err := node.Info(ctx, "cluster").Result()
	if err != nil {
		return fmt.Errorf("read INFO cluster from -redis-addr: %w", err)
	}
	enabled, found := clusterEnabled(info)
	if !found {
		return errors.New("INFO cluster from -redis-addr has no cluster_enabled field; cannot tell whether it is a Redis Cluster node")
	}
	if enabled {
		return errStandaloneOnClusterNode
	}
	return nil
}

// clusterEnabled 从 INFO cluster 的输出里取 cluster_enabled:返回 (是否为 1, 是否有这一行)。
func clusterEnabled(info string) (enabled, found bool) {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "cluster_enabled:"); ok {
			return strings.TrimSpace(v) == "1", true
		}
	}
	return false, false
}

// zoneRankKey 与 go/guild 的 zoneRankKey 同一个构造:tag 经 redisslot.ZoneControlTag 生成,两边不各自手拼。
func zoneRankKey(zone uint32) string {
	return zoneRankKeyPrefix + redisslot.ZoneControlTag(redisslot.ControlFamilyGuildRank, zone)
}

// parseZoneRankKey 把分区榜键解析回区号。只认 guild 服务自己会写出的那一个键:取出 {gr:<数字>} 里的区号之后,
// 用 zoneRankKey 重新拼一遍,与原键逐字相同才算(前导零、正负号、别的族、多余的段一律不认)。
func parseZoneRankKey(key string) (uint32, bool) {
	tag, ok := strings.CutPrefix(key, zoneRankKeyPrefix)
	if !ok {
		return 0, false
	}
	digits, ok := strings.CutPrefix(tag, "{"+redisslot.ControlFamilyGuildRank.Prefix()+":")
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, "}")
	if !ok {
		return 0, false
	}
	zone, err := strconv.ParseUint(digits, 10, 32)
	if err != nil || zoneRankKey(uint32(zone)) != key {
		return 0, false
	}
	return uint32(zone), true
}

// checkGuildRankZoneOrphans — guild_rank:zone:{id} ZSETs that point at zones that are not in knownZones.
//
// 合服是别名合服,不改写玩家与路由行:已合走的源区仍在 player_route.home_zone 的取值里(显式 -live-zones 时要自己列上),
// 所以源区的榜键不会在这里报孤儿;guild 服务会把源区的榜周期折叠进最终区并删掉旧键(merge-zone-runbook.md §4,
// 默认一个折叠周期 10 秒)。这里报的是 knownZones 之外的区留下的榜键,即从未存在过的区,或折叠 / 重建出错后残留的键。
//
// 要找的正是不在区清单里的键,所以只能枚举整个库;不能像 guild 服务的重建那样改成按 SQL 区清单逐个处理(设计 §4.8)。
//
// 结论口径:
//   - 扫描没走完(任一节点出错、超时、扫描期间拓扑或节点身份变化,或没写 -redis-cluster 却连到 Cluster 节点)
//     → fail「NOT CHECKED」,计数清零:半截计数既不是孤儿数也不是总数。2026-11-12 之前这里报 warn 并带着半截计数,
//     进程照样退出 0。
//   - 命中前缀、却不是 guild_rank:zone:{gr:<区号>} 的键,不算孤儿也不静默跳过:计数并给样例,至少报 warn
//     (改形之前留下的旧键、手工写进来的键,或者榜键又改了形状而本工具没跟上)。2026-11-12 之前这类键计入总数后被跳过。
//   - SCAN 可能把同一个键返回不止一次,按键去重。
func checkGuildRankZoneOrphans(ctx context.Context, cfg runConfig) CheckResult {
	r := CheckResult{Name: "guild_rank:zone:*"}
	if cfg.scanRankKeys == nil {
		r.Severity = "warn"
		r.SampleNotes = "NOT CHECKED: no guild rank Redis handle"
		return r
	}
	seen := make(map[string]struct{})
	orphans := make(map[uint32]struct{})
	var unrecognized []string
	err := cfg.scanRankKeys(ctx, zoneRankKeyPattern, func(keys []string) error {
		for _, k := range keys {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			zone, ok := parseZoneRankKey(k)
			if !ok {
				unrecognized = append(unrecognized, k)
				continue
			}
			if _, live := cfg.knownZones[zone]; !live {
				orphans[zone] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		r.Severity = "fail"
		r.SampleNotes = fmt.Sprintf("NOT CHECKED: scan of %s did not complete, counts discarded: %v", zoneRankKeyPattern, err)
		return r
	}

	r.TotalCount = int64(len(seen))
	r.BadCount = int64(len(orphans))
	var notes []string
	switch {
	case len(orphans) > 0:
		r.Severity = "block"
		notes = append(notes, fmt.Sprintf("orphan rank ZSETs for zones %v outside the live zone set (player_route home zones, "+
			"or -live-zones): zones that never existed, or keys left behind by a failed fold / rebuild", sortedKeys(orphans)))
	case len(unrecognized) > 0:
		r.Severity = "warn"
	default:
		r.Severity = "info"
		notes = append(notes, "no orphan rank ZSETs")
	}
	if len(unrecognized) > 0 {
		notes = append(notes, unrecognizedRankKeysNote(unrecognized, len(seen)))
	}
	r.SampleNotes = strings.Join(notes, "; ")
	return r
}

// unrecognizedRankKeysNote 写「形状不认识」的键数,以及按字典序排在最前的几个样例(加引号,键名里的怪字符原样可见)。
func unrecognizedRankKeysNote(keys []string, total int) string {
	sort.Strings(keys)
	samples := keys
	if len(samples) > rankKeySampleLimit {
		samples = samples[:rankKeySampleLimit]
	}
	quoted := make([]string, len(samples))
	for i, k := range samples {
		quoted[i] = strconv.Quote(k)
	}
	shape := zoneRankKeyPrefix + "{" + redisslot.ControlFamilyGuildRank.Prefix() + ":<zone>}"
	return fmt.Sprintf("%d of %d keys matching %s are not in the %s shape and were not checked "+
		"(left over from before the hash-tag reshape, hand-written, or the rank key shape changed without updating this tool), e.g. [%s]",
		len(keys), total, zoneRankKeyPattern, shape, strings.Join(quoted, ", "))
}
