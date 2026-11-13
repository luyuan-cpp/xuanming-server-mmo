package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"payment/internal/channel"
	"payment/internal/config"
	"payment/internal/core"
	paymentpb "proto/payment"
)

// buildChannels 按配置装配开启的渠道。密钥只从配置点名的环境变量读;缺了、太短、带首尾空白都拒启,报错只点变量名。
func buildChannels(c config.Config, lookup func(string) (string, bool)) ([]channel.Channel, error) {
	p := c.Payment
	notifyURL := func(name string) string { return strings.TrimRight(p.NotifyBaseURL, "/") + "/notify/" + name }
	var out []channel.Channel
	if p.DevPay.Enabled {
		secret, err := secretValue(lookup, "Payment.DevPay.SecretEnv", p.DevPay.SecretEnv, 32)
		if err != nil {
			return nil, err
		}
		ch, err := channel.NewDevPay(secret, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if w := p.WechatPay; w.Enabled {
		privateKey, err := pemValue(lookup, "Payment.WechatPay.PrivateKeyEnv", w.PrivateKeyEnv, channel.ParsePrivateKey)
		if err != nil {
			return nil, err
		}
		publicKey, err := pemValue(lookup, "Payment.WechatPay.PlatformPublicKeyEnv", w.PlatformPublicKeyEnv, channel.ParsePublicKey)
		if err != nil {
			return nil, err
		}
		apiV3Key, err := secretValue(lookup, "Payment.WechatPay.APIv3KeyEnv", w.APIv3KeyEnv, 32)
		if err != nil {
			return nil, err
		}
		ch, err := channel.NewWechatPay(channel.WechatPayConfig{AppID: w.AppID, MchID: w.MchID, MerchantSerialNo: w.MerchantSerialNo, MerchantKey: privateKey,
			APIv3Key: []byte(apiV3Key), PlatformKeyID: w.PlatformPublicKeyID, PlatformKey: publicKey, NotifyURL: notifyURL(channel.NameWechatPay),
			Description: w.Description, Endpoint: w.Endpoint})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if a := p.Alipay; a.Enabled {
		privateKey, err := pemValue(lookup, "Payment.Alipay.PrivateKeyEnv", a.PrivateKeyEnv, channel.ParsePrivateKey)
		if err != nil {
			return nil, err
		}
		publicKey, err := pemValue(lookup, "Payment.Alipay.PublicKeyEnv", a.PublicKeyEnv, channel.ParsePublicKey)
		if err != nil {
			return nil, err
		}
		ch, err := channel.NewAlipay(channel.AlipayConfig{AppID: a.AppID, AppKey: privateKey, AlipayKey: publicKey, SellerID: a.SellerID,
			Gateway: a.Gateway, NotifyURL: notifyURL(channel.NameAlipay), Subject: a.Subject})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if w := p.WechatMinigame; w.Enabled {
		appSecret, err := secretValue(lookup, "Payment.WechatMinigame.AppSecretEnv", w.AppSecretEnv, 16)
		if err != nil {
			return nil, err
		}
		appKey, err := secretValue(lookup, "Payment.WechatMinigame.AppKeyEnv", w.AppKeyEnv, 16)
		if err != nil {
			return nil, err
		}
		pushToken, err := secretValue(lookup, "Payment.WechatMinigame.PushTokenEnv", w.PushTokenEnv, 3)
		if err != nil {
			return nil, err
		}
		aesKey := ""
		if w.EncodingAESKeyEnv != "" {
			if aesKey, err = secretValue(lookup, "Payment.WechatMinigame.EncodingAESKeyEnv", w.EncodingAESKeyEnv, 43); err != nil {
				return nil, err
			}
		}
		ch, err := channel.NewWechatMinigame(channel.WechatMinigameConfig{AppID: w.AppID, AppSecret: appSecret, OfferID: w.OfferID, AppKey: appKey,
			Env: w.Env, ZoneID: w.ZoneID, PushToken: pushToken, EncodingAESKey: aesKey})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if d := p.DouyinMinigame; d.Enabled {
		pushToken, err := secretValue(lookup, "Payment.DouyinMinigame.PushTokenEnv", d.PushTokenEnv, 3)
		if err != nil {
			return nil, err
		}
		ch, err := channel.NewDouyinMinigame(channel.DouyinMinigameConfig{AppID: d.AppID, PushToken: pushToken, ZoneID: d.ZoneID, GoodName: d.GoodName})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if a := p.Apple; a.Enabled {
		privateKey, err := pemValue(lookup, "Payment.Apple.PrivateKeyEnv", a.PrivateKeyEnv, channel.ParseECPrivateKey)
		if err != nil {
			return nil, err
		}
		roots, err := pemValue(lookup, "Payment.Apple.RootCertificatesEnv", a.RootCertificatesEnv, channel.ParseCertificates)
		if err != nil {
			return nil, err
		}
		ch, err := channel.NewApple(channel.AppleConfig{BundleID: a.BundleID, AppAppleID: a.AppAppleID, IssuerID: a.IssuerID, KeyID: a.KeyID,
			PrivateKey: privateKey, RootCertificates: roots, AllowSandbox: a.AllowSandbox})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if g := p.Google; g.Enabled {
		raw, err := secretValue(lookup, "Payment.Google.ServiceAccountEnv", g.ServiceAccountEnv, 64)
		if err != nil {
			return nil, err
		}
		var account struct {
			ClientEmail string `json:"client_email"`
			PrivateKey  string `json:"private_key"`
		}
		if err := json.Unmarshal([]byte(raw), &account); err != nil || account.ClientEmail == "" || account.PrivateKey == "" {
			return nil, fmt.Errorf("payment config: %s does not hold a Google service account JSON key", g.ServiceAccountEnv)
		}
		privateKey, err := channel.ParsePrivateKey(account.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("payment config: the private key in %s cannot be read: %w", g.ServiceAccountEnv, err)
		}
		ch, err := channel.NewGoogle(channel.GoogleConfig{PackageName: g.PackageName, ClientEmail: account.ClientEmail, PrivateKey: privateKey,
			PushAudience: g.PushAudience, PushServiceAccount: g.PushServiceAccount})
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// secretValue 读一个单行密钥:变量名不能空,值至少 minBytes 字节,不能带首尾空白(常见于 Secret 带进的换行,原样发给渠道只会得到「签名错误」)。
func secretValue(lookup func(string) (string, bool), field, envName string, minBytes int) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("payment config: %s names no environment variable", field)
	}
	value, ok := lookup(envName)
	switch {
	case !ok || value == "":
		return "", fmt.Errorf("payment config: %s=%s, but that environment variable is unset or empty", field, envName)
	case value != strings.TrimSpace(value):
		return "", fmt.Errorf("payment config: the value of %s has leading or trailing whitespace", envName)
	case len(value) < minBytes:
		return "", fmt.Errorf("payment config: the value of %s is shorter than %d bytes", envName, minBytes)
	}
	return value, nil
}

// pemValue 读一段多行的密钥或证书(允许首尾换行)并解析。
func pemValue[T any](lookup func(string) (string, bool), field, envName string, parse func(string) (T, error)) (T, error) {
	var zero T
	if envName == "" {
		return zero, fmt.Errorf("payment config: %s names no environment variable", field)
	}
	value, ok := lookup(envName)
	if !ok || strings.TrimSpace(value) == "" {
		return zero, fmt.Errorf("payment config: %s=%s, but that environment variable is unset or empty", field, envName)
	}
	parsed, err := parse(value)
	if err != nil {
		return zero, fmt.Errorf("payment config: the value of %s cannot be read: %w", envName, err)
	}
	return parsed, nil
}

var platformNames = map[string]paymentpb.PayClientPlatform{
	"android_app":     paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_ANDROID_APP,
	"ios_app":         paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_IOS_APP,
	"wechat_minigame": paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_WECHAT_MINIGAME,
	"douyin_minigame": paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_DOUYIN_MINIGAME,
	"desktop":         paymentpb.PayClientPlatform_PAY_CLIENT_PLATFORM_DESKTOP,
}

var channelNames = map[string]paymentpb.PayChannel{
	channel.NameDevPay:         paymentpb.PayChannel_PAY_CHANNEL_DEVPAY,
	channel.NameWechatPay:      paymentpb.PayChannel_PAY_CHANNEL_WECHAT_PAY,
	channel.NameAlipay:         paymentpb.PayChannel_PAY_CHANNEL_ALIPAY,
	channel.NameWechatMinigame: paymentpb.PayChannel_PAY_CHANNEL_WECHAT_MINIGAME,
	channel.NameDouyinMinigame: paymentpb.PayChannel_PAY_CHANNEL_DOUYIN_MINIGAME,
	channel.NameApple:          paymentpb.PayChannel_PAY_CHANNEL_APPLE,
	channel.NameGoogle:         paymentpb.PayChannel_PAY_CHANNEL_GOOGLE,
}

func platformChannels(list []config.PlatformConf) (map[paymentpb.PayClientPlatform][]paymentpb.PayChannel, error) {
	out := map[paymentpb.PayClientPlatform][]paymentpb.PayChannel{}
	for _, entry := range list {
		platform, ok := platformNames[entry.Platform]
		if !ok {
			return nil, errors.New("payment config: unknown platform " + entry.Platform)
		}
		for _, name := range entry.Channels {
			id, ok := channelNames[name]
			if !ok {
				return nil, errors.New("payment config: unknown channel " + name)
			}
			out[platform] = append(out[platform], id)
		}
	}
	return out, nil
}

func testPolicy(t config.TestConf) core.TestPolicy {
	policy := core.TestPolicy{Accept: t.Accept}
	if len(t.Players) > 0 {
		policy.Players = map[uint64]bool{}
		for _, player := range t.Players {
			policy.Players[player] = true
		}
	}
	return policy
}

func serviceOptions(c config.Config) core.Options {
	var products []core.Product
	for _, product := range c.Payment.Products {
		byChannel := map[string]string{}
		if product.Apple != "" {
			byChannel[channel.NameApple] = product.Apple
		}
		if product.Google != "" {
			byChannel[channel.NameGoogle] = product.Google
		}
		if product.WechatMinigame != "" {
			byChannel[channel.NameWechatMinigame] = product.WechatMinigame
		}
		products = append(products, core.Product{SkuID: product.SkuId, Title: product.Title, ByChannel: byChannel})
	}
	return core.Options{Products: products, TestPolicies: map[string]core.TestPolicy{
		channel.NameApple:          testPolicy(c.Payment.Apple.Test),
		channel.NameGoogle:         testPolicy(c.Payment.Google.Test),
		channel.NameWechatMinigame: testPolicy(c.Payment.WechatMinigame.Test),
	}}
}
