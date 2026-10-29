package unity

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"slices"
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

// 内存描述符沿正式分类与生成入口流转，不调用 protoc，也不触碰客户端仓。
func rpcRegressionFixture() (internal.RPCServiceInfoList, []*descriptorpb.FileDescriptorProto) {
	service := &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("GuildService"), Options: &descriptorpb.ServiceOptions{},
	}
	proto.SetExtension(service.Options, messageoption.E_OptionIsClientProtocolService, true)
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String("proto/test/client.proto"), Package: proto.String("fixturepb"),
		Options: &descriptorpb.FileOptions{CsharpNamespace: proto.String("Example.Api")},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Request")}, {Name: proto.String("Response")}, {Name: proto.String("Changed")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{service},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{
			Path: []int32{6, 0, 2, 0}, LeadingComments: proto.String(" 读取 <name> & 角色。\n"),
		}}},
	}
	info := &internal.RPCServiceInfo{ProtoFileInfo: internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}}
	for index, spec := range []struct {
		name, output string
		kind         internal.ClientRpcKind
	}{
		{"Read", ".fixturepb.Response", internal.ClientRpcRequest},
		{"Move", ".Empty", internal.ClientRpcOneWay},
		{"NotifyChanged", ".Empty", internal.ClientRpcPush},
		{"Disconnect", ".Empty", internal.ClientRpcServerOnly},
		{"CancelQueue", ".Empty", internal.ClientRpcRequest},
	} {
		input := ".fixturepb.Request"
		if spec.kind == internal.ClientRpcPush {
			input = ".fixturepb.Changed"
		}
		method := &descriptorpb.MethodDescriptorProto{
			Name: proto.String(spec.name), InputType: proto.String(input), OutputType: proto.String(spec.output),
		}
		rpcRegressionMarkKind(method, spec.kind)
		service.Method = append(service.Method, method)
		info.Methods = append(info.Methods, &internal.MethodInfo{
			ProtoFileInfo: info.ProtoFileInfo, MethodDescriptorProto: method, Id: uint64(index),
		})
	}
	return internal.RPCServiceInfoList{info}, []*descriptorpb.FileDescriptorProto{file, rpcRegressionEmptyFile()}
}

// rpcRegressionTemplate 是模板相对本包目录的路径;go test 以包目录为工作目录,与调用者的当前目录无关。
var rpcRegressionTemplate = filepath.Join("..", "..", "template", "unity_client_rpc.cs.tmpl")

// rpcRegressionMarkKind 把 OptionClientRpcKind 按字段号写进方法 options 的未知字段,
// 与流水线从描述符集读到的形状一致(外部 protooption 绑定里没有这个扩展)。
func rpcRegressionMarkKind(method *descriptorpb.MethodDescriptorProto, kind internal.ClientRpcKind) {
	if method.Options == nil {
		method.Options = &descriptorpb.MethodOptions{}
	}
	method.Options.ProtoReflect().SetUnknown(protowire.AppendVarint(
		protowire.AppendTag(nil, internal.OptionClientRpcKindNumber, protowire.VarintType), uint64(kind)))
}

// rpcRegressionEmptyFile 是 proto/common/base/empty.proto 的最小描述符:无 package 的 Empty。
func rpcRegressionEmptyFile() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name:        proto.String("proto/common/base/empty.proto"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
	}
}

// rpcRegressionMethod 是 rpcRegressionServiceFile 夹具里的一个方法;类型写描述符全名(带前导点)。
type rpcRegressionMethod struct {
	name, input, output string
	kind                internal.ClientRpcKind
	id                  uint64
}

// rpcRegressionServiceFile 构造一个 proto 文件:一个标了 OptionIsClientProtocolService 的服务,
// package 下有 Request / Response / Changed 三个消息;返回的 RPCServiceInfo 与描述符共用指针,同流水线。
func rpcRegressionServiceFile(fileName, pkg, serviceName string,
	methods ...rpcRegressionMethod) (*internal.RPCServiceInfo, *descriptorpb.FileDescriptorProto) {
	service := &descriptorpb.ServiceDescriptorProto{
		Name: proto.String(serviceName), Options: &descriptorpb.ServiceOptions{},
	}
	proto.SetExtension(service.Options, messageoption.E_OptionIsClientProtocolService, true)
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String(fileName), Package: proto.String(pkg),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Request")}, {Name: proto.String("Response")}, {Name: proto.String("Changed")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{service},
	}
	info := &internal.RPCServiceInfo{ProtoFileInfo: internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}}
	for _, spec := range methods {
		method := &descriptorpb.MethodDescriptorProto{
			Name: proto.String(spec.name), InputType: proto.String(spec.input), OutputType: proto.String(spec.output),
		}
		rpcRegressionMarkKind(method, spec.kind)
		service.Method = append(service.Method, method)
		info.Methods = append(info.Methods, &internal.MethodInfo{
			ProtoFileInfo: info.ProtoFileInfo, MethodDescriptorProto: method, Id: spec.id,
		})
	}
	return info, file
}

func rpcRegressionGenerate(t *testing.T, root string, services internal.RPCServiceInfoList,
	files []*descriptorpb.FileDescriptorProto) (rpcDirLayout, int, error) {
	t.Helper()
	classified, err := internal.ClassifyClientRpcServices(services, func(method *internal.MethodInfo) bool {
		return method.Method() == "Move"
	})
	if err != nil {
		return rpcDirLayout{}, 0, err
	}
	layout, err := newRpcDirLayout(root, filepath.Join(root, "Assets", "Scripts", "Net", "Generated", "Rpc"))
	if err != nil {
		return rpcDirLayout{}, 0, err
	}
	count, err := generateUnityClientRpc(classified, files, layout, rpcRegressionTemplate)
	return layout, count, err
}

func rpcRegressionRead(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func rpcRegressionLogger(t *testing.T) {
	t.Helper()
	previous := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previous })
}

func TestUnityClientRpcGeneratesRequestOneWayPushAndOmitsServerOnly(t *testing.T) {
	rpcRegressionLogger(t)
	services, files := rpcRegressionFixture()
	layout, count, err := rpcRegressionGenerate(t, t.TempDir(), services, files)
	if err != nil || count != 1 {
		t.Fatalf("生成结果 count=%d err=%v，预期仅一个服务文件", count, err)
	}
	content := string(rpcRegressionRead(t, filepath.Join(layout.dir, "GuildServiceRpc.cs")))
	for _, expected := range []string{
		"public const uint ReadMessageId = 0;",
		"public static void Read(IRpcChannel channel, global::Example.Api.Request request,",
		"RpcDone<global::Example.Api.Response> done)",
		"channel.CallMethod(ReadMessageId, request, global::Example.Api.Response.Parser, done)",
		"public static void Move(IRpcChannel channel, global::Example.Api.Request request)",
		"channel.SendOneWay(MoveMessageId, request)",
		"public static void OnNotifyChanged(IRpcChannel channel, Action<global::Example.Api.Changed> handler,",
		"channel.RegisterNotify(NotifyChangedMessageId, mc =>",
		"global::Example.Api.Changed.Parser.ParseFrom(mc.SerializedMessage)",
		"RpcDone<global::Empty> done)",
		"channel.CallMethod(CancelQueueMessageId, request, global::Empty.Parser, done)",
		"/// 读取 &lt;name&gt; &amp; 角色。",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("缺少契约输出 %q", expected)
		}
	}
	if strings.Contains(content, "Disconnect") {
		t.Error("SERVER_ONLY 不得生成方法或消息号常量")
	}
	if strings.Contains(content, "this IRpcChannel") {
		t.Error("调用桩不得生成为扩展方法")
	}
	// 没有前导注释的方法不写 <summary>:Move 的签名紧跟它的消息号常量。
	lines := strings.Split(content, "\n")
	moveIndex := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "public static void Move(")
	})
	if moveIndex < 1 {
		t.Fatal("缺少 Move 的签名行")
	}
	previous := strings.TrimSpace(lines[moveIndex-1])
	if previous == "/// </summary>" {
		t.Error("没有前导注释的方法不得输出 <summary>")
	}
	if previous != "public const uint MoveMessageId = 1;" {
		t.Errorf("Move 的签名必须紧跟它的消息号常量,前一行是 %q", previous)
	}
}

func TestUnityClientRpcRemovesStaleStubsAndKeepsStableMeta(t *testing.T) {
	rpcRegressionLogger(t)
	root := t.TempDir()
	rpcDir := filepath.Join(root, "Assets", "Scripts", "Net", "Generated", "Rpc")
	if err := os.MkdirAll(rpcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(rpcDir, "RemovedServiceRpc.cs")
	staleSource := "// <auto-generated>\n//   Generated by protogen from proto/removed.proto. DO NOT EDIT.\n// </auto-generated>\n"
	for name, content := range map[string]string{
		stale: staleSource, stale + ".meta": "fileFormatVersion: 2\nguid: 00000000000000000000000000000000\n",
		filepath.Join(rpcDir, "Handwritten.cs"): "// 手写文件不归生成器清理\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	services, files := rpcRegressionFixture()
	layout, _, err := rpcRegressionGenerate(t, root, services, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stale, stale + ".meta"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("已退役生成桩必须删除: %s, err=%v", name, err)
		}
	}
	if got := string(rpcRegressionRead(t, filepath.Join(rpcDir, "Handwritten.cs"))); got != "// 手写文件不归生成器清理\n" {
		t.Fatal("清理旧桩改写了手写文件")
	}
	paths := []string{filepath.Join(layout.dir, "GuildServiceRpc.cs"), filepath.Join(layout.dir, "GuildServiceRpc.cs.meta"), layout.dir + ".meta"}
	before := make([][]byte, len(paths))
	for index, name := range paths {
		before[index] = rpcRegressionRead(t, name)
	}
	// 这两个固定值来自设计规定的路径盐值，不从被测 GUID 函数计算期望值。
	if !bytes.Contains(before[1], []byte("guid: f8c3a4472e314b57c73e4a297adf4aa7\n")) {
		t.Error("脚本 meta GUID 不符合 protogen: + 相对 Assets 路径的既定值")
	}
	if !bytes.Contains(before[2], []byte("guid: bba74473eac1dc59d05e9d357073d8da\n")) {
		t.Error("目录 meta GUID 不符合既定值")
	}
	if _, _, err := rpcRegressionGenerate(t, root, services, files); err != nil {
		t.Fatal(err)
	}
	for index, name := range paths {
		if !bytes.Equal(before[index], rpcRegressionRead(t, name)) {
			t.Errorf("同一输入再次生成内容不稳定: %s", name)
		}
	}
}

func TestUnityClientRpcRefusesToOverwriteHandWrittenFileWithStubName(t *testing.T) {
	rpcRegressionLogger(t)
	root := t.TempDir()
	rpcDir := filepath.Join(root, "Assets", "Scripts", "Net", "Generated", "Rpc")
	if err := os.MkdirAll(rpcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(rpcDir, "GuildServiceRpc.cs")
	const handWritten = "// 与生成桩同名的手写脚本。\r\nnamespace Game { public static class GuildServiceRpc {} }\r\n"
	if err := os.WriteFile(target, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	// AlphaServiceRpc.cs 按文件名排在 GuildServiceRpc.cs 之前:检查必须在写第一个文件之前全部做完。
	services, files := rpcRegressionFixture()
	alpha, alphaFile := rpcRegressionServiceFile("proto/test/alpha.proto", "alphapb", "AlphaService",
		rpcRegressionMethod{"Read", ".alphapb.Request", ".alphapb.Response", internal.ClientRpcRequest, 40})
	services = append(services, alpha)
	files = append(files, alphaFile)

	_, count, err := rpcRegressionGenerate(t, root, services, files)
	if err == nil || count != 0 {
		t.Fatalf("与生成桩同名的手写脚本必须让生成失败: count=%d err=%v", count, err)
	}
	for _, expected := range []string{"GuildServiceRpc.cs", "refusing to overwrite"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("错误必须指明 %q: %v", expected, err)
		}
	}
	if got := string(rpcRegressionRead(t, target)); got != handWritten {
		t.Error("手写脚本被覆盖")
	}
	entries, err := os.ReadDir(rpcDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"GuildServiceRpc.cs"}) {
		t.Errorf("拒绝覆盖时目录里不得新增文件: %v", names)
	}
	if _, err := os.Stat(rpcDir + ".meta"); !os.IsNotExist(err) {
		t.Errorf("拒绝覆盖时不得写目录 meta: err=%v", err)
	}
}

func TestUnityClientRpcKeepsStubWhoseOldNameDiffersOnlyInCase(t *testing.T) {
	rpcRegressionLogger(t)
	foo, fooFile := rpcRegressionServiceFile("proto/test/foo.proto", "foopb", "FooService",
		rpcRegressionMethod{"Read", ".foopb.Request", ".foopb.Response", internal.ClientRpcRequest, 1})
	services := internal.RPCServiceInfoList{foo}
	files := []*descriptorpb.FileDescriptorProto{fooFile}

	// 期望内容取自干净目录里的同一输入:产物逐字节确定,两次生成的相对 Assets 路径相同,.meta GUID 也相同。
	clean, _, err := rpcRegressionGenerate(t, t.TempDir(), services, files)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := rpcRegressionRead(t, filepath.Join(clean.dir, "FooServiceRpc.cs"))
	wantMeta := rpcRegressionRead(t, filepath.Join(clean.dir, "FooServiceRpc.cs.meta"))
	if !bytes.Contains(wantSource, []byte("public static class FooServiceRpc")) {
		t.Fatalf("基准产物不是 FooServiceRpc:\n%s", wantSource)
	}

	// 预置服务名改大小写之前的旧桩 fooServiceRpc.cs(带生成标记)。内容就是本次产物,写入时内容不变不落盘,
	// 磁盘上的名字保持小写开头,目录列表返回的就是 fooServiceRpc.cs。
	root := t.TempDir()
	rpcDir := filepath.Join(root, "Assets", "Scripts", "Net", "Generated", "Rpc")
	if err := os.MkdirAll(rpcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rpcDir, "fooServiceRpc.cs"), wantSource, 0o644); err != nil {
		t.Fatal(err)
	}
	layout, count, err := rpcRegressionGenerate(t, root, services, files)
	if err != nil || count != 1 {
		t.Fatalf("预期生成一个服务文件: count=%d err=%v", count, err)
	}

	// 大小写不敏感的文件系统(Windows、默认配置的 macOS)上 fooServiceRpc.cs 与 FooServiceRpc.cs 是同一个文件:
	// 按原样比较文件名会把它当成旧桩删掉,连同刚写的 FooServiceRpc.cs.meta。大小写敏感的文件系统上二者是两个文件,
	// FooServiceRpc.cs 是新写的,fooServiceRpc.cs 按小写比较也不删(本用例不对它下断言)。
	// 两种文件系统上,下面的断言都必须成立。
	for _, check := range []struct {
		name string
		want []byte
	}{
		{"FooServiceRpc.cs", wantSource},
		{"FooServiceRpc.cs.meta", wantMeta},
	} {
		got, err := os.ReadFile(filepath.Join(layout.dir, check.name))
		if err != nil {
			t.Errorf("%s 不得被当成旧桩删除: %v", check.name, err)
			continue
		}
		if !bytes.Equal(got, check.want) {
			t.Errorf("%s 内容必须是本次产物\nwant:\n%s\ngot:\n%s", check.name, check.want, got)
		}
	}
}

func TestUnityClientRpcSelectsServiceOptionRatherThanName(t *testing.T) {
	rpcRegressionLogger(t)
	services, files := rpcRegressionFixture()
	unmarked := &descriptorpb.ServiceDescriptorProto{Name: proto.String("ClientPlayerUnmarked")}
	method := &descriptorpb.MethodDescriptorProto{
		Name: proto.String("Read"), InputType: proto.String(".fixturepb.Request"), OutputType: proto.String(".fixturepb.Response"),
	}
	unmarked.Method = []*descriptorpb.MethodDescriptorProto{method}
	files[0].Service = append(files[0].Service, unmarked)
	info := internal.ProtoFileInfo{Fd: files[0], ServiceDescriptorProto: unmarked}
	services = append(services, &internal.RPCServiceInfo{
		ProtoFileInfo: info, Methods: internal.RPCMethods{{ProtoFileInfo: info, MethodDescriptorProto: method, Id: 19}},
	})
	layout, count, err := rpcRegressionGenerate(t, t.TempDir(), services, files)
	if err != nil || count != 1 {
		t.Fatalf("服务必须按显式 option 筛选: count=%d err=%v", count, err)
	}
	if _, err := os.Stat(filepath.Join(layout.dir, "GuildServiceRpc.cs")); err != nil {
		t.Error("不含 ClientPlayer 的已标注服务未生成")
	}
	if _, err := os.Stat(filepath.Join(layout.dir, "ClientPlayerUnmarkedRpc.cs")); !os.IsNotExist(err) {
		t.Errorf("未标注服务不应生成: err=%v", err)
	}
}

func TestUnityClientRpcMapsNestedAndNamespacedTypes(t *testing.T) {
	files := []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("proto/test/types.proto"), Package: proto.String("sample_proto.v2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Outer"), NestedType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Inner")},
				// map<..> 字段的合成 entry 消息:protoc 的 C# 生成器不为它生成类型。
				{Name: proto.String("ItemsEntry"), Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)}},
			},
		}},
	}, {
		Name: proto.String("proto/test/override.proto"), Package: proto.String("ignored"),
		Options:     &descriptorpb.FileOptions{CsharpNamespace: proto.String("Custom.Types")},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Reply")}},
	}}
	index := newCSharpTypeIndex(files)
	for input, expected := range map[string]string{
		".sample_proto.v2.Outer.Inner": "global::SampleProto.V2.Outer.Types.Inner",
		".ignored.Reply":               "global::Custom.Types.Reply",
	} {
		actual, err := index.resolve(input)
		if err != nil || actual != expected {
			t.Errorf("类型映射 %s: got=%q err=%v want=%q", input, actual, err, expected)
		}
	}
	if _, err := index.resolve(".not_present.Request"); err == nil {
		t.Error("缺失的描述符类型必须拒绝，不能输出猜测的 C# 类型")
	}
	if actual, err := index.resolve(".sample_proto.v2.Outer.ItemsEntry"); err == nil {
		t.Errorf("map entry 没有 C# 类型,必须拒绝: got=%q", actual)
	}
}

func TestUnityClientRpcRejectsClassNameCollisionAcrossPackages(t *testing.T) {
	rpcRegressionLogger(t)
	for _, test := range []struct{ name, first, second string }{
		{"同名服务", "Foo", "Foo"},
		// Windows 文件系统大小写不敏感,FooRpc.cs 与 fooRpc.cs 会互相覆盖。
		{"仅大小写不同", "Foo", "foo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			alpha, alphaFile := rpcRegressionServiceFile("proto/alpha/foo.proto", "alphapb", test.first,
				rpcRegressionMethod{"Read", ".alphapb.Request", ".alphapb.Response", internal.ClientRpcRequest, 1})
			beta, betaFile := rpcRegressionServiceFile("proto/beta/foo.proto", "betapb", test.second,
				rpcRegressionMethod{"Read", ".betapb.Request", ".betapb.Response", internal.ClientRpcRequest, 2})
			_, count, err := rpcRegressionGenerate(t, root, internal.RPCServiceInfoList{alpha, beta},
				[]*descriptorpb.FileDescriptorProto{alphaFile, betaFile})
			if err == nil || count != 0 {
				t.Fatalf("两个服务映射到同一个 C# 类名必须拒绝: count=%d err=%v", count, err)
			}
			for _, protoFile := range []string{"proto/alpha/foo.proto", "proto/beta/foo.proto"} {
				if !strings.Contains(err.Error(), protoFile) {
					t.Errorf("错误必须指出冲突双方的 proto 文件 %s: %v", protoFile, err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(root, "Assets")); !os.IsNotExist(statErr) {
				t.Errorf("校验失败时不得写出任何文件: err=%v", statErr)
			}
		})
	}
}

func TestUnityClientRpcRejectsMemberNameCollisions(t *testing.T) {
	rpcRegressionLogger(t)
	const (
		request  = ".fixturepb.Request"
		response = ".fixturepb.Response"
	)
	for _, test := range []struct {
		name    string
		methods []rpcRegressionMethod
		want    []string
	}{
		{"推送生成的On成员与同名方法", []rpcRegressionMethod{
			{"NotifyChanged", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush, 1},
			{"OnNotifyChanged", request, response, internal.ClientRpcRequest, 2},
		}, []string{"GuildService.OnNotifyChanged", "GuildService.NotifyChanged"}},
		{"消息号常量与同名方法", []rpcRegressionMethod{
			{"Read", request, response, internal.ClientRpcRequest, 1},
			{"ReadMessageId", request, response, internal.ClientRpcRequest, 2},
		}, []string{"GuildService.ReadMessageId"}},
		// CS0542:成员不得与所在类型同名。
		{"方法与所在类同名", []rpcRegressionMethod{
			{"GuildServiceRpc", request, response, internal.ClientRpcRequest, 1},
		}, []string{"GuildService.GuildServiceRpc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			service, file := rpcRegressionServiceFile("proto/test/client.proto", "fixturepb", "GuildService", test.methods...)
			_, count, err := rpcRegressionGenerate(t, root, internal.RPCServiceInfoList{service},
				[]*descriptorpb.FileDescriptorProto{file, rpcRegressionEmptyFile()})
			if err == nil || count != 0 {
				t.Fatalf("同一类里的 C# 成员名冲突必须拒绝: count=%d err=%v", count, err)
			}
			for _, expected := range test.want {
				if !strings.Contains(err.Error(), expected) {
					t.Errorf("错误必须指出冲突方法 %s: %v", expected, err)
				}
			}
			if _, statErr := os.Stat(filepath.Join(root, "Assets")); !os.IsNotExist(statErr) {
				t.Errorf("校验失败时不得写出任何文件: err=%v", statErr)
			}
		})
	}
}

func TestUnityClientRpcMessageIDMustFitCSharpUint(t *testing.T) {
	rpcRegressionLogger(t)
	const largestConst = "public const uint ReadMessageId = 4294967295;"
	generate := func(id uint64) (string, int, error) {
		service, file := rpcRegressionServiceFile("proto/test/client.proto", "fixturepb", "GuildService",
			rpcRegressionMethod{"Read", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest, id})
		layout, count, err := rpcRegressionGenerate(t, t.TempDir(), internal.RPCServiceInfoList{service},
			[]*descriptorpb.FileDescriptorProto{file})
		return layout.dir, count, err
	}

	dir, count, err := generate(math.MaxUint32)
	if err != nil || count != 1 {
		t.Fatalf("C# uint 上界必须接受: count=%d err=%v", count, err)
	}
	if content := string(rpcRegressionRead(t, filepath.Join(dir, "GuildServiceRpc.cs"))); !strings.Contains(content, largestConst) {
		t.Errorf("缺少消息号常量 %q", largestConst)
	}

	_, count, err = generate(math.MaxUint32 + 1)
	if err == nil || count != 0 {
		t.Fatalf("放不进 C# uint 的消息号必须拒绝: count=%d err=%v", count, err)
	}
	for _, expected := range []string{"GuildService.Read", "4294967296"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("错误必须指出 %s: %v", expected, err)
		}
	}
}

func TestUnityClientRpcDirLayoutMustBeInsideAssets(t *testing.T) {
	root := t.TempDir()
	assets := filepath.Join(root, "Assets")
	separator := string(filepath.Separator)
	for _, test := range []struct{ name, clientDir, rpcDir string }{
		{"Assets本身", root, assets},
		{"Assets本身带末尾分隔符", root, assets + separator},
		{"客户端根目录", root, root},
		{"Assets之外", root, filepath.Join(root, "Library", "Rpc")},
		{"名字以Assets开头的兄弟目录", root, filepath.Join(root, "AssetsBackup", "Rpc")},
		// 不能用 filepath.Join:它会先把 .. 化简掉。
		{"经..跳出Assets", root, assets + separator + ".." + separator + "Library"},
		{"未配置rpc目录", root, ""},
		{"未配置客户端目录", "", filepath.Join(assets, "Rpc")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if layout, err := newRpcDirLayout(test.clientDir, test.rpcDir); err == nil {
				t.Errorf("rpc 目录 %q 必须拒绝: layout=%+v", test.rpcDir, layout)
			}
		})
	}

	// etc/proto_gen.yaml 的形状:正斜杠,两个目录都带末尾分隔符。
	const wantAssetsRelDir = "Scripts/Net/Generated/Rpc"
	slashRoot := filepath.ToSlash(root) + "/"
	layout, err := newRpcDirLayout(slashRoot, slashRoot+"Assets/Scripts/Net/Generated/Rpc/")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(assets, "Scripts", "Net", "Generated", "Rpc")
	if layout.dir != wantDir || layout.assetsRelDir != wantAssetsRelDir {
		t.Errorf("layout=%+v, want dir=%s assetsRelDir=%s", layout, wantDir, wantAssetsRelDir)
	}
}

func TestUnityClientRpcRemovesOnlyOwnOrphanMeta(t *testing.T) {
	rpcRegressionLogger(t)
	// 前两个 GUID 是 md5("protogen:Scripts/Net/Generated/Rpc/<文件名>"),在测试外按设计规定的盐值独立算出,不调被测函数。
	const (
		retiredMetaGUID     = "65c208565c5d4cd19ac678fc5c11471d"
		handwrittenMetaGUID = "056a88cb6600841895f9be74a46c2a08"
		foreignMetaGUID     = "0123456789abcdef0123456789abcdef"
	)
	root := t.TempDir()
	rpcDir := filepath.Join(root, "Assets", "Scripts", "Net", "Generated", "Rpc")
	if err := os.MkdirAll(rpcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seeded := []struct {
		name, content string
		kept          bool
	}{
		// 源文件已删、GUID 正是本生成器按该路径算出的孤立 .meta(CRLF 检出):删除。
		{"RetiredServiceRpc.cs.meta", "fileFormatVersion: 2\r\nguid: " + retiredMetaGUID + "\r\n", false},
		// 孤立但 GUID 对不上,不是本生成器写的:保留。
		{"ForeignTool.cs.meta", "fileFormatVersion: 2\nguid: " + foreignMetaGUID + "\n", true},
		// 源文件还在,.meta 归源文件,哪怕 GUID 恰好与本生成器的算法一致:都保留。
		{"HandwrittenTool.cs", "// 手写工具脚本\n", true},
		{"HandwrittenTool.cs.meta", "fileFormatVersion: 2\nguid: " + handwrittenMetaGUID + "\n", true},
		// 检出成 CRLF 且带 UTF-8 BOM 的旧桩仍认作生成产物:连同它的 .meta 一起删除。
		{"CrlfServiceRpc.cs", "\uFEFF// <auto-generated>\r\n//   Generated by protogen from proto/crlf.proto. DO NOT EDIT.\r\n// </auto-generated>\r\n", false},
		{"CrlfServiceRpc.cs.meta", "fileFormatVersion: 2\nguid: " + foreignMetaGUID + "\n", false},
	}
	for _, file := range seeded {
		if err := os.WriteFile(filepath.Join(rpcDir, file.name), []byte(file.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	services, files := rpcRegressionFixture()
	if _, _, err := rpcRegressionGenerate(t, root, services, files); err != nil {
		t.Fatal(err)
	}
	for _, file := range seeded {
		content, err := os.ReadFile(filepath.Join(rpcDir, file.name))
		switch {
		case file.kept && (err != nil || string(content) != file.content):
			t.Errorf("%s 必须原样保留: err=%v", file.name, err)
		case !file.kept && !os.IsNotExist(err):
			t.Errorf("%s 必须删除: err=%v", file.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rpcDir, "GuildServiceRpc.cs")); err != nil {
		t.Errorf("本次产物缺失: %v", err)
	}
}

func TestUnityClientRpcSkipsServicesWithoutClientMembers(t *testing.T) {
	rpcRegressionLogger(t)
	root := t.TempDir()
	services, files := rpcRegressionFixture()
	legacy, legacyFile := rpcRegressionServiceFile("proto/test/legacy.proto", "legacypb", "LegacyService",
		rpcRegressionMethod{"Kick", ".legacypb.Request", ".legacypb.Response", internal.ClientRpcRequest, 30})
	silent, silentFile := rpcRegressionServiceFile("proto/test/silent.proto", "silentpb", "SilentService")
	services = append(services, legacy, silent)
	files = append(files, legacyFile, silentFile)

	layout, count, err := rpcRegressionGenerate(t, root, services, files)
	if err != nil || count != 2 {
		t.Fatalf("首轮应生成 GuildService 与 LegacyService 两个文件: count=%d err=%v", count, err)
	}
	legacyStub := filepath.Join(layout.dir, "LegacyServiceRpc.cs")
	rpcRegressionRead(t, legacyStub)

	// 第二轮:LegacyService 唯一的方法改标 SERVER_ONLY,服务不再有客户端成员;
	// 上一轮由真实模板写出的桩必须被认出是生成产物并删除(守住 rpcSourceMarker 与模板头部同步)。
	rpcRegressionMarkKind(legacy.Methods[0].MethodDescriptorProto, internal.ClientRpcServerOnly)
	if _, count, err = rpcRegressionGenerate(t, root, services, files); err != nil || count != 1 {
		t.Fatalf("只应剩 GuildService 一个文件: count=%d err=%v", count, err)
	}
	for _, name := range []string{legacyStub, legacyStub + ".meta", filepath.Join(layout.dir, "SilentServiceRpc.cs")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("没有非 SERVER_ONLY 方法的服务不得留下调用桩: %s err=%v", name, err)
		}
	}
}

func TestUnityClientRpcOrdersFilesByOutputName(t *testing.T) {
	tmpl, err := template.ParseFiles(rpcRegressionTemplate)
	if err != nil {
		t.Fatal(err)
	}
	// proto 路径的顺序与类名顺序故意相反:排序键是产物文件名,与 proto 路径和输入顺序都无关。
	zeta, zetaFile := rpcRegressionServiceFile("proto/a/zeta.proto", "zetapb", "ZetaService",
		rpcRegressionMethod{"Read", ".zetapb.Request", ".zetapb.Response", internal.ClientRpcRequest, 1})
	middle, middleFile := rpcRegressionServiceFile("proto/m/middle.proto", "middlepb", "MiddleService",
		rpcRegressionMethod{"Read", ".middlepb.Request", ".middlepb.Response", internal.ClientRpcRequest, 2})
	alpha, alphaFile := rpcRegressionServiceFile("proto/z/alpha.proto", "alphapb", "AlphaService",
		rpcRegressionMethod{"Read", ".alphapb.Request", ".alphapb.Response", internal.ClientRpcRequest, 3})
	types := newCSharpTypeIndex([]*descriptorpb.FileDescriptorProto{zetaFile, middleFile, alphaFile})
	want := []string{"AlphaServiceRpc.cs", "MiddleServiceRpc.cs", "ZetaServiceRpc.cs"}

	var first []rpcFile
	for _, order := range []internal.RPCServiceInfoList{{zeta, alpha, middle}, {middle, zeta, alpha}} {
		classified, err := internal.ClassifyClientRpcServices(order, func(*internal.MethodInfo) bool { return false })
		if err != nil {
			t.Fatal(err)
		}
		outputs, err := renderRpcFiles(classified, types, tmpl)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(outputs))
		for index, output := range outputs {
			names[index] = output.Name
		}
		if !slices.Equal(names, want) {
			t.Fatalf("产物必须按文件名排序: got %v, want %v", names, want)
		}
		if first == nil {
			first = outputs
			continue
		}
		for index := range outputs {
			if !bytes.Equal(outputs[index].Content, first[index].Content) {
				t.Errorf("输入顺序不同时 %s 的内容不得变化", outputs[index].Name)
			}
		}
	}
}
