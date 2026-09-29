package main

// MatchInternal 会话守卫(docs/design/guild-phase2/06-activities.md §6.18.3、§6.40):
// 带会话 metadata(客户端经 gate 来的)调 MatchInternal.* → PermissionDenied 且不进 handler;
// 无会话(guild 直连)→ 放行;客户端协议 MatchService.* 带会话照旧放行并把会话解进 ctx。
// 直接调 sessionInterceptor,不起 gRPC server。

import (
	"context"
	"encoding/base64"
	"testing"

	"match/internal/pkg/ctxkeys"

	base "proto/common/base"
	matchpb "proto/match"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// incomingSession 构造 gate 路由过来的入站 ctx:x-session-detail-bin = base64(SessionDetails)。
func incomingSession(t *testing.T, playerId uint64) context.Context {
	t.Helper()
	raw, err := proto.Marshal(&base.SessionDetails{SessionId: 7, PlayerId: playerId})
	if err != nil {
		t.Fatalf("序列化 SessionDetails 失败: %v", err)
	}
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(sessionMetadataKey, base64.StdEncoding.EncodeToString(raw)))
}

// recordingHandler 记录是否被调用,以及 ctx 里是否带会话。
type recordingHandler struct {
	called     bool
	hasSession bool
}

func (h *recordingHandler) handle(ctx context.Context, _ any) (any, error) {
	h.called = true
	_, h.hasSession = ctxkeys.GetSessionDetails(ctx)
	return &matchpb.StartActivityBattleResponse{}, nil
}

// 前缀由生成的 ServiceDesc 派生,必须恰好是 proto 里的 package.service。
func TestMatchInternalMethodPrefix(t *testing.T) {
	if matchInternalMethodPrefix != "/match.MatchInternal/" {
		t.Fatalf("MatchInternal 前缀 = %q,期望 /match.MatchInternal/", matchInternalMethodPrefix)
	}
	if got := matchpb.MatchInternal_StartActivityBattle_FullMethodName; got != "/match.MatchInternal/StartActivityBattle" {
		t.Fatalf("StartActivityBattle 完整方法名 = %q", got)
	}
}

// 带会话调 MatchInternal/StartActivityBattle:PermissionDenied,handler 未调用。
func TestSessionInterceptorRejectsClientCallToMatchInternal(t *testing.T) {
	h := &recordingHandler{}
	info := &grpc.UnaryServerInfo{FullMethod: matchpb.MatchInternal_StartActivityBattle_FullMethodName}

	resp, err := sessionInterceptor(incomingSession(t, 42), &matchpb.StartActivityBattleRequest{}, info, h.handle)

	if h.called {
		t.Fatal("带会话调用 MatchInternal 走到了 handler")
	}
	if resp != nil {
		t.Fatalf("被拒时不应有响应,得到 %v", resp)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
	}
}

// 会话头无法解码也算"带会话"(客户端来源),同样 PermissionDenied:不能退化成"当内部调用放行"。
func TestSessionInterceptorRejectsBrokenSessionOnMatchInternal(t *testing.T) {
	h := &recordingHandler{}
	info := &grpc.UnaryServerInfo{FullMethod: matchpb.MatchInternal_StartActivityBattle_FullMethodName}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionMetadataKey, "!!不是 base64!!"))

	_, err := sessionInterceptor(ctx, &matchpb.StartActivityBattleRequest{}, info, h.handle)

	if h.called {
		t.Fatal("坏会话头调用 MatchInternal 走到了 handler")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
	}
}

// 不带会话(guild 直连的内部调用):放行,handler 眼里没有会话。
func TestSessionInterceptorAllowsInternalCallToMatchInternal(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"无 metadata":    context.Background(),
		"metadata 无会话键": metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-other", "1")),
	} {
		t.Run(name, func(t *testing.T) {
			h := &recordingHandler{}
			info := &grpc.UnaryServerInfo{FullMethod: matchpb.MatchInternal_StartActivityBattle_FullMethodName}

			if _, err := sessionInterceptor(ctx, &matchpb.StartActivityBattleRequest{}, info, h.handle); err != nil {
				t.Fatalf("无会话的内部调用必须放行: %v", err)
			}
			if !h.called {
				t.Fatal("无会话的内部调用必须走到 handler")
			}
			if h.hasSession {
				t.Fatal("内部调用的 ctx 里不应有会话")
			}
		})
	}
}

// 客户端协议 MatchService/JoinQueue 带会话:照旧放行,会话解进 ctx(守卫只作用于 MatchInternal)。
func TestSessionInterceptorStillAllowsClientCallToMatchService(t *testing.T) {
	h := &recordingHandler{}
	info := &grpc.UnaryServerInfo{FullMethod: matchpb.MatchService_JoinQueue_FullMethodName}

	if _, err := sessionInterceptor(incomingSession(t, 42), &matchpb.JoinQueueRequest{}, info, h.handle); err != nil {
		t.Fatalf("客户端调 MatchService 必须放行: %v", err)
	}
	if !h.called {
		t.Fatal("客户端调 MatchService 必须走到 handler")
	}
	if !h.hasSession {
		t.Fatal("客户端调 MatchService 时会话必须解进 ctx")
	}
}
