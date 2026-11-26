#include <memory>
#include <string>
#include <unordered_map>
#include "rpc/player_service_interface.h"
#include "client_player_common_handler.h"
#include "player_bag_handler.h"
#include "player_activity_handler.h"
#include "player_afk_pass_handler.h"
#include "player_appearance_handler.h"
#include "player_attribute_handler.h"
#include "player_battle_result_handler.h"
#include "player_currency_handler.h"
#include "player_lifecycle_handler.h"
#include "player_mission_handler.h"
#include "player_movement_handler.h"
#include "player_mystery_merchant_handler.h"
#include "player_pet_handler.h"
#include "player_rewarded_ad_handler.h"
#include "player_scene_handler.h"
#include "player_season_handler.h"
#include "player_skill_handler.h"
#include "player_spirit_shop_handler.h"
#include "player_state_attribute_sync_handler.h"
#include "player_trial_journal_handler.h"
#include "s2s_player_scene_handler.h"
class SceneClientPlayerCommonImpl : public SceneClientPlayerCommon {};
class SceneBagClientPlayerImpl : public SceneBagClientPlayer {};
class SceneActivityClientPlayerImpl : public SceneActivityClientPlayer {};
class SceneAfkPassClientPlayerImpl : public SceneAfkPassClientPlayer {};
class SceneAppearanceClientPlayerImpl : public SceneAppearanceClientPlayer {};
class SceneAttributeClientPlayerImpl : public SceneAttributeClientPlayer {};
class SceneBattleResultClientPlayerImpl : public SceneBattleResultClientPlayer {};
class SceneCurrencyClientPlayerImpl : public SceneCurrencyClientPlayer {};
class ScenePlayerImpl : public ScenePlayer {};
class SceneMissionClientPlayerImpl : public SceneMissionClientPlayer {};
class SceneMovementClientPlayerImpl : public SceneMovementClientPlayer {};
class SceneMysteryMerchantClientPlayerImpl : public SceneMysteryMerchantClientPlayer {};
class ScenePetClientPlayerImpl : public ScenePetClientPlayer {};
class SceneRewardedAdClientPlayerImpl : public SceneRewardedAdClientPlayer {};
class SceneSceneClientPlayerImpl : public SceneSceneClientPlayer {};
class SceneSeasonClientPlayerImpl : public SceneSeasonClientPlayer {};
class SceneSkillClientPlayerImpl : public SceneSkillClientPlayer {};
class SceneSpiritShopClientPlayerImpl : public SceneSpiritShopClientPlayer {};
class ScenePlayerSyncImpl : public ScenePlayerSync {};
class SceneTrialJournalClientPlayerImpl : public SceneTrialJournalClientPlayer {};
class SceneScenePlayerImpl : public SceneScenePlayer {};

std::unordered_map<std::string, std::unique_ptr<PlayerService>> gPlayerService;

void InitPlayerService()
{
    gPlayerService.emplace("SceneClientPlayerCommon", std::make_unique<SceneClientPlayerCommonHandler>(std::make_unique<SceneClientPlayerCommonImpl>()));
    gPlayerService.emplace("SceneBagClientPlayer", std::make_unique<SceneBagClientPlayerHandler>(std::make_unique<SceneBagClientPlayerImpl>()));
    gPlayerService.emplace("SceneActivityClientPlayer", std::make_unique<SceneActivityClientPlayerHandler>(std::make_unique<SceneActivityClientPlayerImpl>()));
    gPlayerService.emplace("SceneAfkPassClientPlayer", std::make_unique<SceneAfkPassClientPlayerHandler>(std::make_unique<SceneAfkPassClientPlayerImpl>()));
    gPlayerService.emplace("SceneAppearanceClientPlayer", std::make_unique<SceneAppearanceClientPlayerHandler>(std::make_unique<SceneAppearanceClientPlayerImpl>()));
    gPlayerService.emplace("SceneAttributeClientPlayer", std::make_unique<SceneAttributeClientPlayerHandler>(std::make_unique<SceneAttributeClientPlayerImpl>()));
    gPlayerService.emplace("SceneBattleResultClientPlayer", std::make_unique<SceneBattleResultClientPlayerHandler>(std::make_unique<SceneBattleResultClientPlayerImpl>()));
    gPlayerService.emplace("SceneCurrencyClientPlayer", std::make_unique<SceneCurrencyClientPlayerHandler>(std::make_unique<SceneCurrencyClientPlayerImpl>()));
    gPlayerService.emplace("ScenePlayer", std::make_unique<ScenePlayerHandler>(std::make_unique<ScenePlayerImpl>()));
    gPlayerService.emplace("SceneMissionClientPlayer", std::make_unique<SceneMissionClientPlayerHandler>(std::make_unique<SceneMissionClientPlayerImpl>()));
    gPlayerService.emplace("SceneMovementClientPlayer", std::make_unique<SceneMovementClientPlayerHandler>(std::make_unique<SceneMovementClientPlayerImpl>()));
    gPlayerService.emplace("SceneMysteryMerchantClientPlayer", std::make_unique<SceneMysteryMerchantClientPlayerHandler>(std::make_unique<SceneMysteryMerchantClientPlayerImpl>()));
    gPlayerService.emplace("ScenePetClientPlayer", std::make_unique<ScenePetClientPlayerHandler>(std::make_unique<ScenePetClientPlayerImpl>()));
    gPlayerService.emplace("SceneRewardedAdClientPlayer", std::make_unique<SceneRewardedAdClientPlayerHandler>(std::make_unique<SceneRewardedAdClientPlayerImpl>()));
    gPlayerService.emplace("SceneSceneClientPlayer", std::make_unique<SceneSceneClientPlayerHandler>(std::make_unique<SceneSceneClientPlayerImpl>()));
    gPlayerService.emplace("SceneSeasonClientPlayer", std::make_unique<SceneSeasonClientPlayerHandler>(std::make_unique<SceneSeasonClientPlayerImpl>()));
    gPlayerService.emplace("SceneSkillClientPlayer", std::make_unique<SceneSkillClientPlayerHandler>(std::make_unique<SceneSkillClientPlayerImpl>()));
    gPlayerService.emplace("SceneSpiritShopClientPlayer", std::make_unique<SceneSpiritShopClientPlayerHandler>(std::make_unique<SceneSpiritShopClientPlayerImpl>()));
    gPlayerService.emplace("ScenePlayerSync", std::make_unique<ScenePlayerSyncHandler>(std::make_unique<ScenePlayerSyncImpl>()));
    gPlayerService.emplace("SceneTrialJournalClientPlayer", std::make_unique<SceneTrialJournalClientPlayerHandler>(std::make_unique<SceneTrialJournalClientPlayerImpl>()));
    gPlayerService.emplace("SceneScenePlayer", std::make_unique<SceneScenePlayerHandler>(std::make_unique<SceneScenePlayerImpl>()));
}
