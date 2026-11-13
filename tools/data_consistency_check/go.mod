module data_consistency_check

go 1.27.2

require (
	github.com/go-sql-driver/mysql v1.9.2
	github.com/redis/go-redis/v9 v9.17.3
	shared v0.0.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	proto v0.0.0-00010101000000-000000000000 // indirect
)

// 帮会榜检查用 shared/clusterredis 的构造器与逐主节点扫描(guild_rank_check.go),与 tools/merge_zone、tools/dbmove 一样走本地模块。
// 改 go.mod 之后要先 go mod tidy 补齐 go.sum。
replace shared => ../../go/shared

replace proto => ../../go/proto
