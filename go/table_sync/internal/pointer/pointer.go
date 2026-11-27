// Package pointer 定义配置表轨道指针与状态回报的 etcd 键布局、本地文件原子写入与过期包清理
// (docs/design/config-table-hot-reload.md §6.1、§7)。
package pointer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultPrefix 是 etcd 中表包指针与状态键的公共前缀。
	DefaultPrefix = "/mmorpg/tables"
	// PointerFilename 是 sidecar 写入到本地卷的轨道指针文件。
	PointerFilename = "POINTER.json"
	// StatusFilename 是业务进程写入到状态卷的运行态回报文件。
	StatusFilename = "STATUS.json"
	// DefaultPruneGrace 是未被引用的旧表包目录保留时长(10 分钟,设计 §7)。
	DefaultPruneGrace = 10 * time.Minute
)

// Target 描述一条 schema 线的 current 或 next 表包目标。
type Target struct {
	ContentDigest string `json:"content_digest"`
	BundleSHA256  string `json:"bundle_sha256,omitempty"`
	Version       uint64 `json:"version,omitempty"`
	ActivateAtMs  uint64 `json:"activate_at_ms,omitempty"`
	Immediate     bool   `json:"immediate,omitempty"`
}

// Line 对应 /mmorpg/tables/pointer/<track>/<schema_digest> 的 JSON 结构。
type Line struct {
	Current     *Target `json:"current,omitempty"`
	Next        *Target `json:"next,omitempty"`
	UpdatedAtMs uint64  `json:"updated_at_ms,omitempty"`
	UpdatedBy   string  `json:"updated_by,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

// Document 对应 sidecar 落盘的 POINTER.json 格式。
type Document struct {
	Track string          `json:"track"`
	Lines map[string]Line `json:"lines"`
	// SyncErrors 按 schema_digest 记「etcd 里有这条线,但这一拍没同步下来」的原因(表包取不到、摘要对不上、指针写坏)。
	// 出错的线在 Lines 里沿用上一次的合法内容;从没同步成功过的线在 Lines 里没有,只在这里有。
	// 业务进程启动时靠它分清「etcd 里本来就没有这条线」(可以用镜像自带的表)和「有,但表包没到位」
	// (必须拒绝启动:否则重启的进程会静默退回更旧的表,而别的实例还在用热更后的表)。
	SyncErrors map[string]string `json:"sync_errors,omitempty"`
}

// MaxSyncErrorChars 是写进 POINTER.json 的单条同步错误原因的长度上限。
const MaxSyncErrorChars = 256

// TrimSyncError 把同步错误原因截到 MaxSyncErrorChars 以内,POINTER.json 不因为一条长错误而膨胀。
func TrimSyncError(reason string) string {
	if len(reason) <= MaxSyncErrorChars {
		return reason
	}
	return reason[:MaxSyncErrorChars]
}

// ReadDocument 读回上一次落盘的 POINTER.json;文件还不存在返回 (nil, nil)。
func ReadDocument(path string) (*Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse pointer document %s: %w", path, err)
	}
	return &doc, nil
}

// StatusReport 对应业务进程写入的 STATUS.json 与上报到 etcd 的状态结构。
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

// PointerPrefix 返回指定轨道在 etcd 中的指针前缀:/mmorpg/tables/pointer/<track>/。
func PointerPrefix(track string) string {
	return fmt.Sprintf("%s/pointer/%s/", DefaultPrefix, track)
}

// PointerKey 返回指定轨道与 schema 线的 etcd 键。
func PointerKey(track, schemaDigest string) string {
	return fmt.Sprintf("%s/pointer/%s/%s", DefaultPrefix, track, schemaDigest)
}

// StatusKey 返回指定轨道与 Pod 的状态回报 etcd 键(挂 30 秒租约)。
func StatusKey(track, pod string) string {
	return fmt.Sprintf("%s/status/%s/%s", DefaultPrefix, track, pod)
}

// ValidTrack 仅接受 stable / canary 两条发布轨道(设计 §6.2)。
func ValidTrack(track string) bool {
	return track == "stable" || track == "canary"
}

// ReferencedDigests 收集 POINTER.json 与 STATUS.json 当前引用的全部 content_digest。
func ReferencedDigests(doc *Document, status *StatusReport) map[string]bool {
	refs := map[string]bool{}
	if doc != nil {
		for _, line := range doc.Lines {
			if line.Current != nil && line.Current.ContentDigest != "" {
				refs[line.Current.ContentDigest] = true
			}
			if line.Next != nil && line.Next.ContentDigest != "" {
				refs[line.Next.ContentDigest] = true
			}
		}
	}
	if status != nil {
		if status.LoadedDigest != "" {
			refs[status.LoadedDigest] = true
		}
		if status.PendingDigest != "" {
			refs[status.PendingDigest] = true
		}
	}
	return refs
}

// WriteAtomicFile 先写同目录临时文件、落盘 Sync 后原子改名,避免业务进程读到半写 POINTER.json。
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
		_ = os.Remove(targetPath)
		if err2 := os.Rename(tmpPath, targetPath); err2 != nil {
			return err2
		}
	}
	cleanup = false
	return nil
}

// ReadStatusFile 读取业务进程写入的 STATUS.json;文件尚未出现返回 (nil, nil)。
func ReadStatusFile(path string) (*StatusReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st StatusReport
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse status %s: %w", path, err)
	}
	return &st, nil
}

// PruneUnusedBundles 清理 bundlesDir 中既不被 POINTER 也不被 STATUS 引用、且修改时间超过 grace 的旧表包目录。
func PruneUnusedBundles(bundlesDir string, referenced map[string]bool, grace time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(bundlesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if referenced[name] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < grace {
			continue
		}
		if err := os.RemoveAll(filepath.Join(bundlesDir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}
