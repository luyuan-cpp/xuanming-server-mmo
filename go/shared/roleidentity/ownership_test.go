package roleidentity

import (
	"context"
	"errors"
	"testing"

	dspb "proto/data_service"
	"shared/callerauth/callerauthtest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errOwnerChangedForTest = errors.New("角色已换主")

// ownershipOutcome 记录一次「入口准入 → 归属预检」的结果。
type ownershipOutcome struct {
	checked bool                       // 预检的 check 被调用了
	seen    *dspb.AccountRoleOwnership // check 从 ctx 里取到的原身份
	handled bool                       // 走到了业务 handler
	code    codes.Code
}

// runOwnership 按生产的先后把入口准入与归属预检串起来:入口准入在前,归属预检在后。
func runOwnership(t *testing.T, ctx context.Context, fullMethod string, checkErr error) ownershipOutcome {
	t.Helper()
	entry, err := EntryInterceptor(testCallable, anonymousRead)
	require.NoError(t, err)
	var out ownershipOutcome
	ownership, err := OwnershipInterceptor(func(ctx context.Context) error {
		out.checked = true
		out.seen, _ = Original(ctx)
		return checkErr
	}, func(err error) bool { return errors.Is(err, errOwnerChangedForTest) })
	require.NoError(t, err)

	info := &grpc.UnaryServerInfo{FullMethod: fullMethod}
	_, err = entry(ctx, nil, info, func(ctx context.Context, request any) (any, error) {
		return ownership(ctx, request, info, func(context.Context, any) (any, error) {
			out.handled = true
			return nil, nil
		})
	})
	out.code = status.Code(err)
	return out
}

func TestOwnershipInterceptorChecksTheSignedOriginalAndPasses(t *testing.T) {
	// 账号原样带首尾空格:原账号取 gate 签名原件本身,不做任何规范化。
	in := callerauthtest.OnlineSession(" seller ", 11, 7)
	out := runOwnership(t, callerauthtest.Incoming(t, onlineMethod, in), onlineMethod, nil)
	require.Equal(t, codes.OK, out.code)
	require.True(t, out.checked)
	require.True(t, out.handled)
	require.NotNil(t, out.seen, "预检按入口准入捕获的原身份判")
	require.Equal(t, " seller ", out.seen.Account)
	require.EqualValues(t, 7, out.seen.OwnerRevision)
	require.EqualValues(t, 11, out.seen.Role.GetPlayerId())
}

// 过户、删角之后旧连接还在发请求:读方法也拿不到这名角色的数据。
func TestOwnershipInterceptorRejectsRoleThatNoLongerBelongsToTheSession(t *testing.T) {
	in := callerauthtest.OnlineSession("seller", 11, 7)
	out := runOwnership(t, callerauthtest.Incoming(t, onlineMethod, in), onlineMethod, errOwnerChangedForTest)
	require.True(t, out.checked)
	require.False(t, out.handled)
	require.Equal(t, codes.PermissionDenied, out.code)
}

// 目录读不到、库故障:这一次判不了。客户端要能重试;不能回 PermissionDenied(角色没变),
// 也不回 Unavailable(只读、确定没写,不该把排空证明打成 Hold)。
func TestOwnershipInterceptorInconclusiveCheckIsRetryableNotARejection(t *testing.T) {
	in := callerauthtest.OnlineSession("seller", 11, 7)
	out := runOwnership(t, callerauthtest.Incoming(t, onlineMethod, in), onlineMethod, errors.New("目录读不到"))
	require.True(t, out.checked)
	require.False(t, out.handled)
	require.Equal(t, codes.Aborted, out.code)
}

// 不带会话的调用(内部调用、登记的匿名只读例外)ctx 里没有原身份,不做归属预检。
func TestOwnershipInterceptorSkipsCallsWithoutOriginal(t *testing.T) {
	for _, method := range []string{internalMethod, anonymousRead} {
		out := runOwnership(t, context.Background(), method, errOwnerChangedForTest)
		require.Equal(t, codes.OK, out.code, method)
		require.False(t, out.checked, method)
		require.True(t, out.handled, method)
	}
}

// 入口准入就拒掉的身份(不完整)不进归属预检。
func TestOwnershipInterceptorIsNotReachedByIdentitiesTheEntryRejects(t *testing.T) {
	incomplete := callerauthtest.OnlineSession("seller", 11, 0)
	out := runOwnership(t, callerauthtest.Incoming(t, onlineMethod, incomplete), onlineMethod, nil)
	require.Equal(t, codes.PermissionDenied, out.code)
	require.False(t, out.checked)
	require.False(t, out.handled)
}

func TestOwnershipInterceptorRejectsBadAssembly(t *testing.T) {
	check := func(context.Context) error { return nil }
	rejected := func(error) bool { return false }
	_, err := OwnershipInterceptor(nil, rejected)
	require.Error(t, err, "没有判定函数不能带着空预检运行")
	_, err = OwnershipInterceptor(check, nil)
	require.Error(t, err, "分不清拒绝与判不了时不能运行")
	_, err = OwnershipInterceptor(check, rejected)
	require.NoError(t, err)
}
