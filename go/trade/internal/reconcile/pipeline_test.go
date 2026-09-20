package reconcile

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"trade/internal/data"

	assetpb "proto/common/asset"

	"shared/assetop"
)

// 资产管线的纯单测:不连库、不发 RPC。这里只钉住"装配拒绝什么"和"入队在碰数据库之前就该失败的
// 那几种入参" —— 真正的投递语义在 shared/assetop 的单测里,端到端在冒烟里。

// fakeIDs 是假号段。err 非 nil 时每次发号都失败。
type fakeIDs struct {
	next  uint64
	err   error
	calls int
}

func (f *fakeIDs) Next(context.Context) (uint64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	return f.next, nil
}

// newTestRepo 建一个**没有连接池**的 outbox 仓库。任何真的碰数据库的调用都会 panic,
// 这正是我们要的:用例一旦越过了"碰库之前"的边界,测试会当场炸而不是悄悄通过。
func newTestRepo(t *testing.T) *data.AssetOpRepo {
	t.Helper()
	repo, err := data.NewAssetOpRepo(nil, time.Second)
	if err != nil {
		t.Fatalf("NewAssetOpRepo: %v", err)
	}
	return repo
}

// newTestCaller 建一个**带 Signer** 的 Caller。不能用 &assetop.Caller{}:没有 Signer 的
// Caller 正是 New 要拒的那种形态,拿它当基准会把缺口固化进回归基线。
func newTestCaller(t *testing.T) *assetop.Caller {
	t.Helper()
	signer, err := assetop.NewSigner("trade", strings.Repeat("k", assetop.MinSecretLen))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return &assetop.Caller{Signer: signer}
}

// newTestPipeline 建一条装配完整的管线,并把假号段一并交回给调用方核对发号次数。
func newTestPipeline(t *testing.T, ids *fakeIDs) *Pipeline {
	t.Helper()
	p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: ids})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// validDebitBundle 是 v1 唯一合法的托管包形状:恰一条货币。
func validDebitBundle() *assetpb.AssetBundle {
	return &assetpb.AssetBundle{
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}},
	}
}

func TestNewRejectsIncompleteDeps(t *testing.T) {
	repo := newTestRepo(t)
	cases := []struct {
		name string
		deps Deps
		want error // nil 表示只要求"返回错误"
	}{
		{"缺 outbox 存储", Deps{Caller: newTestCaller(t), OpIDs: &fakeIDs{}}, nil},
		{"缺 Caller(密钥未配)", Deps{Ops: repo, OpIDs: &fakeIDs{}}, ErrSignerMissing},
		// Caller 在但没带 Signer:Caller.Do 每次回 ErrNoSigner,decide 判 ActionRetry,
		// 于是每行无限重投、永不终结 —— 一条"收得下托管、就是投不出去"的管线。
		{"Caller 没带 Signer", Deps{Ops: repo, Caller: &assetop.Caller{}, OpIDs: &fakeIDs{}}, ErrSignerMissing},
		{"缺号段客户端", Deps{Ops: repo, Caller: newTestCaller(t)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.deps)
			if err == nil {
				t.Fatal("应被拒绝")
			}
			if p != nil {
				t.Error("被拒绝时不能返回管线")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(..., %v)", err, tc.want)
			}
		})
	}
}

func TestNewAcceptsCompleteDeps(t *testing.T) {
	p, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("应返回管线")
	}
}

// TestNilPipelineFailsInsteadOfPanicking:密钥缺失时 svc 把 Assets.Pipeline 留成 nil,
// 启动 / 停机 / 业务路径都会拿着这个 nil 调进来。必须以 ErrSignerMissing 返回 ——
// panic 会把整个请求线程炸掉,而调用方本来就准备处理"托管失败"。
func TestNilPipelineFailsInsteadOfPanicking(t *testing.T) {
	var p *Pipeline
	_, err := p.EnqueueEscrowDebit(context.Background(), EscrowRequest{
		SellerPlayerID: 42, ListingID: 7, Bundle: validDebitBundle(),
	})
	if !errors.Is(err, ErrSignerMissing) {
		t.Errorf("空管线入队 err = %v, want errors.Is(..., ErrSignerMissing)", err)
	}
	if _, err := p.ResolveManually(context.Background(), assetop.ManualResolution{OpID: 1}); !errors.Is(err, ErrSignerMissing) {
		t.Errorf("空管线人工终结 err = %v, want errors.Is(..., ErrSignerMissing)", err)
	}
	p.Start(context.Background()) // 不得 panic
}

// TestEnqueueRejectsMalformedRequestBeforeTouchingStorage:入参不全、或包形状不合 v1 时,
// 必须在发号与建 seq 行**之前**失败。反过来(先发号再校验)会白白消耗 op_id 号段,
// 还会在库里留下半截痕迹。
//
// 包形状这几条尤其要在本地判:送到 scene 的后果各不相同,没有一条是"白跑一趟"这么轻的。
//   - 空包 / 多币种 / 带物品:§4.10 判 REJECTED 且**记账**,那个 seq 被永久钉死;
//   - item_uuids / pet_id:未进签名串,scene 回 UNKNOWN 且不记账 → 无限重排,行永久卡住,
//     16 条之后这个卖家连纯货币上架都会被 I5 未决守卫拒。
func TestEnqueueRejectsMalformedRequestBeforeTouchingStorage(t *testing.T) {
	ids := &fakeIDs{}
	p := newTestPipeline(t, ids)

	cases := []struct {
		name string
		req  EscrowRequest
	}{
		{"缺卖家", EscrowRequest{ListingID: 7, Bundle: validDebitBundle()}},
		{"缺商品", EscrowRequest{SellerPlayerID: 42, Bundle: validDebitBundle()}},
		{"缺资产包", EscrowRequest{SellerPlayerID: 42, ListingID: 7}},
		{"空包", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{}}},
		{"两条货币", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}, {CurrencyType: 2, Amount: 1}},
		}}},
		{"带可叠加物品", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}},
			Items:      []*assetpb.ItemGrant{{ConfigId: 1001, Count: 1}},
		}}},
		{"金额为 0", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 0}},
		}}},
		{"金额超 INT64_MAX", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: math.MaxInt64 + 1}},
		}}},
		{"带 item_uuids(未进签名串)", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}},
			ItemUuids:  []uint64{900001},
		}}},
		{"带 pet_id(未进签名串)", EscrowRequest{SellerPlayerID: 42, ListingID: 7, Bundle: &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}},
			PetId:      900001,
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.EnqueueEscrowDebit(context.Background(), tc.req); err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
	if ids.calls != 0 {
		t.Errorf("入参校验期间发了 %d 次号,应为 0(校验必须先于发号)", ids.calls)
	}
}

// TestEnqueueFailsWhenIDSegmentIsDown:发不出 op_id 就是本次上架失败,绝不自造 id
// (自造 = outbox 撞主键,或者两行不同的指令共用一个 id)。
//
// 这里必须传**合法**的包:传空包的话会在形状校验那一步就被挡回去,根本走不到发号,
// 这条用例也就测不到它要测的东西。
func TestEnqueueFailsWhenIDSegmentIsDown(t *testing.T) {
	ids := &fakeIDs{err: errors.New("data_service unreachable")}
	p := newTestPipeline(t, ids)
	_, err := p.EnqueueEscrowDebit(context.Background(), EscrowRequest{
		SellerPlayerID: 42, ListingID: 7, Bundle: validDebitBundle(),
	})
	if err == nil {
		t.Fatal("号段故障时必须失败")
	}
	if ids.calls != 1 {
		t.Errorf("发号调用 %d 次,应为 1", ids.calls)
	}
}

// TestValidateDebitBundleAcceptsTheOnlyLegalShape:边界值也要过 —— amount 恰好 1 与恰好
// INT64_MAX 是合法的,写成开区间会让"全部身家上架"这种真实用法被拒。
func TestValidateDebitBundleAcceptsTheOnlyLegalShape(t *testing.T) {
	for _, amount := range []uint64{1, 100, math.MaxInt64} {
		bundle := &assetpb.AssetBundle{
			Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: amount}},
		}
		if err := validateDebitBundle(bundle); err != nil {
			t.Errorf("amount=%d 应被接受,却得到 %v", amount, err)
		}
	}
}

// TestTradeStreamsAreTheTwoTradeStreams:I6 流独占 —— trade 只发这两条流的 seq。
// 多写一条(比如 GUILD_DEBIT)会让两个服务在同一条流上各自发号,seq 撞车。
func TestTradeStreamsAreTheTwoTradeStreams(t *testing.T) {
	got := TradeStreams()
	want := []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT,
	}
	if len(got) != len(want) {
		t.Fatalf("TradeStreams() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("TradeStreams()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// 返回新切片:调用方改了不能影响下一个调用方。
	got[0] = assetpb.AssetOpStream_ASSET_OP_STREAM_UNSPECIFIED
	if TradeStreams()[0] != assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT {
		t.Error("TradeStreams() 必须每次返回新切片")
	}
}

// TestLeaseTokenOfIsNonZeroAndDistinct:令牌 0 与"没有租约"同形,Reschedule 的 CAS 会失灵;
// 两行共用一个令牌则会互相把对方刚排好的时间覆盖掉。
func TestLeaseTokenOfIsNonZeroAndDistinct(t *testing.T) {
	p := &Pipeline{}
	seen := map[uint64]uint64{}
	// 最后一个是"恰好等于打散掩码"的 op_id:不加兜底的话它会算出 0。
	for _, opID := range []uint64{1, 2, 3, 1 << 40, ^uint64(0), 0x7472_6164_6500_0000} {
		token := p.leaseTokenOf(opID)
		if token == 0 {
			t.Errorf("op_id=%d 的令牌为 0", opID)
		}
		if prev, dup := seen[token]; dup {
			t.Errorf("op_id=%d 与 op_id=%d 的令牌相同", opID, prev)
		}
		seen[token] = opID
		if again := p.leaseTokenOf(opID); again != token {
			t.Errorf("op_id=%d 的令牌不稳定:%d != %d(插行与提交后那次投递必须用同一个值)", opID, again, token)
		}
	}
}

// TestLoopConstantsSatisfyAssetopValidation:循环参数与 scene 的 1024 seq 窗口、租约是一条
// 正确性证明。改坏其中一项(比如 Lease < OpBudget + 余量)会让同一行被两个副本同时处理。
func TestLoopConstantsSatisfyAssetopValidation(t *testing.T) {
	if _, err := New(Deps{Ops: newTestRepo(t), Caller: newTestCaller(t), OpIDs: &fakeIDs{}}); err != nil {
		t.Fatalf("本包的循环常量不被 assetop.NewLoop 接受: %v", err)
	}
	if loopOpBudget >= loopLease {
		t.Errorf("OpBudget(%v)必须显著小于 Lease(%v)", loopOpBudget, loopLease)
	}
	if CallTimeout >= loopOpBudget {
		t.Errorf("CallTimeout(%v)必须小于 OpBudget(%v),否则重查没有预算", CallTimeout, loopOpBudget)
	}
}

// TestManualResolverIsWiredIn:§4.38 的人工终结通道必须挂上 —— Loop.Manual 为 nil 时
// ResolveManually 只会回"未配置人工终结通道",卡死的行(UNKNOWN 无限重排、玩家长期离线)
// 就没有任何受控的处置手段。这里用非法状态触发 assetop 的入参校验:能走到状态校验,
// 就说明没有在 Manual == nil 那一步被挡掉。
func TestManualResolverIsWiredIn(t *testing.T) {
	p := newTestPipeline(t, &fakeIDs{})
	_, err := p.ResolveManually(context.Background(), assetop.ManualResolution{
		OpID: 1, Final: assetop.StatusPending, Operator: "ops", Reason: "test",
	})
	if err == nil {
		t.Fatal("PENDING 不是终结状态,应被拒绝")
	}
	if strings.Contains(err.Error(), "未配置人工终结通道") {
		t.Errorf("Loop.Manual 没挂上:%v", err)
	}
}
