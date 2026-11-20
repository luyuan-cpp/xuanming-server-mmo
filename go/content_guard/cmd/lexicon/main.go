// lexicon:内容安全词库的发布工具(docs/design/rename-and-content-safety.md §6.4)。
//
// 词库放在 etcd,不进 git。本工具直接读写 etcd:能连上 etcd 就能发布,所以它的权限就是 etcd 的权限,
// 服务本身没有发布接口。发布之后各副本在几秒内自动换上新词库,不用重启。
//
//	lexicon publish  -by <发布人> -deny <类别>=<文件> [-deny ...] [-allow <文件>] [-dry-run]
//	lexicon add      -by <发布人> -category <类别> -word <词> [-mode sub|token|prefix]
//	lexicon add      -by <发布人> -allow -word <白名单词>
//	lexicon remove   -by <发布人> -word <词> [-allow]
//	lexicon rollback -by <发布人> -to <版本号>
//	lexicon show
//
// 公共参数:-etcd(逗号分隔的地址,缺省 127.0.0.1:2379)、-prefix(与服务配置 LexiconPrefix 相同,缺省同服务)、-timeout。
// 类别:politics、porn、abuse、ad、impersonation、other。
// 词文件:一行一个词;空行和 # 开头的行忽略;词后面可以用制表符接一个匹配方式,不写按 wordlist.DefaultMode。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"content_guard/internal/lexstore"
	"content_guard/internal/textnorm"
	"content_guard/internal/wordlist"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"publish":  runPublish,
		"add":      runAdd,
		"remove":   runRemove,
		"rollback": runRollback,
		"show":     runShow,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "lexicon:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lexicon <publish|add|remove|rollback|show> [flags]   (run a command with -h for its flags)")
}

// target 是每个子命令都有的连接参数。
type target struct {
	etcd    string
	prefix  string
	timeout time.Duration
}

func (t *target) register(fs *flag.FlagSet) {
	fs.StringVar(&t.etcd, "etcd", "127.0.0.1:2379", "comma-separated etcd endpoints")
	fs.StringVar(&t.prefix, "prefix", lexstore.DefaultPrefix, "lexicon prefix in etcd; must equal the service's LexiconPrefix")
	fs.DurationVar(&t.timeout, "timeout", 60*time.Second, "overall time limit")
}

func (t *target) connect() (*clientv3.Client, context.Context, context.CancelFunc, error) {
	if !strings.HasSuffix(t.prefix, "/") {
		return nil, nil, nil, fmt.Errorf("-prefix %q must end with a slash", t.prefix)
	}
	var endpoints []string
	for _, endpoint := range strings.Split(t.etcd, ",") {
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return nil, nil, nil, errors.New("-etcd is empty")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect etcd: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	return cli, ctx, cancel, nil
}

// repeated 是可以重复给的字符串参数。
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func runPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	var t target
	t.register(fs)
	var deny, allow repeated
	fs.Var(&deny, "deny", "<category>=<file>, repeatable; one word per line")
	fs.Var(&allow, "allow", "allow-list file, repeatable; one word per line")
	by := fs.String("by", "", "publisher name (required, recorded in the pointer)")
	dryRun := fs.Bool("dry-run", false, "compile and print the statistics without touching etcd")
	_ = fs.Parse(args)

	lex := &wordlist.Lexicon{}
	for _, spec := range deny {
		name, path, ok := strings.Cut(spec, "=")
		if !ok {
			return fmt.Errorf("-deny %q must be <category>=<file>", spec)
		}
		category, known := wordlist.ParseCategory(name)
		if !known {
			return fmt.Errorf("-deny %q: unknown category %q", spec, name)
		}
		entries, err := readDenyFile(path, category)
		if err != nil {
			return err
		}
		lex.Deny = append(lex.Deny, entries...)
	}
	for _, path := range allow {
		words, err := readAllowFile(path)
		if err != nil {
			return err
		}
		lex.Allow = append(lex.Allow, words...)
	}
	if len(lex.Deny) == 0 {
		return errors.New("nothing to publish: give at least one -deny <category>=<file> with words in it")
	}
	if *dryRun {
		_, err := encode(lex)
		return err
	}
	return publish(&t, lex, *by)
}

func runAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	var t target
	t.register(fs)
	by := fs.String("by", "", "publisher name (required)")
	word := fs.String("word", "", "the word to add (required)")
	categoryName := fs.String("category", "", "category of a deny word")
	modeName := fs.String("mode", "", "sub | token | prefix; default depends on the word")
	toAllow := fs.Bool("allow", false, "add to the allow list instead of the deny list")
	_ = fs.Parse(args)

	if err := wordlist.ValidateWord(*word); err != nil {
		return err
	}
	lex, err := loadCurrent(&t)
	if err != nil {
		return err
	}
	if *toAllow {
		lex.Allow = append(lex.Allow, *word)
		return publish(&t, lex, *by)
	}
	category, known := wordlist.ParseCategory(*categoryName)
	if !known {
		return fmt.Errorf("-category %q is not one of politics, porn, abuse, ad, impersonation, other", *categoryName)
	}
	mode := wordlist.DefaultMode(*word)
	if *modeName != "" {
		parsed, ok := wordlist.ParseMode(*modeName)
		if !ok {
			return fmt.Errorf("-mode %q is not one of sub, token, prefix", *modeName)
		}
		mode = parsed
	}
	lex.Deny = append(lex.Deny, wordlist.Entry{Word: *word, Category: category, Mode: mode})
	return publish(&t, lex, *by)
}

func runRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	var t target
	t.register(fs)
	by := fs.String("by", "", "publisher name (required)")
	word := fs.String("word", "", "the word to remove (required); compared after folding, so width / case / script variants match")
	fromAllow := fs.Bool("allow", false, "remove from the allow list instead of the deny list")
	_ = fs.Parse(args)

	want := string(textnorm.FoldWord(*word))
	if want == "" {
		return errors.New("-word is empty after folding")
	}
	lex, err := loadCurrent(&t)
	if err != nil {
		return err
	}
	removed := 0
	if *fromAllow {
		kept := lex.Allow[:0]
		for _, w := range lex.Allow {
			if string(textnorm.FoldWord(w)) == want {
				removed++
				continue
			}
			kept = append(kept, w)
		}
		lex.Allow = kept
	} else {
		kept := lex.Deny[:0]
		for _, e := range lex.Deny {
			if string(textnorm.FoldWord(e.Word)) == want {
				removed++
				continue
			}
			kept = append(kept, e)
		}
		lex.Deny = kept
	}
	if removed == 0 {
		return fmt.Errorf("%q is not in the lexicon (built-in words cannot be removed)", *word)
	}
	fmt.Printf("removing %d entr(ies)\n", removed)
	return publish(&t, lex, *by)
}

func runRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	var t target
	t.register(fs)
	by := fs.String("by", "", "publisher name (required)")
	to := fs.Uint64("to", 0, "the version to bring back (required); it is re-published as a new version")
	_ = fs.Parse(args)
	if *to == 0 {
		return errors.New("-to is required")
	}
	cli, ctx, cancel, err := t.connect()
	if err != nil {
		return err
	}
	defer cancel()
	defer cli.Close()
	text, err := lexstore.ReadVersion(ctx, cli, t.prefix, *to)
	if err != nil {
		return err
	}
	lex, err := wordlist.Parse(text)
	if err != nil {
		return fmt.Errorf("version %d: %w", *to, err)
	}
	return publishWith(ctx, cli, &t, lex, *by)
}

func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	var t target
	t.register(fs)
	_ = fs.Parse(args)
	cli, ctx, cancel, err := t.connect()
	if err != nil {
		return err
	}
	defer cancel()
	defer cli.Close()
	versions, err := lexstore.ListVersions(ctx, cli, t.prefix)
	if err != nil {
		return err
	}
	fmt.Printf("prefix:   %s\nversions: %v (versions that still have chunks in etcd)\n", t.prefix, versions)
	loaded, err := lexstore.Load(ctx, cli, t.prefix)
	if err != nil {
		return fmt.Errorf("current lexicon cannot be loaded: %w", err)
	}
	if loaded.Text == nil {
		fmt.Println("current:  none (no lexicon has been published under this prefix)")
		return nil
	}
	lex, err := wordlist.Parse(loaded.Text)
	if err != nil {
		return fmt.Errorf("current lexicon version %d does not parse: %w", loaded.Pointer.Version, err)
	}
	p := loaded.Pointer
	fmt.Printf("current:  version=%d chunks=%d bytes=%d sha256=%s\n", p.Version, p.Chunks, p.Bytes, p.SHA256)
	fmt.Printf("          published_by=%q published_at=%s\n", p.UpdatedBy, time.UnixMilli(p.UpdatedMs).UTC().Format(time.RFC3339))
	fmt.Printf("          deny=%d allow=%d\n", len(lex.Deny), len(lex.Allow))
	return nil
}

// loadCurrent 读 etcd 里当前的词库;还没有词库时返回空词库(第一次 add 就等于第一次发布)。
func loadCurrent(t *target) (*wordlist.Lexicon, error) {
	cli, ctx, cancel, err := t.connect()
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer cli.Close()
	loaded, err := lexstore.Load(ctx, cli, t.prefix)
	if err != nil {
		return nil, fmt.Errorf("load the current lexicon: %w", err)
	}
	if loaded.Text == nil {
		return &wordlist.Lexicon{}, nil
	}
	return wordlist.Parse(loaded.Text)
}

func publish(t *target, lex *wordlist.Lexicon, by string) error {
	cli, ctx, cancel, err := t.connect()
	if err != nil {
		return err
	}
	defer cancel()
	defer cli.Close()
	return publishWith(ctx, cli, t, lex, by)
}

func publishWith(ctx context.Context, cli *clientv3.Client, t *target, lex *wordlist.Lexicon, by string) error {
	if strings.TrimSpace(by) == "" {
		return errors.New("-by is required")
	}
	text, err := encode(lex)
	if err != nil {
		return err
	}
	pointer, err := lexstore.Publish(ctx, cli, t.prefix, text, len(lex.Deny)+len(lex.Allow), by, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("published version=%d chunks=%d bytes=%d sha256=%s\n", pointer.Version, pointer.Chunks, pointer.Bytes, pointer.SHA256)
	removed, err := lexstore.Prune(ctx, cli, t.prefix, pointer.Version)
	if err != nil {
		// 新版本已经生效;清旧版本失败只是多占一点 etcd 空间,下次发布会再清。
		fmt.Fprintf(os.Stderr, "lexicon: published, but pruning old versions failed: %v\n", err)
		return nil
	}
	if len(removed) > 0 {
		fmt.Printf("pruned versions %v\n", removed)
	}
	return nil
}

// encode 给词库排好序、去掉完全相同的行,写成文本,并编译一遍打印统计。
// 编译只为在发布之前发现问题和给出数字;服务加载时会自己再编译。
func encode(lex *wordlist.Lexicon) ([]byte, error) {
	sort.SliceStable(lex.Deny, func(a, b int) bool {
		if lex.Deny[a].Category != lex.Deny[b].Category {
			return lex.Deny[a].Category < lex.Deny[b].Category
		}
		return lex.Deny[a].Word < lex.Deny[b].Word
	})
	deny := lex.Deny[:0]
	for i, e := range lex.Deny {
		if i > 0 && e == lex.Deny[i-1] {
			continue
		}
		deny = append(deny, e)
	}
	lex.Deny = deny
	sort.Strings(lex.Allow)
	allow := lex.Allow[:0]
	for i, w := range lex.Allow {
		if i > 0 && w == lex.Allow[i-1] {
			continue
		}
		allow = append(allow, w)
	}
	lex.Allow = allow

	text, err := lex.Marshal()
	if err != nil {
		return nil, err
	}
	_, report := wordlist.Compile(lex)
	fmt.Printf("lexicon: deny=%d allow=%d (compiled deny_words=%d incl. built-in, allow_words=%d, dropped=%d, duplicates=%d), %d bytes\n",
		len(lex.Deny), len(lex.Allow), report.DenyWords, report.AllowWords, report.Dropped, report.Duplicates, len(text))
	return text, nil
}

// readLines 读一个词文件:去掉 BOM、行尾空白、空行和 # 开头的注释行,返回剩下的行和它们的行号。
func readLines(path string) (lines []string, numbers []int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	content := strings.TrimPrefix(string(raw), string(rune(0xFEFF))) // 去掉文件开头的 BOM
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
		numbers = append(numbers, i+1)
	}
	return lines, numbers, nil
}

func readDenyFile(path string, category wordlist.Category) ([]wordlist.Entry, error) {
	lines, numbers, err := readLines(path)
	if err != nil {
		return nil, err
	}
	entries := make([]wordlist.Entry, 0, len(lines))
	for i, line := range lines {
		word, modeName, hasMode := strings.Cut(line, "\t")
		word = strings.TrimSpace(word)
		if err := wordlist.ValidateWord(word); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, numbers[i], err)
		}
		mode := wordlist.DefaultMode(word)
		if hasMode {
			parsed, ok := wordlist.ParseMode(strings.TrimSpace(modeName))
			if !ok {
				return nil, fmt.Errorf("%s:%d: unknown mode %q", path, numbers[i], modeName)
			}
			mode = parsed
		}
		entries = append(entries, wordlist.Entry{Word: word, Category: category, Mode: mode})
	}
	return entries, nil
}

func readAllowFile(path string) ([]string, error) {
	lines, numbers, err := readLines(path)
	if err != nil {
		return nil, err
	}
	words := make([]string, 0, len(lines))
	for i, line := range lines {
		word := strings.TrimSpace(line)
		if err := wordlist.ValidateWord(word); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, numbers[i], err)
		}
		words = append(words, word)
	}
	return words, nil
}
