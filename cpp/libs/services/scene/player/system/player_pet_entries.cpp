#include "player_pet.h"
#include "pet_entry_rules.h"
#include "player_mutability.h"
#include <algorithm>
#include <limits>
#include "thread_context/ecs_context.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "battle/system/player_battle.h"
#include "table/code/petentry_table.h"
#include "table/code/petrule_table.h"
#include "table/code/pettier_table.h"
#include "table/code/pet_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/scene/player_pet.pb.h"

namespace {
using petentries::Points;
struct Config { uint32_t version=0, budget=0, maxDirections=0; petentries::Rules rules; };
Config ReadConfig() {
    Config c;
    const auto& globals=PetRuleTableManager::Instance().FindAll().data();
    if (globals.empty()) return c;
    const auto& g=globals.Get(0); c.version=g.entry_config_version(); c.budget=g.entry_total_points(); c.maxDirections=g.entry_max_directions();
    for(const auto& row:PetEntryTableManager::Instance().FindAll().data()) {
        if(row.id()==0 || row.cap()==0 || row.channel()<1 || row.channel()>9 ||
           row.per_point_bps()==0 || static_cast<uint64_t>(row.cap())*row.per_point_bps()>10000 ||
           ((row.channel()==5 || row.channel()==6) && (row.state_kind()<1 || row.state_kind()>7))) { c.version=0; break; }
        c.rules[row.id()]={row.cap(),row.per_point_bps()};
    }
    if(c.rules.empty() || c.budget==0) c.version=0;
    return c;
}
Points Copy(const google::protobuf::Map<uint32_t,uint32_t>& map) { return {map.begin(),map.end()}; }
PetInstance* FindOwned(entt::entity player,uint64_t petId,uint32_t& err) {
    if(!tlsEcs.actorRegistry.valid(player)) {err=kEntityIsNull;return nullptr;}
    auto* comp=tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
    if(!comp) {err=kServiceUnavailable;return nullptr;}
    for(auto& pet:*comp->mutable_pets()) if(pet.pet_id()==petId) {err=kSuccess;return &pet;}
    err=kPetNotFound;return nullptr;
}
uint32_t MapError(petentries::Error e) {
    switch(e) {
    case petentries::Error::None:return kSuccess;
    case petentries::Error::Unknown:return kPetDimensionNotFound;
    case petentries::Error::Cap:return kPetDimensionCapExceeded;
    case petentries::Error::Budget:return kPetPointsNotEnough;
    default:return kInvalidParameter;
    }
}
void AddEffect(PetCultivationEffects& e,const PetEntryTable& row,uint32_t bps) {
    switch(row.channel()) {
    case 1:e.set_physical_damage_bps(e.physical_damage_bps()+bps);break;
    case 2:e.set_magic_damage_bps(e.magic_damage_bps()+bps);break;
    case 3:e.set_outgoing_heal_bps(e.outgoing_heal_bps()+bps);break;
    case 4:e.set_shield_capacity_bps(e.shield_capacity_bps()+bps);break;
    case 5:(*e.mutable_ailment_hit_bps())[row.state_kind()]+=bps;break;
    case 6:(*e.mutable_ailment_resist_bps())[row.state_kind()]+=bps;break;
    case 7:e.set_poison_damage_bps(e.poison_damage_bps()+bps);break;
    case 8:e.set_retreat_hit_bps(e.retreat_hit_bps()+bps);break;
    case 9:e.set_retreat_resist_bps(e.retreat_resist_bps()+bps);break;
    }
}
uint32_t Fill(const PetInstance& pet,const Config& config,const Points& values,PetEntriesInfo& info) {
    info.Clear(); if(config.version==0) return kInvalidTableData;
    info.set_pet_id(pet.pet_id());info.set_state_version(petentries::Version(pet.entry_state_version()));
    info.set_config_version(config.version);info.set_total_points(config.budget);info.set_max_directions(config.maxDirections);
    info.set_last_request_id(pet.entry_last_request_id());
    Points known;uint64_t used=0,unknown=0;
    for(const auto& [id,v]:values) {used+=v;if(config.rules.contains(id))known[id]=v;else unknown+=v;}
    const bool effective=petentries::Validate(values,known,config.rules,config.budget,config.maxDirections)==petentries::Error::None;
    info.set_effective(effective);info.set_unknown_used_points(unknown);
    info.set_remaining_points(effective && used<=config.budget?static_cast<uint32_t>(config.budget-used):0);
    std::vector<const PetEntryTable*> rows;
    for(const auto& row:PetEntryTableManager::Instance().FindAll().data())rows.push_back(&row);
    std::sort(rows.begin(),rows.end(),[](auto* a,auto* b){return a->sort()==b->sort()?a->id()<b->id():a->sort()<b->sort();});
    for(const auto* row:rows) {
        auto* out=info.add_entries();out->set_id(row->id());out->set_name(row->name());out->set_desc(row->desc());
        out->set_category(row->category());out->set_channel(row->channel());out->set_state_kind(row->state_kind());
        out->set_cap(row->cap());out->set_per_point_bps(row->per_point_bps());out->set_sort(row->sort());out->set_icon(row->icon());
        const auto it=values.find(row->id());const uint32_t points=it==values.end()?0:it->second;
        out->set_allocated(points);const auto bps=effective?points*row->per_point_bps():0;out->set_effective_bps(bps);
        AddEffect(*info.mutable_effects(),*row,bps);
    }
    return kSuccess;
}
}

uint32_t PetSystem::BuildEntriesInfo(const PetInstance& pet,PetEntriesInfo& info) {
    return Fill(pet,ReadConfig(),Copy(pet.entry_allocated()),info);
}
uint32_t PetSystem::BuildEntries(entt::entity player,uint64_t petId,PetEntriesInfo& info) {
    info.Clear();uint32_t error;const auto* pet=FindOwned(player,petId,error);
    return pet?BuildEntriesInfo(*pet,info):error;
}
void PetSystem::FillCultivationTier(const PetInstance& pet,PetInfo& info) {
    uint32_t tier=pet.cultivation_tier();
    if(tier==0) { const auto* row=PetTableManager::Instance().FindByIdSilent(pet.pet_table_id()).first;if(row)tier=row->cultivation_tier(); }
    info.set_cultivation_tier(tier);
    if(const auto* row=PetTierTableManager::Instance().FindByIdSilent(tier).first) {
        info.set_cultivation_tier_name(row->name());info.set_cultivation_art_key(row->art_key());
    }
}
void PetSystem::PreviewEntries(entt::entity player,const PreviewPetEntriesRequest& request,PreviewPetEntriesResponse& response) {
    response.Clear();uint32_t error;const auto* pet=FindOwned(player,request.pet_id(),error);
    if(!pet) {response.mutable_error_message()->set_id(error);return;}
    const auto config=ReadConfig();const auto values=Copy(request.target());
    error=config.version==0?kInvalidTableData:
        request.config_version()!=config.version || request.state_version()!=petentries::Version(pet->entry_state_version())?kInvalidParameter:
        MapError(petentries::Validate(Copy(pet->entry_allocated()),values,config.rules,config.budget,config.maxDirections));
    if(error!=kSuccess) {response.mutable_error_message()->set_id(error);BuildEntriesInfo(*pet,*response.mutable_entries());return;}
    Fill(*pet,config,petentries::ReplaceKnown(Copy(pet->entry_allocated()),values,config.rules),*response.mutable_entries());
}
void PetSystem::SaveEntries(entt::entity player,const SavePetEntriesRequest& request,SavePetEntriesResponse& response) {
    response.Clear();response.set_request_id(request.request_id());uint32_t error;
    auto* pet=FindOwned(player,request.pet_id(),error);
    if(!pet) {response.mutable_error_message()->set_id(error);return;}
    const auto fail=[&](uint32_t err){response.mutable_error_message()->set_id(err);BuildEntriesInfo(*pet,*response.mutable_entries());};
    if(!player_mutability::IsPlayerMutable(player)) {fail(kInvalidParameter);return;}
    if(PlayerBattleSystem::IsInBattle(player)) {fail(kPetInBattle);return;}
    if(!petentries::ValidRequestId(request.request_id())) {fail(kInvalidParameter);return;}
    const auto target=Copy(request.target());
    if(pet->entry_last_request_id()==request.request_id()) {
        if(pet->entry_last_base_version()!=request.state_version() || pet->entry_last_config_version()!=request.config_version() || Copy(pet->entry_last_target())!=target) {fail(kInvalidParameter);return;}
        response.set_replayed(true);BuildEntriesInfo(*pet,*response.mutable_entries());return;
    }
    const auto config=ReadConfig();
    if(config.version==0) {fail(kInvalidTableData);return;}
    if(request.config_version()!=config.version || request.state_version()!=petentries::Version(pet->entry_state_version()) ||
       pet->entry_state_version()==std::numeric_limits<uint64_t>::max()) {fail(kInvalidParameter);return;}
    const auto stored=Copy(pet->entry_allocated());
    error=MapError(petentries::Validate(stored,target,config.rules,config.budget,config.maxDirections));
    if(error!=kSuccess) {fail(error);return;}
    // scene 单线程原子替换一只宝宝；整条组件与玩家存档/迁移/交易快照一起保留回执。
    PetInstance candidate=*pet;
    candidate.mutable_entry_allocated()->clear();
    for(const auto& [id,value]:petentries::ReplaceKnown(stored,target,config.rules))(*candidate.mutable_entry_allocated())[id]=value;
    candidate.set_entry_state_version(petentries::Version(pet->entry_state_version())+1);
    candidate.set_entry_last_request_id(request.request_id());candidate.set_entry_last_base_version(request.state_version());
    candidate.set_entry_last_config_version(request.config_version());*candidate.mutable_entry_last_target()=request.target();
    PetEntriesInfo validated;
    if(Fill(candidate,config,Copy(candidate.entry_allocated()),validated)!=kSuccess) {fail(kInvalidTableData);return;}
    pet->Swap(&candidate);*response.mutable_entries()=std::move(validated);
}
