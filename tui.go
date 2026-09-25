package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	bold    = lipgloss.NewStyle().Bold(true)
	green   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	yellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	nsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("111")).Bold(true)
	cursorS = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	helpS   = dim
)

type row struct {
	key  string // expansion key
	proc string // owning process, "" for namespace rows
	text string
	leaf bool
	open bool
}

type refreshMsg struct {
	wins  map[string]Window
	table Table
	peek  bool // the peek pane can be closed from tmux, not just with 'l'
}

type tickMsg time.Time

type statusMsg string

type queuedAction struct {
	run func() string
}

type actionDoneMsg struct {
	status  string
	refresh refreshMsg
}

// ask is a prompt waiting on a keypress; any key not in choices cancels it.
type ask struct {
	text    string
	danger  bool
	choices map[string]func(*model) tea.Cmd
}

type model struct {
	cfg      *Config
	sess     *Session
	wins     map[string]Window
	table    Table
	expanded map[string]bool
	rows     []row
	cursor   int
	height   int
	width    int
	status   string
	asking   *ask
	peek     bool
	cmdOff   int  // characters shifted from the tail of command lines (0 = show the end)
	showCmds bool // M: command-list pane above peek/output
	running  bool
	queued   []queuedAction
}

func newModel(cfg *Config, s *Session) model {
	m := model{cfg: cfg, sess: s, expanded: map[string]bool{}, height: 24, width: 100,
		wins: map[string]Window{}, table: Table{Procs: map[int]PS{}, Kids: map[int][]int{}}}
	ns, _ := cfg.Namespaces()
	for _, n := range ns { // namespaces open, process trees collapsed
		m.expanded["ns:"+n] = true
	}
	return m
}

func refresh(s *Session) tea.Cmd {
	return func() tea.Msg {
		snapshot := s.Snapshot()
		return refreshMsg{wins: snapshot.Windows, table: snapshot.Table, peek: snapshot.Peeking}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(refresh(m.sess), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(refresh(m.sess), tick())
	case refreshMsg:
		m.wins, m.table, m.peek = msg.wins, msg.table, msg.peek
		if !m.peek {
			m.showCmds = false
		}
		m.build()
		if m.showCmds {
			m.sess.RefreshCommands(m.row().proc)
		}
	case actionDoneMsg:
		m.running = false
		m.status = msg.status
		m.wins, m.table, m.peek = msg.refresh.wins, msg.refresh.table, msg.refresh.peek
		if !m.peek {
			m.showCmds = false
		}
		m.build()
		return m.startNext()
	case statusMsg:
		m.status = string(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cur := m.row()
	if a := m.asking; a != nil {
		run, ok := a.choices[msg.String()]
		m.asking = nil
		if !ok { // every other key cancels
			m.status = "cancelled"
			return m, nil
		}
		return m, run(&m)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		sess, closePeek := m.sess, m.peek
		cleanup := func() tea.Msg {
			if closePeek {
				sess.PeekClose()
			}
			sess.DebugClose()
			return nil
		}
		return m, tea.Sequence(cleanup, tea.Quit)
	case "down", "j":
		m.cursor = min(m.cursor+1, len(m.rows)-1)
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = len(m.rows) - 1
	case "enter", " ", "tab":
		if !cur.leaf {
			m.expanded[cur.key] = !m.expanded[cur.key]
			m.build()
		}
	case "right": // 'l' is the peek toggle; enter/space still expand
		m.expanded[cur.key] = true
		m.build()
	case "left", "h":
		m.expanded[cur.key] = false
		m.build()
	case "H": // command lines default to the tail; H/L scroll toward start/end
		m.cmdOff++
		m.build()
	case "L":
		if m.cmdOff > 0 {
			m.cmdOff--
		}
		m.build()
	case "M": // command list above the output; off until pressed
		if m.showCmds {
			m.sess.HideCommands()
			m.showCmds = false
			m.status = "commands hidden"
			break
		}
		if cur.proc == "" {
			m.status = "M: put the cursor on a process"
			break
		}
		if !m.peek {
			if err := m.sess.Peek(cur.proc); err != nil {
				m.status = cur.proc + ": " + err.Error()
				break
			}
			m.peek = true
		}
		if err := m.sess.ShowCommands(cur.proc); err != nil {
			m.status = "M: " + err.Error()
			break
		}
		m.showCmds = true
		m.status = "commands"
	case "s":
		return m.enqueue(m.action(m.targets(cur), func(n string) string { return m.sess.Start(n) }, cur.proc))
	case "x":
		return m.confirm(m.targets(cur), "stop", func(n string) string { return m.sess.Stop(n, syscall.SIGTERM) })
	case "X":
		return m.confirm(m.targets(cur), "SIGKILL", func(n string) string { return m.sess.Stop(n, syscall.SIGKILL) })
	case "r":
		return m.confirm(m.targets(cur), "restart", func(n string) string { return m.sess.Restart(n) })
	case "u":
		return m.enqueue(m.action(m.cfg.names(), func(n string) string { return m.sess.Start(n) }, ""))
	case "d":
		return m.confirm(m.cfg.names(), "stop", func(n string) string { return m.sess.Stop(n, syscall.SIGTERM) })
	case "D":
		return m.enqueue(queuedAction{run: func() string {
			open, err := m.sess.ToggleDebug()
			if err != nil {
				return "debug: " + err.Error()
			}
			if open {
				return "debug pane opened"
			}
			return "debug pane closed"
		}})
	case "l": // sneak peek: a pane on the right that follows the cursor
		switch {
		case m.peek:
			m.sess.PeekClose()
			m.peek = false
			m.showCmds = false
			m.status = "peek closed"
		case cur.proc == "":
			m.status = "peek: put the cursor on a process"
		default:
			if err := m.sess.Peek(cur.proc); err != nil {
				m.status = cur.proc + ": " + err.Error()
				break
			}
			m.peek = true
			m.status = "peeking at " + cur.proc
		}

	case "o":
		if cur.proc == "" {
			break
		}
		if w, ok := m.wins[cur.proc]; !ok || w.External {
			m.status = cur.proc + ": no tmux output — not started by pcx"
			break
		}
		m.asking = &ask{
			text: "open " + cur.proc + " in a [p]ane · [t]ab · [w]orkspace",
			choices: map[string]func(*model) tea.Cmd{
				"p": func(*model) tea.Cmd { return m.open(cur.proc, OpenPane)() },
				"t": func(*model) tea.Cmd { return m.open(cur.proc, OpenTab)() },
				"w": func(*model) tea.Cmd { return m.open(cur.proc, OpenWorkspace)() },
			},
		}
	}
	if next := m.row(); next.proc != cur.proc { // the cursor moved to another process
		m.follow(next.proc)
	}
	return m, nil
}

func (m model) row() row {
	if m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return row{}
}

// follow re-points an open peek pane; a no-op when there is none.
func (m *model) follow(proc string) {
	if !m.peek || proc == "" {
		return
	}
	if err := m.sess.Peek(proc); err != nil {
		m.status = proc + ": " + err.Error()
	}
	if m.showCmds {
		m.sess.RefreshCommands(proc)
	}
}

// targets is the row's process, or every process in the namespace when the
// cursor sits on a namespace header.
func (m model) targets(cur row) []string {
	if cur.proc != "" {
		return []string{cur.proc}
	}
	ns, ok := strings.CutPrefix(cur.key, "ns:")
	if !ok {
		return nil
	}
	_, byNS := m.cfg.Namespaces()
	var out []string
	for _, p := range byNS[ns] {
		out = append(out, p.Name)
	}
	return out
}

func (m model) action(names []string, fn func(string) string, follow string) queuedAction {
	return queuedAction{run: func() string {
		status := ""
		for _, n := range names {
			status = fn(n)
		}
		if len(names) > 1 {
			status = fmt.Sprintf("%s: %d processes", status, len(names))
		}
		if follow != "" && m.peek {
			if err := m.sess.Peek(follow); err != nil {
				status = follow + ": " + err.Error()
			} else if m.showCmds {
				m.sess.RefreshCommands(follow)
			}
		}
		return status
	}}
}

func (m model) enqueue(action queuedAction) (tea.Model, tea.Cmd) {
	m.queued = append(m.queued, action)
	return m.startNext()
}

func (m model) startNext() (tea.Model, tea.Cmd) {
	if m.running || len(m.queued) == 0 {
		return m, nil
	}
	action := m.queued[0]
	m.queued = m.queued[1:]
	m.running = true
	sess := m.sess
	return m, func() tea.Msg {
		status := action.run()
		snapshot := sess.Snapshot()
		return actionDoneMsg{
			status: status,
			refresh: refreshMsg{
				wins:  snapshot.Windows,
				table: snapshot.Table,
				peek:  snapshot.Peeking,
			},
		}
	}
}

func (m model) open(name, mode string) func() tea.Cmd {
	sess := m.sess
	return func() tea.Cmd {
		cmd, err := sess.Open(name, mode)
		switch {
		case err != nil:
			return func() tea.Msg { return statusMsg(err.Error()) }
		case cmd == nil:
			return func() tea.Msg { return statusMsg(name + ": opened in a " + mode) }
		default: // hand the terminal over for a full attach
			return tea.ExecProcess(cmd, func(error) tea.Msg { return statusMsg("") })
		}
	}
}

// confirm stages a destructive action behind a y/N prompt.
func (m model) confirm(names []string, verb string, fn func(string) string) (tea.Model, tea.Cmd) {
	if len(names) == 0 {
		return m, nil
	}
	what := names[0]
	if len(names) > 1 {
		what = fmt.Sprintf("%d processes", len(names))
	}
	action := m.action(names, fn, "")
	if len(names) > 1 {
		original := action.run
		action.run = func() string {
			original()
			return fmt.Sprintf("%s: %d processes", verb, len(names))
		}
	}
	run := func(next *model) tea.Cmd {
		updated, cmd := next.enqueue(action)
		*next = updated.(model)
		return cmd
	}
	m.asking = &ask{
		text:    fmt.Sprintf("%s %s? [y/N]", verb, what),
		danger:  true,
		choices: map[string]func(*model) tea.Cmd{"y": run, "Y": run},
	}
	return m, nil
}

// build flattens the tree into the visible rows, honouring expansion state.
func (m *model) build() {
	order, byNS := m.cfg.Namespaces()
	m.rows = m.rows[:0]
	for _, ns := range order {
		running := 0
		for _, p := range byNS[ns] {
			if w, ok := m.wins[p.Name]; ok && !w.Dead {
				running++
			}
		}
		open := m.expanded["ns:"+ns]
		m.rows = append(m.rows, row{
			key:  "ns:" + ns,
			text: fmt.Sprintf("%s %s %s", arrow(open), nsStyle.Render(ns), dim.Render(fmt.Sprintf("(%d/%d running)", running, len(byNS[ns])))),
		})
		if !open {
			continue
		}
		for _, p := range byNS[ns] {
			key := "proc:" + p.Name
			w, live := m.wins[p.Name]
			hasTree := live && !w.Dead
			m.rows = append(m.rows, row{
				key: key, proc: p.Name, leaf: !hasTree,
				text: "  " + arrowIf(hasTree, m.expanded[key]) + " " + m.procLine(p),
			})
			if hasTree && m.expanded[key] {
				m.addNode(w.PID, 2, p.Name)
			}
		}
	}
	m.cursor = clamp(m.cursor, 0, len(m.rows)-1)
}

func (m *model) addNode(pid, depth int, proc string) {
	info := m.table.Procs[pid]
	cpu, rss := totals(m.table, pid)
	key := fmt.Sprintf("pid:%d", pid)
	hasKids := len(m.table.Kids[pid]) > 0
	indent := 2 * depth
	// stats sit at the right; the rest of the row is the command, tail-aligned
	stats := fmt.Sprintf("%6.1f%% %9s  %d", cpu, human(rss), pid)
	cmdW := m.width - indent - 2 - len(stats) - 2
	if cmdW < 16 {
		cmdW = 16
	}
	args := visCmd(info.Args, cmdW, m.cmdOff)
	m.rows = append(m.rows, row{
		key: key, proc: proc, leaf: !hasKids,
		text: strings.Repeat("  ", depth) + arrowIf(hasKids, m.expanded[key]) + " " +
			dim.Render(args) + dim.Render(stats),
	})
	if hasKids && m.expanded[key] {
		for _, kid := range m.table.Kids[pid] {
			m.addNode(kid, depth+1, proc)
		}
	}
}

func (m *model) procLine(p *Proc) string {
	name := fmt.Sprintf("%-24s", trunc(p.Name, 24))
	w, ok := m.wins[p.Name]
	switch {
	case !ok:
		return bold.Render(name) + dim.Render(fmt.Sprintf("%-14s", "stopped")) + dim.Render(trunc(p.Description, 40))
	case w.Dead:
		style := yellow // killed by a signal: usually means we stopped it
		if w.Signal == "" {
			style = red
			if w.ExitCode == 0 {
				style = green
			}
		}
		return bold.Render(name) + style.Render(fmt.Sprintf("%-14s", w.Status())) + dim.Render(trunc(p.Description, 40))
	default:
		cpu, rss := totals(m.table, w.PID)
		n := len(descendants(m.table, w.PID))
		style := green
		if w.External { // ours to manage, but not ours to show output for
			style = yellow
		}
		return bold.Render(name) + style.Render(fmt.Sprintf("%-14s", w.Status())) +
			fmt.Sprintf("%6.1f%% %9s  ", cpu, human(rss)) +
			dim.Render(fmt.Sprintf("pid %-7d %d proc", w.PID, n+1))
	}
}

func (m model) View() string {
	head := bold.Render(" pcx ") + dim.Render(filepath.Base(m.cfg.Path)+" · session "+m.cfg.Name) + "\n"
	body := m.height - 4
	if body < 3 {
		body = 3
	}
	// scroll just far enough to keep the cursor on screen
	off := 0
	if m.cursor >= body {
		off = m.cursor - body + 1
	}

	var b strings.Builder
	b.WriteString(head)
	for i := off; i < len(m.rows) && i < off+body; i++ {
		line := " " + m.rows[i].text
		if i == m.cursor {
			line = cursorS.Render(fmt.Sprintf("%-*s", max(m.width-1, 1), lipgloss.NewStyle().Render(line)))
		}
		b.WriteString(line + "\n")
	}
	for i := len(m.rows) - off; i < body; i++ {
		b.WriteString("\n")
	}
	b.WriteString(helpS.Render(" s start · x stop · X kill · r restart · l peek · o output · M commands · D debug · H/L scroll cmd · u/d all · space expand · q quit") + "\n")
	switch {
	case m.asking != nil:
		style := bold
		if m.asking.danger {
			style = bold.Foreground(red.GetForeground())
		}
		b.WriteString(" " + style.Render(m.asking.text))
	case m.status != "":
		b.WriteString(" " + yellow.Render(m.status))
	}
	return b.String()
}

func arrow(open bool) string {
	if open {
		return "▾"
	}
	return "▸"
}

func arrowIf(has, open bool) string {
	if !has {
		return " "
	}
	return arrow(open)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// visCmd is the command-column viewport: by default the tail (the flags and
// the thing that actually ran), H/L shift toward the start or back to the end.
func visCmd(s string, width, fromEnd int) string {
	if width < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return string(r) + strings.Repeat(" ", width-len(r))
	}
	if fromEnd < 0 {
		fromEnd = 0
	}
	start := len(r) - width - fromEnd
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(r) {
		end = len(r)
		start = end - width
	}
	chunk := append([]rune{}, r[start:end]...)
	if start > 0 {
		chunk[0] = '…'
	}
	if end < len(r) {
		chunk[len(chunk)-1] = '…'
	}
	return string(chunk)
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

func runTUI(cfg *Config, s *Session) error {
	_, err := tea.NewProgram(newModel(cfg, s), tea.WithAltScreen()).Run()
	return err
}
