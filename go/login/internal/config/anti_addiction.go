package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"shared/antiaddiction"
)

// AntiAddictionConf 是实名与未成年人防沉迷的部署配置(docs/design/real-name-anti-addiction.md §4、§5.8)。
// 整块缺省 = 关闭,海外部署就是这样。
type AntiAddictionConf struct {
	// Enabled 只在国内部署打开;开着时 Nppa 必须填全,否则拒绝启动。
	Enabled bool `json:",optional"`
	// PlatformManagedAuthTypes 是由渠道平台实名并执行防沉迷的 auth_type(微信、抖音小游戏)。不在名单里的 auth_type
	// 一律要游戏方自己实名,新加的登录方式默认也要(fail-closed)。没有缺省值:国内部署要显式写出来。
	PlatformManagedAuthTypes []string `json:",optional"`
	// ExtraPlayDays 是未成年人除周五到周日之外的可玩日,北京日期 2006-01-02:法定节假日中不落在周五到周日的那些天,
	// 运维按每年的国务院放假通知填。漏填只会更严。
	ExtraPlayDays []string `json:",optional"`
	// DailySubmitLimit 是每个账号每天提交实名的次数上限:不设上限,这个接口就能被拿来批量核验别人的姓名和身份证号。
	DailySubmitLimit int      `json:",default=5"`
	Nppa             NppaConf `json:",optional"`
}

// NppaConf 是国家新闻出版署网络游戏防沉迷实名认证系统的接入参数。地址留空用官方地址,接口测试阶段换成企业端给的测试地址。
type NppaConf struct {
	AppId string `json:",optional"`
	BizId string `json:",optional"` // 游戏备案识别码
	// SecretKeyEnv 是 secretKey 所在的环境变量名。值只从 Secret 注入,不写进配置文件。
	SecretKeyEnv string `json:",optional"`
	CheckUrl     string `json:",optional"`
	QueryUrl     string `json:",optional"`
	LoginoutUrl  string `json:",optional"`
}

// 部署脚本开了防沉迷(k8s_deploy.ps1 -LoginNppaAppId / -LoginNppaBizId)时注入的期望值。K8s 上 login 是 SQL 写者,常驻进程读运维
// 抄进 runtimeConfigSecret 的配置:漏抄 AntiAddiction 段时 login 照样起得来、防沉迷却是关的,抄成别的应用则国家系统全部验签失败。
// 启动时与配置比,不一致拒启(同 data_service 的 MMORPG_ID_ISSUER_EXPECTED_REGION)。本机与 docker 联调没有这两个变量,不比。
const (
	ExpectedNppaAppIdEnv = "MMORPG_LOGIN_NPPA_EXPECTED_APP_ID"
	ExpectedNppaBizIdEnv = "MMORPG_LOGIN_NPPA_EXPECTED_BIZ_ID"
)

// checkExpectedAntiAddiction 比部署期望与实际读到的配置。两个变量都没有 = 部署没开或不在 K8s 上,不比;
// 只有一个、开关关着、AppId / BizId 不同,一律拒绝(fail-closed)。按字节比,不 trim:部署往两处写的是同一个值。
func checkExpectedAntiAddiction(c AntiAddictionConf, lookup func(string) (string, bool)) error {
	wantAppId, _ := lookup(ExpectedNppaAppIdEnv)
	wantBizId, _ := lookup(ExpectedNppaBizIdEnv)
	if wantAppId == "" && wantBizId == "" {
		return nil
	}
	if !c.Enabled {
		return fmt.Errorf("the deployment turns anti-addiction on (%s / %s are injected) but AntiAddiction.Enabled is false: copy the AntiAddiction section of the rendered login ConfigMap into the runtime config", ExpectedNppaAppIdEnv, ExpectedNppaBizIdEnv)
	}
	if c.Nppa.AppId != wantAppId || c.Nppa.BizId != wantBizId {
		return fmt.Errorf("AntiAddiction.Nppa.AppId / BizId differ from the deployment (%s / %s): fix the runtime config", ExpectedNppaAppIdEnv, ExpectedNppaBizIdEnv)
	}
	return nil
}

// resolveAntiAddiction 在任何外部连接之前核防沉迷配置并取出 secretKey(ResolveSecrets 调)。先与部署期望比;关着时别的都不查。
// 错误文案不带密钥的值。
func resolveAntiAddiction(c AntiAddictionConf, lookup func(string) (string, bool)) (string, error) {
	if err := checkExpectedAntiAddiction(c, lookup); err != nil {
		return "", err
	}
	if !c.Enabled {
		return "", nil
	}
	if strings.TrimSpace(c.Nppa.AppId) == "" || strings.TrimSpace(c.Nppa.BizId) == "" {
		return "", errors.New("AntiAddiction.Enabled requires AntiAddiction.Nppa.AppId and BizId")
	}
	if c.Nppa.SecretKeyEnv == "" {
		return "", errors.New("AntiAddiction.Enabled requires AntiAddiction.Nppa.SecretKeyEnv (the NPPA secretKey only comes from the environment)")
	}
	secret, ok := lookup(c.Nppa.SecretKeyEnv)
	if !ok || secret == "" {
		return "", fmt.Errorf("environment variable %s (AntiAddiction.Nppa.SecretKeyEnv) is missing or empty", c.Nppa.SecretKeyEnv)
	}
	if key, err := hex.DecodeString(secret); err != nil || len(key) != 16 {
		return "", fmt.Errorf("environment variable %s must hold the 32-hex-character NPPA secretKey", c.Nppa.SecretKeyEnv)
	}
	for name, raw := range map[string]string{"CheckUrl": c.Nppa.CheckUrl, "QueryUrl": c.Nppa.QueryUrl, "LoginoutUrl": c.Nppa.LoginoutUrl} {
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("AntiAddiction.Nppa.%s must be an http(s) URL", name)
		}
	}
	if _, err := antiaddiction.NewCalendar(c.ExtraPlayDays); err != nil {
		return "", fmt.Errorf("AntiAddiction.ExtraPlayDays: %w", err)
	}
	if c.DailySubmitLimit < 1 {
		return "", errors.New("AntiAddiction.DailySubmitLimit must be at least 1")
	}
	return secret, nil
}
