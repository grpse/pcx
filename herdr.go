package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// An explicit session overrides inherited Herdr context. Outside Herdr, use a
// dedicated server so pcx never operates on somebody else's focused terminal.
type herdrBackend struct {
	Session string `json:"session"`
	Socket  string `json:"socket,omitempty"`
}

func newHerdrBackend() herdrBackend {
	if name := os.Getenv("PCX_HERDR_SESSION"); name != "" {
		return herdrBackend{Session: name}
	}
	if os.Getenv("HERDR_ENV") == "1" {
		return currentHerdrBackend()
	}
	return herdrBackend{Session: "pcx"}
}

func currentHerdrBackend() herdrBackend {
	name := os.Getenv("HERDR_SESSION")
	if name == "" {
		name = "default"
	}
	return herdrBackend{Session: name, Socket: os.Getenv("HERDR_SOCKET_PATH")}
}

func (h herdrBackend) command(args ...string) *exec.Cmd {
	if h.Session != "" {
		args = append([]string{"--session", h.Session}, args...)
	}
	c := exec.Command("herdr", args...)
	if h.Socket != "" {
		c.Env = mergedEnvironment(os.Environ(), []string{"HERDR_SOCKET_PATH=" + h.Socket})
	}
	return c
}

func (h herdrBackend) output(args ...string) ([]byte, error) {
	c := h.command(args...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cancelCmd := exec.CommandContext(ctx, c.Path, c.Args[1:]...)
	cancelCmd.Env = c.Env
	out, err := cancelCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (h herdrBackend) call(dst any, args ...string) error {
	out, err := h.output(args...)
	if err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return fmt.Errorf("herdr response: %w", err)
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("herdr: missing result in response")
	}
	return json.Unmarshal(response.Result, dst)
}

// New terminals can accept input while the interactive shell is still reading
// its startup files. In particular, Bash can discard the submitted Enter while
// initializing readline. Wait for a settled shell prompt before typing.
func (h herdrBackend) waitForShell(paneID string) error {
	deadline := time.Now().Add(15 * time.Second)
	var previous string
	var settled time.Time
	for time.Now().Before(deadline) {
		var response struct {
			ProcessInfo struct {
				ShellPID   int `json:"shell_pid"`
				Foreground []struct {
					PID int `json:"pid"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		}
		if err := h.call(&response, "pane", "process-info", "--pane", paneID); err != nil {
			return err
		}
		info := response.ProcessInfo
		idle := info.ShellPID > 0 && len(info.Foreground) == 1 && info.Foreground[0].PID == info.ShellPID
		if idle {
			out, err := h.output("pane", "read", paneID, "--source", "recent-unwrapped", "--format", "text", "--lines", "40")
			if err != nil {
				return err
			}
			prompt := strings.TrimRight(string(out), " \t\r")
			if prompt != "" && !strings.HasSuffix(prompt, "\n") {
				if previous != string(out) || settled.IsZero() {
					previous = string(out)
					settled = time.Now()
				}
				if time.Since(settled) >= 300*time.Millisecond {
					return nil
				}
			} else {
				settled = time.Time{}
			}
		} else {
			settled = time.Time{}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("Herdr pane %s: timed out waiting for the shell prompt", paneID)
}

// Confirm execution as well as submission. Pane creation or echoed command
// text alone does not establish that the opening command actually ran.
func (h herdrBackend) runCommand(paneID, command string) error {
	if err := h.waitForShell(paneID); err != nil {
		return err
	}
	dir, err := h.stateDir()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "started-*")
	if err != nil {
		return err
	}
	marker := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(marker)
		return err
	}
	defer os.Remove(marker)
	script := "printf started > " + shellQuote(marker) + "; " + command
	if err := h.call(nil, "pane", "run", paneID, "/bin/sh -c "+shellQuote(script)); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(marker); err == nil && info.Size() > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("Herdr pane %s: opening command was submitted but did not execute", paneID)
}

type herdrWorkspace struct {
	ID     string            `json:"workspace_id"`
	Label  string            `json:"label"`
	Tokens map[string]string `json:"tokens"`
}
type herdrTab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}
type herdrPane struct {
	ID          string            `json:"pane_id"`
	TabID       string            `json:"tab_id"`
	WorkspaceID string            `json:"workspace_id"`
	Tokens      map[string]string `json:"tokens"`
}
type herdrSnapshot struct {
	Workspaces []herdrWorkspace `json:"workspaces"`
	Tabs       []herdrTab       `json:"tabs"`
	Panes      []herdrPane      `json:"panes"`
}
type herdrCreated struct {
	Workspace herdrWorkspace `json:"workspace"`
	Tab       herdrTab       `json:"tab"`
	Pane      herdrPane      `json:"pane"`
	RootPane  herdrPane      `json:"root_pane"`
}

func (h herdrBackend) snapshot() (herdrSnapshot, error) {
	var response struct {
		Snapshot herdrSnapshot `json:"snapshot"`
	}
	err := h.call(&response, "api", "snapshot")
	return response.Snapshot, err
}

func (h herdrBackend) stateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	identity := h.Session
	if identity == "" {
		identity = h.Socket
	}
	dir := filepath.Join(base, "pcx", hash([]byte(identity+"\x00"+os.Getenv("HERDR_CONFIG_PATH"))))
	return dir, os.MkdirAll(dir, 0o700)
}

func (h herdrBackend) lock() (*os.File, error) {
	dir, err := processStateDir()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "manager.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockManager(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

func (h herdrBackend) ensureServer() error {
	if _, err := h.snapshot(); err == nil {
		return nil
	}
	if h.Session == "" {
		return fmt.Errorf("cannot reach the current Herdr server")
	}
	// Only start the dedicated named server, never restart an existing server.
	dir, err := h.stateDir()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	c := h.command("server")
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	_ = c.Process.Release()
	var last error
	for i := 0; i < 50; i++ {
		if _, last = h.snapshot(); last == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("start Herdr session %s: %w (see %s)", h.Session, last, log.Name())
}

func signalTree(pid int, sig syscall.Signal) {
	if pid <= 1 {
		return
	}
	kids := descendants(psTable(), pid)
	_ = syscall.Kill(pid, sig)
	for _, kid := range kids {
		_ = syscall.Kill(kid, sig)
	}
}
func (s *Session) stop(name string, sig syscall.Signal) string {
	w, ok := s.windowsWith(psTable())[name]
	if !ok || w.Dead {
		return name + ": not running"
	}
	if w.LockPath != "" {
		if err := syscall.Kill(w.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Sprintf("%s: kill: %v", name, err)
		}
		if err := os.Remove(w.LockPath); err != nil && !os.IsNotExist(err) {
			return fmt.Sprintf("%s: killed pid %d; remove lock: %v", name, w.PID, err)
		}
		return fmt.Sprintf("%s: sent %v", name, syscall.SIGKILL)
	}
	signalTree(w.PID, sig)
	return fmt.Sprintf("%s: sent %v", name, sig)
}
func (s *Session) restart(name string) string {
	w, ok := s.windowsWith(psTable())[name]
	if ok && !w.Dead {
		s.stop(name, syscall.SIGTERM)
		for i := 0; i < 20; i++ {
			if syscall.Kill(w.PID, 0) != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if syscall.Kill(w.PID, 0) == nil {
			signalTree(w.PID, syscall.SIGKILL)
		}
		// Wait for the runner to publish the final state and return to the shell.
		for i := 0; i < 100; i++ {
			var state runState
			if readJSON(w.StatePath, &state) == nil && state.Dead {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return s.start(name)
}
