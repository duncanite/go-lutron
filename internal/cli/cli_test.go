package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forkcloser/xos"
	"github.com/mycophonic/primordium/filesystem"

	"github.com/duncanite/go-lutron/internal/cli"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

const wait = 30 * time.Second

// house is a test bridge holding a dimmer, a Pico and a scene, and a
// directory with credentials for it.
func house(t *testing.T) (*leaptest.Bridge, string) {
	t.Helper()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	ref := func(href string) map[string]any { return map[string]any{"href": href} }

	bridge.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		leaptest.Itself(),
		{
			"href": "/device/2", "Name": "Lights", "FullyQualifiedName": []string{"Hallway", "Lights"},
			"SerialNumber": 2345, "ModelNumber": "PD-6WCL-XX", "DeviceType": "WallDimmer",
			"LocalZones": []any{ref("/zone/1")},
			"FirmwareImage": map[string]any{"Contents": []any{map[string]any{
				"Type": "CCA", "OS": map[string]any{"Firmware": map[string]any{"DisplayName": "001.019.000r000"}},
			}}},
		},
		{
			"href": "/device/8", "Name": "Pico", "FullyQualifiedName": []string{"Bedroom", "Pico"},
			"SerialNumber": 4567, "ModelNumber": "PJ2-3BRL", "DeviceType": "Pico3ButtonRaiseLower",
			"ButtonGroups": []any{ref("/buttongroup/2")},
		},
	}})
	bridge.Set("/button", "MultipleButtonDefinition", map[string]any{"Buttons": []map[string]any{
		{"href": "/button/101", "ButtonNumber": 0, "Name": "Button 1", "Parent": ref("/buttongroup/2")},
	}})
	bridge.Set("/virtualbutton", "MultipleVirtualButtonDefinition", map[string]any{"VirtualButtons": []map[string]any{
		{"href": "/virtualbutton/1", "Name": "Movie", "IsProgrammed": true},
		{"href": "/virtualbutton/2", "Name": "", "IsProgrammed": false},
	}})
	bridge.Set("/zone/1/status", "OneZoneStatus", map[string]any{
		"ZoneStatus": map[string]any{"Level": 75, "Zone": ref("/zone/1")},
	})

	dir := t.TempDir()
	write(t, dir, bridge.Credentials())

	return bridge, dir
}

func write(t *testing.T, dir string, credentials *leap.Credentials) {
	t.Helper()

	for name, data := range map[string][]byte{
		"key.pem": credentials.Key, "certificate.pem": credentials.Certificate, "root.pem": credentials.Root,
	} {
		if err := filesystem.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func run(ctx context.Context, bridge *leaptest.Bridge, dir string, args ...string) (string, int) {
	var out syncBuffer

	status := cli.Run(ctx, append([]string{"--host", bridge.Addr(), "--credentials", dir}, args...), &out, &out)

	return out.String(), status
}

// syncBuffer is a buffer the command's reading goroutine and the test
// can both use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// await waits for the command to have printed text.
func await(t *testing.T, out *syncBuffer, text string) {
	t.Helper()

	deadline := time.Now().Add(wait)
	for !strings.Contains(out.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %q; the command printed:\n%s", text, out.String())
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheCommands: what the bridge holds as a person reads it, and each
// command as the bridge gets it.
func TestTheCommands(t *testing.T) {
	t.Parallel()

	bridge, dir := house(t)

	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"ping"}, "LEAP 1.115"},
		{[]string{"devices"}, "DEVICE  NAME            TYPE                   MODEL       SERIAL  FIRMWARE         ZONE  BUTTONS"},
		{[]string{"devices"}, "1       Smart Bridge    SmartBridge            L-BDG2-WH   1001"},
		{[]string{"devices"}, "2       Hallway Lights  WallDimmer             PD-6WCL-XX  2345    001.019.000r000  1"},
		// The Pico's answer carries no firmware: the column is empty.
		{[]string{"devices"}, "8       Bedroom Pico    Pico3ButtonRaiseLower  PJ2-3BRL    4567                           101=0"},
		{[]string{"scenes"}, "1\tMovie\n"},
		{[]string{"level", "1"}, "zone 1 is at 75"},
		{[]string{"level", "lights"}, "zone 1 is at 75"},
		{[]string{"level", "Hallway Lights"}, "zone 1 is at 75"},
		{[]string{"call", "ReadRequest", "/zone/1/status"}, `"StatusCode": "200 OK"`},
	} {
		out, status := run(t.Context(), bridge, dir, step.args...)
		if status != cli.ExitOK || !strings.Contains(out, step.want) {
			t.Fatalf("%v: status %d, output:\n%s\nwant %q", step.args, status, out, step.want)
		}
	}

	if out, _ := run(t.Context(), bridge, dir, "scenes"); strings.Contains(out, "2\t") {
		t.Fatalf("scenes printed a slot with no scene:\n%s", out)
	}

	for _, step := range []struct {
		args []string
		url  string
		body string
	}{
		{[]string{"set", "lights", "40"}, "/zone/1/commandprocessor", `"GoToLevel","Parameter":[{"Type":"Level","Value":40}]`},
		{[]string{"set", "1", "10", "--fade", "90s"}, "/zone/1/commandprocessor", `"Level":10,"FadeTime":"00:01:30"`},
		{[]string{"on", "1"}, "/zone/1/commandprocessor", `"Value":100`},
		{[]string{"off", "1"}, "/zone/1/commandprocessor", `"Value":0`},
		{[]string{"raise", "1"}, "/zone/1/commandprocessor", `"CommandType":"Raise"`},
		{[]string{"lower", "1"}, "/zone/1/commandprocessor", `"CommandType":"Lower"`},
		{[]string{"stop", "1"}, "/zone/1/commandprocessor", `"CommandType":"Stop"`},
		{[]string{"press", "101"}, "/button/101/commandprocessor", `"CommandType":"PressAndRelease"`},
		{[]string{"run", "1"}, "/virtualbutton/1/commandprocessor", `"CommandType":"PressAndRelease"`},
		{
			[]string{"call", "UpdateRequest", "/system/away/1/status", `{"AwayStatus":{"EnabledState":"Enabled"}}`},
			"/system/away/1/status", `{"AwayStatus":{"EnabledState":"Enabled"}}`,
		},
	} {
		before := len(bridge.Commands())

		if out, status := run(t.Context(), bridge, dir, step.args...); status != cli.ExitOK {
			t.Fatalf("%v: status %d\n%s", step.args, status, out)
		}

		commands := bridge.Commands()
		if len(commands) != before+1 || commands[before].URL != step.url ||
			!strings.Contains(string(commands[before].Body), step.body) {
			t.Fatalf("%v: the bridge got %+v", step.args, commands[before:])
		}
	}
}

// TestTheErrors: what a person sees when it goes wrong, and that nothing
// reached the bridge.
func TestTheErrors(t *testing.T) {
	t.Parallel()

	bridge, dir := house(t)

	other := leaptest.New()
	t.Cleanup(other.Close)

	stranger := t.TempDir()
	write(t, stranger, other.Credentials())

	for _, step := range []struct {
		dir  string
		args []string
		want string
	}{
		{dir, []string{"level", "kitchen"}, `no zone by that id or name: "kitchen"`},
		{dir, []string{"set", "1", "101"}, "not a valid argument: a level of 101"},
		{dir, []string{"press", "1/2"}, `not a valid argument: the id "1/2"`},
		{dir, []string{"call", "ReadRequest", "/nothing"}, `"StatusCode": "404 NotFound"`},
		{dir, []string{"call", "ReadRequest", "/nothing"}, "the bridge refused the request"},
		{dir, []string{"call", "CreateRequest", "/zone/1/commandprocessor", "{not json"}, "the body is not JSON"},
		{dir, []string{"call", "DeleteRequest", "/zone/1"}, "must be one of"},
		{t.TempDir(), []string{"ping"}, "pair writes them"},
		{stranger, []string{"ping"}, "the credentials are not usable"},
	} {
		out, status := run(t.Context(), bridge, step.dir, step.args...)
		if status != cli.ExitError || !strings.Contains(out, step.want) {
			t.Fatalf("%v: status %d, output:\n%s\nwant %q", step.args, status, out, step.want)
		}
	}

	if commands := bridge.Commands(); len(commands) != 0 {
		t.Fatalf("a refused command reached the bridge: %+v", commands)
	}
}

// TestPairCommand: the button asked for, the credentials written with
// the key private, used by the next command; credentials already there
// kept unless forced.
func TestPairCommand(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	app := t.TempDir()
	certificate, key, root := bridge.BootstrapPEM()
	write(t, app, &leap.Credentials{Key: key, Certificate: certificate, Root: root})

	// A file left under the reserved name by a run that died, readable
	// by all: the key does not inherit it.
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := filesystem.WriteFile(filepath.Join(dir, "key.pem.new"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	pair := []string{
		"--host", bridge.PairingAddr(), "--credentials", dir, "pair",
		"--app-certificate", filepath.Join(app, "certificate.pem"),
		"--app-key", filepath.Join(app, "key.pem"), "--app-root", filepath.Join(app, "root.pem"),
		"--name", "the test",
	}

	var out syncBuffer

	done := make(chan int, 1)

	go func() { done <- cli.Run(t.Context(), pair, &out, &out) }()

	await(t, &out, "Press the button on the bridge.")
	bridge.Press()

	if status := <-done; status != cli.ExitOK || !strings.Contains(out.String(), `Paired as "the test"`) {
		t.Fatalf("pair: %d\n%s", status, out.String())
	}

	ownerOnly(t, filepath.Join(dir, "key.pem"))

	// The file the key was written into is gone, renamed to the key's.
	if _, statErr := xos.Stat(filepath.Join(dir, "key.pem.new")); statErr == nil {
		t.Fatal("pair left the reserved file behind")
	}

	if text, status := run(t.Context(), bridge, dir, "ping"); status != cli.ExitOK {
		t.Fatalf("ping under the paired credentials: %d\n%s", status, text)
	}

	var again syncBuffer

	if status := cli.Run(t.Context(), pair, &again, &again); status != cli.ExitError ||
		!strings.Contains(again.String(), "credentials are already there") {
		t.Fatalf("pairing over credentials: %d\n%s", status, again.String())
	}

	if paired := bridge.Paired(); len(paired) != 1 {
		t.Fatalf("the bridge signed %d times", len(paired))
	}

	// Forced, it pairs over them; the button is still pressed.
	var forced syncBuffer

	if status := cli.Run(t.Context(), append(pair, "--force"), &forced, &forced); status != cli.ExitOK {
		t.Fatalf("pairing over credentials, forced: %d\n%s", status, forced.String())
	}

	if paired := bridge.Paired(); len(paired) != 2 {
		t.Fatalf("forced, the bridge signed %d times in all", len(paired))
	}
}

// TestPairBeforeTheButton: what pair settles before it asks for the
// button: where the credentials go, which port, and how long it waits.
func TestPairBeforeTheButton(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	app := t.TempDir()
	certificate, key, root := bridge.BootstrapPEM()
	write(t, app, &leap.Credentials{Key: key, Certificate: certificate, Root: root})

	pair := func(host, dir string, more ...string) (string, int) {
		var out syncBuffer

		args := append([]string{
			"--host", host, "--credentials", dir, "pair",
			"--app-certificate", filepath.Join(app, "certificate.pem"),
			"--app-key", filepath.Join(app, "key.pem"), "--app-root", filepath.Join(app, "root.pem"),
		}, more...)

		status := cli.Run(t.Context(), args, &out, &out)

		return out.String(), status
	}

	// No button within the timeout: it gives up, and the bridge signed
	// nothing.
	unpaired := filepath.Join(t.TempDir(), "c")

	out, status := pair(bridge.PairingAddr(), unpaired, "--timeout", "300ms")
	if status != cli.ExitError || !strings.Contains(out, "Press the button") ||
		!strings.Contains(out, "deadline exceeded") {
		t.Fatalf("no button within the timeout: %d\n%s", status, out)
	}

	// A file left under the reserved name by a run that died, readable
	// by all, is not what the key is written into.
	leftover := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := filesystem.WriteFile(filepath.Join(leftover, "key.pem.new"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	paired := leaptest.New()
	t.Cleanup(paired.Close)

	pairedApp := t.TempDir()
	certificate, key, root = paired.BootstrapPEM()
	write(t, pairedApp, &leap.Credentials{Key: key, Certificate: certificate, Root: root})

	var over syncBuffer

	done := make(chan int, 1)

	go func() {
		done <- cli.Run(t.Context(), []string{
			"--host", paired.PairingAddr(), "--credentials", leftover, "pair",
			"--app-certificate", filepath.Join(pairedApp, "certificate.pem"),
			"--app-key", filepath.Join(pairedApp, "key.pem"), "--app-root", filepath.Join(pairedApp, "root.pem"),
		}, &over, &over)
	}()

	await(t, &over, "Press the button on the bridge.")

	// While it waits, another writer of the directory puts a file of
	// its own under the reserved name: the key must not go into it.
	swapped := filepath.Join(leftover, "key.pem.new")
	if err := os.Remove(swapped); err != nil {
		t.Fatal(err)
	}

	if err := filesystem.WriteFile(swapped, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	paired.Press()

	if status = <-done; status != cli.ExitError || !strings.Contains(over.String(), "no longer the one by its name") {
		t.Fatalf("a reserved file replaced during the wait: %d\n%s", status, over.String())
	}

	if _, err := xos.Stat(filepath.Join(leftover, "key.pem")); err == nil {
		t.Fatal("the key was written though its file had been replaced")
	}

	// The file reserved for the key does not outlive a pairing that
	// did not happen.
	if left, err := xos.ReadDir(unpaired); err != nil || len(left) != 0 {
		t.Fatalf("after a pairing that did not happen the directory holds %v (%v)", left, err)
	}

	// A directory that cannot be made is found out before the button is
	// asked for: a pairing granted and not written would be lost.
	file := filepath.Join(t.TempDir(), "a file")
	if err := filesystem.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out, status = pair(bridge.PairingAddr(), filepath.Join(file, "c"))
	if status != cli.ExitError || !strings.Contains(out, "making ") || strings.Contains(out, "Press the button") {
		t.Fatalf("a credentials directory that cannot be made: %d\n%s", status, out)
	}

	// A host with no port is dialled on the pairing port; nothing
	// listens there, and the error names what was dialled.
	for host, dialled := range map[string]string{
		"127.0.0.1": "127.0.0.1:8083", "::1": "[::1]:8083", "[::1]": "[::1]:8083", "127.0.0.1:1": "127.0.0.1:1",
	} {
		out, status = pair(host, filepath.Join(t.TempDir(), "c"), "--timeout", "2s")
		if status != cli.ExitError ||
			!strings.Contains(out, "the pairing port: connecting to the bridge at "+dialled+":") {
			t.Fatalf("%s: %d\n%s\nwant a dial of %s", host, status, out, dialled)
		}
	}

	// With no identity given, pair brings the Caseta app's: the test
	// bridge's pairing port chains to another root, and is refused as
	// not Lutron's.
	var bare syncBuffer

	status = cli.Run(t.Context(), []string{
		"--host", bridge.PairingAddr(), "--credentials", filepath.Join(t.TempDir(), "c"), "pair", "--timeout", "5s",
	}, &bare, &bare)
	if status != cli.ExitError || !strings.Contains(bare.String(), "the bridge's certificate") {
		t.Fatalf(
			"pair under the Caseta app's identity, to a bridge that is not Lutron's: %d\n%s",
			status,
			bare.String(),
		)
	}

	// Another identity is three files, all or none.
	files := map[string]string{
		"--app-certificate": filepath.Join(app, "certificate.pem"),
		"--app-key":         filepath.Join(app, "key.pem"),
		"--app-root":        filepath.Join(app, "root.pem"),
	}

	for left := range files {
		for _, some := range [][]string{{left}, without(files, left)} {
			args := []string{"--host", bridge.PairingAddr(), "--credentials", filepath.Join(t.TempDir(), "c"), "pair"}
			for _, flag := range some {
				args = append(args, flag, files[flag])
			}

			var partial syncBuffer

			if status = cli.Run(t.Context(), args, &partial, &partial); status != cli.ExitError ||
				!strings.Contains(partial.String(), "go together") {
				t.Fatalf("pair with %v: %d\n%s", some, status, partial.String())
			}
		}
	}

	if paired := bridge.Paired(); len(paired) != 0 {
		t.Fatalf("the bridge signed without its button: %v", paired)
	}
}

// TestANameTwoRoomsShare: a name that fits two zones acts on neither and
// says which they are; the room with it, or the zone's id, picks one.
func TestANameTwoRoomsShare(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	device := func(id, room, zone string) map[string]any {
		return map[string]any{
			"href": "/device/" + id, "Name": "Main Lights", "FullyQualifiedName": []string{room, "Main Lights"},
			"DeviceType": "WallDimmer", "LocalZones": []any{map[string]any{"href": "/zone/" + zone}},
		}
	}

	bridge.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		leaptest.Itself(), device("2", "Kitchen", "1"), device("3", "Bedroom", "5"),
	}})

	dir := t.TempDir()
	write(t, dir, bridge.Credentials())

	out, status := run(t.Context(), bridge, dir, "off", "Main Lights")
	if status != cli.ExitError || !strings.Contains(out, "several zones by that name") ||
		!strings.Contains(out, `"Kitchen Main Lights" (zone 1), "Bedroom Main Lights" (zone 5)`) {
		t.Fatalf("a name two rooms share: %d\n%s", status, out)
	}

	if commands := bridge.Commands(); len(commands) != 0 {
		t.Fatalf("an ambiguous name reached the bridge: %+v", commands)
	}

	for _, target := range []string{"bedroom main lights", "5"} {
		before := len(bridge.Commands())

		if out, status = run(t.Context(), bridge, dir, "off", target); status != cli.ExitOK {
			t.Fatalf("off %q: %d\n%s", target, status, out)
		}

		if commands := bridge.Commands(); len(commands) != before+1 ||
			commands[before].URL != "/zone/5/commandprocessor" {
			t.Fatalf("off %q: the bridge got %+v", target, commands[before:])
		}
	}
}

// TestWatchCommand: zone changes and button presses printed as they
// come, until the context ends.
func TestWatchCommand(t *testing.T) {
	t.Parallel()

	bridge, dir := house(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out syncBuffer

	done := make(chan int, 1)

	go func() {
		done <- cli.Run(ctx, []string{"--host", bridge.Addr(), "--credentials", dir, "watch"}, &out, &out)
	}()

	// Each zone is read once first, and printed where it stands; then
	// the command says that what follows is what happens.
	await(t, &out, "zone 1 is at 75")
	await(t, &out, "watching\n")

	bridge.Unsolicited("/zone/1/status", "OneZoneStatus", map[string]any{
		"ZoneStatus": map[string]any{"Level": 30, "Zone": map[string]any{"href": "/zone/1"}},
	})
	bridge.Emit("/button/101/status/event", "OneButtonStatusEvent", map[string]any{
		"ButtonStatus": map[string]any{
			"Button": map[string]any{"href": "/button/101"}, "ButtonEvent": map[string]any{"EventType": "Press"},
		},
	})

	await(t, &out, "zone 1 is at 30")
	await(t, &out, "button 101 press")
	cancel()

	if status := <-done; status != cli.ExitOK {
		t.Fatalf("watch: %d\n%s", status, out.String())
	}
}

// without is the map's keys but one.
func without(all map[string]string, left string) []string {
	var rest []string

	for key := range all {
		if key != left {
			rest = append(rest, key)
		}
	}

	return rest
}
