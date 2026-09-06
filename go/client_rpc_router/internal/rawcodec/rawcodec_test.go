package rawcodec

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/encoding"
)

// 编译期保证实现了 gRPC 的 Codec 接口。
var _ encoding.Codec = Codec{}

func TestNameIsProto(t *testing.T) {
	require.Equal(t, "proto", Codec{}.Name(), "content-subtype 必须是 proto,目标才会用 protobuf codec 反序列化")
}

// Marshal 原样透传,不改一个字节;Unmarshal 拷贝后与源一致 —— 往返恒等。
func TestRoundTrip(t *testing.T) {
	src := []byte{0x08, 0x96, 0x01, 0x12, 0x03, 'a', 'b', 'c'}
	wire, err := Codec{}.Marshal(src)
	require.NoError(t, err)
	require.Equal(t, src, wire)

	var out []byte
	require.NoError(t, Codec{}.Unmarshal(wire, &out))
	require.Equal(t, src, out)
}

func TestMarshalAcceptsPointerToBytes(t *testing.T) {
	src := []byte("payload")
	wire, err := Codec{}.Marshal(&src)
	require.NoError(t, err)
	require.Equal(t, src, wire)

	var nilPtr *[]byte
	_, err = Codec{}.Marshal(nilPtr)
	require.Error(t, err)
}

func TestMarshalRejectsOtherTypes(t *testing.T) {
	_, err := Codec{}.Marshal("not bytes")
	require.Error(t, err)
	_, err = Codec{}.Marshal(struct{}{})
	require.Error(t, err)
}

// Unmarshal 必须拷贝:gRPC 回收缓冲区后源切片会被复用。
func TestUnmarshalCopiesInsteadOfAliasing(t *testing.T) {
	wire := []byte{1, 2, 3, 4}
	var out []byte
	require.NoError(t, Codec{}.Unmarshal(wire, &out))
	wire[0] = 0xFF
	require.Equal(t, []byte{1, 2, 3, 4}, out, "Unmarshal 结果不能与输入共享底层数组")
}

func TestUnmarshalEmptyYieldsEmptyNonNil(t *testing.T) {
	var out []byte
	require.NoError(t, Codec{}.Unmarshal(nil, &out))
	require.NotNil(t, out)
	require.Len(t, out, 0)
}

func TestUnmarshalRejectsOtherTargets(t *testing.T) {
	var s string
	require.Error(t, Codec{}.Unmarshal([]byte("x"), &s))
	require.Error(t, Codec{}.Unmarshal([]byte("x"), []byte{}))
	var nilPtr *[]byte
	require.Error(t, Codec{}.Unmarshal([]byte("x"), nilPtr))
}
