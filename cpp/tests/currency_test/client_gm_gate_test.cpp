// 客户端 GM 指令统一闸门的用例(docs/design/guild-phase2/04-asset-channel.md §4.12、§4.34)。
//
// 闸门本身是两件事的组合:
//   ① **判据**:这个方法名是不是 GM 指令 —— scene_gm_guard::IsClientGmMethodName;
//   ② **策略**:GM 指令在当前运行模式下放不放行 —— gate_security::ClassifyGmClientMessage
//      (判据读 SCENE_RUN_MODE,默认 prod = 拒绝,见 cpp/nodes/gate/SECURITY.md §3)。
// 接线在 SceneHandler::ProcessClientPlayerMessage(客户端唯一入口)与各 Gm* handler
// (节点路由那条路)。这里只验这两个纯函数的契约 —— 它们是闸门唯一会出错的地方,
// 而且能在不起 ECS / 不连网络的情况下确定性地验。
//
// **最有价值的是 DescriptorScan 那条**:前缀判据的已知缝隙是"名字不叫 Gm* 的后门"。
// 它遍历四个服务的描述符,凡是不区分大小写以 `gm` 开头的方法都必须被判据认出来 ——
// 于是将来有人写出 `GMGrant` / `gmAdd`,是这条用例红,而不是线上多一条免鉴权印钞机。

#include <gtest/gtest.h>

#include <cctype>
#include <string>
#include <string_view>

#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_currency.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "proto/scene/player_rollback.pb.h"

#include "../../nodes/scene/handler/rpc/player/player_gm_guard.h"

namespace
{

// 2026-09-18 实得的、挂在 player service 上的全部 GM 方法:货币 4、宠物 1、属性 1、
// 回档/欠款 12。清单写在这里**不是**为了让运行时去查它(运行时用前缀判据,不查表),
// 而是为了让"新增一条 Gm* 却没被判据认出来"这件事在测试里就炸掉。
constexpr std::string_view kKnownClientGmMethods[] = {
	"GmAddCurrency", "GmDeductCurrency", "GmBlockCurrency", "GmUnblockCurrency",
	"GmGrantPet",
	"GmSetPlayerLevel",
	"GmAttachDebt", "GmWaiveDebt", "GmAdjustDebt", "GmFreezeDebt", "GmQueryDebt",
	"GmCreateSnapshot", "GmListSnapshots", "GmPreviewRollback", "GmExecuteRollback",
	"GmQueryTransactionLog", "GmTraceItem", "GmClawbackItem",
};

bool StartsWithGmIgnoringCase(std::string_view name)
{
	return name.size() >= 2 &&
		   std::tolower(static_cast<unsigned char>(name[0])) == 'g' &&
		   std::tolower(static_cast<unsigned char>(name[1])) == 'm';
}

void ExpectEveryGmMethodIsGated(const google::protobuf::ServiceDescriptor *service)
{
	ASSERT_NE(nullptr, service);
	for (int i = 0; i < service->method_count(); ++i)
	{
		// 直接初始化,不用 `=`:MethodDescriptor::name() 返回 absl::string_view
		// (在启用 ABSL_USES_STD_STRING_VIEW 时就是 std::string_view),
		// 而 std::string 从 string_view 的构造是 explicit 的,拷贝初始化编不过。
		const std::string name(service->method(i)->name());
		if (!StartsWithGmIgnoringCase(name))
		{
			continue;
		}
		EXPECT_TRUE(scene_gm_guard::IsClientGmMethodName(name))
			<< std::string(service->full_name()) << "." << name
			<< " 以 gm 开头却过不了闸门判据(判据要求 `Gm` + 大写字母)。"
			<< "要么把方法改名成 Gm<大写>,要么别把它挂在客户端 player service 上 ——"
			<< "照 proto/trade/trade_admin.proto 的做法,不标 OptionIsClientProtocolService。";
	}
}

} // namespace

TEST(ClientGmGateTest, MethodNamePrefix)
{
	EXPECT_TRUE(scene_gm_guard::IsClientGmMethodName("GmAddCurrency"));
	EXPECT_TRUE(scene_gm_guard::IsClientGmMethodName("GmX"));

	// 边界:长度不足、第三个字符不是大写、大小写不符的都不算 —— 判据必须窄而明确,
	// 否则 `GetBag` 这类正常 RPC 会被误伤下线。
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName(""));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("Gm"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("Gmx"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("Gm1"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("GetGmList"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("gmAdd"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("GMAdd"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("GetBag"));
	EXPECT_FALSE(scene_gm_guard::IsClientGmMethodName("MoveStart"));
}

TEST(ClientGmGateTest, AllKnownClientGmMethodsMatch)
{
	for (const auto name : kKnownClientGmMethods)
	{
		EXPECT_TRUE(scene_gm_guard::IsClientGmMethodName(name)) << name;
	}
}

TEST(ClientGmGateTest, DescriptorScan)
{
	ExpectEveryGmMethodIsGated(SceneCurrencyClientPlayer::descriptor());
	ExpectEveryGmMethodIsGated(ScenePetClientPlayer::descriptor());
	ExpectEveryGmMethodIsGated(SceneAttributeClientPlayer::descriptor());
	ExpectEveryGmMethodIsGated(SceneRollbackClientPlayer::descriptor());
}

TEST(ClientGmGateTest, DefaultRunModeRefusesGmCommands)
{
	using gate_security::ClassifyGmClientMessage;
	using gate_security::GmClientMessageVerdict;

	// 默认值在安全侧:未设置 / 拼错的值都被 ParseRunMode 归成 prod,prod 一律拒绝。
	// 这条与 gate_security_test.cpp 的同名断言重复是**故意**的:scene 这一侧的闸门
	// 依赖的正是这个结论,而两份测试分属两个可执行文件,任何一侧单独跑都得能发现回归。
	EXPECT_EQ(GmClientMessageVerdict::kRefuse,
			  ClassifyGmClientMessage(gate_security::ParseRunMode("", nullptr)));
	EXPECT_EQ(GmClientMessageVerdict::kRefuse,
			  ClassifyGmClientMessage(gate_security::ParseRunMode("prodd", nullptr)));
	EXPECT_EQ(GmClientMessageVerdict::kAllow,
			  ClassifyGmClientMessage(gate_security::ParseRunMode("dev", nullptr)));
	EXPECT_EQ(GmClientMessageVerdict::kAllow,
			  ClassifyGmClientMessage(gate_security::ParseRunMode("test", nullptr)));
}

TEST(ClientGmGateTest, SceneGateReadsItsOwnRunModeVariable)
{
	// 每个节点各自声明运行模式(gate: GATE_RUN_MODE,scene: SCENE_RUN_MODE)。
	// 钉死这个名字:改掉它等于让线上的 scene 闸门按一个没人设置的变量判断 ——
	// 方向仍是拒绝,但本地联调会突然全被拦,排查时没人想得到是变量名改了。
	EXPECT_EQ(std::string_view("SCENE_RUN_MODE"), std::string_view(scene_gm_guard::kSceneRunModeEnv));
}
