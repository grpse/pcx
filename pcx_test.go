package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"time"
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
	if got := shellCmd(&Proc{Command: "x", Availability: struct {
		Restart string `yaml:"restart"`
	}{Restart: "always"}}); !strings.Contains(got, "while :;") || !strings.Contains(got, "( x )") {
		t.Fatalf("restart policy not wrapped: %q", got)
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
		next, _ := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		return next.(model)
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
