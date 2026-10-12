package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/ansi"
)

// Processes belong to pcx, independently of any Herdr server or output view.
type processSession struct {
	Name    string            `json:"name"`
	Config  string            `json:"config"`
	Derived bool              `json:"derived"`
	Runs    map[string]Window `json:"runs"`
}

func processStateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "pcx", "processes")
	return dir, os.MkdirAll(dir, 0o700)
}

func sessionPath(name string) (string, error) {
	dir, err := processStateDir()
	return filepath.Join(dir, "session-"+hash([]byte(name))+".json"), err
}
func (s *Session) record() (processSession, error) {
	path, err := sessionPath(s.cfg.Name)
	if err != nil {
		return processSession{}, err
	}
	var record processSession
	err = readJSON(path, &record)
	return record, err
}
func saveSession(record processSession) error {
	path, err := sessionPath(record.Name)
	if err != nil {
		return err
	}
	return writeJSON(path, record)
}
func processSessions() ([]processSession, error) {
	dir, err := processStateDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "session-*.json"))
	if err != nil {
		return nil, err
	}
	var records []processSession
	for _, path := range paths {
		var record processSession
		if err := readJSON(path, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}
func (s *Session) Exists() bool { v, _ := sessionCall(s, s.exists); return v }
func (s *Session) exists() bool { _, err := s.record(); return err == nil }
func (s *Session) Ensure() (bool, error) {
	type result struct {
		created bool
		err     error
	}
	v, err := sessionCall(s, func() result { c, e := s.ensure(); return result{c, e} })
	if err != nil {
		return false, err
	}
	return v.created, v.err
}
func (s *Session) ensure() (bool, error) {
	if _, err := s.record(); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return true, saveSession(processSession{Name: s.cfg.Name, Config: s.cfg.Path, Derived: s.cfg.Derived, Runs: map[string]Window{}})
}
func (s *Session) Adopt() error {
	err, managerErr := sessionCall(s, s.adopt)
	if managerErr != nil {
		return managerErr
	}
	return err
}
func (s *Session) adopt() error {
	if !s.cfg.Derived {
		return nil
	}
	records, err := processSessions()
	if err != nil {
		return err
	}
	mine, err := s.record()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if mine.Runs == nil {
		mine = processSession{Name: s.cfg.Name, Config: s.cfg.Path, Derived: true, Runs: map[string]Window{}}
	}
	for _, other := range records {
		if other.Name == mine.Name || !other.Derived || other.Config != s.cfg.Path {
			continue
		}
		for name, w := range other.Runs {
			if s.cfg.Processes[name] == nil {
				continue
			}
			if current, ok := mine.Runs[name]; ok && (!windowState(current).Dead || windowState(w).Dead) {
				continue
			}
			mine.Runs[name] = w
			delete(other.Runs, name)
		}
		if err := saveSession(mine); err != nil {
			return err
		}
		if len(other.Runs) == 0 {
			path, err := sessionPath(other.Name)
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if err := saveSession(other); err != nil {
			return err
		}
	}
	return nil
}
func windowState(w Window) Window {
	var state runState
	if readJSON(w.StatePath, &state) == nil {
		w.PID, w.Dead, w.ExitCode, w.Signal = state.PID, state.Dead, state.ExitCode, state.Signal
		if !w.Dead && (w.PID <= 1 || syscall.Kill(w.PID, 0) != nil) {
			w.Dead = true
		}
	}
	return w
}
func (s *Session) ownWindows() map[string]Window {
	record, err := s.record()
	wins := map[string]Window{}
	if err != nil {
		return wins
	}
	for name, w := range record.Runs {
		wins[name] = windowState(w)
	}
	return wins
}
func (s *Session) windowsWith(t Table) map[string]Window {
	_ = s.adopt()
	wins := s.ownWindows()
	s.external(t, wins)
	return wins
}
func (s *Session) Kill() error {
	err, merr := sessionCall(s, s.kill)
	if merr != nil {
		return merr
	}
	return err
}
func (s *Session) kill() error {
	for _, w := range s.ownWindows() {
		if !w.Dead {
			signalTree(w.PID, syscall.SIGKILL)
		}
	}
	path, err := sessionPath(s.cfg.Name)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *Session) start(name string) string {
	p := s.cfg.Processes[name]
	if p == nil {
		return name + ": unknown process"
	}
	before := s.ownWindows()
	if err := s.adopt(); err != nil {
		return name + ": " + err.Error()
	}
	if w, ok := s.windowsWith(psTable())[name]; ok && !w.Dead {
		if w.External {
			return fmt.Sprintf("%s: already running outside pcx (pid %d)", name, w.PID)
		}
		if b, ok := before[name]; !ok || b.Dead {
			return fmt.Sprintf("%s: adopted the copy already running for this config (pid %d)", name, w.PID)
		}
		return name + ": already running"
	}
	if _, err := s.ensure(); err != nil {
		return name + ": " + err.Error()
	}
	w := s.ownWindows()[name]
	w.Name = name
	if err := s.launch(w, p); err != nil {
		return name + ": " + err.Error()
	}
	return name + ": started"
}
func (s *Session) logs(name string, lines int) (string, error) {
	if lines <= 0 {
		return "", fmt.Errorf("logs: line count must be positive")
	}
	w, ok := s.ownWindows()[name]
	if !ok {
		return "", fmt.Errorf("%s: no captured output — not started by pcx", name)
	}
	out, _, err := readTail(w.LogPath, lines)
	if err != nil {
		return "", err
	}
	return ansi.Strip(strings.ReplaceAll(string(out), "\r\n", "\n")), nil
}
