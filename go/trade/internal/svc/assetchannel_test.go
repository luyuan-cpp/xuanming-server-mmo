package svc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"shared/assetop"
	"shared/idsegment"
)

// 资产通道装配的纯单测:不连 Redis / MySQL / etcd。
// 这里守的是"密钥怎么才算数""缺东西时降级还是拒绝""密钥会不会漏进日志"这三件事。

// 32 字节的测试密钥。刻意不是真实开发值:测试里出现的字符串会被 grep 到,
// 不该让人误以为某个具体值有特殊含义。
const testSecret32 = "trade-test-secret-0123456789abc0"

func TestNewAssetOpSignerRequiresSecret(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, "")
	signer, err := NewAssetOpSigner()
	if err == nil {
		t.Fatal("环境变量未设置时必须失败(资产路径不做 dev 放行)")
	}
	if signer != nil {
		t.Error("失败时不能返回签名器")
	}
	if !errors.Is(err, ErrAssetSecretMissing) {
		t.Errorf("err = %v, want errors.Is(..., ErrAssetSecretMissing)", err)
	}
}

func TestNewAssetOpSignerRejectsShortSecret(t *testing.T) {
	short := strings.Repeat("a", assetop.MinSecretLen-1)
	t.Setenv(AssetOpSecretEnv, short)
	_, err := NewAssetOpSigner()
	if err == nil {
		t.Fatal("短于 32 字节的密钥必须被拒绝")
	}
	if !errors.Is(err, ErrAssetSecretMissing) || !errors.Is(err, assetop.ErrWeakSecret) {
		t.Errorf("err = %v, want 同时满足 ErrAssetSecretMissing 与 assetop.ErrWeakSecret", err)
	}
	// 密钥值绝不进错误文本(AGENTS §9):错误会被原样打进启动日志。
	if strings.Contains(err.Error(), short) {
		t.Error("错误文本里出现了密钥值")
	}
}

// TestNewAssetOpSignerTrimsAndAccepts:去首尾空白后够 32 字节即可 —— K8s Secret 经 yaml
// 注入时很容易带上换行。caller 必须恰是 "trade":scene 按 (stream, caller) 双向白名单校验。
func TestNewAssetOpSignerTrimsAndAccepts(t *testing.T) {
	t.Setenv(AssetOpSecretEnv, "  "+testSecret32+"\n")
	signer, err := NewAssetOpSigner()
	if err != nil {
		t.Fatalf("NewAssetOpSigner: %v", err)
	}
	if got := signer.Caller(); got != AssetOpCaller {
		t.Errorf("caller = %q, want %q(TRADE_* 两条流只认这个名字)", got, AssetOpCaller)
	}
	if AssetOpCaller != "trade" {
		t.Errorf("AssetOpCaller = %q:改名会让 scene 侧白名单当场拒签", AssetOpCaller)
	}
}

// TestNilAssetChannelIsSafe:密钥缺失时 Assets.Pipeline 为 nil,装配整体也可能为 nil
// (未来若有不建通道的形态)。这些方法都会被启动 / 停机路径无条件调用,必须能空调用。
func TestNilAssetChannelIsSafe(t *testing.T) {
	var ch *AssetChannel
	if ch.Enabled() {
		t.Error("nil 通道不能报告为可用")
	}
	ch.Start(context.Background(), nil)
	ch.WarmAssetOpIDSegment()
	ch.Close()

	// Pipeline 为 nil(密钥缺失)的通道同样报告不可用。
	disabled := &AssetChannel{}
	if disabled.Enabled() {
		t.Error("没有管线的通道不能报告为可用")
	}
}

// TestAssetOpIDBizTagIsDistinct:op_id 与 listing_id 各走一个 biz_tag。
// 共用一个的后果是两种 id 交错取值,对账时分不清这个号是商品还是资产指令。
func TestAssetOpIDBizTagIsDistinct(t *testing.T) {
	if AssetOpIDBizTag == ListingIDBizTag {
		t.Fatalf("op_id 与 listing_id 共用了 biz_tag %q", AssetOpIDBizTag)
	}
	if AssetOpIDBizTag != "trade_asset_op" {
		t.Errorf("AssetOpIDBizTag = %q:改名必须同步 data_service 的 BootstrapTags 四处登记"+
			"(config.go / store/id_segment_store.go / etc/data_service.yaml / k8s_deploy.ps1)", AssetOpIDBizTag)
	}
}

func TestNewAssetOpIDSegmentRejectsUnusableConfig(t *testing.T) {
	if _, err := NewAssetOpIDSegment(idsegment.Conf{Enabled: false}, nil); err == nil {
		t.Error("IdSegment.Enabled=false 必须被拒绝:trade 没有 snowflake 回退")
	}
	if _, err := NewAssetOpIDSegment(idsegment.Conf{Enabled: true}, nil); err == nil {
		t.Error("data_service 客户端为 nil 必须被拒绝")
	}
}

// TestAssetChannelMetricsIsSingleton:assetop / scenenode 的指标用 promauto 注册,
// 重复构造会因指标重名 panic。装配层必须只建一次。
func TestAssetChannelMetricsIsSingleton(t *testing.T) {
	a1, s1 := assetChannelMetrics()
	a2, s2 := assetChannelMetrics()
	if a1 == nil || s1 == nil {
		t.Fatal("指标句柄不能为 nil")
	}
	if a1 != a2 || s1 != s2 {
		t.Error("重复调用必须返回同一份句柄(promauto 重复注册会 panic)")
	}
}
