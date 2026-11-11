package zonepolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeEtcd 是 Getter 的假实现:按顺序回放预设的结果,记下读过的键。
type fakeEtcd struct {
	keys    []string
	results []fakeResult
}

type fakeResult struct {
	value string
	found bool
	err   error
}

func (f *fakeEtcd) get(_ context.Context, key string) ([]byte, bool, error) {
	f.keys = append(f.keys, key)
	if len(f.results) == 0 {
		return nil, false, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	return []byte(r.value), r.found, r.err
}

// 金样是两种语言之间的契约:网关写出来的就是这个形状,login 必须原样读懂。
func TestParse_GoldenMatchesGatewayContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "policy.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(golden): %v", err)
	}
	if want := (Policy{Capacity: 45000, CreateRoleClosed: true}); got != want {
		t.Errorf("Parse(golden) = %+v, want %+v", got, want)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Policy
		wantErr bool
	}{
		{"both fields", `{"capacity":36000,"createRoleClosed":false}`, Policy{Capacity: 36000}, false},
		{"missing field reads as zero (additive evolution)", `{"capacity":100}`, Policy{Capacity: 100}, false},
		{"unknown field ignored", `{"capacity":100,"createRoleClosed":true,"future":1}`, Policy{Capacity: 100, CreateRoleClosed: true}, false},
		{"negative capacity", `{"capacity":-1}`, Policy{}, true},
		{"capacity beyond uint32", `{"capacity":4294967296}`, Policy{}, true},
		{"not json", `1;DROP`, Policy{}, true},
		{"wrong type", `{"createRoleClosed":"yes"}`, Policy{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse(%s) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("Parse(%s) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	if got := Key(101); got != "/mmorpg/zone_policy/zone/101" {
		t.Errorf("Key(101) = %q", got)
	}
}

// 读到值就用;键不存在清掉;读失败、值坏了都保留上一次的值。
func TestRefresh_KeepsLastKnownOnErrorAndClearsOnMissing(t *testing.T) {
	f := &fakeEtcd{results: []fakeResult{
		{value: `{"capacity":45000,"createRoleClosed":false}`, found: true},
		{err: errors.New("etcd unavailable")},
		{value: `not json`, found: true},
		{value: `{"capacity":50000,"createRoleClosed":true}`, found: true},
		{found: false},
	}}
	r := NewReader(101, f.get)
	ctx := context.Background()

	if _, ok := r.Current(); ok {
		t.Fatal("Current before any read must be unknown")
	}

	steps := []struct {
		outcome Outcome
		want    Policy
		ok      bool
	}{
		{OutcomeOK, Policy{Capacity: 45000}, true},
		{OutcomeError, Policy{Capacity: 45000}, true},
		{OutcomeInvalid, Policy{Capacity: 45000}, true},
		{OutcomeOK, Policy{Capacity: 50000, CreateRoleClosed: true}, true},
		{OutcomeMissing, Policy{}, false},
	}
	for i, s := range steps {
		outcome, err := r.Refresh(ctx)
		if outcome != s.outcome {
			t.Fatalf("step %d: outcome = %s (err %v), want %s", i, outcome, err, s.outcome)
		}
		if (err != nil) != (s.outcome == OutcomeError || s.outcome == OutcomeInvalid) {
			t.Errorf("step %d: err = %v for outcome %s", i, err, outcome)
		}
		got, ok := r.Current()
		if ok != s.ok || got != s.want {
			t.Errorf("step %d: Current = (%+v, %v), want (%+v, %v)", i, got, ok, s.want, s.ok)
		}
	}
	for _, k := range f.keys {
		if k != "/mmorpg/zone_policy/zone/101" {
			t.Errorf("read key %q, want only this zone's key", k)
		}
	}
}

func TestNilReaderIsUnknown(t *testing.T) {
	var r *Reader
	if _, ok := r.Current(); ok {
		t.Error("nil reader must report no policy")
	}
	if r.ZoneID() != 0 {
		t.Error("nil reader zone id must be 0")
	}
}

// Run 第一次立刻读,不等第一个周期;ctx 结束就返回。
func TestRun_ReadsImmediatelyAndStopsWithContext(t *testing.T) {
	read := make(chan struct{}, 8)
	r := NewReader(7, func(context.Context, string) ([]byte, bool, error) {
		read <- struct{}{}
		return []byte(`{"capacity":10}`), true, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()

	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not read immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
	if got, ok := r.Current(); !ok || got.Capacity != 10 {
		t.Errorf("Current = (%+v, %v), want capacity 10", got, ok)
	}
}

func TestChanged(t *testing.T) {
	p := Policy{Capacity: 1}
	q := Policy{Capacity: 2}
	cases := []struct {
		name  string
		known bool
		last  *Policy
		cur   Policy
		ok    bool
		want  bool
	}{
		{"first result is always logged", false, nil, p, true, true},
		{"first missing is logged", false, nil, Policy{}, false, true},
		{"same value", true, &p, p, true, false},
		{"value changed", true, &p, q, true, true},
		{"became missing", true, &p, Policy{}, false, true},
		{"still missing", true, nil, Policy{}, false, false},
		{"appeared", true, nil, p, true, true},
	}
	for _, tc := range cases {
		if got := changed(tc.known, tc.last, tc.cur, tc.ok); got != tc.want {
			t.Errorf("%s: changed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
