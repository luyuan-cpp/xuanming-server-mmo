// Package dsclient 收口 Go 调用方构造 data_service zrpc 客户端的公共配置规则。
package dsclient

import "github.com/zeromicro/go-zero/zrpc"

// Conf 返回建 data_service 客户端用的配置:无条件关掉 zrpc 按「目标 + 方法」的客户端熔断,yaml 写开也不认。
//
// data_service 背后按逻辑库分片(目录 64 片、存档库按 storage_id 各一),不同逻辑库可落在不同集群。
// 保留客户端按方法熔断时,单个集群失联且流量占比过三分之一(或客户端短超时在服务端熔断前触发
// DeadlineExceeded)就会把同一方法上指向健康逻辑库的调用也挡掉。隔离改由服务端在建连处按逻辑库做
// (docs/design/data-service-breaker-per-logical-db.md §4.2、§5.2)。
// 所有构造 data_service 客户端的地方都必须过本函数,由 dsclient_test.go 扫 go/ 源码机械守住。
func Conf(c zrpc.RpcClientConf) zrpc.RpcClientConf {
	c.Middlewares.Breaker = false
	return c
}
