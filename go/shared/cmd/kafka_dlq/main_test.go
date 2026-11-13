package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"shared/kafkautil"
)

func deadLetterEntry(t *testing.T, dlqOffset int64, group, cause string, originalOffset int64) entry {
	t.Helper()
	message := kafkautil.DeadLetterMessage(kafkautil.DeadLetterRecord{
		Group:    group,
		Original: kafkago.Message{Topic: "match-results", Partition: 1, Offset: originalOffset, Key: []byte("9001"), Value: []byte("raw-result"), Headers: []kafkago.Header{{Key: "traceparent", Value: []byte("00-a-b-01")}}},
		Cause:    cause,
		Attempts: 20,
		Reason:   "tidb: connection refused",
		At:       time.UnixMilli(1_700_000_000_000),
	})
	message.Offset = dlqOffset
	record, err := kafkautil.ParseDeadLetter(message)
	if err != nil {
		t.Fatal(err)
	}
	return entry{offset: dlqOffset, record: record}
}

// 重放只写原件:原 topic、原分区、原 key / value / headers(dlq.* 头已去掉);同一原位置在区间里出现多次只写一次。
func TestSelectReplaysWritesEachOriginalPositionOnceWithoutDeadLetterHeaders(t *testing.T) {
	entries := []entry{
		deadLetterEntry(t, 0, "guild-trial", kafkautil.DeadLetterCauseExhausted, 40),
		deadLetterEntry(t, 1, "guild-trial", kafkautil.DeadLetterCauseExhausted, 40), // 转死信后提交失败、新属主又转了一次
		deadLetterEntry(t, 2, "guild-trial", kafkautil.DeadLetterCauseExhausted, 41),
	}
	messages, skipped, err := selectReplays(entries, "guild-trial", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || len(skipped) != 1 || !strings.Contains(skipped[0], "dlq_offset=1") {
		t.Fatalf("同一原位置只写一次: messages=%d skipped=%v", len(messages), skipped)
	}
	got := messages[0]
	if got.Topic != "match-results" || got.Partition != 1 || string(got.Key) != "9001" || string(got.Value) != "raw-result" {
		t.Fatalf("重放的原件不对: %+v", got)
	}
	if len(got.Headers) != 1 || got.Headers[0].Key != "traceparent" {
		t.Fatalf("写回原 topic 时不能带 dlq.* 头: %+v", got.Headers)
	}
}

// 组对不上、解不出的死信、(默认)unprocessable 的死信都拒绝重放,整批不写。
func TestSelectReplaysRefusesForeignUnparsableAndUnprocessableEntries(t *testing.T) {
	if _, _, err := selectReplays([]entry{deadLetterEntry(t, 0, "match-rating", kafkautil.DeadLetterCauseExhausted, 1)}, "guild-trial", false); err == nil {
		t.Fatal("别的组的死信不能重放")
	}
	if _, _, err := selectReplays([]entry{{offset: 3, err: errors.New("dead letter header dlq.group missing")}}, "guild-trial", false); err == nil {
		t.Fatal("解不出的死信不能重放")
	}
	unprocessable := []entry{deadLetterEntry(t, 4, "guild-trial", kafkautil.DeadLetterCauseUnprocessable, 7)}
	if _, _, err := selectReplays(unprocessable, "guild-trial", false); err == nil || !strings.Contains(err.Error(), "-allow-unprocessable") {
		t.Fatalf("无法处理的原件默认拒绝重放(写回会卡住不开死信的组): %v", err)
	}
	if messages, _, err := selectReplays(unprocessable, "guild-trial", true); err != nil || len(messages) != 1 {
		t.Fatalf("显式放行后可以重放: %v", err)
	}
}

// list 逐条列出原位置、原因、次数,标出重复的原位置;输出全英文。
func TestListMarksDuplicateOriginals(t *testing.T) {
	var out bytes.Buffer
	entries := []entry{
		deadLetterEntry(t, 5, "guild-trial", kafkautil.DeadLetterCauseExhausted, 40),
		deadLetterEntry(t, 6, "guild-trial", kafkautil.DeadLetterCauseExhausted, 40),
		{offset: 7, err: errors.New("dead letter header dlq.group missing")},
	}
	if err := list(&out, entries); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"dlq_offset=5 original=match-results[1]@40 group=guild-trial cause=exhausted attempts=20",
		"dlq_offset=6 original=match-results[1]@40",
		"duplicate_of_dlq_offset=5",
		"dlq_offset=7 NOT A DEAD LETTER",
		"3 dead letter(s)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("list 输出缺 %q:\n%s", want, text)
		}
	}
}
