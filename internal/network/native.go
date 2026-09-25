package network

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func RunNative(ctx context.Context, config Configuration, errorsOutput io.Writer, ready func()) error {
	if runtime.GOOS != "darwin" {
		return errors.New("native private networking currently requires macOS; use infra proxy on this platform")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// Authentication and keychain access have already completed without elevation.
	helperCtx, stopHelper := context.WithCancel(ctx)
	defer stopHelper()
	cmd := exec.CommandContext(helperCtx, "/usr/bin/sudo", executable, "network-helper")
	cmd.Stderr = errorsOutput
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Interrupt sudo as well as closing the pipe, including while it is still
	// waiting for a password. sudo forwards the signal to the helper for cleanup.
	cmd.Cancel = func() error { input.Close(); return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := json.NewEncoder(input).Encode(config); err != nil {
		input.Close()
		cmd.Wait()
		return err
	}
	defer input.Close()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "ready\n" {
		input.Close()
		cmd.Wait()
		return errors.New("private network setup failed")
	}
	if err := waitForNativeDNS(ctx, config.Session, nativeDNSOutput); err != nil {
		// Reap the helper and let it remove routes/DNS before reporting failure.
		stopHelper()
		cmd.Wait()
		return err
	}
	ready()
	return cmd.Wait()
}
