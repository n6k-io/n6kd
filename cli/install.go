package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/n6k-io/n6kd/engine"
)

func newInstallCmd() *cobra.Command {
	var configPath string
	var skipInstalled, debug, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download the n6k DuckDB extensions into DuckDB's extension directory",
		Long: "Installs the n6k extensions this server loads from the release bucket, and the ones\n" +
			"quack loads from DuckDB's, re-downloading any already installed unless --skip-installed\n" +
			"is given.\n\n" +
			"--config can pin each n6k extension to a release version or tag:\n\n" +
			"  version: 1\n" +
			"  extensions:\n" +
			"    vcatbridge:\n" +
			"      version: v0\n\n" +
			"Set " + engine.ExtensionHomeEnv + " to choose where they land (default: an extensions directory beside the binary if one exists, else DuckDB's own)\n" +
			"and " + engine.ExtensionRepoEnv + " to install from somewhere other than the default bucket.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			log, err := installLogger(debug, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			if configPath == "" {
				return errors.New("--config is required when neither XDG_CONFIG_HOME nor the home directory is known")
			}
			config, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			return engine.InstallExtensions(cmd.Context(), log, engine.InstallOptions{
				Force:    !skipInstalled,
				Versions: config.extensionVersions(),
			})
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath(), "n6kd config with extension versions")
	cmd.Flags().BoolVar(&skipInstalled, "skip-installed", os.Getenv("N6KD_SKIP_INSTALLED") != "",
		"keep extensions that are already installed instead of re-downloading them")
	cmd.Flags().BoolVar(&debug, "debug", os.Getenv("N6KD_DEBUG") != "", "log per-extension detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}
