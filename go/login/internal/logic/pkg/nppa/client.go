// Package nppa 是国家新闻出版署「网络游戏防沉迷实名认证系统」的客户端:实名认证、认证结果查询、上下线上报
// (《接口对接技术规范》V1.9;docs/design/real-name-anti-addiction.md §2)。
//
// 报文体用 secretKey 做 AES-128-GCM 后 Base64,请求头按规范签名。姓名和身份证号只出现在加密后的报文体里:
// 本包的错误文案只带系统回的错误码与错误描述,不带请求内容。
package nppa

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"shared/antiaddiction"
)

// 官方地址(技术规范 V1.9)。查询与上报是 http:报文体已经加密,照规范写。接口测试阶段由配置换成企业端给的测试地址。
const (
	DefaultCheckURL    = "https://api.wlc.nppa.gov.cn/idcard/authentication/check"
	DefaultQueryURL    = "http://api2.wlc.nppa.gov.cn/idcard/authentication/query"
	DefaultLoginoutURL = "http://api2.wlc.nppa.gov.cn/behavior/collection/loginout"
)

// RequestTimeout 是规范建议的客户端超时;报文时间戳与系统时钟相差超过 5 秒也会被拒(1007)。
const RequestTimeout = 5 * time.Second

// MaxBatch 是一次上报的条目上限。
const MaxBatch = 128

// maxResponseBytes 与 login 其它渠道 provider 同一个上限。
const maxResponseBytes = 64 << 10

var (
	// ErrInvalidIdentity:系统判定姓名或身份证号格式不合法(2001、2005),玩家改了再提交。
	ErrInvalidIdentity = errors.New("nppa: name or id number rejected")
	// ErrNoRecord:查询的 ai 没有认证记录(2003),多半是已过 48 小时或结果已被取走;玩家重新提交。
	ErrNoRecord = errors.New("nppa: no authentication record for ai")
	// ErrUnavailable:网络、非 2xx、应答不可解析、系统类错误码(签名、IP、限流、权限等)。服务端故障,玩家稍后重试。
	ErrUnavailable = errors.New("nppa: service unavailable")
)

// Config 是一家运营单位在国家系统里的接入凭据与地址。
type Config struct {
	AppID       string
	BizID       string // 游戏备案识别码
	SecretKey   string // 32 位十六进制;签名用原串,加密用解码后的 16 字节
	CheckURL    string
	QueryURL    string
	LoginoutURL string
}

// Client 可并发使用。
type Client struct {
	cfg  Config
	gcm  cipher.AEAD
	http *http.Client
	now  func() time.Time
}

// NewClient 校验凭据并建好加密器。httpClient 为 nil 时用带 RequestTimeout 的默认客户端。
func NewClient(cfg Config, httpClient *http.Client) (*Client, error) {
	if cfg.AppID == "" || cfg.BizID == "" {
		return nil, errors.New("nppa: AppId and BizId are required")
	}
	gcm, err := newGCM(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	if cfg.CheckURL == "" {
		cfg.CheckURL = DefaultCheckURL
	}
	if cfg.QueryURL == "" {
		cfg.QueryURL = DefaultQueryURL
	}
	if cfg.LoginoutURL == "" {
		cfg.LoginoutURL = DefaultLoginoutURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: RequestTimeout}
	}
	return &Client{cfg: cfg, gcm: gcm, http: httpClient, now: time.Now}, nil
}

// AuthStatus 是实名认证结果。
type AuthStatus int

const (
	AuthOK      AuthStatus = 0
	AuthPending AuthStatus = 1 // 系统没能立即给出结果,48 小时内用同一个 ai 查询
	AuthFailed  AuthStatus = 2 // 姓名与身份证号不匹配
)

// AuthResult 是认证或查询的结果;Status 为 AuthOK 时 PI 一定是合法的 38 位标识。
type AuthResult struct {
	Status AuthStatus
	PI     string
}

// Check 提交一次实名认证。ai 每次提交都要新生成:没被删除的 ai 不能再提交。
func (c *Client) Check(ctx context.Context, ai, name, idNumber string) (AuthResult, error) {
	plain := struct {
		AI    string `json:"ai"`
		Name  string `json:"name"`
		IDNum string `json:"idNum"`
	}{ai, name, idNumber}
	resp, err := c.post(ctx, c.cfg.CheckURL, plain)
	if err != nil {
		return AuthResult{}, err
	}
	return authResult(resp)
}

// Query 查询认证中的结果。
func (c *Client) Query(ctx context.Context, ai string) (AuthResult, error) {
	resp, err := c.get(ctx, c.cfg.QueryURL, map[string]string{"ai": ai})
	if err != nil {
		return AuthResult{}, err
	}
	return authResult(resp)
}

// Behavior 是一条上下线记录。No 由 Loginout 按批内顺序填。
type Behavior struct {
	No int    `json:"no"`
	SI string `json:"si"` // 会话标识,一次上线配一次下线
	BT int    `json:"bt"` // 0 下线、1 上线
	OT int64  `json:"ot"` // 发生时刻,Unix 秒
	CT int    `json:"ct"` // 0 已认证用户(本系统没有游客模式,恒为 0)
	PI string `json:"pi"`
}

// 上下线取值。
const (
	BehaviorLogout = 0
	BehaviorLogin  = 1
)

// Loginout 上报一批(1..MaxBatch 条)。整批失败返回错误,调用方可以重试;系统只拒收其中几条(3001)时
// 返回被拒条目的序号,这些条目重发也不会被接受。
func (c *Client) Loginout(ctx context.Context, batch []Behavior) (rejected []int, err error) {
	if len(batch) == 0 || len(batch) > MaxBatch {
		return nil, fmt.Errorf("nppa: batch size %d out of range 1..%d", len(batch), MaxBatch)
	}
	numbered := make([]Behavior, len(batch))
	for i, b := range batch {
		b.No = i + 1
		numbered[i] = b
	}
	resp, err := c.post(ctx, c.cfg.LoginoutURL, struct {
		Collections []Behavior `json:"collections"`
	}{numbered})
	if err != nil {
		return nil, err
	}
	switch resp.ErrCode {
	case 0:
		return nil, nil
	case 3001: // 部分上报失败:逐条看结果
		var data struct {
			Results []struct {
				No      int `json:"no"`
				ErrCode int `json:"errcode"`
			} `json:"results"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, fmt.Errorf("%w: partial failure without parsable results", ErrUnavailable)
		}
		for _, r := range data.Results {
			if r.ErrCode != 0 {
				rejected = append(rejected, r.No)
			}
		}
		return rejected, nil
	default:
		return nil, resp.err()
	}
}

type response struct {
	ErrCode int             `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
	Data    json.RawMessage `json:"data"`
}

func (r *response) err() error {
	return fmt.Errorf("%w: errcode=%d errmsg=%q", ErrUnavailable, r.ErrCode, r.ErrMsg)
}

func authResult(resp *response) (AuthResult, error) {
	switch resp.ErrCode {
	case 0:
	case 2001, 2005:
		return AuthResult{}, fmt.Errorf("%w: errcode=%d", ErrInvalidIdentity, resp.ErrCode)
	case 2003:
		return AuthResult{}, ErrNoRecord
	default:
		return AuthResult{}, resp.err()
	}
	var data struct {
		Result struct {
			Status int    `json:"status"`
			PI     string `json:"pi"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return AuthResult{}, fmt.Errorf("%w: unparsable result", ErrUnavailable)
	}
	switch AuthStatus(data.Result.Status) {
	case AuthOK:
		if !antiaddiction.ValidPI(data.Result.PI) {
			return AuthResult{}, fmt.Errorf("%w: malformed pi in a successful result", ErrUnavailable)
		}
		return AuthResult{Status: AuthOK, PI: data.Result.PI}, nil
	case AuthPending:
		return AuthResult{Status: AuthPending}, nil
	case AuthFailed:
		return AuthResult{Status: AuthFailed}, nil
	default:
		return AuthResult{}, fmt.Errorf("%w: unknown status %d", ErrUnavailable, data.Result.Status)
	}
}

func (c *Client) post(ctx context.Context, target string, plain any) (*response, error) {
	raw, err := json.Marshal(plain)
	if err != nil {
		return nil, fmt.Errorf("nppa: marshal request: %w", err)
	}
	data, err := c.encrypt(raw)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"data": data})
	if err != nil {
		return nil, fmt.Errorf("nppa: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	return c.do(req, nil, string(body))
}

func (c *Client) get(ctx context.Context, target string, query map[string]string) (*response, error) {
	values := url.Values{}
	for k, v := range query {
		values.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+values.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	return c.do(req, query, "")
}

// do 补上系统参数与签名后发出请求。请求头的键名照规范大小写写入,不经 Go 的规范化。
func (c *Client) do(req *http.Request, query map[string]string, body string) (*response, error) {
	timestamps := strconv.FormatInt(c.now().UnixMilli(), 10)
	params := map[string]string{"appId": c.cfg.AppID, "bizId": c.cfg.BizID, "timestamps": timestamps}
	for k, v := range query {
		params[k] = v
	}
	req.Header["appId"] = []string{c.cfg.AppID}
	req.Header["bizId"] = []string{c.cfg.BizID}
	req.Header["timestamps"] = []string{timestamps}
	req.Header["sign"] = []string{sign(c.cfg.SecretKey, params, body)}

	httpResp, err := c.http.Do(req)
	if err != nil {
		// 错误里不带 URL 之外的东西;URL 只有 ai 这一个参数,不含个人信息。
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: http status %d", ErrUnavailable, httpResp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: unreadable or oversized response", ErrUnavailable)
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: unparsable response", ErrUnavailable)
	}
	return &resp, nil
}

// newGCM 用 secretKey(32 位十六进制)建报文体的 AES-128-GCM。
func newGCM(secretKey string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(secretKey)
	if err != nil || len(key) != 16 {
		return nil, errors.New("nppa: secret key must be 32 hex characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("nppa: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("nppa: %w", err)
	}
	return gcm, nil
}

// encrypt 返回 Base64(nonce || 密文 || 认证标签),nonce 12 字节随机。
func (c *Client) encrypt(plain []byte) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nppa: nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(c.gcm.Seal(nonce, nonce, plain, nil)), nil
}

// sign 按规范第五节:除 sign 之外的系统参数与 URL 参数按键名字典序拼成「键值」串,接上请求体原文,
// 最前面加 secretKey,取 SHA-256 十六进制。
func sign(secretKey string, params map[string]string, body string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(secretKey)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params[k])
	}
	b.WriteString(body)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
