package combatai

import "math"

// 轻量可训练副本怪物效用打分器（纯 Go 标准库，与 C++ modules/combat_ai/monster_utility_ai.h 逐位对齐）。
//
// 线上推理全部采用 int64 定点数（万分比基准 UtilityScale = 10000），零浮点误差、零随机数，
// 保证 C++ / Go / Java 跨语言逐字节确定性回放。

const UtilityScale int32 = 10000

// UtilityScorer 定点效用打分接口（LinearUtilityModel / QuantizedMlpModel 均实现）。
type UtilityScorer interface {
	Score(features []int32) int64
}

// LinearUtilityModel 线上线性打分模型。
type LinearUtilityModel struct {
	Weights []int32
	Bias    int32
}

func (m LinearUtilityModel) Score(features []int32) int64 {
	sum := int64(m.Bias) * int64(UtilityScale)
	n := len(m.Weights)
	if len(features) < n {
		n = len(features)
	}
	for i := 0; i < n; i++ {
		sum += int64(m.Weights[i]) * int64(features[i])
	}
	return sum
}

// QuantizedMlpModel 进阶 2 层量化 MLP 模型（全 int64 + ReLU）。
type QuantizedMlpModel struct {
	W1 [][]int32
	B1 []int32
	W2 []int32
	B2 int32
}

func (m QuantizedMlpModel) Score(features []int32) int64 {
	out := int64(m.B2) * int64(UtilityScale)
	hiddenCount := len(m.W1)
	if len(m.B1) < hiddenCount {
		hiddenCount = len(m.B1)
	}
	if len(m.W2) < hiddenCount {
		hiddenCount = len(m.W2)
	}
	for h := 0; h < hiddenCount; h++ {
		hidden := int64(m.B1[h]) * int64(UtilityScale)
		row := m.W1[h]
		n := len(row)
		if len(features) < n {
			n = len(features)
		}
		for i := 0; i < n; i++ {
			hidden += int64(row[i]) * int64(features[i])
		}
		if hidden > 0 {
			activated := hidden / int64(UtilityScale)
			out += int64(m.W2[h]) * activated
		}
	}
	return out
}

// ActionCandidate 单个合法候选行动。
type ActionCandidate struct {
	ActionID      uint32
	TargetID      uint64
	FormationSlot uint32
	Features      []int32
}

type UtilitySelection struct {
	Index int
	Score int64
}

// SelectBestCandidate 确定性挑选最高分候选：
// 分数降序 -> 阵位升序 -> TargetID 升序 -> ActionID 升序，不消耗任何随机数。
func SelectBestCandidate(candidates []ActionCandidate, model UtilityScorer) (UtilitySelection, bool) {
	if len(candidates) == 0 || model == nil {
		return UtilitySelection{}, false
	}
	bestIdx := 0
	bestScore := model.Score(candidates[0].Features)

	for i := 1; i < len(candidates); i++ {
		score := model.Score(candidates[i].Features)
		cur := candidates[i]
		best := candidates[bestIdx]
		better := score > bestScore ||
			(score == bestScore && cur.FormationSlot < best.FormationSlot) ||
			(score == bestScore && cur.FormationSlot == best.FormationSlot && cur.TargetID < best.TargetID) ||
			(score == bestScore && cur.FormationSlot == best.FormationSlot && cur.TargetID == best.TargetID && cur.ActionID < best.ActionID)
		if better {
			bestIdx = i
			bestScore = score
		}
	}
	return UtilitySelection{Index: bestIdx, Score: bestScore}, true
}

// PairwiseTrainingSample 一条成对偏好样本：chosen 比 rejected 更优。
type PairwiseTrainingSample struct {
	Chosen       []int32
	Rejected     []int32
	SampleWeight float64
}

type PairwiseTrainConfig struct {
	Epochs           uint32
	LearningRate     float64
	L2Regularization float64
	MaxAbsWeight     float64
}

func DefaultPairwiseTrainConfig() PairwiseTrainConfig {
	return PairwiseTrainConfig{
		Epochs:           200,
		LearningRate:     0.25,
		L2Regularization: 0.001,
		MaxAbsWeight:     20.0,
	}
}

type PairwiseTrainMetrics struct {
	InitialLoss       float64
	FinalLoss         float64
	AccuracyPermyriad uint32
}

// TrainPairwiseLinearModel 确定性全批量 Bradley-Terry 成对排序逻辑回归 SGD：
// 全程无随机数，与 C++ TrainPairwiseLinearModel 产出完全一致的 int32 量化权重。
func TrainPairwiseLinearModel(
	featureCount int,
	samples []PairwiseTrainingSample,
	cfg PairwiseTrainConfig,
) (LinearUtilityModel, PairwiseTrainMetrics) {
	quantized := LinearUtilityModel{
		Weights: make([]int32, featureCount),
	}
	var metrics PairwiseTrainMetrics
	if featureCount <= 0 || len(samples) == 0 || cfg.Epochs == 0 {
		return quantized, metrics
	}

	const invScale = 1.0 / float64(UtilityScale)
	w := make([]float64, featureCount)

	computeLoss := func(weights []float64) float64 {
		totalLoss := 0.0
		totalWeight := 0.0
		for _, s := range samples {
			if s.SampleWeight <= 0.0 {
				continue
			}
			margin := 0.0
			for i := 0; i < featureCount; i++ {
				diff := float64(s.Chosen[i]-s.Rejected[i]) * invScale
				margin += weights[i] * diff
			}
			var sampleLoss float64
			if margin >= 0.0 {
				sampleLoss = math.Log1p(math.Exp(-margin))
			} else {
				sampleLoss = -margin + math.Log1p(math.Exp(margin))
			}
			totalLoss += s.SampleWeight * sampleLoss
			totalWeight += s.SampleWeight
		}
		if totalWeight > 0.0 {
			return totalLoss / totalWeight
		}
		return 0.0
	}

	initialLoss := computeLoss(w)
	grad := make([]float64, featureCount)

	for epoch := uint32(0); epoch < cfg.Epochs; epoch++ {
		clear(grad)
		totalWeight := 0.0

		for _, s := range samples {
			if s.SampleWeight <= 0.0 {
				continue
			}
			margin := 0.0
			for i := 0; i < featureCount; i++ {
				diff := float64(s.Chosen[i]-s.Rejected[i]) * invScale
				margin += w[i] * diff
			}
			var probWrong float64
			if margin >= 0.0 {
				e := math.Exp(-margin)
				probWrong = e / (1.0 + e)
			} else {
				probWrong = 1.0 / (1.0 + math.Exp(margin))
			}
			factor := s.SampleWeight * probWrong
			for i := 0; i < featureCount; i++ {
				diff := float64(s.Chosen[i]-s.Rejected[i]) * invScale
				grad[i] -= factor * diff
			}
			totalWeight += s.SampleWeight
		}

		if totalWeight <= 0.0 {
			break
		}

		for i := 0; i < featureCount; i++ {
			g := (grad[i] / totalWeight) + cfg.L2Regularization*w[i]
			next := w[i] - cfg.LearningRate*g
			if next < -cfg.MaxAbsWeight {
				next = -cfg.MaxAbsWeight
			} else if next > cfg.MaxAbsWeight {
				next = cfg.MaxAbsWeight
			}
			w[i] = next
		}
	}

	for i := 0; i < featureCount; i++ {
		quantized.Weights[i] = int32(math.Round(w[i] * float64(UtilityScale)))
	}

	metrics.InitialLoss = initialLoss
	metrics.FinalLoss = computeLoss(w)
	correct := uint64(0)
	for _, s := range samples {
		if quantized.Score(s.Chosen) > quantized.Score(s.Rejected) {
			correct++
		}
	}
	metrics.AccuracyPermyriad = uint32((correct * uint64(UtilityScale)) / uint64(len(samples)))
	return quantized, metrics
}
