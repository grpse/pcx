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

// holder is an idle shell window that keeps the session alive when every
// managed process is stopped.
const holder = "__pcx"

type Session struct {
	cfg        *Config
	peekPane   string // the output pane beside the TUI (#{pane_id})
	cmdPane    string // M's command-list pane above it
	debugPane  string // D's internal diagnostics pane below the TUI
	debugFile  *os.File
	clientPane string
	requests   chan managerRequest
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
		clientPane: os.Getenv("TMUX_PANE"),
	}
	s.debugf("manager created shell=%s client_pane=%s", executionShell(), s.clientPane)
	go s.manage()
	return s
}

// manage is the only goroutine that communicates with tmux and the process
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

func (s *Session) target() string { return "=" + s.cfg.Name }

func tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s *Session) Exists() bool {
	v, _ := sessionCall(s, s.exists)
	return v
}

func (s *Session) exists() bool {
	return exec.Command("tmux", "has-session", "-t", s.target()).Run() == nil
}

// Ensure creates the session if needed; reports whether it created it.
func (s *Session) Ensure() (bool, error) {
	type result struct {
		created bool
		err     error
	}
	v, managerErr := sessionCall(s, func() result {
		created, err := s.ensure()
		return result{created, err}
	})
	if managerErr != nil {
		return false, managerErr
	}
	return v.created, v.err
}

func (s *Session) ensure() (bool, error) {
	if s.exists() {
		return false, nil
	}
	args := append([]string{"new-session", "-d", "-s", s.cfg.Name, "-n", holder}, clientEnv()...)
	if _, err := tmux(args...); err != nil {
		return false, err
	}
	// lets a later run find this session again after the config is edited
	tmux("set-environment", "-t", s.cfg.Name, "PCX_CONFIG", s.cfg.Path)
	return true, nil
}

// configSessions lists the other pcx sessions running this same config file: a
// session left behind by an edit (the name is a content hash, so it moved), or
// a second pcx that started the same commands. Hash-named ones only — `name:`
// or -n means the user asked for a separate instance.
func (s *Session) configSessions() []string {
	if !s.cfg.Derived {
		return nil
	}
	out, err := tmux("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil
	}
	var found []string
	for _, name := range strings.Fields(out) {
		// view and peek sessions are grouped with ours: same windows, not a sibling
		if !strings.HasPrefix(name, "pcx-") || name == s.cfg.Name ||
			strings.Contains(name, "-view-") || strings.Contains(name, "-peek") {
			continue
		}
		env, err := tmux("show-environment", "-t", name, "PCX_CONFIG")
		if err != nil || strings.TrimSpace(env) != "PCX_CONFIG="+s.cfg.Path {
			continue
		}
		found = append(found, name)
	}
	return found
}

// Adopt pulls everything already running for this config into one session:
// renames the session an edit left behind, and absorbs the windows of any other
// pcx running the same file. Without it an edit would orphan running processes
// and a second pcx would start a duplicate of each command.
func (s *Session) Adopt() {
	_, _ = sessionCall(s, func() struct{} {
		s.adopt()
		return struct{}{}
	})
}

func (s *Session) adopt() {
	others := s.configSessions()
	if len(others) == 0 {
		return
	}
	if !s.exists() {
		tmux("rename-session", "-t", others[0], s.cfg.Name)
		others = others[1:]
	}
	for _, o := range others {
		s.absorb(o)
	}
}

// absorb moves a sibling's windows into our session — the running process keeps
// its pid, its output and its window, it just answers to us now — and drops the
// sibling once it is empty.
func (s *Session) absorb(other string) {
	mine := listWindows(s.target())
	for name, w := range listWindows("=" + other) {
		if s.cfg.Processes[name] == nil {
			continue
		}
		cur, have := mine[name]
		switch {
		case have && !cur.Dead, have && w.Dead:
			continue // ours is the live one, or neither is: window names stay unique
		case have:
			tmux("kill-window", "-t", s.target()+":"+name) // our corpse, their live process
		}
		tmux("move-window", "-s", "="+other+":"+name, "-t", s.target()+":")
	}
	if len(listWindows("="+other)) == 0 { // nothing but the holder left
		tmux("kill-session", "-t", "="+other)
	}
}

// Kill destroys the session and any view sessions grouped with it. Views share
// the windows, so leaving one alive would keep the processes alive too.
func (s *Session) Kill() error {
	err, managerErr := sessionCall(s, s.kill)
	if managerErr != nil {
		return managerErr
	}
	return err
}

func (s *Session) kill() error {
	out, _ := tmux("list-sessions", "-F", "#{session_name}")
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(name, s.cfg.Name+"-view-") || strings.HasPrefix(name, s.cfg.Name+"-peek") {
			tmux("kill-session", "-t", "="+name)
		}
	}
	if !s.exists() {
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
	External bool   // running outside our session; no tmux window behind it
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

func (s *Session) windowsWith(t Table) map[string]Window {
	wins := map[string]Window{}
	if s.exists() {
		wins = listWindows(s.target())
		if s.pull(wins) {
			wins = listWindows(s.target())
		}
	}
	s.external(t, wins) // no session, or a process running some other way
	return wins
}

// pull folds any other pcx session running this same config into ours, so a
// command someone else already opened for this file is managed here instead of
// started a second time. It only goes looking when something is not running, so
// the steady state costs nothing.
func (s *Session) pull(wins map[string]Window) bool {
	if !s.missing(wins) {
		return false
	}
	others := s.configSessions()
	for _, o := range others {
		s.absorb(o)
	}
	return len(others) > 0
}

func (s *Session) missing(wins map[string]Window) bool {
	for _, n := range s.cfg.names() {
		if w, ok := wins[n]; !ok || w.Dead {
			return true
		}
	}
	return false
}

// listWindows reads one session's windows, minus the holder.
func listWindows(target string) map[string]Window {
	wins := map[string]Window{}
	out, err := tmux("list-windows", "-t", target, "-F",
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

// external fills in processes that are already running some other way — by
// hand, by another tool, by another pcx — so the same process is managed from
// anywhere instead of pcx starting a second copy of it.
//
// ponytail: the args have to *end* with the command, which is the shape a real
// start has (`sh -c "cmd"`, or the exec'd program itself) and keeps shells and
// editors that merely mention it out. A command that execs something else (npx,
// wrapper scripts) runs under different args and will not match; start those
// through pcx if you want them managed.
func (s *Session) external(t Table, wins map[string]Window) {
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
		if w, ok := wins[name]; ok && !w.Dead {
			continue
		}
		cmd := strings.TrimSpace(s.cfg.Processes[name].Command)
		if cmd == "" {
			continue
		}
		match := map[int]bool{}
		for pid, p := range t.Procs {
			if !ours[pid] && strings.HasSuffix(p.Args, cmd) {
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

// shellCmd applies the restart policy without a supervisor: the loop lives
// inside the pane, so it keeps working when nothing is attached.
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

// asCurrentUser keeps the pane on the person who invoked pcx. A root tmux
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

// clientEnv is the invoking process's environment, minus the tmux vars that
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
		if !ok || k == "TMUX" || k == "TMUX_PANE" {
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
		out = append(out, "-e", k+"="+vals[k])
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

func (s *Session) start(name string) string {
	p := s.cfg.Processes[name]
	if p == nil {
		return name + ": unknown process"
	}
	if _, err := s.ensure(); err != nil {
		return name + ": " + err.Error()
	}
	before := listWindows(s.target())       // to tell an adoption from a plain no-op
	w, ok := s.windowsWith(psTable())[name] // absorbs another pcx's window for this config
	if ok && w.External {
		return fmt.Sprintf("%s: already running outside pcx (pid %d)", name, w.PID)
	}
	if ok && !w.Dead {
		if b, had := before[name]; !had || b.Dead {
			return fmt.Sprintf("%s: adopted the copy already running for this config (pid %d)", name, w.PID)
		}
		return name + ": already running"
	}
	env := windowEnv(p)
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
	msg, err := sessionCall(s, func() string { return s.stop(name, sig) })
	if err != nil {
		return name + ": " + err.Error()
	}
	return msg
}

func (s *Session) stop(name string, sig syscall.Signal) string {
	w, ok := s.windowsWith(psTable())[name]
	if !ok || w.Dead {
		return name + ": not running"
	}
	kids := descendants(psTable(), w.PID)
	if w.External {
		// not ours: its process group can be the shell job that started it, so
		// signal only the tree we matched
		_ = syscall.Kill(w.PID, sig)
	} else {
		// The pane leader owns its process group; that covers the normal case.
		_ = syscall.Kill(-w.PID, sig)
	}
	for _, pid := range kids { // anything that escaped the group (setsid daemons)
		_ = syscall.Kill(pid, sig)
	}
	return fmt.Sprintf("%s: sent %v", name, sig)
}

func (s *Session) Restart(name string) string {
	msg, err := sessionCall(s, func() string { return s.restart(name) })
	if err != nil {
		return name + ": " + err.Error()
	}
	return msg
}

func (s *Session) restart(name string) string {
	s.stop(name, syscall.SIGTERM)
	return s.start(name) // respawn -k finishes off whatever survived
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

func (s *Session) open(name, mode string) (*exec.Cmd, error) {
	win := s.target() + ":" + name
	if os.Getenv("TMUX") == "" {
		// nothing to split or tab into: every mode collapses to attaching
		ui, err := s.outputView(name)
		if err != nil {
			return nil, err
		}
		return exec.Command("tmux", "attach-session", "-t", "="+ui), nil
	}
	switch mode {
	case OpenPane, OpenTab:
		ui, err := s.outputView(name)
		if err != nil {
			return nil, err
		}
		// TMUX= lets the inner client attach; the session dies with it
		cmd := fmt.Sprintf("TMUX= tmux attach -t '=%s'; tmux kill-session -t '=%s' 2>/dev/null; tmux kill-session -t '=%s' 2>/dev/null",
			ui, ui, s.cfg.Name+"-view-"+name)
		if mode == OpenPane {
			id, err := tmux("split-window", "-h", "-P", "-F", "#{pane_id}", cmd)
			if err != nil {
				return nil, err
			}
			s.peekPane = strings.TrimSpace(id)
			return nil, nil
		}
		_, err = tmux("new-window", "-n", name, cmd)
		return nil, err
	default:
		_, err := tmux("switch-client", "-t", win)
		return nil, err
	}
}

// outputView is a nested attach to the process window. The attach session is
// grouped with ours so closing the view cannot kill the process. M adds a
// command-list pane above the output; it is hidden until then.
func (s *Session) outputView(name string) (string, error) {
	grouped := s.cfg.Name + "-view-" + name
	ui := grouped + "-ui"
	if exec.Command("tmux", "has-session", "-t", "="+grouped).Run() != nil {
		if _, err := tmux("new-session", "-d", "-t", s.cfg.Name, "-s", grouped); err != nil {
			return "", err
		}
		tmux("set-option", "-t", grouped, "status", "off")
	}
	if _, err := tmux("select-window", "-t", "="+grouped+":"+name); err != nil {
		return "", err
	}
	if err := s.ensureViewUI(ui, grouped); err != nil {
		return "", err
	}
	return ui, nil
}

// ---------------------------------------------------------------- peek

// The peek pane is one nested client on one grouped session, so following the
// cursor is a select-window rather than a new pane per process.
func (s *Session) peekSession() string   { return s.cfg.Name + "-peek" }
func (s *Session) peekUISession() string { return s.cfg.Name + "-peek-ui" }

func (s *Session) Peeking() bool {
	v, _ := sessionCall(s, s.peeking)
	return v
}

func (s *Session) peeking() bool {
	return exec.Command("tmux", "has-session", "-t", "="+s.peekUISession()).Run() == nil
}

// Peek splits a read-only view of name's output beside the TUI, or re-points an
// open one at name. Focus stays on the TUI (-d), so it reads as a preview.
// The command-list pane above the output is off until M.
func (s *Session) Peek(name string) error {
	err, managerErr := sessionCall(s, func() error { return s.peek(name) })
	if managerErr != nil {
		return managerErr
	}
	return err
}

func (s *Session) peek(name string) error {
	view := s.peekSession()
	ui := s.peekUISession()
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf("peek needs tmux")
	}
	if w, ok := s.windowsWith(psTable())[name]; !ok || w.External {
		return fmt.Errorf("no tmux output — not started by pcx")
	}
	if s.peeking() {
		_, err := tmux("select-window", "-t", "="+view+":"+name)
		return err
	}
	if exec.Command("tmux", "has-session", "-t", "="+view).Run() != nil {
		if _, err := tmux("new-session", "-d", "-t", s.cfg.Name, "-s", view); err != nil {
			return err
		}
		// plain name: set-option rejects the '=' exact-match prefix
		tmux("set-option", "-t", view, "status", "off")
	}
	if _, err := tmux("select-window", "-t", "="+view+":"+name); err != nil {
		return err
	}
	if err := s.ensureViewUI(ui, view); err != nil {
		return err
	}
	cmd := fmt.Sprintf("TMUX= tmux attach -t '=%s'; tmux kill-session -t '=%s' 2>/dev/null; tmux kill-session -t '=%s' 2>/dev/null",
		ui, ui, view)
	id, err := tmux("split-window", "-h", "-d", "-P", "-F", "#{pane_id}", cmd)
	if err != nil {
		return err
	}
	s.peekPane = strings.TrimSpace(id)
	return nil
}

// PeekClose drops the pane; the nested client exits with its session.
func (s *Session) PeekClose() {
	_, _ = sessionCall(s, func() struct{} {
		s.peekClose()
		return struct{}{}
	})
}

func (s *Session) peekClose() {
	s.hideCommands()
	if s.peekPane != "" {
		tmux("kill-pane", "-t", s.peekPane)
		s.peekPane = ""
	}
	tmux("kill-session", "-t", "="+s.peekUISession())
	tmux("kill-session", "-t", "="+s.peekSession())
}

func (s *Session) headerFile() string {
	return filepath.Join(os.TempDir(), "pcx-header-"+sanitize(s.cfg.Name))
}

func (s *Session) writeHeader(lines []string) {
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	os.WriteFile(s.headerFile(), []byte(body), 0o644)
}

func (s *Session) headerWatch() string {
	return fmt.Sprintf("while :; do printf '\\033[H\\033[2J'; cat %s 2>/dev/null; sleep 1; done",
		strconv.Quote(s.headerFile()))
}

func (s *Session) outputPane() string {
	if s.peekPane != "" {
		return s.peekPane
	}
	out, err := tmux("list-panes", "-F", "#{pane_id}")
	if err != nil {
		return ""
	}
	panes := strings.Fields(out)
	if len(panes) < 2 {
		return ""
	}
	return panes[len(panes)-1]
}

func (s *Session) paneExists(id string) bool {
	if id == "" {
		return false
	}
	return exec.Command("tmux", "display-message", "-t", id, "-p", "#{pane_id}").Run() == nil
}

// ShowCommands splits a pane above the process output with the argv tree
// that starter actually launched.
func (s *Session) ShowCommands(focus string) error {
	err, managerErr := sessionCall(s, func() error { return s.showCommands(focus) })
	if managerErr != nil {
		return managerErr
	}
	return err
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
	target := s.outputPane()
	if target == "" {
		return fmt.Errorf("no output pane to split")
	}
	n := len(lines)
	if n < 3 {
		n = 3
	}
	if n > 12 {
		n = 12
	}
	id, err := tmux("split-window", "-t", target, "-b", "-v", "-l", strconv.Itoa(n),
		"-P", "-F", "#{pane_id}", s.headerWatch())
	if err != nil {
		// small peek pane: fall back to a percentage split
		id, err = tmux("split-window", "-t", target, "-b", "-v", "-p", "30",
			"-P", "-F", "#{pane_id}", s.headerWatch())
		if err != nil {
			return err
		}
	}
	s.cmdPane = strings.TrimSpace(id)
	return nil
}

// HideCommands removes the command-list pane.
func (s *Session) HideCommands() {
	_, _ = sessionCall(s, func() struct{} {
		s.hideCommands()
		return struct{}{}
	})
}

func (s *Session) hideCommands() {
	if s.cmdPane != "" {
		tmux("kill-pane", "-t", s.cmdPane)
		s.cmdPane = ""
	}
	s.writeHeader(nil)
}

func (s *Session) RefreshCommands(focus string) {
	_, _ = sessionCall(s, func() struct{} {
		s.refreshCommands(focus)
		return struct{}{}
	})
}

func (s *Session) refreshCommands(focus string) {
	if !s.paneExists(s.cmdPane) {
		s.cmdPane = ""
		return
	}
	s.writeHeader(s.executedCommands(focus))
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

func (s *Session) ensureViewUI(ui, grouped string) error {
	if exec.Command("tmux", "has-session", "-t", "="+ui).Run() == nil {
		return nil
	}
	attach := fmt.Sprintf("TMUX= tmux attach -t '=%s'", grouped)
	if _, err := tmux("new-session", "-d", "-s", ui, "-n", "view", attach); err != nil {
		return err
	}
	tmux("set-option", "-t", ui, "status", "off")
	return nil
}

// ---------------------------------------------------------------- debug

// ToggleDebug opens or closes a pane below the TUI that follows the process
// manager's internal request log.
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

func (s *Session) toggleDebug() (bool, error) {
	if s.paneExists(s.debugPane) {
		s.debugClose()
		return false, nil
	}
	if s.clientPane == "" {
		return false, fmt.Errorf("debug pane needs pcx to run inside tmux")
	}
	if s.debugPath() == "" {
		return false, fmt.Errorf("debug log is unavailable")
	}
	s.debugf("opening debug pane")
	command := "exec tail -n 200 -f " + shellQuote(s.debugPath())
	id, err := tmux("split-window", "-v", "-d", "-p", "30", "-t", s.clientPane,
		"-P", "-F", "#{pane_id}", command)
	if err != nil {
		return false, err
	}
	s.debugPane = strings.TrimSpace(id)
	return true, nil
}

func (s *Session) DebugClose() {
	_, _ = sessionCall(s, func() struct{} {
		s.debugClose()
		return struct{}{}
	})
}

func (s *Session) debugClose() {
	if s.paneExists(s.debugPane) {
		tmux("kill-pane", "-t", s.debugPane)
	}
	s.debugPane = ""
	s.debugf("debug pane closed")
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

func (s *Session) logs(name string, lines int) (string, error) {
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
