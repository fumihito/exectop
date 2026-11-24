package tui

import (
	"exectop/src/coordinator"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"testing"
	"time"
)

func TestModelQueryIncludesSummaryAndSortsLast(t *testing.T) {
	m := New()
	now := time.Now()
	m.Apply(coordinator.Entry{Spec: spec.Spec{ID: "one", Name: "First"}, Latest: &runner.Result{ID: "one", Status: status.OK, Summary: "database healthy", Start: now.Add(-time.Minute)}})
	m.Apply(coordinator.Entry{Spec: spec.Spec{ID: "two", Name: "Second"}, Latest: &runner.Result{ID: "two", Status: status.OK, Summary: "cache healthy", Start: now}})
	m.Query = "database"
	rows := m.Snapshot()
	if len(rows) != 1 || rows[0].Spec.ID != "one" {
		t.Fatalf("summary query=%+v", rows)
	}
	m.Query = ""
	m.SortBy = "last"
	rows = m.Snapshot()
	if len(rows) != 2 || rows[0].Spec.ID != "two" {
		t.Fatalf("last sort=%+v", rows)
	}
}

func TestModelSortsByTableColumns(t *testing.T) {
	m := New()
	now := time.Now()
	m.Apply(coordinator.Entry{Spec: spec.Spec{ID: "z/check", Name: "Zulu", Interval: 2 * time.Minute}, Latest: &runner.Result{ID: "z/check", Status: status.OK, Summary: "z summary", Start: now.Add(-time.Minute), Duration: 2 * time.Second}})
	m.Apply(coordinator.Entry{Spec: spec.Spec{ID: "a/check", Name: "Alpha", Interval: time.Minute}, Latest: &runner.Result{ID: "a/check", Status: status.Fail, Summary: "a summary", Start: now, Duration: time.Second}})
	for _, test := range []struct {
		by, want string
	}{
		{"status", "a/check"}, {"name", "a/check"}, {"group", "a/check"}, {"last", "a/check"},
		{"duration", "z/check"}, {"interval", "a/check"}, {"summary", "a/check"},
	} {
		m.SortBy = test.by
		rows := m.Snapshot()
		if len(rows) != 2 || rows[0].Spec.ID != test.want {
			t.Fatalf("sort %s = %+v, want %s first", test.by, rows, test.want)
		}
	}
}

func TestViewRowsAggregatesDirectoriesRecursively(t *testing.T) {
	m := New()
	m.Flat = false
	for _, e := range []coordinator.Entry{
		{Spec: spec.Spec{ID: "examples/ok", Name: "ok"}, Latest: &runner.Result{Status: status.OK}},
		{Spec: spec.Spec{ID: "examples/nested/fail", Name: "fail"}, Latest: &runner.Result{Status: status.Fail}},
		{Spec: spec.Spec{ID: "root", Name: "root"}, Latest: &runner.Result{Status: status.OK}},
	} {
		m.Apply(e)
	}
	rows := m.ViewRows("")
	if len(rows) != 2 || rows[0].Kind != DirectoryRow || rows[0].Directory != "examples" {
		t.Fatalf("root rows=%+v", rows)
	}
	if rows[0].Aggregate.Status != status.Fail || rows[0].Aggregate.Total != 2 || rows[0].Aggregate.Fail != 1 {
		t.Fatalf("aggregate=%+v", rows[0].Aggregate)
	}
	inside := m.ViewRows("examples")
	if len(inside) != 3 || inside[0].Kind != ParentRow || inside[1].Kind != DirectoryRow || inside[1].Directory != "examples/nested" {
		t.Fatalf("inside rows=%+v", inside)
	}
}

func TestViewRowsFlatOmitsDirectoryRows(t *testing.T) {
	m := New()
	m.Flat = true
	m.Apply(coordinator.Entry{Spec: spec.Spec{ID: "examples/check", Name: "check"}})
	rows := m.ViewRows("")
	if len(rows) != 1 || rows[0].Kind != CheckRow || rows[0].Entry.Spec.ID != "examples/check" {
		t.Fatalf("flat rows=%+v", rows)
	}
}
