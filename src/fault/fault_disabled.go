//go:build !fault

// Package fault is inert in release builds.
package fault

func Hit(string) bool { return false }
