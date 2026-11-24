package tui

import (
	"context"
	"encoding/json"
	"exectop/src/coordinator"
	"exectop/src/metrics"
	"exectop/src/runner"
	"exectop/src/sanitize"
	"exectop/src/spec"
	"exectop/src/status"
	"fmt"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type RequestFunc func(context.Context, []string, bool) ([]runner.Result, error)
type WatchEvent struct {
	State  string
	Result *runner.Result
	Spec   *spec.Spec
}
type EventFunc func(context.Context) (<-chan WatchEvent, error)
type ControlFunc func(context.Context, string) error
type HistoryFunc func(context.Context, string) ([]string, error)
type EntryMsg coordinator.Entry
type ResultMsg runner.Result
type tickMsg time.Time
type App struct {
	model           *Model
	selected        int
	detail          bool
	detailScroll    int
	input           textinput.Model
	filtering       bool
	queryBefore     string
	request         RequestFunc
	events          EventFunc
	control         ControlFunc
	historyFunc     HistoryFunc
	history         []string
	paused          bool
	eventCh         <-chan WatchEvent
	err             error
	width, height   int
	changed         map[string]time.Time
	offset          int
	column          int
	columns         []tableColumn
	columnResolver  func(string) []string
	directory       string
	confirmExit     bool
	showHelp        bool
	system          systemHeader
	lastSystemRead  time.Time
	previousCPU     cpuCounters
	havePreviousCPU bool
}

func NewApp(request RequestFunc) *App {
	in := textinput.New()
	in.Prompt = "/ "
	return &App{model: New(), input: in, request: request, changed: map[string]time.Time{}, column: 0, columns: defaultTableColumns()}
}
func NewAppWithEvents(request RequestFunc, events EventFunc) *App {
	a := NewApp(request)
	a.events = events
	return a
}
func (a *App) SetControl(fn ControlFunc)                  { a.control = fn }
func (a *App) SetHistory(fn HistoryFunc)                  { a.historyFunc = fn }
func (a *App) SetFlat(flat bool)                          { a.model.Flat = flat }
func (a *App) SetColumns(keys []string)                   { a.columns = makeTableColumns(keys) }
func (a *App) SetColumnResolver(fn func(string) []string) { a.columnResolver = fn }
func (a *App) activeColumns() []tableColumn {
	if a.columnResolver != nil {
		if c := makeTableColumns(a.columnResolver(a.directory)); len(c) > 0 {
			return c
		}
	}
	if len(a.columns) > 0 {
		return a.columns
	}
	return defaultTableColumns()
}
func (a *App) AddSpec(s spec.Spec) { a.model.Apply(coordinator.Entry{Spec: s}) }
func (a *App) Init() tea.Cmd {
	if a.events != nil {
		return tea.Batch(tickCmd(), a.eventCmd())
	}
	return tickCmd()
}

type resultsMsg []runner.Result
type errMsg error
type eventReady struct{ ch <-chan WatchEvent }
type eventReadyMsg struct{}
type eventMsg WatchEvent
type pagerDoneMsg struct{ err error }

func (a *App) pollCmd() tea.Cmd {
	return func() tea.Msg {
		if a.request == nil {
			return nil
		}
		rs, e := a.request(context.Background(), nil, false)
		if e != nil {
			return errMsg(e)
		}
		return resultsMsg(rs)
	}
}
func (a *App) eventCmd() tea.Cmd {
	return func() tea.Msg {
		ch, e := a.events(context.Background())
		if e != nil {
			return errMsg(e)
		}
		return eventReady{ch}
	}
}
func waitEvent(ch <-chan WatchEvent) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return errMsg(fmt.Errorf("subscription closed"))
		}
		return eventMsg(r)
	}
}
func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
		a.keepSelectedVisible(len(a.model.ViewRows(a.directory)))
	case EntryMsg:
		a.model.Apply(coordinator.Entry(m))
	case ResultMsg:
		if e, ok := a.model.rows[m.ID]; ok {
			r := runner.Result(m)
			e.Latest = &r
			e.Running = false
			a.model.Apply(e)
		}
	case tickMsg:
		if a.events != nil {
			return a, tickCmd()
		}
		return a, tea.Batch(tickCmd(), a.pollCmd())
	case resultsMsg:
		for _, r := range m {
			a.applyResult(r)
		}
	case errMsg:
		a.err = error(m)
		a.showHelp = false
		if a.detail {
			a.detailScroll = 1 << 30
		}
		if a.events != nil {
			return a, tea.Tick(time.Second, func(time.Time) tea.Msg { return eventReadyMsg{} })
		}
	case eventReadyMsg:
		return a, a.eventCmd()
	case eventReady:
		a.eventCh = m.ch
		a.err = nil
		return a, waitEvent(m.ch)
	case eventMsg:
		event := WatchEvent(m)
		if event.Spec != nil && event.State == "added" {
			a.AddSpec(*event.Spec)
		} else if event.Result != nil {
			a.applyResult(*event.Result)
		}
		return a, waitEvent(a.eventCh)
	case pagerDoneMsg:
		a.err = m.err
		a.showHelp = false
		if a.detail && a.err != nil {
			a.detailScroll = 1 << 30
		}
		return a, nil
	case tea.KeyMsg:
		if a.confirmExit {
			switch m.String() {
			case "y", "Y", "enter":
				return a, tea.Quit
			case "n", "N", "esc", "q":
				a.confirmExit = false
			}
			return a, nil
		}
		if a.detail && !a.filtering {
			switch m.String() {
			case "?":
				a.err = fmt.Errorf(detailHelpText())
				a.showHelp = true
				a.detailScroll = 0
			case "p":
				rows := a.model.ViewRows(a.directory)
				if a.selected < len(rows) && rows[a.selected].Kind == CheckRow {
					cmd, err := pagerCommand(rows[a.selected].Entry.Spec.Path)
					if err != nil {
						a.err = err
						a.showHelp = false
						a.detailScroll = 1 << 30
					} else {
						return a, tea.ExecProcess(cmd, func(err error) tea.Msg { return pagerDoneMsg{err: err} })
					}
				}
			case "r":
				rows := a.model.ViewRows(a.directory)
				if a.request != nil && a.selected < len(rows) && rows[a.selected].Kind == CheckRow {
					rs, err := a.request(context.Background(), []string{rows[a.selected].Entry.Spec.ID}, true)
					for _, r := range rs {
						a.applyResult(r)
					}
					a.err = err
					a.showHelp = false
					if err != nil {
						a.detailScroll = 1 << 30
					}
				}
			case "q":
				a.confirmExit = true
				a.detailScroll = 1 << 30
			case "ctrl+c":
				return a, tea.Quit
			case "up", "k":
				if a.detailScroll > 0 {
					a.detailScroll--
				}
			case "down", "j":
				a.detailScroll++
			case " ", "f", "ctrl+f", "pgdown":
				a.detailScroll += a.detailPageSize()
			case "b":
				a.detail = false
				a.detailScroll = 0
			case "ctrl+b", "pgup":
				a.detailScroll -= a.detailPageSize()
				if a.detailScroll < 0 {
					a.detailScroll = 0
				}
			case "ctrl+d":
				a.detailScroll += a.detailPageSize() / 2
			case "ctrl+u":
				a.detailScroll -= a.detailPageSize() / 2
				if a.detailScroll < 0 {
					a.detailScroll = 0
				}
			case "g", "home":
				a.detailScroll = 0
			case "G", "end":
				a.detailScroll = 1 << 30
			case "enter", "esc":
				a.detail = false
				a.detailScroll = 0
			}
			return a, nil
		}
		if a.filtering {
			switch m.String() {
			case "enter":
				a.filtering = false
				a.input.Blur()
				a.model.Query = a.input.Value()
			case "esc":
				a.filtering = false
				a.input.Blur()
				a.model.Query = a.queryBefore
			default:
				var c tea.Cmd
				a.input, c = a.input.Update(m)
				a.model.Query = a.input.Value()
				return a, c
			}
		} else {
			switch m.String() {
			case "q":
				a.confirmExit = true
			case "ctrl+c":
				return a, tea.Quit
			case "up", "k":
				a.column = -1
				if a.selected > 0 {
					a.selected--
				}
				a.keepSelectedVisible(len(a.model.ViewRows(a.directory)))
			case "down", "j":
				a.column = -1
				if a.selected+1 < len(a.model.ViewRows(a.directory)) {
					a.selected++
				}
				a.keepSelectedVisible(len(a.model.ViewRows(a.directory)))
			case "left":
				if a.column < 0 {
					a.column = len(a.activeColumns()) - 1
				} else if a.column > 0 {
					a.column--
				}
			case "right":
				if a.column < 0 {
					a.column = 0
				} else if a.column+1 < len(a.activeColumns()) {
					a.column++
				}
			case "/":
				a.queryBefore = a.model.Query
				a.filtering = true
				a.input.Focus()
				return a, a.input.Focus()
			case "enter":
				rows := a.model.ViewRows(a.directory)
				if a.selected < len(rows) && rows[a.selected].Kind == ParentRow {
					a.directory = rows[a.selected].Directory
					a.selected, a.offset = 0, 0
				} else if a.selected < len(rows) && rows[a.selected].Kind == DirectoryRow {
					a.directory = rows[a.selected].Directory
					a.selected, a.offset = 0, 0
				} else if a.column < 0 && a.selected < len(rows) {
					a.detail = true
					a.detailScroll = 0
				} else {
					if a.column >= 0 {
						a.model.SortBy = a.activeColumns()[a.column].sortBy
					}
				}
			case "esc":
				if a.directory != "" {
					if i := strings.LastIndexByte(a.directory, '/'); i >= 0 {
						a.directory = a.directory[:i]
					} else {
						a.directory = ""
					}
					a.selected, a.offset = 0, 0
				} else {
					a.column = -1
				}
			case "f":
				a.model.ErrorsOnly = !a.model.ErrorsOnly
			case "s", "n", "g", "l", "d", "i", "t", "m":
				for i, column := range a.activeColumns() {
					if column.key == m.String() {
						a.column = i
						break
					}
				}
			case "v":
				a.detail = !a.detail
				a.detailScroll = 0
			case "T":
				a.model.Flat = !a.model.Flat
				a.directory = ""
				a.selected, a.offset = 0, 0
			case "?":
				a.err = fmt.Errorf(operationHelpText())
				a.showHelp = true
			case "p":
				if a.control != nil {
					op := "pause"
					if a.paused {
						op = "resume"
					}
					a.err = a.control(context.Background(), op)
					a.showHelp = false
					if a.err == nil {
						a.paused = !a.paused
					}
				}
			case "h":
				rows := a.model.ViewRows(a.directory)
				if a.historyFunc != nil && a.selected < len(rows) && rows[a.selected].Kind == CheckRow {
					a.history, a.err = a.historyFunc(context.Background(), rows[a.selected].Entry.Spec.ID)
					if a.err != nil {
						a.showHelp = false
					}
					a.detail = true
					a.detailScroll = 0
					if a.err != nil {
						a.detailScroll = 1 << 30
					}
				}
			case "r":
				if a.request != nil {
					rows := a.model.ViewRows(a.directory)
					if a.selected < len(rows) && rows[a.selected].Kind == CheckRow {
						rs, e := a.request(context.Background(), []string{rows[a.selected].Entry.Spec.ID}, true)
						for _, r := range rs {
							a.applyResult(r)
						}
						a.err = e
					}
				}
			case "R":
				if a.request != nil {
					rs, e := a.request(context.Background(), nil, true)
					for _, r := range rs {
						a.applyResult(r)
					}
					a.err = e
				}
			}
		}
	}
	return a, nil
}

func operationHelpText() string {
	return "q quit  r/R run now  s/n/g/l/d/i/t/m select column  T tree/flat  arrows move  Enter open/detail/sort  Esc up/clear column  v detail  f errors  / search  ? help"
}

func detailHelpText() string {
	return "Enter/Esc/b back  Space/f/Ctrl-F/PageDown page down  Ctrl-B/PageUp page up  Ctrl-D/U half page  g/G top/bottom  r run now  p view script  q quit  ? help"
}

func (a *App) detailPageSize() int {
	page := a.height - a.detailFooterLineCount()
	if page < 1 {
		page = 1
	}
	return page
}

func (a *App) detailFooterLineCount() int {
	lines := 1 // operation hints are always present
	if a.showHelp || a.err != nil {
		lines++
	}
	if a.confirmExit {
		lines++
	}
	return lines
}

func (a *App) applyResult(r runner.Result) {
	if e, ok := a.model.rows[r.ID]; ok {
		if r.Changed {
			e.Changed = true
			e.ChangedAt = r.ChangedAt
			if e.ChangedAt.IsZero() {
				e.ChangedAt = time.Now()
			}
			a.model.Apply(e)
			return
		}
		previous := status.Pending
		if e.Latest != nil {
			previous = e.Latest.Status
		}
		if previous != r.Status {
			a.changed[r.ID] = time.Now()
		}
		if r.Status == status.Running {
			e.Running = true
			a.model.Apply(e)
			return
		}
		e.Latest = &r
		e.Running = false
		e.Changed = false
		e.ChangedAt = time.Time{}
		a.model.Apply(e)
	}
}
func (a *App) View() string {
	started := time.Now()
	defer func() { metrics.Observe("tui.render", time.Since(started)) }()
	if a.lastSystemRead.IsZero() || time.Since(a.lastSystemRead) >= time.Second {
		a.system, a.previousCPU = readSystemHeader(a.previousCPU, a.havePreviousCPU)
		a.havePreviousCPU = true
		a.lastSystemRead = time.Now()
	}
	rows := a.model.ViewRows(a.directory)
	a.keepSelectedVisible(len(rows))
	if a.detail && a.selected < len(rows) {
		var e coordinator.Entry
		if rows[a.selected].Kind == CheckRow {
			e = *rows[a.selected].Entry
		} else if first, ok := a.model.FirstEntry(rows[a.selected].Directory); ok {
			// Keep programmatic/detail toggles useful for a single-directory view.
			e = first
		} else {
			return ""
		}
		var b strings.Builder
		detailSummary := "-"
		if e.Latest != nil {
			detailSummary = e.Latest.Summary
		}
		fmt.Fprintf(&b, "%s\n", sanitize.Text([]byte(e.Spec.Name)))
		fmt.Fprintf(&b, "\n%s %s\n%s %s\n%s %s\n%s %s\n%s %s\n",
			detailLabel("STATUS:"), styleStatus(latest(e), fmt.Sprintf("%-10s", latest(e))),
			detailLabel("SUMMARY:"), sanitize.Text([]byte(detailSummary)),
			detailLabel("NEXT:"), nextText(e),
			detailLabel("PATH:"), sanitize.Text([]byte(e.Spec.Path)),
			detailLabel("DESCRIPTION:"), sanitize.Text([]byte(e.Spec.Description)))
		if e.Latest != nil {
			fmt.Fprintf(&b, "%s %d\n", detailLabel("EXIT CODE:"), e.Latest.ExitCode)
			if e.Latest.Signal != "" {
				fmt.Fprintf(&b, "%s %s\n", detailLabel("SIGNAL:"), sanitize.Text([]byte(e.Latest.Signal)))
			}
			if e.Latest.Reason != "" {
				fmt.Fprintf(&b, "%s %s\n", detailLabel("REASON:"), sanitize.Text([]byte(e.Latest.Reason)))
			}
			if e.Latest.Truncated {
				fmt.Fprintf(&b, "%s truncated\n", detailLabel("OUTPUT:"))
			}
		}
		if e.Changed {
			changedAt := e.ChangedAt.Format(time.RFC3339)
			if e.ChangedAt.IsZero() {
				changedAt = "unknown"
			}
			fmt.Fprintf(&b, "%s script or configuration changed; latest result is stale\n%s %s\n", detailLabel("CHANGE:"), detailLabel("CHANGED AT:"), changedAt)
		}
		if e.Spec.Runbook != "" {
			fmt.Fprintf(&b, "%s %s\n", detailLabel("RUNBOOK:"), sanitize.Text([]byte(e.Spec.Runbook)))
		}
		fmt.Fprintf(&b, "%s\n", detailLabel("REFERENCES:"))
		for _, reference := range e.Spec.References {
			fmt.Fprintf(&b, "  %s\n", sanitize.Text([]byte(reference)))
		}
		if len(e.Spec.Sources) > 0 {
			keys := make([]string, 0, len(e.Spec.Sources))
			for key := range e.Spec.Sources {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			fmt.Fprintf(&b, "%s\n", detailLabel("SOURCES:"))
			for _, key := range keys {
				fmt.Fprintf(&b, "  %s=%s\n", sanitize.Text([]byte(key)), sanitize.Text([]byte(e.Spec.Sources[key])))
			}
		}
		if e.Latest != nil {
			fmt.Fprintf(&b, "\n%s\n%s\n%s\n", detailLabel("STDOUT:"), mutedText(sanitize.Text(e.Latest.Stdout)), detailErrorLabel("STDERR:"))
			b.WriteString(mutedText(sanitize.Text(e.Latest.Stderr)) + "\n")
		}
		b.WriteString("\nHISTORY GRAPH: " + historyGraph(e.History, e.Latest, 80) + "\n")
		if len(a.history) > 0 {
			fmt.Fprintf(&b, "\nHISTORY:\n")
			for _, line := range a.history {
				fmt.Fprintf(&b, "%s\n", mutedText(line))
			}
		}
		return a.renderDetail(b.String(), a.detailFooter())
	}
	counts := map[status.Status]int{}
	running := 0
	changed := 0
	checks := 0
	for _, row := range rows {
		if row.Kind == DirectoryRow {
			checks += row.Aggregate.Total
			counts[status.OK] += row.Aggregate.OK
			counts[status.Warn] += row.Aggregate.Warn
			counts[status.Fail] += row.Aggregate.Fail
			counts[status.Timeout] += row.Aggregate.Timeout
			counts[status.Error] += row.Aggregate.Error
			counts[status.Vanished] += row.Aggregate.Vanished
			counts[status.Unknown] += row.Aggregate.Unknown
			counts[status.Running] += row.Aggregate.Running
			counts[status.Pending] += row.Aggregate.Pending
			counts[status.Skip] += row.Aggregate.Skip
			changed += row.Aggregate.Changed
			continue
		}
		if row.Kind == ParentRow {
			continue
		}
		checks++
		e := *row.Entry
		if e.Running {
			running++
		}
		counts[latest(e)]++
		if e.Changed {
			changed++
		}
	}
	viewName := "tree"
	if a.model.Flat {
		viewName = "flat"
	}
	title := lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("exectop  view=%s /%s  checks=%d running=%d changed=%d OK=%d WARN=%d FAIL=%d TIMEOUT=%d VANISHED=%d UNKNOWN=%d", viewName, a.directory, checks, running, changed, counts[status.OK], counts[status.Warn], counts[status.Fail]+counts[status.Error], counts[status.Timeout], counts[status.Vanished], counts[status.Unknown]))
	if a.events != nil && a.err != nil {
		title += "  DISCONNECTED"
	}
	var b strings.Builder
	b.WriteString(title + "\n")
	for _, line := range a.system.lines() {
		b.WriteString(line + "\n")
	}
	b.WriteString("  ")
	columns := a.activeColumns()
	for i, column := range columns {
		if i > 0 {
			b.WriteByte(' ')
		}
		label := column.label + "(" + column.key + ")"
		label = fmt.Sprintf(column.headerFormat, label)
		if a.column >= 0 && i == a.column {
			label = lipgloss.NewStyle().Reverse(true).Render(label)
		}
		b.WriteString(label)
	}
	b.WriteByte('\n')
	startRow := a.offset
	if startRow < 0 {
		startRow = 0
	}
	if startRow > len(rows) {
		startRow = len(rows)
	}
	endRow := len(rows)
	if a.height > 0 {
		visible := a.height - 6 - historyGraphReservedLines(a.height)
		if visible < 1 {
			visible = 1
		}
		if endRow > startRow+visible {
			endRow = startRow + visible
		}
	}
	for i, row := range rows[startRow:endRow] {
		i += startRow
		if row.Kind == DirectoryRow {
			a.renderDirectoryRow(&b, i, row)
			continue
		}
		if row.Kind == ParentRow {
			a.renderParentRow(&b, i)
			continue
		}
		e := *row.Entry
		st := latest(e)
		stText := string(st)
		if e.Changed {
			stText += "*"
		}
		group := ""
		if n := strings.IndexByte(e.Spec.ID, '/'); n >= 0 {
			group = e.Spec.ID[:n]
		}
		summary := "-"
		last := "-"
		duration := "-"
		if e.Running {
			duration = "(" + time.Since(e.RunStart).Round(time.Millisecond).String() + ")"
		}
		if e.Latest != nil {
			summary = e.Latest.Summary
			if e.Latest.Cached {
				last = e.Latest.Age.Round(time.Second).String() + " ago (cached)"
			} else {
				last = e.Latest.Start.Format("15:04:05")
			}
			duration = e.Latest.Duration.Round(time.Millisecond).String()
		}
		summary = fmt.Sprintf("%s next=%s streak=%s", summary, nextText(e), statusStreak(e))
		if avg := historyAverage(e.History); avg > 0 {
			summary += fmt.Sprintf(" avg=%s", avg.Round(time.Millisecond))
		}
		name := sanitize.Text([]byte(e.Spec.Name))
		group = sanitize.Text([]byte(group))
		summary = sanitize.Text([]byte(summary))
		name = truncate(name, 30)
		group = truncate(group, 12)
		summary = truncate(summary, 30)
		hist := historyText(e.History)
		interval := e.Spec.Interval.String()
		cells := make([]string, len(columns))
		for n, column := range columns {
			cells[n] = formatColumn(column, e, stText, name, summary, interval, hist, group, last, duration)
		}
		for n := range cells {
			if a.column >= 0 && n == a.column {
				cells[n] = lipgloss.NewStyle().Reverse(true).Render(cells[n])
			}
		}
		if len(cells) > 0 && (strings.EqualFold(columns[0].key, "s") || strings.EqualFold(columns[0].key, "status")) {
			cells[0] = styleStatus(st, cells[0])
		}
		prefix := "  "
		if i == a.selected {
			prefix = "> "
		}
		line := prefix + strings.Join(cells, " ")
		if i != a.selected {
			if at, ok := a.changed[e.Spec.ID]; ok && time.Since(at) < 2*time.Second {
				line = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render(line)
			}
		}
		b.WriteString(line + "\n")
	}
	if a.filtering {
		b.WriteString(a.input.View())
	}
	b.WriteString("\n")
	b.WriteString(renderWatchHistoryGraph(rows, a.selected, a.width, historyGraphHeight(a.height)))
	if a.err != nil {
		errorLine := "\nX ERROR: " + a.err.Error()
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")).Render(errorLine))
	}
	if a.confirmExit {
		b.WriteString("\n\nExit watch? [y/N]")
	}
	return b.String()
}

func pagerCommand(path string) (*exec.Cmd, error) {
	pager := strings.Fields(os.Getenv("PAGER"))
	if len(pager) == 0 {
		pager = []string{"less"}
	}
	if pager[0] == "" {
		return nil, fmt.Errorf("PAGER is empty")
	}
	args := append(append([]string{}, pager[1:]...), path)
	return exec.Command(pager[0], args...), nil
}

func (a *App) renderDirectoryRow(b *strings.Builder, i int, row ViewRow) {
	st := string(row.Aggregate.Status)
	if row.Aggregate.Changed > 0 {
		st += "*"
	}
	name := truncate(sanitize.Text([]byte(row.Directory+"/")), 30)
	summary := fmt.Sprintf("%d checks: OK=%d WARN=%d FAIL=%d TIMEOUT=%d ERROR=%d UNKNOWN=%d", row.Aggregate.Total, row.Aggregate.OK, row.Aggregate.Warn, row.Aggregate.Fail, row.Aggregate.Timeout, row.Aggregate.Error, row.Aggregate.Unknown)
	if row.Aggregate.Running > 0 {
		summary += fmt.Sprintf(" RUNNING=%d", row.Aggregate.Running)
	}
	if row.Aggregate.Vanished > 0 {
		summary += fmt.Sprintf(" VANISHED=%d", row.Aggregate.Vanished)
	}
	if row.Aggregate.Pending > 0 {
		summary += fmt.Sprintf(" PENDING=%d", row.Aggregate.Pending)
	}
	summary = truncate(sanitize.Text([]byte(summary)), 30)
	if summary == "" {
		summary = "-"
	}
	columns := a.activeColumns()
	cells := make([]string, len(columns))
	for n, column := range columns {
		cells[n] = formatDirectoryColumn(column, row, st, name, summary)
	}
	for n := range cells {
		if a.column >= 0 && n == a.column {
			cells[n] = lipgloss.NewStyle().Reverse(true).Render(cells[n])
		}
	}
	if len(cells) > 0 && (strings.EqualFold(columns[0].key, "s") || strings.EqualFold(columns[0].key, "status")) {
		cells[0] = styleStatus(row.Aggregate.Status, cells[0])
	}
	prefix := "  "
	if i == a.selected {
		prefix = "> "
	}
	b.WriteString(prefix + strings.Join(cells, " ") + "\n")
}

// styleStatus uses ANSI 16-color palette entries and an ASCII marker, keeping status
// recognition usable without distinguishing red/green/yellow hues.
func styleStatus(st status.Status, cell string) string {
	marker := ""
	color := ""
	switch st {
	case status.OK:
		marker, color = "+ ", "2"
	case status.Warn:
		marker, color = "! ", "3"
	case status.Fail, status.Timeout, status.Error, status.Vanished:
		marker, color = "X ", "1"
	case status.Unknown:
		marker, color = "? ", "5"
	case status.Running:
		marker, color = "> ", "6"
	case status.Pending:
		marker, color = ". ", "6"
	case status.Skip:
		marker, color = "- ", "7"
	default:
		return cell
	}
	// The marker replaces leading padding so the column width stays stable.
	content := marker + strings.TrimLeft(cell, " ")
	content = truncate(content, len([]rune(cell)))
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(st != status.OK && st != status.Skip)
	if st == status.Fail || st == status.Timeout || st == status.Error || st == status.Vanished {
		style = style.Background(lipgloss.Color("1")).Foreground(lipgloss.Color("15")).Reverse(true)
	}
	return style.Render(fmt.Sprintf("%-10s", content))
}

func detailLabel(label string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(label)
}

func mutedText(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(text)
}

func detailErrorLabel(label string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")).Render(label)
}

func (a *App) renderParentRow(b *strings.Builder, i int) {
	columns := a.activeColumns()
	cells := make([]string, len(columns))
	for n, column := range columns {
		cells[n] = formatParentColumn(column)
	}
	for n := range cells {
		if a.column >= 0 && n == a.column {
			cells[n] = lipgloss.NewStyle().Reverse(true).Render(cells[n])
		}
	}
	prefix := "  "
	if i == a.selected {
		prefix = "> "
	}
	b.WriteString(prefix + strings.Join(cells, " ") + "\n")
}

type tableColumn struct {
	label, key, sortBy, headerFormat string
}

var tableColumns = []tableColumn{
	{label: "ST", key: "s", sortBy: "status", headerFormat: "%-10s"},
	{label: "NAME", key: "n", sortBy: "name", headerFormat: "%-30s"},
	{label: "SUMMARY", key: "m", sortBy: "summary", headerFormat: "%-30s"},
	{label: "INT", key: "i", sortBy: "interval", headerFormat: "%-10s"},
	{label: "HIST", key: "t", sortBy: "history", headerFormat: "%-8s"},
	{label: "GROUP", key: "g", sortBy: "group", headerFormat: "%-12s"},
	{label: "LAST", key: "l", sortBy: "last", headerFormat: "%-20s"},
	{label: "DUR", key: "d", sortBy: "duration", headerFormat: "%-10s"},
}

func defaultTableColumns() []tableColumn { return append([]tableColumn(nil), tableColumns...) }

func makeTableColumns(keys []string) []tableColumn {
	known := map[string]tableColumn{}
	for _, c := range tableColumns {
		known[strings.ToLower(c.key)] = c
		known[strings.ToLower(c.label)] = c
	}
	out := make([]tableColumn, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if c, ok := known[strings.ToLower(key)]; ok {
			out = append(out, c)
			continue
		}
		out = append(out, tableColumn{label: key, key: key, headerFormat: "%-20s"})
	}
	return out
}

func formatColumn(c tableColumn, e coordinator.Entry, st, name, summary, interval, hist, group, last, duration string) string {
	values := map[string]string{"s": st, "status": st, "n": name, "name": name, "m": summary, "summary": summary, "i": interval, "interval": interval, "t": hist, "history": hist, "g": group, "group": group, "l": last, "last": last, "d": duration, "duration": duration, "path": e.Spec.ID}
	v := values[strings.ToLower(c.key)]
	if v == "" && e.Latest != nil {
		if x, ok := e.Latest.Variables[c.key]; ok {
			b, _ := json.Marshal(x)
			v = string(b)
		}
	}
	if v == "" {
		v = "-"
	}
	v = truncate(sanitize.Text([]byte(v)), 30)
	return fmt.Sprintf(c.headerFormat, v)
}

func formatDirectoryColumn(c tableColumn, row ViewRow, st, name, summary string) string {
	switch strings.ToLower(c.key) {
	case "s", "status":
		return fmt.Sprintf(c.headerFormat, st)
	case "n", "name", "path":
		return fmt.Sprintf(c.headerFormat, name)
	case "m", "summary":
		return fmt.Sprintf(c.headerFormat, summary)
	default:
		return fmt.Sprintf(c.headerFormat, "-")
	}
}

func formatParentColumn(c tableColumn) string {
	if strings.EqualFold(c.key, "n") || strings.EqualFold(c.key, "name") || strings.EqualFold(c.key, "path") {
		return fmt.Sprintf(c.headerFormat, "..")
	}
	return fmt.Sprintf(c.headerFormat, "-")
}

func (a *App) scrollDetail(content string) string {
	lines := strings.Split(content, "\n")
	visible := a.height - 1
	if visible < 1 || len(lines) <= visible {
		return content
	}
	maxOffset := len(lines) - visible
	if a.detailScroll < 0 {
		a.detailScroll = 0
	}
	if a.detailScroll > maxOffset {
		a.detailScroll = maxOffset
	}
	return strings.Join(lines[a.detailScroll:a.detailScroll+visible], "\n")
}

func (a *App) detailFooter() []string {
	footer := make([]string, 0, a.detailFooterLineCount())
	if a.showHelp {
		footer = append(footer, detailLabel("HELP:")+" "+detailHelpText())
	} else if a.err != nil {
		footer = append(footer, detailErrorLabel("ERROR: ")+a.err.Error())
	}
	footer = append(footer, "Enter: back  p: view script  r: run now  q: quit")
	if a.confirmExit {
		footer = append(footer, "Exit watch? [y/N]")
	}
	return footer
}

// renderDetail keeps the footer anchored to the bottom of the viewport while
// only the detail body follows the pager offset.
func (a *App) renderDetail(body string, footer []string) string {
	bodyLines := strings.Split(body, "\n")
	if a.height <= 0 {
		return strings.Join(append(bodyLines, footer...), "\n")
	}
	visible := a.height - len(footer)
	if visible < 1 {
		visible = 1
	}
	maxOffset := len(bodyLines) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if a.detailScroll < 0 {
		a.detailScroll = 0
	}
	if a.detailScroll > maxOffset {
		a.detailScroll = maxOffset
	}
	end := a.detailScroll + visible
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	viewport := append([]string(nil), bodyLines[a.detailScroll:end]...)
	for len(viewport) < visible {
		viewport = append(viewport, "")
	}
	return strings.Join(append(viewport, footer...), "\n")
}

func (a *App) keepSelectedVisible(n int) {
	if n == 0 {
		a.offset = 0
		return
	}
	if a.selected >= n {
		a.selected = n - 1
	}
	visible := a.height - 2
	if visible < 1 {
		visible = 1
	}
	if a.selected < a.offset {
		a.offset = a.selected
	}
	if a.selected >= a.offset+visible {
		a.offset = a.selected - visible + 1
	}
}
func historyText(h []coordinator.HistoryPoint) string {
	if len(h) == 0 {
		return "-"
	}
	start := 0
	if len(h) > 20 {
		start = len(h) - 20
	}
	var b strings.Builder
	for _, p := range h[start:] {
		switch p.Status {
		case status.OK:
			b.WriteByte('O')
		case status.Warn:
			b.WriteByte('W')
		case status.Fail, status.Timeout, status.Error, status.Vanished:
			b.WriteByte('X')
		case status.Skip:
			b.WriteByte('-')
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func historyGraph(h []coordinator.HistoryPoint, latestResult *runner.Result, width int) string {
	points := append([]coordinator.HistoryPoint(nil), h...)
	if latestResult != nil && (len(points) == 0 || !points[len(points)-1].Start.Equal(latestResult.Start)) {
		points = append(points, coordinator.HistoryPoint{Status: latestResult.Status, Duration: latestResult.Duration, Start: latestResult.Start, ExitCode: latestResult.ExitCode, Summary: latestResult.Summary})
	}
	if width < 1 {
		return "-"
	}
	if len(points) > width {
		points = points[len(points)-width:]
	}
	var b strings.Builder
	for _, p := range points {
		switch p.Status {
		case status.OK:
			b.WriteRune('O')
		case status.Warn:
			b.WriteRune('W')
		case status.Fail, status.Timeout, status.Error:
			b.WriteRune('X')
		case status.Unknown:
			b.WriteRune('?')
		case status.Skip:
			b.WriteRune('-')
		case status.Running:
			b.WriteRune('>')
		default:
			b.WriteRune('.')
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String() + "  (O OK W WARN X FAIL ? UNKNOWN - SKIP)"
}

// renderWatchHistoryGraph draws the selected check's execution history as a
// compact chronological list. A list is deliberately used instead of a bar
// chart: it makes both the status and the actual duration directly readable.
func renderWatchHistoryGraph(rows []ViewRow, selected, width, height int) string {
	if selected < 0 || selected >= len(rows) || rows[selected].Kind != CheckRow || rows[selected].Entry == nil {
		return "HISTORY: -"
	}
	e := rows[selected].Entry
	points := append([]coordinator.HistoryPoint(nil), e.History...)
	if e.Latest != nil && (len(points) == 0 || !points[len(points)-1].Start.Equal(e.Latest.Start)) {
		points = append(points, coordinator.HistoryPoint{Status: e.Latest.Status, Duration: e.Latest.Duration, Start: e.Latest.Start})
	}
	if len(points) == 0 {
		return "HISTORY " + truncate(sanitize.Text([]byte(e.Spec.Name)), 24) + ": -"
	}
	if height <= 0 {
		return "HISTORY " + truncate(sanitize.Text([]byte(e.Spec.Name)), 24) + ": " + historyText(points)
	}
	if width < 1 {
		width = 1
	}
	// Account for the label and a little breathing room around the chart.
	chartWidth := width - 25
	if chartWidth < 1 {
		chartWidth = width
	}
	if len(points) > chartWidth {
		points = points[len(points)-chartWidth:]
	}
	if height < 1 {
		height = 1
	}
	if height > 6 {
		height = 6
	}
	if len(points) > height {
		points = points[len(points)-height:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HISTORY %-24s\n", truncate(sanitize.Text([]byte(e.Spec.Name)), 24))
	for i, p := range points {
		newest := ""
		if i == len(points)-1 {
			newest = "  <- newest"
		}
		line := fmt.Sprintf("  %s  %-7s %9s", p.Start.Format("15:04:05"), historyStatusLabel(p.Status), p.Duration.Round(time.Millisecond))
		fmt.Fprintf(&b, "%s%s\n", mutedText(line), newest)
	}
	b.WriteString("  O OK  W WARN  X FAIL  ? UNKNOWN  - SKIP\n")
	return strings.TrimRight(b.String(), "\n")
}

func historyStatusLabel(st status.Status) string {
	switch st {
	case status.OK:
		return "OK"
	case status.Warn:
		return "WARN"
	case status.Fail:
		return "FAIL"
	case status.Timeout:
		return "TIMEOUT"
	case status.Error:
		return "ERROR"
	case status.Vanished:
		return "VANISHED"
	case status.Unknown:
		return "UNKNOWN"
	case status.Running:
		return "RUNNING"
	case status.Pending:
		return "PENDING"
	case status.Skip:
		return "SKIP"
	default:
		return "-"
	}
}

func historyGraphRune(st status.Status) byte {
	switch st {
	case status.OK:
		return 'O'
	case status.Warn:
		return 'W'
	case status.Fail, status.Timeout, status.Error, status.Vanished:
		return 'X'
	case status.Unknown:
		return '?'
	case status.Skip:
		return '-'
	case status.Running:
		return '>'
	default:
		return '.'
	}
}

func historyGraphHeight(screenHeight int) int {
	switch {
	case screenHeight >= 14:
		return 5
	case screenHeight >= 10:
		return 3
	case screenHeight >= 7:
		return 1
	default:
		return 0
	}
}

func historyGraphReservedLines(screenHeight int) int {
	if historyGraphHeight(screenHeight) == 0 {
		return 1
	}
	return historyGraphHeight(screenHeight) + 4
}

func statusStreak(e coordinator.Entry) string {
	current := latest(e)
	count := 0
	if e.Latest != nil {
		count = 1
	}
	for i := len(e.History) - 1; i >= 0; i-- {
		if e.Latest != nil && i == len(e.History)-1 && e.History[i].Start.Equal(e.Latest.Start) {
			continue
		}
		if e.History[i].Status != current {
			break
		}
		count++
	}
	if count == 0 {
		return "-"
	}
	return fmt.Sprintf("%sx%d", current, count)
}

func historyAverage(h []coordinator.HistoryPoint) time.Duration {
	if len(h) == 0 {
		return 0
	}
	var total time.Duration
	for _, p := range h {
		total += p.Duration
	}
	return total / time.Duration(len(h))
}

func nextText(e coordinator.Entry) string {
	if e.Running {
		return "running"
	}
	if e.Latest == nil {
		return "pending"
	}
	next := e.Latest.NextEligible
	if next.IsZero() && !e.Latest.Start.IsZero() && e.Spec.Interval > 0 {
		next = e.Latest.Start.Add(e.Spec.Interval)
	}
	if next.IsZero() {
		return "unknown"
	}
	remaining := time.Until(next)
	if remaining <= 0 {
		return "due"
	}
	return remaining.Round(time.Second).String()
}
func latest(e coordinator.Entry) status.Status {
	if e.Latest == nil {
		return status.Pending
	}
	return e.Latest.Status
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "."
	}
	return string(r[:max-1]) + "."
}
