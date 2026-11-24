package authoritysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
)

func TestTiDBVersionAtLeast(t *testing.T) {
	cases := []struct {
		name       string
		version    string
		major      int
		minor      int
		wantReject bool
	}{
		{"VERSION_exact", "8.0.11-TiDB-v7.4.0", 7, 4, false},
		{"VERSION_higher", "8.0.11-TiDB-v8.5.2", 7, 4, false},
		{"tidb_version_multiline", "Release Version: v8.5.2\nEdition: Community", 7, 4, false},
		{"VERSION_too_old", "8.0.11-TiDB-v7.3.0", 7, 4, true},
		{"tidb_version_too_old", "Release Version: v6.5.0\nEdition: Community", 7, 4, true},
		{"unparseable_fail_closed", "8.0.11-TiDB-nightly", 7, 4, true},
		{"plain_mysql_rejected", "8.4.0", 7, 4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tidbVersionAtLeast(tc.version, tc.major, tc.minor)
			if tc.wantReject && err == nil {
				t.Fatalf("tidbVersionAtLeast(%q, %d, %d) = nil, want error", tc.version, tc.major, tc.minor)
			}
			if !tc.wantReject && err != nil {
				t.Fatalf("tidbVersionAtLeast(%q, %d, %d) = %v, want nil", tc.version, tc.major, tc.minor, err)
			}
		})
	}
}

func TestAssertColumnCollationSemantics_BehaviorProbe(t *testing.T) {
	newDB := func(collation string, caseInsensitive, padSpace int64) *sql.DB {
		conn := &backendFakeConn{
			answer: func(query string) ([]string, []driver.Value, error) {
				if strings.Contains(query, "information_schema.COLUMNS") {
					return fakeColumns(1), []driver.Value{collation}, nil
				}
				if strings.HasPrefix(query, "SELECT 'A' COLLATE ") {
					return fakeColumns(2), []driver.Value{caseInsensitive, padSpace}, nil
				}
				return nil, nil, fmt.Errorf("unexpected query: %s", query)
			},
		}
		db := sql.OpenDB(&backendFakeConnector{conn: conn})
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	ctx := context.Background()

	// utf8mb4_0900_bin：大小写敏感 + NO PAD（proto2mysql v0.2.0 主键/唯一键标准）。
	binDB := newDB("utf8mb4_0900_bin", 0, 0)
	if err := AssertColumnCollationSemantics(ctx, binDB, "guild", "name_norm", false, true); err != nil {
		t.Fatalf("expected utf8mb4_0900_bin to pass binary+NO_PAD, got %v", err)
	}
	if err := AssertColumnCollationSemantics(ctx, binDB, "guild", "name_norm", true, true); err == nil {
		t.Fatal("case-sensitive column must fail when case-insensitive is required")
	}

	// utf8mb4_0900_ai_ci：大小写不敏感 + NO PAD。
	ciDB := newDB("utf8mb4_0900_ai_ci", 1, 0)
	if err := AssertColumnCollationSemantics(ctx, ciDB, "accounts", "account", true, true); err != nil {
		t.Fatalf("expected utf8mb4_0900_ai_ci to pass ci+NO_PAD, got %v", err)
	}
	if err := AssertColumnCollationSemantics(ctx, ciDB, "accounts", "account", false, true); err == nil {
		t.Fatal("case-insensitive column must fail when binary/case-sensitive is required")
	}

	// 退化为 PAD SPACE 时必须拒绝。
	padDB := newDB("utf8mb4_general_ci", 1, 1)
	if err := AssertColumnCollationSemantics(ctx, padDB, "accounts", "account", true, true); err == nil {
		t.Fatal("PAD SPACE collation must fail when NO PAD is required")
	}

	// 恶意/异常 collation 名必须被标识符白名单拦截。
	badIdentDB := newDB("utf8mb4; DROP TABLE x", 1, 0)
	if err := AssertColumnCollationSemantics(ctx, badIdentDB, "accounts", "account", true, true); err == nil {
		t.Fatal("unsafe collation identifier must be rejected")
	}
}
