package placement

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestResolveMergedZone(t *testing.T) {
	// 恰好 MaxMergedZoneHops 跳的链:1→2→...→17,共 16 跳。
	longest := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops; z++ {
		longest[z] = z + 1
	}
	// 多一跳的链:1→2→...→18,共 17 跳。
	tooLong := map[uint32]uint32{}
	for z := uint32(1); z <= MaxMergedZoneHops+1; z++ {
		tooLong[z] = z + 1
	}

	for _, tc := range []struct {
		name   string
		merges map[uint32]uint32
		zone   uint32
		want   uint32
		bad    bool
	}{
		{name: "不在表里返回自身", merges: map[uint32]uint32{5: 6}, zone: 7, want: 7},
		{name: "空表返回自身", merges: map[uint32]uint32{}, zone: 7, want: 7},
		{name: "nil 表返回自身", merges: nil, zone: 7, want: 7},
		{name: "一跳", merges: map[uint32]uint32{5: 6}, zone: 5, want: 6},
		{name: "连续合服沿链解析", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 5, want: 7},
		{name: "链中间的区解析到链尾", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 6, want: 7},
		{name: "链尾是自身", merges: map[uint32]uint32{5: 6, 6: 7}, zone: 7, want: 7},
		{name: "恰好 16 跳成功", merges: longest, zone: 1, want: MaxMergedZoneHops + 1},
		{name: "17 跳超限", merges: tooLong, zone: 1, bad: true},
		{name: "自环", merges: map[uint32]uint32{5: 5}, zone: 5, bad: true},
		{name: "两区成环", merges: map[uint32]uint32{5: 6, 6: 5}, zone: 5, bad: true},
		{name: "入环之前有一段尾巴", merges: map[uint32]uint32{4: 5, 5: 6, 6: 5}, zone: 4, bad: true},
		{name: "目标区是 0", merges: map[uint32]uint32{5: 0}, zone: 5, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMergedZone(tc.merges, tc.zone)
			if tc.bad {
				if !errors.Is(err, ErrMergedZoneUnresolvable) {
					t.Fatalf("ResolveMergedZone(%v, %d) = (%d, %v), want ErrMergedZoneUnresolvable", tc.merges, tc.zone, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ResolveMergedZone(%v, %d) = (%d, %v), want (%d, nil)", tc.merges, tc.zone, got, err, tc.want)
			}
		})
	}
}

// fakeMergedZoneDB 是只认一种语句的假驱动:按 src_zone 点查内存映射,记下每条语句。
type fakeMergedZoneDB struct {
	mu      sync.Mutex
	merges  map[uint32]uint32
	fail    error
	queries []string
	args    []uint32
}

func (f *fakeMergedZoneDB) Connect(context.Context) (driver.Conn, error) {
	return &fakeMergedZoneConn{f}, nil
}
func (f *fakeMergedZoneDB) Driver() driver.Driver { return nil }

type fakeMergedZoneConn struct{ db *fakeMergedZoneDB }

func (c *fakeMergedZoneConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *fakeMergedZoneConn) Close() error { return nil }
func (c *fakeMergedZoneConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not supported")
}
func (c *fakeMergedZoneConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("%d args", len(args))
	}
	src, ok := args[0].Value.(int64)
	if !ok {
		return nil, fmt.Errorf("arg %T", args[0].Value)
	}
	f.queries = append(f.queries, query)
	f.args = append(f.args, uint32(src))
	dst, found := f.merges[uint32(src)]
	return &fakeMergedZoneRows{dst: int64(dst), left: found}, nil
}

type fakeMergedZoneRows struct {
	dst  int64
	left bool
}

func (r *fakeMergedZoneRows) Columns() []string { return []string{"dst_zone"} }
func (r *fakeMergedZoneRows) Close() error      { return nil }
func (r *fakeMergedZoneRows) Next(dest []driver.Value) error {
	if !r.left {
		return io.EOF
	}
	r.left = false
	dest[0] = r.dst
	return nil
}

func TestResolveMergedZoneSQL(t *testing.T) {
	ctx := context.Background()
	open := func(f *fakeMergedZoneDB) *sql.DB {
		db := sql.OpenDB(f)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	t.Run("逐跳点查到链尾", func(t *testing.T) {
		f := &fakeMergedZoneDB{merges: map[uint32]uint32{5: 6, 6: 7}}
		got, err := ResolveMergedZoneSQL(ctx, open(f), "mmorpg_global", 5)
		if err != nil || got != 7 {
			t.Fatalf("got (%d, %v), want (7, nil)", got, err)
		}
		if fmt.Sprint(f.args) != "[5 6 7]" {
			t.Fatalf("queried src_zone %v, want [5 6 7]", f.args)
		}
		for _, q := range f.queries {
			if strings.Contains(strings.ToUpper(q), "FOR UPDATE") {
				t.Fatalf("resolving must not lock: %s", q)
			}
			if !strings.Contains(q, "`mmorpg_global`.zone_merged_into") {
				t.Fatalf("query must name the schema: %s", q)
			}
		}
	})

	t.Run("不在表里返回自身", func(t *testing.T) {
		f := &fakeMergedZoneDB{merges: map[uint32]uint32{5: 6}}
		if got, err := ResolveMergedZoneSQL(ctx, open(f), "mmorpg_global", 9); err != nil || got != 9 {
			t.Fatalf("got (%d, %v), want (9, nil)", got, err)
		}
	})

	t.Run("成环报错且跳数有界", func(t *testing.T) {
		f := &fakeMergedZoneDB{merges: map[uint32]uint32{5: 6, 6: 5}}
		if got, err := ResolveMergedZoneSQL(ctx, open(f), "mmorpg_global", 5); !errors.Is(err, ErrMergedZoneUnresolvable) {
			t.Fatalf("got (%d, %v), want ErrMergedZoneUnresolvable", got, err)
		}
		if len(f.queries) != MaxMergedZoneHops+1 {
			t.Fatalf("issued %d queries, want %d", len(f.queries), MaxMergedZoneHops+1)
		}
	})

	t.Run("目标区是 0 报错", func(t *testing.T) {
		f := &fakeMergedZoneDB{merges: map[uint32]uint32{5: 0}}
		if got, err := ResolveMergedZoneSQL(ctx, open(f), "mmorpg_global", 5); !errors.Is(err, ErrMergedZoneUnresolvable) {
			t.Fatalf("got (%d, %v), want ErrMergedZoneUnresolvable", got, err)
		}
	})

	t.Run("SQL 错误不当成没有合服", func(t *testing.T) {
		boom := errors.New("boom")
		f := &fakeMergedZoneDB{fail: boom}
		if got, err := ResolveMergedZoneSQL(ctx, open(f), "mmorpg_global", 5); !errors.Is(err, boom) {
			t.Fatalf("got (%d, %v), want the SQL error", got, err)
		}
	})

	t.Run("schema 不合法一条语句都不发", func(t *testing.T) {
		f := &fakeMergedZoneDB{}
		if _, err := ResolveMergedZoneSQL(ctx, open(f), "bad`schema", 5); !errors.Is(err, ErrRoleRouteFenced) {
			t.Fatalf("err = %v, want ErrRoleRouteFenced", err)
		}
		if len(f.queries) != 0 {
			t.Fatalf("issued %v", f.queries)
		}
	})

	t.Run("q 为 nil 拒绝", func(t *testing.T) {
		if _, err := ResolveMergedZoneSQL(ctx, nil, "mmorpg_global", 5); !errors.Is(err, ErrRoleRouteFenced) {
			t.Fatalf("err = %v, want ErrRoleRouteFenced", err)
		}
	})
}
