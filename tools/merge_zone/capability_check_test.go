package main

// -mode capability-check 的判定单测(capability_check.go)。本 module 没有 miniredis(见 gap_fixes_test.go 头注释),
// Redis 边界经 capabilityGetter 接缝用内存替身;退出码经纯函数 evaluateCapabilityCheck 测,不起进程。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCapabilityStore 是 capabilityGetter 的内存替身:vals 里有的键存在,failing 里的键读失败。
type fakeCapabilityStore struct {
	vals    map[string]string
	failing map[string]bool
	asked   []string
}

func (f *fakeCapabilityStore) get(_ context.Context, key string) (string, bool, error) {
	f.asked = append(f.asked, key)
	if f.failing[key] {
		return "", false, errors.New("i/o timeout")
	}
	v, ok := f.vals[key]
	return v, ok, nil
}

func runCapabilityCheckFake(t *testing.T, f *fakeCapabilityStore, zones ...uint32) (int, string) {
	t.Helper()
	code, report := evaluateCapabilityCheck(probeCapabilityMarkers(context.Background(), f.get, zones))
	return code, strings.Join(report, "\n")
}

func TestCapabilityCheck_AllPresentExit0(t *testing.T) {
	f := &fakeCapabilityStore{vals: map[string]string{
		capabilityKey(101): capabilityRoutingV1,
		capabilityKey(902): capabilityRoutingV1,
	}}
	code, out := runCapabilityCheckFake(t, f, 101, 902)
	if code != capabilityCheckExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{"zone 101: present", "zone 902: present", "2 present, 0 missing, 0 unreadable"} {
		if !strings.Contains(out, want) {
			t.Errorf("report must contain %q:\n%s", want, out)
		}
	}
	// 逐 zone 读,每个 zone 恰好读一次自己的键。
	if strings.Join(f.asked, ",") != capabilityKey(101)+","+capabilityKey(902) {
		t.Errorf("asked = %v", f.asked)
	}
}

func TestCapabilityCheck_MissingExit1(t *testing.T) {
	f := &fakeCapabilityStore{vals: map[string]string{
		capabilityKey(101): capabilityRoutingV1,
		capabilityKey(103): "placement-routing-v0", // 值不认识 = missing,并打印实际值
	}}
	code, out := runCapabilityCheckFake(t, f, 101, 102, 103)
	if code != capabilityCheckExitMissing {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"zone 101: present", "zone 102: missing", `zone 103: missing`, `"placement-routing-v0"`,
		"1 present, 2 missing, 0 unreadable", "NOT confirmed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report must contain %q:\n%s", want, out)
		}
	}
}

// 读失败 = 结论不可信,exit 2 优先于 exit 1(与 audit 同一口径)。
func TestCapabilityCheck_UnreadableExit2(t *testing.T) {
	f := &fakeCapabilityStore{
		vals:    map[string]string{capabilityKey(101): capabilityRoutingV1},
		failing: map[string]bool{capabilityKey(103): true},
	}
	code, out := runCapabilityCheckFake(t, f, 101, 102, 103)
	if code != capabilityCheckExitUnreadable {
		t.Fatalf("exit = %d, want 2 (unreadable wins over missing)\n%s", code, out)
	}
	for _, want := range []string{"zone 102: missing", "zone 103: unreadable", "i/o timeout", "1 present, 1 missing, 1 unreadable",
		"could NOT complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("report must contain %q:\n%s", want, out)
		}
	}
	// 只有读失败、没有缺失,同样是 2。
	f = &fakeCapabilityStore{failing: map[string]bool{capabilityKey(101): true}}
	if code, out := runCapabilityCheckFake(t, f, 101); code != capabilityCheckExitUnreadable {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
}

// 参数用法错误一个标记都没查,按「没查成」(2)退出,不能与 missing(1)同码:runbook §8 Step 6 的 exit 1 处置是换 go/db。
func TestCapabilityCheckZones_UsageErrorsAreUnreadable(t *testing.T) {
	for _, raw := range []string{"", "   ", "none", "dst", "src,101", "0", "abc", "101,none", ","} {
		zones, code, err := capabilityCheckZones(raw)
		if err == nil {
			t.Errorf("%q: must be refused, got zones %v", raw, zones)
			continue
		}
		if code != capabilityCheckExitUnreadable {
			t.Errorf("%q: usage error exit = %d, want %d (not checked is not missing): %v", raw, code, capabilityCheckExitUnreadable, err)
		}
		if !strings.Contains(err.Error(), "-db-capability-zones") {
			t.Errorf("%q: the error must name -db-capability-zones: %v", raw, err)
		}
	}
	zones, code, err := capabilityCheckZones(" 102, 101 ")
	if err != nil || code != capabilityCheckExitOK || len(zones) != 2 || zones[0] != 101 || zones[1] != 102 {
		t.Fatalf("a valid list must pass sorted: zones=%v code=%d err=%v", zones, code, err)
	}
}

// 什么都没查不能当通过(入口已由 requireCapabilityZones 拦住,这里是纯判定的兜底)。
func TestCapabilityCheck_NoProbeIsNotAPass(t *testing.T) {
	if code, _ := evaluateCapabilityCheck(nil); code != capabilityCheckExitUnreadable {
		t.Fatalf("exit = %d, want 2", code)
	}
}
