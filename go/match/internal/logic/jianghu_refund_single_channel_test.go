package logic

// 生死状普通战书银两只由 scene 退(数据覆写审计 r2 第 4 轮,群仙问道赛设计 V1 第七节「生死状退费」):
// match 不发退费邮件,解冻(CancelBattlePrepare)也从不要求 scene 跳过退还。原来 gather 失败时 match 先发退费邮件,
// 邮件结果不明就删标记、再以 skip=false 让 scene 在线退,邮件其实已建时就是双退。
// 真跑 runFormationGather 要真 TiDB 加桩 scene / battle,这里按源码结构核对(同 gather_original_test.go 的 AST 守卫做法)。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJianghuSilverRefundHasOnlyTheSceneChannel(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	mailCallers := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "解析 %s 失败", name)
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok && key.Name == "SkipJianghuSilverRefund" {
					t.Errorf("%s 给 CancelBattlePrepareRequest 置了 SkipJianghuSilverRefund:退费只有 scene 一条通道,match 不能让 scene 跳过", fset.Position(x.Pos()))
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "SendPersonalMail" {
					mailCallers[name] = true
				}
			}
			return true
		})
	}
	require.Equal(t, map[string]bool{"tournamentlogic.go": true}, mailCallers,
		"match 唯一的发信点是群仙问道赛领奖;生死状退费不经邮件,别的玩法要发信先在设计稿里写清结果不明时的处理")
}
