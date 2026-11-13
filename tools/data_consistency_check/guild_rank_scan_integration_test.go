//go:build integration

package main

// 真 Redis 用例:帮会榜检查的扫描接线(newRankRedis + scanEachMaster)在真单实例、真 Redis Cluster 上都把每个主节点扫全;
// Cluster 那一轮另验误配:同一个种子节点不写 -redis-cluster 时,扫描前就拒绝(errStandaloneOnClusterNode)。
//
// 运行(Codex):
//
//	# 单实例(本机 compose 的 redis,或任意一次性实例)
//	$env:DATA_CONSISTENCY_IT_RANK_REDIS_ADDRS = "127.0.0.1:6379"
//	# Cluster(deploy/compose.redis-cluster.yml:3 主 3 从,宣告 127.0.0.1:7100-7105)
//	$env:DATA_CONSISTENCY_IT_RANK_REDIS_ADDRS = "127.0.0.1:7100"
//	$env:DATA_CONSISTENCY_IT_RANK_REDIS_CLUSTER = "1"
//	$env:DATA_CONSISTENCY_IT_REQUIRE_RANK_REDIS = "1"   # 缺地址 / 连不上判 FAIL 而不是 SKIP
//	go test -tags integration -run TestRankKeyScanIT -count=1 -v .
//
// 口令(如有)放环境变量 DATA_CONSISTENCY_IT_RANK_REDIS_PASSWORD,不写进任何文件。用例只写、只删自己前缀下的键
// (dcc_it:<随机数>:<序号>,10 分钟 TTL 兜底),不碰 guild_rank:* 与别人的数据。

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRankKeyScanITFindsKeysOnEveryMaster(t *testing.T) {
	addrs := strings.TrimSpace(os.Getenv("DATA_CONSISTENCY_IT_RANK_REDIS_ADDRS"))
	required := os.Getenv("DATA_CONSISTENCY_IT_REQUIRE_RANK_REDIS") == "1"
	if addrs == "" {
		if required {
			t.Fatal("DATA_CONSISTENCY_IT_REQUIRE_RANK_REDIS=1 but DATA_CONSISTENCY_IT_RANK_REDIS_ADDRS is empty")
		}
		t.Skip("set DATA_CONSISTENCY_IT_RANK_REDIS_ADDRS (and DATA_CONSISTENCY_IT_RANK_REDIS_CLUSTER=1 for a Redis Cluster)")
	}
	cluster := os.Getenv("DATA_CONSISTENCY_IT_RANK_REDIS_CLUSTER") == "1"
	c, err := newRankRedis(addrs, cluster, os.Getenv("DATA_CONSISTENCY_IT_RANK_REDIS_PASSWORD"), 0)
	if err != nil {
		t.Fatalf("newRankRedis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping %s: %v", addrs, err)
	}

	prefix := fmt.Sprintf("dcc_it:%d:", rand.Uint64())
	const n = 64
	want := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		key := prefix + strconv.Itoa(i)
		if err := c.Set(ctx, key, "1", 10*time.Minute).Err(); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
		want[key] = struct{}{}
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		for key := range want {
			_ = c.Del(cctx, key).Err() // 逐键删:键分在不同 slot,多键 DEL 会被控制集群守卫拒绝
		}
	})

	if cc, ok := c.(*redis.ClusterClient); ok {
		// 前提:键确实散在不止一个主节点上,否则这条用例证明不了「逐主节点」(只扫一个节点就会漏键)。
		masters := map[string]struct{}{}
		for key := range want {
			node, err := cc.MasterForKey(ctx, key)
			if err != nil {
				t.Fatalf("master for %s: %v", key, err)
			}
			masters[node.Options().Addr] = struct{}{}
		}
		if len(masters) < 2 {
			t.Fatalf("前提不成立:%d 个键只落在 %d 个主节点上(DATA_CONSISTENCY_IT_RANK_REDIS_CLUSTER=1 却不是多主集群?)", n, len(masters))
		}
		t.Logf("%d keys spread over %d masters", n, len(masters))

		// 误配:同一个种子节点、却没写 -redis-cluster。单实例客户端只扫得到这一个主节点,必须在扫描前拒绝。
		seed := strings.TrimSpace(strings.Split(addrs, ",")[0])
		single, err := newRankRedis(seed, false, os.Getenv("DATA_CONSISTENCY_IT_RANK_REDIS_PASSWORD"), 0)
		if err != nil {
			t.Fatalf("newRankRedis(single %s): %v", seed, err)
		}
		t.Cleanup(func() { _ = single.Close() })
		if err := scanEachMaster(single)(ctx, prefix+"*", func([]string) error { return nil }); !errors.Is(err, errStandaloneOnClusterNode) {
			t.Fatalf("没写 -redis-cluster 却连到 Cluster 节点必须拒绝扫描, got %v", err)
		}
	} else if cluster {
		t.Fatalf("-redis-cluster 应得到 *redis.ClusterClient, got %T", c)
	}

	got := map[string]struct{}{}
	if err := scanEachMaster(c)(ctx, prefix+"*", func(keys []string) error {
		for _, k := range keys {
			got[k] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("逐主节点扫描应找到全部 %d 个键, got %d", len(want), len(got))
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Fatalf("扫描漏掉了 %s", key)
		}
	}
}
