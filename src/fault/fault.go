//go:build fault

// Package fault contains opt-in fault-injection hooks for fixed-host tests.
package fault

import "os"

// Hit reports whether the requested fault point is enabled by EXECTOP_FAULT.
func Hit(point string) bool {
	v := os.Getenv("EXECTOP_FAULT")
	for i := 0; i < len(v); i++ {
		if v[i] == ':' || v[i] == ',' {
			if v[:i] == point {
				return true
			}
			v = v[i+1:]
			i = -1
		}
	}
	return v == point
}
