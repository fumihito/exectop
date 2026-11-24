//go:build fault

package fault

import "os"

func Hit(point string) bool {
	v := os.Getenv("EXECTOP_FAULT")
	for _, x := range stringsSplit(v) {
		if x == point {
			return true
		}
	}
	return false
}
func Hang(point string) {
	if Hit(point) {
		select {}
	}
}
func stringsSplit(v string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ':' || v[i] == ',' {
			out = append(out, v[start:i])
			start = i + 1
		}
	}
	return out
}
