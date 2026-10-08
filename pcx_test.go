package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConfigExpandsVarsAndKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	os.WriteFile(path, []byte(`
name: t
vars:
  WORKING_DIR: "./from-vars"
  XILO_API: "../xilo-api"
processes:
  b-proc:
    command: "echo b"
    working_dir: '{{or "${WORKING_DIR}" .WORKING_DIR}}'
    namespace: ns1
  a-proc:
    command: "echo a"
    working_dir: '{{or "${XILO_API}" .XILO_API}}'
    namespace: ns2
    disabled: true
`), 0o644)

	t.Setenv("WORKING_DIR", "/env/wins")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{"b-proc", "a-proc"}; strings.Join(cfg.Order, ",") != strings.Join(got, ",") {
		t.Fatalf("file order lost: %v", cfg.Order)
	}
	if cfg.Processes["b-proc"].WorkingDir != "/env/wins" {
		t.Fatalf("env should beat the vars default, got %q", cfg.Processes["b-proc"].WorkingDir)
	}
	if want := filepath.Join(dir, "../xilo-api"); cfg.Processes["a-proc"].WorkingDir != filepath.Clean(want) {
		t.Fatalf("vars default + relative resolve: got %q want %q", cfg.Processes["a-proc"].WorkingDir, want)
	}
	ns, byNS := cfg.Namespaces()
	if len(ns) != 2 || byNS["ns1"][0].Name != "b-proc" {
		t.Fatalf("namespace grouping: %v %v", ns, byNS)
	}
	if !cfg.Processes["a-proc"].Disabled {
		t.Fatal("disabled not parsed")
	}
	// the subshell matters: without it an `exit` in the command kills the loop
	if got := shellCommand(&Proc{Command: "x", Availability: struct {
		Restart string `yaml:"restart"`
	}{Restart: "always"}}, "/bin/sh", "/usr/bin:/bin"); !strings.Contains(got, "while :;") || !strings.Contains(got, "( x )") {
		t.Fatalf("restart policy not wrapped: %q", got)
	}
}

func TestShellCommandUsesSelectedShellSyntax(t *testing.T) {
	posix := shellCommand(&Proc{Command: "echo 'hello'", Availability: struct {
		Restart string `yaml:"restart"`
	}{Restart: "on_failure"}}, "/bin/zsh", "/custom/bin:/usr/bin")
	if !strings.HasPrefix(posix, "exec '/bin/zsh' -l -c ") ||
		!strings.Contains(posix, "/custom/bin:/usr/bin") ||
		!strings.Contains(posix, "until ( echo") {
		t.Fatalf("zsh command did not use POSIX shell environment: %q", posix)
	}

	fish := shellCommand(&Proc{Command: "echo 'hello'", Availability: struct {
		Restart string `yaml:"restart"`
	}{Restart: "on_failure"}}, "/opt/homebrew/bin/fish", "/custom/bin:/usr/bin")
	if !strings.HasPrefix(fish, "exec '/opt/homebrew/bin/fish' -l -c ") ||
		!strings.Contains(fish, "set -gx PATH") ||
		!strings.Contains(fish, "while not") ||
		!strings.Contains(fish, "/opt/homebrew/bin/fish") {
		t.Fatalf("fish command did not use fish syntax: %q", fish)
	}

	quoted := shellQuote("it's safe")
	out, err := exec.Command("/bin/sh", "-c", "printf %s "+quoted).Output()
	if err != nil || string(out) != "it's safe" {
		t.Fatalf("shell quoting failed: output=%q err=%v quote=%q", out, err, quoted)
	}
}

func TestEnvFlagsOverrideAndDropTmux(t *testing.T) {
	got := envFlags(
		[]string{"PATH=/bin", "TMUX=old", "TMUX_PANE=%0", "HOME=/root", "MARK=from-base"},
		[]string{"HOME=/me", "MARK=from-yaml", "EXTRA=1"},
	)
	want := []string{"-e", "PATH=/bin", "-e", "HOME=/me", "-e", "MARK=from-yaml", "-e", "EXTRA=1"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("env flags:\n got %q\nwant %q", got, want)
	}
}

func TestVisCmdDefaultsToTailAndScrolls(t *testing.T) {
	long := "aaaaaaaaaaBBBBBB"
	if got := visCmd(long, 6, 0); got != "…BBBBB" {
		t.Fatalf("default should show the end, got %q", got)
	}
	if got := visCmd(long, 6, 100); got != "aaaaa…" {
		t.Fatalf("scroll toward start, got %q", got)
	}
	if got := visCmd("short", 8, 0); got != "short   " {
		t.Fatalf("short command should pad, got %q", got)
	}
}

func TestExecutedCommandsIsTheLiveTree(t *testing.T) {
	tble := Table{
		Procs: map[int]PS{
			10: {PID: 10, Args: "sh -c npx nx run web:start"},
			11: {PID: 11, PPID: 10, Args: "node /home/me/.nvm/versions/node/v22.0.0/bin/nx"},
		},
		Kids: map[int][]int{10: {11}},
	}
	tree := commandTree(tble, 10, 0)
	if len(tree) != 2 || tree[0] != "sh -c npx nx run web:start" || !strings.Contains(tree[1], "/home/me/.nvm") {
		t.Fatalf("executed tree should be the full argv, got %q", tree)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	os.WriteFile(path, []byte("name: cmds\nprocesses:\n  web:\n    command: \"npx nx run web:start\"\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	if got := s.executedCommands("web"); len(got) != 1 || got[0] != "npx nx run web:start" {
		t.Fatalf("stopped process should show the configured command, got %q", got)
	}
	if s.executedCommands("") != nil {
		t.Fatal("no process selected")
	}
}

func TestAsCurrentUserNoWrapWhenAlreadySelf(t *testing.T) {
	if got := asCurrentUser("echo hi"); got != "echo hi" {
		t.Fatalf("should not wrap when already the invoking user, got %q", got)
	}
}

func TestUserPathPrefersHomeBinsNotVendorTrees(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, ".local", "bin")
	os.MkdirAll(local, 0o755)
	os.MkdirAll(filepath.Join(home, ".nvm", "versions", "node", "v22.0.0", "bin"), 0o755)

	got := userPath(home, "/usr/bin:/bin")
	if !strings.HasPrefix(got, local+":") {
		t.Fatalf("~/.local/bin should lead PATH, got %q", got)
	}
	if strings.Contains(got, ".nvm") {
		t.Fatalf("PATH must not special-case nvm, got %q", got)
	}
}

func TestNodeExecutesFromUserSpaceNotSystem(t *testing.T) {
	home := t.TempDir()
	userBin := filepath.Join(home, ".local", "bin")
	sysBin := filepath.Join(t.TempDir(), "sys")
	os.MkdirAll(userBin, 0o755)
	os.MkdirAll(sysBin, 0o755)
	writeExec := func(dir, body string) {
		p := filepath.Join(dir, "node")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(userBin, `echo user-space`)
	writeExec(sysBin, `echo system`)

	path := userPath(home, sysBin+":/usr/bin:/bin")
	which, err := exec.Command("/bin/sh", "-c", "export PATH="+strconv.Quote(path)+"; command -v node").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(which)); got != filepath.Join(userBin, "node") {
		t.Fatalf("command -v node: got %q want user-space bin", got)
	}
	out, err := exec.Command("/bin/sh", "-c", "export PATH="+strconv.Quote(path)+"; node").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "user-space" {
		t.Fatalf("node ran %q, want user-space (not system)", got)
	}
}

// A started process sees the environment of the pcx that launched it, not
// whatever the tmux server happened to be started with.
func TestProcessInheritsInvokerEnvironment(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	mark := "pcx-env-" + filepath.Base(dir)
	out := filepath.Join(dir, "mark")
	t.Setenv("PCX_TEST_MARK", mark)
	path := filepath.Join(dir, "process-compose-x.yaml")
	os.WriteFile(path, []byte(fmt.Sprintf(`
name: pcx-test-env
processes:
  writer:
    command: "printf %%s \"$PCX_TEST_MARK\" > %s"
    environment: ["PCX_TEST_EXTRA=yaml"]
`, out)), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })
	if msg := s.Start("writer"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	waitFor2(t, s, "writer", func(w Window) bool { return w.Dead })
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != mark {
		t.Fatalf("process did not see the invoking environment: got %q want %q", b, mark)
	}
}

// End-to-end against a real tmux server: start, see children, stop, respawn.
func TestSessionLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	os.WriteFile(path, []byte(`
name: pcx-test-lifecycle
processes:
  spawner:
    command: "sleep 300 & sleep 300 & wait"
    namespace: test
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", s.target()).Run() })

	if msg := s.Start("spawner"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	w := waitFor(t, s, func(w Window) bool { return !w.Dead && len(psTable().Kids[w.PID]) >= 2 })

	if cpu, rss := totals(psTable(), w.PID); rss == 0 {
		t.Fatalf("no memory accounted for the tree (cpu=%v rss=%v)", cpu, rss)
	}
	if msg := s.Start("spawner"); !strings.Contains(msg, "already running") {
		t.Fatalf("double start should be a no-op, got %q", msg)
	}

	s.Stop("spawner", syscall.SIGKILL)
	dead := waitFor(t, s, func(w Window) bool { return w.Dead })
	if dead.PID == 0 {
		t.Fatal("window disappeared; remain-on-exit did not stick")
	}
	if kids := psTable().Kids[w.PID]; len(kids) > 0 {
		t.Fatalf("children survived the stop: %v", kids)
	}

	if msg := s.Restart("spawner"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	waitFor(t, s, func(w Window) bool { return !w.Dead })
}

func waitFor(t *testing.T, s *Session, ok func(Window) bool) Window {
	t.Helper()
	return waitFor2(t, s, "spawner", ok)
}

// The confirmation gate is the point: 'x' must not stop anything until 'y'.
func TestStopNeedsConfirmation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	os.WriteFile(path, []byte(`
name: pcx-test-confirm
processes:
  spawner:
    command: "sleep 300"
    namespace: test
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", s.target()).Run() })
	s.Start("spawner")
	waitFor(t, s, func(w Window) bool { return !w.Dead })

	m := newModel(cfg, s)
	m.wins, m.table = s.Windows(), psTable()
	m.build()
	for i, r := range m.rows {
		if r.proc == "spawner" {
			m.cursor = i
		}
	}

	press := func(m model, key string) model {
		next, cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		m = next.(model)
		for cmd != nil {
			next, cmd = m.Update(cmd())
			m = next.(model)
		}
		return m
	}
	alive := func() bool { w, ok := s.Windows()["spawner"]; return ok && !w.Dead }

	m = press(m, "x")
	if m.asking == nil {
		t.Fatal("stop was not gated behind a confirmation")
	}
	if !alive() {
		t.Fatal("stop ran before it was confirmed")
	}

	m = press(m, "n") // anything but y cancels
	if m.asking != nil || !alive() {
		t.Fatal("n should cancel the pending stop")
	}

	m = press(m, "x")
	m = press(m, "y")
	if m.asking != nil {
		t.Fatal("y should clear the prompt")
	}
	waitFor(t, s, func(w Window) bool { return w.Dead })
}

func TestTUIActionsRunInKeypressOrder(t *testing.T) {
	cfg := &Config{
		Name:      "pcx-test-action-queue",
		Processes: map[string]*Proc{},
	}
	m := newModel(cfg, NewSession(cfg))
	var order []string

	first, firstCmd := m.enqueue(queuedAction{run: func() string {
		order = append(order, "first")
		return "first done"
	}})
	m = first.(model)
	second, secondCmd := m.enqueue(queuedAction{run: func() string {
		order = append(order, "second")
		return "second done"
	}})
	m = second.(model)
	if firstCmd == nil || secondCmd != nil {
		t.Fatal("the first action should start and the second should wait")
	}

	next, nextCmd := m.Update(firstCmd())
	m = next.(model)
	if nextCmd == nil || strings.Join(order, ",") != "first" {
		t.Fatalf("second action started before the first completed: %v", order)
	}
	next, nextCmd = m.Update(nextCmd())
	m = next.(model)
	if nextCmd != nil || m.running || strings.Join(order, ",") != "first,second" {
		t.Fatalf("actions did not drain in order: %v", order)
	}
}

func TestSessionManagerOwnsAndRecoversItsExecutionContext(t *testing.T) {
	cfg := &Config{Name: "pcx-test-manager", Processes: map[string]*Proc{}}
	s := NewSession(cfg)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})

	go func() {
		_, _ = sessionCall(s, func() struct{} {
			close(firstEntered)
			<-releaseFirst
			return struct{}{}
		})
		close(firstDone)
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		_, _ = sessionCall(s, func() struct{} {
			close(secondEntered)
			return struct{}{}
		})
		close(secondDone)
	}()
	select {
	case <-secondEntered:
		t.Fatal("a second request entered while the first was still executing")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone

	if _, err := sessionCall(s, func() struct{} {
		panic("test panic")
	}); err == nil || !strings.Contains(err.Error(), "recovered from panic") {
		t.Fatalf("manager did not contain its worker panic: %v", err)
	}
	value, err := sessionCall(s, func() int { return 42 })
	if err != nil || value != 42 {
		t.Fatalf("manager did not continue after recovery: value=%d err=%v", value, err)
	}
}

// Identity follows the file's contents, and an edit must not orphan what is
// already running for that file.
func TestSessionIdentityFollowsConfigContent(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	body := "processes:\n  ticker:\n    command: \"sleep 300\"\n"
	write := func(dir, extra string) *Config {
		path := filepath.Join(dir, "process-compose-x.yaml")
		os.WriteFile(path, []byte(body+extra), 0o644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	a, b := write(t.TempDir(), ""), write(t.TempDir(), "")
	if !a.Derived || !strings.HasPrefix(a.Name, "pcx-") {
		t.Fatalf("name should come from the content hash, got %q", a.Name)
	}
	if a.Name != b.Name {
		t.Fatalf("same contents in another directory should share a session: %q vs %q", a.Name, b.Name)
	}

	s := NewSession(a)
	t.Cleanup(func() { s.Kill() })
	s.Start("ticker")
	pid := waitFor2(t, s, "ticker", func(w Window) bool { return !w.Dead }).PID

	edited := write(a.Dir, "\n# a harmless edit\n")
	if edited.Name == a.Name {
		t.Fatal("editing the file should change the hash")
	}
	s2 := NewSession(edited)
	if s2.Exists() {
		t.Fatal("the edited config should not have a session yet")
	}
	s2.Adopt()
	t.Cleanup(func() { s2.Kill() })
	if w := waitFor2(t, s2, "ticker", func(w Window) bool { return !w.Dead }); w.PID != pid {
		t.Fatalf("edit orphaned the running process: pid %d became %d", pid, w.PID)
	}
}

func waitFor2(t *testing.T, s *Session, name string, ok func(Window) bool) Window {
	t.Helper()
	for i := 0; i < 100; i++ {
		if w, found := s.Windows()[name]; found && ok(w) {
			return w
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out; windows=%v", s.Windows())
	return Window{}
}

// A process started some other way is captured: reported, not started twice,
// and stoppable through pcx.
func TestExternalProcessIsAdopted(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	cmd := "sleep 300 && echo pcx-external-" + filepath.Base(dir)
	os.WriteFile(path, []byte("name: pcx-test-external\nprocesses:\n  outsider:\n    command: \""+cmd+"\"\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })

	// started behind pcx's back, with no session in sight
	outside := exec.Command("/bin/sh", "-c", cmd)
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	defer outside.Process.Kill()

	w, ok := s.Windows()["outsider"]
	if !ok || !w.External || w.PID != outside.Process.Pid {
		t.Fatalf("external process not captured: %+v (pid %d)", w, outside.Process.Pid)
	}
	if w.Status() != "external" {
		t.Fatalf("status: %q", w.Status())
	}
	if msg := s.Start("outsider"); !strings.Contains(msg, "outside pcx") {
		t.Fatalf("start should refuse a second copy, got %q", msg)
	}
	if msg := s.Stop("outsider", syscall.SIGKILL); !strings.Contains(msg, "sent") {
		t.Fatalf("stop: %q", msg)
	}
	outside.Wait()
	if w, ok := s.Windows()["outsider"]; ok {
		t.Fatalf("still reported after the kill: %+v", w)
	}
}

// A second pcx running the same config must not end up with two copies of the
// same command: starting it takes over the window that is already running.
func TestStartAdoptsSiblingSessionWindow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	// no name: the session identity has to be derived for siblings to be ours
	os.WriteFile(path, []byte("processes:\n  ticker:\n    command: \"sleep 300\"\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })
	if _, err := s.Ensure(); err != nil {
		t.Fatal(err)
	}

	// another pcx, same config file, already running the command
	sibling := "pcx-sibling-" + filepath.Base(dir)
	t.Cleanup(func() { exec.Command("tmux", "kill-session", "-t", "="+sibling).Run() })
	tmux("new-session", "-d", "-s", sibling, "-n", holder)
	tmux("set-environment", "-t", sibling, "PCX_CONFIG", cfg.Path)
	tmux("new-window", "-d", "-t", "="+sibling+":", "-n", "ticker", "sleep 300")
	pid := listWindows("=" + sibling)["ticker"].PID
	if pid == 0 {
		t.Fatal("sibling window did not start")
	}

	if msg := s.Start("ticker"); !strings.Contains(msg, "adopted the copy already running") {
		t.Fatalf("start should have taken the running window, got %q", msg)
	}
	w, ok := s.Windows()["ticker"]
	if !ok || w.Dead || w.PID != pid {
		t.Fatalf("adopted window lost the process: %+v want pid %d", w, pid)
	}
	if exec.Command("tmux", "has-session", "-t", "="+sibling).Run() == nil {
		t.Fatal("emptied sibling session should be gone")
	}
	if msg := s.Start("ticker"); !strings.Contains(msg, "already running") {
		t.Fatalf("second start: %q", msg)
	}
}

func TestLockedInnerCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("HOME"), ".pcx", "pids")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "42.lock")
	if err := os.WriteFile(lock, []byte("43\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSession(&Config{Processes: map[string]*Proc{"parent": {Command: "launcher"}, "child": {Command: "worker --serve"}}})
	table := Table{Procs: map[int]PS{42: {PID: 42, Args: "launcher"}, 43: {PID: 43, PPID: 42, Args: "worker --serve"}}, Kids: map[int][]int{42: {43}}}
	wins := map[string]Window{"parent": {PID: 42}}
	s.external(table, wins)
	w := wins["child"]
	if w.PID != 43 || w.LockPath != lock || w.Status() != "running" {
		t.Fatalf("locked child: %+v", w)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	delete(wins, "child")
	s.external(table, wins)
	if _, ok := wins["child"]; ok {
		t.Fatal("unlocked child adopted")
	}
	for _, invalid := range []string{"", "not a pid", "1", "99999"} {
		if err := os.WriteFile(lock, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if len(pidLocks(table)) != 0 {
			t.Fatalf("accepted invalid lock %q", invalid)
		}
	}
}

func TestCommandMatchesExactly(t *testing.T) {
	for _, args := range []string{"worker --serve", "sh -c worker --serve", "/bin/bash -c worker --serve"} {
		if !commandMatches(args, "worker --serve") {
			t.Errorf("did not match %q", args)
		}
	}
	for _, args := range []string{"other-worker --serve", "echo worker --serve", "sh -c echo worker --serve", "worker --serve --extra", "sh -c worker --serve && sleep 10"} {
		if commandMatches(args, "worker --serve") {
			t.Errorf("matched %q", args)
		}
	}
}

func TestLockedStopForcesKillAndRemovesLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	dir := filepath.Join(os.Getenv("HOME"), ".pcx", "pids")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, fmt.Sprintf("%d.lock", cmd.Process.Pid))
	if err := os.WriteFile(lock, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSession(&Config{Name: "pcx-test-locked-stop", Processes: map[string]*Proc{"worker": {Command: "sleep 300"}}})
	if msg := s.stop("worker", syscall.SIGTERM); !strings.Contains(msg, "sent killed") {
		t.Fatalf("stop: %s", msg)
	}
	cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("exit: %v", cmd.ProcessState)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock remains: %v", err)
	}
}
