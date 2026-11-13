module payment

// 仓库统一使用 Go 1.27.2,与 go/rank、go/trade 同一条 go-zero / grpc / protobuf 线。
go 1.27.2

replace proto => ../proto

replace shared => ../shared

// 表以 proto/payment/payment_table.proto 为唯一事实源,由本地的 go/schemamigrate 建表(`payment -f etc/payment.yaml -migrate`)。
replace schemamigrate => ../schemamigrate

// 直接依赖按 go/rank/go.mod 的版本对齐:各服务是独立 module,版本各走各的会让 shared/ 下的共享包在不同服务里编出不同行为。
// 渠道的签名、验签、解密只用标准库(crypto/rsa、crypto/aes、crypto/hmac),不引第三方支付 SDK:
// 离线构建(GOPROXY=off)不需要新模块,协议细节也都在本仓可审。
// indirect 依赖块与 go.sum 由 `go mod tidy` 生成(模块缓存里已有 go/rank 用到的全部版本),不要手写。
require (
	github.com/go-sql-driver/mysql v1.9.3
	github.com/prometheus/client_golang v1.23.2
	github.com/zeromicro/go-zero v1.10.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	proto v0.0.0
	schemamigrate v0.0.0
	shared v0.0.0
)

// replace 不随依赖传递:与 schemamigrate 同步固定迁名后的发布包,保留旧 module/import 身份。
replace github.com/luyuancpp/proto2mysql v0.2.0 => github.com/luyuan-cpp/proto2mysql v0.2.0
