//go:build !linux

package cli

import "syscall"

func dup2(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }
