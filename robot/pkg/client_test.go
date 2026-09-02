package pkg

import (
	"reflect"
	"testing"

	"proto/common/base"
)

func TestReplayDeferredMessagesPreservesFIFO(t *testing.T) {
	gc := &GameClient{}
	gc.DeferMessage(&base.MessageContent{MessageId: 79})
	gc.DeferMessage(&base.MessageContent{MessageId: 80})

	var got []uint32
	gc.replayDeferredMessages(func(_ *GameClient, msg *base.MessageContent) {
		got = append(got, msg.GetMessageId())
	})

	want := []uint32{79, 80}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed message ids = %v, want %v", got, want)
	}
	if msg := gc.popDeferredMessage(); msg != nil {
		t.Fatalf("deferred queue not empty after replay: message_id=%d", msg.GetMessageId())
	}
}

func TestDeferMessageIgnoresNil(t *testing.T) {
	gc := &GameClient{}
	gc.DeferMessage(nil)
	if msg := gc.popDeferredMessage(); msg != nil {
		t.Fatalf("nil defer unexpectedly queued message_id=%d", msg.GetMessageId())
	}
}
