package clientplayerloginlogic

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// TestGateDrainSettleCoversEnterGameChainBudget:gate 排空迁移对停在选角界面的连接,转发 EnterGame 之后 kEnterGameSettle
// 之内不迁(cpp/nodes/gate/handler/rpc/drain_migration.h,docs/design/gate-drain-session-migration.md §4.3)。这个窗口必须
// 盖住本包 EnterGame 后台链的总预算 enterGameChainBudget:那段时间里 login 可能还在给这条连接绑角色,迁走会让新连接上的
// EnterGame 撞上「登录中」。两边各写一份字面量,这里直接读 C++ 头文件核对,哪一边改短了都会在这里红。
func TestGateDrainSettleCoversEnterGameChainBudget(t *testing.T) {
	headerPath := filepath.Join("..", "..", "..", "..", "..", "cpp", "nodes", "gate", "handler", "rpc", "drain_migration.h")
	data, err := os.ReadFile(headerPath)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("C++ source not found (only go/ was copied into the build environment); skipping the cross-language check")
	}
	if err != nil {
		t.Fatalf("read drain_migration.h: %v", err)
	}
	m := regexp.MustCompile(`inline constexpr std::chrono::(minutes|seconds) kEnterGameSettle\{(\d+)\}`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("kEnterGameSettle not found in drain_migration.h: the declaration changed, update this test together with it")
	}
	count, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		t.Fatalf("parse kEnterGameSettle value %q: %v", m[2], err)
	}
	unit := time.Minute
	if m[1] == "seconds" {
		unit = time.Second
	}
	settle := time.Duration(count) * unit
	if settle < enterGameChainBudget {
		t.Fatalf("gate kEnterGameSettle = %v is shorter than login enterGameChainBudget = %v: change both sides in the same batch",
			settle, enterGameChainBudget)
	}
}
