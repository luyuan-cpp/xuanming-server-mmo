package assetop

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	dspb "proto/data_service"
)

// DataServiceLedger 的契约测试(docs/design/guild-phase2/07-rollback-fail-closed.md §7.10.2 A1–A3)。

// fakeLedgerService 只实现 GetPlayerAssetOpLedger;嵌入的接口为 nil,误调其它方法会直接 panic。
// call 决定这一次的返回;它拿到的 ctx 就是适配器传下来的那个,A3 靠它观察超时。
type fakeLedgerService struct {
	dspb.DataServiceClient
	call  func(ctx context.Context, req *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error)
	calls int
}

func (f *fakeLedgerService) GetPlayerAssetOpLedger(ctx context.Context, req *dspb.GetPlayerAssetOpLedgerRequest, _ ...grpc.CallOption) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
	f.calls++
	return f.call(ctx, req)
}

func ledgerReply(resp *dspb.GetPlayerAssetOpLedgerResponse, err error) func(context.Context, *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
	return func(context.Context, *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
		return resp, err
	}
}

// A1:found=false → (nil, nil);found=true → 原样返回账本,账本字段缺席时回空消息(不是 nil)。
func TestDataServiceLedger_A1_FoundMapping(t *testing.T) {
	t.Run("found=false", func(t *testing.T) {
		svc := &fakeLedgerService{call: ledgerReply(&dspb.GetPlayerAssetOpLedgerResponse{Found: false}, nil)}
		got, err := (&DataServiceLedger{Client: svc}).ReadPersistedLedger(context.Background(), 7)
		if err != nil || got != nil {
			t.Fatalf("found=false 应得 (nil, nil),实际 (%v, %v)", got, err)
		}
	})

	t.Run("found=true 带账本", func(t *testing.T) {
		want := &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{{
			Stream:      assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
			Watermark:   3,
			MaxSeq:      5,
			StreamEpoch: 2,
		}}}
		var gotPlayer uint64
		svc := &fakeLedgerService{call: func(_ context.Context, req *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
			gotPlayer = req.GetPlayerId()
			return &dspb.GetPlayerAssetOpLedgerResponse{Found: true, Ledger: want}, nil
		}}
		got, err := (&DataServiceLedger{Client: svc}).ReadPersistedLedger(context.Background(), 7)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if gotPlayer != 7 {
			t.Fatalf("请求里的 player_id 应为 7,实际 %d", gotPlayer)
		}
		if !proto.Equal(want, got) {
			t.Fatalf("账本应原样返回: want=%v got=%v", want, got)
		}
	})

	t.Run("found=true 账本缺席", func(t *testing.T) {
		svc := &fakeLedgerService{call: ledgerReply(&dspb.GetPlayerAssetOpLedgerResponse{Found: true}, nil)}
		got, err := (&DataServiceLedger{Client: svc}).ReadPersistedLedger(context.Background(), 7)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if got == nil {
			t.Fatal("found=true 必须回非 nil 账本;nil 专属于\"没有 blob\"")
		}
		if len(got.GetStreams()) != 0 {
			t.Fatalf("应为空账本,实际 %v", got)
		}
	})
}

// A2:任何 gRPC 错误 → (nil, err),且保留原 code;不重试(只调一次)。零值适配器回错误而不是 panic。
func TestDataServiceLedger_A2_ErrorPassthrough(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable, codes.Internal} {
		t.Run(code.String(), func(t *testing.T) {
			svc := &fakeLedgerService{call: ledgerReply(
				// 即使对端同时回了响应体,只要有 error 就不得采信。
				&dspb.GetPlayerAssetOpLedgerResponse{Found: true, Ledger: &componentpb.PlayerAssetOpLedgerComp{}},
				status.Error(code, "boom"))}
			got, err := (&DataServiceLedger{Client: svc}).ReadPersistedLedger(context.Background(), 7)
			if got != nil {
				t.Fatalf("有错误时账本必须为 nil,实际 %v", got)
			}
			if status.Code(err) != code {
				t.Fatalf("应保留 gRPC code %s,实际 %v", code, err)
			}
			if svc.calls != 1 {
				t.Fatalf("适配器不得自行重试,实际调用 %d 次", svc.calls)
			}
		})
	}

	t.Run("未装配客户端", func(t *testing.T) {
		got, err := (&DataServiceLedger{}).ReadPersistedLedger(context.Background(), 7)
		if err == nil || got != nil {
			t.Fatalf("零值适配器应回 (nil, err),实际 (%v, %v)", got, err)
		}
	})
}

// A3:Timeout 生效。fake 一直阻塞到 ctx 结束,再像真实 gRPC 客户端那样把 ctx 错误翻成 status;
// 调用方 ctx 没有 deadline,所以能结束只可能是适配器套的那层超时。
// 另有 5s 兜底:超时没套上时测试以明确的失败结束,而不是挂住。
func TestDataServiceLedger_A3_TimeoutApplied(t *testing.T) {
	errNoDeadline := errors.New("适配器没有套上超时")
	svc := &fakeLedgerService{call: func(ctx context.Context, _ *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errNoDeadline
		}
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(5 * time.Second):
			return nil, errNoDeadline
		}
	}}

	got, err := (&DataServiceLedger{Client: svc, Timeout: time.Millisecond}).ReadPersistedLedger(context.Background(), 7)
	if got != nil {
		t.Fatalf("超时时账本必须为 nil,实际 %v", got)
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("应得 DeadlineExceeded,实际 %v", err)
	}

	t.Run("Timeout=0 不另加截止时间", func(t *testing.T) {
		var hadDeadline bool
		svc := &fakeLedgerService{call: func(ctx context.Context, _ *dspb.GetPlayerAssetOpLedgerRequest) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
			_, hadDeadline = ctx.Deadline()
			return &dspb.GetPlayerAssetOpLedgerResponse{}, nil
		}}
		if _, err := (&DataServiceLedger{Client: svc}).ReadPersistedLedger(context.Background(), 7); err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if hadDeadline {
			t.Fatal("Timeout=0 时应只受调用方 ctx 约束,不得凭空加截止时间")
		}
	})
}
