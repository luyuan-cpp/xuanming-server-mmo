// marks.go — gate 状态标记在 Redis 里的键名与过期时间,写者与读者都引用这里,只定义一份。
//
// 两种标记都放在全服共用的核心 Redis(login 的 Node.RedisClient、scene_manager 的 CoreRedis 指向它,
// G4a §25.3 的 10-23 订正;docs/design/control-plane-zone-scoping-20261105.md 的键表)。键里只有 node_id:
// gate 的 node_id 由 etcd 全局分配(C++ EtcdManager::MakeNodeAllocationPrefix 不带 zone),全服唯一,不会跨 zone 撞号。
// tools/scripts/tests/k8s_gate_drain.tests.ps1 钉住排空键名与排空脚本(tools/scripts/k8s_gate_drain.ps1)里的字面量一致。
package gatepick

import (
	"fmt"
	"time"
)

// DrainingKeyFmt:运维缩容前打的排空标记。写者是 tools/scripts/k8s_gate_drain.ps1 与 login 的 MarkGateDraining,
// 值是标记时的 Unix 秒,必须带 TTL(标记的人中途挂了,gate 也不会永久不接客)。
const DrainingKeyFmt = "gate:%d:draining"

// StaleKeyFmt:login 把本 zone 判为停写的 gate 写成的标记(loginqueue.PublishStaleMarks),值固定为 "1"。
// 给没有连续观察历史的读者用:scene_manager 跨 zone 重定向每次直读目标 zone 的 etcd,本进程第一次读那个 zone 时
// 判不出停写(RevisionTracker 要两次读才判得出),读这条标记就判得出。做法同 Kubernetes 节点控制器:
// 观察者用自己的单调时钟判,消费者只读结果,不比较两台机器的墙钟。
const StaleKeyFmt = "gate:%d:stale"

// StaleMarkTTL 是停写标记的过期时间,写者每 ReportPeriod 续写一次、从不主动删。
// 取 StaleAfter:一条标记跨三个续写周期,写者漏写两次也不断;gate 恢复之后写者不再续写,标记至多再留这么久就自然过期,
// 恢复的 gate 最多晚这么久重新参与跨 zone 重定向(login 自己挑 gate 读的是镜像,不受影响)。
const StaleMarkTTL time.Duration = StaleAfter

// DrainingKey 返回 nodeID 的排空标记键。
func DrainingKey(nodeID uint32) string {
	return fmt.Sprintf(DrainingKeyFmt, nodeID)
}

// StaleKey 返回 nodeID 的停写标记键。
func StaleKey(nodeID uint32) string {
	return fmt.Sprintf(StaleKeyFmt, nodeID)
}
