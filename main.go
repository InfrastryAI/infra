package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/InfrastryAI/infra/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := cli.New(cli.Dependencies{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Version: cli.Version{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	})

	os.Exit(command.Run(ctx, os.Args[1:]))
}
