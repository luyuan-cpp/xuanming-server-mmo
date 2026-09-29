package internal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"protogen/logger"

	"go.uber.org/zap"
)

// event_id.txt 的墓碑行(turn-based §22 D67)只占号:Go 事件常量生成必须整行跳过,
// 否则会拼出 `const reserved:BEventId = 1`,写进各 Go 服务的 generated/pb/game/event_id.go 后全部编译失败。
// 内容按磁盘格式逐字写出(不用 ReservedEventIdPrefix 拼),同时钉住墓碑行的文件格式。
func TestGenerateWithSuffixSkipsReservedEventIds(t *testing.T) {
	originalLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = originalLogger })

	path := filepath.Join(t.TempDir(), "event_id.txt")
	if err := os.WriteFile(path, []byte("0=A\n1=reserved:B\n2=C\n"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	consts, err := NewConstantsGenerator(path).GenerateWithSuffix("EventId")
	if err != nil {
		t.Fatalf("GenerateWithSuffix: %v", err)
	}

	want := []string{
		"const AEventId = 0",
		"const CEventId = 2",
	}
	if !reflect.DeepEqual(consts, want) {
		t.Fatalf("consts = %q, want %q (tombstone line 1 must produce no constant)", consts, want)
	}
}
