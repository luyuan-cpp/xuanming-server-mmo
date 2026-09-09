package idsegment

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
)

// Response 是 AllocateIdSegmentResponse 在本包眼里的最小视图。
//
// 不直接 import proto/data_service:shared 模块刻意不依赖 proto 模块(go.mod 边界,
// shared 今天没有一处引用服务协议),生成的响应类型靠方法集约束进来。
type Response interface {
	GetErrorCode() uint32
	GetLo() uint64
	GetHi() uint64
}

// RPCError 表示服务端在响应体里回了非 0 的 error_code(gRPC status 仍是 OK)。
// 码值原样带出来,由调用方 / 日志定性;本包不解释它的含义。
type RPCError struct {
	BizTag string
	Code   uint32
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("idsegment: AllocateIdSegment(biz_tag=%s) returned error_code=%d", e.BizTag, e.Code)
}

// Adapt 把生成的 gRPC 客户端方法(data_service.DataServiceClient.AllocateIdSegment)包成
// AllocateFunc:
//
//	idsegment.Adapt(cli.AllocateIdSegment, func(tag string, step uint32) *dspb.AllocateIdSegmentRequest {
//		return &dspb.AllocateIdSegmentRequest{BizTag: tag, Step: step}
//	})
//
// build 负责构造请求消息 —— 这一行留给调用方,理由见 Response。
// 传输层错误原样返回;error_code != 0 映射成 *RPCError。
func Adapt[Req any, Resp Response](
	call func(ctx context.Context, in Req, opts ...grpc.CallOption) (Resp, error),
	build func(bizTag string, step uint32) Req,
) AllocateFunc {
	return func(ctx context.Context, bizTag string, step uint32) (uint64, uint64, error) {
		resp, err := call(ctx, build(bizTag, step))
		if err != nil {
			return 0, 0, err
		}
		if code := resp.GetErrorCode(); code != 0 {
			return 0, 0, &RPCError{BizTag: bizTag, Code: code}
		}
		return resp.GetLo(), resp.GetHi(), nil
	}
}
