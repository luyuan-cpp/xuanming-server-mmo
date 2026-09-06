// Package rawcodec 是路由服的 gRPC 原始字节 codec(设计决策 D32)。
//
// 路由服对业务 proto 零依赖:ClientRequest.body 已经是目标方法请求消息的
// protobuf 线格式,直接作为 gRPC 载荷发出;目标的响应字节原样收回,塞进
// MessageContent.serialized_message 交给 gate 下发。
//
// Name 必须返回 "proto":gRPC 用它拼 content-type(application/grpc+proto),
// 目标服务据此挑自己的 protobuf codec 反序列化。返回别的名字目标会以
// Unimplemented/Internal 拒收。
package rawcodec

import (
	"errors"
	"fmt"
)

// Name 是 codec 名,决定线上的 content-subtype。
const Name = "proto"

// Codec 实现 google.golang.org/grpc/encoding.Codec:
//   - Marshal 只接受 []byte / *[]byte,原样透传(不拷贝:请求体由本次 Forward 的
//     ForwardRequest 持有,生命周期覆盖整个 Invoke);
//   - Unmarshal 只接受 *[]byte,**拷贝**进去:gRPC 交给 Unmarshal 的切片可能来自
//     可回收缓冲区,函数返回后即失效,直接引用会读到被复用的内存。
type Codec struct{}

// Name 返回 "proto",见包注释。
func (Codec) Name() string { return Name }

// Marshal 把 []byte / *[]byte 原样作为线格式返回。
func (Codec) Marshal(v any) ([]byte, error) {
	switch body := v.(type) {
	case []byte:
		return body, nil
	case *[]byte:
		if body == nil {
			return nil, errors.New("rawcodec: Marshal 收到 nil *[]byte")
		}
		return *body, nil
	default:
		return nil, fmt.Errorf("rawcodec: Marshal 只接受 []byte / *[]byte,收到 %T", v)
	}
}

// Unmarshal 把线格式拷贝进 *[]byte。
func (Codec) Unmarshal(data []byte, v any) error {
	out, ok := v.(*[]byte)
	if !ok || out == nil {
		return fmt.Errorf("rawcodec: Unmarshal 只接受非 nil *[]byte,收到 %T", v)
	}
	copied := make([]byte, len(data))
	copy(copied, data)
	*out = copied
	return nil
}
