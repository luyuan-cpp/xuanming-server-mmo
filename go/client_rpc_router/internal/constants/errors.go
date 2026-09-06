// Package constants 是路由服写进 MessageContent.error_message 的 tip 码。
//
// 路由服自己没有业务域段:它拒绝转发时对客户端表现为「参数非法」或
// 「服务不可用」,直接复用 common 段的两个码(设计文档 §3)。
// 码值一律引用导表器生成的枚举(AGENTS.md §7.5),不许手写数字;
// errors_test.go 的 TestNoHandWrittenTipCodes 机械守住这条。
package constants

import "shared/generated/pb/table"

const (
	// ErrInvalidParameter:消息号不在路由表里,或所属 service 不是客户端协议
	// (gate 白名单放过来的都应该在表里;到这里说明 gate 与路由服的生成物版本不一致)。
	ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)
	// ErrServiceUnavailable:目标是 Battle(战斗只走直连,D33)、目标类型没有
	// 可用实例、拨号失败,或目标返回 gRPC 错误 / 超时。
	ErrServiceUnavailable = uint32(table.CommonError_kServiceUnavailable)
)
