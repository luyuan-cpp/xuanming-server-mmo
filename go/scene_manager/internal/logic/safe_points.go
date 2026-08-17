package logic

// 本包所有后台 goroutine / 循环的**点位名**。
//
// 为什么集中在一个文件:这些字符串会直接变成 Prometheus label
// safego_panic_total{point="..."},必须是**代码里写死的常量**,不能拼进
// zone_id / scene_id 之类运行期值(仓库 CLAUDE.md §9)。集中放置也让
// "本服务到底有哪些后台链路" 一眼可数 —— 裸 `go func(...)` 最大的问题不是
// panic 会打死进程,而是打死之后你看不出是哪条链路炸的。
//
// 命名约定:`scene_manager.<链路名>`,与服务名对齐,便于跨服务聚合告警。
const (
	// SafePointLoadReporter 是 etcd list-watch 主循环(节点发现 + 负载刷新 +
	// 死节点收尾 + 周期 rebalance)。它停摆等于 SceneManager 失去全部感知。
	SafePointLoadReporter = "scene_manager.load_reporter"

	// SafePointInstanceLifecycle 是空闲副本自动销毁循环。
	SafePointInstanceLifecycle = "scene_manager.instance_lifecycle"

	// SafePointWorldAutoscale 是大世界频道按人数自动扩缩容循环。
	SafePointWorldAutoscale = "scene_manager.world_autoscale"

	// SafePointAgonesReconcile 是 Agones rooms 计数与 Redis 映射的周期比对。
	SafePointAgonesReconcile = "scene_manager.agones_reconcile"

	// SafePointReleasePlayer 是跨节点交接时对老节点的异步 ReleasePlayer
	// (含重试链)。它是**每次调用起一条**的短命 goroutine,不是常驻循环。
	SafePointReleasePlayer = "scene_manager.release_player"
)
