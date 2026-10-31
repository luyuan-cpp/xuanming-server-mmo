package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 行号来源的无库用例。真库行为见 snapshot_store_integration_test.go。

type failingRowIDs struct{ err error }

func (f failingRowIDs) Next(context.Context) (uint64, error) { return 0, f.err }

// TestSnapshotWritesFailWithoutRowID:发不出行号时四个写入口都要失败,而且在碰库之前(db 为 nil 不 panic)。
// 没有回退:用 0 或自造的值顶替,撞上别的行就是静默丢数据。
func TestSnapshotWritesFailWithoutRowID(t *testing.T) {
	mintFailure := errors.New("issuer down")
	for _, tc := range []struct {
		name string
		s    *SnapshotStore
		want error
	}{
		{"没接来源", &SnapshotStore{}, ErrNoRowIDSource},
		{"来源报错", &SnapshotStore{snapshotRowIDs: failingRowIDs{mintFailure}, auditRowIDs: failingRowIDs{mintFailure}}, mintFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			row := &SnapshotRow{PlayerID: 1, SnapshotGuid: 7, Source: SnapshotSourceSceneKafka}
			if _, err := tc.s.InsertSnapshot(ctx, row); !errors.Is(err, tc.want) {
				t.Fatalf("InsertSnapshot: %v", err)
			}
			if _, inserted, err := tc.s.InsertSnapshotIfGuidAbsent(ctx, row); !errors.Is(err, tc.want) || inserted {
				t.Fatalf("InsertSnapshotIfGuidAbsent: inserted=%v err=%v", inserted, err)
			}
			if _, err := tc.s.InsertSnapshotsIfGuidAbsent(ctx, []*SnapshotRow{row}); !errors.Is(err, tc.want) {
				t.Fatalf("InsertSnapshotsIfGuidAbsent: %v", err)
			}
			if err := tc.s.InsertAuditLog(ctx, &AuditLogRow{}); !errors.Is(err, tc.want) {
				t.Fatalf("InsertAuditLog: %v", err)
			}
		})
	}
}

// TestSnapshotInsertCarriesRowID:两条单行语句都写 id 列,行号是第一个实参,列数、占位符数、实参数相同。
func TestSnapshotInsertCarriesRowID(t *testing.T) {
	args := snapshotInsertArgs(99, &SnapshotRow{PlayerID: 1, SnapshotGuid: 7})
	if args[0] != any(uint64(99)) {
		t.Fatalf("第一个实参应是行号,got %v", args[0])
	}
	if got := strings.Count(snapshotValuesTupleSQL, "?"); got != len(args) {
		t.Fatalf("占位符 %d 个,实参 %d 个", got, len(args))
	}
	for name, stmt := range map[string]string{
		"insertSnapshotSQL":                   insertSnapshotSQL,
		"insertSnapshotIfGuidAbsentLegacySQL": insertSnapshotIfGuidAbsentLegacySQL,
	} {
		if !strings.Contains(stmt, "(id, player_id,") {
			t.Fatalf("%s 没写 id 列,主键去掉自增之后插入会失败:\n%s", name, stmt)
		}
	}
}
