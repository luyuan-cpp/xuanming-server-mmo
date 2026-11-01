package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// journal 把每一步写到屏幕和本地文件。文件是事后对账用的:etcd 里只留最新状态,这里留过程。
// 内容不含连接串和口令。追加写失败不打断切换窗口,只在屏幕上提示。
type journal struct {
	out  io.Writer
	file *os.File
	now  func() time.Time
}

func openJournal(dir, db, runID string, out io.Writer, now func() time.Time) (*journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := "dbmove-" + db + "-" + runID + ".log"
	if runID == "" {
		name = "dbmove-" + db + "-plan.log"
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &journal{out: out, file: file, now: now}, nil
}

func (j *journal) write(step, line string) {
	fmt.Fprintf(j.out, "[%s] %s\n", step, line)
	j.record(step, line)
}

// record 只写文件。
func (j *journal) record(step, line string) {
	if j.file == nil {
		return
	}
	raw, err := json.Marshal(struct {
		Time string `json:"time"`
		Step string `json:"step"`
		Line string `json:"line"`
	}{j.now().UTC().Format(time.RFC3339Nano), step, line})
	if err == nil {
		_, err = j.file.Write(append(raw, '\n'))
	}
	if err != nil {
		fmt.Fprintf(j.out, "[%s] WARN: cannot append to the step log: %v\n", step, err)
	}
}

func (j *journal) Close() error {
	if j.file == nil {
		return nil
	}
	return j.file.Close()
}
