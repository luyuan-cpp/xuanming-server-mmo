package internal

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// methodCommentFixture 构造含 FirstService / SecondService 两个服务、各有 Alpha / Beta 两个方法的文件;
// locations 原样作为 source code info,nil 表示描述符不带 source info。
// 返回按(服务下标, 方法下标)取 MethodInfo 的函数;MethodInfo 与文件共用描述符指针,同流水线。
func methodCommentFixture(locations []*descriptorpb.SourceCodeInfo_Location) func(service, method int) *MethodInfo {
	file := &descriptorpb.FileDescriptorProto{Name: proto.String("proto/test/comment.proto")}
	for _, serviceName := range []string{"FirstService", "SecondService"} {
		service := &descriptorpb.ServiceDescriptorProto{Name: proto.String(serviceName)}
		for _, methodName := range []string{"Alpha", "Beta"} {
			service.Method = append(service.Method, &descriptorpb.MethodDescriptorProto{Name: proto.String(methodName)})
		}
		file.Service = append(file.Service, service)
	}
	if locations != nil {
		file.SourceCodeInfo = &descriptorpb.SourceCodeInfo{Location: locations}
	}
	return func(service, method int) *MethodInfo {
		info := ProtoFileInfo{Fd: file, ServiceDescriptorProto: file.Service[service]}
		return &MethodInfo{ProtoFileInfo: info, MethodDescriptorProto: file.Service[service].Method[method]}
	}
}

func methodCommentLocation(leading string, path ...int32) *descriptorpb.SourceCodeInfo_Location {
	return &descriptorpb.SourceCodeInfo_Location{Path: path, LeadingComments: proto.String(leading)}
}

func TestMethodLeadingCommentsMatchesExactMethodPath(t *testing.T) {
	method := methodCommentFixture([]*descriptorpb.SourceCodeInfo_Location{
		methodCommentLocation(" 第一个服务的 Alpha\n", 6, 0, 2, 0),
		methodCommentLocation(" 第二个服务\n", 6, 1),
		// 方法 options 的位置以方法路径为前缀,排在方法本身之前,不能被当成方法的注释。
		methodCommentLocation(" 方法选项\n", 6, 1, 2, 1, 4),
		methodCommentLocation(" 第二个服务的 Beta\n", 6, 1, 2, 1),
	})
	for _, test := range []struct {
		name            string
		service, method int
		want            []string
	}{
		{"第一个服务第一个方法", 0, 0, []string{"第一个服务的 Alpha"}},
		{"第二个服务第二个方法", 1, 1, []string{"第二个服务的 Beta"}},
		{"没有前导注释的方法", 0, 1, nil},
		{"服务级注释不算方法注释", 1, 0, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := MethodLeadingComments(method(test.service, test.method)); !reflect.DeepEqual(got, test.want) {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestMethodLeadingCommentsTrimsCommentLayout(t *testing.T) {
	for _, test := range []struct {
		name    string
		leading string
		want    []string
	}{
		// 首尾空行丢弃、中间空行保留;每行只去一个前导空格(第二个空格属于原文缩进),行尾空白去掉。
		{"首尾空行与一个前导空格", "\n \n 第一行\n  保留一个缩进\n\n 末行 \t\n \n", []string{"第一行", " 保留一个缩进", "", "末行"}},
		{"CRLF换行", " 第一行\r\n 第二行\r\n", []string{"第一行", "第二行"}},
		{"只有空白视为没有注释", " \n\n \t\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := methodCommentFixture([]*descriptorpb.SourceCodeInfo_Location{
				methodCommentLocation(test.leading, 6, 0, 2, 0),
			})
			if got := MethodLeadingComments(method(0, 0)); !reflect.DeepEqual(got, test.want) {
				t.Errorf("前导注释 %q: got %q, want %q", test.leading, got, test.want)
			}
		})
	}

	// 只有尾随注释(没有 leading_comments)同样视为没有前导注释。
	method := methodCommentFixture([]*descriptorpb.SourceCodeInfo_Location{{
		Path: []int32{6, 0, 2, 0}, TrailingComments: proto.String(" 尾随注释\n"),
	}})
	if got := MethodLeadingComments(method(0, 0)); got != nil {
		t.Errorf("只有尾随注释应返回 nil: %q", got)
	}
}

func TestMethodLeadingCommentsReturnsNilWithoutSourceInfoOrOwnership(t *testing.T) {
	owned := methodCommentFixture([]*descriptorpb.SourceCodeInfo_Location{
		methodCommentLocation(" 注释\n", 6, 0, 2, 0),
	})(0, 0)
	// 对照:同一夹具里真正属于文件的方法取得到注释,下面的 nil 不是夹具本身取不到。
	if got := MethodLeadingComments(owned); !reflect.DeepEqual(got, []string{"注释"}) {
		t.Fatalf("对照方法应取到注释: %q", got)
	}

	// 服务不在文件里:即使它的第 0 个方法恰好对得上文件里 6/0/2/0 的位置,也不能借用别的服务的注释。
	foreignService := &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("ForeignService"), Method: []*descriptorpb.MethodDescriptorProto{owned.MethodDescriptorProto},
	}
	for _, test := range []struct {
		name   string
		method *MethodInfo
	}{
		{"描述符不带source info", methodCommentFixture(nil)(0, 0)},
		{"服务不在所属文件里", &MethodInfo{
			ProtoFileInfo:         ProtoFileInfo{Fd: owned.Fd, ServiceDescriptorProto: foreignService},
			MethodDescriptorProto: owned.MethodDescriptorProto,
		}},
		{"方法不在所属服务里", &MethodInfo{
			ProtoFileInfo:         owned.ProtoFileInfo,
			MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: proto.String("Gamma")},
		}},
		{"缺文件描述符", &MethodInfo{
			ProtoFileInfo:         ProtoFileInfo{ServiceDescriptorProto: owned.ServiceDescriptorProto},
			MethodDescriptorProto: owned.MethodDescriptorProto,
		}},
		{"空方法", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := MethodLeadingComments(test.method); got != nil {
				t.Errorf("应返回 nil: %q", got)
			}
		})
	}
}
