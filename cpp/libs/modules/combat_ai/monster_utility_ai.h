#pragma once

#include <algorithm>
#include <array>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <optional>
#include <span>

// 轻量可训练副本怪物效用打分器(纯 C++ 标准库,无外部 ML 框架依赖,无裸指针成员)。
//
// 为什么这样做:
// 1. 线上推理完全使用 int64_t 定点数(万分比基准 kUtilityScale = 10000),零浮点误差,
//    保证 TurnBattleEngine 的跨编译器 / 跨平台 / 跨语言逐字节确定性回放。
// 2. 离线训练内置 Bradley-Terry 成对排序逻辑回归 SGD(纯 <cmath>,直接吃玩家通关回放或
//    TurnBattleEngine 仿真采样的"更优出招 vs 次优出招"特征对),训练后量化成 int32_t 权重表,
//    可直接塞进配置表或嵌进 BehaviorTree 的 Action 节点里使用。

namespace combat_ai
{

inline constexpr int32_t kUtilityScale = 10000; // 万分比定点基准(与全仓万分比口径一致)

template <std::size_t FeatureCount>
using FeatureVec = std::array<int32_t, FeatureCount>;

// 线上线性打分模型:每个 Boss 仅需 (FeatureCount + 1) * 4 字节。
template <std::size_t FeatureCount>
struct LinearUtilityModel
{
    std::array<int32_t, FeatureCount> weights{};
    int32_t bias{0};

    [[nodiscard]] int64_t Score(const FeatureVec<FeatureCount>& features) const noexcept
    {
        int64_t sum = static_cast<int64_t>(bias) * kUtilityScale;
        for (std::size_t i = 0; i < FeatureCount; ++i)
        {
            sum += static_cast<int64_t>(weights[i]) * static_cast<int64_t>(features[i]);
        }
        return sum;
    }
};

// 进阶 2 层量化 MLP 模型(全 int64_t + ReLU):可表达非线性组合特征(如"敌方奶妈残血 且 己方大招就绪")。
template <std::size_t FeatureCount, std::size_t HiddenCount>
struct QuantizedMlpModel
{
    std::array<std::array<int32_t, FeatureCount>, HiddenCount> w1{};
    std::array<int32_t, HiddenCount> b1{};
    std::array<int32_t, HiddenCount> w2{};
    int32_t b2{0};

    [[nodiscard]] int64_t Score(const FeatureVec<FeatureCount>& features) const noexcept
    {
        int64_t out = static_cast<int64_t>(b2) * kUtilityScale;
        for (std::size_t h = 0; h < HiddenCount; ++h)
        {
            int64_t hidden = static_cast<int64_t>(b1[h]) * kUtilityScale;
            for (std::size_t i = 0; i < FeatureCount; ++i)
            {
                hidden += static_cast<int64_t>(w1[h][i]) * static_cast<int64_t>(features[i]);
            }
            if (hidden > 0)
            {
                const int64_t activated = hidden / kUtilityScale;
                out += static_cast<int64_t>(w2[h]) * activated;
            }
        }
        return out;
    }
};

// 单个合法候选行动(已过 CheckActionPrerequisites 校验)。
template <std::size_t FeatureCount>
struct ActionCandidate
{
    uint32_t actionId{0};       // 技能表 id 或普攻/防御枚举
    uint64_t targetId{0};       // 目标 actor_id
    uint32_t formationSlot{0};  // 同分决胜键 1:阵位升序
    FeatureVec<FeatureCount> features{};
};

struct UtilitySelection
{
    std::size_t index{0};
    int64_t score{0};
};

// 确定性挑选最高分候选:分数降序 -> 阵位升序 -> targetId 升序 -> actionId 升序,不消耗任何随机数。
template <std::size_t FeatureCount, typename Model>
[[nodiscard]] std::optional<UtilitySelection> SelectBestCandidate(
    std::span<const ActionCandidate<FeatureCount>> candidates, const Model& model) noexcept
{
    if (candidates.empty())
    {
        return std::nullopt;
    }
    std::size_t bestIdx = 0;
    int64_t bestScore = model.Score(candidates[0].features);

    for (std::size_t i = 1; i < candidates.size(); ++i)
    {
        const int64_t score = model.Score(candidates[i].features);
        const auto& cur = candidates[i];
        const auto& best = candidates[bestIdx];
        const bool better = (score > bestScore) ||
                            (score == bestScore && cur.formationSlot < best.formationSlot) ||
                            (score == bestScore && cur.formationSlot == best.formationSlot &&
                             cur.targetId < best.targetId) ||
                            (score == bestScore && cur.formationSlot == best.formationSlot &&
                             cur.targetId == best.targetId && cur.actionId < best.actionId);
        if (better)
        {
            bestIdx = i;
            bestScore = score;
        }
    }
    return UtilitySelection{bestIdx, bestScore};
}

// ---- 离线轻量训练器(仅离线工具或单测调用,线上战斗节点不跑训练) ----

// 一条成对偏好样本:在同一次副本回合决策里,chosen 比 rejected 更优(来自玩家胜局录像或自博弈回报)。
template <std::size_t FeatureCount>
struct PairwiseTrainingSample
{
    FeatureVec<FeatureCount> chosen{};
    FeatureVec<FeatureCount> rejected{};
    double sampleWeight{1.0};
};

struct PairwiseTrainConfig
{
    uint32_t epochs{200};
    double learningRate{0.25};
    double l2Regularization{0.001};
    double maxAbsWeight{20.0}; // 量化前权重截断上限,防 int32 乘加溢出
};

struct PairwiseTrainMetrics
{
    double initialLoss{0.0};
    double finalLoss{0.0};
    uint32_t accuracyPermyriad{0}; // 量化后整数模型在训练集上的成对准确率(0..10000)
};

// 用确定性全批量梯度下降最小化成对排序交叉熵:
//   L(w) = avg( weight * log(1 + exp(-w · (x_chosen - x_rejected))) ) + 0.5 * lambda * ||w||^2
// 全程无随机数,相同样本必产出相同量化权重。
template <std::size_t FeatureCount>
LinearUtilityModel<FeatureCount> TrainPairwiseLinearModel(
    std::span<const PairwiseTrainingSample<FeatureCount>> samples,
    const PairwiseTrainConfig& config = {},
    PairwiseTrainMetrics* outMetrics = nullptr)
{
    LinearUtilityModel<FeatureCount> quantized{};
    if (samples.empty() || config.epochs == 0)
    {
        return quantized;
    }

    constexpr double invScale = 1.0 / static_cast<double>(kUtilityScale);
    std::array<double, FeatureCount> w{};

    auto computeLoss = [&](const std::array<double, FeatureCount>& weights) -> double {
        double totalLoss = 0.0;
        double totalWeight = 0.0;
        for (const auto& s : samples)
        {
            if (s.sampleWeight <= 0.0)
            {
                continue;
            }
            double margin = 0.0;
            for (std::size_t i = 0; i < FeatureCount; ++i)
            {
                const double diff = static_cast<double>(s.chosen[i] - s.rejected[i]) * invScale;
                margin += weights[i] * diff;
            }
            // 数值稳定版 log(1 + exp(-margin))
            const double sampleLoss = (margin >= 0.0)
                                          ? std::log1p(std::exp(-margin))
                                          : (-margin + std::log1p(std::exp(margin)));
            totalLoss += s.sampleWeight * sampleLoss;
            totalWeight += s.sampleWeight;
        }
        return totalWeight > 0.0 ? (totalLoss / totalWeight) : 0.0;
    };

    const double initialLoss = computeLoss(w);

    for (uint32_t epoch = 0; epoch < config.epochs; ++epoch)
    {
        std::array<double, FeatureCount> grad{};
        double totalWeight = 0.0;

        for (const auto& s : samples)
        {
            if (s.sampleWeight <= 0.0)
            {
                continue;
            }
            double margin = 0.0;
            for (std::size_t i = 0; i < FeatureCount; ++i)
            {
                const double diff = static_cast<double>(s.chosen[i] - s.rejected[i]) * invScale;
                margin += w[i] * diff;
            }
            // sigmoid(-margin) = 1 / (1 + exp(margin))
            const double probWrong = (margin >= 0.0)
                                         ? (std::exp(-margin) / (1.0 + std::exp(-margin)))
                                         : (1.0 / (1.0 + std::exp(margin)));
            const double factor = s.sampleWeight * probWrong;
            for (std::size_t i = 0; i < FeatureCount; ++i)
            {
                const double diff = static_cast<double>(s.chosen[i] - s.rejected[i]) * invScale;
                grad[i] -= factor * diff;
            }
            totalWeight += s.sampleWeight;
        }

        if (totalWeight <= 0.0)
        {
            break;
        }

        for (std::size_t i = 0; i < FeatureCount; ++i)
        {
            const double g = (grad[i] / totalWeight) + config.l2Regularization * w[i];
            w[i] = std::clamp(w[i] - config.learningRate * g, -config.maxAbsWeight, config.maxAbsWeight);
        }
    }

    for (std::size_t i = 0; i < FeatureCount; ++i)
    {
        quantized.weights[i] = static_cast<int32_t>(std::llround(w[i] * kUtilityScale));
    }

    if (outMetrics != nullptr)
    {
        outMetrics->initialLoss = initialLoss;
        outMetrics->finalLoss = computeLoss(w);
        std::size_t correct = 0;
        for (const auto& s : samples)
        {
            if (quantized.Score(s.chosen) > quantized.Score(s.rejected))
            {
                ++correct;
            }
        }
        outMetrics->accuracyPermyriad = static_cast<uint32_t>(
            (static_cast<uint64_t>(correct) * kUtilityScale) / samples.size());
    }
    return quantized;
}

} // namespace combat_ai
