package serverbase

import (
	"context"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"

	"shared/grpcstats"
)

// EventInbandFault 是 in-band 故障的**稳定事件名**。
// 日志检索、告警规则、压测判据都认这个字符串,改它等于改对外契约。
const EventInbandFault = "rpc_inband_fault"

// EventInbandUnknownCode 是"业务码超出已知码表"的稳定事件名。
const EventInbandUnknownCode = "rpc_inband_unknown_code"

// Options 配置 in-band 故障拦截器。零值可用(只是 error_code 分支不判故障)。
type Options struct {
	// ErrorCodeClassifier 定性 `error_code` 字段里的码。
	//
	// **没有安全的默认值**:data_service 与 scene_manager 各有一套私有码表,
	// 数值区间还和 tip 码表重叠(tip 的 1 是 kSuccess,data_service 的 1 是
	// Redis 失败)。留 nil 时,error_code 分支的非 0 码一律只记成
	// VerdictBizReject —— 少报,但绝不误报。
	// 用 FaultCodeSet 三行就能给出本服务的定性函数。
	ErrorCodeClassifier Classifier

	// TipClassifier 定性 TipInfoMessage.id。留 nil 用 TipVerdict。
	// 一般不需要覆盖;个别服务想微调某个码的定性时才传。
	TipClassifier Classifier

	// SuppressFaultLog 为 true 时只记指标不打日志。
	// 压测期某条方法已知会大量返回故障码、日志已成噪音时用,平时别开。
	SuppressFaultLog bool

	// MethodName 把 gRPC 全方法名压成 Prometheus label 用的短名。
	// 留 nil 用 grpcstats.ShortMethod("/pkg.Svc/M" → "Svc/M")。
	MethodName func(fullMethod string) string
}

// Classify 按业务码的**出处**选对应码表的定性函数。
// 单独导出是为了可测:这是整个拦截器里唯一有分支逻辑的部分。
func (o Options) Classify(bc BizCode) Verdict {
	switch bc.Source {
	case SourceTipInfo:
		if o.TipClassifier != nil {
			return o.TipClassifier(bc.Code)
		}
		return TipVerdict(bc.Code)

	case SourceErrorCode:
		if o.ErrorCodeClassifier != nil {
			return o.ErrorCodeClassifier(bc.Code)
		}
		// 码表未知:0 是成功,非 0 只敢说"失败了",不敢说"是故障"。
		if bc.Code == 0 {
			return VerdictOK
		}
		return VerdictBizReject

	default:
		// 响应体里压根没有业务码字段,只要 handler 没返错就是成功。
		return VerdictOK
	}
}

func (o Options) methodName(fullMethod string) string {
	if o.MethodName != nil {
		return o.MethodName(fullMethod)
	}
	return grpcstats.ShortMethod(fullMethod)
}

// UnaryInterceptor 返回一个把 in-band 业务失败翻成日志 + 指标的拦截器。
//
// 它**不改动响应内容、不吞错、不把业务码翻成 gRPC status**
// —— 只观测。挂上它对客户端完全无感。
func UnaryInterceptor(opts Options) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)

		method := opts.methodName(info.FullMethod)

		// handler 真的返回了 error:gRPC status 已经不是 OK,
		// go-zero 自带的那套会记它,这里只补一条耗时,不重复告警。
		if err != nil {
			observeTransportError(method, elapsed)
			return resp, err
		}

		bc := ExtractBizCode(resp)
		verdict := opts.Classify(bc)
		observe(method, bc, verdict, elapsed)

		if !opts.SuppressFaultLog {
			switch verdict {
			case VerdictFault:
				logx.Errorw(EventInbandFault,
					logx.Field("method", info.FullMethod),
					logx.Field("code", bc.Code),
					logx.Field("code_source", bc.Source.String()),
					logx.Field("code_domain", codeDomainLabel(bc)),
					logx.Field("latency_ms", elapsed.Milliseconds()),
				)
			case VerdictUnknown:
				logx.Errorw(EventInbandUnknownCode,
					logx.Field("method", info.FullMethod),
					logx.Field("code", bc.Code),
					logx.Field("code_source", bc.Source.String()),
				)
			}
		}

		return resp, nil
	}
}

// codeDomainLabel 只对 tip 码给得出域名;私有码表没有域的概念。
func codeDomainLabel(bc BizCode) string {
	if bc.Source == SourceTipInfo {
		return TipDomain(bc.Code)
	}
	return "service_private"
}
