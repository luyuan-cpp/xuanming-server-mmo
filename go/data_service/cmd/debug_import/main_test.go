package main

import "testing"

func TestRequireLocalAddr(t *testing.T) {
	for addr, allowed := range map[string]bool{
		"127.0.0.1:4000":  true,
		"[::1]:4000":      true,
		"localhost:3306":  true,
		"LOCALHOST":       true,
		"mysql:3306":      false,
		"10.0.0.8:4000":   false,
		"db.example:4000": false,
	} {
		if err := requireLocalAddr(addr, ""); (err == nil) != allowed {
			t.Errorf("%s: err=%v, allowed=%v", addr, err, allowed)
		}
	}
	if err := requireLocalAddr("mysql:3306", "mysql"); err != nil {
		t.Errorf("explicit local service name: %v", err)
	}
	if err := requireLocalAddr("db.example:4000", "mysql"); err == nil {
		t.Error("allow-target-host only allows the given name")
	}
}

func TestRunRefusesMissingArguments(t *testing.T) {
	for name, o := range map[string]options{
		"no file":   {dsnEnv: "D", player: 1},
		"no dsn":    {file: "f", player: 1},
		"no player": {file: "f", dsnEnv: "D"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(o); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

func TestNonNilKeepsNotNullColumnsWritable(t *testing.T) {
	if got := nonNil(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil payload: %v", got)
	}
	if got := nonNil([]byte{1}); len(got) != 1 {
		t.Fatalf("payload: %v", got)
	}
}
