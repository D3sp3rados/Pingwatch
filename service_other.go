//go:build !windows

package main

import "fmt"

var serviceMode bool

func runService([]string) {
	fmt.Println("Der Dienst-Modus gibt es nur unter Windows (Linux: systemd über install.sh).")
}

func serviceWait(*Agent) {}

func serviceInstalled() bool { return false }
