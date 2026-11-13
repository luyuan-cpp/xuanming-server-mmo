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
	sends, pulls int
	send         func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error)
	pull         func(context.Context, *chatpb.PullChatHistoryRequest, int) ([]*chatpb.ChatMessage, error)
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
func TestSensitiveMaskCannotBypassOriginalContentByteLimit(t *testing.T) {
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
