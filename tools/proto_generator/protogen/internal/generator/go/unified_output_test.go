package _go

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"protogen/logger"
)

func TestUnifiedOutputPublishesOnlySuccessfulChangedBindings(t *testing.T) {
	previous := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previous })
	parent := t.TempDir()
	output := filepath.Join(parent, "output")
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(output, "stable.pb.go")
	if err := os.WriteFile(stable, []byte("stable"), 0644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1000, 0)
	if err := os.Chtimes(stable, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("protoc 写出部分结果后失败")
	err := generateUnifiedOutput(output, parent, func(scratch string) error {
		if err := os.WriteFile(filepath.Join(scratch, "stable.pb.go"), []byte("partial"), 0644); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("没有保留生成失败: %v", err)
	}
	if actual, err := os.ReadFile(stable); err != nil || string(actual) != "stable" {
		t.Fatalf("生成失败污染原绑定: %q, %v", actual, err)
	}
	err = generateUnifiedOutput(output, parent, func(scratch string) error {
		if err := os.WriteFile(filepath.Join(scratch, "stable.pb.go"), []byte("stable"), 0644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(scratch, "guild"), 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(scratch, "guild", "guild.pb.go"), []byte("updated"), 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(filepath.Join(output, "guild", "guild.pb.go")); err != nil || string(actual) != "updated" {
		t.Fatalf("未发布成功的新绑定: %q, %v", actual, err)
	}
	if info, err := os.Stat(stable); err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("未变化绑定被重写: %v, %v", info, err)
	}
}

func TestGoPackageRewriteReleasesItsOwnReader(t *testing.T) {
	previous := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previous })
	path := filepath.Join(t.TempDir(), "guild.proto")
	if err := os.WriteFile(path, []byte("syntax = \"proto3\";\noption go_package = \"old\";\n"), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := AddGoPackage(path, "proto/guild", false)
	if err != nil || !changed {
		t.Fatalf("生成器自己的读句柄不能阻塞发布: changed=%v err=%v", changed, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "syntax = \"proto3\";\noption go_package = \"proto/guild\";\n" {
		t.Fatalf("契约未完整转换: %q, %v", content, err)
	}
}

// 统一 Go 绑定不收 opentelemetry/(官方 OTLP 拷贝,只给 C++ 用):暂存目录里删掉它,别的目录一个都不动;
// 名单里写错成带路径分隔符或 ".." 的项被忽略,不会删到暂存目录之外。
func TestPruneUnifiedGoExcludedDirsDropsOnlyTheListedTopLevelDirectories(t *testing.T) {
	parent := t.TempDir()
	staging := filepath.Join(parent, "proto")
	write := func(relative string) {
		path := filepath.Join(staging, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("syntax = \"proto3\";\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("opentelemetry/proto/trace/v1/trace.proto")
	write("opentelemetry/proto/collector/trace/v1/trace_service.proto")
	write("common/base/empty.proto")
	write("scene/opentelemetry_like.proto")
	outside := filepath.Join(parent, "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := pruneUnifiedGoExcludedDirs(staging, append([]string{"..", "../keep.txt", ""}, unifiedGoExcludedDirs...)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "opentelemetry")); !os.IsNotExist(err) {
		t.Fatalf("opentelemetry/ 应从暂存目录删掉: %v", err)
	}
	for _, kept := range []string{"common/base/empty.proto", "scene/opentelemetry_like.proto"} {
		if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(kept))); err != nil {
			t.Fatalf("%s 不该被删: %v", kept, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("暂存目录之外的文件不该被删: %v", err)
	}
	// 再删一次(目录已经不在)不算错误。
	if err := pruneUnifiedGoExcludedDirs(staging, unifiedGoExcludedDirs); err != nil {
		t.Fatal(err)
	}
}
