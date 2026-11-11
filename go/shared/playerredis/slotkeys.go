package playerredis

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"shared/redisslot"
)

// slotKeysSlack 是计数与列举之间允许新增的键数:列举时比计数多要这么多;真拿满了,说明这一瞬间涨得更多,按不完整报错。
const slotKeysSlack = 64

// SlotKeys 列出 Redis Cluster 里一个 slot 的全部键,给按 slot 分片的离线全量任务用(go/player_locator 的补建 / 审计)。
//
// 在持有这个 slot 的主节点上执行 CLUSTER COUNTKEYSINSLOT 与 CLUSTER GETKEYSINSLOT。GETKEYSINSLOT 是一条命令,Redis 单线程执行,
// 拿到的是那一刻这个 slot 的全部键;它没有游标,也就没有 SCAN 那种「游标跨了进程或拓扑就漏键」的问题。要核的只是执行它的节点
// 当时确实持有这个 slot:前后各读一次那个节点的 CLUSTER NODES(myself 行)和 run_id,不持有、有进行中的迁移、或前后不一致,
// 返回 ErrTopologyChanged(并刷新客户端的拓扑缓存),调用方不得据此断言「这个 slot 里没有某键」。
//
// GETKEYSINSLOT 截断就是漏键,所以不截断:计数加 slotKeysSlack 超过 limit 时报错;列举拿满了(这一瞬间又涨了 slotKeysSlack 个以上)
// 同样报错,调用方下一轮重试。单实例客户端没有 slot,返回 ErrUnsupportedClient,调用方改用 ScanEachMaster。
func SlotKeys(ctx context.Context, c redis.UniversalClient, slot uint16, limit int) ([]string, error) {
	if slot >= redisslot.SlotCount {
		return nil, fmt.Errorf("playerredis: slot %d is out of range", slot)
	}
	if limit <= slotKeysSlack {
		return nil, fmt.Errorf("playerredis: SlotKeys limit %d must be larger than %d", limit, slotKeysSlack)
	}
	cc, ok := c.(*redis.ClusterClient)
	if !ok {
		return nil, ErrUnsupportedClient
	}
	node, err := cc.MasterForKey(ctx, redisslot.SlotIndexTag(slot))
	if err != nil {
		return nil, err
	}
	before, err := slotOwnership(ctx, node, slot)
	if err != nil {
		if errors.Is(err, ErrTopologyChanged) {
			cc.ReloadState(ctx)
		}
		return nil, err
	}
	count, err := node.ClusterCountKeysInSlot(ctx, int(slot)).Result()
	if err != nil {
		return nil, err
	}
	want := int(count) + slotKeysSlack
	if want > limit {
		return nil, fmt.Errorf("playerredis: slot %d has %d keys, more than one listing of at most %d keys may return", slot, count, limit)
	}
	keys, err := node.ClusterGetKeysInSlot(ctx, int(slot), want).Result()
	if err != nil {
		return nil, err
	}
	if len(keys) >= want {
		return nil, fmt.Errorf("playerredis: slot %d grew by %d keys or more while it was listed, retry later", slot, slotKeysSlack)
	}
	after, err := slotOwnership(ctx, node, slot)
	if err != nil {
		if errors.Is(err, ErrTopologyChanged) {
			cc.ReloadState(ctx)
		}
		return nil, err
	}
	if before != after {
		cc.ReloadState(ctx)
		return nil, ErrTopologyChanged
	}
	return keys, nil
}

// slotOwnership 返回节点持有 slot 的凭据「节点 ID|run_id」。不持有这个 slot、或节点上有进行中的迁移时返回 ErrTopologyChanged。
func slotOwnership(ctx context.Context, node *redis.Client, slot uint16) (string, error) {
	nodes, err := node.ClusterNodes(ctx).Result()
	if err != nil {
		return "", err
	}
	self, err := parseMyselfLine(nodes)
	if err != nil {
		return "", err
	}
	if !rangesContain(self.ranges, slot) {
		return "", fmt.Errorf("%w: node %s does not own slot %d", ErrTopologyChanged, self.id, slot)
	}
	runID, err := runIDReader(ctx, node)
	if err != nil {
		return "", err
	}
	return self.id + "|" + runID, nil
}

// rangesContain 判断 CLUSTER NODES 的 slot 段("a" 或 "a-b",已由 parseMyselfLine 校验过)里有没有 slot。
func rangesContain(ranges []string, slot uint16) bool {
	for _, tok := range ranges {
		lo, hi, err := parseSlotRange(tok)
		if err == nil && int(slot) >= lo && int(slot) <= hi {
			return true
		}
	}
	return false
}
