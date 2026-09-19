//go:build darwin

package tmbk

import "golang.org/x/sys/unix"

func getxattr(path, attr string, dest []byte) (int, error) {
	return unix.Getxattr(path, attr, dest)
}
