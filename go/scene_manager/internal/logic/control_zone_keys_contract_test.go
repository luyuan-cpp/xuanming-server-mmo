package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 控制域只读写本 zone 的键(docs/design/control-plane-zone-scoping-20261105.md §4.2、§4.3)。
// 几个 zone 共用一个控制域实例、各自选主之后,全服唯一的键会让一个 zone 的领导者去动同组别的 zone:
// 原来的三把全服键里,选主锁与死节点收尾任务表改按 zone,zone 登记集合删除。本用例钉住这三点,
// 并在源码里查旧键名的字面量,防止有人照旧写回全服键。

func TestControlZoneKeysContract_RenamedKeysArePerZone(t *testing.T) {
	for _, key := range []func(uint32) string{LeaderLockKey, deadNodeReconcileTasksKey} {
		a, b := key(101), key(102)
		assert.NotEqual(t, a, b, "不同 zone 必须是不同的键")
		assert.True(t, strings.HasSuffix(a, ":zone:101"), "%s 要以 :zone:<z> 结尾", a)
		assert.False(t, strings.HasPrefix(a, "node:zone:"), "%s 不得落进合服按 node:zone:{S}:* 清理的前缀", a)
	}
}

func TestControlZoneKeysContract_NoGlobalControlKeysInSource(t *testing.T) {
	// 旧的全服键字面量(带引号结尾:按 zone 的新键名以它们为前缀,不算)。
	forbidden := []string{
		`"scene_manager:leader:lock"`,
		`"scene_manager:dead_node_reconcile"`,
		`"scene_manager:zones"`,
	}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		text := string(raw)
		for _, literal := range forbidden {
			assert.NotContains(t, text, literal, "%s 里出现了全服控制域键 %s", file, literal)
		}
	}
	// 选主接线在 package main,同样不得写死全服锁。
	raw, err := os.ReadFile(filepath.Join("..", "..", "scene_manager_service.go"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"scene_manager:leader:lock"`)
	assert.Contains(t, string(raw), "logic.LeaderLockKey(c.ZoneId)", "选主锁必须按配置的 zone 取")
}
