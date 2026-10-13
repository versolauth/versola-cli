//go:build windows

package proxy

func setUmask(m int) int { return 0 }
