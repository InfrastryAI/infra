package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func (command *CLI) databaseDumpCommand() *cobra.Command {
	options := proxyOptions{database: true, connect: true}
	var output string
	cmd := &cobra.Command{
		Use:     "dump team/app",
		Short:   "Save a database dump to a local SQL file",
		Long:    "Dump a database's schema and data through a private connection using an installed pg_dump or mysqldump. Saves a timestamped SQL file in the current directory unless --output is supplied. Existing files are never overwritten.",
		Example: "  infra db dump acme/customer-api\n  infra db dump acme/customer-api --database primary --output backup.sql",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return command.runDatabaseDump(cmd.Context(), args[0], options, output)
		},
	}
	databaseClientFlags(cmd, &options)
	cmd.Flags().StringVarP(&output, "output", "o", "", "destination SQL file (defaults to team-app-<UTC timestamp>.sql)")
	_ = cmd.MarkFlagFilename("output", "sql")
	return cmd
}

func (command *CLI) runDatabaseDump(ctx context.Context, ref string, options proxyOptions, output string) error {
	if !appReference.MatchString(ref) {
		return usageError{"specify an unambiguous team/app reference"}
	}
	if output == "" {
		output = strings.ReplaceAll(ref, "/", "-") + "-" + command.dependencies.Now().UTC().Format("20060102T150405.000000000Z") + ".sql"
	}
	if !filepath.IsAbs(output) {
		directory, err := command.dependencies.WorkingDir()
		if err != nil {
			return err
		}
		output = filepath.Join(directory, output)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("dump file %q already exists; choose another path with --output", output)
	}
	if err != nil {
		return fmt.Errorf("cannot create dump file: %w", err)
	}
	completed := false
	defer func() {
		file.Close()
		if !completed {
			if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(command.dependencies.Stderr, "Could not remove incomplete dump %q: %v\n", output, err)
			}
		}
	}()

	options.dumpOutput = file
	if err := command.runNetwork(ctx, ref, &options); err != nil {
		if errors.Is(err, context.Canceled) {
			return errors.New("database dump was interrupted; run it again")
		}
		return err
	}
	if ctx.Err() != nil {
		return errors.New("database dump was interrupted; run it again")
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("could not save database dump: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("could not save database dump: %w", err)
	}
	completed = true
	fmt.Fprintf(command.dependencies.Stdout, "Database dump saved to %s.\n", output)
	return nil
}
