//go:build !fault

package fault

func Hit(string) bool { return false }
func Hang(string)     {}
