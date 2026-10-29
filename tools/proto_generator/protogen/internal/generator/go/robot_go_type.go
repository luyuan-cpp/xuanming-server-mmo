package _go

import (
	"fmt"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/types/descriptorpb"
)

// robot 生成代码(robot/generated/rpc 调用桩、robot/logic/handler 推送桩)引用 proto 消息的 Go 类型
// (docs/design/client-rpc-callback.md §5.5)。
//
// robot 模块经 go.mod 的 replace proto => ../go/proto 引用 Go 绑定,包目录与 proto 文件目录一一对应:
// proto/friend/friend.proto 的消息在 import "proto/friend" 里。类型名按 protoc-gen-go 的规则从 proto 全名推出。
// 这里只读描述符(FileDescriptorProto),不依赖 internal.ActiveMsgDescCache 等全局缓存,单测可直接注入。

// robotGoType 是一个 proto 消息在 robot 模块里的 Go 类型(未带包限定)。
type robotGoType struct {
	ImportPath string // 例如 proto/friend
	Name       string // 例如 AddFriendRequest;嵌套消息为 Outer_Inner
}

// robotGoTypeEntry 是索引里的一项;File 只用于错误信息。
type robotGoTypeEntry struct {
	robotGoType
	File string
}

// robotGoTypeIndex 以 proto 全名(带前导点,与 MethodDescriptorProto 的 input_type / output_type 同形)为键。
type robotGoTypeIndex map[string]robotGoTypeEntry

// newRobotGoTypeIndex 收录 files 里全部消息(含嵌套消息)。同一全名出现多次时保留第一次(合法描述符集里全名唯一)。
func newRobotGoTypeIndex(files []*descriptorpb.FileDescriptorProto) robotGoTypeIndex {
	index := robotGoTypeIndex{}
	for _, file := range files {
		prefix := "."
		if pkg := file.GetPackage(); pkg != "" {
			prefix = "." + pkg + "."
		}
		importPath := robotGoImportPath(file.GetName())
		var visit func(relative string, messages []*descriptorpb.DescriptorProto)
		visit = func(relative string, messages []*descriptorpb.DescriptorProto) {
			for _, message := range messages {
				name := relative + message.GetName()
				if _, seen := index[prefix+name]; !seen {
					index[prefix+name] = robotGoTypeEntry{
						robotGoType: robotGoType{ImportPath: importPath, Name: goCamelCase(name)},
						File:        file.GetName(),
					}
				}
				visit(name+".", message.GetNestedType())
			}
		}
		visit("", file.GetMessageType())
	}
	return index
}

// resolve 返回 proto 全名对应的 Go 类型。
// 错误:类型不在描述符集里,或所在文件不在 proto/<dir>/ 之下(google/protobuf/*.proto 一类在 robot 模块里没有对应包)。
func (index robotGoTypeIndex) resolve(fullName string) (robotGoType, error) {
	entry, ok := index[fullName]
	if !ok {
		return robotGoType{}, fmt.Errorf("message type %s is not in the descriptor set", fullName)
	}
	if entry.ImportPath == "" {
		return robotGoType{}, fmt.Errorf("message type %s is defined in %s, which has no Go package under proto/ in the robot module",
			fullName, entry.File)
	}
	return entry.robotGoType, nil
}

// robotGoImportPath 把 proto 文件路径换成 robot 模块里的 Go 包路径:proto/friend/friend.proto → proto/friend。
// 描述符集以仓库根为 -I 生成(prototools.buildDescriptorArgs),项目内的文件名都带 proto/ 前缀;不在 proto/<dir>/
// 之下的文件返回空串。与 internal.goProtoImportDir 同一口径(那里返回去掉 proto/ 的目录,模板再拼 "proto/")。
func robotGoImportPath(fileName string) string {
	dir := path.Dir(strings.ReplaceAll(fileName, "\\", "/"))
	if !strings.HasPrefix(dir, "proto/") {
		return ""
	}
	return dir
}

// goCamelCase 与 protoc-gen-go 的 internal/strs.GoCamelCase 逐字节一致:proto 名字(嵌套消息以 . 分隔、
// 相对所在 package)→ Go 标识符。改动前对照 vendor/google.golang.org/protobuf/internal/strs/strings.go。
func goCamelCase(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i+1 < len(s) && isASCIILower(s[i+1]):
			// 跳过 ".<小写>" 里的 '.'。
		case c == '.':
			b = append(b, '_')
		case c == '_' && (i == 0 || s[i-1] == '.'):
			// 开头的 '_'(以及 '.' 之后的 '_')换成 'X',保证首字母大写。
			b = append(b, 'X')
		case c == '_' && i+1 < len(s) && isASCIILower(s[i+1]):
			// 跳过 "_<小写>" 里的 '_'。
		case isASCIIDigit(c):
			b = append(b, c)
		default:
			// 一个词以大写字母开头,后面的小写字母原样跟上。
			if isASCIILower(c) {
				c -= 'a' - 'A'
			}
			b = append(b, c)
			for ; i+1 < len(s) && isASCIILower(s[i+1]); i++ {
				b = append(b, s[i+1])
			}
		}
	}
	return string(b)
}

func isASCIILower(c byte) bool { return 'a' <= c && c <= 'z' }

func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

// robotImportAliases 给一个 Go 文件里用到的 proto 包分配 import 别名(纯函数,结果只取决于输入集合)。
//
// 产物总是写显式别名,不依赖 Go 包声明的名字恰好等于目录末段。按路径升序依次尝试:目录末段 →
// proto/ 之后的整段目录('/' 换成 '_')→ 后者加数字后缀;跳过 Go 关键字、预声明标识符、reserved
// 与已分配的别名。reserved 是产物里已占用的标识符(固定 import 的包名、参数与局部变量名、包级声明),不会被修改。
func robotImportAliases(importPaths []string, reserved map[string]struct{}) map[string]string {
	sorted := append([]string(nil), importPaths...)
	sort.Strings(sorted)

	taken := make(map[string]struct{}, len(reserved)+len(sorted))
	for name := range reserved {
		taken[name] = struct{}{}
	}
	aliases := make(map[string]string, len(sorted))
	for _, importPath := range sorted {
		if _, done := aliases[importPath]; done {
			continue
		}
		alias := claimRobotImportAlias(importPath, taken)
		aliases[importPath] = alias
		taken[alias] = struct{}{}
	}
	return aliases
}

func claimRobotImportAlias(importPath string, taken map[string]struct{}) string {
	dir := strings.TrimPrefix(importPath, "proto/")
	short := robotGoIdentifier(path.Base(dir))
	long := robotGoIdentifier(strings.ReplaceAll(dir, "/", "_"))
	for _, candidate := range []string{short, long} {
		if robotAliasUsable(candidate, taken) {
			return candidate
		}
	}
	for n := 2; ; n++ {
		candidate := long + strconv.Itoa(n)
		if robotAliasUsable(candidate, taken) {
			return candidate
		}
	}
}

func robotAliasUsable(candidate string, taken map[string]struct{}) bool {
	if candidate == "_" || !token.IsIdentifier(candidate) || types.Universe.Lookup(candidate) != nil {
		return false
	}
	_, used := taken[candidate]
	return !used
}

// robotGoIdentifier 把目录名里不能出现在 Go 标识符中的字符换成 '_';以数字开头时补前缀 p。
func robotGoIdentifier(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r):
			b.WriteRune(r)
		case unicode.IsDigit(r):
			if i == 0 {
				b.WriteByte('p')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// lowerFirstRune 把首字符换成小写:ClientPlayerFriend → clientPlayerFriend。
func lowerFirstRune(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// robotGoBuildSuffixes 是 Go 文件名(去掉 .go 后按 '_' 切开的最后一段)里有特殊含义的取值:_test 只进测试构建,
// _<GOOS> / _<GOARCH> 是隐式构建约束(go/build 的 goodOSArchFile)。生成的文件名落在这里会被静默排除出构建,
// 注册表与调用方随之编不过或丢功能,所以生成期直接报错。
var robotGoBuildSuffixes = map[string]struct{}{
	"test": {},
	// GOOS
	"aix": {}, "android": {}, "darwin": {}, "dragonfly": {}, "freebsd": {}, "hurd": {}, "illumos": {}, "ios": {},
	"js": {}, "linux": {}, "nacl": {}, "netbsd": {}, "openbsd": {}, "plan9": {}, "solaris": {}, "wasip1": {},
	"windows": {}, "zos": {},
	// GOARCH
	"386": {}, "amd64": {}, "amd64p32": {}, "arm": {}, "armbe": {}, "arm64": {}, "arm64be": {}, "loong64": {},
	"mips": {}, "mipsle": {}, "mips64": {}, "mips64le": {}, "mips64p32": {}, "mips64p32le": {}, "ppc": {},
	"ppc64": {}, "ppc64le": {}, "riscv": {}, "riscv64": {}, "s390": {}, "s390x": {}, "sparc": {}, "sparc64": {},
	"wasm": {},
}

// checkRobotGoFileName 拒绝会被 Go 工具链特殊对待的生成文件名(见 robotGoBuildSuffixes)。
func checkRobotGoFileName(fileName string) error {
	stem := strings.TrimSuffix(fileName, ".go")
	last := stem[strings.LastIndex(stem, "_")+1:]
	if _, special := robotGoBuildSuffixes[last]; special && last != stem {
		return fmt.Errorf("generated file name %s ends with _%s, which the Go toolchain treats as a test file or a build constraint; rename the service or method",
			fileName, last)
	}
	return nil
}
