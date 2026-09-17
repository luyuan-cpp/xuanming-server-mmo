package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// setInjected 模拟链接器 -X 注入,测试结束恢复。改的是包级变量,所以本文件的用例都不能 t.Parallel。
func setInjected(t *testing.T, version, commit, buildTime string) {
	t.Helper()
	oldVersion, oldCommit, oldBuildTime := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = oldVersion, oldCommit, oldBuildTime
	})
	Version, Commit, BuildTime = version, commit, buildTime
}

func TestGet_InjectedValuesWin(t *testing.T) {
	setInjected(t, "v1.2.3", "0123456789ab", "2026-09-16T08:00:00Z")

	got := Get()
	want := Info{
		Version:   "v1.2.3",
		Commit:    "0123456789ab",
		BuildTime: "2026-09-16T08:00:00Z",
		GoVersion: runtime.Version(),
	}
	if got != want {
		t.Fatalf("注入值应原样返回且不回落 vcs:got %+v, want %+v", got, want)
	}
}

func TestGet_EmptyInjectionNormalizedToDefaults(t *testing.T) {
	// `-X shared/buildinfo.Commit=` 这类空注入与没注入同义;Commit 注入了非空值才能确定性地跳过 vcs 回落。
	setInjected(t, "", "0123456789ab", " ")

	got := Get()
	if got.Version != "dev" {
		t.Errorf("空 Version 应归一为 dev,got %q", got.Version)
	}
	if got.BuildTime != "unknown" {
		t.Errorf("空白 BuildTime 应归一为 unknown,got %q", got.BuildTime)
	}
}

func TestApplyVCSSettings(t *testing.T) {
	const fullRevision = "0123456789abcdef0123456789abcdef01234567"
	base := Info{Version: "dev", Commit: "unknown", BuildTime: "unknown", GoVersion: "go1.26.5"}

	tests := []struct {
		name     string
		info     Info
		settings []debug.BuildSetting
		want     Info
	}{
		{
			name: "回落解析:revision 截 12 位、time 补 BuildTime、modified=true",
			info: base,
			settings: []debug.BuildSetting{
				{Key: "-trimpath", Value: "true"},
				{Key: "vcs", Value: "git"},
				{Key: "vcs.revision", Value: fullRevision},
				{Key: "vcs.time", Value: "2026-09-15T10:20:30Z"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: Info{Version: "dev", Commit: "0123456789ab", BuildTime: "2026-09-15T10:20:30Z", GoVersion: "go1.26.5", Modified: true},
		},
		{
			name: "modified=false 不算脏树",
			info: base,
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: fullRevision},
				{Key: "vcs.modified", Value: "false"},
			},
			want: Info{Version: "dev", Commit: "0123456789ab", BuildTime: "unknown", GoVersion: "go1.26.5"},
		},
		{
			name:     "revision 不足 12 位原样保留",
			info:     base,
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			want:     Info{Version: "dev", Commit: "abc123", BuildTime: "unknown", GoVersion: "go1.26.5"},
		},
		{
			name:     "没有 vcs 设置时保持 unknown",
			info:     base,
			settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "0"}},
			want:     base,
		},
		{
			name: "已注入的 BuildTime 不被 vcs.time 覆盖",
			info: Info{Version: "dev", Commit: "unknown", BuildTime: "2026-09-16T08:00:00Z", GoVersion: "go1.26.5"},
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: fullRevision},
				{Key: "vcs.time", Value: "2026-09-15T10:20:30Z"},
			},
			want: Info{Version: "dev", Commit: "0123456789ab", BuildTime: "2026-09-16T08:00:00Z", GoVersion: "go1.26.5"},
		},
		{
			name: "已注入 Commit 时完全不回落",
			info: Info{Version: "v1.2.3", Commit: "fedcba987654-dirty", BuildTime: "unknown", GoVersion: "go1.26.5"},
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: fullRevision},
				{Key: "vcs.time", Value: "2026-09-15T10:20:30Z"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: Info{Version: "v1.2.3", Commit: "fedcba987654-dirty", BuildTime: "unknown", GoVersion: "go1.26.5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyVCSSettings(tt.info, tt.settings); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFormatStartupLine(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "干净树",
			info: Info{Version: "v1.2.3", Commit: "0123456789ab", BuildTime: "2026-09-16T08:00:00Z", GoVersion: "go1.26.5"},
			want: "service starting service=login version=v1.2.3 commit=0123456789ab build_time=2026-09-16T08:00:00Z go_version=go1.26.5",
		},
		{
			name: "Modified 时 commit 带 -dirty",
			info: Info{Version: "dev", Commit: "0123456789ab", BuildTime: "unknown", GoVersion: "go1.26.5", Modified: true},
			want: "service starting service=login version=dev commit=0123456789ab-dirty build_time=unknown go_version=go1.26.5",
		},
		{
			name: "commit 已带 -dirty 不重复追加",
			info: Info{Version: "dev", Commit: "0123456789ab-dirty", BuildTime: "unknown", GoVersion: "go1.26.5", Modified: true},
			want: "service starting service=login version=dev commit=0123456789ab-dirty build_time=unknown go_version=go1.26.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatStartupLine("login", tt.info); got != tt.want {
				t.Fatalf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestStartupLine_UsesInjectedValues(t *testing.T) {
	setInjected(t, "v1.2.3", "0123456789ab", "2026-09-16T08:00:00Z")

	got := StartupLine("trade")
	want := "service starting service=trade version=v1.2.3 commit=0123456789ab build_time=2026-09-16T08:00:00Z go_version=" + runtime.Version()
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
