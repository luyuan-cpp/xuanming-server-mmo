#include "bag_profile_registry.h"

#include <utility>

#include "muduo/base/Logging.h"

BagProfileRegistry &BagProfileRegistry::Instance()
{
	static BagProfileRegistry instance;
	return instance;
}

void BagProfileRegistry::Register(uint32_t profileId, BagProfileFactory factory)
{
	if (profileId == kBagProfileUnspecified)
	{
		LOG_ERROR << "BagProfileRegistry::Register: profile id 0 is the \"unspecified\" sentinel "
					 "and cannot be registered";
		return;
	}
	if (factory == nullptr)
	{
		LOG_ERROR << "BagProfileRegistry::Register: null factory for profile " << profileId;
		return;
	}
	if (const auto it = factories_.find(profileId); it != factories_.end())
	{
		// 覆盖是合法的(活动改版 / 热更),但必须留痕 —— 否则"这个包的规则怎么变了"
		// 会变成一个查不到的问题。
		LOG_WARN << "BagProfileRegistry::Register: overwriting existing factory for profile "
				 << profileId;
	}
	factories_[profileId] = std::move(factory);
}

bool BagProfileRegistry::Contains(uint32_t profileId) const
{
	return factories_.contains(profileId);
}

BagProfile BagProfileRegistry::Make(uint32_t profileId, std::size_t capacity) const
{
	if (const auto it = factories_.find(profileId); it != factories_.end())
	{
		BagProfile profile = it->second(capacity);
		// 工厂忘了填 id 是常见笔误(它自己不必知道自己的 id)。这里统一补上,
		// 否则存盘时 id 会丢,跨服一跳规则就没了。
		profile.id = profileId;
		return profile;
	}

	// fail-open:活动下线 / 尚未注册 / 脏数据。退化成自由格,但**保留 id**,
	// 于是这个包下次存盘仍带着它,活动重新上线后会自己变回去。
	if (profileId != kBagProfileUnspecified)
	{
		LOG_ERROR << "BagProfileRegistry::Make: no factory registered for profile " << profileId
				  << "; falling back to a plain flat bag of capacity " << capacity
				  << " (items are never dropped for a missing rule set)";
	}
	BagProfile fallback = BagProfile::Flat(capacity);
	fallback.id = profileId;
	return fallback;
}

void BagProfileRegistry::ClearForTest()
{
	factories_.clear();
}
