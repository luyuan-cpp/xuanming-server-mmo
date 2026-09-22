package data

// point_lock_order_test.go —— "带复核条件的写之前必有主键点锁"的静态回归(死锁复核 V1 / V2,不连库,任何环境都跑)。
//
// # 为什么要有它
//
// TiDB 悲观事务里,带复核谓词的 UPDATE / DELETE(`op_id = ? AND status = ? …`、`… AND expire_ms <= ?`)不走 Point_Get
// 快路径,执行期间只读不锁,到语句末尾才把 {行 key, PRIMARY key(非聚簇主键), 唯一 key} 按 region 并行加悲观锁,批内顺序
// 应用控制不了;同一行上的另一个写者若以 Point_Get(先 PRIMARY key 后行 key)或另一条这样的语句取锁,双方可能各持一半互等。
// 修法是在同一事务里、写之前先跑一条"完整主键等值 + FOR UPDATE、不带复核条件"的点锁,让所有写者先在 PRIMARY key 上排队
// (asset_store.go 文件头 TiDB 附加规则、guild_manage_repo.go 文件头 (e))。MySQL 下点锁与随后的写锁同一条聚簇记录,锁集不变。
//
// 这条纪律 EXPLAIN 看不出来(它只看单条语句的计划,不看语句先后),MySQL 上的并发用例也撞不出来(MySQL 本来就不成环),
// TiDB 上的并发用例只能按概率撞 —— 所以在这里对生产源码做确定性的结构检查:在指定函数体内,按源码顺序,
// 点锁常量的第一次出现必须早于写语句常量的第一次出现。函数体是直线代码或"循环内先锁后写",源码顺序就是执行顺序。
// 判据刻意做轻:只认标识符出现的先后,不解释控制流;有人把点锁挪到写之后、删掉点锁、或改了函数 / 常量名,这里都会红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pointLockOrderCase:file 里名为 fn 的函数(或方法)体内,order 中的标识符必须按给定顺序第一次出现。
type pointLockOrderCase struct {
	file  string
	fn    string
	order []string
}

func TestPointLockPrecedesConditionalWrites(t *testing.T) {
	cases := []pointLockOrderCase{
		// V1:guild_asset_op 的四类悲观写者。
		{"economy_repo.go", "accelerateDonationDeadlines", []string{"sqlLockAssetOp", "sqlAccelerateDonationDeadline"}},
		// 终结 / 人工终结共用 terminate:对侧账的行(锁序靠前的表)→ op 行点锁 → CAS(casQuery 是 Finalize / ResolveManually 传入的 CAS 语句)。
		{"asset_store.go", "terminate", []string{"lockCounterparty", "sqlLockAssetOp", "casQuery"}},
		{"asset_store.go", "Reschedule", []string{"sqlLockAssetOp", "sqlRescheduleAssetOp"}},
		{"asset_store.go", "markPoison", []string{"sqlLockAssetOp", "sqlPoisonAssetOp"}},
		// 清理短事务(C5):点锁语句由调用方以 lockQuery 传入,点删以 query 传入。
		{"asset_store.go", "execCleanupDelete", []string{"lockRowExists", "ExecContext"}},
		{"asset_store.go", "cleanupTerminalOpsBatch", []string{"sqlLockAssetOp", "sqlCleanupTerminalOp"}},
		{"asset_store.go", "cleanupCountersBatch", []string{"sqlLockCleanupCounter", "sqlCleanupCounter"}},
		// V2:guild_application 上带 expire_ms 复核的点删。
		{"guild_manage_repo.go", "purgeExpiredApplicationsOfPlayer", []string{"sqlSelectApplicationExpire", "sqlDeleteExpiredApplication"}},
		{"guild_manage_repo.go", "purgeExpiredApplicationsOfGuild", []string{"sqlSelectApplicationExpire", "sqlDeleteExpiredApplication"}},
		{"guild_manage_repo.go", "CancelApplication", []string{"sqlSelectApplicationExpire", "sqlCancelApplication", "sqlDeleteExpiredApplication"}},
	}

	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			file, ok := parsed[tc.file]
			if !ok {
				var err error
				file, err = parser.ParseFile(fset, tc.file, nil, parser.SkipObjectResolution)
				require.NoError(t, err, "go test 的工作目录是包目录,%s 必须能直接读到", tc.file)
				parsed[tc.file] = file
			}
			body := pointLockFuncBody(t, file, tc.fn)
			first := pointLockFirstIdents(body, tc.order)
			for i, name := range tc.order {
				pos, found := first[name]
				require.True(t, found, "%s.%s 里找不到 %s:点锁 / 写语句被删或改名了,先回来确认新写法仍是\"先点锁、后写\"",
					tc.file, tc.fn, name)
				if i == 0 {
					continue
				}
				prev := tc.order[i-1]
				assert.Less(t, first[prev], pos, "%s.%s:%s 必须先于 %s(%s 在 %s,%s 在 %s)",
					tc.file, tc.fn, prev, name, prev, fset.Position(first[prev]), name, fset.Position(pos))
			}
		})
	}
}

// TestPointLockStatementsAreFastPathShape:点锁语句只许"完整主键等值 + FOR UPDATE",不带任何复核条件 ——
// 带了条件 TiDB 就不走 Point_Get 快路径,点锁自己又成了语句末尾并行加锁的那种写者。
// sqlLockAssetOp / sqlLockCleanupCounter / sqlLockSeqGuard 的形状由 economy_repo_test.go 的
// TestEconomyCandidateReadsTakeNoLocks 钉着,这里补申请表那一条。
func TestPointLockStatementsAreFastPathShape(t *testing.T) {
	assert.Equal(t,
		"SELECT expire_ms FROM guild_application WHERE guild_id = ? AND player_id = ? FOR UPDATE",
		sqlSelectApplicationExpire)
	// 撤回的兜底点删带 `expire_ms <= ?` 复核:锁在手里时它与 sqlCancelApplication 的 `expire_ms > ?` 恰好互补,
	// 不会误删一条活申请。
	assert.True(t, strings.HasSuffix(sqlDeleteExpiredApplication, "AND expire_ms <= ?"), sqlDeleteExpiredApplication)
	assert.True(t, strings.HasSuffix(sqlCancelApplication, "AND expire_ms > ?"), sqlCancelApplication)
}

// pointLockFuncBody 找 file 里名为 name 的顶层函数或方法,要求恰好一个。
func pointLockFuncBody(t *testing.T, file *ast.File, name string) *ast.BlockStmt {
	t.Helper()
	var bodies []*ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			bodies = append(bodies, fn.Body)
		}
	}
	require.Len(t, bodies, 1, "函数 %s 应恰好一个", name)
	return bodies[0]
}

// pointLockFirstIdents 返回 names 中每个标识符在 body 内(含闭包)第一次出现的位置。
// 选择器 x.ExecContext 的 Sel 也是 *ast.Ident,所以方法名同样按出现位置计。
func pointLockFirstIdents(body *ast.BlockStmt, names []string) map[string]token.Pos {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	first := make(map[string]token.Pos, len(names))
	ast.Inspect(body, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || !want[ident.Name] {
			return true
		}
		if pos, seen := first[ident.Name]; !seen || ident.Pos() < pos {
			first[ident.Name] = ident.Pos()
		}
		return true
	})
	return first
}
