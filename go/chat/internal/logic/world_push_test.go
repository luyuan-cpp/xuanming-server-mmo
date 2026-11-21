package logic

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"chat/internal/authority"
	"chat/internal/constants"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 发送与世界频道推送的先后与次数(docs/design/chat-world-channel-fanout.md §4):只有首次提交推一次,
// 推的是已提交的显示原件;重放、限流、私聊都不推;推送失败不改变已经生效的发送应答。

type pushSpy struct {
	mu        sync.Mutex
	pushed    []*chatpb.ChatMessage
	retracted []*chatpb.ChatRetractedNotify
	err       error
}

func (p *pushSpy) PushWorld(_ context.Context, message *chatpb.ChatMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushed = append(p.pushed, message)
	return p.err
}

func (p *pushSpy) PushWorldRetract(_ context.Context, notify *chatpb.ChatRetractedNotify) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retracted = append(p.retracted, notify)
	return p.err
}

func TestWorldSendPushesCommittedMessageOnlyOnFirstCommit(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{}
	sc.WorldPush = pusher
	committed := &chatpb.ChatMessage{SenderPlayerId: playerA, Channel: world, Content: "已过滤的正文", SendTimeMs: 1000}
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Message: committed}, nil
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), request()))
	require.Len(t, pusher.pushed, 1)
	require.True(t, proto.Equal(committed, pusher.pushed[0]), "推的是 SQL 已接受的显示原件,不是客户端请求体")

	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Duplicate: true}, nil
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), request()))
	require.Len(t, pusher.pushed, 1, "同一 request_id 重放不推第二次")

	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Tip: constants.ErrRateLimited}, nil
	}
	require.Equal(t, constants.ErrRateLimited, send(t, sc, playerCtx(playerA), request()))
	require.Len(t, pusher.pushed, 1, "限流没有写入,不推")
}

func TestPrivateSendIsNotPushedToWorld(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{}
	sc.WorldPush = pusher
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Message: &chatpb.ChatMessage{SenderPlayerId: playerA, TargetPlayerId: playerB, Channel: private, Content: "悄悄话", SendTimeMs: 1000}}, nil
	}
	in := &chatpb.SendChatRequest{Message: &chatpb.ChatMessage{Channel: private, TargetPlayerId: playerB, Content: "悄悄话"}, RequestId: "private-uuid"}
	require.Zero(t, send(t, sc, playerCtx(playerA), in))
	require.Empty(t, pusher.pushed)
}

func TestPushFailureDoesNotFailCommittedSend(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{err: errors.New("kafka unavailable")}
	sc.WorldPush = pusher
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{Message: &chatpb.ChatMessage{SenderPlayerId: playerA, Channel: world, Content: "x", SendTimeMs: 1}}, nil
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), request()), "提交已生效,推送失败照样回成功,否则客户端会换 id 重发出第二条")
	require.Len(t, pusher.pushed, 1)
}

func TestAsyncWorldPlatformRejectRetractsAndBroadcasts(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{}
	sc.WorldPush = pusher
	retractedCh := make(chan struct{}, 1)
	spy.send = func(context.Context, *chatpb.SendChatRequest) (authority.SendResult, error) {
		return authority.SendResult{
			Message:               &chatpb.ChatMessage{SenderPlayerId: playerA, Channel: world, Content: "疑似广告", SendTimeMs: 5555},
			PendingPlatformReview: true,
			PlatformReviewContent: "疑似广告",
		}, nil
	}
	spy.retract = func(_ context.Context, sender uint64, acceptedMs int64, reason string) (bool, uint32, error) {
		require.EqualValues(t, playerA, sender)
		require.EqualValues(t, 5555, acceptedMs)
		require.Equal(t, platformRejectReason, reason)
		retractedCh <- struct{}{}
		return true, 0, nil
	}
	sc.PlatformCheck = func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
		require.Equal(t, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, scene)
		require.EqualValues(t, playerA, playerID)
		require.Equal(t, "疑似广告", text)
		return contentguard.Result{Verdict: contentguard.Reject}
	}
	require.Zero(t, send(t, sc, playerCtx(playerA), request()))
	select {
	case <-retractedCh:
	case <-time.After(time.Second):
		t.Fatal("expected async platform recheck to retract violating world message")
	}
	require.Eventually(t, func() bool {
		pusher.mu.Lock()
		defer pusher.mu.Unlock()
		return len(pusher.retracted) == 1 && pusher.retracted[0].GetSendTimeMs() == 5555
	}, time.Second, 10*time.Millisecond)
}
