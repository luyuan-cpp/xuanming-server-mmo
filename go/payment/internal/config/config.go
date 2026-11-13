// Package config 是支付服务的配置与启动期校验。密钥一律只在配置里写环境变量名,值经 K8s Secret 注入(secretenv 同一口径),不进日志。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/zeromicro/go-zero/zrpc"

	"shared/authoritysql"
	"shared/dbplacement"
)

type Config struct {
	zrpc.RpcServerConf
	ZoneId            uint32             `json:"ZoneId,default=1"`
	MetricsListenAddr string             `json:"MetricsListenAddr,optional"`
	KillSwitchPrefix  string             `json:"KillSwitchPrefix,optional"`
	AdvertiseHost     string             `json:"AdvertiseHost,optional"`
	DSNEnv            string             `json:"DSNEnv"`
	Backend           string             `json:"Backend,default=tidb"` // tidb | mysql,见 authoritysql.ParseBackend
	MaxOpenConns      int                `json:"MaxOpenConns,default=16"`
	AutoMigrate       bool               `json:"AutoMigrate,default=false"`
	DbPlacement       dbplacement.Config `json:",optional"`
	Payment           PaymentConf        `json:"Payment"`
}

type PaymentConf struct {
	// NotifyListenAddr 是渠道回调的 HTTP 监听地址(经 Ingress 暴露到公网;微信只认 80/443)。与 gRPC、指标端口分开。
	NotifyListenAddr string `json:"NotifyListenAddr"`
	// NotifyBaseURL 是渠道回调用的公网根地址(如 https://pay.example.com),微信支付、支付宝下单时带上 <根>/notify/<渠道>。
	NotifyBaseURL  string             `json:"NotifyBaseURL,optional"`
	Mall           MallConf           `json:"Mall"`
	Platforms      []PlatformConf     `json:"Platforms,optional"`
	Products       []ProductConf      `json:"Products,optional"`
	Compliance     ComplianceConf     `json:"Compliance,optional"`
	DevPay         DevPayConf         `json:"DevPay,optional"`
	WechatPay      WechatPayConf      `json:"WechatPay,optional"`
	Alipay         AlipayConf         `json:"Alipay,optional"`
	WechatMinigame WechatMinigameConf `json:"WechatMinigame,optional"`
	DouyinMinigame DouyinMinigameConf `json:"DouyinMinigame,optional"`
	Apple          AppleConf          `json:"Apple,optional"`
	Google         GoogleConf         `json:"Google,optional"`
}

type MallConf struct {
	BaseURL          string `json:"BaseURL"`
	PaymentSubject   string `json:"PaymentSubject,default=payment-adapter"`
	PaymentSecretEnv string `json:"PaymentSecretEnv,default=COMMERCE_PAYMENT_SECRET"`
	TimeoutMs        int    `json:"TimeoutMs,default=2500"`
}

// PlatformConf 是一种发行形态开放的渠道,按展示顺序。
type PlatformConf struct {
	Platform string   `json:"Platform"` // android_app / ios_app / wechat_minigame / douyin_minigame / desktop
	Channels []string `json:"Channels"`
}

// ProductConf 是一个商城 SKU 在按商品定价的渠道上的商品号。没写的渠道不能付这个 SKU。
type ProductConf struct {
	SkuId          uint64 `json:"SkuId"`
	Title          string `json:"Title,optional"`
	Apple          string `json:"Apple,optional"`
	Google         string `json:"Google,optional"`
	WechatMinigame string `json:"WechatMinigame,optional"`
}

// ComplianceConf 是国内收费的合规闸(设计 §12):实名与未成年人消费限额。
// MinorSpendingGate 目前只有 none(本服务还没接实名与限额):生产模式下 APP 内的真钱渠道拒绝启用;
// 小游戏渠道的实名与未成年人消费限制由微信 / 抖音平台执行,照常可用(上线前按平台规则核对)。
type ComplianceConf struct {
	MinorSpendingGate string `json:"MinorSpendingGate,default=none"`
}

// TestConf 是一个渠道的测试交易(沙箱、许可测试账号)发不发货。生产环境只给审核账号开。
type TestConf struct {
	Accept  bool     `json:"Accept,optional"`
	Players []uint64 `json:"Players,optional"`
}

type DevPayConf struct {
	Enabled   bool   `json:"Enabled,optional"`
	SecretEnv string `json:"SecretEnv,default=PAYMENT_DEVPAY_SECRET"`
}

type WechatPayConf struct {
	Enabled              bool   `json:"Enabled,optional"`
	AppID                string `json:"AppID,optional"`
	MchID                string `json:"MchID,optional"`
	MerchantSerialNo     string `json:"MerchantSerialNo,optional"`
	PrivateKeyEnv        string `json:"PrivateKeyEnv,default=PAYMENT_WECHATPAY_PRIVATE_KEY"`
	APIv3KeyEnv          string `json:"APIv3KeyEnv,default=PAYMENT_WECHATPAY_APIV3_KEY"`
	PlatformPublicKeyID  string `json:"PlatformPublicKeyID,optional"`
	PlatformPublicKeyEnv string `json:"PlatformPublicKeyEnv,default=PAYMENT_WECHATPAY_PUBLIC_KEY"`
	Description          string `json:"Description,default=游戏商品"`
	Endpoint             string `json:"Endpoint,optional"`
}

type AlipayConf struct {
	Enabled       bool   `json:"Enabled,optional"`
	AppID         string `json:"AppID,optional"`
	PrivateKeyEnv string `json:"PrivateKeyEnv,default=PAYMENT_ALIPAY_PRIVATE_KEY"`
	PublicKeyEnv  string `json:"PublicKeyEnv,default=PAYMENT_ALIPAY_PUBLIC_KEY"`
	SellerID      string `json:"SellerID,optional"`
	Gateway       string `json:"Gateway,optional"`
	Subject       string `json:"Subject,default=游戏商品"`
}

type WechatMinigameConf struct {
	Enabled           bool     `json:"Enabled,optional"`
	AppID             string   `json:"AppID,optional"`
	AppSecretEnv      string   `json:"AppSecretEnv,default=PAYMENT_WECHAT_MINIGAME_APP_SECRET"`
	OfferID           string   `json:"OfferID,optional"`
	AppKeyEnv         string   `json:"AppKeyEnv,default=PAYMENT_WECHAT_MINIGAME_APP_KEY"`
	Env               uint32   `json:"Env,optional"` // 0 正式,1 沙箱
	ZoneID            string   `json:"ZoneID,optional"`
	PushTokenEnv      string   `json:"PushTokenEnv,default=PAYMENT_WECHAT_MINIGAME_PUSH_TOKEN"`
	EncodingAESKeyEnv string   `json:"EncodingAESKeyEnv,optional"`
	Test              TestConf `json:"Test,optional"`
}

type DouyinMinigameConf struct {
	Enabled      bool   `json:"Enabled,optional"`
	AppID        string `json:"AppID,optional"`
	PushTokenEnv string `json:"PushTokenEnv,default=PAYMENT_DOUYIN_PUSH_TOKEN"`
	ZoneID       string `json:"ZoneID,optional"`
	GoodName     string `json:"GoodName,optional"`
}

type AppleConf struct {
	Enabled             bool     `json:"Enabled,optional"`
	BundleID            string   `json:"BundleID,optional"`
	AppAppleID          int64    `json:"AppAppleID,optional"`
	IssuerID            string   `json:"IssuerID,optional"`
	KeyID               string   `json:"KeyID,optional"`
	PrivateKeyEnv       string   `json:"PrivateKeyEnv,default=PAYMENT_APPLE_PRIVATE_KEY"`
	RootCertificatesEnv string   `json:"RootCertificatesEnv,default=PAYMENT_APPLE_ROOT_CERTIFICATES"`
	AllowSandbox        bool     `json:"AllowSandbox,optional"`
	Test                TestConf `json:"Test,optional"`
}

type GoogleConf struct {
	Enabled            bool     `json:"Enabled,optional"`
	PackageName        string   `json:"PackageName,optional"`
	ServiceAccountEnv  string   `json:"ServiceAccountEnv,default=PAYMENT_GOOGLE_SERVICE_ACCOUNT"`
	PushAudience       string   `json:"PushAudience,optional"`
	PushServiceAccount string   `json:"PushServiceAccount,optional"`
	Test               TestConf `json:"Test,optional"`
}

// IsRelaxedMode:Mode=dev/test 是本地与用例环境,允许联调渠道、http 回调地址与非正式网关。
func IsRelaxedMode(mode string) bool { return mode == "dev" || mode == "test" }

// 每种发行形态能挂的渠道。iOS App 内的虚拟商品只能用 App Store 内购(苹果审核规则 3.1.1);
// 小游戏只能用各自平台的支付;联调渠道只在 dev/test 出现(下面另判)。
var platformChannels = map[string][]string{
	"android_app":     {"wechat_pay", "alipay", "google", "devpay"},
	"ios_app":         {"apple", "devpay"},
	"wechat_minigame": {"wechat_minigame", "devpay"},
	"douyin_minigame": {"douyin_minigame", "devpay"},
	"desktop":         {"devpay"},
}

// Enabled 报告某个渠道开没开。
func (p PaymentConf) Enabled(name string) bool {
	switch name {
	case "devpay":
		return p.DevPay.Enabled
	case "wechat_pay":
		return p.WechatPay.Enabled
	case "alipay":
		return p.Alipay.Enabled
	case "wechat_minigame":
		return p.WechatMinigame.Enabled
	case "douyin_minigame":
		return p.DouyinMinigame.Enabled
	case "apple":
		return p.Apple.Enabled
	case "google":
		return p.Google.Enabled
	default:
		return false
	}
}

// Validate 在 conf.MustLoad 与 run 入口各核一次。不合法就拒启,报错只点字段名,不回显密钥。
func (c Config) Validate() error {
	if c.Name == "" || c.ListenOn == "" || c.ZoneId == 0 || len(c.Etcd.Hosts) == 0 || c.DSNEnv == "" || c.MaxOpenConns < 2 || c.MaxOpenConns > 64 || c.Timeout < 1000 || c.Timeout > 4000 {
		return errors.New("payment config: Name, ListenOn, ZoneId, Etcd.Hosts, DSNEnv, MaxOpenConns (2-64) and Timeout (1000-4000 ms) are required")
	}
	if _, err := authoritysql.ParseBackend(c.Backend); err != nil {
		return err
	}
	p := c.Payment
	relaxed := IsRelaxedMode(c.Mode)
	if p.NotifyListenAddr == "" {
		return errors.New("payment config: Payment.NotifyListenAddr is required")
	}
	if err := checkBaseURL("Payment.Mall.BaseURL", p.Mall.BaseURL, true); err != nil {
		return err
	}
	if p.WechatPay.Enabled || p.Alipay.Enabled {
		if err := checkBaseURL("Payment.NotifyBaseURL", p.NotifyBaseURL, relaxed); err != nil {
			return err
		}
	}
	if p.DevPay.Enabled && !relaxed {
		return errors.New("payment config: Payment.DevPay is only allowed with Mode=dev or Mode=test")
	}
	if (p.WechatPay.Endpoint != "" || p.Alipay.Gateway != "") && !relaxed {
		return errors.New("payment config: overriding the WeChat Pay endpoint or the Alipay gateway is only allowed with Mode=dev or Mode=test")
	}
	if p.Compliance.MinorSpendingGate != "none" {
		return fmt.Errorf("payment config: Payment.Compliance.MinorSpendingGate %q is unknown; the only implemented value is none", p.Compliance.MinorSpendingGate)
	}
	if !relaxed {
		for _, name := range []string{"wechat_pay", "alipay", "apple", "google"} {
			if p.Enabled(name) {
				return fmt.Errorf("payment config: %s cannot be enabled in production mode before the real-name and minor spending limit gate exists "+
					"(docs/design/payment-recharge.md §12, §22.8); mini-game channels rely on the platform's own limits", name)
			}
		}
	}
	seen := map[string]bool{}
	for _, platform := range p.Platforms {
		allowed, known := platformChannels[platform.Platform]
		if !known {
			return fmt.Errorf("payment config: unknown platform %q in Payment.Platforms", platform.Platform)
		}
		if seen[platform.Platform] {
			return fmt.Errorf("payment config: platform %q is listed twice", platform.Platform)
		}
		seen[platform.Platform] = true
		for _, name := range platform.Channels {
			if !contains(allowed, name) {
				return fmt.Errorf("payment config: channel %q cannot be offered on platform %q", name, platform.Platform)
			}
			if !p.Enabled(name) {
				return fmt.Errorf("payment config: platform %q lists channel %q, which is not enabled", platform.Platform, name)
			}
		}
	}
	skus := map[uint64]bool{}
	for _, product := range p.Products {
		if product.SkuId == 0 || skus[product.SkuId] {
			return fmt.Errorf("payment config: Payment.Products has a zero or repeated SkuId %d", product.SkuId)
		}
		skus[product.SkuId] = true
	}
	if p.WechatMinigame.Enabled && p.WechatMinigame.Env == 1 && !relaxed {
		return errors.New("payment config: the WeChat mini-game sandbox (Env=1) is only allowed with Mode=dev or Mode=test")
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// checkBaseURL:scheme://主机[:端口],不带查询串、片段与用户信息。生产模式只接受 https(回调里带着支付结果)。
func checkBaseURL(field, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "${") {
		return fmt.Errorf("payment config: %s must be an absolute URL without query, fragment or user info", field)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("payment config: %s must use https outside Mode=dev/test", field)
		}
	default:
		return fmt.Errorf("payment config: %s must use http or https", field)
	}
	return nil
}
