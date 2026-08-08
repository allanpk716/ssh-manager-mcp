package sshbroker

import "strings"

func hostOf(addr string) string { return addr[:strings.LastIndex(addr, ":")] }
func portOf(addr string) int {
	var p int
	for _, c := range addr[strings.LastIndex(addr, ":")+1:] {
		p = p*10 + int(c-'0')
	}
	return p
}
