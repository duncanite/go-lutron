// lutron speaks to a Lutron Caséta bridge over LEAP; the command is
// internal/cli.
package main

import (
	"context"
	"os"

	"github.com/mycophonic/primordium/app"
	"github.com/mycophonic/primordium/app/shutdown"

	"github.com/duncanite/go-lutron/internal/cli"
)

func main() {
	// Main runs the shutdown handlers however run ends and exits with the
	// status an ExitError asks for, silently: every message is printed.
	app.Main(&app.Options{Name: "lutron"}, func(ctx context.Context) error {
		env := cli.Environment{Args: os.Args[1:], Out: os.Stdout, Err: os.Stderr}

		if code := cli.Run(ctx, env); code != cli.ExitOK {
			return shutdown.ExitError(code)
		}

		return nil
	})
}
