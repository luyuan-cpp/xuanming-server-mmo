// Package s3client 提供基于 Go 标准库的最小 AWS SigV4 对象存储客户端
// (HeadObject / GetObject / PutObject),零外部依赖,兼容 AWS S3、OSS、COS、Ceph RGW、MinIO 与 SeaweedFS。
package s3client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotFound 表示对象不存在(HTTP 404)。
var ErrNotFound = errors.New("s3 object not found")

// Config 配置 S3 兼容端点与凭据。
type Config struct {
	Endpoint           string
	Region             string
	Bucket             string
	AccessKey          string
	SecretKey          string
	PathStyle          bool
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// Client 是线程安全的 S3 HTTP 客户端。
type Client struct {
	endpoint  *url.URL
	region    string
	bucket    string
	accessKey string
	secretKey string
	pathStyle bool
	httpCli   *http.Client
	nowFn     func() time.Time
}

// New 校验配置并创建客户端。
func New(cfg Config) (*Client, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		return nil, errors.New("s3client: endpoint is required")
	}
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	parsed, err := url.Parse(ep)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("s3client: invalid endpoint %q", cfg.Endpoint)
	}
	bucket := strings.Trim(strings.TrimSpace(cfg.Bucket), "/")
	if bucket == "" {
		return nil, errors.New("s3client: bucket is required")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // dev/self-signed S3 only
	}
	return &Client{
		endpoint:  parsed,
		region:    region,
		bucket:    bucket,
		accessKey: strings.TrimSpace(cfg.AccessKey),
		secretKey: strings.TrimSpace(cfg.SecretKey),
		pathStyle: cfg.PathStyle,
		httpCli:   &http.Client{Transport: tr, Timeout: timeout},
		nowFn:     time.Now,
	}, nil
}

// HeadObject 检查对象是否存在;不存在返回 (false, nil)。
func (c *Client) HeadObject(ctx context.Context, key string) (bool, error) {
	req, err := c.newSignedRequest(ctx, http.MethodHead, key, nil, "")
	if err != nil {
		return false, err
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}
	return false, fmt.Errorf("s3 HEAD %s returned status %d", key, resp.StatusCode)
}

// GetObjectToWriter 流式下载对象到 dst,最多读取 maxBytes 字节(超过直接拒绝,防止超大包撑爆磁盘)。
func (c *Client) GetObjectToWriter(ctx context.Context, key string, dst io.Writer, maxBytes int64) (int64, error) {
	req, err := c.newSignedRequest(ctx, http.MethodGet, key, nil, "")
	if err != nil {
		return 0, err
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("s3 GET %s returned status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if maxBytes > 0 {
		if resp.ContentLength > maxBytes {
			return 0, fmt.Errorf("s3 GET %s content-length %d exceeds max %d", key, resp.ContentLength, maxBytes)
		}
		n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return n, err
		}
		if n > maxBytes {
			return n, fmt.Errorf("s3 GET %s body exceeds max %d bytes", key, maxBytes)
		}
		return n, nil
	}
	return io.Copy(dst, resp.Body)
}

// GetObjectBytes 下载小对象(.sha256 等)到内存。
func (c *Client) GetObjectBytes(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := c.GetObjectToWriter(ctx, key, &buf, maxBytes); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PutObject 上传对象(整块计算 SHA-256 用于 SigV4 x-amz-content-sha256)。
func (c *Client) PutObject(ctx context.Context, key string, body []byte, contentType string) error {
	req, err := c.newSignedRequest(ctx, http.MethodPut, key, body, contentType)
	if err != nil {
		return err
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 PUT %s returned status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (c *Client) objectURL(key string) (rawURL string, host string, canonicalURI string) {
	cleanKey := strings.TrimLeft(key, "/")
	encodedKey := encodeS3Path(cleanKey)
	basePath := strings.TrimRight(c.endpoint.Path, "/")

	if c.pathStyle {
		canonicalURI = basePath + "/" + encodeS3Segment(c.bucket) + "/" + encodedKey
		host = c.endpoint.Host
	} else {
		canonicalURI = basePath + "/" + encodedKey
		host = c.bucket + "." + c.endpoint.Host
	}
	rawURL = c.endpoint.Scheme + "://" + host + canonicalURI
	return rawURL, host, canonicalURI
}

func (c *Client) newSignedRequest(ctx context.Context, method, key string, body []byte, contentType string) (*http.Request, error) {
	rawURL, host, canonicalURI := c.objectURL(key)
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Host = host
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// 未配置凭据时允许匿名访问(仅本地 SeaweedFS dev 夹具场景)。
	if c.accessKey == "" || c.secretKey == "" {
		return req, nil
	}

	now := c.nowFn().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadSum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payloadSum[:])

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{
		"host":                 strings.ToLower(host),
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if contentType != "" {
		headers["content-type"] = strings.TrimSpace(contentType)
	}

	signedNames := make([]string, 0, len(headers))
	for k := range headers {
		signedNames = append(signedNames, k)
	}
	sort.Strings(signedNames)

	var canonHeaders strings.Builder
	for _, k := range signedNames {
		canonHeaders.WriteString(k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(headers[k])
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signedNames, ";")
	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		"",
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	canonHash := sha256.Sum256([]byte(canonicalRequest))

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, c.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hex.EncodeToString(canonHash[:]),
	}, "\n")

	signingKey := deriveSigningKey(c.secretKey, dateStamp, c.region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
	return req, nil
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func encodeS3Path(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = encodeS3Segment(p)
	}
	return strings.Join(parts, "/")
}

// encodeS3Segment 按 RFC 3986 逐字节转义(AWS SigV4 要求空格编为 %20 而非 '+'，保留 - _ . ~)。
func encodeS3Segment(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
