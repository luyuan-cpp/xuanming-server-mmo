package svc

// 落地恢复(docs/design/landing-recovery-sync-digest.md §6.4)在 trade 装配层的回归,不连 Redis / MySQL / etcd:
// 配置 → reconcile.Recovery / assetopwake.Config / data.CleanupConf 的换算,离线读账本的挂载形态,
// 以及终态行清理与通道生命周期的启停。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"trade/internal/config"
	"trade/internal/data"
	"trade/internal/reconcile"

	dspb "proto/data_service"

	"shared/assetop"
	"shared/assetopwake"
	"shared/scenenode"
)

// recoveryDefaults 是 AssetOp 段落地恢复与清理键的默认值(与 etc/trade.yaml、guild 的同名键同值)。
func recoveryDefaults() config.AssetOpConf {
	return config.AssetOpConf{
		Enabled:                true,
		SecretEnv:              AssetOpSecretEnv,
		OfflineAfterAttempts:   5,
		OfflineBackoffMs:       1800000,
		AbandonAfterDays:       180,
		AbandonSafetyGapMs:     3600000,
		AbandonBatch:           200,
		AbandonIntervalMs:      3600000,
		AuditIntervalMs:        86400000,
		CleanupEnabled:         true,
		CleanupIntervalMinutes: 10,
		TerminalRetentionDays:  30,
	}
}

// fakeOpIDs 是假号段,只为满足 reconcile.Deps.OpIDs;本文件的用例不发号。
type fakeOpIDs struct{}

func (fakeOpIDs) Next(context.Context) (uint64, error) { return 1, nil }

// TestRecoveryFromMapsEveryKey:七个落地恢复键逐项换算(毫秒 → Duration、天 → Duration)。
// 漏一项的表现是"配置改了没反应",且全程零报错。
func TestRecoveryFromMapsEveryKey(t *testing.T) {
	c := config.AssetOpConf{
		OfflineAfterAttempts: 7,
		OfflineBackoffMs:     2700000,
		AbandonAfterDays:     90,
		AbandonSafetyGapMs:   7200000,
		AbandonBatch:         150,
		AbandonIntervalMs:    1800000,
		AuditIntervalMs:      43200000,
	}
	got := recoveryFrom(c)
	want := reconcile.Recovery{
		OfflineAfterAttempts: 7,
		OfflineBackoff:       45 * time.Minute,
		AbandonAfter:         90 * 24 * time.Hour,
		AbandonSafetyGap:     2 * time.Hour,
		AbandonBatch:         150,
		AbandonInterval:      30 * time.Minute,
		AuditInterval:        12 * time.Hour,
	}
	if got != want {
		t.Errorf("recoveryFrom = %+v\nwant          %+v", got, want)
	}
	if zero := recoveryFrom(config.AssetOpConf{}); zero != (reconcile.Recovery{}) {
		t.Errorf("全 0 的配置必须换算成全部关闭,得到 %+v", zero)
	}
}

// TestRecoveryFromDefaultsMatchAssetopAndPassNewLoop:默认配置换算出的 Recovery 必须等于 assetop.DefaultLoopConfig
// 的同名项,并被 reconcile.New(内部 assetop.NewLoop)接受 —— 否则把通道打开的那一刻服务就起不来。
func TestRecoveryFromDefaultsMatchAssetopAndPassNewLoop(t *testing.T) {
	r := recoveryFrom(recoveryDefaults())
	d := assetop.DefaultLoopConfig()
	if r.OfflineAfterAttempts != d.OfflineAfterAttempts || r.OfflineBackoff != d.OfflineBackoff ||
		r.AbandonAfter != d.AbandonAfter || r.AbandonSafetyGap != d.AbandonSafetyGap ||
		r.AbandonBatch != d.AbandonBatch || r.AbandonInterval != d.AbandonInterval || r.AuditInterval != d.AuditInterval {
		t.Errorf("默认配置换算 = %+v,与 assetop.DefaultLoopConfig 不一致: %+v", r, d)
	}

	repo, err := data.NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	signer, err := assetop.NewSigner(AssetOpCaller, testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	p, err := reconcile.New(reconcile.Deps{
		Ops:      repo,
		Caller:   &assetop.Caller{Signer: signer},
		OpIDs:    fakeOpIDs{},
		Ledger:   persistedLedger(dspb.NewDataServiceClient(nil)),
		Recovery: r,
	})
	if err != nil {
		t.Fatalf("默认落地恢复参数被 reconcile.New 拒绝: %v", err)
	}
	if !p.MaintenanceEnabled() {
		t.Error("默认配置开着对账与放弃,装配层应为它竞选领导权")
	}
}

// TestWakeConfigFrom:没写的项留给 assetopwake 取默认值之前先取生效值(与 guild 同口径);
// Service 必须是本服务的调用方名 —— 它进跨副本去重键 assetop:wake:trade:{player_id},与 guild 互不去重。
func TestWakeConfigFrom(t *testing.T) {
	got := wakeConfigFrom(config.AssetOpWakeConf{})
	if got.Service != AssetOpCaller {
		t.Errorf("Service = %q, want %q", got.Service, AssetOpCaller)
	}
	if got.Concurrency != assetopwake.DefaultConcurrency || got.WakeTimeout != assetopwake.DefaultWakeTimeout {
		t.Errorf("没写时 = (%d, %v), want 默认 (%d, %v)", got.Concurrency, got.WakeTimeout,
			assetopwake.DefaultConcurrency, assetopwake.DefaultWakeTimeout)
	}
	if got.DedupTTL != 0 {
		t.Errorf("DedupTTL = %v:去重窗口不开放配置,留 0 让 assetopwake 取默认值", got.DedupTTL)
	}
	explicit := wakeConfigFrom(config.AssetOpWakeConf{Concurrency: 9, TimeoutMs: 20000})
	if explicit.Concurrency != 9 || explicit.WakeTimeout != 20*time.Second {
		t.Errorf("显式值 = (%d, %v), want (9, 20s)", explicit.Concurrency, explicit.WakeTimeout)
	}
	if !strings.HasPrefix(assetopwake.DedupKey(got.Service, 42), "assetop:wake:trade:") {
		t.Errorf("去重键 = %q,必须按服务名区分", assetopwake.DedupKey(got.Service, 42))
	}
}

// TestCleanupConfFrom:清理键与 guild 同名,分钟 / 天换算成 Duration。
func TestCleanupConfFrom(t *testing.T) {
	got := cleanupConfFrom(recoveryDefaults())
	if got.Interval != 10*time.Minute || got.TerminalRetention != 30*24*time.Hour {
		t.Errorf("cleanupConfFrom = %+v, want {10m, 720h}", got)
	}
}

// TestPersistedLedger:有 data_service 客户端时挂 DataServiceLedger(单次上限与 guild 同值);
// 没有时必须回 **nil 接口** —— 装着 nil 指针的接口会骗过 reconcile.New 的"非 nil 才挂"判断。
func TestPersistedLedger(t *testing.T) {
	if l := persistedLedger(nil); l != nil {
		t.Errorf("没有客户端时应回 nil 接口,得到 %#v", l)
	}
	l := persistedLedger(dspb.NewDataServiceClient(nil))
	ds, ok := l.(*assetop.DataServiceLedger)
	if !ok || ds == nil {
		t.Fatalf("有客户端时应回 *assetop.DataServiceLedger,得到 %#v", l)
	}
	if ds.Client == nil || ds.Timeout != persistedLedgerReadTimeout {
		t.Errorf("DataServiceLedger = %+v, want 带客户端、单次上限 %v", ds, persistedLedgerReadTimeout)
	}
}

// TestMaintenanceLeaderKeyAndID:选主锁键带服务前缀(与 guild 的 guild:assetop:maintenance:leader 互不相干),
// 竞选者标识非空(只进锁值与日志)。
func TestMaintenanceLeaderKeyAndID(t *testing.T) {
	if !strings.HasPrefix(maintenanceLeaderKey, "trade:") {
		t.Errorf("maintenanceLeaderKey = %q,必须以 trade: 打头", maintenanceLeaderKey)
	}
	if maintenanceLeaderID() == "" {
		t.Error("maintenanceLeaderID 不得为空")
	}
}

// TestStartAssetOpCleanupRejectsNilDB:没有连接池时拒绝装配(调用方据此拒启),而不是起一个必然 panic 的 goroutine。
func TestStartAssetOpCleanupRejectsNilDB(t *testing.T) {
	if stop, err := StartAssetOpCleanup(nil, recoveryDefaults()); err == nil || stop != nil {
		t.Errorf("nil 连接池 = (stop!=nil:%v, %v), want 报错且不返回 stop", stop != nil, err)
	}
}

// TestStartAssetOpCleanupStopWaits:stop 取消清理并等 goroutine 退出 —— 清理语句跑在 MySQL 上,必须在关库之前停下。
// sql.Open 不建连;间隔取最大值,随机初始延迟在本用例里不会到点,于是不会真去连库。
func TestStartAssetOpCleanupStopWaits(t *testing.T) {
	db, err := sql.Open("mysql", "user:pw@tcp(127.0.0.1:1)/"+data.DatabaseName)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := recoveryDefaults()
	c.CleanupIntervalMinutes = 1440
	stop, err := StartAssetOpCleanup(db, c)
	if err != nil {
		t.Fatalf("StartAssetOpCleanup: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop 没有在取消后返回")
	}
}

// TestAssetChannelStartThenCloseReturns:Start 起的节点镜像与重投循环都记进 wg,Close 取消之后等它们退出才去关连接
// (与 guild 的 AssetPipeline.Stop 同序)。每个 wg.Add 都必须有对应的 Done —— 漏一个,Close 就永远等下去,这里限时判红;
// Pipeline.Run 若不阻塞(自己另起 goroutine 立刻返回),wg 会等了个空,那一条由 reconcile 的 TestRunBlocksUntilCancel 守住。
// etcd 客户端为 nil 时镜像打 ERROR 后直接返回;重投循环第一轮要等满 Interval(2s),本用例在那之前就关,不会碰没有连接池的仓库。
func TestAssetChannelStartThenCloseReturns(t *testing.T) {
	repo, err := data.NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	signer, err := assetop.NewSigner(AssetOpCaller, testSecret32)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	p, err := reconcile.New(reconcile.Deps{Ops: repo, Caller: &assetop.Caller{Signer: signer}, OpIDs: fakeOpIDs{},
		Ledger: persistedLedger(dspb.NewDataServiceClient(nil))})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	ch := &AssetChannel{
		Conns:    scenenode.NewConnCache(),
		Watcher:  scenenode.NewWatcher(sceneWatcherName, scenenode.SceneNodeRpcPrefix, nil, nil),
		Pipeline: p,
	}
	ch.Start(context.Background(), nil)
	if ch.stopLoops == nil {
		t.Fatal("Start 应当起了后台 goroutine")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ch.Close()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 没有返回:Start 里有 wg.Add 没配上 Done,或某个后台 goroutine 不响应取消")
	}
	ch.Close() // 幂等
}

// TestAssetChannelStartAfterCloseIsNoop:Close 之后的 Start 是空操作 —— 否则会在 wg.Wait 之后再 wg.Add,
// 或在已关掉的连接上起新的后台 goroutine。用 stopLoops 是否被设置来判断 Start 有没有真的起东西。
func TestAssetChannelStartAfterCloseIsNoop(t *testing.T) {
	ch := &AssetChannel{}
	ch.Close()
	ch.Start(context.Background(), nil)
	if ch.stopLoops != nil {
		t.Error("Close 之后 Start 不得再起任何后台 goroutine")
	}
	ch.Close() // 幂等
}
