package authoritysql

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows仅支持预建的本地固定NTFS消费目录；不修ACL、不注册主体、不取得管理特权。
// 私有指修改权限仅授当前账号、SYSTEM和管理员；沿用遵约生命周期闸门，不宣称抗同账号篡改。
// CREATE_NEW保留单次消费语义。NTFS的WRITE_THROUGH请求同时flush相关元数据，
// 继而显式FlushFileBuffers和Close；这里采用该平台提交点，不调用不受支持的目录Sync。
// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew#caching-behavior
func consumeGrantMarker(dir, name string, content []byte) error {
	return writeWindowsControlFile(dir, name, content, false)
}
func writePrivateControlFile(dir, name string, content []byte) error {
	return writeWindowsControlFile(dir, name, content, true)
}

type windowsControlDirectory struct {
	clean   string
	handles []windows.Handle
	user    *windows.SID
}

func (d *windowsControlDirectory) close() error {
	var err error
	for i := len(d.handles) - 1; i >= 0; i-- {
		err = errors.Join(err, windows.CloseHandle(d.handles[i]))
	}
	return err
}
func openWindowsControlDirectory(dir string, private bool) (result *windowsControlDirectory, err error) {
	clean, err := windowsConsumePath(dir)
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	handles, err := lockWindowsConsumePath(clean)
	d := &windowsControlDirectory{clean: clean, handles: handles, user: user.User.Sid}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.close())
			result = nil
		}
	}()
	if err != nil {
		return nil, err
	}
	parent := handles[len(handles)-1]
	var fsName [32]uint16
	var flags uint32
	if err = windows.GetVolumeInformationByHandle(parent, nil, 0, nil, nil, &flags, &fsName[0], uint32(len(fsName))); err != nil {
		return nil, err
	}
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(clean) + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	if err = validateWindowsMarkerVolume(windows.GetDriveType(root), windows.UTF16ToString(fsName[:]), flags); err != nil {
		return nil, err
	}
	if err = checkWindowsControlACL(parent, d.user, private); err != nil {
		return nil, err
	}
	return d, nil
}
func checkPrivateControlDirectory(dir string) error {
	d, err := openWindowsControlDirectory(dir, true)
	if err != nil {
		return err
	}
	return d.close()
}
func writeWindowsControlFile(dir, name string, content []byte, private bool) (err error) {
	d, err := openWindowsControlDirectory(dir, private)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.close()) }()
	target, err := windows.UTF16PtrFromString(filepath.Join(d.clean, name))
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(target, windows.GENERIC_WRITE|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_WRITE_THROUGH|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), "私有控制文件")
	if file == nil {
		return errors.Join(errors.New("控制文件句柄无效"), windows.CloseHandle(handle))
	}
	// CREATE_NEW后ACL必须以实际新文件复核；失败保留原件，绝不覆盖或删除。
	if err = checkWindowsControlACL(handle, d.user, private); err != nil {
		return errors.Join(err, file.Close())
	}
	return writeGrantMarker(file, content)
}
func readPrivateControlFile(dir, name string, maxBytes int64) (raw []byte, err error) {
	d, err := openWindowsControlDirectory(dir, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, d.close())
		if err != nil {
			raw = nil
		}
	}()
	target, err := windows.UTF16PtrFromString(filepath.Join(d.clean, name))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(target, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "私有控制文件")
	if file == nil {
		return nil, errors.Join(errors.New("控制文件句柄无效"), windows.CloseHandle(handle))
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return nil, errors.Join(errors.New("控制文件不是普通文件"), file.Close())
	}
	if err = checkWindowsControlACL(handle, d.user, true); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return readControlBytes(file, maxBytes)
}
func checkWindowsControlACL(handle windows.Handle, user *windows.SID, private bool) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validateWindowsControlACL(sd, user, private)
}

func windowsConsumePath(dir string) (string, error) {
	clean := filepath.Clean(dir)
	volume := filepath.VolumeName(clean)
	if !filepath.IsAbs(clean) || len(volume) != 2 || volume[1] != ':' ||
		!((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) ||
		strings.ContainsAny(clean[2:], ":\x00") || clean == volume+`\` {
		return "", errors.New("SQL writer消费目录必须是本地盘绝对路径")
	}
	for _, part := range strings.Split(clean[3:], `\`) {
		if part == "" || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", errors.New("SQL writer消费目录含歧义路径")
		}
	}
	return clean, nil
}

func lockWindowsConsumePath(dir string) ([]windows.Handle, error) {
	paths := []string{filepath.VolumeName(dir) + `\`}
	for _, part := range strings.Split(dir[3:], `\`) {
		paths = append(paths, filepath.Join(paths[len(paths)-1], part))
	}
	var handles []windows.Handle
	for _, path := range paths {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return handles, err
		}
		access := uint32(windows.FILE_READ_ATTRIBUTES)
		if path == dir {
			access |= windows.READ_CONTROL
		}
		handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return handles, err
		}
		handles = append(handles, handle)
		var info windows.ByHandleFileInformation
		if err = windows.GetFileInformationByHandle(handle, &info); err != nil {
			return handles, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return handles, errors.New("SQL writer消费路径不是普通目录")
		}
	}
	return handles, nil
}

func validateWindowsMarkerVolume(drive uint32, fs string, flags uint32) error {
	if drive != windows.DRIVE_FIXED || fs != "NTFS" || flags&windows.FILE_PERSISTENT_ACLS == 0 || flags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return errors.New("SQL writer消费目录要求可写固定本地NTFS及持久ACL")
	}
	return nil
}

func checkWindowsMarkerACL(handle windows.Handle, user *windows.SID) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validateWindowsMarkerACL(sd, user)
}

func validateWindowsMarkerACL(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	return validateWindowsControlACL(sd, user, false)
}
func validateWindowsControlACL(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID, private bool) error {
	bad := errors.New("SQL writer消费目录或标记ACL不安全")
	trusted := func(sid *windows.SID) bool {
		return sid != nil && sid.IsValid() && (sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	if sd == nil || !sd.IsValid() || user == nil || !user.IsValid() {
		return bad
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return bad
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return bad
	}
	// x/sys v0.39.0未导出此目录权限；WinNT.h固定值见官方File Access Rights Constants。
	// https://learn.microsoft.com/en-us/windows/win32/fileio/file-access-rights-constants
	const fileDeleteChild windows.ACCESS_MASK = 0x00000040
	const mutating = windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE |
		fileDeleteChild | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
	forbidden := windows.ACCESS_MASK(mutating)
	if private {
		forbidden |= windows.GENERIC_READ | windows.FILE_READ_DATA | windows.FILE_EXECUTE
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || uintptr(ace.Header.AceSize) < unsafe.Offsetof(ace.SidStart)+8 {
			return bad
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || uintptr(sid.Len()) > uintptr(ace.Header.AceSize)-unsafe.Offsetof(ace.SidStart) {
			return bad
		}
		// INHERIT_ONLY同样核对：否则目录安全却会把可改写权限继承给新marker。
		if ace.Mask&forbidden != 0 && !trusted(sid) {
			return bad
		}
	}
	return nil
}
