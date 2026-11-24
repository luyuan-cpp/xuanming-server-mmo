#pragma once
#include <cstdint>

#include "proto/scene/player_trial_journal.pb.h"

constexpr uint32_t SceneTrialJournalClientPlayerGetTrialJournalMessageId = 456;
constexpr uint32_t SceneTrialJournalClientPlayerGetTrialJournalIndex = 0;
#define SceneTrialJournalClientPlayerGetTrialJournalMethod  ::SceneTrialJournalClientPlayer_Stub::descriptor()->method(0)

constexpr uint32_t SceneTrialJournalClientPlayerClaimTrialJournalMilestoneMessageId = 457;
constexpr uint32_t SceneTrialJournalClientPlayerClaimTrialJournalMilestoneIndex = 1;
#define SceneTrialJournalClientPlayerClaimTrialJournalMilestoneMethod  ::SceneTrialJournalClientPlayer_Stub::descriptor()->method(1)

constexpr uint32_t SceneTrialJournalClientPlayerRedeemBoundArtifactVoucherMessageId = 458;
constexpr uint32_t SceneTrialJournalClientPlayerRedeemBoundArtifactVoucherIndex = 2;
#define SceneTrialJournalClientPlayerRedeemBoundArtifactVoucherMethod  ::SceneTrialJournalClientPlayer_Stub::descriptor()->method(2)
