package app

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A pathname alone does not prove ownership. A child can fail to bind because
// another listener took the path between retries. Keep the filesystem identity
// only after the kernel associates that exact inode with a socket FD in child.
type runtimeSocketIdentity struct {
	dev, ino   uint64
	uid        uint32
	ctime      syscall.Timespec
	mode       uint32
	targetPath string
	targetLink string
	target     *runtimeSocketIdentity
}

// Recent Codex versions publish --listen as a symlink to their daemon socket.
// Record both directory entries; neither may be removed merely by following a
// symlink at cleanup time. Only one direct socket target is accepted here.
func runtimeSocketSnapshot(path string) (entry, target *syscall.Stat_t, targetPath string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, "", err
	}
	entry, ok := info.Sys().(*syscall.Stat_t)
	if !ok || entry.Uid != uint32(os.Getuid()) {
		return nil, nil, "", errors.New("socket entry owner is unknown")
	}
	if info.Mode()&os.ModeSocket != 0 {
		return entry, entry, path, nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil, nil, "", errors.New("socket entry is neither socket nor link")
	}
	targetPath, err = os.Readlink(path)
	if err != nil {
		return nil, nil, "", err
	}
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(filepath.Dir(path), targetPath)
	}
	targetPath = filepath.Clean(targetPath)
	target, err = runtimeSocketStat(targetPath)
	return entry, target, targetPath, err
}

func runtimeSameSocketStat(first, second *syscall.Stat_t) bool {
	return first != nil && second != nil && first.Dev == second.Dev && first.Ino == second.Ino && first.Uid == second.Uid && first.Mode == second.Mode && first.Ctim == second.Ctim
}

func runtimeSocketIdentityFrom(st *syscall.Stat_t) *runtimeSocketIdentity {
	return &runtimeSocketIdentity{dev: st.Dev, ino: st.Ino, uid: st.Uid, ctime: st.Ctim, mode: st.Mode}
}
func runtimeSocketIdentityMatches(st *syscall.Stat_t, id *runtimeSocketIdentity) bool {
	return st != nil && id != nil && st.Dev == id.dev && st.Ino == id.ino && st.Uid == id.uid && st.Ctim == id.ctime && st.Mode == id.mode
}

func runtimeSocketStat(path string) (*syscall.Stat_t, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || st.Uid != uint32(os.Getuid()) {
		return nil, errors.New("socket identity cannot be established")
	}
	return st, nil
}

func runtimeChildSocket(path string, child runtimeChild) *runtimeSocketIdentity {
	if child.BootID != "" {
		boot, err := runtimeBootID()
		if err != nil || child.BootID != boot {
			return nil
		}
	}
	start, err := runtimeProcessStart(child.PID)
	if err != nil || start != child.Start {
		return nil
	}
	entryBefore, before, targetPath, err := runtimeSocketSnapshot(path)
	if err != nil {
		return nil
	}
	fdRoot := fmt.Sprintf("/proc/%d/fd", child.PID)
	entries, err := os.ReadDir(fdRoot)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join(fdRoot, entry.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
			continue
		}
		ino, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32)
		if err != nil {
			continue
		}
		boundIno, boundDev, uid, err := runtimeSocketVFS(uint32(ino))
		if err != nil || uid != before.Uid || boundIno != uint32(before.Ino) || boundDev != runtimeSocketDevice(before.Dev) {
			continue
		}
		entryAfter, after, targetAfter, err := runtimeSocketSnapshot(path)
		if err != nil || targetPath != targetAfter || !runtimeSameSocketStat(before, after) || !runtimeSameSocketStat(entryBefore, entryAfter) {
			return nil
		}
		start, err = runtimeProcessStart(child.PID)
		if err != nil || start != child.Start {
			return nil
		}
		identity := runtimeSocketIdentityFrom(entryAfter)
		if targetPath != path {
			identity.targetPath = targetPath
			identity.targetLink, err = os.Readlink(path)
			if err != nil {
				return nil
			}
			identity.target = runtimeSocketIdentityFrom(after)
		}
		return identity
	}
	return nil
}

func runtimeSocketDevice(dev uint64) uint32 {
	major, minor := unix.Major(dev), unix.Minor(dev)
	// unix_diag_vfs reports the kernel's internal dev_t, not stat's encoding.
	return major<<20 | minor
}

func runtimeSocketMatches(path string, owned *runtimeSocketIdentity) bool {
	if owned == nil {
		return false
	}
	entry, target, targetPath, err := runtimeSocketSnapshot(path)
	if err != nil || !runtimeSocketIdentityMatches(entry, owned) {
		return false
	}
	if owned.target == nil {
		return targetPath == path
	}
	return targetPath == owned.targetPath && runtimeSocketIdentityMatches(target, owned.target)
}

// SOCK_DIAG exposes the filesystem inode of the bound UNIX socket. /proc/net/unix
// alone exposes a different socket inode and cannot prove pathname ownership
// after another process unlinks and replaces a still-open listening socket.
func runtimeSocketVFS(ino uint32) (vfsIno, vfsDev, uid uint32, err error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, 0, 0, err
	}
	defer unix.Close(fd)
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 100000}); err != nil {
		return 0, 0, 0, err
	}
	// nlmsghdr(16) + unix_diag_req(24), querying one exact kernel socket inode.
	req := make([]byte, 40)
	native := binary.NativeEndian
	native.PutUint32(req[0:4], uint32(len(req)))
	native.PutUint16(req[4:6], unix.SOCK_DIAG_BY_FAMILY)
	native.PutUint16(req[6:8], unix.NLM_F_REQUEST)
	native.PutUint32(req[8:12], 1)
	req[16] = unix.AF_UNIX
	native.PutUint32(req[20:24], ^uint32(0))
	native.PutUint32(req[24:28], ino)
	native.PutUint32(req[28:32], 0x02|0x40) // UDIAG_SHOW_VFS | UDIAG_SHOW_UID
	native.PutUint32(req[32:36], ^uint32(0))
	native.PutUint32(req[36:40], ^uint32(0))
	if err = unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, 0, 0, err
	}
	buf := make([]byte, 4096)
	n, from, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	source, ok := from.(*unix.SockaddrNetlink)
	if !ok || source.Pid != 0 {
		return 0, 0, 0, errors.New("socket diagnostic response is not from kernel")
	}
	messages, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return 0, 0, 0, err
	}
	for _, msg := range messages {
		if msg.Header.Type != unix.SOCK_DIAG_BY_FAMILY || msg.Header.Seq != 1 || len(msg.Data) < 16 || msg.Data[0] != unix.AF_UNIX || native.Uint32(msg.Data[4:8]) != ino {
			continue
		}
		attrs := msg.Data[16:]
		foundVFS, foundUID := false, false
		for len(attrs) >= 4 {
			size := int(native.Uint16(attrs[:2]))
			typ := native.Uint16(attrs[2:4])
			if size < 4 || size > len(attrs) {
				return 0, 0, 0, errors.New("invalid socket diagnostic attribute")
			}
			switch typ {
			case 1: // UNIX_DIAG_VFS (enum starts at zero)
				if size >= 12 {
					vfsIno = native.Uint32(attrs[4:8])
					vfsDev = native.Uint32(attrs[8:12])
					foundVFS = true
				}
			case 7: // UNIX_DIAG_UID
				if size >= 8 {
					uid = native.Uint32(attrs[4:8])
					foundUID = true
				}
			}
			aligned := (size + 3) &^ 3
			if aligned > len(attrs) {
				break
			}
			attrs = attrs[aligned:]
		}
		if foundVFS && foundUID {
			return vfsIno, vfsDev, uid, nil
		}
	}
	return 0, 0, 0, errors.New("socket filesystem ownership is unavailable")
}

func (a *App) runtimeRemoveChildSocket(owned *runtimeSocketIdentity) {
	if owned == nil {
		return
	}
	info, err := os.Lstat(a.P.Socket)
	if err != nil {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !runtimeSocketIdentityMatches(st, owned) {
		return
	}
	if owned.target != nil {
		// Remove only the target whose identity was proved while the child held
		// its FD. Never resolve a newly substituted link when cleaning up.
		target, err := runtimeSocketStat(owned.targetPath)
		if err == nil && runtimeSocketIdentityMatches(target, owned.target) {
			_ = os.Remove(owned.targetPath)
		}
	}
	_ = os.Remove(a.P.Socket)
}
