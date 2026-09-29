package assetop

// seq_integration_test.go 里那套锁等待观察助手(itMySQLErrorNumberIs / itLockWaitsQueryError /
// itIsLockWaitsUnobservable)的定性回归。**不连库**:不设 ASSETOP_TEST_MYSQL_DSN 也照跑。
//
// 为什么要单独钉住定性:分类错了,TestIntegrationEnsureSeqRowRetrySurvivesRolledBackFirstInserter
// 卡住时会被报成 SKIP,而它是"首个插入者回滚 → 两个排队者继承间隙锁 → 1213 由有界重试吸收"唯一的确定性证据。
// 有人把 ctx 那条分支改回"任何查询错误都算不可观测",这里必须立刻变红。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// mysqlErrText 按 go-sql-driver v1.9.0 errors.go 的 MySQLError.Error() 拼出两种形态的错误文本。
// 这里刻意手拼而不是构造 *mysql.MySQLError:shared 不直接引 MySQL 驱动(理由见 seq_integration_test.go 文件头)。
// 形态一旦在驱动侧变了,本用例会先红——那正是我们要的提醒。
func mysqlErrText(number uint16, sqlState, message string) error {
	if sqlState == "" {
		return fmt.Errorf("Error %d: %s", number, message)
	}
	return fmt.Errorf("Error %d (%s): %s", number, sqlState, message)
}

// TestItMySQLErrorNumberIsMatchesDriverFormatsOnly 钉住错误号的文本判据:两种形态都认,
// 号是前缀的更长数字(11420 之于 1142)不认,只在自由文案里出现 "Error 1142" 字样而没有分隔符的不认。
func TestItMySQLErrorNumberIsMatchesDriverFormatsOnly(t *testing.T) {
	if !itMySQLErrorNumberIs(mysqlErrText(1142, "42000", "SELECT command denied"), 1142) {
		t.Fatal("带 SQLSTATE 的形态应当认出 1142")
	}
	if !itMySQLErrorNumberIs(mysqlErrText(1142, "", "SELECT command denied"), 1142) {
		t.Fatal("不带 SQLSTATE 的形态应当认出 1142")
	}
	// 被 fmt.Errorf("%w") 包过之后前缀不在开头:仍要认出来。
	wrapped := fmt.Errorf("轮询锁等待: %w", mysqlErrText(1146, "42S02", "Table doesn't exist"))
	if !itMySQLErrorNumberIs(wrapped, 1146) {
		t.Fatal("包过一层的错误应当仍能认出 1146")
	}
	// 1142 是 11420 的前缀:裸 strings.Contains 会误判,这里必须不认。
	if itMySQLErrorNumberIs(mysqlErrText(11420, "HY000", "编造的更长错误号"), 1142) {
		t.Fatal("11420 不是 1142,不许误判")
	}
	// 没有 ':' / ' (' 分隔的自由文案不算。
	if itMySQLErrorNumberIs(errors.New("retry after Error 1142 happened"), 1142) {
		t.Fatal("自由文案里的 \"Error 1142\" 后面没有分隔符,不算错误号")
	}
	if itMySQLErrorNumberIs(nil, 1142) {
		t.Fatal("nil 不该匹配任何错误号")
	}
}

// TestItIsLockConflictRecognizesDeadlockAndLockWaitTimeout 钉住 1213 / 1205 的识别:
// 并发用例的 retryable 判据用它,认错了会把"真死锁"当成不可重试错误直接失败,或反过来把别的错误无限吞掉。
func TestItIsLockConflictRecognizesDeadlockAndLockWaitTimeout(t *testing.T) {
	if !itIsLockConflict(mysqlErrText(1213, "40001", "Deadlock found when trying to get lock")) {
		t.Fatal("1213 必须认作锁冲突")
	}
	if !itIsLockConflict(mysqlErrText(1205, "HY000", "Lock wait timeout exceeded")) {
		t.Fatal("1205 必须认作锁冲突")
	}
	if itIsLockConflict(mysqlErrText(1062, "23000", "Duplicate entry")) {
		t.Fatal("1062 是唯一键冲突,不是锁冲突")
	}
	if itIsLockConflict(nil) {
		t.Fatal("nil 不是锁冲突")
	}
}

// TestItLockWaitsQueryErrorClassification 钉住 itLockWaitsQueryError 的定性:只有权限 / 对象缺失类错误号算
// "不可观测";ctx 结束与其余错误都必须判红。与 go/friend、go/trade 的同名用例同一口径。
func TestItLockWaitsQueryErrorClassification(t *testing.T) {
	live := context.Background()

	for _, number := range []uint16{1044, 1142, 1143, 1146, 1227} {
		cause := fmt.Errorf("中间包了一层: %w", mysqlErrText(number, "42000", "denied"))
		err := itLockWaitsQueryError(live, cause)
		if !errors.Is(err, errItLockWaitsUnobservable) {
			t.Fatalf("错误号 %d 是权限 / 对象缺失类,应归为不可观测,实际: %v", number, err)
		}
	}

	for _, cause := range []error{
		mysqlErrText(1213, "40001", "Deadlock found"),
		errors.New("invalid connection"),
		errors.New("任意非 MySQL 错误"),
	} {
		err := itLockWaitsQueryError(live, cause)
		if errors.Is(err, errItLockWaitsUnobservable) {
			t.Fatalf("%v 不是权限 / 对象缺失类错误,必须判红", cause)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("判红时要带出原始错误,实际: %v", err)
		}
	}

	// ctx 已结束时以 ctx 为准判红,哪怕查询错误碰巧是权限类:卡住不许被报成 SKIP。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := itLockWaitsQueryError(canceled, mysqlErrText(1142, "42000", "denied"))
	if errors.Is(err, errItLockWaitsUnobservable) {
		t.Fatalf("ctx 被取消时必须判红,不许归为不可观测: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("判红时要带出 context.Canceled,实际: %v", err)
	}

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	err = itLockWaitsQueryError(expired, errors.New("invalid connection"))
	if errors.Is(err, errItLockWaitsUnobservable) {
		t.Fatalf("ctx 超预算时必须判红,不许归为不可观测: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("判红时要带出 context.DeadlineExceeded,实际: %v", err)
	}
}

// TestAssetopIntegrationGateIsHonored 是"全体 Skip 不算通过"的机械保障(与 go/friend 的
// TestFriendIntegrationGateIsHonored 同形)。本包的集成用例全部挂在 openIntegrationDB 的 Skip 上;
// 没有这条标记用例时,"DSN 忘了设"与"锁序全对"在报告里是同一个绿。
func TestAssetopIntegrationGateIsHonored(t *testing.T) {
	if !itRequireMySQLTests() {
		t.Skipf("%s 未设置:本用例只在验收时启用", assetopRequireMySQLEnv)
	}
	// 门控名在 openIntegrationDB 里按字面量读,两处必须同名。
	if strings.TrimSpace(os.Getenv("ASSETOP_TEST_MYSQL_DSN")) == "" {
		t.Fatalf("%s 要求跑 MySQL 集成用例,但 ASSETOP_TEST_MYSQL_DSN 为空 —— 本包的全部集成用例"+
			"(含锁序、守卫上限、补行重试)会静默 Skip", assetopRequireMySQLEnv)
	}
}
