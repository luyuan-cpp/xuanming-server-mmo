package svc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"guild/internal/config"
	assetpb "proto/common/asset"
	"shared/assetop"
)

// stubAssetStore 满足 assetop.Store,什么都不做:这里只测装配与启停,不测 outbox 语义
// (那是 data.GuildAssetStore 的真库用例)。
type stubAssetStore struct{}

func (stubAssetStore) ListDue(context.Context, uint64, int) ([]uint64, error) { return nil, nil }

func (stubAssetStore) Claim(context.Context, uint64, uint64, uint64, uint64, uint64) (assetop.Op, bool, error) {
	return assetop.Op{}, false, nil
}

func (stubAssetStore) Finalize(context.Context, assetop.Op, assetop.Status, assetop.Result, uint64) (bool, error) {
	return false, nil
}

func (stubAssetStore) Reschedule(context.Context, assetop.Op, uint64, assetop.Result, uint64) error {
	return nil
}

type stubApplier struct{}

func (stubApplier) Do(context.Context, assetop.RPC, *assetpb.AssetOpRequest) (assetop.Result, error) {
	return assetop.Result{}, nil
}

// y06Defaults 是 90-consistency Y-06 的默认值,加落地恢复(docs/design/landing-recovery-sync-digest.md §6)
// 七项的默认值(与 etc/guild.yaml 一致)。Wake 段留零值 = 订阅开、并发与超时取 assetopwake 默认值,与 yaml 显式写的同值。
func y06Defaults() config.AssetOpConf {
	return config.AssetOpConf{
		Enabled:               true,
		ReconcileIntervalMs:   2000,
		ReconcileBatch:        100,
		Workers:               8,
		LeaseMs:               10000,
		OpBudgetMs:            2500,
		MaxBackoffMs:          60000,
		PoisonDelayMs:         3600000,
		LedgerReadMinAttempts: 3,
		OfflineAfterAttempts:  5,
		OfflineBackoffMs:      1800000,
		AbandonAfterDays:      180,
		AbandonSafetyGapMs:    3600000,
		AbandonBatch:          200,
		AbandonIntervalMs:     3600000,
		AuditIntervalMs:       86400000,
	}
}

// withoutLanding 把落地恢复的停放档 / 自动放弃 / 日对账全部关掉(0 = 关闭,从属参数也清零):
// config.Validate 与 assetop.NewLoop 对"关闭"的口径一致,关着时从属参数不校验。
func withoutLanding(c config.AssetOpConf) config.AssetOpConf {
	c.OfflineAfterAttempts, c.OfflineBackoffMs = 0, 0
	c.AbandonAfterDays, c.AbandonSafetyGapMs, c.AbandonBatch, c.AbandonIntervalMs = 0, 0, 0, 0
	c.AuditIntervalMs = 0
	return c
}

// ── 签名器 ────────────────────────────────────────────────────

func TestNewAssetOpSignerRequiresSecret(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, "")
	if _, err := NewAssetOpSigner(); !errors.Is(err, ErrAssetSecretMissing) {
		t.Fatalf("没设密钥必须回 ErrAssetSecretMissing,得到 %v", err)
	}
}

// 短密钥:两个哨兵都要认得出来,且错误文本不许带出密钥值(AGENTS §11.3)。
func TestNewAssetOpSignerRejectsShortSecretWithoutEchoing(t *testing.T) {
	const short = "short-guild-secret-value"
	t.Setenv(AssetOpSecretEnv, short)
	_, err := NewAssetOpSigner()
	if !errors.Is(err, ErrAssetSecretMissing) || !errors.Is(err, assetop.ErrWeakSecret) {
		t.Fatalf("短密钥必须同时是 ErrAssetSecretMissing 与 assetop.ErrWeakSecret,得到 %v", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("错误文本泄露了密钥值: %v", err)
	}
}

// 首尾空白由 assetop.NewSigner 去掉:本机脚本写出的值不带空白,但手工 export 时常带换行。
func TestNewAssetOpSignerTrimsAndAccepts(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, "  "+strings.Repeat("k", assetop.MinSecretLen)+"\n")
	signer, err := NewAssetOpSigner()
	if err != nil {
		t.Fatalf("32 字节密钥(带首尾空白)应被接受: %v", err)
	}
	if signer.Caller() != AssetOpCaller {
		t.Fatalf("Caller = %q,scene 白名单认的是 %q", signer.Caller(), AssetOpCaller)
	}
}

// ── 循环参数换算 ──────────────────────────────────────────────

// TestLoopConfigFromMapsY06:毫秒 → Duration 的换算逐项对(含落地恢复七项,天 → 24h),Streams 填了 guild 独占的两条流。
func TestLoopConfigFromMapsY06(t *testing.T) {
	cfg := LoopConfigFrom(y06Defaults(), 1500*time.Millisecond)
	if cfg.Interval != 2*time.Second || cfg.Batch != 100 || cfg.Workers != 8 ||
		cfg.Lease != 10*time.Second || cfg.OpBudget != 2500*time.Millisecond ||
		cfg.MaxBackoff != time.Minute || cfg.PoisonDelay != time.Hour || cfg.LedgerReadMinAttempts != 3 {
		t.Fatalf("换算不符: %+v", cfg)
	}
	if cfg.BaseBackoff != 1500*time.Millisecond {
		t.Fatalf("BaseBackoff = %v,应取配表传进来的 1500ms", cfg.BaseBackoff)
	}
	if cfg.AwaitDurableDelay != assetop.DefaultLoopConfig().AwaitDurableDelay {
		t.Fatalf("AwaitDurableDelay = %v,应与 assetop 默认值同源", cfg.AwaitDurableDelay)
	}
	want := []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
	}
	if len(cfg.Streams) != len(want) || cfg.Streams[0] != want[0] || cfg.Streams[1] != want[1] {
		t.Fatalf("Streams = %v,期望 %v(漏写 = 最老未决行年龄告警永远没有序列)", cfg.Streams, want)
	}

	// 落地恢复七项:天 → 24h 的整数倍,毫秒 → Duration,次数与批大小原样。放弃不可逆,AbandonAfterDays
	// 换算错一个量级(按毫秒、按小时)就是提前放弃或永不放弃,所以逐项钉死。默认值里 AbandonSafetyGap 与
	// AbandonInterval 同为 1h,两者互换看不出来,再用一组两两不同的值查一遍字段对应关系。
	distinct := y06Defaults()
	distinct.OfflineAfterAttempts = 7
	distinct.OfflineBackoffMs = 120000
	distinct.AbandonAfterDays = 45
	distinct.AbandonSafetyGapMs = 900000
	distinct.AbandonBatch = 33
	distinct.AbandonIntervalMs = 300000
	distinct.AuditIntervalMs = 7200000
	landing := []struct {
		name            string
		conf            config.AssetOpConf
		offlineAfter    uint32
		offlineBackoff  time.Duration
		abandonAfter    time.Duration
		abandonGap      time.Duration
		abandonBatch    int
		abandonInterval time.Duration
		auditInterval   time.Duration
	}{
		{"默认", y06Defaults(), 5, 30 * time.Minute, 180 * 24 * time.Hour, time.Hour, 200, time.Hour, 24 * time.Hour},
		{"两两不同", distinct, 7, 2 * time.Minute, 45 * 24 * time.Hour, 15 * time.Minute, 33, 5 * time.Minute, 2 * time.Hour},
		{"全关", withoutLanding(y06Defaults()), 0, 0, 0, 0, 0, 0, 0},
	}
	for _, tc := range landing {
		got := LoopConfigFrom(tc.conf, 1500*time.Millisecond)
		if got.OfflineAfterAttempts != tc.offlineAfter || got.OfflineBackoff != tc.offlineBackoff {
			t.Errorf("%s: 停放档换算不符 OfflineAfterAttempts=%d OfflineBackoff=%v,期望 %d / %v",
				tc.name, got.OfflineAfterAttempts, got.OfflineBackoff, tc.offlineAfter, tc.offlineBackoff)
		}
		if got.AbandonAfter != tc.abandonAfter {
			t.Errorf("%s: AbandonAfter = %v,期望 %v(AbandonAfterDays × 24h)", tc.name, got.AbandonAfter, tc.abandonAfter)
		}
		if got.AbandonSafetyGap != tc.abandonGap || got.AbandonBatch != tc.abandonBatch || got.AbandonInterval != tc.abandonInterval {
			t.Errorf("%s: 自动放弃换算不符 AbandonSafetyGap=%v AbandonBatch=%d AbandonInterval=%v,期望 %v / %d / %v",
				tc.name, got.AbandonSafetyGap, got.AbandonBatch, got.AbandonInterval, tc.abandonGap, tc.abandonBatch, tc.abandonInterval)
		}
		if got.AuditInterval != tc.auditInterval {
			t.Errorf("%s: AuditInterval = %v,期望 %v", tc.name, got.AuditInterval, tc.auditInterval)
		}
	}
	// guild 的 yaml 默认值与 assetop 的默认值是同一组数(设计 §6),两边分叉时这里报出来。
	def := assetop.DefaultLoopConfig()
	if cfg.OfflineAfterAttempts != def.OfflineAfterAttempts || cfg.OfflineBackoff != def.OfflineBackoff ||
		cfg.AbandonAfter != def.AbandonAfter || cfg.AbandonSafetyGap != def.AbandonSafetyGap ||
		cfg.AbandonBatch != def.AbandonBatch || cfg.AbandonInterval != def.AbandonInterval || cfg.AuditInterval != def.AuditInterval {
		t.Errorf("落地恢复默认值与 assetop.DefaultLoopConfig 不一致: guild=%+v assetop=%+v", cfg, def)
	}
}

// TestLoopConfigFromSatisfiesNewLoop:config.Validate 允许的边界值(见 config 包的区间常量)
// 换算后必须也能过 assetop.NewLoop,否则会出现"配置校验放过、启动时才炸"。
// 退避基数取配表的合法下界 100ms,不超过任何合法的 MaxBackoffMs(≥ 1000)。
//
// 落地恢复七项同样取 config 允许的端点(config.TestAssetOpLandingBounds 的合法端点):
//   - 下界:停放次数 1、停放间隔 = MaxBackoffMs、放弃门槛 30 天、安全间隔 10 分钟(≥ LeaseMs)、批 1、
//     放弃 / 对账间隔 1 分钟;
//   - 上界:停放次数 1000、停放间隔 24h、放弃门槛 3650 天、安全间隔恰好等于 LeaseMs(config 没有上限,
//     约束是 ≥ LeaseMs)、批 1000、放弃间隔 24h、对账间隔 7 天;
//   - 全关:三项都写 0,从属参数清零 —— 两边对"关闭时不校验从属参数"的口径必须一致。
func TestLoopConfigFromSatisfiesNewLoop(t *testing.T) {
	lower := config.AssetOpConf{
		Enabled: true, ReconcileIntervalMs: 200, ReconcileBatch: 1, Workers: 1,
		LeaseMs: 3000, OpBudgetMs: 1000, MaxBackoffMs: 1000, PoisonDelayMs: 60000, LedgerReadMinAttempts: 1,
	}
	lower.OfflineAfterAttempts, lower.OfflineBackoffMs = 1, 1000
	lower.AbandonAfterDays, lower.AbandonSafetyGapMs, lower.AbandonBatch, lower.AbandonIntervalMs = 30, 600000, 1, 60000
	lower.AuditIntervalMs = 60000

	upper := config.AssetOpConf{
		Enabled: true, ReconcileIntervalMs: 60000, ReconcileBatch: 1000, Workers: 64,
		LeaseMs: 600000, OpBudgetMs: 10000, MaxBackoffMs: 600000, PoisonDelayMs: 86400000, LedgerReadMinAttempts: 3,
	}
	upper.OfflineAfterAttempts, upper.OfflineBackoffMs = 1000, 86400000
	upper.AbandonAfterDays, upper.AbandonSafetyGapMs, upper.AbandonBatch, upper.AbandonIntervalMs = 3650, 600000, 1000, 86400000
	upper.AuditIntervalMs = 7 * 86400000
	// 不进 LoopConfig,写出来只为让这一份确实是 config.Validate 放过的配置:唤醒超时须 ≥ 2 × OpBudgetMs。
	upper.Wake.TimeoutMs = 20000

	cases := map[string]config.AssetOpConf{
		"默认":     y06Defaults(),
		"下界":     lower,
		"上界":     upper,
		"落地恢复全关": withoutLanding(y06Defaults()),
	}
	for name, c := range cases {
		if _, err := assetop.NewLoop(LoopConfigFrom(c, 100*time.Millisecond), stubAssetStore{}, stubApplier{}, nil, nil); err != nil {
			t.Errorf("%s配置应能构造循环: %v", name, err)
		}
	}
}

// 配表退避基数大于 MaxBackoffMs 时 NewLoop 拒绝 —— guild 启动在这里 fail-fast,钉住这条行为。
func TestLoopConfigFromRejectsBaseAboveMax(t *testing.T) {
	c := y06Defaults()
	c.MaxBackoffMs = 1000
	if _, err := assetop.NewLoop(LoopConfigFrom(c, 2*time.Second), stubAssetStore{}, stubApplier{}, nil, nil); err == nil {
		t.Fatal("退避基数 2s > MaxBackoff 1s 时 NewLoop 必须拒绝")
	}
}

func TestGuildAssetStreamsAreGuildOwnedAndFresh(t *testing.T) {
	a := GuildAssetStreams()
	for _, s := range a {
		if _, ok := assetop.ApplyRPCOf(s); !ok {
			t.Fatalf("流 %v 没有投递方向", s)
		}
	}
	a[0] = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	if GuildAssetStreams()[0] != assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT {
		t.Fatal("GuildAssetStreams 必须每次返回新切片")
	}
}

func TestCleanupConfFrom(t *testing.T) {
	c := CleanupConfFrom(config.AssetOpConf{CleanupIntervalMinutes: 10, TerminalRetentionDays: 30, CounterRetentionDays: 7})
	if c.Interval != 10*time.Minute || c.TerminalRetention != 30*24*time.Hour || c.CounterRetention != 7*24*time.Hour {
		t.Fatalf("清理参数换算不符: %+v", c)
	}
}

// TestRollbackProofRetention:AssetOp 段缺失(字段全零)时回档检查用默认保留期而不是 0 ——
// 0 会让 GuildInternal 恒回 Unavailable,所有回档被拒且放行无效;配了值的原样使用。
func TestRollbackProofRetention(t *testing.T) {
	const day = 24 * time.Hour
	want := time.Duration(config.DefaultTerminalRetentionDays) * day
	if got := RollbackProofRetention(config.AssetOpConf{}); got != want {
		t.Fatalf("AssetOp 段缺失: 保留期 = %v, 期望默认 %v", got, want)
	}
	if got := RollbackProofRetention(config.AssetOpConf{TerminalRetentionDays: -3}); got != want {
		t.Fatalf("非正值: 保留期 = %v, 期望默认 %v", got, want)
	}
	if got := RollbackProofRetention(config.AssetOpConf{TerminalRetentionDays: 60}); got != 60*day {
		t.Fatalf("配了 60 天: 保留期 = %v", got)
	}
	// 与清理用的换算一致:配了值时两处给出同一个保留期。
	conf := config.AssetOpConf{CleanupIntervalMinutes: 10, TerminalRetentionDays: 45, CounterRetentionDays: 30}
	if got := RollbackProofRetention(conf); got != CleanupConfFrom(conf).TerminalRetention {
		t.Fatalf("回档检查保留期 %v 与清理保留期 %v 不一致", got, CleanupConfFrom(conf).TerminalRetention)
	}
}

// ── 装配与启停 ────────────────────────────────────────────────

func TestAssetOpIDBizTagIsDistinct(t *testing.T) {
	if AssetOpBizTag == guildIDBizTag {
		t.Fatal("op_id 与 guild_id 不能共用号段业务键")
	}
}

// 两次取指标必须是同一组句柄:promauto 重复注册同名指标会 panic。
func TestAssetChannelMetricsIsSingleton(t *testing.T) {
	a1, n1 := assetChannelMetrics()
	a2, n2 := assetChannelMetrics()
	if a1 == nil || n1 == nil || a1 != a2 || n1 != n2 {
		t.Fatal("assetChannelMetrics 必须只建一次")
	}
}

func TestNewAssetPipelineRejectsIncompleteDeps(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, strings.Repeat("s", assetop.MinSecretLen))
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	if _, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, rdb, nil); err == nil {
		t.Fatal("缺 Store 必须报错")
	}
	if _, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, nil, stubAssetStore{}); err == nil {
		t.Fatal("缺唤醒频道 Redis 必须报错")
	}
	if _, err := NewAssetPipeline(y06Defaults(), time.Second, nil, rdb, stubAssetStore{}); err == nil {
		t.Fatal("缺位置键 Redis 必须报错")
	}

	t.Setenv(AssetOpSecretEnv, "")
	if _, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, rdb, stubAssetStore{}); !errors.Is(err, ErrAssetSecretMissing) {
		t.Fatalf("缺密钥必须回 ErrAssetSecretMissing(资产路径 fail-closed),得到 %v", err)
	}
}

func TestNilAssetPipelineIsSafe(t *testing.T) {
	var p *AssetPipeline
	p.AttachMaintenanceLeader(nil, "")
	p.Start(context.Background(), nil)
	p.Stop()
}

// TestAssetPipelineStopWaitsAndIsIdempotent:Stop 必须等两个 goroutine 退出才返回(guild.go 靠它
// 保证先停循环再关 DB / etcd),重复 Stop 无害,Stop 之后的 Start 是空操作。
// etcd 传 nil:watcher 打 ERROR 后立即返回(fail-closed),循环照跑,Stop 取消它。
func TestAssetPipelineStopWaitsAndIsIdempotent(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, strings.Repeat("s", assetop.MinSecretLen))
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	p, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, rdb, stubAssetStore{})
	if err != nil {
		t.Fatalf("NewAssetPipeline: %v", err)
	}
	if p.Loop == nil {
		t.Fatal("Loop 必须已构造(logic 的同步投递要用它)")
	}
	p.Start(context.Background(), nil)
	p.Start(context.Background(), nil) // 重复 Start 只生效一次

	done := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 5s 内没有返回:后台 goroutine 没有随取消退出")
	}
	p.Start(context.Background(), nil) // Stop 之后再 Start 是空操作,不能在已 Wait 过的 WaitGroup 上 Add
}

// hasMaintenanceElector 在 mu 下读选主器是否装上:Start 之后后台 goroutine 已在跑,读字段也走与写同一把锁。
func hasMaintenanceElector(p *AssetPipeline) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maintenance != nil
}

// TestAttachMaintenanceLeaderBranches:"日对账 + 自动放弃"的选主只在「Start 之前、配置开了对账或放弃、有 Redis」时装上;
// 其余三条分支都不竞选(maintenance 保持 nil,Start 不起 guild.asset_op_maintenance)。
// 第一个子用例是对照组:证明 y06Defaults 下确实会装上,后面三条"没装上"不是因为别的原因。
// 选主器构造时不连 Redis(leader.New 只存参数),所以指向一个不可达地址即可;唤醒订阅关掉,本用例只测选主。
func TestAttachMaintenanceLeaderBranches(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, strings.Repeat("s", assetop.MinSecretLen))
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	newPipeline := func(t *testing.T, c config.AssetOpConf) *AssetPipeline {
		t.Helper()
		wakeOff := false
		c.Wake.Enabled = &wakeOff
		p, err := NewAssetPipeline(c, time.Second, rdb, rdb, stubAssetStore{})
		if err != nil {
			t.Fatalf("NewAssetPipeline: %v", err)
		}
		t.Cleanup(p.Stop) // Stop 幂等:子用例里已经 Stop 过也无妨
		return p
	}

	t.Run("对照:Start 之前且开了维护时装上", func(t *testing.T) {
		p := newPipeline(t, y06Defaults())
		p.AttachMaintenanceLeader(rdb, "node-a")
		if !hasMaintenanceElector(p) {
			t.Fatal("对账与放弃开着、Start 之前、有 Redis:必须装上选主器")
		}
	})

	t.Run("Start 之后与 Stop 之后调用被忽略", func(t *testing.T) {
		p := newPipeline(t, y06Defaults())
		p.Start(context.Background(), nil)
		p.AttachMaintenanceLeader(rdb, "node-a")
		if hasMaintenanceElector(p) {
			t.Fatal("Start 之后再装选主器就是与后台 goroutine 的数据竞争,必须忽略")
		}
		p.Stop()
		p.AttachMaintenanceLeader(rdb, "node-a")
		if hasMaintenanceElector(p) {
			t.Fatal("Stop 之后装选主器必须忽略")
		}
	})

	t.Run("rdb 为 nil 不竞选", func(t *testing.T) {
		p := newPipeline(t, y06Defaults())
		p.AttachMaintenanceLeader(nil, "node-a")
		if hasMaintenanceElector(p) {
			t.Fatal("没有 Redis 时不得装上选主器(维护不跑,fail-closed)")
		}
	})

	t.Run("对账与放弃都关时不竞选", func(t *testing.T) {
		c := y06Defaults()
		c.AuditIntervalMs, c.AbandonAfterDays = 0, 0
		p := newPipeline(t, c)
		p.AttachMaintenanceLeader(rdb, "node-a")
		if hasMaintenanceElector(p) {
			t.Fatal("AuditIntervalMs=0 且 AbandonAfterDays=0 时不得竞选")
		}
	})
}

// 号段关闭(或没有 data_service 客户端)时 op_id 号段保持 nil,guild.go 据此把 OpIDs 留成 nil 接口。
func TestInitAssetOpIDSegmentStaysNilWhenUnavailable(t *testing.T) {
	s := &ServiceContext{}
	s.initAssetOpIDSegment()
	if s.AssetOpIDSegment != nil {
		t.Fatal("IdSegment.Enabled=false 时 op_id 号段必须为 nil")
	}

	s = &ServiceContext{}
	s.Config.IdSegment.Enabled = true
	s.initAssetOpIDSegment()
	if s.AssetOpIDSegment != nil {
		t.Fatal("没有 data_service 客户端时 op_id 号段必须为 nil")
	}
	s.WarmAssetOpIDSegment() // nil 号段上 Warm 是空操作,不能 panic
}
