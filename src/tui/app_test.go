package tui

import (
	"context"
	"exectop/src/coordinator"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
	"time"
)

func TestPagerCommandUsesPagerAndScriptPath(t *testing.T) {
	t.Setenv("PAGER", "less -R")
	cmd, err := pagerCommand("/tmp/check.sh")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path == "" || len(cmd.Args) != 3 || cmd.Args[0] != "less" || cmd.Args[1] != "-R" || cmd.Args[2] != "/tmp/check.sh" {
		t.Fatalf("pager command=%v args=%v", cmd.Path, cmd.Args)
	}
}

func TestHistoryGraphUsesLatestAndTruncates(t *testing.T) {
	now := time.Now()
	h := []coordinator.HistoryPoint{
		{Status: status.OK, Start: now.Add(-2 * time.Minute)},
		{Status: status.Fail, Start: now.Add(-time.Minute)},
	}
	graph := historyGraph(h, &runner.Result{Status: status.Warn, Start: now}, 2)
	if !strings.HasPrefix(graph, "XW") {
		t.Fatalf("history graph=%q", graph)
	}
}

func TestWatchHistoryGraphIsChronologicalAndUsesDurationBars(t *testing.T) {
	now := time.Now()
	a := NewApp(nil)
	a.width, a.height = 100, 14
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	a.model.Apply(coordinator.Entry{
		Spec: spec.Spec{ID: "check", Name: "check"},
		History: []coordinator.HistoryPoint{
			{Status: status.OK, Start: now.Add(-2 * time.Minute), Duration: time.Second},
			{Status: status.Fail, Start: now.Add(-time.Minute), Duration: 3 * time.Second},
		},
	})
	view := a.View()
	if !strings.Contains(view, "HISTORY check") || strings.Contains(view, "(old -> new)") {
		t.Fatalf("history graph header missing: %s", view)
	}
	graphStart := strings.Index(view, "HISTORY check")
	graph := view[graphStart:]
	if strings.Index(graph, "O") > strings.Index(graph, "X") {
		t.Fatalf("history graph is not oldest-to-newest: %s", view)
	}
	if !strings.Contains(view, "FAIL") || !strings.Contains(view, "3s") || !strings.Contains(view, "<- newest") {
		t.Fatalf("history details were not rendered clearly: %s", view)
	}
}

func TestSearchUpdatesWhileTyping(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "alpha", Name: "alpha"})
	a.AddSpec(spec.Spec{ID: "beta", Name: "beta"})
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if a.model.Query != "a" {
		t.Fatalf("query while typing=%q", a.model.Query)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	if got := truncate("あいうえお", 4); got != "あいう." {
		t.Fatalf("truncate=%q", got)
	}
	if got := truncate("abc", 1); got != "." {
		t.Fatalf("single=%q", got)
	}
}

func TestViewIncludesStatusCountsAndScheduleColumns(t *testing.T) {
	a := NewApp(nil)
	a.width, a.height = 180, 20
	now := time.Now()
	a.AddSpec(spec.Spec{ID: "ops/check", Name: "check", Interval: time.Minute, Runbook: "opsrunbook://service/check", References: []string{"https://status.example.test/check"}})
	a.model.Apply(coordinator.Entry{Spec: spec.Spec{ID: "ops/check", Name: "check", Interval: time.Minute, Runbook: "opsrunbook://service/check", References: []string{"https://status.example.test/check"}}, Latest: &runner.Result{ID: "ops/check", Status: status.Timeout, Start: now, End: now, Duration: time.Second}})
	view := a.View()
	for _, want := range []string{"TIMEOUT=1", "LAST", "DUR", "INT", "HIST", "SUMMARY"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %s", want, view)
		}
	}
	a.detail = true
	detail := a.View()
	if !strings.Contains(detail, "RUNBOOK: opsrunbook://service/check") {
		t.Fatalf("detail missing runbook: %s", detail)
	}
	if !strings.Contains(detail, "REFERENCES:") || !strings.Contains(detail, "https://status.example.test/check") {
		t.Fatalf("detail missing references: %s", detail)
	}
	if strings.Index(detail, "STATUS:") > strings.Index(detail, "SUMMARY:") {
		t.Fatalf("summary is not below status: %s", detail)
	}
	if !strings.Contains(view, "> ") {
		t.Fatalf("selected row marker missing: %s", view)
	}
	if !strings.Contains(view, "\n  ST(s)") {
		t.Fatalf("header does not account for row marker width: %s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !strings.Contains(a.View(), "HELP: Enter/Esc/b back") {
		t.Fatalf("detail help missing: %s", a.View())
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	lines := strings.Split(strings.TrimRight(a.View(), "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] != "Exit watch? [y/N]" {
		t.Fatalf("exit confirmation is not at viewport bottom: %s", a.View())
	}
}

func TestViewIncludesTopSystemHeader(t *testing.T) {
	a := NewApp(nil)
	a.width, a.height = 180, 14
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	view := a.View()
	for _, want := range []string{"top - ", "%Cpu(s):", "MiB Mem :", "MiB Swap:"} {
		if !strings.Contains(view, want) {
			t.Fatalf("top header missing %q: %s", want, view)
		}
	}
}

func TestViewRendersConfiguredVariableColumn(t *testing.T) {
	a := NewApp(nil)
	a.width, a.height = 120, 8
	a.SetColumns([]string{"name", "owner", "status"})
	a.AddSpec(spec.Spec{ID: "ops/check", Name: "check"})
	a.model.Apply(coordinator.Entry{Spec: spec.Spec{ID: "ops/check", Name: "check"}, Latest: &runner.Result{ID: "ops/check", Status: status.OK, Variables: map[string]any{"owner": "alice"}}})
	view := a.View()
	if !strings.Contains(view, "owner(owner)") || !strings.Contains(view, "alice") || strings.Contains(view, "SUMMARY(summary)") {
		t.Fatalf("configured columns not rendered: %s", view)
	}
}

func TestViewMarksChangedResultsAsStale(t *testing.T) {
	a := NewApp(nil)
	a.width, a.height = 180, 10
	a.AddSpec(spec.Spec{ID: "ops/check", Name: "check"})
	a.applyResult(runner.Result{ID: "ops/check", Status: status.Fail, Summary: "old", SpecHash: "old"})
	a.applyResult(runner.Result{ID: "ops/check", Status: status.Fail, Summary: "old", SpecHash: "new", Changed: true, ChangedAt: time.Now()})
	view := a.View()
	if !strings.Contains(view, "changed=1") || !strings.Contains(view, "FAIL*") {
		t.Fatalf("changed result not marked: %s", view)
	}
	a.applyResult(runner.Result{ID: "ops/check", Status: status.OK, Summary: "new", SpecHash: "new"})
	if strings.Contains(a.View(), "FAIL*") || strings.Contains(a.View(), "changed=1") {
		t.Fatalf("changed marker not cleared: %s", a.View())
	}
}

func TestDetailScrollIsBoundedByViewport(t *testing.T) {
	a := NewApp(nil)
	a.height = 3
	a.detailScroll = 99
	if got := a.scrollDetail("one\ntwo\nthree\nfour"); got != "three\nfour" {
		t.Fatalf("scrolled detail=%q", got)
	}
	a.detailScroll = 0
	if got := a.scrollDetail("one\ntwo\nthree\nfour"); got != "one\ntwo" {
		t.Fatalf("initial detail=%q", got)
	}
}

func TestDetailFooterStaysAtBottomWhileBodyScrolls(t *testing.T) {
	a := NewApp(nil)
	a.height = 5
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	a.model.Apply(coordinator.Entry{Spec: spec.Spec{ID: "check", Name: "check"}, Latest: &runner.Result{
		ID: "check", Status: status.OK, Stdout: []byte("one\ntwo\nthree\nfour\nfive\nsix"),
	}})
	a.detail = true
	a.detailScroll = 99
	lines := strings.Split(a.View(), "\n")
	if len(lines) != 5 || lines[len(lines)-1] != "Enter: back  p: view script  r: run now  q: quit" {
		t.Fatalf("footer is not anchored: %q", a.View())
	}
	a.showHelp = true
	a.err = fmt.Errorf(detailHelpText())
	lines = strings.Split(a.View(), "\n")
	if len(lines) != 5 || !strings.Contains(lines[len(lines)-2], "HELP:") || lines[len(lines)-1] != "Enter: back  p: view script  r: run now  q: quit" {
		t.Fatalf("help footer is not anchored: %q", a.View())
	}
}

func TestDetailAcceptsPagerKeys(t *testing.T) {
	a := NewApp(nil)
	a.height = 10
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	a.detail = true
	a.detailScroll = 3

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if a.detailScroll != 12 {
		t.Fatalf("space page down=%d, want 12", a.detailScroll)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if a.detail {
		t.Fatal("b did not return from detail")
	}
	a.detail = true
	a.detailScroll = 12
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if a.detailScroll != 3 {
		t.Fatalf("ctrl+b page up=%d, want 3", a.detailScroll)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if a.detailScroll != 0 {
		t.Fatalf("g top=%d, want 0", a.detailScroll)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if a.detailScroll != 1<<30 {
		t.Fatalf("G bottom=%d, want %d", a.detailScroll, 1<<30)
	}
}

func TestDetailRerunsSelectedCheckImmediately(t *testing.T) {
	a := NewApp(func(_ context.Context, ids []string, force bool) ([]runner.Result, error) {
		if !force || len(ids) != 1 || ids[0] != "check" {
			t.Fatalf("request ids=%v force=%v", ids, force)
		}
		return []runner.Result{{ID: "check", Status: status.OK, Summary: "run now complete"}}, nil
	})
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	a.detail = true
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	entry := a.model.rows["check"]
	if entry.Latest == nil || entry.Latest.Summary != "run now complete" {
		t.Fatalf("run now result=%+v", entry.Latest)
	}
}

func TestColumnSelectionAndSortKeys(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "a", Name: "Alpha"})
	a.AddSpec(spec.Spec{ID: "b", Name: "Beta"})

	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.column != 1 {
		t.Fatalf("column after right=%d, want 1", a.column)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.model.SortBy != "name" {
		t.Fatalf("sort after enter=%q, want name", a.model.SortBy)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if a.column != 7 {
		t.Fatalf("column after d=%d, want 7", a.column)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.column != -1 {
		t.Fatalf("column after esc=%d, want -1", a.column)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRight})
	if a.column != 0 {
		t.Fatalf("column after right from cleared state=%d, want 0", a.column)
	}
	view := a.View()
	for _, want := range []string{"ST(s)", "NAME(n)", "DUR(d)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing column shortcut %q: %s", want, view)
		}
	}
}

func TestViewPrioritizesSummaryColumns(t *testing.T) {
	a := NewApp(nil)
	a.width, a.height = 180, 6
	a.AddSpec(spec.Spec{ID: "check", Name: "check", Interval: time.Minute})
	a.model.Apply(coordinator.Entry{Spec: spec.Spec{ID: "check", Name: "check", Interval: time.Minute}, Latest: &runner.Result{
		ID: "check", Status: status.OK, Summary: "1234567890123456789012345",
	}})
	view := a.View()
	header := ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "ST(s)") {
			header = line
			break
		}
	}
	if header == "" {
		t.Fatalf("column header missing: %s", view)
	}
	ordered := []string{"ST(s)", "NAME(n)", "SUMMARY(m)", "INT(i)", "HIST(t)", "GROUP(g)", "LAST(l)", "DUR(d)"}
	last := -1
	for _, column := range ordered {
		at := strings.Index(header, column)
		if at <= last {
			t.Fatalf("header order invalid for %q: %s", column, header)
		}
		last = at
	}
	if !strings.Contains(view, "1234567890123456789012345 nex.") {
		t.Fatalf("summary was not clipped to 30 characters: %s", view)
	}
}

func TestDirectoryEnterAndEscape(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "examples/check", Name: "check"})
	a.model.Flat = false
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.directory != "examples" {
		t.Fatalf("directory after enter=%q", a.directory)
	}
	if rows := a.model.ViewRows(a.directory); len(rows) != 2 || rows[0].Kind != ParentRow || rows[1].Kind != CheckRow {
		t.Fatalf("directory rows=%+v", rows)
	}
	if view := a.View(); !strings.Contains(view, "..") {
		t.Fatalf("parent row missing from view: %s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.directory != "" {
		t.Fatalf("directory after parent enter=%q", a.directory)
	}
	return
}

func TestVerticalMovementClearsColumnSelection(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "a", Name: "a"})
	a.AddSpec(spec.Spec{ID: "b", Name: "b"})
	a.column = 1
	a.Update(tea.KeyMsg{Type: tea.KeyDown})
	if a.column != -1 || a.selected != 1 {
		t.Fatalf("after down: column=%d selected=%d", a.column, a.selected)
	}
	a.column = 2
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	if a.column != -1 || a.selected != 0 {
		t.Fatalf("after up: column=%d selected=%d", a.column, a.selected)
	}
}

func TestQuitRequiresConfirmation(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "check", Name: "check"})
	model, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if model != a || cmd != nil || !a.confirmExit {
		t.Fatalf("q: confirm=%v cmd=%v", a.confirmExit, cmd)
	}
	if !strings.Contains(a.View(), "Exit watch? [y/N]") {
		t.Fatalf("confirmation prompt missing: %s", a.View())
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if a.confirmExit {
		t.Fatal("n did not cancel exit")
	}
	_, cmd = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if !a.confirmExit || cmd != nil {
		t.Fatalf("second q: confirm=%v cmd=%v", a.confirmExit, cmd)
	}
	_, cmd = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("y did not request quit")
	}
}

func TestTreeFlatToggle(t *testing.T) {
	a := NewApp(nil)
	a.AddSpec(spec.Spec{ID: "examples/check", Name: "check"})
	if !a.model.Flat || len(a.model.ViewRows("")) != 1 || a.model.ViewRows("")[0].Kind != CheckRow {
		t.Fatalf("flat view not enabled by default: %+v", a.model.ViewRows(""))
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	if a.model.Flat || len(a.model.ViewRows("")) != 1 || a.model.ViewRows("")[0].Kind != DirectoryRow {
		t.Fatalf("tree view not enabled: %+v", a.model.ViewRows(""))
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	if !a.model.Flat {
		t.Fatal("flat view not restored")
	}
}
