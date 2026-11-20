// devpaynotify 给本机或测试环境的支付服务发一条 devpay 联调通知,冒烟用(docs/design/payment-recharge.md §22.9)。
// 正文与签名和 channel.DevPay 的解析逐字对应。密钥只从环境变量读(与支付服务 Payment.DevPay.SecretEnv 指的是同一个值),不进命令行。
//
//	付款:     go run ./cmd/devpaynotify -order <订单号> -amount <订单金额,分>
//	重复付款: 再加 -seq 1(同一张订单的另一笔渠道交易)
//	渠道撤销: 再加 -revoke(撤销 -seq 指的那一笔)
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"payment/internal/channel"
	"payment/internal/data"
)

func main() {
	order := flag.String("order", "", "mall order id (UUID with dashes)")
	amount := flag.Uint64("amount", 0, "paid amount in minor units; the mall binds the payment only when it equals the order amount")
	currency := flag.String("currency", "CNY", "paid currency")
	seq := flag.Uint("seq", 0, "0 = the first channel transaction of the order; a non-zero value reports another transaction of the same order")
	revoke := flag.Bool("revoke", false, "report that the channel revoked that transaction instead of reporting a payment")
	url := flag.String("url", "http://127.0.0.1:18130/notify/devpay", "notification URL of the payment service")
	secretEnv := flag.String("secret-env", "PAYMENT_DEVPAY_SECRET", "environment variable holding the devpay secret")
	flag.Parse()
	if err := send(*order, *amount, *currency, uint32(*seq), *revoke, *url, os.Getenv(*secretEnv)); err != nil {
		fmt.Fprintln(os.Stderr, "[devpaynotify]", err)
		os.Exit(1)
	}
}

func send(order string, amount uint64, currency string, seq uint32, revoke bool, url, secret string) error {
	outTradeNo, err := data.OutTradeNo(order)
	if err != nil {
		return fmt.Errorf("-order is not a mall order id: %w", err)
	}
	if secret == "" {
		return errors.New("the devpay secret environment variable is unset")
	}
	if !revoke && amount == 0 {
		return errors.New("-amount is required for a payment")
	}
	// 字段名同 channel 包的 devPayNotice:解析端拒绝未知字段。
	body, err := json.Marshal(struct {
		OutTradeNo  string `json:"out_trade_no"`
		AmountMinor uint64 `json:"amount_minor"`
		Currency    string `json:"currency"`
		Seq         uint32 `json:"seq"`
		Revoke      bool   `json:"revoke"`
	}{outTradeNo, amount, currency, seq, revoke})
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(channel.DevPaySignatureHeader, hex.EncodeToString(channel.DevPaySign([]byte(secret), body)))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the payment service answered HTTP %d: %s", response.StatusCode, bytes.TrimSpace(answer))
	}
	fmt.Printf("accepted: out_trade_no=%s seq=%d revoke=%t\n", outTradeNo, seq, revoke)
	return nil
}
