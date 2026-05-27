package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"microwave/internal/microwave"
)

const Version = "0.0.1"

var (
	outPath string
	pkgName string
	version bool
)

var rootCmd = &cobra.Command{
	Use:           "microwave <path>... --out <file> --pkg <name>",
	Short:         "generate a single-file umbrella package for a Go module",
	Long:          "microwave scans tagged declarations across one or more Go packages and emits a single umbrella file that re-exports them under a chosen package name.",
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if version {
			fmt.Printf("microwave version %s\n", Version)
			return nil
		}

		if len(args) == 0 {
			return usageError("at least one scan path is required")
		}
		if outPath == "" {
			return usageError("--out is required")
		}
		if pkgName == "" {
			return usageError("--pkg is required")
		}

		return microwave.Run(microwave.Config{
			Paths:  args,
			Out:    outPath,
			Pkg:    pkgName,
			Args:   os.Args[1:],
			Stderr: os.Stderr,
		})
	},
}

type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

func usageError(msg string) error { return &usageErr{msg: msg} }

func init() {
	rootCmd.Flags().StringVar(
		&outPath,
		"out",
		"",
		"path of the generated Go file (required)",
	)
	rootCmd.Flags().StringVar(
		&pkgName,
		"pkg",
		"",
		"package name declared at the top of the generated file (required)",
	)
	rootCmd.Flags().BoolVarP(
		&version,
		"version",
		"v",
		false,
		"print the version of microwave",
	)
}

func Execute() {
	err := rootCmd.Execute()
	if err == nil {
		return
	}

	var ue *usageErr
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(os.Stderr, "microwave: "+ue.Error())
		os.Exit(2)
	case errors.Is(err, microwave.ErrUsage):
		os.Exit(2)
	case errors.Is(err, microwave.ErrPipeline):
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "microwave: "+err.Error())
		os.Exit(1)
	}
}
