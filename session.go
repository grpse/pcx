package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Session struct {
	cfg        *Config
	peekPane   string // the output pane beside the TUI
	cmdPane    string // M's command-list pane below it
	debugPane  string // D's internal diagnostics pane below the TUI
	debugFile  *os.File
	clientPane string
	requests   chan managerRequest
	backend    herdrBackend
	views      herdrBackend
	peekFile   string
	lastOutput string
}

type managerRequest struct {
	name string
	run  func()
	done chan error
}

func NewSession(c *Config) *Session {
	debugPath := filepath.Join(os.TempDir(), fmt.Sprintf("pcx-debug-%s-%d.log", sanitize(c.Name), os.Getpid()))
	_ = os.Remove(debugPath)
	debugFile, _ := os.OpenFile(debugPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	// The buffer lets producers enqueue while a command is running. The single
	// consumer below remains the only execution context for manager work.
	s := &Session{
		cfg:        c,
		requests:   make(chan managerRequest, 256),
		debugFile:  debugFile,
		clientPane: os.Getenv("HERDR_PANE_ID"),
		backend:    newHerdrBackend(),
		views:      newHerdrBackend(),
	}
	if os.Getenv("HERDR_ENV") == "1" {
		s.views = currentHerdrBackend()
	}
	s.debugf("manager created shell=%s client_pane=%s", executionShell(), s.clientPane)
	go s.manage()
	return s
}

// manage is the only goroutine that communicates with Herdr and the process
// table for this session. Callers submit work and wait for its result, so a
// second command can never enter halfway through the first one's transaction.
func (s *Session) manage() {
	for req := range s.requests {
		var err error
		started := time.Now()
		s.debugf("start %s queued=%d", req.name, len(s.requests))
		func() {
			defer func() {
				if v := recover(); v != nil {
					err = fmt.Errorf("process manager recovered from panic: %v", v)
				}
			}()
			lock, lockErr := s.backend.lock()
			if lockErr != nil {
				err = lockErr
				return
			}
			defer unlockManager(lock)
			req.run()
		}()
		s.debugf("done  %s duration=%s error=%v", req.name, time.Since(started).Round(time.Millisecond), err)
		req.done <- err
	}
}

func sessionCall[T any](s *Session, fn func() T) (result T, err error) {
	done := make(chan error, 1)
	name := managerCaller()
	s.debugf("queue %s queued=%d", name, len(s.requests))
	s.requests <- managerRequest{
		name: name,
		run:  func() { result = fn() },
		done: done,
	}
	err = <-done
	s.debugf("result %s %s", name, debugResult(result))
	return
}

func managerCaller() string {
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	name := runtime.FuncForPC(pc).Name()
	if at := strings.LastIndex(name, "."); at >= 0 {
		name = name[at+1:]
	}
	return name
}

func debugResult(v any) string {
	switch value := v.(type) {
	case Snapshot:
		return fmt.Sprintf("windows=%d processes=%d peeking=%t", len(value.Windows), len(value.Table.Procs), value.Peeking)
	case map[string]Window:
		return fmt.Sprintf("windows=%d", len(value))
	}
	text := fmt.Sprintf("%v", v)
	if len(text) > 500 {
		text = text[:499] + "…"
	}
	return text
}

func (s *Session) debugf(format string, args ...any) {
	if s.debugFile == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(s.debugFile, "%s %s\n", time.Now().Format("15:04:05.000"), line)
}

func (s *Session) debugPath() string {
	if s.debugFile == nil {
		return ""
	}
	return s.debugFile.Name()
}

type Window struct {
	LockPath  string // PID lock authorizing global management
	LogPath   string
	StatePath string
	Name      string
	PID       int
	Dead      bool
	ExitCode  int    // -1 when unknown
	Signal    string // signal name that killed the process ("term"), empty when none
	External  bool   // running outside our session; no captured output
}

type Snapshot struct {
	Windows map[string]Window
	Table   Table
	Peeking bool
}

func (s *Session) Snapshot() Snapshot {
	v, err := sessionCall(s, s.snapshot)
	if err != nil {
		return Snapshot{Windows: map[string]Window{}, Table: Table{Procs: map[int]PS{}, Kids: map[int][]int{}}}
	}
	return v
}

func (s *Session) snapshot() Snapshot {
	table := psTable()
	return Snapshot{
		Windows: s.windowsWith(table),
		Table:   table,
		Peeking: s.peeking(),
	}
}

// Status is the one-word state shown by both the CLI and the TUI.
func (w Window) Status() string {
	switch {
	case w.LockPath != "":
		return "running"
	case w.External:
		return "external"
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
	wins, err := sessionCall(s, func() map[string]Window { return s.windowsWith(psTable()) })
	if err != nil {
		return map[string]Window{}
	}
	return wins
}

// WindowsWith takes the ps sweep from the caller, so a refresh that already has
// one does not run a second.
func (s *Session) WindowsWith(t Table) map[string]Window {
	wins, err := sessionCall(s, func() map[string]Window { return s.windowsWith(t) })
	if err != nil {
		return map[string]Window{}
	}
	return wins
}

// external fills in processes that are already running some other way — by
// hand, by another tool, by another pcx — so the same process is managed from
// anywhere instead of pcx starting a second copy of it.
//
// Lock-backed matches also include children of other commands or pcx windows.
// The lock contents identify the actual process to display and stop.
func (s *Session) external(t Table, wins map[string]Window) {
	locks := pidLocks(t)
	ours := map[int]bool{}
	// our own process and the shell chain above it: their command lines quote
	// the config's commands back at us
	for pid := os.Getpid(); pid > 1; pid = t.Procs[pid].PPID {
		if ours[pid] {
			break
		}
		ours[pid] = true
	}
	for _, w := range wins {
		for _, pid := range append([]int{w.PID}, descendants(t, w.PID)...) {
			ours[pid] = true
		}
	}
	for _, name := range s.cfg.names() {
		cmd := strings.TrimSpace(s.cfg.Processes[name].Command)
		if cmd == "" {
			continue
		}
		locked := 0
		for pid := range locks {
			if commandMatches(t.Procs[pid].Args, cmd) && (locked == 0 || pid < locked) {
				locked = pid
			}
		}
		if locked != 0 {
			wins[name] = Window{Name: name, PID: locked, External: true, LockPath: locks[locked]}
			continue
		}
		if w, ok := wins[name]; ok && !w.Dead {
			continue
		}
		match := map[int]bool{}
		for pid, p := range t.Procs {
			if !ours[pid] && commandMatches(p.Args, cmd) {
				match[pid] = true
			}
		}
		// keep the root of a matching tree: `sh -c cmd` and its child both match
		best := 0
		for pid := range match {
			if match[t.Procs[pid].PPID] {
				continue
			}
			if best == 0 || pid < best {
				best = pid
			}
		}
		if best != 0 {
			wins[name] = Window{Name: name, PID: best, External: true}
		}
	}
}

// commandMatches accepts the exact program arguments or an exact shell -c
// payload, never an arbitrary suffix or a portion of a compound command.
func commandMatches(args, command string) bool {
	if args == command {
		return true
	}
	for _, shell := range []string{"sh", "bash", "zsh", "dash", "ksh"} {
		for _, prefix := range []string{shell, "/bin/" + shell, "/usr/bin/" + shell} {
			if args == prefix+" -c "+command {
				return true
			}
		}
	}
	return false
}

func pidLocks(t Table) map[int]string {
	locks := map[int]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return locks
	}
	dir := filepath.Join(home, ".pcx", "pids")
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".lock")); err != nil || n <= 1 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			continue
		}
		if _, alive := t.Procs[pid]; alive {
			locks[pid] = path
		}
	}
	return locks
}

// shellCmd applies the restart policy without a supervisor: the loop lives
// inside the detached runner, so it keeps working when nothing is attached.
func shellCmd(p *Proc) string {
	shell := executionShell()
	return asCurrentUser(shellCommand(p, shell, resolvedUserPath()))
}

func shellCommand(p *Proc, shell, path string) string {
	var script string
	if filepath.Base(shell) == "fish" {
		run := shellQuote(shell) + " -l -c " + shellQuote(p.Command)
		switch p.Availability.Restart {
		case "always":
			script = fmt.Sprintf("while true; %s; sleep 1; end", run)
		case "on_failure", "on-failure":
			script = fmt.Sprintf("while not %s; sleep 1; end", run)
		default:
			script = p.Command
		}
	} else {
		switch p.Availability.Restart {
		case "always":
			// subshell, so an `exit` in the command cannot kill the loop
			script = fmt.Sprintf("while :; do ( %s ); sleep 1; done", p.Command)
		case "on_failure", "on-failure":
			script = fmt.Sprintf("until ( %s ); do sleep 1; done", p.Command)
		default:
			script = p.Command
		}
	}
	// Start the same kind of login/interactive shell that invoked pcx. PATH is
	// re-exported inside it so profile setup such as path_helper cannot put
	// /usr/bin ahead of the account's own bins.
	script = withUserPath(script, shell, path)
	return "exec " + shellQuote(shell) + " -l -c " + shellQuote(script)
}

func withUserPath(cmd, shell, path string) string {
	if path == "" {
		return cmd
	}
	if filepath.Base(shell) == "fish" {
		dirs := strings.Split(path, ":")
		for i, dir := range dirs {
			dirs[i] = shellQuote(dir)
		}
		return "set -gx PATH " + strings.Join(dirs, " ") + "; " + cmd
	}
	return "export PATH=" + shellQuote(path) + "; " + cmd
}

func resolvedUserPath() string {
	home := ""
	if u := invokeUser(); u != nil {
		home = u.HomeDir
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	current := os.Getenv("PATH")
	if sh := userShellPath(); sh != "" {
		current = sh + ":" + current
	}
	return userPath(home, current)
}

// userPath puts the account's own bin dirs in front of PATH so a command
// installed for the user wins over /usr/bin after macOS path_helper runs.
func userPath(home, current string) string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string, mustExist bool) {
		if dir == "" || seen[dir] {
			return
		}
		if mustExist {
			fi, err := os.Stat(dir)
			if err != nil || !fi.IsDir() {
				return
			}
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range userBinDirs(home) {
		add(dir, true)
	}
	for _, dir := range strings.Split(current, ":") {
		add(dir, false)
	}
	return strings.Join(out, ":")
}

func userBinDirs(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
	}
}

func userShellPath() string {
	shell := executionShell()
	printPath := `printf '\n__PCX_PATH__%s\n' "$PATH"`
	if filepath.Base(shell) == "fish" {
		printPath = `printf '\n__PCX_PATH__%s\n' (string join : $PATH)`
	}
	out, err := exec.Command(shell, "-l", "-c", printPath).Output()
	if err != nil {
		return ""
	}
	const marker = "__PCX_PATH__"
	text := string(out)
	at := strings.LastIndex(text, marker)
	if at < 0 {
		return ""
	}
	path := text[at+len(marker):]
	if end := strings.IndexByte(path, '\n'); end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}

// executionShell finds the shell that launched pcx rather than assuming that
// $SHELL still describes the current process. Wrappers such as sudo can sit
// between them, so walk the complete parent chain before falling back.
func executionShell() string {
	table := psTable()
	for pid, seen := os.Getppid(), map[int]bool{}; pid > 1 && !seen[pid]; {
		seen[pid] = true
		if p, ok := table.Procs[pid]; ok {
			if sh := supportedShell(strings.Fields(p.Args)); sh != "" {
				return sh
			}
			pid = p.PPID
			continue
		}
		break
	}
	if sh := supportedShell([]string{os.Getenv("SHELL")}); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func supportedShell(argv []string) string {
	if len(argv) == 0 || argv[0] == "" {
		return ""
	}
	candidate := strings.TrimPrefix(argv[0], "-")
	switch filepath.Base(candidate) {
	case "bash", "zsh", "sh", "dash", "ash", "ksh", "mksh", "fish":
	default:
		return ""
	}
	if filepath.IsAbs(candidate) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		return ""
	}
	path, err := exec.LookPath(candidate)
	if err != nil {
		return ""
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// asCurrentUser runs commands as the person who invoked pcx. A root
// server (sudo pcx) would otherwise run every command as root.
func asCurrentUser(cmd string) string {
	u := invokeUser()
	if u == nil || u.Uid == strconv.Itoa(os.Geteuid()) {
		return cmd
	}
	return fmt.Sprintf("sudo -u %s -H -E -- /bin/sh -c %s", shellQuote(u.Username), shellQuote(cmd))
}

func invokeUser() *user.User {
	if os.Geteuid() == 0 {
		if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
			if u, err := user.Lookup(name); err == nil {
				return u
			}
		}
	}
	u, err := user.Current()
	if err != nil {
		return nil
	}
	return u
}

// clientEnv is the invoking process's environment, minus the multiplexer vars that
// would make a pane think it is the TUI client.
func clientEnv() []string {
	return envFlags(withResolvedPath(os.Environ()), nil)
}

func windowEnv(p *Proc) []string {
	base := withResolvedPath(os.Environ())
	if u := invokeUser(); u != nil && u.Uid != strconv.Itoa(os.Geteuid()) {
		base = append(base, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	}
	return envFlags(base, p.Environment)
}

func withResolvedPath(env []string) []string {
	path := resolvedUserPath()
	if path == "" {
		return env
	}
	out := append([]string{}, env...)
	for i, e := range out {
		if k, _, ok := strings.Cut(e, "="); ok && k == "PATH" {
			out[i] = "PATH=" + path
			return out
		}
	}
	return append(out, "PATH="+path)
}

func envFlags(base, overlay []string) []string {
	order := make([]string, 0, len(base)+len(overlay))
	vals := map[string]string{}
	add := func(e string) {
		k, v, ok := strings.Cut(e, "=")
		if !ok || strings.HasPrefix(k, "TMUX") || strings.HasPrefix(k, "HERDR_") {
			return
		}
		if _, seen := vals[k]; !seen {
			order = append(order, k)
		}
		vals[k] = v
	}
	for _, e := range base {
		add(e)
	}
	for _, e := range overlay {
		add(e)
	}
	out := make([]string, 0, len(order)*2)
	for _, k := range order {
		out = append(out, "--env", k+"="+vals[k])
	}
	return out
}

func (s *Session) Start(name string) string {
	msg, err := sessionCall(s, func() string { return s.start(name) })
	if err != nil {
		return name + ": " + err.Error()
	}
	return msg
}

func (s *Session) Stop(name string, sig syscall.Signal) string {
	msg, err := sessionCall(s, func() string { return s.stop(name, sig) })
	if err != nil {
		return name + ": " + err.Error()
	}
	return msg
}

func (s *Session) Restart(name string) string {
	msg, err := sessionCall(s, func() string { return s.restart(name) })
	if err != nil {
		return name + ": " + err.Error()
	}
	return msg
}

// Open modes for a process's live output.
const (
	OpenPane      = "pane"      // split the current tab
	OpenTab       = "tab"       // new tab in the current workspace
	OpenWorkspace = "workspace" // new workspace containing a live tail
)

var OpenModes = []string{OpenPane, OpenTab, OpenWorkspace}

// Open shows a process's live output. It returns a command only when the
// terminal has to be handed over (attaching from outside Herdr); the Herdr modes
// do their work immediately and return nil.
func (s *Session) Open(name, mode string) (*exec.Cmd, error) {
	type result struct {
		cmd *exec.Cmd
		err error
	}
	v, managerErr := sessionCall(s, func() result {
		cmd, err := s.open(name, mode)
		return result{cmd, err}
	})
	if managerErr != nil {
		return nil, managerErr
	}
	return v.cmd, v.err
}

func (s *Session) Peeking() bool {
	v, _ := sessionCall(s, s.peeking)
	return v
}

func (s *Session) Peek(name string) error {
	err, managerErr := sessionCall(s, func() error { return s.peek(name) })
	if managerErr != nil {
		return managerErr
	}
	return err
}

func (s *Session) PeekClose() {
	_, _ = sessionCall(s, func() struct{} {
		s.peekClose()
		return struct{}{}
	})
}

func (s *Session) ShowCommands(focus string) error {
	err, managerErr := sessionCall(s, func() error { return s.showCommands(focus) })
	if managerErr != nil {
		return managerErr
	}
	return err
}

func (s *Session) HideCommands() {
	_, _ = sessionCall(s, func() struct{} {
		s.hideCommands()
		return struct{}{}
	})
}

func (s *Session) RefreshCommands(focus string) {
	_, _ = sessionCall(s, func() struct{} {
		s.refreshCommands(focus)
		return struct{}{}
	})
}

// executedCommands is the live argv tree of the process that starter launched.
func (s *Session) executedCommands(focus string) []string {
	if focus == "" {
		return nil
	}
	t := psTable()
	wins := s.windowsWith(t)
	w, ok := wins[focus]
	if !ok || w.Dead {
		if p := s.cfg.Processes[focus]; p != nil {
			return []string{p.Command}
		}
		return nil
	}
	return commandTree(t, w.PID, 0)
}

func commandTree(t Table, pid, depth int) []string {
	var lines []string
	var walk func(int, int)
	walk = func(pid, depth int) {
		if p, ok := t.Procs[pid]; ok {
			lines = append(lines, strings.Repeat("  ", depth)+p.Args)
		}
		for _, kid := range t.Kids[pid] {
			walk(kid, depth+1)
		}
	}
	walk(pid, depth)
	return lines
}

func (s *Session) ToggleDebug() (bool, error) {
	type result struct {
		open bool
		err  error
	}
	v, managerErr := sessionCall(s, func() result {
		open, err := s.toggleDebug()
		return result{open, err}
	})
	if managerErr != nil {
		return false, managerErr
	}
	return v.open, v.err
}

func (s *Session) DebugClose() {
	_, _ = sessionCall(s, func() struct{} {
		s.debugClose()
		return struct{}{}
	})
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

// Logs returns the tail of captured process output.
func (s *Session) Logs(name string, lines int) (string, error) {
	type result struct {
		logs string
		err  error
	}
	v, managerErr := sessionCall(s, func() result {
		logs, err := s.logs(name, lines)
		return result{logs, err}
	})
	if managerErr != nil {
		return "", managerErr
	}
	return v.logs, v.err
}

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
