//go:build windows

package main

import "os/exec"

// radiusFirewall: eingehend UDP 1812 in der Windows-Firewall erlauben (einmalig, Regel mit festem Namen).
func radiusFirewall() {
	chk := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name=PingWacht RADIUS")
	hideWindow(chk)
	if chk.Run() == nil {
		return
	}
	cmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name=PingWacht RADIUS", "dir=in", "action=allow", "protocol=UDP", "localport=1812")
	hideWindow(cmd)
	cmd.Run()
}
