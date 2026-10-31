package playerredis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"shared/redisslot"
)

// 按请求分页的跨玩家扫描(docs/design/redis-player-store-cluster.md §5.7)。
//
// 一次调用读一批:一个主节点上的一条 SCAN。游标 = 拓扑指纹摘要 + 主节点序号 + 节点内 SCAN 游标,由客户端带回来。
// 主节点序号只有在拓扑不变时才有意义,所以指纹变了游标就失效,从第一个主节点重新开始:玩家会看到重复的条目,不会有
// 错读,在线目录这种展示面按玩家 id 去重即可。不承诺跨页快照。
//
// 指纹与 ScanEachMaster 的不同(有意如此):ScanEachMaster 扫完才给结论,逐主节点读 run_id 与迁移标记是离线任务付得起的
// 代价;ScanPage 串在在线请求上,每页逐主节点读一遍就是 2 × 主节点数次往返,随规模线性增长。这里只读一个节点的
// CLUSTER NODES(一次往返),指纹取其中的稳定字段:主节点 ID 与它持有的 slot 段。代价:
//   - 主节点原地重启(节点 ID 不变、run_id 变)看不出来,旧 SCAN 游标在新进程上可能漏掉或重复一些键。展示面可以接受,
//     要「扫完 = 没有遗漏」的任务必须用 ScanEachMaster;
//   - 各节点的集群视图在拓扑变化期间可能短暂不一致,读到不同节点时指纹会多变一两次,多从头来一两次。
//
// 单实例没有拓扑,指纹是常量,不多一次往返。

// scanDigestLen 是游标里指纹摘要的长度(十六进制字符)。
const scanDigestLen = 8

// standaloneScanDigest 是单实例的指纹摘要。
const standaloneScanDigest = "00000000"

// scanCursorMaxMaster 是游标里主节点序号的上界。Redis Cluster 设计上限约 1000 个节点,这里只防客户端塞入离谱的数字。
const scanCursorMaxMaster = 65535

// ScanCursor 是 ScanPage 的游标。零值 = 从头开始。
type ScanCursor struct {
	digest string
	master int
	node   uint64
}

// IsStart 报告游标是否是零值。
func (c ScanCursor) IsStart() bool {
	return c == ScanCursor{}
}

// String 编码成不透明串:"<摘要>.<主节点序号>.<节点内游标>",零值编码成空串。最长 35 字节。
func (c ScanCursor) String() string {
	if c.IsStart() {
		return ""
	}
	return c.digest + "." + strconv.Itoa(c.master) + "." + strconv.FormatUint(c.node, 10)
}

// ErrInvalidScanCursor:游标不是 ScanCursor.String 产出的形状。它来自客户端,调用方按参数错误处理。
var ErrInvalidScanCursor = errors.New("playerredis: invalid scan cursor")

// ParseScanCursor 是 String 的逆。空串是零值。
func ParseScanCursor(raw string) (ScanCursor, error) {
	if raw == "" {
		return ScanCursor{}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || !isScanDigest(parts[0]) {
		return ScanCursor{}, ErrInvalidScanCursor
	}
	master, err := strconv.Atoi(parts[1])
	if err != nil || master < 0 || master > scanCursorMaxMaster {
		return ScanCursor{}, ErrInvalidScanCursor
	}
	node, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return ScanCursor{}, ErrInvalidScanCursor
	}
	return ScanCursor{digest: parts[0], master: master, node: node}, nil
}

func isScanDigest(s string) bool {
	if len(s) != scanDigestLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ScanPageResult 是一次 ScanPage 的结果。
type ScanPageResult struct {
	// Keys 是这一批的键,可能为空(SCAN 允许空批次)。
	Keys []string
	// Current 是实际扫这一批所用的位置。调用方只消费了这一批的一部分时,下一次带 Current 重读这一批。
	Current ScanCursor
	// Next 是下一批的位置,零值 = 整个玩家域已扫完。
	Next ScanCursor
	// Restarted:带来的游标已失效,这一批是从头扫的。调用方记在游标旁的「这一批消费到哪」不再有意义。
	Restarted bool
}

// ScanPage 从 cursor 起读一批键。count 是 SCAN 的 COUNT 提示。出错时不返回部分结果。
func ScanPage(ctx context.Context, c redis.UniversalClient, cursor ScanCursor, match string, count int64) (ScanPageResult, error) {
	topology, err := loadScanTopology(ctx, c)
	if err != nil {
		return ScanPageResult{}, err
	}
	result := ScanPageResult{Current: cursor}
	if cursor.IsStart() || cursor.digest != topology.digest || cursor.master >= len(topology.masters) {
		result.Restarted = !cursor.IsStart()
		result.Current = ScanCursor{digest: topology.digest}
	}
	node, err := topology.client(ctx, c, result.Current.master)
	if err != nil {
		return ScanPageResult{}, err
	}
	keys, next, err := node.Scan(ctx, result.Current.node, match, count).Result()
	if err != nil {
		return ScanPageResult{}, err
	}
	result.Keys = keys
	switch {
	case next != 0:
		result.Next = ScanCursor{digest: topology.digest, master: result.Current.master, node: next}
	case result.Current.master+1 < len(topology.masters):
		result.Next = ScanCursor{digest: topology.digest, master: result.Current.master + 1}
	}
	return result, nil
}

// scanMaster 是拓扑里的一个主节点。firstSlot 用来经 MasterForKey 取到它的客户端(ClusterClient 不暴露按节点 ID 取客户端)。
type scanMaster struct {
	id        string
	ranges    []string
	firstSlot int
}

// scanTopology 是一次拓扑快照。masters 按节点 ID 排序,下标就是游标里的主节点序号。
type scanTopology struct {
	digest  string
	masters []scanMaster
}

func (t scanTopology) client(ctx context.Context, c redis.UniversalClient, master int) (*redis.Client, error) {
	switch cc := c.(type) {
	case *redis.Client:
		return cc, nil
	case *redis.ClusterClient:
		// go-redis 的路由表落后时这里可能取到旧节点:读到的还是玩家键,下一页指纹一变就从头来。
		return cc.MasterForKey(ctx, redisslot.SlotIndexTag(uint16(t.masters[master].firstSlot)))
	default:
		return nil, ErrUnsupportedClient
	}
}

func loadScanTopology(ctx context.Context, c redis.UniversalClient) (scanTopology, error) {
	switch cc := c.(type) {
	case *redis.Client:
		return scanTopology{digest: standaloneScanDigest, masters: []scanMaster{{}}}, nil
	case *redis.ClusterClient:
		nodes, err := cc.ClusterNodes(ctx).Result()
		if err != nil {
			return scanTopology{}, err
		}
		return parseScanTopology(nodes)
	default:
		return scanTopology{}, ErrUnsupportedClient
	}
}

// parseScanTopology 从 CLUSTER NODES 输出里取主节点与它们的 slot 段,算指纹摘要。迁移标记 "[...]" 只出现在节点自己那一行,
// 不进指纹:否则读到不同节点时指纹会来回变。没有 slot 的主节点不持有键,不算。
func parseScanTopology(nodes string) (scanTopology, error) {
	masters := make([]scanMaster, 0)
	for _, line := range strings.Split(nodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || !hasFlag(fields[2], "master") {
			continue
		}
		master := scanMaster{id: fields[0], firstSlot: -1}
		for _, tok := range fields[8:] {
			if strings.HasPrefix(tok, "[") {
				continue
			}
			lo, _, err := parseSlotRange(tok)
			if err != nil {
				return scanTopology{}, err
			}
			if master.firstSlot < 0 || lo < master.firstSlot {
				master.firstSlot = lo
			}
			master.ranges = append(master.ranges, tok)
		}
		if len(master.ranges) > 0 {
			sort.Strings(master.ranges)
			masters = append(masters, master)
		}
	}
	if len(masters) == 0 {
		return scanTopology{}, fmt.Errorf("%w: CLUSTER NODES lists no master holding slots", ErrTopologyChanged)
	}
	sort.Slice(masters, func(i, j int) bool { return masters[i].id < masters[j].id })
	hash := sha256.New()
	for _, master := range masters {
		hash.Write([]byte(master.id + "|" + strings.Join(master.ranges, ",") + ";"))
	}
	return scanTopology{digest: hex.EncodeToString(hash.Sum(nil))[:scanDigestLen], masters: masters}, nil
}
