//go:build !dev

package main

import "github.com/spf13/cobra"

// registerDev is a no-op in release builds: the P0 tooling is not shipped.
func registerDev(*cobra.Command, *App) {}
