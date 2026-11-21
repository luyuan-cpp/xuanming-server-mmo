package tablebundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// BundleRootEnv 指向 sidecar 挂载的只读表包卷(含 bundles/<digest>/ 与 POINTER.json)。
	// 未设置表示不启用表包模式,进程按传统配置目录加载镜像内配表。
	BundleRootEnv = "MMORPG_TABLE_BUNDLE_ROOT"
	// StatusRootEnv 指向业务进程写 STATUS.json 的状态卷。
	StatusRootEnv = "MMORPG_TABLE_STATUS_ROOT"
	// PointerFilename 是 sidecar 原子替换的轨道指针文件。
	PointerFilename = "POINTER.json"
	// StatusFilename 是业务进程原子替换的运行态回报文件。
	StatusFilename = "STATUS.json"
)

// PointerTarget 描述一条 schema 线上的 current 或 next 目标表包。
type PointerTarget struct {
	ContentDigest string `json:"content_digest"`
	BundleSHA256  string `json:"bundle_sha256,omitempty"`
	Version       uint64 `json:"version,omitempty"`
	ActivateAtMs  uint64 `json:"activate_at_ms,omitempty"`
	Immediate     bool   `json:"immediate,omitempty"`
}

// PointerLine 对应 /mmorpg/tables/pointer/<track>/<schema_digest> 的结构。
type PointerLine struct {
	Current     *PointerTarget `json:"current,omitempty"`
	Next        *PointerTarget `json:"next,omitempty"`
	UpdatedAtMs uint64         `json:"updated_at_ms,omitempty"`
	UpdatedBy   string         `json:"updated_by,omitempty"`
	Reason      string         `json:"reason,omitempty"`
}

// PointerDocument 是 sidecar 落盘的 POINTER.json 格式。
type PointerDocument struct {
	Track string                 `json:"track,omitempty"`
	Lines map[string]PointerLine `json:"lines"`
}

// StatusReport 是业务进程写入 $MMORPG_TABLE_STATUS_ROOT/STATUS.json 的格式,
// 由 table-sync sidecar 周期读取并带租约上报到 etcd。
type StatusReport struct {
	Service        string `json:"service"`
	Pod            string `json:"pod,omitempty"`
	Track          string `json:"track,omitempty"`
	SchemaDigest   string `json:"schema_digest"`
	Source         string `json:"source"`
	LoadedDigest   string `json:"loaded_digest"`
	LoadedVersion  uint64 `json:"loaded_version"`
	LoadedAtMs     uint64 `json:"loaded_at_ms"`
	LoadSeq        uint64 `json:"load_seq"`
	PendingState   string `json:"pending_state,omitempty"`
	PendingDigest  string `json:"pending_digest,omitempty"`
	PendingVersion uint64 `json:"pending_version,omitempty"`
	PendingReason  string `json:"pending_reason,omitempty"`
	UpdatedAtMs    uint64 `json:"updated_at_ms"`
}

// ActiveTarget 按设计 §6.1 返回当下应当生效的指针目标:
// 有 next 且(immediate 或 nowMs >= activate_at_ms)时取 next,否则取 current。
func ActiveTarget(line *PointerLine, nowMs uint64) *PointerTarget {
	if line == nil {
		return nil
	}
	if line.Next != nil && line.Next.ContentDigest != "" && (line.Next.Immediate || nowMs >= line.Next.ActivateAtMs) {
		return line.Next
	}
	if line.Current != nil && line.Current.ContentDigest != "" {
		return line.Current
	}
	return nil
}

// ReadPointerLine 读取 POINTER.json 并取出 schemaDigest 对应的线。
// 兼容带顶层 "lines" 包装与直接 "<schema_digest>": {...} 两种形状;线不存在返回 (nil, false, nil)。
func ReadPointerLine(pointerPath, schemaDigest string) (*PointerLine, bool, error) {
	raw, err := os.ReadFile(pointerPath)
	if err != nil {
		return nil, false, fmt.Errorf("read pointer %s: %w", pointerPath, err)
	}
	var doc PointerDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parse pointer %s: %w", pointerPath, err)
	}
	if doc.Lines != nil {
		line, ok := doc.Lines[schemaDigest]
		if !ok {
			return nil, false, nil
		}
		if err := validatePointerLine(&line); err != nil {
			return nil, false, fmt.Errorf("pointer line %s invalid: %w", schemaDigest, err)
		}
		return &line, true, nil
	}

	// 兼容不带 "lines" 外层的扁平映射形状。
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, false, fmt.Errorf("parse pointer %s as map: %w", pointerPath, err)
	}
	lineRaw, ok := flat[schemaDigest]
	if !ok {
		return nil, false, nil
	}
	var line PointerLine
	if err := json.Unmarshal(lineRaw, &line); err != nil {
		return nil, false, fmt.Errorf("parse pointer line %s: %w", schemaDigest, err)
	}
	if err := validatePointerLine(&line); err != nil {
		return nil, false, fmt.Errorf("pointer line %s invalid: %w", schemaDigest, err)
	}
	return &line, true, nil
}

func validatePointerLine(line *PointerLine) error {
	if line.Current != nil && line.Current.ContentDigest != "" && !IsLowerHex(line.Current.ContentDigest, 64) {
		return fmt.Errorf("current.content_digest invalid: %q", line.Current.ContentDigest)
	}
	if line.Next != nil && line.Next.ContentDigest != "" && !IsLowerHex(line.Next.ContentDigest, 64) {
		return fmt.Errorf("next.content_digest invalid: %q", line.Next.ContentDigest)
	}
	return nil
}

// WriteAtomicFile 先写同目录临时文件再原子改名,避免读端看到半写内容。
func WriteAtomicFile(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		// Windows 上目标已存在时 Rename 可能失败;先删再改名仅作非原子兜底。
		if strings.Contains(strings.ToLower(err.Error()), "exists") || os.IsExist(err) {
			_ = os.Remove(targetPath)
			if err2 := os.Rename(tmpPath, targetPath); err2 != nil {
				return err2
			}
			cleanup = false
			return nil
		}
		return err
	}
	cleanup = false
	return nil
}
