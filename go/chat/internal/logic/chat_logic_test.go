package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chat/internal/authority"
	"chat/internal/config"
	"chat/internal/constants"
	"chat/internal/session"
	"chat/internal/svc"
	chatpb "proto/chat"
	base "proto/common/base"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	world   = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD
	private = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE
	playerA = uint64(11)
	playerB = uint64(22)
)

type authoritySpy struct {
	sends, pulls, retracts, reports, notices int
	send                                     func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error)
	pull                                     func(context.Context, *chatpb.PullChatHistoryRequest, int) ([]*chatpb.ChatMessage, error)
	retract                                  func(context.Context, uint64, int64, string) (bool, uint32, error)
	report                                   func(context.Context, *chatpb.ReportChatRequest) (uint32, error)
	notice                                   func(context.Context, *chatpb.PublishSystemWorldNoticeRequest) (authority.SendResult, error)
}

func (s *authoritySpy) Send(ctx context.Context, in *chatpb.SendChatRequest) (authority.SendResult, error) {
	s.sends++
	if s.send != nil {
		return s.send(ctx, in)
	}
	return authority.SendResult{}, nil
}
func (s *authoritySpy) Pull(ctx context.Context, in *chatpb.PullChatHistoryRequest, limit int) ([]*chatpb.ChatMessage, error) {
	s.pulls++
	if s.pull != nil {
		return s.pull(ctx, in, limit)
	}
	return nil, nil
}
func (s *authoritySpy) RetractWorld(ctx context.Context, sender uint64, acceptedMs int64, reason string) (bool, uint32, error) {
	s.retracts++
	if s.retract != nil {
		return s.retract(ctx, sender, acceptedMs, reason)
	}
	return true, 0, nil
}
func (s *authoritySpy) ReportMessage(ctx context.Context, in *chatpb.ReportChatRequest) (uint32, error) {
	s.reports++
	if s.report != nil {
		return s.report(ctx, in)
	}
	return 0, nil
}
func (s *authoritySpy) PublishSystemWorldNotice(ctx context.Context, in *chatpb.PublishSystemWorldNoticeRequest) (authority.SendResult, error) {
	s.notices++
	if s.notice != nil {
		return s.notice(ctx, in)
	}
	text, _ := authority.RenderSystemWorldNotice(in)
	return authority.SendResult{Message: &chatpb.ChatMessage{
		Channel:    chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		Content:    text,
		SendTimeMs: 123456,
	}}, nil
}
func testConfig() config.Config {
	return config.Config{MaxContentBytes: 512, HistoryDefaultLimit: 20, HistoryMaxLimit: 50}
}
func fixture() (*svc.ServiceContext, *authoritySpy) {
	spy := &authoritySpy{}
	return &svc.ServiceContext{Config: testConfig(), Authority: spy}, spy
}
func playerCtx(id uint64) context.Context {
	return session.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: id})
}
func request() *chatpb.SendChatRequest {
	return &chatpb.SendChatRequest{Message: &chatpb.ChatMessage{Channel: world, Content: "原始正文"}, RequestId: "original-uuid"}
}
func send(t *testing.T, sc *svc.ServiceContext, ctx context.Context, in *chatpb.SendChatRequest) uint32 {
	t.Helper()
	r, e := NewChatLogic(ctx, sc).SendChat(in)
	require.NoError(t, e)
	return r.GetErrorMessage().GetId()
}

func TestNoSessionRejected(t *testing.T) {
	sc, spy := fixture()
	for _, ctx := range []context.Context{context.Background(), playerCtx(0)} {
		require.Equal(t, constants.ErrInvalidParameter, send(t, sc, ctx, request()))
		r, e := NewChatLogic(ctx, sc).PullChatHistory(&chatpb.PullChatHistoryRequest{Channel: world})
		require.NoError(t, e)
		require.Equal(t, constants.ErrInvalidParameter, r.GetErrorMessage().GetId())
	}
	require.Zero(t, spy.sends)
	require.Zero(t, spy.pulls)
}
func TestContentLengthLimits(t *testing.T) {
	sc, spy := fixture()
	for _, value := range []string{"", " \t\n", strings.Repeat("x", 513)} {
		in := request()
		in.Message.Content = value
		require.Equal(t, constants.ErrMessageTooLong, send(t, sc, playerCtx(playerA), in))
	}
	require.Zero(t, spy.sends)
	in := request()
	in.Message.Content = strings.Repeat("x", 512)
	require.Zero(t, send(t, sc, playerCtx(playerA), in))
	require.Equal(t, 1, spy.sends)
}

// 长度按原文字节算,在送检之前判:打码会让正文变短,不能靠它绕过上限。
func TestMaskingCannotBypassOriginalContentByteLimit(t *testing.T) {
	sc, spy := fixture()
	in := request()
	in.Message.Content = strings.Repeat("广告", 100)
	require.Equal(t, constants.ErrMessageTooLong, send(t, sc, playerCtx(playerA), in))
	require.Zero(t, spy.sends)
}
func TestNonOpenChannelsRejected(t *testing.T) {
	sc, spy := fixture()
	for _, channel := range []chatpb.ChatChannelType{chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM} {
		in := request()
		in.Message.Channel = channel
		require.Equal(t, constants.ErrChannelUnavailable, send(t, sc, playerCtx(playerA), in))
	}
	in := request()
	in.Message.Channel = 99
	require.Equal(t, constants.ErrInvalidParameter, send(t, sc, playerCtx(playerA), in))
	require.Zero(t, spy.sends)
}
func TestPrivateInvalidPeerRejectedBeforeAuthority(t *testing.T) {
	sc, spy := fixture()
	for _, peer := range []uint64{0, playerA} {
		in := request()
		in.Message.Channel = private
		in.Message.TargetPlayerId = peer
		require.Equal(t, constants.ErrInvalidParameter, send(t, sc, playerCtx(playerA), in))
	}
	require.Zero(t, spy.sends)
}
func TestRequestIDAndUnknownPayloadAreBounded(t *testing.T) {
	sc, spy := fixture()
	in := request()
	in.RequestId = strings.Repeat("r", 65)
	require.Equal(t, constants.ErrInvalidParameter, send(t, sc, playerCtx(playerA), in))
	in = request()
	in.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 200, protowire.BytesType), make([]byte, 9000)))
	require.Equal(t, constants.ErrInvalidParameter, send(t, sc, playerCtx(playerA), in))
	require.Zero(t, spy.sends)
}
func TestOriginalCompletePBIsPassedWithoutMutation(t *testing.T) {
	sc, spy := fixture()
	in := request()
	in.Message.SenderPlayerId = 999
	in.Message.SendTimeMs = 77
	in.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 200, protowire.VarintType), 99))
	before := proto.Clone(in)
	spy.send = func(_ context.Context, got *chatpb.SendChatRequest) (authority.SendResult, error) {
		require.True(t, proto.Equal(in, got))
		got.Message.Content = "替身修改"
		return authority.SendResult{}, nil
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), in))
	require.True(t, proto.Equal(before, in), "调用者PB及未知字段不得被深接口修改")
}
func TestAuthorityFailuresStayInBand(t *testing.T) {
	for name, tc := range map[string]struct {
		err error
		tip uint32
	}{"old-owner": {authority.ErrOriginal, constants.ErrInvalidParameter}, "body-conflict": {authority.ErrConflict, constants.ErrInvalidParameter}, "SQL-unknown": {errors.New("依赖故障"), constants.ErrStorage}} {
		t.Run(name, func(t *testing.T) {
			sc, spy := fixture()
			spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
				return authority.SendResult{}, tc.err
			}
			require.Equal(t, tc.tip, send(t, sc, playerCtx(playerA), request()))
		})
	}
}
func TestRateRejectionAndAcceptedDuplicateAreRelayed(t *testing.T) {
	sc, spy := fixture()
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Tip: constants.ErrRateLimited}, nil
	}
	require.Equal(t, constants.ErrRateLimited, send(t, sc, playerCtx(playerA), request()))
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Duplicate: true}, nil
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), request()))
}

// 世界频道发言间隔与每秒条数回给客户端的是同一个限流码,指标分开记:看得出是哪一道在挡。
func TestWorldCooldownRejectionSharesTheTipButHasItsOwnOutcome(t *testing.T) {
	sc, spy := fixture()
	label := svc.ChannelLabel(request().GetMessage().GetChannel())
	cooldownBefore := svc.SendTotalValue(label, svc.OutcomeWorldCooldown)
	rateBefore := svc.SendTotalValue(label, svc.OutcomeRateLimited)
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Tip: constants.ErrRateLimited, Cooldown: true}, nil
	}
	require.Equal(t, constants.ErrRateLimited, send(t, sc, playerCtx(playerA), request()))
	require.Equal(t, cooldownBefore+1, svc.SendTotalValue(label, svc.OutcomeWorldCooldown))
	require.Equal(t, rateBefore, svc.SendTotalValue(label, svc.OutcomeRateLimited), "发言间隔的拒绝不记进每秒限流")
}

// 正文送检没过的两种结局各有自己的提示码和指标,不记进限流。
func TestContentReviewRejectionsHaveTheirOwnOutcomes(t *testing.T) {
	sc, spy := fixture()
	label := svc.ChannelLabel(request().GetMessage().GetChannel())
	rateBefore := svc.SendTotalValue(label, svc.OutcomeRateLimited)
	for tip, outcome := range map[uint32]string{
		constants.ErrContentRejected:         svc.OutcomeContentRejected,
		constants.ErrContentCheckUnavailable: svc.OutcomeContentUnavailable,
		constants.ErrLevelTooLow:             svc.OutcomeLevelTooLow,
		constants.ErrDuplicateSpam:           svc.OutcomeDuplicateSpam,
		constants.ErrPrivateTargetLimit:      svc.OutcomePrivateTargetLimit,
		constants.ErrContactRejected:         svc.OutcomeContactRejected,
	} {
		before := svc.SendTotalValue(label, outcome)
		spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
			return authority.SendResult{Tip: tip}, nil
		}
		require.Equal(t, tip, send(t, sc, playerCtx(playerA), request()))
		require.Equal(t, before+1, svc.SendTotalValue(label, outcome))
	}
	require.Equal(t, rateBefore, svc.SendTotalValue(label, svc.OutcomeRateLimited))
}
func TestPullLimitClamped(t *testing.T) {
	sc, spy := fixture()
	for _, value := range []struct {
		requested uint32
		want      int
	}{{0, 20}, {1, 1}, {999, 50}} {
		spy.pull = func(_ context.Context, _ *chatpb.PullChatHistoryRequest, n int) ([]*chatpb.ChatMessage, error) {
			require.Equal(t, value.want, n)
			return nil, nil
		}
		r, e := NewChatLogic(playerCtx(playerA), sc).PullChatHistory(&chatpb.PullChatHistoryRequest{Channel: world, Limit: value.requested})
		require.NoError(t, e)
		require.Nil(t, r.ErrorMessage)
	}
}
func TestPullNeverReturnsContentsWhenAuthorityRejects(t *testing.T) {
	sc, spy := fixture()
	spy.pull = func(context.Context, *chatpb.PullChatHistoryRequest, int) ([]*chatpb.ChatMessage, error) {
		return []*chatpb.ChatMessage{{Content: "不得泄露"}}, authority.ErrOriginal
	}
	r, e := NewChatLogic(playerCtx(playerA), sc).PullChatHistory(&chatpb.PullChatHistoryRequest{Channel: world})
	require.NoError(t, e)
	require.Equal(t, constants.ErrInvalidParameter, r.GetErrorMessage().GetId())
	require.Empty(t, r.Messages)
}
func TestMissingAuthorityNeverFallsBackToRedis(t *testing.T) {
	sc, _ := fixture()
	sc.Authority = nil
	require.Equal(t, constants.ErrStorage, send(t, sc, playerCtx(playerA), request()))
	r, e := NewChatLogic(playerCtx(playerA), sc).PullChatHistory(&chatpb.PullChatHistoryRequest{Channel: world})
	require.NoError(t, e)
	require.Equal(t, constants.ErrStorage, r.GetErrorMessage().GetId())
}

func TestReportChatValidationAndRelays(t *testing.T) {
	sc, spy := fixture()
	resp, err := NewChatLogic(playerCtx(playerA), sc).ReportChat(&chatpb.ReportChatRequest{
		Channel:              world,
		TargetSenderPlayerId: playerB,
		SendTimeMs:           12345,
		Reason:               0, // 枚举非法
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
	require.Zero(t, spy.reports)

	spy.report = func(_ context.Context, in *chatpb.ReportChatRequest) (uint32, error) {
		require.EqualValues(t, playerB, in.GetTargetSenderPlayerId())
		return constants.ErrMessageNotFound, nil
	}
	resp, err = NewChatLogic(playerCtx(playerA), sc).ReportChat(&chatpb.ReportChatRequest{
		Channel:              world,
		TargetSenderPlayerId: playerB,
		SendTimeMs:           12345,
		Reason:               1,
		Detail:               "广告引流",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrMessageNotFound, resp.GetErrorMessage().GetId())
	require.Equal(t, 1, spy.reports)
}

const opsToken = "ops-token-0123456789"

// opsContext 是带着运维口令的内部调用。
func opsContext(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(AdminTokenMetadataKey, token))
}

// 撤回是处置动作:口令没配时整个入口停用,口令不对或没带一律拒绝,都不碰库、不广播。
func TestRetractWorldChatRequiresTheOpsToken(t *testing.T) {
	request := &chatpb.RetractWorldChatRequest{SenderPlayerId: playerB, SendTimeMs: 999, Reason: "gm"}
	for name, tc := range map[string]struct {
		configured string
		ctx        context.Context
	}{
		"ops surface disabled": {configured: "", ctx: opsContext(opsToken)},
		"no token":             {configured: opsToken, ctx: context.Background()},
		"wrong token":          {configured: opsToken, ctx: opsContext(opsToken + "x")},
		"empty token":          {configured: opsToken, ctx: opsContext("")},
	} {
		t.Run(name, func(t *testing.T) {
			sc, spy := fixture()
			pusher := &pushSpy{}
			sc.WorldPush = pusher
			sc.AdminToken = tc.configured
			resp, err := NewChatLogic(tc.ctx, sc).RetractWorldChat(request)
			require.Nil(t, resp)
			require.Equal(t, codes.PermissionDenied, status.Code(err))
			require.NotContains(t, err.Error(), opsToken)
			require.Zero(t, spy.retracts)
			require.Empty(t, pusher.retracted)
		})
	}
}

func TestResolveAdminToken(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == AdminTokenEnv {
				return value
			}
			return ""
		}
	}
	token, err := ResolveAdminToken(env(""))
	require.NoError(t, err)
	require.Empty(t, token)
	token, err = ResolveAdminToken(env(opsToken))
	require.NoError(t, err)
	require.Equal(t, opsToken, token)
	_, err = ResolveAdminToken(env("short"))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "short")
}

func TestRetractWorldChatPublishesOnlyWhenRetracted(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{}
	sc.WorldPush = pusher
	sc.AdminToken = opsToken
	spy.retract = func(_ context.Context, sender uint64, ms int64, reason string) (bool, uint32, error) {
		return false, 0, nil // 幂等重复撤回
	}
	resp, err := NewChatLogic(opsContext(opsToken), sc).RetractWorldChat(&chatpb.RetractWorldChatRequest{
		SenderPlayerId: playerB,
		SendTimeMs:     999,
		Reason:         "gm",
	})
	require.NoError(t, err)
	require.Nil(t, resp.ErrorMessage)
	require.False(t, resp.GetRetracted())
	require.Empty(t, pusher.retracted)

	spy.retract = func(_ context.Context, sender uint64, ms int64, reason string) (bool, uint32, error) {
		return true, 0, nil
	}
	resp, err = NewChatLogic(opsContext(opsToken), sc).RetractWorldChat(&chatpb.RetractWorldChatRequest{
		SenderPlayerId: playerB,
		SendTimeMs:     999,
		Reason:         "gm",
	})
	require.NoError(t, err)
	require.True(t, resp.GetRetracted())
	require.Len(t, pusher.retracted, 1)
}
