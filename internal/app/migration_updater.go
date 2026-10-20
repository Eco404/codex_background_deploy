package app

import (
	"bytes"
	"context"
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

	"golang.org/x/sys/unix"
)

// The native daemon's update loop is independent of its model server. Never
// stop an executable by name: every signal requires both the native PID record
// and a matching live process, including the kernel boot and start identities.
type LegacyUpdaterPlan struct {
	Daemon, CodexHome, SourceMigration    string
	Process                               *LegacyCodexAppProcess
	BootID, PIDFile, PIDHash, PIDBackupID string
	Socket                                string
	SocketIdentity                        *runtimeSocketFile
	StopRequested                         bool
	Archived                              []string
}

type nativeUpdaterPID struct {
	PID      int `json:"pid"`
	Identity struct {
		BootID     string `json:"bootId"`
		StartTicks uint64 `json:"startTicks"`
	} `json:"processIdentity"`
}

func legacyUpdaterFile(path string) (migrationFile, error) {
	f, err := migrationFileInfo(path)
	if err != nil {
		return f, err
	}
	i, err := os.Lstat(path)
	if err != nil {
		return f, err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) {
		return f, fmt.Errorf("旧 updater 文件所有者无法核实: %s", path)
	}
	return f, nil
}

func updaterProcessEnvironment(pid int) (home, codexHome string, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", "", err
	}
	for _, item := range strings.Split(string(raw), "\x00") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if key == "HOME" {
			home = value
		}
		if key == "CODEX_HOME" {
			codexHome = value
		}
	}
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(home) || !filepath.IsAbs(codexHome) {
		return "", "", errors.New("旧 updater HOME/CODEX_HOME 不是绝对路径")
	}
	return filepath.Clean(home), filepath.Clean(codexHome), nil
}

func (a *App) updaterProcesses(codexHome string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		info, err := os.Stat(filepath.Join("/proc", entry.Name()))
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) < 4 || args[1] != "app-server" || args[2] != "daemon" || args[3] != "pid-update-loop" {
			continue
		}
		_, actual, err := updaterProcessEnvironment(pid)
		if err != nil {
			return nil, err
		}
		if actual == filepath.Clean(codexHome) {
			result = append(result, pid)
		}
	}
	return result, nil
}

func (a *App) validateLegacyUpdater(p *LegacyUpdaterPlan) error {
	if p == nil || p.Process == nil || p.Process.UID != uint32(os.Getuid()) {
		return errors.New("缺少已核实的旧 updater 身份")
	}
	boot, err := runtimeBootID()
	if err != nil || boot != p.BootID || !legacyCodexAppSameProcess(p.Process) {
		return errors.New("旧 updater 的 boot/PID/start/executable 已变化；未发送信号")
	}
	if !sameExecutable(p.Daemon, p.Process.Executable) {
		return errors.New("旧 updater 可执行文件与已迁移 daemon 不一致；保留进程")
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.Process.PID))
	if err != nil {
		return err
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) != 4 || !sameExecutable(args[0], p.Daemon) || args[1] != "app-server" || args[2] != "daemon" || args[3] != "pid-update-loop" {
		return errors.New("旧 updater 命令参数不符合已核实实现")
	}
	home, codexHome, err := updaterProcessEnvironment(p.Process.PID)
	if err != nil {
		return err
	}
	if home != a.P.Home || codexHome != p.CodexHome {
		return errors.New("旧 updater 的 HOME/CODEX_HOME 与本次迁移不一致")
	}
	f, err := legacyUpdaterFile(p.PIDFile)
	if err != nil || f.Hash != p.PIDHash {
		return errors.New("旧 updater PID 记录已变化；未发送信号")
	}
	return nil
}

// daemon must come from this migration's recognized startup hook/unit or a
// committed migration record, never merely from a PATH discovery candidate.
func (a *App) planLegacyUpdater(daemon, source string) (*LegacyUpdaterPlan, error) {
	processes, err := a.updaterProcesses(a.S.CodexHome)
	if err != nil {
		return nil, err
	}
	pidFile := filepath.Join(a.S.CodexHome, "app-server-daemon", "app-server-updater.pid")
	if _, err := os.Lstat(pidFile); errors.Is(err, os.ErrNotExist) && len(processes) == 0 {
		return nil, nil
	}
	f, err := legacyUpdaterFile(pidFile)
	if errors.Is(err, os.ErrNotExist) && len(processes) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("旧 updater 缺少可靠 PID 记录，保留: %w", err)
	}
	if len(processes) == 0 {
		// A stale record is not enough evidence to claim old files. Keep it for
		// inspection; normal migration can still proceed without a live updater.
		fmt.Fprintln(a.Out, "保留旧 updater PID 记录：未发现对应运行进程", pidFile)
		return nil, nil
	}
	if len(processes) != 1 || daemon == "" {
		return nil, errors.New("旧 updater 归属不明确或存在多个进程；未接管")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return nil, err
	}
	var record nativeUpdaterPID
	if err = json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if record.PID != processes[0] || record.Identity.BootID == "" || record.Identity.StartTicks == 0 {
		return nil, errors.New("旧 updater PID 记录与运行进程不一致")
	}
	identity, err := legacyCodexAppIdentity(record.PID)
	if err != nil {
		return nil, err
	}
	if identity.Start != strconv.FormatUint(record.Identity.StartTicks, 10) {
		return nil, errors.New("旧 updater PID 记录的启动时间不匹配")
	}
	p := &LegacyUpdaterPlan{Daemon: daemon, CodexHome: a.S.CodexHome, SourceMigration: source, Process: identity, BootID: record.Identity.BootID, PIDFile: pidFile, PIDHash: f.Hash, Socket: filepath.Join(filepath.Dir(pidFile), "app-server-updater.sock")}
	if err = a.validateLegacyUpdater(p); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(p.Socket); err == nil {
		owned := runtimeChildSocket(p.Socket, runtimeChild{PID: identity.PID, Start: identity.Start, BootID: p.BootID})
		if owned == nil || owned.target != nil {
			return nil, errors.New("旧 updater socket 不能核实为该进程持有的直接 socket；保留")
		}
		file := runtimeSocketFileFrom(owned)
		p.SocketIdentity = &file
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fmt.Fprintf(a.Out, "旧 native updater: PID=%d UID=%d start=%s；迁移停止该独立更新循环，保留复用 Codex 程序和用户数据\n", identity.PID, identity.UID, identity.Start)
	return p, nil
}

func (a *App) prepareLegacyUpdater(t *Transaction, p *LegacyUpdaterPlan) error {
	if p == nil {
		return nil
	}
	if err := a.validateLegacyUpdater(p); err != nil {
		return err
	}
	b, err := a.BackupFile(p.PIDFile, "legacy-updater-pid", t.ID)
	if err != nil {
		return err
	}
	if b.Hash != p.PIDHash {
		return errors.New("备份时旧 updater PID 记录变化")
	}
	p.PIDBackupID = b.ID
	t.LegacyUpdater = p
	return a.saveTx(t)
}

func (a *App) stopLegacyUpdater(ctx context.Context, t *Transaction) error {
	p := t.LegacyUpdater
	if p == nil {
		return nil
	}
	if p.StopRequested {
		gone, err := runtimeRecordedProcessGone(p.Process.PID, p.Process.Start, p.BootID)
		if err != nil {
			return err
		}
		if gone {
			return a.archiveLegacyUpdater(t)
		}
	}
	if err := a.validateLegacyUpdater(p); err != nil {
		return err
	}
	processes, err := a.updaterProcesses(p.CodexHome)
	if err != nil {
		return err
	}
	if len(processes) != 1 || processes[0] != p.Process.PID {
		return errors.New("旧 updater 在预览后变化；未停止")
	}
	fd, err := unix.PidfdOpen(p.Process.PID, 0)
	if err != nil {
		return fmt.Errorf("无法安全固定旧 updater 身份: %w", err)
	}
	defer unix.Close(fd)
	if err = a.validateLegacyUpdater(p); err != nil {
		return err
	}
	p.StopRequested = true
	if err = a.saveTx(t); err != nil {
		return err
	}
	if err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && err != unix.ESRCH {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if n, err := unix.Poll(poll, 0); err != nil {
			return err
		} else if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("旧 updater 未正常停止；未强制杀死，请检查后 recover")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return a.archiveLegacyUpdater(t)
}

func (a *App) archiveLegacyUpdater(t *Transaction) error {
	p := t.LegacyUpdater
	processes, err := a.updaterProcesses(p.CodexHome)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return errors.New("旧 updater 被其他管理器重新启动，保留文件并等待处理")
	}
	if p.SocketIdentity != nil {
		st, err := runtimeSocketStat(p.Socket)
		if err == nil {
			if !p.SocketIdentity.matches(st) {
				return errors.New("旧 updater socket 被替换；保留")
			}
			// A directory FD also supports installations whose absolute parent
			// path exceeds sockaddr_un's limit; the fixed basename is preserved.
			dir, openErr := os.Open(filepath.Dir(p.Socket))
			if openErr != nil {
				return openErr
			}
			shortSocket := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(p.Socket))
			connection, dialErr := net.DialTimeout("unix", shortSocket, 200*time.Millisecond)
			_ = dir.Close()
			if dialErr == nil {
				connection.Close()
				return errors.New("旧 updater socket 仍有监听器；保留")
			}
			if !errors.Is(dialErr, syscall.ECONNREFUSED) {
				return fmt.Errorf("不能确认旧 updater socket 已关闭: %w", dialErr)
			}
			st, err = runtimeSocketStat(p.Socket)
			if err != nil || !p.SocketIdentity.matches(st) {
				return errors.New("旧 updater socket 在清理前变化；保留")
			}
			if err = os.Remove(p.Socket); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, path := range []string{p.PIDFile, filepath.Join(filepath.Dir(p.PIDFile), "app-server-updater.stderr.log"), filepath.Join(filepath.Dir(p.PIDFile), "app-server-updater.stderr.log.previous")} {
		f, err := legacyUpdaterFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if path == p.PIDFile && f.Hash != p.PIDHash {
			return errors.New("旧 updater PID 记录被替换；保留")
		}
		backup, err := a.BackupFile(path, "legacy-updater-artifact", t.ID)
		if err != nil {
			return err
		}
		if backup.Hash != f.Hash {
			return errors.New("旧 updater 资料在备份时变化；保留")
		}
		p.Archived = appendUnique(p.Archived, backup.ID)
		if t.Action == "migrate" {
			found := false
			for _, r := range t.Resources {
				if r.Path == path {
					found = true
				}
			}
			if !found {
				t.Resources = append(t.Resources, TxResource{Path: path, BackupID: backup.ID, Deleted: true})
			}
		}
		if err = a.saveTx(t); err != nil {
			return err
		}
		if err = f.unchanged(); err != nil {
			return err
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	log := a.RuntimeLog("migration-updater")
	fmt.Fprintf(log, "%s stopped verified native updater PID=%d start=%s; archived=%v; Codex binaries, daemon lock files, auth and sessions retained\n", time.Now().UTC().Format(time.RFC3339), p.Process.PID, p.Process.Start, p.Archived)
	_ = log.Close()
	return nil
}

// Native daemon start recreates its own updater. Recovery verifies that
// capability explicitly instead of claiming success when only the core returns.
func (a *App) restoreLegacyUpdater(ctx context.Context, t *Transaction) error {
	p := t.LegacyUpdater
	if p == nil || !p.StopRequested {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		restored, err := a.planLegacyUpdater(p.Daemon, "")
		if err == nil && restored != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("旧后台已恢复，但 native updater 尚未恢复；保留事务以便重试: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a *App) migratedUpdaterSource() (string, string, error) {
	if !a.S.Installed {
		return "", "", errors.New("仅已迁移的安装可清理旧 updater")
	}
	entries, err := os.ReadDir(filepath.Join(a.P.Data, "history"))
	if err != nil {
		return "", "", err
	}
	var newest *Transaction
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(a.P.Data, "history", entry.Name()))
		if err != nil {
			return "", "", err
		}
		var t Transaction
		if err = json.Unmarshal(raw, &t); err != nil {
			return "", "", err
		}
		if t.Action != "migrate" || t.LegacyDaemon == "" || t.Previous.CodexHome != a.S.CodexHome || t.Previous.DataID != a.S.DataID {
			continue
		}
		ownedRuntime := false
		for _, r := range t.Resources {
			if r.Path == a.P.Binary() && r.Created {
				ownedRuntime = true
			}
		}
		if !ownedRuntime {
			continue
		}
		if newest == nil || newest.Started.Before(t.Started) {
			copy := t
			newest = &copy
		}
	}
	if newest == nil {
		return "", "", errors.New("没有属于当前运行目录的已提交 native daemon 迁移记录；不猜测服务归属")
	}
	// A verified old startup backup proves the migration retired the manager,
	// rather than only having happened to see a Codex executable on PATH.
	for _, r := range newest.Resources {
		if !r.Deleted || filepath.Base(r.Path) != "05-start-codex-daemon.sh" {
			continue
		}
		b, err := a.ReadBackup(r.BackupID)
		if err != nil {
			return "", "", err
		}
		if b.Original != r.Path || a.VerifyBackup(b.ID) != nil {
			return "", "", errors.New("旧 daemon 启动项备份缺失或校验失败")
		}
		raw, err := os.ReadFile(b.DataPath)
		if err != nil {
			return "", "", err
		}
		if !bytes.Contains(raw, []byte("app-server daemon start")) || !bytes.Contains(raw, []byte(".codex/packages/standalone/current/codex")) {
			return "", "", errors.New("旧 daemon 启动项来源不匹配")
		}
		if _, err = os.Lstat(r.Path); !errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("旧 daemon 启动项仍存在或无法检查；先处理其管理归属")
		}
		return newest.LegacyDaemon, newest.ID, nil
	}
	return "", "", errors.New("当前迁移缺少已移除的 native daemon 启动项证据；未清理")
}

func (a *App) planLegacyUpdaterCleanup(ctx context.Context) (*LegacyUpdaterPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hasPending(a.P.Data) {
		return nil, errors.New("存在未完成事务，先 recover")
	}
	daemon, source, err := a.migratedUpdaterSource()
	if err != nil {
		return nil, err
	}
	p, err := a.planLegacyUpdater(daemon, source)
	if err != nil {
		return nil, err
	}
	if p == nil {
		fmt.Fprintln(a.Out, "未发现需要停止的旧 native updater；不修改现有运行服务。")
		return nil, nil
	}
	fmt.Fprintln(a.Out, "计划：只向已核实的 updater 进程发送 SIGTERM；PID 记录和 updater 日志集中备份后移除；仅清理该进程持有的原 socket。")
	fmt.Fprintln(a.Out, "保留：新 Go 后台、CodexApp、复用 Codex 版本、认证会话、旧 daemon 锁文件与其他历史资料。")
	fmt.Fprintln(a.Out, "此操作不重启当前 Codex；失败/中断后 recover 继续同一清理，不重新启用旧自动更新。")
	return p, nil
}

func (a *App) PreviewLegacyUpdaterCleanup(ctx context.Context) (string, error) {
	copy := *a
	var output bytes.Buffer
	copy.Out, copy.Err = &output, &output
	_, err := copy.planLegacyUpdaterCleanup(ctx)
	return output.String(), err
}

func (a *App) CleanupLegacyUpdater(ctx context.Context, yes bool) error {
	if !yes {
		return errors.New("清理旧 updater 需要预览并明确确认")
	}
	unlock, err := a.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	p, err := a.planLegacyUpdaterCleanup(ctx)
	if err != nil || p == nil {
		return err
	}
	t, err := a.begin("legacy-updater-cleanup")
	if err != nil {
		return err
	}
	if err = a.prepareLegacyUpdater(t, p); err != nil {
		return err
	}
	return a.recoverLegacyUpdaterCleanup(ctx, t)
}

func (a *App) recoverLegacyUpdaterCleanup(ctx context.Context, t *Transaction) error {
	if t.Action != "legacy-updater-cleanup" {
		return errors.New("invalid legacy updater cleanup transaction")
	}
	if t.LegacyUpdater == nil {
		return a.commit(t)
	}
	if err := a.stopLegacyUpdater(ctx, t); err != nil {
		return err
	}
	return a.commit(t)
}
