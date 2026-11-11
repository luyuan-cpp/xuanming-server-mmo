package authoritysql

import (
	"errors"
	"strings"
)

// 本文件是 R6(docs/design/infra-reconnect-overview.md §4.1,负责人 2026-11-10 拍板)的判定材料。
//
// 结论:一笔事务里执行过的语句全是纯 DML 时,COMMIT 发出之前的任何失败都不可能已经提交。
// 服务端回包、连接断开、调用取消、回滚失败都算:COMMIT 没发出去,连接断开时库会回滚未提交的事务。
// 执行过 DDL、LOCK TABLES、BEGIN 之类会隐式提交当前事务的语句之后,这个论证不成立,
// 仍按「没有明确证据的类别不可升级为确定」处理(driver.go 的 failLocked / endLocked)。
// COMMIT 自身的错误不在此列,一律是结局未知。

// ErrTxAbortedBeforeCommit 表示事务在 COMMIT 发出之前因连接问题中止(连接断开、调用取消、回滚没确认):
// 确定没写入,可以换一条连接把整笔事务重跑一遍。
//
// 它与 ErrOutcomeUnknown 互斥,两者 errors.Is 互不命中;它也不包 driver.ErrBadConn,database/sql 不会拿它自动重放。
// 它只说明这一笔事务:一个请求里可能先提交过别的事务,所以连接层不据此置「这次请求确定没写」的观察位(NoteNoWriteRetry)。
var ErrTxAbortedBeforeCommit = errors.New("authority SQL transaction aborted before commit: nothing written, retry the whole transaction")

// IsAbortedBeforeCommit 报告 err 是不是「事务在提交前中止、确定没写」,调用方的事务重试判定用它决定能不能整笔重跑。
// 错误树里同时有结局未知时回假:只要有一处可能已经提交,整体就不能按确定没写处理(与 ClassifyOutcome「有一个未知整体就是未知」同一口径)。
func IsAbortedBeforeCommit(err error) bool {
	return errors.Is(err, ErrTxAbortedBeforeCommit) && !errors.Is(err, ErrOutcomeUnknown)
}

// plainDMLStatement 报告 query 是不是纯 DML:首个关键字是 SELECT / INSERT / UPDATE / DELETE / REPLACE / WITH。
// 这几类语句不会隐式提交当前事务。别的一律按可能隐式提交处理,包括本身不提交的 SHOW、EXPLAIN:
// 判错成「可能提交」只是少了一次确定判定,判错成「纯 DML」会把可能已提交的事务说成确定没写。
func plainDMLStatement(query string) bool {
	switch leadingKeyword(query) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return true
	default:
		return false
	}
}

// leadingKeyword 返回语句的首个关键字(大写)。跳过空白与注释:`#`、`-- `、`/* */`(含优化器提示 `/*+ */`),
// 与 statement.go 里 validatePermissionStatement 读语句的那段循环同一口径。以下情况返回空串:
//   - 注释没闭合;
//   - 首个非注释字符不是字母,或关键字后面紧跟标识符字符(SELECTX 不是 SELECT,与 validateStatementEnvelope 的词边界一致);
//   - 关键字之前出现可执行注释(`/*!`、`/*T!`、`/*M!`):服务端会执行里面的内容,不能当空白跳过。
//     运行池的词法包络本来就拒绝这种写法,这里再挡一次,免得本函数被单独调用时把注释里的 DDL 当成空白。
func leadingKeyword(query string) string {
	for i := 0; i < len(query); {
		ch := query[i]
		if sqlSpace(ch) {
			i++
			continue
		}
		if ch == '#' || ch == '-' && i+2 < len(query) && query[i+1] == '-' && sqlSpace(query[i+2]) {
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			end := strings.Index(query[i+2:], "*/")
			if end < 0 || executableComment(query[i+2:i+2+end]) {
				return ""
			}
			i += 2 + end + 2
			continue
		}
		start := i
		for i < len(query) && asciiLetter(query[i]) {
			i++
		}
		if start == i || i < len(query) && identifierByte(query[i]) {
			return ""
		}
		return strings.ToUpper(query[start:i])
	}
	return ""
}

// executableComment 报告注释正文(不含 `/*`、`*/`)是不是 MySQL / TiDB 的可执行注释,判法与 validateStatementEnvelope 相同。
func executableComment(body string) bool {
	if strings.HasPrefix(body, "!") {
		return true
	}
	return len(body) >= 2 && body[1] == '!' && (body[0] == 'T' || body[0] == 't' || body[0] == 'M' || body[0] == 'm')
}
