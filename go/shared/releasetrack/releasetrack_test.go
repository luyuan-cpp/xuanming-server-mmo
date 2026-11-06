package releasetrack

import (
	"testing"
)

// 下面两张样例表与 C++ cpp/tests/client_endpoint_test/release_track_test.cpp 逐行相同,
// 两边的解析与选点规则必须给出同样的结果。改一边时同步改另一边。

func TestParseSamples(t *testing.T) {
	cases := []struct {
		raw     string
		want    Track
		wantErr bool
	}{
		{raw: "", want: Stable},
		{raw: "stable", want: Stable},
		{raw: "canary", want: Canary},
		{raw: "Canary", wantErr: true},
		{raw: "STABLE", wantErr: true},
		{raw: " stable", wantErr: true},
		{raw: "stable\n", wantErr: true},
		{raw: "beta", wantErr: true},
	}
	for _, c := range cases {
		got, err := Parse(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", c.raw, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %v, %v; want %v, nil", c.raw, got, err, c.want)
		}
	}
}

type candidate struct {
	name  string
	track Track
}

func names(items []candidate) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.name)
	}
	return out
}

func TestSelectSamples(t *testing.T) {
	s1 := candidate{"s1", Stable}
	s2 := candidate{"s2", Stable}
	c1 := candidate{"c1", Canary}
	c2 := candidate{"c2", Canary}
	cases := []struct {
		name       string
		caller     Track
		candidates []candidate
		want       []string
	}{
		{"canary 调用方有 canary 时只留 canary", Canary, []candidate{s1, c1, s2, c2}, []string{"c1", "c2"}},
		{"canary 调用方没有 canary 时退回 stable", Canary, []candidate{s1, s2}, []string{"s1", "s2"}},
		{"stable 调用方只留 stable", Stable, []candidate{s1, c1, s2}, []string{"s1", "s2"}},
		{"stable 调用方永远不进 canary", Stable, []candidate{c1, c2}, []string{}},
		{"canary 调用方没有候选", Canary, nil, []string{}},
		{"stable 调用方没有候选", Stable, nil, []string{}},
	}
	trackOf := func(c candidate) Track { return c.track }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := names(Select(c.caller, c.candidates, trackOf))
			if len(got) != len(c.want) {
				t.Fatalf("Select = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Select = %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestSelectDoesNotModifyCandidates(t *testing.T) {
	candidates := []candidate{{"s1", Stable}, {"c1", Canary}}
	_ = Select(Canary, candidates, func(c candidate) Track { return c.track })
	if candidates[0].name != "s1" || candidates[1].name != "c1" || len(candidates) != 2 {
		t.Fatalf("Select modified its input: %v", candidates)
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, track := range []Track{Stable, Canary} {
		got, err := Parse(track.String())
		if err != nil || got != track {
			t.Fatalf("Parse(%q) = %v, %v; want %v", track.String(), got, err, track)
		}
	}
}

// 全局后台工作只由 stable 承担(设计文档 §2.5)。
func TestRunsGlobalBackgroundWork(t *testing.T) {
	if !Stable.RunsGlobalBackgroundWork() {
		t.Fatal("stable must run global background work")
	}
	if Canary.RunsGlobalBackgroundWork() {
		t.Fatal("canary must not run global background work")
	}
}

// 越界值只能由强转得到;它的 String 不能是 "stable",否则写出去会被当成 stable。
func TestOutOfRangeTrackDoesNotPassAsStable(t *testing.T) {
	bogus := Track(7)
	if _, err := Parse(bogus.String()); err == nil {
		t.Fatalf("Parse(%q) accepted an out-of-range track", bogus.String())
	}
	if bogus.IsCanary() {
		t.Fatalf("Track(7).IsCanary() = true")
	}
}

func TestFromEnv(t *testing.T) {
	cases := []struct {
		value   string
		want    Track
		wantErr bool
	}{
		{value: "", want: Stable}, // 未设置与空值同义(os.Getenv 都返回空串)
		{value: "stable", want: Stable},
		{value: "canary", want: Canary},
		{value: "gray", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			t.Setenv(EnvVar, c.value)
			got, err := FromEnv()
			if c.wantErr {
				if err == nil {
					t.Fatalf("FromEnv() with %q = %v, want error", c.value, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("FromEnv() with %q = %v, %v; want %v", c.value, got, err, c.want)
			}
		})
	}
}
