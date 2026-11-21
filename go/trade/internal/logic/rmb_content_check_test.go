package logic

import (
	"context"
	"testing"

	cgpb "proto/content_guard"
	tradepb "proto/trade"

	"shared/contentguard"
	"shared/generated/pb/table"

	"trade/internal/commerce"
	"trade/internal/config"
	"trade/internal/constants"
)

func TestCreateRmbConsignmentContentCheck(t *testing.T) {
	newRMBLogic := func(checker contentguard.Checker, hz HomeZoneLookup) *JubaozhaiLogic {
		t.Helper()
		f := newFixture(t, config.ScopeZone)
		engine := &commerce.Engine{Now: f.deps.Now}
		f.deps.Commerce = engine
		f.deps.Rmb = &commerce.RmbConsignments{Engine: engine}
		f.deps.Mall = &commerce.Mall{Engine: engine}
		f.deps.RmbIdentity = missingContractIdentity{}
		f.deps.HomeZones = hz
		f.deps.ContentGuard = checker
		return f.as(playerA)
	}

	req := func(title string) *tradepb.CreateRmbConsignmentRequest {
		return &tradepb.CreateRmbConsignmentRequest{
			Title:           title,
			PriceFen:        1000,
			ClientRequestId: "rmb-req-001",
			Assets:          &tradepb.TradeAssetSelection{CurrencyType: 0, CurrencyAmount: 100},
		}
	}

	for _, tc := range []struct {
		name     string
		res      contentguard.Result
		wantCode uint32
	}{
		{"reject returns sensitive tip", contentguard.Result{Verdict: contentguard.Reject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD}, constants.ErrTitleSensitive},
		{"masked is not usable for consignment title", contentguard.Result{Verdict: contentguard.Masked, Masked: "***", Category: cgpb.TextCategory_TEXT_CATEGORY_ABUSE}, constants.ErrTitleSensitive},
		{"unavailable returns check unavailable fault tip", contentguard.Result{Verdict: contentguard.Unavailable}, constants.ErrTextCheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hzCalls := 0
			var gotScene cgpb.TextScene
			var gotPlayer uint64
			var gotText string
			checker := contentguard.Func(func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
				gotScene, gotPlayer, gotText = scene, playerID, text
				return tc.res
			})
			l := newRMBLogic(checker, fakeHomeZone{fn: func(context.Context, uint64) (uint32, error) {
				hzCalls++
				return zoneA, nil
			}})
			out, err := l.CreateRmbConsignment(req("违规标题"))
			if err != nil {
				t.Fatalf("expected in-band tip, got rpc error: %v", err)
			}
			if out.GetErrorMessage().GetId() != tc.wantCode {
				t.Fatalf("tip code = %d, want %d", out.GetErrorMessage().GetId(), tc.wantCode)
			}
			if gotScene != cgpb.TextScene_TEXT_SCENE_TRADE_TITLE || gotPlayer != playerA || gotText != "违规标题" {
				t.Fatalf("unexpected content guard input: scene=%v player=%d text=%q", gotScene, gotPlayer, gotText)
			}
			if hzCalls != 0 {
				t.Fatalf("rejected title must fail before downstream lookups, got %d HomeZone calls", hzCalls)
			}
		})
	}

	t.Run("pass proceeds downstream", func(t *testing.T) {
		hzCalls := 0
		checker := contentguard.Func(func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
			return contentguard.Result{Verdict: contentguard.Pass}
		})
		l := newRMBLogic(checker, fakeHomeZone{fn: func(context.Context, uint64) (uint32, error) {
			hzCalls++
			return 0, errStoreDown
		}})
		out, err := l.CreateRmbConsignment(req("极品屠龙刀"))
		if err != nil {
			t.Fatalf("unexpected rpc error: %v", err)
		}
		if hzCalls != 1 || out.GetErrorMessage().GetId() != uint32(table.CommonError_kServiceUnavailable) {
			t.Fatalf("expected downstream HomeZone check after Pass, calls=%d code=%d", hzCalls, out.GetErrorMessage().GetId())
		}
	})

	t.Run("blank title returns invalid without calling content guard", func(t *testing.T) {
		cgCalls := 0
		checker := contentguard.Func(func(context.Context, cgpb.TextScene, uint64, string) contentguard.Result {
			cgCalls++
			return contentguard.Result{Verdict: contentguard.Pass}
		})
		l := newRMBLogic(checker, fakeHomeZone{fn: func(context.Context, uint64) (uint32, error) { return zoneA, nil }})
		out, err := l.CreateRmbConsignment(req("   "))
		if err != nil || out.GetErrorMessage().GetId() != constants.ErrInvalidParameter || cgCalls != 0 {
			t.Fatalf("blank title: err=%v code=%d cgCalls=%d", err, out.GetErrorMessage().GetId(), cgCalls)
		}
	})
}

type fakeHomeZone struct {
	fn func(context.Context, uint64) (uint32, error)
}

func (f fakeHomeZone) HomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	return f.fn(ctx, playerID)
}
