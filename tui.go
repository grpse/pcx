package main

import (
	"fmt"
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
}

type tickMsg time.Time

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
	return func() tea.Msg { return refreshMsg{wins: s.Windows(), table: psTable()} }
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
		m.wins, m.table = msg.wins, msg.table
		m.build()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cur := row{}
	if m.cursor < len(m.rows) {
		cur = m.rows[m.cursor]
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
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
	case "right", "l":
		m.expanded[cur.key] = true
		m.build()
	case "left", "h":
		m.expanded[cur.key] = false
		m.build()
	case "s":
		return m.each(cur, func(n string) string { return m.sess.Start(n) })
	case "x":
		return m.each(cur, func(n string) string { return m.sess.Stop(n, syscall.SIGTERM) })
	case "X":
		return m.each(cur, func(n string) string { return m.sess.Stop(n, syscall.SIGKILL) })
	case "r":
		return m.each(cur, func(n string) string { return m.sess.Restart(n) })
	case "u":
		for _, p := range m.cfg.Processes {
			m.sess.Start(p.Name)
		}
		m.status = "started all"
		return m, refresh(m.sess)
	case "d":
		for _, p := range m.cfg.Processes {
			m.sess.Stop(p.Name, syscall.SIGTERM)
		}
		m.status = "stopped all"
		return m, refresh(m.sess)
	case "o":
		if cur.proc == "" {
			break
		}
		if _, ok := m.wins[cur.proc]; !ok {
			m.status = cur.proc + ": no output yet, start it first"
			break
		}
		return m, tea.ExecProcess(m.sess.Attach(cur.proc), func(error) tea.Msg { return refreshMsg{m.sess.Windows(), psTable()} })
	}
	return m, nil
}

// each applies fn to the row's process, or to every process in the namespace
// when the cursor is on a namespace header.
func (m model) each(cur row, fn func(string) string) (tea.Model, tea.Cmd) {
	if cur.proc != "" {
		m.status = fn(cur.proc)
		return m, refresh(m.sess)
	}
	if ns, ok := strings.CutPrefix(cur.key, "ns:"); ok {
		_, byNS := m.cfg.Namespaces()
		for _, p := range byNS[ns] {
			m.status = fn(p.Name)
		}
		m.status = ns + ": done"
	}
	return m, refresh(m.sess)
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
			hasKids := live && !w.Dead && len(m.table.Kids[w.PID]) > 0
			m.rows = append(m.rows, row{
				key: key, proc: p.Name, leaf: !hasKids,
				text: "  " + arrowIf(hasKids, m.expanded[key]) + " " + m.procLine(p),
			})
			if hasKids && m.expanded[key] {
				m.addKids(w.PID, 2, p.Name)
			}
		}
	}
	m.cursor = clamp(m.cursor, 0, len(m.rows)-1)
}

func (m *model) addKids(pid, depth int, proc string) {
	for _, kid := range m.table.Kids[pid] {
		info := m.table.Procs[kid]
		cpu, rss := totals(m.table, kid)
		key := fmt.Sprintf("pid:%d", kid)
		hasKids := len(m.table.Kids[kid]) > 0
		// 36 keeps a first-level child's stats under the parent's columns
		args := trunc(info.Args, 36)
		m.rows = append(m.rows, row{
			key: key, proc: proc, leaf: !hasKids,
			text: strings.Repeat("  ", depth) + arrowIf(hasKids, m.expanded[key]) + " " +
				dim.Render(fmt.Sprintf("%-36s", args)) +
				dim.Render(fmt.Sprintf("%6.1f%% %9s  %d", cpu, human(rss), kid)),
		})
		if hasKids && m.expanded[key] {
			m.addKids(kid, depth+1, proc)
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
		return bold.Render(name) + green.Render(fmt.Sprintf("%-14s", "running")) +
			fmt.Sprintf("%6.1f%% %9s  ", cpu, human(rss)) +
			dim.Render(fmt.Sprintf("pid %-7d %d proc", w.PID, n+1))
	}
}

func (m model) View() string {
	head := bold.Render(" pcx ") + dim.Render("session "+m.cfg.Name) + "\n"
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
	b.WriteString(helpS.Render(" s start · x stop · X kill · r restart · o output · u/d all · space expand · q quit") + "\n")
	if m.status != "" {
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
