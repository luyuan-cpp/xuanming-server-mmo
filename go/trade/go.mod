module trade

// go 1.26.5:schemamigrate(proto2mysql v0.1.1)要求 1.26.5,依赖它的 module 只能跟着抬(D-14 理由 3)。
go 1.26.5

replace proto => ../proto

replace shared => ../shared

replace schemamigrate => ../schemamigrate

// go.sum 与 indirect 依赖块刻意不手写,由 Codex 执行 go mod tidy 生成(AGENTS.md §10.1)。
require (
	github.com/go-sql-driver/mysql v1.9.3
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.11.1
	github.com/zeromicro/go-zero v1.10.0
	go.etcd.io/etcd/client/v3 v3.5.15
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	proto v0.0.0
	schemamigrate v0.0.0
	shared v0.0.0
)
