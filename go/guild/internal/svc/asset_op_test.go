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

// y06Defaults 是 90-consistency Y-06 的默认值(与 etc/guild.yaml 一致)。
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
	}
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

// TestLoopConfigFromMapsY06:毫秒 → Duration 的换算逐项对,Streams 填了 guild 独占的两条流。
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
}

// TestLoopConfigFromSatisfiesNewLoop:config.Validate 允许的边界值(见 config 包的区间常量)
// 换算后必须也能过 assetop.NewLoop,否则会出现"配置校验放过、启动时才炸"。
// 退避基数取配表的合法下界 100ms,不超过任何合法的 MaxBackoffMs(≥ 1000)。
func TestLoopConfigFromSatisfiesNewLoop(t *testing.T) {
	lower := config.AssetOpConf{
		Enabled: true, ReconcileIntervalMs: 200, ReconcileBatch: 1, Workers: 1,
		LeaseMs: 3000, OpBudgetMs: 1000, MaxBackoffMs: 1000, PoisonDelayMs: 60000, LedgerReadMinAttempts: 1,
	}
	upper := config.AssetOpConf{
		Enabled: true, ReconcileIntervalMs: 60000, ReconcileBatch: 1000, Workers: 64,
		LeaseMs: 600000, OpBudgetMs: 10000, MaxBackoffMs: 600000, PoisonDelayMs: 86400000, LedgerReadMinAttempts: 3,
	}
	for name, c := range map[string]config.AssetOpConf{"默认": y06Defaults(), "下界": lower, "上界": upper} {
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

	if _, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, nil); err == nil {
		t.Fatal("缺 Store 必须报错")
	}
	if _, err := NewAssetPipeline(y06Defaults(), time.Second, nil, stubAssetStore{}); err == nil {
		t.Fatal("缺位置键 Redis 必须报错")
	}

	t.Setenv(AssetOpSecretEnv, "")
	if _, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, stubAssetStore{}); !errors.Is(err, ErrAssetSecretMissing) {
		t.Fatalf("缺密钥必须回 ErrAssetSecretMissing(资产路径 fail-closed),得到 %v", err)
	}
}

func TestNilAssetPipelineIsSafe(t *testing.T) {
	var p *AssetPipeline
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

	p, err := NewAssetPipeline(y06Defaults(), time.Second, rdb, stubAssetStore{})
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
