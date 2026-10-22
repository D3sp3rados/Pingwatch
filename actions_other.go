//go:build !windows && !linux

package main

import "errors"

func installUpdates(string, string) (any, error) {
	return nil, errors.New("Updates über PingWacht gibt es nur für Windows und Linux")
}

func rebootMachine(int) error {
	return errors.New("Neustart über PingWacht gibt es nur für Windows und Linux")
}
