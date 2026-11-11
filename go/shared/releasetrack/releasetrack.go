// Package releasetrack 是发布轨道(stable / canary)的唯一定义,以及客户端请求链路上
// 按轨道选实例的唯一规则(docs/design/release-track-gray-canary.md §2.1、§2.4)。
//
// # 轨道从哪来
//
// 轨道是进程的部署属性:部署经环境变量 MMORPG_RELEASE_TRACK 注入,不设等于 stable,
// 进程存续期间不变。进程启动时读一次(FromEnv),取值非法就拒绝启动,所以注册进 etcd 的
// NodeInfo.release_track 只会是 "stable" 或 "canary"。别的节点没写这个字段(空串)也按 stable 算。
//
// Track 刻意不是 string:etcd 里读出的字符串不能直接转换成 Track,只能经 Parse。
// 若能直接转换,认不出的新取值(比如将来的第三条轨道)会被当成 stable,stable 调用方就会被送进它。
//
// # 选实例的规则(Select)
//
//   - 调用方是 canary:候选里有 canary 就只在 canary 里选,没有就退回 stable;
//   - 调用方是 stable:只在 stable 里选,永远不进 canary。
//
// 退回只有一个方向:canary 调用方没有 canary 实例时由 stable 接,stable 调用方永远不会被送进新版本。
// 本规则只用在客户端请求链路上:gate 选路由服、路由服给全局类型选实例。zone 内类型按 zone 选
// (一个 zone 只有一条轨道),服务间调用只连 stable(设计文档 §2.5)。
//
// C++ 的同一份规则在 cpp/libs/engine/core/node/system/node/release_track.h,两边单测用同一组样例。
//
// 本包只依赖标准库,无状态,并发安全。
package releasetrack

import (
	"fmt"
	"os"
)

// Track 是发布轨道。零值是 Stable。
type Track uint8

const (
	// Stable 是当前稳定版,也是零值:没写轨道的节点、没设环境变量的进程都是 stable。
	Stable Track = iota
	// Canary 是正在验证的版本。
	Canary
)

// EnvVar 是部署注入本进程轨道的环境变量名。C++ 节点读同一个名字。
const EnvVar = "MMORPG_RELEASE_TRACK"

// MetadataKey 是客户端请求链路上把会话轨道带给目标服务的 gRPC metadata 键(设计文档 §2.6)。
// 路由服按 ForwardRequest.release_track 写它(覆盖 gate 带来的同名键),gate 直连模式写 gate 自己的轨道;
// 取值同 NodeInfo.release_track,读不到按 stable 算。只决定由哪条轨道承载后续工作(比如战斗放在哪条轨道的 battle 上),
// 不是安全边界,所以不进 gate 的身份签名。C++ release_track.h 的 kMetadataKey 与它同名,对拍测试守着。
const MetadataKey = "x-release-track"

// 写进 NodeInfo.release_track 与 ForwardRequest.release_track 的两个取值。
const (
	stableName = "stable"
	canaryName = "canary"
)

// Parse 解析一个轨道取值。空串是 stable;其余只认 "stable"、"canary",区分大小写、不去空白:
// 部署写出的值必须原样就是这两个之一,宽松匹配会让拼错的配置悄悄落到某条轨道上。
func Parse(raw string) (Track, error) {
	switch raw {
	case "", stableName:
		return Stable, nil
	case canaryName:
		return Canary, nil
	default:
		return Stable, fmt.Errorf("release track %q is neither %q nor %q", raw, stableName, canaryName)
	}
}

// FromEnv 读本进程的轨道(EnvVar)。取值非法时返回 error,调用方必须拒绝启动:
// 带着认不出的轨道注册,节点会被所有选点方当作无效节点跳过,不报错,却也收不到流量。
func FromEnv() (Track, error) {
	track, err := Parse(os.Getenv(EnvVar))
	if err != nil {
		return Stable, fmt.Errorf("%s: %w", EnvVar, err)
	}
	return track, nil
}

// IsCanary 报告 t 是否为 canary。
func (t Track) IsCanary() bool { return t == Canary }

// RunsGlobalBackgroundWork 报告本轨道的进程是否承担全局作用域的后台工作:全局单例选主、全局 Kafka 消费组、
// 后台租约、清理与对账(设计文档 §2.5)。只有 stable 承担;全局服务的 canary 副本只处理路由服转来的客户端请求,
// 否则新版本的后台逻辑会作用于全部玩家,金丝雀的影响面就说不清了。zone 作用域的工作由该 zone 的进程照常承担,不归它管。
func (t Track) RunsGlobalBackgroundWork() bool { return !t.IsCanary() }

// String 返回写进 NodeInfo / ForwardRequest 的取值。越界值(只能由 Track(n) 强转得到)
// 不归一成 stable,而是返回一个 Parse 不认的串:写出去会被消费方当作无效值拒掉,不会悄悄变成 stable。
func (t Track) String() string {
	switch t {
	case Stable:
		return stableName
	case Canary:
		return canaryName
	default:
		return fmt.Sprintf("invalid-track-%d", uint8(t))
	}
}

// Select 按包注释的规则,从 candidates 里留下调用方 caller 能用的那部分,保持原有相对顺序。
// trackOf 返回候选的轨道,必须是 Parse 过的值;轨道无效的候选应在调用 Select 之前就剔掉。
// 返回新切片,不改 candidates;没有可用候选时返回空切片。
func Select[T any](caller Track, candidates []T, trackOf func(T) Track) []T {
	if caller.IsCanary() {
		if canary := withTrack(candidates, trackOf, Canary); len(canary) > 0 {
			return canary
		}
	}
	return withTrack(candidates, trackOf, Stable)
}

func withTrack[T any](candidates []T, trackOf func(T) Track, want Track) []T {
	kept := make([]T, 0, len(candidates))
	for _, candidate := range candidates {
		if trackOf(candidate) == want {
			kept = append(kept, candidate)
		}
	}
	return kept
}
