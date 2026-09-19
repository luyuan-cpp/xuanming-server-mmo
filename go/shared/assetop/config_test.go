package assetop

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

// 默认配置的 golden 用例。
//
// **改这里的任何一个数字 = 改运维契约,必须同步改 docs/design/guild-phase2/90-consistency.md
// 的 Y-06 表**(那张表把这批默认值写成了 `AssetOpConf` 的配置项默认值,帮会 / 聚宝斋的
// yaml 与 §5.39 的 config_test 都照着它写)。没有这道断言时,顺手把某个数调一下,
// 代码、文档、两个服务的 yaml 会分成三份真相,而且全程不会有任何测试变红。
//
// 这些值不是随便取的,改之前先想清楚它压着什么:
//   - Workers 8 压着 scene 的 sync server poller 上限(reconcile.go 的 LoopConfig.Workers
//     注释),调大会把玩家的进场 / 开战请求一起挤掉;
//   - Batch/Interval 决定一个副本每秒最多投多少行;
//   - Lease 与 OpBudget 的差(leaseHeadroom)决定会不会出现「还在处理就被别的副本领走」;
//   - PoisonDelay 决定一行解不开的毒行多久回来看一次。

// TestDefaultLoopConfigGolden 逐项钉死 DefaultLoopConfig 的默认值。
func TestDefaultLoopConfigGolden(t *testing.T) {
	cfg := DefaultLoopConfig()

	// 前 8 项就是 Y-06 表里的 8 个字段(名字按配置项写法:ReconcileIntervalMs、
	// ReconcileBatch、Workers、LeaseMs、OpBudgetMs、MaxBackoffMs、PoisonDelayMs、
	// LedgerReadMinAttempts);后 2 项不在那张表里:BaseBackoff 帮会用
	// GuildRule.asset_op_retry_base_ms 覆盖,AwaitDurableDelay 由 S4 §4.19 固定。
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Interval(Y-06 ReconcileIntervalMs=2000)", cfg.Interval, 2 * time.Second},
		{"Batch(Y-06 ReconcileBatch=100)", cfg.Batch, 100},
		{"Workers(Y-06 Workers=8)", cfg.Workers, 8},
		{"Lease(Y-06 LeaseMs=10000)", cfg.Lease, 10 * time.Second},
		{"OpBudget(Y-06 OpBudgetMs=2500)", cfg.OpBudget, 2500 * time.Millisecond},
		{"MaxBackoff(Y-06 MaxBackoffMs=60000)", cfg.MaxBackoff, 60 * time.Second},
		{"PoisonDelay(Y-06 PoisonDelayMs=3600000)", cfg.PoisonDelay, time.Hour},
		{"LedgerReadMinAttempts(Y-06 = 3)", cfg.LedgerReadMinAttempts, uint32(3)},
		{"BaseBackoff(S4 §4.19,帮会按 GuildRule 覆盖)", cfg.BaseBackoff, time.Second},
		{"AwaitDurableDelay(S4 §4.19)", cfg.AwaitDurableDelay, 500 * time.Millisecond},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 应为 %v,实际 %v;要改就连 90-consistency.md 的 Y-06 表一起改", c.name, c.want, c.got)
		}
	}

	// Streams 是每个服务自己的独占流(不变量 I6),默认不能替调用方猜。
	if cfg.Streams != nil {
		t.Errorf("Streams 默认应为 nil,实际 %v", cfg.Streams)
	}

	// 字段数守卫:新增字段时本用例必须一起改,否则新默认值会无人认领地溜进运维契约。
	if n := reflect.TypeOf(cfg).NumField(); n != len(checks)+1 {
		t.Errorf("LoopConfig 现有 %d 个字段,本用例只钉了 %d 个;新增字段要同时补断言和 Y-06 表",
			n, len(checks)+1)
	}

	// 默认值本身必须过自检:默认配置启动不起来是最糟的一类回归。
	if err := cfg.validate(); err != nil {
		t.Errorf("默认配置应自洽,实际: %v", err)
	}
}

// TestDefaultTxRetryConfigGolden 钉死事务基座的默认值。
//
// 隔离级尤其要钉:90-consistency part2 §2 规则 2/6(裁决 D7)要求帮会全部写事务统一
// READ COMMITTED,而规则 6 给的 WithTxRetry 调用形式里没有隔离级参数 —— 这条规则完全
// 落在这个默认值上。退避区间钉住的是「重试之间真的会等」以及「等的量级在事务子预算内」。
func TestDefaultTxRetryConfigGolden(t *testing.T) {
	cfg := DefaultTxRetryConfig()

	if cfg.Isolation != sql.LevelReadCommitted {
		t.Errorf("默认隔离级应为 READ COMMITTED(D7),实际 %v", cfg.Isolation)
	}
	if cfg.Attempts != 3 {
		t.Errorf("默认尝试次数应为 3(90-consistency part2 §2 第 6 条),实际 %d", cfg.Attempts)
	}
	if cfg.BaseBackoff != 10*time.Millisecond {
		t.Errorf("默认退避基数应为 10ms,实际 %v", cfg.BaseBackoff)
	}
	if cfg.MaxBackoff != 200*time.Millisecond {
		t.Errorf("默认退避封顶应为 200ms,实际 %v", cfg.MaxBackoff)
	}
	// Rand 留 nil 会让 NextAttemptMs 取中值,退避就没有抖动了 —— 同一批撞在同一行上的
	// 事务会整齐地一起回来,退避只是把惊群推迟了一下。
	if cfg.Rand == nil {
		t.Error("默认必须带随机源,否则退避没有抖动")
	}
	// 分类函数默认为 nil = 一次都不重试:shared 不引 MySQL 驱动,分类只能由调用方注入,
	// 默认"不猜"比默认"全重试"安全(AGENTS §11.3 fail-closed)。
	if cfg.IsRetryable != nil {
		t.Error("默认不应自带错误分类函数")
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("默认配置应自洽,实际: %v", err)
	}

	// 字段数守卫,理由同上:Attempts / IsRetryable / Isolation / BaseBackoff /
	// MaxBackoff / Rand / sleep。
	if n := reflect.TypeOf(cfg).NumField(); n != 7 {
		t.Errorf("TxRetryConfig 现有 %d 个字段,本用例按 7 个写;新增字段要同时补断言", n)
	}
}
