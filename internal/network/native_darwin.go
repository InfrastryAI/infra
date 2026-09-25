//go:build darwin

package network

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

// RunNativeHelper receives one bounded configuration from its launching CLI's
// inherited pipe. It exposes no IPC server, shell, arbitrary paths, or commands.
// Pipe EOF, a signal, or session expiry tears down its interface/routes/DNS.
func RunNativeHelper(input io.Reader, output io.Writer) error {
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if os.Geteuid() != 0 || err != nil || uid <= 0 {
		return errors.New("start the network helper through infra network connect")
	}
	reader := bufio.NewReader(io.LimitReader(input, 65536))
	data, err := reader.ReadBytes('\n')
	if err != nil || len(data) > 60000 {
		return errors.New("invalid helper request")
	}
	var config Configuration
	if err := json.Unmarshal(data, &config); err != nil {
		return errors.New("invalid helper request")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.Session.Mode != "native" {
		return errors.New("native connection required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, config.Session.ExpiresAt)
	defer cancel()
	// The inherited pipe is owned by the authenticated, unprivileged parent.
	go func() { io.Copy(io.Discard, input); cancel() }()
	resolver := filepath.Join("/etc/resolver", config.Session.DNSSuffix)
	marker := fmt.Sprintf("# Infrastry owner %d interface ", uid)
	if prior, err := os.ReadFile(resolver); err == nil {
		first := strings.SplitN(string(prior), "\n", 2)[0]
		if !strings.HasPrefix(first, marker) {
			return errors.New("private DNS is already configured by another connection")
		}
		old := strings.TrimPrefix(first, marker)
		if !strings.HasPrefix(old, "utun") {
			return errors.New("invalid previous private DNS configuration")
		}
		if _, err := net.InterfaceByName(old); err == nil {
			return errors.New("a private network connection is already active")
		}
		if err := os.Remove(resolver); err != nil {
			return errors.New("could not clean up the previous private DNS configuration")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect private DNS configuration")
	}
	addresses := []string{config.Session.DNS}
	for _, service := range config.Session.Services {
		addresses = append(addresses, service.Address)
	}
	inspectCtx, finishInspect := context.WithTimeout(ctx, 5*time.Second)
	routes, inspectErr := exec.CommandContext(inspectCtx, "/usr/sbin/netstat", "-rn", "-f", "inet6").Output()
	finishInspect()
	if inspectErr != nil {
		return errors.New("cannot inspect existing private network routes")
	}
	if routeConflicts(string(routes), addresses) {
		return errors.New("private service addresses overlap an existing network; disconnect the conflicting VPN and retry")
	}
	t, err := tun.CreateTUN("utun", 1280)
	if err != nil {
		return errors.New("cannot create the private network interface")
	}
	name, err := t.Name()
	if err != nil {
		t.Close()
		return err
	}
	dev, err := wireguard(config, t)
	if err != nil {
		return err
	}
	defer dev.Close()
	command := func(binary string, args ...string) error {
		commandCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(commandCtx, binary, args...).Run()
	}
	if err := command("/sbin/ifconfig", name, "inet6", config.Session.Source, "prefixlen", "128", "up"); err != nil {
		return errors.New("cannot configure the private network interface")
	}
	installed := []string{}
	defer func() {
		for _, address := range installed {
			command("/sbin/route", "-n", "delete", "-inet6", "-host", address, "-interface", name)
		}
	}()
	for _, address := range addresses {
		if err := command("/sbin/route", "-n", "add", "-inet6", "-host", address, "-interface", name); err != nil {
			return errors.New("a private network route conflicts with an existing connection")
		}
		installed = append(installed, address)
	}
	if err := os.MkdirAll("/etc/resolver", 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(resolver, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("private DNS is already configured")
	}
	ownedFile, statErr := file.Stat()
	if statErr != nil {
		file.Close()
		return statErr
	}
	configured := false
	content := marker + name + "\nnameserver " + dnsAddress + "\nsearch_order 1\ntimeout 2\n"
	defer func() {
		if current, err := os.Lstat(resolver); err == nil && os.SameFile(current, ownedFile) {
			data, readErr := os.ReadFile(resolver)
			if !configured || (readErr == nil && string(data) == content) {
				os.Remove(resolver)
			}
		}
	}()
	_, writeErr := io.WriteString(file, content)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("could not configure private DNS")
	}
	configured = true
	fmt.Fprintln(output, "ready")
	<-ctx.Done()
	return nil
}
