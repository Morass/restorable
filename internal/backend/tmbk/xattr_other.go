//go:build !darwin

package tmbk

import "errors"

// Time Machine exists only on macOS; elsewhere there is no attribute to read.
func getxattr(string, string, []byte) (int, error) { return 0, errors.New("unsupported") }
