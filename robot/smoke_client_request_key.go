package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// smokeClientRequestKey 生成一次操作的业务幂等键(client_request_id,docs/design/client-request-idempotency.md §2.1):
// crypto/rand 取的非 0 uint64。同一次操作的所有重发复用同一个键(冒烟里就是复用同一个请求对象);
// 拿到明确结果之后,下一次操作换新键。0 表示旧客户端、服务器不去重,所以不会返回 0。
//
// 不能退化成时间戳或自增号:账本随玩家存档落盘,同一个账号反复跑冒烟时,自增号会从 1 重数、撞上上一轮记下的键,
// 新操作会被当成重放(什么都不做、回 replayed)或换参数(回 kInvalidParameter)。所以这里不像 chatSmokeRandomBytes
// 那样留时间戳退路:Go 1.24 起 crypto/rand.Read 不返回错误(取不到随机数时进程直接终止),真返回了就 panic,
// 不发一个弱键出去。
func smokeClientRequestKey() uint64 {
	var buf [8]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(fmt.Sprintf("crypto/rand failed while generating a client_request_id: %v", err))
		}
		if key := binary.LittleEndian.Uint64(buf[:]); key != 0 {
			return key
		}
	}
}
