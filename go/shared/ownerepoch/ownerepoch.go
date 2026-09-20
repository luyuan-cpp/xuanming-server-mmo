// Package ownerepoch 是玩家「归属 epoch」与「跨 zone 交接落盘标记」在 Redis 里的
// 键名 / 值格式契约,供 Go 侧的两个消费者共用:
//
//   - scene_manager:EnterScene 是唯一的归属变更闸口,负责用 INCR 铸造 epoch、
//     用 Lua CAS 写 player:{id}:location,并在放行跨节点 / 跨 zone 交接前比对
//     handoff 标记(docs/design/cross-zone-scene-travel.md CZ-4);
//   - db:消费 DBTask 落 MySQL 前按 DBTask.owner_epoch 与当前 epoch 守卫
//     (docs/design/scene-owner-reentry-barrier.md §6.3)。
//
// C++ scene 节点是这两个键的另一半参与者(存盘 Lua 里做 epoch CAS、落地回调后写
// handoff 标记),它不读本包,只按下面写死的格式镜像一份。**改格式必须两边同改**,
// 而且改错的表现是静默的:C++ 比对失败会把正常存盘当成"已被废黜"而拒写。
//
// 键与值(与 C++ 一字不差):
//
//	player:{player_id}:owner_epoch   纯十进制整数字符串。只由 scene_manager 用 INCR
//	                                  铸造,单调递增;不存在 / "0" 表示旧版未铸造,
//	                                  校验方一律跳过比对并计数(兼容窗口)。
//	player:{player_id}:handoff       "{epoch}:{saved_at_ms}"。源 scene 在 Redis 落地
//	                                  回调之后写,EX 300 秒;scene_manager 只比对不删除,
//	                                  放行之后它自然过时,靠 TTL 回收。
//
// 本包刻意只依赖标准库:db 与 scene_manager 用的 Redis 客户端不同(go-redis vs
// go-zero),键名与解析放在这里,读写各自用自己的客户端。
package ownerepoch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HandoffTTL 是 handoff 标记的存活时间。写标记的是 C++ 源 scene(EX 300),这里
// 镜像一份只为让 Go 侧的测试与文档有唯一出处;scene_manager 从不写这个键。
const HandoffTTL = 300 * time.Second

// OwnerEpochKey 返回 player:{id}:owner_epoch。
func OwnerEpochKey(playerID uint64) string {
	return fmt.Sprintf("player:%d:owner_epoch", playerID)
}

// HandoffKey 返回 player:{id}:handoff。
func HandoffKey(playerID uint64) string {
	return fmt.Sprintf("player:%d:handoff", playerID)
}

// ErrMalformedHandoff 表示 handoff 标记的值不是 "{epoch}:{saved_at_ms}"。
// 调用方应把它当作「没有可信标记」(fail-closed),而不是故障。
var ErrMalformedHandoff = errors.New("ownerepoch: malformed handoff marker")

// ParseEpoch 把 owner_epoch 键的原始值解析成整数。空串(键不存在,go-zero 的 Get
// 把 redis.Nil 吞成 "")按 0 返回,表示「未铸造」;非十进制整数才是错误。
func ParseEpoch(raw string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ownerepoch: invalid owner_epoch %q: %w", raw, err)
	}
	return epoch, nil
}

// Handoff 是一条已解析的交接落盘标记。
type Handoff struct {
	// Epoch 是源 scene 存盘时持有的归属 epoch。它等于当前 owner_epoch 才能证明
	// 「持有者本人已把最终态落地」;小于当前值说明标记来自更早的一次归属。
	Epoch uint64
	// SavedAtMs 是源 scene 观察到 Redis 落地回调的墙钟毫秒。只用于日志与排障,
	// 放行判定不看它(归属只认 epoch,不认时间)。
	SavedAtMs int64
}

// String 按契约格式 "{epoch}:{saved_at_ms}" 输出,与 C++ 写入的字节完全一致。
func (h Handoff) String() string {
	return strconv.FormatUint(h.Epoch, 10) + ":" + strconv.FormatInt(h.SavedAtMs, 10)
}

// ParseHandoff 解析 "{epoch}:{saved_at_ms}"。空串表示键不存在,返回 ErrMalformedHandoff
// 之外的、可用 errors.Is(err, ErrNoHandoff) 识别的哨兵,让调用方区分「没写」与「写坏」。
func ParseHandoff(raw string) (Handoff, error) {
	if raw == "" {
		return Handoff{}, ErrNoHandoff
	}
	epochText, savedText, ok := strings.Cut(raw, ":")
	if !ok {
		return Handoff{}, fmt.Errorf("%w: %q", ErrMalformedHandoff, raw)
	}
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return Handoff{}, fmt.Errorf("%w: %q: %v", ErrMalformedHandoff, raw, err)
	}
	savedAt, err := strconv.ParseInt(savedText, 10, 64)
	if err != nil {
		return Handoff{}, fmt.Errorf("%w: %q: %v", ErrMalformedHandoff, raw, err)
	}
	return Handoff{Epoch: epoch, SavedAtMs: savedAt}, nil
}

// ErrNoHandoff 表示 handoff 键不存在(源 scene 还没落盘,或标记已过 TTL)。
var ErrNoHandoff = errors.New("ownerepoch: no handoff marker")
