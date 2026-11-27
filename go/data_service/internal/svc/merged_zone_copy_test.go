package svc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"data_service/internal/store"

	"shared/authoritysql"
	"shared/mergedzonecopy"
)

// 副本键、编码、真 etcd 上的 CAS 用例在 shared/mergedzonecopy;这里只测 data_service 这一侧的读法。

// testMergedZoneMapping 造一份映射;pairs 依次是源区、目标区。
func testMergedZoneMapping(generation string, version uint64, pairs ...uint32) store.MergedZoneMapping {
	merges := make(map[uint32]uint32, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		merges[pairs[i]] = pairs[i+1]
	}
	return store.MergedZoneMapping{Generation: generation, Version: version, Merges: merges}
}

func cloneMergedZoneMapping(m store.MergedZoneMapping) store.MergedZoneMapping {
	m.Merges = maps.Clone(m.Merges)
	return m
}

// errShardZeroUnreachable 是第 0 片的池启动时连不上、至今关着时 RouteStore 回的那种错误。
var errShardZeroUnreachable = fmt.Errorf("read zone_merged_into: %w", authoritysql.ErrUnreachable)

// fakeMergedZoneTruth 是目录第 0 片的替身。
type fakeMergedZoneTruth struct {
	mapping store.MergedZoneMapping
	err     error
	reads   int
}

func (f *fakeMergedZoneTruth) ReadMergedZoneMapping(context.Context) (store.MergedZoneMapping, error) {
	f.reads++
	if f.err != nil {
		return store.MergedZoneMapping{}, f.err
	}
	return cloneMergedZoneMapping(f.mapping), nil
}

// fakeMergedZoneCopies 是 etcd 副本的替身:revision 0 = 没有键,每写一次加 1;Put 按 revision 比较,和 etcd 的 ModRevision 同一语义。
type fakeMergedZoneCopies struct {
	value    store.MergedZoneMapping
	revision int64
	getErr   error
	putErr   error
	// landThenErr 非 nil 时,Put 照常写进去,再回这个错误:模拟请求落地了、应答丢了。
	landThenErr error
	// racer 在每次 Put 比较 revision 之前调用,模拟别的副本抢先写了一次。
	racer func(*fakeMergedZoneCopies)
	puts  int
}

func (f *fakeMergedZoneCopies) Key() string { return mergedzonecopy.KeyPrefix + "testdb" }

func (f *fakeMergedZoneCopies) Get(context.Context) (store.MergedZoneMapping, int64, bool, error) {
	if f.getErr != nil {
		return store.MergedZoneMapping{}, 0, false, f.getErr
	}
	if f.revision == 0 {
		return store.MergedZoneMapping{}, 0, false, nil
	}
	return cloneMergedZoneMapping(f.value), f.revision, true, nil
}

func (f *fakeMergedZoneCopies) Put(_ context.Context, revision int64, mapping store.MergedZoneMapping) (bool, error) {
	f.puts++
	if f.racer != nil {
		f.racer(f)
	}
	if f.putErr != nil {
		return false, f.putErr
	}
	if revision != f.revision {
		return false, nil
	}
	f.value, f.revision = cloneMergedZoneMapping(mapping), f.revision+1
	if f.landThenErr != nil {
		return false, f.landThenErr
	}
	return true, nil
}

// set 换一份副本,revision 跟着变(etcd 里每写一次 ModRevision 都会变)。
func (f *fakeMergedZoneCopies) set(mapping store.MergedZoneMapping) {
	f.value, f.revision = mapping, f.revision+1
}

func newTestMergedZoneSource(truth mergedZoneTruth, copies mergedzonecopy.Copies) *mergedZoneSource {
	return &mergedZoneSource{truth: truth, copies: copies}
}

// 先发布后回答:没有副本时先建;版本相同不再写;版本变大按读到的 revision 覆盖;发布失败时这一拍不回答新版本。
func TestMergedZoneSourcePublishesBeforeAnswering(t *testing.T) {
	ctx := context.Background()
	truth := &fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 1, 5, 6)}
	copies := &fakeMergedZoneCopies{}
	source := newTestMergedZoneSource(truth, copies)

	got, err := source.read(ctx)
	if err != nil || !maps.Equal(got, map[uint32]uint32{5: 6}) {
		t.Fatalf("read = (%v, %v)", got, err)
	}
	if copies.puts != 1 || copies.revision != 1 || copies.value.Version != 1 || copies.value.Generation != "g1" {
		t.Fatalf("没有副本时要先建副本再回答: %+v revision=%d puts=%d", copies.value, copies.revision, copies.puts)
	}
	if _, err := source.read(ctx); err != nil || copies.puts != 1 {
		t.Fatalf("版本相同时不再写: err=%v puts=%d", err, copies.puts)
	}

	truth.mapping = testMergedZoneMapping("g1", 2, 5, 6, 7, 6)
	got, err = source.read(ctx)
	if err != nil || !maps.Equal(got, map[uint32]uint32{5: 6, 7: 6}) || copies.puts != 2 || copies.value.Version != 2 {
		t.Fatalf("版本变大时覆盖副本再回答: (%v, %v) %+v puts=%d", got, err, copies.value, copies.puts)
	}

	truth.mapping = testMergedZoneMapping("g1", 3, 5, 6, 7, 6, 8, 6)
	copies.putErr = errors.New("etcd request timed out")
	if _, err := source.read(ctx); err == nil || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("发布失败时这一拍必须失败: %v", err)
	}
	if source.answered == nil || source.answered.Version != 2 {
		t.Fatalf("发布没成之前不能把版本 3 记成已回答: %+v", source.answered)
	}
}

// 发布的请求落地了、应答丢了:这一拍失败,下一拍读到同一版本、同一内容就直接回答,不再写。
func TestMergedZoneSourceHealsAPublishWhoseAnswerWasLost(t *testing.T) {
	ctx := context.Background()
	copies := &fakeMergedZoneCopies{landThenErr: errors.New("etcd request timed out")}
	source := newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 1, 5, 6)}, copies)
	if _, err := source.read(ctx); err == nil || copies.revision != 1 {
		t.Fatalf("应答丢了要算失败,副本里其实已经写进去: err=%v revision=%d", err, copies.revision)
	}
	copies.landThenErr = nil
	got, err := source.read(ctx)
	if err != nil || !maps.Equal(got, map[uint32]uint32{5: 6}) || copies.puts != 1 {
		t.Fatalf("下一拍按已经落地的那一版回答,不再写: (%v, %v) puts=%d", got, err, copies.puts)
	}
}

// 第 0 片和副本对不上时谁对拿不准:落后于副本(回档、清表、丢提交)、同版本内容不同(手改映射)、副本是别的目录库的(指错库或库重建过),
// 都失败,也不写副本。删掉副本键之后,新进程按第 0 片重新发布。
func TestMergedZoneSourceRefusesWhenShardZeroDisagreesWithTheCopy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		truth     store.MergedZoneMapping
		published store.MergedZoneMapping
		want      string
	}{
		{"第 0 片落后于副本", testMergedZoneMapping("g1", 4), testMergedZoneMapping("g1", 5, 5, 6), "behind version 5"},
		{"同版本内容不同", testMergedZoneMapping("g1", 4, 5, 7), testMergedZoneMapping("g1", 4, 5, 6), "different contents"},
		{"副本是别的目录库的", testMergedZoneMapping("new", 0), testMergedZoneMapping("old", 9, 5, 6), "another directory database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copies := &fakeMergedZoneCopies{value: tc.published, revision: 9}
			source := newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: tc.truth}, copies)
			_, err := source.read(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), copies.Key()) {
				t.Fatalf("必须失败并点名副本键 %s: %v", copies.Key(), err)
			}
			if copies.puts != 0 || copies.revision != 9 || source.answered != nil {
				t.Fatalf("对不上时不写副本、不回答: puts=%d revision=%d answered=%+v", copies.puts, copies.revision, source.answered)
			}

			// 运维确认以第 0 片为准:停掉全部 data_service、删副本键、再起。
			copies.value, copies.revision = store.MergedZoneMapping{}, 0
			restarted := newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: tc.truth}, copies)
			if _, err := restarted.read(ctx); err != nil || copies.value.Generation != tc.truth.Generation || copies.value.Version != tc.truth.Version {
				t.Fatalf("删键之后按第 0 片重新发布: err=%v %+v", err, copies.value)
			}
		})
	}
}

// 同一个进程回答过的版本只增不减、同一个版本只对应一份映射、世代不变:第 0 片回档后有人在副本还在跑时删了副本键,
// 这个进程不能跟着退回去,也不能把同号不同内容的新版本当成同一份;读副本那条路上同样守住。
func TestMergedZoneSourceNeverGoesBackWithinAProcess(t *testing.T) {
	ctx := context.Background()
	truth := &fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 9, 5, 6)}
	copies := &fakeMergedZoneCopies{}
	source := newTestMergedZoneSource(truth, copies)
	if _, err := source.read(ctx); err != nil {
		t.Fatal(err)
	}

	copies.value, copies.revision = store.MergedZoneMapping{}, 0
	truth.mapping = testMergedZoneMapping("g1", 8)
	if _, err := source.read(ctx); err == nil || !strings.Contains(err.Error(), "behind version 9 that this process already answers with") ||
		!strings.Contains(err.Error(), "restart this replica") {
		t.Fatalf("第 0 片退回去时这个进程不跟着退: %v", err)
	}
	if copies.puts != 1 || copies.revision != 0 {
		t.Fatalf("退回去的版本不能发布: puts=%d revision=%d", copies.puts, copies.revision)
	}

	truth.mapping = testMergedZoneMapping("g1", 9, 7, 6)
	if _, err := source.read(ctx); err == nil || !strings.Contains(err.Error(), "contents different from what this process answers with") {
		t.Fatalf("同号不同内容的另一条历史必须拒绝: %v", err)
	}
	truth.mapping = testMergedZoneMapping("g2", 9, 5, 6)
	if _, err := source.read(ctx); err == nil || !strings.Contains(err.Error(), "generation g2") {
		t.Fatalf("第 0 片换了一个库必须拒绝: %v", err)
	}

	truth.err = errShardZeroUnreachable
	copies.set(testMergedZoneMapping("g1", 9, 7, 6))
	if _, err := source.read(ctx); !errors.Is(err, authoritysql.ErrUnreachable) || !strings.Contains(err.Error(), "contents different") {
		t.Fatalf("读副本时同号不同内容同样拒绝: %v", err)
	}
	if source.answered.Version != 9 || !maps.Equal(source.answered.Merges, map[uint32]uint32{5: 6}) {
		t.Fatalf("已回答的那一份不变: %+v", source.answered)
	}
}

// 第 0 片连不上(池关着,或这一代缺它的写资格):启动用副本;之后只接受副本里更新的版本,同版本照旧回答但记失败,
// 更旧的、别的世代的、没有副本都失败;这条路上从不写副本。第 0 片恢复后回到真源。
func TestMergedZoneSourceStartsFromTheCopyWhenShardZeroIsUnreadable(t *testing.T) {
	ctx := context.Background()
	noGrant := fmt.Errorf("read zone_merged_into: logical database testdb_00: %w", store.ErrNoWriterGrant)
	for _, startErr := range []error{errShardZeroUnreachable, noGrant} {
		started := newTestMergedZoneSource(&fakeMergedZoneTruth{err: startErr}, &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 3, 5, 6), revision: 7})
		if got, err := started.read(ctx); err != nil || !maps.Equal(got, map[uint32]uint32{5: 6}) {
			t.Fatalf("%v: 启动时从副本起: (%v, %v)", startErr, got, err)
		}
	}

	truth := &fakeMergedZoneTruth{err: errShardZeroUnreachable}
	copies := &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 3, 5, 6), revision: 7}
	source := newTestMergedZoneSource(truth, copies)
	if _, err := source.read(ctx); err != nil || !source.fromCopy {
		t.Fatalf("启动时从副本起: err=%v fromCopy=%t", err, source.fromCopy)
	}
	if _, err := source.read(ctx); !errors.Is(err, authoritysql.ErrUnreachable) {
		t.Fatalf("同版本照旧回答,但这一拍要算失败并带出第 0 片的错误: %v", err)
	}

	copies.set(testMergedZoneMapping("g1", 4, 5, 6, 7, 6))
	got, err := source.read(ctx)
	if err != nil || !maps.Equal(got, map[uint32]uint32{5: 6, 7: 6}) {
		t.Fatalf("副本里有更新的版本(别的副本发布的)就采用: (%v, %v)", got, err)
	}

	copies.set(testMergedZoneMapping("g1", 2, 5, 6))
	if _, err := source.read(ctx); !errors.Is(err, authoritysql.ErrUnreachable) || !strings.Contains(err.Error(), "behind version 4") {
		t.Fatalf("副本比已回答的旧(etcd 被回档)必须失败: %v", err)
	}
	copies.set(testMergedZoneMapping("g2", 9, 5, 6))
	if _, err := source.read(ctx); !errors.Is(err, authoritysql.ErrUnreachable) || !strings.Contains(err.Error(), "generation g2") {
		t.Fatalf("副本是别的目录库的必须失败: %v", err)
	}
	if copies.puts != 0 || source.answered.Version != 4 {
		t.Fatalf("读不到第 0 片时从不写副本,已回答停在 4: puts=%d answered=%+v", copies.puts, source.answered)
	}

	truth.err, truth.mapping = nil, testMergedZoneMapping("g1", 4, 5, 6, 7, 6)
	copies.set(testMergedZoneMapping("g1", 4, 5, 6, 7, 6))
	if _, err := source.read(ctx); err != nil || source.fromCopy {
		t.Fatalf("第 0 片恢复后回到真源: err=%v fromCopy=%t", err, source.fromCopy)
	}

	empty := newTestMergedZoneSource(&fakeMergedZoneTruth{err: errShardZeroUnreachable}, &fakeMergedZoneCopies{})
	if _, err := empty.read(ctx); !errors.Is(err, authoritysql.ErrUnreachable) || !strings.Contains(err.Error(), "no copy") {
		t.Fatalf("第 0 片读不到、也没有副本时必须失败: %v", err)
	}
}

// 第 0 片连得上却读不对(权限、缺表、语句错):启动时不拿副本顶替,照旧拒启,滚动发布停在第一个副本。
// 已经在跑的副本照旧可以采用副本里更新的版本(那是别的副本从第 0 片读到并发布的),其余各拍都算失败。
func TestMergedZoneSourceDoesNotStartFromTheCopyOnADeterministicError(t *testing.T) {
	ctx := context.Background()
	denied := errors.New("Error 1142 (42000): SELECT command denied to user for table 'zone_merge_history'")
	truth := &fakeMergedZoneTruth{err: fmt.Errorf("read zone_merged_into: %w", denied)}
	copies := &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 3, 5, 6), revision: 7}
	source := newTestMergedZoneSource(truth, copies)
	if _, err := source.read(ctx); !errors.Is(err, denied) || !strings.Contains(err.Error(), "not a connectivity failure") {
		t.Fatalf("启动时确定性的错误不能拿副本顶替: %v", err)
	}
	if source.answered != nil {
		t.Fatalf("拒启时什么都没回答: %+v", source.answered)
	}

	truth.err, truth.mapping = nil, testMergedZoneMapping("g1", 3, 5, 6)
	if _, err := source.read(ctx); err != nil {
		t.Fatal(err)
	}
	truth.err = fmt.Errorf("read zone_merged_into: %w", denied)
	copies.set(testMergedZoneMapping("g1", 4, 5, 6, 7, 6))
	if got, err := source.read(ctx); err != nil || !maps.Equal(got, map[uint32]uint32{5: 6, 7: 6}) {
		t.Fatalf("在跑的副本照旧采用副本里更新的版本: (%v, %v)", got, err)
	}
	if _, err := source.read(ctx); !errors.Is(err, denied) {
		t.Fatalf("没有更新的版本时这一拍算失败,带出第 0 片的错误: %v", err)
	}
}

// 发布撞上别的副本:重读,发现对方已经发布了同一个版本就不再写;一直被抢就按次数放弃。
func TestMergedZoneSourceRetriesAPublishRace(t *testing.T) {
	ctx := context.Background()
	truth := &fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 2, 5, 6)}
	copies := &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 1), revision: 3}
	copies.racer = func(f *fakeMergedZoneCopies) {
		f.value, f.revision, f.racer = testMergedZoneMapping("g1", 2, 5, 6), f.revision+1, nil
	}
	source := newTestMergedZoneSource(truth, copies)
	got, err := source.read(ctx)
	if err != nil || !maps.Equal(got, map[uint32]uint32{5: 6}) || copies.puts != 1 || truth.reads != 2 {
		t.Fatalf("输一次之后重读,对方已发布同一版本就直接回答: (%v, %v) puts=%d reads=%d", got, err, copies.puts, truth.reads)
	}

	truth.mapping = testMergedZoneMapping("g1", 3, 5, 6, 7, 6)
	copies.racer = func(f *fakeMergedZoneCopies) { f.revision++ }
	if _, err := source.read(ctx); err == nil || copies.puts != 1+mergedzonecopy.PublishAttempts {
		t.Fatalf("一直被抢时按次数放弃: err=%v puts=%d", err, copies.puts)
	}
	if source.answered.Version != 2 {
		t.Fatalf("放弃时不回答新版本: %+v", source.answered)
	}
}

// 解析不通的映射(成环)与写不进副本的映射(区号 0:zone_merged_into 没有这条 CHECK)既不发布也不回答。
func TestMergedZoneSourceNeverPublishesABrokenMapping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		truth store.MergedZoneMapping
		want  string
	}{
		{"成环", testMergedZoneMapping("g1", 2, 5, 6, 6, 5), "broken"},
		{"区号 0", testMergedZoneMapping("g1", 2, 0, 6), "cannot be published"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copies := &fakeMergedZoneCopies{value: testMergedZoneMapping("g1", 1, 5, 6), revision: 2}
			source := newTestMergedZoneSource(&fakeMergedZoneTruth{mapping: tc.truth}, copies)
			if _, err := source.read(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("必须失败: %v", err)
			}
			if copies.puts != 0 || copies.value.Version != 1 {
				t.Fatalf("坏映射不能进副本: puts=%d %+v", copies.puts, copies.value)
			}
		})
	}
}

// 副本读不到时,第 0 片读得到也不回答:不知道别人回答过什么,也就发布不了。副本先读,第 0 片一次都不读。
func TestMergedZoneSourceFailsWithoutTheCopy(t *testing.T) {
	truth := &fakeMergedZoneTruth{mapping: testMergedZoneMapping("g1", 1, 5, 6)}
	etcdDown := errors.New("etcd unavailable")
	source := newTestMergedZoneSource(truth, &fakeMergedZoneCopies{getErr: etcdDown})
	if _, err := source.read(context.Background()); !errors.Is(err, etcdDown) {
		t.Fatalf("副本读不到必须失败: %v", err)
	}
	if truth.reads != 0 {
		t.Fatalf("副本要先读:读副本失败时不该去读第 0 片,读了 %d 次", truth.reads)
	}
}
