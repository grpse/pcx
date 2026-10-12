package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type runSpec struct {
	Command   string   `json:"command"`
	Dir       string   `json:"dir"`
	Env       []string `json:"env"`
	StatePath string   `json:"state_path"`
	LogPath   string   `json:"log_path"`
}
type runState struct {
	PID      int    `json:"pid"`
	Dead     bool   `json:"dead"`
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

// Internal entry points also work in test binaries. Managed commands run
// independently of both Herdr and the CLI/TUI that launched them.
func init() {
	if len(os.Args) != 3 {
		return
	}
	switch os.Args[1] {
	case "__pcx_run":
		os.Exit(runManaged(os.Args[2]))
	case "__pcx_view":
		os.Exit(runView(os.Args[2]))
	}
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pcx-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *Session) launch(w Window, p *Proc) error {
	dir, err := processStateDir()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "run-*.json")
	if err != nil {
		return err
	}
	specPath := f.Name()
	f.Close()
	defer os.Remove(specPath)
	w.StatePath = specPath + ".state"
	if w.LogPath == "" {
		w.LogPath = specPath + ".log"
	}
	w.Dead, w.ExitCode = true, -1
	flags := windowEnv(p)
	var env []string
	for i := 1; i < len(flags); i += 2 {
		env = append(env, flags[i])
	}
	spec := runSpec{Command: shellCmd(p), Dir: p.WorkingDir, Env: env, StatePath: w.StatePath, LogPath: w.LogPath}
	if err := writeJSON(specPath, spec); err != nil {
		return err
	}
	if err := writeJSON(w.StatePath, runState{Dead: true, ExitCode: -1}); err != nil {
		return err
	}
	record, err := s.record()
	if err != nil {
		return err
	}
	record.Runs[w.Name] = w
	if err := saveSession(record); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(w.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := exec.Command(exe, "__pcx_run", specPath)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	go command.Wait() // Reap the launcher while this client is still alive.
	for i := 0; i < 250; i++ {
		var state runState
		if readJSON(w.StatePath, &state) == nil && state.PID > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = command.Process.Kill()
	return fmt.Errorf("process runner did not start (see %s)", w.LogPath)
}

func mergedEnvironment(base, overlay []string) []string {
	order := []string{}
	values := map[string]string{}
	for _, env := range [][]string{base, overlay} {
		for _, item := range env {
			k, v, ok := strings.Cut(item, "=")
			if !ok {
				continue
			}
			if _, seen := values[k]; !seen {
				order = append(order, k)
			}
			values[k] = v
		}
	}
	result := make([]string, 0, len(order))
	for _, k := range order {
		result = append(result, k+"="+values[k])
	}
	return result
}

func runManaged(path string) int {
	var spec runSpec
	if err := readJSON(path, &spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = os.Remove(path) // The launch environment can contain secrets.
	c := exec.Command("/bin/sh", "-c", spec.Command)
	c.Dir = spec.Dir
	c.Env = spec.Env
	log, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Close()
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	terminal, err := pty.StartWithSize(c, &pty.Winsize{Rows: 40, Cols: 160})
	if err != nil {
		_ = writeJSON(spec.StatePath, runState{PID: os.Getpid(), Dead: true, ExitCode: 127})
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	defer terminal.Close()
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(log, terminal); close(copied) }()
	state := runState{PID: c.Process.Pid, ExitCode: -1}
	if err := writeJSON(spec.StatePath, state); err != nil {
		signalTree(state.PID, syscall.SIGKILL)
		_ = c.Wait()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case sig := <-signals:
			signalTree(state.PID, sig.(syscall.Signal))
		case <-done:
			select {
			case <-copied:
			case <-time.After(300 * time.Millisecond):
				terminal.Close()
				<-copied
			}
			state.Dead = true
			state.ExitCode = c.ProcessState.ExitCode()
			if status, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				state.Signal = strings.TrimPrefix(strings.ToLower(status.Signal().String()), "sig")
			}
			_ = writeJSON(spec.StatePath, state)
			return 0
		}
	}
}

type viewSpec struct {
	LogPath string `json:"log_path"`
}

// readTail seeks backwards, bounding memory even for long-running processes.
func readTail(path string, lines int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	end := info.Size()
	start := end
	var data []byte
	for start > 0 && len(data) < 1024*1024 {
		size := int64(8192)
		if size > start {
			size = start
		}
		start -= size
		chunk := make([]byte, size)
		n, err := f.ReadAt(chunk, start)
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		data = append(chunk[:n], data...)
		if bytes.Count(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) >= lines {
			break
		}
	}
	trimmed := bytes.TrimSuffix(data, []byte("\n"))
	for i, count := len(trimmed)-1, 0; i >= 0; i-- {
		if trimmed[i] == '\n' {
			count++
			if count == lines {
				return data[i+1:], end, nil
			}
		}
	}
	return data, end, nil
}

func runView(path string) int {
	defer os.Remove(path)
	fmt.Print("\033[?1049h\033[H\033[2J")
	defer fmt.Print("\033[?1049l")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	var current string
	var offset int64
	empty := true
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		var view viewSpec
		if readJSON(path, &view) != nil {
			return 0
		}
		if current != view.LogPath {
			current = view.LogPath
			out, end, err := readTail(current, 100)
			fmt.Print("\033[H\033[2J")
			offset, empty = end, len(out) == 0
			if err != nil {
				fmt.Println(err)
			} else if empty {
				fmt.Print("No output yet. Waiting for output…")
			} else {
				os.Stdout.Write(out)
			}
		} else {
			f, err := os.Open(current)
			if err == nil {
				info, statErr := f.Stat()
				if statErr == nil && info.Size() < offset {
					current = ""
				} else {
					_, _ = f.Seek(offset, io.SeekStart)
					out := make([]byte, 64*1024)
					n, _ := f.Read(out)
					if n > 0 {
						if empty {
							fmt.Print("\033[H\033[2J")
							empty = false
						}
						os.Stdout.Write(out[:n])
						offset += int64(n)
					}
				}
				f.Close()
			}
		}
		select {
		case <-signals:
			return 0
		case <-tick.C:
		}
	}
}
