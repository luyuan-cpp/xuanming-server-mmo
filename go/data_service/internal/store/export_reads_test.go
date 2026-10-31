package store

// 第 3 批读方(route_reads.go)给同目录 store_test 包(真库集成用例)的只读出口;_test.go 文件不进生产二进制。
// 与 export_test.go 分开放:那份属于其它 store 的用例,本批不改它。

const (
	// 让 EXPLAIN 用例对着生产**同一段文本**取计划(抄一份就会在改 SQL 时与生产漂移)。
	RouteScanByHomeSQLForTest    = routeScanByHomeSQL
	RouteScanByStorageSQLForTest = routeScanByStorageSQL
)
