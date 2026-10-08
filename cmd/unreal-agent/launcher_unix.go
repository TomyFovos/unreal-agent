//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type backgroundHost struct {
	process *os.Process
	done    <-chan struct{}
	waitErr error // published by closing done
}

func checkPrivateDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("cannot open private directory %s: %w", path, err)
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0777 != 0700 {
		return fmt.Errorf("directory %s must belong to the current user with permissions 0700", path)
	}
	return nil
}

func makePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return checkPrivateDirectory(path)
}

func checkPrivateSocket(path string) error {
	var info unix.Stat_t
	if err := unix.Lstat(path, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFSOCK || info.Uid != uint32(os.Geteuid()) || info.Mode&0777 != 0600 {
		return errors.New("Host socket must belong to the current user with permissions 0600")
	}
	return nil
}

func privateLaunchFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		f.Close()
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 || info.Mode&0777 != 0600 {
		f.Close()
		return nil, fmt.Errorf("unsafe launcher file %s; a current-user regular file with permissions 0600 is required", path)
	}
	return f, nil
}

// Never unlink a lock. The startup lock coordinates launchers; the existing
// gateway lock remains authoritative for manual serve and socket replacement.
func tryStartupLock(path string) (*os.File, error) {
	f, err := privateLaunchFile(path, unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

func gatewaySocketOwned(path string) (bool, error) {
	f, err := privateLaunchFile(path, unix.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}

func backgroundArguments(o launchOptions) []string {
	args := []string{"serve", "--config", o.config, "--session-directory", o.sessions, "--socket", o.socket}
	if o.normal {
		args = append(args, "--normal-runtime")
	}
	if o.credentials != "" {
		args = append(args, "--credential-directory", o.credentials)
	}
	if o.codexFile != "" {
		args = append(args, "--codex-auth-file", o.codexFile)
	}
	return args
}

func backgroundEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "OPENAI_CODEX_ACCESS_TOKEN" && key != "OPENAI_CODEX_ACCOUNT_ID" {
			env = append(env, entry)
		}
	}
	return env
}

func startBackgroundHost(o launchOptions) (*backgroundHost, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return spawnBackgroundHost(binary, nil, o)
}

func spawnBackgroundHost(binary string, prefix []string, o launchOptions) (*backgroundHost, error) {
	log, err := privateLaunchFile(filepath.Join(o.state, "host.log"), unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	input, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	args := append(append([]string(nil), prefix...), backgroundArguments(o)...)
	cmd := exec.Command(binary, args...)
	// The same executable serves both names. A background invocation always
	// enters the low-level serve implementation, including an installed unreal.
	cmd.Args[0] = "unreal-agent"
	cmd.Env = backgroundEnvironment()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Deliberately independent of the launcher's/TUI's cancellation context.
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	child := &backgroundHost{process: cmd.Process, done: done}
	go func() { child.waitErr = cmd.Wait(); close(done) }()
	return child, nil
}
