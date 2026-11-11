package logic

import (
	"fmt"
	"strconv"

	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 节点号的当前持有者(缺口清单第 33 条,docs/design/scene-node-ownership-reconcile.md §4.6)。
//
// C++ 节点按「最小空闲 id」分配 node_id。一个节点与 etcd 单边失联、租约到期之后,它的号可以被新起的进程接走,场景映射
// scene:{id}:node 里的那个号从此指新进程。失联的老进程只看映射分不清这个号还是不是自己,读到「是本节点」就会一直留着
// 已经交给新进程的场景。这里把每个节点号最近一次登记的进程写进本 zone 的控制域:老进程读到持有者换了一代、而且比自己
// 那次登记更新,就知道这个号已经不归自己,把本地场景全部排空(cpp/libs/services/scene/world/scene_ownership_reconciler.cpp)。
//
// 值是 "<etcd ModRevision>:<node_uuid>"。ModRevision 是 etcd 全局单调的修订号:同一个节点号的新进程只能在老进程的分配键
// 过期之后登记,它那次登记的修订号一定更大。每个副本的 watch PUT 都写,领导者的全量同步再写一遍;写入只在新值的修订号
// 不小于已存值时覆盖,副本之间先后乱序也不会把新持有者盖回旧的。
//
// 不随 watch DELETE 删除:节点死了,它是这个号最后一次登记的进程仍是事实,新进程登记时自然覆盖。键带 TTL,全量同步给
// 在册节点每次续上;号长期没人用时自己过期。键以 node:zone:{z}: 开头,与其他节点级控制域键一样只由本 zone 的
// scene_manager 写(control-plane-zone-scoping-20261105.md §4.3),合服按前缀清得到。

// NodeHolderKeyFmt 是节点号持有者键。跨语言契约:C++ 侧 scene_ownership::NodeHolderKey 读同一个键,改名必须同批改。
const NodeHolderKeyFmt = "node:zone:%d:%s:holder"

// nodeHolderTTLSeconds 是持有者记录的存活时间。全量同步约每分钟续一次,一天足够盖住领导者长时间缺位;
// 失联的老进程只在号被别人接走之后才需要它,那时新进程在册、记录一直被续着。
const nodeHolderTTLSeconds = 24 * 60 * 60

func nodeHolderKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeHolderKeyFmt, zoneID, nodeID)
}

// luaPublishNodeHolder 写入一次登记,已存的登记更新时不动。修订号按十进制串比较(generation_after,见
// luaPlacementGenerationAfter:先比长度再比字典序,不经 tonumber,大于 2^53 不丢精度)。
//
// KEYS[1] = node:zone:{z}:{n}:holder。ARGV[1] = 修订号十进制串,ARGV[2] = node_uuid,ARGV[3] = 存活秒数。
// 回 1 = 写入(同一次登记重写时只是续期),0 = 已存的登记更新,一个字节不动。已存值没有冒号(损坏)时按没有处理,覆盖。
const luaPublishNodeHolder = luaPlacementGenerationAfter + `
local current = redis.call('GET', KEYS[1])
if current then
    local sep = string.find(current, ':', 1, true)
    if sep and generation_after(string.sub(current, 1, sep - 1), ARGV[1]) then
        return 0
    end
end
redis.call('SET', KEYS[1], ARGV[1] .. ':' .. ARGV[2], 'EX', ARGV[3])
return 1
`

// nodeHolderRecord 是一次登记:哪个 zone 的哪个节点号、哪一代进程、etcd 修订号。
type nodeHolderRecord struct {
	ZoneID      uint32
	NodeID      string
	NodeUUID    string
	ModRevision int64
}

// publishNodeHolder 把一次登记写成这个节点号的持有者。尽力而为:写失败只记日志。C++ 节点读不到持有者时按「没有信息」处理,
// 不会因此排空,只是认不出自己被顶替;下一次 PUT 或领导者的全量同步会补上。node_uuid 为空或修订号无效的登记不写:
// 没有可比较的东西。
func publishNodeHolder(svcCtx *svc.ServiceContext, rec nodeHolderRecord) {
	if rec.NodeID == "" || rec.NodeUUID == "" || rec.ModRevision <= 0 {
		return
	}
	if _, err := svcCtx.Redis.Eval(luaPublishNodeHolder, []string{nodeHolderKey(rec.ZoneID, rec.NodeID)},
		strconv.FormatInt(rec.ModRevision, 10), rec.NodeUUID, nodeHolderTTLSeconds); err != nil {
		logx.Errorf("[NodeHolder] publishing the holder of zone=%d node=%s (uuid=%s revision=%d) failed: %v",
			rec.ZoneID, rec.NodeID, rec.NodeUUID, rec.ModRevision, err)
	}
}

// publishNodeHolders 是全量同步用的批量版本:领导者对快照里每一条在册登记写一次,补上 watch 可能漏掉的 PUT 并续 TTL。
func publishNodeHolders(svcCtx *svc.ServiceContext, records []nodeHolderRecord) {
	for _, rec := range records {
		publishNodeHolder(svcCtx, rec)
	}
}
