//go:build !windows && !linux && !darwin

package main

type cpuState struct{}

func osMachineID() string                        { return "" }
func collectFast(*sysCollector, *sysReport)      {}
func serviceState(string) string                 { return "unknown" }
func eventCounts() (int, int, bool)              { return -1, -1, false }
func rebootPending() int                         { return -1 }
func antivirus() *sysAV                          { return nil }
func pendingUpdates() (int, int, []string, bool) { return -1, 0, nil, false }

func restartService(string) error { return errNotSupported }
func collectInventory() *sysInventory {
	return &sysInventory{HW: invHW{NICs: netInventory()}}
}

var errNotSupported = errorString("auf diesem System nicht unterstützt")

type errorString string

func (e errorString) Error() string { return string(e) }
