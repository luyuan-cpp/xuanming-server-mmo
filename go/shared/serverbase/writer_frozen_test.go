package serverbase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"shared/authoritysql"
)

// frozenTestResponse 的形状和各服务的响应相同:带内错误码在 GetErrorMessage().GetId()。
type frozenTestTip struct{ Id uint32 }

func (t *frozenTestTip) GetId() uint32 { return t.Id }

type frozenTestResponse struct{ ErrorMessage *frozenTestTip }

func (r *frozenTestResponse) GetErrorMessage() *frozenTestTip { return r.ErrorMessage }

// 模拟 handler 的存储层撞上冻结的闸门:连接层只会置观察位,这里用同一个入口把观察位置上。
func hitFrozenGate(ctx context.Context) {
	authoritysql.NoteWriterFrozen(ctx, authoritysql.ErrWriterFrozen)
}

func TestWriterFrozenFailureBecomesUnavailable(t *testing.T) {
	interceptors := map[string]grpc.UnaryServerInterceptor{
		"UnaryInterceptor":        UnaryInterceptor(Options{}),
		"WriterFrozenInterceptor": WriterFrozenInterceptor(),
	}
	cases := []struct {
		name    string
		handler grpc.UnaryHandler
		want    codes.Code
	}{
		{
			name: "哨兵错误一路包着回来",
			handler: func(context.Context, any) (any, error) {
				return nil, fmt.Errorf("save: %w", authoritysql.ErrWriterFrozen)
			},
			want: codes.Unavailable,
		},
		{
			name: "handler 把存储错误翻成了 Internal",
			handler: func(ctx context.Context, _ any) (any, error) {
				hitFrozenGate(ctx)
				return nil, status.Error(codes.Internal, "storage failed")
			},
			want: codes.Unavailable,
		},
		{
			name: "handler 把存储错误翻成了带内错误码",
			handler: func(ctx context.Context, _ any) (any, error) {
				hitFrozenGate(ctx)
				return &frozenTestResponse{ErrorMessage: &frozenTestTip{Id: 7}}, nil
			},
			want: codes.Unavailable,
		},
		{
			name: "资格不符不是可重试错误",
			handler: func(context.Context, any) (any, error) {
				return nil, status.Error(codes.PermissionDenied, authoritysql.ErrWriterDenied.Error())
			},
			want: codes.PermissionDenied,
		},
		{
			name: "别的错误原样返回",
			handler: func(context.Context, any) (any, error) {
				return nil, errors.New("plain")
			},
			want: codes.Unknown,
		},
		{
			// 建角撞上冻结的闸门:handler 已经下了「确定没提交」的结论,login 靠这个码释放名字,不能改写成 Unavailable。
			name: "确定没提交的结论不改写",
			handler: func(ctx context.Context, _ any) (any, error) {
				hitFrozenGate(ctx)
				return nil, status.Error(codes.Aborted, "not committed")
			},
			want: codes.Aborted,
		},
	}
	for interceptorName, interceptor := range interceptors {
		for _, test := range cases {
			t.Run(interceptorName+"/"+test.name, func(t *testing.T) {
				_, err := interceptor(context.Background(), nil, newUnaryInfo("/test.Service/Method"), test.handler)
				if got := status.Code(err); got != test.want {
					t.Fatalf("状态码 %s,应为 %s: %v", got, test.want, err)
				}
			})
		}
	}
}

// 错误树里混了别的失败就不能说这次只是撞了冻结:原样返回,由外层按未知算。
func TestWriterFrozenMixedWithOtherFailureStaysAsIs(t *testing.T) {
	mixed := errors.Join(authoritysql.ErrWriterFrozen, errors.New("other"))
	_, err := WriterFrozenInterceptor()(context.Background(), nil, newUnaryInfo("/test.Service/Method"),
		func(context.Context, any) (any, error) { return nil, mixed })
	if err != mixed {
		t.Fatalf("混合错误被改写: %v", err)
	}
}

// handler 吞掉冻结错误、照常回成功时(尽力而为的旁路写)不改它的结果。
func TestWriterFrozenSwallowedByHandlerKeepsResponse(t *testing.T) {
	want := &frozenTestResponse{}
	resp, err := WriterFrozenInterceptor()(context.Background(), nil, newUnaryInfo("/test.Service/Method"),
		func(ctx context.Context, _ any) (any, error) {
			hitFrozenGate(ctx)
			return want, nil
		})
	if err != nil || resp != any(want) {
		t.Fatalf("成功响应被改了: %v %v", resp, err)
	}
}
