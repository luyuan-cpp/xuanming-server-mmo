// kafka_dlq 列出与人工重放消费组死信 topic(<消费组>-dlq)里的记录(设计 docs/design/kafka-bound-consumer-group.md §5,
// 处置步骤 docs/ops/kafka-bound-consumer-runbook.md)。
//
// 用法(在 go/shared 下):
//
//	go run ./cmd/kafka_dlq list   -brokers <b1,b2> -group <消费组> [-from <死信 offset>] [-to <死信 offset>]
//	go run ./cmd/kafka_dlq replay -brokers <b1,b2> -group <消费组> -from <死信 offset> -to <死信 offset> [-execute] [-allow-unprocessable]
//
// list 只读。replay 默认只演练(打印将写回的原位置),带 -execute 才写:把原件(key、value、去掉 dlq.* 之后的原 headers)
// 原样写回原 topic 的原分区,acks=all、逐条同步写。同一原位置(topic/分区/offset)在区间里出现多次(转死信后提交失败、
// 新属主又转了一次)只写一次。写回之后原 topic 的全部消费组都会再收到它,所以要求各组的处理幂等(本仓的消费组都是)。
// cause=unprocessable(记录本身处理不了)默认拒绝重放:写回只会再撞一次,而且同一 topic 上不开死信的组
// (match-rating 保留位点等人工)会被它卡住;修好解码或生产者之后再带 -allow-unprocessable。
// 死信 topic 是 append-only,重放过的条目不会被删除:手册要求把重放过的死信 offset 区间记进处置记录。
// 标准输出与标准错误只打英文。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kafkago "github.com/segmentio/kafka-go"

	"shared/kafkautil"
)

// dlqPartition 是死信 topic 唯一的分区(kafkautil.DeadLetterTopicSpec 建成 1 个分区)。
const dlqPartition = 0

// ioTimeout 是一次读写 broker 的上限。
const ioTimeout = 15 * time.Second

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "list" && os.Args[1] != "replay") {
		fmt.Fprintln(os.Stderr, "usage: kafka_dlq list|replay -brokers <b1,b2> -group <group> [-from N] [-to N] [-execute] [-allow-unprocessable]")
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	brokers := flags.String("brokers", "", "comma separated Kafka brokers")
	group := flags.String("group", "", "consumer group whose dead-letter topic (<group>-dlq) is read")
	from := flags.Int64("from", -1, "first dead-letter offset (inclusive; list defaults to the log start)")
	to := flags.Int64("to", -1, "last dead-letter offset (inclusive; list defaults to the last record)")
	execute := flags.Bool("execute", false, "replay: actually write the originals back (default is a dry run)")
	allowUnprocessable := flags.Bool("allow-unprocessable", false, "replay: also replay records dead-lettered as unprocessable (only after the cause is fixed)")
	_ = flags.Parse(os.Args[2:])

	options := options{brokers: splitBrokers(*brokers), group: strings.TrimSpace(*group), from: *from, to: *to}
	if len(options.brokers) == 0 || options.group == "" {
		fmt.Fprintln(os.Stderr, "kafka_dlq: -brokers and -group are required")
		os.Exit(2)
	}
	if command == "replay" && (options.from < 0 || options.to < options.from) {
		fmt.Fprintln(os.Stderr, "kafka_dlq: replay needs an explicit range: 0 <= -from <= -to")
		os.Exit(2)
	}
	ctx := context.Background()
	entries, err := readDeadLetters(ctx, options)
	if err == nil {
		if command == "list" {
			err = list(os.Stdout, entries)
		} else {
			err = replay(ctx, os.Stdout, options, entries, *execute, *allowUnprocessable)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kafka_dlq:", err)
		os.Exit(1)
	}
}

type options struct {
	brokers  []string
	group    string
	from, to int64
}

// entry 是死信 topic 里的一条:死信自己的 offset 与解出来的死信记录(解不出时 err 非 nil)。
type entry struct {
	offset int64
	record kafkautil.DeadLetterRecord
	err    error
}

func splitBrokers(raw string) []string {
	var out []string
	for _, broker := range strings.Split(raw, ",") {
		if broker = strings.TrimSpace(broker); broker != "" {
			out = append(out, broker)
		}
	}
	return out
}

// readDeadLetters 读死信 topic 在 [from, to] 里的记录(缺省为整段日志),按 offset 升序。
func readDeadLetters(ctx context.Context, o options) ([]entry, error) {
	topic := kafkautil.DeadLetterTopic(o.group)
	first, end, err := readBounds(ctx, o.brokers, topic, dlqPartition)
	if err != nil {
		return nil, fmt.Errorf("read offsets of %s: %w", topic, err)
	}
	from, to := o.from, o.to
	if from < 0 {
		from = first
	}
	if to < 0 {
		to = end - 1
	}
	if from < first {
		return nil, fmt.Errorf("-from %d is before the log start %d of %s (retention already deleted it)", from, first, topic)
	}
	if to >= end {
		return nil, fmt.Errorf("-to %d is at or past the log end %d of %s", to, end, topic)
	}
	if from > to {
		return nil, nil
	}
	reader := kafkago.NewReader(kafkago.ReaderConfig{Brokers: o.brokers, Topic: topic, Partition: dlqPartition, MinBytes: 1, MaxBytes: 16 << 20, MaxWait: 500 * time.Millisecond})
	defer reader.Close()
	if err := reader.SetOffset(from); err != nil {
		return nil, err
	}
	var entries []entry
	for {
		fetchCtx, cancel := context.WithTimeout(ctx, ioTimeout)
		message, err := reader.FetchMessage(fetchCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("fetch %s at offset >= %d: %w", topic, from, err)
		}
		if message.Offset > to {
			return entries, nil
		}
		record, parseErr := kafkautil.ParseDeadLetter(message)
		entries = append(entries, entry{offset: message.Offset, record: record, err: parseErr})
		if message.Offset == to {
			return entries, nil
		}
	}
}

// readBounds 读分区的日志起点与末端(末端 = 下一条要写的 offset)。
func readBounds(ctx context.Context, brokers []string, topic string, partition int) (int64, int64, error) {
	var errs error
	for _, broker := range brokers {
		dialCtx, cancel := context.WithTimeout(ctx, ioTimeout)
		conn, err := kafkago.DialLeader(dialCtx, "tcp", broker, topic, partition)
		cancel()
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(ioTimeout))
		first, end, err := conn.ReadOffsets()
		return first, end, errors.Join(err, conn.Close())
	}
	return 0, 0, errs
}

func list(out io.Writer, entries []entry) error {
	seen := map[string]int64{}
	for _, e := range entries {
		if e.err != nil {
			if _, err := fmt.Fprintf(out, "dlq_offset=%d NOT A DEAD LETTER (cannot replay): %v\n", e.offset, e.err); err != nil {
				return err
			}
			continue
		}
		line := describe(e)
		if first, dup := seen[position(e.record)]; dup {
			line += fmt.Sprintf(" duplicate_of_dlq_offset=%d", first)
		} else {
			seen[position(e.record)] = e.offset
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "%d dead letter(s)\n", len(entries))
	return err
}

func describe(e entry) string {
	r := e.record
	return fmt.Sprintf("dlq_offset=%d original=%s group=%s cause=%s attempts=%d dead_lettered_at=%s key=%s value_bytes=%d reason=%s",
		e.offset, position(r), r.Group, r.Cause, r.Attempts, r.At.UTC().Format(time.RFC3339), printable(r.Original.Key, 64), len(r.Original.Value), printable([]byte(r.Reason), 200))
}

func position(r kafkautil.DeadLetterRecord) string {
	return fmt.Sprintf("%s[%d]@%d", r.Original.Topic, r.Original.Partition, r.Original.Offset)
}

// printable 把字节串截短并转成可打印的带引号文本(非 UTF-8 用转义)。
func printable(raw []byte, limit int) string {
	if len(raw) > limit {
		raw = raw[:limit]
	}
	if utf8.Valid(raw) {
		return strconv.Quote(string(raw))
	}
	return fmt.Sprintf("%q", raw)
}

// selectReplays 选出要写回的原件:组必须对得上,解不出的死信与(默认)unprocessable 的拒绝,同一原位置只写一次。
// 返回要写的原件(带原 topic 与原分区)与逐条的跳过说明。
func selectReplays(entries []entry, group string, allowUnprocessable bool) ([]kafkago.Message, []string, error) {
	var (
		messages []kafkago.Message
		skipped  []string
	)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.err != nil {
			return nil, nil, fmt.Errorf("dlq_offset=%d is not a dead letter written by kafkautil: %v", e.offset, e.err)
		}
		if e.record.Group != group {
			return nil, nil, fmt.Errorf("dlq_offset=%d belongs to group %q, not %q", e.offset, e.record.Group, group)
		}
		if e.record.Cause == kafkautil.DeadLetterCauseUnprocessable && !allowUnprocessable {
			return nil, nil, fmt.Errorf("dlq_offset=%d was dead-lettered as unprocessable; fix the decoder or the producer first, then rerun with -allow-unprocessable", e.offset)
		}
		key := position(e.record)
		if seen[key] {
			skipped = append(skipped, fmt.Sprintf("dlq_offset=%d original=%s skipped: same original already selected", e.offset, key))
			continue
		}
		seen[key] = true
		original := e.record.Original
		messages = append(messages, kafkago.Message{Topic: original.Topic, Partition: original.Partition, Key: original.Key, Value: original.Value, Headers: original.Headers})
	}
	return messages, skipped, nil
}

func replay(ctx context.Context, out io.Writer, o options, entries []entry, execute, allowUnprocessable bool) error {
	messages, skipped, err := selectReplays(entries, o.group, allowUnprocessable)
	if err != nil {
		return err
	}
	for _, line := range skipped {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if err := checkPartitions(ctx, o.brokers, messages); err != nil {
		return err
	}
	if !execute {
		for _, message := range messages {
			if _, err := fmt.Fprintf(out, "dry run: would write %s[%d] key=%s value_bytes=%d\n", message.Topic, message.Partition, printable(message.Key, 64), len(message.Value)); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(out, "dry run: %d original(s) selected from dlq offsets %d..%d; rerun with -execute to write them\n", len(messages), o.from, o.to)
		return err
	}
	writer := &kafkago.Writer{
		Addr: kafkago.TCP(o.brokers...),
		// 原样写回原分区:kafka-go 的 Writer 只问 Balancer,Message.Partition 由它交回(checkPartitions 已核分区存在)。
		Balancer:               kafkago.BalancerFunc(func(message kafkago.Message, _ ...int) int { return message.Partition }),
		RequiredAcks:           kafkago.RequireAll,
		Async:                  false,
		MaxAttempts:            1,
		BatchSize:              1,
		BatchTimeout:           10 * time.Millisecond,
		WriteTimeout:           ioTimeout,
		AllowAutoTopicCreation: false,
	}
	defer writer.Close()
	for i, message := range messages {
		writeCtx, cancel := context.WithTimeout(ctx, ioTimeout)
		err := writer.WriteMessages(writeCtx, message)
		cancel()
		if err != nil {
			return fmt.Errorf("write %s[%d] failed after %d of %d original(s) were written; rerun from this record: %w", message.Topic, message.Partition, i, len(messages), err)
		}
		if _, err := fmt.Fprintf(out, "written %s[%d] key=%s\n", message.Topic, message.Partition, printable(message.Key, 64)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "replayed %d original(s) from dlq offsets %d..%d; record this range in the incident notes\n", len(messages), o.from, o.to)
	return err
}

// checkPartitions 核每条原件的原分区在原 topic 上还存在(topic 没被重建成更少的分区)。
func checkPartitions(ctx context.Context, brokers []string, messages []kafkago.Message) error {
	known := map[string]map[int]bool{}
	for _, message := range messages {
		if known[message.Topic] == nil {
			partitions, err := readPartitions(ctx, brokers, message.Topic)
			if err != nil {
				return fmt.Errorf("read partitions of %s: %w", message.Topic, err)
			}
			known[message.Topic] = partitions
		}
		if !known[message.Topic][message.Partition] {
			return fmt.Errorf("original partition %s[%d] does not exist any more; replay refused", message.Topic, message.Partition)
		}
	}
	return nil
}

func readPartitions(ctx context.Context, brokers []string, topic string) (map[int]bool, error) {
	var errs error
	for _, broker := range brokers {
		dialCtx, cancel := context.WithTimeout(ctx, ioTimeout)
		conn, err := kafkago.DialContext(dialCtx, "tcp", broker)
		cancel()
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(ioTimeout))
		partitions, err := conn.ReadPartitions(topic)
		closeErr := conn.Close()
		if err != nil {
			return nil, errors.Join(err, closeErr)
		}
		out := make(map[int]bool, len(partitions))
		for _, partition := range partitions {
			if partition.Topic == topic {
				out[partition.ID] = true
			}
		}
		return out, nil
	}
	return nil, errs
}
