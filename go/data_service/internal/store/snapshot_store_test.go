package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	dbpb "proto/common/database"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
)

// InsertSnapshotIfGuidAbsent 的就地死锁重试(审计 #11)的纯逻辑部分。真库上"死锁确实撞上、并被就地吸收"
// 的确定性交错见 snapshot_store_integration_test.go(-tags=integration)。

func mysqlErrNo(n uint16) error {
	return &mysql.MySQLError{Number: n, Message: "injected by unit test"}
}

func noBackoff() time.Duration { return 0 }

func TestRetryOnDeadlock_RerunsDeadlockUntilSuccess(t *testing.T) {
	calls := 0
	reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
		calls++
		if calls < 3 {
			return mysqlErrNo(mysqlErrDeadlock)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("两次 1213 之后第三次成功,应返回 nil,got %v", err)
	}
	if calls != 3 || reruns != 2 {
		t.Fatalf("应执行 3 次、重跑 2 次,got calls=%d reruns=%d", calls, reruns)
	}
}

func TestRetryOnDeadlock_BoundedByAttempts(t *testing.T) {
	calls := 0
	reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
		calls++
		return mysqlErrNo(mysqlErrDeadlock)
	})
	if !isDeadlockMySQL(err) {
		t.Fatalf("预算用尽时必须把最后一次的 1213 原样交给调用方(消费者据此做通用重试),got %v", err)
	}
	if calls != 3 || reruns != 2 {
		t.Fatalf("上限 3 次:应执行 3 次、重跑 2 次,got calls=%d reruns=%d", calls, reruns)
	}
}

// 1205 刻意不就地重试:它已经白等了一整个 innodb_lock_wait_timeout,就地再等会把消费者的最坏等待放大数倍。
func TestRetryOnDeadlock_DoesNotRerunLockWaitTimeoutOrOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"1205 锁等待超时", mysqlErrNo(mysqlErrLockWaitTimeout)},
		{"1062 重复键", mysqlErrNo(mysqlErrDupEntry)},
		{"非 MySQL 错误", errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			reruns, err := retryOnDeadlock(context.Background(), 3, noBackoff, func() error {
				calls++
				return tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("应原样返回 %v,got %v", tc.err, err)
			}
			if calls != 1 || reruns != 0 {
				t.Fatalf("非 1213 不许重跑:got calls=%d reruns=%d", calls, reruns)
			}
		})
	}
}

func TestRetryOnDeadlock_BackoffIsCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	// 退避给一小时:若等待不可取消,本用例会挂到 go test 超时,而不是靠墙钟断言。
	reruns, err := retryOnDeadlock(ctx, 3, func() time.Duration { return time.Hour }, func() error {
		calls++
		return mysqlErrNo(mysqlErrDeadlock)
	})
	if calls != 1 || reruns != 0 {
		t.Fatalf("ctx 已取消时不应再重跑:got calls=%d reruns=%d", calls, reruns)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应带上 ctx 的取消原因,got %v", err)
	}
	if !isDeadlockMySQL(err) {
		t.Fatalf("错误应同时保留最后一次的 1213,调用方才知道为什么在等,got %v", err)
	}
}

func TestSnapshotGuidDeadlockBackoffWithinBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := snapshotGuidDeadlockBackoff()
		if d < snapshotGuidDeadlockBackoffMin || d > snapshotGuidDeadlockBackoffMax {
			t.Fatalf("第 %d 次退避 %v 超出 [%v, %v]", i, d, snapshotGuidDeadlockBackoffMin, snapshotGuidDeadlockBackoffMax)
		}
	}
}

// 两层重试叠加的预算:就地重试必须至少能重跑一次(否则 1213 照旧冒泡成消费者 1s 重试 + ERROR),
// 而最坏多睡的时长必须远小于消费者的 1s 通用退避(见 snapshotGuidDeadlockBackoffMax 的注释)。
func TestSnapshotGuidInPlaceRetryBudget(t *testing.T) {
	if snapshotGuidInsertAttempts < 2 {
		t.Fatalf("snapshotGuidInsertAttempts=%d:至少要 2 次才能就地吸收一次 1213", snapshotGuidInsertAttempts)
	}
	worst := time.Duration(snapshotGuidInsertAttempts-1) * snapshotGuidDeadlockBackoffMax
	if worst > 100*time.Millisecond {
		t.Fatalf("就地重试最坏多睡 %v,超过 100ms 预算:会与消费者 1s×DBMaxAttempts 的通用重试叠加出超预算等待", worst)
	}
}

// TestLockErrorClassifiersShareOneSource:1213 / 1205 在本包只有一个权威定义(id_segment_store.go 的常量),
// 两个分类器各自的口径也钉在这里 —— isRetryableMySQL 认 1213 + 1205,isDeadlockMySQL 只认 1213。
func TestLockErrorClassifiersShareOneSource(t *testing.T) {
	if mysqlErrDeadlock != 1213 || mysqlErrLockWaitTimeout != 1205 {
		t.Fatalf("错误号常量被改了:deadlock=%d lock_wait_timeout=%d", mysqlErrDeadlock, mysqlErrLockWaitTimeout)
	}
	for _, tc := range []struct {
		name                string
		err                 error
		retryable, deadlock bool
	}{
		{"1213", mysqlErrNo(mysqlErrDeadlock), true, true},
		{"1205", mysqlErrNo(mysqlErrLockWaitTimeout), true, false},
		{"1062", mysqlErrNo(mysqlErrDupEntry), false, false},
		{"非 MySQL 错误", errors.New("boom"), false, false},
	} {
		if got := isRetryableMySQL(tc.err); got != tc.retryable {
			t.Errorf("isRetryableMySQL(%s)=%v,want %v", tc.name, got, tc.retryable)
		}
		if got := isDeadlockMySQL(tc.err); got != tc.deadlock {
			t.Errorf("isDeadlockMySQL(%s)=%v,want %v", tc.name, got, tc.deadlock)
		}
	}
}

// TestJitteredBackoffDegenerateInterval:区间退化(hi <= lo)时返回 lo,不 panic。
func TestJitteredBackoffDegenerateInterval(t *testing.T) {
	for _, hi := range []time.Duration{10 * time.Millisecond, 5 * time.Millisecond, 0} {
		if d := jitteredBackoff(10*time.Millisecond, hi); d != 10*time.Millisecond {
			t.Fatalf("jitteredBackoff(10ms, %v)=%v,want 10ms", hi, d)
		}
	}
}

// ── 审计 #11 第二步:可空唯一列(snapshot_guid_nz + uk_snapshot_guid_nz)的纯逻辑守卫 ──
//
// 真库上"键真的在、EXPLAIN 真的走它、并发真的只落一行"的证据在
// snapshot_store_integration_test.go(-tags=integration)。这里钉的是不连库也必须成立的判据:
// 写入语句的形状、以及"proto2mysql 的列 / 索引同步不会碰这一列与这个键"的那几条前提。

// TestNullableUniqueKeyRegistryMatchesExportedNames:清单里的表名 / 列名 / 键名必须与对外常量一致。
// store 与真库用例都按常量引用它们,清单里写歪一个字母,症状是"迁移建了个谁也不认识的键"。
func TestNullableUniqueKeyRegistryMatchesExportedNames(t *testing.T) {
	var found int
	for _, k := range nullableUniqueKeys {
		if k.table != PlayerSnapshotTableName {
			continue
		}
		found++
		if k.column != SnapshotGuidNzColumn {
			t.Errorf("生成列名 %q != SnapshotGuidNzColumn %q", k.column, SnapshotGuidNzColumn)
		}
		if k.indexName != SnapshotGuidUniqueKey {
			t.Errorf("唯一键名 %q != SnapshotGuidUniqueKey %q", k.indexName, SnapshotGuidUniqueKey)
		}
		if k.sourceColumn != "snapshot_guid" {
			t.Errorf("源列 %q != snapshot_guid", k.sourceColumn)
		}
		if k.zeroValue != 0 {
			t.Errorf("GM 行的 guid 是 0,zeroValue=%d 会把它当成要约束的值", k.zeroValue)
		}
		for _, want := range []string{k.column, k.sourceColumn, k.table} {
			if !strings.Contains(k.columnDDL, want) {
				t.Errorf("列 DDL 里没有 %q:\n%s", want, k.columnDDL)
			}
		}
		for _, want := range []string{k.indexName, k.column, k.table} {
			if !strings.Contains(k.indexDDL, want) {
				t.Errorf("唯一键 DDL 里没有 %q:\n%s", want, k.indexDDL)
			}
		}
	}
	if found != 1 {
		t.Fatalf("player_snapshot 应恰好有一条可空唯一列登记,got %d", found)
	}
}

// TestSnapshotGuidNzColumnDDLShape 钉住生成列 DDL 的几个要点,每一个都对着一种事故形态:
//   - NULLIF(snapshot_guid, 0):没有它,GM 行(guid 恒为 0)第二条就撞唯一键;
//   - STORED:VIRTUAL 与 STORED 在 TiDB 上的 ALTER 能力不同,改动必须是有意识的(见 schema.go 的注释);
//   - 不写 NOT NULL:NULL 才不被唯一键约束,GM 行靠这一点共存;
//   - 列注释**不以 pb: 开头**:pb:N 会让 proto2mysql 把它当成某个 proto 字段的列而 CHANGE COLUMN 改名改类型。
func TestSnapshotGuidNzColumnDDLShape(t *testing.T) {
	ddl := snapshotGuidNzColumnDDL
	for _, want := range []string{"NULLIF(`snapshot_guid`, 0)", "GENERATED ALWAYS AS", "STORED"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("生成列 DDL 缺少 %q:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "NOT NULL") {
		t.Errorf("生成列必须可空(NULL 才不被唯一键约束,GM 行靠这一点共存):\n%s", ddl)
	}
	if strings.Contains(ddl, "pb:") {
		t.Errorf("生成列的注释不能带 pb:N —— proto2mysql 按它认字段号,会把这一列当成 proto 字段来改:\n%s", ddl)
	}
}

// TestSnapshotGuidNzSurvivesProtoSync:proto2mysql 的列 / 索引同步不会删掉或改掉这一列与这个键。
// 逐条钉住 schema.go 头注释里列的那几个前提 —— 它们是"为什么这条手写 DDL 能与 proto 驱动的表共存"的全部依据。
func TestSnapshotGuidNzSurvivesProtoSync(t *testing.T) {
	snapshot := &dbpb.PlayerSnapshot{}
	name, ok := proto2mysql.TableNameFromDescriptor(snapshot.ProtoReflect().Descriptor())
	if !ok || name != PlayerSnapshotTableName {
		t.Fatalf("player_snapshot 的 OptionTableName 变了:%q(ok=%v)", name, ok)
	}

	// 1) 生成列不是 proto 字段 —— 所以 buildColumnClauses 既不会 MODIFY 它,也不会与它抢字段号。
	fields := snapshot.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if got := string(fields.Get(i).Name()); got == SnapshotGuidNzColumn {
			t.Fatalf("proto 里出现了字段 %s:它是索引形态而不是业务字段,进 proto 会让 C++/Go 两侧都以为要填它,"+
				"同时让 proto2mysql 接管这一列(手写 DDL 与同步会互相打架)", SnapshotGuidNzColumn)
		}
	}

	// 2) 生成的建表语句里不该出现这一列与这个键 —— 新库由 proto 建表,然后由 ensureNullableUniqueKeys 追加。
	model := proto2mysql.NewDB()
	model.RegisterTable(snapshot)
	ddl := model.GetCreateTableSQL(snapshot)
	for _, unwanted := range []string{SnapshotGuidNzColumn, SnapshotGuidUniqueKey} {
		if strings.Contains(ddl, unwanted) {
			t.Errorf("proto 生成的建表语句里已经有 %q —— 说明它被搬进了 proto,手写 DDL 这一步应随之删掉:\n%s", unwanted, ddl)
		}
	}

	// 3) ensureIndexes 的 wanted 清单里不该出现这个键(它不由 proto 驱动),
	//    且清单里"真正需要的" idx(snapshot_guid) 不会被本键冒名覆盖。
	wanted := parseWantedIndexes(ddl)
	wanted = append(wanted, extraIndexes[PlayerSnapshotTableName]...)
	var guidWanted bool
	for _, w := range wanted {
		if w.name == SnapshotGuidUniqueKey {
			t.Errorf("parseWantedIndexes 解出了 %s:唯一键不该由 proto 驱动", SnapshotGuidUniqueKey)
		}
		if len(w.cols) == 1 && w.cols[0] == "snapshot_guid" {
			guidWanted = true
		}
	}
	if !guidWanted {
		t.Fatalf("proto 不再声明 idx(snapshot_guid):去重迁移与 GM 读路径会退化成全表扫,先确认这是有意的\n%s", ddl)
	}
	// 只有唯一键存在时,idx(snapshot_guid) 仍必须被判为"缺失"而补建 —— 两列名字只差后缀,
	// indexCovered 若按前缀误判,真正需要的索引就永远不会被建出来。
	onlyUnique := map[string][]string{SnapshotGuidUniqueKey: {SnapshotGuidNzColumn}}
	if indexCovered(onlyUnique, []string{"snapshot_guid"}) {
		t.Errorf("%s(%s) 被误判为覆盖了 idx(snapshot_guid)", SnapshotGuidUniqueKey, SnapshotGuidNzColumn)
	}
	// 反过来也要成立:有了 idx(snapshot_guid) 不代表有唯一键,ensureNullableUniqueKey 必须按键名单独判。
	onlyPlain := map[string][]string{"idx_player_snapshot_2": {"snapshot_guid"}}
	if indexCovered(onlyPlain, []string{SnapshotGuidNzColumn}) {
		t.Errorf("idx(snapshot_guid) 被误判为覆盖了 %s", SnapshotGuidNzColumn)
	}

	// 4) player_snapshot 不是 bootstrap 表:assertColumnsPresent / assertUniqueColumns 不作用于它,
	//    不会因为"proto 里没有 snapshot_guid_nz"而拒绝迁移。
	if isBootstrapTable(PlayerSnapshotTableName) {
		t.Fatalf("player_snapshot 进了 bootstrapTables:那会连带停掉它的列同步,proto 以后加字段就补不上列了")
	}
}

// TestSnapshotDedupeBudget:批大小不得超过单次上限,否则一批就能越界,分批与上限一起失去意义。
func TestSnapshotDedupeBudget(t *testing.T) {
	if snapshotGuidDedupeGuidsPerBatch <= 0 {
		t.Fatalf("批大小 %d 非正:去重循环会空转", snapshotGuidDedupeGuidsPerBatch)
	}
	if snapshotGuidDedupeGuidsPerBatch > snapshotGuidDedupeMaxRows {
		t.Fatalf("批大小 %d 超过单次上限 %d", snapshotGuidDedupeGuidsPerBatch, snapshotGuidDedupeMaxRows)
	}
}

// TestGuidDedupeStatementFollowsProbe:探到唯一键 → 普通 INSERT + 按唯一键点查;探不到 → NOT EXISTS + 按普通索引点查。
// 占位符个数与实参个数必须相等 —— 两条语句的实参不同(NOT EXISTS 多绑一次 guid),错一个就是运行期
// "Column count doesn't match",且只在退路那条分支上才出现。
func TestGuidDedupeStatementFollowsProbe(t *testing.T) {
	row := &SnapshotRow{PlayerID: 1, ZoneID: 2, SnapshotType: 3, CreatedAt: 4, Reason: "r", Operator: "o",
		Data: []byte("d"), SnapshotGuid: 777, Source: SnapshotSourceSceneKafka}

	for _, tc := range []struct {
		name       string
		hasKey     bool
		wantInsert string
		wantLookup string
	}{
		{"有唯一键", true, insertSnapshotOnDuplicateKeepSQL, lookupSnapshotIDByGuidNzSQL},
		{"无唯一键", false, insertSnapshotIfGuidAbsentLegacySQL, lookupSnapshotIDByGuidSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &SnapshotStore{uniqueGuidKey: tc.hasKey}
			query, args := s.guidDedupeStatement(row)
			if query != tc.wantInsert {
				t.Fatalf("选错了写入语句:\n%s", query)
			}
			if got, want := strings.Count(query, "?"), len(args); got != want {
				t.Fatalf("占位符 %d 个,实参 %d 个:\n%s", got, want, query)
			}
			if got := s.guidLookupStatement(); got != tc.wantLookup {
				t.Fatalf("选错了点查语句:\n%s", got)
			}
			if got := strings.Count(s.guidLookupStatement(), "?"); got != 1 {
				t.Fatalf("点查语句应恰好一个占位符(guid),got %d", got)
			}
		})
	}

	// 普通 INSERT 与 NOT EXISTS 的前九个实参必须一致:两条语句列清单相同,顺序只在 snapshotInsertArgs 定义一次。
	base := snapshotInsertArgs(row)
	if len(base) != 9 {
		t.Fatalf("列清单是 9 列,snapshotInsertArgs 给了 %d 个", len(base))
	}
	_, legacyArgs := (&SnapshotStore{}).guidDedupeStatement(row)
	if len(legacyArgs) != len(base)+1 || legacyArgs[len(legacyArgs)-1] != any(row.SnapshotGuid) {
		t.Fatalf("NOT EXISTS 形态应在九个实参之后再绑一次 guid,got %v", legacyArgs)
	}
}

// ── 唯一键路径的写入语句形态(审计复审:S→X 升级环用 ODKU 消掉,不留给重试)──────────────

// TestInsertSnapshotOnDuplicateKeepSQLShape 钉住 ODKU 那条语句的三个要点,每一个都对着一种事故形态:
//   - 与普通 INSERT 共用同一段列清单文本:各写一份就会在加列时漏掉一处;
//   - `ON DUPLICATE KEY UPDATE`:少了它,重复键检查取 S 而不是 X,手册三会话例那个死锁环就回来了
//     (见 insertSnapshotOnDuplicateKeepSQL 的注释与手册 innodb-locks-set);
//   - 赋的是 `id = LAST_INSERT_ID(id)`:值不变(affected-rows=0,快照保持不可变),
//     同时把已存在那行的 id 经 OK 包回带。写成别的列或别的值,就成了一次真实修改。
func TestInsertSnapshotOnDuplicateKeepSQLShape(t *testing.T) {
	stmt := insertSnapshotOnDuplicateKeepSQL
	if !strings.HasPrefix(stmt, insertSnapshotSQL) {
		t.Fatalf("ODKU 语句必须由 insertSnapshotSQL 接出来(列清单只定义一次):\n%s", stmt)
	}
	if !strings.Contains(stmt, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("少了 ON DUPLICATE KEY UPDATE:重复键检查会退回 S 锁,同 guid 的排队者重新成环:\n%s", stmt)
	}
	if !strings.Contains(stmt, "`id` = LAST_INSERT_ID(`id`)") {
		t.Fatalf("撞键时必须只写 `id` = LAST_INSERT_ID(`id`)(值不变 + 回带已有 id);"+
			"赋成别的值就是一次真实修改,快照不再是不可变记录:\n%s", stmt)
	}
	// 撞键分支不许多出占位符:多一个就会与 snapshotInsertArgs 的九个实参对不上,
	// 且只在"真的撞键"时才暴露成运行期 Column count doesn't match。
	if got, want := strings.Count(stmt, "?"), strings.Count(insertSnapshotSQL, "?"); got != want {
		t.Fatalf("ODKU 段不该引入新的占位符:got %d,want %d\n%s", got, want, stmt)
	}
}

// TestSnapshotDSNKeepsAffectedRowsSemantics:affected-rows 的判据依赖连接**没有**打开 CLIENT_FOUND_ROWS。
//
// 手册 "INSERT ... ON DUPLICATE KEY UPDATE Statement":affected-rows 是 1 = 新插入、
// 0 = "an existing row is set to its current values";但"If you specify the CLIENT_FOUND_ROWS flag ...
// the affected-rows value is 1 (not 0) if an existing row is set to its current values"。
// 一旦有人给 DSN 加上 clientFoundRows=true,InsertSnapshotIfGuidAbsent 的 `affected == 1 → inserted`
// 就会把**重放**判成新插入:调用方拿到 inserted=true 与一个不存在的 id,而库里一行没变,全程零报错。
func TestSnapshotDSNKeepsAffectedRowsSemantics(t *testing.T) {
	dsn := buildDSN(MySQLConfig{Host: "h:3306", User: "u", Password: "p", DBName: "d"})
	if strings.Contains(strings.ToLower(dsn), "clientfoundrows") {
		t.Fatalf("DSN 打开了 CLIENT_FOUND_ROWS:ODKU 的重放会被 affected==1 误判成新插入,"+
			"见 insertSnapshotOnDuplicateKeepSQL 的注释:\n%s", dsn)
	}
}

// TestNormalizeSQLExpr:回读生成列表达式时只归一大小写 / 反引号 / 空白,不做 SQL 解析。
// 归一太少会在不同 MySQL 版本上乱红;归一太多(比如把 NULLIF 和 CASE WHEN 当同一个)会放过一次真实的 DDL 漂移。
func TestNormalizeSQLExpr(t *testing.T) {
	const want = "nullif(snapshot_guid,0)"
	for _, in := range []string{
		"NULLIF(`snapshot_guid`, 0)",
		"nullif(`snapshot_guid`,0)",
		"  NULLIF( snapshot_guid , 0 )  ",
	} {
		if got := normalizeSQLExpr(in); got != want {
			t.Errorf("normalizeSQLExpr(%q)=%q,want %q", in, got, want)
		}
	}
	if normalizeSQLExpr("case when snapshot_guid = 0 then null else snapshot_guid end") == want {
		t.Error("语义等价但写法不同的表达式必须判为不同:那是一次有意的 DDL 变更,应走迁移而不是被归一悄悄接受")
	}
	// 生产 DDL 里的表达式必须与登记在清单里的那一份一致(两处写歪一个字母,症状是复核永远通过)。
	if !strings.Contains(snapshotGuidNzColumnDDL, snapshotGuidNzExpr) {
		t.Fatalf("生成列 DDL 没有用 snapshotGuidNzExpr 拼出来:\n%s", snapshotGuidNzColumnDDL)
	}
}

// TestMigrateOptionsDedupeMaxRows:上限可由调用方覆盖,零值 / 负值回落到编译期默认。
// 上限是"删数据"这一步的唯一闸门,回落方向错了就是"默认不限量"。
func TestMigrateOptionsDedupeMaxRows(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int64
	}{
		{0, snapshotGuidDedupeMaxRows},
		{-1, snapshotGuidDedupeMaxRows},
		{7, 7},
	} {
		if got := (MigrateOptions{SnapshotGuidDedupeMaxRows: tc.in}).dedupeMaxRows(); got != tc.want {
			t.Errorf("dedupeMaxRows(%d)=%d,want %d", tc.in, got, tc.want)
		}
	}
	// 默认必须 fail-closed:TiDB 上建不出唯一键时迁移应失败,而不是靠零值放行。
	if (MigrateOptions{}).AllowMissingGuidUniqueKey {
		t.Fatal("AllowMissingGuidUniqueKey 的零值必须是 false —— 否则 TiDB 上快照 guid 去重会静默失效")
	}
}

// TestDedupePointDeleteSQLIsPrimaryKeyPoint:迁移去重删的是**主键点删**,不是二级索引上的范围删。
// 范围删的取锁顺序与在线写者相反(先 idx(snapshot_guid) 的 next-key X 再回表),是残余环的构成条件;
// 真库上"计划确实走 PRIMARY"的证据在 TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey。
func TestDedupePointDeleteSQLIsPrimaryKeyPoint(t *testing.T) {
	stmt := dedupePointDeleteSQL(PlayerSnapshotTableName)
	if !strings.Contains(stmt, "`id` = ?") {
		t.Fatalf("去重删必须按主键等值:\n%s", stmt)
	}
	for _, unwanted := range []string{"snapshot_guid", ">", "<", "LIMIT"} {
		if strings.Contains(stmt, unwanted) {
			t.Fatalf("去重删里出现了 %q —— 退化成范围删了:\n%s", unwanted, stmt)
		}
	}
	if got := strings.Count(stmt, "?"); got != 1 {
		t.Fatalf("去重删应恰好一个占位符(id),got %d:\n%s", got, stmt)
	}
}

// TestIsDupEntryMySQL:1062 在**退路**形态上是正常的去重结果,必须与 1213 / 1205 / 非 MySQL 错误分开。
// 把它误判成故障,症状是重放一条已落库的快照就报错;把别的错误误判成 1062,症状是丢写入还报成功。
func TestIsDupEntryMySQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"1062", mysqlErrNo(mysqlErrDupEntry), true},
		{"包了一层的 1062", fmt.Errorf("外层: %w", mysqlErrNo(mysqlErrDupEntry)), true},
		{"1213", mysqlErrNo(mysqlErrDeadlock), false},
		{"1205", mysqlErrNo(mysqlErrLockWaitTimeout), false},
		{"非 MySQL 错误", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		if got := isDupEntryMySQL(tc.err); got != tc.want {
			t.Errorf("isDupEntryMySQL(%s)=%v,want %v", tc.name, got, tc.want)
		}
	}
}
