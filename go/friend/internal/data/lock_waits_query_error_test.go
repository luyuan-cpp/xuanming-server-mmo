package data

// 场景 (g)(h) 的锁等待观察助手(friend_guard_lock_order_mysql_test.go 的 lockWaitsQueryError)的定性回归。
// 不连库、不受 FRIEND_TEST_MYSQL_DSN 门控:定性错了,(g)(h) 卡住时会被报成 SKIP,而那两条是 delete-marked 记录上
// S→X 成环与否唯一的确定性证据 —— 所以这条必须在任何环境下都跑。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
)

// TestLockWaitsQueryErrorClassification 与 go/trade/internal/data/listing_repo_integration_test.go 的同名用例同一口径:
// 只有权限 / 对象缺失类错误号算"不可观测";ctx 结束与其余错误都必须判红 —— 2026-09-21 复审前这里把 ctx 超时
// 也包成了"不可观测"。
func TestLockWaitsQueryErrorClassification(t *testing.T) {
	live := context.Background()
	for _, errNo := range []uint16{1044, 1142, 1143, 1146, 1227} {
		err := lockWaitsQueryError(live, fmt.Errorf("中间包了一层: %w", &drivermysql.MySQLError{Number: errNo}))
		assert.ErrorIs(t, err, errLockWaitsUnobservable, "错误号 %d 是权限 / 对象缺失类,应归为不可观测", errNo)
	}
	for _, cause := range []error{
		&drivermysql.MySQLError{Number: 1213},
		drivermysql.ErrInvalidConn,
		errors.New("任意非 MySQL 错误"),
	} {
		err := lockWaitsQueryError(live, cause)
		assert.NotErrorIs(t, err, errLockWaitsUnobservable, "%v 不是权限 / 对象缺失类错误,必须判红", cause)
		assert.ErrorIs(t, err, cause, "判红时要带出原始错误")
	}

	// ctx 已结束时以 ctx 为准判红,哪怕查询错误碰巧是权限类:卡住不许被报成 SKIP。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := lockWaitsQueryError(canceled, &drivermysql.MySQLError{Number: 1142})
	assert.NotErrorIs(t, err, errLockWaitsUnobservable)
	assert.ErrorIs(t, err, context.Canceled)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	err = lockWaitsQueryError(expired, drivermysql.ErrInvalidConn)
	assert.NotErrorIs(t, err, errLockWaitsUnobservable)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
