// Package serverbase 提供各 Go 服务共用的 gRPC 服务端基础拦截器。
//
// # 它解决的问题
//
// 本仓的业务失败**不走 gRPC status**:handler 一律 `return resp, nil`,
// 把失败塞进响应体的业务码字段(in-band 错误码)。后果是——
//
//   - gRPC status 恒 OK,go-zero 自带的 metric/日志拦截器把每一次
//     "Redis 挂了""场景节点全不可用"都记成一次成功请求;
//   - 监控上看不出任何异常,直到玩家投诉。
//
// 本包在 handler 外面加一层:把响应体里的业务码读出来定性,
// 属于**服务端内部故障**的打稳定事件名日志 + Prometheus counter,
// 属于**正常业务拒绝**(背包满、队伍满、冷却未到)的只计数不告警。
//
// # 三套互不兼容的码表(踩过就知道有多重要)
//
// 全仓的 in-band 码**不是一套**,至少三套,数值区间还高度重叠:
//
//  1. tip 码表(1000 起,单命名空间,每域 1000 槽的分段轴):
//     由 `TipInfoMessage error_message` 字段承载,见
//     generated/code/proto/tip/*.proto、Go 常量在 shared/generated/pb/table。
//     login / friend / guild / chat / instance / match 走这套。
//  2. data_service 私有码表(0..17):`uint32 error_code`,
//     见 go/data_service/internal/constants/error_codes.go。
//     其低位值与 tip 轴没有任何对应关系。
//  3. scene_manager 私有码表(0..16):`uint32 error_code`,
//     见 go/scene_manager/internal/constants/errors.go。
//
// 所以本包**不提供"一个函数判所有码"**的假象:
//
//   - 从 TipInfoMessage 读出来的码,类型本身就把码表钉死了,直接用 TipVerdict;
//   - 从 error_code 读出来的码,码表由服务自己定义,必须由服务通过
//     Options.ErrorCodeClassifier 传入定性函数(用 FaultCodeSet 三行搞定)。
//     不传 = 只统计"业务失败"、绝不擅自判成故障,免得刷出满屏假告警。
//
// # 用法
//
//	itc := serverbase.UnaryInterceptor(serverbase.Options{
//	    // scene_manager 的例子:哪些私有码属于服务端内部故障
//	    ErrorCodeClassifier: serverbase.FaultCodeSet(
//	        constants.ErrNoAvailableNode,
//	        constants.ErrRedis,
//	        constants.ErrKafkaRoute,
//	    ),
//	})
//	server.AddUnaryInterceptors(itc)
//
// # 低基数纪律
//
// Prometheus label 只放 method(gRPC 方法名,有界)、source(3 个取值)、
// code(只在判成故障时才带,取值被故障码集合限死)。
// **绝不放 player_id / scene_id / request_id**(见仓库 CLAUDE.md §9)。
//
// # 边界
//
// 本包只做观测,不改任何响应内容、不吞错、不把 in-band 码翻译成 gRPC status
// ——那会改变现有服务的对外行为,是另一件事。
package serverbase
