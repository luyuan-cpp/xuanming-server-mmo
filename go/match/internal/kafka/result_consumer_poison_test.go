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

// 解码失败的坏消息永远处理不了:记录后跳过(返回 nil,由 owner 提交),不进处理器、不原地重试,
// 否则这个分区后续的评分与结算归档全部停住(docs/design/infra-reconnect-overview.md §4 R11)。
func TestProcessResultRecordSkipsUndecodableMessage(t *testing.T) {
	handled := false
	err := processResultRecord(context.Background(), kafkago.Message{Value: []byte{0xff, 0xff, 0xff}}, func(context.Context, *kafkapb.BattleResultEvent) error {
		handled = true
		return nil
	}, func(context.Context, time.Duration) bool {
		t.Fatal("坏消息不应等待重试")
		return false
	}, noArchive)
	require.NoError(t, err)
	require.False(t, handled)
}

// 同一局已归档的原件与这条不同:永久错误,只归档尝试一次就跳过,不进处理器、不等待重试。
func TestProcessResultRecordSkipsOriginalConflictWithoutRetry(t *testing.T) {
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
	require.NoError(t, err)
	require.Equal(t, 1, archives)
	require.False(t, handled)
}
