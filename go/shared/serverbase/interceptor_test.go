package serverbase

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc"

	"shared/generated/pb/table"
)

func TestOptionsClassify(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		bc   BizCode
		want Verdict
	}{
		{
			name: "无业务码字段一律成功",
			bc:   BizCode{Source: SourceNone},
			want: VerdictOK,
		},
		{
			name: "tip 码走 tip 码表:背包满是业务拒绝",
			bc:   BizCode{Code: uint32(table.BagError_kBagAddItemBagFull), Source: SourceTipInfo},
			want: VerdictBizReject,
		},
		{
			name: "tip 码走 tip 码表:Redis 出错是故障",
			bc:   BizCode{Code: uint32(table.LoginError_kLoginRedisError), Source: SourceTipInfo},
			want: VerdictFault,
		},
		{
			// 关键回归点:data_service 的 error_code=1 是 Redis 失败,
			// 而 tip 的 1 是 kSuccess。没配 ErrorCodeClassifier 时
			// 绝不能拿 tip 码表去判它 —— 那会把失败读成成功。
			name: "error_code 未配 Classifier:非 0 只算业务拒绝,绝不套 tip 码表",
			bc:   BizCode{Code: 1, Source: SourceErrorCode},
			want: VerdictBizReject,
		},
		{
			name: "error_code 未配 Classifier:0 是成功",
			bc:   BizCode{Code: 0, Source: SourceErrorCode},
			want: VerdictOK,
		},
		{
			name: "error_code 配了 Classifier:命中故障集合",
			opts: Options{ErrorCodeClassifier: FaultCodeSet(1, 8)},
			bc:   BizCode{Code: 8, Source: SourceErrorCode},
			want: VerdictFault,
		},
		{
			name: "error_code 配了 Classifier:未命中集合是业务拒绝",
			opts: Options{ErrorCodeClassifier: FaultCodeSet(1, 8)},
			bc:   BizCode{Code: 9, Source: SourceErrorCode},
			want: VerdictBizReject,
		},
		{
			name: "TipClassifier 可覆盖",
			opts: Options{TipClassifier: func(uint32) Verdict { return VerdictFault }},
			bc:   BizCode{Code: uint32(table.BagError_kBagAddItemBagFull), Source: SourceTipInfo},
			want: VerdictFault,
		},
		{
			name: "tip 码超界记成码表漂移",
			bc:   BizCode{Code: TipMaxKnownCode + 1, Source: SourceTipInfo},
			want: VerdictUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.Classify(tt.bc); got != tt.want {
				t.Fatalf("Classify(%+v) = %v, 期望 %v", tt.bc, got, tt.want)
			}
		})
	}
}

// newUnaryInfo 造一个 gRPC 方法描述。
func newUnaryInfo(fullMethod string) *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: fullMethod}
}

// handlerReturning 返回一个固定应答的 handler。
func handlerReturning(resp any, err error) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) { return resp, err }
}

func faultCount(method, source string, code uint32) float64 {
	return testutil.ToFloat64(inbandFaultTotal.WithLabelValues(
		method, source, strconv.FormatUint(uint64(code), 10)))
}

func rejectCount(method, source string) float64 {
	return testutil.ToFloat64(inbandRejectTotal.WithLabelValues(method, source))
}

func unknownCount(method, source string) float64 {
	return testutil.ToFloat64(inbandUnknownCodeTotal.WithLabelValues(method, source))
}

func TestUnaryInterceptorMetricsAndLog(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		// method 每个用例都用**独立**的全方法名,这样 counter 是新的、
		// 断言可以直接比绝对值,不受用例执行顺序影响。
		fullMethod string
		resp       any
		handlerErr error

		wantFaultCode  uint32 // 0 表示不期望 fault
		wantFaultSrc   string
		wantReject     bool
		wantRejectSrc  string
		wantUnknown    bool
		wantUnknownSrc string
		wantLogEvent   string // "" 表示不期望任何事件日志
	}{
		{
			name:       "tip 故障:打事件日志 + fault counter",
			fullMethod: "/login.LoginService/Case1",
			resp:       &respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginRedisError)}},

			wantFaultCode: uint32(table.LoginError_kLoginRedisError),
			wantFaultSrc:  "tip_info",
			wantLogEvent:  EventInbandFault,
		},
		{
			name:       "tip 业务拒绝:只进 reject counter,不打 Error 日志",
			fullMethod: "/bag.BagService/Case2",
			resp:       &respTip{tip: &fakeTip{id: uint32(table.BagError_kBagAddItemBagFull)}},

			wantReject:    true,
			wantRejectSrc: "tip_info",
		},
		{
			name:       "成功响应:三个 counter 都不动",
			fullMethod: "/login.LoginService/Case3",
			resp:       &respTip{},
		},
		{
			name:       "error_code 配了 Classifier 后能判成故障",
			fullMethod: "/scene_manager.SceneManagerService/Case4",
			opts:       Options{ErrorCodeClassifier: FaultCodeSet(1, 8)},
			resp:       &respErrorCode{code: 8},

			wantFaultCode: 8,
			wantFaultSrc:  "error_code",
			wantLogEvent:  EventInbandFault,
		},
		{
			name:       "error_code 未配 Classifier:只记业务拒绝,不误报故障",
			fullMethod: "/data_service.DataService/Case5",
			resp:       &respErrorCode{code: 1},

			wantReject:    true,
			wantRejectSrc: "error_code",
		},
		{
			name:       "码表漂移:进 unknown counter 并打漂移事件",
			fullMethod: "/login.LoginService/Case6",
			resp:       &respTip{tip: &fakeTip{id: TipMaxKnownCode + 1}},

			wantUnknown:    true,
			wantUnknownSrc: "tip_info",
			wantLogEvent:   EventInbandUnknownCode,
		},
		{
			name:       "SuppressFaultLog 只掐日志,指标照记",
			fullMethod: "/login.LoginService/Case7",
			opts:       Options{SuppressFaultLog: true},
			resp:       &respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginTimeout)}},

			wantFaultCode: uint32(table.LoginError_kLoginTimeout),
			wantFaultSrc:  "tip_info",
		},
		{
			name:       "handler 返回真 error:走 transport_error,不碰 in-band 三个 counter",
			fullMethod: "/login.LoginService/Case8",
			resp:       &respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginRedisError)}},
			handlerErr: errors.New("网络断了"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)

			method := tt.opts.methodName(tt.fullMethod)
			itc := UnaryInterceptor(tt.opts)

			resp, err := itc(context.Background(), struct{}{},
				newUnaryInfo(tt.fullMethod), handlerReturning(tt.resp, tt.handlerErr))

			// 拦截器只观测:响应与错误必须原样透传。
			if !errors.Is(err, tt.handlerErr) {
				t.Fatalf("err = %v, 期望原样透传 %v", err, tt.handlerErr)
			}
			if resp != any(tt.resp) {
				t.Fatalf("resp 被改动了: %+v", resp)
			}

			if tt.wantFaultCode != 0 {
				if got := faultCount(method, tt.wantFaultSrc, tt.wantFaultCode); got != 1 {
					t.Errorf("fault counter = %v, 期望 1", got)
				}
			}
			if tt.wantReject {
				if got := rejectCount(method, tt.wantRejectSrc); got != 1 {
					t.Errorf("reject counter = %v, 期望 1", got)
				}
			}
			if tt.wantUnknown {
				if got := unknownCount(method, tt.wantUnknownSrc); got != 1 {
					t.Errorf("unknown counter = %v, 期望 1", got)
				}
			}

			// 成功 / transport_error 两条路径:三个 in-band counter 一个都不能动。
			if tt.wantFaultCode == 0 && !tt.wantReject && !tt.wantUnknown {
				for _, src := range []string{"tip_info", "error_code", "none"} {
					if got := rejectCount(method, src); got != 0 {
						t.Errorf("reject counter(%s) = %v, 期望 0", src, got)
					}
					if got := unknownCount(method, src); got != 0 {
						t.Errorf("unknown counter(%s) = %v, 期望 0", src, got)
					}
				}
			}

			out := logs.String()
			if tt.wantLogEvent != "" {
				if !strings.Contains(out, tt.wantLogEvent) {
					t.Errorf("日志里没有事件名 %q, 实际内容: %s", tt.wantLogEvent, out)
				}
			} else {
				for _, ev := range []string{EventInbandFault, EventInbandUnknownCode} {
					if strings.Contains(out, ev) {
						t.Errorf("不该打事件 %q, 实际内容: %s", ev, out)
					}
				}
			}
		})
	}
}

// TestUnaryInterceptorNeverLogsPlayerID 守住低基数纪律:
// 故障日志里只允许出现 method / code / source / domain / 耗时,
// 绝不能把 player_id 之类高基数字段带进去(仓库 CLAUDE.md §9)。
func TestUnaryInterceptorNeverLogsPlayerID(t *testing.T) {
	logs := logtest.NewCollector(t)

	itc := UnaryInterceptor(Options{})
	_, _ = itc(context.Background(), struct{ PlayerId uint64 }{PlayerId: 1234567890},
		newUnaryInfo("/login.LoginService/CaseNoPlayerID"),
		handlerReturning(&respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginRedisError)}}, nil))

	out := logs.String()
	if !strings.Contains(out, EventInbandFault) {
		t.Fatalf("没打故障事件, 日志: %s", out)
	}
	for _, forbidden := range []string{"player_id", "PlayerId", "1234567890"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("故障日志里出现了高基数字段 %q: %s", forbidden, out)
		}
	}
}

func TestOptionsMethodName(t *testing.T) {
	// 默认压成短名,避免 Prometheus label 里塞完整包路径。
	if got := (Options{}).methodName("/login.LoginService/Login"); got != "LoginService/Login" {
		t.Fatalf("默认 methodName = %q, 期望 %q", got, "LoginService/Login")
	}
	// 可覆盖。
	custom := Options{MethodName: func(string) string { return "x" }}
	if got := custom.methodName("/login.LoginService/Login"); got != "x" {
		t.Fatalf("自定义 methodName = %q, 期望 %q", got, "x")
	}
}
