package data

// 数据库版本下限自检(2026-09-21 死锁修复第四轮,纵深防御)。
//
// 本包的"不成环"推演只对两类库成立:TiDB 悲观事务 + RC,以及 MySQL 8.0.29 起的 InnoDB + RC。
// MySQL 的版本下限来自取锁规则 R2 的订正:审批通过在 guild_member 上重新插入 (G,p) 时,若该主键是一条尚未 purge 的
// 删除标记记录,插入要先取 S|REC_NOT_GAP、再为改写它升级成 X(R12 的 S → X 升级)。8.0.29 之前,升级请求会排在别人
// 已在排队的 X(捐献 / 兑换预留、兑换终结对同一成员行的 FOR UPDATE)后面,自己持 S、对方等 X、自己再等对方 —— 1213;
// 8.0.29 修了 Bug #11745929:已持 S|REC_NOT_GAP 的事务申请 X 时可以越过他人排队中的 X,这一处升级不再成环。
// 这是 InnoDB 自身的行为,代码改不了,只能在启动期拒绝跑在更老的版本上(部署文档另有版本钉死,这里是第二道闸)。
//
// TiDB 没有 S 锁(唯一性检查直接取 X 悲观锁),不受这条限制,按 VERSION() 里的 "TiDB" 字样放行;它的 VERSION() 形如
// "8.0.11-TiDB-v7.5.1",前面的 8.0.11 只是兼容协议号,不能按 MySQL 版本比较。MariaDB 的 InnoDB 与 MySQL 8 分叉已久,
// 取锁推演不适用,拒绝。无法解析的版本串一律拒绝(fail-closed):宁可启动失败让人看一眼,也不带着未知的锁语义上线。

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// sqlSelectServerVersion:启动期读一次数据库版本串。
const sqlSelectServerVersion = `SELECT VERSION()`

// minMySQLVersion:MySQL 的最低版本(主, 次, 修订)。理由见文件头(Bug #11745929,8.0.29 修复)。
var minMySQLVersion = [3]int{8, 0, 29}

// CheckServerVersion 读 VERSION() 并按 validateServerVersion 判定;通过时返回版本串(供启动日志)。
// 调用方(guild.go)在启动期、建全局插入守卫之前调用,失败即拒启。
func (r *GuildRepo) CheckServerVersion(ctx context.Context) (string, error) {
	var version string
	if err := r.db.QueryRowContext(ctx, sqlSelectServerVersion).Scan(&version); err != nil {
		return "", fmt.Errorf("read database version: %w", err)
	}
	if err := validateServerVersion(version); err != nil {
		return version, err
	}
	return version, nil
}

// validateServerVersion 是纯函数:TiDB 放行;MariaDB 拒绝;其余按 MySQL 解析"主.次.修订",低于 8.0.29 或解析不出一律拒绝。
func validateServerVersion(version string) error {
	trimmed := strings.TrimSpace(version)
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "tidb") {
		return nil
	}
	const why = "审批通过重新插入删除标记的成员行时要做 S → X 升级,不成环依赖 InnoDB Bug #11745929 的修复" +
		"(8.0.29 起,已持 S|REC_NOT_GAP 的事务申请 X 可越过他人排队中的 X)"
	if strings.Contains(lower, "mariadb") {
		return fmt.Errorf("数据库版本 %q 是 MariaDB,帮会服务的取锁推演只对 MySQL 8.0.29+ 与 TiDB 成立,拒绝启动", version)
	}
	got, ok := parseMySQLVersion(trimmed)
	if !ok {
		return fmt.Errorf("无法从数据库版本 %q 解析出 MySQL 的主.次.修订号,按低于 %d.%d.%d 处理并拒绝启动(fail-closed):%s",
			version, minMySQLVersion[0], minMySQLVersion[1], minMySQLVersion[2], why)
	}
	if slices.Compare(got[:], minMySQLVersion[:]) < 0 {
		return fmt.Errorf("MySQL 版本 %q 低于 %d.%d.%d,拒绝启动:%s",
			version, minMySQLVersion[0], minMySQLVersion[1], minMySQLVersion[2], why)
	}
	return nil
}

// parseMySQLVersion 取版本串开头连续的"数字与点"那一段,要求至少三段、前三段都是十进制数;其后的后缀
// (-0ubuntu0.22.04.1、-log、-27 等发行版 / 构建标记)忽略。解析不出返回 ok=false。
func parseMySQLVersion(version string) ([3]int, bool) {
	head := version
	if end := strings.IndexFunc(version, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); end >= 0 {
		head = version[:end]
	}
	parts := strings.Split(head, ".")
	if len(parts) < 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}
