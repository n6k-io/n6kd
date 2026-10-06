package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/joho/godotenv"
	"github.com/spf13/pflag"
)

const dotenvFlag = "load-dotenv"

func loadDotenv(args []string) error {
	flags := pflag.NewFlagSet("dotenv", pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.SetOutput(io.Discard)
	path := addDotenvFlag(flags)
	if err := flags.Parse(args); err != nil && !errors.Is(err, pflag.ErrHelp) {
		return err
	}
	if *path == "" {
		return nil
	}
	if err := godotenv.Load(*path); err != nil {
		return fmt.Errorf("--%s: %w", dotenvFlag, err)
	}
	return nil
}

func addDotenvFlag(flags *pflag.FlagSet) *string {
	path := flags.String(dotenvFlag, "",
		"load environment variables from this file (default .env) before anything reads them; set variables win")
	flags.Lookup(dotenvFlag).NoOptDefVal = ".env"
	return path
}
