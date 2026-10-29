package playerstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	"shared/redisslot"
)

// MGetBySlot 是跨玩家批量读的唯一正确写法:按 slot 分组,每组一条 MGET,整批走一个 pipeline
// (ClusterClient 会按节点拆批),结果按输入顺序还原。返回值与 MGet 同形:缺键为 nil,存在为 string。
//
// go-redis 的 ClusterClient 按第一个键路由 MGET,不会拆分跨槽的键,直接 MGET 会收到 CROSSSLOT。
// 任何一组失败都返回错误、不返回部分结果:调用方自行决定 fail-open 还是 fail-closed。
func MGetBySlot(ctx context.Context, c redis.UniversalClient, keys []string) ([]interface{}, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	groups := make(map[uint16][]int)
	order := make([]uint16, 0)
	for i, key := range keys {
		slot := redisslot.HashSlot(key)
		if _, seen := groups[slot]; !seen {
			order = append(order, slot)
		}
		groups[slot] = append(groups[slot], i)
	}
	cmds := make([]*redis.SliceCmd, len(order))
	_, err := c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for gi, slot := range order {
			idx := groups[slot]
			groupKeys := make([]string, len(idx))
			for j, i := range idx {
				groupKeys[j] = keys[i]
			}
			cmds[gi] = p.MGet(ctx, groupKeys...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, len(keys))
	for gi, slot := range order {
		vals, cmdErr := cmds[gi].Result()
		if cmdErr != nil {
			return nil, cmdErr
		}
		idx := groups[slot]
		if len(vals) != len(idx) {
			return nil, fmt.Errorf("playerstore: MGET 返回 %d 个值,期望 %d 个", len(vals), len(idx))
		}
		for j, i := range idx {
			out[i] = vals[j]
		}
	}
	return out, nil
}

// ErrTopologyChanged:扫描期间集群拓扑或节点身份变化,或有进行中的 slot 迁移,结果不完整。
// 调用方必须把这次扫描当作不完整,不得据此断言「某键不存在」。
var ErrTopologyChanged = errors.New("playerstore: 扫描期间拓扑或节点身份变化,结果不完整")

// ErrUnsupportedClient:不是单实例 Client,也不是 ClusterClient。
var ErrUnsupportedClient = errors.New("playerstore: 不支持的客户端类型")

// runIDReader 读节点的 run_id(INFO server)。run_id 每次进程启动都换:节点原地重启后旧 SCAN 游标指向另一批桶,
// 续扫会漏键而不报错(go/player_locator session_reconciler 的同一理由)。测试可替换。
var runIDReader = func(ctx context.Context, node *redis.Client) (string, error) {
	info, err := node.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("playerstore: INFO server 缺少 run_id")
}

// ScanEachMaster 逐主节点 SCAN 完整个玩家域,对每批键调用 fn。fn 串行调用(内部加锁),但各主节点的扫描并行进行,
// 所以 fn 看到的批次顺序不确定。只用于离线全量任务;按请求分页的读取不得用它(全量遍历,延迟随规模线性增长)。
//
// fail-closed:任何一个节点出错、fn 返回错误、或扫描前后指纹不一致(含节点重启、slot 归属变化、有进行中的迁移),
// 整体返回错误,调用方不得把它当作「已扫完」。单实例客户端核对扫描前后的 run_id。
func ScanEachMaster(ctx context.Context, c redis.UniversalClient, match string, count int64, fn func(keys []string) error) error {
	var mu sync.Mutex
	serial := func(keys []string) error {
		mu.Lock()
		defer mu.Unlock()
		return fn(keys)
	}
	switch cc := c.(type) {
	case *redis.Client:
		before, err := runIDReader(ctx, cc)
		if err != nil {
			return err
		}
		if err := scanNode(ctx, cc, match, count, serial); err != nil {
			return err
		}
		after, err := runIDReader(ctx, cc)
		if err != nil {
			return err
		}
		if before != after {
			return ErrTopologyChanged
		}
		return nil
	case *redis.ClusterClient:
		before, err := clusterFingerprint(ctx, cc)
		if err != nil {
			return err
		}
		if err := cc.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return scanNode(ctx, node, match, count, serial)
		}); err != nil {
			return err
		}
		after, err := clusterFingerprint(ctx, cc)
		if err != nil {
			return err
		}
		if before != after {
			return ErrTopologyChanged
		}
		return nil
	default:
		return ErrUnsupportedClient
	}
}

func scanNode(ctx context.Context, node *redis.Client, match string, count int64, fn func(keys []string) error) error {
	var cursor uint64
	for {
		keys, next, err := node.Scan(ctx, cursor, match, count).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := fn(keys); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// clusterFingerprint 逐个主节点读取「节点 ID | run_id | 自己持有的 slot 段」,排序后拼成指纹。
// 主节点集合与扫描用的是同一个 ForEachMaster 状态;再要求全部主节点自报的 slot 合计正好 16384、且没有任何
// 迁移标记——这样即使 ForEachMaster 回落到过期的缓存状态(漏掉新主),合计也对不上,不会静默通过。
func clusterFingerprint(ctx context.Context, cc *redis.ClusterClient) (string, error) {
	var mu sync.Mutex
	parts := make([]string, 0)
	total := 0
	err := cc.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		nodes, err := node.ClusterNodes(ctx).Result()
		if err != nil {
			return err
		}
		self, err := parseMyselfLine(nodes)
		if err != nil {
			return err
		}
		runID, err := runIDReader(ctx, node)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		parts = append(parts, self.id+"|"+runID+"|"+strings.Join(self.ranges, ","))
		total += self.slots
		return nil
	})
	if err != nil {
		return "", err
	}
	if total != redisslot.SlotCount {
		return "", fmt.Errorf("%w: 主节点自报 slot 合计 %d", ErrTopologyChanged, total)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";"), nil
}

// myselfLine 是 CLUSTER NODES 里 myself 那一行的解析结果。
type myselfLine struct {
	id     string
	ranges []string
	slots  int
}

// parseMyselfLine 解析 CLUSTER NODES 输出中带 myself 标志的那一行:
// <id> <ip:port@cport[,hostname]> <flags> <master> <ping> <pong> <epoch> <link> <slot>...
// slot 字段是 "a-b" 或 "a";"[s->-id]" / "[s-<-id]" 是逐键迁移的迁出 / 迁入标记,出现即视为拓扑不稳定。
func parseMyselfLine(nodes string) (myselfLine, error) {
	for _, line := range strings.Split(nodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || !hasFlag(fields[2], "myself") {
			continue
		}
		out := myselfLine{id: fields[0]}
		for _, tok := range fields[8:] {
			if strings.HasPrefix(tok, "[") {
				return myselfLine{}, fmt.Errorf("%w: 节点 %s 有进行中的迁移 %s", ErrTopologyChanged, fields[0], tok)
			}
			lo, hi, err := parseSlotRange(tok)
			if err != nil {
				return myselfLine{}, err
			}
			out.ranges = append(out.ranges, tok)
			out.slots += hi - lo + 1
		}
		return out, nil
	}
	return myselfLine{}, errors.New("playerstore: CLUSTER NODES 中没有 myself 行")
}

func hasFlag(flags, want string) bool {
	for _, f := range strings.Split(flags, ",") {
		if f == want {
			return true
		}
	}
	return false
}

func parseSlotRange(tok string) (int, int, error) {
	loText, hiText, isRange := strings.Cut(tok, "-")
	lo, err := strconv.Atoi(loText)
	if err != nil {
		return 0, 0, fmt.Errorf("playerstore: 无法解析 slot 段 %q", tok)
	}
	hi := lo
	if isRange {
		if hi, err = strconv.Atoi(hiText); err != nil {
			return 0, 0, fmt.Errorf("playerstore: 无法解析 slot 段 %q", tok)
		}
	}
	if lo < 0 || hi < lo || hi >= redisslot.SlotCount {
		return 0, 0, fmt.Errorf("playerstore: slot 段越界 %q", tok)
	}
	return lo, hi, nil
}
