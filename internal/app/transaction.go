package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type TxResource struct {
	Path, BackupID, Hash string
	Created              bool
	Deleted              bool
}
type Transaction struct {
	ID, Action     string
	Started        time.Time
	Previous       State
	Resources      []TxResource
	NewBindings    []string
	Stage          string
	LegacyUnits    []LegacyUnit
	LegacySettings []LegacySetting
	LegacyControl  string
	LegacyDaemon   string
}
type LegacyUnit struct {
	Name            string
	Active, Enabled bool
}
type LegacySetting struct{ Path, Value string }

func hasPending(data string) bool {
	_, e := os.Stat(filepath.Join(data, "transactions", "active.json"))
	return e == nil
}
func (a *App) saveTx(t *Transaction) error {
	b, e := json.MarshalIndent(t, "", "  ")
	if e != nil {
		return e
	}
	return AtomicWrite(filepath.Join(a.P.Data, "transactions", "active.json"), b, 0600)
}
func (a *App) begin(action string) (*Transaction, error) {
	if e := a.EnsureData(); e != nil {
		return nil, e
	}
	if hasPending(a.P.Data) {
		return nil, errors.New("存在未完成操作，先 recover")
	}
	raw, _ := json.Marshal(a.S)
	var old State
	_ = json.Unmarshal(raw, &old)
	t := &Transaction{ID: ID(), Action: action, Started: time.Now().UTC(), Previous: old}
	return t, a.saveTx(t)
}
func (a *App) txWrite(t *Transaction, path string, data []byte, mode os.FileMode, kind string) error {
	r := TxResource{Path: path, Hash: HashBytes(data), Created: true}
	if _, e := os.Lstat(path); e == nil {
		b, e := a.BackupFile(path, "transaction", t.ID)
		if e != nil {
			return e
		}
		r.BackupID = b.ID
		r.Created = false
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	t.Resources = append(t.Resources, r)
	if e := a.saveTx(t); e != nil {
		return e
	}
	if e := a.ReplaceBytes(path, data, mode); e != nil {
		return e
	}
	for i, f := range a.S.Files {
		if f.Path == path {
			a.S.Files[i] = ManagedFile{Path: path, Hash: r.Hash, Kind: kind}
			return a.Save()
		}
	}
	a.S.Files = append(a.S.Files, ManagedFile{Path: path, Hash: r.Hash, Kind: kind})
	return a.Save()
}
func (a *App) commit(t *Transaction) error {
	b, e := json.MarshalIndent(t, "", "  ")
	if e != nil {
		return e
	}
	if e = AtomicWrite(filepath.Join(a.P.Data, "history", t.ID+".json"), b, 0600); e != nil {
		return e
	}
	bs, e := a.Backups()
	if e != nil {
		return e
	}
	for _, backup := range bs {
		if backup.Ref == t.ID {
			backup.Ref = ""
			if t.Action == "update" {
				backup.Ref = "rollback:" + t.ID
			}
			if e = a.SaveBackup(backup); e != nil {
				return e
			}
		}
	}
	return os.Remove(filepath.Join(a.P.Data, "transactions", "active.json"))
}
