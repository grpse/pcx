package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// holder is an idle shell window that keeps the session alive when every
// managed process is stopped.
const holder = "__pcx"

type Session struct{ cfg *Config }

func NewSession(c *Config) *Session { return &Session{cfg: c} }

func (s *Session) target() string { return "=" + s.cfg.Name }

func tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s *Session) Exists() bool {
	return exec.Command("tmux", "has-session", "-t", s.target()).Run() == nil
}

// Ensure creates the session if needed; reports whether it created it.
func (s *Session) Ensure() (bool, error) {
	if s.Exists() {
		return false, nil
	}
	if _, err := tmux("new-session", "-d", "-s", s.cfg.Name, "-n", holder); err != nil {
		return false, err
	}
	// lets a later run find this session again after the config is edited
	tmux("set-environment", "-t", s.cfg.Name, "PCX_CONFIG", s.cfg.Path)
	return true, nil
}

// Adopt reattaches to the session already running this config file when the
// file has been edited since: the name is a content hash, so it moved. Without
// this an edit would orphan every running process.
func (s *Session) Adopt() {
	if !s.cfg.Derived || s.Exists() {
		return
	}
	out, err := tmux("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(name, "pcx-") || strings.Contains(name, "-view-") {
			continue
		}
		env, err := tmux("show-environment", "-t", name, "PCX_CONFIG")
		if err != nil || strings.TrimSpace(env) != "PCX_CONFIG="+s.cfg.Path {
			continue
		}
		tmux("rename-session", "-t", name, s.cfg.Name)
		return
	}
}

// Kill destroys the session and any view sessions grouped with it. Views share
// the windows, so leaving one alive would keep the processes alive too.
func (s *Session) Kill() error {
	out, _ := tmux("list-sessions", "-F", "#{session_name}")
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(name, s.cfg.Name+"-view-") {
			tmux("kill-session", "-t", "="+name)
		}
	}
	if !s.Exists() {
		return nil
	}
	_, err := tmux("kill-session", "-t", s.target())
	return err
}

type Window struct {
	Name     string
	PID      int
	Dead     bool
	ExitCode int    // -1 when unknown
	Signal   string // signal name that killed the pane ("term"), empty when none
}

// Status is the one-word state shown by both the CLI and the TUI.
func (w Window) Status() string {
	switch {
	case !w.Dead:
		return "running"
	case w.Signal != "":
		return "signal(" + w.Signal + ")"
	case w.ExitCode >= 0:
		return fmt.Sprintf("exited(%d)", w.ExitCode)
	default:
		return "exited"
	}
}

func (s *Session) Windows() map[string]Window {
	wins := map[string]Window{}
	if !s.Exists() {
		return wins
	}
	out, err := tmux("list-windows", "-t", s.target(), "-F",
		"#{window_name}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}")
	if err != nil {
		return wins
	}
	for _, line := range strings.Split(out, "\n") {
		// the status/signal fields are empty for live panes, so keep them
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 5)
		if len(f) < 5 || f[0] == holder {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		code := -1
		if n, err := strconv.Atoi(strings.TrimSpace(f[3])); err == nil {
			code = n
		}
		wins[f[0]] = Window{Name: f[0], PID: pid, Dead: f[2] == "1", ExitCode: code,
			Signal: strings.TrimSpace(f[4])}
	}
	return wins
}

// shellCmd applies the restart policy without a supervisor: the loop lives
// inside the pane, so it keeps working when nothing is attached.
func shellCmd(p *Proc) string {
	switch p.Availability.Restart {
	case "always":
		// subshell, so an `exit` in the command cannot kill the loop
		return fmt.Sprintf("while :; do ( %s ); sleep 1; done", p.Command)
	case "on_failure", "on-failure":
		return fmt.Sprintf("until ( %s ); do sleep 1; done", p.Command)
	default:
		return p.Command
	}
}

func (s *Session) Start(name string) string {
	p := s.cfg.Processes[name]
	if p == nil {
		return name + ": unknown process"
	}
	if _, err := s.Ensure(); err != nil {
		return name + ": " + err.Error()
	}
	w, ok := s.Windows()[name]
	if ok && !w.Dead {
		return name + ": already running"
	}
	env := []string{}
	for _, e := range p.Environment {
		env = append(env, "-e", e)
	}
	cmd := shellCmd(p)
	var err error
	if ok { // dead window: respawn in place, keeping its position and scrollback
		args := append([]string{"respawn-window", "-k", "-c", p.WorkingDir}, env...)
		_, err = tmux(append(args, "-t", s.target()+":"+name, cmd)...)
	} else {
		// single tmux invocation, so the window cannot exit before
		// remain-on-exit is set and leave us with nothing to inspect
		args := append([]string{"new-window", "-d", "-c", p.WorkingDir}, env...)
		args = append(args, "-t", s.target()+":", "-n", name, cmd,
			";", "set-option", "-w", "-t", s.target()+":"+name, "remain-on-exit", "on")
		_, err = tmux(args...)
	}
	if err != nil {
		return name + ": " + err.Error()
	}
	return name + ": started"
}

func (s *Session) Stop(name string, sig syscall.Signal) string {
	w, ok := s.Windows()[name]
	if !ok || w.Dead {
		return name + ": not running"
	}
	kids := descendants(psTable(), w.PID)
	// The pane leader owns its process group; that covers the normal case.
	_ = syscall.Kill(-w.PID, sig)
	for _, pid := range kids { // anything that escaped the group (setsid daemons)
		_ = syscall.Kill(pid, sig)
	}
	return fmt.Sprintf("%s: sent %v", name, sig)
}

func (s *Session) Restart(name string) string {
	s.Stop(name, syscall.SIGTERM)
	return s.Start(name) // respawn -k finishes off whatever survived
}

// Open modes for a process's live output.
const (
	OpenPane      = "pane"      // split the current window
	OpenTab       = "tab"       // new window in the current session
	OpenWorkspace = "workspace" // move the client to the pcx session
)

var OpenModes = []string{OpenPane, OpenTab, OpenWorkspace}

// Open shows a process's live output. It returns a command only when the
// terminal has to be handed over (attaching from outside tmux); the tmux modes
// do their work immediately and return nil.
//
// Pane and tab attach a nested client to a session grouped with ours, so the
// process keeps its own window: closing the view cannot kill the process, which
// linking the real window into the user's session would risk.
func (s *Session) Open(name, mode string) (*exec.Cmd, error) {
	win := s.target() + ":" + name
	if os.Getenv("TMUX") == "" {
		// nothing to split or tab into: every mode collapses to attaching
		return exec.Command("tmux", "attach-session", "-t", win, ";", "select-window", "-t", win), nil
	}
	switch mode {
	case OpenPane, OpenTab:
		view := s.cfg.Name + "-view-" + name
		if exec.Command("tmux", "has-session", "-t", "="+view).Run() != nil {
			if _, err := tmux("new-session", "-d", "-t", s.cfg.Name, "-s", view); err != nil {
				return nil, err
			}
			// plain name: set-option rejects the '=' exact-match prefix
			tmux("set-option", "-t", view, "status", "off")
		}
		if _, err := tmux("select-window", "-t", "="+view+":"+name); err != nil {
			return nil, err
		}
		// TMUX= lets the inner client attach; the session dies with it
		cmd := fmt.Sprintf("TMUX= tmux attach -t '=%s'; tmux kill-session -t '=%s' 2>/dev/null", view, view)
		if mode == OpenPane {
			_, err := tmux("split-window", "-h", cmd)
			return nil, err
		}
		_, err := tmux("new-window", "-n", name, cmd)
		return nil, err
	default:
		_, err := tmux("switch-client", "-t", win)
		return nil, err
	}
}

// ---------------------------------------------------------------- ps

type PS struct {
	PID, PPID int
	CPU       float64
	RSS       int // kilobytes
	Args      string
}

type Table struct {
	Procs map[int]PS
	Kids  map[int][]int
}

func psTable() Table {
	t := Table{Procs: map[int]PS{}, Kids: map[int][]int{}}
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,pcpu=,rss=,args=").Output()
	if err != nil {
		return t
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		cpu, _ := strconv.ParseFloat(f[2], 64)
		rss, _ := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		t.Procs[pid] = PS{pid, ppid, cpu, rss, strings.Join(f[4:], " ")}
		t.Kids[ppid] = append(t.Kids[ppid], pid)
	}
	return t
}

func descendants(t Table, pid int) []int {
	var out []int
	stack := append([]int{}, t.Kids[pid]...)
	for len(stack) > 0 {
		p := stack[0]
		stack = stack[1:]
		out = append(out, p)
		stack = append(stack, t.Kids[p]...)
	}
	return out
}

// totals sums cpu and rss over a process and everything it spawned.
func totals(t Table, pid int) (cpu float64, rss int) {
	for _, p := range append([]int{pid}, descendants(t, pid)...) {
		if info, ok := t.Procs[p]; ok {
			cpu += info.CPU
			rss += info.RSS
		}
	}
	return
}

// Logs returns a process's scrollback without tmux's blank pane padding.
func (s *Session) Logs(name string, lines int) (string, error) {
	out, err := tmux("capture-pane", "-p", "-S", fmt.Sprintf("-%d", lines), "-t", s.target()+":"+name)
	if err != nil {
		return "", err
	}
	// tmux pads the capture out to the pane height; squeeze that away
	return strings.TrimRight(blankRun.ReplaceAllString(out, "\n\n"), " \n\t") + "\n", nil
}

var blankRun = regexp.MustCompile(`\n{3,}`)

func human(kb int) string {
	switch f := float64(kb); {
	case f < 1024:
		return fmt.Sprintf("%dK", kb)
	case f < 1024*1024:
		return fmt.Sprintf("%.1fM", f/1024)
	default:
		return fmt.Sprintf("%.1fG", f/1024/1024)
	}
}
