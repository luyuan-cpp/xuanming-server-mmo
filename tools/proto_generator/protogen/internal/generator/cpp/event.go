package cpp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"go.uber.org/zap"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"
)

// generateClassNameFromFile generates a C++ class name from a proto file name (underscore to upper camel case) with a suffix.
func generateClassNameFromFile(file os.DirEntry, suffix string) string {
	return generateClassNameFromProtoName(file.Name(), suffix)
}

func generateClassNameFromProtoName(name, suffix string) string {
	baseName := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.Split(baseName, "_")
	title := cases.Title(language.English)

	var classParts []string
	for _, part := range parts {
		if part != "" {
			classParts = append(classParts, title.String(part))
		}
	}
	if len(classParts) == 0 {
		return "Default" + suffix
	}
	return strings.Join(classParts, "") + suffix
}

// parseProtoMessages extracts all message names from a proto file.
func parseProtoMessages(protoFilePath string) ([]string, error) {
	file, err := os.Open(protoFilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var messages []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "message ") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				messages = append(messages, parts[1])
			}
		}
	}
	return messages, scanner.Err()
}

func parseProtoPackage(protoFilePath string) (string, error) {
	file, err := os.Open(protoFilePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "package ") {
			continue
		}

		packageName := strings.TrimPrefix(line, "package ")
		packageName = strings.TrimSuffix(packageName, ";")
		return strings.TrimSpace(packageName), nil
	}

	return "", scanner.Err()
}

func qualifyProtoType(packageName, typeName string) string {
	if packageName == "" {
		return typeName
	}

	parts := strings.Split(packageName, ".")
	return strings.Join(parts, "::") + "::" + typeName
}

// buildEventHandlerMethodName builds a safe event handler method name.
func buildEventHandlerMethodName(className, eventName string) string {
	methodName := eventName + "Handler"
	if methodName == className {
		return "Handle" + eventName
	}
	return methodName
}

// buildEventHandlerSignature builds the definition signature for an event handler function.
func buildEventHandlerSignature(className, qualifiedTypeName, eventName string) string {
	return fmt.Sprintf("void %s::%s(const %s& event)\n", className, buildEventHandlerMethodName(className, eventName), qualifiedTypeName)
}

// extractUserCodeBlocks extracts user-written code blocks from each event handler function in a .cpp file.
//
// 与 handler 生成器共用 internal.ReadCodeSectionsByKeys 的取段规则(internal/code_parser.go 的 codeParser):
// 守护段内不认查找键、函数体内不认签名、以 ";" 结尾的声明不进入方法、不归任何方法的段整段跳过。
// 修复前这里每一行都匹配签名 —— 守护段里出现某个事件 handler 的签名字面量(注释、手写的转调)会把解析拉进那个方法,
// 随后的结束标记把它已取到的代码覆盖成只剩结束标记;FirstCode 缺失时第一个方法段会被当成全局段。
// 结构损坏或带不走的非空段返回 error(拒绝生成,不吞代码);文件不存在时全部取默认空段。
// 返回的 codeMap 以 methodSignatures 原样(含行尾换行)为键,值去掉行尾换行,供模板按 .Signature 取用。
func extractUserCodeBlocks(cppPath string, methodSignatures []string) (map[string]string, string, error) {
	// 签名带行尾换行(buildEventHandlerSignature);查找键去掉它,在签名起始行上逐字匹配,CRLF 文件同样认得出。
	keys := make([]string, 0, len(methodSignatures))
	for _, signature := range methodSignatures {
		keys = append(keys, strings.TrimRight(signature, "\r\n"))
	}
	sections, err := internal.ReadCodeSectionsByKeys(cppPath, keys)
	if err != nil {
		return nil, "", err
	}
	if err := internal.RefuseLostCodeSections(cppPath, sections); err != nil {
		return nil, "", err
	}

	codeMap := make(map[string]string, len(methodSignatures))
	for i, signature := range methodSignatures {
		codeMap[signature] = utils2.TrimTrailingLineBreaks(sections.CodeMap[keys[i]])
	}
	return codeMap, utils2.TrimTrailingLineBreaks(sections.FirstCode), nil
}

// EventTemplateData holds data for rendering templates.
type EventTemplateData struct {
	ClassName      string
	HeaderFile     string
	ProtoInclude   string
	EventMessages  []EventHandlerMessage
	GlobalUserCode string
	UserCodeBlocks map[string]string
}

type EventHandlerMessage struct {
	Name          string
	HandlerName   string
	QualifiedName string
	Signature     string
	IsLast        bool
}

// Kafka handler 只接受显式入口事件,顺序沿用 proto,不再为内部消息生成空 handler。
func kafkaIngressMessages(protoRelativeDir string, file os.DirEntry, messages []string) []string {
	protoInclude := filepath.ToSlash(filepath.Join(_config.Global.DirectoryNames.ProtoDirName, protoRelativeDir,
		strings.Replace(file.Name(), _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbH, 1)))
	allowed := make(map[string]struct{})
	for _, event := range globalProtoEventList {
		if event.ProtoInclude == protoInclude && event.Ingress != eventIngressNone {
			allowed[event.MessageName] = struct{}{}
		}
	}
	var result []string
	for _, message := range messages {
		if _, ok := allowed[message]; ok {
			result = append(result, message)
		}
	}
	return result
}

func filterKafkaIngressFiles(protoRelativeDir string, files []os.DirEntry) []os.DirEntry {
	var result []os.DirEntry
	for _, file := range files {
		messages, err := parseProtoMessages(filepath.Join(_config.Global.Paths.ProtoDir, protoRelativeDir, file.Name()))
		if err != nil {
			logger.Global.Fatal("Failed to read Kafka ingress proto", zap.String("file", file.Name()), zap.Error(err))
		}
		if len(kafkaIngressMessages(protoRelativeDir, file, messages)) > 0 {
			result = append(result, file)
		}
	}
	return result
}

// generateEventHandlerFiles generates .h and .cpp files for each proto using templates.
func generateEventHandlerFiles(wg *sync.WaitGroup, file os.DirEntry, protoRelativeDir string, outputDir string) {
	defer wg.Done()

	protoFilePath := filepath.Join(_config.Global.Paths.ProtoDir, protoRelativeDir, file.Name())
	eventMessages, err := parseProtoMessages(protoFilePath)
	if err != nil {
		logger.Global.Error("Failed to parse proto file",
			zap.String("proto_file", protoFilePath),
			zap.Error(err),
		)
		return
	}
	eventMessages = kafkaIngressMessages(protoRelativeDir, file, eventMessages)
	if len(eventMessages) == 0 {
		return
	}

	packageName, err := parseProtoPackage(protoFilePath)
	if err != nil {
		logger.Global.Warn("Failed to parse proto package, treating as no namespace",
			zap.String("proto_file", protoFilePath),
			zap.Error(err),
		)
		packageName = ""
	}

	className := generateClassNameFromFile(file, _config.Global.Naming.HandlerFile)
	baseName := strings.ToLower(strings.TrimSuffix(file.Name(), _config.Global.FileExtensions.Proto))
	filePrefix := filepath.Join(outputDir, baseName)

	headerFilePath := filePrefix + _config.Global.FileExtensions.HandlerH
	cppFilePath := filePrefix + _config.Global.FileExtensions.HandlerCpp
	headerFileBase := filepath.Base(headerFilePath)

	var handlerSignatures []string
	for _, evt := range eventMessages {
		handlerSignatures = append(handlerSignatures, buildEventHandlerSignature(className, qualifyProtoType(packageName, evt), evt))
	}

	userCodeBlocks, globalCode, err := extractUserCodeBlocks(cppFilePath, handlerSignatures)
	if err != nil {
		// 读失败或守护段带不走时继续渲染,会把自定义代码覆盖成空段:拒绝生成(与 handler 生成器同口径)。
		logger.Global.Fatal("Refusing to regenerate event handler: user code sections cannot be preserved",
			zap.String("cpp_file", cppFilePath),
			zap.Error(err),
		)
	}

	eventHandlers := make([]EventHandlerMessage, 0, len(eventMessages))
	for index, eventName := range eventMessages {
		qualifiedName := qualifyProtoType(packageName, eventName)
		eventHandlers = append(eventHandlers, EventHandlerMessage{
			Name:          eventName,
			HandlerName:   buildEventHandlerMethodName(className, eventName),
			QualifiedName: qualifiedName,
			Signature:     buildEventHandlerSignature(className, qualifiedName, eventName),
			IsLast:        index == len(eventMessages)-1,
		})
	}

	tmplData := EventTemplateData{
		ClassName:      className,
		HeaderFile:     headerFileBase,
		ProtoInclude:   filepath.ToSlash(filepath.Join(_config.Global.DirectoryNames.ProtoDirName, protoRelativeDir, strings.Replace(file.Name(), _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbH, 1))),
		EventMessages:  eventHandlers,
		GlobalUserCode: globalCode,
		UserCodeBlocks: userCodeBlocks,
	}

	if err := utils2.RenderTemplateToFile("internal/template/event_handler.h.tmpl", headerFilePath, tmplData); err != nil {
		logger.Global.Fatal("Failed to generate event handler header",
			zap.String("header_file", headerFilePath),
			zap.Error(err),
		)
	}
	if err := utils2.RenderTemplateToFile("internal/template/event_handler.cpp.tmpl", cppFilePath, tmplData); err != nil {
		logger.Global.Fatal("Failed to generate event handler source",
			zap.String("cpp_file", cppFilePath),
			zap.Error(err),
		)
	}
}

func filterProtoFiles(files []os.DirEntry) []os.DirEntry {
	filtered := make([]os.DirEntry, 0, len(files))
	for _, file := range files {
		if utils2.IsProtoFile(file) {
			filtered = append(filtered, file)
		}
	}
	return filtered
}

func filterProtoFilesBySuffix(files []os.DirEntry, suffix string) []os.DirEntry {
	filtered := make([]os.DirEntry, 0, len(files))
	for _, file := range files {
		if !utils2.IsProtoFile(file) {
			continue
		}
		if !strings.HasSuffix(file.Name(), suffix) {
			continue
		}
		filtered = append(filtered, file)
	}
	return filtered
}

func readProtoFiles(relativeDir string) ([]os.DirEntry, error) {
	return os.ReadDir(filepath.Join(_config.Global.Paths.ProtoDir, relativeDir))
}

// GenerateAllEventHandlers generates all event handlers.
func GenerateAllEventHandlers(wg *sync.WaitGroup) {
	logicEventFiles, err := readProtoFiles(_config.Global.PathLists.ProtoDirs.LogicEvent)
	if err != nil {
		logger.Global.Fatal("Failed to read proto directory",
			zap.String("dir", _config.Global.PathLists.ProtoDirs.LogicEvent),
			zap.Error(err),
		)
	}
	logicEventFiles = filterKafkaIngressFiles(_config.Global.PathLists.ProtoDirs.LogicEvent, filterProtoFiles(logicEventFiles))

	for _, file := range logicEventFiles {
		wg.Add(1)
		go generateEventHandlerFiles(wg, file, _config.Global.PathLists.ProtoDirs.LogicEvent, _config.Global.Paths.SceneNodeEventHandlerDirectory)
	}

	kafkaContractFiles := []os.DirEntry{}
	if _config.Global.PathLists.ProtoDirs.ContractsKafka != "" {
		files, readErr := readProtoFiles(_config.Global.PathLists.ProtoDirs.ContractsKafka)
		if readErr != nil {
			logger.Global.Warn("Failed to read Kafka proto directory, skipping Gate event handler generation",
				zap.String("dir", _config.Global.PathLists.ProtoDirs.ContractsKafka),
				zap.Error(readErr),
			)
		} else {
			kafkaContractFiles = filterKafkaIngressFiles(_config.Global.PathLists.ProtoDirs.ContractsKafka, filterProtoFilesBySuffix(files, "_event.proto"))
			for _, file := range kafkaContractFiles {
				wg.Add(1)
				go generateEventHandlerFiles(wg, file, _config.Global.PathLists.ProtoDirs.ContractsKafka, _config.Global.Paths.GateNodeEventHandlerDirectory)
			}
		}
	}

}

type Config struct {
	ProtoDirs                      []string
	EventProtoDirIndex             int
	ProtoEx                        string
	HandlerHeaderExtension         string
	EventHandlerHeaderFileName     string
	EventHandlerCppFileName        string
	SceneNodeEventHandlerDirectory string
	GateNodeEventHandlerDirectory  string
	IncludeBegin                   string
	IncludeEndLine                 string
	ClassNameSuffix                string
}
