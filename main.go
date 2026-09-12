package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"
)

const usage = `pcx - tmux-backed process manager

  pcx                      open the TUI (starts the session, autostarts enabled processes)
  pcx up [name...]         start processes detached (no names: every non-disabled one)
  pcx down [-9] [name...]  stop processes (SIGTERM, or SIGKILL with -9)
                           down, restart and kill ask before acting
  pcx restart [name...]
  pcx status
  pcx logs <name> [-n N]   dump a process's output from the tmux scrollback
  pcx attach <name> [--pane|--tab|--workspace]
                           show a process's live output: split the current
                           window, open a new window, or switch to the pcx
                           session (default). Outside tmux, all three attach.
  pcx kill                 kill the whole tmux session
  pcx sessions             list running pcx sessions and their config files

A config gets one session, named after a hash of its contents, so opening the
same file from anywhere reattaches to the processes already running for it; a
second session for the same file is folded into it, windows and all. A process
already running some other way shows as "external": pcx will not start a second
copy, and down/restart act on the one that is running.

  -f <file>                config file (default: search up for process-compose-x.yaml)
  -n <id>                  run a second, independent instance of the same config
  -y                       skip the confirmation on destructive commands
`

func main() {
	fs := flag.NewFlagSet("pcx", flag.ExitOnError)
	file := fs.String("f", "", "config file")
	id := fs.String("n", "", "instance id (default: a hash of the config's contents)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()

	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "sessions" { // global: no config needed
		listSessions()
		return
	}

	path, err := FindConfig(*file)
	check(err)
	cfg, err := Load(path)
	check(err)
	if *id != "" {
		cfg.Name, cfg.Derived = sanitize(*id), false
	}
	sess := NewSession(cfg)
	sess.Adopt()

	// accepted anywhere, so `pcx down web -y` works like `pcx -y down web`
	args, yes := takeFlag(args, "-y", "--yes")

	switch cmd {
	case "":
		created, err := sess.Ensure()
		check(err)
		if created {
			for _, n := range cfg.names() {
				if !cfg.Processes[n].Disabled {
					sess.Start(n)
				}
			}
		}
		check(runTUI(cfg, sess))

	case "up":
		for _, n := range pick(cfg, args, true) {
			fmt.Println(sess.Start(n))
		}

	case "down":
		args, hard := takeFlag(args, "-9", "--kill")
		sig := syscall.SIGTERM
		if hard {
			sig = syscall.SIGKILL
		}
		names := pick(cfg, args, false)
		confirm(yes, "send %v to %s", sig, describe(names))
		for _, n := range names {
			fmt.Println(sess.Stop(n, sig))
		}

	case "restart":
		names := pick(cfg, args, false)
		confirm(yes, "restart %s", describe(names))
		for _, n := range names {
			fmt.Println(sess.Restart(n))
		}

	case "status":
		printStatus(cfg, sess)

	case "logs":
		if len(args) == 0 {
			fatal("logs: need a process name")
		}
		name, lines := args[0], 200
		for i, a := range args {
			if a == "-n" && i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				if err != nil {
					fatal("logs: -n wants a number")
				}
				lines = n
			}
		}
		out, err := sess.Logs(name, lines)
		check(err)
		fmt.Print(out)

	case "attach":
		mode := OpenWorkspace
		for _, m := range OpenModes {
			var found bool
			if args, found = takeFlag(args, "--"+m); found {
				mode = m
			}
		}
		if len(args) == 0 {
			fatal("attach: need a process name")
		}
		c, err := sess.Open(args[0], mode)
		check(err)
		if c != nil {
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			check(c.Run())
		}

	case "kill":
		confirm(yes, "kill session %s and every process in it", cfg.Name)
		check(sess.Kill())
		fmt.Println(cfg.Name + ": session killed")

	default:
		fatal("unknown command %q\n\n%s", cmd, usage)
	}
}

// pick resolves the requested names, defaulting to every process. skipDisabled
// only applies to the default set — naming a disabled process still starts it.
func pick(cfg *Config, names []string, skipDisabled bool) []string {
	if len(names) > 0 {
		for _, n := range names {
			if cfg.Processes[n] == nil {
				fatal("unknown process %q", n)
			}
		}
		return names
	}
	var out []string
	for _, n := range cfg.names() {
		if skipDisabled && cfg.Processes[n].Disabled {
			continue
		}
		out = append(out, n)
	}
	return out
}

func printStatus(cfg *Config, sess *Session) {
	table := psTable()
	wins := sess.WindowsWith(table)
	fmt.Printf("%-24s %-16s %-14s %8s %10s %s\n", "NAME", "NAMESPACE", "STATUS", "CPU", "MEM", "PID")
	for _, n := range cfg.names() {
		p := cfg.Processes[n]
		w, ok := wins[n]
		switch {
		case !ok:
			fmt.Printf("%-24s %-16s %-14s\n", n, p.Namespace, "stopped")
		case w.Dead:
			fmt.Printf("%-24s %-16s %-14s\n", n, p.Namespace, w.Status())
		default:
			cpu, rss := totals(table, w.PID)
			fmt.Printf("%-24s %-16s %-14s %7.1f%% %10s %d\n", n, p.Namespace, w.Status(), cpu, human(rss), w.PID)
		}
	}
}

func listSessions() {
	out, _ := tmux("list-sessions", "-F", "#{session_name}")
	for _, n := range strings.Fields(out) {
		// PCX_CONFIG, not the name, is what marks a session as ours
		env, err := tmux("show-environment", "-t", n, "PCX_CONFIG")
		if err != nil {
			continue
		}
		fmt.Printf("%-20s %s\n", n, strings.TrimPrefix(strings.TrimSpace(env), "PCX_CONFIG="))
	}
}

// takeFlag removes a boolean flag from args, wherever it appears.
func takeFlag(args []string, names ...string) ([]string, bool) {
	found := false
	out := args[:0:0]
	for _, a := range args {
		if slices.Contains(names, a) {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

func describe(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return fmt.Sprintf("%d processes (%s)", len(names), strings.Join(names, ", "))
}

// confirm gates destructive commands. Without a terminal to ask on it refuses
// rather than assuming yes, so scripts have to opt in with -y.
func confirm(yes bool, format string, a ...any) {
	if yes {
		return
	}
	if !term.IsTerminal(os.Stdin.Fd()) { // never block a script on an answer
		fatal("%s: no terminal to confirm on, pass -y", fmt.Sprintf(format, a...))
	}
	fmt.Fprintf(os.Stderr, "%s? [y/N] ", fmt.Sprintf(format, a...))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	resp := strings.TrimSpace(line)
	if errors.Is(err, io.EOF) && resp == "" { // no terminal to answer on
		fatal("\nnothing to read an answer from, pass -y to skip the confirmation")
	}
	if resp != "y" && resp != "Y" && resp != "yes" {
		fatal("cancelled")
	}
}

func check(err error) {
	if err != nil {
		fatal("%v", err)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
