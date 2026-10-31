package placement

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// revValidVectors / revInvalidVectors 是 rev 规范形式的测试向量。发布脚本的 valid_rev 与 data_service 的
// 导入 / 审计(routetool)都按这一口径判 rev;改这里必须同步看 PublishRouteScript 与 ValidRev。
var revValidVectors = []string{
	"1",
	"9",
	"10",
	"18446744073709551615", // MaxUint64
}

var revInvalidVectors = []string{
	"",
	"0",
	"01",
	"-1",
	"1a",
	" 1",
	"1 ",
	"+1",
	"123456789012345678901", // 21 位
	"18446744073709551616",  // MaxUint64 + 1:20 位但溢出
	"99999999999999999999",
}

func TestRouteKeys(t *testing.T) {
	if got := MapRevKey(90002); got != "player:maprev:90002" {
		t.Fatalf("MapRevKey = %q", got)
	}
	if RouteReadyKey != "mapping:route:ready" {
		t.Fatalf("RouteReadyKey = %q", RouteReadyKey)
	}

	keys := PublishRouteKeys(7)
	want := []string{"player:zone:7", "player:placement:7", "player:maprev:7"}
	if len(keys) != len(want) {
		t.Fatalf("PublishRouteKeys(7) = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("PublishRouteKeys(7)[%d] = %q, want %q(KEYS 顺序是脚本契约)", i, keys[i], want[i])
		}
	}
}

func TestRevVectors(t *testing.T) {
	for _, s := range revValidVectors {
		if !ValidRev(s) {
			t.Fatalf("ValidRev(%q) = false, want true", s)
		}
	}
	for _, s := range revInvalidVectors {
		if ValidRev(s) {
			t.Fatalf("ValidRev(%q) = true, want false", s)
		}
	}

	if got := FormatRev(0); got != "" {
		t.Fatalf("FormatRev(0) = %q, want empty(0 不是合法版本)", got)
	}
	for _, rev := range []uint64{1, 10, 18446744073709551615} {
		s := FormatRev(rev)
		if !ValidRev(s) {
			t.Fatalf("FormatRev(%d) = %q 过不了 ValidRev", rev, s)
		}
	}
}

func TestCompareRevLengthBeforeLexicographic(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"9", "10", -1}, // 字典序会说 "9" > "10",必须先比长度
		{"2", "10", -1},
		{"10", "9", 1},
		{"10", "10", 0},
		{"18446744073709551615", "18446744073709551614", 1},
		{"18446744073709551614", "18446744073709551615", -1},
	} {
		got, err := CompareRev(tc.a, tc.b)
		if err != nil {
			t.Fatalf("CompareRev(%q, %q) error: %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Fatalf("CompareRev(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, bad := range []string{"", "01", "18446744073709551616"} {
		if _, err := CompareRev(bad, "1"); !errors.Is(err, ErrMalformed) {
			t.Fatalf("CompareRev(%q, \"1\") error = %v, want ErrMalformed", bad, err)
		}
		if _, err := CompareRev("1", bad); !errors.Is(err, ErrMalformed) {
			t.Fatalf("CompareRev(\"1\", %q) error = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestClassifyPublishReply(t *testing.T) {
	const (
		rev  = "5"
		home = "2"
		plc  = "2:1"
	)
	for _, tc := range []struct {
		name string
		raw  any
		want PublishReply
	}{
		{
			name: "已写入",
			raw:  []any{int64(1), rev},
			want: PublishReply{Outcome: PublishApplied, CurRev: rev, CurHome: home, CurPlacement: plc},
		},
		{
			name: "缓存更新_被取代",
			raw:  []any{int64(0), "10", home, plc},
			want: PublishReply{Outcome: PublishSuperseded, CurRev: "10", CurHome: home, CurPlacement: plc},
		},
		{
			name: "同版本同值_已确认",
			raw:  []any{int64(0), rev, home, plc},
			want: PublishReply{Outcome: PublishConfirmed, CurRev: rev, CurHome: home, CurPlacement: plc},
		},
		{
			name: "同版本_home不同_冲突",
			raw:  []any{int64(0), rev, "3", plc},
			want: PublishReply{Outcome: PublishConflict, CurRev: rev, CurHome: "3", CurPlacement: plc},
		},
		{
			name: "同版本_落点不同_冲突",
			raw:  []any{int64(0), rev, home, "2:2"},
			want: PublishReply{Outcome: PublishConflict, CurRev: rev, CurHome: home, CurPlacement: "2:2"},
		},
		{
			name: "无maprev_遗留值不同",
			raw:  []any{int64(2), "", "3", ""},
			want: PublishReply{Outcome: PublishLegacyConflict, CurHome: "3", CurPlacement: ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyPublishReply(tc.raw, rev, home, plc)
			if err != nil {
				t.Fatalf("ClassifyPublishReply(%#v) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ClassifyPublishReply(%#v) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}

	// 脚本不可能产生的回复:一律 fail-closed。
	for _, tc := range []struct {
		name string
		raw  any
	}{
		{"已写入但rev不是本次", []any{int64(1), "6"}},
		{"已写入但rev不是串", []any{int64(1), int64(5)}},
		{"未改但缓存rev小于本次", []any{int64(0), "4", home, plc}},
		{"未改但缓存rev是20位且溢出", []any{int64(0), "18446744073709551616", home, plc}},
		{"未改但缓存rev有前导零", []any{int64(0), "05", home, plc}},
		{"未改但元素个数不对", []any{int64(0), rev, home}},
		{"遗留回复但值反而相同", []any{int64(2), "", home, plc}},
		{"遗留回复带了rev", []any{int64(2), "1", "3", ""}},
		{"nil", nil},
		{"空数组", []any{}},
		{"首元素不是整数", []any{"1", "3"}},
		{"未知回复码", []any{int64(4)}},
		{"已删除的围栏回复码", []any{int64(3)}},
		{"不是数组", "OK"},
	} {
		t.Run("拒绝_"+tc.name, func(t *testing.T) {
			if got, err := ClassifyPublishReply(tc.raw, rev, home, plc); !errors.Is(err, ErrUnexpectedPublishReply) {
				t.Fatalf("ClassifyPublishReply(%#v) = (%+v, %v), want ErrUnexpectedPublishReply", tc.raw, got, err)
			}
		})
	}
}

func TestPublishOutcomeStringsAreStableLowCardinality(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{PublishApplied.String(), "applied"},
		{PublishSuperseded.String(), "superseded"},
		{PublishConfirmed.String(), "confirmed"},
		{PublishConflict.String(), "conflict"},
		{PublishLegacyConflict.String(), "legacy_conflict"},
		{PublishOutcome(0).String(), "unknown"},
	} {
		if tc.got != tc.want {
			t.Fatalf("outcome label = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestRouteRowShard 钉住 player_route 的行分片:模数是聚簇主键的一部分,任何环境建过表之后改它,
// 存量行的 route_shard 全部与新算法不一致(按新片点查读不到旧行,CHECK 也会拒绝新写)。
func TestRouteRowShard(t *testing.T) {
	if RouteRowShards != 64 {
		t.Fatalf("RouteRowShards = %d, want 64:改它会让存量行的主键全部失效(player_route 聚簇主键首列 = player_id %% 64,"+
			"schema.go 的 chk_route_shard 写死 64),只能新表迁移", RouteRowShards)
	}
	for _, tc := range []struct {
		id   uint64
		want uint32
	}{
		{0, 0},
		{1, 1},
		{15, 15},
		{16, 16}, // 模数还是 16 的话这里是 0
		{63, 63}, // 最后一片
		{64, 0},  // 回绕
		{65, 1},
		// 4242 = 66×64 + 18;后两个的低 6 位全 1。
		{4242, 18},
		{1<<55 - 1, 63},
		{math.MaxUint64, 63},
	} {
		if got := RouteRowShard(tc.id); got != tc.want {
			t.Fatalf("RouteRowShard(%d) = %d, want %d", tc.id, got, tc.want)
		}
	}
}

// ── 就绪标记(第 1b-1 批)─────────────────────────────────────────────────────────

// validReadyMarker 是一份合规的标记(各字段取边界附近的合法值:generation 32 字符、leader 带全部允许的标点)。
func validReadyMarker() RouteReadyMarker {
	return RouteReadyMarker{
		V:            RouteReadyFormatVersion,
		Generation:   "0123456789abcdef0123456789abcdef",
		RedisRunID:   strings.Repeat("a1", 20),
		MasterReplID: strings.Repeat("0f", 20),
		SetAtMs:      1_760_000_000_123,
		WatermarkMs:  1_760_000_000_000,
		Leader:       "route_leader@pod-1.data-service:0a1b2c3d",
	}
}

// TestRouteReadyMarkerRoundTrip:编码后解码逐字段相等、编码确定(同一份值逐字节相同,CAS 按原值比较靠它);
// V 为 0 时按当前版本编码;JSON 里多出的未知字段解码时忽略(字段只增不改名)。
func TestRouteReadyMarkerRoundTrip(t *testing.T) {
	m := validReadyMarker()
	raw, err := EncodeRouteReadyMarker(m)
	if err != nil {
		t.Fatalf("EncodeRouteReadyMarker: %v", err)
	}
	const want = `{"v":1,"generation":"0123456789abcdef0123456789abcdef",` +
		`"redis_run_id":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1","master_replid":"0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",` +
		`"set_at_ms":1760000000123,"watermark_ms":1760000000000,"leader":"route_leader@pod-1.data-service:0a1b2c3d"}`
	if raw != want {
		t.Fatalf("encoded = %s\nwant      %s(字段名与顺序是读写双方的契约)", raw, want)
	}
	got, err := DecodeRouteReadyMarker(raw)
	if err != nil || got != m {
		t.Fatalf("DecodeRouteReadyMarker(%s) = (%+v, %v), want %+v", raw, got, err, m)
	}

	zeroV := m
	zeroV.V = 0
	if again, err := EncodeRouteReadyMarker(zeroV); err != nil || again != raw {
		t.Fatalf("V=0 应按当前版本编码:got (%s, %v)", again, err)
	}

	withUnknown := strings.TrimSuffix(raw, "}") + `,"evicted_keys_baseline":7,"note":"x"}`
	got, err = DecodeRouteReadyMarker(withUnknown)
	if err != nil || got != m {
		t.Fatalf("未知字段应被忽略:got (%+v, %v)", got, err)
	}
}

// TestRouteReadyMarkerRejects:不合规的值一律 ErrMalformed(调用方可以按原值撤掉);v 大于当前版本是另一个错误
// ErrRouteReadyMarkerNewerVersion(只记日志,不删也不信任),而且不是 ErrMalformed。
func TestRouteReadyMarkerRejects(t *testing.T) {
	// encode 在合规夹具上改一处,绕过 Encode 的校验直接编成 JSON(edit 为 nil 时就是合规值本身)。
	encode := func(edit func(*RouteReadyMarker)) string {
		t.Helper()
		m := validReadyMarker()
		if edit != nil {
			edit(&m)
		}
		return marshalUnchecked(t, m)
	}
	malformed := []struct {
		name string
		raw  string
	}{
		{"不是JSON", "ready"},
		{"JSON数组", `[1]`},
		{"null", `null`},
		{"v缺席", strings.Replace(encode(nil), `"v":1,`, "", 1)},
		{"v为0", encode(func(m *RouteReadyMarker) { m.V = 0 })},
		{"v为负", encode(func(m *RouteReadyMarker) { m.V = -1 })},
		{"v不是整数", strings.Replace(encode(nil), `"v":1`, `"v":"1"`, 1)},
		{"缺generation", encode(func(m *RouteReadyMarker) { m.Generation = "" })},
		{"generation超长", encode(func(m *RouteReadyMarker) { m.Generation = strings.Repeat("a", 33) })},
		{"generation大写", encode(func(m *RouteReadyMarker) { m.Generation = "ABCDEF" })},
		{"run_id长度不对", encode(func(m *RouteReadyMarker) { m.RedisRunID = strings.Repeat("a", 39) })},
		{"run_id非十六进制", encode(func(m *RouteReadyMarker) { m.RedisRunID = strings.Repeat("g", 40) })},
		{"replid长度不对", encode(func(m *RouteReadyMarker) { m.MasterReplID = strings.Repeat("a", 41) })},
		{"set_at为0", encode(func(m *RouteReadyMarker) { m.SetAtMs = 0 })},
		{"watermark为0", encode(func(m *RouteReadyMarker) { m.WatermarkMs = 0 })},
		{"leader为空", encode(func(m *RouteReadyMarker) { m.Leader = "" })},
		{"leader非ASCII", encode(func(m *RouteReadyMarker) { m.Leader = "route_leader@主机:0a1b2c3d" })},
		{"leader带空格", encode(func(m *RouteReadyMarker) { m.Leader = "route leader" })},
		{"leader超过96字节", encode(func(m *RouteReadyMarker) { m.Leader = strings.Repeat("a", 97) })},
		// 2026-10-18:v=1 时已知字段类型不对照旧是畸形(只有 v 更大才不校验其余字段)。
		{"v为1而set_at不是整数", `{"v":1,"set_at_ms":"x"}`},
	}
	for _, tc := range malformed {
		t.Run("畸形_"+tc.name, func(t *testing.T) {
			_, err := DecodeRouteReadyMarker(tc.raw)
			if !errors.Is(err, ErrMalformed) || errors.Is(err, ErrRouteReadyMarkerNewerVersion) {
				t.Fatalf("DecodeRouteReadyMarker(%s) error = %v, want ErrMalformed", tc.raw, err)
			}
		})
	}

	t.Run("v大于当前版本_只认新版本不当畸形", func(t *testing.T) {
		// 新版本的值连其余字段都可能不合本版本的规则(这里故意把 generation 写空):不校验,只报「看不懂」。
		raw := marshalUnchecked(t, RouteReadyMarker{V: RouteReadyFormatVersion + 1})
		_, err := DecodeRouteReadyMarker(raw)
		if !errors.Is(err, ErrRouteReadyMarkerNewerVersion) || errors.Is(err, ErrMalformed) {
			t.Fatalf("DecodeRouteReadyMarker(%s) error = %v, want ErrRouteReadyMarkerNewerVersion(且不是 ErrMalformed)", raw, err)
		}
	})

	t.Run("v大于当前版本且已知字段改了类型_仍是新版本", func(t *testing.T) {
		// 2026-10-18 修复评审:一步整体解时 json 类型错先于 v 的判断,这种值会被判成畸形、随后被 compare-and-delete 删掉。
		raw := `{"v":2,"set_at_ms":"x"}`
		_, err := DecodeRouteReadyMarker(raw)
		if !errors.Is(err, ErrRouteReadyMarkerNewerVersion) || errors.Is(err, ErrMalformed) {
			t.Fatalf("DecodeRouteReadyMarker(%s) error = %v, want ErrRouteReadyMarkerNewerVersion(且不是 ErrMalformed)", raw, err)
		}
	})

	t.Run("编码也拒绝不合规的值", func(t *testing.T) {
		bad := validReadyMarker()
		bad.Leader = "route_leader@主机"
		if _, err := EncodeRouteReadyMarker(bad); !errors.Is(err, ErrMalformed) {
			t.Fatalf("EncodeRouteReadyMarker(非 ASCII leader) error = %v, want ErrMalformed", err)
		}
		bad = validReadyMarker()
		bad.V = RouteReadyFormatVersion + 1
		if _, err := EncodeRouteReadyMarker(bad); !errors.Is(err, ErrMalformed) {
			t.Fatalf("EncodeRouteReadyMarker(v=%d) error = %v, want ErrMalformed(本二进制只写当前版本)", bad.V, err)
		}
	})
}

// TestRouteReadyScriptsShape 只钉住两个脚本的判据文本:「当前值等于读到的原值」才写 / 删,其余零写入。
// 执行语义(miniredis 上 CAS / CAD 的命中与落空)由 data_service routetool 的用例覆盖。
func TestRouteReadyScriptsShape(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         []string
		forbid       []string
	}{
		{
			name:   "compare-and-set",
			script: RouteReadyCompareAndSetScript,
			want:   []string{"redis.call('GET', KEYS[1]) == ARGV[1]", "redis.call('SET', KEYS[1], ARGV[2])", "return 1", "return 0"},
			forbid: []string{"'DEL'", " NX", " EX", " PX"},
		},
		{
			name:   "compare-and-delete",
			script: RouteReadyCompareAndDeleteScript,
			want:   []string{"redis.call('GET', KEYS[1]) == ARGV[1]", "redis.call('DEL', KEYS[1])", "return 1", "return 0"},
			forbid: []string{"'SET'", "ARGV[2]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range tc.want {
				if !strings.Contains(tc.script, w) {
					t.Fatalf("脚本缺少 %q:\n%s", w, tc.script)
				}
			}
			for _, f := range tc.forbid {
				if strings.Contains(tc.script, f) {
					t.Fatalf("脚本不该出现 %q:\n%s", f, tc.script)
				}
			}
		})
	}
}

// marshalUnchecked 不经校验地把 m 编成 JSON(只给用例拼不合规的值)。
func marshalUnchecked(t *testing.T, m RouteReadyMarker) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
