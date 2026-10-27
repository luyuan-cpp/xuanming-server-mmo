// private_control_file只做本账号私有文件I/O，不读取SQL凭据、不连接服务、不提供进程或目录清理能力。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"shared/authoritysql"
)

func run(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("private_control_file", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	command := flags.String("command", "", "固定文件操作")
	path := flags.String("path", "", "绝对目标路径")
	maxBytes := flags.Int64("max-bytes", 0, "正文上限")
	if err := flags.Parse(args); err != nil {
		return errors.New("参数无效")
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*path) || *maxBytes < 1 || *maxBytes > 16<<20 {
		return errors.New("参数无效")
	}
	switch *command {
	case "read-file":
		raw, err := authoritysql.ReadPrivateControlFile(*path, *maxBytes)
		if err != nil {
			return err
		}
		n, err := output.Write(raw)
		if n != len(raw) {
			err = errors.Join(err, io.ErrShortWrite)
		}
		return err
	case "create-file":
		raw, err := io.ReadAll(io.LimitReader(input, *maxBytes+1))
		if err != nil {
			return err
		}
		if len(raw) < 1 || int64(len(raw)) > *maxBytes {
			return errors.New("正文大小无效")
		}
		return authoritysql.WritePrivateControlFile(*path, raw)
	case "validate-directory":
		return authoritysql.CheckPrivateControlDirectory(*path)
	default:
		return errors.New("操作无效")
	}
}
func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "私有控制文件操作未确认")
		os.Exit(1)
	}
}
