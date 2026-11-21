package tablebundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"shared/generated/table"
	"shared/safego"
)

const defaultPollInterval = 2 * time.Second

// PrepareHook 在新 TableSet 解析完成后、提交生效前运行(启动与热更共用同一逻辑):
// 负责整批校验与派生数据编译,返回无错的 commit 闭包(将在 table.Commit 同一次提交里执行)。
type PrepareHook func(dir string, ts *table.TableSet) (commit func(), err error)

// Options 配置 Manager 的加载目录、校验钩子与轮询行为。
type Options struct {
	ServiceName   string
	ImageTableDir string
	UseBinary     bool
	// BundleRoot 为空时自动读 MMORPG_TABLE_BUNDLE_ROOT;仍为空则不启用表包模式。
	BundleRoot string
	// StatusRoot 为空时自动读 MMORPG_TABLE_STATUS_ROOT;为空时跳过写 STATUS.json。
	StatusRoot string
	// PrepareHook 可选:校验候选 TableSet 并编译派生数据(例如 match 的 Catalog 与官方剧本)。
	PrepareHook PrepareHook
	// StrictStartupValidate 控制启动期 PrepareHook 失败是否直接报错拒启。
	// match / guild 启动校验失败必须拒启(true);其余只记录日志的服务设 false(设计 §8.1)。
	StrictStartupValidate bool
	// PollInterval 轮询 POINTER.json 的周期,默认 2 秒。
	PollInterval time.Duration

	// 以下钩子仅用于单测注入,生产保持 nil 即可使用 table 包默认实现。
	LoadTablesFn      func(dir string, useBinary bool) (*table.TableSet, error)
	CommitTablesFn    func(ts *table.TableSet)
	SchemaDigest      string
	IsHotReloadableFn func(tableName string) bool
	NowFn             func() time.Time
}

type preparedBundle struct {
	target   PointerTarget
	manifest *BundleManifest
	tableSet *table.TableSet
	commitFn func()
}

// Manager 负责 Go 服务端的配置表启动加载与后台两阶段热更。
type Manager struct {
	opts              Options
	bundleMode        bool
	loadTablesFn      func(dir string, useBinary bool) (*table.TableSet, error)
	commitTablesFn    func(ts *table.TableSet)
	schemaDigest      string
	isHotReloadableFn func(tableName string) bool
	nowFn             func() time.Time
	pollInterval      time.Duration
	podName           string
	releaseTrack      string

	mu                  sync.Mutex
	currentManifest     *BundleManifest
	currentSource       string
	currentDigest       string
	currentVersion      uint64
	loadedAtMs          uint64
	loadSeq             uint64
	prepared            *preparedBundle
	pendingState        string
	pendingDigest       string
	pendingVersion      uint64
	pendingReason       string
	lastRejectedDigest  string
	lastNeedsRestartDig string
	started             bool
	stopCh              chan struct{}
	stopOnce            sync.Once
	wg                  sync.WaitGroup
}

// LoadAtStartup 按设计 §8.1 执行启动加载并提交首套表:
//   - 表包模式开启时读 POINTER.json,命中本 schema_digest 线且有生效目标则从 bundles/<digest>/ 加载,否则回落镜像目录;
//   - 无论哪种模式,目录里存在 manifest.json 即核对第 2 版清单的 schema_digest 与逐文件 sha256;
//   - 返回已提交首套表的 Manager,调用方按需 Start() 启动 2 秒热更轮询并在停机时 Stop()。
func LoadAtStartup(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.BundleRoot) == "" {
		opts.BundleRoot = strings.TrimSpace(os.Getenv(BundleRootEnv))
	}
	if strings.TrimSpace(opts.StatusRoot) == "" {
		opts.StatusRoot = strings.TrimSpace(os.Getenv(StatusRootEnv))
	}
	m := &Manager{
		opts:              opts,
		bundleMode:        opts.BundleRoot != "",
		loadTablesFn:      opts.LoadTablesFn,
		commitTablesFn:    opts.CommitTablesFn,
		schemaDigest:      opts.SchemaDigest,
		isHotReloadableFn: opts.IsHotReloadableFn,
		nowFn:             opts.NowFn,
		pollInterval:      opts.PollInterval,
		podName:           strings.TrimSpace(os.Getenv("POD_NAME")),
		releaseTrack:      strings.TrimSpace(os.Getenv("MMORPG_RELEASE_TRACK")),
		stopCh:            make(chan struct{}),
	}
	if m.loadTablesFn == nil {
		m.loadTablesFn = table.Prepare
	}
	if m.commitTablesFn == nil {
		m.commitTablesFn = table.Commit
	}
	if m.schemaDigest == "" {
		m.schemaDigest = table.SchemaDigest
	}
	if m.isHotReloadableFn == nil {
		m.isHotReloadableFn = table.IsHotReloadable
	}
	if m.nowFn == nil {
		m.nowFn = time.Now
	}
	if m.pollInterval <= 0 {
		m.pollInterval = defaultPollInterval
	}
	if m.podName == "" {
		m.podName, _ = os.Hostname()
	}

	nowMs := uint64(m.nowFn().UnixMilli())
	loadDir := opts.ImageTableDir
	source := "image"
	var expectedContentDigest string
	var pointerVersion uint64

	if m.bundleMode {
		pointerPath := filepath.Join(opts.BundleRoot, PointerFilename)
		line, found, err := ReadPointerLine(pointerPath, m.schemaDigest)
		if err != nil {
			return nil, fmt.Errorf("startup read pointer failed: %w", err)
		}
		if found {
			if target := ActiveTarget(line, nowMs); target != nil {
				loadDir = filepath.Join(opts.BundleRoot, "bundles", target.ContentDigest)
				source = "bundle"
				expectedContentDigest = target.ContentDigest
				pointerVersion = target.Version
			}
		}
	}

	var manifest *BundleManifest
	manifestPath := filepath.Join(loadDir, ManifestFilename)
	_, statErr := os.Stat(manifestPath)
	if source == "bundle" || statErr == nil {
		verified, err := VerifyTableDirManifest(loadDir, opts.UseBinary, m.schemaDigest)
		if err != nil {
			return nil, fmt.Errorf("startup verify manifest (%s): %w", loadDir, err)
		}
		if expectedContentDigest != "" && verified.ContentDigest != expectedContentDigest {
			return nil, fmt.Errorf("startup bundle digest mismatch: manifest=%s pointer=%s", verified.ContentDigest, expectedContentDigest)
		}
		manifest = verified
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat manifest %s: %w", manifestPath, statErr)
	}

	ts, err := m.loadTablesFn(loadDir, opts.UseBinary)
	if err != nil {
		return nil, fmt.Errorf("startup load tables from %s: %w", loadDir, err)
	}
	if ts == nil {
		ts = &table.TableSet{}
	}
	contentDigest := ""
	version := pointerVersion
	if manifest != nil {
		contentDigest = manifest.ContentDigest
		if version == 0 {
			version = manifest.Version
		}
	}
	ts.Identity = table.BundleIdentity{
		SchemaDigest:  m.schemaDigest,
		ContentDigest: contentDigest,
		Version:       version,
		Source:        source,
	}

	var commitFn func()
	if opts.PrepareHook != nil {
		var hookErr error
		commitFn, hookErr = opts.PrepareHook(loadDir, ts)
		if hookErr != nil {
			if opts.StrictStartupValidate {
				return nil, fmt.Errorf("startup validate tables (%s): %w", loadDir, hookErr)
			}
			logx.Errorf("[TableBundle] startup validation warning service=%s dir=%s err=%v", opts.ServiceName, loadDir, hookErr)
			commitFn = nil
		}
	}

	m.commitTablesFn(ts)
	if commitFn != nil {
		commitFn()
	}

	m.mu.Lock()
	m.currentManifest = manifest
	m.currentSource = source
	m.currentDigest = contentDigest
	m.currentVersion = version
	m.loadedAtMs = nowMs
	m.loadSeq = ts.Identity.LoadSeq
	if m.loadSeq == 0 {
		m.loadSeq = 1
	}
	m.mu.Unlock()

	setActiveBundleMetric(m.schemaDigest, contentDigest, source)
	logx.Infof("[TableBundle] startup loaded service=%s source=%s version=%d schema_digest=%s content_digest=%s dir=%s",
		opts.ServiceName, source, version, m.schemaDigest, shortDigest(contentDigest), loadDir)
	m.writeStatusBestEffort(nowMs)
	return m, nil
}

// SetPrepareHook 在启动完成之后、Start 之前装配或替换运行期校验/编译钩子(用于避免主包与 svc 包循环依赖)。
func (m *Manager) SetPrepareHook(hook PrepareHook) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.opts.PrepareHook = hook
	m.mu.Unlock()
}

// Start 在表包模式下启动后台 2 秒轮询协程;非表包模式为空操作。
func (m *Manager) Start() {
	if m == nil || !m.bundleMode {
		return
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	m.wg.Add(1)
	safego.Go("tablebundle.poll", func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.PollOnce()
			}
		}
	})
}

// Stop 优雅停止后台轮询协程,幂等安全。
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
	m.wg.Wait()
}

// PollOnce 执行单轮指针检查、准备、校验与到点提交(同时可供单测直接驱动)。
func (m *Manager) PollOnce() {
	if m == nil || !m.bundleMode {
		return
	}
	nowMs := uint64(m.nowFn().UnixMilli())

	// 第一步:如果已有准备好的包且已到生效时刻,先读最新指针确认没被撤销再提交。
	pointerPath := filepath.Join(m.opts.BundleRoot, PointerFilename)
	line, found, err := ReadPointerLine(pointerPath, m.schemaDigest)
	if err != nil {
		logx.Errorf("[TableBundle] poll read pointer failed service=%s err=%v", m.opts.ServiceName, err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !found {
		m.clearPendingLocked(nowMs)
		return
	}

	// 决定期望切换的目标:优先看未切换的 next,其次看 current(例如直接发布或跳过 next)。
	var desired *PointerTarget
	if line.Next != nil && line.Next.ContentDigest != "" && line.Next.ContentDigest != m.currentDigest {
		desired = line.Next
	} else if line.Current != nil && line.Current.ContentDigest != "" && line.Current.ContentDigest != m.currentDigest &&
		(line.Next == nil || line.Next.ContentDigest == "") {
		// 仅当指针已直接推进 current(无 next)时按立即切换对待。
		copyTarget := *line.Current
		copyTarget.Immediate = true
		desired = &copyTarget
	}

	if desired == nil {
		// 指针回到当前已加载版本或 next 被撤销:丢弃尚未提交的准备态。
		if m.prepared != nil || m.pendingState != "" {
			logx.Infof("[TableBundle] pending reload cleared service=%s active_digest=%s",
				m.opts.ServiceName, shortDigest(m.currentDigest))
			m.clearPendingLocked(nowMs)
		}
		return
	}

	// 如果已准备好同一目标,仅刷新生效时刻并检查是否到点提交。
	if m.prepared != nil && m.prepared.target.ContentDigest == desired.ContentDigest {
		m.prepared.target = *desired
		if desired.Immediate || nowMs >= desired.ActivateAtMs {
			m.commitPreparedLocked(nowMs)
		}
		return
	}

	// 指针指向新的目标时,清理旧准备态。
	if m.prepared != nil {
		logx.Infof("[TableBundle] dropping superseded prepared bundle service=%s old=%s new=%s",
			m.opts.ServiceName, shortDigest(m.prepared.target.ContentDigest), shortDigest(desired.ContentDigest))
		m.prepared = nil
	}

	// 已被拒绝或已判定需要重启的同一个目标不再重复尝试,直到指针切走(设计 §8.1)。
	if desired.ContentDigest == m.lastRejectedDigest || desired.ContentDigest == m.lastNeedsRestartDig {
		return
	}

	m.prepareCandidateLocked(*desired, nowMs)
}

func (m *Manager) clearPendingLocked(nowMs uint64) {
	dirty := m.prepared != nil || m.pendingState != "" || m.lastRejectedDigest != "" || m.lastNeedsRestartDig != ""
	m.prepared = nil
	m.pendingState = ""
	m.pendingDigest = ""
	m.pendingVersion = 0
	m.pendingReason = ""
	m.lastRejectedDigest = ""
	m.lastNeedsRestartDig = ""
	if dirty {
		m.writeStatusLocked(nowMs)
	}
}

func (m *Manager) prepareCandidateLocked(target PointerTarget, nowMs uint64) {
	start := m.nowFn()
	bundleDir := filepath.Join(m.opts.BundleRoot, "bundles", target.ContentDigest)
	logx.Infof("[TableBundle] prepare start service=%s schema_digest=%s target_digest=%s version=%d activate_at_ms=%d immediate=%v",
		m.opts.ServiceName, m.schemaDigest, shortDigest(target.ContentDigest), target.Version, target.ActivateAtMs, target.Immediate)

	candManifest, err := VerifyTableDirManifest(bundleDir, m.opts.UseBinary, m.schemaDigest)
	if err != nil {
		m.rejectCandidateLocked(target, fmt.Sprintf("manifest verification failed: %v", err), nowMs)
		return
	}
	if candManifest.ContentDigest != target.ContentDigest {
		m.rejectCandidateLocked(target, fmt.Sprintf("manifest content_digest=%s does not match pointer=%s",
			candManifest.ContentDigest, target.ContentDigest), nowMs)
		return
	}

	// 按本二进制编译期名单检查改动表是否全部允许热更(设计 §5.3)。
	changed := DiffChangedTables(m.currentManifest, candManifest, m.opts.UseBinary)
	var nonReloadable []string
	for _, name := range changed {
		if !m.isHotReloadableFn(name) {
			nonReloadable = append(nonReloadable, name)
		}
	}
	if len(nonReloadable) > 0 {
		reason := fmt.Sprintf("non-hot-reloadable tables changed: %s", strings.Join(nonReloadable, ","))
		m.lastNeedsRestartDig = target.ContentDigest
		m.pendingState = "needs_restart"
		m.pendingDigest = target.ContentDigest
		m.pendingVersion = target.Version
		if m.pendingVersion == 0 {
			m.pendingVersion = candManifest.Version
		}
		m.pendingReason = reason
		observeReloadResult("needs_restart")
		logx.Errorf("[TableBundle] reload requires restart service=%s schema_digest=%s target_digest=%s reason=%s",
			m.opts.ServiceName, m.schemaDigest, shortDigest(target.ContentDigest), reason)
		m.writeStatusLocked(nowMs)
		return
	}

	ts, err := m.loadTablesFn(bundleDir, m.opts.UseBinary)
	if err != nil {
		m.rejectCandidateLocked(target, fmt.Sprintf("parse tables failed: %v", err), nowMs)
		return
	}
	if ts == nil {
		ts = &table.TableSet{}
	}
	version := target.Version
	if version == 0 {
		version = candManifest.Version
	}
	ts.Identity = table.BundleIdentity{
		SchemaDigest:  m.schemaDigest,
		ContentDigest: candManifest.ContentDigest,
		Version:       version,
		Source:        "bundle",
	}

	var commitFn func()
	if m.opts.PrepareHook != nil {
		commitFn, err = m.opts.PrepareHook(bundleDir, ts)
		if err != nil {
			m.rejectCandidateLocked(target, fmt.Sprintf("validation failed: %v", err), nowMs)
			return
		}
	}

	elapsedSec := m.nowFn().Sub(start).Seconds()
	if elapsedSec < 0 {
		elapsedSec = 0
	}
	observePrepareSeconds(elapsedSec)

	m.prepared = &preparedBundle{
		target:   target,
		manifest: candManifest,
		tableSet: ts,
		commitFn: commitFn,
	}
	m.lastRejectedDigest = ""
	m.lastNeedsRestartDig = ""

	if target.Immediate || nowMs >= target.ActivateAtMs {
		m.commitPreparedLocked(nowMs)
		return
	}

	m.pendingState = "prepared"
	m.pendingDigest = candManifest.ContentDigest
	m.pendingVersion = version
	m.pendingReason = ""
	logx.Infof("[TableBundle] prepare ready waiting for activate_at service=%s schema_digest=%s target_digest=%s version=%d activate_at_ms=%d elapsed_ms=%.1f",
		m.opts.ServiceName, m.schemaDigest, shortDigest(candManifest.ContentDigest), version, target.ActivateAtMs, elapsedSec*1000)
	m.writeStatusLocked(nowMs)
}

func (m *Manager) rejectCandidateLocked(target PointerTarget, reason string, nowMs uint64) {
	m.prepared = nil
	m.lastRejectedDigest = target.ContentDigest
	m.pendingState = "rejected"
	m.pendingDigest = target.ContentDigest
	m.pendingVersion = target.Version
	m.pendingReason = reason
	observeReloadResult("rejected")
	logx.Errorf("[TableBundle] reload rejected service=%s schema_digest=%s target_digest=%s reason=%s",
		m.opts.ServiceName, m.schemaDigest, shortDigest(target.ContentDigest), reason)
	m.writeStatusLocked(nowMs)
}

func (m *Manager) commitPreparedLocked(nowMs uint64) {
	if m.prepared == nil {
		return
	}
	prep := m.prepared
	m.prepared = nil

	m.commitTablesFn(prep.tableSet)
	if prep.commitFn != nil {
		prep.commitFn()
	}

	m.currentManifest = prep.manifest
	m.currentSource = "bundle"
	m.currentDigest = prep.manifest.ContentDigest
	m.currentVersion = prep.tableSet.Identity.Version
	m.loadedAtMs = nowMs
	m.loadSeq++
	if prep.tableSet.Identity.LoadSeq > 0 {
		m.loadSeq = prep.tableSet.Identity.LoadSeq
	}
	m.pendingState = ""
	m.pendingDigest = ""
	m.pendingVersion = 0
	m.pendingReason = ""
	m.lastRejectedDigest = ""
	m.lastNeedsRestartDig = ""

	setActiveBundleMetric(m.schemaDigest, m.currentDigest, m.currentSource)
	observeReloadResult("committed")
	logx.Infof("[TableBundle] reload committed service=%s schema_digest=%s content_digest=%s version=%d load_seq=%d",
		m.opts.ServiceName, m.schemaDigest, shortDigest(m.currentDigest), m.currentVersion, m.loadSeq)
	m.writeStatusLocked(nowMs)
}

// SnapshotStatus 返回当前内存态的一份状态快照(供单测和观测使用)。
func (m *Manager) SnapshotStatus() StatusReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildStatusLocked(uint64(m.nowFn().UnixMilli()))
}

func (m *Manager) buildStatusLocked(nowMs uint64) StatusReport {
	return StatusReport{
		Service:        m.opts.ServiceName,
		Pod:            m.podName,
		Track:          m.releaseTrack,
		SchemaDigest:   m.schemaDigest,
		Source:         m.currentSource,
		LoadedDigest:   m.currentDigest,
		LoadedVersion:  m.currentVersion,
		LoadedAtMs:     m.loadedAtMs,
		LoadSeq:        m.loadSeq,
		PendingState:   m.pendingState,
		PendingDigest:  m.pendingDigest,
		PendingVersion: m.pendingVersion,
		PendingReason:  m.pendingReason,
		UpdatedAtMs:    nowMs,
	}
}

func (m *Manager) writeStatusBestEffort(nowMs uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeStatusLocked(nowMs)
}

func (m *Manager) writeStatusLocked(nowMs uint64) {
	if m.opts.StatusRoot == "" {
		return
	}
	report := m.buildStatusLocked(nowMs)
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		logx.Errorf("[TableBundle] marshal status failed service=%s err=%v", m.opts.ServiceName, err)
		return
	}
	statusPath := filepath.Join(m.opts.StatusRoot, StatusFilename)
	if err := WriteAtomicFile(statusPath, raw); err != nil {
		logx.Errorf("[TableBundle] write status %s failed service=%s err=%v", statusPath, m.opts.ServiceName, err)
	}
}
