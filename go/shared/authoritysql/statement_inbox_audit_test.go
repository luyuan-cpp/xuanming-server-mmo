package authoritysql

import (
	"fmt"
	"strings"
	"testing"

	dbpb "proto/db"
	inboxpb "proto/player_inbox"

	"google.golang.org/protobuf/proto"
)

// 使用当前生成描述符中的真实audit选项；不维护第二份完整业务表结构。
// proto2mysql v0.2.0的tidbTableOptionsSQL在PreSplit为0时生成单独Shard块。
// 此处只保留权限包络相关的最小DDL表达，不连接数据库，也不新增生成器依赖。
func currentInboxAuditShardDDL(t *testing.T) string {
	t.Helper()
	options := (&inboxpb.PlayerInboxAudit{}).ProtoReflect().Descriptor().Options()
	table := proto.GetExtension(options, dbpb.E_OptionTableName).(string)
	shard := proto.GetExtension(options, dbpb.E_OptionTiDBShardRowIDBits).(uint32)
	preSplit := proto.GetExtension(options, dbpb.E_OptionTiDBPreSplitRegions).(uint32)
	nonclustered := proto.GetExtension(options, dbpb.E_OptionTiDBNonclusteredPK).(bool)
	if table != "player_inbox_audit" || shard != 4 || preSplit != 0 || !nonclustered {
		t.Fatal("当前audit描述符已偏离单Shard4的回归前提，须重新审定迁移表达")
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` (`shard` INT UNSIGNED NOT NULL, PRIMARY KEY (`shard`) /*T![clustered_index] NONCLUSTERED */) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=%d */", table, shard)
}

func TestSchemaPermissionAcceptsExistingInboxAuditShardBits4(t *testing.T) {
	if err := validatePermissionStatement(currentInboxAuditShardDDL(t), permissionSchema); err != nil {
		t.Fatalf("现有audit单Shard4迁移表达被Schema能力拒绝: %v", err)
	}
}

func TestInboxAuditShardDDLDoesNotBroadenNonSchemaPermissions(t *testing.T) {
	query := currentInboxAuditShardDDL(t)
	for _, tc := range []struct {
		name string
		kind permissionKind
	}{
		{"reader", permissionReader},
		{"writer", permissionWriter},
		{"operator", permissionOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validatePermissionStatement(query, tc.kind) == nil {
				t.Fatal("非Schema能力越界接受audit迁移DDL")
			}
		})
	}
}

func TestSchemaAuditShardAnnotationRejectsUnreviewedExecutableComments(t *testing.T) {
	query := currentInboxAuditShardDDL(t)
	for _, tc := range []struct {
		name, annotation string
	}{
		{"非4零值", "/*T! SHARD_ROW_ID_BITS=0 */"},
		{"非4正值", "/*T! SHARD_ROW_ID_BITS=5 */"},
		{"单独预切", "/*T! PRE_SPLIT_REGIONS=4 */"},
		{"未审预切组合", "/*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=5 */"},
		{"未知选项", "/*T! UNKNOWN_OPTION=4 */"},
		{"追加未知选项", "/*T! SHARD_ROW_ID_BITS=4 UNKNOWN_OPTION=1 */"},
		{"任意TiDB执行内容", "/*T! ; DROP TABLE other_table */"},
		{"MySQL执行注释", "/*!80000 SET autocommit=0 */"},
		{"MariaDB执行注释", "/*M! SHARD_ROW_ID_BITS=4 */"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := strings.Replace(query, "/*T! SHARD_ROW_ID_BITS=4 */", tc.annotation, 1)
			if validatePermissionStatement(candidate, permissionSchema) == nil {
				t.Fatal("Schema能力接受了未审定的可执行注释")
			}
		})
	}
}
