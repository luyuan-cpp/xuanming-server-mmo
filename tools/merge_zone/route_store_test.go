package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoRouteTableWritesOutsideMergeRouteStore:本工具对路由表的写只有 zone_merged_into 的插入 / 删除与
// zone_merge_history 的追加,且都在 route_store.go;历史只增不改,任何地方都不许改写或删除它。
// data_service 的 route_store.go 文件头引用这条用例的名字,改名要同步。
func TestNoRouteTableWritesOutsideMergeRouteStore(t *testing.T) {
	const verbs = "(?i)(INSERT\\s+INTO|UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?"
	routeWrite := regexp.MustCompile(verbs + "zone_merged_into\\b")
	historyAppend := regexp.MustCompile("(?i)INSERT\\s+INTO\\s+`?zone_merge_history\\b")
	neverWrite := regexp.MustCompile(verbs + "(player_route|route_meta|guild|trade_listing)\\b")
	historyRewrite := regexp.MustCompile("(?i)(UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?zone_merge_history\\b")
	const allowed = "route_store.go"

	var files, allowedHits, historyHits int
	var offenders []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		rel := filepath.ToSlash(path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if neverWrite.MatchString(s) {
				offenders = append(offenders, rel+" (never written by merge_zone): "+s)
			}
			if historyRewrite.MatchString(s) {
				offenders = append(offenders, rel+" (zone_merge_history is append-only): "+s)
			}
			if routeWrite.MatchString(s) {
				if rel == allowed {
					allowedHits++
				} else {
					offenders = append(offenders, rel+": "+s)
				}
			}
			if historyAppend.MatchString(s) {
				if rel == allowed {
					historyHits++
				} else {
					offenders = append(offenders, rel+": "+s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 3 {
		t.Fatalf("scanned only %d files: the walk is broken", files)
	}
	if allowedHits != 2 {
		t.Errorf("route_store.go has %d zone_merged_into writes, want exactly the INSERT and the DELETE", allowedHits)
	}
	if historyHits != 1 {
		t.Errorf("route_store.go has %d zone_merge_history appends, want exactly the one INSERT both Insert and Delete go through", historyHits)
	}
	if len(offenders) != 0 {
		t.Errorf("route table writes outside route_store.go:\n%s", strings.Join(offenders, "\n"))
	}
}

// 这几条 SQL 的形状是互斥的依据:判定读必须持锁,按 (src, dst) 删,插入用库时钟;
// 历史的序号是加锁读出来的,操作员取库侧 CURRENT_USER()。
func TestRouteStatementsShape(t *testing.T) {
	for name, tc := range map[string]struct {
		query string
		want  string
	}{
		"route_meta 锁":  {routeMetaLockSQL, "FROM route_meta WHERE id = 1 FOR UPDATE"},
		"映射行锁":          {mappingLockSQL, "WHERE src_zone = ? FOR UPDATE"},
		"删除按 (src,dst)": {mappingDeleteSQL, "WHERE src_zone = ? AND dst_zone = ?"},
		"插入用库时钟":        {mappingInsertSQL, "NOW(3)"},
		"列表按 src 排序":    {mappingListSQL, "ORDER BY src_zone"},
		"撤销前加锁读 run_id": {mappingRunIDSQL, "WHERE src_zone = ? AND dst_zone = ? FOR UPDATE"},
		"历史序号加锁读":       {historyLastSeqSQL, "ORDER BY event_seq DESC LIMIT 1 FOR UPDATE"},
		"历史记库侧身份":       {historyInsertSQL, "CURRENT_USER()"},
		"历史用库时钟":        {historyInsertSQL, "NOW(3)"},
		"历史按时间列出":       {historyListSQL, "ORDER BY at_ms, src_zone, event_seq"},
	} {
		if !strings.Contains(tc.query, tc.want) {
			t.Errorf("%s: %q does not contain %q", name, tc.query, tc.want)
		}
	}
	if strings.Contains(routeMetaReadSQL, "FOR UPDATE") {
		t.Errorf("dry-run 和审计的读不能加锁: %q", routeMetaReadSQL)
	}
}
