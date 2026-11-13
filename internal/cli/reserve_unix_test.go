//go:build !windows

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duncanite/go-lutron/internal/cli"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

// TestPairReservesBeforeTheButton: a credentials directory the key's
// file cannot be made in is found out before the button is asked for. A
// directory's mode is what refuses here, which Windows does not have.
func TestPairReservesBeforeTheButton(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root writes where a mode says no")
	}

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	dir := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	var out syncBuffer

	app := t.TempDir()
	certificate, key, root := bridge.BootstrapPEM()
	write(t, app, &leap.Credentials{Key: key, Certificate: certificate, Root: root})

	status := cli.Run(t.Context(), []string{
		"--host", bridge.PairingAddr(), "--credentials", dir, "pair",
		"--app-certificate", filepath.Join(app, "certificate.pem"),
		"--app-key", filepath.Join(app, "key.pem"), "--app-root", filepath.Join(app, "root.pem"),
	}, &out, &out)
	if status != cli.ExitError || !strings.Contains(out.String(), "key.pem.new") ||
		strings.Contains(out.String(), "Press the button") {
		t.Fatalf("a directory the key's file cannot be made in: %d\n%s", status, out.String())
	}

	if paired := bridge.Paired(); len(paired) != 0 {
		t.Fatalf("the bridge signed: %v", paired)
	}
}
