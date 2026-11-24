package tui

import (
	"exectop/src/coordinator"
	"exectop/src/status"
	"sort"
	"strings"
	"sync"
	"time"
)

type RowKind int

const (
	CheckRow RowKind = iota
	DirectoryRow
	ParentRow
)

type Aggregate struct {
	Status   status.Status
	Total    int
	OK       int
	Warn     int
	Fail     int
	Timeout  int
	Error    int
	Vanished int
	Unknown  int
	Running  int
	Pending  int
	Skip     int
	Changed  int
}

type ViewRow struct {
	Kind      RowKind
	Entry     *coordinator.Entry
	Directory string
	Aggregate Aggregate
}

type Model struct {
	mu         sync.RWMutex
	rows       map[string]coordinator.Entry
	Query      string
	ErrorsOnly bool
	SortBy     string
	Flat       bool
}

func New() *Model                          { return &Model{rows: map[string]coordinator.Entry{}, Flat: true} }
func (m *Model) Apply(e coordinator.Entry) { m.mu.Lock(); m.rows[e.Spec.ID] = e; m.mu.Unlock() }
func (m *Model) Snapshot() []coordinator.Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]coordinator.Entry, 0, len(m.rows))
	for _, e := range m.rows {
		summary := ""
		if e.Latest != nil {
			summary = e.Latest.Summary
		}
		if m.Query != "" && !strings.Contains(strings.ToLower(e.Spec.ID), strings.ToLower(m.Query)) && !strings.Contains(strings.ToLower(e.Spec.Name), strings.ToLower(m.Query)) && !strings.Contains(strings.ToLower(summary), strings.ToLower(m.Query)) {
			continue
		}
		if m.ErrorsOnly && (e.Latest == nil || e.Latest.Status == status.OK || e.Latest.Status == status.Skip) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if m.SortBy == "status" {
			return compareStatus(out[i], out[j])
		}
		if m.SortBy == "name" {
			return out[i].Spec.Name < out[j].Spec.Name
		}
		if m.SortBy == "group" {
			return groupOf(out[i].Spec.ID) < groupOf(out[j].Spec.ID)
		}
		if m.SortBy == "duration" {
			di, dj := time.Duration(0), time.Duration(0)
			if out[i].Latest != nil {
				di = out[i].Latest.Duration
			}
			if out[j].Latest != nil {
				dj = out[j].Latest.Duration
			}
			return di > dj
		}
		if m.SortBy == "last" {
			var ti, tj time.Time
			if out[i].Latest != nil {
				ti = out[i].Latest.Start
			}
			if out[j].Latest != nil {
				tj = out[j].Latest.Start
			}
			return ti.After(tj)
		}
		if m.SortBy == "interval" {
			return out[i].Spec.Interval < out[j].Spec.Interval
		}
		if m.SortBy == "history" {
			return len(out[i].History) > len(out[j].History)
		}
		if m.SortBy == "summary" {
			return summaryOf(out[i]) < summaryOf(out[j])
		}
		if out[i].Latest == nil {
			return false
		}
		if out[j].Latest == nil {
			return true
		}
		if out[i].Latest.Status.Severity() != out[j].Latest.Status.Severity() {
			return out[i].Latest.Status.Severity() > out[j].Latest.Status.Severity()
		}
		return out[i].Spec.Name < out[j].Spec.Name
	})
	return out
}

// ViewRows returns the rows for a directory-oriented TUI view. Directory is
// relative to the configured root; an empty value represents the root view.
func (m *Model) ViewRows(directory string) []ViewRow {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]coordinator.Entry, 0, len(m.rows))
	for _, e := range m.rows {
		entries = append(entries, e)
	}
	if m.Flat {
		rows := make([]ViewRow, 0, len(entries))
		for i := range entries {
			if !m.matches(entries[i]) {
				continue
			}
			copy := entries[i]
			rows = append(rows, ViewRow{Kind: CheckRow, Entry: &copy})
		}
		sort.Slice(rows, func(i, j int) bool { return compareEntries(*rows[i].Entry, *rows[j].Entry, m.SortBy) })
		return rows
	}
	rows := make([]ViewRow, 0)
	if directory != "" {
		rows = append(rows, ViewRow{Kind: ParentRow, Directory: parentPath(directory)})
	}
	dirs := map[string][]coordinator.Entry{}
	for i := range entries {
		e := entries[i]
		if !underDirectory(e.Spec.ID, directory) {
			continue
		}
		rel := relativeToDirectory(e.Spec.ID, directory)
		if rel == "" {
			if m.matches(e) {
				copy := e
				rows = append(rows, ViewRow{Kind: CheckRow, Entry: &copy})
			}
			continue
		}
		slash := strings.IndexByte(rel, '/')
		if slash >= 0 {
			child := joinPath(directory, rel[:slash])
			dirs[child] = append(dirs[child], e)
		}
		if slash < 0 && m.matches(e) {
			copy := e
			rows = append(rows, ViewRow{Kind: CheckRow, Entry: &copy})
		}
	}
	for dir, children := range dirs {
		agg := aggregate(children)
		if m.ErrorsOnly && !aggregateVisible(agg) {
			continue
		}
		if m.Query != "" && !strings.Contains(strings.ToLower(dir), strings.ToLower(m.Query)) && !anyMatch(children, m.matches) {
			continue
		}
		rows = append(rows, ViewRow{Kind: DirectoryRow, Directory: dir, Aggregate: agg})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Kind == ParentRow && b.Kind == ParentRow {
			return false
		}
		if a.Kind == ParentRow {
			return true
		}
		if b.Kind == ParentRow {
			return false
		}
		if a.Kind != b.Kind {
			return a.Kind == DirectoryRow
		}
		if a.Kind == DirectoryRow {
			if m.SortBy == "status" || m.SortBy == "" {
				if a.Aggregate.Status.Severity() != b.Aggregate.Status.Severity() {
					return a.Aggregate.Status.Severity() > b.Aggregate.Status.Severity()
				}
			}
			return a.Directory < b.Directory
		}
		return compareStatus(*a.Entry, *b.Entry)
	})
	return rows
}

func (m *Model) FirstEntry(directory string) (coordinator.Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.rows {
		if underDirectory(e.Spec.ID, directory) {
			return e, true
		}
	}
	return coordinator.Entry{}, false
}

func (m *Model) matches(e coordinator.Entry) bool {
	if m.ErrorsOnly && (e.Latest == nil || e.Latest.Status == status.OK || e.Latest.Status == status.Skip) {
		return false
	}
	if m.Query == "" {
		return true
	}
	q := strings.ToLower(m.Query)
	return strings.Contains(strings.ToLower(e.Spec.ID), q) || strings.Contains(strings.ToLower(e.Spec.Name), q) || strings.Contains(strings.ToLower(summaryOf(e)), q)
}

func underDirectory(id, directory string) bool {
	if directory == "" {
		return true
	}
	return id == directory || strings.HasPrefix(id, directory+"/")
}

func relativeToDirectory(id, directory string) string {
	if directory == "" {
		return id
	}
	if id == directory {
		return ""
	}
	return strings.TrimPrefix(id, directory+"/")
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentPath(directory string) string {
	if i := strings.LastIndexByte(directory, '/'); i >= 0 {
		return directory[:i]
	}
	return ""
}

func anyMatch(entries []coordinator.Entry, fn func(coordinator.Entry) bool) bool {
	for _, e := range entries {
		if fn(e) {
			return true
		}
	}
	return false
}

func aggregateVisible(a Aggregate) bool {
	return a.Status != status.OK && a.Status != status.Skip
}

func aggregate(entries []coordinator.Entry) Aggregate {
	a := Aggregate{Total: len(entries), Status: status.OK}
	allSkip := len(entries) > 0
	for _, e := range entries {
		st := latest(e)
		switch st {
		case status.OK:
			a.OK++
		case status.Warn:
			a.Warn++
		case status.Fail:
			a.Fail++
		case status.Timeout:
			a.Timeout++
		case status.Error:
			a.Error++
		case status.Vanished:
			a.Vanished++
		case status.Unknown:
			a.Unknown++
		case status.Running:
			a.Running++
		case status.Pending:
			a.Pending++
		case status.Skip:
			a.Skip++
		}
		if e.Changed {
			a.Changed++
		}
		if st != status.Skip {
			allSkip = false
		}
	}
	if a.Fail+a.Timeout+a.Error+a.Vanished+a.Unknown > 0 {
		a.Status = status.Fail
	} else if a.Warn > 0 {
		a.Status = status.Warn
	} else if a.Running > 0 {
		a.Status = status.Running
	} else if a.Pending > 0 {
		a.Status = status.Pending
	} else if allSkip {
		a.Status = status.Skip
	}
	return a
}

func groupOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return ""
}

func summaryOf(e coordinator.Entry) string {
	if e.Latest == nil {
		return ""
	}
	return e.Latest.Summary
}

func compareStatus(a, b coordinator.Entry) bool {
	return compareEntries(a, b, "status")
}

func compareEntries(a, b coordinator.Entry, sortBy string) bool {
	if sortBy == "name" {
		return a.Spec.Name < b.Spec.Name
	}
	if sortBy == "group" {
		return groupOf(a.Spec.ID) < groupOf(b.Spec.ID)
	}
	if sortBy == "duration" {
		return durationOf(a) > durationOf(b)
	}
	if sortBy == "last" {
		return startOf(a).After(startOf(b))
	}
	if sortBy == "interval" {
		return a.Spec.Interval < b.Spec.Interval
	}
	if sortBy == "history" {
		return len(a.History) > len(b.History)
	}
	if sortBy == "summary" {
		return summaryOf(a) < summaryOf(b)
	}
	sa, sb := status.Pending, status.Pending
	if a.Latest != nil {
		sa = a.Latest.Status
	}
	if b.Latest != nil {
		sb = b.Latest.Status
	}
	if sa.Severity() != sb.Severity() {
		return sa.Severity() > sb.Severity()
	}
	return a.Spec.Name < b.Spec.Name
}

func durationOf(e coordinator.Entry) time.Duration {
	if e.Latest != nil {
		return e.Latest.Duration
	}
	return 0
}

func startOf(e coordinator.Entry) time.Time {
	if e.Latest != nil {
		return e.Latest.Start
	}
	return time.Time{}
}
