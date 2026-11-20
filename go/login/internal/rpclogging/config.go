// Package rpclogging 统一登录入口的请求体日志边界。
package rpclogging

import (
	loginpb "proto/login"

	"github.com/zeromicro/go-zero/zrpc"
)

// ProtectCredentials 必须在创建 zrpc 服务之前调用。Stat 拦截器在业务拦截器外层
// 序列化原请求，业务层脱敏不能保护这里；仅屏蔽敏感方法的内容，保留统计和慢调用日志。
// 已有运维屏蔽项与其他中间件配置原样保留，重复调用不会重复追加。
func ProtectCredentials(cfg *zrpc.RpcServerConf) {
	methods := []string{
		loginpb.ClientPlayerLogin_Login_FullMethodName,
		loginpb.ClientPlayerLogin_RefreshToken_FullMethodName,
		loginpb.LoginPreGate_AssignGate_FullMethodName,
		loginpb.LoginPreGate_QueryQueueStatus_FullMethodName,
		// 请求体是姓名和身份证号(docs/design/real-name-anti-addiction.md §5.2)。
		loginpb.LoginPreGate_SubmitRealName_FullMethodName,
	}
	listed := make(map[string]struct{}, len(cfg.Middlewares.StatConf.IgnoreContentMethods)+len(methods))
	for _, method := range cfg.Middlewares.StatConf.IgnoreContentMethods {
		listed[method] = struct{}{}
	}
	for _, method := range methods {
		if _, exists := listed[method]; !exists {
			cfg.Middlewares.StatConf.IgnoreContentMethods = append(cfg.Middlewares.StatConf.IgnoreContentMethods, method)
			listed[method] = struct{}{}
		}
	}
}
