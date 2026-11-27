// Package config 是内容安全服务的配置结构与启动期校验(docs/design/rename-and-content-safety.md §6)。
package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"

	"content_guard/internal/lexstore"
)

// CheckTextFullMethod / CheckPlatformTextFullMethod 是带玩家正文请求体的 gRPC 方法全名,Stat 内容屏蔽校验用。
// server 包的测试核对它们与生成的常量相同。
const (
	CheckTextFullMethod         = "/content_guard.ContentGuard/CheckText"
	CheckPlatformTextFullMethod = "/content_guard.ContentGuard/CheckPlatformText"
)

const (
	minTextBytes = 64
	maxTextBytes = 64 * 1024
)

// Config 是内容安全服务的全部配置。服务无状态:没有数据库、Redis、Kafka,只连 etcd(读词库、注册自己、热关停规则)。
type Config struct {
	zrpc.RpcServerConf

	// ZoneId:etcd 注册路径用的 zone id。本服务全服一份,zone 只影响自己的注册路径。
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL:etcd 节点注册的租约 TTL(秒)。进程崩溃后,调用方最多在这么久之内还会选到它。
	LeaseTTL int64 `json:",default=60"`

	// MetricsListenAddr:Prometheus /metrics 与 /healthz 的监听地址,留空关闭。
	MetricsListenAddr string `json:",optional"`

	// KillSwitchPrefix:热关停规则的 etcd 前缀,留空用 shared/killswitch 的缺省值。
	KillSwitchPrefix string `json:",optional"`

	// LexiconPrefix:词库在 etcd 里的前缀,必须以 / 结尾;留空用 lexstore.DefaultPrefix。
	LexiconPrefix string `json:",optional"`

	// RequireLexicon:为真时,没有从 etcd 加载到词库就不对外判定(CheckText 回 Unavailable,/healthz 回 503)。
	// 为假时只凭内置的几个防冒充词也照常判定,只给本机开发用。Mode 是 pre / pro 时必须为真。
	RequireLexicon bool `json:",default=false"`

	// MaxTextBytes:单次送检文字的字节上限,超过回 InvalidArgument。聊天正文上限是 512 字节,公告更长,缺省给 4096。
	MaxTextBytes int `json:",default=4096"`

	// Platform:可选外部平台内容安全审核配置(微信 msgSecCheck / 抖音 antidirt;docs/design/rename-and-content-safety.md §6.5)。
	// 未配置 AppID 时仅运行本地词库层;密钥通过 AppSecretEnv 从环境变量读取,不进配置文件。
	Platform PlatformConfig `json:",optional"`

	// DataServiceRpc:可选目录服务 RPC 客户端,用于在仅传入 player_id 时查询玩家渠道与 openid。
	DataServiceRpc zrpc.RpcClientConf `json:",optional"`
}

// MiniGamePlatformConf 是微信/抖音小游戏内容安全接口配置。
type MiniGamePlatformConf struct {
	AppID        string `json:",optional"`
	AppSecretEnv string `json:",optional"`
	BaseURL      string `json:",optional"`
}

// PlatformConfig 控制外部平台审核的接入与超时策略。
type PlatformConfig struct {
	WeChat          MiniGamePlatformConf `json:",optional"`
	Douyin          MiniGamePlatformConf `json:",optional"`
	SyncTimeoutMs   int                  `json:",default=1500"`
	WorldTimeoutMs  int                  `json:",default=250"`
	AsyncTimeoutMs  int                  `json:",default=2500"`
	LookupTimeoutMs int                  `json:",default=150"`
}

// HasDataServiceRpc 报告是否配置了目录服务连接目标。
func (c *Config) HasDataServiceRpc() bool {
	return len(c.DataServiceRpc.Endpoints) > 0 || c.DataServiceRpc.Target != "" ||
		(len(c.DataServiceRpc.Etcd.Hosts) > 0 && c.DataServiceRpc.Etcd.Key != "")
}

// EffectiveLexiconPrefix 返回实际使用的词库前缀。
func (c *Config) EffectiveLexiconPrefix() string {
	if c.LexiconPrefix == "" {
		return lexstore.DefaultPrefix
	}
	return c.LexiconPrefix
}

// Validate 做启动期的整体校验,不通过就不起服。
func (c *Config) Validate() error {
	if len(c.Etcd.Hosts) == 0 {
		return errors.New("Etcd.Hosts is required: the lexicon, the node registration and the kill switch all live in etcd")
	}
	if c.ZoneId == 0 {
		return errors.New("ZoneId must not be 0")
	}
	if c.LeaseTTL <= 0 {
		return errors.New("LeaseTTL must be positive (seconds)")
	}
	if c.MaxTextBytes < minTextBytes || c.MaxTextBytes > maxTextBytes {
		return fmt.Errorf("MaxTextBytes (%d) must be in [%d, %d]", c.MaxTextBytes, minTextBytes, maxTextBytes)
	}
	if c.LexiconPrefix != "" && !strings.HasSuffix(c.LexiconPrefix, "/") {
		return fmt.Errorf("LexiconPrefix (%q) must end with a slash", c.LexiconPrefix)
	}
	// 正式环境只凭内置的几个词对外判定,等于没有屏蔽字:宁可起不来。
	if (c.Mode == service.PreMode || c.Mode == service.ProMode) && !c.RequireLexicon {
		return fmt.Errorf("RequireLexicon must be true when Mode is %q", c.Mode)
	}
	if err := c.validatePlatform(); err != nil {
		return err
	}
	return c.validateStatContentSuppressed()
}

func (c *Config) validatePlatform() error {
	for _, entry := range []struct {
		name string
		conf MiniGamePlatformConf
	}{
		{"Platform.WeChat", c.Platform.WeChat},
		{"Platform.Douyin", c.Platform.Douyin},
	} {
		if entry.conf.AppID == "" && entry.conf.AppSecretEnv == "" {
			continue
		}
		if entry.conf.AppID == "" || strings.Contains(entry.conf.AppID, "${") {
			return fmt.Errorf("%s.AppID must be set and not contain unexpanded placeholders", entry.name)
		}
		if entry.conf.AppSecretEnv == "" || strings.Contains(entry.conf.AppSecretEnv, "${") {
			return fmt.Errorf("%s.AppSecretEnv must be an environment variable name", entry.name)
		}
	}
	if c.Platform.SyncTimeoutMs < 0 || c.Platform.SyncTimeoutMs > 5000 {
		return fmt.Errorf("Platform.SyncTimeoutMs (%d) must be within [0, 5000]", c.Platform.SyncTimeoutMs)
	}
	if c.Platform.WorldTimeoutMs < 0 || c.Platform.WorldTimeoutMs > 2000 {
		return fmt.Errorf("Platform.WorldTimeoutMs (%d) must be within [0, 2000]", c.Platform.WorldTimeoutMs)
	}
	if c.Platform.AsyncTimeoutMs < 0 || c.Platform.AsyncTimeoutMs > 10000 {
		return fmt.Errorf("Platform.AsyncTimeoutMs (%d) must be within [0, 10000]", c.Platform.AsyncTimeoutMs)
	}
	if c.Platform.LookupTimeoutMs < 0 || c.Platform.LookupTimeoutMs > 2000 {
		return fmt.Errorf("Platform.LookupTimeoutMs (%d) must be within [0, 2000]", c.Platform.LookupTimeoutMs)
	}
	if c.Platform.WeChat.AppID == "" && c.Platform.Douyin.AppID == "" {
		return nil
	}
	// 调用方(chat、guild、scene……)只带 player_id,渠道与 openid 要到目录里查。没配这条连接时每一次平台审核都会被
	// 悄悄跳过,只剩本地词库,而配置看起来是接了平台的:宁可起不来。
	if !c.HasDataServiceRpc() {
		return errors.New("DataServiceRpc is required when Platform.WeChat or Platform.Douyin is configured: callers send only the player id, and without the directory lookup every platform review would be skipped silently")
	}
	// zrpc 服务端的 Timeout 到点就掐断处理函数:小于最长的一段平台调用加查渠道身份时,那一段永远等不满(0 = 不设上限)。
	need := int64(max(orDefault(c.Platform.SyncTimeoutMs, 1500), orDefault(c.Platform.AsyncTimeoutMs, 2500)) + orDefault(c.Platform.LookupTimeoutMs, 150) + platformTimeoutMarginMs)
	if c.Timeout > 0 && c.Timeout < need {
		return fmt.Errorf("Timeout (%d ms) must be at least %d ms when a platform is configured: the longest platform call (Platform.SyncTimeoutMs / AsyncTimeoutMs) plus Platform.LookupTimeoutMs has to finish inside the server timeout", c.Timeout, need)
	}
	return nil
}

// platformTimeoutMarginMs 是平台调用之外留给本地判定、序列化与调度的余量。
const platformTimeoutMarginMs = 200

// orDefault 把「没写」(0)换成缺省值:三个平台时限写 0 时路由器用的就是这几个缺省值(platform.NewRouter)。
func orDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// validateStatContentSuppressed 确认带原文请求体的 RPC 不会被 go-zero 的 Stat 拦截器打进日志。
func (c *Config) validateStatContentSuppressed() error {
	if !c.Middlewares.Stat {
		return nil
	}
	ignored := make(map[string]bool, len(c.Middlewares.StatConf.IgnoreContentMethods))
	for _, method := range c.Middlewares.StatConf.IgnoreContentMethods {
		ignored[method] = true
	}
	for _, req := range []string{CheckTextFullMethod, CheckPlatformTextFullMethod} {
		if !ignored[req] {
			return fmt.Errorf("Middlewares.StatConf.IgnoreContentMethods must contain %q (otherwise go-zero logs player text), or set Middlewares.Stat=false", req)
		}
	}
	return nil
}
