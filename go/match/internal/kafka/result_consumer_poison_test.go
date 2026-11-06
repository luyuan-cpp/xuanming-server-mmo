package kafka

import (
	"context"
	"fmt"
	"testing"
	"time"

	"match/internal/authority"
	kafkapb "proto/contracts/kafka"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 损坏原件尚未归档，不能让 owner 把错误当成已处理并提交位点；单轮处理不做无效重试。
func TestProcessResultRecordRejectsUndecodableMessageWithoutRetry(t *testing.T) {
	handled := false
	err := processResultRecord(context.Background(), kafkago.Message{Value: []byte{0xff, 0xff, 0xff}}, func(context.Context, *kafkapb.BattleResultEvent) error {
		handled = true
		return nil
	}, func(context.Context, time.Duration) bool {
		t.Fatal("坏消息不应等待重试")
		return false
	}, func(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error {
		t.Fatal("坏消息不能归档")
		return nil
	})
	require.Error(t, err)
	require.False(t, handled)
}

// 同局原件冲突需要人工核对两份原件；只尝试一次，保留错误，不评分、不等待重试。
func TestProcessResultRecordRejectsOriginalConflictWithoutRetry(t *testing.T) {
	body, err := proto.Marshal(&kafkapb.BattleResultEvent{BattleId: 7})
	require.NoError(t, err)
	archives := 0
	handled := false
	err = processResultRecord(context.Background(), kafkago.Message{Value: body}, func(context.Context, *kafkapb.BattleResultEvent) error {
		handled = true
		return nil
	}, func(context.Context, time.Duration) bool {
		t.Fatal("永久错误不应等待重试")
		return false
	}, func(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error {
		archives++
		return fmt.Errorf("archive: %w", authority.ErrCompletionOriginalConflict)
	})
	require.ErrorIs(t, err, authority.ErrCompletionOriginalConflict)
	require.Equal(t, 1, archives)
	require.False(t, handled)
}

func TestGlobalOriginalConflictCannotAdvanceOffset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := proto.Marshal(&kafkapb.BattleResultEvent{BattleId: 7})
	require.NoError(t, err)
	r := &resultReaderFixture{message: kafkago.Message{Value: body}, cancel: cancel}
	archives := 0
	runResultConsumer(ctx, r, func(context.Context, *kafkapb.BattleResultEvent) error {
		t.Fatal("原件冲突不能评分")
		return nil
	}, func(context.Context, time.Duration) bool {
		t.Fatal("永久错误单轮处理不应等待重试")
		return false
	}, func(context.Context, *kafkapb.BattleResultEvent, []byte, string, int, int64) error {
		archives++
		return authority.ErrCompletionOriginalConflict
	})
	require.Equal(t, 1, archives)
	require.Equal(t, 1, r.fetched)
	require.Zero(t, r.committed)
}
