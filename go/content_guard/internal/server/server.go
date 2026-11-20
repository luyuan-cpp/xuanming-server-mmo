// Package server 是内容安全服务的 gRPC 入口:把协议里的场景换成动作大类,调 engine,记指标。
package server

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cgpb "proto/content_guard"

	"content_guard/internal/engine"
	"content_guard/internal/metrics"
)

// Server 实现 ContentGuard。
type Server struct {
	cgpb.UnimplementedContentGuardServer

	engine         *engine.Engine
	requireLexicon bool
	maxTextBytes   int
}

// New 构造入口。requireLexicon、maxTextBytes 来自配置(config.RequireLexicon、config.MaxTextBytes)。
func New(e *engine.Engine, requireLexicon bool, maxTextBytes int) *Server {
	return &Server{engine: e, requireLexicon: requireLexicon, maxTextBytes: maxTextBytes}
}

// Ready 报告本副本能否对外判定:要求词库时,必须已经从 etcd 加载到一份。/healthz 用它。
func (s *Server) Ready() bool {
	return !s.requireLexicon || s.engine.Current().Version != 0
}

// classOf 是协议场景到动作大类的全部映射,第二个返回值是指标标签里的场景名。
// 加场景时必须在这里加一行:server_test.go 逐个核对 proto 里的每个场景都有映射,漏了会红。
func classOf(scene cgpb.TextScene) (engine.SceneClass, string, bool) {
	switch scene {
	case cgpb.TextScene_TEXT_SCENE_ROLE_NAME:
		return engine.SceneName, "role_name", true
	case cgpb.TextScene_TEXT_SCENE_GUILD_NAME:
		return engine.SceneName, "guild_name", true
	case cgpb.TextScene_TEXT_SCENE_PET_NAME:
		return engine.SceneName, "pet_name", true
	case cgpb.TextScene_TEXT_SCENE_PRIVATE_LABEL:
		return engine.SceneName, "private_label", true
	case cgpb.TextScene_TEXT_SCENE_GUILD_ANNOUNCEMENT:
		return engine.SceneNotice, "guild_announcement", true
	case cgpb.TextScene_TEXT_SCENE_CHAT_WORLD:
		return engine.SceneChat, "chat_world", true
	case cgpb.TextScene_TEXT_SCENE_CHAT_TEAM:
		return engine.SceneChat, "chat_team", true
	case cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE:
		return engine.SceneChat, "chat_private", true
	}
	return engine.SceneName, "unknown", false
}

// CheckText 判定一段文字。被检文字不进日志(也不进错误信息):它是玩家的聊天原文和名字。
func (s *Server) CheckText(_ context.Context, in *cgpb.CheckTextRequest) (*cgpb.CheckTextResponse, error) {
	started := time.Now()
	class, sceneName, ok := classOf(in.GetScene())
	if !ok {
		metrics.ObserveCheck(sceneName, "invalid", "none", time.Since(started))
		return nil, status.Error(codes.InvalidArgument, "unknown text scene")
	}
	if len(in.GetText()) > s.maxTextBytes {
		metrics.ObserveCheck(sceneName, "invalid", "none", time.Since(started))
		return nil, status.Errorf(codes.InvalidArgument, "text is %d bytes, the limit is %d", len(in.GetText()), s.maxTextBytes)
	}
	if !s.Ready() {
		metrics.ObserveCheck(sceneName, "unavailable", "none", time.Since(started))
		return nil, status.Error(codes.Unavailable, "lexicon not loaded")
	}

	result := s.engine.Check(class, in.GetText())
	out := &cgpb.CheckTextResponse{
		Category:       cgpb.TextCategory(result.Category),
		LexiconVersion: result.Version,
	}
	verdictName := "reject"
	switch result.Verdict {
	case engine.VerdictPass:
		out.Verdict = cgpb.TextVerdict_TEXT_VERDICT_PASS
		verdictName = "pass"
	case engine.VerdictMasked:
		out.Verdict = cgpb.TextVerdict_TEXT_VERDICT_MASKED
		out.MaskedText = result.Masked
		verdictName = "masked"
	default:
		out.Verdict = cgpb.TextVerdict_TEXT_VERDICT_REJECT
	}
	metrics.ObserveCheck(sceneName, verdictName, result.Category.String(), time.Since(started))
	return out, nil
}
