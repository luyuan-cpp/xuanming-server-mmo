// Package config 是路由服的配置结构与校验(docs/design/client-rpc-router.md §6)。
package config

import (
	"errors"
	"fmt"
	"time"

	base "proto/common/base"

	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是路由服的全部配置。路由服无状态:没有 Redis、Kafka、数据库,
// 只有 etcd(注册自己 + 发现目标业务服务)。
type Config struct {
	zrpc.RpcServerConf

	// ZoneId:C++ 约定 etcd 注册路径用的 zone id。ClientRpcRouterNodeService
	// 是全局池节点类型,zone 只影响本服务自己的注册路径,与转发时对目标实例的
	// zone 过滤无关(过滤用 ForwardRequest.zone_id)。
	ZoneId uint32 `json:",default=1"`

	// LeaseTTL:etcd 节点注册 keepalive 的租约 TTL(秒)。
	LeaseTTL int64 `json:",default=60"`

	// ForwardTimeoutMs:单次向目标业务服务 Invoke 的超时(毫秒)。
	// 超时按 kServiceUnavailable 回给客户端(设计文档 §3)。
	ForwardTimeoutMs int64 `json:",default=5000"`

	// ZoneScopedNodeTypes:只挑同 zone 实例的目标节点类型(ENodeType 枚举名,
	// 设计决策 D33)。留空按 DefaultZoneScopedNodeTypes。用字符串而非枚举值:
	// yaml 里写名字可读,启动时 ResolveZoneScopedNodeTypes 校验、非法名字 fail-fast。
	ZoneScopedNodeTypes []string `json:",optional"`

	// GlobalTargetSubsetSize:全服池目标类型(不在 ZoneScopedNodeTypes 里的)每条发布轨道,本路由服只在约这么多台里挑
	// (确定性子集,discovery/subset.go;docs/design/connection-fanout-subsetting-20261106.md §3.3)。同轨道各路由服的
	// 范围在环上均匀错开,每台目标收到的请求一样多、一台都不漏;一台路由服长期运行也只连这几台,不会连上全服全部目标。
	// 目标实例多于「同轨道路由服数 × 本值」时,每台路由服多连几台,保证每台目标都有路由服连。
	// 缺省 4;0 = 不取子集(在全部实例里挑);不许是 1:只挑一台时它一重启,本路由服对这类目标在租约到期前全部失败。
	// 实例数不多于本值时取全部。
	GlobalTargetSubsetSize int `json:",default=4"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。
	// 端口分工:9101=login / 9150=scene_manager / 9160=db / 9170=guild / 9180=friend /
	// 9190=player_locator;路由服开启时用 9200(设计文档写的 9180 与 friend 撞,待修正)。
	MetricsListenAddr string `json:",optional"`
}

// DefaultZoneScopedNodeTypes 是 ZoneScopedNodeTypes 留空时的默认值:
// 只有 login 按 zone 注册且只服务本 zone(与 gate 的 IsZoneScopedNodeType 一致)。
var DefaultZoneScopedNodeTypes = []string{base.ENodeType_name[int32(base.ENodeType_LoginNodeService)]}

// ForwardTimeout 把 ForwardTimeoutMs 换成 time.Duration。
func (c *Config) ForwardTimeout() time.Duration {
	return time.Duration(c.ForwardTimeoutMs) * time.Millisecond
}

// ResolveZoneScopedNodeTypes 把配置的枚举名解析成节点类型集合;
// 任一名字不在 ENodeType 里即返回错误(配错名字会让 login 走全局随机,
// 跨 zone 打到别的 login 上,必须在启动期拦住)。
func (c *Config) ResolveZoneScopedNodeTypes() (map[base.ENodeType]struct{}, error) {
	names := c.ZoneScopedNodeTypes
	if len(names) == 0 {
		names = DefaultZoneScopedNodeTypes
	}
	resolved := make(map[base.ENodeType]struct{}, len(names))
	for _, name := range names {
		value, ok := base.ENodeType_value[name]
		if !ok {
			return nil, fmt.Errorf("ZoneScopedNodeTypes 含未知节点类型名 %q(须是 ENodeType 枚举名)", name)
		}
		resolved[base.ENodeType(value)] = struct{}{}
	}
	return resolved, nil
}

// Validate 做启动期的整体校验(fail-fast):
//   - ForwardTimeoutMs 必须为正,否则每次转发都会立刻超时;
//   - zrpc 的 Timeout 必须为 0 或大于 ForwardTimeoutMs,否则服务端整体超时先于
//     目标超时触发,目标的正常慢响应会被误判成故障;
//   - ZoneScopedNodeTypes 必须全部可解析;
//   - Forward 必须在 Stat 拦截器的 IgnoreContentMethods 里(或整体关掉 Stat):
//     否则 go-zero 会把 ForwardRequest 整包 JSON 打进 INFO 日志,其中的 body
//     base64 一解就是目标请求原文,登录消息即明文密码。k8s ConfigMap 模板漏了
//     这一段时必须拒绝启动,而不是安静地把凭据写进日志。
func (c *Config) Validate() error {
	if c.ForwardTimeoutMs <= 0 {
		return errors.New("ForwardTimeoutMs 必须为正数(毫秒)")
	}
	if c.Timeout != 0 && c.Timeout <= c.ForwardTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须为 0 或大于 ForwardTimeoutMs(%d ms)", c.Timeout, c.ForwardTimeoutMs)
	}
	if _, err := c.ResolveZoneScopedNodeTypes(); err != nil {
		return err
	}
	if c.GlobalTargetSubsetSize < 0 || c.GlobalTargetSubsetSize == 1 {
		return fmt.Errorf("GlobalTargetSubsetSize (%d) must be 0 (no subsetting) or at least 2: with a single target per router, every forward of that type fails while that one instance restarts", c.GlobalTargetSubsetSize)
	}
	if err := c.validateStatContentSuppressed(); err != nil {
		return err
	}
	return nil
}

// ForwardFullMethod 是本服务唯一的 gRPC 方法全名,用于 Stat 内容屏蔽校验。
const ForwardFullMethod = "/client_rpc_router.ClientRpcRouter/Forward"

// validateStatContentSuppressed 确认 Forward 的请求内容不会被 go-zero 的 Stat
// 拦截器打进日志:要么整体关掉 Stat,要么把 Forward 列进 IgnoreContentMethods。
func (c *Config) validateStatContentSuppressed() error {
	if !c.Middlewares.Stat {
		return nil
	}
	for _, method := range c.Middlewares.StatConf.IgnoreContentMethods {
		if method == ForwardFullMethod {
			return nil
		}
	}
	return fmt.Errorf("Middlewares.StatConf.IgnoreContentMethods 必须包含 %q(否则 go-zero Stat 拦截器会把转发请求整包 JSON 打进 INFO 日志,登录消息即明文密码);或显式设 Middlewares.Stat=false", ForwardFullMethod)
}
