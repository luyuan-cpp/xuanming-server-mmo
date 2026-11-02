package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	_config "protogen/internal/config"
)

// 生成器用的 protoc 必须和 third_party/protobuf 的 C++ 运行库同版本:pb.h 写死 PROTOBUF_VERSION,差一个补丁号就编不过,
// 而且要等到 C++ 编译才报错。所以优先用 SDK 里和运行库同源编出来的 protoc(tools/scripts/third_party/build_protobuf.ps1),
// 没有才退到 PATH 上的,两种都先核对版本,对不上就不生成。

var (
	protocOnce sync.Once
	protocPath string
	protocErr  error
)

// ProtocPath 返回本次生成用的 protoc,整个进程只解析一次。
func ProtocPath() (string, error) {
	protocOnce.Do(func() {
		protocPath, protocErr = resolveProtoc(_config.Global.Paths.ProtobufDir)
	})
	return protocPath, protocErr
}

func resolveProtoc(protobufSrcDir string) (string, error) {
	header, err := os.ReadFile(filepath.Join(protobufSrcDir, "google", "protobuf", "runtime_version.h"))
	if err != nil {
		return "", fmt.Errorf("read protobuf runtime version: %w", err)
	}
	want, err := runtimeProtocVersion(string(header))
	if err != nil {
		return "", err
	}
	return chooseProtoc(want, protocCandidates(protobufSrcDir), protocVersionOf)
}

// protocCandidates 按优先级列出本机的 protoc:SDK 的 Debug、Release,最后是 PATH。
func protocCandidates(protobufSrcDir string) []string {
	protobufRoot := filepath.Dir(filepath.Clean(protobufSrcDir))
	var candidates []string
	for _, sdk := range []string{"install_vs2026_dbg", "install_vs2026_rel"} {
		path := filepath.Join(protobufRoot, sdk, "bin", "protoc.exe")
		if _, err := os.Stat(path); err == nil {
			candidates = append(candidates, path)
		}
	}
	if path, err := exec.LookPath("protoc"); err == nil {
		candidates = append(candidates, path)
	}
	return candidates
}

// chooseProtoc 返回候选里第一个版本等于 want 的。都不是时错误里列出每个候选的实际版本。
func chooseProtoc(want string, candidates []string, versionOf func(string) (string, error)) (string, error) {
	found := make([]string, 0, len(candidates))
	for _, path := range candidates {
		got, err := versionOf(path)
		if err != nil {
			found = append(found, fmt.Sprintf("%s (%v)", path, err))
			continue
		}
		if got == want {
			return path, nil
		}
		found = append(found, fmt.Sprintf("%s (%s)", path, got))
	}
	return "", fmt.Errorf("no protoc matches C++ runtime version %s, found [%s]; build the SDK with tools/scripts/third_party/build_protobuf.ps1",
		want, strings.Join(found, ", "))
}

func protocVersionOf(path string) (string, error) {
	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", err
	}
	return parseProtocVersion(string(output))
}

var runtimeVersionPattern = regexp.MustCompile(`(?m)^#define\s+PROTOBUF_OSS_VERSION\s+(\d+)\s*$`)

// runtimeProtocVersion 从 runtime_version.h 取对应的 protoc 版本。C++ 版本号是 主*1000000 + 次*1000 + 补丁,
// protoc 只报次和补丁:7035001 对应 35.1。
func runtimeProtocVersion(header string) (string, error) {
	match := runtimeVersionPattern.FindStringSubmatch(header)
	if match == nil {
		return "", fmt.Errorf("PROTOBUF_OSS_VERSION not found in runtime_version.h")
	}
	version, err := strconv.Atoi(match[1])
	if err != nil {
		return "", fmt.Errorf("invalid PROTOBUF_OSS_VERSION %q: %w", match[1], err)
	}
	return fmt.Sprintf("%d.%d", version/1000%1000, version%1000), nil
}

// parseProtocVersion 解 protoc --version 的输出,形如 libprotoc 35.1。
func parseProtocVersion(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[0] != "libprotoc" {
		return "", fmt.Errorf("unexpected protoc --version output %q", strings.TrimSpace(output))
	}
	return fields[1], nil
}
