package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"shared/accountdirectory"
)

func globalLoginConfigForTest() Config {
	c := Config{}
	c.Node.ZoneId = 7
	c.DataServiceRpc.Target = "direct:///127.0.0.1:1"
	return c
}

func globalDirectoryTestEnv(key string) string {
	if key == accountdirectory.SecretEnv {
		return strings.Repeat("s", 32)
	}
	return ""
}

func TestGlobalLoginAuthorityGuard(t *testing.T) {
	for _, mode := range []string{"", "pro", service.DevMode, service.TestMode} {
		for _, tc := range []struct {
			name   string
			mutate func(*Config)
			key    string
		}{
			{"归属区为空", func(c *Config) { c.Node.ZoneId = 0 }, "Node.ZoneId"},
			{"无DS", func(c *Config) { c.DataServiceRpc.Target = "" }, "DataServiceRpc"},
			{"空白DS目标", func(c *Config) {
				c.DataServiceRpc.Target = " \t"
				c.DataServiceRpc.Endpoints = []string{" "}
				c.DataServiceRpc.Etcd.Key = "dataservice.rpc"
			}, "DataServiceRpc"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				c := globalLoginConfigForTest()
				c.Mode = mode
				if err := c.ValidateGlobalAuthority(globalDirectoryTestEnv); err != nil {
					t.Fatalf("完整全球配置必须通过: %v", err)
				}
				tc.mutate(&c)
				if err := c.ValidateGlobalAuthority(globalDirectoryTestEnv); err == nil || !strings.Contains(err.Error(), tc.key) {
					t.Fatalf("必须拒绝%s: %v", tc.key, err)
				}
			})
		}
	}
	// 目标配置沿go-zero既有三种形式，不能因为号段关闭而静默丢失DS目录接缝。
	for _, kind := range []string{"target", "endpoint", "etcd"} {
		c := globalLoginConfigForTest()
		if kind != "target" {
			c.DataServiceRpc.Target = ""
		}
		if kind == "endpoint" {
			c.DataServiceRpc.Endpoints = []string{"127.0.0.1:1"}
		}
		if kind == "etcd" {
			c.DataServiceRpc.Etcd.Key = "dataservice.rpc"
			c.DataServiceRpc.Etcd.Hosts = []string{"127.0.0.1:1"}
		}
		if err := c.ValidateGlobalAuthority(globalDirectoryTestEnv); err != nil {
			t.Fatalf("DS目标形状%s必须保留: %v", kind, err)
		}
	}
}

func TestGlobalLoginDirectorySecretRequiredBeforeConnections(t *testing.T) {
	c := globalLoginConfigForTest()
	if err := c.ValidateGlobalAuthority(nil); err == nil {
		t.Fatal("缺环境读取器必须拒绝")
	}
	for _, secret := range []string{"", " \t", strings.Repeat("x", 31), "  " + strings.Repeat("x", 31) + "  "} {
		err := c.ValidateGlobalAuthority(func(key string) string {
			if key != accountdirectory.SecretEnv {
				t.Fatalf("不得读取其它域密钥变量: %s", key)
			}
			return secret
		})
		if err == nil || !strings.Contains(err.Error(), accountdirectory.SecretEnv) {
			t.Fatal("缺失/短账号目录凭据必须在启动拒绝")
		}
		if len(strings.TrimSpace(secret)) > 2 && strings.Contains(err.Error(), strings.TrimSpace(secret)) {
			t.Fatal("错误不得输出凭据值")
		}
	}
	if err := c.ValidateGlobalAuthority(globalDirectoryTestEnv); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalLoginYamlPinsFormalHome(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateGlobalAuthority(globalDirectoryTestEnv); err != nil {
		t.Fatal(err)
	}
	if c.Placement.NewPlayerStorageId != 0 || c.Placement.StorageIDForNewPlayer(c.Node.ZoneId) != c.Node.ZoneId {
		t.Fatal("正规默认必须全球钉落点，storage0仅取已配置Node.ZoneId")
	}
}

func TestGlobalLoginStartupGuardsBeforeExternalWork(t *testing.T) {
	for _, entry := range []struct {
		path, function string
		effects        []string
	}{
		{"../../login.go", "main", []string{"EnsureTopics", "NewServiceContext"}},
		{"../svc/servicecontext.go", "NewServiceContext", []string{"LoadTables", "InitAuthProviders", "NewClient"}},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), entry.path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == entry.function {
				if body != nil {
					t.Fatalf("启动函数不唯一: %s", entry.path)
				}
				body = function.Body
			}
		}
		if body == nil {
			t.Fatalf("缺启动函数: %s", entry.path)
		}
		var guard token.Pos
		calls := map[string]token.Pos{}
		ast.Inspect(body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch function := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = function.Sel.Name
			case *ast.Ident:
				name = function.Name
			}
			if name == "ValidateGlobalAuthority" && guard == token.NoPos {
				guard = call.Pos()
			}
			if old, exists := calls[name]; !exists || call.Pos() < old {
				calls[name] = call.Pos()
			}
			return true
		})
		if guard == token.NoPos {
			t.Fatalf("缺全球Login启动闸: %s", entry.path)
		}
		for _, name := range entry.effects {
			if call, exists := calls[name]; !exists || guard >= call {
				t.Fatalf("原账号目录/Pin校验必须先于%s: %s", name, entry.path)
			}
		}
	}
}
