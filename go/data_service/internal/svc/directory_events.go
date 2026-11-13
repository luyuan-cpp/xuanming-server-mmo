package svc

import (
	"data_service/internal/metrics"
	"data_service/internal/store"
)

// directoryEvents 把账号目录「失败只记日志、不改结局」那几步的计数接到 internal/metrics(store.DirectoryEvents)。
// 无状态,并发安全;label 取值来自 store 的封闭集合,不带 player_id。
type directoryEvents struct{}

func (directoryEvents) NameBindFailed(reason string) { metrics.ObserveRoleNameBindFailure(reason) }

func (directoryEvents) RemovalSettleFailed(step string) { metrics.ObserveRoleRemovalSettleFailure(step) }

// configureDirectoryObservers 给账号目录接上失败计数与名字缓存失效,并预置计数序列(告警要序列先存在)。
// 名字缓存只是展示用的读缓存:目录补回或删掉名字行之后删它,补回的名字马上查得到(03-names.md §3.4a)。
func (s *ServiceContext) configureDirectoryObservers(directory *store.AccountDirectoryStore) {
	metrics.PrimeAccountDirectory(store.NameBindFailureReasons(), store.RemovalSettleSteps())
	directory.ConfigureEvents(directoryEvents{})
	// 判空之后才转成接口:nil 的 *routing.PlayerNameCache 转进去就是「非 nil 接口包着 nil 指针」,调用即 panic。
	if s.PlayerNameCache != nil {
		directory.ConfigureNameCache(s.PlayerNameCache)
	}
}
