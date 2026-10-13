package app

import (
	"context"
	"io"
	"time"
)

const Version = "4.0.0"
const Marker = "codex-bg managed extension entry v4"

type Paths struct{ Home, Installer, Data, Runtime, Socket string }

func (p Paths) StateFile() string { return p.Runtime + "/state.json" }
func (p Paths) Binary() string    { return p.Runtime + "/codex-bg" }

type Candidate struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Version    string `json:"version"`
	Source     string `json:"source"`
	Compatible bool   `json:"compatible"`
	Reason     string `json:"reason,omitempty"`
}
type Extension struct {
	Root, Directory, Entry, Version, Product string
	Hooked                                   bool
	Legacy                                   bool
}
type Inventory struct {
	Host, User              string
	Container, Systemd, Apt bool
	EntrypointDir, MediaDir string
	Candidates              map[string][]Candidate
	Extensions              []Extension
	Legacy                  []string
	Warnings                []string
}
type Binding struct {
	ID             string    `json:"id"`
	Root           string    `json:"root"`
	Entry          string    `json:"entry"`
	Version        string    `json:"version"`
	BackupID       string    `json:"backup_id"`
	Hash           string    `json:"hash"`
	LastConnection time.Time `json:"last_connection,omitempty"`
}
type ManagedFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Kind string `json:"kind"`
}
type Component struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Version   string            `json:"version"`
	Ownership string            `json:"ownership"`
	Status    string            `json:"status"`
	Files     []ManagedFile     `json:"files,omitempty"`
	Backups   []string          `json:"backups,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
}
type State struct {
	Schema          int                   `json:"schema"`
	Installed       bool                  `json:"installed"`
	Installer       string                `json:"installer"`
	DataID          string                `json:"data_id"`
	CodexHome       string                `json:"codex_home"`
	Backend         string                `json:"backend"`
	EntrypointDir   string                `json:"entrypoint_dir,omitempty"`
	Codex           Candidate             `json:"codex"`
	Bindings        []Binding             `json:"bindings"`
	Components      map[string]*Component `json:"components"`
	Files           []ManagedFile         `json:"files"`
	SelectedRoots   []string              `json:"selected_roots"`
	RecoveryMissing []string              `json:"recovery_missing,omitempty"`
	LingerOwned     bool                  `json:"linger_owned"`
}
type Backup struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Original string    `json:"original"`
	DataPath string    `json:"data_path"`
	Hash     string    `json:"hash"`
	Mode     uint32    `json:"mode"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	Ref      string    `json:"ref,omitempty"`
	Deleted  bool      `json:"deleted,omitempty"`
}
type InstallOptions struct {
	Backend, CodexPath, CodexVersion, EntrypointDir string
	Roots                                           []string
	Yes, DryRun, Force, Linger                      bool
	Components                                      []string
	ComponentOptions                                map[string]ComponentOptions
}
type ComponentOptions struct {
	Path, Version, PasswordFile, Port, MediaDir, Shell string
	ShellIntegration                                   bool
	InstallNode                                        bool
	Yes, DryRun                                        bool
}
type App struct {
	P        Paths
	S        State
	Out, Err io.Writer
	// Injectable only for isolated tests. Production uses exec.CommandContext.
	RunCommand func(context.Context, string, []string, []string) ([]byte, error)
}

// The UI invokes these same operations as CLI. No UI mutation logic.
type UIActions struct {
	Inventory    func(context.Context) (Inventory, error)
	Status       func(context.Context) string
	Install      func(context.Context, InstallOptions) error
	Migrate      func(context.Context, InstallOptions) error
	Diagnose     func(context.Context) string
	Repair       func(context.Context) error
	Service      func(context.Context, string, bool) error
	Update       func(context.Context, string, bool) error
	Rollback     func(context.Context, string, bool) error
	Uninstall    func(context.Context, bool) error
	Component    func(context.Context, string, string, ComponentOptions) error
	Backups      func() ([]Backup, error)
	BackupAction func(context.Context, string, string, bool) error
	DataAction   func(context.Context, string, string) error
	Logs         func() string
}
