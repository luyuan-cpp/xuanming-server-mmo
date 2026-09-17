module schemamigrate

go 1.26.5

// D-14 第 7 条:proto2mysql 必须 require 未被移动过、含 TiDB 选项与 191 索引前缀的 tag(≥ v0.1.1);
// 禁止 v0.1.0(该 tag 被移动过),禁止 replace 到仓库外目录。
// 本 module 不许 import proto / shared module。go.sum 与 indirect 依赖由 Codex 执行 go mod tidy 生成。
require (
	// 只被 //go:build integration 的真库用例使用。
	github.com/go-sql-driver/mysql v1.9.3
	github.com/luyuancpp/proto2mysql v0.1.1
	google.golang.org/protobuf v1.36.11
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	golang.org/x/text v0.20.0 // indirect
	gorm.io/gorm v1.30.0 // indirect
)

// 上游仓库已迁名；旧仓名的代理缓存内容不同。固定新仓名发布包，保留校验，不依赖仓库外目录。
replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1
