package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/InfrastryAI/infra/internal/network"
	"github.com/spf13/cobra"
)

var appReference = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}/[a-z0-9][a-z0-9-]{0,62}$`)

type proxyOptions struct {
	service                    string
	port, remotePort           int
	database                   bool
	connect                    bool
	passwordPrompt             bool
	engine, user, databaseName string
	dumpOutput                 io.Writer
}

func (command *CLI) networkCommands(root *cobra.Command) {
	group := &cobra.Command{Use: "network", Short: "Connect to private application services", GroupID: "operations", RunE: helpCommand}
	group.AddCommand(&cobra.Command{Use: "connect team/app", Short: "Connect this Mac to an application's private services", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return command.runNetwork(cmd.Context(), args[0], nil) }})
	group.AddCommand(&cobra.Command{Use: "status", Short: "Show your active private network sessions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := command.apiClient()
		if err != nil {
			return err
		}
		sessions, err := client.NetworkSessions(cmd.Context())
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Fprintln(command.dependencies.Stdout, "No private network sessions are active.")
		}
		for _, session := range sessions {
			fmt.Fprintf(command.dependencies.Stdout, "%s  %s  expires %s  %s\n", session.AppRef, session.Mode, session.ExpiresAt.Local().Format(time.RFC3339), session.ID)
		}
		return nil
	}})
	group.AddCommand(&cobra.Command{Use: "disconnect [session-id]", Short: "Disconnect this computer or a selected session", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			client, err := command.apiClient()
			if err != nil {
				return err
			}
			return client.DisconnectNetwork(cmd.Context(), args[0])
		}
		socket, err := net.DialTimeout("unix", command.networkSocket(), time.Second)
		if err != nil {
			return errors.New("no connection is active on this computer; use infra network status to find other sessions")
		}
		defer socket.Close()
		socket.SetDeadline(time.Now().Add(10 * time.Second))
		_, err = socket.Write([]byte("disconnect\n"))
		if err != nil {
			return err
		}
		// Wait for the running CLI to finish its cleanup before reporting completion.
		var result [16]byte
		n, err := socket.Read(result[:])
		if err != nil {
			return err
		}
		if string(result[:n]) != "disconnected\n" {
			return errors.New("connection cleanup did not finish")
		}
		fmt.Fprintln(command.dependencies.Stdout, "Disconnected.")
		return nil
	}})
	root.AddCommand(group, command.proxyCommand(false))
	db := &cobra.Command{Use: "db", Short: "Connect local database tools to a private database", GroupID: "operations", RunE: helpCommand}
	db.AddCommand(command.proxyCommand(true), command.databaseConnectCommand(), command.databaseDumpCommand())
	var resetMode string
	reset := &cobra.Command{Use: "reset-device team", Short: "Replace this computer's local key after device removal", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if !appReference.MatchString(args[0]+"/app") || (resetMode != "native" && resetMode != "proxy") {
			return usageError{"specify a team and --mode native or proxy"}
		}
		control, err := command.networkControl()
		if err != nil {
			return err
		}
		defer func() { control.Close(); os.Remove(command.networkSocket()) }()
		if err := network.ForgetDeviceKey(command.apiURL, args[0], resetMode); err != nil {
			return err
		}
		fmt.Fprintln(command.dependencies.Stdout, "Device key removed. Your next connection will register a new device.")
		return nil
	}}
	reset.Flags().StringVar(&resetMode, "mode", "native", "connection mode: native or proxy")
	group.AddCommand(reset)
	root.AddCommand(db)
	root.AddCommand(&cobra.Command{Use: "network-helper", Hidden: true, Annotations: map[string]string{skipInitialization: "true"}, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		return network.RunNativeHelper(command.dependencies.Stdin, command.dependencies.Stdout)
	}})
}

func (command *CLI) proxyCommand(database bool) *cobra.Command {
	options := proxyOptions{database: database}
	cmd := &cobra.Command{Use: "proxy team/app", Short: "Forward a local TCP port to a private service", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return command.runNetwork(cmd.Context(), args[0], &options)
	}}
	if !database {
		cmd.GroupID = "operations"
		cmd.Flags().StringVar(&options.service, "service", "", "private service name")
	} else {
		cmd.Flags().StringVar(&options.service, "database", "", "database service name (optional when the application has only one database)")
	}
	cmd.Flags().IntVar(&options.port, "port", 0, "local loopback port (automatically selected when omitted)")
	cmd.Flags().IntVar(&options.remotePort, "remote-port", 0, "declared service port (required if the service has several)")
	return cmd
}

func (command *CLI) runNetwork(parent context.Context, ref string, options *proxyOptions) error {
	if !appReference.MatchString(ref) {
		return usageError{"specify an unambiguous team/app reference"}
	}
	if options != nil {
		if options.service == "" && !options.database {
			return usageError{"specify a service name with --service"}
		}
		if options.port < 0 || options.port > 65535 || options.remotePort < 0 || options.remotePort > 65535 {
			return usageError{"specify valid TCP ports"}
		}
	}
	profile, _ := command.configuration.Profile(command.apiURL)
	if options != nil && options.connect && !options.passwordPrompt && !contains(profile.Scopes, "databases:credentials:read") {
		return errors.New("approve database access with infra auth login --super")
	}
	for _, scope := range []string{"networks:connect", "networks:manage"} {
		if !contains(profile.Scopes, scope) {
			return errors.New("approve private network access with infra auth login --super")
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	localControl, err := command.networkControl()
	if err != nil {
		return err
	}
	var controllers sync.WaitGroup
	finished := make(chan struct{})
	defer func() {
		localControl.Close()
		close(finished)
		controllers.Wait()
		os.Remove(command.networkSocket())
	}()
	// Only one network operation per installation runs on this computer. The
	// owner-only Unix socket never accepts credentials or network configuration.
	controllers.Add(1)
	go func() {
		defer controllers.Done()
		for {
			c, err := localControl.Accept()
			if err != nil {
				return
			}
			controllers.Add(1)
			go func() {
				defer controllers.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(15 * time.Second))
				var b [16]byte
				n, err := c.Read(b[:])
				if err == nil && string(b[:n]) == "disconnect\n" {
					cancel()
					<-finished
					c.Write([]byte("disconnected\n"))
				}
			}()
		}
	}()
	var listener net.Listener
	if options != nil {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", options.port))
		if err != nil {
			return errors.New("the requested local port is already in use; choose another port")
		}
		defer listener.Close()
	}
	parts := strings.Split(ref, "/")
	mode := "native"
	if options != nil {
		mode = "proxy"
	}
	keyFunc := command.dependencies.NetworkKey
	if keyFunc == nil {
		keyFunc = network.DeviceKey
	}
	private, public, err := keyFunc(command.apiURL, parts[0], mode)
	if err != nil {
		return err
	}
	client, err := command.apiClient()
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "My computer"
	}
	device, err := client.EnrollNetworkDevice(ctx, parts[0], hostname, public, mode)
	if err != nil {
		return err
	}
	session, err := client.CreateNetworkSession(ctx, parts[0], parts[1], device.ID)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 8*time.Second)
		defer stop()
		if err := client.DisconnectNetwork(cleanup, session.ID); err != nil {
			fmt.Fprintln(command.dependencies.Stderr, "Local access closed. The remote connection will expire automatically.")
		}
	}()
	config := network.Configuration{Session: session, PrivateKey: private}
	if session.AppRef != ref || session.Mode != mode {
		return errors.New("the server returned a different application or connection mode")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if options != nil && options.database && options.service == "" {
		var names []string
		for _, service := range session.Services {
			if service.Kind == "database" {
				names = append(names, service.Name)
			}
		}
		switch len(names) {
		case 0:
			return errors.New("no database services are available in this application")
		case 1:
			resolved := *options
			resolved.service = names[0]
			options = &resolved
		default:
			return usageError{fmt.Sprintf("multiple database services are available; select one with --database: %s", strings.Join(names, ", "))}
		}
	}
	expired, stop := context.WithDeadline(ctx, session.ExpiresAt)
	defer stop()
	// Poll with current OAuth credentials to detect revocation and application
	// changes. Traffic itself is still governed by the gateway's shorter lease.
	errorsCh := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-expired.Done():
				return
			case <-ticker.C:
				current, err := client.NetworkSession(expired, session.ID)
				if err != nil {
					errorsCh <- err
					cancel()
					return
				}
				if !sameTopology(session, current) {
					errorsCh <- errors.New("private services changed; reconnect to update the connection")
					cancel()
					return
				}
			}
		}
	}()
	ready := func() {
		fmt.Fprintf(command.dependencies.Stdout, "Connected to %s until %s.\n", ref, session.ExpiresAt.Local().Format(time.RFC3339))
		for _, service := range session.Services {
			for _, port := range service.Ports {
				fmt.Fprintf(command.dependencies.Stdout, "  %s:%d\n", service.Hostname, port.Port)
			}
		}
		fmt.Fprintln(command.dependencies.Stdout, "Press Ctrl-C or run infra network disconnect to disconnect.")
	}
	if options == nil {
		err = network.RunNative(expired, config, command.dependencies.Stderr, ready)
	} else {
		destination, selectErr := proxyDestination(session, *options)
		if selectErr != nil {
			return selectErr
		}
		var binary string
		var arguments []string
		var environment []string
		if options.connect {
			resolved := databaseOptions(session, *options)
			binary, arguments, err = databaseClient(resolved, listener.Addr().String())
			if err != nil {
				return err
			}
			if _, err := exec.LookPath(binary); err != nil {
				return fmt.Errorf("install %s or use infra db proxy with your database tool", binary)
			}
			if !options.passwordPrompt {
				var selected api.NetworkService
				for _, service := range session.Services {
					if service.Name == options.service {
						selected = service
						break
					}
				}
				if selected.ID == "" {
					return errors.New("the database is unavailable; reconnect and try again")
				}
				if resolved.user != selected.Connection.Username || resolved.engine != selected.Connection.Engine {
					return usageError{"use --password-prompt to connect with a different user or engine"}
				}
				credentials, fetchErr := client.DatabaseCredentials(expired, session.ID, selected.ID)
				if fetchErr != nil {
					return fetchErr
				}
				if credentials.Engine != selected.Connection.Engine || credentials.Username != selected.Connection.Username || credentials.Database != selected.Connection.Database {
					return errors.New("the database configuration changed; reconnect and try again")
				}
				var cleanup func()
				arguments, environment, cleanup, err = databaseAuthentication(resolved, listener.Addr().String(), credentials.Password)
				if err != nil {
					return err
				}
				defer cleanup()
			}
		}
		tunnel, openErr := network.Open(config)
		if openErr != nil {
			return openErr
		}
		defer tunnel.Close()
		ready()
		fmt.Fprintf(command.dependencies.Stdout, "Forwarding %s to %s.\n", listener.Addr(), destination)
		if options.connect {
			forwardCtx, stopForward := context.WithCancel(expired)
			forwarded := make(chan error, 1)
			go func() { forwarded <- tunnel.Forward(forwardCtx, listener, destination) }()
			child := exec.CommandContext(expired, binary, arguments...)
			child.Env = environment
			child.Stdin, child.Stdout, child.Stderr = command.dependencies.Stdin, command.dependencies.Stdout, command.dependencies.Stderr
			if options.dumpOutput != nil {
				child.Stdout = options.dumpOutput
			}
			clientErr := child.Run()
			stopForward()
			<-forwarded
			err = clientErr
		} else {
			err = tunnel.Forward(expired, listener, destination)
		}
	}
	select {
	case failure := <-errorsCh:
		return failure
	default:
	}
	if expired.Err() != nil {
		if options != nil && options.dumpOutput != nil {
			return errors.New("database dump was interrupted; run it again")
		}
		return nil
	}
	return err
}

func proxyDestination(session api.NetworkSession, options proxyOptions) (string, error) {
	for _, service := range session.Services {
		if service.Name == options.service {
			if options.database && service.Kind != "database" {
				return "", errors.New("the selected service is not a database")
			}
			for _, port := range service.Ports {
				if port.Protocol == "tcp" && ((options.remotePort == 0 && len(service.Ports) == 1) || options.remotePort == int(port.Port)) {
					return net.JoinHostPort(service.Address, fmt.Sprint(port.Port)), nil
				}
			}
			return "", errors.New("select a declared TCP port with --remote-port")
		}
	}
	return "", errors.New("the selected service is not available in this application")
}
func sameTopology(a, b api.NetworkSession) bool {
	left, _ := json.Marshal(struct {
		Gateway  any
		Services any
	}{a.Gateway, a.Services})
	right, _ := json.Marshal(struct {
		Gateway  any
		Services any
	}{b.Gateway, b.Services})
	return string(left) == string(right)
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (command *CLI) networkSocket() string {
	digest := sha256.Sum256([]byte(command.apiURL))
	return filepath.Join(filepath.Dir(command.configPath), "network-"+hex.EncodeToString(digest[:6])+".sock")
}
func (command *CLI) networkControl() (net.Listener, error) {
	path := command.networkSocket()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		existing, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			existing.Close()
			return nil, errors.New("this computer already has a private connection; disconnect it first")
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("cannot use the private connection control path")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func (command *CLI) databaseConnectCommand() *cobra.Command {
	options := proxyOptions{database: true, connect: true}
	cmd := &cobra.Command{Use: "connect team/app", Short: "Open psql or mysql through a private connection", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return command.runNetwork(cmd.Context(), args[0], &options)
	}}
	databaseClientFlags(cmd, &options)
	return cmd
}

func databaseClientFlags(cmd *cobra.Command, options *proxyOptions) {
	cmd.Flags().StringVar(&options.service, "database", "", "database service name (optional when the application has only one database)")
	cmd.Flags().StringVar(&options.engine, "engine", "", "database client: postgres or mysql (defaults to the deployed configuration)")
	cmd.Flags().BoolVar(&options.passwordPrompt, "password-prompt", false, "enter a password in the database client instead of retrieving it")
	cmd.Flags().StringVar(&options.user, "user", "", "database username (defaults to the deployed configuration)")
	cmd.Flags().StringVar(&options.databaseName, "database-name", "", "database name (defaults to the deployed configuration)")
	cmd.Flags().IntVar(&options.remotePort, "remote-port", 0, "declared database port")
}

func databaseOptions(session api.NetworkSession, options proxyOptions) proxyOptions {
	for _, service := range session.Services {
		if service.Name == options.service && service.Kind == "database" {
			if options.engine == "" {
				options.engine = service.Connection.Engine
			}
			if options.user == "" {
				options.user = service.Connection.Username
			}
			if options.databaseName == "" {
				options.databaseName = service.Connection.Database
			}
			break
		}
	}
	return options
}

// The base invocation prompts for a password. Automatic login replaces the
// prompt with a private credential file in databaseAuthentication.
func databaseClient(options proxyOptions, address string) (string, []string, error) {
	if options.user == "" || options.databaseName == "" || strings.HasPrefix(options.user, "-") || strings.HasPrefix(options.databaseName, "-") || strings.ContainsAny(options.databaseName, "=\x00\r\n") || strings.Contains(options.databaseName, "://") || strings.ContainsAny(options.user, "\x00\r\n") {
		return "", nil, usageError{"specify --user and --database-name"}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", nil, err
	}
	switch options.engine {
	case "postgres":
		if options.dumpOutput != nil {
			return "pg_dump", []string{"--format=plain", "-W", "-h", host, "-p", port, "-U", options.user, "-d", options.databaseName}, nil
		}
		return "psql", []string{"-X", "-W", "-h", host, "-p", port, "-U", options.user, "-d", options.databaseName}, nil
	case "mysql":
		if options.dumpOutput != nil {
			return "mysqldump", []string{"--protocol=TCP", "--host=" + host, "--port=" + port, "--user=" + options.user, "--single-transaction", "--quick", "--no-tablespaces", "--set-gtid-purged=OFF", "--password", options.databaseName}, nil
		}
		return "mysql", []string{"--protocol=TCP", "--host=" + host, "--port=" + port, "--user=" + options.user, "--database=" + options.databaseName, "--password"}, nil
	default:
		return "", nil, usageError{"choose --engine postgres or mysql"}
	}
}
