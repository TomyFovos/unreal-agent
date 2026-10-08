//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

type backgroundHost struct {
	process *os.Process
	done    <-chan struct{}
	waitErr error
}

func unsupportedLauncher() error {
	return errors.New("unreal requires Linux or macOS (Windows users can use WSL)")
}
func checkPrivateDirectory(string) error                         { return unsupportedLauncher() }
func makePrivateDirectory(string) error                          { return unsupportedLauncher() }
func checkPrivateSocket(string) error                            { return unsupportedLauncher() }
func tryStartupLock(string) (*os.File, error)                    { return nil, unsupportedLauncher() }
func gatewaySocketOwned(string) (bool, error)                    { return false, unsupportedLauncher() }
func startBackgroundHost(launchOptions) (*backgroundHost, error) { return nil, unsupportedLauncher() }
