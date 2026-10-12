// coopr-run-sequence runs direct argv commands within one container execution.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "coopr-run-sequence: expected one command-file path")
		return 125
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return report(err, 125)
	}
	var commands [][]string
	if err := json.Unmarshal(data, &commands); err != nil {
		return report(err, 125)
	}
	if len(commands) < 2 {
		return report(errors.New("expected at least two commands"), 125)
	}
	for _, argv := range commands {
		if len(argv) == 0 || argv[0] == "" {
			return report(errors.New("command executable cannot be empty"), 125)
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, 0) {
				return report(errors.New("command arguments cannot contain NUL"), 125)
			}
		}
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	// OCI PID 1 owns descendant groups; chroot children must remain in the
	// existing worker group so its supervisor can kill the whole tree.
	isolated := os.Getpid() == 1
	var targets []int
	forward := func(sig os.Signal) {
		for _, target := range targets {
			if isolated {
				target = -target
			}
			_ = syscall.Kill(target, sig.(syscall.Signal))
		}
	}
	for _, argv := range commands {
		select {
		case sig := <-signals:
			forward(sig)
			return signalStatus(sig)
		default:
		}
		command := exec.Command(argv[0], argv[1:]...)
		// A caller's explicit PATH may include its working directory, as in a shell.
		if errors.Is(command.Err, exec.ErrDot) {
			command.Err = nil
		}
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: isolated, Pdeathsig: syscall.SIGKILL}
		if err := command.Start(); err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				return report(err, 127)
			}
			return report(err, 126)
		}
		if isolated {
			targets = append(targets, command.Process.Pid)
		} else {
			targets = []int{command.Process.Pid}
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		var interrupted os.Signal
		var waitErr error
	wait:
		for {
			select {
			case sig := <-signals:
				if interrupted == nil {
					interrupted = sig
				}
				// Never signal our own group recursively. The chroot supervisor
				// owns group cleanup; OCI PID 1 forwards to child groups.
				forward(sig)
			case waitErr = <-done:
				break wait
			}
		}
		// Prefer cancellation over a simultaneous successful child exit.
		select {
		case sig := <-signals:
			if interrupted == nil {
				interrupted = sig
			}
			forward(sig)
		default:
		}
		reapOrphans()
		if interrupted != nil {
			return signalStatus(interrupted)
		}
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal())
				}
				return exitErr.ExitCode()
			}
			return report(waitErr, 125)
		}
		if !isolated {
			targets = nil
		}
	}
	return 0
}

func signalStatus(sig os.Signal) int { return 128 + int(sig.(syscall.Signal)) }

func reapOrphans() {
	if os.Getpid() != 1 {
		return
	}
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return
		}
	}
}

func report(err error, status int) int {
	fmt.Fprintln(os.Stderr, "coopr-run-sequence:", err)
	return status
}
