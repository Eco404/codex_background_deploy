package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runtime evidence belongs beside control.json, not in disposable maintenance
// data: it is required to recover a socket after SIGKILL or container rebuild.
type runtimeSocketFile struct {
	Device uint64           `json:"device"`
	Inode  uint64           `json:"inode"`
	UID    uint32           `json:"uid"`
	Mode   uint32           `json:"mode"`
	CTime  syscall.Timespec `json:"ctime"`
}

type runtimeSocketOwnership struct {
	Schema     int                `json:"schema"`
	Generation string             `json:"generation"`
	Path       string             `json:"path"`
	Entry      runtimeSocketFile  `json:"entry"`
	TargetPath string             `json:"target_path,omitempty"`
	TargetLink string             `json:"target_link,omitempty"`
	Target     *runtimeSocketFile `json:"target,omitempty"`
	Supervisor runtimePID         `json:"supervisor"`
	Child      runtimeChild       `json:"child"`
	Recorded   time.Time          `json:"recorded"`
}

func runtimeBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	boot := strings.TrimSpace(string(raw))
	if len(boot) != 36 {
		return "", errors.New("kernel boot identity is unavailable")
	}
	return boot, nil
}

// Different start ticks or boot identities prove the recorded process is gone,
// even when an unrelated process now has the same PID. Never signal that PID.
func runtimeRecordedProcessGone(pid int, start, boot string) (bool, error) {
	if pid <= 1 || start == "" || boot == "" {
		return false, errors.New("incomplete process ownership")
	}
	currentBoot, err := runtimeBootID()
	if err != nil {
		return false, err
	}
	if boot != currentBoot {
		return true, nil
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	i := strings.LastIndex(string(raw), ") ")
	if i < 0 {
		return false, errors.New("invalid process identity")
	}
	fields := strings.Fields(string(raw)[i+2:])
	if len(fields) <= 19 {
		return false, errors.New("incomplete process identity")
	}
	return fields[0] == "Z" || fields[19] != start, nil
}

func runtimeProcessParent(pid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := strings.LastIndex(string(raw), ") ")
	if i < 0 {
		return 0, errors.New("invalid child process identity")
	}
	fields := strings.Fields(string(raw)[i+2:])
	if len(fields) < 2 {
		return 0, errors.New("incomplete child process identity")
	}
	return strconv.Atoi(fields[1])
}

func runtimeSocketFileFrom(identity *runtimeSocketIdentity) runtimeSocketFile {
	return runtimeSocketFile{Device: identity.dev, Inode: identity.ino, UID: identity.uid, Mode: identity.mode, CTime: identity.ctime}
}

func (f runtimeSocketFile) matches(st *syscall.Stat_t) bool {
	return st != nil && st.Dev == f.Device && st.Ino == f.Inode && st.Uid == f.UID && st.Mode == f.Mode && st.Ctim == f.CTime
}

func (a *App) runtimeSocketOwnershipFile() string {
	return filepath.Join(a.P.Runtime, "run", "socket-owner.json")
}

// RecordRuntimeSocketOwnership upgrades an already-running Go installation's
// evidence without starting, stopping or signaling it. The only accepted child
// must be the current supervisor's direct child and own the kernel socket FD.
func (a *App) RecordRuntimeSocketOwnership() error {
	c, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if c.Desired != "running" || c.Mode != "normal" || c.Status != "running" || c.Child == nil {
		return errors.New("only a running managed child can register socket ownership")
	}
	owned := runtimeChildSocket(a.P.Socket, *c.Child)
	if owned == nil {
		return errors.New("kernel socket ownership cannot be verified")
	}
	return a.recordRuntimeSocketOwnership(c.Generation, *c.Child, owned)
}

func (a *App) recordRuntimeSocketOwnership(generation string, child runtimeChild, owned *runtimeSocketIdentity) error {
	_, err := a.updateRuntimeControl(generation, func(c *runtimeControl) error {
		if c.Desired != "running" || c.Child == nil || *c.Child != child {
			return errRuntimeSuperseded
		}
		supervisor, err := a.runtimeReadPID()
		if err != nil {
			return err
		}
		if !runtimeValidPID(supervisor) {
			return errors.New("supervisor identity cannot be verified")
		}
		parent, err := runtimeProcessParent(child.PID)
		if err != nil || parent != supervisor.PID {
			return errors.New("socket owner is not the managed supervisor's child")
		}
		boot, err := runtimeBootID()
		if err != nil {
			return err
		}
		// Recheck kernel ownership inside the control lock. A stale health probe
		// must never replace evidence for a newer child or a new stop generation.
		verified := runtimeChildSocket(a.P.Socket, child)
		if verified == nil || owned == nil || !runtimeSocketMatches(a.P.Socket, owned) {
			return errors.New("socket changed before ownership registration")
		}
		supervisor.BootID, child.BootID = boot, boot
		proof := runtimeSocketOwnership{Schema: 1, Generation: generation, Path: a.P.Socket, Entry: runtimeSocketFileFrom(verified), Supervisor: supervisor, Child: child, Recorded: time.Now().UTC()}
		if verified.target != nil {
			target := runtimeSocketFileFrom(verified.target)
			proof.Target, proof.TargetPath, proof.TargetLink = &target, verified.targetPath, verified.targetLink
		}
		raw, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			return err
		}
		return AtomicWrite(a.runtimeSocketOwnershipFile(), append(raw, '\n'), 0600)
	})
	return err
}

func (a *App) readRuntimeSocketOwnership() (runtimeSocketOwnership, error) {
	var proof runtimeSocketOwnership
	raw, err := os.ReadFile(a.runtimeSocketOwnershipFile())
	if err != nil {
		return proof, err
	}
	if err = json.Unmarshal(raw, &proof); err != nil {
		return proof, err
	}
	if proof.Schema != 1 || proof.Generation == "" || proof.Path != a.P.Socket || proof.Supervisor.Runtime != a.P.Runtime || proof.Supervisor.Binary != a.P.Binary() || proof.Supervisor.UID != os.Getuid() || proof.Entry.UID != uint32(os.Getuid()) || proof.Entry.Inode == 0 || proof.Child.BootID == "" || proof.Child.BootID != proof.Supervisor.BootID {
		return proof, errors.New("socket ownership record does not match this runtime")
	}
	if proof.Target != nil {
		resolved := proof.TargetLink
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(proof.Path), resolved)
		}
		if proof.TargetLink == "" || filepath.Clean(resolved) != proof.TargetPath || proof.TargetPath == proof.Path || proof.Entry.Mode&syscall.S_IFMT != syscall.S_IFLNK || proof.Target.Mode&syscall.S_IFMT != syscall.S_IFSOCK || proof.Target.UID != uint32(os.Getuid()) {
			return proof, errors.New("invalid socket target ownership record")
		}
	} else if proof.Entry.Mode&syscall.S_IFMT != syscall.S_IFSOCK || proof.TargetPath != "" || proof.TargetLink != "" {
		return proof, errors.New("invalid direct socket ownership record")
	}
	return proof, nil
}

func runtimeInactiveSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, 150*time.Millisecond)
	if err == nil {
		conn.Close()
		return errors.New("socket still has an active listener")
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("socket inactivity could not be established: %w", err)
}

// Caller holds supervisor.lock throughout. The control lock also excludes live
// evidence refresh and intent replacement while checking and removing entries.
// An absent /tmp target is allowed only for the exact recorded symlink entry.
func (a *App) recoverRuntimeSocketLocked(generation string) error {
	if _, err := os.Lstat(a.P.Socket); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := a.updateRuntimeControl(generation, func(c *runtimeControl) error {
		proof, err := a.readRuntimeSocketOwnership()
		if err != nil {
			return fmt.Errorf("socket exists without reliable recovery evidence; preserving it: %w", err)
		}
		for _, process := range []runtimeChild{{PID: proof.Supervisor.PID, Start: proof.Supervisor.Start, BootID: proof.Supervisor.BootID}, proof.Child} {
			gone, err := runtimeRecordedProcessGone(process.PID, process.Start, process.BootID)
			if err != nil || !gone {
				return errors.New("recorded socket owner is still running or cannot be verified; preserving socket")
			}
		}
		if c.Child != nil && (c.Child.PID != proof.Child.PID || c.Child.Start != proof.Child.Start || c.Child.BootID != "" && c.Child.BootID != proof.Child.BootID) {
			return errors.New("current child differs from socket recovery evidence; preserving socket")
		}
		entry, err := os.Lstat(a.P.Socket)
		if err != nil {
			return err
		}
		st, ok := entry.Sys().(*syscall.Stat_t)
		if !ok || !proof.Entry.matches(st) {
			return errors.New("socket entry was replaced; preserving it")
		}
		targetExists := false
		if proof.Target != nil {
			link, err := os.Readlink(a.P.Socket)
			if err != nil || link != proof.TargetLink {
				return errors.New("socket link was changed; preserving it")
			}
			target, err := runtimeSocketStat(proof.TargetPath)
			if err == nil {
				if !proof.Target.matches(target) {
					return errors.New("socket target was replaced; preserving it")
				}
				targetExists = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("socket target is unknown; preserving it: %w", err)
			}
		}
		if err := runtimeInactiveSocket(a.P.Socket); err != nil {
			return err
		}
		if targetExists {
			if err := runtimeInactiveSocket(proof.TargetPath); err != nil {
				return err
			}
			target, err := runtimeSocketStat(proof.TargetPath)
			if err != nil || !proof.Target.matches(target) {
				return errors.New("socket target changed during recovery")
			}
			if err = os.Remove(proof.TargetPath); err != nil {
				return err
			}
		}
		entry, err = os.Lstat(a.P.Socket)
		if err != nil {
			return err
		}
		st, ok = entry.Sys().(*syscall.Stat_t)
		if !ok || !proof.Entry.matches(st) {
			return errors.New("socket entry changed during recovery")
		}
		if err = os.Remove(a.P.Socket); err != nil {
			return err
		}
		a.runtimeStatus("socket-recovery", map[string]any{"ok": true, "code": "registered_stale_socket_removed"})
		return nil
	})
	return err
}

func (a *App) runtimeCleanupStoppedSupervisor(previous *runtimePID) error {
	lock, err := runtimeLock(filepath.Join(a.P.Runtime, "run", "supervisor.lock"))
	if err != nil {
		return fmt.Errorf("supervisor still owns its lock; preserving runtime: %w", err)
	}
	defer runtimeUnlock(lock)
	if previous != nil {
		boot := previous.BootID
		if boot == "" {
			boot, err = runtimeBootID()
			if err != nil {
				return err
			}
		}
		gone, err := runtimeRecordedProcessGone(previous.PID, previous.Start, boot)
		if err != nil || !gone {
			return errors.New("supervisor PID identity changed; refusing to signal it")
		}
	}
	c, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if err = a.recoverRuntimeSocketLocked(c.Generation); err != nil {
		return err
	}
	if previous != nil {
		current, err := a.runtimeReadPID()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != *previous {
			return errors.New("supervisor record changed during cleanup")
		}
		return os.Remove(a.runtimePIDFile())
	}
	return nil
}
