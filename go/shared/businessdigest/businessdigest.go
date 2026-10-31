// Package businessdigest 定义玩家 SQL 行「业务全列摘要」的唯一协议。
//
// go/db 维护工具（barrierrestore 的 promotion 计划与显式解除）和 data_service 的旧 holder 恢复证明
// 回答的是同一个问题：除两列存盘围栏外，这一行的全部字节是否仍是审定时的原件。三处曾各写一份
// 逐列实现、只靠注释约定一致；任何一处单独改动，都会让「计划时摘要」与「解除/恢复时摘要」静默失配，
// 而且失配的表现是维护流程永久拒绝，不会报出原因。收敛到这里后，跨模块一致由 golden 测试守住。
//
// 协议：按 SQL 物理列序逐列写入，跳过 fence_owner_epoch 与 fence_save_seq；每列先写 8 字节大端
// 列名长度与列名，再写 NULL 标记 0x00，或非 NULL 标记 0x01 加 8 字节大端值长度与原始字节。
// 长度前缀与 NULL 标记保证字段边界、NULL 与空串不会撞出同一摘要。
// 已落库的维护原件都按此协议产生；协议改动会作废全部在途原件，只能另起版本，不能就地修改。
package businessdigest

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

// IsFenceColumn 报告该列是否是存盘围栏列；围栏列由维护流程显式推进，不属于业务内容。
func IsFenceColumn(name string) bool {
	return name == "fence_owner_epoch" || name == "fence_save_seq"
}

// Writer 按物理列序累加一行的业务摘要。零值不可用，用 New 构造；不支持并发写。
type Writer struct{ h hash.Hash }

// New 返回空摘要器。
func New() *Writer { return &Writer{h: sha256.New()} }

// Column 追加一列；null 为真时忽略 value（空串与 NULL 由标记区分）。围栏列直接跳过。
func (w *Writer) Column(name string, value []byte, null bool) {
	if IsFenceColumn(name) {
		return
	}
	writePart(w.h, []byte(name))
	if null {
		_, _ = w.h.Write([]byte{0})
		return
	}
	_, _ = w.h.Write([]byte{1})
	writePart(w.h, value)
}

// Sum 返回 32 字节摘要。
func (w *Writer) Sum() []byte { return w.h.Sum(nil) }

func writePart(h hash.Hash, v []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(v)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(v)
}
