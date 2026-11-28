package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSteamDlcVerifier_GrantAndRefundRevocation(t *testing.T) {
	owned := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ISteamUser/CheckAppOwnership/v3/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("key") != "secret-publisher-key" || r.URL.Query().Get("appid") != "3100001" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if owned {
			_, _ = w.Write([]byte(`{"appownership":{"ownsapp":true,"permanent":true,"timestamp":"2026-11-27T10:00:00Z","result":"OK"}}`))
		} else {
			_, _ = w.Write([]byte(`{"appownership":{"ownsapp":false,"permanent":false,"result":"OK"}}`))
		}
	}))
	defer srv.Close()

	verifier, err := NewSteamDlcVerifier(SteamDlcConfig{
		BaseURL:         srv.URL,
		PublisherAPIKey: "secret-publisher-key",
		FounderDlcAppID: 3100001,
		HTTPClient:      srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewSteamDlcVerifier: %v", err)
	}

	// 1. 首次核验通过：映射「留灯院创始支持者典藏包」90天季享卡 + 3×3402 + CosmeticShowcase#13 + 称谓。
	res, err := verifier.VerifyOwnership(context.Background(), "76561198012345678", 3100001, false)
	if err != nil {
		t.Fatalf("VerifyOwnership owned: %v", err)
	}
	if !res.Owned || res.Revoked {
		t.Fatalf("expected owned=true revoked=false, got %+v", res)
	}
	if res.Bundle.LightCardSeasonDays != 90 || res.Bundle.AppearanceCardItemID != 3402 ||
		res.Bundle.AppearanceCardCount != 3 || res.Bundle.ShowcaseCosmeticID != 13 ||
		res.Bundle.FounderTitleKey != "title_founder_lamp" {
		t.Fatalf("unexpected bundle contents: %+v", res.Bundle)
	}
	pay, rev, ok := res.ToPaymentOrRevocation(99001, time.Now())
	if !ok || rev != nil || pay.TransactionID != "steam-dlc:3100001:76561198012345678" || pay.ProductID != FounderSupporterDlcSKU {
		t.Fatalf("unexpected payment projection: pay=%+v rev=%+v ok=%v", pay, rev, ok)
	}

	// 2. 玩家在 Steam 侧退款后复查（previouslyGranted=true 且 ownsapp=false）触发撤回钩子。
	owned = false
	revRes, err := verifier.VerifyOwnership(context.Background(), "76561198012345678", 3100001, true)
	if err != nil {
		t.Fatalf("VerifyOwnership revoked: %v", err)
	}
	if revRes.Owned || !revRes.Revoked {
		t.Fatalf("expected owned=false revoked=true, got %+v", revRes)
	}
	_, rev, ok = revRes.ToPaymentOrRevocation(99001, time.Now())
	if ok || rev == nil || !rev.Revoked || rev.TransactionID != "steam-dlc:3100001:76561198012345678" {
		t.Fatalf("expected revocation event, got rev=%+v ok=%v", rev, ok)
	}
}

func TestDistributionPolicy_M8ComplianceMatrix(t *testing.T) {
	// 国内无版号小游戏：严禁露出任何 IAP / 赠礼 / DLC / 外链支付
	for _, ch := range []DistributionChannel{DistWechatMinigameUnlicensed, DistDouyinMinigameUnlicensed} {
		p := ResolveDistributionPolicy(ch)
		if p.IapEnabled || p.GiftingEnabled || p.SteamDlcEnabled || p.ExternalPaymentAllowed || !p.RewardedAdAllowed {
			t.Fatalf("unlicensed minigame policy violated for %s: %+v", ch, p)
		}
	}
	// Steam PC：允许 IAP / 赠礼 / 透明 DLC，物理禁广告、禁外链支付
	steam := ResolveDistributionPolicy(DistSteamPC)
	if !steam.IapEnabled || !steam.GiftingEnabled || !steam.SteamDlcEnabled || steam.RewardedAdAllowed || steam.ExternalPaymentAllowed {
		t.Fatalf("steam policy violated: %+v", steam)
	}
	// 海外 iOS / Android：允许官方 IAP / 赠礼，禁 Steam DLC 与外链支付
	for _, ch := range []DistributionChannel{DistGlobalIOS, DistGlobalAndroid} {
		p := ResolveDistributionPolicy(ch)
		if !p.IapEnabled || !p.GiftingEnabled || p.SteamDlcEnabled || p.ExternalPaymentAllowed {
			t.Fatalf("global mobile policy violated for %s: %+v", ch, p)
		}
	}
	// 未知发行通道 fail-closed 全关闭
	unknown := ResolveDistributionPolicy("unknown_channel")
	if unknown.IapEnabled || unknown.GiftingEnabled || unknown.SteamDlcEnabled || unknown.RewardedAdAllowed || unknown.ExternalPaymentAllowed {
		t.Fatalf("unknown channel must fail-closed: %+v", unknown)
	}
}
