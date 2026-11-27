// table-sync 是与业务 Pod 同生共死的原生 sidecar(docs/design/config-table-hot-reload.md §7):
//   - 全量读 + watch + 20 秒全量复核 etcd /mmorpg/tables/pointer/<track>/;
//   - 从对象存储下载引用到的表包,先核对压缩包 SHA-256、安全解包、再逐文件核对 manifest v2;
//   - 全部目标到位后才原子写 $MMORPG_TABLE_BUNDLE_ROOT/POINTER.json;
//   - 周期读取 $MMORPG_TABLE_STATUS_ROOT/STATUS.json,带 30 秒租约写回 etcd;
//   - 清理超过 10 分钟不再被引用的旧表包目录;
//   - 暴露 /readyz 与 /metrics。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/buildinfo"
	"shared/etcdwatch"
	"table_sync/internal/bundle"
	"table_sync/internal/pointer"
	"table_sync/internal/s3client"
)

const (
	resyncInterval       = 20 * time.Second
	statusReportInterval = 5 * time.Second
	statusLeaseTTLSec    = 30
	pruneInterval        = 60 * time.Second
	etcdOpTimeout        = 5 * time.Second
)

var (
	etcdLastReadTs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "table_sync_etcd_last_read_timestamp_seconds",
		Help: "Unix timestamp in seconds of the last successful etcd full pointer read.",
	})
	bundleFetchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "table_sync_bundle_fetch_total",
		Help: "Total table bundle fetch attempts by result.",
	}, []string{"result"})
)

type syncConfig struct {
	EtcdEndpoints []string
	Track         string
	BundleRoot    string
	StatusRoot    string
	PodName       string
	PodNamespace  string
	ListenAddr    string
	S3            s3client.Config
}

// statusIdentity 是本 Pod 在 etcd 状态键里的身份:<namespace>/<pod>,没有 namespace(不在 K8s 里跑)时只用 Pod 名。
// 只用 Pod 名不够:各 zone namespace 里的 StatefulSet 同名(guild-0、login-0、gate-0…),却共用 infra 的同一个 etcd,
// 同一轨道上会互相覆盖状态键。管理接口因此少算实例,还可能看不到某个实例的拒绝,发布工具就不会自动撤回。
func (c syncConfig) statusIdentity() string {
	if c.PodNamespace == "" {
		return c.PodName
	}
	return c.PodNamespace + "/" + c.PodName
}

type syncer struct {
	cfg   syncConfig
	etcd  *clientv3.Client
	s3    *s3client.Client
	ready atomic.Bool

	mu           sync.Mutex
	publishedDoc pointer.Document

	statusLeaseID clientv3.LeaseID
	// lastReportedStatus 是挂在当前租约下、最近一次写成功的状态值;换了租约或写失败就清空。内容没变时只续租不写。
	lastReportedStatus string
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

func parseConfig() (syncConfig, error) {
	showVersion := flag.Bool("version", false, "print build version and exit")
	etcdFlag := flag.String("etcd-endpoints", envOrDefault("MMORPG_TABLE_SYNC_ETCD_ENDPOINTS", ""), "comma-separated etcd endpoints")
	trackFlag := flag.String("track", envOrDefault("MMORPG_RELEASE_TRACK", "stable"), "release track (stable|canary)")
	bundleRootFlag := flag.String("bundle-root", envOrDefault("MMORPG_TABLE_BUNDLE_ROOT", "/var/lib/mmorpg/tables"), "directory for bundles/ and POINTER.json")
	statusRootFlag := flag.String("status-root", envOrDefault("MMORPG_TABLE_STATUS_ROOT", "/var/lib/mmorpg/tables-status"), "directory containing STATUS.json")
	podNameFlag := flag.String("pod-name", envOrDefault("POD_NAME", ""), "pod name used in etcd status key")
	podNamespaceFlag := flag.String("pod-namespace", envOrDefault("POD_NAMESPACE", ""), "pod namespace joined into the etcd status key so same-named pods in different zone namespaces do not overwrite each other")
	listenFlag := flag.String("listen", envOrDefault("MMORPG_TABLE_SYNC_LISTEN_ADDR", ":9099"), "HTTP listen address for /readyz and /metrics")
	s3EndpointFlag := flag.String("s3-endpoint", envOrDefault("MMORPG_TABLE_S3_ENDPOINT", ""), "S3 compatible endpoint URL")
	s3BucketFlag := flag.String("s3-bucket", envOrDefault("MMORPG_TABLE_S3_BUCKET", "mmorpg-tables"), "S3 bucket name")
	s3RegionFlag := flag.String("s3-region", envOrDefault("MMORPG_TABLE_S3_REGION", "us-east-1"), "S3 region")
	s3AccessKeyFlag := flag.String("s3-access-key", envOrDefault("MMORPG_TABLE_S3_ACCESS_KEY", ""), "S3 access key")
	s3SecretKeyFlag := flag.String("s3-secret-key", envOrDefault("MMORPG_TABLE_S3_SECRET_KEY", ""), "S3 secret key")
	s3PathStyleFlag := flag.Bool("s3-path-style", envBool("MMORPG_TABLE_S3_PATH_STYLE", true), "use path-style S3 URLs")
	s3InsecureFlag := flag.Bool("s3-insecure", envBool("MMORPG_TABLE_S3_INSECURE", false), "skip TLS certificate verification (dev only)")
	flag.Parse()

	fmt.Println(buildinfo.StartupLine("table-sync"))
	if *showVersion {
		os.Exit(0)
	}

	var endpoints []string
	for _, ep := range strings.Split(*etcdFlag, ",") {
		if trimmed := strings.TrimSpace(ep); trimmed != "" {
			endpoints = append(endpoints, trimmed)
		}
	}
	if len(endpoints) == 0 {
		return syncConfig{}, errors.New("MMORPG_TABLE_SYNC_ETCD_ENDPOINTS / -etcd-endpoints is required")
	}
	track := strings.TrimSpace(*trackFlag)
	if !pointer.ValidTrack(track) {
		return syncConfig{}, fmt.Errorf("invalid release track %q (want stable or canary)", track)
	}
	podName := strings.TrimSpace(*podNameFlag)
	if podName == "" {
		podName, _ = os.Hostname()
	}
	if podName == "" {
		return syncConfig{}, errors.New("POD_NAME / -pod-name is required")
	}
	return syncConfig{
		EtcdEndpoints: endpoints,
		Track:         track,
		BundleRoot:    strings.TrimSpace(*bundleRootFlag),
		StatusRoot:    strings.TrimSpace(*statusRootFlag),
		PodName:       podName,
		PodNamespace:  strings.TrimSpace(*podNamespaceFlag),
		ListenAddr:    strings.TrimSpace(*listenFlag),
		S3: s3client.Config{
			Endpoint:           strings.TrimSpace(*s3EndpointFlag),
			Bucket:             strings.TrimSpace(*s3BucketFlag),
			Region:             strings.TrimSpace(*s3RegionFlag),
			AccessKey:          strings.TrimSpace(*s3AccessKeyFlag),
			SecretKey:          strings.TrimSpace(*s3SecretKeyFlag),
			PathStyle:          *s3PathStyleFlag,
			InsecureSkipVerify: *s3InsecureFlag,
		},
	}, nil
}

func main() {
	prometheus.MustRegister(etcdLastReadTs, bundleFetchTotal)
	cfg, err := parseConfig()
	if err != nil {
		log.Fatalf("[table-sync] invalid config: %v", err)
	}

	var s3Cli *s3client.Client
	if cfg.S3.Endpoint != "" {
		s3Cli, err = s3client.New(cfg.S3)
		if err != nil {
			log.Fatalf("[table-sync] init s3 client failed: %v", err)
		}
	} else {
		log.Printf("[table-sync] WARN: S3 endpoint not set; only empty pointer lines can be served")
	}

	etcdCli, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   cfg.EtcdEndpoints,
		DialTimeout: etcdOpTimeout,
	}))
	if err != nil {
		log.Fatalf("[table-sync] connect etcd failed: %v", err)
	}
	defer etcdCli.Close()

	if err := os.MkdirAll(filepath.Join(cfg.BundleRoot, "bundles"), 0o755); err != nil {
		log.Fatalf("[table-sync] create bundles dir failed: %v", err)
	}

	s := &syncer{
		cfg:          cfg,
		etcd:         etcdCli,
		s3:           s3Cli,
		publishedDoc: pointer.Document{Track: cfg.Track, Lines: map[string]pointer.Line{}},
	}
	// sidecar 自己重启、业务容器还在跑时,上一次落盘的 POINTER.json 仍在共享卷里。把里面的线读回来当
	// 「上一次的合法状态」:重启后头几拍哪怕取不到表包,也是沿用旧指针,而不是让这条线从文件里消失。
	// Pod 第一次启动时文件不存在,什么都不带。
	pointerPath := filepath.Join(cfg.BundleRoot, pointer.PointerFilename)
	if prev, err := pointer.ReadDocument(pointerPath); err != nil {
		log.Printf("[table-sync] WARN: ignoring unreadable previous %s: %v", pointerPath, err)
	} else if prev != nil && prev.Track == cfg.Track {
		for schemaDigest, line := range prev.Lines {
			s.publishedDoc.Lines[schemaDigest] = line
		}
		log.Printf("[table-sync] restored %d pointer line(s) from previous %s", len(prev.Lines), pointerPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/metrics", promhttp.Handler())
	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		log.Printf("[table-sync] HTTP listening on %s track=%s pod=%s", cfg.ListenAddr, cfg.Track, cfg.statusIdentity())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[table-sync] HTTP server failed: %v", err)
		}
	}()

	// 启动首拍:不断重试直到首次完整拉取 etcd 并落盘 POINTER.json(随后 /readyz 变绿,业务容器才启动)。
	for {
		if err := s.syncFull(ctx); err != nil {
			log.Printf("[table-sync] initial sync failed (retrying in 2s): %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		s.ready.Store(true)
		log.Printf("[table-sync] initial sync ready track=%s lines=%d", cfg.Track, len(s.publishedDoc.Lines))
		break
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.runWatchAndResyncLoop(ctx) }()
	go func() { defer wg.Done(); s.runStatusReporterLoop(ctx) }()
	go func() { defer wg.Done(); s.runPruneLoop(ctx) }()

	<-ctx.Done()
	log.Printf("[table-sync] shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	wg.Wait()
	s.revokeStatusLease()
}

func (s *syncer) runWatchAndResyncLoop(ctx context.Context) {
	prefix := pointer.PointerPrefix(s.cfg.Track)
	resyncTicker := time.NewTicker(resyncInterval)
	defer resyncTicker.Stop()

	watchCtx, watchCancel := context.WithCancel(ctx)
	watchCh := s.etcd.Watch(watchCtx, prefix, clientv3.WithPrefix())
	defer watchCancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-resyncTicker.C:
			if err := s.syncFull(ctx); err != nil {
				log.Printf("[table-sync] periodic resync failed: %v", err)
			}
		case resp, ok := <-watchCh:
			if !ok || resp.Err() != nil {
				if resp.Err() != nil {
					log.Printf("[table-sync] etcd watch error (re-establishing): %v", resp.Err())
				}
				watchCancel()
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				watchCtx, watchCancel = context.WithCancel(ctx)
				watchCh = s.etcd.Watch(watchCtx, prefix, clientv3.WithPrefix())
				if err := s.syncFull(ctx); err != nil {
					log.Printf("[table-sync] post-reconnect resync failed: %v", err)
				}
				continue
			}
			if len(resp.Events) > 0 {
				if err := s.syncFull(ctx); err != nil {
					log.Printf("[table-sync] watch-triggered sync failed: %v", err)
				}
			}
		}
	}
}

// syncFull 全量读 etcd 指针前缀,逐线下载核对并解包所有引用的表包,全部就位后再原子写 POINTER.json。
// 某条线若下载/校验失败,该线保留上一次已落盘的合法状态(不把缺包的指针写给业务进程,设计 §7 / §10)。
func (s *syncer) syncFull(ctx context.Context) error {
	prefix := pointer.PointerPrefix(s.cfg.Track)
	readCtx, cancel := context.WithTimeout(ctx, etcdOpTimeout)
	resp, err := s.etcd.Get(readCtx, prefix, clientv3.WithPrefix())
	cancel()
	if err != nil {
		return fmt.Errorf("etcd get %s: %w", prefix, err)
	}
	etcdLastReadTs.Set(float64(time.Now().Unix()))

	s.mu.Lock()
	prevLines := make(map[string]pointer.Line, len(s.publishedDoc.Lines))
	for k, v := range s.publishedDoc.Lines {
		prevLines[k] = v
	}
	s.mu.Unlock()

	nextLines := make(map[string]pointer.Line, len(resp.Kvs))
	// 这一拍没同步下来的线:记下原因写进 POINTER.json。出错的线沿用上一次的合法内容;
	// 从没成功过的线不出现在 lines 里,业务进程启动时据此拒绝退回镜像里的旧表(pointer.Document.SyncErrors)。
	syncErrors := map[string]string{}
	for _, kv := range resp.Kvs {
		key := string(kv.Key)
		schemaDigest := strings.TrimPrefix(key, prefix)
		if !bundle.IsLowerHex(schemaDigest, 16) {
			log.Printf("[table-sync] WARN: ignoring malformed pointer key %q", key)
			continue
		}
		var line pointer.Line
		if err := json.Unmarshal(kv.Value, &line); err != nil {
			log.Printf("[table-sync] ERROR: malformed pointer JSON for line %s: %v", schemaDigest, err)
			syncErrors[schemaDigest] = pointer.TrimSyncError("malformed pointer JSON: " + err.Error())
			if prev, ok := prevLines[schemaDigest]; ok {
				nextLines[schemaDigest] = prev
			}
			continue
		}
		if err := s.ensureLineBundles(ctx, schemaDigest, &line); err != nil {
			log.Printf("[table-sync] ERROR: sync bundle failed for line %s (keeping previous pointer): %v", schemaDigest, err)
			syncErrors[schemaDigest] = pointer.TrimSyncError(err.Error())
			if prev, ok := prevLines[schemaDigest]; ok {
				nextLines[schemaDigest] = prev
			}
			continue
		}
		nextLines[schemaDigest] = line
	}

	nextDoc := pointer.Document{
		Track: s.cfg.Track,
		Lines: nextLines,
	}
	if len(syncErrors) > 0 {
		nextDoc.SyncErrors = syncErrors
	}
	raw, err := json.MarshalIndent(nextDoc, "", "  ")
	if err != nil {
		return err
	}
	pointerPath := filepath.Join(s.cfg.BundleRoot, pointer.PointerFilename)
	if err := pointer.WriteAtomicFile(pointerPath, raw); err != nil {
		return fmt.Errorf("write %s: %w", pointerPath, err)
	}

	s.mu.Lock()
	s.publishedDoc = nextDoc
	s.mu.Unlock()
	return nil
}

func (s *syncer) ensureLineBundles(ctx context.Context, schemaDigest string, line *pointer.Line) error {
	for _, target := range []*pointer.Target{line.Current, line.Next} {
		if target == nil || target.ContentDigest == "" {
			continue
		}
		if err := s.ensureBundle(ctx, schemaDigest, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *syncer) ensureBundle(ctx context.Context, schemaDigest string, target *pointer.Target) error {
	if !bundle.IsLowerHex(target.ContentDigest, 64) {
		bundleFetchTotal.WithLabelValues("invalid_pointer").Inc()
		return fmt.Errorf("invalid content_digest %q", target.ContentDigest)
	}
	if !bundle.IsLowerHex(target.BundleSHA256, 64) {
		bundleFetchTotal.WithLabelValues("invalid_pointer").Inc()
		return fmt.Errorf("invalid bundle_sha256 %q", target.BundleSHA256)
	}

	bundlesDir := filepath.Join(s.cfg.BundleRoot, "bundles")
	finalDir := filepath.Join(bundlesDir, target.ContentDigest)
	if _, err := bundle.VerifyDirManifest(finalDir, schemaDigest, target.ContentDigest); err == nil {
		bundleFetchTotal.WithLabelValues("hit").Inc()
		return nil
	}

	if s.s3 == nil {
		bundleFetchTotal.WithLabelValues("error").Inc()
		return errors.New("S3 client is not configured")
	}

	key := bundle.ObjectKey(schemaDigest, target.ContentDigest)
	tmpArchive, err := os.CreateTemp(bundlesDir, ".download-*.tar.gz")
	if err != nil {
		bundleFetchTotal.WithLabelValues("error").Inc()
		return err
	}
	tmpPath := tmpArchive.Name()
	defer os.Remove(tmpPath)

	fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	_, dlErr := s.s3.GetObjectToWriter(fetchCtx, key, tmpArchive, bundle.MaxBundleBytes)
	cancel()
	closeErr := tmpArchive.Close()
	if dlErr != nil {
		bundleFetchTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("download %s: %w", key, dlErr)
	}
	if closeErr != nil {
		bundleFetchTotal.WithLabelValues("error").Inc()
		return closeErr
	}

	if _, _, err := bundle.VerifyAndExtract(tmpPath, target.BundleSHA256, schemaDigest, target.ContentDigest, bundlesDir); err != nil {
		bundleFetchTotal.WithLabelValues("verify_failed").Inc()
		return fmt.Errorf("verify/extract %s: %w", key, err)
	}
	bundleFetchTotal.WithLabelValues("fetched").Inc()
	log.Printf("[table-sync] fetched and verified bundle schema_digest=%s content_digest=%s version=%d",
		schemaDigest, target.ContentDigest[:12], target.Version)
	return nil
}

func (s *syncer) runStatusReporterLoop(ctx context.Context) {
	ticker := time.NewTicker(statusReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reportStatusOnce(ctx); err != nil {
				log.Printf("[table-sync] report status failed: %v", err)
			}
		}
	}
}

func (s *syncer) reportStatusOnce(ctx context.Context) error {
	if s.cfg.StatusRoot == "" {
		return nil
	}
	statusPath := filepath.Join(s.cfg.StatusRoot, pointer.StatusFilename)
	st, err := pointer.ReadStatusFile(statusPath)
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	// 键与值里的 pod 一律用 sidecar 的身份(带 namespace):业务进程只知道自己的 Pod 名,写进来的值会和键对不上。
	st.Pod = s.cfg.statusIdentity()
	if st.Track == "" {
		st.Track = s.cfg.Track
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}

	opCtx, cancel := context.WithTimeout(ctx, etcdOpTimeout)
	defer cancel()

	// 维持或重建 30 秒租约,Pod 消失后状态键自动过期(设计 §6.1、§7)。
	if s.statusLeaseID != 0 {
		if _, err := s.etcd.KeepAliveOnce(opCtx, s.statusLeaseID); err != nil {
			s.statusLeaseID = 0
		}
	}
	if s.statusLeaseID == 0 {
		grant, err := s.etcd.Grant(opCtx, statusLeaseTTLSec)
		if err != nil {
			return fmt.Errorf("grant status lease: %w", err)
		}
		s.statusLeaseID = grant.ID
		s.lastReportedStatus = ""
	}

	// 存活靠续租表达(续租不写键、不产生新 revision),状态键只在内容变了或换了租约时才写。
	// 每 5 秒无条件写一次的话,etcd 写入量随 Pod 数线性涨:2000 个 Pod 就是每秒 400 次写键,
	// 而共用的 infra etcd 按 revision 只保留最近 100 万条历史,等于别的 watcher 只剩约 40 分钟的历史可追。
	// STATUS.json 只在状态变化时改写,稳态下内容不变,写入量趋于 0。
	// 代价:有人绕过管理接口手工删了这个键,要等下一次状态变化或换租约才会补回。
	if string(raw) == s.lastReportedStatus {
		return nil
	}
	key := pointer.StatusKey(s.cfg.Track, s.cfg.statusIdentity())
	if _, err := s.etcd.Put(opCtx, key, string(raw), clientv3.WithLease(s.statusLeaseID)); err != nil {
		s.statusLeaseID = 0
		s.lastReportedStatus = ""
		return fmt.Errorf("put status %s: %w", key, err)
	}
	s.lastReportedStatus = string(raw)
	return nil
}

func (s *syncer) revokeStatusLease() {
	if s.statusLeaseID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s.etcd.Revoke(ctx, s.statusLeaseID)
}

func (s *syncer) runPruneLoop(ctx context.Context) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()

	bundlesDir := filepath.Join(s.cfg.BundleRoot, "bundles")
	statusPath := filepath.Join(s.cfg.StatusRoot, pointer.StatusFilename)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			docCopy := pointer.Document{
				Track: s.publishedDoc.Track,
				Lines: make(map[string]pointer.Line, len(s.publishedDoc.Lines)),
			}
			for k, v := range s.publishedDoc.Lines {
				docCopy.Lines[k] = v
			}
			s.mu.Unlock()

			st, _ := pointer.ReadStatusFile(statusPath)
			refs := pointer.ReferencedDigests(&docCopy, st)
			removed, err := pointer.PruneUnusedBundles(bundlesDir, refs, pointer.DefaultPruneGrace, time.Now())
			if err != nil {
				log.Printf("[table-sync] prune bundles failed: %v", err)
			} else if removed > 0 {
				log.Printf("[table-sync] pruned %d unreferenced bundle directories", removed)
			}
		}
	}
}
