module friend

// 仓库统一使用 Go 1.27.2；schemamigrate(proto2mysql v0.2.0)最低要求 1.26.5(D-14 理由 3)。
// friend 自带 `-migrate` 入口，与 trade / schemamigrate 使用仓库统一版本。
go 1.27.2

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
// 全局登录态只读接缝 shared/loginsession 使用 go-redis/v9，故提升现有锁定版本为直接依赖。
// 好友缓存仍使用 go-zero；登录态独立池显式限定大小、DB和超时，并关闭自动重试。
require (
	github.com/alicebob/miniredis/v2 v2.36.1
	github.com/go-sql-driver/mysql v1.9.3
	github.com/prometheus/client_golang v1.23.2
	github.com/redis/go-redis/v9 v9.17.3
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

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/IBM/sarama v1.43.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.5.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/eapache/go-resiliency v1.6.0 // indirect
	github.com/eapache/go-xerial-snappy v0.0.0-20230731223053-c322873962e3 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/emicklei/go-restful/v3 v3.11.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.19.6 // indirect
	github.com/go-openapi/jsonreference v0.20.2 // indirect
	github.com/go-openapi/swag v0.22.4 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/google/gnostic-models v0.6.8 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grafana/pyroscope-go v1.2.7 // indirect
	github.com/grafana/pyroscope-go/godeltaprof v0.1.9 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.20.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/luyuancpp/proto2mysql v0.2.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/openzipkin/zipkin-go v0.4.3 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pierrec/lz4/v4 v4.1.21 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	github.com/spaolacci/murmur3 v1.1.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.etcd.io/etcd/api/v3 v3.5.15 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.15 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.39.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.24.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.24.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.24.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.24.0 // indirect
	go.opentelemetry.io/otel/exporters/zipkin v1.24.0 // indirect
	go.opentelemetry.io/otel/metric v1.39.0 // indirect
	go.opentelemetry.io/otel/sdk v1.39.0 // indirect
	go.opentelemetry.io/otel/trace v1.39.0 // indirect
	go.opentelemetry.io/proto/otlp v1.3.1 // indirect
	go.uber.org/atomic v1.10.0 // indirect
	go.uber.org/automaxprocs v1.6.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	go.uber.org/multierr v1.9.0 // indirect
	go.uber.org/zap v1.24.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/crypto v0.46.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/term v0.38.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	golang.org/x/time v0.10.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gorm.io/gorm v1.30.0 // indirect
	k8s.io/api v0.29.3 // indirect
	k8s.io/apimachinery v0.29.4 // indirect
	k8s.io/client-go v0.29.3 // indirect
	k8s.io/klog/v2 v2.110.1 // indirect
	k8s.io/kube-openapi v0.0.0-20231010175941-2dd684a91f00 // indirect
	k8s.io/utils v0.0.0-20251222233032-718f0e51e6d2 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.4.1 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)

// go.sum 与上面的 indirect 依赖块由 `go mod tidy` 生成(2026-09-21 首次执行;机器 B 上官方代理拉不到
// github.com/luyuancpp/* 旧组织名的模块,当时用进程级 GOPROXY=https://goproxy.cn,... 跑的,未改 go env)。
// 不要手改;tidy 之后按上面 go-redis 那条验收判据看一眼直接依赖块。

// replace 不随依赖传递:与 schemamigrate 同步固定迁名后的发布包,保留旧 module/import 身份。
replace github.com/luyuancpp/proto2mysql v0.2.0 => github.com/luyuan-cpp/proto2mysql v0.2.0
