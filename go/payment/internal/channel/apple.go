package channel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	paymentpb "proto/payment"
)

// App Store 内购(StoreKit 2,消耗型)。规则核对自 Apple 官方 App Store Server Library(Python 版)源码:
//   - 交易与通知都是 JWS:alg 只认 ES256,x5c 恰好 3 张证书(叶、中间、根),链要验到运维配置的 Apple 根证书;
//     叶证书须带扩展 1.2.840.113635.100.6.11.1,中间证书须带 1.2.840.113635.100.6.2.1;验证时刻取载荷的 signedDate。
//   - 服务端 API:正式 https://api.storekit.apple.com,沙箱 https://api.storekit-sandbox.apple.com;
//     鉴权 JWT 是 ES256(.p8 私钥),载荷 iss / iat / exp / aud=appstoreconnect-v1 / bid,头带 kid。
//   - 查交易 GET /inApps/v1/transactions/{transactionId},应答 signedTransactionInfo;找不到是 404 + errorCode 4040010。
//   - 通知 V2 的正文是 {"signedPayload": JWS};通知历史 POST /inApps/v1/notifications/history。
//
// 订单绑定:appAccountToken 就是商城订单号(UUID),由服务端在调起时交给客户端。客户端只上报交易号(gate 限 1KB),
// 服务端向 App Store 取权威交易再核对。App Store 没有开发者退款接口:要退的钱一律转人工(引导用户向苹果申请)。

const (
	appleProductionAPI = "https://api.storekit.apple.com"
	appleSandboxAPI    = "https://api.storekit-sandbox.apple.com"
	appleLeafOID       = "1.2.840.113635.100.6.11.1"
	appleIntermediate  = "1.2.840.113635.100.6.2.1"
	appleNotFoundCode  = 4040010
)

type AppleConfig struct {
	BundleID         string
	AppAppleID       int64 // App Store 上的应用数字 ID;正式环境的通知要核对它
	IssuerID         string
	KeyID            string
	PrivateKey       *ecdsa.PrivateKey   // App Store Connect 生成的 .p8
	RootCertificates []*x509.Certificate // Apple Root CA - G3 等,运维从 Apple PKI 下载后配置
	AllowSandbox     bool                // 接受沙箱交易(审核员用沙箱账号付款);是否发货由 core 的测试交易规则决定
	ProductionAPI    string              // 缺省正式地址;只有用例改
	SandboxAPI       string
	HTTP             *http.Client
	Now              func() time.Time
}

type Apple struct{ cfg AppleConfig }

func NewApple(cfg AppleConfig) (*Apple, error) {
	switch {
	case cfg.BundleID == "" || cfg.IssuerID == "" || cfg.KeyID == "":
		return nil, errors.New("payment channel: apple needs BundleID, IssuerID and KeyID")
	case cfg.PrivateKey == nil:
		return nil, errors.New("payment channel: apple needs the App Store Connect API private key (.p8)")
	case len(cfg.RootCertificates) == 0:
		return nil, errors.New("payment channel: apple needs the Apple root certificates")
	case cfg.AppAppleID <= 0:
		return nil, errors.New("payment channel: apple needs AppAppleID (the numeric App Store id)")
	}
	if cfg.ProductionAPI == "" {
		cfg.ProductionAPI = appleProductionAPI
	}
	if cfg.SandboxAPI == "" {
		cfg.SandboxAPI = appleSandboxAPI
	}
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Apple{cfg: cfg}, nil
}

func (a *Apple) ID() paymentpb.PayChannel { return paymentpb.PayChannel_PAY_CHANNEL_APPLE }

func (a *Apple) Name() string { return NameApple }

// Supports:价格由 App Store 按店面定,订单币种不限;报给商城的金额取订单冻结值。
func (a *Apple) Supports(currency string) bool { return currency != "" }

func (a *Apple) NeedsProduct() bool { return true }

func (a *Apple) Prepare(_ context.Context, order Order, _ Ref) (Prepared, error) {
	if order.ProductID == "" {
		return Prepared{}, errors.New("payment channel: apple needs a product id for the order")
	}
	return Prepared{Params: ClientParams{Apple: &paymentpb.AppleIapParams{ProductId: order.ProductID, AppAccountToken: order.OrderID}}}, nil
}

func (a *Apple) Query(context.Context, QueryRequest) (QueryResult, error) {
	return QueryResult{}, ErrQueryUnsupported
}

func (a *Apple) Close(context.Context, string) error { return nil }

func (a *Apple) Refund(context.Context, RefundRequest) (RefundResult, error) {
	return refundUnsupported, nil
}

func (a *Apple) AccountRef(uint64) string { return "" }

// appleTransaction 是 JWSTransactionDecodedPayload 里本服务用到的字段(字段名照官方库)。
type appleTransaction struct {
	TransactionID      string `json:"transactionId"`
	BundleID           string `json:"bundleId"`
	ProductID          string `json:"productId"`
	PurchaseDate       int64  `json:"purchaseDate"`
	Quantity           int64  `json:"quantity"`
	Type               string `json:"type"`
	AppAccountToken    string `json:"appAccountToken"`
	Environment        string `json:"environment"`
	Storefront         string `json:"storefront"`
	Price              int64  `json:"price"` // 价格 × 1000(milliunits)
	Currency           string `json:"currency"`
	RevocationDate     int64  `json:"revocationDate"`
	RevocationReason   *int   `json:"revocationReason"`
	SignedDate         int64  `json:"signedDate"`
	InAppOwnershipType string `json:"inAppOwnershipType"`
}

// verifyJWS 验 App Store 的签名数据并解出载荷。chain:x5c 恰好 3 张;中间证书由配置的根签发、叶由中间证书签发;
// 两张都在载荷 signedDate 时刻有效;扩展 OID 齐全;最后用叶公钥验 ES256。
func (a *Apple) verifyJWS(token string, payload any) error {
	parts, err := splitJWS(token)
	if err != nil {
		return err
	}
	var header struct {
		Alg string   `json:"alg"`
		X5c []string `json:"x5c"`
	}
	if err := json.Unmarshal(parts.header, &header); err != nil || header.Alg != "ES256" {
		return errors.New("payment channel: apple JWS algorithm is not ES256")
	}
	if len(header.X5c) != 3 {
		return errors.New("payment channel: apple JWS certificate chain must have exactly 3 certificates")
	}
	certs := make([]*x509.Certificate, 3)
	for i, encoded := range header.X5c {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return errors.New("payment channel: apple JWS certificate is not base64")
		}
		if certs[i], err = x509.ParseCertificate(der); err != nil {
			return errors.New("payment channel: apple JWS certificate cannot be parsed")
		}
	}
	var dated struct {
		SignedDate int64 `json:"signedDate"`
	}
	if err := json.Unmarshal(parts.payload, &dated); err != nil || dated.SignedDate <= 0 {
		return errors.New("payment channel: apple JWS payload has no signedDate")
	}
	at := time.UnixMilli(dated.SignedDate)
	leaf, intermediate := certs[0], certs[1]
	if !intermediate.BasicConstraintsValid || !intermediate.IsCA {
		return errors.New("payment channel: apple intermediate certificate is not a CA")
	}
	trusted := false
	for _, root := range a.cfg.RootCertificates {
		if intermediate.CheckSignatureFrom(root) == nil && validAt(root, at) {
			trusted = true
			break
		}
	}
	if !trusted {
		return errors.New("payment channel: apple certificate chain does not end at a configured Apple root")
	}
	if leaf.CheckSignatureFrom(intermediate) != nil || !validAt(leaf, at) || !validAt(intermediate, at) {
		return errors.New("payment channel: apple leaf certificate is not valid under the intermediate")
	}
	if !hasExtension(leaf, appleLeafOID) || !hasExtension(intermediate, appleIntermediate) {
		return errors.New("payment channel: apple certificates lack the App Store extensions")
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("payment channel: apple leaf key is not ECDSA")
	}
	if err := verifyES256(key, parts.signingInput, parts.signature); err != nil {
		return err
	}
	return json.Unmarshal(parts.payload, payload)
}

func validAt(cert *x509.Certificate, at time.Time) bool {
	return !at.Before(cert.NotBefore) && !at.After(cert.NotAfter)
}

// hasExtension 报告证书带不带这个扩展(解析出的 Extensions 含全部扩展,认不出的也在)。只看有没有,不看内容,与官方库一致。
func hasExtension(cert *x509.Certificate, oid string) bool {
	for _, ext := range cert.Extensions {
		if ext.Id.String() == oid {
			return true
		}
	}
	return false
}

// apiToken 是 App Store Server API 的鉴权 JWT:每次请求现签,有效 5 分钟(与官方库相同)。
func (a *Apple) apiToken() (string, error) {
	now := a.cfg.Now().Unix()
	return jwtSign(map[string]string{"alg": "ES256", "kid": a.cfg.KeyID, "typ": "JWT"},
		map[string]any{"iss": a.cfg.IssuerID, "iat": now, "exp": now + 300, "aud": "appstoreconnect-v1", "bid": a.cfg.BundleID},
		signES256(a.cfg.PrivateKey))
}

func (a *Apple) api(ctx context.Context, base, method, path string, body []byte) (int, []byte, error) {
	token, err := a.apiToken()
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.cfg.HTTP.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := readBody(response)
	return response.StatusCode, raw, err
}

func appleErrorCode(raw []byte) int64 {
	var out struct {
		ErrorCode int64 `json:"errorCode"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.ErrorCode
}

// transaction 按交易号取权威交易:先查正式环境,404 且允许沙箱时再查沙箱(审核员的沙箱交易落在沙箱)。
func (a *Apple) transaction(ctx context.Context, transactionID string) (*appleTransaction, error) {
	if transactionID == "" || len(transactionID) > 64 || !allDigits(transactionID) {
		return nil, errors.New("payment channel: apple transaction id is not numeric")
	}
	path := "/inApps/v1/transactions/" + url.PathEscape(transactionID)
	bases := []string{a.cfg.ProductionAPI}
	if a.cfg.AllowSandbox {
		bases = append(bases, a.cfg.SandboxAPI)
	}
	for _, base := range bases {
		status, raw, err := a.api(ctx, base, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound && appleErrorCode(raw) == appleNotFoundCode {
			continue
		}
		if status != http.StatusOK {
			return nil, &CallError{Channel: NameApple, Op: "transaction", Status: status, Code: strconv.FormatInt(appleErrorCode(raw), 10)}
		}
		var out struct {
			SignedTransactionInfo string `json:"signedTransactionInfo"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.SignedTransactionInfo == "" {
			return nil, errors.New("payment channel: apple transaction answer has no signedTransactionInfo")
		}
		var txn appleTransaction
		if err := a.verifyJWS(out.SignedTransactionInfo, &txn); err != nil {
			return nil, err
		}
		return &txn, nil
	}
	return nil, nil // 两个环境都找不到
}

// classify 把一笔已验签的交易变成核实结论。
func (a *Apple) classify(txn *appleTransaction) (QueryResult, error) {
	if txn.BundleID != a.cfg.BundleID {
		return QueryResult{}, errors.New("payment channel: apple transaction belongs to another app")
	}
	switch txn.Environment {
	case "Production":
	case "Sandbox":
		if !a.cfg.AllowSandbox {
			return QueryResult{State: TradeClosed}, nil
		}
	default:
		return QueryResult{}, errors.New("payment channel: apple transaction environment is not accepted")
	}
	payment := &Payment{TransactionID: txn.TransactionID, OrderID: strings.ToLower(txn.AppAccountToken), Currency: "", PaidAt: time.UnixMilli(txn.PurchaseDate),
		ProductID: txn.ProductID, StorePriced: true, StoreCurrency: txn.Currency, Test: txn.Environment == "Sandbox"}
	if txn.Price > 0 {
		payment.StoreAmountMinor = uint64(txn.Price) / 10 // milliunits → 最小单位(两位小数的币种)
	}
	switch {
	case txn.Type != "Consumable":
		payment.Anomaly = "apple product type " + txn.Type + " is not a consumable"
	case txn.Quantity != 1:
		payment.Anomaly = "apple transaction quantity is " + strconv.FormatInt(txn.Quantity, 10)
	case txn.AppAccountToken == "":
		payment.Anomaly = "apple transaction carries no appAccountToken"
	}
	if txn.RevocationDate > 0 {
		return QueryResult{State: TradeRefundedOutside, Payment: payment}, nil
	}
	return QueryResult{State: TradePaid, Payment: payment}, nil
}

// Verify 按客户端上报的交易号向 App Store 核实。两个环境都查不到:交易号是假的或不是本应用的,不记账。
func (a *Apple) Verify(ctx context.Context, receipt Receipt) (QueryResult, error) {
	txn, err := a.transaction(ctx, receipt.TransactionID)
	if err != nil {
		return QueryResult{}, err
	}
	if txn == nil {
		return QueryResult{State: TradeClosed}, nil
	}
	return a.classify(txn)
}

// notificationPayload 是 ResponseBodyV2DecodedPayload 里本服务用到的字段。
type appleNotification struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	SignedDate       int64  `json:"signedDate"`
	Data             *struct {
		AppAppleID            int64  `json:"appAppleId"`
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	} `json:"data"`
}

// decodeNotification 验签并把一条通知变成事件。只处理消耗型相关的几种:
// ONE_TIME_CHARGE(付款)、REFUND(撤销)、REFUND_REVERSED(撤销被撤回);其余(含 TEST、CONSUMPTION_REQUEST)验过签照常应答。
func (a *Apple) decodeNotification(signedPayload string) (Notification, error) {
	var note appleNotification
	if err := a.verifyJWS(signedPayload, &note); err != nil || note.Data == nil {
		return Notification{}, ErrBadSignature
	}
	if note.Data.BundleID != a.cfg.BundleID {
		return Notification{}, ErrBadSignature
	}
	switch note.Data.Environment {
	case "Production":
		if note.Data.AppAppleID != a.cfg.AppAppleID {
			return Notification{}, ErrBadSignature
		}
	case "Sandbox":
		if !a.cfg.AllowSandbox {
			return Notification{}, nil
		}
	default:
		return Notification{}, nil
	}
	switch note.NotificationType {
	case "ONE_TIME_CHARGE", "REFUND", "REFUND_REVERSED":
	default:
		return Notification{}, nil
	}
	var txn appleTransaction
	if note.Data.SignedTransactionInfo == "" || a.verifyJWS(note.Data.SignedTransactionInfo, &txn) != nil {
		return Notification{}, ErrBadSignature
	}
	result, err := a.classify(&txn)
	if err != nil || result.Payment == nil {
		return Notification{}, nil // 别的应用或不接受的环境:验过签,不处理
	}
	switch note.NotificationType {
	case "ONE_TIME_CHARGE":
		if result.State != TradePaid {
			return Notification{}, nil
		}
		return Notification{Payment: result.Payment}, nil
	case "REFUND":
		return Notification{Revocations: []Revocation{{TransactionID: txn.TransactionID, Revoked: true,
			ObservedAt: time.UnixMilli(note.SignedDate), Detail: "REFUND", Payment: result.Payment}}}, nil
	default: // REFUND_REVERSED
		return Notification{Revocations: []Revocation{{TransactionID: txn.TransactionID, Revoked: false,
			ObservedAt: time.UnixMilli(note.SignedDate), Detail: "REFUND_REVERSED", Payment: result.Payment}}}, nil
	}
}

func (a *Apple) ParseNotification(_ *http.Request, body []byte) (Notification, error) {
	var envelope struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.SignedPayload == "" {
		return Notification{}, ErrBadSignature
	}
	return a.decodeNotification(envelope.SignedPayload)
}

// PollHistory 拉正式环境的通知历史补漏(回调漏掉的付款与撤销)。时间窗 [since, now-1 分钟),翻页拉完为止。
func (a *Apple) PollHistory(ctx context.Context, since time.Time) ([]Notification, time.Time, error) {
	until := a.cfg.Now().Add(-time.Minute)
	if !until.After(since) {
		return nil, since, nil
	}
	request := map[string]any{"startDate": since.UnixMilli(), "endDate": until.UnixMilli()}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, since, err
	}
	var out []Notification
	pagination := ""
	for page := 0; page < 50; page++ {
		path := "/inApps/v1/notifications/history"
		if pagination != "" {
			path += "?paginationToken=" + url.QueryEscape(pagination)
		}
		status, raw, err := a.api(ctx, a.cfg.ProductionAPI, http.MethodPost, path, body)
		if err != nil {
			return out, since, err
		}
		if status != http.StatusOK {
			return out, since, &CallError{Channel: NameApple, Op: "notification_history", Status: status, Code: strconv.FormatInt(appleErrorCode(raw), 10)}
		}
		var history struct {
			NotificationHistory []struct {
				SignedPayload string `json:"signedPayload"`
			} `json:"notificationHistory"`
			HasMore         bool   `json:"hasMore"`
			PaginationToken string `json:"paginationToken"`
		}
		if err := json.Unmarshal(raw, &history); err != nil {
			return out, since, errors.New("payment channel: apple notification history answer is not readable")
		}
		for _, item := range history.NotificationHistory {
			note, err := a.decodeNotification(item.SignedPayload)
			if err != nil {
				return out, since, fmt.Errorf("payment channel: apple notification history item rejected: %w", err)
			}
			if note.Payment != nil || len(note.Revocations) > 0 {
				out = append(out, note)
			}
		}
		if !history.HasMore || history.PaginationToken == "" {
			return out, until, nil
		}
		pagination = history.PaginationToken
	}
	return out, since, errors.New("payment channel: apple notification history has too many pages; will retry")
}

// WriteAck:App Store 收到 2xx 才算送达,其余按退避重试。
func (a *Apple) WriteAck(w http.ResponseWriter, kind AckKind) {
	if kind == AckOK {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
}
