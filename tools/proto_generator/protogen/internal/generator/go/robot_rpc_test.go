package _go

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"protogen/internal"
	"protogen/logger"

	messageoption "github.com/luyuancpp/protooption"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 内存描述符沿正式分类(internal.ClassifyClientRpcServices)与生成入口流转:不调用 protoc,不读真实 proto 目录、
// _config.Global 与工作目录,只写 t.TempDir()。模板路径相对本包目录。

// robotFixtureMethod 描述夹具里的一个方法;input / output 是 proto 全名(带前导点)。
type robotFixtureMethod struct {
	name, input, output string
	kind                internal.ClientRpcKind
}

// robotFixtureFiles 是夹具的描述符集:业务包 proto/test、两个目录末段同为 shared 的包(用来撞 import 别名),
// 以及 Empty 所在、无 package 的 proto/common/base。
func robotFixtureFiles() []*descriptorpb.FileDescriptorProto {
	return []*descriptorpb.FileDescriptorProto{
		{
			Name: proto.String("proto/test/client.proto"), Package: proto.String("fixturepb"),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Request")}, {Name: proto.String("Response")}, {Name: proto.String("Changed")},
				{Name: proto.String("Outer"), NestedType: []*descriptorpb.DescriptorProto{{Name: proto.String("Inner")}}},
			},
		},
		{
			Name: proto.String("proto/other/shared/types.proto"), Package: proto.String("otherpb"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Reply")}},
		},
		{
			Name: proto.String("proto/alt/shared/types.proto"), Package: proto.String("altpb"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Reply")}},
		},
		{
			Name:        proto.String("proto/common/base/empty.proto"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		},
		{
			Name: proto.String("google/protobuf/duration.proto"), Package: proto.String("google.protobuf"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Duration")}},
		},
	}
}

// robotFixtureService 在 file 里构造一个标了 OptionIsClientProtocolService 的服务;消息号从 firstID 起按声明顺序分配。
// 外部模块 protooption 的绑定里没有 OptionClientRpcKind,生产描述符里它就在未知字段里,夹具照样写。
func robotFixtureService(file *descriptorpb.FileDescriptorProto, name string, firstID uint64,
	methods ...robotFixtureMethod) *internal.RPCServiceInfo {
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String(name), Options: &descriptorpb.ServiceOptions{}}
	proto.SetExtension(service.Options, messageoption.E_OptionIsClientProtocolService, true)
	file.Service = append(file.Service, service)
	info := &internal.RPCServiceInfo{ProtoFileInfo: internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}}
	for index, spec := range methods {
		method := &descriptorpb.MethodDescriptorProto{
			Name: proto.String(spec.name), InputType: proto.String(spec.input), OutputType: proto.String(spec.output),
			Options: &descriptorpb.MethodOptions{},
		}
		method.Options.ProtoReflect().SetUnknown(protowire.AppendVarint(
			protowire.AppendTag(nil, internal.OptionClientRpcKindNumber, protowire.VarintType), uint64(spec.kind)))
		service.Method = append(service.Method, method)
		info.Methods = append(info.Methods, &internal.MethodInfo{
			ProtoFileInfo: info.ProtoFileInfo, MethodDescriptorProto: method, Id: firstID + uint64(index),
		})
	}
	return info
}

// robotFixtureUnmarkedService 构造一个没标 OptionIsClientProtocolService、名字却含 ClientPlayer 的服务(不得生成)。
func robotFixtureUnmarkedService(file *descriptorpb.FileDescriptorProto, name string, id uint64) *internal.RPCServiceInfo {
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String(name)}
	method := &descriptorpb.MethodDescriptorProto{
		Name: proto.String("Read"), InputType: proto.String(".fixturepb.Request"), OutputType: proto.String(".fixturepb.Response"),
	}
	service.Method = []*descriptorpb.MethodDescriptorProto{method}
	file.Service = append(file.Service, service)
	info := internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}
	return &internal.RPCServiceInfo{
		ProtoFileInfo: info,
		Methods:       internal.RPCMethods{{ProtoFileInfo: info, MethodDescriptorProto: method, Id: id}},
	}
}

// robotFixture 返回夹具描述符集与覆盖四种形态的 GuildService(名字不含 ClientPlayer,只靠 option 入选)。
func robotFixture() ([]*descriptorpb.FileDescriptorProto, *internal.RPCServiceInfo) {
	files := robotFixtureFiles()
	guild := robotFixtureService(files[0], "GuildService", 1,
		robotFixtureMethod{"Read", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest},
		robotFixtureMethod{"Move", ".fixturepb.Request", ".Empty", internal.ClientRpcOneWay},
		robotFixtureMethod{"NotifyChanged", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"Disconnect", ".fixturepb.Request", ".Empty", internal.ClientRpcServerOnly},
		robotFixtureMethod{"CancelQueue", ".fixturepb.Request", ".Empty", internal.ClientRpcRequest},
		robotFixtureMethod{"Fetch", ".fixturepb.Outer.Inner", ".otherpb.Reply", internal.ClientRpcRequest},
		robotFixtureMethod{"Peek", ".fixturepb.Request", ".altpb.Reply", internal.ClientRpcRequest},
	)
	return files, guild
}

// robotFixtureClassify 走正式分类;夹具里以 Move 开头的方法走 scene TCP(ONE_WAY 只在那条路径上合法)。
func robotFixtureClassify(t *testing.T, services ...*internal.RPCServiceInfo) []internal.ClientRpcService {
	t.Helper()
	classified, err := internal.ClassifyClientRpcServices(internal.RPCServiceInfoList(services), func(method *internal.MethodInfo) bool {
		return strings.HasPrefix(method.Method(), "Move")
	})
	if err != nil {
		t.Fatalf("夹具必须通过方法形态校验: %v", err)
	}
	return classified
}

func robotFixtureGenerate(t *testing.T, files []*descriptorpb.FileDescriptorProto, dir string,
	services ...*internal.RPCServiceInfo) (int, error) {
	t.Helper()
	return generateRobotRpc(robotFixtureClassify(t, services...), files, dir, robotTemplate("robot_rpc.go.tmpl"), "MessageId")
}

func robotTemplate(name string) string {
	return filepath.Join("..", "..", "template", name)
}

func robotFixtureLogger(t *testing.T) {
	t.Helper()
	previous := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previous })
}

func robotReadFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func robotWriteFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// robotAssertGoSource 断言产物是 gofmt 规范格式的合法 Go 源码,且每个 import 都被引用(未使用的 import 编不过)。
func robotAssertGoSource(t *testing.T, name, content string) {
	t.Helper()
	formatted, err := format.Source([]byte(content))
	if err != nil {
		t.Fatalf("%s 不是合法 Go 源码: %v\n%s", name, err, content)
	}
	if !bytes.Equal(formatted, []byte(content)) {
		t.Errorf("%s 不是 gofmt 规范格式;gofmt 之后:\n%s", name, formatted)
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, content, 0)
	if err != nil {
		t.Fatal(err)
	}
	qualifiers := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok {
				qualifiers[ident.Name] = true
			}
		}
		return true
	})
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		localName := path.Base(importPath)
		if spec.Name != nil {
			localName = spec.Name.Name
		}
		if !qualifiers[localName] {
			t.Errorf("%s: import %s (%s) is never referenced", name, importPath, localName)
		}
	}
}

func TestRobotRpcGeneratesRequestAndOneWayStubsOnly(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	dir := t.TempDir()
	count, err := robotFixtureGenerate(t, files, dir, guild)
	if err != nil || count != 1 {
		t.Fatalf("预期只生成一个服务文件: count=%d err=%v", count, err)
	}
	content := robotReadFile(t, filepath.Join(dir, "guild_service.go"))
	robotAssertGoSource(t, "guild_service.go", content)

	if !strings.HasPrefix(content, "// Code generated by protogen. DO NOT EDIT.\n// Source: proto/test/client.proto\n\npackage rpc\n") {
		t.Errorf("文件头必须是 Go 标准生成标记 + 来源行 + package rpc:\n%s", content)
	}
	for _, expected := range []string{
		"var GuildService guildServiceRpc\n",
		"type guildServiceRpc struct{}\n",
		// REQUEST 异步:走 Call,结局交给 decode 后恰好一次回调 done。
		"func (r guildServiceRpc) Read(c *pkg.GameClient, req *test.Request, timeout time.Duration, done func(*test.Response, error)) {\n" +
			"\tc.Call(game.GuildServiceReadMessageId, req, timeout, func(reply *base.MessageContent, err error) {\n" +
			"\t\tdone(r.decodeRead(reply, err))\n" +
			"\t})\n}\n",
		// REQUEST 同步:走 CallSync,同形返回。
		"func (r guildServiceRpc) ReadSync(ctx context.Context, c *pkg.GameClient, req *test.Request, timeout time.Duration) (*test.Response, error) {\n" +
			"\treturn r.decodeRead(c.CallSync(ctx, game.GuildServiceReadMessageId, req, timeout))\n}\n",
		// 解码:信封错误与其余错误原样传出;解码失败包裹服务.方法与原错误。
		"func (guildServiceRpc) decodeRead(reply *base.MessageContent, err error) (*test.Response, error) {\n" +
			"\tif err != nil {\n\t\treturn nil, err\n\t}\n" +
			"\tresp := &test.Response{}\n" +
			"\tif err = proto.Unmarshal(reply.GetSerializedMessage(), resp); err != nil {\n" +
			"\t\treturn nil, fmt.Errorf(\"rpc: decode GuildService.Read reply: %w\", err)\n" +
			"\t}\n\treturn resp, nil\n}\n",
		// ONE_WAY:只发,id = 0。
		"func (guildServiceRpc) Move(c *pkg.GameClient, req *test.Request) error {\n" +
			"\treturn c.SendRequest(game.GuildServiceMoveMessageId, req)\n}\n",
		// gRPC 路径上的 Empty 响应照常回包,仍是 REQUEST。
		"func (r guildServiceRpc) CancelQueue(c *pkg.GameClient, req *test.Request, timeout time.Duration, done func(*base.Empty, error)) {\n",
		// 嵌套消息按 protoc-gen-go 命名;其它包的类型带各自别名。
		"func (r guildServiceRpc) Fetch(c *pkg.GameClient, req *test.Outer_Inner, timeout time.Duration, done func(*other_shared.Reply, error)) {\n",
		"func (r guildServiceRpc) Peek(c *pkg.GameClient, req *test.Request, timeout time.Duration, done func(*shared.Reply, error)) {\n",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("缺少契约输出:\n%s", expected)
		}
	}
	for _, unexpected := range []string{"MoveSync", "decodeMove", "NotifyChanged", ") Disconnect(", "GuildServiceDisconnectMessageId"} {
		if strings.Contains(content, unexpected) {
			t.Errorf("ONE_WAY 不得有同步版,PUSH 与 SERVER_ONLY 不得生成调用函数: 出现了 %q", unexpected)
		}
	}
}

func TestRobotRpcDeduplicatesImportsAndResolvesAliasCollisions(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	dir := t.TempDir()
	if _, err := robotFixtureGenerate(t, files, dir, guild); err != nil {
		t.Fatal(err)
	}
	content := robotReadFile(t, filepath.Join(dir, "guild_service.go"))
	// 路径升序;proto/alt/shared 先占 shared,proto/other/shared 退到整段目录;proto/test 被多个方法引用只 import 一次。
	want := "import (\n" +
		"\t\"context\"\n\t\"fmt\"\n\t\"time\"\n\n" +
		"\t\"google.golang.org/protobuf/proto\"\n\n" +
		"\tshared \"proto/alt/shared\"\n" +
		"\tbase \"proto/common/base\"\n" +
		"\tother_shared \"proto/other/shared\"\n" +
		"\ttest \"proto/test\"\n" +
		"\t\"robot/generated/pb/game\"\n" +
		"\t\"robot/pkg\"\n" +
		")\n"
	if !strings.Contains(content, want) {
		t.Errorf("import 块不符合预期,want:\n%s\ngot:\n%s", want, content)
	}
	if n := strings.Count(content, "\"proto/test\""); n != 1 {
		t.Errorf("proto/test 必须只 import 一次,实际 %d 次", n)
	}
}

func TestRobotRpcOneWayOnlyServiceImportsNoCallMachinery(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	moveOnly := robotFixtureService(files[0], "SceneMoveOnly", 50,
		robotFixtureMethod{"MoveStart", ".fixturepb.Request", ".Empty", internal.ClientRpcOneWay},
		robotFixtureMethod{"NotifyMoved", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
	)
	dir := t.TempDir()
	if _, err := robotFixtureGenerate(t, files, dir, moveOnly); err != nil {
		t.Fatal(err)
	}
	content := robotReadFile(t, filepath.Join(dir, "scene_move_only.go"))
	robotAssertGoSource(t, "scene_move_only.go", content)
	want := "import (\n\ttest \"proto/test\"\n\t\"robot/generated/pb/game\"\n\t\"robot/pkg\"\n)\n"
	if !strings.Contains(content, want) {
		t.Errorf("只有 ONE_WAY 的服务不得 import context / fmt / time / protobuf / base,want:\n%s\ngot:\n%s", want, content)
	}
}

func TestRobotRpcRejectsNameCollisions(t *testing.T) {
	robotFixtureLogger(t)
	request := func(name string) robotFixtureMethod {
		return robotFixtureMethod{name, ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest}
	}
	for _, test := range []struct {
		name       string
		services   func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo
		identities []string
	}{
		{
			name: "方法与另一个方法的同步版撞名",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerShop", 100, request("Get"), request("GetSync"))}
			},
			identities: []string{"ClientPlayerShop.GetSync", "ClientPlayerShop.Get;"},
		},
		{
			name: "两个服务映射到同一变量名",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{
					robotFixtureService(files[0], "ClientPlayerShop", 100, request("Get")),
					robotFixtureService(files[1], "ClientPlayerShop", 110, request("Get")),
				}
			},
			identities: []string{"proto/test/client.proto", "proto/other/shared/types.proto"},
		},
		{
			name: "大小写不同的变量名",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{
					robotFixtureService(files[0], "ClientPlayerShop", 100, request("Get")),
					robotFixtureService(files[0], "ClientPlayershop", 110, request("Get")),
				}
			},
			identities: []string{"ClientPlayerShop", "ClientPlayershop"},
		},
		{
			name: "两个服务映射到同一文件名",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{
					robotFixtureService(files[0], "ClientPlayerShop", 100, request("Get")),
					robotFixtureService(files[0], "Client_Player_Shop", 110, request("Get")),
				}
			},
			identities: []string{"client_player_shop.go", "ClientPlayerShop", "Client_Player_Shop"},
		},
		{
			name: "文件名会被Go工具链当成测试文件",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerTest", 100, request("Get"))}
			},
			identities: []string{"ClientPlayerTest", "client_player_test.go"},
		},
		{
			name: "类型不在proto目录下",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerShop", 100,
					robotFixtureMethod{"Wait", ".google.protobuf.Duration", ".fixturepb.Response", internal.ClientRpcRequest})}
			},
			identities: []string{"ClientPlayerShop.Wait", "google/protobuf/duration.proto"},
		},
		{
			// internal.WriteGoMessageId 会把 "__" 折成 "_",调用桩引用的消息号常量名就对不上。
			name: "消息号常量名含双下划线",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerShop", 100, request("Get__All"))}
			},
			identities: []string{"ClientPlayerShop.Get__All", "ClientPlayerShopGet__AllMessageId"},
		},
		{
			name: "消息号放不进uint32",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerShop", math.MaxUint32+1, request("Get"))}
			},
			identities: []string{"ClientPlayerShop.Get", "4294967296"},
		},
		{
			name: "服务名小写开头",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "clientPlayerShop", 100, request("Get"))}
			},
			identities: []string{"clientPlayerShop (proto/test/client.proto)", "upper-case"},
		},
		{
			name: "方法名小写开头",
			services: func(files []*descriptorpb.FileDescriptorProto) []*internal.RPCServiceInfo {
				return []*internal.RPCServiceInfo{robotFixtureService(files[0], "ClientPlayerShop", 100, request("get"))}
			},
			identities: []string{"ClientPlayerShop.get", "upper-case"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := robotFixtureFiles()
			dir := t.TempDir()
			_, err := robotFixtureGenerate(t, files, dir, test.services(files)...)
			if err == nil {
				t.Fatal("冲突必须让生成失败")
			}
			for _, identity := range test.identities {
				if !strings.Contains(err.Error(), identity) {
					t.Errorf("错误信息必须指明 %q: %v", identity, err)
				}
			}
			if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
				t.Errorf("生成失败时不得写出任何文件: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestRobotRpcOutputIsIndependentOfServiceOrder(t *testing.T) {
	robotFixtureLogger(t)
	tmpl, err := template.ParseFiles(robotTemplate("robot_rpc.go.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	files, guild := robotFixture()
	auction := robotFixtureService(files[0], "AuctionService", 100,
		robotFixtureMethod{"Bid", ".fixturepb.Request", ".altpb.Reply", internal.ClientRpcRequest},
		robotFixtureMethod{"MoveBid", ".fixturepb.Request", ".Empty", internal.ClientRpcOneWay},
	)
	types := newRobotGoTypeIndex(files)
	wantNames := []string{"auction_service.go", "guild_service.go"}

	var first []robotRpcFile
	for _, order := range [][]*internal.RPCServiceInfo{{guild, auction}, {auction, guild}} {
		outputs, err := renderRobotRpcFiles(robotFixtureClassify(t, order...), types, tmpl, "MessageId")
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(outputs))
		for index, output := range outputs {
			names[index] = output.Name
		}
		if !slices.Equal(names, wantNames) {
			t.Fatalf("产物必须按文件名排序: got %v, want %v", names, wantNames)
		}
		if first == nil {
			first = outputs
			continue
		}
		for index := range outputs {
			if !bytes.Equal(outputs[index].Content, first[index].Content) {
				t.Errorf("服务传入顺序不同时 %s 的内容必须逐字节相同", outputs[index].Name)
			}
		}
	}
}

func TestRobotRpcRewritesDirectoryAndKeepsHandWrittenFiles(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	dir := t.TempDir()
	staleFiles := []string{filepath.Join(dir, "removed_service.go"), filepath.Join(dir, "removed_crlf.go")}
	robotWriteFile(t, staleFiles[0], "// Code generated by protogen. DO NOT EDIT.\n// Source: proto/removed.proto\n\npackage rpc\n")
	robotWriteFile(t, staleFiles[1], "\uFEFF// Code generated by protogen. DO NOT EDIT.\r\n// Source: proto/removed.proto\r\n\r\npackage rpc\r\n")
	keptFiles := []struct{ path, content string }{
		{filepath.Join(dir, "helpers.go"), "package rpc\n\n// 手写辅助函数不归生成器清理。\n"},
		// 只有第一行生成标记(其它生成器的产物形状,例如 route_table.go)不算本生成器的旧桩。
		{filepath.Join(dir, "route_table.go"), "// Code generated by protogen. DO NOT EDIT.\npackage rpc\n"},
		{filepath.Join(dir, "notes.txt"), "// Code generated by protogen. DO NOT EDIT.\n// Source: proto/removed.proto\n"},
		// 不递归子目录。
		{filepath.Join(dir, "sub", "old_service.go"), "// Code generated by protogen. DO NOT EDIT.\n// Source: proto/removed.proto\n"},
	}
	for _, kept := range keptFiles {
		robotWriteFile(t, kept.path, kept.content)
	}

	if _, err := robotFixtureGenerate(t, files, dir, guild); err != nil {
		t.Fatal(err)
	}
	for _, stale := range staleFiles {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("带生成标记、已不在清单里的旧桩必须删除: %s err=%v", stale, err)
		}
	}
	for _, kept := range keptFiles {
		if got := robotReadFile(t, kept.path); got != kept.content {
			t.Errorf("整写不得改动非本生成器产物或子目录: %s", kept.path)
		}
	}

	stub := filepath.Join(dir, "guild_service.go")
	before := robotReadFile(t, stub)
	if _, err := robotFixtureGenerate(t, files, dir, guild); err != nil {
		t.Fatal(err)
	}
	if after := robotReadFile(t, stub); after != before {
		t.Error("同一输入再次生成,产物必须逐字节不变")
	}
}

func TestRobotRpcRefusesToOverwriteHandWrittenFileWithStubName(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	dir := t.TempDir()
	target := filepath.Join(dir, "guild_service.go")
	const handWritten = "package rpc\n\n// 与生成桩同名的手写文件。\n"
	robotWriteFile(t, target, handWritten)

	_, err := robotFixtureGenerate(t, files, dir, guild)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("与生成桩同名的手写文件必须让生成失败: %v", err)
	}
	if got := robotReadFile(t, target); got != handWritten {
		t.Error("手写文件被覆盖")
	}
}

func TestRobotRpcSkipsServicesWithoutCallableMethods(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	empty := robotFixtureService(files[0], "EmptyService", 200)
	pushOnly := robotFixtureService(files[0], "PushOnlyService", 210,
		robotFixtureMethod{"NotifyChanged", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"Kick", ".fixturepb.Request", ".Empty", internal.ClientRpcServerOnly},
	)
	unmarked := robotFixtureUnmarkedService(files[0], "ClientPlayerUnmarked", 220)
	dir := t.TempDir()
	count, err := robotFixtureGenerate(t, files, dir, empty, pushOnly, unmarked)
	if err != nil || count != 0 {
		t.Fatalf("没有 REQUEST / ONE_WAY 方法的服务与未标 option 的服务都不出文件: count=%d err=%v", count, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("不应写出任何文件: entries=%v err=%v", entries, err)
	}
}

func TestRobotGoTypeHelpers(t *testing.T) {
	for input, want := range map[string]string{
		"SceneInfoC2S": "SceneInfoC2S",
		"Outer.Inner":  "Outer_Inner",
		"Outer.inner":  "OuterInner",
		"foo_bar":      "FooBar",
		"HTTPServer":   "HTTPServer",
	} {
		if got := goCamelCase(input); got != want {
			t.Errorf("goCamelCase(%q) = %q, want %q", input, got, want)
		}
	}

	aliases := robotImportAliases([]string{"proto/c/base", "proto/a/map", "proto/b/string", "proto/d/9lives"},
		map[string]struct{}{"base": {}})
	for importPath, want := range map[string]string{
		"proto/a/map":    "a_map",    // Go 关键字
		"proto/b/string": "b_string", // 预声明标识符
		"proto/c/base":   "c_base",   // 已被占用
		"proto/d/9lives": "p9lives",  // 数字开头
	} {
		if got := aliases[importPath]; got != want {
			t.Errorf("alias(%s) = %q, want %q", importPath, got, want)
		}
	}

	for name, special := range map[string]bool{
		"client_player_friend.go": false,
		"linux.go":                false,
		"client_player_test.go":   true,
		"scene_linux.go":          true,
		"scene_arm64.go":          true,
	} {
		if err := checkRobotGoFileName(name); (err != nil) != special {
			t.Errorf("checkRobotGoFileName(%q) err=%v, want special=%v", name, err, special)
		}
	}

	index := newRobotGoTypeIndex(robotFixtureFiles())
	if _, err := index.resolve(".not_present.Request"); err == nil {
		t.Error("描述符集里没有的类型必须报错,不能猜一个 Go 类型")
	}
	if got, err := index.resolve(".Empty"); err != nil || got != (robotGoType{ImportPath: "proto/common/base", Name: "Empty"}) {
		t.Errorf("无 package 的 Empty: got=%+v err=%v", got, err)
	}
}
