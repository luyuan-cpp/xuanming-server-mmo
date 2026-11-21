package dsclient

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/zrpc"
)

func TestConfDisablesOnlyBreaker(t *testing.T) {
	in := zrpc.RpcClientConf{
		Target:   "127.0.0.1:9000",
		Timeout:  1500,
		NonBlock: true,
		Middlewares: zrpc.ClientMiddlewaresConf{
			Trace:      true,
			Duration:   true,
			Prometheus: true,
			Breaker:    true,
			Timeout:    true,
		},
	}
	got := Conf(in)
	if got.Middlewares.Breaker {
		t.Fatal("Conf must set Middlewares.Breaker = false")
	}
	want := in
	want.Middlewares.Breaker = false
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Conf altered fields other than Middlewares.Breaker:\ngot  = %+v\nwant = %+v", got, want)
	}
}

func skippedDir(name string) bool {
	switch name {
	case "proto", "generated", "vendor", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// 机械守卫(docs/design/data-service-breaker-per-logical-db.md §5.2 第 3 条):
// go/ 下任何构造 data_service gRPC 客户端的非测试文件都必须通过 dsclient.Conf 关掉 zrpc 按方法的客户端熔断,
// 防止新增调用方悄悄沿用缺省 true 把分片隔离破坏掉。
func TestAllDataServiceClientConstructorsUseDsclientConf(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "shared", "dsclient", "dsclient.go")); err != nil {
		t.Fatalf("go/ root not found at %s: %v", root, err)
	}

	var matched int
	var violations []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".pb.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(content)
		if !strings.Contains(text, "NewDataServiceClient(") {
			return nil
		}
		matched++
		if !strings.Contains(text, "dsclient.Conf(") {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			violations = append(violations, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if matched < 10 {
		t.Fatalf("found only %d files calling NewDataServiceClient under %s; expected at least 10", matched, root)
	}
	if len(violations) > 0 {
		t.Fatalf("data_service client constructor(s) missing dsclient.Conf(...) to disable per-method client breaker:\n  - %s", strings.Join(violations, "\n  - "))
	}
}
