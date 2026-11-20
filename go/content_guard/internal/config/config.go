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

// CheckTextFullMethod 是 CheckText 的 gRPC 方法全名,Stat 内容屏蔽校验用。
// server 包的测试核对它与生成的常量相同。
const CheckTextFullMethod = "/content_guard.ContentGuard/CheckText"

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
	return c.validateStatContentSuppressed()
}

// validateStatContentSuppressed 确认 CheckText 的请求内容不会被 go-zero 的 Stat 拦截器打进日志:
// 请求里是玩家的聊天原文和名字,不该进日志,量也太大。要么整体关掉 Stat,要么把 CheckText 列进 IgnoreContentMethods。
func (c *Config) validateStatContentSuppressed() error {
	if !c.Middlewares.Stat {
		return nil
	}
	for _, method := range c.Middlewares.StatConf.IgnoreContentMethods {
		if method == CheckTextFullMethod {
			return nil
		}
	}
	return fmt.Errorf("Middlewares.StatConf.IgnoreContentMethods must contain %q (otherwise go-zero logs player text), or set Middlewares.Stat=false", CheckTextFullMethod)
}
