package kafkautil

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	db_proto "proto/db"

	"github.com/IBM/sarama"
	"google.golang.org/protobuf/proto"
)

// dbTaskPartitionVectors 是 testdata/dbtask_partition_vectors.json 的结构。
// 语言中立:C++ 侧 cpp/tests/kafka_command_test/kafka_partition_contract_test.cpp 读同一份文件,
// 直接调用 librdkafka 的 rd_kafka_msg_partitioner_consistent_random 断言同一批用例。
// 两端都过 = login 的读与 C++ scene 的写逐位落在同一分区。
type dbTaskPartitionVectors struct {
	Schema string `json:"schema"`
	Check  struct {
		Key   string `json:"key"`
		CRC32 uint32 `json:"crc32"`
	} `json:"check"`
	Cases []struct {
		Key             string `json:"key"`
		N               int32  `json:"n"`
		ExpectPartition int32  `json:"expect_partition"`
	} `json:"cases"`
}

func loadDBTaskPartitionVectors(t *testing.T) dbTaskPartitionVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/dbtask_partition_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v dbTaskPartitionVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("vector file has no cases")
	}
	return v
}

// TestDBTaskPartitionCheckValue 先确认本机的 crc32 就是 CRC-32/IEEE:标准自检值 0xCBF43926。
func TestDBTaskPartitionCheckValue(t *testing.T) {
	v := loadDBTaskPartitionVectors(t)
	if v.Check.Key != "123456789" {
		t.Fatalf("check key = %q, want the standard CRC check input \"123456789\"", v.Check.Key)
	}
	if v.Check.CRC32 != 0xCBF43926 {
		t.Fatalf("vector check.crc32 = %d, want 0xCBF43926 (3421780262)", v.Check.CRC32)
	}
	if got := crc32.ChecksumIEEE([]byte(v.Check.Key)); got != v.Check.CRC32 {
		t.Fatalf("crc32.ChecksumIEEE(%q) = %#x, want %#x", v.Check.Key, got, v.Check.CRC32)
	}
}

// TestDBTaskPartitionGoldenVectors 是与 C++ 的对拍本体:四种 Go 侧算法都必须等于表里的值。
// 不一致时保留 -v 全文回报,以机器结果为准,不直接改表。
func TestDBTaskPartitionGoldenVectors(t *testing.T) {
	v := loadDBTaskPartitionVectors(t)
	if len(v.Cases) < 45 {
		t.Fatalf("vector file has %d cases, want >= 45", len(v.Cases))
	}
	coveredN := map[int32]bool{}
	for _, c := range v.Cases {
		coveredN[c.N] = true
		key := []byte(c.Key)

		// 1. 契约函数本身。
		got, err := DBTaskPartition(key, c.N)
		if err != nil {
			t.Fatalf("DBTaskPartition(%q, %d): %v", c.Key, c.N, err)
		}
		if got != c.ExpectPartition {
			t.Errorf("DBTaskPartition(%q, %d) = %d, want %d", c.Key, c.N, got, c.ExpectPartition)
		}

		// 2. 独立重算:不经过契约函数,直接按定义写一遍。
		if indep := int32(crc32.ChecksumIEEE(key) % uint32(c.N)); indep != c.ExpectPartition {
			t.Errorf("crc32.ChecksumIEEE(%q) %% %d = %d, want %d", c.Key, c.N, indep, c.ExpectPartition)
		}

		// 3. sarama 自带的 librdkafka 兼容分区器。
		msg := &sarama.ProducerMessage{Topic: "db_task_zone_1", Key: sarama.StringEncoder(c.Key)}
		saramaPart, err := sarama.NewConsistentCRCHashPartitioner("db_task_zone_1").Partition(msg, c.N)
		if err != nil {
			t.Fatalf("sarama consistent CRC partitioner(%q, %d): %v", c.Key, c.N, err)
		}
		if saramaPart != c.ExpectPartition {
			t.Errorf("sarama.NewConsistentCRCHashPartitioner(%q, %d) = %d, want %d", c.Key, c.N, saramaPart, c.ExpectPartition)
		}

		// 4. 生产者实际装的构造函数。
		ourPart, err := NewDBTaskPartitioner("db_task_zone_1").Partition(msg, c.N)
		if err != nil {
			t.Fatalf("NewDBTaskPartitioner(%q, %d): %v", c.Key, c.N, err)
		}
		if ourPart != c.ExpectPartition {
			t.Errorf("NewDBTaskPartitioner(%q, %d) = %d, want %d", c.Key, c.N, ourPart, c.ExpectPartition)
		}
	}
	for _, n := range []int32{7, 10, 16, 20, math.MaxInt32} {
		if !coveredN[n] {
			t.Errorf("vector file does not cover n=%d", n)
		}
	}
}

// TestNewDBTaskPartitionerMatchesDBTaskPartition 在大样本上确认 sarama 分区器与契约函数逐个相同,
// 防止以后升级 sarama 时分区器的取模写法悄悄变了(1.43.1 与 1.46.2 的 partitioner.go 目前逐字节相同)。
func TestNewDBTaskPartitionerMatchesDBTaskPartition(t *testing.T) {
	ids := []uint64{0, math.MaxUint64}
	for id := uint64(1); id <= 50000; id++ {
		ids = append(ids, id)
	}
	ids = append(ids, bwmarrinShapedIDs(50000)...)
	if len(ids) < 100000 {
		t.Fatalf("sample has %d keys, want >= 1e5", len(ids))
	}

	for _, n := range []int32{1, 2, 3, 5, 7, 8, 10, 16, 20, 32, 64} {
		partitioner := NewDBTaskPartitioner("db_task_zone_1")
		mismatches := 0
		for _, id := range ids {
			key := DBTaskKey(id)
			want, err := DBTaskPartition([]byte(key), n)
			if err != nil {
				t.Fatalf("DBTaskPartition(%q, %d): %v", key, n, err)
			}
			got, err := partitioner.Partition(&sarama.ProducerMessage{Key: sarama.StringEncoder(key)}, n)
			if err != nil {
				t.Fatalf("partitioner(%q, %d): %v", key, n, err)
			}
			if got != want {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("n=%d key=%q: sarama partitioner=%d, DBTaskPartition=%d", n, key, got, want)
				}
			}
		}
		if mismatches > 0 {
			t.Errorf("n=%d: %d/%d keys disagree", n, mismatches, len(ids))
		}
	}
}

// TestDBTaskKeyEncoding 钉住 key 的字节形式:无符号十进制、无前导零,与 C++ std::to_string(uint64_t) 相同。
func TestDBTaskKeyEncoding(t *testing.T) {
	for _, tc := range []struct {
		id   uint64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{1<<63 - 1, "9223372036854775807"},
		{math.MaxUint64, "18446744073709551615"},
	} {
		if got := DBTaskKey(tc.id); got != tc.want {
			t.Errorf("DBTaskKey(%d) = %q, want %q", tc.id, got, tc.want)
		}
		var buf [20]byte
		if got := string(AppendDBTaskKey(buf[:0], tc.id)); got != tc.want {
			t.Errorf("AppendDBTaskKey(%d) = %q, want %q", tc.id, got, tc.want)
		}
	}

	// 向量里的每个 key 都必须是规范形式:ParseUint 再 DBTaskKey 能还原成原串。
	v := loadDBTaskPartitionVectors(t)
	for _, c := range v.Cases {
		id, err := strconv.ParseUint(c.Key, 10, 64)
		if err != nil {
			t.Errorf("vector key %q is not an unsigned decimal: %v", c.Key, err)
			continue
		}
		if canonical := DBTaskKey(id); canonical != c.Key {
			t.Errorf("vector key %q is not canonical (DBTaskKey gives %q)", c.Key, canonical)
		}
	}
}

func TestDBTaskPartitionRejectsEmptyKeyAndBadCount(t *testing.T) {
	for _, key := range [][]byte{nil, {}} {
		got, err := DBTaskPartition(key, 10)
		if !errors.Is(err, ErrEmptyDBTaskKey) {
			t.Errorf("DBTaskPartition(%v, 10) err = %v, want ErrEmptyDBTaskKey", key, err)
		}
		if got != -1 {
			t.Errorf("DBTaskPartition(%v, 10) = %d, want -1", key, got)
		}
	}
	for _, n := range []int32{0, -1} {
		got, err := DBTaskPartition([]byte("1001"), n)
		if !errors.Is(err, ErrInvalidDBTaskPartitionCount) {
			t.Errorf("DBTaskPartition(\"1001\", %d) err = %v, want ErrInvalidDBTaskPartitionCount", n, err)
		}
		if got != -1 {
			t.Errorf("DBTaskPartition(\"1001\", %d) = %d, want -1", n, got)
		}
	}
}

func TestIsDBTaskTopic(t *testing.T) {
	for _, topic := range []string{"db_task_zone_1", "db_task_zone_1_g2"} {
		if !IsDBTaskTopic(topic) {
			t.Errorf("IsDBTaskTopic(%q) = false, want true", topic)
		}
	}
	for _, topic := range []string{"db_task", "gate-cmd_g1", "player_snapshot_topic_g1"} {
		if IsDBTaskTopic(topic) {
			t.Errorf("IsDBTaskTopic(%q) = true, want false", topic)
		}
	}
}

// TestDBTaskPartitionDistribution 是信息性检查:打出存量形状 id 在 N=10 下的分区分布。
//
// 写侧(C++ scene)一直就在 CRC 分布上,这里即使有偏斜也不是本项引入的,也不能靠改契约修
// (换函数 = 换 TopicGeneration)。所以只拦明显异常(某个分区偏离均值超过 30%),
// 失败时回报负责人,不改契约。
func TestDBTaskPartitionDistribution(t *testing.T) {
	const (
		n            = int32(10)
		sampleSize   = 100000
		maxDeviation = 0.30
	)
	counts := make([]int, n)
	for _, id := range bwmarrinShapedIDs(sampleSize) {
		p, err := DBTaskPartition([]byte(DBTaskKey(id)), n)
		if err != nil {
			t.Fatalf("DBTaskPartition: %v", err)
		}
		counts[p]++
	}
	expected := float64(sampleSize) / float64(n)
	for p, c := range counts {
		deviation := (float64(c) - expected) / expected
		t.Logf("partition %d: %d (%.2f%%, deviation %+.2f%%)", p, c, 100*float64(c)/sampleSize, 100*deviation)
		if math.Abs(deviation) > maxDeviation {
			t.Errorf("partition %d holds %d of %d keys (deviation %+.2f%% > %.0f%%): report to the owner, do not change the contract",
				p, c, sampleSize, 100*deviation, 100*maxDeviation)
		}
	}
}

// bwmarrinShapedIDs 确定性地生成 count 个 bwmarrin 形状的 id(毫秒<<22 | node<<9 | step),
// 位布局与 login 存量 SnowFlake PlayerId 相同(13-bit node / 9-bit step / 毫秒,AGENTS.md §7.1)。
// 形状贴近线上:每毫秒连发几个(step 0..7),十几个 login 节点轮流发号。
func bwmarrinShapedIDs(count int) []uint64 {
	const (
		baseMs   = uint64(1_743_000_000_000)
		nodeBits = 13
		stepBits = 9
	)
	ids := make([]uint64, count)
	for i := range ids {
		ms := baseMs + uint64(i/8)*3
		node := uint64(1+(i*7)%16) & (1<<nodeBits - 1)
		step := uint64(i%8) & (1<<stepBits - 1)
		ids[i] = ms<<(nodeBits+stepBits) | node<<stepBits | step
	}
	return ids
}

// ---------------------------------------------------------------------------
// 上线抽样的离线复核器(opt-in,不是生产代码)。
//
// 输入是 kafka-console-consumer 对 db_task topic 的 assign 模式抽样,参数:
//   --property print.partition=true --property print.offset=true --property print.key=true --property print.value=true
//   --value-deserializer org.apache.kafka.common.serialization.BytesDeserializer
// 每行形如 `Partition:3<TAB>Offset:42<TAB>1001<TAB><value>`。value 是 Kafka Bytes.toString() 的输出:
// 0x20..0x7E 之间除反斜杠外原样输出,其余字节(含反斜杠、TAB、换行)一律写成 \xNN。
// 所以 value 里不会有裸 TAB / 换行,反斜杠只会以 \x 转义出现。
//
// 复核口径与 go/db 的 observeProducerPartition 相同:先反转义再 proto.Unmarshal 成 DBTask,
// op 与 key 都取自解码结果;Kafka key 必须等于 DBTaskKey(task.Key),分区必须等于 DBTaskPartition(key, N)。
// ---------------------------------------------------------------------------

// utf8BOM 是 pwsh 写文件时可能带上的 UTF-8 BOM。
const utf8BOM = "\xEF\xBB\xBF"

const (
	auditVerdictOK                = "ok"
	auditVerdictPartitionMismatch = "partition_mismatch"
	auditVerdictKeyMismatch       = "key_mismatch"
)

// dumpRecord 是抽样输出里解析好的一行。
type dumpRecord struct {
	partition int32
	offset    int64
	key       string
	task      *db_proto.DBTask
}

// parseConsoleConsumerLine 解析一行抽样输出:剥掉 UTF-8 BOM 与行尾 \r,按 TAB 切分,
// 读出开头的 Partition: / Offset:(允许前面还有时间戳字段),其后第一个字段是 key,
// 剩余字段拼回作 value;value 反转义后解成 DBTask。任何一步不符都报错(fail-closed)。
func parseConsoleConsumerLine(line string) (dumpRecord, error) {
	line = strings.TrimPrefix(line, utf8BOM)
	line = strings.TrimRight(line, "\r\n")
	fields := strings.Split(line, "\t")

	var rec dumpRecord
	havePartition, haveOffset := false, false
	i := 0
metadata:
	for ; i < len(fields); i++ {
		f := fields[i]
		switch {
		case strings.HasPrefix(f, "Partition:"):
			if havePartition {
				return dumpRecord{}, fmt.Errorf("duplicate Partition field")
			}
			p, err := strconv.ParseInt(strings.TrimPrefix(f, "Partition:"), 10, 32)
			if err != nil || p < 0 {
				return dumpRecord{}, fmt.Errorf("bad Partition field %q", f)
			}
			rec.partition = int32(p)
			havePartition = true
		case strings.HasPrefix(f, "Offset:"):
			if haveOffset {
				return dumpRecord{}, fmt.Errorf("duplicate Offset field")
			}
			o, err := strconv.ParseInt(strings.TrimPrefix(f, "Offset:"), 10, 64)
			if err != nil || o < 0 {
				return dumpRecord{}, fmt.Errorf("bad Offset field %q", f)
			}
			rec.offset = o
			haveOffset = true
		case strings.HasPrefix(f, "CreateTime:"), strings.HasPrefix(f, "LogAppendTime:"), f == "NO_TIMESTAMP":
			// print.timestamp=true 时排在最前面,跳过。
		default:
			// 第一个不是元数据前缀的字段就是 key。
			break metadata
		}
	}
	if !havePartition {
		return dumpRecord{}, fmt.Errorf("missing Partition field (run the console consumer with print.partition=true)")
	}
	if !haveOffset {
		return dumpRecord{}, fmt.Errorf("missing Offset field (run the console consumer with print.offset=true)")
	}
	if i+1 >= len(fields) {
		return dumpRecord{}, fmt.Errorf("missing key or value field (run with print.key=true and print.value=true)")
	}
	rec.key = fields[i]
	raw, err := unescapeKafkaBytes(strings.Join(fields[i+1:], "\t"))
	if err != nil {
		return dumpRecord{}, fmt.Errorf("value: %w", err)
	}
	var task db_proto.DBTask
	if err := proto.Unmarshal(raw, &task); err != nil {
		return dumpRecord{}, fmt.Errorf("value is not a DBTask (was BytesDeserializer used?): %w", err)
	}
	rec.task = &task
	return rec, nil
}

// unescapeKafkaBytes 把 Kafka Bytes.toString() 的输出还原成原始字节。
// 只接受 0x20..0x7E 的可打印字符(反斜杠除外)与 \xNN;其它字节说明输出格式或文件编码不对,报错。
func unescapeKafkaBytes(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			if i+4 > len(s) {
				return nil, fmt.Errorf("truncated escape at byte %d", i)
			}
			if s[i+1] != 'x' {
				return nil, fmt.Errorf("unexpected escape %q at byte %d", s[i:i+2], i)
			}
			b, err := strconv.ParseUint(s[i+2:i+4], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("bad hex escape %q at byte %d", s[i:i+4], i)
			}
			out = append(out, byte(b))
			i += 3
			continue
		}
		if c < 0x20 || c > 0x7E {
			return nil, fmt.Errorf("unescaped byte 0x%02X at byte %d (wrong deserializer or file encoding)", c, i)
		}
		out = append(out, c)
	}
	return out, nil
}

// classifyDumpOp 按解码出的 DBTask.op 归类:read / write,其余一律 other。
func classifyDumpOp(task *db_proto.DBTask) string {
	if task == nil {
		return "other"
	}
	switch task.Op {
	case db_proto.DBOp_DB_OP_READ:
		return "read"
	case db_proto.DBOp_DB_OP_WRITE:
		return "write"
	default:
		return "other"
	}
}

// auditDumpVerdict 与 go/db observer 同口径:先核对 Kafka key == DBTaskKey(task.Key),再核对分区。
func auditDumpVerdict(rec dumpRecord, partitionCount int32) (verdict string, expected int32) {
	if rec.task == nil || rec.key != DBTaskKey(rec.task.Key) {
		return auditVerdictKeyMismatch, -1
	}
	want, err := DBTaskPartition([]byte(rec.key), partitionCount)
	if err != nil {
		// key 已与 DBTaskKey 相等(非空),partitionCount 已由调用方校验为正,走不到这里;
		// 万一走到也按不符计,不放行。
		return auditVerdictPartitionMismatch, -1
	}
	if want != rec.partition {
		return auditVerdictPartitionMismatch, want
	}
	return auditVerdictOK, want
}

// TestParseConsoleConsumerLine 是解析器本身的 hermetic 用例(不需要任何环境)。
func TestParseConsoleConsumerLine(t *testing.T) {
	// DBTask{key=1001, op=DB_OP_WRITE, msg_type="player_database"} 的线上字节按 Bytes.toString() 转义后的样子:
	// 0x08 0xE9 0x07 = field 1 varint 1001;0x18 0x02 = field 3 enum WRITE;'"'(0x22) 0x0F "player_database" = field 4。
	const writeValue = `\x08\xE9\x07\x18\x02"\x0Fplayer_database`
	line := "Partition:3\tOffset:42\t1001\t" + writeValue

	rec, err := parseConsoleConsumerLine(line)
	if err != nil {
		t.Fatalf("parse write line: %v", err)
	}
	if rec.partition != 3 || rec.offset != 42 || rec.key != "1001" {
		t.Fatalf("parsed partition=%d offset=%d key=%q, want 3/42/\"1001\"", rec.partition, rec.offset, rec.key)
	}
	if rec.task.Key != 1001 || rec.task.MsgType != "player_database" {
		t.Fatalf("decoded task key=%d msgType=%q, want 1001/player_database", rec.task.Key, rec.task.MsgType)
	}
	if op := classifyDumpOp(rec.task); op != "write" {
		t.Fatalf("op = %q, want write", op)
	}

	readLine := strings.Replace(line, `\x18\x02`, `\x18\x01`, 1)
	rec, err = parseConsoleConsumerLine(readLine)
	if err != nil {
		t.Fatalf("parse read line: %v", err)
	}
	if op := classifyDumpOp(rec.task); op != "read" {
		t.Fatalf("op = %q, want read", op)
	}

	// pwsh 写文件可能带 BOM,Windows 换行带 \r;前面还可能有时间戳字段。
	for _, variant := range []string{
		utf8BOM + line,
		line + "\r",
		"CreateTime:1760500000000\t" + line,
	} {
		if _, err := parseConsoleConsumerLine(variant); err != nil {
			t.Errorf("variant %q must parse: %v", variant, err)
		}
	}

	// 反斜杠本身在 value 里只会以 \x5C 出现,反转义后要还原成原字节。
	bs, err := parseConsoleConsumerLine("Partition:0\tOffset:1\t1001\t" + `\x08\xE9\x07"\x03a\x5Cb`)
	if err != nil {
		t.Fatalf("parse escaped backslash: %v", err)
	}
	if bs.task.MsgType != `a\b` {
		t.Fatalf("msgType = %q, want a\\b", bs.task.MsgType)
	}

	for name, bad := range map[string]string{
		"missing offset":    "Partition:3\t1001\t" + writeValue,
		"missing partition": "Offset:42\t1001\t" + writeValue,
		"missing value":     "Partition:3\tOffset:42\t1001",
		"bad hex escape":    "Partition:3\tOffset:42\t1001\t" + `\x0G`,
		"truncated escape":  "Partition:3\tOffset:42\t1001\t" + `\x0`,
		"raw control byte":  "Partition:3\tOffset:42\t1001\t\x08\xE9\x07",
		"null value":        "Partition:3\tOffset:42\t1001\tnull",
		"stderr summary":    "Processed a total of 500 messages",
	} {
		if _, err := parseConsoleConsumerLine(bad); err == nil {
			t.Errorf("%s: %q must fail to parse", name, bad)
		}
	}

	// 判定口径:key 不等于 DBTaskKey(task.Key) 判 key_mismatch;分区不等于契约判 partition_mismatch。
	rec, err = parseConsoleConsumerLine(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want, err := DBTaskPartition([]byte("1001"), 10)
	if err != nil {
		t.Fatalf("DBTaskPartition: %v", err)
	}
	rec.partition = want
	if v, _ := auditDumpVerdict(rec, 10); v != auditVerdictOK {
		t.Errorf("verdict = %q, want ok", v)
	}
	rec.partition = (want + 1) % 10
	if v, exp := auditDumpVerdict(rec, 10); v != auditVerdictPartitionMismatch || exp != want {
		t.Errorf("verdict = %q expected=%d, want partition_mismatch/%d", v, exp, want)
	}
	rec.partition = want
	rec.key = "1002"
	if v, _ := auditDumpVerdict(rec, 10); v != auditVerdictKeyMismatch {
		t.Errorf("verdict = %q, want key_mismatch", v)
	}
}

// TestDBTaskPartitionAuditDump 是上线前只读抽样的离线复核(rollout 第 1 步),默认 Skip。
//
//	$env:DBTASK_AUDIT_DUMP='<console consumer 的 stdout 抽样文件>'; $env:DBTASK_AUDIT_PARTITIONS='10'
//	go test ./kafkautil/ -run TestDBTaskPartitionAuditDump -count=1 -v
//
// 抽样文件只能捕获 stdout(stderr 的 "Processed a total of N messages" 混进来会被判为解析失败);
// Windows 上用 pwsh 7 的 Out-File -Encoding utf8NoBOM,或在容器内落盘再 kubectl cp 拷出,
// PS 5.1 的 `>` 会写成 UTF-16LE。
//
// 失败条件(任一即 Fatal,fail-closed):有解析失败的行;有效行数为 0;一条写都没有(证明不了写侧);
// 有 op 既不是 read 也不是 write 的行;写有任何非 ok。读不符只记录比例(旧 login 下约 0.9)。
func TestDBTaskPartitionAuditDump(t *testing.T) {
	dumpPath := os.Getenv("DBTASK_AUDIT_DUMP")
	partitionsEnv := os.Getenv("DBTASK_AUDIT_PARTITIONS")
	if dumpPath == "" || partitionsEnv == "" {
		t.Skip("set DBTASK_AUDIT_DUMP and DBTASK_AUDIT_PARTITIONS to audit a console-consumer dump")
	}
	n64, err := strconv.ParseInt(partitionsEnv, 10, 32)
	if err != nil || n64 <= 0 {
		t.Fatalf("DBTASK_AUDIT_PARTITIONS=%q must be a positive int32", partitionsEnv)
	}
	n := int32(n64)

	f, err := os.Open(dumpPath)
	if err != nil {
		t.Fatalf("open dump: %v", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if head, err := r.Peek(2); err == nil && ((head[0] == 0xFF && head[1] == 0xFE) || (head[0] == 0xFE && head[1] == 0xFF)) {
		t.Fatalf("dump file is UTF-16 encoded: capture stdout with pwsh 7 Out-File -Encoding utf8NoBOM (or kubectl cp a file written inside the container)")
	}

	type mismatch struct {
		lineNo    int
		partition int32
		offset    int64
		key       string
		op        string
		verdict   string
		expected  int32
	}
	counts := map[string]map[string]int{}
	var mismatches []mismatch
	var parseErrors []string
	parseErrorCount, valid, lineNo := 0, 0, 0

	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if strings.TrimSpace(strings.TrimPrefix(line, utf8BOM)) != "" {
				rec, err := parseConsoleConsumerLine(line)
				if err != nil {
					parseErrorCount++
					if len(parseErrors) < 20 {
						parseErrors = append(parseErrors, fmt.Sprintf("line %d: %v", lineNo, err))
					}
				} else {
					valid++
					op := classifyDumpOp(rec.task)
					verdict, expected := auditDumpVerdict(rec, n)
					if counts[op] == nil {
						counts[op] = map[string]int{}
					}
					counts[op][verdict]++
					if verdict != auditVerdictOK && len(mismatches) < 20 {
						mismatches = append(mismatches, mismatch{lineNo, rec.partition, rec.offset, rec.key, op, verdict, expected})
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatalf("read dump: %v", readErr)
		}
	}

	total := func(op string) int {
		sum := 0
		for _, c := range counts[op] {
			sum += c
		}
		return sum
	}
	ops := make([]string, 0, len(counts))
	for op := range counts {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	t.Logf("audit summary: partitions=%d lines=%d valid=%d parseErrors=%d", n, lineNo, valid, parseErrorCount)
	for _, op := range ops {
		t.Logf("  op=%-5s ok=%d partition_mismatch=%d key_mismatch=%d",
			op, counts[op][auditVerdictOK], counts[op][auditVerdictPartitionMismatch], counts[op][auditVerdictKeyMismatch])
	}
	if reads := total("read"); reads > 0 {
		t.Logf("  read mismatch ratio = %.4f (%d/%d; about 0.9 while the old FNV login is still running)",
			float64(reads-counts["read"][auditVerdictOK])/float64(reads), reads-counts["read"][auditVerdictOK], reads)
	}
	for _, m := range mismatches {
		t.Logf("  mismatch line=%d partition=%d offset=%d key=%q op=%s verdict=%s expectedPartition=%d",
			m.lineNo, m.partition, m.offset, m.key, m.op, m.verdict, m.expected)
	}
	for _, e := range parseErrors {
		t.Logf("  parse error %s", e)
	}

	if parseErrorCount > 0 {
		t.Fatalf("%d lines failed to parse: fix the capture (stdout only, BytesDeserializer, print.partition/offset/key/value) and rerun", parseErrorCount)
	}
	if valid == 0 {
		t.Fatal("dump has no records: an empty sample proves nothing")
	}
	if total("write") == 0 {
		t.Fatal("dump has no write records: the sample cannot prove the C++ writer follows the contract")
	}
	if others := total("other"); others > 0 {
		t.Fatalf("%d records have an op other than read/write: the capture format or the producer is not what the audit assumes", others)
	}
	if bad := total("write") - counts["write"][auditVerdictOK]; bad > 0 {
		t.Fatalf("%d write records violate the partition contract: stop the rollout and find the producer from the mismatches above", bad)
	}
}
