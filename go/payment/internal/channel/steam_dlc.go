// Batch B5: Steam DLC 权益核验与发行渠道功能门禁（docs/planning/fair-monetization-expansion-v2.md §2.5, M-8）。
//
// 1. SteamDlcVerifier：调用 Steam Partner Web API `ISteamUser/CheckAppOwnership/v3/`
//    核验玩家对「留灯院创始支持者典藏包」等透明 DLC 的归属权与退款撤回状态；
//    单次发放完全复用既有 90 天仙途季享卡 + 3×3402 形象卡 + CosmeticShowcase#13 阵纹 + 称谓（0 战力加成），
//    并由 (steam_id, dlc_app_id) 确定性交易号保证幂等发放与退款撤销。
// 2. DistributionPolicy（M-8 红线）：
//    - 国内无版号小游戏（wechat_minigame_unlicensed / douyin_minigame_unlicensed）：物理关闭全部 IAP/赠礼/DLC，仅留流量主 IAA 开关；
//    - Steam PC（steam_pc）：允许 Steam IAP / 赠礼 / 透明 DLC，物理禁用全部激励广告；
//    - 海外 iOS / Android（global_ios / global_android）：允许官方 IAP / 赠礼，禁用 Steam DLC 与国内切支付。
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	NameSteamDLC = "steam_dlc"

	// FounderSupporterDlcSKU 对应「留灯院创始支持者典藏包」透明支持者 DLC（0 战力加成）。
	FounderSupporterDlcSKU = "sku_steam_founder_lamp_pack"
)

// SteamDlcGrantBundle 描述单个透明 DLC 的固定绑定发放清单（纯外观/便利，0 数值加成）。
type SteamDlcGrantBundle struct {
	SKU                   string
	DlcAppID              uint32
	LightCardSeasonDays   uint32 // 90 天仙途季享卡（遵守 180 天叠加上限）
	AppearanceCardItemID  uint32 // 3402 绑定角色形象卡
	AppearanceCardCount   uint32 // 3 张
	ShowcaseCosmeticID    uint32 // CosmeticShowcase#13「星河初启」限定阵纹
	FounderTitleKey       string // 限定称谓「初灯同道」（0 战力）
	PriceMinorCNY         uint64 // 参考价 4800 分（¥48）
	PriceMinorUSD         uint64 // 参考价 699 分（$6.99）
}

// DefaultSteamDlcCatalog 返回现役 Steam 透明 DLC 清单。
func DefaultSteamDlcCatalog(founderDlcAppID uint32) map[uint32]SteamDlcGrantBundle {
	if founderDlcAppID == 0 {
		return map[uint32]SteamDlcGrantBundle{}
	}
	return map[uint32]SteamDlcGrantBundle{
		founderDlcAppID: {
			SKU:                  FounderSupporterDlcSKU,
			DlcAppID:             founderDlcAppID,
			LightCardSeasonDays:  90,
			AppearanceCardItemID: 3402,
			AppearanceCardCount:  3,
			ShowcaseCosmeticID:   13,
			FounderTitleKey:      "title_founder_lamp",
			PriceMinorCNY:        4800,
			PriceMinorUSD:        699,
		},
	}
}

type SteamDlcConfig struct {
	BaseURL         string // 默认 https://partner.steam-api.com
	PublisherAPIKey string
	FounderDlcAppID uint32
	HTTPClient      *http.Client
	Now             func() time.Time
}

type SteamDlcOwnership struct {
	SteamID       string
	DlcAppID      uint32
	Owned         bool
	Permanent     bool
	PurchasedAt   time.Time
	TransactionID string // 确定性幂等单号：steam-dlc:<app_id>:<steam_id>
	Bundle        SteamDlcGrantBundle
	Revoked       bool // true = 原已核验发放但 Steam 返回 own=false（触发退款撤回链路）
}

type SteamDlcVerifier struct {
	baseURL string
	apiKey  string
	catalog map[uint32]SteamDlcGrantBundle
	client  *http.Client
	now     func() time.Time
}

func NewSteamDlcVerifier(cfg SteamDlcConfig) (*SteamDlcVerifier, error) {
	if strings.TrimSpace(cfg.PublisherAPIKey) == "" {
		return nil, errors.New("steam_dlc: publisher api key is required")
	}
	if cfg.FounderDlcAppID == 0 {
		return nil, errors.New("steam_dlc: founder dlc app id is required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://partner.steam-api.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = newHTTPClient()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &SteamDlcVerifier{
		baseURL: base,
		apiKey:  strings.TrimSpace(cfg.PublisherAPIKey),
		catalog: DefaultSteamDlcCatalog(cfg.FounderDlcAppID),
		client:  client,
		now:     now,
	}, nil
}

type steamCheckOwnershipReply struct {
	AppOwnership struct {
		OwnsApp   bool   `json:"ownsapp"`
		Permanent bool   `json:"permanent"`
		Timestamp string `json:"timestamp"`
		Result    string `json:"result"`
	} `json:"appownership"`
}

// VerifyOwnership 向 Steam Partner API 核验指定 SteamID 是否仍持有该 DLC；
// previouslyGranted=true 且 Steam 返回 ownsapp=false 时标记 Revoked=true，驱动调用方走退款回收钩子。
func (v *SteamDlcVerifier) VerifyOwnership(ctx context.Context, steamID string, dlcAppID uint32, previouslyGranted bool) (SteamDlcOwnership, error) {
	steamID = strings.TrimSpace(steamID)
	if steamID == "" || !allDigits(steamID) {
		return SteamDlcOwnership{}, errors.New("steam_dlc: steam_id must be numeric")
	}
	bundle, ok := v.catalog[dlcAppID]
	if !ok {
		return SteamDlcOwnership{}, fmt.Errorf("steam_dlc: unknown dlc app id %d", dlcAppID)
	}
	q := url.Values{}
	q.Set("key", v.apiKey)
	q.Set("steamid", steamID)
	q.Set("appid", strconv.FormatUint(uint64(dlcAppID), 10))
	endpoint := v.baseURL + "/ISteamUser/CheckAppOwnership/v3/?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SteamDlcOwnership{}, err
	}
	resp, err := send(v.client, req)
	if err != nil {
		return SteamDlcOwnership{}, err
	}
	defer resp.Body.Close()
	raw, err := readBody(resp)
	if err != nil {
		return SteamDlcOwnership{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return SteamDlcOwnership{}, &CallError{Channel: NameSteamDLC, Op: "CheckAppOwnership", Status: resp.StatusCode, Code: strconv.Itoa(resp.StatusCode)}
	}
	var parsed steamCheckOwnershipReply
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return SteamDlcOwnership{}, fmt.Errorf("steam_dlc: invalid json: %w", err)
	}
	if parsed.AppOwnership.Result != "" && !strings.EqualFold(parsed.AppOwnership.Result, "OK") {
		return SteamDlcOwnership{}, &CallError{Channel: NameSteamDLC, Op: "CheckAppOwnership", Status: resp.StatusCode, Code: parsed.AppOwnership.Result}
	}
	var purchasedAt time.Time
	if parsed.AppOwnership.Timestamp != "" {
		if t, parseErr := time.Parse(time.RFC3339, parsed.AppOwnership.Timestamp); parseErr == nil {
			purchasedAt = t
		}
	}
	if purchasedAt.IsZero() && parsed.AppOwnership.OwnsApp {
		purchasedAt = v.now().UTC()
	}
	txID := fmt.Sprintf("steam-dlc:%d:%s", dlcAppID, steamID)
	revoked := previouslyGranted && !parsed.AppOwnership.OwnsApp
	return SteamDlcOwnership{
		SteamID:       steamID,
		DlcAppID:      dlcAppID,
		Owned:         parsed.AppOwnership.OwnsApp,
		Permanent:     parsed.AppOwnership.Permanent,
		PurchasedAt:   purchasedAt,
		TransactionID: txID,
		Bundle:        bundle,
		Revoked:       revoked,
	}, nil
}

// ToPaymentOrRevocation 将 Steam DLC 核验结果转成统一支付/撤回事件供 payment/mall 核心消费。
func (o SteamDlcOwnership) ToPaymentOrRevocation(playerID uint64, observedAt time.Time) (Payment, *Revocation, bool) {
	if o.Revoked {
		rev := &Revocation{
			TransactionID: o.TransactionID,
			Revoked:       true,
			ObservedAt:    observedAt,
			Detail:        "steam_dlc_ownership_revoked",
		}
		return Payment{}, rev, false
	}
	if !o.Owned {
		return Payment{}, nil, false
	}
	return Payment{
		TransactionID: o.TransactionID,
		OrderID:       fmt.Sprintf("%s:%d", o.TransactionID, playerID),
		AmountMinor:   o.Bundle.PriceMinorCNY,
		Currency:      "CNY",
		PaidAt:        o.PurchasedAt,
		ProductID:     o.Bundle.SKU,
		StorePriced:   true,
	}, nil, true
}

// ---------------------------------------------------------------------------
// M-8 渠道发行形态功能门禁矩阵（零拒审硬红线）
// ---------------------------------------------------------------------------

type DistributionChannel string

const (
	DistWechatMinigameUnlicensed DistributionChannel = "wechat_minigame_unlicensed"
	DistDouyinMinigameUnlicensed DistributionChannel = "douyin_minigame_unlicensed"
	DistGlobalIOS                DistributionChannel = "global_ios"
	DistGlobalAndroid            DistributionChannel = "global_android"
	DistSteamPC                  DistributionChannel = "steam_pc"
)

type DistributionPolicy struct {
	Channel                 DistributionChannel
	IapEnabled              bool
	GiftingEnabled          bool
	SteamDlcEnabled         bool
	RewardedAdAllowed       bool
	ExternalPaymentAllowed  bool // 永远为 false（零外链切支付/收款码红线）
}

// ResolveDistributionPolicy 返回各发行形态的权威功能开关；未知渠道 fail-closed 全部关闭。
func ResolveDistributionPolicy(ch DistributionChannel) DistributionPolicy {
	switch ch {
	case DistWechatMinigameUnlicensed, DistDouyinMinigameUnlicensed:
		// 国内无版号小游戏：彻底隐藏全部真钱 IAP、赠礼与 DLC，仅保留受服务端 enabled=0 总控的官方流量主 IAA。
		return DistributionPolicy{
			Channel:                ch,
			IapEnabled:             false,
			GiftingEnabled:         false,
			SteamDlcEnabled:        false,
			RewardedAdAllowed:      true,
			ExternalPaymentAllowed: false,
		}
	case DistSteamPC:
		// Steam PC：允许官方 IAP / 赠礼 / 透明支持者 DLC；彻底禁用全部内置激励广告。
		return DistributionPolicy{
			Channel:                ch,
			IapEnabled:             true,
			GiftingEnabled:         true,
			SteamDlcEnabled:        true,
			RewardedAdAllowed:      false,
			ExternalPaymentAllowed: false,
		}
	case DistGlobalIOS, DistGlobalAndroid:
		// 海外官方商店：允许 Apple IAP / Play Billing 与赠礼；禁用 Steam DLC 与一切外链支付。
		return DistributionPolicy{
			Channel:                ch,
			IapEnabled:             true,
			GiftingEnabled:         true,
			SteamDlcEnabled:        false,
			RewardedAdAllowed:      true,
			ExternalPaymentAllowed: false,
		}
	default:
		return DistributionPolicy{Channel: ch}
	}
}
