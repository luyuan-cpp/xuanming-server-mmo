package internal

import (
	"strings"
	"text/template"

	_config "protogen/internal/config"
	"protogen/internal/utils"
)

// GrpcHandlerMethod holds data for a gRPC handler method.
type GrpcHandlerMethod struct {
	HandlerName string
	CppRequest  string
	CppResponse string
	Code        string
	HasCode     bool
	IsVoidResponse bool
	// PreDispatchCode 是 gRPC 包装函数里、投递到事件循环之前的守护段(含首尾标记)。
	// 用于必须在 gRPC 线程上完成的准入检查(如 Agones 建场景许可),可返回非 OK。
	// 包装函数原本没有守护段,守护段外的手写代码会被重生成吞掉(commit 6c4021ae5 事故)。
	PreDispatchCode string
}

// GrpcHandlerHeadData holds data for generating a gRPC service handler header.
type GrpcHandlerHeadData struct {
	GrpcInclude string
	Service     string
	Package     string
	Methods     []*MethodInfo
}

// GrpcHandlerCppData holds data for generating a gRPC service handler CPP file.
type GrpcHandlerCppData struct {
	IncludeName string
	Service     string
	Package     string
	FirstCode   string
	YourCodePair string
	Methods     []GrpcHandlerMethod
}

// GetGrpcServiceHandlerHeadStr generates the gRPC service handler header file.
// The generated class extends the grpc-generated ::Service base and dispatches
// each RPC to a static Handle* method. Handle* methods contain BEGIN WRITING YOUR CODE blocks.
func GetGrpcServiceHandlerHeadStr(methods RPCMethods) (string, error) {
	const tmplStr = `#pragma once

#include <grpcpp/grpcpp.h>
#include <muduo/net/EventLoop.h>
{{.GrpcInclude}}
// gRPC service implementation for {{.Service}}.
// All RPC methods dispatch to the muduo event loop via runInLoop + promise/future
// (blocking the gRPC thread pool thread until the event loop processes the request).
//
// IMPORTANT: Handle* methods run on the event loop thread.
//   - Do NOT perform blocking I/O or long-running operations.
//   - Always return grpc::Status::OK; communicate errors via response fields.
//   - Each RPC wrapper also has a WRITING YOUR CODE section that runs on the gRPC
//     thread BEFORE dispatch (admission checks such as Agones permits); only that
//     section may return a non-OK status.
class {{.Service}}Impl final : public {{.Package}}{{.Service}}::Service
{
public:
    explicit {{.Service}}Impl(muduo::net::EventLoop& loop);
{{range .Methods}}
    grpc::Status {{.Method}}(grpc::ServerContext* context,
        const {{.CppRequest}}* request,
        {{.CppResponse}}* response) override;
{{end}}
private:
    // Handler functions -- run on the muduo event loop thread.
    // WARNING: Must complete quickly. The gRPC thread is blocked waiting via promise/future.{{range .Methods}}
    static void Handle{{.Method}}(const {{.CppRequest}}* request{{handleResponseParam .}});{{end}}

    muduo::net::EventLoop& loop_;
};
`

	if len(methods) == 0 {
		return "", nil
	}

	first := methods[0]

	data := GrpcHandlerHeadData{
		GrpcInclude: first.GrpcIncludeHeadName(),
		Service:     first.Service(),
		Package:     cppPackagePrefix(first),
		Methods:     methods,
	}

	funcs := template.FuncMap{
		"handleResponseParam": func(m *MethodInfo) string {
			if isVoidResponse(m) {
				return ""
			}
			return ", " + m.CppResponse() + "* response"
		},
	}

	return utils.GlobalEngine.MustExecute("grpcServiceHandlerHead", tmplStr, data, funcs)
}

// GetGrpcServiceHandlerCppStr generates the gRPC service handler CPP file content.
func GetGrpcServiceHandlerCppStr(dst string, methods RPCMethods, className string, includeName string) string {
	const tmplStr = `{{ .IncludeName }}

{{- if .FirstCode }}
{{ .FirstCode }}
{{- end }}

{{ .Service }}Impl::{{ .Service }}Impl(muduo::net::EventLoop& loop)
    : loop_(loop)
{
}
{{ range .Methods }}
void {{ $.Service }}Impl::Handle{{ .HandlerName }}(const {{ .CppRequest }}* request{{ if not .IsVoidResponse }},
    {{ .CppResponse }}* response{{ end }})
{
{{ if .HasCode }}{{ .Code -}}
{{ else }}{{ $.YourCodePair }}
{{ end }}}
{{ end }}
{{- range .Methods }}
grpc::Status {{ $.Service }}Impl::{{ .HandlerName }}(grpc::ServerContext* /*context*/,
    const {{ .CppRequest }}* request,
    {{ .CppResponse }}* response)
{
{{ .PreDispatchCode }}
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request{{ if not .IsVoidResponse }}, response{{ end }}, &promise]
                    {
        Handle{{ .HandlerName }}(request{{ if not .IsVoidResponse }}, response{{ end }});
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}
{{ end }}`

	if len(methods) == 0 {
		return ""
	}

	emptyStr := ""
	yourCodesMap, firstCode, _ := ReadCodeSectionsFromFile(dst, &methods, GenerateGrpcHandlerNameWrapper, emptyStr)
	// 第二遍按包装函数签名取"投递前"守护段。解析器只在命中方法签名之后才收集守护段,
	// Handle* 的签名不含 "(grpc::ServerContext",所以两遍互不串段;
	// 旧文件的包装函数没有守护段时由 fillMissingMethods 补默认空段。
	preDispatchMap, _, _ := ReadCodeSectionsFromFile(dst, &methods, GenerateGrpcWrapperNameWrapper, emptyStr)

	first := methods[0]

	var methodList []GrpcHandlerMethod
	for _, methodInfo := range methods {
		handlerName := methodInfo.Method()
		lookupKey := GenerateGrpcHandlerNameWrapper(methodInfo, emptyStr)
		code, exists := yourCodesMap[lookupKey]
		preDispatchCode, ok := preDispatchMap[GenerateGrpcWrapperNameWrapper(methodInfo, emptyStr)]
		if !ok {
			preDispatchCode = _config.Global.Naming.YourCodePair
		}
		methodList = append(methodList, GrpcHandlerMethod{
			HandlerName:     handlerName,
			CppRequest:      methodInfo.CppRequest(),
			CppResponse:     methodInfo.CppResponse(),
			Code:            code,
			HasCode:         exists,
			IsVoidResponse:  isVoidResponse(methodInfo),
			PreDispatchCode: strings.TrimRight(preDispatchCode, "\n"),
		})
	}

	data := GrpcHandlerCppData{
		IncludeName:  grpcHandlerIncludeName(first),
		Service:      first.Service(),
		Package:      cppPackagePrefix(first),
		FirstCode:    firstCode,
		YourCodePair: _config.Global.Naming.YourCodePair,
		Methods:      methodList,
	}

	return utils.ExecuteTemplate("grpcServiceHandlerCpp", tmplStr, data)
}

// GenerateGrpcHandlerNameWrapper generates the method lookup key for code parsing.
func GenerateGrpcHandlerNameWrapper(info *MethodInfo, _ string) string {
	return info.Service() + "Impl::Handle" + info.Method() + "("
}

// GenerateGrpcWrapperNameWrapper 生成 gRPC 包装函数(投递前守护段)的查找键。
// 带上 "(grpc::ServerContext" 是为了不与 Handle* 键、以及同名前缀方法(如 Create / CreateScene)互相命中。
func GenerateGrpcWrapperNameWrapper(info *MethodInfo, _ string) string {
	return info.Service() + "Impl::" + info.Method() + "(grpc::ServerContext"
}

// cppPackagePrefix returns the C++ namespace prefix with trailing :: if non-empty.
func cppPackagePrefix(info *MethodInfo) string {
	pkg := info.CppPackage()
	if pkg == "" {
		return ""
	}
	return pkg + "::"
}

// isVoidResponse checks if the response type is Empty (void-like).
func isVoidResponse(m *MethodInfo) bool {
	resp := m.CppResponse()
	return resp == "::Empty" || resp == "Empty"
}

// grpcHandlerIncludeName returns the #include for the gRPC handler header.
func grpcHandlerIncludeName(info *MethodInfo) string {
	return "#include \"" + info.FileBaseNameNoEx() + _config.Global.FileExtensions.GrpcHandlerH + "\"\n" +
		"#include <future>"
}
