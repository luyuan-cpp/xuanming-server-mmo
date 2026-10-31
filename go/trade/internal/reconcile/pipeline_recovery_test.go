package reconcile

// 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6.4)在 trade 管线上的接线回归,不连库、不发 RPC。
// 停放 / 对账 / 放弃 / 唤醒的语义本身在 shared/assetop 的单测里;这里只钉"trade 有没有把它们接上、接对"。

import (
	"context"
	"errors"
	"testing"
	"time"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"

	"shared/assetop"
	"shared/assetopwake"
)

// 编译期:Pipeline 必须满足唤醒订阅的落点接口,svc 才能把它直接交给 assetopwake.New。
var _ assetopwake.Waker = (*Pipeline)(nil)

// fakeLedger 是假的已落盘账本读取器;本文件只关心它有没有被挂上,不会真被调用。
type fakeLedger struct{}

func (fakeLedger) ReadPersistedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return nil, errors.New("fakeLedger 不该被调用")
}

// fakeResolver 是假的位置解析器;同样只验证挂载。
type fakeResolver struct{ assetop.Resolver }

// defaultRecovery 按 assetop.DefaultLoopConfig() 拼出 Recovery —— 配置层的默认值与它同值(config_test.go 守住)。
func defaultRecovery() Recovery {
	d := assetop.DefaultLoopConfig()
	return Recovery{
		OfflineAfterAttempts: d.OfflineAfterAttempts,
		OfflineBackoff:       d.OfflineBackoff,
		AbandonAfter:         d.AbandonAfter,
		AbandonSafetyGap:     d.AbandonSafetyGap,
		AbandonBatch:         d.AbandonBatch,
		AbandonInterval:      d.AbandonInterval,
		AuditInterval:        d.AuditInterval,
	}
}

// TestLoopConfigCarriesRecovery:Recovery 的七项必须原样进 LoopConfig,且不改动正确性证明那一组常量。
// 漏传一项的表现是"配置改了没反应":运维以为开了停放档,实际每 60 秒照投一次离线行,全程零报错。
func TestLoopConfigCarriesRecovery(t *testing.T) {
	r := Recovery{
		OfflineAfterAttempts: 7,
		OfflineBackoff:       45 * time.Minute,
		AbandonAfter:         90 * 24 * time.Hour,
		AbandonSafetyGap:     2 * time.Hour,
		AbandonBatch:         150,
		AbandonInterval:      30 * time.Minute,
		AuditInterval:        12 * time.Hour,
	}
	cfg := loopConfig(r)
	if cfg.OfflineAfterAttempts != r.OfflineAfterAttempts || cfg.OfflineBackoff != r.OfflineBackoff ||
		cfg.AbandonAfter != r.AbandonAfter || cfg.AbandonSafetyGap != r.AbandonSafetyGap ||
		cfg.AbandonBatch != r.AbandonBatch || cfg.AbandonInterval != r.AbandonInterval ||
		cfg.AuditInterval != r.AuditInterval {
		t.Errorf("loopConfig 没有原样带上 Recovery:\n  got  %+v\n  want %+v", cfg, r)
	}
	if cfg.Lease != loopLease || cfg.OpBudget != loopOpBudget || cfg.MaxBackoff != loopMaxBackoff ||
		cfg.Workers != loopWorkers || cfg.PoisonDelay != loopPoisonDelay {
		t.Errorf("Recovery 不得改动循环的正确性常量: %+v", cfg)
	}
	if len(cfg.Streams) != 2 {
		t.Errorf("Streams = %v:日对账按它逐流置值,漏写则按类 gauge 没有序列", cfg.Streams)
	}
	if zero := loopConfig(Recovery{}); zero.OfflineAfterAttempts != 0 || zero.AbandonAfter != 0 || zero.AuditInterval != 0 {
		t.Errorf("零值 Recovery 必须等于全部关闭: %+v", zero)
	}
}

// TestNewAcceptsDefaultRecovery:配置默认值(停放 5 次 / 30 分钟、放弃 180 天 / 安全间隔 1 小时 / 每轮 200 /
// 每小时、对账 24 小时)必须被 assetop.NewLoop 接受 —— 尤其要与 trade 自己的 Lease(10s)、MaxBackoff(60s)
// 常量相容(安全间隔 ≥ Lease、停放间隔 ≥ MaxBackoff)。不相容的表现是开了通道就起不来。
func TestNewAcceptsDefaultRecovery(t *testing.T) {
	p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Recovery: defaultRecovery()})
	if err != nil {
		t.Fatalf("默认落地恢复参数被拒: %v", err)
	}
	if !p.MaintenanceEnabled() {
		t.Error("默认参数开着对账与放弃,MaintenanceEnabled 必须为真")
	}
}

// TestNewRejectsRecoveryBeyondAssetopBounds:放弃不可逆,门槛 / 安全间隔越过 assetop 的硬边界必须拒绝构造
// (config.Validate 先拦一道,这里是装配期兜底)。
func TestNewRejectsRecoveryBeyondAssetopBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Recovery)
	}{
		{"放弃门槛不足 30 天", func(r *Recovery) { r.AbandonAfter = 24 * time.Hour }},
		{"安全间隔短于租约", func(r *Recovery) { r.AbandonSafetyGap = loopLease - time.Second }},
		{"停放间隔短于退避封顶", func(r *Recovery) { r.OfflineBackoff = loopMaxBackoff - time.Second }},
		{"放弃开着但单轮行数为 0", func(r *Recovery) { r.AbandonBatch = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := defaultRecovery()
			tc.mutate(&r)
			if _, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Recovery: r}); err == nil {
				t.Fatalf("越界的 Recovery %+v 必须被拒绝", r)
			}
		})
	}
}

// TestNewAttachesLedgerOnlyWhenGiven:Ledger 是自动放弃的唯一证据来源;给了就必须挂上,没给就保持 nil
// (assetop 据此 fail-closed、不运行放弃)。
func TestNewAttachesLedgerOnlyWhenGiven(t *testing.T) {
	bare := newTestPipeline(t, &fakeIDs{})
	if bare.loop.Ledger != nil {
		t.Errorf("没给 Ledger 时必须保持 nil,得到 Ledger=%v", bare.loop.Ledger)
	}

	ledger := fakeLedger{}
	p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Ledger: ledger})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.loop.Ledger != assetop.LedgerReader(ledger) {
		t.Error("给了 Ledger 却没挂到 Loop 上:离线读账本与自动放弃都不会运行")
	}
	if p.loop.Manual == nil {
		t.Error("挂落地恢复依赖时不得丢掉人工终结通道")
	}
}

// TestNewRequireSQLCommitNeedsSQLLedger:资产 SQL 提交证据开关打开时,没有能读证据的 Ledger 必须拒绝构造
// (否则每笔终结都静默停在 AwaitDurable,fence-inflight.md F6 (b));开关原样进循环。关闭时 Ledger 照旧可选。
func TestNewRequireSQLCommitNeedsSQLLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger assetop.LedgerReader
		ok     bool
	}{
		{"没挂 Ledger", nil, false},
		{"只会读父键 blob", fakeLedger{}, false},
		{"DataServiceLedger", &assetop.DataServiceLedger{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Ledger: tc.ledger, RequireSQLCommit: true})
			if (err == nil) != tc.ok {
				t.Fatalf("New err=%v,期望通过=%t", err, tc.ok)
			}
			if tc.ok && !p.loop.RequireSQLCommit() {
				t.Fatal("Deps.RequireSQLCommit 必须原样进 assetop.LoopConfig")
			}
		})
	}
	if p := newTestPipeline(t, &fakeIDs{}); p.loop.RequireSQLCommit() {
		t.Fatal("开关缺省必须关闭(HEAD 口径)")
	}
}

// TestMaintenanceEnabled:对账或放弃至少开一个才需要竞选领导权;都关着时装配层不竞选(省一把锁)。
func TestMaintenanceEnabled(t *testing.T) {
	cases := []struct {
		name string
		r    Recovery
		want bool
	}{
		{"全关", Recovery{}, false},
		{"只开停放档", Recovery{OfflineAfterAttempts: 5, OfflineBackoff: 30 * time.Minute}, false},
		{"只开对账", Recovery{AuditInterval: time.Hour}, true},
		{"只开放弃", func() Recovery { r := defaultRecovery(); r.AuditInterval = 0; return r }(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Recovery: tc.r})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := p.MaintenanceEnabled(); got != tc.want {
				t.Errorf("MaintenanceEnabled = %v, want %v", got, tc.want)
			}
		})
	}
	var nilPipe *Pipeline
	if nilPipe.MaintenanceEnabled() {
		t.Error("nil 管线不能报告维护已开启")
	}
}

// TestNilPipelineRecoveryEntrypoints:唤醒与维护入口同样要能在 nil 管线上空调用(同 EnqueueEscrowDebit 的口径)。
func TestNilPipelineRecoveryEntrypoints(t *testing.T) {
	var p *Pipeline
	if n, err := p.WakePlayer(context.Background(), 42); n != 0 || !errors.Is(err, ErrSignerMissing) {
		t.Errorf("nil 管线 WakePlayer = (%d, %v), want (0, ErrSignerMissing)", n, err)
	}
	p.RunMaintenance(context.Background()) // 不得 panic,也不得阻塞
}

// TestRunMaintenanceReturnsOnCancel:RunMaintenance 阻塞到 ctx 结束;svc 以领导权 ctx 调它,卸任即返回、释放选主锁。
// 只开对账、而仓库没有连接池时,首轮对账在 assetop 里各自 recover,不影响"ctx 一取消就返回"这件事。
func TestRunMaintenanceReturnsOnCancel(t *testing.T) {
	p := newTestPipeline(t, &fakeIDs{}) // Recovery 零值:对账与放弃都关,只剩等 ctx
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunMaintenance(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 RunMaintenance 没有返回")
	}
}

// TestRunBlocksUntilCancel:Run 阻塞到 ctx 结束才返回 —— svc.AssetChannel 把它记进 WaitGroup,Close 在 wg.Wait() 之后才关
// scene 连接、共享 Redis 与 MySQL;Run 若自己另起 goroutine 立刻返回,wg 就等了个空,停机时落库会撞上已关的连接。
// 仓库没有连接池,但循环第一轮要等满 Interval(2s)才开始,本用例在那之前就取消,不会碰库。
func TestRunBlocksUntilCancel(t *testing.T) {
	p := newTestPipeline(t, &fakeIDs{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	// 只能证"没有提前返回"的反例:这里等不到 done 说明 Run 仍在阻塞;等到了就是 Run 没把循环跑在自己身上。
	select {
	case <-done:
		t.Fatal("ctx 未取消 Run 就返回了:svc 的 WaitGroup 将等不到真正的重投循环")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Run 没有返回")
	}
}

// TestNewEscrowRecordWritesLastSendMs:LastSendMs 契约第 1 条 —— 插行即持租约、提交后的同步投递紧接着就可能发出,
// 所以新行的 last_send_ms 必须不小于 created_ms(这里取同一时刻)。写 0 会让这一行在第一次被领取之前没有任何安全依据。
func TestNewEscrowRecordWritesLastSendMs(t *testing.T) {
	fixed := time.UnixMilli(1_760_000_000_000)
	p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := p.newEscrowRecord(EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: validDebitBundle()},
		1, assetop.Alloc{Epoch: 1, Seq: 1}, nil)
	if rec.GetLastSendMs() == 0 || rec.GetLastSendMs() < rec.GetCreatedMs() {
		t.Errorf("last_send_ms = %d, created_ms = %d:新行的 last_send_ms 必须不小于 created_ms",
			rec.GetLastSendMs(), rec.GetCreatedMs())
	}
	if rec.GetStream() != uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT) {
		t.Errorf("托管行的流 = %d, want TRADE_DEBIT", rec.GetStream())
	}
}
