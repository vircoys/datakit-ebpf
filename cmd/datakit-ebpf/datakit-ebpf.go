//go:build linux
// +build linux

package main

import (
	"fmt"

	"github.com/GuanceCloud/datakit-ebpf/internal/cmd"
	"github.com/spf13/cobra"
)

var (
	Version = "0.0.0-dev"
	Arch    = "unknown"
	Date    = "unknown"
)

func main() {
	rootCmd := cmd.NewRootCmd()
	rootCmd.AddCommand(newVersionCmd())

	cmd.Execute(rootCmd)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "show datakit-ebpf version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("datakit-ebpf version %s %s %s\n",
				Version, Arch, Date)
		},
	}
}
