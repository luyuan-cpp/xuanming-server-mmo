package savebarrier

import (
	"errors"
	"strconv"
	"strings"
)

// HandoffFenceField 是屏障 hash(Key)里交接资格的字段名,与 C++ handoff_sql_barrier::kFenceField 相同。
const HandoffFenceField = "handoff_fence"

// HandoffFenceState 是交接资格的状态字母。
type HandoffFenceState byte

const (
	// HandoffFenceHeld:持有进程已登记、正在持有(加载成功后由它自己写入)。
	HandoffFenceHeld HandoffFenceState = 'H'
	// HandoffFenceFrozen:持有进程已冻结并发布交接 / 释放标记。
	HandoffFenceFrozen HandoffFenceState = 'F'
	// HandoffFenceRevoked:资格已撤销(登记取消、或恢复墓碑)。
	HandoffFenceRevoked HandoffFenceState = 'R'
)

// HandoffFence 是 C++ handoff_sql_barrier::Fence 的 Go 只读镜像。原文 "E|S|N|A|K|T":
// owner_epoch / 写下它的 scene 进程实例(NodeInfo.node_uuid,进程启动时生成)/ SQL admission generation /
// 本次加载 nonce / 状态 / 资格 nonce。
//
// 写者只有 C++ 持有进程(登记 / 冻结 / 撤销 Lua)与 go/db 的 save_barrier_restore;Go 服务只读。
// SceneInstance 是持有进程**自己**盖的章:只有真的登记成功、开始持有这名玩家的进程才会把自己的实例号写进来,
// 所以它回答的是「此刻谁持有这名玩家」,不是「路由当时打算派给谁」。
type HandoffFence struct {
	OwnerEpoch     uint64
	SceneInstance  string
	Generation     uint64
	AdmissionToken string
	State          HandoffFenceState
	Token          string
}

const handoffFenceNonceLen = 32

var errHandoffFenceMalformed = errors.New("交接资格原文不规范")

// ParseHandoffFence 与 C++ handoff_sql_barrier::ParseFence、Lua kFenceLuaHelpers 的 fence() 同一口径。
// 任何不规范的原文都返回错误;调用方按「没有证据」处理,不能拿解析失败当任何结论。
func ParseHandoffFence(raw string) (HandoffFence, error) {
	parts := strings.Split(raw, "|")
	if len(parts) != 6 {
		return HandoffFence{}, errHandoffFenceMalformed
	}
	owner, ok := parseHandoffFenceNumber(parts[0], false)
	if !ok {
		return HandoffFence{}, errHandoffFenceMalformed
	}
	generation, ok := parseHandoffFenceNumber(parts[2], true)
	if !ok || !isHandoffFenceNonce(parts[3], true) || !isHandoffFenceNonce(parts[5], false) ||
		len(parts[4]) != 1 || !isHandoffFenceInstance(parts[1]) {
		return HandoffFence{}, errHandoffFenceMalformed
	}
	state := HandoffFenceState(parts[4][0])
	switch state {
	case HandoffFenceHeld, HandoffFenceFrozen, HandoffFenceRevoked:
	default:
		return HandoffFence{}, errHandoffFenceMalformed
	}
	zeroNonce := strings.Repeat("0", handoffFenceNonceLen)
	// 没有历史 admission 下限的恢复墓碑不是一名虚构持有者(C++ 同名注释)。
	if generation == 0 {
		if state != HandoffFenceRevoked || parts[1] != "" || parts[3] != zeroNonce {
			return HandoffFence{}, errHandoffFenceMalformed
		}
	} else if parts[1] == "" || parts[3] == zeroNonce {
		return HandoffFence{}, errHandoffFenceMalformed
	}
	return HandoffFence{OwnerEpoch: owner, SceneInstance: parts[1], Generation: generation,
		AdmissionToken: parts[3], State: state, Token: parts[5]}, nil
}

// parseHandoffFenceNumber:规范十进制 uint64(非空、≤20 位、无前导零、不溢出)。
func parseHandoffFenceNumber(value string, allowZero bool) (uint64, bool) {
	if value == "" || len(value) > 20 || (len(value) != 1 && value[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (!allowZero && number == 0) {
		return 0, false
	}
	return number, true
}

// isHandoffFenceNonce:32 位小写十六进制;allowZero 为假时不能全零。
func isHandoffFenceNonce(value string, allowZero bool) bool {
	if len(value) != handoffFenceNonceLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return allowZero || value != strings.Repeat("0", handoffFenceNonceLen)
}

// isHandoffFenceInstance:≤256 字节,不含控制字符与空白(C++ 判 ch <= 32 || ch == 127,Lua 判 [%c%s])。
func isHandoffFenceInstance(value string) bool {
	if len(value) > 256 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= 32 || value[i] == 127 {
			return false
		}
	}
	return true
}
