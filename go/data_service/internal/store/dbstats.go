package store

import "database/sql"

// DBStatsOf 返回 v 背后那个 MySQL store 的独立连接池统计(给 /metrics 的连接池收集器,G2b;不做 I/O,
// sql.DB.Stats 只取一把内存锁)。v 不是本包的五种 store 之一、或是 nil 指针 / 没有连接池时 ok=false。
//
// 用一个类型分支而不是给每个 store 加方法、再在 svc 的接口上扩一项:svc 的 store 接口是 logic 的依赖面,
// 单测的假实现不该为了观测去实现它;五个 store 是封闭集合,新增 store 时在这里加一行(dbstats_test 的表会提醒)。
// 每个 store 各开一个池(svc.NewServiceContext 里各自 openMySQL),所以这里一个 store 就是一个池。
func DBStatsOf(v any) (sql.DBStats, bool) {
	var db *sql.DB
	switch s := v.(type) {
	case *SnapshotStore:
		if s != nil {
			db = s.db
		}
	case *TransactionLogStore:
		if s != nil {
			db = s.db
		}
	case *PlayerNameStore:
		if s != nil {
			db = s.db
		}
	case *RouteStore:
		if s != nil {
			db = s.db
		}
	}
	if db == nil {
		return sql.DBStats{}, false
	}
	return db.Stats(), true
}
