
package table

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// SchemaDigest 是整批服务端配置表的数据布局摘要(16 位小写十六进制,docs/design/config-table-hot-reload.md §5.1)。
const SchemaDigest = "db59930c8d215166"

var hotReloadableTables = map[string]bool{
	"Reward": true,
}

var allTableNames = []string{
	"Mission",
	"Reward",
}

// IsHotReloadable 按本二进制编译期名单判定该表是否允许运行期热更(cfg_hot_reload)。
func IsHotReloadable(tableName string) bool {
	return hotReloadableTables[tableName]
}

// TableNames 返回本二进制包含的全部表名副本(按字典序)。
func TableNames() []string {
	out := make([]string, len(allTableNames))
	copy(out, allTableNames)
	return out
}

// BundleIdentity 记录一套 TableSet 的来源与摘要身份。
type BundleIdentity struct {
	SchemaDigest  string
	ContentDigest string
	Version       uint64
	LoadSeq       uint64
	Source        string // "image" 或 "bundle"
}

// TableSet 持有整批不可变表快照:需要跨多张表读同一版本的业务先取 ts := table.Current() 再经 ts 读取。
type TableSet struct {
	Identity BundleIdentity
	Mission *MissionTableManager
	Reward *RewardTableManager
}

var (
	loadSuccessCallback func()
	currentTableSet     atomic.Pointer[TableSet]
	commitSeq           atomic.Uint64
	commitCallbacksMu   sync.Mutex
	commitCallbacks     []func(*TableSet)
)

// Current 返回当前已提交的整套不可变 TableSet;尚未 LoadTables / Commit 前返回 nil。
func Current() *TableSet {
	return currentTableSet.Load()
}

// Prepare 从 configDir 解析全部配置表到一套全新的独立 TableSet,不修改任何现役单例。
// 任意一张表失败立即返回错误。
func Prepare(configDir string, useBinary bool) (*TableSet, error) {
	ts := &TableSet{
		Identity: BundleIdentity{
			SchemaDigest: SchemaDigest,
		},
		Mission: NewMissionTableManager(),
		Reward: NewRewardTableManager(),
	}
	if err := ts.Mission.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Mission table failed: %w", err)
	}
	if err := ts.Reward.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Reward table failed: %w", err)
	}
	return ts, nil
}

// Commit 把 Prepare 产出的整套 TableSet 提交生效:
// 先替换全部包级单例的原子快照指针,再原子发布 Current(),最后按序回调 OnTablesCommit 监听器。
func Commit(ts *TableSet) {
	if ts == nil {
		return
	}
	ts.Identity.LoadSeq = commitSeq.Add(1)
	if ts.Mission != nil {
		MissionTableManagerInstance.swapSnapshot(ts.Mission.snap.Load())
	}
	if ts.Reward != nil {
		RewardTableManagerInstance.swapSnapshot(ts.Reward.snap.Load())
	}
	currentTableSet.Store(ts)

	commitCallbacksMu.Lock()
	cbs := make([]func(*TableSet), len(commitCallbacks))
	copy(cbs, commitCallbacks)
	commitCallbacksMu.Unlock()
	for _, cb := range cbs {
		cb(ts)
	}
}

// OnTablesCommit 注册每次整批 Commit 生效后的回调(启动与热更均会触发)。
func OnTablesCommit(cb func(*TableSet)) {
	if cb == nil {
		return
	}
	commitCallbacksMu.Lock()
	commitCallbacks = append(commitCallbacks, cb)
	commitCallbacksMu.Unlock()
}

// LoadTables loads all config tables synchronously.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTables(configDir string, useBinary bool) {
	ts, err := Prepare(configDir, useBinary)
	if err != nil {
		log.Fatalf("failed to load tables from %s: %v", configDir, err)
	}
	ts.Identity.Source = "image"
	Commit(ts)

	if loadSuccessCallback != nil {
		loadSuccessCallback()
	}
}

// LoadTablesAsync loads all config tables using goroutines.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTablesAsync(configDir string, useBinary bool) {
	ts := &TableSet{
		Identity: BundleIdentity{
			SchemaDigest: SchemaDigest,
			Source:       "image",
		},
		Mission: NewMissionTableManager(),
		Reward: NewRewardTableManager(),
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := ts.Mission.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Mission table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Reward.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Reward table: %v", err)
		}
	}()

	wg.Wait()
	Commit(ts)

	if loadSuccessCallback != nil {
		loadSuccessCallback()
	}
}

// OnTablesLoadSuccess registers a callback invoked after initial table load completes.
func OnTablesLoadSuccess(cb func()) {
	loadSuccessCallback = cb
}