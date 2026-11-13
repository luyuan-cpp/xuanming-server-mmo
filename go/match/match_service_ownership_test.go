package main

// match 的原角色归属预检(docs/design/gate-online-identity.md §9.3):入口准入之后,对带原身份的客户端请求不加锁读一次
// 本库护栏副本。战报列表与回放、作战脚本列表这些只读方法不经 Fenced,靠这一层核归属。
// 行为用例把 match_service.go 装配的两个拦截器按同样的先后串起来,不起 gRPC server;装配用例按源码结构核对 main。

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"match/internal/authority"

	matchpb "proto/match"
	rfpb "proto/role_fence"
	teampb "proto/team"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runMatchOwnership 按生产的先后跑「入口准入 → 归属预检」,判定函数换成测试给的结果;拒绝判据用生产的 authority.OwnerRejected。
func runMatchOwnership(t *testing.T, ctx context.Context, fullMethod string, checkErr error) (checked, handled bool, code codes.Code) {
	t.Helper()
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
	require.NoError(t, err)
	ownership, err := roleidentity.OwnershipInterceptor(func(ctx context.Context) error {
		checked = true
		original, ok := roleidentity.Original(ctx)
		require.True(t, ok, "归属预检要拿到入口准入捕获的原身份")
		require.Equal(t, "原账号", original.Account)
		require.EqualValues(t, 9, original.OwnerRevision)
		require.EqualValues(t, 42, original.Role.GetPlayerId())
		return checkErr
	}, authority.OwnerRejected)
	require.NoError(t, err)

	info := &grpc.UnaryServerInfo{FullMethod: fullMethod}
	_, err = entry(ctx, nil, info, func(ctx context.Context, request any) (any, error) {
		return ownership(ctx, request, info, func(context.Context, any) (any, error) {
			handled = true
			return nil, nil
		})
	})
	return checked, handled, status.Code(err)
}

// 过户、删角之后旧连接还在发请求:不经 Fenced 的只读方法也必须在 handler 之前被拒;经 Fenced 的方法同样先过这一层。
func TestMatchOwnershipPrecheckRejectsRoleThatNoLongerBelongsToTheSession(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 42, 9)
	for _, method := range []string{
		matchpb.MatchService_ListBattleReports_FullMethodName,
		matchpb.MatchService_GetBattleReport_FullMethodName,
		matchpb.MatchService_ListBattleScripts_FullMethodName,
		matchpb.MatchService_JoinQueue_FullMethodName,
		teampb.ClientPlayerTeam_GetMyTeam_FullMethodName,
	} {
		t.Run(method, func(t *testing.T) {
			checked, handled, code := runMatchOwnership(t, callerauthtest.Incoming(t, method, in), method, authority.ErrOriginalPlayer)
			require.True(t, checked)
			require.False(t, handled)
			require.Equal(t, codes.PermissionDenied, code)

			checked, handled, code = runMatchOwnership(t, callerauthtest.Incoming(t, method, in), method, nil)
			require.True(t, checked)
			require.True(t, handled, "角色仍归原身份时照常放行")
			require.Equal(t, codes.OK, code)
		})
	}
}

// 目录读不到、库故障:这一次判不了。客户端可以重试;不回 PermissionDenied(角色没变),
// 也不回 Unavailable(只读、确定没写,不该把排空证明打成 Hold)。
func TestMatchOwnershipPrecheckInconclusiveIsRetryable(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 42, 9)
	method := matchpb.MatchService_ListBattleReports_FullMethodName
	checked, handled, code := runMatchOwnership(t, callerauthtest.Incoming(t, method, in), method, errors.New("目录读不到"))
	require.True(t, checked)
	require.False(t, handled)
	require.Equal(t, codes.Aborted, code)
}

// 不带会话的内部调用(guild 开局、battle 交终局、协调者冻结角色)没有原身份,不做归属预检。
func TestMatchOwnershipPrecheckSkipsInternalCalls(t *testing.T) {
	for _, method := range []string{
		matchpb.MatchInternal_CommitBattleOutcome_FullMethodName,
		matchpb.MatchInternal_StartActivityBattle_FullMethodName,
		teampb.TeamAuthority_ReadMembership_FullMethodName,
		rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName,
	} {
		t.Run(method, func(t *testing.T) {
			checked, handled, code := runMatchOwnership(t, context.Background(), method, authority.ErrOriginalPlayer)
			require.False(t, checked)
			require.True(t, handled)
			require.Equal(t, codes.OK, code)
		})
	}
}

// 归属预检必须真的装在链上:main 里用 Authority 的 VerifyOriginalOwner 与 authority.OwnerRejected 装配,
// 并且在 AddUnaryInterceptors 里紧跟 onlineEntry(它要用入口准入捕获的原身份)。
// 删掉 ownership 那一行,其余测试与联调全绿,旧主的连接却还能读这名角色的战报与作战脚本。
func TestMainInstallsOwnershipPrecheckRightAfterOnlineEntry(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "match_service.go", nil, 0)
	require.NoError(t, err, "解析 match_service.go 失败;文件改名就同步改这里")
	var entry *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			entry = fn
		}
	}
	require.NotNil(t, entry, "match_service.go 里找不到 main")

	var assembled, chained bool
	ast.Inspect(entry, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) == 0 || len(node.Rhs) != 1 {
				return true
			}
			target, ok := node.Lhs[0].(*ast.Ident)
			call, isCall := node.Rhs[0].(*ast.CallExpr)
			if !ok || !isCall || target.Name != "ownership" {
				return true
			}
			fun, ok := call.Fun.(*ast.SelectorExpr)
			require.True(t, ok && fun.Sel.Name == "OwnershipInterceptor", "ownership 必须来自 roleidentity.OwnershipInterceptor")
			require.Len(t, call.Args, 2)
			check, ok := call.Args[0].(*ast.SelectorExpr)
			require.True(t, ok && check.Sel.Name == "VerifyOriginalOwner", "归属判定必须是 Authority 的 VerifyOriginalOwner")
			rejected, ok := call.Args[1].(*ast.SelectorExpr)
			require.True(t, ok && rejected.Sel.Name == "OwnerRejected", "拒绝判据必须是 authority.OwnerRejected")
			assembled = true
		case *ast.CallExpr:
			fun, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || fun.Sel.Name != "AddUnaryInterceptors" {
				return true
			}
			for i, arg := range node.Args {
				if name, ok := arg.(*ast.Ident); ok && name.Name == "onlineEntry" {
					require.Less(t, i+1, len(node.Args), "onlineEntry 之后必须还有归属预检")
					next, ok := node.Args[i+1].(*ast.Ident)
					require.True(t, ok && next.Name == "ownership", "归属预检要紧跟入口准入 onlineEntry")
					chained = true
				}
			}
		}
		return true
	})
	require.True(t, assembled, "main 不再装配 ownership:只读方法没有归属检查")
	require.True(t, chained, "main 的 AddUnaryInterceptors 里找不到 onlineEntry;改了写法就同步改这里")
}
