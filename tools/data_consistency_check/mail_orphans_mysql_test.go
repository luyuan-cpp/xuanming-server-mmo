package main

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	mailConsistencyDSNEnv     = "MAIL_CONSISTENCY_TEST_MYSQL_DSN"
	mailConsistencyRequireEnv = "MAIL_CONSISTENCY_REQUIRE_MYSQL_TESTS"
)

var mailConsistencyDBPattern = regexp.MustCompile(`^mail_consistency_it_[0-9]+_[0-9]+$`)

func TestMailSystemStateOrphansWithoutHandle(t *testing.T) {
	r := checkMailSystemStateOrphans(t.Context(), runConfig{})
	assertMailOrphanResult(t, r, "warn", 0, 0)
	if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: no mmorpg_mail handle") {
		t.Fatalf("缺连接不能伪装成已检查: %q", r.SampleNotes)
	}
}

// 本组直接调用生产检查函数,不复制它的 DISTINCT / 反连接 SQL,也不经过无关的 Redis 检查。
// 两张 TEMPORARY TABLE 只提供检查所需的关系与整数键,不是生产 schema 的另一份定义。
// 夹具使用新建空库和单连接;不建/删持久表,不与其他测试或运行服务共用库。
func TestMailSystemStateOrphansMySQL(t *testing.T) {
	db := openMailConsistencyMySQL(t)

	t.Run("正常多条state按不同系统邮件计数", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		execMailFixture(t, ctx, db, `INSERT INTO mail_message (mail_id) VALUES (101), (102), (103)`)
		execMailFixture(t, ctx, db, `INSERT INTO mail_state (player_id, mail_id) VALUES
			(1,101), (2,101), (3,101), (1,102), (2,102)`)

		r := checkMailSystemStateOrphans(ctx, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "info", 0, 2)
		if r.SampleNotes != "0 orphan mail_id among 2 distinct ids referenced by state rows" {
			t.Fatalf("应检查到2个被引用的系统邮件,不能按5条state或3封系统邮件计数: %q", r.SampleNotes)
		}
	})

	t.Run("真实孤儿去重计数并给出有序样例", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		execMailFixture(t, ctx, db, `INSERT INTO mail_message (mail_id) VALUES (101)`)
		execMailFixture(t, ctx, db, `INSERT INTO mail_state (player_id, mail_id) VALUES
			(1,101), (2,101), (1,202), (1,201), (2,201), (3,201)`)

		r := checkMailSystemStateOrphans(ctx, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "warn", 2, 3)
		if strings.Contains(r.SampleNotes, "NOT CHECKED") || !strings.Contains(r.SampleNotes, "e.g. [201,202]") {
			t.Fatalf("应真实识别2个孤儿id并按id排序,不是4条孤儿state,也不是未检查: %q", r.SampleNotes)
		}
	})

	t.Run("孤儿样例最多五个且不掩盖完整计数", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		execMailFixture(t, ctx, db, `INSERT INTO mail_state (player_id, mail_id) VALUES
			(1,207), (1,203), (1,205), (1,201), (1,206), (1,202), (1,204), (2,201)`)

		r := checkMailSystemStateOrphans(ctx, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "warn", 7, 7)
		if !strings.Contains(r.SampleNotes, "e.g. [201,202,203,204,205]") {
			t.Fatalf("7个孤儿只展示前5个样例,计数不能被样例上限截断: %q", r.SampleNotes)
		}
	})

	t.Run("缺state表不能报干净", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		execMailFixture(t, ctx, db, `DROP TEMPORARY TABLE mail_state`)

		r := checkMailSystemStateOrphans(ctx, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "warn", 0, 0)
		if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: mail_state unreadable") || !strings.Contains(r.SampleNotes, "1146") {
			t.Fatalf("state缺表应返回真实MySQL缺表错误而不是0孤儿: %q", r.SampleNotes)
		}
	})

	t.Run("系统表缺失使反连接失败不能报干净", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		execMailFixture(t, ctx, db, `INSERT INTO mail_state (player_id, mail_id) VALUES (1,101), (2,101), (1,102)`)
		execMailFixture(t, ctx, db, `DROP TEMPORARY TABLE mail_message`)

		r := checkMailSystemStateOrphans(ctx, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "warn", 0, 2)
		if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: orphan count query failed") || !strings.Contains(r.SampleNotes, "1146") {
			t.Fatalf("第一条计数成功不能掩盖反连接查询失败: %q", r.SampleNotes)
		}
	})

	t.Run("查询开始前取消必须报未检查", func(t *testing.T) {
		ctx := prepareMailOrphanFixture(t, db)
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		r := checkMailSystemStateOrphans(canceled, runConfig{mailDB: db})
		assertMailOrphanResult(t, r, "warn", 0, 0)
		if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED:") || !strings.Contains(r.SampleNotes, context.Canceled.Error()) {
			t.Fatalf("取消不能被计为0孤儿: %q", r.SampleNotes)
		}
	})

	// 各子用例的临时表清理已经完成;独占库中不应留下任何持久表。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var persistentTables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()`).Scan(&persistentTables); err != nil {
		t.Fatalf("复核夹具清理后的测试库失败: %v", err)
	}
	if persistentTables != 0 {
		t.Fatalf("夹具清理后持久表应为0,实际%d", persistentTables)
	}
	t.Log("所有临时表夹具已清理;专用测试库持久表数仍为0")
}

func assertMailOrphanResult(t *testing.T, r CheckResult, severity string, bad, total int64) {
	t.Helper()
	if r.Name != "mail_state.mail_id" || r.Severity != severity || r.BadCount != bad || r.TotalCount != total {
		t.Fatalf("邮件孤儿报告错误: 得到 %+v; 期望 severity=%s bad=%d total=%d", r, severity, bad, total)
	}
}

func openMailConsistencyMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(mailConsistencyDSNEnv))
	if dsn == "" {
		if os.Getenv(mailConsistencyRequireEnv) != "" {
			t.Fatalf("%s 已设置,但 %s 为空;验收模式不能跳过真库测试", mailConsistencyRequireEnv, mailConsistencyDSNEnv)
		}
		t.Skipf("%s 未设置,跳过真实MySQL邮件孤儿检查", mailConsistencyDSNEnv)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试DSN格式非法;不回显连接串")
	}
	if !mailConsistencyDBPattern.MatchString(cfg.DBName) {
		t.Fatal("测试仅允许新建专用库 mail_consistency_it_<数字>_<数字>;拒绝业务库和其他测试库")
	}
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("创建MySQL测试连接失败;不回显连接串")
	}
	t.Cleanup(func() { _ = db.Close() })
	// TEMPORARY TABLE 属于会话;整组串行用同一条空闲连接,不得调用 t.Parallel。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("连接专用MySQL测试库失败: %v", err)
	}
	var selectedDB, version, comment string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE(), VERSION(), @@version_comment`).Scan(&selectedDB, &version, &comment); err != nil {
		t.Fatalf("读取测试库身份失败: %v", err)
	}
	if selectedDB != cfg.DBName || !mailConsistencyDBPattern.MatchString(selectedDB) {
		t.Fatal("实际连接的数据库不符合专用库白名单或与DSN不一致")
	}
	server := strings.ToLower(version + " " + comment)
	if strings.Contains(server, "tidb") || strings.Contains(server, "mariadb") {
		t.Fatal("该组验收要求真实MySQL,不接受TiDB或MariaDB替代")
	}
	var persistentTables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()`).Scan(&persistentTables); err != nil {
		t.Fatalf("确认测试库为空失败: %v", err)
	}
	if persistentTables != 0 {
		t.Fatal("专用测试库包含持久表,拒绝运行;请提供新建空库")
	}
	t.Logf("真实MySQL %s (%s);专用空库校验通过,只操作会话临时表", version, comment)
	return db
}

func prepareMailOrphanFixture(t *testing.T, db *sql.DB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// TEMPORARY 关键字保证错误路径也不可能删除同名持久表。
		if _, err := db.ExecContext(cleanupCtx, `DROP TEMPORARY TABLE IF EXISTS mail_state, mail_message`); err != nil {
			t.Errorf("清理本用例临时表失败: %v", err)
		}
	})
	execMailFixture(t, ctx, db, `CREATE TEMPORARY TABLE mail_message (
		mail_id BIGINT UNSIGNED NOT NULL PRIMARY KEY
	) ENGINE=InnoDB`)
	execMailFixture(t, ctx, db, `CREATE TEMPORARY TABLE mail_state (
		player_id BIGINT UNSIGNED NOT NULL,
		mail_id BIGINT UNSIGNED NOT NULL,
		PRIMARY KEY (player_id, mail_id),
		KEY idx_mail_id (mail_id)
	) ENGINE=InnoDB`)
	return ctx
}

func execMailFixture(t *testing.T, ctx context.Context, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, statement); err != nil {
		t.Fatalf("构造临时关系夹具失败: %v", err)
	}
}
