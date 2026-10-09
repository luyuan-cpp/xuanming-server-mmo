package cpp

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 事件号墓碑(turn-based §22 D67):event_id.txt 里出现过、但 proto 里已删掉的事件,
// 改写成 `N=reserved:<原名>` 永久占号,新事件发号时跳过。这里只测纯函数
// (parseEventIdLine / eventIdFileState.add / assignEventIds / renderEventIdFile),
// 不碰真实的 event_id.txt、不依赖 logger 初始化。

func mustLoadEventIdFile(t *testing.T, content string) *eventIdFileState {
	t.Helper()
	state := newEventIdFileState()
	if content == "" {
		return state
	}
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		entry, ok, err := parseEventIdLine(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if !ok {
			t.Fatalf("unexpected invalid line %q", line)
		}
		if err := state.add(entry); err != nil {
			t.Fatalf("add %q: %v", line, err)
		}
	}
	return state
}

// newProtoEvents 模拟 ReadAllProtoEvents 的产出:未定号(Id = MaxUint64),顺序即入参顺序。
func newProtoEvents(names ...string) []*ProtoEventInfo {
	events := make([]*ProtoEventInfo, 0, len(names))
	for _, name := range names {
		events = append(events, &ProtoEventInfo{Id: math.MaxUint64, IdName: name})
	}
	return events
}

func eventIdsByName(events []*ProtoEventInfo) map[string]uint64 {
	ids := make(map[string]uint64, len(events))
	for _, event := range events {
		ids[event.IdName] = event.Id
	}
	return ids
}

func TestParseEventIdLine(t *testing.T) {
	cases := []struct {
		line   string
		want   eventIdFileEntry
		wantOK bool
	}{
		{line: "7=CombatStateAddedEvent", want: eventIdFileEntry{Id: 7, Name: "CombatStateAddedEvent"}, wantOK: true},
		{line: " 43 = reserved:ContractsKafkaBindBattleEvent\r", want: eventIdFileEntry{Id: 43, Name: "ContractsKafkaBindBattleEvent", Reserved: true}, wantOK: true},
		{line: "44=reserved: ContractsKafkaUnbindBattleEvent", want: eventIdFileEntry{Id: 44, Name: "ContractsKafkaUnbindBattleEvent", Reserved: true}, wantOK: true},
		{line: "", wantOK: false},
		{line: "no equals sign", wantOK: false},
		{line: "1=a=b", wantOK: false},
	}
	for _, tc := range cases {
		got, ok, err := parseEventIdLine(tc.line)
		if err != nil {
			t.Fatalf("parseEventIdLine(%q) unexpected error: %v", tc.line, err)
		}
		if ok != tc.wantOK {
			t.Fatalf("parseEventIdLine(%q) ok = %v, want %v", tc.line, ok, tc.wantOK)
		}
		if ok && got != tc.want {
			t.Fatalf("parseEventIdLine(%q) = %+v, want %+v", tc.line, got, tc.want)
		}
	}

	if _, _, err := parseEventIdLine("x=SomeEvent"); err == nil {
		t.Fatal("non-numeric id must be an error, got nil")
	}
}

func TestEventIdFileStateRejectsDuplicates(t *testing.T) {
	cases := []struct {
		name    string
		content string
		bad     string
	}{
		{name: "same id twice", content: "3=A", bad: "3=B"},
		{name: "live id also reserved", content: "3=A", bad: "3=reserved:B"},
		{name: "reserved id also live", content: "3=reserved:A", bad: "3=B"},
		{name: "reserved id twice", content: "3=reserved:A", bad: "3=reserved:B"},
		{name: "same live name twice", content: "3=A", bad: "4=A"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := mustLoadEventIdFile(t, tc.content)
			entry, ok, err := parseEventIdLine(tc.bad)
			if err != nil || !ok {
				t.Fatalf("test line %q must parse: ok=%v err=%v", tc.bad, ok, err)
			}
			if err := state.add(entry); err == nil {
				t.Fatalf("add(%q) after %q must fail", tc.bad, tc.content)
			}
		})
	}

	// 同名的活事件与墓碑可以共存:删掉后又加回同名消息,拿的是新号,墓碑照旧占着老号。
	state := mustLoadEventIdFile(t, "1=reserved:B\n2=B\n")
	if state.Active["B"] != 2 || state.Reserved[1] != "B" || state.MaxId != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestAssignEventIdsTombstonesRemovedEventsInsteadOfRecyclingThem(t *testing.T) {
	file := mustLoadEventIdFile(t, "0=A\n1=B\n2=C\n3=D\n4=E\n")
	events := newProtoEvents("A", "B", "D", "E", "X")

	result := assignEventIds(events, file)

	want := map[string]uint64{"A": 0, "B": 1, "D": 3, "E": 4, "X": 5}
	if got := eventIdsByName(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v (removed C's id 2 must not be recycled)", got, want)
	}
	if !reflect.DeepEqual(result.NewlyReserved, []uint64{2}) {
		t.Fatalf("NewlyReserved = %v, want [2]", result.NewlyReserved)
	}
	if result.Reserved[2] != "C" {
		t.Fatalf("Reserved[2] = %q, want C", result.Reserved[2])
	}
	if _, live := result.Infos[2]; live {
		t.Fatal("tombstoned id 2 must not appear among live events")
	}
	if result.MaxId != 5 {
		t.Fatalf("MaxId = %d, want 5", result.MaxId)
	}
}

func TestAssignEventIdsSkipsExistingTombstonesButFillsPlainHoles(t *testing.T) {
	// 1 是墓碑,2 是从未发过的空洞(墓碑机制之前删掉的旧号,已无从追溯)。
	file := mustLoadEventIdFile(t, "0=A\n1=reserved:Old\n3=C\n")
	events := newProtoEvents("A", "C", "New1", "New2", "New3")

	result := assignEventIds(events, file)

	want := map[string]uint64{"A": 0, "C": 3, "New1": 2, "New2": 4, "New3": 5}
	if got := eventIdsByName(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if len(result.NewlyReserved) != 0 {
		t.Fatalf("NewlyReserved = %v, want none", result.NewlyReserved)
	}
	if result.Reserved[1] != "Old" {
		t.Fatalf("existing tombstone 1 must be kept, Reserved = %v", result.Reserved)
	}
}

func TestAssignEventIdsGivesReaddedNameANewId(t *testing.T) {
	file := mustLoadEventIdFile(t, "0=A\n1=reserved:B\n")
	events := newProtoEvents("A", "B")

	result := assignEventIds(events, file)

	if got := eventIdsByName(events)["B"]; got != 2 {
		t.Fatalf("re-added B got id %d, want fresh id 2", got)
	}
	if result.Reserved[1] != "B" {
		t.Fatalf("tombstone 1 must stay, Reserved = %v", result.Reserved)
	}
}

func TestAssignEventIdsNeverHandsOutTombstoneZero(t *testing.T) {
	// 改前的 InitEventId 有个"空表从 0 开始"的特判:表里只剩墓碑 0 时会把 0 再发出去。
	file := mustLoadEventIdFile(t, "0=reserved:Gone\n")
	events := newProtoEvents("New")

	result := assignEventIds(events, file)

	if got := events[0].Id; got != 1 {
		t.Fatalf("New got id %d, want 1", got)
	}
	if result.MaxId != 1 {
		t.Fatalf("MaxId = %d, want 1", result.MaxId)
	}
}

func TestAssignEventIdsFromEmptyFileStartsAtZero(t *testing.T) {
	events := newProtoEvents("X", "Y")

	result := assignEventIds(events, mustLoadEventIdFile(t, ""))

	want := map[string]uint64{"X": 0, "Y": 1}
	if got := eventIdsByName(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if result.MaxId != 1 || len(result.Reserved) != 0 {
		t.Fatalf("MaxId = %d Reserved = %v, want 1 and none", result.MaxId, result.Reserved)
	}
}

func TestRenderEventIdFileWritesTombstonesBackVerbatim(t *testing.T) {
	const content = "0=A\n1=reserved:Old\n2=B\n"
	file := mustLoadEventIdFile(t, content)
	events := newProtoEvents("A", "B")

	result := assignEventIds(events, file)
	rendered := renderEventIdFile(result.Infos, result.Reserved)

	if rendered != content {
		t.Fatalf("render must be byte-identical to input:\n got: %q\nwant: %q", rendered, content)
	}
}

// D67 的实际场景:删掉 gate_event.proto 里的 BindBattleEvent / UnbindBattleEvent 之后,
// 43/44 永久保留;C++ 注册表长度不变;下一个新事件拿 49 而不是 43;再跑一遍是不动点。
func TestAssignEventIdsBattleBindingRemoval(t *testing.T) {
	tail := []string{
		"ContractsKafkaBindBattleEvent",   // 43
		"ContractsKafkaUnbindBattleEvent", // 44
		"BattleConfirmedEvent",            // 45
		"ContractsKafkaBattleResultEvent", // 46
		"ContractsKafkaBattleResultTeam",  // 47
		"PlayerTeamRefreshEvent",          // 48
	}
	var builder strings.Builder
	liveNames := make([]string, 0, 50)
	for id := 0; id < 43; id++ {
		name := fmt.Sprintf("Event%02d", id)
		builder.WriteString(fmt.Sprintf("%d=%s\n", id, name))
		liveNames = append(liveNames, name)
	}
	for i, name := range tail {
		builder.WriteString(fmt.Sprintf("%d=%s\n", 43+i, name))
		if name != "ContractsKafkaBindBattleEvent" && name != "ContractsKafkaUnbindBattleEvent" {
			liveNames = append(liveNames, name)
		}
	}

	// 第一轮:只删两条消息,不加新事件。
	firstEvents := newProtoEvents(liveNames...)
	first := assignEventIds(firstEvents, mustLoadEventIdFile(t, builder.String()))

	if !reflect.DeepEqual(first.NewlyReserved, []uint64{43, 44}) {
		t.Fatalf("NewlyReserved = %v, want [43 44]", first.NewlyReserved)
	}
	if first.MaxId+1 != 49 {
		t.Fatalf("registry length = %d, want unchanged 49 (kMaxEventCount)", first.MaxId+1)
	}
	for _, event := range firstEvents {
		if event.Id == 43 || event.Id == 44 {
			t.Fatalf("live event %s must not hold a tombstoned id %d", event.IdName, event.Id)
		}
	}
	rendered := renderEventIdFile(first.Infos, first.Reserved)
	for _, wantLine := range []string{
		"43=reserved:ContractsKafkaBindBattleEvent\n",
		"44=reserved:ContractsKafkaUnbindBattleEvent\n",
		"45=BattleConfirmedEvent\n",
	} {
		if !strings.Contains(rendered, wantLine) {
			t.Fatalf("rendered file lacks %q:\n%s", wantLine, rendered)
		}
	}

	// 第二轮:读回第一轮写出的文件,再加一个新事件 —— 墓碑不回收,新号接在最大号之后;
	// 其余号与第一轮完全一致,且没有新的墓碑。
	secondEvents := newProtoEvents(append(liveNames, "ContractsKafkaSomethingNewEvent")...)
	second := assignEventIds(secondEvents, mustLoadEventIdFile(t, rendered))

	secondIds := eventIdsByName(secondEvents)
	if got := secondIds["ContractsKafkaSomethingNewEvent"]; got != 49 {
		t.Fatalf("new event got id %d, want 49", got)
	}
	for name, id := range eventIdsByName(firstEvents) {
		if secondIds[name] != id {
			t.Fatalf("event %s moved from %d to %d", name, id, secondIds[name])
		}
	}
	if len(second.NewlyReserved) != 0 {
		t.Fatalf("second run NewlyReserved = %v, want none", second.NewlyReserved)
	}
	if second.Reserved[43] != "ContractsKafkaBindBattleEvent" || second.Reserved[44] != "ContractsKafkaUnbindBattleEvent" {
		t.Fatalf("tombstones lost on second run: %v", second.Reserved)
	}
}

// liveEventIdFile 生成 `0=Event0 .. n-1=Event<n-1>` 全活号的 event_id.txt 内容及对应事件名。
func liveEventIdFile(n int) (string, []string) {
	var builder strings.Builder
	names := make([]string, 0, n)
	for id := 0; id < n; id++ {
		name := fmt.Sprintf("Event%d", id)
		builder.WriteString(fmt.Sprintf("%d=%s\n", id, name))
		names = append(names, name)
	}
	return builder.String(), names
}

// 事件目录配空 / 指错到空目录:本轮 proto 一个事件都没读到,不能把全部活号转墓碑。
func TestCheckEventIdInputCompleteRejectsEmptyProtoInput(t *testing.T) {
	content, _ := liveEventIdFile(10)
	file := mustLoadEventIdFile(t, content)
	result := assignEventIds(newProtoEvents(), file)

	err := checkEventIdInputComplete(file, result, true, false)
	if err == nil {
		t.Fatal("tombstoning all 10 live ids from an empty proto input must be rejected")
	}
	if !strings.Contains(err.Error(), "0=Event0") || !strings.Contains(err.Error(), "9=Event9") {
		t.Fatalf("error must list the ids and names about to be tombstoned, got: %v", err)
	}

	if err := checkEventIdInputComplete(file, result, true, true); err != nil {
		t.Fatalf("explicit mass-tombstone override must pass, got: %v", err)
	}
}

func TestCheckEventIdInputCompleteTombstoneLimitBoundary(t *testing.T) {
	content, names := liveEventIdFile(10)
	cases := []struct {
		removed int
		wantErr bool
	}{
		{removed: 0, wantErr: false},
		{removed: 2, wantErr: false}, // D67:删 Bind/UnbindBattleEvent
		{removed: maxNewTombstonesPerRun, wantErr: false},
		{removed: maxNewTombstonesPerRun + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("removed=%d", tc.removed), func(t *testing.T) {
			file := mustLoadEventIdFile(t, content)
			result := assignEventIds(newProtoEvents(names[tc.removed:]...), file)
			if len(result.NewlyReserved) != tc.removed {
				t.Fatalf("NewlyReserved = %v, want %d ids", result.NewlyReserved, tc.removed)
			}

			err := checkEventIdInputComplete(file, result, true, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkEventIdInputComplete() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// contracts_kafka 没配:event_id.txt 里只要还有 Kafka 活事件就中止,且不受批量放行开关影响。
func TestCheckEventIdInputCompleteRejectsUnconfiguredKafkaDir(t *testing.T) {
	file := mustLoadEventIdFile(t, "0=A\n1=ContractsKafkaSomeEvent\n")
	result := assignEventIds(newProtoEvents("A"), file)

	err := checkEventIdInputComplete(file, result, false, true)
	if err == nil {
		t.Fatal("unconfigured contracts_kafka with live Kafka events must be rejected even with the override")
	}
	if !strings.Contains(err.Error(), "1=ContractsKafkaSomeEvent") {
		t.Fatalf("error must name the live Kafka event, got: %v", err)
	}

	// 只剩 Kafka 墓碑、没有 Kafka 活事件:没什么会被误转,放行。
	tombstonedOnly := mustLoadEventIdFile(t, "0=A\n1=reserved:ContractsKafkaOldEvent\n")
	if err := checkEventIdInputComplete(tombstonedOnly, assignEventIds(newProtoEvents("A"), tombstonedOnly), false, false); err != nil {
		t.Fatalf("Kafka tombstones alone must not block generation, got: %v", err)
	}
}

func TestEventIdLenCountsTombstonedIds(t *testing.T) {
	originalInfos := eventIdInfoMap
	originalReserved := reservedEventIdMap
	originalMaxID := eventFileMaxId
	t.Cleanup(func() {
		eventIdInfoMap = originalInfos
		reservedEventIdMap = originalReserved
		eventFileMaxId = originalMaxID
	})

	// 最大号本身是墓碑:数组仍须覆盖它,旧节点发来的该号消息才会落到 default 分支被丢弃,
	// 而不是被 IsValidEventId 判成越界。
	result := assignEventIds(newProtoEvents("A"), mustLoadEventIdFile(t, "0=A\n1=Gone\n"))
	eventIdInfoMap = result.Infos
	reservedEventIdMap = result.Reserved
	eventFileMaxId = result.MaxId

	if got := EventIdLen(); got != 2 {
		t.Fatalf("EventIdLen() = %d, want 2", got)
	}
}
