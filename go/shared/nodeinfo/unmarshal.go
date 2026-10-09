// Package nodeinfo 是 Go 侧解析 etcd 服务发现值(protojson 序列化的 NodeInfo)的唯一入口
// (docs/design/k8s-client-entry.md D77)。
//
// # 为什么必须忽略未知字段
//
// NodeInfo 会随发布新增字段(例如 client_endpoint=11),而滚动升级期间新旧进程并存:新 gate /
// battle 写进 etcd 的值带着新字段,旧解析方用默认严格模式(protojson.Unmarshal)会整条报错。
// 报错的后果不是"少一个字段",而是整台节点从视图里消失 —— 旧 player_locator 会把整台 gate
// 判为缺席,两轮对账后把其上会话大面积打成 DISCONNECTING;旧 login 会报 no gate available。
// 所以所有读 NodeInfo 的 Go 调用点一律经本包解析,且这条宽松策略**永久保留、不随回退撤销**。
//
// # 契约
//
//   - 只放宽"未知字段名"与"未知的枚举名":按 protojson DiscardUnknown 语义,未知枚举名
//     (字符串形式)被静默忽略 —— 单值字段保持零值,repeated / map 里的该元素被丢弃,不报错。
//     NodeInfo 目前没有枚举字段;以后给注册消息加枚举时,别指望未知枚举值会在这里报错,
//     消费方要自己把零值当"未知"处理。
//   - 已知字段的类型错误、JSON 语法错误仍然返回 error,调用方沿用原有的失败处理
//     (跳过该条 / 当作非 NodeInfo 值)。
//   - 只用于读 etcd 里的节点注册值;写入侧(protojson.Marshal)不变。客户端请求等需要严格
//     校验的输入不要走本包。
//   - 用 encoding/json 解析镜像结构的 watcher 天然容忍未知字段,不需要改走本包。
//   - 无状态、并发安全。
package nodeinfo

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Unmarshal 以 DiscardUnknown 解析 protojson 编码的 NodeInfo(或其他节点注册消息)到 m。
// 未知字段与未知枚举名被丢弃(见包注释「契约」);JSON 语法错误、已知字段类型不符仍返回 error。
func Unmarshal(b []byte, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
}
