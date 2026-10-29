package _go

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"text/template"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

// robotHandlerTotalTemplatePath 相对 protogen 根目录(生成器的工作目录)。
const robotHandlerTotalTemplatePath = "internal/template/robot_handler_total.go.tmpl"

type HandlerCase struct {
	MessageID    string
	FunctionCall string
}

type CasesData struct {
	Cases []HandlerCase
}

// GoRobotTotalHandlerGenerator 重写 robot/logic/handler/message_body_handler.go:id = 0 的消息(推送,以及迁移前
// 旧场景用 SendRequest 发出的请求的回包)按 message_id 派发的注册表(docs/design/client-rpc-callback.md §5.5)。
// 登记规则见 robotHandlerCases;判定 handler 文件是否存在只看磁盘(robotHandlerFileExists),理由同那里的注释。
// handler 文件名会被 Go 工具链特殊对待、stat 失败或路径不是普通文件都 Fatal,不写注册表。
func GoRobotTotalHandlerGenerator(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		services, err := internal.ClassifyClientRpcServices(internal.GlobalRPCServiceList, internal.IsSceneTCPMethod)
		if err != nil {
			logger.Global.Fatal("Client RPC method kinds failed validation", zap.Error(err))
			return
		}

		handlerDir := _config.Global.PathLists.MethodHandlerDirectories.Robot
		handlerCases, err := robotHandlerCases(services, _config.Global.Naming.MessageId, func(fileName string) (bool, error) {
			return robotHandlerFileExists(filepath.Join(handlerDir, fileName))
		})
		if err != nil {
			logger.Global.Fatal("Failed to compute robot message handler registry",
				zap.String("dir", handlerDir),
				zap.Error(err),
			)
			return
		}

		logger.Global.Info("Generating robot message handler file",
			zap.Int("handler_case_count", len(handlerCases)),
		)

		err = generateTotalHandlerFile(_config.Global.Paths.RobotMsgBodyHandlerFile, handlerCases, robotHandlerTotalTemplatePath)
		if err != nil {
			logger.Global.Fatal("生成机器人消息处理器文件失败",
				zap.Error(err),
			)
			return
		}

		logger.Global.Info("Robot message handler file generated",
			zap.String("file_path", _config.Global.Paths.RobotMsgBodyHandlerFile),
			zap.Int("handler_case_count", len(handlerCases)),
		)
	}()
}

// robotHandlerCases 计算注册表条目(纯函数)。服务与方法保持分类结果的顺序(GlobalRPCServiceList 顺序、proto 声明顺序),
// 所以已有条目的相对次序不变。客户端协议服务只按 OptionIsClientProtocolService 判定。
//
// 登记规则:
//   - CLIENT_RPC_PUSH:一律登记。GoRobotHandlerGenerator 在同一轮里保证桩存在(缺了就建,失败即 Fatal)。
//     两者在同一并行组里跑,这里不能去磁盘看那个 goroutine 是否已写出桩,所以按「本轮一定存在」处理。
//   - REQUEST / ONE_WAY / SERVER_ONLY:只在 handler 文件(robotHandlerFileName)存在时登记。生成器不再为它们建桩,
//     注册表引用不存在的函数会编不过;已有的手写 handler 与旧桩带着副作用(登录握手、压测主循环、AI 循环、
//     robot 场景的 callDispatch 都靠它们),照常登记,id = 0 路径的行为不变。本轮没有任何生成器写这类文件,
//     所以 handlerFileExists 在生成期间任何时刻的答案都等于生成开始时磁盘上的状态。
//     反过来,删掉旧的 REQUEST / ONE_WAY / SERVER_ONLY handler 文件后,必须先重跑 proto 生成(注册表随之去掉该条)
//     再编译 robot,否则注册表引用已删除的函数。
//
// handlerFileExists 收 handler 文件名(不含目录),回答它是否作为普通文件存在;生产传 robotHandlerFileExists。
// 错误(收集全部后返回,结果为 nil):handlerFileExists 出错(stat 失败、路径不是普通文件)——无法确认的文件
// 既不能当作存在而登记,也不能当作不存在而漏登;或者非 PUSH 方法的 handler 文件**存在**、但文件名会被 Go 工具链
// 特殊对待(checkRobotGoFileName:_test.go 只进测试构建,_<GOOS> / _<GOARCH> 是隐式构建约束),登记进去正式构建
// 就引用不到这个函数。文件不存在时不查文件名:这类方法已不建桩、本来就不登记,不能因为方法名恰好以 Test / Windows
// 之类结尾就让整次生成失败(PUSH 仍按 ensureRobotPushHandlers 的规则一律校验,因为它的桩一定会被建出来)。
func robotHandlerCases(services []internal.ClientRpcService, messageIDSuffix string,
	handlerFileExists func(fileName string) (bool, error)) ([]HandlerCase, error) {
	cases := make([]HandlerCase, 0)
	var errs []error
	for _, service := range services {
		for _, classified := range service.Methods {
			method := classified.Method
			key := method.KeyName()
			if classified.Kind != internal.ClientRpcPush {
				where := method.Service() + "." + method.Method()
				fileName := robotHandlerFileName(key)
				exists, err := handlerFileExists(fileName)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", where, err))
					continue
				}
				if !exists {
					continue
				}
				if err := checkRobotGoFileName(fileName); err != nil {
					errs = append(errs, fmt.Errorf("%s: existing robot handler file would be left out of a normal build, so the registry cannot reference it: %w",
						where, err))
					continue
				}
			}
			cases = append(cases, HandlerCase{
				MessageID:    key + messageIDSuffix,
				FunctionCall: key + "Handler",
			})
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cases, nil
}

// generateTotalHandlerFile 用 templatePath 渲染注册表并写入 fileName(内容不变不落盘)。
func generateTotalHandlerFile(fileName string, cases []HandlerCase, templatePath string) error {
	if fileName == "" {
		return fmt.Errorf("robot message handler file path is empty")
	}

	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return fmt.Errorf("parse template %s: %w", templatePath, err)
	}

	var content bytes.Buffer
	if err := tmpl.Execute(&content, CasesData{Cases: cases}); err != nil {
		return fmt.Errorf("render %s: %w", fileName, err)
	}
	if err := utils.WriteFileIfChanged(fileName, content.Bytes()); err != nil {
		return err
	}

	logger.Global.Debug("Handler template executed",
		zap.String("file_name", fileName),
		zap.Int("case_count", len(cases)),
	)
	return nil
}
