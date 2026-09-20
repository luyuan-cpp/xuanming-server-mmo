module friend

// go 1.26.5:schemamigrate(proto2mysql v0.1.1)要求 1.26.5,依赖它的 module 只能跟着抬
// (D-14 理由 3)。friend 自带 `-migrate` 入口,所以必须与 trade / schemamigrate 同版本线。
go 1.26.5

replace proto => ../proto

replace shared => ../shared

// friend 的表以 proto/friend/friend_table.proto 为唯一事实源,由本地的 go/schemamigrate 建表
// (启动期 AutoMigrate 或 `friend -f etc/friend.yaml -migrate`),所以是路径 replace 而非发布版本。
replace schemamigrate => ../schemamigrate

// 直接依赖块按 go/trade/go.mod 的版本对齐(同一条 go-zero / grpc / protobuf 线):
// 各服务是独立 module,版本各走各的会让 shared/ 下的共享包在不同服务里编出不同行为。
//
// 刻意**不再**直接依赖:
//   - github.com/google/uuid:节点 uuid 由 shared/noderegistry 生成,friend 自己不再拼
//     (原先只有已删除的 internal/node 包用它,全仓 friend 下已无 import)。
//   - github.com/redis/go-redis/v9:Redis 句柄统一用 go-zero 的 core/stores/redis(契约 §4);
//     同进程两套客户端 = 两套连接池与超时语义,排障对不上账。
//     internal/data/friend_repo.go 与其 mysql 单测的句柄改型(规格 §3.8 第 3 条)已随本批完成,
//     go/friend 下**零**直接 import 它,所以 require 块里不写它是正确的、不是漏写;
//     go-redis 只应作为 go-zero 的 indirect 出现(go-zero 的 core/stores/redis 把 Pipeliner /
//     IntCmd / Nil 都别名导出了,业务代码不需要直接依赖)。
//     ⚠ 验收判据:`go mod tidy` 之后如果它又落进**直接依赖块**,说明还有一处 import 没改完 ——
//     那是真实缺陷,先去找那处 import,不要直接接受 tidy 结果。
require (
	github.com/alicebob/miniredis/v2 v2.35.0
	github.com/go-sql-driver/mysql v1.9.3
	github.com/prometheus/client_golang v1.23.2
	github.com/segmentio/kafka-go v0.4.47
	github.com/stretchr/testify v1.11.1
	github.com/zeromicro/go-zero v1.10.0
	go.etcd.io/etcd/client/v3 v3.5.15
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	proto v0.0.0
	schemamigrate v0.0.0
	shared v0.0.0
)

// go.sum 与 indirect 依赖块刻意不手写,由 Codex 执行 `go mod tidy` 生成(AGENTS.md §10.1)。
// 在那之前本 module 无法构建 —— 这是预期状态,不是漏写。

// replace 不随依赖传递:与 schemamigrate 同步固定迁名后的发布包,保留旧 module/import 身份。
replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1
