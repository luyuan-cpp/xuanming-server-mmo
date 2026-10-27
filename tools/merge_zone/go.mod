module merge_zone

go 1.27.2

require (
	github.com/go-sql-driver/mysql v1.9.2
	github.com/redis/go-redis/v9 v9.16.0
	shared v0.0.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/grpc v1.79.3 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	proto v0.0.0-00010101000000-000000000000 // indirect
)

replace shared => ../../go/shared
replace proto => ../../go/proto
