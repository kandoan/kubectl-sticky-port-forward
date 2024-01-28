package main

import (
	"os"

	"github.com/kubectl-stickyforward/pkg/cmd"
	"github.com/spf13/pflag"

	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func main() {
	flags := pflag.NewFlagSet("kubectl-stickyforward", pflag.ExitOnError)
	pflag.CommandLine = flags

	root := cmd.NewCmd(genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
