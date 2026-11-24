package aggregate

import (
	"exectop/src/runner"
	"exectop/src/status"
)

func Exit(rs []runner.Result, failOn string) int {
	best := 0
	for _, r := range rs {
		if r.Status == status.Skip {
			continue
		}
		if r.Status.Severity() > best {
			best = r.Status.Severity()
		}
	}
	if failOn == "ok" {
		return 0
	}
	if failOn == "warn" {
		for _, r := range rs {
			if r.Status != status.OK && r.Status != status.Skip {
				return 1
			}
		}
		return 0
	}
	switch {
	case best >= status.Fail.Severity():
		return 1
	case best == status.Warn.Severity():
		return 2
	case best == status.Unknown.Severity():
		return 3
	}
	return 0
}
