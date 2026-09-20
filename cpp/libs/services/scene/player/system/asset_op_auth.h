#pragma once

// 通用资产通道的请求验签(docs/design/guild-phase2/04-asset-channel.md §4.32,不变量 I9)。
//
// 一句话:Go 服务(guild / trade)用**自己独有的密钥**对请求的关键字段做 HMAC-SHA256,
// 签名写进 AssetOpRequest.auth;scene 在 loop 线程上用同一份规则重算并常数时间比对,
// 不一致一律回 UNKNOWN + kAssetAuthFailed、**不记账**(§4.9 第 1b 步)。
//
// 为什么签名放在**请求体**里而不是 gRPC metadata:gRPC 包装函数由生成器产出,context 参数
// 被注释掉(grpc_handler_gen.go:125 `grpc::ServerContext* /*context*/`),守护段里根本读不到
// metadata;要读就得改生成器并重生成全部 gRPC 包装。而 scene 的 gRPC 端口用的是明文不安全
// 凭据(node.cpp:623 InsecureServerCredentials),集群内任何进程都能连、都能发 AssetCredit
// 凭空发币。签名是这条通道的**主防线**,NetworkPolicy 只是纵深防御(仓库内暂无该清单)。
//
// 为什么不要 nonce:同一 (player, stream, epoch, seq) 重放要么只读答复、要么就是那唯一一次
// 应用(不变量 I2 结局固定),Go 总以 scene 的结局为准。时间窗只用来限制截获包的寿命
// (应对回档后旧 seq 重新变成"未见")。
//
// **跨语言契约**:canonical 串必须与 Go go/shared/assetop/auth.go 的 Canonical **逐字节一致**。
// 两边各有一条同输入的 golden 字面量(本地 cpp/tests/currency_test/asset_op_auth_test.cpp,
// 对侧 go/shared/assetop/auth_test.go),任一侧改了拼串规则,golden 会当场把它抓出来 ——
// 否则线上表现是"全部资产 RPC 验签失败",帮会捐献与聚宝斋寄售整体卡死。
// 改 canonical 必须两边同批改、两边 golden 同批更新。
//
// 线程模型:三个资产 RPC 入口都已 runInLoop 投递,本文件的函数只在 scene 的 **loop 线程**上
// 被调用;默认密钥来源的缓存因此不加锁。纯函数层(AssetOpCanonical)无任何全局状态。
//
// 依赖:标准库 + engine/core/security/token_security.h(header-only,内部 include OpenSSL)。
// 不碰 ECS、不碰配表、不打业务日志 —— 除了"密钥未配置"那一条启动期 ERROR。
//
// 单测:cpp/tests/currency_test/asset_op_auth_test.cpp。

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "proto/common/asset/asset_op.pb.h"

// canonical 串的第一行,同时是协议版本号。它是跨语言契约的一部分:
// Go 侧 assetop.CanonicalVersion 用同一字面量,改它等于改协议。
constexpr std::string_view kAssetOpCanonicalVersion = "mmorpg-asset-op/v1";

// scene 接受的签名时间窗:|nowMs − auth.timestamp_ms| <= 300000(5 分钟)。
// 与 Go 侧 assetop.MaxClockSkewMs 必须同值;真正的判定在这里。
constexpr int64_t kAssetOpAuthMaxSkewMs = 300000;

// 调用方密钥的最小字节数(去首尾空白之后)。不足这么长视同**未配置**,一律拒绝。
// 与 Go 侧 assetop.MinSecretLen 同值:Go 在构造 Signer 时就拒,scene 在验签时再拒一次,
// 两边都 fail-closed。资产路径**不做** token_security::ClassifyTokenSecret 那种 dev 放行,
// 本地开发值由启动脚本显式注入(§4.32)。
constexpr size_t kAssetOpAuthMinSecretBytes = 32;

// 验签判决。最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2:不用宏生成声明)。
// 除 kOk 外的每一种都让调用方回 UNKNOWN + kAssetAuthFailed(27008),**不区别对待**:
// 把"密钥没配"和"签名不对"在响应里分开,等于免费告诉攻击者他猜对了哪一半。
// 区分只出现在 scene 自己的 WARN 日志里(verdict 名),给运维定位用。
enum class AssetOpAuthVerdict : uint8_t
{
	kOk,                // 通过
	kCallerNotAllowed,  // caller 为空 / 不认识 / 与该条流的白名单不符(含 SYSTEM_CREDIT,v1 一律拒)
	kSecretMissing,     // 该调用方的密钥未配置,或去空白后不足 kAssetOpAuthMinSecretBytes
	kClockSkew,         // |now − timestamp_ms| > kAssetOpAuthMaxSkewMs
	kSignatureMismatch, // canonical 重算出的 HMAC 与 auth.signature_hex 不符
	kCount
};

// 判决名,只进日志,不进响应。越界返回 "unknown"(它被写在 LOG_WARN 里,不该有崩的机会)。
const char* AssetOpAuthVerdictName(AssetOpAuthVerdict verdict);

// 按调用方名取密钥。返回空串 = 未配置(调用方按 kSecretMissing 拒)。
//
// 传 nullptr 给 VerifyAssetOpAuth 表示走**默认实现**:读环境变量
// MMORPG_ASSET_OP_SECRET_GUILD / MMORPG_ASSET_OP_SECRET_TRADE 并缓存。
// 单测经 PlayerAssetOpSystem::SetSecretLookupForTest 换成夹具密钥,于是不依赖进程环境
// (AGENTS §11.4:测试必须确定、可重复)。
//
// **密钥值绝不进仓库、日志、指标或错误文本**(AGENTS §11.3);本文件只记录变量**名**。
using AssetOpSecretLookup = std::string (*)(std::string_view caller);

// 拼出待签名串:LF 分隔、末尾无换行、全部十进制。
//
//   mmorpg-asset-op/v1
//   <caller>            // 取自 request.auth().caller()
//   <rpc>               // "debit" | "abort_debit" | "credit"
//   <player_id>
//   <stream 数值>
//   <stream_epoch>
//   <seq>
//   <correlation_id>
//   <tx_type>
//   <bundle>            // "c=" 货币 "<type>:<amount>" 逗号分隔 ";i=" 物品 "<config_id>:<count>";空则 "c=;i="
//   <timestamp_ms>      // 取自 request.auth().timestamp_ms()
//
// rpc 进串,防止拿 AbortDebit 的签名去调 Credit;bundle 进串,防止中途改金额。
// 货币与物品**按请求顺序**拼,不排序:scene 按同一份请求重算,排序只会给两侧各留一个出错的机会。
//
// **注意**:AssetBundle.item_uuids / pet_id(聚宝斋 P2 的按 guid 扣物/扣宝宝)**不在**本串里。
// 它们因此不受签名保护,TRADE_* 流启用前必须先把本函数与 Go Canonical 同批扩成
// `…;i=…;u=…;p=…` 并更新两边 golden(asset_op.proto 的 AssetBundle 注释逐字写明了这条硬前置)。
// 在那之前,asset_op_system.cpp 的 HasUnsupportedP2Fields 把带这两个字段的请求**忽略**掉。
//
// 纯函数:不读时钟、不读环境、不打日志,可脱离 ECS 单测。
std::string AssetOpCanonical(std::string_view rpc, const ::AssetOpRequest& request);

// 校验一次资产 RPC 的签名。顺序固定,先白名单后密钥(理由见实现处的注释)。
//   rpc     "debit" / "abort_debit" / "credit",与进 canonical 串的那一份是同一个值;
//   nowMs   scene 的当前墙钟毫秒(由调用方注入,单测可冻结);
//   lookup  密钥来源,nullptr = 默认读环境变量并缓存。
// 返回非 kOk 时调用方一律回 UNKNOWN + kAssetAuthFailed 且**不记账**(§4.9 第 1b 步)。
AssetOpAuthVerdict VerifyAssetOpAuth(std::string_view rpc, const ::AssetOpRequest& request, int64_t nowMs,
									 AssetOpSecretLookup lookup);
