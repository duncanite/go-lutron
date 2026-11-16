//go:build !windows

package credentials

// private makes the file at path readable by its owner alone. The mode it
// was made with already says so here.
func private(string) error { return nil }
