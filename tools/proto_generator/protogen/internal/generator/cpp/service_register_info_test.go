package cpp

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"protogen/internal"

	messageoption "github.com/luyuancpp/protooption"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 消息号墓碑(muduo-rpc-channel.md 批 7):message_id.txt 里出现过、但 proto 里已删掉的 RPC 方法,
// 改写成 `N=reserved:<原 KeyName>` 永久占号,新方法发号时跳过。这里只测纯函数
// (parseMessageIdFile / assignMessageIds / checkMessageIdInputComplete / renderMessageIdFile /
// rpcMethodMetaInitLines),不碰真实的 message_id.txt、不依赖 logger 与配置初始化、不依赖墙钟。

// mustParseMessageIdFile 走与 ReadMessageIdFile 相同的解析路径。
func mustParseMessageIdFile(t *testing.T, content string) *idFileState {
	t.Helper()
	state, err := parseMessageIdFile(strings.NewReader(content))
	if err != nil {
		t.Fatalf("parseMessageIdFile(%q): %v", content, err)
	}
	return state
}

// newRpcServices 模拟 ReadProtoFileService 的产出:入参为 "Service.Method",未定号(Id = MaxUint64),
// 顺序即入参顺序(即 GlobalRPCServiceList 的顺序);相邻的同名服务归进同一个 RPCServiceInfo。
// KeyName = 服务名 + 方法名。
func newRpcServices(t *testing.T, qualifiedMethods ...string) internal.RPCServiceInfoList {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{Name: strPtr("proto/test/service.proto")}
	var services internal.RPCServiceInfoList
	for _, qualified := range qualifiedMethods {
		serviceName, methodName, ok := strings.Cut(qualified, ".")
		if !ok {
			t.Fatalf("newRpcServices wants Service.Method, got %q", qualified)
		}
		if len(services) == 0 || services[len(services)-1].Service() != serviceName {
			services = append(services, &internal.RPCServiceInfo{ProtoFileInfo: internal.ProtoFileInfo{
				Fd:                     file,
				ServiceDescriptorProto: &descriptorpb.ServiceDescriptorProto{Name: strPtr(serviceName)},
			}})
		}
		service := services[len(services)-1]
		service.Methods = append(service.Methods, &internal.MethodInfo{
			ProtoFileInfo:         service.ProtoFileInfo,
			Id:                    math.MaxUint64,
			Index:                 uint64(len(service.Methods)),
			MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: strPtr(methodName)},
		})
	}
	return services
}

func messageIdsByKey(services internal.RPCServiceInfoList) map[string]uint64 {
	ids := make(map[string]uint64)
	for _, service := range services {
		for _, method := range service.Methods {
			ids[method.KeyName()] = method.Id
		}
	}
	return ids
}

func mustAssignMessageIds(t *testing.T, services internal.RPCServiceInfoList, file *idFileState) messageIdAssignment {
	t.Helper()
	result, err := assignMessageIds(services, file)
	if err != nil {
		t.Fatalf("assignMessageIds: %v", err)
	}
	return result
}

// 改前的 TestInitMessageIdUpdatesRegistrySizeWhenAssigningUnusedId:文件只有 0 号,新方法接在最大号之后拿 1,
// 注册表长度(最大号 + 1)随之变成 2。
func TestAssignMessageIdsAppendsNewMethodAfterFileMax(t *testing.T) {
	file := mustParseMessageIdFile(t, "0=RegistryExisting\n")
	services := newRpcServices(t, "Registry.Existing", "Registry.New")

	result := mustAssignMessageIds(t, services, file)

	want := map[string]uint64{"RegistryExisting": 0, "RegistryNew": 1}
	if got := messageIdsByKey(services); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if result.MaxId+1 != 2 {
		t.Fatalf("registry length = %d, want 2", result.MaxId+1)
	}
}

func TestAssignMessageIdsTombstonesRemovedMethodsInsteadOfRecyclingThem(t *testing.T) {
	file := mustParseMessageIdFile(t, "0=SA\n1=SB\n2=SC\n")
	services := newRpcServices(t, "S.A", "S.C", "S.D")

	result := mustAssignMessageIds(t, services, file)

	want := map[string]uint64{"SA": 0, "SC": 2, "SD": 3}
	if got := messageIdsByKey(services); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v (removed SB's id 1 must not be recycled)", got, want)
	}
	if !reflect.DeepEqual(result.NewlyReserved, []uint64{1}) {
		t.Fatalf("NewlyReserved = %v, want [1]", result.NewlyReserved)
	}
	if result.Reserved[1] != "SB" {
		t.Fatalf("Reserved[1] = %q, want SB", result.Reserved[1])
	}
	if _, live := result.Infos[1]; live {
		t.Fatal("tombstoned id 1 must not appear among live methods")
	}
	if result.MaxId != 3 {
		t.Fatalf("MaxId = %d, want 3", result.MaxId)
	}
}

func TestAssignMessageIdsSkipsTombstonesButFillsPlainHoles(t *testing.T) {
	// 1 是墓碑,2 是从未发过的空洞(墓碑机制之前删掉的旧号,已无从追溯)。
	file := mustParseMessageIdFile(t, "0=SA\n1=reserved:SB\n3=SC\n")
	services := newRpcServices(t, "S.A", "S.C", "S.D", "S.E")

	result := mustAssignMessageIds(t, services, file)

	want := map[string]uint64{"SA": 0, "SC": 3, "SD": 2, "SE": 4}
	if got := messageIdsByKey(services); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if len(result.NewlyReserved) != 0 {
		t.Fatalf("NewlyReserved = %v, want none", result.NewlyReserved)
	}
	if result.Reserved[1] != "SB" {
		t.Fatalf("existing tombstone 1 must be kept, Reserved = %v", result.Reserved)
	}
}

// 改前的 InitMessageId 从 Go map 里随机取空号,同一份输入每次生成可能发出不同的号。
// 现在空洞升序、新方法按 GlobalRPCServiceList 顺序领号,反复跑结果逐字节一致。
func TestAssignMessageIdsFillsHolesInAscendingOrderDeterministically(t *testing.T) {
	const content = "0=SA\n2=reserved:SOld\n5=SB\n6=SGone\n9=SC\n11=SX\n12=SY\n"
	order := []string{"S.A", "S.N1", "S.B", "S.N2", "S.N3", "S.C", "S.N4", "S.N5", "S.N6", "S.N7"}
	// 空洞 = 0..12 中既非活号(0/5/9)也非墓碑(2,以及本轮新转的 6/11/12)的号:1 3 4 7 8 10。
	want := map[string]uint64{
		"SA": 0, "SB": 5, "SC": 9,
		"SN1": 1, "SN2": 3, "SN3": 4, "SN4": 7, "SN5": 8, "SN6": 10, "SN7": 13,
	}

	var firstRendered string
	for run := 0; run < 50; run++ {
		services := newRpcServices(t, order...)
		result := mustAssignMessageIds(t, services, mustParseMessageIdFile(t, content))

		if got := messageIdsByKey(services); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: ids = %v, want %v", run, got, want)
		}
		if !reflect.DeepEqual(result.NewlyReserved, []uint64{6, 11, 12}) {
			t.Fatalf("run %d: NewlyReserved = %v, want [6 11 12]", run, result.NewlyReserved)
		}
		rendered := renderMessageIdFile(result.Infos, result.Reserved)
		if run == 0 {
			firstRendered = rendered
		} else if rendered != firstRendered {
			t.Fatalf("run %d rendered a different file:\n got: %q\nwant: %q", run, rendered, firstRendered)
		}
	}
}

func TestAssignMessageIdsGivesReaddedNameANewIdAndKeepsTombstone(t *testing.T) {
	file := mustParseMessageIdFile(t, "0=SA\n1=reserved:SB\n")
	services := newRpcServices(t, "S.A", "S.B")

	result := mustAssignMessageIds(t, services, file)

	if got := messageIdsByKey(services)["SB"]; got != 2 {
		t.Fatalf("re-added SB got id %d, want fresh id 2", got)
	}
	if result.Reserved[1] != "SB" {
		t.Fatalf("tombstone 1 must stay, Reserved = %v", result.Reserved)
	}
	if !reflect.DeepEqual(result.ReaddedOverTombstone, []uint64{2}) {
		t.Fatalf("ReaddedOverTombstone = %v, want [2]", result.ReaddedOverTombstone)
	}
	if got := reservedIdsNamed(result.Reserved, "SB"); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("reservedIdsNamed(SB) = %v, want [1]", got)
	}

	// 同名的墓碑与活行写回后能再读回来(idFileState 允许两者共存),下一轮不再算"复活"。
	const wantRendered = "0=SA\n1=reserved:SB\n2=SB\n"
	rendered := renderMessageIdFile(result.Infos, result.Reserved)
	if rendered != wantRendered {
		t.Fatalf("rendered = %q, want %q", rendered, wantRendered)
	}
	second := mustAssignMessageIds(t, newRpcServices(t, "S.A", "S.B"), mustParseMessageIdFile(t, rendered))
	if len(second.ReaddedOverTombstone) != 0 || len(second.NewlyReserved) != 0 {
		t.Fatalf("second run must be a fixed point, got readded %v newly reserved %v",
			second.ReaddedOverTombstone, second.NewlyReserved)
	}
}

// KeyName = 服务名 + 方法名,拼接有歧义:服务 Ab 的方法 C 与服务 A 的方法 bC 都是 AbC,会争同一个号。
func TestAssignMessageIdsRejectsDuplicateKeyName(t *testing.T) {
	services := newRpcServices(t, "Ab.C", "A.bC")

	_, err := assignMessageIds(services, mustParseMessageIdFile(t, "0=AbC\n"))

	if err == nil {
		t.Fatal("two live methods with the same KeyName must be rejected")
	}
	if !strings.Contains(err.Error(), "AbC") {
		t.Fatalf("error must name the clashing key, got: %v", err)
	}
	for key, id := range messageIdsByKey(services) {
		if id != math.MaxUint64 {
			t.Fatalf("rejected assignment must not touch method ids, %s got %d", key, id)
		}
	}
}

func TestParseMessageIdFileRejectsMalformedLines(t *testing.T) {
	// 空行(含只有 \r 的行)跳过;CRLF 文件照常解析。
	state := mustParseMessageIdFile(t, "0=SA\r\n\r\n\n1=reserved:SB\r\n")
	if state.Active["SA"] != 0 || state.Reserved[1] != "SB" || state.MaxId != 1 {
		t.Fatalf("unexpected state: %+v", state)
	}

	// event_id.txt 对畸形行只告警跳过;message_id.txt 一律中止:跳过的号下一轮会被当成空洞发给别的方法。
	for _, content := range []string{
		"0=SA\nno equals sign\n",
		"0=SA\n1=a=b\n",
		"0=SA\nx=SB\n",
		"0=SA\n1=\n",
		"0=SA\n1=reserved:\n",
		"0=SA\n0=SB\n",
		"0=SA\n0=reserved:SB\n",
		"0=SA\n1=SA\n",
		// 线上 message_id 是 uint32;MaxUint64 还是"未定号"标记,放进来会让找空洞的循环停不下来。
		"0=SA\n4294967295=SB\n",
		"0=SA\n18446744073709551615=SB\n",
		"0=SA\n4294967295=reserved:SB\n",
	} {
		if _, err := parseMessageIdFile(strings.NewReader(content)); err == nil {
			t.Fatalf("parseMessageIdFile(%q) must fail", content)
		}
	}
}

// 手改把号多打几位:最大号被推到远处,注册表静默膨胀、该方法换号。空洞超过上限即中止,
// 错误信息指出最大号所在行;上限以内(墓碑机制之前的旧删除)照常放行。
func TestParseMessageIdFileRejectsFarJumpingIds(t *testing.T) {
	atLimit := fmt.Sprintf("0=SA\n%d=SB\n", maxMessageIdFileHoles+1)
	if state := mustParseMessageIdFile(t, atLimit); state.MaxId != maxMessageIdFileHoles+1 {
		t.Fatalf("%q: MaxId = %d", atLimit, state.MaxId)
	}
	overLimit := fmt.Sprintf("0=SA\n%d=SB\n", maxMessageIdFileHoles+2)
	if _, err := parseMessageIdFile(strings.NewReader(overLimit)); err == nil {
		t.Fatalf("parseMessageIdFile(%q) must fail: %d holes exceed the limit", overLimit, maxMessageIdFileHoles+1)
	}

	content, _ := liveMessageIdFile(10)
	typo := strings.Replace(content, "\n4=SM4\n", "\n4000=SM4\n", 1)
	_, err := parseMessageIdFile(strings.NewReader(typo))
	if err == nil {
		t.Fatal("a mistyped far-away ID must be rejected")
	}
	if !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), "4000=SM4") {
		t.Fatalf("error must point at the line holding the max ID, got: %v", err)
	}
}

func TestRenderMessageIdFileRoundTripsByteForByte(t *testing.T) {
	const content = "0=SA\n1=reserved:SOld\n2=SB\n3=reserved:SGone\n"
	result := mustAssignMessageIds(t, newRpcServices(t, "S.A", "S.B"), mustParseMessageIdFile(t, content))
	if rendered := renderMessageIdFile(result.Infos, result.Reserved); rendered != content {
		t.Fatalf("render must be byte-identical to input:\n got: %q\nwant: %q", rendered, content)
	}

	// 本轮新转的墓碑写出后,读回再跑一遍是不动点。
	first := mustAssignMessageIds(t, newRpcServices(t, "S.A", "S.C"), mustParseMessageIdFile(t, "0=SA\n1=SB\n2=SC\n"))
	firstRendered := renderMessageIdFile(first.Infos, first.Reserved)
	if firstRendered != "0=SA\n1=reserved:SB\n2=SC\n" {
		t.Fatalf("first render = %q", firstRendered)
	}
	second := mustAssignMessageIds(t, newRpcServices(t, "S.A", "S.C"), mustParseMessageIdFile(t, firstRendered))
	if secondRendered := renderMessageIdFile(second.Infos, second.Reserved); secondRendered != firstRendered {
		t.Fatalf("second render = %q, want %q", secondRendered, firstRendered)
	}
	if len(second.NewlyReserved) != 0 {
		t.Fatalf("second run NewlyReserved = %v, want none", second.NewlyReserved)
	}
}

// liveMessageIdFile 生成 `0=SM0 .. n-1=SM<n-1>` 全活号的 message_id.txt 内容及对应的 "S.M<i>" 方法名。
func liveMessageIdFile(n int) (string, []string) {
	var builder strings.Builder
	methods := make([]string, 0, n)
	for id := 0; id < n; id++ {
		builder.WriteString(fmt.Sprintf("%d=SM%d\n", id, id))
		methods = append(methods, fmt.Sprintf("S.M%d", id))
	}
	return builder.String(), methods
}

func TestCheckMessageIdInputCompleteTombstoneLimit(t *testing.T) {
	content, methods := liveMessageIdFile(10)
	cases := []struct {
		removed   int
		allowMass bool
		wantErr   bool
	}{
		{removed: 0},
		{removed: maxNewMessageTombstonesPerRun}, // 批 7:Route 一族 4 个
		{removed: maxNewMessageTombstonesPerRun + 1, wantErr: true},
		{removed: maxNewMessageTombstonesPerRun + 1, allowMass: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("removed=%d allowMass=%v", tc.removed, tc.allowMass), func(t *testing.T) {
			file := mustParseMessageIdFile(t, content)
			result := mustAssignMessageIds(t, newRpcServices(t, methods[tc.removed:]...), file)
			if len(result.NewlyReserved) != tc.removed {
				t.Fatalf("NewlyReserved = %v, want %d ids", result.NewlyReserved, tc.removed)
			}

			err := checkMessageIdInputComplete(file, result, tc.allowMass)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkMessageIdInputComplete() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				for _, want := range []string{"0=SM0", "4=SM4", allowMassMessageTombstoneEnv} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error must mention %q, got: %v", want, err)
					}
				}
			}
		})
	}
}

// 已在文件里的墓碑不计入本轮上限:只数本轮新转的。
func TestCheckMessageIdInputCompleteIgnoresExistingTombstones(t *testing.T) {
	content, methods := liveMessageIdFile(10)
	for id := 10; id < 16; id++ {
		content += fmt.Sprintf("%d=reserved:SOld%d\n", id, id)
	}
	file := mustParseMessageIdFile(t, content)
	result := mustAssignMessageIds(t, newRpcServices(t, methods[maxNewMessageTombstonesPerRun:]...), file)

	if err := checkMessageIdInputComplete(file, result, false); err != nil {
		t.Fatalf("6 old + %d new tombstones must pass, got: %v", maxNewMessageTombstonesPerRun, err)
	}
}

// proto 目录配空 / 描述符集没生成:本轮一个方法都没读到,不能把全部活号转墓碑,批量放行开关也不行。
func TestCheckMessageIdInputCompleteRejectsEmptyProtoInput(t *testing.T) {
	content, _ := liveMessageIdFile(10)
	file := mustParseMessageIdFile(t, content)
	result := mustAssignMessageIds(t, newRpcServices(t), file)

	if err := checkMessageIdInputComplete(file, result, true); err == nil {
		t.Fatal("tombstoning every live id from an empty proto input must be rejected even with the override")
	}

	// 空文件 + 空输入(全新仓库)没什么可误转的,放行。
	empty := mustParseMessageIdFile(t, "")
	if err := checkMessageIdInputComplete(empty, mustAssignMessageIds(t, newRpcServices(t), empty), false); err != nil {
		t.Fatalf("empty file with empty input must pass, got: %v", err)
	}
}

// 最大号本身是墓碑:注册表长度仍须覆盖它,旧节点发来的该号消息才会落到空槽被判为不可派发,
// 而不是被当成越界。
func TestMessageIdLenCoversTombstoneAtMaxId(t *testing.T) {
	originalFileMax := internal.FileMaxMessageId
	t.Cleanup(func() { internal.FileMaxMessageId = originalFileMax })

	for _, content := range []string{
		"0=SA\n1=SB\n",          // SB 本轮转墓碑
		"0=SA\n1=reserved:SB\n", // 文件里原有的墓碑
	} {
		result := mustAssignMessageIds(t, newRpcServices(t, "S.A"), mustParseMessageIdFile(t, content))
		internal.FileMaxMessageId = result.MaxId
		if got := internal.MessageIdLen(); got != 2 {
			t.Fatalf("%q: MessageIdLen() = %d, want 2", content, got)
		}
	}
}

// 批 7 的实际场景:删掉 Gate.RouteNodeMessage / RoutePlayerMessage 与 Scene.RouteNodeStringMsg /
// RoutePlayerStringMsg 之后,41/74/78/81 恰好 4 条新墓碑、卡线通过;注册表长度不变;其余号逐行不动、
// 没有删行;下一个新方法接在最大号之后;再跑一遍是不动点。第三轮是批 7 第二步:已有这 4 条墓碑时再删
// 1/40/42/56 四个已退役方法,已有墓碑不计入,恰好 4 条新墓碑、不放行也通过。
func TestAssignMessageIdsRouteFamilyRemoval(t *testing.T) {
	const fileLen = 301 // 0..300,与 2026-10-15 的 message_id.txt 同长
	special := map[int]string{
		1:  "Scene.SendMessageToPlayer", // Route 一族那一轮里仍是活方法(本用例第三轮才删)
		40: "Scene.InvokePlayerService", // 同上
		41: "Gate.RouteNodeMessage",
		42: "Gate.PlayerEnterGameNode", // 同上
		56: "Gate.BindSessionToGate",   // 同上
		74: "Gate.RoutePlayerMessage",
		78: "Scene.RouteNodeStringMsg",
		81: "Scene.RoutePlayerStringMsg",
	}
	routeIds := map[int]bool{41: true, 74: true, 78: true, 81: true}

	var builder strings.Builder
	originalLines := make([]string, 0, fileLen)
	keptMethods := make([]string, 0, fileLen)
	for id := 0; id < fileLen; id++ {
		qualified, ok := special[id]
		if !ok {
			qualified = fmt.Sprintf("Filler.M%03d", id)
		}
		line := fmt.Sprintf("%d=%s", id, strings.Replace(qualified, ".", "", 1))
		builder.WriteString(line + "\n")
		originalLines = append(originalLines, line)
		if !routeIds[id] {
			keptMethods = append(keptMethods, qualified)
		}
	}
	content := builder.String()
	// proto 顺序与号无关:倒序喂入,号仍只由文件决定。
	for i, j := 0, len(keptMethods)-1; i < j; i, j = i+1, j-1 {
		keptMethods[i], keptMethods[j] = keptMethods[j], keptMethods[i]
	}

	// 第一轮:只删 Route 一族,不加新方法。
	file := mustParseMessageIdFile(t, content)
	firstServices := newRpcServices(t, keptMethods...)
	first := mustAssignMessageIds(t, firstServices, file)

	if !reflect.DeepEqual(first.NewlyReserved, []uint64{41, 74, 78, 81}) {
		t.Fatalf("NewlyReserved = %v, want [41 74 78 81]", first.NewlyReserved)
	}
	if err := checkMessageIdInputComplete(file, first, false); err != nil {
		t.Fatalf("exactly %d new tombstones must pass without the override, got: %v", maxNewMessageTombstonesPerRun, err)
	}
	if first.MaxId+1 != fileLen {
		t.Fatalf("registry length = %d, want unchanged %d (kMaxRpcMethodCount)", first.MaxId+1, fileLen)
	}
	for key, id := range messageIdsByKey(firstServices) {
		if want := file.Active[key]; id != want {
			t.Fatalf("live method %s moved from %d to %d", key, want, id)
		}
	}

	rendered := renderMessageIdFile(first.Infos, first.Reserved)
	renderedLines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(renderedLines) != fileLen {
		t.Fatalf("rendered %d lines, want %d (no line may be deleted)", len(renderedLines), fileLen)
	}
	wantTombstones := map[int]string{
		41: "41=reserved:GateRouteNodeMessage",
		74: "74=reserved:GateRoutePlayerMessage",
		78: "78=reserved:SceneRouteNodeStringMsg",
		81: "81=reserved:SceneRoutePlayerStringMsg",
	}
	for id, line := range renderedLines {
		want := originalLines[id]
		if tombstone, ok := wantTombstones[id]; ok {
			want = tombstone
		}
		if line != want {
			t.Fatalf("rendered line for id %d = %q, want %q", id, line, want)
		}
	}

	// 若同批再删一个已退役方法(第 5 条新墓碑),不放行就必须中止 —— 所以退役的四个方法要等 Route 一族的墓碑落盘后,
	// 在下一轮才删(见本用例第三轮)。
	fiveRemoved := make([]string, 0, len(keptMethods))
	for _, qualified := range keptMethods {
		if qualified != "Scene.SendMessageToPlayer" {
			fiveRemoved = append(fiveRemoved, qualified)
		}
	}
	fiveResult := mustAssignMessageIds(t, newRpcServices(t, fiveRemoved...), file)
	if err := checkMessageIdInputComplete(file, fiveResult, false); err == nil {
		t.Fatal("a 5th new tombstone in the same run must be rejected without the override")
	}

	// 第二轮:读回第一轮写出的文件,再加一个新方法 —— 墓碑不回收,新号接在最大号之后;
	// 其余号与第一轮完全一致,且没有新的墓碑。
	secondFile := mustParseMessageIdFile(t, rendered)
	secondServices := newRpcServices(t, append(append([]string{}, keptMethods...), "Mail.SomethingNew")...)
	second := mustAssignMessageIds(t, secondServices, secondFile)

	secondIds := messageIdsByKey(secondServices)
	if got := secondIds["MailSomethingNew"]; got != fileLen {
		t.Fatalf("new method got id %d, want %d", got, fileLen)
	}
	for key, id := range messageIdsByKey(firstServices) {
		if secondIds[key] != id {
			t.Fatalf("method %s moved from %d to %d", key, id, secondIds[key])
		}
	}
	if len(second.NewlyReserved) != 0 {
		t.Fatalf("second run NewlyReserved = %v, want none", second.NewlyReserved)
	}
	for id, name := range map[uint64]string{41: "GateRouteNodeMessage", 74: "GateRoutePlayerMessage", 78: "SceneRouteNodeStringMsg", 81: "SceneRoutePlayerStringMsg"} {
		if second.Reserved[id] != name {
			t.Fatalf("tombstone %d lost on second run: Reserved = %v", id, second.Reserved)
		}
	}

	// 第三轮(批 7 第二步的实际场景):Route 一族的墓碑已在文件里,再删四个已退役方法。已有墓碑不计入本轮,
	// 所以恰好 4 条新墓碑、不放行也通过;8 条墓碑全在,注册表长度不变,其余号不动,没有删行。
	retiredIds := map[int]bool{1: true, 40: true, 42: true, 56: true}
	thirdMethods := make([]string, 0, len(keptMethods))
	for _, qualified := range keptMethods {
		if qualified != special[1] && qualified != special[40] && qualified != special[42] && qualified != special[56] {
			thirdMethods = append(thirdMethods, qualified)
		}
	}
	thirdServices := newRpcServices(t, thirdMethods...)
	third := mustAssignMessageIds(t, thirdServices, secondFile)
	if !reflect.DeepEqual(third.NewlyReserved, []uint64{1, 40, 42, 56}) {
		t.Fatalf("third run NewlyReserved = %v, want [1 40 42 56]", third.NewlyReserved)
	}
	if err := checkMessageIdInputComplete(secondFile, third, false); err != nil {
		t.Fatalf("4 new tombstones on top of existing ones must pass without the override, got: %v", err)
	}
	if third.MaxId+1 != fileLen {
		t.Fatalf("third run registry length = %d, want unchanged %d", third.MaxId+1, fileLen)
	}
	for key, id := range messageIdsByKey(thirdServices) {
		if want := secondFile.Active[key]; id != want {
			t.Fatalf("third run moved live method %s from %d to %d", key, want, id)
		}
	}
	thirdLines := strings.Split(strings.TrimSuffix(renderMessageIdFile(third.Infos, third.Reserved), "\n"), "\n")
	if len(thirdLines) != fileLen {
		t.Fatalf("third run rendered %d lines, want %d (no line may be deleted)", len(thirdLines), fileLen)
	}
	for id, line := range thirdLines {
		want := renderedLines[id]
		if retiredIds[id] {
			want = fmt.Sprintf("%d=reserved:%s", id, strings.Replace(special[id], ".", "", 1))
		}
		if line != want {
			t.Fatalf("third run rendered line for id %d = %q, want %q", id, line, want)
		}
	}
}

// RpcMethodMeta 已删 handler 成员:init 行不再带 `std::make_unique<XxxImpl>()`,gRPC 方法也不再有占位的 `nullptr, `。
func TestRpcMethodMetaInitLinesHaveNoHandler(t *testing.T) {
	cases := []struct {
		name string
		meta rpcMethodMetaInit
		want []string
	}{
		{
			name: "muduo method",
			meta: rpcMethodMetaInit{
				MessageIdConst: "ScenePlayerEnterGameNodeMessageId",
				ServiceName:    "Scene",
				MethodName:     "PlayerEnterGameNode",
				CppRequest:     "::PlayerEnterGameNodeRequest",
				CppResponse:    "::Empty",
				Protocol:       0,
				NodeType:       "common::base::eNodeType::SceneNodeService",
			},
			want: []string{
				"gRpcMethodRegistry[ScenePlayerEnterGameNodeMessageId] = RpcMethodMeta{",
				`    "Scene", "PlayerEnterGameNode",`,
				"    std::make_unique<::PlayerEnterGameNodeRequest>(),",
				"    std::make_unique<::Empty>(),",
				"    0, common::base::eNodeType::SceneNodeService};",
			},
		},
		{
			name: "grpc method with sender",
			meta: rpcMethodMetaInit{
				MessageIdConst: "BattleClientPlayerGetBattleStateMessageId",
				ServiceName:    "BattleClientPlayer",
				MethodName:     "GetBattleState",
				CppRequest:     "::GetBattleStateRequest",
				CppResponse:    "::BattleStateS2C",
				Protocol:       1,
				NodeType:       "common::base::eNodeType::BattleNodeService",
				SenderName:     "::SendBattleClientPlayerGetBattleState",
			},
			want: []string{
				"gRpcMethodRegistry[BattleClientPlayerGetBattleStateMessageId] = RpcMethodMeta{",
				`    "BattleClientPlayer", "GetBattleState",`,
				"    std::make_unique<::GetBattleStateRequest>(),",
				"    std::make_unique<::BattleStateS2C>(),",
				"    1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerGetBattleState};",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rpcMethodMetaInitLines(tc.meta)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
			for _, line := range got {
				if strings.Contains(line, "Impl") || strings.Contains(line, "nullptr,") {
					t.Fatalf("init line must not carry a handler: %q", line)
				}
			}
		})
	}
}

// 通用发送器必须显式接收完成回调;流式与 RPC0 方法不进入此路径。
func TestGrpcSenderFor(t *testing.T) {
	for _, tc := range []struct {
		name, pkg                                             string
		generic, clientStreaming, serverStreaming, wantSender bool
	}{
		{name: "RPC0", generic: true},
		{name: "客户端流", clientStreaming: true},
		{name: "服务端流", serverStreaming: true},
		{name: "有包名", pkg: "scene.manager", wantSender: true},
		{name: "无包名", wantSender: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := &internal.MethodInfo{
				ProtoFileInfo: internal.ProtoFileInfo{
					Fd:                     &descriptorpb.FileDescriptorProto{Package: strPtr(tc.pkg), Options: &descriptorpb.FileOptions{CcGenericServices: &tc.generic}},
					ServiceDescriptorProto: &descriptorpb.ServiceDescriptorProto{Name: strPtr("Svc")},
				},
				MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: strPtr("M"), ClientStreaming: &tc.clientStreaming, ServerStreaming: &tc.serverStreaming},
			}
			decl, sender, ok := grpcSenderFor(method)
			if ok != tc.wantSender {
				t.Fatalf("sender 存在=%v，期望 %v", ok, tc.wantSender)
			}
			if !ok {
				if decl != "" || sender != "" {
					t.Fatal("不支持的方法不得生成声明或绑定")
				}
				return
			}
			wantDecl := "void SendSvcMErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );"
			wantSender := "::SendSvcMErased"
			if tc.pkg != "" {
				ns := strings.ReplaceAll(tc.pkg, ".", "::")
				wantDecl = "namespace " + ns + "{" + wantDecl + "}"
				wantSender = ns + wantSender
			}
			if decl != wantDecl || sender != wantSender {
				t.Fatalf("声明=%q 绑定=%q；期望 %q / %q", decl, sender, wantDecl, wantSender)
			}
		})
	}
}

// gate 上行白名单 IsClientMessageId 的收号规则(docs/design/client-rpc-callback.md §7 R4)。
// 这里测的是 writeServiceInfoCppFile 实际调用的 clientAllowlistCases(接线),判定本身由 internal 包的
// TestClientRpcKindAcceptsClientUpstreamOnlyForClientInitiatedKinds 覆盖。

// allowlistFixtureMethod 是白名单单测里的一个方法;kind 为 nil 表示不标 OptionClientRpcKind(按 REQUEST)。
type allowlistFixtureMethod struct {
	name   string
	output string
	kind   *uint64
}

func allowlistKind(value uint64) *uint64 { return &value }

// newAllowlistService 构造一个服务。clientProtocol 决定是否标 OptionIsClientProtocolService;fileNode 不是
// NODE_UNSPECIFIED 时写进文件级 OptionFileDefaultNode。方法号从 firstId 起顺序分配(分类校验拒收未分配的号)。
// OptionClientRpcKind 与真实描述符一样写在未知字段里(外部 option 绑定里没有这个扩展)。
func newAllowlistService(file, service string, clientProtocol bool, fileNode messageoption.NodeType, firstId uint64,
	methods ...allowlistFixtureMethod) *internal.RPCServiceInfo {
	fd := &descriptorpb.FileDescriptorProto{Name: strPtr(file)}
	if fileNode != messageoption.NodeType_NODE_UNSPECIFIED {
		fd.Options = &descriptorpb.FileOptions{}
		proto.SetExtension(fd.Options, messageoption.E_OptionFileDefaultNode, fileNode)
	}
	sd := &descriptorpb.ServiceDescriptorProto{Name: strPtr(service), Options: &descriptorpb.ServiceOptions{}}
	if clientProtocol {
		proto.SetExtension(sd.Options, messageoption.E_OptionIsClientProtocolService, true)
	}
	info := internal.ProtoFileInfo{Fd: fd, ServiceDescriptorProto: sd}
	result := &internal.RPCServiceInfo{ProtoFileInfo: info}
	for i, m := range methods {
		md := &descriptorpb.MethodDescriptorProto{
			Name:       strPtr(m.name),
			InputType:  strPtr(".fixture." + m.name + "Request"),
			OutputType: strPtr(m.output),
			Options:    &descriptorpb.MethodOptions{},
		}
		if m.kind != nil {
			md.Options.ProtoReflect().SetUnknown(protowire.AppendVarint(
				protowire.AppendTag(nil, internal.OptionClientRpcKindNumber, protowire.VarintType), *m.kind))
		}
		sd.Method = append(sd.Method, md)
		result.Methods = append(result.Methods, &internal.MethodInfo{
			ProtoFileInfo:         info,
			Id:                    firstId + uint64(i),
			Index:                 uint64(i),
			MethodDescriptorProto: md,
		})
	}
	return result
}

// withGuildNodeType 让 proto/guild/ 下的服务对上节点类型枚举(HasNodeType 按目录名回退到 GuildNodeService)。
func withGuildNodeType(t *testing.T) {
	t.Helper()
	original := internal.NodeEnumValueSet
	t.Cleanup(func() { internal.NodeEnumValueSet = original })
	internal.NodeEnumValueSet = map[string]bool{"GuildNodeService": true}
}

// 只收进注册表的客户端协议服务里客户端能发起的方法:未标注(= REQUEST)与 ONE_WAY 收;PUSH、SERVER_ONLY、
// 没标客户端协议的服务、db 节点的服务、对不上节点类型的服务都不收。改前按服务全收,推送号与 UpdateGuildScore 也进了白名单。
func TestClientAllowlistCasesOnlyAdmitsClientInitiatedMethods(t *testing.T) {
	withGuildNodeType(t)
	sceneTCP := func(*internal.MethodInfo) bool { return true }

	services := internal.RPCServiceInfoList{
		newAllowlistService("proto/guild/guild.proto", "GuildService", true, messageoption.NodeType_NODE_UNSPECIFIED, 10,
			allowlistFixtureMethod{"CreateGuild", ".fixture.CreateGuildResponse", nil},
			allowlistFixtureMethod{"NotifyGuildEvent", ".Empty", allowlistKind(uint64(internal.ClientRpcPush))},
			allowlistFixtureMethod{"Move", ".Empty", allowlistKind(uint64(internal.ClientRpcOneWay))},
			allowlistFixtureMethod{"UpdateGuildScore", ".fixture.UpdateGuildScoreResponse", allowlistKind(uint64(internal.ClientRpcServerOnly))},
		),
		newAllowlistService("proto/guild/guild_admin.proto", "GuildAdmin", false, messageoption.NodeType_NODE_UNSPECIFIED, 20,
			allowlistFixtureMethod{"SetScore", ".fixture.SetScoreResponse", nil},
		),
		newAllowlistService("proto/guild/guild_db.proto", "GuildDb", true, messageoption.NodeType_NODE_DB, 30,
			allowlistFixtureMethod{"Load", ".fixture.LoadResponse", nil},
		),
		newAllowlistService("proto/role_fence/role_fence.proto", "RoleFenceParticipant", true, messageoption.NodeType_NODE_UNSPECIFIED, 40,
			allowlistFixtureMethod{"ApplyRoleFence", ".fixture.ApplyRoleFenceResponse", nil},
		),
	}

	got, err := clientAllowlistCases(services, sceneTCP, "MessageId")
	if err != nil {
		t.Fatalf("clientAllowlistCases: %v", err)
	}
	want := []string{"case GuildServiceCreateGuildMessageId:", "case GuildServiceMoveMessageId:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("白名单 = %q, want %q", got, want)
	}
}

// 形态读不出来或违反 §5.2 的方法让生成失败,不按 REQUEST 兜底放进白名单(fail-closed)。
func TestClientAllowlistCasesFailsClosedOnInvalidClientRpcKind(t *testing.T) {
	for _, test := range []struct {
		name   string
		method allowlistFixtureMethod
	}{
		{"越界的形态值", allowlistFixtureMethod{"CreateGuild", ".fixture.CreateGuildResponse", allowlistKind(9)}},
		{"Notify前缀没标推送", allowlistFixtureMethod{"NotifyGuildEvent", ".fixture.GuildEvent", nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			withGuildNodeType(t)
			services := internal.RPCServiceInfoList{
				newAllowlistService("proto/guild/guild.proto", "GuildService", true, messageoption.NodeType_NODE_UNSPECIFIED, 10, test.method),
			}
			got, err := clientAllowlistCases(services, func(*internal.MethodInfo) bool { return false }, "MessageId")
			var validation *internal.ClientRpcValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("err = %v, want *internal.ClientRpcValidationError", err)
			}
			if got != nil {
				t.Fatalf("出错时不得返回白名单, got %q", got)
			}
		})
	}
}
