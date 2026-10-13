//go:build !windows

package proxy

import "syscall"

func setUmask(m int) int { return syscall.Umask(m) }
