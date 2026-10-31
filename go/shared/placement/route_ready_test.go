package placement

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	testRunID  = "0123456789abcdef0123456789abcdef01234567"
	testReplID = "fedcba9876543210fedcba9876543210fedcba98"
	testGen    = "00112233445566778899aabbccddeeff"
)

func testMarker(t *testing.T) (RouteReadyMarker, string) {
	t.Helper()
	m := RouteReadyMarker{
		Generation:   testGen,
		RedisRunID:   testRunID,
		MasterReplID: testReplID,
		SetAtMs:      1700000000001,
		WatermarkMs:  1700000000000,
		Leader:       "route_leader@host:0a1b2c3d",
	}
	raw, err := EncodeRouteReadyMarker(m)
	if err != nil {
		t.Fatalf("encode marker: %v", err)
	}
	m.V = RouteReadyFormatVersion
	return m, raw
}

// TestRouteReadyReadScriptShape 钉住读脚本的判据文本(执行语义只能在真 Redis 上测:miniredis 不支持 INFO server / replication)。
func TestRouteReadyReadScriptShape(t *testing.T) {
	for _, want := range []string{
		"redis.pcall('GET', KEYS[1])",
		"if type(m) ~= 'string' then m = false end",
		"redis.call('INFO', 'server')",
		"redis.call('INFO', 'replication')",
		"'run_id:(%x+)'",
		"'role:(%a+)'",
		"'master_replid:(%x+)'",
		"redis.call('MGET', unpack(KEYS, 2))",
	} {
		if !strings.Contains(RouteReadyReadScript, want) {
			t.Errorf("RouteReadyReadScript missing %q", want)
		}
	}
	for _, banned := range []string{"'SET'", "'DEL'", "'EXPIRE'"} {
		if strings.Contains(RouteReadyReadScript, banned) {
			t.Errorf("RouteReadyReadScript must be read-only, found %s", banned)
		}
	}
	keys := RouteReadyReadKeys(Key(7), HomeZoneKey(7))
	if len(keys) != 3 || keys[0] != RouteReadyKey || keys[1] != Key(7) || keys[2] != HomeZoneKey(7) {
		t.Fatalf("RouteReadyReadKeys = %v", keys)
	}
}

func TestParseRouteReadyRead(t *testing.T) {
	_, raw := testMarker(t)
	ok := []any{raw, testRunID, "master", testReplID, "102:1", nil}
	r, err := ParseRouteReadyRead(ok, 2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !r.MarkerPresent || r.MarkerRaw != raw || r.Identity.RunID != testRunID || r.Identity.Role != "master" ||
		r.Identity.MasterReplID != testReplID || r.Values[0] != "102:1" || r.Values[1] != nil {
		t.Fatalf("parsed %+v", r)
	}
	absent, err := ParseRouteReadyRead([]any{nil, "", "", ""}, 0)
	if err != nil || absent.MarkerPresent || len(absent.Values) != 0 {
		t.Fatalf("absent marker: %+v %v", absent, err)
	}
	bad := map[string]struct {
		raw any
		n   int
	}{
		"not array":          {raw: "x", n: 0},
		"too short":          {raw: []any{nil, "", ""}, n: 0},
		"too long":           {raw: []any{nil, "", "", "", "v"}, n: 0},
		"marker int":         {raw: []any{int64(1), "", "", ""}, n: 0},
		"identity nil":       {raw: []any{nil, nil, "", ""}, n: 0},
		"value int":          {raw: []any{nil, "", "", "", int64(3)}, n: 1},
		"negative key count": {raw: []any{nil, "", "", ""}, n: -1},
	}
	for name, c := range bad {
		if _, err := ParseRouteReadyRead(c.raw, c.n); !errors.Is(err, ErrUnexpectedRouteReadyReadReply) {
			t.Errorf("%s: err = %v, want ErrUnexpectedRouteReadyReadReply", name, err)
		}
	}
}

func TestVerifyRouteReady(t *testing.T) {
	_, raw := testMarker(t)
	id := RedisInstanceIdentity{RunID: testRunID, Role: "master", MasterReplID: testReplID}
	newer, _ := json.Marshal(map[string]any{"v": RouteReadyFormatVersion + 1, "anything": true})
	otherRun := strings.Repeat("a", 40)
	cases := []struct {
		name string
		r    RouteReadyRead
		want RouteReadyVerdict
	}{
		{"trusted", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: id}, RouteReadyTrusted},
		{"absent", RouteReadyRead{Identity: id}, RouteReadyAbsent},
		{"malformed", RouteReadyRead{MarkerPresent: true, MarkerRaw: "{", Identity: id}, RouteReadyMalformed},
		{"empty string marker", RouteReadyRead{MarkerPresent: true, MarkerRaw: "", Identity: id}, RouteReadyMalformed},
		{"newer version", RouteReadyRead{MarkerPresent: true, MarkerRaw: string(newer), Identity: id}, RouteReadyNewerVersion},
		{"no run_id", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: RedisInstanceIdentity{Role: "master", MasterReplID: testReplID}}, RouteReadyIdentityUnavailable},
		{"no role", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: RedisInstanceIdentity{RunID: testRunID, MasterReplID: testReplID}}, RouteReadyIdentityUnavailable},
		{"replica", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: RedisInstanceIdentity{RunID: testRunID, Role: "slave", MasterReplID: testReplID}}, RouteReadyNotMaster},
		{"restarted (run_id changed)", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: RedisInstanceIdentity{RunID: otherRun, Role: "master", MasterReplID: testReplID}}, RouteReadyIdentityMismatch},
		{"failover (replid changed)", RouteReadyRead{MarkerPresent: true, MarkerRaw: raw, Identity: RedisInstanceIdentity{RunID: testRunID, Role: "master", MasterReplID: otherRun}}, RouteReadyIdentityMismatch},
	}
	seen := map[RouteReadyVerdict]bool{}
	for _, c := range cases {
		got, _ := VerifyRouteReady(c.r)
		if got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
		seen[got] = true
	}
	for v := RouteReadyVerdict(0); v < routeReadyVerdictCount; v++ {
		if !seen[v] {
			t.Errorf("verdict %v not covered", v)
		}
		if v.String() == "unknown" || v.String() == "" {
			t.Errorf("verdict %d has no name", v)
		}
	}
	if got := RouteReadyVerdict(routeReadyVerdictCount).String(); got != "unknown" {
		t.Errorf("out-of-range verdict name = %q", got)
	}
	if names := RouteReadyVerdictNames(); len(names) != int(routeReadyVerdictCount) || names[0] != "trusted" {
		t.Errorf("RouteReadyVerdictNames = %v", names)
	}
}

func TestParseRedisInstanceIdentity(t *testing.T) {
	server := "# Server\r\nredis_version:7.2.4\r\nrun_id:" + testRunID + "\r\ntcp_port:6379\r\n"
	repl := "# Replication\r\nrole:master\r\nconnected_slaves:0\r\nmaster_replid:" + testReplID +
		"\r\nmaster_replid2:" + strings.Repeat("0", 40) + "\r\n"
	id, err := ParseRedisInstanceIdentity(server, repl)
	if err != nil || id.RunID != testRunID || id.Role != "master" || id.MasterReplID != testReplID {
		t.Fatalf("CRLF: %+v %v", id, err)
	}
	lf := strings.ReplaceAll(repl, "\r\n", "\n")
	lf = strings.Replace(lf, "role:master", "role:slave", 1)
	id, err = ParseRedisInstanceIdentity(strings.ReplaceAll(server, "\r\n", "\n"), lf)
	if err != nil || id.Role != "slave" {
		t.Fatalf("LF replica: %+v %v", id, err)
	}
	// master_replid2 不能被当成 master_replid。
	onlyReplid2 := "role:master\r\nmaster_replid2:" + testReplID + "\r\n"
	if _, err := ParseRedisInstanceIdentity(server, onlyReplid2); !errors.Is(err, ErrMalformed) {
		t.Fatalf("replid2 only: err = %v", err)
	}
	for name, c := range map[string][2]string{
		"no run_id":    {"redis_version:7.2.4\r\n", repl},
		"short run_id": {"run_id:abc\r\n", repl},
		"no role":      {server, "master_replid:" + testReplID + "\r\n"},
		"upper hex":    {"run_id:" + strings.ToUpper(testRunID) + "\r\n", repl},
	} {
		if _, err := ParseRedisInstanceIdentity(c[0], c[1]); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestRouteWatermarkRoundTripAndRejects(t *testing.T) {
	raw, err := EncodeRouteWatermark(RouteWatermark{Generation: testGen, WatermarkMs: 42})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if want := `{"v":1,"generation":"` + testGen + `","watermark_ms":42}`; raw != want {
		t.Fatalf("encoded %s, want %s", raw, want)
	}
	w, err := DecodeRouteWatermark(raw)
	if err != nil || w.V != 1 || w.Generation != testGen || w.WatermarkMs != 42 {
		t.Fatalf("decode: %+v %v", w, err)
	}
	if got := w.FloorFor(testGen); got != 42 {
		t.Errorf("FloorFor(same) = %d", got)
	}
	if got := w.FloorFor("ff"); got != 0 {
		t.Errorf("FloorFor(other) = %d", got)
	}
	if _, err := DecodeRouteWatermark(`{"v":2,"watermark_ms":"later"}`); !errors.Is(err, ErrRouteWatermarkNewerVersion) {
		t.Errorf("newer: err = %v", err)
	}
	for name, raw := range map[string]string{
		"not json":  "x",
		"v0":        `{"v":0,"generation":"` + testGen + `","watermark_ms":1}`,
		"no gen":    `{"v":1,"watermark_ms":1}`,
		"upper gen": `{"v":1,"generation":"AA","watermark_ms":1}`,
		"wm zero":   `{"v":1,"generation":"aa","watermark_ms":0}`,
		"wm string": `{"v":1,"generation":"aa","watermark_ms":"1"}`,
	} {
		if _, err := DecodeRouteWatermark(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	if _, err := EncodeRouteWatermark(RouteWatermark{V: 2, Generation: testGen, WatermarkMs: 1}); !errors.Is(err, ErrMalformed) {
		t.Errorf("encode v2: err = %v", err)
	}
}

func TestRouteReadyArmScriptShapeAndArgs(t *testing.T) {
	for _, want := range []string{
		"redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0",
		"redis.call('EXISTS', KEYS[2]) == 1 then return 2",
		"~= ARGV[3]",
		"~= ARGV[4]",
		"~= 'master'",
		"redis.call('SET', KEYS[2], ARGV[2])",
	} {
		if !strings.Contains(RouteReadyArmScript, want) {
			t.Errorf("RouteReadyArmScript missing %q", want)
		}
	}
	if strings.Contains(RouteReadyArmScript, "'PX'") || strings.Contains(RouteReadyArmScript, "'EX'") {
		t.Errorf("ready marker must not carry a TTL")
	}
	m, raw := testMarker(t)
	token := strings.Repeat("ab", 16)
	keys, args, err := RouteReadyArmKeysAndArgs(token, m)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if len(keys) != 2 || keys[0] != RouteWarmKey || keys[1] != RouteReadyKey {
		t.Fatalf("keys = %v", keys)
	}
	if len(args) != 4 || args[0] != token || args[1] != raw || args[2] != testRunID || args[3] != testReplID {
		t.Fatalf("args = %v", args)
	}
	if _, _, err := RouteReadyArmKeysAndArgs("short", m); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad token: err = %v", err)
	}
	bad := m
	bad.RedisRunID = "x"
	if _, _, err := RouteReadyArmKeysAndArgs(token, bad); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad marker: err = %v", err)
	}
	for raw, want := range map[int64]RouteReadyArmOutcome{0: RouteReadyArmTokenMismatch, 1: RouteReadyArmArmed, 2: RouteReadyArmAlreadyPresent, 3: RouteReadyArmIdentityChanged} {
		got, err := ClassifyRouteReadyArmReply(raw)
		if err != nil || got != want || got.String() == "unknown" {
			t.Errorf("classify %d = %v %v", raw, got, err)
		}
	}
	for _, raw := range []any{int64(4), int64(-1), "1", nil} {
		if _, err := ClassifyRouteReadyArmReply(raw); !errors.Is(err, ErrUnexpectedRouteReadyReadReply) {
			t.Errorf("classify %v: err = %v", raw, err)
		}
	}
	if !ValidRouteWarmToken(token) || ValidRouteWarmToken(strings.ToUpper(token)) || ValidRouteWarmToken(token+"0") {
		t.Errorf("ValidRouteWarmToken boundaries")
	}
}

// TestMergedIntoValueMatchesMergeZoneEncoding:与 tools/merge_zone/merged_into.go 的 mergedIntoValue 用 json.Marshal 写出的值逐字节相同。
func TestMergedIntoValueMatchesMergeZoneEncoding(t *testing.T) {
	mergeZoneShape := struct {
		Dst   uint32 `json:"dst"`
		RunID string `json:"run_id"`
	}{Dst: 2, RunID: "1-2-20261018T000000Z-0a1b2c3d"}
	want, _ := json.Marshal(mergeZoneShape)
	got, err := EncodeMergedIntoValue(2, mergeZoneShape.RunID)
	if err != nil || got != string(want) {
		t.Fatalf("EncodeMergedIntoValue = %q %v, want %q", got, err, want)
	}
	dst, run, err := DecodeMergedIntoValue(1, got)
	if err != nil || dst != 2 || run != mergeZoneShape.RunID {
		t.Fatalf("decode = %d %q %v", dst, run, err)
	}
	if MergedIntoKey(7) != "merge:merged_into:7" || !strings.HasPrefix(MergedIntoKey(7), MergedIntoKeyPrefix) {
		t.Fatalf("MergedIntoKey(7) = %q", MergedIntoKey(7))
	}
	if _, err := EncodeMergedIntoValue(0, "r"); !errors.Is(err, ErrMalformed) {
		t.Errorf("dst 0: %v", err)
	}
	if _, err := EncodeMergedIntoValue(2, "bad run"); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad run id: %v", err)
	}
	for name, raw := range map[string]string{
		"self":   `{"dst":1,"run_id":"r"}`,
		"zero":   `{"dst":0,"run_id":"r"}`,
		"no run": `{"dst":2}`,
		"junk":   `nope`,
	} {
		if _, _, err := DecodeMergedIntoValue(1, raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
