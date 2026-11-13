package main

import (
	"os"
	"regexp"
	"testing"

	"google.golang.org/grpc"

	loginpb "proto/login"
)

// goctl 生成的服务包装(internal/server/*/…server.go)都嵌着 pb 的 Unimplemented…Server:proto 里新加一个方法之后,
// 不重生包装(go/build.ps1)也照样编得过,只是这个方法每次调用都回 codes.Unimplemented,没有任何单测会红。
//
// gate 排空迁移的 RequestGateMigration 就这样漏过:gate 每次要票都被拒,迁移一律退避、永远不生效
// (docs/design/gate-drain-session-migration.md §18.3)。这里逐个服务核对:pb 服务描述里的每个方法,包装里都要有自己的实现。
// 用例红了就重生包装,不要手改生成文件(AGENTS.md §3)。
func TestGoctlServerWrappersImplementEveryRPC(t *testing.T) {
	wrappers := []struct {
		desc     grpc.ServiceDesc
		file     string
		receiver string
	}{
		{loginpb.ClientPlayerLogin_ServiceDesc, "internal/server/clientplayerlogin/clientplayerloginserver.go", "ClientPlayerLoginServer"},
		{loginpb.LoginPreGate_ServiceDesc, "internal/server/loginpregate/loginpregateserver.go", "LoginPreGateServer"},
		{loginpb.LoginAdmin_ServiceDesc, "internal/server/loginadmin/loginadminserver.go", "LoginAdminServer"},
	}
	for _, w := range wrappers {
		source, err := os.ReadFile(w.file)
		if err != nil {
			t.Fatalf("read %s: %v", w.file, err)
		}
		names := make([]string, 0, len(w.desc.Methods)+len(w.desc.Streams))
		for _, m := range w.desc.Methods {
			names = append(names, m.MethodName)
		}
		for _, s := range w.desc.Streams {
			names = append(names, s.StreamName)
		}
		if len(names) == 0 {
			t.Fatalf("%s declares no RPC; the service descriptor is not the one this test expects", w.desc.ServiceName)
		}
		for _, name := range names {
			declared := regexp.MustCompile(`(?m)^func \(\w+ \*` + regexp.QuoteMeta(w.receiver) + `\) ` + regexp.QuoteMeta(name) + `\(`)
			if !declared.Match(source) {
				t.Errorf("%s does not implement %s.%s: the embedded Unimplemented stub answers it with codes.Unimplemented; "+
					"regenerate the goctl wrappers with go/build.ps1", w.file, w.desc.ServiceName, name)
			}
		}
	}
}
