package dbguard

import (
	"encoding/base64"
	"errors"
	"testing"

	component "proto/common/component"
	dbpb "proto/common/database"

	"google.golang.org/protobuf/proto"
)

// recordingObserver 收集上报,断言 histogram / counter 真的被喂了数据。
type recordingObserver struct {
	blob map[string]int64
	row  map[string]int64
	gate []string
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{blob: map[string]int64{}, row: map[string]int64{}}
}

func (o *recordingObserver) ObserveBlobBytes(table, column string, bytes int64) {
	o.blob[table+"."+column] = bytes
}

func (o *recordingObserver) ObserveRowBytes(table string, bytes int64) {
	o.row[table] = bytes
}

func (o *recordingObserver) CountGate(table, column, level string) {
	o.gate = append(o.gate, table+"."+column+"="+level)
}

// snapshotWithData 造一条 player_snapshot,data 是唯一的 bytes blob 列。
func snapshotWithData(n int) proto.Message {
	return &dbpb.PlayerSnapshot{PlayerId: 1, Data: make([]byte, n)}
}

func TestMeasureCountsStoredBase64Bytes(t *testing.T) {
	// proto2mysql 走 pbconv.SerializeFieldAsString,blob 列是 base64 之后才入库。
	// 闸门量的必须是 base64 之后的长度,否则实际占用会比闸门以为的多 33%。
	const payload = 3000
	result := Measure("player_snapshot", snapshotWithData(payload))

	var data *ColumnMeasure
	for i := range result.Columns {
		if result.Columns[i].Column == "data" {
			data = &result.Columns[i]
		}
	}
	if data == nil {
		t.Fatalf("data column not measured, got %+v", result.Columns)
	}
	if data.PayloadBytes != payload {
		t.Fatalf("payload bytes = %d, want %d", data.PayloadBytes, payload)
	}
	want := int64(base64.StdEncoding.EncodedLen(payload))
	if data.StoredBytes != want {
		t.Fatalf("stored bytes = %d, want base64 length %d", data.StoredBytes, want)
	}
	if result.TotalStoredBytes != want {
		t.Fatalf("row total = %d, want %d", result.TotalStoredBytes, want)
	}
}

func TestMeasureSkipsScalarColumns(t *testing.T) {
	// 标量列(uint64 / string)不落 MEDIUMBLOB,不该被算进 blob 预算。
	result := Measure("player_snapshot", &dbpb.PlayerSnapshot{
		Id: 7, PlayerId: 8, ZoneId: 1, Reason: "some very long reason string", Operator: "gm",
	})
	if len(result.Columns) != 0 {
		t.Fatalf("scalar-only row must have no blob columns, got %+v", result.Columns)
	}
	if result.TotalStoredBytes != 0 {
		t.Fatalf("scalar-only row total = %d, want 0", result.TotalStoredBytes)
	}
}

func TestGuardThreeTiers(t *testing.T) {
	const limit = 4096
	// warn 水位 = 4096 × 0.8 = 3276.8 → 3276
	cases := []struct {
		name    string
		payload int
		want    Level
	}{
		// base64 长度 = ceil(n/3)*4
		{"正常", 1200, LevelOK},     // 1600B < 3276B
		{"预警", 2500, LevelWarn},   // 3336B ∈ [3276, 4096]
		{"超限", 3200, LevelReject}, // 4268B > 4096B
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := newRecordingObserver()
			g := NewGuard(Limits{MaxColumnBytes: limit, MaxRowBytes: 1 << 20, WarnRatio: 0.8}, obs)
			result, err := g.Check("player_snapshot", snapshotWithData(tc.payload))
			if result.Level != tc.want {
				t.Fatalf("level = %v, want %v (stored=%d limit=%d)", result.Level, tc.want, result.TotalStoredBytes, limit)
			}
			if tc.want == LevelReject {
				var tooLarge *ErrBlobTooLarge
				if !errors.As(err, &tooLarge) {
					t.Fatalf("over-limit write must be rejected, got err=%v", err)
				}
			} else if err != nil {
				t.Fatalf("level %v must not reject: %v", tc.want, err)
			}
			if _, ok := obs.blob["player_snapshot.data"]; !ok {
				t.Fatal("histogram must be fed on every write, not only on violations")
			}
			if _, ok := obs.row["player_snapshot"]; !ok {
				t.Fatal("row histogram must be fed on every write")
			}
			if tc.want == LevelOK && len(obs.gate) != 0 {
				t.Fatalf("normal write must not increment the gate counter: %v", obs.gate)
			}
			if tc.want != LevelOK && len(obs.gate) == 0 {
				t.Fatal("warn/reject must increment the gate counter")
			}
		})
	}
}

func TestGuardRowTotalTripsIndependently(t *testing.T) {
	// 单列都不超,但一行里多个 blob 列加起来超了 —— 这正是 player_database
	// 有 9 个 blob 列时的真实风险,单列闸拦不住。
	msg := &dbpb.PlayerDatabase{
		PlayerId:        1,
		SkillList:       &component.PlayerSkillListComp{},
		StressTestProbe: &component.PlayerStressTestProbe{TestSig: make([]byte, 900)},
		Currency:        &component.CurrencyComp{},
	}
	obs := newRecordingObserver()
	g := NewGuard(Limits{MaxColumnBytes: 1 << 20, MaxRowBytes: 1000, WarnRatio: 0.8}, obs)

	result, err := g.Check("player_database", msg)
	if result.RowLevel != LevelReject {
		t.Fatalf("row level = %v, want reject (total=%d limit=%d)", result.RowLevel, result.TotalStoredBytes, result.RowLimit)
	}
	if err == nil {
		t.Fatal("row-total violation must reject the write")
	}
	for _, c := range result.Columns {
		if c.Level != LevelOK {
			t.Fatalf("no single column should trip here, but %s did (%d/%d)", c.Column, c.StoredBytes, c.Limit)
		}
	}
	var sawRowGate bool
	for _, g := range obs.gate {
		if g == "player_database."+RowColumnLabel+"=reject" {
			sawRowGate = true
		}
	}
	if !sawRowGate {
		t.Fatalf("row-level violation must be counted under %s, got %v", RowColumnLabel, obs.gate)
	}
}

func TestGuardReportOnlyDoesNotReject(t *testing.T) {
	g := NewGuard(Limits{MaxColumnBytes: 16, MaxRowBytes: 16, WarnRatio: 0.8, ReportOnly: true}, nil)
	result, err := g.Check("player_snapshot", snapshotWithData(4096))
	if err != nil {
		t.Fatalf("report-only mode must not reject: %v", err)
	}
	if result.Level != LevelReject {
		t.Fatalf("report-only must still classify as reject, got %v", result.Level)
	}
}

func TestGuardPerTableOverride(t *testing.T) {
	// player_snapshot 天然就大,必须能单独放宽而不影响 player_database。
	g := NewGuard(Limits{
		MaxColumnBytes: 1024,
		MaxRowBytes:    1024,
		WarnRatio:      0.8,
		PerTable:       map[string]TableLimit{"player_snapshot": {MaxColumnBytes: 1 << 20, MaxRowBytes: 1 << 20}},
	}, nil)

	if _, err := g.Check("player_snapshot", snapshotWithData(4096)); err != nil {
		t.Fatalf("per-table override must apply: %v", err)
	}
	if _, err := g.Check("player_database", &dbpb.PlayerDatabase{
		PlayerId:        1,
		StressTestProbe: &component.PlayerStressTestProbe{TestSig: make([]byte, 4096)},
	}); err == nil {
		t.Fatal("tables without an override must keep the global limit")
	}
}

func TestNewGuardRejectsZeroLimits(t *testing.T) {
	// 零值 Limits 直接构造出来的闸如果按字面执行,上限就是 0 = 全部拒写。
	g := NewGuard(Limits{}, nil)
	if g.Limits().MaxColumnBytes <= 0 || g.Limits().MaxRowBytes <= 0 || g.Limits().WarnRatio <= 0 {
		t.Fatalf("zero limits must be backfilled, got %+v", g.Limits())
	}
	if _, err := g.Check("player_database", &dbpb.PlayerDatabase{PlayerId: 1}); err != nil {
		t.Fatalf("an empty row must never be rejected: %v", err)
	}
}
