package _go

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"protogen/internal"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// robot 注册表(robotHandlerCases / generateTotalHandlerFile)与推送建桩(ensureRobotPushHandlers)的契约测试
// (docs/design/client-rpc-callback.md §5.5)。夹具复用 robot_rpc_test.go:内存描述符走正式分类,
// 不读 _config.Global 与工作目录,只写 t.TempDir();模板路径相对本包目录。

// 期望值一律手写,不调用被测函数推导。

const robotPushStubGuildNotifyChanged = "package handler\n\n" +
	"import (\n" +
	"\ttest \"proto/test\"\n" +
	"\t\"robot/logic/gameobject\"\n" +
	")\n\n" +
	"func GuildServiceNotifyChangedHandler(player *gameobject.Player, response *test.Changed) {\n}\n"

// 载荷所在目录末段 player 与桩的参数名撞名,import 别名退到整段目录 game_player。
const robotPushStubPlayerNotifyState = "package handler\n\n" +
	"import (\n" +
	"\tgame_player \"proto/game/player\"\n" +
	"\t\"robot/logic/gameobject\"\n" +
	")\n\n" +
	"func PlayerServiceNotifyStateHandler(player *gameobject.Player, response *game_player.State) {\n}\n"

// 嵌套消息按 protoc-gen-go 命名。
const robotPushStubPlayerNotifyNested = "package handler\n\n" +
	"import (\n" +
	"\ttest \"proto/test\"\n" +
	"\t\"robot/logic/gameobject\"\n" +
	")\n\n" +
	"func PlayerServiceNotifyNestedHandler(player *gameobject.Player, response *test.Outer_Inner) {\n}\n"

// 条目按传入顺序逐行渲染(Guild 在 Auction 之前,不排序)。
const robotTotalWantMap = "var messageHandlers = map[uint32]handlerFunc{\n" +
	"\tgame.GuildServiceNotifyChangedMessageId: unmarshalAndCall(GuildServiceNotifyChangedHandler),\n" +
	"\tgame.AuctionServiceBidMessageId: unmarshalAndCall(AuctionServiceBidHandler),\n" +
	"}\n"

const robotTotalWantEmptyMap = "var messageHandlers = map[uint32]handlerFunc{\n}\n"

// robotCaseNonPushFiles 是 robotCaseRun 夹具里 REQUEST / ONE_WAY / SERVER_ONLY 方法的 handler 文件名。
func robotCaseNonPushFiles() []string {
	return []string{
		"guild_service_read.go", "guild_service_move.go", "guild_service_disconnect.go",
		"guild_service_cancel_queue.go", "guild_service_fetch.go", "guild_service_peek.go",
		"auction_service_bid.go",
	}
}

// robotCasePushFiles 是 robotCaseRun 夹具里 PUSH 方法的 handler 文件名。
func robotCasePushFiles() []string {
	return []string{"guild_service_notify_changed.go", "auction_service_notify_bid.go"}
}

// robotCaseRun 在 GuildService(四种形态)+ AuctionService 上跑 robotHandlerCases。
// existing 是「磁盘上已存在」的 handler 文件名;queried 按调用顺序记录 handlerFileExists 收到的全部参数。
func robotCaseRun(t *testing.T, suffix string, existing ...string) (cases []HandlerCase, queried []string) {
	t.Helper()
	files, guild := robotFixture()
	auction := robotFixtureService(files[0], "AuctionService", 100,
		robotFixtureMethod{"Bid", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest},
		robotFixtureMethod{"NotifyBid", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
	)
	// GuildService 排在 AuctionService 之前:注册表保持服务的输入顺序,不按名字排序。
	services := robotFixtureClassify(t, guild, auction)
	var err error
	cases, err = robotHandlerCases(services, suffix, func(fileName string) (bool, error) {
		queried = append(queried, fileName)
		return slices.Contains(existing, fileName), nil
	})
	if err != nil {
		t.Fatalf("夹具的 handler 文件名都合法、存在性判断不出错,注册表计算不得失败: %v", err)
	}
	return cases, queried
}

// robotCaseAssert 比较注册表条目,并确认 handlerFileExists 只收到 robotHandlerFileName 的结果(不含目录),
// 且每个非 PUSH 方法都按自己的 handler 文件至少问过一次。
func robotCaseAssert(t *testing.T, got, want []HandlerCase, queried []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("注册表条目不符合预期\nwant: %+v\ngot:  %+v", want, got)
	}
	known := append(robotCaseNonPushFiles(), robotCasePushFiles()...)
	for _, name := range queried {
		if !slices.Contains(known, name) {
			t.Errorf("handlerFileExists 只应收到 robotHandlerFileName 的结果,收到 %q", name)
		}
	}
	for _, name := range robotCaseNonPushFiles() {
		if !slices.Contains(queried, name) {
			t.Errorf("非 PUSH 方法必须按 handler 文件 %s 是否存在决定登记,但没有查询它", name)
		}
	}
}

func TestRobotHandlerCasesRegistersPushAlwaysAndOthersOnlyWithHandlerFile(t *testing.T) {
	robotFixtureLogger(t)

	t.Run("部分handler文件存在", func(t *testing.T) {
		cases, queried := robotCaseRun(t, "MessageId",
			"guild_service_move.go", "guild_service_disconnect.go", "guild_service_fetch.go", "auction_service_bid.go")
		// Read / CancelQueue / Peek(REQUEST)没有 handler 文件,不登记;ONE_WAY Move、SERVER_ONLY Disconnect 有文件就登记。
		want := []HandlerCase{
			{MessageID: "GuildServiceMoveMessageId", FunctionCall: "GuildServiceMoveHandler"},
			{MessageID: "GuildServiceNotifyChangedMessageId", FunctionCall: "GuildServiceNotifyChangedHandler"},
			{MessageID: "GuildServiceDisconnectMessageId", FunctionCall: "GuildServiceDisconnectHandler"},
			{MessageID: "GuildServiceFetchMessageId", FunctionCall: "GuildServiceFetchHandler"},
			{MessageID: "AuctionServiceBidMessageId", FunctionCall: "AuctionServiceBidHandler"},
			{MessageID: "AuctionServiceNotifyBidMessageId", FunctionCall: "AuctionServiceNotifyBidHandler"},
		}
		robotCaseAssert(t, cases, want, queried)
	})

	t.Run("handler文件都不存在", func(t *testing.T) {
		cases, queried := robotCaseRun(t, "MessageId")
		// PUSH 的桩由 GoRobotHandlerGenerator 在同一轮保证存在,不看磁盘,照样登记。
		want := []HandlerCase{
			{MessageID: "GuildServiceNotifyChangedMessageId", FunctionCall: "GuildServiceNotifyChangedHandler"},
			{MessageID: "AuctionServiceNotifyBidMessageId", FunctionCall: "AuctionServiceNotifyBidHandler"},
		}
		robotCaseAssert(t, cases, want, queried)
	})

	t.Run("handler文件都存在", func(t *testing.T) {
		// 后缀取自参数(这里故意不用生产配置的 MessageId);PUSH 文件存在也只登记一次。
		cases, queried := robotCaseRun(t, "Id", append(robotCaseNonPushFiles(), robotCasePushFiles()...)...)
		want := []HandlerCase{
			{MessageID: "GuildServiceReadId", FunctionCall: "GuildServiceReadHandler"},
			{MessageID: "GuildServiceMoveId", FunctionCall: "GuildServiceMoveHandler"},
			{MessageID: "GuildServiceNotifyChangedId", FunctionCall: "GuildServiceNotifyChangedHandler"},
			{MessageID: "GuildServiceDisconnectId", FunctionCall: "GuildServiceDisconnectHandler"},
			{MessageID: "GuildServiceCancelQueueId", FunctionCall: "GuildServiceCancelQueueHandler"},
			{MessageID: "GuildServiceFetchId", FunctionCall: "GuildServiceFetchHandler"},
			{MessageID: "GuildServicePeekId", FunctionCall: "GuildServicePeekHandler"},
			{MessageID: "AuctionServiceBidId", FunctionCall: "AuctionServiceBidHandler"},
			{MessageID: "AuctionServiceNotifyBidId", FunctionCall: "AuctionServiceNotifyBidHandler"},
		}
		robotCaseAssert(t, cases, want, queried)
	})
}

func TestRobotHandlerCasesRejectsExistingNonPushHandlerFilesWithGoBuildSuffix(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	guild := robotFixtureService(files[0], "GuildService", 1,
		robotFixtureMethod{"ReadTest", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest},
		robotFixtureMethod{"MoveWindows", ".fixturepb.Request", ".Empty", internal.ClientRpcOneWay},
		robotFixtureMethod{"KickArm64", ".fixturepb.Request", ".Empty", internal.ClientRpcServerOnly},
		robotFixtureMethod{"Read", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest},
	)
	// guild_service_read_test.go 只进测试构建、guild_service_move_windows.go 只进 Windows 构建:文件存在就会被登记,
	// 正式构建引用不到函数,必须报错。guild_service_kick_arm64.go 不存在:这类方法不建桩、本来就不登记,
	// 方法名带构建后缀不能让整次生成失败。
	existing := []string{"guild_service_read_test.go", "guild_service_move_windows.go", "guild_service_read.go"}
	cases, err := robotHandlerCases(robotFixtureClassify(t, guild), "MessageId", func(fileName string) (bool, error) {
		return slices.Contains(existing, fileName), nil
	})
	if err == nil {
		t.Fatalf("已存在的非 PUSH handler 文件名命中 Go 构建后缀必须报错: cases=%+v", cases)
	}
	if cases != nil {
		t.Errorf("报错时不得返回注册表条目: %+v", cases)
	}
	// 错误收集全部方法后一起返回,每条都指明服务.方法与文件名。
	for _, identity := range []string{
		"GuildService.ReadTest", "guild_service_read_test.go",
		"GuildService.MoveWindows", "guild_service_move_windows.go",
	} {
		if !strings.Contains(err.Error(), identity) {
			t.Errorf("错误信息必须指明 %q: %v", identity, err)
		}
	}
	for _, unexpected := range []string{"GuildService.KickArm64", "GuildService.Read:"} {
		if strings.Contains(err.Error(), unexpected) {
			t.Errorf("没有 handler 文件或文件名合法的 %s 不得报错: %v", unexpected, err)
		}
	}
}

func TestRobotHandlerCasesSkipsMissingNonPushHandlersWithGoBuildSuffix(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	guild := robotFixtureService(files[0], "GuildService", 1,
		robotFixtureMethod{"KickArm64", ".fixturepb.Request", ".Empty", internal.ClientRpcServerOnly},
		robotFixtureMethod{"Read", ".fixturepb.Request", ".fixturepb.Response", internal.ClientRpcRequest},
	)
	cases, err := robotHandlerCases(robotFixtureClassify(t, guild), "MessageId", func(fileName string) (bool, error) {
		return fileName == "guild_service_read.go", nil
	})
	if err != nil {
		t.Fatalf("没有 handler 文件的非 PUSH 方法只跳过,不因方法名带构建后缀报错: %v", err)
	}
	want := []HandlerCase{{MessageID: "GuildServiceReadMessageId", FunctionCall: "GuildServiceReadHandler"}}
	if !slices.Equal(cases, want) {
		t.Fatalf("注册表条目: got=%+v want=%+v", cases, want)
	}
}

func TestRobotHandlerCasesRejectsHandlerFileStatErrors(t *testing.T) {
	robotFixtureLogger(t)

	t.Run("stat出错", func(t *testing.T) {
		_, guild := robotFixture()
		statErr := errors.New("access is denied")
		cases, err := robotHandlerCases(robotFixtureClassify(t, guild), "MessageId", func(fileName string) (bool, error) {
			if fileName == "guild_service_fetch.go" {
				return false, statErr
			}
			return true, nil
		})
		// 读不到的文件既不能当作存在(登记一个可能不存在的函数),也不能当作不存在(漏登手写 handler)。
		if !errors.Is(err, statErr) || cases != nil {
			t.Fatalf("stat 出错必须让注册表计算失败: cases=%+v err=%v", cases, err)
		}
		if !strings.Contains(err.Error(), "GuildService.Fetch") {
			t.Errorf("错误信息必须指明 GuildService.Fetch: %v", err)
		}
	})

	t.Run("handler路径不是普通文件", func(t *testing.T) {
		// 生产判定 robotHandlerFileExists:与 handler 同名的目录不能当作 handler 已存在。
		_, guild := robotFixture()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "guild_service_read.go"), 0o755); err != nil {
			t.Fatal(err)
		}
		cases, err := robotHandlerCases(robotFixtureClassify(t, guild), "MessageId", func(fileName string) (bool, error) {
			return robotHandlerFileExists(filepath.Join(dir, fileName))
		})
		if err == nil || cases != nil {
			t.Fatalf("handler 路径不是普通文件必须让注册表计算失败: cases=%+v err=%v", cases, err)
		}
		for _, identity := range []string{"GuildService.Read:", "guild_service_read.go", "not a regular file"} {
			if !strings.Contains(err.Error(), identity) {
				t.Errorf("错误信息必须指明 %q: %v", identity, err)
			}
		}
	})
}

func TestRobotHandlerFileNameMatchesExistingHandlerFiles(t *testing.T) {
	// 右列是 robot/logic/handler 下已有 handler 的文件名:规则一变,注册表就查不到它们,id = 0 的派发随之丢失。
	for _, test := range []struct{ key, want string }{
		{"GuildServiceNotifyGuildChanged", "guild_service_notify_guild_changed.go"},
		{"BattleClientPlayerNotifyBattleAssigned", "battle_client_player_notify_battle_assigned.go"},
		{"ClientPlayerJubaozhaiBuyRmbConsignment", "client_player_jubaozhai_buy_rmb_consignment.go"},
		// 数字之后的大写字母另起一段。
		{"SceneSceneClientPlayerSceneInfoC2S", "scene_scene_client_player_scene_info_c2_s.go"},
		// 连续大写按缩写处理,缩写后接小写时在末个大写字母前断开。
		{"GuildServiceNotifyHTTPServer", "guild_service_notify_http_server.go"},
	} {
		if got := robotHandlerFileName(test.key); got != test.want {
			t.Errorf("robotHandlerFileName(%q) = %q, want %q", test.key, got, test.want)
		}
	}
}

func robotPushEnsure(t *testing.T, files []*descriptorpb.FileDescriptorProto, dir string,
	services ...*internal.RPCServiceInfo) ([]string, error) {
	t.Helper()
	return ensureRobotPushHandlers(robotFixtureClassify(t, services...), files, dir, robotTemplate("robot_handler.go.tmpl"))
}

// robotPushStubDir 返回 dir 下的文件名(os.ReadDir 按名字升序)。
func robotPushStubDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRobotPushHandlersCreateMissingTypedStubsOnly(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	files = append(files, &descriptorpb.FileDescriptorProto{
		Name: proto.String("proto/game/player/state.proto"), Package: proto.String("playerpb"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("State")}},
	})
	player := robotFixtureService(files[0], "PlayerService", 20,
		robotFixtureMethod{"NotifyState", ".playerpb.State", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"NotifyNested", ".fixturepb.Outer.Inner", ".fixturepb.Outer.Inner", internal.ClientRpcPush},
	)
	dir := t.TempDir()

	created, err := robotPushEnsure(t, files, dir, guild, player)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []string{
		filepath.Join(dir, "guild_service_notify_changed.go"),
		filepath.Join(dir, "player_service_notify_state.go"),
		filepath.Join(dir, "player_service_notify_nested.go"),
	}
	if !slices.Equal(created, wantCreated) {
		t.Errorf("新建的桩按服务顺序、proto 声明顺序返回\nwant: %v\ngot:  %v", wantCreated, created)
	}
	// GuildService 的 REQUEST / ONE_WAY / SERVER_ONLY 都不建桩;写盘也不得留下临时文件。
	wantNames := []string{"guild_service_notify_changed.go", "player_service_notify_nested.go", "player_service_notify_state.go"}
	if names := robotPushStubDir(t, dir); !slices.Equal(names, wantNames) {
		t.Errorf("目录里只能有 PUSH 桩\nwant: %v\ngot:  %v", wantNames, names)
	}
	stubs := []struct{ name, want string }{
		{"guild_service_notify_changed.go", robotPushStubGuildNotifyChanged},
		{"player_service_notify_state.go", robotPushStubPlayerNotifyState},
		{"player_service_notify_nested.go", robotPushStubPlayerNotifyNested},
	}
	for _, stub := range stubs {
		got := robotReadFile(t, filepath.Join(dir, stub.name))
		if got != stub.want {
			t.Errorf("%s 内容不符合预期\nwant:\n%s\ngot:\n%s", stub.name, stub.want, got)
		}
		robotAssertGoSource(t, stub.name, got)
	}

	// 再跑一次:桩都已存在,不新建、不改写。
	again, err := robotPushEnsure(t, files, dir, guild, player)
	if err != nil || len(again) != 0 {
		t.Fatalf("桩已存在时不得新建: created=%v err=%v", again, err)
	}
	for _, stub := range stubs {
		if got := robotReadFile(t, filepath.Join(dir, stub.name)); got != stub.want {
			t.Errorf("已存在的桩被改写: %s", stub.name)
		}
	}
}

func TestRobotPushHandlersKeepExistingFiles(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	dir := t.TempDir()
	kept := []struct{ name, content string }{
		// 手写推送处理(带业务逻辑、CRLF 换行):生成器只看文件是否存在,一字不动。
		{"guild_service_notify_changed.go", "package handler\r\n\r\n// 手写推送处理。\r\nfunc GuildServiceNotifyChangedHandler(player *gameobject.Player, response *test.Changed) {\r\n\tplayer.Refresh(response)\r\n}\r\n"},
		// 迁移前遗留的 REQUEST 旧桩:不建、不删、不改。
		{"guild_service_read.go", "package handler\n\n// 旧场景按 message_id 派发的 REQUEST 回包。\n"},
	}
	for _, file := range kept {
		robotWriteFile(t, filepath.Join(dir, file.name), file.content)
	}

	created, err := robotPushEnsure(t, files, dir, guild)
	if err != nil || len(created) != 0 {
		t.Fatalf("PUSH 桩文件已存在时不得新建: created=%v err=%v", created, err)
	}
	for _, file := range kept {
		if got := robotReadFile(t, filepath.Join(dir, file.name)); got != file.content {
			t.Errorf("已存在的文件被改动: %s\nwant:\n%q\ngot:\n%q", file.name, file.content, got)
		}
	}
	wantNames := []string{"guild_service_notify_changed.go", "guild_service_read.go"}
	if names := robotPushStubDir(t, dir); !slices.Equal(names, wantNames) {
		t.Errorf("不得新增任何文件\nwant: %v\ngot:  %v", wantNames, names)
	}
}

func TestRobotPushHandlersRejectUnmappablePayloadTypes(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	auction := robotFixtureService(files[0], "AuctionService", 100,
		// 载荷所在文件不在 proto/<dir>/ 之下,robot 模块里没有对应的 Go 包。
		robotFixtureMethod{"NotifyWait", ".google.protobuf.Duration", ".Empty", internal.ClientRpcPush},
		// 载荷类型不在描述符集里。
		robotFixtureMethod{"NotifyGone", ".missingpb.Gone", ".Empty", internal.ClientRpcPush},
	)
	dir := t.TempDir()

	_, err := robotPushEnsure(t, files, dir, auction)
	if err == nil {
		t.Fatal("载荷类型无法映射必须让建桩失败,不能猜一个 Go 类型")
	}
	// 错误收集全部方法后一起返回,每条都指明服务.方法与原因。
	for _, identity := range []string{
		"AuctionService.NotifyWait", "google/protobuf/duration.proto",
		"AuctionService.NotifyGone", ".missingpb.Gone",
	} {
		if !strings.Contains(err.Error(), identity) {
			t.Errorf("错误信息必须指明 %q: %v", identity, err)
		}
	}
	if names := robotPushStubDir(t, dir); len(names) != 0 {
		t.Errorf("载荷类型无法映射时不得写出桩: %v", names)
	}
}

func TestRobotPushHandlersRejectGoBuildSuffixFileNames(t *testing.T) {
	robotFixtureLogger(t)
	files := robotFixtureFiles()
	guild := robotFixtureService(files[0], "GuildService", 1,
		robotFixtureMethod{"NotifyWindows", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"NotifyArm64", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"NotifyTest", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
	)
	dir := t.TempDir()
	// 同名文件已存在也要报错:_test.go 只进测试构建,注册表在正式构建里照样引用不到这个函数。
	const existingTestFile = "package handler\n\n// 只进测试构建的文件。\n"
	robotWriteFile(t, filepath.Join(dir, "guild_service_notify_test.go"), existingTestFile)

	_, err := robotPushEnsure(t, files, dir, guild)
	if err == nil {
		t.Fatal("桩文件名命中 Go 构建后缀必须让建桩失败")
	}
	for _, identity := range []string{
		"GuildService.NotifyWindows", "guild_service_notify_windows.go",
		"GuildService.NotifyArm64", "guild_service_notify_arm64.go",
		"GuildService.NotifyTest", "guild_service_notify_test.go",
	} {
		if !strings.Contains(err.Error(), identity) {
			t.Errorf("错误信息必须指明 %q: %v", identity, err)
		}
	}
	wantNames := []string{"guild_service_notify_test.go"}
	if names := robotPushStubDir(t, dir); !slices.Equal(names, wantNames) {
		t.Errorf("不得写出会被构建排除的桩\nwant: %v\ngot:  %v", wantNames, names)
	}
	if got := robotReadFile(t, filepath.Join(dir, "guild_service_notify_test.go")); got != existingTestFile {
		t.Error("已存在的文件被改动")
	}
}

func TestRobotPushHandlersKeepNonstandardPackageFunctions(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	for _, test := range []struct {
		name, source string
		wantCreated  int
	}{
		{"包级函数", "package handler\nfunc GuildServiceNotifyChangedHandler() {}\n", 0},
		{"接收者方法不是包级函数", "package handler\ntype receiver struct{}\nfunc (*receiver) GuildServiceNotifyChangedHandler() {}\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "custom_notifications.go")
			robotWriteFile(t, path, test.source)
			created, err := robotPushEnsure(t, files, dir, guild)
			if err != nil || len(created) != test.wantCreated {
				t.Fatalf("正式包级函数决定是否建桩: created=%v err=%v", created, err)
			}
			if got := robotReadFile(t, path); got != test.source {
				t.Fatal("非标准文件中的手写源码被改动")
			}
			if test.wantCreated == 1 {
				if got := robotReadFile(t, filepath.Join(dir, "guild_service_notify_changed.go")); got != robotPushStubGuildNotifyChanged {
					t.Fatalf("接收者方法不能取代注册表引用的包级桩: %s", got)
				}
			}
		})
	}
}

func TestRobotPushHandlersRejectCollisionsBeforeAnyWrite(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	other := robotFixtureService(files[0], "OtherService", 100,
		robotFixtureMethod{"NotifyHTTPServer", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
		robotFixtureMethod{"NotifyHttpServer", ".fixturepb.Changed", ".Empty", internal.ClientRpcPush},
	)
	dir := t.TempDir()
	created, err := robotPushEnsure(t, files, dir, guild, other)
	if err == nil {
		t.Fatal("不同方法映射同一文件必须在首个桩写入前拒绝")
	}
	for _, identity := range []string{"OtherService.NotifyHTTPServer", "OtherService.NotifyHttpServer", "other_service_notify_http_server.go"} {
		if !strings.Contains(err.Error(), identity) {
			t.Errorf("碰撞错误必须含 %q: %v", identity, err)
		}
	}
	if len(created) != 0 || len(robotPushStubDir(t, dir)) != 0 {
		t.Fatalf("批内后置碰撞不得留下前面服务的桩: %v", created)
	}
}

func TestRobotPushHandlersRejectUnreadableHandlerDirectory(t *testing.T) {
	robotFixtureLogger(t)
	files, guild := robotFixture()
	t.Run("目录实际是文件", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "handlers")
		robotWriteFile(t, path, "保留原文件")
		created, err := robotPushEnsure(t, files, path, guild)
		if err == nil || len(created) != 0 || robotReadFile(t, path) != "保留原文件" {
			t.Fatalf("目录读取失败必须拒绝且保留原件: created=%v err=%v", created, err)
		}
	})
	for _, test := range []struct{ name, source string }{
		{"损坏源码", "package handler\nfunc GuildServiceNotifyChangedHandler(\n"},
		{"仅条件编译函数", "//go:build windows\n\npackage handler\nfunc GuildServiceNotifyChangedHandler() {}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "custom_notifications.go")
			robotWriteFile(t, path, test.source)
			created, err := robotPushEnsure(t, files, dir, guild)
			if err == nil || len(created) != 0 || len(robotPushStubDir(t, dir)) != 1 || robotReadFile(t, path) != test.source {
				t.Fatalf("不能把未能确认的函数当作缺失或正式函数: created=%v err=%v", created, err)
			}
		})
	}
}

// robotTotalAssertParses 断言注册表是合法 Go 源码(注册表模板的 import 顺序与 map 对齐不是 gofmt 规范格式,这里不比 gofmt)。
func robotTotalAssertParses(t *testing.T, name, content string) {
	t.Helper()
	if !strings.HasPrefix(content, "package handler\n") {
		t.Errorf("%s 必须以 package handler 开头:\n%s", name, content)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), name, content, 0); err != nil {
		t.Fatalf("%s 不是合法 Go 源码: %v\n%s", name, err, content)
	}
}

func TestRobotTotalHandlerFileRendersEveryCase(t *testing.T) {
	robotFixtureLogger(t)
	target := filepath.Join(t.TempDir(), "handler", "message_body_handler.go")
	cases := []HandlerCase{
		{MessageID: "GuildServiceNotifyChangedMessageId", FunctionCall: "GuildServiceNotifyChangedHandler"},
		{MessageID: "AuctionServiceBidMessageId", FunctionCall: "AuctionServiceBidHandler"},
	}
	if err := generateTotalHandlerFile(target, cases, robotTemplate("robot_handler_total.go.tmpl")); err != nil {
		t.Fatal(err)
	}
	content := robotReadFile(t, target)
	robotTotalAssertParses(t, target, content)
	if !strings.Contains(content, robotTotalWantMap) {
		t.Errorf("注册表 map 不符合预期,want:\n%s\ngot:\n%s", robotTotalWantMap, content)
	}
	for _, line := range []string{
		"\tgame.GuildServiceNotifyChangedMessageId: unmarshalAndCall(GuildServiceNotifyChangedHandler),\n",
		"\tgame.AuctionServiceBidMessageId: unmarshalAndCall(AuctionServiceBidHandler),\n",
	} {
		if n := strings.Count(content, line); n != 1 {
			t.Errorf("每个条目必须恰好一行,%q 出现 %d 次", line, n)
		}
	}
}

func TestRobotTotalHandlerFileRendersEmptyCases(t *testing.T) {
	robotFixtureLogger(t)
	// nil 与空切片都要能渲染(robotHandlerCases 返回非 nil 的空切片)。
	for _, cases := range [][]HandlerCase{nil, {}} {
		target := filepath.Join(t.TempDir(), "message_body_handler.go")
		if err := generateTotalHandlerFile(target, cases, robotTemplate("robot_handler_total.go.tmpl")); err != nil {
			t.Fatalf("空条目也必须能渲染: cases=%#v err=%v", cases, err)
		}
		content := robotReadFile(t, target)
		robotTotalAssertParses(t, target, content)
		if !strings.Contains(content, robotTotalWantEmptyMap) {
			t.Errorf("空条目的注册表 map 不符合预期,want:\n%s\ngot:\n%s", robotTotalWantEmptyMap, content)
		}
	}
}

func TestRobotTotalHandlerFileRejectsEmptyPath(t *testing.T) {
	robotFixtureLogger(t)
	cases := []HandlerCase{{MessageID: "GuildServiceNotifyChangedMessageId", FunctionCall: "GuildServiceNotifyChangedHandler"}}
	err := generateTotalHandlerFile("", cases, robotTemplate("robot_handler_total.go.tmpl"))
	if err == nil || !strings.Contains(err.Error(), "path is empty") {
		t.Fatalf("注册表文件路径为空必须报错: %v", err)
	}
}
