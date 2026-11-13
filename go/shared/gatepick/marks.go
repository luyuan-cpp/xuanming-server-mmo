// marks.go — gate 状态标记在 Redis 里的键名与过期时间,写者与读者都引用这里,只定义一份。
//
// 三种标记都在控制集群(docs/design/redis-cluster-all-domains-20261110.md §4.4),键是 gate:{gm:<zone>}:<node_id>:<标记>。
// hash tag 按 zone 打、不按 gate 打:读者一次 MGET 读一个 zone 的全部 gate,排空脚本的标记脚本要在同一段 Lua 里核对
// 「同 zone 其它候选」的排空键,都要求一个 zone 的标记同槽;按 gate 打只能保住 draining / drained 这一对。
//
// zone 取 gate 登记所在的 etcd 前缀里的 zone(…/zone/<z>/node_type/<gate>/,与 NodeInfo.zone_id 同值):
//   - login 只看本 zone 的 gate(GateWatcher 的前缀带 Node.ZoneId),一律传自己的 Node.ZoneId;
//   - 排空脚本传它读到的 login ConfigMap 里的 Node.ZoneId(它也按这个 zone 的 etcd 前缀找 gate);
//   - scene_manager 跨 zone 重定向传目标 zone(它按目标 zone 的前缀直读 etcd)。
//
// 三方用的是同一个数,所以读写落在同一组键上。gate 的 node_id 由 etcd 全局分配
// (C++ EtcdManager::MakeNodeAllocationPrefix 不带 zone),全服唯一。
// tools/scripts/tests/k8s_gate_drain.tests.ps1 钉住这里的键形与排空脚本(tools/scripts/k8s_gate_drain.ps1)里的字面量一致。
package gatepick

import (
	"fmt"
	"time"

	"shared/redisslot"
)

// 三种标记的键形。第一个 %s 是 MarkTag(zone),%d 是 gate 的 node_id。
const (
	// DrainingKeyFmt:运维缩容前打的排空标记。写者是 tools/scripts/k8s_gate_drain.ps1 与 login 的 MarkGateDraining,
	// 值是标记时的 Unix 秒,必须带 TTL(标记的人中途挂了,gate 也不会永久不接客)。
	DrainingKeyFmt = "gate:%s:%d:draining"

	// DrainedKeyFmt:login 的排空判定循环判为「可以安全缩容」时写的标记(loginqueue.EvaluateDrainingGates),
	// 值是判定理由。排空脚本见到它才删 Pod。寿命不超过同一台 gate 的 draining 标记。
	DrainedKeyFmt = "gate:%s:%d:drained"

	// StaleKeyFmt:login 把本 zone 判为停写的 gate 写成的标记(loginqueue.PublishStaleMarks),值固定为 "1"。
	// 给没有连续观察历史的读者用:scene_manager 跨 zone 重定向每次直读目标 zone 的 etcd,本进程第一次读那个 zone 时
	// 判不出停写(RevisionTracker 要两次读才判得出),读这条标记就判得出。做法同 Kubernetes 节点控制器:
	// 观察者用自己的单调时钟判,消费者只读结果,不比较两台机器的墙钟。
	StaleKeyFmt = "gate:%s:%d:stale"
)

// StaleMarkTTL 是停写标记的过期时间,写者每 ReportPeriod 续写一次、从不主动删。
// 取 StaleAfter:一条标记跨三个续写周期,写者漏写两次也不断;gate 恢复之后写者不再续写,标记至多再留这么久就自然过期,
// 恢复的 gate 最多晚这么久重新参与跨 zone 重定向(login 自己挑 gate 读的是镜像,不受影响)。
const StaleMarkTTL time.Duration = StaleAfter

// MarkTag 返回 zone 的 gate 标记 hash tag,例如 MarkTag(3) = "{gm:3}"。
func MarkTag(zoneID uint32) string {
	return redisslot.ZoneControlTag(redisslot.ControlFamilyGateMark, zoneID)
}

// DrainingKey 返回 zoneID 里 nodeID 这台 gate 的排空标记键。
func DrainingKey(zoneID, nodeID uint32) string {
	return fmt.Sprintf(DrainingKeyFmt, MarkTag(zoneID), nodeID)
}

// DrainedKey 返回 zoneID 里 nodeID 这台 gate 的「已排空」标记键。
func DrainedKey(zoneID, nodeID uint32) string {
	return fmt.Sprintf(DrainedKeyFmt, MarkTag(zoneID), nodeID)
}

// StaleKey 返回 zoneID 里 nodeID 这台 gate 的停写标记键。
func StaleKey(zoneID, nodeID uint32) string {
	return fmt.Sprintf(StaleKeyFmt, MarkTag(zoneID), nodeID)
}
