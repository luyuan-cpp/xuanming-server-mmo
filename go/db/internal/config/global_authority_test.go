package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

func globalConfigForTest() Config {
	c := Config{}
	c.ServerConfig.Database.Backend = "tidb"
	c.Placement.Required = true
	c.Placement.RequireRouteReady = true
	return c
}

func legacyConfigForTest() Config {
	c := Config{LegacyNonGlobal: true}
	c.Mode = service.TestMode
	return c
}

func TestGlobalAuthorityRejectsSilentFallback(t *testing.T) {
	for _, mode := range []string{"", "pro", service.DevMode, service.TestMode} {
		for _, tc := range []struct {
			name   string
			mutate func(*Config)
			key    string
		}{
			{"非法后端档位", func(c *Config) { c.ServerConfig.Database.Backend = "oracle" }, "ServerConfig.Database.Backend"},
			{"落点可缺失", func(c *Config) { c.Placement.Required = false }, "Placement.Required"},
			{"ready关闭", func(c *Config) { c.Placement.RequireRouteReady = false }, "Placement.RequireRouteReady"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				c := globalConfigForTest()
				c.Mode = mode
				if err := c.Validate(); err != nil {
					t.Fatalf("完整全球配置必须可启动: %v", err)
				}
				tc.mutate(&c)
				c.Normalize()
				if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.key) {
					t.Fatalf("必须拒绝%s，Mode不能隐式放宽，错误=%v", tc.key, err)
				}
			})
		}
	}
	if err := (&Config{}).ValidateGlobalAuthority(); err == nil {
		t.Fatal("缺失Placement/数据库段不能被解释为旧非全球默认")
	}
}

// 门禁只看档位是否合法:两档都能过全球门禁,隔离夹具也不能带非法档位(player-data-tidb-scaling-plan.md §9)。
func TestGlobalAuthorityAcceptsEveryLegalBackend(t *testing.T) {
	for _, backend := range []string{"", "tidb", "mysql"} {
		c := globalConfigForTest()
		c.ServerConfig.Database.Backend = backend
		if err := c.ValidateGlobalAuthority(); err != nil {
			t.Fatalf("Backend=%q 应可启动: %v", backend, err)
		}
	}
	c := legacyConfigForTest()
	c.ServerConfig.Database.Backend = "oracle"
	if err := c.ValidateGlobalAuthority(); err == nil || !strings.Contains(err.Error(), "ServerConfig.Database.Backend") {
		t.Fatalf("隔离夹具也必须拒绝非法档位: %v", err)
	}
}

func TestLegacyNonGlobalOnlyExplicitDevelopment(t *testing.T) {
	for _, mode := range []string{"", "pro", "production", "DEV", service.DevMode, service.TestMode} {
		c := Config{LegacyNonGlobal: true}
		c.Mode = mode
		err := c.ValidateGlobalAuthority()
		allowed := mode == service.DevMode || mode == service.TestMode
		if allowed != (err == nil) {
			t.Fatalf("LegacyNonGlobal模式=%q，错误=%v", mode, err)
		}
	}
	for _, mode := range []string{service.DevMode, service.TestMode} {
		c := Config{}
		c.Mode = mode
		if err := c.ValidateGlobalAuthority(); err == nil {
			t.Fatal("dev/test本身不能授权历史降级")
		}
	}
}

func TestGlobalDBYamlAndLoaderRejectDisabledFlags(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(text), &c); err != nil {
		t.Fatal(err)
	}
	if c.LegacyNonGlobal || c.ServerConfig.Database.Backend != "tidb" || !c.Placement.Required || !c.Placement.RequireRouteReady {
		t.Fatal("正规样例必须显式选择全球强SQL与两个落点门禁")
	}
	for _, pair := range []struct{ old, replacement, key string }{
		{"    Backend: tidb", "    Backend: oracle", "ServerConfig.Database.Backend"},
		{"  Required: true", "  Required: false", "Placement.Required"},
		{"  RequireRouteReady: true", "  RequireRouteReady: false", "Placement.RequireRouteReady"},
	} {
		if strings.Count(text, pair.old) != 1 {
			t.Fatalf("样例键必须唯一: %s", pair.key)
		}
		var rejected Config
		err := conf.LoadFromYamlBytes([]byte(strings.Replace(text, pair.old, pair.replacement, 1)), &rejected)
		if err == nil || !strings.Contains(err.Error(), pair.key) {
			t.Fatalf("加载阶段必须拒绝%s: %v", pair.key, err)
		}
	}
}

func TestGlobalDBStartupGuardsBeforeExternalWork(t *testing.T) {
	for _, entry := range []struct {
		path, function, guard string
		effects               []string
	}{
		{"../../db.go", "main", "Validate", []string{"NewServiceContext"}},
		{"../svc/servicecontext.go", "NewServiceContext", "ValidateGlobalAuthority", []string{"NewClient", "Ping"}},
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
			if name == entry.guard && guard == token.NoPos {
				guard = call.Pos()
			}
			if old, exists := calls[name]; !exists || call.Pos() < old {
				calls[name] = call.Pos()
			}
			return true
		})
		if guard == token.NoPos {
			t.Fatalf("缺必需启动闸%s: %s", entry.guard, entry.path)
		}
		for _, name := range entry.effects {
			if call, exists := calls[name]; !exists || guard >= call {
				t.Fatalf("%s必须先于外部/消费步骤%s: %s", entry.guard, name, entry.path)
			}
		}
	}
}
