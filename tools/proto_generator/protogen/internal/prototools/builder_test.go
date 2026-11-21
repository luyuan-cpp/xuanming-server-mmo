package prototools

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	_config "protogen/internal/config"
	"protogen/logger"

	"go.uber.org/zap"
)

func TestCopyProtoToGenDirRefreshesRobotLiveContractWithoutClearingUnrelatedFiles(t *testing.T) {
	previousConfig := _config.Global
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() {
		_config.Global = previousConfig
		logger.Global = previousLogger
	})

	root := t.TempDir()
	sourceDir := filepath.Join(root, "proto")
	stagingDir := filepath.Join(root, "generated", "proto")
	robotDir := filepath.Join(root, "generated", "robot", "proto")
	commonRelative := filepath.Join("common", "base", "common.proto")
	const currentProto = "syntax = \"proto3\";\nmessage NodeInfo { uint32 node_id = 1; }\n"
	const staleProto = currentProto + "message NodeInfoListComp { repeated NodeInfo nodes = 1; }\n"
	const keepProto = "syntax = \"proto3\";\nmessage KeepExistingRobotContract {}\n"
	writeFixture := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	readFixture := func(path string) string {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	writeFixture(filepath.Join(sourceDir, commonRelative), currentProto)
	writeFixture(filepath.Join(robotDir, commonRelative), staleProto)
	writeFixture(filepath.Join(robotDir, "keep.proto"), keepProto)

	_config.Global.Paths.ProtoDir = sourceDir
	// 既有复制入口按前缀拼接 db/login 相对路径，保留它要求的目录分隔符。
	_config.Global.Paths.GeneratorProtoDir = stagingDir + string(os.PathSeparator)
	_config.Global.Paths.RobotGeneratedProto = robotDir
	_config.Global.DirectoryNames.NormalGoProto = "proto"
	_config.Global.Generators.EnableRobotProto = true
	var wg sync.WaitGroup
	CopyProtoToGenDir(&wg)
	wg.Wait()

	if actual := readFixture(filepath.Join(robotDir, commonRelative)); actual != currentProto {
		t.Errorf("robot 活协议未从当前源刷新：got=%q，want=%q", actual, currentProto)
	}
	if actual := readFixture(filepath.Join(robotDir, "keep.proto")); actual != keepProto {
		t.Errorf("复制不应清理无关的目标文件：got=%q，want=%q", actual, keepProto)
	}
	for _, service := range []string{"db", "login"} {
		path := filepath.Join(stagingDir, service)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("goctl 退役后不应再为 %s 生成暂存目录：%s (err=%v)", service, path, err)
		}
	}
}
