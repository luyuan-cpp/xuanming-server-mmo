package internal

import (
	"bufio"
	"os"
	"strings"

	_config "protogen/internal/config"
	"protogen/logger"

	"go.uber.org/zap"
)

// MethodNameFunc generates handler names from method info.
type MethodNameFunc func(info *MethodInfo, funcParam string) string

// CodeSections holds parsed code sections from a CPP file.
type CodeSections struct {
	CodeMap   map[string]string
	FirstCode string
}

// ReadCodeSectionsFromFile parses user-written code sections between markers in a CPP file.
func ReadCodeSectionsFromFile(
	cppFileName string,
	methods *RPCMethods,
	methodFunc MethodNameFunc,
	funcParam string,
) (map[string]string, string, error) {
	codeMap := make(map[string]string)

	fd, err := os.Open(cppFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return buildDefaultCodeMap(methods, methodFunc, funcParam), _config.Global.Naming.YourCodePair, nil
		}
		logger.Global.Error("Failed to open CPP file",
			zap.String("file_name", cppFileName),
			zap.Error(err),
		)
		return codeMap, "", err
	}
	defer fd.Close()

	scanner := bufio.NewScanner(fd)
	parser := newCodeParser(methods, methodFunc, funcParam)
	result := parser.parse(scanner)

	if err := scanner.Err(); err != nil {
		logger.Global.Error("Error reading CPP file",
			zap.String("file_name", cppFileName),
			zap.Error(err),
		)
		return result.CodeMap, result.FirstCode, err
	}

	// Fill missing methods with default code.
	fillMissingMethods(result.CodeMap, methods, methodFunc, funcParam)
	return result.CodeMap, result.FirstCode, nil
}

// codeParser is a stateful parser for extracting user code sections.
type codeParser struct {
	methods    *RPCMethods
	methodFunc MethodNameFunc
	funcParam  string

	codeMap       map[string]string
	firstCode     string
	currentCode   string
	currentMethod *MethodInfo
	inFirstCode   bool
	inMethodCode  bool
	isFirstCode   bool
	hasSeenMethod bool
}

func newCodeParser(methods *RPCMethods, methodFunc MethodNameFunc, funcParam string) *codeParser {
	return &codeParser{
		methods:    methods,
		methodFunc: methodFunc,
		funcParam:  funcParam,
		codeMap:    make(map[string]string),
	}
}

func (p *codeParser) parse(scanner *bufio.Scanner) CodeSections {
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		p.processLine(line)
	}

	if p.firstCode == "" {
		p.firstCode = _config.Global.Naming.YourCodePair
	}

	return CodeSections{
		CodeMap:   p.codeMap,
		FirstCode: p.firstCode,
	}
}

func (p *codeParser) processLine(line string) {
	if p.handleGlobalCode(line) {
		return
	}
	p.handleMethodCode(line)
}

func (p *codeParser) handleGlobalCode(line string) bool {
	if p.inFirstCode && strings.Contains(line, _config.Global.Naming.YourCodeEnd) {
		p.firstCode += line
		p.inFirstCode = false
		return true
	}
	if p.inFirstCode {
		p.firstCode += line
		return true
	}
	// Global preserved-code block is only valid before any method signature appears.
	if !p.isFirstCode && !p.hasSeenMethod && strings.Contains(line, _config.Global.Naming.YourCodeBegin) {
		p.firstCode = line
		p.inFirstCode = true
		p.isFirstCode = true
		return true
	}
	return false
}

func (p *codeParser) handleMethodCode(line string) {
	// 生成函数的结束大括号固定顶格。旧包装函数没有守护段时也必须在此结束匹配,
	// 否则下一个函数的守护段会被归给当前方法;守护段内的自定义代码不参与边界识别。
	if !p.inMethodCode && strings.TrimRight(line, "\r\n") == "}" {
		p.currentMethod = nil
		return
	}
	if p.currentMethod == nil {
		for _, method := range *p.methods {
			handlerName := p.methodFunc(method, p.funcParam)
			if strings.Contains(line, handlerName) {
				p.currentMethod = method
				p.hasSeenMethod = true
				break
			}
		}
		return
	}
	if strings.Contains(line, _config.Global.Naming.YourCodeBegin) {
		p.inMethodCode = true
		p.currentCode += line
		return
	}
	if strings.Contains(line, _config.Global.Naming.YourCodeEnd) {
		p.currentCode += line
		handlerName := p.methodFunc(p.currentMethod, p.funcParam)
		p.codeMap[handlerName] = p.currentCode
		p.currentMethod = nil
		p.currentCode = ""
		p.inMethodCode = false
		return
	}
	if p.inMethodCode {
		p.currentCode += line
	}
}

func buildDefaultCodeMap(methods *RPCMethods, methodFunc MethodNameFunc, funcParam string) map[string]string {
	codeMap := make(map[string]string)
	for _, method := range *methods {
		handlerName := methodFunc(method, funcParam)
		codeMap[handlerName] = _config.Global.Naming.YourCodePair
	}
	return codeMap
}

func fillMissingMethods(codeMap map[string]string, methods *RPCMethods, methodFunc MethodNameFunc, funcParam string) {
	for _, method := range *methods {
		handlerName := methodFunc(method, funcParam)
		if _, exists := codeMap[handlerName]; !exists {
			codeMap[handlerName] = _config.Global.Naming.YourCodePair
		}
	}
}
