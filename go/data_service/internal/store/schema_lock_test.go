package store

import (
	"strings"
	"testing"
	"time"
)

// 锁名带库名(不同库互不阻塞),超长时截断到 MySQL 的 64 字符上限 —— 超了 GET_LOCK 会直接报错。
func TestMigrateLockName(t *testing.T) {
	if got := migrateLockName("mmorpg_global"); got != "data_service.schema_migrate.mmorpg_global" {
		t.Fatalf("unexpected lock name: %q", got)
	}
	if migrateLockName("a") == migrateLockName("b") {
		t.Fatal("different databases must not share one migrate lock")
	}
	long := migrateLockName(strings.Repeat("x", 200))
	if len(long) != maxMigrateLockNameLen || !strings.HasPrefix(long, migrateLockPrefix) {
		t.Fatalf("an over-long name must be truncated to %d chars and keep the prefix, got %d: %q", maxMigrateLockNameLen, len(long), long)
	}
}

// GET_LOCK 的等待秒数:0 是"不等"、负数是"无限等",两者都不能传出去;有截止时间时不能等过它。
func TestMigrateLockWaitSeconds(t *testing.T) {
	now := time.Unix(1_000, 0)
	for _, tc := range []struct {
		name        string
		wait        time.Duration
		remaining   time.Duration
		hasDeadline bool
		want        int64
	}{
		{"zero_uses_default", 0, 0, false, int64(defaultMigrateLockWait / time.Second)},
		{"negative_uses_default", -time.Second, 0, false, int64(defaultMigrateLockWait / time.Second)},
		{"explicit_wait", 5 * time.Minute, 0, false, 300},
		{"rounds_up_to_whole_seconds", 1500 * time.Millisecond, 0, false, 2},
		{"sub_second_is_at_least_one", 10 * time.Millisecond, 0, false, 1},
		{"capped_by_deadline", 5 * time.Minute, 20 * time.Second, true, 20},
		{"deadline_further_than_wait", 30 * time.Second, 5 * time.Minute, true, 30},
		{"deadline_already_passed", 30 * time.Second, -3 * time.Second, true, 1},
		{"default_capped_by_deadline", 0, 7 * time.Second, true, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := migrateLockWaitSeconds(tc.wait, now.Add(tc.remaining), tc.hasDeadline, now)
			if got != tc.want {
				t.Fatalf("migrateLockWaitSeconds(%v, remaining=%v, hasDeadline=%v) = %d, want %d", tc.wait, tc.remaining, tc.hasDeadline, got, tc.want)
			}
		})
	}
}
