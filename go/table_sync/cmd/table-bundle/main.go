// table-bundle 是配置表热更发布命令行工具(docs/design/config-table-hot-reload.md §5.4、§6.4、§9):
//   - pack:        校验 generated/tables 并打成不可变 tar.gz + .sha256;
//   - upload:      上传到对象存储(已存在且 sha256 相同则跳过,不同则拒绝覆盖);
//   - verify:      用真实服务二进制执行 --validate-tables 预检;
//   - publish:     调 gateway_node PUT /admin/tables,到点前轮询若任一实例 rejected 自动撤回 next,到点后等收敛;
//   - rollback:    检查 ids_permanent 表是否会丢失行(命中须加 -force),然后立即切回旧包。检查缺省必做:要给 -current-dir 与 -target-dir,
//     并核对它们分别是这条线的线上生效包与回滚目标包;只有 -force 才能不给目录;
//   - cancel:      撤销未到点的 next(DELETE /admin/tables/next);
//   - delete-line: 删除已无在线实例回报的旧 schema 线(DELETE /admin/tables);
//   - status:      查询当前所有轨道指针与实例回报汇总(GET /admin/tables)。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"table_sync/internal/bundle"
	"table_sync/internal/pointer"
	"table_sync/internal/s3client"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "pack":
		err = cmdPack(args)
	case "upload":
		err = cmdUpload(args)
	case "verify":
		err = cmdVerify(args)
	case "publish":
		err = cmdPublish(args)
	case "rollback":
		err = cmdRollback(args)
	case "cancel":
		err = cmdCancel(args)
	case "delete-line":
		err = cmdDeleteLine(args)
	case "status":
		err = cmdStatus(args)
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[table-bundle] %s failed: %v\n", cmd, err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: table-bundle <subcommand> [flags]

Subcommands:
  pack         Pack generated/tables into immutable tar.gz + .sha256
  upload       Upload packed bundle to S3-compatible object storage (no overwrite on mismatch)
  verify       Run --validate-tables across service binaries against a table directory
  publish      PUT /admin/tables, auto-cancel on pre-activation rejection, wait for convergence
  rollback     Immediate switch back to a previous bundle (requires -current-dir/-target-dir to guard cfg_ids_permanent unless -force)
  cancel       DELETE /admin/tables/next before activate_at
  delete-line  DELETE /admin/tables for an unused schema_digest line
  status       GET /admin/tables and display line pointers + pod convergence summary
`)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	dir := fs.String("dir", "generated/tables", "source generated tables directory")
	out := fs.String("out", "run/table_bundles/out", "output directory for tar.gz and .sha256")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := bundle.Pack(*dir, *out)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func cmdUpload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	dir := fs.String("dir", "generated/tables", "source generated tables directory (packed into temp dir when -archive is empty)")
	archive := fs.String("archive", "", "optional existing tar.gz produced by pack")
	schemaDigest := fs.String("schema-digest", "", "required only when -archive is set")
	contentDigest := fs.String("content-digest", "", "required only when -archive is set")
	endpoint := fs.String("s3-endpoint", envOr("MMORPG_TABLE_S3_ENDPOINT", ""), "S3 endpoint URL")
	bucket := fs.String("s3-bucket", envOr("MMORPG_TABLE_S3_BUCKET", "mmorpg-tables"), "S3 bucket")
	region := fs.String("s3-region", envOr("MMORPG_TABLE_S3_REGION", "us-east-1"), "S3 region")
	accessKey := fs.String("s3-access-key", envOr("MMORPG_TABLE_S3_ACCESS_KEY", ""), "S3 write access key")
	secretKey := fs.String("s3-secret-key", envOr("MMORPG_TABLE_S3_SECRET_KEY", ""), "S3 write secret key")
	pathStyle := fs.Bool("s3-path-style", true, "use path-style S3 URLs")
	insecure := fs.Bool("s3-insecure", false, "skip TLS verification (dev only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var archivePath, shaHex, sDigest, cDigest string
	var version uint64
	if strings.TrimSpace(*archive) == "" {
		tmpDir, err := os.MkdirTemp("", "table-bundle-upload-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)
		res, err := bundle.Pack(*dir, tmpDir)
		if err != nil {
			return err
		}
		archivePath = res.ArchivePath
		shaHex = res.BundleSHA256
		sDigest = res.SchemaDigest
		cDigest = res.ContentDigest
		version = res.Version
	} else {
		archivePath = *archive
		sDigest = strings.TrimSpace(*schemaDigest)
		cDigest = strings.TrimSpace(*contentDigest)
		if !bundle.IsLowerHex(sDigest, 16) || !bundle.IsLowerHex(cDigest, 64) {
			return errors.New("-schema-digest (16 hex) and -content-digest (64 hex) are required with -archive")
		}
		_, sum, err := bundle.HashFile(archivePath)
		if err != nil {
			return err
		}
		shaHex = sum
	}

	cli, err := s3client.New(s3client.Config{
		Endpoint:           *endpoint,
		Bucket:             *bucket,
		Region:             *region,
		AccessKey:          *accessKey,
		SecretKey:          *secretKey,
		PathStyle:          *pathStyle,
		InsecureSkipVerify: *insecure,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	objKey := bundle.ObjectKey(sDigest, cDigest)
	shaKey := bundle.ObjectSHA256Key(sDigest, cDigest)

	// 不可变校验(设计 §5.2):同名对象已存在且 sha256 一致则跳过,不同则拒绝覆盖。
	exists, err := cli.HeadObject(ctx, objKey)
	if err != nil {
		return fmt.Errorf("check existing object %s: %w", objKey, err)
	}
	if exists {
		remoteSHA, err := cli.GetObjectBytes(ctx, shaKey, 512)
		if err != nil {
			return fmt.Errorf("object %s already exists but %s could not be verified: %w", objKey, shaKey, err)
		}
		remoteFields := strings.Fields(string(remoteSHA))
		if len(remoteFields) == 0 || remoteFields[0] != shaHex {
			return fmt.Errorf("immutability violation: remote %s has sha256=%q, local=%s; refusing to overwrite",
				objKey, strings.TrimSpace(string(remoteSHA)), shaHex)
		}
		fmt.Printf("[table-bundle] already uploaded (sha256 verified) key=%s schema_digest=%s content_digest=%s bundle_sha256=%s version=%d\n",
			objKey, sDigest, cDigest, shaHex, version)
		return nil
	}

	data, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	if err := cli.PutObject(ctx, objKey, data, "application/gzip"); err != nil {
		return fmt.Errorf("upload %s: %w", objKey, err)
	}
	shaPayload := []byte(fmt.Sprintf("%s  %s\n", shaHex, filepath.Base(objKey)))
	if err := cli.PutObject(ctx, shaKey, shaPayload, "text/plain; charset=utf-8"); err != nil {
		return fmt.Errorf("upload %s: %w", shaKey, err)
	}
	fmt.Printf("[table-bundle] uploaded key=%s bytes=%d schema_digest=%s content_digest=%s bundle_sha256=%s version=%d\n",
		objKey, len(data), sDigest, cDigest, shaHex, version)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "generated/tables", "table directory to validate")
	bins := fs.String("bins", "", "comma-separated service binaries to run --validate-tables against")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := bundle.VerifyDirManifest(*dir, "", "")
	if err != nil {
		return fmt.Errorf("manifest check failed: %w", err)
	}
	fmt.Printf("[table-bundle] manifest OK schema_digest=%s content_digest=%s version=%d tables=%d\n",
		manifest.SchemaDigest, manifest.ContentDigest[:12], manifest.Version, len(manifest.Tables))

	for _, rawBin := range strings.Split(*bins, ",") {
		bin := strings.TrimSpace(rawBin)
		if bin == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		c := exec.CommandContext(ctx, bin, "--validate-tables", *dir)
		out, runErr := c.CombinedOutput()
		cancel()
		if runErr != nil {
			return fmt.Errorf("binary %s rejected tables: %v\n%s", bin, runErr, strings.TrimSpace(string(out)))
		}
		fmt.Printf("[table-bundle] verified binary=%s\n", bin)
	}
	return nil
}

type adminClient struct {
	baseURL  string
	adminKey string
	operator string
	httpCli  *http.Client
}

// tableBundleOperator 是网关机器口令(admin.machine-key)唯一接受的操作人:用机器口令发布 / 回滚时不用再传 -operator;
// 用人用口令时要传自己的名字,网关不许人用口令以这个名字写(docs/design/admin-identity-and-dual-control.md §11.3)。
const tableBundleOperator = "table-bundle"

func newAdminClient(baseURL, adminKey, operator string) (*adminClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("-admin-url / MMORPG_ADMIN_URL is required")
	}
	if strings.TrimSpace(adminKey) == "" {
		return nil, errors.New("-admin-key / MMORPG_ADMIN_KEY is required")
	}
	return &adminClient{
		baseURL:  baseURL,
		adminKey: strings.TrimSpace(adminKey),
		operator: strings.TrimSpace(operator),
		httpCli:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *adminClient) doJSON(method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.baseURL+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Admin-Key", c.adminKey)
	if c.operator != "" {
		req.Header.Set("X-Admin-Operator", c.operator)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	return respBytes, resp.StatusCode, err
}

type adminLineView struct {
	Track        string `json:"track"`
	SchemaDigest string `json:"schema_digest"`
	// Active 是此刻生效的那一包(管理接口按 activate_at 在 current 与 next 里选好的);没有指针时为空。
	Active *struct {
		ContentDigest string `json:"content_digest"`
	} `json:"active"`
	Summary struct {
		Total        int `json:"total"`
		Loaded       int `json:"loaded"`
		Prepared     int `json:"prepared"`
		Rejected     int `json:"rejected"`
		NeedsRestart int `json:"needs_restart"`
	} `json:"summary"`
	RejectedPods []struct {
		Pod    string `json:"pod"`
		Reason string `json:"reason"`
	} `json:"rejected_pods"`
}

func (c *adminClient) getStatus() ([]adminLineView, []byte, error) {
	raw, code, err := c.doJSON(http.MethodGet, "/admin/tables", nil)
	if err != nil {
		return nil, nil, err
	}
	if code < 200 || code >= 300 {
		return nil, raw, fmt.Errorf("GET /admin/tables returned %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var lines []adminLineView
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, raw, fmt.Errorf("parse GET /admin/tables response: %w", err)
	}
	return lines, raw, nil
}

// activeContentDigest 返回这条轨道、这条 schema 线此刻生效的包。没有生效指针时进程用的是镜像自带的表,
// 它的摘要从管理接口拿不到、也谈不上「回滚」,按错误返回。
func (c *adminClient) activeContentDigest(track, schemaDigest string) (string, error) {
	lines, _, err := c.getStatus()
	if err != nil {
		return "", err
	}
	for _, line := range lines {
		if line.Track == track && line.SchemaDigest == schemaDigest && line.Active != nil && line.Active.ContentDigest != "" {
			return line.Active.ContentDigest, nil
		}
	}
	return "", fmt.Errorf("no active table bundle pointer for track=%s schema_digest=%s; nothing to roll back from", track, schemaDigest)
}

func (c *adminClient) cancelNext(track, schemaDigest string) error {
	q := url.Values{}
	q.Set("track", track)
	q.Set("schema_digest", schemaDigest)
	raw, code, err := c.doJSON(http.MethodDelete, "/admin/tables/next?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("DELETE /admin/tables/next returned %d: %s", code, strings.TrimSpace(string(raw)))
	}
	return nil
}

func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envOr("MMORPG_ADMIN_URL", ""), "gateway_node admin base URL")
	adminKey := fs.String("admin-key", envOr("MMORPG_ADMIN_KEY", ""), "X-Admin-Key value")
	operator := fs.String("operator", envOr("MMORPG_ADMIN_OPERATOR", tableBundleOperator), "X-Admin-Operator value (the gateway machine key only writes as "+tableBundleOperator+"; with the human key pass your own name)")
	track := fs.String("track", "stable", "release track (stable|canary)")
	schemaDigest := fs.String("schema-digest", "", "16-hex schema digest")
	contentDigest := fs.String("content-digest", "", "64-hex content digest")
	bundleSHA := fs.String("bundle-sha256", "", "64-hex tar.gz sha256")
	version := fs.Uint64("version", 0, "human-readable manifest version")
	activateAt := fs.String("activate-at", "", "RFC3339 activation timestamp (default: now + 60s)")
	immediate := fs.Bool("immediate", false, "activate immediately without waiting")
	reason := fs.String("reason", "", "audit reason")
	waitTimeout := fs.Duration("wait", 2*time.Minute, "max time to wait for convergence after activate_at")
	noWait := fs.Bool("no-wait", false, "return immediately after PUT without polling convergence")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !pointer.ValidTrack(*track) {
		return fmt.Errorf("invalid track %q", *track)
	}
	if !bundle.IsLowerHex(*schemaDigest, 16) || !bundle.IsLowerHex(*contentDigest, 64) || !bundle.IsLowerHex(*bundleSHA, 64) {
		return errors.New("-schema-digest (16 hex), -content-digest (64 hex), and -bundle-sha256 (64 hex) are required")
	}

	var activateAtMs uint64
	if !*immediate {
		if strings.TrimSpace(*activateAt) != "" {
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*activateAt))
			if err != nil {
				return fmt.Errorf("parse -activate-at (RFC3339): %w", err)
			}
			activateAtMs = uint64(parsed.UnixMilli())
		} else {
			activateAtMs = uint64(time.Now().Add(65 * time.Second).UnixMilli())
		}
	}

	cli, err := newAdminClient(*adminURL, *adminKey, *operator)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"track":          *track,
		"schema_digest":  *schemaDigest,
		"content_digest": *contentDigest,
		"bundle_sha256":  *bundleSHA,
		"version":        *version,
		"immediate":      *immediate,
		"reason":         *reason,
	}
	if activateAtMs > 0 {
		payload["activate_at_ms"] = activateAtMs
	}

	raw, code, err := cli.doJSON(http.MethodPut, "/admin/tables", payload)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("PUT /admin/tables returned %d: %s", code, strings.TrimSpace(string(raw)))
	}
	fmt.Printf("[table-bundle] pointer published track=%s schema_digest=%s content_digest=%s immediate=%v activate_at_ms=%d\n",
		*track, *schemaDigest, (*contentDigest)[:12], *immediate, activateAtMs)

	if *noWait {
		return nil
	}

	deadline := time.Now().Add(*waitTimeout)
	if activateAtMs > 0 {
		actTime := time.UnixMilli(int64(activateAtMs))
		if d := actTime.Add(*waitTimeout); d.After(deadline) {
			deadline = d
		}
	}

	for time.Now().Before(deadline) {
		lines, _, err := cli.getStatus()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[table-bundle] WARN: poll status failed: %v\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		var matched *adminLineView
		for i := range lines {
			if lines[i].Track == *track && lines[i].SchemaDigest == *schemaDigest {
				matched = &lines[i]
				break
			}
		}
		if matched != nil {
			nowMs := uint64(time.Now().UnixMilli())
			if matched.Summary.Rejected > 0 {
				// 到点前任一实例拒绝 -> 自动撤回 next 并返回非 0(设计 §6.4)。
				if !*immediate && activateAtMs > 0 && nowMs < activateAtMs {
					fmt.Fprintf(os.Stderr, "[table-bundle] ERROR: %d pod(s) rejected bundle before activate_at; cancelling next\n",
						matched.Summary.Rejected)
					if cancelErr := cli.cancelNext(*track, *schemaDigest); cancelErr != nil {
						fmt.Fprintf(os.Stderr, "[table-bundle] ERROR: auto-cancel failed: %v\n", cancelErr)
					}
				}
				for _, rp := range matched.RejectedPods {
					fmt.Fprintf(os.Stderr, "  rejected pod=%s reason=%s\n", rp.Pod, rp.Reason)
				}
				return fmt.Errorf("%d pod(s) rejected bundle", matched.Summary.Rejected)
			}
			if (*immediate || nowMs >= activateAtMs) && matched.Summary.Total > 0 {
				if matched.Summary.Loaded == matched.Summary.Total {
					fmt.Printf("[table-bundle] converged: %d/%d pods loaded\n", matched.Summary.Loaded, matched.Summary.Total)
					return nil
				}
				if matched.Summary.Loaded+matched.Summary.NeedsRestart == matched.Summary.Total && matched.Summary.NeedsRestart > 0 {
					fmt.Printf("[table-bundle] finished with needs_restart=%d loaded=%d total=%d (rolling restart required)\n",
						matched.Summary.NeedsRestart, matched.Summary.Loaded, matched.Summary.Total)
					return nil
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return errors.New("timed out waiting for all pods to converge")
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envOr("MMORPG_ADMIN_URL", ""), "gateway_node admin base URL")
	adminKey := fs.String("admin-key", envOr("MMORPG_ADMIN_KEY", ""), "X-Admin-Key value")
	operator := fs.String("operator", envOr("MMORPG_ADMIN_OPERATOR", tableBundleOperator), "X-Admin-Operator value (the gateway machine key only writes as "+tableBundleOperator+"; with the human key pass your own name)")
	track := fs.String("track", "stable", "release track")
	schemaDigest := fs.String("schema-digest", "", "16-hex schema digest")
	targetDigest := fs.String("target-digest", "", "64-hex previous content digest")
	targetSHA := fs.String("target-sha256", "", "64-hex previous bundle sha256")
	targetVersion := fs.Uint64("target-version", 0, "previous manifest version")
	currentDir := fs.String("current-dir", "", "tables dir of the live bundle on this line (required unless -force)")
	targetDir := fs.String("target-dir", "", "tables dir of the rollback target bundle (required unless -force)")
	force := fs.Bool("force", false, "roll back even if cfg_ids_permanent IDs disappear; without -current-dir/-target-dir the check is skipped")
	reason := fs.String("reason", "rollback", "audit reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !bundle.IsLowerHex(*schemaDigest, 16) || !bundle.IsLowerHex(*targetDigest, 64) {
		return errors.New("-schema-digest (16 hex) and -target-digest (64 hex) are required")
	}

	cli, err := newAdminClient(*adminURL, *adminKey, *operator)
	if err != nil {
		return err
	}
	if err := guardPermanentIDRollback(cli, *track, *schemaDigest, *targetDigest, *currentDir, *targetDir, *force); err != nil {
		return err
	}

	return cmdPublish([]string{
		"-admin-url", *adminURL,
		"-admin-key", *adminKey,
		"-operator", *operator,
		"-track", *track,
		"-schema-digest", *schemaDigest,
		"-content-digest", *targetDigest,
		"-bundle-sha256", *targetSHA,
		"-version", fmt.Sprintf("%d", *targetVersion),
		"-immediate",
		"-reason", *reason,
	})
}

// guardPermanentIDRollback 是回滚前的只增不删表检查(设计 §9「回滚」)。缺省必做:两个目录都要给,
// current-dir 必须是这条线此刻生效的那一包(按管理接口的 active 核对),target-dir 必须是 -target-digest 那一包。
// -force 只放过两件事:不给目录(跳过检查)、比出了丢行(照样回滚);给了目录却对不上,-force 也不放过。
func guardPermanentIDRollback(cli *adminClient, track, schemaDigest, targetDigest, currentDir, targetDir string, force bool) error {
	if currentDir == "" || targetDir == "" {
		if !force {
			return errors.New("rollback requires -current-dir and -target-dir to check cfg_ids_permanent tables; pass -force to roll back without the check")
		}
		fmt.Fprintln(os.Stderr, "[table-bundle] WARN: -force without -current-dir/-target-dir; cfg_ids_permanent check skipped")
		return nil
	}
	liveDigest, err := cli.activeContentDigest(track, schemaDigest)
	if err != nil {
		return err
	}
	losses, err := bundle.CheckPermanentIDRollback(currentDir, targetDir, schemaDigest, liveDigest, targetDigest)
	if err != nil {
		return fmt.Errorf("permanent-id rollback check failed: %w", err)
	}
	if len(losses) == 0 {
		return nil
	}
	var parts []string
	for _, l := range losses {
		parts = append(parts, fmt.Sprintf("%s=%v", l.Table, l.MissingIDs))
	}
	if !force {
		return fmt.Errorf("rollback would remove cfg_ids_permanent IDs (%s); prefer forward fix or pass -force",
			strings.Join(parts, "; "))
	}
	fmt.Fprintf(os.Stderr, "[table-bundle] WARN: -force rollback removes cfg_ids_permanent IDs (%s)\n", strings.Join(parts, "; "))
	return nil
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envOr("MMORPG_ADMIN_URL", ""), "gateway_node admin base URL")
	adminKey := fs.String("admin-key", envOr("MMORPG_ADMIN_KEY", ""), "X-Admin-Key value")
	operator := fs.String("operator", envOr("MMORPG_ADMIN_OPERATOR", tableBundleOperator), "X-Admin-Operator value (the gateway machine key only writes as "+tableBundleOperator+"; with the human key pass your own name)")
	track := fs.String("track", "stable", "release track")
	schemaDigest := fs.String("schema-digest", "", "16-hex schema digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cli, err := newAdminClient(*adminURL, *adminKey, *operator)
	if err != nil {
		return err
	}
	if err := cli.cancelNext(*track, *schemaDigest); err != nil {
		return err
	}
	fmt.Printf("[table-bundle] cancelled next pointer track=%s schema_digest=%s\n", *track, *schemaDigest)
	return nil
}

func cmdDeleteLine(args []string) error {
	fs := flag.NewFlagSet("delete-line", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envOr("MMORPG_ADMIN_URL", ""), "gateway_node admin base URL")
	adminKey := fs.String("admin-key", envOr("MMORPG_ADMIN_KEY", ""), "X-Admin-Key value")
	operator := fs.String("operator", envOr("MMORPG_ADMIN_OPERATOR", tableBundleOperator), "X-Admin-Operator value (the gateway machine key only writes as "+tableBundleOperator+"; with the human key pass your own name)")
	track := fs.String("track", "stable", "release track")
	schemaDigest := fs.String("schema-digest", "", "16-hex schema digest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cli, err := newAdminClient(*adminURL, *adminKey, *operator)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("track", *track)
	q.Set("schema_digest", *schemaDigest)
	raw, code, err := cli.doJSON(http.MethodDelete, "/admin/tables?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("DELETE /admin/tables returned %d: %s", code, strings.TrimSpace(string(raw)))
	}
	fmt.Printf("[table-bundle] deleted schema line track=%s schema_digest=%s\n", *track, *schemaDigest)
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	adminURL := fs.String("admin-url", envOr("MMORPG_ADMIN_URL", ""), "gateway_node admin base URL")
	adminKey := fs.String("admin-key", envOr("MMORPG_ADMIN_KEY", ""), "X-Admin-Key value")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cli, err := newAdminClient(*adminURL, *adminKey, "")
	if err != nil {
		return err
	}
	_, raw, err := cli.getStatus()
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println(pretty.String())
	return nil
}
