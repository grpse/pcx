package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHerdrContextSelection(t *testing.T) {
	t.Setenv("PCX_HERDR_SESSION", "")
	t.Setenv("HERDR_ENV", "")
	if got := newHerdrBackend(); got.Session != "pcx" {
		t.Fatalf("outside Herdr: %+v", got)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SESSION", "my-session")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr-example.sock")
	if got := newHerdrBackend(); got.Session != "my-session" || got.Socket != "/tmp/herdr-example.sock" {
		t.Fatalf("inherited context: %+v", got)
	}
	t.Setenv("PCX_HERDR_SESSION", "explicit")
	if got := newHerdrBackend(); got.Session != "explicit" {
		t.Fatalf("explicit session: %+v", got)
	}
	if got := currentHerdrBackend(); got.Session != "my-session" {
		t.Fatalf("views should use calling session: %+v", got)
	}
}

func TestHerdrOpeningCommandsWaitForBashStartup(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("no herdr")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	// Unix socket paths must fit sockaddr_un even after Herdr adds its suffix.
	home, err := os.MkdirTemp("", "pcx-shell-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	// Simulate startup code which consumes input before readline is ready.
	startup := "printf 'initializing shell\\n'\nsleep 0.4\nread -r -t 0.2 discarded || :\nPS1='pcx-ready> '\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(startup), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "herdr.toml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("onboarding = false\n[terminal]\ndefault_shell = %q\n", bash)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("HERDR_CONFIG_PATH", config)
	t.Setenv("PCX_HERDR_SESSION", fmt.Sprintf("pcx-startup-%d", os.Getpid()))
	backend := newHerdrBackend()
	if err := backend.ensureServer(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		backend.command("session", "stop", backend.Session).Run()
		backend.command("session", "delete", backend.Session).Run()
	})
	var workspace herdrCreated
	if err := backend.call(&workspace, "workspace", "create", "--cwd", home, "--label", "startup-test", "--no-focus"); err != nil {
		t.Fatal(err)
	}
	checkStarted := func(pane, mode string) {
		t.Helper()
		marker := filepath.Join(home, mode+"-executed")
		if err := backend.runCommand(pane, "printf executed >> "+shellQuote(marker)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			data, err := os.ReadFile(marker)
			if err == nil && string(data) == "executed" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s opening command was not executed automatically exactly once", mode)
	}
	checkStarted(workspace.RootPane.ID, "workspace")
	var tab herdrCreated
	if err := backend.call(&tab, "tab", "create", "--workspace", workspace.Workspace.ID, "--cwd", home, "--no-focus"); err != nil {
		t.Fatal(err)
	}
	checkStarted(tab.RootPane.ID, "tab")
	var pane herdrCreated
	if err := backend.call(&pane, "pane", "split", workspace.RootPane.ID, "--direction", "right", "--cwd", home, "--no-focus"); err != nil {
		t.Fatal(err)
	}
	checkStarted(pane.Pane.ID, "pane")
}

func herdrTestConfig(t *testing.T, body string) *Config {
	t.Helper()
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("no herdr")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "process-compose-x.yaml")
	if err := os.WriteFile(path, []byte("name: pcx-test-"+filepath.Base(dir)+"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCapturedLogsExitStatusAndRestart(t *testing.T) {
	cfg := herdrTestConfig(t, `processes:
  writer.with:punctuation:
    command: '/bin/sh env-probe.sh'
    environment: ["PCX_TEST_EXTRA=from-yaml"]
`)
	if err := os.WriteFile(filepath.Join(cfg.Dir, "env-probe.sh"), []byte("printf 'pcx-log:%s:%s\\n' \"$PCX_TEST_EXTRA\" \"$HERDR_PANE_ID\"\npwd\nprintf 'stderr-output\\n' >&2\n[ -t 1 ] && [ -t 2 ] && echo tty-output\nexit 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PCX_TEST_EXTRA", "from-invoker")
	t.Setenv("HERDR_PANE_ID", "stale-caller-id")
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })
	name := "writer.with:punctuation"
	if msg := s.Start(name); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	w := waitFor2(t, s, name, func(w Window) bool { return w.Dead && w.ExitCode == 7 })
	if got := w.Status(); got != "exited(7)" {
		t.Fatal(got)
	}
	// Terminal output arrives asynchronously after the runner's exit record.
	var output string
	for i := 0; i < 100; i++ {
		var err error
		output, err = s.Logs(name, 100)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output, "pcx-log:from-yaml:\n") && strings.Contains(output, cfg.Dir) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(output, "pcx-log:from-yaml:\n") || strings.Contains(output, "stale-caller-id") || !strings.Contains(output, cfg.Dir) || !strings.Contains(output, "stderr-output") || !strings.Contains(output, "tty-output") {
		t.Fatalf("wrong environment, pane context, or directory: %q", output)
	}
	if _, err := s.Logs(name, 0); err == nil {
		t.Fatal("accepted zero log lines")
	}
	if msg := s.Restart(name); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	previousPID := w.PID
	w2 := waitFor2(t, s, name, func(w Window) bool { return w.Dead && w.ExitCode == 7 && w.PID != previousPID })
	if w2.LogPath != w.LogPath || w2.StatePath == w.StatePath {
		t.Fatalf("restart lost log history or reused exit state: %+v -> %+v", w, w2)
	}
}

func TestHerdrConcurrentClientsStartOneCopy(t *testing.T) {
	cfg := herdrTestConfig(t, `processes:
  worker:
    command: 'echo pcx-concurrent; sleep 300'
`)
	a, b := NewSession(cfg), NewSession(cfg)
	t.Cleanup(func() { a.Kill() })
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for _, s := range []*Session{a, b} {
		wg.Add(1)
		go func(s *Session) { defer wg.Done(); results <- s.Start("worker") }(s)
	}
	wg.Wait()
	close(results)
	started := 0
	for msg := range results {
		if strings.Contains(msg, ": started") {
			started++
		} else if !strings.Contains(msg, "already running") {
			t.Fatal(msg)
		}
	}
	if started != 1 {
		t.Fatalf("started %d copies", started)
	}
	wa, wb := a.Windows()["worker"], b.Windows()["worker"]
	if wa.PID == 0 || wa.PID != wb.PID {
		t.Fatalf("clients disagree: %+v %+v", wa, wb)
	}
}

func TestHerdrPreviewAndDebugDoNotStopProcesses(t *testing.T) {
	cfg := herdrTestConfig(t, `processes:
  worker:
    command: '/bin/sh live.sh'
  quiet:
    command: 'sleep 302'
  other:
    command: 'echo pcx-other-preview; sleep 301'
`)
	if err := os.WriteFile(filepath.Join(cfg.Dir, "live.sh"), []byte("echo pcx-preview\nwhile [ ! -f output-ready ]; do sleep 0.1; done\necho new-live-output\nsleep 300\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := newHerdrBackend()
	if err := backend.ensureServer(); err != nil {
		t.Fatal(err)
	}
	var ui herdrCreated
	if err := backend.call(&ui, "workspace", "create", "--label", "pcx-test-ui", "--cwd", cfg.Dir, "--no-focus"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.call(nil, "workspace", "close", ui.Workspace.ID) })
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SESSION", backend.Session)
	t.Setenv("HERDR_PANE_ID", ui.RootPane.ID)
	t.Setenv("HERDR_SOCKET_PATH", "")
	s := NewSession(cfg)
	t.Cleanup(func() { s.PeekClose(); s.DebugClose(); s.Kill() })
	before, err := backend.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cfg.names() {
		if msg := s.Start(name); !strings.Contains(msg, "started") {
			t.Fatal(msg)
		}
	}
	after, err := backend.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Workspaces) != len(before.Workspaces) || len(after.Tabs) != len(before.Tabs) || len(after.Panes) != len(before.Panes) {
		t.Fatalf("starting processes changed Herdr topology: %+v -> %+v", before, after)
	}
	pid := s.Windows()["worker"].PID
	if err := s.Peek("worker"); err != nil {
		t.Fatal(err)
	}
	if !s.Peeking() {
		t.Fatal("preview pane not created")
	}
	if err := s.Peek("other"); err != nil {
		t.Fatal(err)
	}
	var view viewSpec
	if err := readJSON(s.peekFile, &view); err != nil || view.LogPath != s.Windows()["other"].LogPath {
		t.Fatalf("preview did not follow selection: %+v %v", view, err)
	}
	var rendered string
	for i := 0; i < 100; i++ {
		out, err := s.views.output("pane", "read", s.peekPane, "--format", "text", "--lines", "100")
		if err != nil {
			t.Fatal(err)
		}
		rendered = string(out)
		if strings.Contains(rendered, "pcx-other-preview") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(rendered, "pcx-other-preview") {
		t.Fatalf("preview did not render process output: %q", rendered)
	}
	for _, launcher := range []string{"__pcx_run", "__pcx_view", "printf started", "/bin/sh -c"} {
		if strings.Contains(rendered, launcher) {
			t.Fatalf("preview included launcher %q: %q", launcher, rendered)
		}
	}
	if err := s.Peek("quiet"); err != nil {
		t.Fatal(err)
	}
	waitRendered(t, s, s.peekPane, "No output yet")
	if err := s.Peek("worker"); err != nil {
		t.Fatal(err)
	}
	waitRendered(t, s, s.peekPane, "pcx-preview")
	if err := os.WriteFile(filepath.Join(cfg.Dir, "output-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitRendered(t, s, s.peekPane, "new-live-output")
	if err := s.ShowCommands("worker"); err != nil {
		t.Fatal(err)
	}
	if !s.paneExists(s.cmdPane) {
		t.Fatal("command pane not created")
	}
	s.HideCommands()
	if open, err := s.ToggleDebug(); err != nil || !open {
		t.Fatalf("debug pane: %v %v", open, err)
	}
	s.DebugClose()
	s.PeekClose()
	if s.Peeking() {
		t.Fatal("preview pane still exists")
	}
	if w := s.Windows()["worker"]; w.Dead || w.PID != pid {
		t.Fatalf("closing preview stopped process: %+v", w)
	}
	for _, mode := range []string{OpenPane, OpenTab, OpenWorkspace} {
		cmd, err := s.Open("worker", mode)
		if err != nil || cmd != nil {
			t.Fatalf("open %s: %v %v", mode, cmd, err)
		}
		if !s.paneExists(s.lastOutput) {
			t.Fatalf("no %s output view", mode)
		}
		if err := s.views.call(nil, "pane", "close", s.lastOutput); err != nil {
			t.Fatal(err)
		}
		if w := s.Windows()["worker"]; w.Dead || w.PID != pid {
			t.Fatalf("closing %s view stopped process: %+v", mode, w)
		}
	}
	if err := s.Kill(); err != nil {
		t.Fatal(err)
	}
	if !s.paneExists(ui.RootPane.ID) {
		t.Fatal("kill closed an unrelated workspace")
	}
	if _, err := backend.snapshot(); err != nil {
		t.Fatalf("kill stopped Herdr server: %v", err)
	}
}

func TestHerdrRunnerLaunchFilesArePrivateAndConsumed(t *testing.T) {
	cfg := herdrTestConfig(t, fmt.Sprintf("processes:\n  worker:\n    command: 'echo private-launch-%d; sleep 300'\n", os.Getpid()))
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })
	if msg := s.Start("worker"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	w := s.Windows()["worker"]
	info, err := os.Stat(w.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions: %v", info.Mode())
	}
	if _, err := os.Stat(strings.TrimSuffix(w.StatePath, ".state")); !os.IsNotExist(err) {
		t.Fatalf("launch environment retained: %v", err)
	}
}

func waitRendered(t *testing.T, s *Session, pane, want string) string {
	t.Helper()
	var rendered string
	for i := 0; i < 100; i++ {
		out, err := s.views.output("pane", "read", pane, "--format", "text", "--lines", "100")
		if err != nil {
			t.Fatal(err)
		}
		rendered = string(out)
		if strings.Contains(rendered, want) {
			return rendered
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tail did not render %q: %q", want, rendered)
	return ""
}

func TestProcessManagementNeverInvokesHerdr(t *testing.T) {
	cfg := herdrTestConfig(t, "processes:\n  worker:\n    command: 'echo detached-tail; sleep 303'\n")
	dir := t.TempDir()
	called := filepath.Join(dir, "herdr-called")
	script := "#!/bin/sh\ntouch " + shellQuote(called) + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	s := NewSession(cfg)
	t.Cleanup(func() { s.Kill() })
	if _, err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	if msg := s.Start("worker"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	waitFor2(t, s, "worker", func(w Window) bool { return !w.Dead })
	if err := NewSession(cfg).Adopt(); err != nil {
		t.Fatal(err)
	}
	if msg := s.Restart("worker"); !strings.Contains(msg, "started") {
		t.Fatal(msg)
	}
	if _, err := s.Logs("worker", 20); err != nil {
		t.Fatal(err)
	}
	s.Snapshot()
	s.Stop("worker", syscall.SIGTERM)
	if err := s.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("background process management invoked Herdr: %v", err)
	}
}
