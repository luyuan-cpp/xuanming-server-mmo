package maintenancephase

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"shared/redisslot"
)

// Redis Cluster 目标的 P0 / P4 核对(docs/design/redis-player-store-cluster.md §5.7、docs/ops/redis-player-cluster-runbook.md)。
//
// 玩家域部署成 Cluster 时,player:{*}:handoff_fence 散在各主节点上,计划要把每个主节点列成一个物理目标。取样还是
// tools/scripts/maintenance_drain.ps1 的 collector 做(逐目标一条连接,SCAN 与读键都只看本节点,不跟随重定向),它不是
// playerredis.ScanEachMaster,但保证相同:本文件把全部目标的原件放在一起核,证明没有漏主节点,取样首尾拓扑没变;
// 任何一条不成立都按未闭合拒绝,不得据此断言键不存在。
//
// 单实例目标没有 Cluster 这一段,规则不变。

// authorityRedisClusterEvidence 是 collector 在一个目标上读到的 CLUSTER NODES:本节点身份,以及取样首尾两次拓扑指纹。
// 指纹把持有 slot 的主节点按节点 ID 排序,写成 <id>|<段>,<段>;<id>|<段>,段是 a-b 或 a。
type authorityRedisClusterEvidence struct {
	NodeID            string `json:"node_id"`
	FingerprintBefore string `json:"fingerprint_before"`
	FingerprintAfter  string `json:"fingerprint_after"`
}

type clusterSlotRange struct{ low, high int }

// parseClusterFingerprint 解析指纹,返回节点 ID → 所持 slot 段。要求节点不重复、段不重叠、合计正好 16384:
// 少了说明有 slot 没有主(或者指纹不全),这时扫完所有目标也不能断言扫完了玩家域。
func parseClusterFingerprint(fingerprint string) (map[string][]clusterSlotRange, error) {
	if fingerprint == "" {
		return nil, errors.New("Redis Cluster拓扑指纹为空")
	}
	masters := map[string][]clusterSlotRange{}
	var owner [redisslot.SlotCount]bool
	total := 0
	for _, entry := range strings.Split(fingerprint, ";") {
		id, ranges, found := strings.Cut(entry, "|")
		if !found || id == "" || ranges == "" {
			return nil, errors.New("Redis Cluster拓扑指纹格式非法")
		}
		if _, duplicated := masters[id]; duplicated {
			return nil, errors.New("Redis Cluster拓扑指纹节点重复")
		}
		for _, token := range strings.Split(ranges, ",") {
			lowText, highText, isRange := strings.Cut(token, "-")
			if !isRange {
				highText = lowText
			}
			low, lowErr := strconv.Atoi(lowText)
			high, highErr := strconv.Atoi(highText)
			if lowErr != nil || highErr != nil || low < 0 || high < low || high >= redisslot.SlotCount {
				return nil, errors.New("Redis Cluster拓扑指纹slot段非法")
			}
			for slot := low; slot <= high; slot++ {
				if owner[slot] {
					return nil, errors.New("Redis Cluster拓扑指纹slot重叠")
				}
				owner[slot] = true
			}
			total += high - low + 1
			masters[id] = append(masters[id], clusterSlotRange{low, high})
		}
	}
	if total != redisslot.SlotCount {
		return nil, fmt.Errorf("Redis Cluster拓扑指纹只覆盖%d个slot", total)
	}
	return masters, nil
}

// redisNonceKeyMatches 核对原件里的 nonce 键。单实例就是 run 派生的那一把;Cluster 下每个主节点一把,键尾带 hash tag,
// 让它落在这个主节点自己持有的 slot 上(否则 SET 回 MOVED,种不进去)。
func redisNonceKeyMatches(source *authorityRedisScan, base string) bool {
	if source.Cluster == nil {
		return source.NonceKey == base
	}
	if !strings.HasPrefix(source.NonceKey, base+":{") || !strings.HasSuffix(source.NonceKey, "}") {
		return false
	}
	masters, err := parseClusterFingerprint(source.Cluster.FingerprintAfter)
	if err != nil {
		return false
	}
	slot := int(redisslot.HashSlot(source.NonceKey))
	for _, slotRange := range masters[source.Cluster.NodeID] {
		if slot >= slotRange.low && slot <= slotRange.high {
			return true
		}
	}
	return false
}

// verifyAuthorityRedisClusterViews 把同一个阶段全部 Cluster 目标的读数放在一起核:每个目标取样首尾拓扑不变;
// 同一个集群的目标读到同一份拓扑;目标集合恰好等于持有 slot 的主节点。拓扑变了或少了主节点都拒绝。
// views 里只有 Cluster 目标,一个都没有时(全是单实例)直接通过。
func verifyAuthorityRedisClusterViews(views []*authorityRedisClusterEvidence) error {
	seenByFingerprint := map[string]map[string]bool{}
	for _, view := range views {
		if view.FingerprintBefore != view.FingerprintAfter {
			return errors.New("Redis Cluster拓扑在取样期间变化")
		}
		if seenByFingerprint[view.FingerprintAfter] == nil {
			seenByFingerprint[view.FingerprintAfter] = map[string]bool{}
		}
		if seenByFingerprint[view.FingerprintAfter][view.NodeID] {
			return errors.New("Redis Cluster同一主节点重复取样")
		}
		seenByFingerprint[view.FingerprintAfter][view.NodeID] = true
	}
	// 只允许一份拓扑:计划只列玩家域一个集群。两份说明目标之间取样时拓扑已经不同,不能缝合。
	if len(seenByFingerprint) > 1 {
		return errors.New("Redis Cluster目标读到的拓扑不一致")
	}
	for fingerprint, seen := range seenByFingerprint {
		masters, err := parseClusterFingerprint(fingerprint)
		if err != nil {
			return err
		}
		for id := range masters {
			if !seen[id] {
				return fmt.Errorf("Redis Cluster主节点%s没有取样,不能断言扫完了玩家域", id)
			}
		}
		for id := range seen {
			if _, ok := masters[id]; !ok {
				return fmt.Errorf("Redis Cluster目标%s不是持有slot的主节点", id)
			}
		}
	}
	return nil
}
