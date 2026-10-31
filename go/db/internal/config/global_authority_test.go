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
	return c
}

// 非法档位一律拒启,Mode 不放宽。
func TestGlobalAuthorityRejectsIllegalBackend(t *testing.T) {
	for _, mode := range []string{"", "pro", service.DevMode, service.TestMode} {
		t.Run(mode, func(t *testing.T) {
			c := globalConfigForTest()
			c.Mode = mode
			if err := c.Validate(); err != nil {
				t.Fatalf("完整配置必须可启动: %v", err)
			}
			c.ServerConfig.Database.Backend = "oracle"
			c.Normalize()
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ServerConfig.Database.Backend") {
				t.Fatalf("必须拒绝非法档位,错误=%v", err)
			}
		})
	}
}

// 门禁只看档位是否合法:两档都能过(player-data-tidb-scaling-plan.md §9)。
func TestGlobalAuthorityAcceptsEveryLegalBackend(t *testing.T) {
	for _, backend := range []string{"", "tidb", "mysql"} {
		c := globalConfigForTest()
		c.ServerConfig.Database.Backend = backend
		if err := c.ValidateGlobalAuthority(); err != nil {
			t.Fatalf("Backend=%q 应可启动: %v", backend, err)
		}
	}
}

func TestGlobalDBYamlAndLoaderRejectIllegalBackend(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(text), &c); err != nil {
		t.Fatal(err)
	}
	if c.ServerConfig.Database.Backend != "tidb" {
		t.Fatal("正规样例必须显式选择全球强SQL")
	}
	const old = "    Backend: tidb"
	if strings.Count(text, old) != 1 {
		t.Fatal("样例键必须唯一: ServerConfig.Database.Backend")
	}
	var rejected Config
	err = conf.LoadFromYamlBytes([]byte(strings.Replace(text, old, "    Backend: oracle", 1)), &rejected)
	if err == nil || !strings.Contains(err.Error(), "ServerConfig.Database.Backend") {
		t.Fatalf("加载阶段必须拒绝非法档位: %v", err)
	}
}

func TestGlobalDBStartupGuardsBeforeExternalWork(t *testing.T) {
	for _, entry := range []struct {
		path, function, guard string
		effects               []string
	}{
		{"../../db.go", "main", "Validate", []string{"NewServiceContext", "InitDB"}},
		{"../svc/servicecontext.go", "NewServiceContext", "ValidateGlobalAuthority", nil},
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
