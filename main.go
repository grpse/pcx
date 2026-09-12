package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

const usage = `pcx - tmux-backed process manager

  pcx                      open the TUI (starts the session, autostarts enabled processes)
  pcx up [name...]         start processes detached (no names: every non-disabled one)
  pcx down [-9] [name...]  stop processes (SIGTERM, or SIGKILL with -9)
  pcx restart [name...]
  pcx status
  pcx logs <name> [-n N]   dump a process's output from the tmux scrollback
  pcx attach <name>        jump to a process's tmux window
  pcx kill                 kill the whole tmux session

  -f <file>                config file (default: search up for process-compose-x.yaml)
`

func main() {
	fs := flag.NewFlagSet("pcx", flag.ExitOnError)
	file := fs.String("f", "", "config file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()

	path, err := FindConfig(*file)
	check(err)
	cfg, err := Load(path)
	check(err)
	sess := NewSession(cfg)

	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

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
		sig := syscall.SIGTERM
		var names []string
		for _, a := range args {
			if a == "-9" || a == "--kill" {
				sig = syscall.SIGKILL
			} else {
				names = append(names, a)
			}
		}
		for _, n := range pick(cfg, names, false) {
			fmt.Println(sess.Stop(n, sig))
		}

	case "restart":
		for _, n := range pick(cfg, args, false) {
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
		if len(args) == 0 {
			fatal("attach: need a process name")
		}
		c := sess.Attach(args[0])
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		check(c.Run())

	case "kill":
		if sess.Exists() {
			_, err := tmux("kill-session", "-t", sess.target())
			check(err)
		}
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
	wins := sess.Windows()
	table := psTable()
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
			fmt.Printf("%-24s %-16s %-14s %7.1f%% %10s %d\n", n, p.Namespace, "running", cpu, human(rss), w.PID)
		}
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
