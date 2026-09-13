package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// holder is an idle shell window that keeps the session alive when every
// managed process is stopped.
const holder = "__pcx"

type Session struct {
	cfg      *Config
	peekPane string // the output pane beside the TUI (#{pane_id})
	cmdPane  string // M's command-list pane above it
}

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
	others := s.configSessions()
	if len(others) == 0 {
		return
	}
	if !s.Exists() {
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
	out, _ := tmux("list-sessions", "-F", "#{session_name}")
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(name, s.cfg.Name+"-view-") || strings.HasPrefix(name, s.cfg.Name+"-peek") {
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
	External bool   // running outside our session; no tmux window behind it
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

func (s *Session) Windows() map[string]Window { return s.WindowsWith(psTable()) }

// WindowsWith takes the ps sweep from the caller, so a refresh that already has
// one does not run a second.
func (s *Session) WindowsWith(t Table) map[string]Window {
	wins := map[string]Window{}
	if s.Exists() {
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
	var cmd string
	switch p.Availability.Restart {
	case "always":
		// subshell, so an `exit` in the command cannot kill the loop
		cmd = fmt.Sprintf("while :; do ( %s ); sleep 1; done", p.Command)
	case "on_failure", "on-failure":
		cmd = fmt.Sprintf("until ( %s ); do sleep 1; done", p.Command)
	default:
		cmd = p.Command
	}
	// export after the pane shell starts: macOS path_helper (in zshenv)
	// otherwise puts /usr/bin first and the system node wins
	return asCurrentUser(withUserPath(cmd))
}

func withUserPath(cmd string) string {
	path := resolvedUserPath()
	if path == "" {
		return cmd
	}
	return "export PATH=" + strconv.Quote(path) + "; " + cmd
}

func resolvedUserPath() string {
	home := ""
	if u := invokeUser(); u != nil {
		home = u.HomeDir
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return userPath(home, os.Getenv("PATH"))
}

// userPath puts version-manager and user-local bins in front of PATH so
// `node` / `npx` resolve to the account's install, not /usr/bin.
func userPath(home, current string) string {
	seen := map[string]bool{}
	var out []string
	addDir := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range userBinDirs(home) {
		addDir(dir)
	}
	for _, dir := range strings.Split(current, ":") {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return strings.Join(out, ":")
}

func userBinDirs(home string) []string {
	nvm := os.Getenv("NVM_DIR")
	if nvm == "" && home != "" {
		nvm = filepath.Join(home, ".nvm")
	}
	fnm := os.Getenv("FNM_DIR")
	if fnm == "" && home != "" {
		fnm = filepath.Join(home, ".fnm")
	}
	return []string{
		nvmCurrentBin(nvm),
		os.Getenv("FNM_MULTISHELL_PATH"),
		filepath.Join(fnm, "aliases", "default", "bin"),
		filepath.Join(home, ".local", "share", "fnm", "aliases", "default", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".asdf", "shims"),
		filepath.Join(home, ".local", "share", "mise", "shims"),
		filepath.Join(home, ".nodenv", "shims"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".local", "share", "pnpm"),
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
	}
}

func nvmCurrentBin(nvm string) string {
	if nvm == "" {
		return ""
	}
	seen := map[string]bool{}
	var resolve func(string, int) string
	resolve = func(name string, depth int) string {
		name = strings.TrimSpace(name)
		if name == "" || depth > 8 || seen[name] {
			return ""
		}
		seen[name] = true
		p := filepath.Join(nvm, "versions", "node", name, "bin")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		b, err := os.ReadFile(filepath.Join(nvm, "alias", name))
		if err != nil {
			return ""
		}
		return resolve(string(b), depth+1)
	}
	if p := resolve("default", 0); p != "" {
		return p
	}
	current := filepath.Join(nvm, "versions", "node", "current", "bin")
	if fi, err := os.Stat(current); err == nil && fi.IsDir() {
		return current
	}
	return ""
}

// asCurrentUser keeps the pane on the person who invoked pcx. A root tmux
// server (sudo pcx) would otherwise run every command as root.
func asCurrentUser(cmd string) string {
	u := invokeUser()
	if u == nil || u.Uid == strconv.Itoa(os.Geteuid()) {
		return cmd
	}
	return fmt.Sprintf("sudo -u %s -H -E -- /bin/sh -c %s", strconv.Quote(u.Username), strconv.Quote(cmd))
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
	p := s.cfg.Processes[name]
	if p == nil {
		return name + ": unknown process"
	}
	if _, err := s.Ensure(); err != nil {
		return name + ": " + err.Error()
	}
	before := listWindows(s.target()) // to tell an adoption from a plain no-op
	w, ok := s.Windows()[name]        // absorbs another pcx's window for this config
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
	w, ok := s.Windows()[name]
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
	return exec.Command("tmux", "has-session", "-t", "="+s.peekUISession()).Run() == nil
}

// Peek splits a read-only view of name's output beside the TUI, or re-points an
// open one at name. Focus stays on the TUI (-d), so it reads as a preview.
// The command-list pane above the output is off until M.
func (s *Session) Peek(name string) error {
	view := s.peekSession()
	ui := s.peekUISession()
	if os.Getenv("TMUX") == "" {
		return fmt.Errorf("peek needs tmux")
	}
	if w, ok := s.Windows()[name]; !ok || w.External {
		return fmt.Errorf("no tmux output — not started by pcx")
	}
	if s.Peeking() {
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
	s.HideCommands()
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
	if s.cmdPane != "" {
		tmux("kill-pane", "-t", s.cmdPane)
		s.cmdPane = ""
	}
	s.writeHeader(nil)
}

func (s *Session) RefreshCommands(focus string) {
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
	wins := s.WindowsWith(t)
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
