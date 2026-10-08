package main

// docs/design/player-storage-placement.md 第二段(合服 pin / copy、能力标记、pin-placement、storage-audit、
// 合服后验证的落点判据)的纯单测,不连库。与真 Redis / MySQL 的交互(CAS 脚本、端到端)在
// placement_integration_test.go(build tag merge_integration)。搬库状态机的单测在 relocate_test.go。
//
// 本 module 没有 miniredis / sqlmock(见 gap_fixes_test.go 头注释),Redis / MySQL 边界一律经纯函数或接缝测。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// placementReadOf 是测试夹具:按 MGET 的原始值造一条 placementRead(nil = 键不存在)。
func placementReadOf(t *testing.T, rec, home any) placementRead {
	t.Helper()
	rd, err := decodePlacementRead(rec, home)
	if err != nil {
		t.Fatalf("decodePlacementRead(%v, %v): %v", rec, home, err)
	}
	return rd
}

// ── 读 ───────────────────────────────────────────────────────────

func TestDecodePlacementRead(t *testing.T) {
	rd := placementReadOf(t, nil, nil)
	if rd.RecordPresent || rd.HomePresent || rd.RecordErr != nil || rd.HomeErr != nil {
		t.Fatalf("absent keys = %+v", rd)
	}
	rd = placementReadOf(t, "901:3", "902")
	if !rd.RecordPresent || rd.Record != (placementRecord{StorageID: 901, Version: 3}) || !rd.HomePresent || rd.Home != 902 {
		t.Fatalf("valid values = %+v", rd)
	}
	if rd := placementReadOf(t, "garbage", nil); !rd.RecordPresent || !errors.Is(rd.RecordErr, errPlacementMalformed) {
		t.Fatalf("a malformed record must stay present and carry the error: %+v", rd)
	}
	// home 的口径与 go/db 相同:空串 = 缺席;0 / 非数字 / 超出 zone 库编号范围 = 畸形。
	if rd := placementReadOf(t, nil, ""); rd.HomePresent || rd.HomeErr != nil {
		t.Fatalf("an empty home value is absent: %+v", rd)
	}
	for _, bad := range []string{"0", "abc", "1000000"} {
		if rd := placementReadOf(t, nil, bad); rd.HomePresent || !errors.Is(rd.HomeErr, errPlacementMalformed) {
			t.Errorf("home %q must be malformed: %+v", bad, rd)
		}
	}
	if _, err := decodePlacementRead(int64(1), nil); err == nil {
		t.Error("an unexpected MGET element type must be an error, not an absent key")
	}
}

// ── 合服扫描 ─────────────────────────────────────────────────────

func TestClassifyMergePlacements_PinSkipsTheDefaultCopyKeepsEveryRecord(t *testing.T) {
	ids := []uint64{1, 2, 3, 4}
	reads := []placementRead{
		placementReadOf(t, nil, "901"),
		placementReadOf(t, "901:1", "901"),     // 与 pin 要钉的值相同(早先钉过 / 上次钉的)
		placementReadOf(t, "901:5", "901"),     // 已有别的记录
		placementReadOf(t, "1000000:2", "901"), // 已在全局库
	}
	pin, err := classifyMergePlacements(ids, reads, playerRowsModePin, 901)
	if err != nil {
		t.Fatal(err)
	}
	if want := []manifestPlacement{{3, "901:5"}, {4, "1000000:2"}}; !reflect.DeepEqual(pin, want) {
		t.Errorf("pin existing = %v, want %v (the \"901:1\" record equals the pin and is not an exception)", pin, want)
	}
	cp, err := classifyMergePlacements(ids, reads, playerRowsModeCopy, 901)
	if err != nil {
		t.Fatal(err)
	}
	if want := []manifestPlacement{{2, "901:1"}, {3, "901:5"}, {4, "1000000:2"}}; !reflect.DeepEqual(cp, want) {
		t.Errorf("copy existing = %v, want %v (any record keeps the rows where they are)", cp, want)
	}
}

func TestClassifyMergePlacements_FrozenAndMalformedAreRefused(t *testing.T) {
	_, err := classifyMergePlacements([]uint64{1, 2}, []placementRead{placementReadOf(t, nil, "901"), placementReadOf(t, "901:1:frozen:reloc-a", "901")},
		playerRowsModePin, 901)
	if err == nil {
		t.Fatal("a frozen manifest player must refuse the merge (a relocation is moving him)")
	}
	for _, want := range []string{"FROZEN", "2(run_id=reloc-a)", "relocate-abort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	_, err = classifyMergePlacements([]uint64{7}, []placementRead{placementReadOf(t, "7:0", "901")}, playerRowsModeCopy, 901)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed record must refuse the merge in copy mode too: %v", err)
	}
	if _, err := classifyMergePlacements([]uint64{1, 2}, []placementRead{placementReadOf(t, nil, nil)}, playerRowsModePin, 901); err == nil {
		t.Fatal("a short answer must not be read as \"no records\"")
	}
}

func TestDiffPlacementExisting(t *testing.T) {
	recorded := []manifestPlacement{{3, "901:5"}, {4, "1000000:2"}}
	if n, _ := diffPlacementExisting(recorded, append([]manifestPlacement(nil), recorded...)); n != 0 {
		t.Fatalf("identical scans differ in %d players", n)
	}
	n, sample := diffPlacementExisting(recorded, []manifestPlacement{{3, "901:6"}, {9, "902:1"}})
	if n != 3 {
		t.Fatalf("changed=3, disappeared=4, appeared=9 → 3 differences, got %d (%v)", n, sample)
	}
	if !strings.Contains(strings.Join(sample, " "), `3(manifest="901:5" now="901:6")`) {
		t.Errorf("sample must show both values: %v", sample)
	}
}

func TestFrozenPlacementIDsAndPlacedIDs(t *testing.T) {
	ids := []uint64{1, 2, 3, 4}
	reads := []placementRead{
		placementReadOf(t, nil, "902"),
		placementReadOf(t, "901:1:frozen:reloc-b", "902"),
		placementReadOf(t, "901:1", "902"),
		placementReadOf(t, "bad", "902"),
	}
	if n, sample := frozenPlacementIDs(ids, reads); n != 1 || len(sample) != 1 || !strings.Contains(sample[0], "reloc-b") {
		t.Errorf("frozen = %d %v", n, sample)
	}
	// 畸形记录也算「有记录」:撤销据此不碰他的行,这个方向是安全的。
	if got := idsWithPlacementRecord(ids, reads); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Errorf("placed = %v, want [2 3 4]", got)
	}
}

// 撤销改回 home 之后,落点记录不指向 src 的人要求 src 的 go/db 按记录选库(§10.4 第 1 条的镜像)。
func TestIdsPlacedOffStorage(t *testing.T) {
	ids := []uint64{1, 2, 3, 4, 5}
	reads := []placementRead{
		placementReadOf(t, nil, "902"),         // 无记录:有效落点就是 home,不计入
		placementReadOf(t, "901:1", "902"),     // 记录 == src:改回 home 后 home 与记录一致,不计入
		placementReadOf(t, "902:1", "902"),     // 合服后被 pin-placement 钉在 dst:计入
		placementReadOf(t, "bad", "902"),       // 畸形:旧版 go/db 会无视它写进本 zone 库,计入
		placementReadOf(t, "1000000:3", "902"), // 被 relocate 搬到非 zone 落点库:计入
	}
	if got := idsPlacedOffStorage(ids, reads, 901); !reflect.DeepEqual(got, []uint64{3, 4, 5}) {
		t.Errorf("off src = %v, want [3 4 5]", got)
	}
	if got := idsPlacedOffStorage(ids[:2], reads[:2], 901); len(got) != 0 {
		t.Errorf("no record / record == src must not require the marker, got %v", got)
	}
}

func TestAnyMappedTo(t *testing.T) {
	for _, c := range []struct {
		name    string
		vals    []string
		present []bool
		want    bool
	}{
		{"merge finished, nobody restored yet", []string{"902", "902"}, []bool{true, true}, false},
		{"one player already back at src", []string{"902", "901"}, []bool{true, true}, true},
		{"missing key reads as empty, not as src", []string{"", "902"}, []bool{false, true}, false},
		{"third zone is not a half-undo", []string{"903"}, []bool{true}, false},
		{"empty manifest", nil, nil, false},
	} {
		if got := anyMappedTo(c.vals, c.present, 901); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// ── 能力标记 ─────────────────────────────────────────────────────

func TestParseCapabilityZoneSpec(t *testing.T) {
	got, err := parseCapabilityZoneSpec(" 103, 902,101,103 ,")
	if err != nil || !got.given || got.none || !reflect.DeepEqual(got.zones, []uint32{101, 103, 902}) {
		t.Fatalf("mixed list = %+v %v, want sorted and deduplicated", got, err)
	}
	for _, raw := range []string{"none", " NONE "} {
		got, err := parseCapabilityZoneSpec(raw)
		if err != nil || !got.given || !got.none || len(got.zones) != 0 {
			t.Fatalf("%q = %+v %v, want the none literal", raw, got, err)
		}
	}
	// 没给 / 空串:语法上不是错,由 requireCapabilityZones 按用途拒绝。
	for _, raw := range []string{"", "   "} {
		if got, err := parseCapabilityZoneSpec(raw); err != nil || got.given {
			t.Fatalf("%q = %+v %v, want not given", raw, got, err)
		}
	}
	// 旧的 src / dst 记号已删除:报错并说明原因(T-0 它们已下线)。
	for _, raw := range []string{"src,dst", "101,dst"} {
		if _, err := parseCapabilityZoneSpec(raw); err == nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("%q must be refused with the reason: %v", raw, err)
		}
	}
	for _, bad := range []string{" , ", "abc", "0", "1000000", "-1", "none,101", "101,none"} {
		if _, err := parseCapabilityZoneSpec(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// TestRequireCapabilityZones 钉住「没有缺省值」与 none 的适用面:合服 / 撤销缺省即拒、接受 none;
// 搬库与 capability-check 缺省即拒、也拒 none。
func TestRequireCapabilityZones(t *testing.T) {
	notGiven := capabilityZoneSpec{}
	none, _ := parseCapabilityZoneSpec("none")
	list, _ := parseCapabilityZoneSpec("101,103")
	for _, use := range []capabilityUse{capabilityForMerge, capabilityForUnmerge, capabilityForRelocate, capabilityForCheck} {
		if err := requireCapabilityZones(notGiven, use); err == nil || !strings.Contains(err.Error(), "-db-capability-zones") {
			t.Errorf("use %d: a missing -db-capability-zones must be refused with the flag named: %v", use, err)
		}
		if err := requireCapabilityZones(list, use); err != nil {
			t.Errorf("use %d: an explicit list must pass: %v", use, err)
		}
	}
	// 合服 / 撤销的缺省提示要说清 T-0 的口径:src / dst 已下线,只列在跑的 zone,dst 在 zone-up 后用 capability-check。
	err := requireCapabilityZones(notGiven, capabilityForMerge)
	for _, want := range []string{"no default", "'none'", "T-0", "-mode capability-check"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("merge refusal must mention %q: %v", want, err)
		}
	}
	for _, use := range []capabilityUse{capabilityForMerge, capabilityForUnmerge} {
		if err := requireCapabilityZones(none, use); err != nil {
			t.Errorf("use %d must accept none: %v", use, err)
		}
	}
	for _, use := range []capabilityUse{capabilityForRelocate, capabilityForCheck} {
		if err := requireCapabilityZones(none, use); err == nil || !strings.Contains(err.Error(), "none") {
			t.Errorf("use %d must refuse none: %v", use, err)
		}
	}
	if err := requireCapabilityZones(notGiven, capabilityForRelocate); err == nil || !strings.Contains(err.Error(), "every zone") {
		t.Errorf("relocate refusal must ask for every running zone: %v", err)
	}
}

// TestMergeAndUnmergeCapabilityFlag:pin 合服 / pin 撤销在入口就要求 -db-capability-zones;copy 合服在入口不要求
// (S 段发现有记录者时才要),copy 撤销不要求。
func TestMergeAndUnmergeCapabilityFlag(t *testing.T) {
	none, _ := parseCapabilityZoneSpec("none")
	if err := checkMergeCapabilityFlag(options{playerRowsMode: playerRowsModePin}); err == nil {
		t.Error("a pin merge without -db-capability-zones must be refused")
	}
	if err := checkMergeCapabilityFlag(options{playerRowsMode: playerRowsModePin, capSpec: none}); err != nil {
		t.Errorf("a pin merge must accept none: %v", err)
	}
	if err := checkMergeCapabilityFlag(options{playerRowsMode: playerRowsModeCopy}); err != nil {
		t.Errorf("a copy merge must not require the flag up front: %v", err)
	}
	if err := checkUnmergeCapabilityFlag(playerRowsModePin, capabilityZoneSpec{}); err == nil {
		t.Error("unmerging a pin merge without -db-capability-zones must be refused")
	}
	if err := checkUnmergeCapabilityFlag(playerRowsModePin, none); err != nil {
		t.Errorf("unmerging a pin merge must accept none: %v", err)
	}
	if err := checkUnmergeCapabilityFlag(playerRowsModeCopy, capabilityZoneSpec{}); err != nil {
		t.Errorf("a copy unmerge must not require the flag: %v", err)
	}
}

func TestCapabilityDownZonesHint(t *testing.T) {
	if h := capabilityDownZonesHint([]uint32{101, 103}, 901, 902); h != "" {
		t.Errorf("no src/dst listed, no hint: %q", h)
	}
	h := capabilityDownZonesHint([]uint32{101, 902}, 901, 902)
	for _, want := range []string{"[902]", "T-0", "capability-check -db-capability-zones 902"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint must mention %q: %q", want, h)
		}
	}
}

// 提示为空(T-0 的常见写法:只列其他在跑的 zone)时原样返回,调用方接「. Upgrade ...」不能出现「database.. Upgrade」;
// 有提示时保留错误链。
func TestWithCapabilityDownZonesHint(t *testing.T) {
	// 与 checkPlacementCapabilities 的真实文案同样以「database」收尾、不带句号(替身里不能自带「..」,否则断言无意义)。
	base := errors.New("go/db capability placement-routing-v1 is not confirmed and would write players into the wrong database")
	// merge_run.go 的 preflight C 就是这样接的。
	callerText := func(err error) string {
		return "preflight C (-player-rows-mode pin): " + err.Error() + ". Upgrade and restart go/db"
	}

	got := withCapabilityDownZonesHint(base, []uint32{101, 103}, 901, 902)
	if got != base {
		t.Fatalf("no src/dst listed: err must be returned unchanged, got %q", got)
	}
	if s := callerText(got); strings.Contains(s, "..") {
		t.Errorf("no src/dst listed: caller text must not contain a double period: %q", s)
	}

	got = withCapabilityDownZonesHint(base, []uint32{101, 902}, 901, 902)
	if !errors.Is(got, base) {
		t.Errorf("the hint must keep the error chain (%%w): %v", got)
	}
	if want := base.Error() + ". Zones [902]"; !strings.HasPrefix(got.Error(), want) {
		t.Errorf("hint must follow the error after a single period: got %q, want prefix %q", got, want)
	}
	if s := callerText(got); strings.Contains(s, "..") {
		t.Errorf("with hint: caller text must not contain a double period: %q", s)
	}
}

func TestCapabilityShortfall(t *testing.T) {
	missing, unexpected := capabilityShortfall([]uint32{101, 102, 103}, []any{capabilityRoutingV1, nil, "placement-routing-v0"})
	if !reflect.DeepEqual(missing, []uint32{102}) || len(unexpected) != 1 || !strings.Contains(unexpected[0], "zone 103") {
		t.Fatalf("missing=%v unexpected=%v", missing, unexpected)
	}
	if missing, unexpected := capabilityShortfall([]uint32{101}, []any{capabilityRoutingV1}); missing != nil || unexpected != nil {
		t.Fatalf("a confirmed zone must pass: %v %v", missing, unexpected)
	}
	// 回复比 zone 少:少的那几个按缺标记算,不能当成通过。
	if missing, _ := capabilityShortfall([]uint32{101, 102}, []any{capabilityRoutingV1}); !reflect.DeepEqual(missing, []uint32{102}) {
		t.Fatalf("a short reply must count as missing: %v", missing)
	}
	// 搬库对列表之外的 home zone 逐个确认:只有值正是 placement-routing-v1 的才算。
	got := capabilityConfirmed([]uint32{101, 102, 103, 104}, []any{capabilityRoutingV1, nil, "placement-routing-v0"})
	if !reflect.DeepEqual(got, map[uint32]bool{101: true}) {
		t.Fatalf("confirmed = %v, want only 101", got)
	}
}

// ── CAS ──────────────────────────────────────────────────────────

func TestDecodePlacementCASReply(t *testing.T) {
	res, err := decodePlacementCASReply([]any{int64(1), "", "901"})
	if err != nil || res != (placementCASResult{Outcome: placementCASWritten, CurrentHome: "901"}) {
		t.Fatalf("written reply = %+v %v", res, err)
	}
	res, err = decodePlacementCASReply([]any{int64(3), "901:1", "901"})
	if err != nil || res.Outcome != placementCASFenced || res.CurrentRecord != "901:1" {
		t.Fatalf("fenced reply = %+v %v", res, err)
	}
	for _, bad := range []any{nil, []any{int64(1), ""}, []any{"1", "", ""}, []any{int64(9), "", ""}, []any{int64(1), nil, ""}} {
		if _, err := decodePlacementCASReply(bad); err == nil {
			t.Errorf("reply %v must be rejected", bad)
		}
	}
	// 返回码是与 Lua 共享的契约,字面量不能漂移。
	if placementCASRecordChanged != 0 || placementCASWritten != 1 || placementCASHomeChanged != 2 || placementCASFenced != 3 {
		t.Fatal("placementCASOutcome values drifted from placementCASScript's return codes")
	}
}

func TestPlacementCAS_ValidateAndKeys(t *testing.T) {
	if err := (placementCAS{PlayerID: 1, ExpectAbsent: true, NewValue: "901:0"}).validate(); err == nil {
		t.Error("a malformed new value must never be written")
	}
	if err := (placementCAS{PlayerID: 1, NewValue: "901:1"}).validate(); err == nil {
		t.Error("a CAS on an existing record needs its expected value")
	}
	op := placementCAS{PlayerID: 42, ExpectRecord: "901:3", ExpectHome: "901", FenceZone: 901, NewValue: "901:3:frozen:reloc-a"}
	if err := op.validate(); err != nil {
		t.Fatal(err)
	}
	keys, args := op.keysAndArgs()
	if !reflect.DeepEqual(keys, []string{"player:placement:42", "player:zone:42", "merge:in_progress:901"}) {
		t.Errorf("keys = %v", keys)
	}
	if !reflect.DeepEqual(args, []any{"0", "901:3", "901", "901:3:frozen:reloc-a"}) {
		t.Errorf("args = %v", args)
	}
	// 持围栏的合服自己写:不传围栏键(否则会被自己的围栏挡住)。
	keys, args = placementCAS{PlayerID: 42, ExpectAbsent: true, ExpectHome: "901", NewValue: "901:1"}.keysAndArgs()
	if len(keys) != 2 || args[0] != "1" {
		t.Errorf("no fence zone → 2 keys and the absent flag: %v %v", keys, args)
	}
}

// ── 钉落点 ───────────────────────────────────────────────────────

func TestDecidePin(t *testing.T) {
	for _, c := range []struct {
		record, home string
		want         pinVerdict
	}{
		{"", "901", pinVerdictPin},
		{"901:1", "901", pinVerdictAlreadyPinned},
		{"901:1", "902", pinVerdictAlreadyPinned}, // 记录已是要钉的值:home 走没走都不再动它
		{"901:5", "901", pinVerdictKept},
		{"1000000:2", "901", pinVerdictKept},
		{"901:1:frozen:reloc-a", "901", pinVerdictKept},
		{"", "902", pinVerdictHomeMoved},
		{"", "", pinVerdictHomeMoved},
	} {
		if got := decidePin(c.record, c.home, 901); got != c.want {
			t.Errorf("decidePin(%q, %q) = %d, want %d", c.record, c.home, got, c.want)
		}
	}
}

func TestPinReport(t *testing.T) {
	var rep pinReport
	for i := 0; i < unmappedSampleSize+3; i++ {
		rep.anomaly("a%d", i)
	}
	if rep.AnomalyCount != unmappedSampleSize+3 || len(rep.Anomalies) != unmappedSampleSize {
		t.Fatalf("anomalies must be counted in full and sampled: %d / %d", rep.AnomalyCount, len(rep.Anomalies))
	}
	rep = pinReport{Candidates: 5, Pinned: 2, AlreadyPinned: 1, Kept: 1, HomeMoved: 1}
	if s := rep.String(); s != "candidates=5 pinned=2 already_pinned=1 kept_existing=1 home_moved=1 anomalies=0" {
		t.Errorf("String = %q", s)
	}
}

func TestCountPinVerdict(t *testing.T) {
	var rep pinReport
	for _, v := range []pinVerdict{pinVerdictPin, pinVerdictAlreadyPinned, pinVerdictKept, pinVerdictHomeMoved, pinVerdictKept} {
		countPinVerdict(&rep, v)
	}
	if rep.Pinned != 1 || rep.AlreadyPinned != 1 || rep.Kept != 2 || rep.HomeMoved != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

// ── 玩家行模式 / 清单 ─────────────────────────────────────────────

func TestParsePlayerRowsModeAndRowsStep(t *testing.T) {
	for _, ok := range []string{"pin", "copy"} {
		if got, err := parsePlayerRowsMode(ok); err != nil || got != ok {
			t.Errorf("%q = %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "Pin", "move"} {
		if _, err := parsePlayerRowsMode(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if rowsStepFor(playerRowsModePin) != stepPinPlacement || rowsStepFor(playerRowsModeCopy) != stepPlayerRows {
		t.Fatal("rows step mapping drifted")
	}
	// 步骤名进清单,字面量不能改。
	if stepPinPlacement != "pin_placement" || playerRowsModePin != "pin" || playerRowsModeCopy != "copy" {
		t.Fatal("manifest literals drifted")
	}
}

func TestValidateManifestRowsMode(t *testing.T) {
	if err := validateManifestRowsMode(nil, playerRowsModePin); err != nil {
		t.Fatalf("no manifest = fresh run: %v", err)
	}
	legacy := newMergeManifest("r", 901, 902, "op", time.Now()) // 没有 player_rows_mode = copy
	if legacy.rowsMode() != playerRowsModeCopy {
		t.Fatalf("a manifest without the field must read as copy, got %q", legacy.rowsMode())
	}
	if err := validateManifestRowsMode(legacy, playerRowsModeCopy); err != nil {
		t.Fatalf("resuming a legacy manifest in copy mode must pass: %v", err)
	}
	err := validateManifestRowsMode(legacy, playerRowsModePin)
	if err == nil || !strings.Contains(err.Error(), "-player-rows-mode copy") || !strings.Contains(err.Error(), "written before pin mode existed") {
		t.Fatalf("resuming a legacy manifest with the pin default must be refused with the fix: %v", err)
	}
	pin := newMergeManifest("r", 901, 902, "op", time.Now())
	pin.PlayerRowsMode = playerRowsModePin
	if err := validateManifestRowsMode(pin, playerRowsModeCopy); err == nil || !strings.Contains(err.Error(), "-player-rows-mode pin") {
		t.Fatalf("a pin manifest resumed in copy mode must be refused: %v", err)
	}
	pin.PlayerRowsMode = "move"
	if err := validateManifestRowsMode(pin, playerRowsModePin); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("an unknown recorded mode must be refused: %v", err)
	}
}

func TestManifestPlacementFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	m.PlayerRowsMode = playerRowsModePin
	m.PlacementScanned = true
	m.PlacementExisting = []manifestPlacement{{PlayerID: 3, Value: "901:5"}}
	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	back, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.rowsMode() != playerRowsModePin || !back.PlacementScanned || !reflect.DeepEqual(back.PlacementExisting, m.PlacementExisting) {
		t.Fatalf("placement fields lost: %+v", back)
	}
	if got := back.placementExistingMap(); got[3] != "901:5" || len(got) != 1 {
		t.Errorf("placementExistingMap = %v", got)
	}
	if got := placementIDs(back.PlacementExisting); !reflect.DeepEqual(got, []uint64{3}) {
		t.Errorf("placementIDs = %v", got)
	}
	// 合服清单不写 kind(旧版工具读得懂的形状不变)。
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"kind"`) {
		t.Errorf("a merge manifest must not carry kind: %s", raw)
	}
}

func TestLoadManifest_RefusesARelocateManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reloc.json")
	rm := newRelocateManifest("reloc-1", 901, 902, 1, []string{"player_database"}, []uint64{1}, "op", time.Now())
	if err := saveRelocateManifest(path, rm); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil || !strings.Contains(err.Error(), "not a merge manifest") {
		t.Fatalf("a relocate manifest must never be resumed / unmerged / verified as a merge: %v", err)
	}
	// 旧版工具不认 kind、只认 version:搬库清单没有 version 字段,读出来是 0,同样拒绝。
	var probe map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, has := probe["version"]; has {
		t.Error("a relocate manifest must not carry the merge manifest's version field")
	}
}

func TestCheckPinModeFlags(t *testing.T) {
	pin := options{playerRowsMode: playerRowsModePin}
	if err := checkPinModeFlags(pin); err != nil {
		t.Fatalf("plain pin: %v", err)
	}
	withRows := pin
	withRows.skipRows = true
	if err := checkPinModeFlags(withRows); err == nil || !strings.Contains(err.Error(), "copy") {
		t.Errorf("pin + -skip-player-rows must be refused: %v", err)
	}
	withBlobs := pin
	withBlobs.migrateBlobs = true
	if err := checkPinModeFlags(withBlobs); err == nil || !strings.Contains(err.Error(), "-player-rows-mode copy") {
		t.Errorf("pin + -migrate-player-blobs must be refused with the alternative: %v", err)
	}
	withBlobs.skipBlobs = true // 应急开关:blob 步骤本来就不跑
	if err := checkPinModeFlags(withBlobs); err != nil {
		t.Errorf("-skip-player-blob-migration disables the blob step, nothing to refuse: %v", err)
	}
	cp := options{playerRowsMode: playerRowsModeCopy, skipRows: true, migrateBlobs: true}
	if err := checkPinModeFlags(cp); err != nil {
		t.Errorf("copy mode keeps both flags: %v", err)
	}
}

// ── 合服后验证:主数据行在哪 ───────────────────────────────────────

func TestExpectedMainRowStore(t *testing.T) {
	for _, c := range []struct {
		name  string
		rd    placementRead
		store uint32
		ok    bool
	}{
		{"pinned to src", placementReadOf(t, "901:1", "902"), 901, true},
		{"kept elsewhere", placementReadOf(t, "1000000:4", "902"), 1000000, true},
		{"frozen: data still in its store", placementReadOf(t, "901:2:frozen:reloc-a", "902"), 901, true},
		{"no record: copied into the target zone", placementReadOf(t, nil, "902"), 902, true},
		{"no record, mapping wrong: still the target zone", placementReadOf(t, nil, "903"), 902, true},
		{"malformed record", placementReadOf(t, "x", "902"), 0, false},
	} {
		store, ok := expectedMainRowStore(c.rd, 902)
		if store != c.store || ok != c.ok {
			t.Errorf("%s: (%d,%v), want (%d,%v)", c.name, store, ok, c.store, c.ok)
		}
	}
}

func TestVerifyManifestRows_PinModeGatesOnThePinStep(t *testing.T) {
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	m.PlayerRowsMode = playerRowsModePin
	m.PlayerIDs = []uint64{1}
	// pin 模式不标 player_rows:以 pin_placement 为准,没做完报 NOT VERIFIED。
	m.markStep(stepPlayerRows, "not used by pin")
	r := verifyManifestRows(context.Background(), auditConfig{src: 901, dst: 902, manifest: m})
	if r.Severity != "warn" || !strings.Contains(r.Notes, stepPinPlacement) {
		t.Fatalf("a pin manifest without %s must be NOT VERIFIED, got %+v", stepPinPlacement, r)
	}
	m.markStep(stepPinPlacement, "pinned=1")
	// 做完了、却没有句柄:落点记录决定行在哪,查不成 = INFRA。
	r = verifyManifestRows(context.Background(), auditConfig{src: 901, dst: 902, manifest: m})
	if !isInfraAudit(r) {
		t.Fatalf("no handles must be INFRA, got %+v", r)
	}
}

// ── storage-audit ────────────────────────────────────────────────

func TestClassifyStorageRow(t *testing.T) {
	for _, c := range []struct {
		name string
		rd   placementRead
		want storageRowVerdict
	}{
		{"record points here", placementReadOf(t, "901:2", "902"), storageRowLive},
		{"frozen record points here", placementReadOf(t, "901:2:frozen:reloc-a", "902"), storageRowLive},
		{"record points elsewhere (record wins over home)", placementReadOf(t, "1000000:3", "901"), storageRowCold},
		{"no record, home here", placementReadOf(t, nil, "901"), storageRowLive},
		{"no record, home elsewhere", placementReadOf(t, nil, "902"), storageRowCold},
		{"neither", placementReadOf(t, nil, nil), storageRowUnmapped},
		{"malformed record: go/db fails closed, home is not consulted", placementReadOf(t, "bad", "901"), storageRowUnreadable},
		{"malformed home", placementReadOf(t, nil, "0"), storageRowUnreadable},
		{"valid record but malformed home: go/db fails the whole lookup", placementReadOf(t, "901:1", "abc"), storageRowUnreadable},
	} {
		if got := classifyStorageRow(c.rd, 901); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestStorageAuditReport(t *testing.T) {
	clean := storageAuditReport{Storage: 901, Rows: 3, ColdCopies: 3}
	if b := clean.RetireBlockers(); len(b) != 0 {
		t.Fatalf("only cold copies left must be safe to retire: %v", b)
	}
	r := storageAuditReport{Storage: 901, ByRecord: 1, ByHome: 2, UnmappedRows: 1, UnmappedSample: []uint64{7},
		UnreadableRows: 1, MalformedRecords: 1}
	if r.Effective() != 3 {
		t.Fatalf("effective = %d", r.Effective())
	}
	joined := strings.Join(r.RetireBlockers(), " | ")
	for _, want := range []string{"3 players are still placed", "neither a placement record nor a home_zone", "malformed", "may point at storage 901"} {
		if !strings.Contains(joined, want) {
			t.Errorf("blockers %q do not mention %q", joined, want)
		}
	}
}

func TestStoragePlayersAllIsSortedAndMerged(t *testing.T) {
	sp := storagePlayers{ByRecord: []uint64{5, 1}, ByHome: []uint64{3}}
	if got := sp.all(); !reflect.DeepEqual(got, []uint64{1, 3, 5}) {
		t.Fatalf("all = %v", got)
	}
}
