package assetopwake

// 真 Redis 上的分片唤醒(docs/design/redis-cluster-all-domains-20261110.md §4.6、§9 第 2、3 项)。没设环境变量时 Skip;
// Codex 按 docs/PROGRESS.md 2026-11-12「全部 Redis 上 Cluster 第 3 批」给的命令,对本机两套集群(7.2、8.x)与单机各跑一轮:
//
//	ASSETOPWAKE_TEST_REDIS_CLUSTER  集群种子,逗号分隔(本机 7.2:127.0.0.1:7000;8.x:127.0.0.1:7100)
//	ASSETOPWAKE_TEST_REDIS_ADDR     单机地址(本机 127.0.0.1:6379);两个都设时按集群跑
//
// 发布方照 scene 的写法(player_landing.cpp:SPUBLISH <频道> <player_id 十进制>)直接发,不经 Go 的键守卫;订阅方用与
// 生产订阅方相同的构造(clusterredis.New + ControlClusterKeys)。用例只写自己的去重键(服务名带时间戳),5 秒后自行过期。
// 迁槽用例只在集群上跑:把一个片的 slot 迁到另一个主节点再迁回来,只迁空 slot(不搬键),收尾一定迁回原主。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"shared/clusterredis"
	"shared/redisslot"
)

const (
	realRedisClusterEnv = "ASSETOPWAKE_TEST_REDIS_CLUSTER"
	realRedisAddrEnv    = "ASSETOPWAKE_TEST_REDIS_ADDR"
	// 订阅端第一次订上要等确认,迁槽之后要等 sunsubscribe、重读拓扑与第一档退避(1 秒起),30 秒足够。
	realRedisDeliveryWait = 30 * time.Second
	realRedisRepublish    = 200 * time.Millisecond
)

// realRedisWaker 记下被唤醒的玩家。唤醒在订阅端的 goroutine 里调,所以加锁。
type realRedisWaker struct {
	mu   sync.Mutex
	seen map[uint64]bool
}

func (w *realRedisWaker) WakePlayer(_ context.Context, playerID uint64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen[playerID] = true
	return 0, nil
}

func (w *realRedisWaker) woke(playerID uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen[playerID]
}

// realRedisConfig 按环境变量给出连接配置;都没设时 Skip。
func realRedisConfig(t *testing.T) clusterredis.Config {
	t.Helper()
	if seeds := strings.TrimSpace(os.Getenv(realRedisClusterEnv)); seeds != "" {
		var addrs []string
		for _, seed := range strings.Split(seeds, ",") {
			if seed = strings.TrimSpace(seed); seed != "" {
				addrs = append(addrs, seed)
			}
		}
		return clusterredis.Config{Addrs: addrs, Cluster: true}
	}
	if addr := strings.TrimSpace(os.Getenv(realRedisAddrEnv)); addr != "" {
		return clusterredis.Config{Addrs: []string{addr}}
	}
	t.Skipf("set %s (cluster seeds) or %s (standalone) to run against a real Redis", realRedisClusterEnv, realRedisAddrEnv)
	return clusterredis.Config{}
}

// realRedisSetup 起订阅端,返回发布用的客户端(不挂守卫,扮演 scene)与记录唤醒的 Waker。
func realRedisSetup(t *testing.T, ctx context.Context, cfg clusterredis.Config) (redis.UniversalClient, *realRedisWaker) {
	t.Helper()
	subscriberClient, err := clusterredis.New(cfg, clusterredis.ControlClusterKeys)
	if err != nil {
		t.Fatalf("build subscriber client: %v", err)
	}
	t.Cleanup(func() { _ = subscriberClient.Close() })
	publisher, err := clusterredis.NewPinned(cfg)
	if err != nil {
		t.Fatalf("build publisher client: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	waker := &realRedisWaker{seen: map[uint64]bool{}}
	// 名额给满 64:一轮 64 片的唤醒同时到,默认 4 个名额会丢掉大半(只是多重发几轮,但拖长用例)。
	sub, err := New(subscriberClient, Config{Service: fmt.Sprintf("realredis%d", time.Now().UnixNano()), Concurrency: ShardCount},
		waker, nil)
	if err != nil {
		t.Fatalf("build subscriber: %v", err)
	}
	sub.Start(ctx)
	t.Cleanup(sub.Stop)
	return publisher, waker
}

// realRedisPlayers 返回每片一名、互不相同的玩家:player_id = 64·base + 片号,base 取时间戳,重复跑也不撞上一轮的去重键。
func realRedisPlayers(round uint64) []uint64 {
	base := (uint64(time.Now().UnixNano())/ShardCount + round) % (1 << 40)
	players := make([]uint64, ShardCount)
	for shard := range players {
		players[shard] = (base+1)*ShardCount + uint64(shard)
	}
	return players
}

// realRedisPublishUntilWoken 按 scene 的写法逐个发布,没被唤醒的每隔一会儿重发,直到全部唤醒或超时。
// 订阅确认之前发出的消息会丢(Pub/Sub 不留存),所以靠重发等订阅就位;同一名玩家重复投递由去重键挡掉,不会重复唤醒。
func realRedisPublishUntilWoken(t *testing.T, ctx context.Context, publisher redis.UniversalClient, waker *realRedisWaker,
	players []uint64, what string) {
	t.Helper()
	deadline := time.Now().Add(realRedisDeliveryWait)
	for {
		pending := 0
		for _, playerID := range players {
			if waker.woke(playerID) {
				continue
			}
			pending++
			channel := ShardChannel(playerID % ShardCount)
			if err := publisher.SPublish(ctx, channel, strconv.FormatUint(playerID, 10)).Err(); err != nil {
				t.Logf("%s: SPUBLISH %s failed (will retry): %v", what, channel, err)
			}
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d of %d players were never woken within %v", what, pending, len(players), realRedisDeliveryWait)
		}
		time.Sleep(realRedisRepublish)
	}
}

// 每一片都投得到:集群上 64 组订阅散在各主节点,单机上一条连接订全部 64 个频道。
func TestRealRedisEveryShardIsDelivered(t *testing.T) {
	cfg := realRedisConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	publisher, waker := realRedisSetup(t, ctx, cfg)
	realRedisPublishUntilWoken(t, ctx, publisher, waker, realRedisPlayers(0), "every shard")
}

// 迁走一个片的 slot 之后照样投得到:原属主推 sunsubscribe,订阅端重读拓扑、到新属主上重订(§4.6)。
func TestRealRedisClusterResubscribesAfterSlotMove(t *testing.T) {
	cfg := realRedisConfig(t)
	if !cfg.Cluster {
		t.Skipf("slot move needs a cluster; set %s", realRedisClusterEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	publisher, waker := realRedisSetup(t, ctx, cfg)
	cluster, ok := publisher.(*redis.ClusterClient)
	if !ok {
		t.Fatalf("publisher for a cluster config is %T, want *redis.ClusterClient", publisher)
	}

	const shard = 0
	before := realRedisPlayers(1)[shard]
	realRedisPublishUntilWoken(t, ctx, publisher, waker, []uint64{before}, "before the slot move")

	realRedisMoveSlot(t, ctx, cluster, ShardChannel(shard))

	after := realRedisPlayers(2)[shard]
	realRedisPublishUntilWoken(t, ctx, publisher, waker, []uint64{after}, "after the slot move")
}

// realRedisMoveSlot 把 key 所在的空 slot 从当前主节点迁到另一个主节点(经典的 SETSLOT 流程,7.2 与 8.x 都支持),
// 并登记收尾把它迁回原主。slot 里有键、或集群只有一个主节点时 Skip。
func realRedisMoveSlot(t *testing.T, ctx context.Context, cluster *redis.ClusterClient, key string) {
	t.Helper()
	slot := int64(redisslot.HashSlot(key))
	source, err := cluster.MasterForKey(ctx, key)
	if err != nil {
		t.Fatalf("find the master of slot %d: %v", slot, err)
	}
	var mu sync.Mutex
	var masters []*redis.Client
	if err := cluster.ForEachMaster(ctx, func(_ context.Context, master *redis.Client) error {
		mu.Lock()
		defer mu.Unlock()
		masters = append(masters, master)
		return nil
	}); err != nil {
		t.Fatalf("list masters: %v", err)
	}
	var target *redis.Client
	for _, master := range masters {
		if master.Options().Addr != source.Options().Addr {
			target = master
			break
		}
	}
	if target == nil {
		t.Skipf("slot move needs at least two masters, got %d", len(masters))
	}
	keys, err := source.Do(ctx, "CLUSTER", "COUNTKEYSINSLOT", slot).Int64()
	if err != nil {
		t.Fatalf("count keys in slot %d: %v", slot, err)
	}
	if keys != 0 {
		t.Skipf("slot %d holds %d keys; this test only moves empty slots", slot, keys)
	}

	move := func(ctx context.Context, from, to *redis.Client) error {
		fromID, err := from.Do(ctx, "CLUSTER", "MYID").Text()
		if err != nil {
			return fmt.Errorf("CLUSTER MYID on %s: %w", from.Options().Addr, err)
		}
		toID, err := to.Do(ctx, "CLUSTER", "MYID").Text()
		if err != nil {
			return fmt.Errorf("CLUSTER MYID on %s: %w", to.Options().Addr, err)
		}
		steps := []struct {
			node *redis.Client
			args []interface{}
		}{
			{to, []interface{}{"CLUSTER", "SETSLOT", slot, "IMPORTING", fromID}},
			{from, []interface{}{"CLUSTER", "SETSLOT", slot, "MIGRATING", toID}},
			// 空 slot:没有键要 MIGRATE。先让目标认领,再让原主放手;原主放手时把这个 slot 的分片频道订阅者全部退订。
			{to, []interface{}{"CLUSTER", "SETSLOT", slot, "NODE", toID}},
			{from, []interface{}{"CLUSTER", "SETSLOT", slot, "NODE", toID}},
		}
		for _, step := range steps {
			if err := step.node.Do(ctx, step.args...).Err(); err != nil {
				return fmt.Errorf("%v on %s: %w", step.args, step.node.Options().Addr, err)
			}
		}
		// 其余主节点也改记新属主,不必等 gossip。失败只记日志:gossip 迟早会传过去。
		for _, master := range masters {
			addr := master.Options().Addr
			if addr == from.Options().Addr || addr == to.Options().Addr {
				continue
			}
			if err := master.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", toID).Err(); err != nil {
				t.Logf("SETSLOT %d NODE on %s: %v", slot, addr, err)
			}
		}
		cluster.ReloadState(ctx) // 发布方的路由表跟着刷新;没刷到也只是多一次 MOVED,客户端会跟过去
		return nil
	}

	if err := move(ctx, source, target); err != nil {
		t.Fatalf("move slot %d from %s to %s: %v", slot, source.Options().Addr, target.Options().Addr, err)
	}
	t.Logf("moved slot %d from %s to %s", slot, source.Options().Addr, target.Options().Addr)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := move(cleanupCtx, target, source); err != nil {
			t.Errorf("move slot %d back to %s: %v (fix by hand: CLUSTER SETSLOT %d NODE <id of %s> on every master)",
				slot, source.Options().Addr, err, slot, source.Options().Addr)
		}
	})
}
