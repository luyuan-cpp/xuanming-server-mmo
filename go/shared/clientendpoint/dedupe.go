package clientendpoint

// DedupeNewest 按「有效客户端地址」去重:同一地址只保留 launch_time 最大的一条(D78)。
//
// 为什么必须做:客户端地址在 external 模式下是稳定的(同一序号的 gate 重建后仍是
// 同一个 host:nodePort),而崩溃 gate 的旧 NodeInfo 要等 NodeTTL(180s)才从 etcd 过期。
// 这段时间里新旧两条记录指向同一个客户端地址,旧记录被选中时客户端能连通 TCP,
// 却会被新 gate 以 token_gate_node_mismatch 拒绝(票据签的是旧 node_id)。
//
// 契约:
//
//   - addr 返回的键必须就是 Select 选中的 host:port(调用方负责拼接,两处出口各自一致即可);
//     addr / launchTime 须是确定性的纯函数(同一条记录会被取多次);
//   - launch_time 相同时保留先出现的那条;
//   - 输出保持入参中的相对顺序;
//   - 不修改入参;没有任何重复时直接返回入参切片本身(不复制),调用方不得假设返回值是副本。
//
// 每丢弃一条记录计一次 deduped。复杂度 O(n),n 是一次候选集的节点数。
func DedupeNewest[T any](items []T, addr func(T) string, launchTime func(T) uint64) []T {
	if len(items) < 2 {
		return items
	}

	// winner[地址] = 该地址当前胜出记录在 items 里的下标。
	// 只有严格更大的 launch_time 才能替换,于是并列时先出现者胜。
	winner := make(map[string]int, len(items))
	for i, item := range items {
		key := addr(item)
		j, seen := winner[key]
		if !seen || launchTime(item) > launchTime(items[j]) {
			winner[key] = i
		}
	}
	if len(winner) == len(items) {
		return items
	}

	kept := make([]T, 0, len(winner))
	for i, item := range items {
		if winner[addr(item)] == i {
			kept = append(kept, item)
		}
	}
	countDeduped(len(items) - len(kept))
	return kept
}
