package authoritysql

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsGrantMarkerRejectsUnsupportedPathsAndVolumes(t *testing.T) {
	for _, path := range []string{`relative`, `C:relative`, `C:\`, `\\server\share\consume`, `\\?\C:\consume`, `C:\consume:stream`, `C:\consume.`, `C:\consume `} {
		if _, err := windowsConsumePath(path); err == nil {
			t.Errorf("错误接受路径%q", path)
		}
	}
	for _, test := range []struct {
		name  string
		drive uint32
		fs    string
		flags uint32
	}{
		{"网络卷", windows.DRIVE_REMOTE, "NTFS", windows.FILE_PERSISTENT_ACLS},
		{"移动卷", windows.DRIVE_REMOVABLE, "NTFS", windows.FILE_PERSISTENT_ACLS},
		{"非NTFS", windows.DRIVE_FIXED, "ReFS", windows.FILE_PERSISTENT_ACLS},
		{"无持久ACL", windows.DRIVE_FIXED, "NTFS", 0},
		{"只读卷", windows.DRIVE_FIXED, "NTFS", windows.FILE_PERSISTENT_ACLS | windows.FILE_READ_ONLY_VOLUME},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateWindowsMarkerVolume(test.drive, test.fs, test.flags); err == nil {
				t.Fatal("错误接受不支持的卷")
			}
		})
	}
	if err := validateWindowsMarkerVolume(windows.DRIVE_FIXED, "NTFS", windows.FILE_PERSISTENT_ACLS); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsGrantMarkerACLRejectsUntrustedMutation(t *testing.T) {
	user, err := windows.StringToSid("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, sddl string
		accepted   bool
	}{
		{"私有账号与系统管理员", "O:S-1-5-21-1-2-3-1001D:P(A;OICI;FA;;;S-1-5-21-1-2-3-1001)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", true},
		{"只读共享不授修改", "O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;S-1-5-21-1-2-3-1001)(A;;GR;;;WD)", true},
		{"全员可写", "O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;WD)", false},
		{"继承给标记的可写权限", "O:S-1-5-21-1-2-3-1001D:P(A;OIIO;GW;;;WD)", false},
		{"全员可删除子项", "O:S-1-5-21-1-2-3-1001D:P(A;;0x40;;;WD)", false},
		{"未知对象ACE拒绝", "O:S-1-5-21-1-2-3-1001D:P(OA;;GR;00000000-0000-0000-0000-000000000001;;WD)", false},
		{"陌生owner可改DACL", "O:S-1-5-21-1-2-3-1002D:P(A;;GR;;;WD)", false},
		{"缺失DACL", "O:S-1-5-21-1-2-3-1001", false},
		{"空指针DACL", "O:S-1-5-21-1-2-3-1001D:NO_ACCESS_CONTROL", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = validateWindowsMarkerACL(sd, user)
			if (err == nil) != test.accepted {
				t.Fatalf("ACL结局=%v，期望接受=%v", err, test.accepted)
			}
		})
	}
}

func TestWindowsGrantMarkerConcurrentConsumptionAndReplay(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "consume")
	wide, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 仅夹具在自己的临时子目录创建私有ACL，生产消费入口没有设权限路径。
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err = windows.CreateDirectory(wide, sa); err != nil {
		t.Fatal(err)
	}
	const name = "00112233445566778899aabbccddeeff.consumed"
	content := []byte("fixed-digest\n")
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- consumeGrantMarker(dir, name, content)
		}()
	}
	close(start)
	group.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("单次消费成功数=%d", winners)
	}
	if err = consumeGrantMarker(dir, name, []byte("replacement\n")); err == nil {
		t.Fatal("重复消费未拒绝")
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(content) {
		t.Fatal("原消费标记被改写")
	}
}

func TestWindowsPrivateControlACLRejectsForeignRead(t *testing.T) {
	user, err := windows.StringToSid("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;S-1-5-21-1-2-3-1001)(A;;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	if validateWindowsControlACL(sd, user, true) == nil {
		t.Fatal("私有凭据允许其他主体读取")
	}
	if err = validateWindowsMarkerACL(sd, user); err != nil {
		t.Fatalf("非秘密消费marker的既有只读共享契约被改动: %v", err)
	}
}
