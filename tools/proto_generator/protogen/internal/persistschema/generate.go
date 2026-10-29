package persistschema

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/internal/utils"
	"protogen/logger"
)

// Generate 是流水线入口:比对台账、发号,写回台账与 C++ / Go 产物。违反规则时整个生成失败(Fatal),
// 不写任何文件 —— 让违规的 proto 改动在生成期就被拦下,而不是等上线后丢字段。
// 依赖 GenerateAllInOneDescriptor 已把描述符载入 internal.FdSet。
func Generate(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		paths := _config.Global.Paths
		gen, err := run(paths.PersistSchemaLedgerFile, paths.PersistSchemaCppHeaderFile, paths.PersistSchemaGoFile, internal.FdSet.GetFile())
		if err != nil {
			logger.Global.Fatal("Persist schema ledger check failed", zap.Error(err))
		}
		logger.Global.Info("Persist schema generation finished", zap.Uint32("persist_schema_gen", gen))
	}()
}

func run(ledgerPath, cppPath, goPath string, files []*descriptorpb.FileDescriptorProto) (uint32, error) {
	if ledgerPath == "" || cppPath == "" || goPath == "" {
		return 0, errors.New("生成器配置缺少 persist_schema_ledger_file / persist_schema_cpp_header_file / persist_schema_go_file")
	}
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		// 台账不自动新建:缺台账多半是误删,自动重建基线会把所有字段重新记成世代 1,发号历史就没了。
		return 0, fmt.Errorf("读取台账 %s 失败;台账不会自动新建,请从版本库恢复: %w", ledgerPath, err)
	}
	old, err := ParseLedger(data)
	if err != nil {
		return 0, fmt.Errorf("解析台账 %s 失败: %w", ledgerPath, err)
	}
	schema := CollectSchema(files)
	if len(schema.PlayerTables) == 0 {
		return 0, errors.New("描述符里找不到标了 OptionIsPlayerDatabase 的玩家表;描述符未加载或选项读取有误,拒绝改动台账")
	}
	result := Apply(old, schema)
	for _, note := range result.Notes {
		logger.Global.Warn("Persist schema note", zap.String("note", note))
	}
	if len(result.Errors) > 0 {
		return 0, fmt.Errorf("%d 处违反玩家持久字段规则(docs/design/mixed-version-persist-field-protection.md §3.2):\n%s",
			len(result.Errors), strings.Join(result.Errors, "\n"))
	}
	if result.Changed {
		if err := utils.WriteFileIfChanged(ledgerPath, result.Ledger.Format()); err != nil {
			return 0, fmt.Errorf("写回台账 %s 失败: %w", ledgerPath, err)
		}
	}
	retired, err := RetiredFieldsOf(result.Ledger.Retired)
	if err != nil {
		return 0, fmt.Errorf("台账退役行不合法: %w", err)
	}
	if err := utils.WriteFileIfChanged(cppPath, RenderCppHeader(result.Gen, schema, retired)); err != nil {
		return 0, fmt.Errorf("写 C++ 产物 %s 失败: %w", cppPath, err)
	}
	if err := utils.WriteFileIfChanged(goPath, RenderGo(result.Gen)); err != nil {
		return 0, fmt.Errorf("写 Go 产物 %s 失败: %w", goPath, err)
	}
	return result.Gen, nil
}
