package store

import (
	"strings"
	"testing"

	"github.com/luyuancpp/proto2mysql"
)

// 直接用本服务的真实描述符生成 DDL,防止代理缓存或依赖回退静默丢掉 TiDB 选项。
// bootstrapTables 里的表(id_segment、player_name)走手写 DDL,proto2mysql 生成的建表语句
// 对它们不作数;这里只验证它们能通过迁移前的表名守卫。
//
// 豁免判据必须用同包的 isBootstrapTable,不能写成 `name == IdSegmentTableName`:
// 后者在 player_name 进 TableMessages 的那一刻就把本测试打成 Fatal(primaryKeys 里查不到),
// 而真正该被这条断言拦住的是"新增**自动迁移**表忘了补主键与 TiDB 契约"。
// 表名清单只有 schema.go 一处答案,测试跟着它走,以后再加 bootstrap 表不用回来改这里。
func TestGlobalTableDDLContract(t *testing.T) {
	primaryKeys := map[string]string{
		"transaction_log":    "tx_id",
		"player_snapshot":    "id",
		"rollback_audit_log": "id",
	}
	for _, table := range TableMessages() {
		name, ok := proto2mysql.TableNameFromDescriptor(table.ProtoReflect().Descriptor())
		if !ok || name == "" {
			t.Fatalf("全局表 %s 缺少明确表名", table.ProtoReflect().Descriptor().FullName())
		}
		t.Run(name, func(t *testing.T) {
			model := proto2mysql.NewDB()
			model.RegisterTable(table)
			if err := assertTableNameLocked(model, table); err != nil {
				t.Fatal(err)
			}
			if isBootstrapTable(name) {
				return
			}
			key, ok := primaryKeys[name]
			if !ok {
				t.Fatalf("新增自动迁移表 %q 必须补齐主键与 TiDB 契约", name)
			}
			ddl := model.GetCreateTableSQL(table)
			for _, want := range []string{
				"PRIMARY KEY (`" + key + "`)",
				"/*T![clustered_index] NONCLUSTERED */",
				"SHARD_ROW_ID_BITS=4",
				"PRE_SPLIT_REGIONS=4",
			} {
				if !strings.Contains(ddl, want) {
					t.Errorf("建表语句丢失 %q:\n%s", want, ddl)
				}
			}
		})
	}
}
