package logic

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// ── 合服闸门(merge:in_progress:{zone})──────────────────────────
//
// 这一份是 data_service 那把闸门的**只读**消费者。契约(真源在
// go/data_service/internal/routing/router.go 顶部,改一处必须同步另一处):
//
//	Redis  : data_service 的 mapping Redis(player:zone:{id} 所在的实例/DB),
//	         guild 侧由 config.MergeMarkerRedis 指过去
//	Key    : merge:in_progress:{zone}   zone = 十进制 zone id
//	Value  : JSON,{"started_at": <RFC3339 UTC>, "started_at_unix_ms": <int>, ...}
//	         —— **从不解析**,只看键在不在
//	TTL    : 由 tools/merge_zone 设定,长于整次合服;进程被杀时的兜底
//	谁清除 : tools/merge_zone 跑完显式 DEL;TTL 到期是兜底
//	判据   : **键存在即封锁**
//
// guild 为什么要看它:合服会把源 zone 的公会行搬到目标 zone,并重建 per-zone 榜。
// 维护窗口里如果还能在源 zone 建帮,那个公会要么在搬迁扫描之后诞生(永远留在
// 一个已下线的 zone,会长登进去看不到自己的会),要么与搬迁并发写同一批行。
// 拦在建帮入口是最便宜的一刀:公会创建是低频操作,拒绝一次的代价只是玩家重试。

// MergeFence 报告某个 zone 是否正处在合服维护窗口。
// 抽成接口是为了让 GuildLogic 在没配 MergeMarkerRedis 时装 nil,
// 以及让单测不必起 Redis。
type MergeFence interface {
	// MergeInProgress 返回 zone 是否被封锁。err 非 nil 时调用方必须按**封锁**处理:
	// 查不到闸门状态就不能证明现在可以写。
	MergeInProgress(ctx context.Context, zoneID uint32) (bool, error)
}

const mergeFenceKeyPrefix = "merge:in_progress:"

// MergeFenceKey 拼合服标记键。与 data_service 的 routing.MergeFenceKey 同一份拼法;
// guild 不 import data_service(两个独立 module),所以这里各留一份并互相指认。
func MergeFenceKey(zoneID uint32) string {
	return mergeFenceKeyPrefix + strconv.FormatUint(uint64(zoneID), 10)
}

// RedisMergeFence 用一个 go-redis 客户端做存在性检查。
type RedisMergeFence struct {
	rdb *redis.Client
}

// NewRedisMergeFence 在 rdb 为 nil 时返回 nil(未配置 = 闸门不生效),
// 这样调用方可以直接把结果塞给 GuildLogic,不必自己判空。
func NewRedisMergeFence(rdb *redis.Client) *RedisMergeFence {
	if rdb == nil {
		return nil
	}
	return &RedisMergeFence{rdb: rdb}
}

// MergeInProgress 只做 EXISTS,不读值、不看 TTL —— 见文件顶部的判据说明。
func (f *RedisMergeFence) MergeInProgress(ctx context.Context, zoneID uint32) (bool, error) {
	if f == nil || f.rdb == nil {
		return false, nil
	}
	if zoneID == 0 {
		return false, nil
	}
	n, err := f.rdb.Exists(ctx, MergeFenceKey(zoneID)).Result()
	if err != nil {
		return false, fmt.Errorf("merge fence check for zone %d: %w", zoneID, err)
	}
	return n > 0, nil
}
