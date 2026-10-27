package authoritysql

import (
	"errors"
	"regexp"
	"strings"
)

// 本文件是隔离候选，尚未编译或运行。它只校验运行池的词法包络，不改写 SQL。
// 契约：调用方通过 driver.Tx 控制事务；运行池只接受单条数据语句。
// 会话固定 STRICT_TRANS_TABLES，禁用 multiStatements、NO_BACKSLASH_ESCAPES、ANSI_QUOTES。
// 数据库账号同时禁止 DDL、EXECUTE、FILE、TRIGGER 与 authority catalog 的写权限。
// 本函数不是通用 MySQL parser；超出封闭语句族、提示族或词法的输入一律拒绝。
var errUnsupportedStatement = errors.New("权威运行池只接受受支持的单条数据语句")

func validateRuntimeStatement(query string) error { return validateStatementEnvelope(query, false) }

func validateStatementEnvelope(query string, allowDDL bool) error {
	first := ""
	terminated := false
	for i := 0; i < len(query); {
		ch := query[i]
		if sqlSpace(ch) {
			i++
			continue
		}
		if ch == '#' || (ch == '-' && i+2 < len(query) && query[i+1] == '-' && sqlSpace(query[i+2])) {
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return errUnsupportedStatement
			}
			body := query[i+2 : i+2+end]
			if strings.Contains(body, "/*") {
				return errUnsupportedStatement
			}
			upper := strings.ToUpper(body)
			// TiDB 与 MySQL 的可执行注释不能被当成普通空白跳过。
			if strings.HasPrefix(body, "!") || strings.HasPrefix(upper, "T!") || strings.HasPrefix(upper, "M!") {
				if !allowDDL || terminated || (first != "CREATE" && first != "ALTER") || !schemaTiDBAnnotation(body) {
					return errUnsupportedStatement
				}
			}
			if strings.HasPrefix(body, "+") {
				if terminated || validateRuntimeHints(body[1:]) != nil {
					return errUnsupportedStatement
				}
			}
			i += 2 + end + 2
			continue
		}
		if terminated {
			return errUnsupportedStatement
		}
		if ch == ';' {
			if first == "" {
				return errUnsupportedStatement
			}
			terminated = true
			i++
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			if first == "" {
				return errUnsupportedStatement
			}
			quote := ch
			i++
			closed := false
			for i < len(query) {
				if query[i] == 0 {
					return errUnsupportedStatement
				}
				if query[i] == '\\' {
					// 反引号标识符不承诺反斜线转义，拒绝歧义。
					if quote == '`' || i+1 >= len(query) {
						return errUnsupportedStatement
					}
					i += 2
					continue
				}
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return errUnsupportedStatement
			}
			continue
		}
		if first == "" {
			start := i
			for i < len(query) && asciiLetter(query[i]) {
				i++
			}
			if start == i {
				return errUnsupportedStatement
			}
			first = strings.ToUpper(query[start:i])
			// 后续必须是词边界，不能把 SELECTxxx 当成 SELECT。
			if i < len(query) && identifierByte(query[i]) {
				return errUnsupportedStatement
			}
			switch first {
			case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH", "SHOW", "EXPLAIN", "DESCRIBE", "DESC":
			case "CREATE", "ALTER", "DROP", "RENAME", "TRUNCATE", "GRANT", "REVOKE":
				if !allowDDL {
					return errUnsupportedStatement
				}
			default:
				return errUnsupportedStatement
			}
			continue
		}
		if ch == 0 || ch >= 0x80 {
			return errUnsupportedStatement
		}
		i++
	}
	if first == "" {
		return errUnsupportedStatement
	}
	return nil
}

// 仅允许仓库现用的四种优化器提示；它们的参数限制在标识符/数字/查询块引用内。
// 不接受 SET_VAR、READ_FROM_STORAGE、未知提示或提示内字符串，防止强会话被语句暗改。
func validateRuntimeHints(body string) error {
	for i := 0; i < len(body); {
		for i < len(body) && sqlSpace(body[i]) {
			i++
		}
		if i == len(body) {
			return nil
		}
		start := i
		for i < len(body) && (asciiLetter(body[i]) || body[i] == '_') {
			i++
		}
		if start == i {
			return errUnsupportedStatement
		}
		switch strings.ToUpper(body[start:i]) {
		case "MAX_EXECUTION_TIME", "SEMIJOIN", "INL_JOIN", "QB_NAME":
		default:
			return errUnsupportedStatement
		}
		for i < len(body) && sqlSpace(body[i]) {
			i++
		}
		if i == len(body) || body[i] != '(' {
			return errUnsupportedStatement
		}
		i++
		closed := false
		for i < len(body) {
			ch := body[i]
			if ch == ')' {
				i++
				closed = true
				break
			}
			if !identifierByte(ch) && !sqlSpace(ch) && ch != '@' && ch != '.' && ch != ',' {
				return errUnsupportedStatement
			}
			i++
		}
		if !closed {
			return errUnsupportedStatement
		}
	}
	return nil
}

func sqlSpace(ch byte) bool    { return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' }
func asciiLetter(ch byte) bool { return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' }
func identifierByte(ch byte) bool {
	return asciiLetter(ch) || ch >= '0' && ch <= '9' || ch == '_' || ch == '$'
}

// Schema只开放现有迁移owner所需的有限控制语句；不开放任意SET、事务切换或KILL CONNECTION。
var schemaLockWaitStatement = regexp.MustCompile(`(?i)^SET\s+SESSION\s+lock_wait_timeout\s*=\s*[0-9]+\s*,\s*innodb_lock_wait_timeout\s*=\s*[0-9]+\s*;?$`)
var schemaKillQueryStatement = regexp.MustCompile(`(?i)^KILL\s+QUERY\s+[1-9][0-9]*\s*;?$`)
var schemaDefaultRoleStatement = regexp.MustCompile(`(?i)^SET\s+DEFAULT\s+ROLE\s+(?:ALL|NONE|'[a-z0-9_]+'(?:@'[a-z0-9_.%:-]+')?(?:\s*,\s*'[a-z0-9_]+'(?:@'[a-z0-9_.%:-]+')?)*)\s+TO\s+'[a-z0-9_]+'@'[a-z0-9_.%:-]+'\s*;?$`)

// 预切只接受现有route_tidb的数字边界；不接受表达式、注释或子查询。
var schemaSplitStatement = regexp.MustCompile("(?i)^SPLIT\\s+TABLE\\s+`[a-z0-9_]+`(?:\\.`[a-z0-9_]+`)?(?:\\s+INDEX\\s+`[a-z0-9_]+`)?\\s+BY\\s+\\([0-9]+(?:\\s*,\\s*[0-9]+)*\\)(?:\\s*,\\s*\\([0-9]+(?:\\s*,\\s*[0-9]+)*\\))*\\s*;?$")

func schemaTiDBAnnotation(body string) bool {
	normalized := strings.Join(strings.Fields(strings.ToUpper(body)), " ")
	switch normalized {
	case "T![CLUSTERED_INDEX] CLUSTERED", "T![CLUSTERED_INDEX] NONCLUSTERED", "T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4":
		return true
	default:
		return false
	}
}

// 分类前先完整扫描，绝不靠前缀绕过多语句、注释或字符串检查。
func validatePermissionStatement(query string, kind permissionKind) error {
	if kind != permissionReader && kind != permissionWriter && kind != permissionSchema && kind != permissionOperator {
		return ErrWriterDenied
	}
	if kind == permissionSchema {
		trimmed := strings.TrimSpace(query)
		if schemaLockWaitStatement.MatchString(trimmed) || schemaKillQueryStatement.MatchString(trimmed) || schemaDefaultRoleStatement.MatchString(trimmed) || schemaSplitStatement.MatchString(trimmed) {
			return nil
		}
	}
	if err := validateStatementEnvelope(query, kind == permissionSchema); err != nil {
		return err
	}
	if kind != permissionReader {
		return nil
	}
	for i := 0; i < len(query); {
		if sqlSpace(query[i]) {
			i++
			continue
		}
		if query[i] == '#' || query[i] == '-' && i+2 < len(query) && query[i+1] == '-' && sqlSpace(query[i+2]) {
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		}
		if query[i] == '/' && i+1 < len(query) && query[i+1] == '*' {
			n := strings.Index(query[i+2:], "*/")
			if n < 0 {
				return errUnsupportedStatement
			}
			i += n + 4
			continue
		}
		start := i
		for i < len(query) && asciiLetter(query[i]) {
			i++
		}
		switch strings.ToUpper(query[start:i]) {
		case "SELECT", "SHOW", "DESCRIBE", "DESC":
			return nil
		default:
			return errUnsupportedStatement
		}
	}
	return errUnsupportedStatement
}
