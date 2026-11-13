package main

// checkGuildRankZoneOrphans 与帮会榜 Redis 句柄的单测。扫描经 keyScanner 假实现分批喂键,不连 Redis;
// 逐主节点扫描本身(ScanEachMaster 的拓扑指纹与 run_id 核对)在 shared/clusterredis 里测,
// 真单实例 / 真 Cluster 上的接线在 guild_rank_scan_integration_test.go(-tags integration)。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"shared/clusterredis"
)

// fakeRankScanner 按顺序把 batches 交给 fn(每批相当于某个主节点上的一条 SCAN),交完再返回 err。
func fakeRankScanner(err error, batches ...[]string) keyScanner {
	return func(_ context.Context, match string, fn func(keys []string) error) error {
		if match != zoneRankKeyPattern {
			return fmt.Errorf("unexpected match pattern %q", match)
		}
		for _, b := range batches {
			if ferr := fn(b); ferr != nil {
				return ferr
			}
		}
		return err
	}
}

// rankKeys 把区号换成 guild 服务会写出的分区榜键。
func rankKeys(zones ...uint32) []string {
	keys := make([]string, len(zones))
	for i, z := range zones {
		keys[i] = zoneRankKey(z)
	}
	return keys
}

func rankConfig(scan keyScanner, live ...uint32) runConfig {
	known := map[uint32]struct{}{}
	for _, z := range live {
		known[z] = struct{}{}
	}
	return runConfig{scanRankKeys: scan, knownZones: known}
}

func assertRankResult(t *testing.T, r CheckResult, severity string, bad, total int64) {
	t.Helper()
	if r.Severity != severity || r.BadCount != bad || r.TotalCount != total {
		t.Fatalf("帮会榜检查报告错误: 得到 %+v; 期望 severity=%s bad=%d total=%d", r, severity, bad, total)
	}
}

// 键形金样:与 go/guild internal/data/guild_repo.go 的 zoneRankKey 逐字一致(两边都经 redisslot.ZoneControlTag 构造)。
func TestZoneRankKeyShape(t *testing.T) {
	if got := zoneRankKey(12); got != "guild_rank:zone:{gr:12}" {
		t.Fatalf("zoneRankKey(12) = %q, 期望 guild_rank:zone:{gr:12}", got)
	}
}

// Cluster 下榜键散在各个主节点上:两个节点各有一个孤儿,都要数到(只扫一个节点会漏掉另一个)。
func TestGuildRankOrphans_CountsKeysFromEveryMaster(t *testing.T) {
	scan := fakeRankScanner(nil,
		rankKeys(1, 7), // 主节点 A
		rankKeys(2),    // 主节点 A 的下一批
		rankKeys(9, 3), // 主节点 B
	)
	r := checkGuildRankZoneOrphans(t.Context(), rankConfig(scan, 1, 2, 3))
	assertRankResult(t, r, "block", 2, 5)
	if !strings.Contains(r.SampleNotes, "zones [7 9]") {
		t.Fatalf("孤儿区要按区号排好列出: %q", r.SampleNotes)
	}
}

// SCAN 中途失败(含拓扑变化、超时、误配到 Cluster 节点):已数到的孤儿与总数都作废,报 fail「NOT CHECKED」,进程退出 1。
// 2026-11-12 之前这里报 warn 并带着半截计数。
func TestGuildRankOrphans_ScanFailureIsNotChecked(t *testing.T) {
	for name, scanErr := range map[string]error{
		"node error mid-scan":     errors.New("read tcp 10.0.0.7:7001: i/o timeout"),
		"topology changed":        clusterredis.ErrTopologyChanged,
		"deadline":                context.DeadlineExceeded,
		"standalone on a cluster": errStandaloneOnClusterNode,
	} {
		t.Run(name, func(t *testing.T) {
			// 失败之前已经交来一批,里面有一个孤儿(区 99):不能当成结论报出去。
			scan := fakeRankScanner(scanErr, rankKeys(1, 99))
			r := checkGuildRankZoneOrphans(t.Context(), rankConfig(scan, 1))
			assertRankResult(t, r, "fail", 0, 0)
			if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: ") || !strings.Contains(r.SampleNotes, scanErr.Error()) {
				t.Fatalf("没扫完必须写明 NOT CHECKED 与原因: %q", r.SampleNotes)
			}
			if strings.Contains(r.SampleNotes, "orphan") {
				t.Fatalf("没扫完不得报孤儿: %q", r.SampleNotes)
			}
			if failing := printReport([]CheckResult{r}, "route source: test"); failing != 1 {
				t.Fatalf("没扫完的检查必须让进程退出 1, printReport 计数 %d", failing)
			}
		})
	}
}

func TestGuildRankOrphans_CleanScanReportsInfo(t *testing.T) {
	scan := fakeRankScanner(nil, rankKeys(1), nil, rankKeys(2))
	r := checkGuildRankZoneOrphans(t.Context(), rankConfig(scan, 1, 2))
	assertRankResult(t, r, "info", 0, 2)
	if r.SampleNotes != "no orphan rank ZSETs" {
		t.Fatalf("干净结果的说明: %q", r.SampleNotes)
	}
}

// 一个榜都没有也是干净的(与「没扫」区分:没扫是 fail / warn「NOT CHECKED」)。
func TestGuildRankOrphans_EmptyScanReportsInfo(t *testing.T) {
	r := checkGuildRankZoneOrphans(t.Context(), rankConfig(fakeRankScanner(nil), 1))
	assertRankResult(t, r, "info", 0, 0)
}

// SCAN 允许把同一个键返回不止一次:总数与孤儿数都按键去重。
func TestGuildRankOrphans_DuplicateKeysCountOnce(t *testing.T) {
	scan := fakeRankScanner(nil, rankKeys(7, 1), rankKeys(7, 1))
	r := checkGuildRankZoneOrphans(t.Context(), rankConfig(scan, 1))
	assertRankResult(t, r, "block", 1, 2)
	if !strings.Contains(r.SampleNotes, "zones [7]") {
		t.Fatalf("重复返回的孤儿只列一次: %q", r.SampleNotes)
	}
}

// 命中前缀却不是 guild_rank:zone:{gr:<区号>} 的键:不算孤儿,也不静默跳过(2026-11-12 之前计入总数后跳过,
// 榜键一改形状检查就变成空转却报干净)。没有孤儿时报 warn;有孤儿时仍报 block,说明里带上没认出的键。
func TestGuildRankOrphans_UnrecognizedKeysAreReportedNotSkipped(t *testing.T) {
	keys := append(rankKeys(1),
		"guild_rank:zone:2",          // 改形之前的旧键形,guild 已不读不写
		"guild_rank:zone:{gr:01}",    // 前导零
		"guild_rank:zone:{gr:3}:old", // 多一段
		"guild_rank:zone:{ctl:3}",    // 别的族的 tag
		"guild_rank:zone:",           // 没有区号
	)

	r := checkGuildRankZoneOrphans(t.Context(), rankConfig(fakeRankScanner(nil, keys), 1, 2, 3))
	assertRankResult(t, r, "warn", 0, 6)
	for _, want := range []string{"5 of 6 keys", "guild_rank:zone:{gr:<zone>} shape",
		`"guild_rank:zone:2"`, `"guild_rank:zone:{gr:01}"`, `"guild_rank:zone:{gr:3}:old"`, `"guild_rank:zone:{ctl:3}"`, `"guild_rank:zone:"`} {
		if !strings.Contains(r.SampleNotes, want) {
			t.Fatalf("没认出的键必须计数并给样例(缺 %s): %q", want, r.SampleNotes)
		}
	}

	withOrphan := append(append([]string(nil), keys...), zoneRankKey(9))
	r = checkGuildRankZoneOrphans(t.Context(), rankConfig(fakeRankScanner(nil, withOrphan), 1, 2, 3))
	assertRankResult(t, r, "block", 1, 7)
	if !strings.Contains(r.SampleNotes, "zones [9]") || !strings.Contains(r.SampleNotes, "5 of 7 keys") {
		t.Fatalf("孤儿与没认出的键都要写进说明: %q", r.SampleNotes)
	}
}

// 样例按字典序取前 rankKeySampleLimit 个,数量照实写。
func TestUnrecognizedRankKeysNote_ListsSortedSamplesUpToTheLimit(t *testing.T) {
	keys := []string{"guild_rank:zone:g", "guild_rank:zone:a", "guild_rank:zone:f", "guild_rank:zone:c",
		"guild_rank:zone:e", "guild_rank:zone:b", "guild_rank:zone:d"}
	note := unrecognizedRankKeysNote(keys, 9)
	want := `7 of 9 keys matching guild_rank:zone:* are not in the guild_rank:zone:{gr:<zone>} shape and were not checked ` +
		`(left over from before the hash-tag reshape, hand-written, or the rank key shape changed without updating this tool), ` +
		`e.g. ["guild_rank:zone:a", "guild_rank:zone:b", "guild_rank:zone:c", "guild_rank:zone:d", "guild_rank:zone:e"]`
	if note != want {
		t.Fatalf("说明文字:\n got %q\nwant %q", note, want)
	}
}

func TestGuildRankOrphans_NoHandleIsNotChecked(t *testing.T) {
	r := checkGuildRankZoneOrphans(t.Context(), runConfig{})
	assertRankResult(t, r, "warn", 0, 0)
	if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED") {
		t.Fatalf("缺句柄不能伪装成已检查: %q", r.SampleNotes)
	}
}

// 只认 guild 服务自己会写出的键:解析出区号后重新拼一遍,逐字相同才算。
func TestParseZoneRankKey(t *testing.T) {
	ok := map[string]uint32{
		"guild_rank:zone:{gr:0}":          0,
		"guild_rank:zone:{gr:7}":          7,
		"guild_rank:zone:{gr:4294967295}": 4294967295,
	}
	for key, want := range ok {
		if got, parsed := parseZoneRankKey(key); !parsed || got != want {
			t.Fatalf("parseZoneRankKey(%q) = %d, %v; 期望 %d, true", key, got, parsed, want)
		}
	}
	for _, key := range []string{
		"guild_rank:zone:{gr:4294967296}", // 越过 uint32
		"guild_rank:zone:{gr:07}",
		"guild_rank:zone:{gr:+7}",
		"guild_rank:zone:{gr: 7}",
		"guild_rank:zone:{gr:}",
		"guild_rank:zone:{gr:7}:x",
		"guild_rank:zone:x{gr:7}",
		"guild_rank:zone:{ctl:7}",
		"guild_rank:zone:7", // 改形之前的旧键形
		"guild_rank:zone:",
		"guild_rank:version:{gr:7}",
		"guild_rank:tombstone:{gr:7}",
		"guild_rank:maintenance_lock:{gr:7}",
	} {
		if got, parsed := parseZoneRankKey(key); parsed {
			t.Fatalf("parseZoneRankKey(%q) 不该认成分区榜键, got %d", key, got)
		}
	}
}

// -redis-cluster 决定客户端形态:单实例是 *redis.Client,Cluster 是 *redis.ClusterClient。
// clusterredis.ScanEachMaster 按这两个类型分支:后者逐个主节点扫完整个库。构造不连网络。
func TestNewRankRedis_ClientShapeFollowsTheClusterFlag(t *testing.T) {
	single, err := newRankRedis("127.0.0.1:6379", false, "", 3)
	if err != nil {
		t.Fatalf("单实例: %v", err)
	}
	t.Cleanup(func() { _ = single.Close() })
	if _, ok := single.(*redis.Client); !ok {
		t.Fatalf("单实例应是 *redis.Client, got %T", single)
	}

	cluster, err := newRankRedis("127.0.0.1:7100, 127.0.0.1:7101", true, "", 0)
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Close() })
	if _, ok := cluster.(*redis.ClusterClient); !ok {
		t.Fatalf("-redis-cluster 应是 *redis.ClusterClient(逐主节点扫描), got %T", cluster)
	}
}

// 没写 -redis-cluster 却连到 Cluster 节点时,扫描前读 INFO cluster 认出来(真 Redis 上的接线在集成用例里)。
func TestClusterEnabled(t *testing.T) {
	for name, tc := range map[string]struct {
		info           string
		enabled, found bool
	}{
		"standalone":    {"# Cluster\r\ncluster_enabled:0\r\n", false, true},
		"cluster node":  {"# Cluster\r\ncluster_enabled:1\r\n", true, true},
		"missing field": {"# Cluster\r\n", false, false},
		"empty":         {"", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			enabled, found := clusterEnabled(tc.info)
			if enabled != tc.enabled || found != tc.found {
				t.Fatalf("clusterEnabled(%q) = (%v, %v); 期望 (%v, %v)", tc.info, enabled, found, tc.enabled, tc.found)
			}
		})
	}
}

func TestNewRankRedis_RejectsInconsistentArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		addrs   string
		cluster bool
		db      int
	}{
		"two addresses without -redis-cluster": {"127.0.0.1:7100,127.0.0.1:7101", false, 0},
		"cluster with a non-zero DB":           {"127.0.0.1:7100", true, 2},
		"empty address":                        {"", false, 0},
		"empty seed in the list":               {"127.0.0.1:7100,,127.0.0.1:7101", true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := newRankRedis(tc.addrs, tc.cluster, "secret-not-in-error", tc.db)
			if err == nil {
				_ = c.Close()
				t.Fatal("参数自相矛盾必须拒绝,不能悄悄只连一个节点")
			}
			if strings.Contains(err.Error(), "secret-not-in-error") {
				t.Fatalf("错误文本不得带口令: %v", err)
			}
		})
	}
}
