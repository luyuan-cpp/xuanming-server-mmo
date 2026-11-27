// tls.go —— MySQL / TiDB 权威连接池的严格 TLS 安全校验与私有 CA 加载器
// （移植自 xuanming-server/pkg/mysqlx/mysqlx.go）。
//
// 四条安全不变量：
//   1. 拒绝 DSN 中降级/跳过验签的 TLS 参数（skip-verify / preferred / AllowFallbackToPlaintext / TLS < 1.2）；
//   2. 私有 CA 包仅加载进新建 x509.NewCertPool()，绝不混入 SystemCertPool，防任意公共 CA 冒签内部库证书；
//   3. 严格逐块解析 CA PEM 文件（常规文件、上限 1 MiB、每块必须是合法 CERTIFICATE，拒绝损坏块或尾随垃圾）；
//   4. 校验 TCP DSN 主机名严格等于 tls_server_name（防证书名错配），并在 NewConnector 后立刻注销临时 TLS key。
package authoritysql

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
)

const maxStrictTLSCAFileBytes = 1 << 20

var (
	// ErrUnsafeTLSConfig 表示 DSN 或 TLS 配置存在跳过证书校验、允许明文回退或低版本 TLS 等安全漏洞。
	ErrUnsafeTLSConfig = errors.New("authority SQL TLS configuration is unsafe")

	strictTLSSeq atomic.Uint64
)

// StrictTLSConfig 指定权威 SQL 连接池使用的纯私有 CA 包与期望服务端主机名。
type StrictTLSConfig struct {
	CAFile     string
	ServerName string
}

// WithStrictTLS 为 Open / OpenUnverified / OpenForMigration 启用纯私有 CA 包 TLS 校验。
// CAFile 与 ServerName 必须同时非空（同时为空等价于不启用）。
func WithStrictTLS(caFile, serverName string) OpenOption {
	return func(o *openOptions) {
		o.strictTLS = StrictTLSConfig{
			CAFile:     strings.TrimSpace(caFile),
			ServerName: strings.TrimSpace(serverName),
		}
	}
}

// ValidateDSNTLSSafety 拦截 DSN 中的不安全 TLS 配置（skip-verify / preferred / 明文回退 / TLS < 1.2）。
// 未开启 TLS（TLSConfig=="" 且 TLS==nil）的本地开发连接直接放行。
func ValidateDSNTLSSafety(cfg *mysql.Config) error {
	if cfg == nil {
		return errors.New("authority SQL mysql config is nil")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.TLSConfig))
	if mode == "skip-verify" || mode == "preferred" || cfg.AllowFallbackToPlaintext || isUnsafeTLSConfig(cfg.TLS) {
		return fmt.Errorf("%w: mode=%q allow_plaintext_fallback=%t", ErrUnsafeTLSConfig, cfg.TLSConfig, cfg.AllowFallbackToPlaintext)
	}
	return nil
}

func isUnsafeTLSConfig(cfg *tls.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.InsecureSkipVerify ||
		(cfg.MinVersion != 0 && cfg.MinVersion < tls.VersionTLS12) ||
		(cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS12)
}

// LoadStrictRootCAs 将 path 指向的私有 CA 包加载到独立的 x509.CertPool（不混入 SystemCertPool）。
// 文件中出现任何非 CERTIFICATE 块、损坏证书或尾随非 PEM 字节均 fail-closed 返回错误。
func LoadStrictRootCAs(path string) (*x509.CertPool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("authority SQL TLS CA file must be a regular file")
	}
	if info.Size() > maxStrictTLSCAFileBytes {
		return nil, fmt.Errorf("authority SQL TLS CA file size %d exceeds %d-byte limit", info.Size(), maxStrictTLSCAFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	pemBytes, err := io.ReadAll(io.LimitReader(file, maxStrictTLSCAFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(pemBytes) > maxStrictTLSCAFileBytes {
		return nil, fmt.Errorf("authority SQL TLS CA content exceeds %d-byte limit", maxStrictTLSCAFileBytes)
	}

	rest := pemBytes
	certCount := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		block, next := pem.Decode(rest)
		if block == nil {
			return nil, errors.New("authority SQL TLS CA file contains non-PEM trailing data")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("authority SQL TLS CA file contains PEM block %q, want CERTIFICATE", block.Type)
		}
		if _, parseErr := x509.ParseCertificate(block.Bytes); parseErr != nil {
			return nil, fmt.Errorf("authority SQL TLS CA parse certificate: %w", parseErr)
		}
		certCount++
		rest = next
	}
	if certCount == 0 {
		return nil, errors.New("authority SQL TLS CA file contains no certificate")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("authority SQL TLS CA append to bundle-only pool failed")
	}
	return pool, nil
}

// applyStrictTLS 将 StrictTLSConfig 校验并绑定到 parsed 上；返回的 cleanup 应在 mysql.NewConnector 后调用。
func applyStrictTLS(parsed *mysql.Config, cfg StrictTLSConfig, skipHostMatch bool) (cleanup func(), err error) {
	if err := ValidateDSNTLSSafety(parsed); err != nil {
		return nil, err
	}
	if cfg.CAFile == "" && cfg.ServerName == "" {
		return func() {}, nil
	}
	if (cfg.CAFile == "") != (cfg.ServerName == "") {
		return nil, fmt.Errorf("%w: ca_file and server_name must be configured together", ErrUnsafeTLSConfig)
	}
	if parsed.TLSConfig != "" || parsed.TLS != nil {
		return nil, fmt.Errorf("%w: DSN must not specify tls when WithStrictTLS is used", ErrUnsafeTLSConfig)
	}
	if !skipHostMatch {
		if parsed.Net != "tcp" {
			return nil, fmt.Errorf("%w: strict TLS requires tcp DSN, got %q", ErrUnsafeTLSConfig, parsed.Net)
		}
		host, _, splitErr := net.SplitHostPort(parsed.Addr)
		if splitErr != nil {
			return nil, fmt.Errorf("%w: parse tcp address %q: %v", ErrUnsafeTLSConfig, parsed.Addr, splitErr)
		}
		if !strings.EqualFold(host, cfg.ServerName) {
			return nil, fmt.Errorf("%w: DSN host %q must equal server_name %q", ErrUnsafeTLSConfig, host, cfg.ServerName)
		}
	}
	rootCAs, err := LoadStrictRootCAs(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("authoritysql-strict-%d", strictTLSSeq.Add(1))
	if err := mysql.RegisterTLSConfig(key, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            rootCAs,
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: false,
	}); err != nil {
		return nil, fmt.Errorf("register strict TLS config: %w", err)
	}
	parsed.TLSConfig = key
	return func() { mysql.DeregisterTLSConfig(key) }, nil
}
