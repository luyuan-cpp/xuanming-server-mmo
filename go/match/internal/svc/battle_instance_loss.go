package svc

import "context"

// BattleInstanceLossWatcher 是原执行进程失效的提前作废的可疑表与取证循环(docs/design/battle-instance-loss-early-abort.md §4,
// 实现在 logic/global_instance_loss.go)。svc 不 import logic,所以这里只放接口。
type BattleInstanceLossWatcher interface {
	// Suspect 把一局记成可疑,不阻塞、不做 IO;已在表里或表已满时什么都不做。可被多个 goroutine 同时调用。
	Suspect(battleID uint64)
	// Round 处理一轮到期的可疑局,每局自带有界预算;由 StartRelayLoop 每秒调一次,同一时刻只有一轮。
	Round(ctx context.Context)
}
