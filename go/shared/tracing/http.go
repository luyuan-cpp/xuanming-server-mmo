package tracing

import (
	"net/http"

	ztrace "github.com/zeromicro/go-zero/core/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// DoHTTP 经 client 发出 request,前后包一个 client span,并把它按全局传播器写进请求头(traceparent 等,设计 §6.2)。
//
// 给调用我们自己服务的裸 http.Client 用(trade 调商城)。做法同 go-zero rest/httpc 的出站请求:span 按 URL 路径命名,
// 先开 span 再注入;不用 httpc 本身,是因为它还带熔断与逐请求日志,会改掉调用方原有的超时、重定向与失败语义。
// 调 K8s API 这类外部基础设施的客户端不用它,那不是这条链路上的服务。
//
// request 的 ctx 里没有 span 时开新根(同 UnaryClientInterceptor)。传输错误与 5xx 把 span 标成 Error;
// span 在拿到应答头时结束,读应答体不计在内。返回值与 client.Do 完全相同。
func DoHTTP(client *http.Client, request *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer(ztrace.TraceName).Start(request.Context(), request.URL.Path,
		trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	request = request.WithContext(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(request.Header))

	response, err := client.Do(request)
	switch {
	case err != nil:
		span.SetStatus(codes.Error, err.Error())
	case response.StatusCode >= http.StatusInternalServerError:
		span.SetStatus(codes.Error, http.StatusText(response.StatusCode))
	}
	return response, err
}
