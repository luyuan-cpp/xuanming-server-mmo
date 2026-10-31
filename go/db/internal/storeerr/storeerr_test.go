package storeerr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, KindNone},
		{"1040 too many connections", &mysql.MySQLError{Number: 1040}, KindConnRefused},
		{"1203 user connections", &mysql.MySQLError{Number: 1203}, KindConnRefused},
		{"1226 user resource limit", &mysql.MySQLError{Number: 1226}, KindConnRefused},
		{"1053 server shutdown", &mysql.MySQLError{Number: 1053}, KindConnLost},
		{"1290 read-only", &mysql.MySQLError{Number: 1290}, KindReadOnly},
		{"1836 read-only mode", &mysql.MySQLError{Number: 1836}, KindReadOnly},
		{"1205 lock wait timeout", &mysql.MySQLError{Number: 1205}, KindBusy},
		{"1213 deadlock", &mysql.MySQLError{Number: 1213}, KindBusy},
		{"9007 tidb write conflict", &mysql.MySQLError{Number: 9007}, KindBusy},
		{"8002 tidb for update conflict", &mysql.MySQLError{Number: 8002}, KindBusy},
		{"driver.ErrBadConn", driver.ErrBadConn, KindConnLost},
		{"mysql.ErrInvalidConn", mysql.ErrInvalidConn, KindConnLost},
		{"sql.ErrConnDone", sql.ErrConnDone, KindConnLost},
		{"io.EOF", io.EOF, KindConnLost},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, KindConnLost},
		{"dial refused", dialRefused, KindConnLost},
		{"statement deadline", context.DeadlineExceeded, KindConnLost},
		// 不是存储侧故障:任务或表结构的问题、上下文取消、普通文本错误。
		{"1062 duplicate key", &mysql.MySQLError{Number: 1062}, KindNone},
		{"1406 data too long", &mysql.MySQLError{Number: 1406}, KindNone},
		{"1146 missing table", &mysql.MySQLError{Number: 1146}, KindNone},
		{"context canceled", context.Canceled, KindNone},
		{"plain text", errors.New("db write rejected by blob size gate"), KindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.err))
			// proto2mysql 与 go/db 都用 %w 包装,包一层之后判定不变。
			if tc.err != nil {
				assert.Equal(t, tc.want, Classify(fmt.Errorf("update existing row for save on table player_store: %w", tc.err)))
			}
		})
	}
}

func TestTagAndKindOf(t *testing.T) {
	assert.NoError(t, Tag(nil))

	plain := errors.New("schema mismatch")
	assert.Same(t, plain, Tag(plain), "non-store errors pass through unchanged")
	assert.Equal(t, KindNone, KindOf(plain))

	raw := &mysql.MySQLError{Number: 1040, Message: "Too many connections"}
	tagged := Tag(raw)
	assert.Equal(t, KindConnRefused, KindOf(tagged))
	assert.Equal(t, KindConnRefused, KindOf(fmt.Errorf("db write failed: %w", tagged)), "the tag survives further wrapping")
	var me *mysql.MySQLError
	assert.True(t, errors.As(tagged, &me), "the driver error stays reachable through the tag")
	assert.Equal(t, raw.Error(), tagged.Error())

	assert.Same(t, tagged, Tag(tagged), "tagging twice does not nest")
}

// Redis 侧的网络错误没有经过 Tag,KindOf 必须是 KindNone —— 汇总处只认标记,不看底层类型。
func TestKindOfIgnoresUntaggedNetworkErrors(t *testing.T) {
	redisTimeout := fmt.Errorf("placement recheck unavailable: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")})
	assert.Equal(t, KindNone, KindOf(redisTimeout))
	assert.Equal(t, KindConnLost, Classify(redisTimeout), "Classify alone cannot tell MySQL from Redis; that is why callers use KindOf")
}
