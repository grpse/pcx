package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *Session) paneExists(id string) bool {
	if id == "" {
		return false
	}
	return s.views.call(nil, "pane", "get", id) == nil
}

func (s *Session) splitCommand(target, direction, command string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("this view needs pcx to run inside Herdr")
	}
	var created herdrCreated
	if err := s.views.call(&created, "pane", "split", target, "--direction", direction, "--cwd", s.cfg.Dir, "--no-focus"); err != nil {
		return "", err
	}
	if err := s.views.runCommand(created.Pane.ID, command); err != nil {
		_ = s.views.call(nil, "pane", "close", created.Pane.ID)
		return "", err
	}
	return created.Pane.ID, nil
}

func (s *Session) viewCommand(name string) (string, string, error) {
	w, ok := s.ownWindows()[name]
	if !ok {
		return "", "", fmt.Errorf("%s: no captured output — not started by pcx", name)
	}
	dir, err := s.backend.stateDir()
	if err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(dir, "view-*.json")
	if err != nil {
		return "", "", err
	}
	path := f.Name()
	f.Close()
	if err := writeJSON(path, viewSpec{LogPath: w.LogPath}); err != nil {
		return "", "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	return shellQuote(exe) + " __pcx_view " + shellQuote(path), path, nil
}

func (s *Session) open(name, mode string) (*exec.Cmd, error) {
	if mode != OpenPane && mode != OpenTab && mode != OpenWorkspace {
		return nil, fmt.Errorf("unknown output mode %q", mode)
	}
	command, path, err := s.viewCommand(name)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			os.Remove(path)
		}
	}()
	if s.clientPane != "" && mode == OpenPane {
		id, err := s.splitCommand(s.clientPane, "right", command)
		if err != nil {
			return nil, err
		}
		s.lastOutput, opened = id, true
		return nil, nil
	}
	if err := s.views.ensureServer(); err != nil {
		return nil, err
	}
	var created herdrCreated
	if s.clientPane == "" || mode == OpenWorkspace {
		if err := s.views.call(&created, "workspace", "create", "--cwd", s.cfg.Dir, "--label", name+" output", "--no-focus"); err != nil {
			return nil, err
		}
	} else {
		snap, err := s.views.snapshot()
		if err != nil {
			return nil, err
		}
		var workspaceID string
		for _, p := range snap.Panes {
			if p.ID == s.clientPane {
				workspaceID = p.WorkspaceID
			}
		}
		if workspaceID == "" {
			return nil, fmt.Errorf("calling Herdr pane no longer exists")
		}
		if err := s.views.call(&created, "tab", "create", "--workspace", workspaceID, "--cwd", s.cfg.Dir, "--label", name+" output", "--no-focus"); err != nil {
			return nil, err
		}
	}
	if err := s.views.runCommand(created.RootPane.ID, command); err != nil {
		if s.clientPane == "" || mode == OpenWorkspace {
			_ = s.views.call(nil, "workspace", "close", created.Workspace.ID)
		} else {
			_ = s.views.call(nil, "tab", "close", created.Tab.ID)
		}
		return nil, err
	}
	s.lastOutput, opened = created.RootPane.ID, true
	if s.clientPane == "" || mode == OpenWorkspace {
		if err := s.views.call(nil, "workspace", "focus", created.Workspace.ID); err != nil {
			return nil, err
		}
	}
	if err := s.views.call(nil, "tab", "focus", created.Tab.ID); err != nil {
		return nil, err
	}
	if s.clientPane == "" {
		return s.views.command(), nil
	}
	return nil, nil
}

func (s *Session) peeking() bool { return s.paneExists(s.peekPane) }
func (s *Session) peek(name string) error {
	if s.clientPane == "" {
		return fmt.Errorf("peek needs pcx to run inside Herdr")
	}
	w, ok := s.ownWindows()[name]
	if !ok {
		return fmt.Errorf("no captured output — not started by pcx")
	}
	if s.peeking() {
		return writeJSON(s.peekFile, viewSpec{LogPath: w.LogPath})
	}
	command, path, err := s.viewCommand(name)
	if err != nil {
		return err
	}
	id, err := s.splitCommand(s.clientPane, "right", command)
	if err != nil {
		os.Remove(path)
		return err
	}
	s.peekPane, s.peekFile = id, path
	return nil
}
func (s *Session) peekClose() {
	s.hideCommands()
	if s.paneExists(s.peekPane) {
		_ = s.views.call(nil, "pane", "close", s.peekPane)
	}
	if s.peekFile != "" {
		_ = os.Remove(s.peekFile)
	}
	s.peekPane, s.peekFile = "", ""
}

func (s *Session) headerFile() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("pcx-header-%s-%d", sanitize(s.cfg.Name), os.Getpid()))
}
func (s *Session) writeHeader(lines []string) {
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	_ = os.WriteFile(s.headerFile(), []byte(body), 0o600)
}
func (s *Session) headerWatch() string {
	return "while :; do printf '\\033[H\\033[2J'; cat " + shellQuote(s.headerFile()) + " 2>/dev/null; sleep 1; done"
}
func (s *Session) showCommands(focus string) error {
	lines := s.executedCommands(focus)
	if len(lines) == 0 {
		return fmt.Errorf("no executed commands")
	}
	s.writeHeader(lines)
	if s.paneExists(s.cmdPane) {
		return nil
	}
	target := s.peekPane
	if target == "" {
		target = s.lastOutput
	}
	if target == "" {
		return fmt.Errorf("no output pane to split")
	}
	id, err := s.splitCommand(target, "down", s.headerWatch())
	if err != nil {
		return err
	}
	s.cmdPane = id
	return nil
}
func (s *Session) hideCommands() {
	if s.paneExists(s.cmdPane) {
		_ = s.views.call(nil, "pane", "close", s.cmdPane)
	}
	s.cmdPane = ""
	_ = os.Remove(s.headerFile())
}
func (s *Session) refreshCommands(focus string) {
	if !s.paneExists(s.cmdPane) {
		s.cmdPane = ""
		return
	}
	s.writeHeader(s.executedCommands(focus))
}

func (s *Session) toggleDebug() (bool, error) {
	if s.paneExists(s.debugPane) {
		s.debugClose()
		return false, nil
	}
	if s.debugPath() == "" {
		return false, fmt.Errorf("debug log is unavailable")
	}
	id, err := s.splitCommand(s.clientPane, "down", "exec tail -n 200 -f "+shellQuote(s.debugPath()))
	if err != nil {
		return false, err
	}
	s.debugPane = id
	return true, nil
}
func (s *Session) debugClose() {
	if s.paneExists(s.debugPane) {
		_ = s.views.call(nil, "pane", "close", s.debugPane)
	}
	s.debugPane = ""
	s.debugf("debug pane closed")
}
