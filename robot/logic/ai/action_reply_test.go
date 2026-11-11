package ai

import (
	"errors"
	"fmt"
	"testing"

	"proto/chat"
	base "proto/common/base"
	"robot/generated/pb/game"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 动作请求按请求号认领回包之后的处理(client-request-ordering.md O6):只有成功回包交给类型化 handler;
// 错误信封(body 为空)、超时、断线都不交,否则空 body 会被解成一份全零响应、看上去像受理。
// stats 为 nil:onActionReply / sendAction 只在 stats 非 nil 时计数,这里不碰计数。

// dispatchRecorder 代替 handler.MessageBodyHandler,记下交给类型化 handler 的回包。
type dispatchRecorder struct{ replies []*base.MessageContent }

func (r *dispatchRecorder) dispatch(_ *pkg.GameClient, reply *base.MessageContent) {
	r.replies = append(r.replies, reply)
}

func newReplyTestAI() (*RobotAI, *dispatchRecorder) {
	recorder := &dispatchRecorder{}
	return &RobotAI{client: &pkg.GameClient{PlayerId: 77}, dispatch: recorder.dispatch}, recorder
}

func TestActionReplyDispatchesOnlySuccessfulReplies(t *testing.T) {
	const messageID = game.SceneSkillClientPlayerReleaseSkillMessageId
	envelopeTip := uint32(tiptable.CommonError_kRateLimitExceeded)
	envelope := &base.MessageContent{Id: 9, MessageId: messageID, ErrorMessage: &base.TipInfoMessage{Id: envelopeTip}}
	cases := []struct {
		name  string
		reply *base.MessageContent
		err   error
	}{
		{"envelope error", envelope, &pkg.ServerTipError{Tip: envelopeTip}},
		{"timeout", nil, pkg.ErrRPCTimeout},
		{"disconnected", nil, fmt.Errorf("wrapped: %w", pkg.ErrRPCDisconnected)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ai, recorder := newReplyTestAI()
			ai.onActionReply(ActionCastSkill, messageID, c.reply, c.err)
			if len(recorder.replies) != 0 {
				t.Fatalf("%s reached the typed handler: %v", c.name, recorder.replies)
			}
		})
	}

	ai, recorder := newReplyTestAI()
	ok := &base.MessageContent{Id: 10, MessageId: messageID}
	ai.onActionReply(ActionCastSkill, messageID, ok, nil)
	if len(recorder.replies) != 1 || recorder.replies[0] != ok {
		t.Fatalf("successful reply must reach the typed handler exactly once, got %v", recorder.replies)
	}
}

// 本地没发出(没有连接)时 sendAction 返回 ErrRPCNotSent,也不把这个结局当回包处理。
func TestSendActionReportsNotSentWithoutDispatch(t *testing.T) {
	ai, recorder := newReplyTestAI()
	err := ai.sendAction(ActionChat, game.ClientPlayerChatSendChatMessageId, &chat.SendChatRequest{})
	if !errors.Is(err, pkg.ErrRPCNotSent) {
		t.Fatalf("sendAction without a connection = %v, want ErrRPCNotSent", err)
	}
	if len(recorder.replies) != 0 {
		t.Fatalf("not-sent outcome reached the typed handler: %v", recorder.replies)
	}
}
