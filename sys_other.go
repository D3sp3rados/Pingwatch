//go:build !windows && !linux && !darwin

package main

type cpuState struct{}

func osMachineID() string                   { return "" }
func collectFast(*sysCollector, *sysReport) {}
func serviceState(string) string            { return "unknown" }
func eventCounts() (int, int, bool)         { return -1, -1, false }
func rebootPending() int                    { return -1 }
func antivirus() *sysAV                     { return nil }
func pendingUpdates() (int, bool)           { return -1, false }
