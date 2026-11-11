package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// 节点号的当前持有者(缺口清单第 33 条,docs/design/scene-node-ownership-reconcile.md §4.6)。
//
// C++ 节点按「最小空闲 id」分配 node_id。一个节点与 etcd 单边失联、租约到期之后,它的号可以被新起的进程接走,场景映射
// scene:{id}:node 里的那个号从此指新进程。失联的老进程只看映射分不清这个号还是不是自己,读到「是本节点」就会一直留着
// 已经交给新进程的场景。这里把每个节点号最近一次登记的进程写进本 zone 的控制域:老进程读到持有者换了一代、而且比自己
// 见过的那条更新,就知道这个号已经不归自己,把本地场景全部排空(cpp/libs/services/scene/world/scene_ownership_reconciler.cpp)。
//
// 值是 "<etcd ModRevision>:<node_uuid>"。ModRevision 是 etcd 全局单调的修订号:同一个节点号的新进程只能在老进程的分配键
// 过期之后登记,它那次登记的修订号一定更大。每个副本的 watch PUT 都写,领导者的全量同步再写一遍;同一个 etcd 纪元(下面)
// 里只在新值的修订号不小于已存值时覆盖,副本之间先后乱序也不会把新持有者盖回旧的。
//
// 不随 watch DELETE 删除:节点死了,它是这个号最后一次登记的进程仍是事实,新进程登记时自然覆盖。键带 TTL,全量同步给
// 在册节点每次续上;号长期没人用时自己过期。键以 node:zone:{z}: 开头,与其他节点级控制域键一样只由本 zone 的
// scene_manager 写(control-plane-zone-scoping-20261105.md §4.3),合服按前缀清得到。
//
// etcd 纪元。etcd 修订号倒退(换成一套新集群,或从备份恢复;运维规定所有节点重启,etcd-maintenance-runbook.md §7a)之后,
// 倒退之前写下的记录与新登记的修订号没有可比性,单调覆盖会把修订号更小的新登记拒掉。所以修订号只在同一个纪元里比:
//   - 每条持有者记录旁边记着写它时本 zone 的纪元(nodeHolderEpochKey)。写入时已存记录的纪元不是当前纪元,不比修订号,
//     直接覆盖;同一个纪元里才按修订号单调。C++ 只读持有者记录本身,不读纪元。
//   - 纪元由领导者的全量同步推进(publishSnapshotHolders):先读本 zone 的修订号水位(领导者读到过的 etcd 当前修订号的
//     最大值),再线性一致地读 etcd 当前修订号 C。C 小于水位就是倒退了:一个脚本里把水位降到 C、纪元加一(比较并写:
//     水位在这期间被别的写者改过就不动),打 ERROR、计数,配告警 SceneManagerEtcdRevisionRegressed
//     (deploy/k8s/scene-manager-alerts.yaml)。随后照常发布快照,快照里的号立刻换成新纪元的记录;不在快照里的号,
//     旧记录留在旧纪元,下一次登记时直接被覆盖。不倒退时水位抬到 C,只升不降。
//
// 先读水位、后读 C 是判据成立的前提:水位里的值都是某一任领导者读 etcd 得到的修订号,读到之后才写进来;同一个 etcd 里,
// 晚于那次读的线性一致读,C 一定不小于它。领导者交接时新旧两任同时在跑、或者这一轮的快照读得很早,都不会误判成倒退。
// 快照头修订号不能当 C:快照读完到写持有者之间,跟随者的 watch PUT 可能已经写进快照之后的新登记,修订号比快照头大,
// 但不是残留。
//
// 换纪元那一轮有一个窗口:倒退之后、换纪元之前写下的新登记也算旧纪元,会被快照里同号的登记覆盖;如果它比快照还新,
// 这个号的记录暂时回到上一代。本副本的 watch 从快照修订号之后接着读(watchAndRefresh),随后在新纪元里按单调覆盖改回。
// 这几秒里只是那个号的上一代进程晚一点认出被顶替;新进程看到的持有者修订号小于自己见过的,不会被误判顶替
// (scene_ownership_rules.h HolderTracker)。
//
// 水位只在控制域丢数据时才会没有;没有水位的那一轮只建立水位。水位刚丢、etcd 又恰在这一轮之前倒退,这次倒退认不出来:
// 旧记录里修订号大于新登记的,要等过期(不再续期,最长一天)才让位。只是少一层识别,不会误判排空。

// NodeHolderKeyFmt 是节点号持有者键。跨语言契约:C++ 侧 scene_ownership::NodeHolderKey 读同一个键,改名必须同批改。
const NodeHolderKeyFmt = "node:zone:%d:%s:holder"

// nodeHolderEpochKeyFmt 记着持有者记录写入时本 zone 的 etcd 纪元,与持有者记录同写同过期。只有 scene_manager 读写。
const nodeHolderEpochKeyFmt = "node:zone:%d:%s:holder_epoch"

// nodeHolderTTLSeconds 是持有者记录的存活时间。全量同步约每分钟续一次,一天足够盖住领导者长时间缺位;
// 失联的老进程只在号被别人接走之后才需要它,那时新进程在册、记录一直被续着。
const nodeHolderTTLSeconds = 24 * 60 * 60

// etcdRevisionSeenKeyFmt 是本 zone 的修订号水位:领导者在全量同步里读到过的 etcd 当前修订号的最大值,十进制串。
// etcdEpochKeyFmt 是本 zone 的 etcd 纪元,每认出一次倒退加一,没有时按 0。两个键都只由本 zone 的领导者写,不设 TTL:
// 水位过期就认不出倒退,纪元过期会让同一纪元的记录全部当成旧纪元重写一遍。只有 scene_manager 读写,不是跨语言契约。
const (
	etcdRevisionSeenKeyFmt = "scene_manager:etcd_revision_seen:zone:%d"
	etcdEpochKeyFmt        = "scene_manager:etcd_epoch:zone:%d"
)

// etcdRevisionReadTimeout 是全量同步里读 etcd 当前修订号的预算。在后台循环上,超时只让这一轮不判断倒退。
const etcdRevisionReadTimeout = 2 * time.Second

func nodeHolderKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(NodeHolderKeyFmt, zoneID, nodeID)
}

func nodeHolderEpochKey(zoneID uint32, nodeID string) string {
	return fmt.Sprintf(nodeHolderEpochKeyFmt, zoneID, nodeID)
}

func etcdRevisionSeenKey(zoneID uint32) string {
	return fmt.Sprintf(etcdRevisionSeenKeyFmt, zoneID)
}

func etcdEpochKey(zoneID uint32) string {
	return fmt.Sprintf(etcdEpochKeyFmt, zoneID)
}

// luaPublishNodeHolder 写入一次登记。已存记录属于当前纪元、而且修订号更大时不动;属于旧纪元(或没有纪元而当前纪元
// 不是 0)时直接覆盖。修订号按十进制串比较(generation_after,见 luaPlacementGenerationAfter:先比长度再比字典序,
// 不经 tonumber,大于 2^53 不丢精度);纪元按串相等比较,没有时按 '0'。
//
// KEYS[1] = node:zone:{z}:{n}:holder,KEYS[2] = node:zone:{z}:{n}:holder_epoch,KEYS[3] = scene_manager:etcd_epoch:zone:{z}。
// ARGV[1] = 修订号十进制串,ARGV[2] = node_uuid,ARGV[3] = 存活秒数。
// 回 1 = 写入(同一次登记重写时只是续期),0 = 已存的登记更新,一个字节不动。已存值没有冒号(损坏)时按没有处理,覆盖。
const luaPublishNodeHolder = luaPlacementGenerationAfter + `
local epoch = redis.call('GET', KEYS[3]) or '0'
local current = redis.call('GET', KEYS[1])
if current and (redis.call('GET', KEYS[2]) or '0') == epoch then
    local sep = string.find(current, ':', 1, true)
    if sep and generation_after(string.sub(current, 1, sep - 1), ARGV[1]) then
        return 0
    end
end
redis.call('SET', KEYS[1], ARGV[1] .. ':' .. ARGV[2], 'EX', ARGV[3])
redis.call('SET', KEYS[2], epoch, 'EX', ARGV[3])
return 1
`

// luaRaiseEtcdRevisionSeen 把水位抬到 ARGV[1],只升不降。已存值不是正的十进制串(损坏)时按没有处理,覆盖。
// KEYS[1] = 水位键。回 1 = 抬了,0 = 已存值不小于 ARGV[1]。
const luaRaiseEtcdRevisionSeen = luaPlacementGenerationAfter + `
local current = redis.call('GET', KEYS[1])
if current and string.match(current, '^[1-9][0-9]*$') and not generation_after(ARGV[1], current) then
    return 0
end
redis.call('SET', KEYS[1], ARGV[1])
return 1
`

// luaStartNewEtcdEpoch 是认出倒退之后的比较并写:水位仍是这一轮读到的 ARGV[1] 时,纪元加一、水位改成 ARGV[2]。
// KEYS[1] = 水位键,KEYS[2] = 纪元键。回新纪元(>= 1);水位已被别的写者改过时回 0,不动。
// 先 INCR 后 SET:纪元不是整数时 INCR 报错,脚本在任何写入之前停下,水位不会单独降下去。
const luaStartNewEtcdEpoch = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
    return 0
end
local epoch = redis.call('INCR', KEYS[2])
redis.call('SET', KEYS[1], ARGV[2])
return epoch
`

// nodeHolderRecord 是一次登记:哪个 zone 的哪个节点号、哪一代进程、etcd 修订号。
type nodeHolderRecord struct {
	ZoneID      uint32
	NodeID      string
	NodeUUID    string
	ModRevision int64
}

// publishNodeHolder 把一次登记写成这个节点号的持有者(规则见 luaPublishNodeHolder)。watch PUT 与全量同步都走这里。
// 尽力而为:写失败只记日志。C++ 节点读不到持有者时按「没有信息」处理,不会因此排空,只是认不出自己被顶替;
// 下一次 PUT 或领导者的全量同步会补上。node_uuid 为空或修订号无效的登记不写:没有可比较的东西。
func publishNodeHolder(svcCtx *svc.ServiceContext, rec nodeHolderRecord) {
	if rec.NodeID == "" || rec.NodeUUID == "" || rec.ModRevision <= 0 {
		return
	}
	keys := []string{nodeHolderKey(rec.ZoneID, rec.NodeID), nodeHolderEpochKey(rec.ZoneID, rec.NodeID), etcdEpochKey(rec.ZoneID)}
	if _, err := svcCtx.Redis.Eval(luaPublishNodeHolder, keys,
		strconv.FormatInt(rec.ModRevision, 10), rec.NodeUUID, nodeHolderTTLSeconds); err != nil {
		logx.Errorf("[NodeHolder] publishing the holder of zone=%d node=%s (uuid=%s revision=%d) failed: %v",
			rec.ZoneID, rec.NodeID, rec.NodeUUID, rec.ModRevision, err)
	}
}

var errEtcdNotConfigured = errors.New("etcd client not configured")

// currentEtcdRevision 线性一致地读 etcd 当前修订号。包级变量只为单测替换(SetCurrentEtcdRevisionForTest);
// 只由 load reporter 的 goroutine 调用,本包的单测顺序执行(没有 t.Parallel)。
var currentEtcdRevision = readCurrentEtcdRevision

func readCurrentEtcdRevision(ctx context.Context, svcCtx *svc.ServiceContext) (int64, error) {
	if svcCtx.Etcd == nil {
		return 0, errEtcdNotConfigured
	}
	readCtx, cancel := context.WithTimeout(ctx, etcdRevisionReadTimeout)
	defer cancel()
	// clientv3 的 Get 默认就是线性一致读(没有 WithSerializable)。只要应答头里的修订号,不取数据。
	resp, err := svcCtx.Etcd.Get(readCtx, sceneNodeZonePrefix(svcCtx.Config.ZoneId), clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}
	return resp.Header.Revision, nil
}

// SetCurrentEtcdRevisionForTest 替换读 etcd 当前修订号的函数并返回恢复函数,须配合 t.Cleanup。
func SetCurrentEtcdRevisionForTest(f func(ctx context.Context, svcCtx *svc.ServiceContext) (int64, error)) (restore func()) {
	prev := currentEtcdRevision
	currentEtcdRevision = f
	return func() { currentEtcdRevision = prev }
}

// publishSnapshotHolders 是领导者全量同步的持有者发布:先判断 etcd 修订号有没有倒退、倒退了就换纪元,再把快照里每条
// 在册登记写一遍(补上 watch 可能漏掉的 PUT,并给在册节点续 TTL),最后抬水位。判据与理由见文件头。
// records 是已过存活集闸门的快照里的登记。
func publishSnapshotHolders(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, records []nodeHolderRecord) {
	// 顺序不能换:先读水位,再读 etcd。
	seenRaw, seen, seenOK := readEtcdRevisionSeen(svcCtx, zoneID)
	current, err := currentEtcdRevision(ctx, svcCtx)
	if err == nil && current <= 0 {
		err = fmt.Errorf("invalid revision %d", current)
	}
	if err != nil {
		logx.Errorf("[NodeHolder] reading the current etcd revision failed; zone %d publishes node holders without checking for an etcd revision regression this round: %v",
			zoneID, err)
		publishNodeHolders(svcCtx, records)
		return
	}
	// 换纪元会让本 zone 所有旧记录让位,动手前再确认一次领导权:中途降级的副本不做,新任领导者的全量同步会重新判断。
	if seenOK && seen > current && isLeader() {
		startNewEtcdEpoch(svcCtx, zoneID, seenRaw, seen, current)
	}
	publishNodeHolders(svcCtx, records)
	raiseEtcdRevisionSeen(svcCtx, zoneID, current)
}

func publishNodeHolders(svcCtx *svc.ServiceContext, records []nodeHolderRecord) {
	for _, rec := range records {
		publishNodeHolder(svcCtx, rec)
	}
}

// readEtcdRevisionSeen 读本 zone 的水位,raw 是读到的原值(换纪元时比较并写用)。ok=false 表示读出错,这一轮判断不了倒退;
// 没有水位、或者水位损坏时 seen=0、ok=true,按还没有水位处理(损坏的值由抬水位的脚本覆盖)。
func readEtcdRevisionSeen(svcCtx *svc.ServiceContext, zoneID uint32) (raw string, seen int64, ok bool) {
	key := etcdRevisionSeenKey(zoneID)
	raw, err := svcCtx.Redis.Get(key)
	if err != nil {
		logx.Errorf("[NodeHolder] reading %s failed; the etcd revision regression check is skipped this round: %v", key, err)
		return "", 0, false
	}
	if raw == "" {
		return "", 0, true
	}
	seen, err = strconv.ParseInt(raw, 10, 64)
	if err != nil || seen <= 0 {
		logx.Errorf("[NodeHolder] ignoring malformed etcd revision watermark %s=%q; it is rewritten this round", key, raw)
		return raw, 0, true
	}
	return raw, seen, true
}

// startNewEtcdEpoch 换纪元(luaStartNewEtcdEpoch)。返回新纪元;水位在这期间被别的写者改过、或者写失败时返回 0,
// 下一轮全量同步重新判断。
func startNewEtcdEpoch(svcCtx *svc.ServiceContext, zoneID uint32, seenRaw string, seen, current int64) int64 {
	res, err := svcCtx.Redis.Eval(luaStartNewEtcdEpoch, []string{etcdRevisionSeenKey(zoneID), etcdEpochKey(zoneID)},
		seenRaw, strconv.FormatInt(current, 10))
	if err != nil {
		logx.Errorf("[NodeHolder] etcd revision went back in zone %d (current %d, recorded %d) but starting a new etcd epoch failed; retried next full sync: %v",
			zoneID, current, seen, err)
		return 0
	}
	epoch, _ := res.(int64)
	if epoch <= 0 {
		logx.Infof("[NodeHolder] the etcd revision watermark of zone %d changed while checking for a regression; checked again next full sync", zoneID)
		return 0
	}
	metrics.ObserveEtcdRevisionRegression()
	logx.Errorf("[NodeHolder] etcd revision went back in zone %d: current revision %d is below the recorded %d (etcd replaced or restored from a backup?); "+
		"started etcd epoch %d, node holder records written before give way to the next registration of each node id",
		zoneID, current, seen, epoch)
	return epoch
}

func raiseEtcdRevisionSeen(svcCtx *svc.ServiceContext, zoneID uint32, current int64) {
	key := etcdRevisionSeenKey(zoneID)
	if _, err := svcCtx.Redis.Eval(luaRaiseEtcdRevisionSeen, []string{key}, strconv.FormatInt(current, 10)); err != nil {
		logx.Errorf("[NodeHolder] raising %s to %d failed; retried next full sync: %v", key, current, err)
	}
}
