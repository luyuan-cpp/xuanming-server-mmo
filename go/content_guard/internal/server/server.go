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
	"content_guard/internal/platform"
	"content_guard/internal/wordlist"
)

// Option 配置 Server 的可选组件。
type Option func(*Server)

// WithReviewer 装配外部平台内容审核路由器(微信/抖音);传 nil 表示仅走本地词库层。
func WithReviewer(r platform.Reviewer) Option {
	return func(s *Server) {
		if r != nil {
			s.reviewer = r
		}
	}
}

// Server 实现 ContentGuard。
type Server struct {
	cgpb.UnimplementedContentGuardServer

	engine         *engine.Engine
	reviewer       platform.Reviewer
	requireLexicon bool
	maxTextBytes   int
}

// New 构造入口。requireLexicon、maxTextBytes 来自配置(config.RequireLexicon、config.MaxTextBytes)。
func New(e *engine.Engine, requireLexicon bool, maxTextBytes int, opts ...Option) *Server {
	s := &Server{engine: e, requireLexicon: requireLexicon, maxTextBytes: maxTextBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
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
	case cgpb.TextScene_TEXT_SCENE_TRADE_TITLE:
		return engine.SceneName, "trade_title", true
	case cgpb.TextScene_TEXT_SCENE_PRIVATE_LABEL:
		return engine.ScenePrivateLabel, "private_label", true
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

func (s *Server) validateAndLocalCheck(scene cgpb.TextScene, text string, started time.Time) (string, *cgpb.CheckTextResponse, string, error) {
	class, sceneName, ok := classOf(scene)
	if !ok {
		metrics.ObserveCheck(sceneName, "invalid", "none", time.Since(started))
		return "", nil, "", status.Error(codes.InvalidArgument, "unknown text scene")
	}
	if len(text) > s.maxTextBytes {
		metrics.ObserveCheck(sceneName, "invalid", "none", time.Since(started))
		return "", nil, "", status.Errorf(codes.InvalidArgument, "text is %d bytes, the limit is %d", len(text), s.maxTextBytes)
	}
	if !s.Ready() {
		metrics.ObserveCheck(sceneName, "unavailable", "none", time.Since(started))
		return "", nil, "", status.Error(codes.Unavailable, "lexicon not loaded")
	}

	result := s.engine.Check(class, text)
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
	return sceneName, out, verdictName, nil
}

func applyPlatformResult(sceneName string, started time.Time, out *cgpb.CheckTextResponse, verdictName string, ext platform.ReviewResult) (string, error) {
	switch ext.Outcome {
	case platform.OutcomeReject:
		out.Verdict = cgpb.TextVerdict_TEXT_VERDICT_REJECT
		out.MaskedText = ""
		if ext.Category != cgpb.TextCategory_TEXT_CATEGORY_UNSPECIFIED {
			out.Category = ext.Category
		}
		return "reject", nil
	case platform.OutcomePending:
		out.PendingPlatformReview = true
		return verdictName, nil
	case platform.OutcomeUnavailable:
		metrics.ObserveCheck(sceneName, "unavailable", "none", time.Since(started))
		return "", status.Error(codes.Unavailable, "external content reviewer unavailable")
	default:
		return verdictName, nil
	}
}

// CheckText 判定一段文字。被检文字不进日志(也不进错误信息):它是玩家的聊天原文和名字。
func (s *Server) CheckText(ctx context.Context, in *cgpb.CheckTextRequest) (*cgpb.CheckTextResponse, error) {
	started := time.Now()
	sceneName, out, verdictName, err := s.validateAndLocalCheck(in.GetScene(), in.GetText(), started)
	if err != nil {
		return nil, err
	}
	// 本地词库未直接拒绝时,若装配了外部平台审核器(微信 msgSecCheck / 抖音 antidirt),按场景策略复审。
	if out.Verdict != cgpb.TextVerdict_TEXT_VERDICT_REJECT && s.reviewer != nil {
		ext := s.reviewer.Review(ctx, in.GetScene(), in.GetPlayerId(), in.GetAccount(), in.GetText(), in.GetAsyncPlatform())
		verdictName, err = applyPlatformResult(sceneName, started, out, verdictName, ext)
		if err != nil {
			return nil, err
		}
	}
	metrics.ObserveCheck(sceneName, verdictName, wordlist.Category(out.Category).String(), time.Since(started))
	return out, nil
}

// CheckPlatformText 对消息同步执行外部平台审核(供世界频道短超时先放行后异步补审使用)。
func (s *Server) CheckPlatformText(ctx context.Context, in *cgpb.CheckTextRequest) (*cgpb.CheckTextResponse, error) {
	started := time.Now()
	sceneName, out, verdictName, err := s.validateAndLocalCheck(in.GetScene(), in.GetText(), started)
	if err != nil {
		return nil, err
	}
	if out.Verdict != cgpb.TextVerdict_TEXT_VERDICT_REJECT && s.reviewer != nil {
		ext := s.reviewer.ReviewSync(ctx, in.GetScene(), in.GetPlayerId(), in.GetAccount(), in.GetText())
		verdictName, err = applyPlatformResult(sceneName, started, out, verdictName, ext)
		if err != nil {
			return nil, err
		}
	}
	metrics.ObserveCheck(sceneName, verdictName, wordlist.Category(out.Category).String(), time.Since(started))
	return out, nil
}
