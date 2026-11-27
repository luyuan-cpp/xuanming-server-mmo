// source_revision.go —— 单调全序 Owner 来源版本编码与铸号器
// （移植自 xuanming-server/pkg/placement/source_revision.go）。
//
// 编码布局（64 位无符号整数）：
//
//	┌──────────────── 40 bit ────────────────┬───── 24 bit ─────┐
//	│ writerTerm (etcd leader CreateRevision)│ 任期内严格递增序号 │
//	└────────────────────────────────────────┴──────────────────┘
//
// 全序性质：
//   - 高 40 位取持久单调递增的写者任期号（如 etcd CreateRevision）；
//   - 低 24 位由当前任期唯一写者在进程内原子自增（从 1 开始；0 留给 SourceRevisionLegacy 哨兵）；
//   - 无需独立持久化发号器，跨任期与进程崩溃重启均严格单调不回退；
//   - 任一侧越界或零参一律 fail-closed 返回 ErrInvalidSourceRevision，绝不截断或回绕。
package ownerauthority

import (
	"errors"
	"fmt"
	"sync/atomic"
)

const (
	// SourceRevisionSeqBits 是低位（任期内序号）所占位宽。
	SourceRevisionSeqBits = 24
	// SourceRevisionMaxSeq 是单个写者任期内可铸造的最大序号（含）。
	SourceRevisionMaxSeq uint64 = uint64(1)<<SourceRevisionSeqBits - 1
	// SourceRevisionMaxTerm 是可编码的最大写者任期号（含）。
	SourceRevisionMaxTerm uint64 = uint64(1)<<(64-SourceRevisionSeqBits) - 1

	// SourceRevisionLegacy 是「未携带来源版本的旧写者」哨兵值（0）。
	// 一旦玩家记录出现非零 SourceRevision，后续 legacy-0 将被 BeginTransition 永久拒绝。
	SourceRevisionLegacy uint64 = 0
)

// ErrInvalidSourceRevision 表示写者任期或任期内序号非法/溢出，必须 fail-closed 拒绝本次切换。
var ErrInvalidSourceRevision = errors.New("ownerauthority: invalid or exhausted source revision")

// ComposeSourceRevision 将 (writerTerm, seq) 编码为单调全序的 uint64 来源版本。
// writerTerm 必须位于 [1, SourceRevisionMaxTerm]，seq 必须位于 [1, SourceRevisionMaxSeq]。
func ComposeSourceRevision(writerTerm, seq uint64) (uint64, error) {
	if writerTerm == 0 {
		return 0, fmt.Errorf("%w: writer term must be > 0", ErrInvalidSourceRevision)
	}
	if writerTerm > SourceRevisionMaxTerm {
		return 0, fmt.Errorf("%w: writer term %d exceeds limit %d", ErrInvalidSourceRevision, writerTerm, SourceRevisionMaxTerm)
	}
	if seq == 0 {
		return 0, fmt.Errorf("%w: sequence must start at 1 (0 is legacy sentinel)", ErrInvalidSourceRevision)
	}
	if seq > SourceRevisionMaxSeq {
		return 0, fmt.Errorf("%w: sequence %d exhausted for term %d (max %d)", ErrInvalidSourceRevision, seq, writerTerm, SourceRevisionMaxSeq)
	}
	return writerTerm<<SourceRevisionSeqBits | seq, nil
}

// SplitSourceRevision 将来源版本拆回 (writerTerm, seq)，仅用于日志与排障。
// 比较新旧一律直接比较 revision 整数本身，无需拆开分字段比较。
func SplitSourceRevision(revision uint64) (writerTerm, seq uint64) {
	return revision >> SourceRevisionSeqBits, revision & SourceRevisionMaxSeq
}

// SourceRevisionMint 是绑定单一写者任期的并发安全来源版本铸号器。
// 任期序号耗尽（超过 SourceRevisionMaxSeq）后持续 fail-closed 返回 ErrInvalidSourceRevision，
// 提示写者主动让位并重新竞选获取新的更高 writerTerm。
type SourceRevisionMint struct {
	writerTerm uint64
	seq        atomic.Uint64
}

// NewSourceRevisionMint 构造绑定 writerTerm 的来源版本铸号器。
func NewSourceRevisionMint(writerTerm uint64) (*SourceRevisionMint, error) {
	if _, err := ComposeSourceRevision(writerTerm, 1); err != nil {
		return nil, err
	}
	return &SourceRevisionMint{writerTerm: writerTerm}, nil
}

// Term 返回本铸号器绑定的写者任期号。
func (m *SourceRevisionMint) Term() uint64 {
	if m == nil {
		return 0
	}
	return m.writerTerm
}

// Next 原子铸造下一个单调递增来源版本；序号越界时 fail-closed 返回错误且不会回绕。
func (m *SourceRevisionMint) Next() (uint64, error) {
	if m == nil {
		return 0, fmt.Errorf("%w: nil source revision mint", ErrInvalidSourceRevision)
	}
	for {
		cur := m.seq.Load()
		if cur >= SourceRevisionMaxSeq {
			return 0, fmt.Errorf("%w: sequence exhausted for term %d", ErrInvalidSourceRevision, m.writerTerm)
		}
		next := cur + 1
		if m.seq.CompareAndSwap(cur, next) {
			return ComposeSourceRevision(m.writerTerm, next)
		}
	}
}
