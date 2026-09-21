module protogen

go 1.26.5

require (
	github.com/iancoleman/strcase v0.3.0
	github.com/luyuancpp/proto2mysql v0.1.1
	github.com/luyuancpp/protooption v0.0.21
	go.uber.org/zap v1.27.0
	golang.org/x/text v0.35.0
	google.golang.org/protobuf v1.36.10
	gopkg.in/yaml.v3 v3.0.1
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	gorm.io/gorm v1.30.0 // indirect
)

// D-14 第 7 条(docs/design/xuanming-port-decisions-20260910.md):禁止 v0.1.0 —— 该 tag 被移动过,
// 缓存与仓库指向不同 commit,新机器拉到的内容与 go.sum 不符(SECURITY ERROR)。
// 上游仓库已迁名;旧仓名的代理缓存内容不同。与 go/schemamigrate/go.mod 同一条映射,保留校验,不依赖仓库外目录。
replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1
