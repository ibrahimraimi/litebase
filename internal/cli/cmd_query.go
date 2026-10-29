package cli

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/litebase/litebase/internal/engine"
	"github.com/spf13/cobra"
)

func newQueryCommand(flags *RuntimeFlags) *cobra.Command {
	var isRemote bool
	cmd := &cobra.Command{
		Use:   "query <sql>",
		Short: "Execute an ad-hoc SQL statement against the local or remote database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if isRemote {
				return executeRemoteQuery(cmd, *flags, args[0])
			}
			return executeLocalQuery(cmd, flags.WorkDir, args[0])
		},
	}
	cmd.Flags().BoolVar(&isRemote, "remote", false, "Run query against the linked remote database")
	return cmd
}

func executeRemoteQuery(cmd *cobra.Command, flags RuntimeFlags, sqlText string) error {
	apiClient, dbName, _, err := ResolveRemoteTarget(flags)
	if err != nil {
		return err
	}
	outcome, err := apiClient.Query(cmd.Context(), dbName, engine.Statement{SQL: sqlText})
	if err != nil {
		return err
	}
	renderQueryResult(cmd.OutOrStdout(), outcome)
	return nil
}

func executeLocalQuery(cmd *cobra.Command, workDir string, sqlText string) error {
	localDBPath := filepath.Join(workDir, DefaultLocalDBRelPath)
	localDB, err := engine.OpenDatabase(cmd.Context(), engine.NewSQLiteEngine(), localDBPath, engine.Options{})
	if err != nil {
		return err
	}
	defer localDB.Close(cmd.Context())

	outcome, err := localDB.Execute(cmd.Context(), engine.Statement{SQL: sqlText})
	if err != nil {
		return err
	}
	renderQueryResult(cmd.OutOrStdout(), outcome)
	return nil
}

func renderQueryResult(out io.Writer, outcome engine.QueryResult) {
	if len(outcome.Columns) == 0 {
		fmt.Fprintf(out, "OK (rows_affected=%d, duration_ms=%.2f)\n", outcome.RowsAffected, outcome.DurationMS)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(outcome.Columns, "\t"))
	for _, row := range outcome.Rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			if cell.Value == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = *cell.Value
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

func newShellCommand(flags *RuntimeFlags) *cobra.Command {
	var isRemote bool
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Start an interactive SQL REPL",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if isRemote {
				return runREPL(cmd, func(sqlLine string) error {
					return executeRemoteQuery(cmd, *flags, sqlLine)
				})
			}
			return runREPL(cmd, func(sqlLine string) error {
				return executeLocalQuery(cmd, flags.WorkDir, sqlLine)
			})
		},
	}
	cmd.Flags().BoolVar(&isRemote, "remote", false, "Connect REPL to the linked remote database")
	return cmd
}

func runREPL(cmd *cobra.Command, executeLine func(string) error) error {
	scanner := bufio.NewScanner(cmd.InOrStdin())
	for {
		fmt.Fprint(cmd.OutOrStdout(), "litebase> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == ".exit" || line == ".quit" || strings.EqualFold(line, "exit") {
			return nil
		}
		if err := executeLine(line); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %v\n", err)
		}
	}
}
