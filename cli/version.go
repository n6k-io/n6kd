package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/n6k-io/n6kd/engine"
)

// Not cobra's own --version: that prints a template, which cannot report a failure to open DuckDB.
func addVersionFlag(cmd *cobra.Command) {
	version := cmd.Flags().Bool("version", false, "print n6kd, DuckDB, MCP and extension versions")
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if *version {
			cmd.SilenceUsage = true
			return writeVersion(cmd.Context(), cmd.OutOrStdout())
		}
		return run(cmd, args)
	}
}

func writeVersion(ctx context.Context, w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "n6kd %s%s\n", buildVersion(), vcsSuffix())
	fmt.Fprintf(&b, "%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	info, err := engine.Versions(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "duckdb %s (%s, duckdb-go %s)\n", info.DuckDB, info.DuckDBSource, depVersion("github.com/duckdb/duckdb-go/v2"))
	fmt.Fprintf(&b, "mcp server %s, client %s (go-sdk %s)\n",
		strings.Join(mcp.SupportedProtocolVersions(), " "), remoteProtocolVersion, depVersion("github.com/modelcontextprotocol/go-sdk"))

	dir := info.ExtensionDir
	if dir == "" {
		dir = "duckdb default"
	}
	fmt.Fprintf(&b, "extensions (%s):\n", dir)
	for _, e := range info.Extensions {
		switch {
		case e.Path == "":
			fmt.Fprintf(&b, "  %-11s not installed\n", e.Name)
		case e.Version == "":
			fmt.Fprintf(&b, "  %-11s %s\n", e.Name, e.Path)
		default:
			fmt.Fprintf(&b, "  %-11s %s\n", e.Name, e.Version)
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func vcsSuffix() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	rev := settings["vcs.revision"]
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if t := settings["vcs.time"]; t != "" {
		return fmt.Sprintf(" (%s, %s)", rev, t)
	}
	return fmt.Sprintf(" (%s)", rev)
}

func depVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == path {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "unknown"
}
