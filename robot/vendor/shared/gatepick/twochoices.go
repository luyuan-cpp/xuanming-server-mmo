// Package gatepick 是「从一批 gate 候选里挑一台」的共享规则。login 的 AssignGate 快路径
// (go/login/internal/logic/pkg/loginqueue PickGate)与 scene_manager 的跨 zone 重定向
// (go/scene_manager/internal/logic/gate_redirect.go)都用它,一条规则只有这一份实现:
//   - TwoChoices:随机二选一(本文件);
//   - Eligible:挑之前剔除停写与满载的 gate,全部不合格时回落全集(eligibility.go);
//   - RevisionTracker:给每次直读 etcd、没有 watch 镜像的调用方判停写(revisions.go)。
//
// 不含网络或存储 I/O:候选怎么来(etcd / 镜像)、排空与客户端地址怎么过滤由调用方决定,
// 随机源与时刻由调用方显式传入。设计与取舍见 docs/design/gate-load-balancing-design.md
// 「gate 目录怎么读、怎么挑」「满载与停写的 gate」。
package gatepick

// TwoChoices 用「随机二选一」从 items 里挑一个,返回下标;items 为空时 ok=false。
//
// 规则:随机抽两个不同的下标,取 load 小的那个;同 load 取第一次抽中的。只有一个就是它,恰好两个时等于取最小。
//   - 为什么不取全局最小:gate 人数每 10s 才上报一次,并发的请求互相看不到别人刚分出去的人,
//     确定性取最小会让一个上报周期里的新连接全部落到同一台。随机二选一在负载信息滞后时仍能摊开,
//     滞后窗口里最闲那台最多拿到 2/n;最忙的那台只有和同样忙的比时才会被选中。
//   - 为什么同 load 不按 NodeID 定胜负:开新区第一个上报窗口里所有 gate 都报 0,按 NodeID 定会让小号拿双倍份额、
//     最大号一个都分不到。两次抽样构成均匀的有序对,同 load 时两个各一半,全体就是均匀的。
//
// intn(n) 必须返回 [0, n) 上均匀分布的整数,生产用 math/rand/v2 的 rand.IntN(并发安全),单测传固定序列。
// 不修改 items,不跨调用记账。
func TwoChoices[T any](items []T, load func(T) uint32, intn func(int) int) (index int, ok bool) {
	n := len(items)
	if n == 0 {
		return 0, false
	}
	if n == 1 {
		return 0, true
	}
	i := intn(n)
	// 第二个从去掉 i 之后的 n-1 个里均匀抽,保证两个不同。
	j := intn(n - 1)
	if j >= i {
		j++
	}
	if load(items[j]) < load(items[i]) {
		return j, true
	}
	return i, true
}
