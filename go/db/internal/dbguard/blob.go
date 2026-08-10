package dbguard

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Level 是大字段三档闸的档位。
type Level uint8

const (
	// LevelOK 正常水位,只进 histogram,不打日志。
	LevelOK Level = iota
	// LevelWarn 到达 WarnRatio 水位:打 WARN + counter,仍然放行。
	LevelWarn
	// LevelReject 超过上限:打 counter;Enforce(非 ReportOnly)时拒写。
	LevelReject
)

// String 给日志与 prometheus label 用。
func (l Level) String() string {
	switch l {
	case LevelWarn:
		return "warn"
	case LevelReject:
		return "reject"
	default:
		return "ok"
	}
}

// TableLimit 是单表的上限覆盖,0 表示沿用全局值。
type TableLimit struct {
	MaxColumnBytes int64
	MaxRowBytes    int64
}

// Limits 是大字段闸的判定参数。
type Limits struct {
	MaxColumnBytes int64
	MaxRowBytes    int64
	WarnRatio      float64
	// ReportOnly 为 true 时超限只计数不拒写(灰度期用)。
	ReportOnly bool
	PerTable   map[string]TableLimit
}

// forTable 解出某张表最终生效的上限。
func (l Limits) forTable(table string) (maxColumn, maxRow int64) {
	maxColumn, maxRow = l.MaxColumnBytes, l.MaxRowBytes
	if override, ok := l.PerTable[table]; ok {
		if override.MaxColumnBytes > 0 {
			maxColumn = override.MaxColumnBytes
		}
		if override.MaxRowBytes > 0 {
			maxRow = override.MaxRowBytes
		}
	}
	return maxColumn, maxRow
}

// ColumnMeasure 是单个 blob 列的量测结果。
type ColumnMeasure struct {
	Column string
	// PayloadBytes 是子消息 / bytes 字段序列化后的原始字节数。
	PayloadBytes int64
	// StoredBytes 是**真正进 MEDIUMBLOB 的字节数**。
	// proto2mysql 走 pbconv.SerializeFieldAsString,把子消息 base64 之后再入库,
	// 所以入库量 = base64.EncodedLen(PayloadBytes) ≈ PayloadBytes × 4/3。
	// 闸门必须量这个数:它才是撞 MEDIUMBLOB 上限、占连接带宽、被原样写回
	// Redis 的那个数。
	StoredBytes int64
	Limit       int64
	Level       Level
}

// Result 是一次落库前量测的完整结果。
type Result struct {
	Table            string
	Columns          []ColumnMeasure
	TotalStoredBytes int64
	RowLimit         int64
	RowLevel         Level
	// Level 是本行所有列与行总量里最严重的那一档。
	Level Level
}

// Warnings 返回需要打 WARN 的可读描述(LevelOK 时为空)。
func (r Result) Warnings() []string {
	var out []string
	for _, c := range r.Columns {
		if c.Level == LevelOK {
			continue
		}
		out = append(out, fmt.Sprintf("column=%s stored=%dB limit=%dB level=%s",
			c.Column, c.StoredBytes, c.Limit, c.Level))
	}
	if r.RowLevel != LevelOK {
		out = append(out, fmt.Sprintf("row_total stored=%dB limit=%dB level=%s",
			r.TotalStoredBytes, r.RowLimit, r.RowLevel))
	}
	return out
}

// String 拼成一行日志。
func (r Result) String() string {
	return fmt.Sprintf("table=%s level=%s %s", r.Table, r.Level, strings.Join(r.Warnings(), "; "))
}

// ErrBlobTooLarge 表示 blob 列或整行超过设计上限,已拒写。
type ErrBlobTooLarge struct {
	Result Result
}

func (e *ErrBlobTooLarge) Error() string {
	return fmt.Sprintf("blob size gate rejected write: %s", e.Result.String())
}

// Observer 接收量测数据。metrics 包实现它;测试可以传 nil。
type Observer interface {
	// ObserveBlobBytes 记录单列入库字节(histogram —— 要看 p99,不是 max)。
	ObserveBlobBytes(table, column string, bytes int64)
	// ObserveRowBytes 记录整行 blob 入库字节之和。
	ObserveRowBytes(table string, bytes int64)
	// CountGate 记录一次 warn / reject。
	CountGate(table, column, level string)
}

// Guard 是大字段三档闸。零值不可用,必须走 NewGuard。
type Guard struct {
	limits Limits
	obs    Observer
}

// NewGuard 构造闸门。limits 里的 0 值由调用方(config.Normalize)负责补齐;
// 这里再兜一次底,避免直接用零值 Limits 构造出「上限 0 = 全部拒写」的闸。
func NewGuard(limits Limits, obs Observer) *Guard {
	if limits.MaxColumnBytes <= 0 {
		limits.MaxColumnBytes = 256 * 1024
	}
	if limits.MaxRowBytes <= 0 {
		limits.MaxRowBytes = 1024 * 1024
	}
	if limits.WarnRatio <= 0 || limits.WarnRatio > 1 {
		limits.WarnRatio = 0.8
	}
	return &Guard{limits: limits, obs: obs}
}

// Limits 返回生效的判定参数(启动日志与测试用)。
func (g *Guard) Limits() Limits { return g.limits }

// Measure 量出一条待落库消息的每个 blob 列入库字节数。
//
// 只量 bytes / message 两种 kind:proto2mysql 的类型映射里只有这两种落到
// MEDIUMBLOB,标量列不可能撑爆。repeated / map 字段被 proto2mysql 的
// SerializeFieldAsString 直接拒绝(表结构里也没有),这里一并跳过。
func Measure(table string, msg proto.Message) Result {
	result := Result{Table: table}
	if msg == nil {
		return result
	}
	ref := msg.ProtoReflect()
	fields := ref.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() {
			continue
		}
		var payload int
		switch fd.Kind() {
		case protoreflect.BytesKind:
			if !ref.Has(fd) {
				continue
			}
			payload = len(ref.Get(fd).Bytes())
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if !ref.Has(fd) {
				continue
			}
			payload = proto.Size(ref.Get(fd).Message().Interface())
		default:
			continue
		}
		stored := int64(base64.StdEncoding.EncodedLen(payload))
		result.Columns = append(result.Columns, ColumnMeasure{
			Column:       string(fd.Name()),
			PayloadBytes: int64(payload),
			StoredBytes:  stored,
		})
		result.TotalStoredBytes += stored
	}
	// 字段顺序按 proto 声明序稳定,再按列名排一次,让日志与告警可比对。
	sort.Slice(result.Columns, func(i, j int) bool {
		return result.Columns[i].Column < result.Columns[j].Column
	})
	return result
}

// Check 量测 + 定档 + 上报。超限且非 ReportOnly 时返回 *ErrBlobTooLarge。
//
// 返回的 Result 始终有效(即便返回了 error),调用方可以据此打日志。
func (g *Guard) Check(table string, msg proto.Message) (Result, error) {
	result := Measure(table, msg)
	maxColumn, maxRow := g.limits.forTable(table)
	columnWarn := int64(float64(maxColumn) * g.limits.WarnRatio)
	rowWarn := int64(float64(maxRow) * g.limits.WarnRatio)

	for i := range result.Columns {
		c := &result.Columns[i]
		c.Limit = maxColumn
		switch {
		case c.StoredBytes > maxColumn:
			c.Level = LevelReject
		case c.StoredBytes >= columnWarn:
			c.Level = LevelWarn
		}
		if g.obs != nil {
			g.obs.ObserveBlobBytes(table, c.Column, c.StoredBytes)
			if c.Level != LevelOK {
				g.obs.CountGate(table, c.Column, c.Level.String())
			}
		}
		if c.Level > result.Level {
			result.Level = c.Level
		}
	}

	result.RowLimit = maxRow
	switch {
	case result.TotalStoredBytes > maxRow:
		result.RowLevel = LevelReject
	case result.TotalStoredBytes >= rowWarn:
		result.RowLevel = LevelWarn
	}
	if g.obs != nil {
		g.obs.ObserveRowBytes(table, result.TotalStoredBytes)
		if result.RowLevel != LevelOK {
			// 行级越限用 column="__row__" 上报,避免再开一个指标名。
			g.obs.CountGate(table, RowColumnLabel, result.RowLevel.String())
		}
	}
	if result.RowLevel > result.Level {
		result.Level = result.RowLevel
	}

	if result.Level == LevelReject && !g.limits.ReportOnly {
		return result, &ErrBlobTooLarge{Result: result}
	}
	return result, nil
}

// RowColumnLabel 是「整行总量」这条越限记录在 counter 里占的 column label。
const RowColumnLabel = "__row__"
