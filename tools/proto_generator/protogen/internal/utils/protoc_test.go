package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeProtocVersion(t *testing.T) {
	header := "#define PROTOBUF_OSS_VERSION 7035001\r\n#define PROTOBUF_OSS_VERSION_SUFFIX \"\"\r\n"
	got, err := runtimeProtocVersion(header)
	if err != nil {
		t.Fatalf("runtimeProtocVersion: %v", err)
	}
	if got != "35.1" {
		t.Fatalf("got %q, want 35.1", got)
	}
	if _, err := runtimeProtocVersion("#define PROTOBUF_OSS_VERSION_SUFFIX \"\"\n"); err == nil {
		t.Fatal("a header without the version must be rejected")
	}
}

func TestParseProtocVersion(t *testing.T) {
	got, err := parseProtocVersion("libprotoc 35.1\r\n")
	if err != nil {
		t.Fatalf("parseProtocVersion: %v", err)
	}
	if got != "35.1" {
		t.Fatalf("got %q, want 35.1", got)
	}
	if _, err := parseProtocVersion("protoc is not installed"); err == nil {
		t.Fatal("unexpected output must be rejected")
	}
}

// PATH 上的 protoc 版本不对、SDK 的对:用 SDK 的。2026-11-01 第一轮验证就是 PATH 上的 35.0 配 35.1 的运行库,
// 全部 C++ 工程编不过。
func TestChooseProtocSkipsWrongVersions(t *testing.T) {
	versions := map[string]string{"sdk": "35.1", "path": "35.0"}
	versionOf := func(path string) (string, error) {
		version, ok := versions[path]
		if !ok {
			return "", errors.New("cannot run")
		}
		return version, nil
	}

	got, err := chooseProtoc("35.1", []string{"broken", "path", "sdk"}, versionOf)
	if err != nil {
		t.Fatalf("chooseProtoc: %v", err)
	}
	if got != "sdk" {
		t.Fatalf("got %q, want sdk", got)
	}

	_, err = chooseProtoc("35.2", []string{"broken", "path", "sdk"}, versionOf)
	if err == nil {
		t.Fatal("no matching version must be an error")
	}
	for _, want := range []string{"35.2", "path (35.0)", "sdk (35.1)", "broken (cannot run)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestProtocCandidatesPrefersReleaseOverDebug(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	relBin := filepath.Join(root, "install_vs2026_rel", "bin", "protoc.exe")
	dbgBin := filepath.Join(root, "install_vs2026_dbg", "bin", "protoc.exe")
	for _, path := range []string{relBin, dbgBin} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	candidates := protocCandidates(srcDir)
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 sdk candidates, got %v", candidates)
	}
	if candidates[0] != relBin || candidates[1] != dbgBin {
		t.Fatalf("expected Release before Debug: got %v, want [%s %s ...]", candidates, relBin, dbgBin)
	}
}
