package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"table_sync/internal/bundle"
)

// 回滚前的只增不删表检查缺省必做、并且比的必须是这次回滚的两包(数据覆写审计 r2 第 4 轮 r2-ops#1)。

const rollbackTestSchema = "abcdef0123456789"

// writeRollbackTestDir 写一个只有 Item(ids_permanent)一张表的清单第 2 版目录,返回它的 content_digest。
func writeRollbackTestDir(t *testing.T, dir, itemJSON string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	raw := []byte(itemJSON)
	fileSum := sha256.Sum256(raw)
	fileHex := hex.EncodeToString(fileSum[:])
	require.NoError(t, os.WriteFile(filepath.Join(dir, "item.json"), raw, 0o644))
	contentSum := sha256.Sum256([]byte("Item" + fileHex))
	content := hex.EncodeToString(contentSum[:])
	manifest := bundle.Manifest{
		SchemaVersion: 2,
		SchemaDigest:  rollbackTestSchema,
		Version:       1,
		ContentDigest: content,
		Tables: map[string]bundle.TableEntry{"Item": {
			IDsPermanent: true,
			Rows:         1,
			Artifacts:    []bundle.Artifact{{Kind: "json", File: "item.json", Size: int64(len(raw)), SHA256: fileHex}},
		}},
	}
	out, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, bundle.ManifestFilename), out, 0o644))
	return content
}

// fakeAdmin 是 gateway_node /admin/tables 的替身:GET 回一条线(生效包 live、实例已全部加载),PUT 记下请求体。
type fakeAdmin struct {
	mu   sync.Mutex
	live string
	gets int
	puts []map[string]any
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		f.gets++
		line := map[string]any{
			"track":         "stable",
			"schema_digest": rollbackTestSchema,
			"summary":       map[string]int{"total": 1, "loaded": 1},
		}
		if f.live != "" {
			line["active"] = map[string]any{"content_digest": f.live}
		}
		_ = json.NewEncoder(w).Encode([]any{line})
	case http.MethodPut:
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.puts = append(f.puts, body)
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// seen 在锁内取替身记下的请求(处理函数跑在服务端 goroutine 上)。
func (f *fakeAdmin) seen() (gets int, puts []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, append([]map[string]any(nil), f.puts...)
}

func (f *fakeAdmin) setLive(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = digest
}

func rollbackArgs(adminURL, targetDigest string, extra ...string) []string {
	return append([]string{
		"-admin-url", adminURL, "-admin-key", "test-key", "-track", "stable",
		"-schema-digest", rollbackTestSchema, "-target-digest", targetDigest,
		"-target-sha256", strings.Repeat("a", 64), "-target-version", "1",
	}, extra...)
}

func TestRollbackRequiresBothDirsUnlessForce(t *testing.T) {
	admin := &fakeAdmin{live: strings.Repeat("b", 64)}
	srv := httptest.NewServer(admin)
	defer srv.Close()
	target := strings.Repeat("c", 64)

	for _, extra := range [][]string{nil, {"-current-dir", t.TempDir()}, {"-target-dir", t.TempDir()}} {
		err := cmdRollback(rollbackArgs(srv.URL, target, extra...))
		require.ErrorContains(t, err, "-current-dir and -target-dir", "extra=%v", extra)
	}
	gets, puts := admin.seen()
	require.Zero(t, gets, "refused before asking the gateway")
	require.Empty(t, puts, "no pointer written")

	// -force 不给目录:跳过检查照常回滚(打 WARN)。
	require.NoError(t, cmdRollback(rollbackArgs(srv.URL, target, "-force")))
	_, puts = admin.seen()
	require.Len(t, puts, 1)
	require.Equal(t, target, puts[0]["content_digest"])
}

func TestRollbackChecksTheLiveBundleAndTheTarget(t *testing.T) {
	root := t.TempDir()
	oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
	oldDigest := writeRollbackTestDir(t, oldDir, `{"data":[{"id":1001}]}`)
	newDigest := writeRollbackTestDir(t, newDir, `{"data":[{"id":1001},{"id":1002}]}`)
	admin := &fakeAdmin{live: newDigest}
	srv := httptest.NewServer(admin)
	defer srv.Close()

	// current-dir 不是线上生效包(同一目录传两次):-force 也不放过。
	err := cmdRollback(rollbackArgs(srv.URL, oldDigest, "-current-dir", oldDir, "-target-dir", oldDir, "-force"))
	require.ErrorContains(t, err, "live bundle")
	// target-dir 不是 -target-digest 那一包。
	err = cmdRollback(rollbackArgs(srv.URL, oldDigest, "-current-dir", newDir, "-target-dir", newDir))
	require.ErrorContains(t, err, "rollback target")
	// 两包对了,新包的 Item 1002 会消失:不加 -force 拒绝。
	err = cmdRollback(rollbackArgs(srv.URL, oldDigest, "-current-dir", newDir, "-target-dir", oldDir))
	require.ErrorContains(t, err, "Item=[1002]")
	_, puts := admin.seen()
	require.Empty(t, puts, "no pointer written while the check refuses")

	// 加 -force 才回滚。
	require.NoError(t, cmdRollback(rollbackArgs(srv.URL, oldDigest, "-current-dir", newDir, "-target-dir", oldDir, "-force")))
	_, puts = admin.seen()
	require.Len(t, puts, 1)
	require.Equal(t, oldDigest, puts[0]["content_digest"])
	require.Equal(t, true, puts[0]["immediate"])

	// 线上没有生效指针(用的是镜像自带的表):给了目录也核不了,拒绝。
	admin.setLive("")
	err = cmdRollback(rollbackArgs(srv.URL, oldDigest, "-current-dir", newDir, "-target-dir", oldDir, "-force"))
	require.ErrorContains(t, err, "no active table bundle pointer")
}
