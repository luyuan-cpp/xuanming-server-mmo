package snowflakealloc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cacheRecord 是本地缓存文件的内容(设计稿 §3.5)。
//
// 它让 etcd 成为启动期的**弱依赖**:etcd 不通时,若 now − lastAckWall < F,这个槽
// 在协议上仍是我的(别人 Q 内不会申领;etcd 不通时别人也拿不到新号),可以直接用
// 缓存的槽起服,发号地板 = lastWatermark + lead,后台重试注册。
//
// 字段名是跨语言契约(C++ 侧同格式),不要改。
type cacheRecord struct {
	Kind        string `json:"kind"`
	Cluster     uint32 `json:"cluster"`
	Slot        uint64 `json:"slot"`
	UUID        string `json:"uuid"`
	Incarnation int64  `json:"incarnation"`
	// LastWatermark 是最近一次成功写入 watermark/<slot> 的值(snowflake.Epoch 起的秒,含前推)。
	LastWatermark uint64 `json:"lastWatermark"`
	// LastWatermarkMs 是 login 毫秒水位(Unix ms)最近一次成功写入的值;非 login 恒 0。
	LastWatermarkMs uint64 `json:"lastWatermarkMs,omitempty"`
	// LocalHighWaterSec / LocalHighWaterMs 是本进程**打算发到**的最高逻辑时刻(与
	// LastWatermark* 的差别只在于"有没有等 etcd 确认")。
	//
	// 为什么必须单独记一份:etcd 不可达时水位 Txn 全部失败,LastWatermark 冻在最后一次
	// 成功写,而发号器还能继续发 F(2h)之久 —— 期间它可以借位到墙钟前面
	// (maxBorrowAheadSec=10s),墙钟也可能被 NTP 往回拨。此时同主机在 F 内重启,
	// 缓存启动路径拿 LastWatermark 当地板就**罩不住前任真正发过的秒**,同槽重号。
	// 故障模式(上一拍水位 Txn 没 Ack 起)它在每次 Txn **之前**落盘,第一次失败则在失败返回时
	// 立刻写;稳态只在内存里推进,一次都不写盘(设计稿 §7.5-5:稳态零写盘)。稳态不预写却
	// 没有空档,靠的是水位 Txn 超时 watermarkTxnTimeout(1.5s)< 前推量 guardLeadSec(2s):
	// 失败被发现之前发出的号仍在上一拍已 Ack 水位之内(见 allocator.go 常量注释)。
	//
	// omitempty + 老文件缺字段读出来是 0(被 max 吃掉),所以旧缓存文件仍然可读。
	LocalHighWaterSec uint64 `json:"localHighWaterSec,omitempty"`
	LocalHighWaterMs  uint64 `json:"localHighWaterMs,omitempty"`
	// LastAckWall 是最近一次水位写成功时的墙钟(Unix ms)。**只有它**是启动资格闸
	// (now − LastAckWall < F);本地高水位只抬地板,不延长资格。
	//
	// 磁盘上的这个值只在申领 / reclaim 后的首次 Ack、故障模式的首写、Close() 时刷新,稳态
	// **不刷新**:健康跑了很久后崩溃、且重启时 etcd 恰好也不通,缓存会因它早于 F 而不可用,
	// 只能等 etcd —— 双重故障,§7.5-5 明确接受;故障期间崩溃则由故障首写保证它是新鲜的。
	LastAckWall int64 `json:"lastAckWall"`
}

// DefaultCachePath 给出消费方约定的缓存文件路径:<dir>/snowflake-<kind>-<affinity>.json。
// 文件名带亲和键:同一主机多实例(match 的 host#port、scene_manager 的 host_port、
// 本地 -Zone 双 login)各自一份,不互相覆盖。dir 为空 = 关闭缓存。
func DefaultCachePath(dir, kind, affinity string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "snowflake-"+sanitizeFileToken(kind)+"-"+sanitizeFileToken(affinity)+".json")
}

func sanitizeFileToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// readCacheFile 读缓存;文件不存在返回 os.ErrNotExist。
func readCacheFile(path string) (*cacheRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec cacheRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("snowflakealloc: malformed cache %s: %w", path, err)
	}
	if rec.UUID == "" || rec.Kind == "" {
		return nil, errors.New("snowflakealloc: cache record missing kind/uuid")
	}
	return &rec, nil
}

// writeCacheFileFn 是 Handle 落盘时实际调用的函数。抽成变量只为让测试数"到底写了几次盘"
// (稳态零写盘是 §7.5-5 的验收项,光看文件内容分不清"没写"和"写了同样的内容");
// 生产路径恒等于 writeCacheFile。
var writeCacheFileFn = writeCacheFile

// writeCacheFile 原子落盘:写临时文件 + rename。目录不存在则创建。
// 半写的文件绝不能被下次启动读到 —— 一个错的槽号就是一次撞号。
func writeCacheFile(path string, rec *cacheRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
