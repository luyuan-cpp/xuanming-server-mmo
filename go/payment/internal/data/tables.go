// Package data 是支付服务独占库 mmorpg_payment 的读写。表结构只来自 proto/payment/payment_table.proto。
package data

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"
	paymentpb "proto/payment"
)

// DatabaseName 是本服务独占的逻辑库(port-decisions D-14:每服务一库)。
const DatabaseName = "mmorpg_payment"

// Tables 是 schemamigrate 的表清单,启动核对与 -migrate 共用。
func Tables() []proto.Message {
	return []proto.Message{&paymentpb.PaymentIntentRecord{}, &paymentpb.PaymentTxnRecord{}}
}

// ErrInvalidKey 表示订单号、商户单号或渠道交易号不是约定的形状。
var ErrInvalidKey = errors.New("payment: identifier is not in the agreed format")

// 商城订单号是小写的 UUID 文本(java/mall_game 的 UUID.randomUUID().toString())。
var orderIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// 商户单号是订单号去掉连字符。微信要求 6~32 个字符,32 个十六进制字符正好用满。
var outTradeNoPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// 商城回执对渠道交易号的要求(CommerceStore.recordTrustedPayment),不符的交易号报不进商城。
var transactionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// OrderKey 把商城订单号拆成主键的两列。全零的键留给「对不上意图」的付款,所以全零的订单号按非法处理;
// 商城发的是随机 UUID,版本位非零,不会是全零。
func OrderKey(orderID string) (hi, lo uint64, err error) {
	if !orderIDPattern.MatchString(orderID) {
		return 0, 0, ErrInvalidKey
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(orderID, "-", ""))
	if err != nil || len(raw) != 16 {
		return 0, 0, ErrInvalidKey
	}
	hi, lo = binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:])
	if hi == 0 && lo == 0 {
		return 0, 0, ErrInvalidKey
	}
	return hi, lo, nil
}

// OrderID 是 OrderKey 的逆:由主键的两列还原订单号文本。
func OrderID(hi, lo uint64) string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], hi)
	binary.BigEndian.PutUint64(raw[8:], lo)
	text := hex.EncodeToString(raw[:])
	return text[0:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:32]
}

// OutTradeNo 是报给渠道的商户单号。它与订单号一一对应:一张订单只有一个支付意图,渠道保证同一个商户单号至多成功付款一次。
func OutTradeNo(orderID string) (string, error) {
	if _, _, err := OrderKey(orderID); err != nil {
		return "", err
	}
	return strings.ReplaceAll(orderID, "-", ""), nil
}

// OrderIDFromOutTradeNo 是 OutTradeNo 的逆。渠道回调里的商户单号不是本服务发出的形状时返回 ErrInvalidKey。
func OrderIDFromOutTradeNo(outTradeNo string) (string, error) {
	if !outTradeNoPattern.MatchString(outTradeNo) {
		return "", ErrInvalidKey
	}
	orderID := outTradeNo[0:8] + "-" + outTradeNo[8:12] + "-" + outTradeNo[12:16] + "-" + outTradeNo[16:20] + "-" + outTradeNo[20:32]
	if _, _, err := OrderKey(orderID); err != nil {
		return "", err
	}
	return orderID, nil
}

// ValidTransactionID 报告渠道交易号能不能原样报给商城。
func ValidTransactionID(transactionID string) bool {
	return transactionIDPattern.MatchString(transactionID)
}

// TxnKey 由渠道交易号派生付款事实行主键的后两列(主键第一列是渠道)。取 SHA-256 的前 16 字节:
// 行里另存交易号原文,读回时比对,撞键按存储损坏处理,不会把两笔钱并成一笔。
func TxnKey(transactionID string) (hi, lo uint64) {
	sum := sha256.Sum256([]byte(transactionID))
	return binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16])
}

// OutRefundNo 是报给渠道的退款单号,也是渠道退款的幂等键。由被退那笔付款的渠道名与交易号确定性派生:
// 一笔付款只有一个退款单号,重试、换副本、重启都不变;不同付款互不相同(商城契约:不能按订单号去重,
// 一张订单可能同时有本单付款和几笔异常付款要退)。32 个字符,微信(≤64)与支付宝(≤64)都放得下。
func OutRefundNo(channelName, transactionID string) string {
	sum := sha256.Sum256([]byte("refund|" + channelName + "|" + transactionID))
	return "rf" + hex.EncodeToString(sum[:15])
}
