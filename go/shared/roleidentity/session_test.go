package roleidentity

import (
	"context"
	"testing"

	base "proto/common/base"
	"shared/callerauth/callerauthtest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	onlineMethod   = "/test.Online/Act"
	anonymousRead  = "/test.Online/Peek"
	internalMethod = "/test.Internal/Settle"
)

func testCallable(fullMethod string) bool {
	return fullMethod == onlineMethod || fullMethod == anonymousRead
}

func TestSessionOwnershipReturnsSignedOriginalWithoutAnyStore(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	ctx := callerauthtest.Incoming(t, onlineMethod, in)

	owned, err := SessionOwnership(ctx)
	require.NoError(t, err, "已签名的在线身份不读任何登录存储")
	require.Equal(t, "原账号", owned.Account)
	require.EqualValues(t, 3, owned.OwnerRevision)
	require.EqualValues(t, 11, owned.Role.GetPlayerId())

	bound, err := CaptureSession(ctx)
	require.NoError(t, err)
	captured, ok := Original(bound)
	require.True(t, ok)
	require.Equal(t, "原账号", captured.Account)
	require.EqualValues(t, 3, captured.OwnerRevision)
	require.EqualValues(t, 11, captured.Role.GetPlayerId())
}

func TestSessionOwnershipRejectsUnsignedOrIncompleteOnlineIdentity(t *testing.T) {
	complete := callerauthtest.OnlineSession("原账号", 11, 3)
	_, err := SessionOwnership(callerauthtest.Unverified(t, complete))
	require.ErrorIs(t, err, ErrUnverifiedSession, "没有验签原件的自报身份一律拒绝")
	_, err = SessionOwnership(context.Background())
	require.ErrorIs(t, err, ErrUnverifiedSession)
	_, err = CaptureSession(callerauthtest.Unverified(t, complete))
	require.ErrorIs(t, err, ErrUnverifiedSession)

	for name, mutate := range map[string]func(*base.SessionDetails){
		"未选角":  func(d *base.SessionDetails) { d.PlayerId = 0 },
		"缺修订":  func(d *base.SessionDetails) { d.OwnerRevision = 0 },
		"未绑账号": func(d *base.SessionDetails) { d.AuthenticatedAccount = "" },
	} {
		t.Run(name, func(t *testing.T) {
			// gate 即使签了名也不能凭不完整的会话授予在线角色身份(例如离场后账号已清空)。
			in := proto.Clone(complete).(*base.SessionDetails)
			mutate(in)
			_, err := SessionOwnership(callerauthtest.Incoming(t, onlineMethod, in))
			require.ErrorIs(t, err, ErrUnverifiedSession)
		})
	}
}

func TestWithSessionStoresACopy(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	ctx := WithSession(context.Background(), in)
	in.PlayerId = 99
	got, ok := Session(ctx)
	require.True(t, ok)
	require.EqualValues(t, 11, got.PlayerId, "放进 ctx 之后改原对象不影响已放入的会话")
	got.PlayerId = 98
	again, _ := Session(ctx)
	require.EqualValues(t, 11, again.PlayerId, "取出的是副本")

	_, ok = Session(WithSession(ctx, nil))
	require.False(t, ok)
	_, ok = Session(context.Background())
	require.False(t, ok)
}

// entryOutcome 记录一次准入的结果:放行时下游看到的会话与原件,拒绝时的 gRPC 码。
type entryOutcome struct {
	passed   bool
	code     codes.Code
	session  *base.SessionDetails
	original bool
}

func runEntry(t *testing.T, ctx context.Context, fullMethod string) entryOutcome {
	t.Helper()
	entry, err := EntryInterceptor(testCallable, anonymousRead)
	require.NoError(t, err)
	var out entryOutcome
	_, err = entry(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, func(next context.Context, _ any) (any, error) {
		out.passed = true
		out.session, _ = Session(next)
		_, out.original = Original(next)
		return nil, nil
	})
	out.code = status.Code(err)
	return out
}

func TestEntryInterceptorAdmitsCompleteClientSessionAndCapturesOriginal(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	out := runEntry(t, callerauthtest.Incoming(t, onlineMethod, in), onlineMethod)
	require.True(t, out.passed)
	require.Equal(t, codes.OK, out.code)
	require.NotNil(t, out.session, "业务取到的会话来自验签原件")
	require.True(t, proto.Equal(in, out.session))
	require.True(t, out.original, "原角色归属已捕获,提交点可复核")
}

func TestEntryInterceptorRejectsClientMethodWithIncompleteIdentity(t *testing.T) {
	for name, in := range map[string]*base.SessionDetails{
		// LeaveGame 之后 gate 仍带 player_id,但账号与原修订已清空。
		"离场后的会话": {SessionId: 7, PlayerId: 11, GateInstanceId: "gate-a", SessionIncarnation: 19},
		"未选角":    callerauthtest.OnlineSession("原账号", 0, 3),
		"缺修订":    callerauthtest.OnlineSession("原账号", 11, 0),
		"未绑账号":   callerauthtest.OnlineSession("", 11, 3),
	} {
		t.Run(name, func(t *testing.T) {
			out := runEntry(t, callerauthtest.Incoming(t, onlineMethod, in), onlineMethod)
			require.False(t, out.passed)
			require.Equal(t, codes.PermissionDenied, out.code)
		})
	}
}

// 不带会话调客户端方法只可能来自内部进程:除登记的匿名只读例外,一律拒绝,不再按请求体身份执行。
func TestEntryInterceptorRejectsClientMethodWithoutSession(t *testing.T) {
	out := runEntry(t, context.Background(), onlineMethod)
	require.False(t, out.passed)
	require.Equal(t, codes.PermissionDenied, out.code)

	read := runEntry(t, context.Background(), anonymousRead)
	require.True(t, read.passed, "登记的匿名只读例外按内部调用放行")
	require.Nil(t, read.session)
	require.False(t, read.original)
}

func TestEntryInterceptorRejectsSessionOnInternalMethod(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	out := runEntry(t, callerauthtest.Incoming(t, internalMethod, in), internalMethod)
	require.False(t, out.passed, "即使签名有效,客户端也不能调内部方法")
	require.Equal(t, codes.PermissionDenied, out.code)

	internal := runEntry(t, context.Background(), internalMethod)
	require.True(t, internal.passed, "不带会话的内部调用交给方法自己的调用方准入")
	require.Nil(t, internal.session)
	require.False(t, internal.original)
}

func TestEntryInterceptorRejectsSessionMetadataWithoutVerifiedOriginal(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	for _, method := range []string{onlineMethod, anonymousRead, internalMethod} {
		out := runEntry(t, callerauthtest.Unverified(t, in), method)
		require.False(t, out.passed, method)
		require.Equal(t, codes.Unauthenticated, out.code, "装配顺序错时 fail-closed: %s", method)
	}
}

func TestEntryInterceptorRejectsBadAssembly(t *testing.T) {
	_, err := EntryInterceptor(nil)
	require.Error(t, err)
	_, err = EntryInterceptor(testCallable, internalMethod)
	require.Error(t, err, "匿名只读例外必须是客户端方法")
}
