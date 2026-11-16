package caseta_test

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

//go:embed testdata/*.json
var recorded embed.FS

//go:embed testdata/smartbridge/*.json
var captured embed.FS

const wait = 30 * time.Second

// bridge starts a test bridge holding the recorded answers, and opens
// it.
func bridge(t *testing.T) (*leaptest.Bridge, *caseta.Bridge) {
	t.Helper()

	fake := leaptest.New()
	t.Cleanup(fake.Close)

	for _, name := range []string{"devices", "areas", "buttons", "scenes"} {
		raw, err := recorded.ReadFile("testdata/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}

		var answer leap.Message
		if err = json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}

		fake.Set(answer.Header.URL, answer.Header.MessageBodyType, answer.Body)
	}

	opened, err := caseta.Open(t.Context(), fake.Addr(), fake.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	return fake, opened
}

// TestWhatTheBridgeHolds: pylutron-caseta's fixtures for a Caseta bridge, read
// into the package's types.
func TestWhatTheBridgeHolds(t *testing.T) {
	t.Parallel()

	_, opened := bridge(t)

	version, err := opened.Ping(t.Context())
	if err != nil || version != leaptest.LEAPVersion {
		t.Fatalf("ping: %v, %v", version, err)
	}

	devices, err := opened.Devices(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(devices) < 9 {
		t.Fatalf("devices: %d", len(devices))
	}

	if self := devices[0]; self.Href != "/device/1" || self.DeviceType != "SmartBridge" ||
		self.ModelNumber != "L-BDG2-WH" || self.SerialNumber != 1234 || len(self.LocalZones) != 0 {
		t.Fatalf("the bridge itself: %+v", self)
	}

	// These answers carry no firmware, and a device says so by an empty
	// version.
	if devices[0].Firmware != "" || devices[1].Firmware != "" {
		t.Fatalf("firmware out of an answer that has none: %q, %q", devices[0].Firmware, devices[1].Firmware)
	}

	dimmer := devices[1]
	if dimmer.Name != "Lights" || dimmer.DeviceType != "WallDimmer" || len(dimmer.LocalZones) != 1 ||
		dimmer.LocalZones[0].ID() != "1" || dimmer.AssociatedArea == nil || dimmer.AssociatedArea.Href != "/area/2" ||
		len(dimmer.FullyQualifiedName) != 2 || dimmer.FullyQualifiedName[0] != "Hallway" {
		t.Fatalf("the dimmer: %+v", dimmer)
	}

	pico := devices[7]
	if pico.DeviceType != "Pico3ButtonRaiseLower" || len(pico.LocalZones) != 0 ||
		len(pico.ButtonGroups) != 1 || pico.ButtonGroups[0].Href != "/buttongroup/2" {
		t.Fatalf("the Pico: %+v", pico)
	}

	buttons, err := opened.Buttons(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if first := buttons[0]; first.Href != "/button/101" || first.ID() != "101" || first.Name != "Button 1" ||
		first.Parent.Href != pico.ButtonGroups[0].Href {
		t.Fatalf("the Pico's first button: %+v", first)
	}

	areas, err := opened.Areas(t.Context())
	if err != nil || len(areas) == 0 || areas[0].Href != "/area/1" || areas[0].Name != "root" {
		t.Fatalf("areas: %v, %+v", err, areas)
	}

	scenes, err := opened.Scenes(t.Context())
	if err != nil || len(scenes) == 0 || scenes[0].Name != "scene 1" || !scenes[0].IsProgrammed ||
		scenes[0].ID() != "1" {
		t.Fatalf("scenes: %v, %+v", err, scenes)
	}
}

// TestCommands: each command as the bridge gets it, byte for byte the
// body the open implementations send.
func TestCommands(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)
	ctx := t.Context()

	for _, step := range []struct {
		send func() error
		url  string
		body string
	}{
		{
			func() error { return opened.SetLevel(ctx, "1", 50, 0) }, "/zone/1/commandprocessor",
			`{"Command":{"CommandType":"GoToLevel","Parameter":[{"Type":"Level","Value":50}]}}`,
		},
		{
			func() error { return opened.SetLevel(ctx, "1", 0, 3725*time.Second+900*time.Millisecond) },
			"/zone/1/commandprocessor",
			`{"Command":{"CommandType":"GoToDimmedLevel","DimmedLevelParameters":{"Level":0,"FadeTime":"01:02:05"}}}`,
		},
		{func() error { return opened.Raise(ctx, "6") }, "/zone/6/commandprocessor", `{"Command":{"CommandType":"Raise"}}`},
		{func() error { return opened.Lower(ctx, "6") }, "/zone/6/commandprocessor", `{"Command":{"CommandType":"Lower"}}`},
		{func() error { return opened.Stop(ctx, "6") }, "/zone/6/commandprocessor", `{"Command":{"CommandType":"Stop"}}`},
		{
			func() error { return opened.Press(ctx, "101") }, "/button/101/commandprocessor",
			`{"Command":{"CommandType":"PressAndRelease"}}`,
		},
		{
			func() error { return opened.Run(ctx, "1") }, "/virtualbutton/1/commandprocessor",
			`{"Command":{"CommandType":"PressAndRelease"}}`,
		},
	} {
		before := len(fake.Commands())

		if err := step.send(); err != nil {
			t.Fatalf("%s: %v", step.url, err)
		}

		commands := fake.Commands()
		if len(commands) != before+1 {
			t.Fatalf("%s: %d commands sent", step.url, len(commands)-before)
		}

		if got := commands[before]; got.CommuniqueType != leap.Create || got.URL != step.url ||
			string(got.Body) != step.body {
			t.Fatalf("got %s %s %s\nwant %s %s", got.CommuniqueType, got.URL, got.Body, step.url, step.body)
		}
	}

	sent := len(fake.Commands())

	for _, refused := range []error{
		opened.SetLevel(ctx, "1", 101, 0),
		opened.SetLevel(ctx, "1", -1, 0),
		opened.SetLevel(ctx, "1", 50, -time.Second),
	} {
		if !errors.Is(refused, caseta.ErrArgument) {
			t.Fatalf("a level out of range: %v", refused)
		}
	}

	for _, refused := range []error{opened.Raise(ctx, ""), opened.Press(ctx, "1/status"), opened.Run(ctx, "1?x")} {
		if !errors.Is(refused, caseta.ErrArgument) {
			t.Fatalf("an id that is not one: %v", refused)
		}
	}

	if len(fake.Commands()) != sent {
		t.Fatal("a refused command reached the bridge")
	}
}

// TestZonesAndEvents: a zone's level read, a zone with none, a zone the
// bridge lacks, and the events of zones and of a button.
func TestZonesAndEvents(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)
	ctx := t.Context()

	fake.Set("/zone/1/status", "OneZoneStatus", zoneStatus("1", 75))
	fake.Set("/zone/2/status", "OneZoneStatus", zoneStatus("2", nil))
	fake.Set("/zone/3/status", "OneZoneStatus", map[string]any{})

	status, err := opened.Zone(ctx, "1")
	if err != nil || status.Level == nil || *status.Level != 75 || status.Zone.ID() != "1" {
		t.Fatalf("zone 1: %v, %+v", err, status)
	}

	if status, err = opened.Zone(ctx, "2"); err != nil || status.Level != nil {
		t.Fatalf("a zone with no level: %v, %+v", err, status)
	}

	if _, err = opened.Zone(ctx, "3"); !errors.Is(err, caseta.ErrAnswer) {
		t.Fatalf("an answer with no status: %v", err)
	}

	if _, err = opened.Zone(ctx, "1/../2"); !errors.Is(err, caseta.ErrArgument) {
		t.Fatalf("a zone id that is a path: %v", err)
	}

	if _, err = opened.Zone(ctx, "9"); !errors.Is(err, caseta.ErrNotFound) || !errors.Is(err, caseta.ErrRefused) {
		t.Fatalf("a zone the bridge lacks: %v, want ErrNotFound, an ErrRefused", err)
	}

	if err = opened.Raise(ctx, "9"); err != nil {
		t.Fatalf("a command the stand-in takes: %v", err)
	}
}

// zoneStatus is a zone's status as a bridge writes it.
func zoneStatus(id string, level any) map[string]any {
	status := map[string]any{"Zone": map[string]any{"href": "/zone/" + id}}
	if level != nil {
		status["Level"] = level
	}

	return map[string]any{"ZoneStatus": status}
}

// press is a button's event as a bridge writes it.
func press(id, kind string) map[string]any {
	return map[string]any{"ButtonStatus": map[string]any{
		"Button": map[string]any{"href": "/button/" + id}, "ButtonEvent": map[string]any{"EventType": kind},
	}}
}

// watched is a Watch running until the test ends or stop is called: what
// it tells, and what it returns.
func watched(t *testing.T, opened *caseta.Bridge) (chan caseta.Event, chan error, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	events, ended := make(chan caseta.Event, 256), make(chan error, 1)

	go func() { ended <- opened.Watch(ctx, func(event caseta.Event) { events <- event }) }()

	return events, ended, cancel
}

// next is the next event a Watch told.
func next(t *testing.T, events chan caseta.Event) caseta.Event {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(wait):
		t.Fatalf("no event within %s", wait)

		return nil
	}
}

// standing sets what each zone of the recorded bridge is at, and reads
// past how things stand: eleven devices, three buttons, five zones.
func standing(t *testing.T, fake *leaptest.Bridge, opened *caseta.Bridge) (chan caseta.Event, chan error, func()) {
	t.Helper()

	for index, zone := range []string{"1", "2", "6", "3", "4"} {
		fake.Set("/zone/"+zone+"/status", "OneZoneStatus", zoneStatus(zone, 10*(index+1)))
	}

	events, ended, stop := watched(t, opened)

	var devices, buttons, zones []string

	for range 11 + 3 + 5 {
		switch told := next(t, events).(type) {
		case caseta.Device:
			devices = append(devices, told.DeviceType)
		case caseta.Button:
			buttons = append(buttons, told.ID())
		case caseta.ZoneStatus:
			zones = append(zones, fmt.Sprint(told.Zone.ID(), "=", *told.Level))
		default:
			t.Fatalf("told %T before anything happened", told)
		}
	}

	if len(devices) != 11 || devices[0] != "SmartBridge" || strings.Join(buttons, " ") != "101 102 103" ||
		strings.Join(zones, " ") != "1=10 2=20 6=30 3=40 4=50" {
		t.Fatalf("how things stand: devices %v, buttons %v, zones %v", devices, buttons, zones)
	}

	return events, ended, stop
}

// TestWatch: first how things stand, every device, every button and what
// every zone is at; then what happens, a zone's change and a button's
// press, and nothing that is neither. It ends with its context.
func TestWatch(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)
	events, ended, stop := standing(t, fake, opened)

	// A zone's change comes unasked, with no tag; what else comes that
	// way is not a zone's, and what is not in the shape of an event is
	// dropped, not told empty.
	fake.Unsolicited("/device/1/status", "OneDeviceStatus", zoneStatus("7", 1))
	fake.Unsolicited("/zone/6/status", "OneZoneStatus", map[string]any{})
	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", map[string]any{})
	fake.Unsolicited("/zone/6/status", "OneZoneStatus", zoneStatus("6", 31))

	if told, ok := next(t, events).(caseta.ZoneStatus); !ok || told.Zone.ID() != "6" || *told.Level != 31 {
		t.Fatalf("after a zone changed, told %+v", told)
	}

	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", press("101", "Press"))
	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", press("101", "Release"))

	for _, kind := range []string{"Press", "Release"} {
		if told, ok := next(t, events).(caseta.ButtonEvent); !ok || told.Button.ID() != "101" || told.Type != kind {
			t.Fatalf("after a button's %s, told %+v", kind, told)
		}
	}

	stop()

	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch, its context ended = %v", err)
		}
	case <-time.After(wait):
		t.Fatal("Watch outlived its context")
	}

	if len(events) != 0 {
		t.Fatalf("told %d events more than happened", len(events))
	}
}

// TestWatchTwice: several watch at once, each told everything; a button
// is subscribed to once however many watch, so its press is told once to
// each; and a second Watch reads how things stand again.
func TestWatchTwice(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)

	first, _, _ := standing(t, fake, opened)
	second, _, _ := standing(t, fake, opened)

	fake.Emit("/button/102/status/event", "OneButtonStatusEvent", press("102", "Press"))
	fake.Unsolicited("/zone/1/status", "OneZoneStatus", zoneStatus("1", 99))

	for name, events := range map[string]chan caseta.Event{"the first": first, "the second": second} {
		if told, ok := next(t, events).(caseta.ButtonEvent); !ok || told.Button.ID() != "102" {
			t.Fatalf("%s watcher was told %+v", name, told)
		}

		if told, ok := next(t, events).(caseta.ZoneStatus); !ok || *told.Level != 99 {
			t.Fatalf("%s watcher was told %+v", name, told)
		}
	}

	// Answered after the bridge's events were read: nothing more is on
	// its way.
	if _, err := opened.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(first)+len(second) != 0 {
		t.Fatalf("a press told more than once: %d and %d events left", len(first), len(second))
	}
}

// TestWatchASlowHandler: of a zone's changes that waited for the handler,
// the last is told, once; every press is told, in order.
func TestWatchASlowHandler(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)

	for _, zone := range []string{"1", "2", "6", "3", "4"} {
		fake.Set("/zone/"+zone+"/status", "OneZoneStatus", zoneStatus(zone, 0))
	}

	told, held, release := make(chan caseta.Event, 256), make(chan struct{}), make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = opened.Watch(ctx, func(event caseta.Event) {
			// The handler stops at the press of button 101, which comes
			// once everything stands, until the test lets it go.
			if pressed, ok := event.(caseta.ButtonEvent); ok && pressed.Button.ID() == "101" {
				close(held)
				<-release

				return
			}

			told <- event
		})
	}()

	for range 11 + 3 + 5 {
		next(t, told)
	}

	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", press("101", "Press"))

	<-held

	for level := range 5 {
		fake.Unsolicited("/zone/6/status", "OneZoneStatus", zoneStatus("6", 60+level))
	}

	fake.Emit("/button/103/status/event", "OneButtonStatusEvent", press("103", "Press"))
	fake.Unsolicited("/zone/1/status", "OneZoneStatus", zoneStatus("1", 11))
	fake.Emit("/button/103/status/event", "OneButtonStatusEvent", press("103", "Release"))

	// Answered once the bridge's events were read into the watcher.
	if _, err := opened.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}

	close(release)

	var heard []string

	for range 4 {
		switch event := next(t, told).(type) {
		case caseta.ZoneStatus:
			heard = append(heard, fmt.Sprint("zone ", event.Zone.ID(), " at ", *event.Level))
		case caseta.ButtonEvent:
			heard = append(heard, event.Type)
		}
	}

	if want := "zone 6 at 64, Press, zone 1 at 11, Release"; strings.Join(heard, ", ") != want {
		t.Fatalf("told %v\nwant %s", heard, want)
	}

	if len(told) != 0 {
		t.Fatalf("told %d events more: a zone's changes were not folded into its last", len(told))
	}
}

// TestWatchEnds: a Watch ends when the bridge goes, saying it is lost,
// and at once when the Bridge is over; a zone the bridge does not answer
// for ends it with that error, and the Bridge still stands.
func TestWatchEnds(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)

	// No zone has a status yet: the first read is refused.
	if err := opened.Watch(t.Context(), func(caseta.Event) {}); !errors.Is(err, caseta.ErrNotFound) {
		t.Fatalf("Watch of a bridge that lacks a zone's status = %v, want ErrNotFound", err)
	}

	if opened.Err() != nil {
		t.Fatalf("Err after a Watch that failed = %v", opened.Err())
	}

	_, ended, _ := standing(t, fake, opened)

	fake.Drop()

	select {
	case err := <-ended:
		if !errors.Is(err, caseta.ErrLost) || errors.Is(err, caseta.ErrClosed) {
			t.Fatalf("Watch after the bridge hung up = %v, want ErrLost", err)
		}
	case <-time.After(wait):
		t.Fatal("Watch outlived the connection")
	}

	select {
	case <-opened.Done():
	case <-time.After(wait):
		t.Fatal("the Bridge is not done after the bridge hung up")
	}

	if err := opened.Watch(t.Context(), func(caseta.Event) {}); !errors.Is(err, caseta.ErrLost) {
		t.Fatalf("Watch of a Bridge that is lost = %v", err)
	}

	again, err := caseta.Open(t.Context(), fake.Addr(), fake.Credentials())
	if err != nil {
		t.Fatalf("Open again after the bridge hung up: %v", err)
	}

	if err = again.Close(); err != nil || !errors.Is(again.Err(), caseta.ErrClosed) {
		t.Fatalf("Close = %v, Err = %v", err, again.Err())
	}

	if err = again.Watch(t.Context(), func(caseta.Event) {}); !errors.Is(err, caseta.ErrClosed) {
		t.Fatalf("Watch of a Bridge that is closed = %v", err)
	}
}

// TestWhoTheBridgeIs: a Bridge has the identity the bridge gives itself,
// its serial; credentials another bridge issued are ErrCredentials, and
// so are credentials that are none; a bridge that does not name itself
// is not opened.
func TestWhoTheBridgeIs(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)

	if opened.ID() != "1234" || opened.Err() != nil {
		t.Fatalf("ID = %q, Err = %v", opened.ID(), opened.Err())
	}

	other := leaptest.New()
	t.Cleanup(other.Close)

	if _, err := caseta.Open(t.Context(), fake.Addr(), other.Credentials()); !errors.Is(err, caseta.ErrCredentials) {
		t.Fatalf("Open under another bridge's credentials = %v, want ErrCredentials", err)
	}

	if _, err := caseta.Open(t.Context(), fake.Addr(), &leap.Credentials{}); !errors.Is(err, caseta.ErrCredentials) {
		t.Fatalf("Open under credentials that are none = %v, want ErrCredentials", err)
	}

	// A new stand-in says who it is by itself.
	theirs, err := caseta.Open(t.Context(), other.Addr(), other.Credentials())
	if err != nil || theirs.ID() != caseta.ID(strconv.Itoa(leaptest.Serial)) {
		t.Fatalf("Open of another bridge = %v, ID %q", err, theirs.ID())
	}

	_ = theirs.Close()

	for name, devices := range map[string][]map[string]any{
		"no device":                     {},
		"no serial of its own":          {{"href": "/device/1"}},
		"another device before its own": {{"href": "/device/2", "SerialNumber": 5}, leaptest.Itself()},
	} {
		other.Set(leaptest.DevicesURL, "MultipleDeviceDefinition", map[string]any{"Devices": devices})

		if _, err = caseta.Open(t.Context(), other.Addr(), other.Credentials()); !errors.Is(err, caseta.ErrAnswer) {
			t.Fatalf("Open of a bridge with %s = %v, want ErrAnswer", name, err)
		}
	}

	// An Open that fails after it connected leaves no connection behind.
	deadline := time.Now().Add(wait)
	for other.Connected() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections left by Opens that failed", other.Connected())
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestABridgeThatRefuses: each way a bridge says no is its own error. A
// bridge that forgot its clients refuses their credentials for good, and
// takes new ones; one that is away is no refusal of the credentials, and
// when it is back the same credentials open it; a request it answers
// outside the 200s is ErrRefused with its status, and the Bridge stands.
func TestABridgeThatRefuses(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)
	ctx := t.Context()
	held := fake.Credentials()

	fake.Refuse("/zone/1/commandprocessor", "500 InternalServerError")

	if err := opened.SetLevel(ctx, "1", 50, 0); !errors.Is(err, caseta.ErrRefused) ||
		errors.Is(err, caseta.ErrNotFound) || !strings.Contains(err.Error(), "500 InternalServerError") {
		t.Fatalf("a command the bridge fails = %v, want ErrRefused with its status", err)
	}

	fake.Refuse("/zone/1/commandprocessor", "404 NotFound")

	if err := opened.SetLevel(ctx, "1", 50, 0); !errors.Is(err, caseta.ErrNotFound) {
		t.Fatalf("a command for a zone the bridge lacks = %v, want ErrNotFound", err)
	}

	fake.Refuse("/zone/1/commandprocessor", "")

	if err := opened.SetLevel(ctx, "1", 50, 0); err != nil || opened.Err() != nil {
		t.Fatalf("the command again = %v, Err = %v", err, opened.Err())
	}

	fake.Away()

	select {
	case <-opened.Done():
	case <-time.After(wait):
		t.Fatal("the Bridge outlived a bridge that went away")
	}

	if err := opened.Err(); !errors.Is(err, caseta.ErrLost) {
		t.Fatalf("Err after the bridge went away = %v, want ErrLost", err)
	}

	if _, err := caseta.Open(ctx, fake.Addr(), held); err == nil || errors.Is(err, caseta.ErrCredentials) {
		t.Fatalf("Open of a bridge that is away = %v, want an error that is not the credentials'", err)
	}

	fake.Back()

	again, err := caseta.Open(ctx, fake.Addr(), held)
	if err != nil || again.ID() != opened.ID() {
		t.Fatalf("Open of a bridge that is back = %v", err)
	}

	fake.Revoke()

	// Refused, and not lost: the two call for opposite things.
	if _, err = caseta.Open(ctx, fake.Addr(), held); !errors.Is(err, caseta.ErrCredentials) ||
		errors.Is(err, caseta.ErrLost) {
		t.Fatalf("Open under credentials the bridge forgot = %v, want ErrCredentials and not ErrLost", err)
	}

	// The connection made before stands; new credentials are taken.
	if _, err = again.Ping(ctx); err != nil {
		t.Fatalf("a connection made before the bridge forgot its clients: %v", err)
	}

	anew, err := caseta.Open(ctx, fake.Addr(), fake.Credentials())
	if err != nil {
		t.Fatalf("Open under credentials issued since: %v", err)
	}

	_ = again.Close()
	_ = anew.Close()
}

// TestNotEveryAlert: a peer that fails the handshake for a reason of its
// own, and not for the certificate it was shown, is no refusal of the
// credentials: trying again may end otherwise.
func TestNotEveryAlert(t *testing.T) {
	t.Parallel()

	fake, _ := bridge(t)

	// A peer with no certificate to show just now: crypto/tls answers the
	// client with an alert, "internal error".
	var listening net.ListenConfig

	plain, err := listening.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	unready := tls.NewListener(plain, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return nil, errNotReady
		},
	})

	t.Cleanup(func() { _ = unready.Close() })

	go func() {
		for {
			conn, gone := unready.Accept()
			if gone != nil {
				return
			}

			if secured, ok := conn.(*tls.Conn); ok {
				_ = secured.HandshakeContext(t.Context())
			}

			_ = conn.Close()
		}
	}()

	_, err = caseta.Open(t.Context(), unready.Addr().String(), fake.Credentials())
	if err == nil || errors.Is(err, caseta.ErrCredentials) || !strings.Contains(err.Error(), "remote error: tls:") {
		t.Fatalf("Open of a peer that fails every handshake = %v, want an alert that is not the credentials'", err)
	}
}

// errNotReady is why the peer of TestNotEveryAlert fails its handshakes.
var errNotReady = errors.New("not ready")

// TestAButtonRefused: a Watch that a button's subscription fails ends
// with that error, and leaves the button to be asked for again: the next
// Watch is told its press.
func TestAButtonRefused(t *testing.T) {
	t.Parallel()

	fake, opened := bridge(t)

	for index, zone := range []string{"1", "2", "6", "3", "4"} {
		fake.Set("/zone/"+zone+"/status", "OneZoneStatus", zoneStatus(zone, index))
	}

	fake.Refuse("/button/102/status/event", "500 InternalServerError")

	if err := opened.Watch(t.Context(), func(caseta.Event) {}); !errors.Is(err, caseta.ErrRefused) {
		t.Fatalf("Watch of a bridge that refuses a button = %v, want ErrRefused", err)
	}

	fake.Refuse("/button/102/status/event", "")

	events, _, _ := watched(t, opened)

	for range 11 + 3 + 5 {
		next(t, events)
	}

	for _, button := range []string{"101", "102", "103"} {
		fake.Emit("/button/"+button+"/status/event", "OneButtonStatusEvent", press(button, "Press"))

		if told, ok := next(t, events).(caseta.ButtonEvent); !ok || told.Button.ID() != button {
			t.Fatalf("after button %s was pressed, told %+v", button, told)
		}
	}

	if _, err := opened.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(events) != 0 {
		t.Fatalf("a press told more than once: %d events left", len(events))
	}
}

// TestOpenTakesLeapsPort: a host with no port is dialled on LEAP's, a
// bare IPv6 address included.
func TestOpenTakesLeapsPort(t *testing.T) {
	t.Parallel()

	fake := leaptest.New()
	t.Cleanup(fake.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// Nothing listens on these: the error names what was dialled.
	for host, dialled := range map[string]string{
		"127.0.0.1":   "127.0.0.1:8081",
		"::1":         "[::1]:8081",
		"[::1]":       "[::1]:8081",
		"127.0.0.1:1": "127.0.0.1:1",
	} {
		_, err := caseta.Open(ctx, host, fake.Credentials())
		if err == nil || !strings.Contains(err.Error(), "the bridge at "+dialled) {
			t.Fatalf("%s: %v, want a dial of %s", host, err, dialled)
		}
	}
}

// TestARealBridge: the answers of a Smart Bridge, its names and serials
// replaced, read into the package's types. What a real bridge sends has
// far more keys than the types name, and gaps in its numbering.
func TestARealBridge(t *testing.T) {
	t.Parallel()

	fake := leaptest.New()
	t.Cleanup(fake.Close)

	for _, name := range []string{"device", "area", "button", "virtualbutton", "zone-1-status"} {
		raw, err := captured.ReadFile("testdata/smartbridge/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}

		var answer leap.Message
		if err = json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}

		fake.Set(answer.Header.URL, answer.Header.MessageBodyType, answer.Body)
	}

	opened, err := caseta.Open(t.Context(), fake.Addr(), fake.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	devices, err := opened.Devices(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]int{}
	zones := map[string]bool{}
	groups := map[string]string{}

	for _, device := range devices {
		kinds[device.DeviceType]++

		if device.Name == "" || device.ModelNumber == "" || device.SerialNumber == 0 ||
			len(device.FullyQualifiedName) == 0 {
			t.Fatalf("a device read short: %+v", device)
		}

		for _, zone := range device.LocalZones {
			zones[zone.ID()] = true
		}

		for _, group := range device.ButtonGroups {
			groups[group.Href] = device.Name
		}
	}

	if len(devices) != 21 || kinds["SmartBridge"] != 1 || kinds["WallDimmer"] != 14 || kinds["WallSwitch"] != 2 ||
		kinds["OutdoorPlugInSwitch"] != 1 || kinds["Pico3ButtonRaiseLower"] != 3 {
		t.Fatalf("devices: %d, %v", len(devices), kinds)
	}

	// Seventeen zones, numbered to nineteen: two numbers are not there.
	if len(zones) != 17 || !zones["1"] || !zones["19"] || zones["13"] || zones["16"] {
		t.Fatalf("zones: %v", zones)
	}

	if self := devices[0]; self.Name != "Smart Bridge" || self.ModelNumber != "L-BDG2-WH" ||
		self.AssociatedArea != nil || len(self.LocalZones) != 0 {
		t.Fatalf("the bridge itself: %+v", self)
	}

	// The bridge reports its own firmware in one place and every other
	// device's in another; each device of this bridge has one.
	if devices[0].Firmware != "08.28.13f000" || devices[1].Firmware != "001.019.000r000" {
		t.Fatalf("firmware: the bridge %q, the first dimmer %q", devices[0].Firmware, devices[1].Firmware)
	}

	for index := range devices {
		if devices[index].Firmware == "" {
			t.Fatalf("a device with no firmware: %+v", devices[index])
		}
	}

	var listing strings.Builder
	if err = caseta.WriteDevices(&listing, devices, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(listing.String(), "SmartBridge            L-BDG2-WH         20000001  08.28.13f000") ||
		!strings.Contains(
			listing.String(),
			"Area 11 Dimmer 1   WallDimmer             PD-6WCL-XX        20000002  001.019.000r000  1     \n",
		) {
		t.Fatalf("the listing:\n%s", listing.String())
	}

	buttons, err := opened.Buttons(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	perPico := map[string][]int{}

	for _, button := range buttons {
		pico, known := groups[button.Parent.Href]
		if !known {
			t.Fatalf("a button of no device: %+v", button)
		}

		perPico[pico] = append(perPico[pico], button.ButtonNumber)
	}

	if len(buttons) != 15 || len(perPico) != 3 {
		t.Fatalf("buttons: %d on %d Picos", len(buttons), len(perPico))
	}

	for pico, numbers := range perPico {
		if len(numbers) != 5 || numbers[0] != 0 || numbers[4] != 4 {
			t.Fatalf("%s: buttons %v", pico, numbers)
		}
	}

	areas, err := opened.Areas(t.Context())
	if err != nil || len(areas) != 18 || areas[0].Name != "Home" {
		t.Fatalf("areas: %v, %d", err, len(areas))
	}

	scenes, err := opened.Scenes(t.Context())
	if err != nil || len(scenes) != 100 {
		t.Fatalf("scenes: %v, %d", err, len(scenes))
	}

	for _, scene := range scenes {
		if scene.IsProgrammed {
			t.Fatalf("a programmed scene on a bridge that has none: %+v", scene)
		}
	}

	status, err := opened.Zone(t.Context(), "1")
	if err != nil || status.Level == nil || *status.Level != 100 || status.Zone.ID() != "1" {
		t.Fatalf("zone 1: %v, %+v", err, status)
	}
}

// failing is a writer that takes nothing.
type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestFirmwareShapes: which version is a device's when the answer could
// give two, a device that is not one, and a listing that cannot be
// written.
func TestFirmwareShapes(t *testing.T) {
	t.Parallel()

	part := func(version string) map[string]any {
		return map[string]any{"Type": "CCA", "OS": map[string]any{"Firmware": map[string]any{"DisplayName": version}}}
	}

	fake := leaptest.New()
	t.Cleanup(fake.Close)

	fake.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		{
			"href": "/device/1", "SerialNumber": 1,
			"FirmwareImage": map[string]any{"Contents": []any{part("first"), part("second")}},
		},
		{"href": "/device/2", "FirmwareImage": map[string]any{
			"Firmware": map[string]any{"DisplayName": "own"}, "Contents": []any{part("a part's")},
		}},
		{"href": "/device/3", "FirmwareImage": map[string]any{}},
		{"href": "/device/4", "FirmwareImage": nil},
	}})

	opened, err := caseta.Open(t.Context(), fake.Addr(), fake.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	devices, err := opened.Devices(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if devices[0].Firmware != "first" || devices[1].Firmware != "own" || devices[2].Firmware != "" ||
		devices[3].Firmware != "" || devices[3].ID() != "4" {
		t.Fatalf(
			"firmware: %q, %q, %q, %q",
			devices[0].Firmware,
			devices[1].Firmware,
			devices[2].Firmware,
			devices[3].Firmware,
		)
	}

	if err = caseta.WriteDevices(
		failing{},
		devices,
		nil,
	); err == nil ||
		!strings.Contains(err.Error(), "writing the devices") {
		t.Fatalf("a listing that cannot be written: %v", err)
	}

	// A device whose name is a number is not a device.
	fake.Set("/device", "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{
		{"href": "/device/1", "Name": 7},
	}})

	if _, err = opened.Devices(t.Context()); !errors.Is(err, caseta.ErrAnswer) {
		t.Fatalf("a device whose name is a number: %v", err)
	}
}

// TestFindZone: an id, a name, a name with its area, a name two rooms
// share, and a name nothing has.
func TestFindZone(t *testing.T) {
	t.Parallel()

	device := func(room, name, zone string) caseta.Device {
		return caseta.Device{
			Name: name, FullyQualifiedName: []string{room, name},
			LocalZones: []caseta.Ref{{Href: "/zone/" + zone}},
		}
	}

	devices := []caseta.Device{
		device("Kitchen", "Main Lights", "1"), device("Bedroom", "Main Lights", "5"),
		device("Hall", "Lamp", "7"),
		{Name: "Pico", FullyQualifiedName: []string{"Hall", "Pico"}},
		// A device named like another's zone: the id is what "5" means.
		device("Attic", "5", "9"),
	}

	for target, want := range map[string]caseta.Zone{
		"7":                   {ID: "7", Name: "Hall Lamp"},
		"lamp":                {ID: "7", Name: "Hall Lamp"},
		"hall LAMP":           {ID: "7", Name: "Hall Lamp"},
		"Bedroom Main Lights": {ID: "5", Name: "Bedroom Main Lights"},
		// An id wins over a name: zone 5, whatever is called "5".
		"5": {ID: "5", Name: "Bedroom Main Lights"},
	} {
		got, err := caseta.FindZone(devices, target)
		if err != nil || got != want {
			t.Fatalf("%q: %+v, %v", target, got, err)
		}
	}

	_, err := caseta.FindZone(devices, "Main Lights")
	if !errors.Is(err, caseta.ErrSeveralZones) ||
		!strings.Contains(err.Error(), `"Kitchen Main Lights" (zone 1), "Bedroom Main Lights" (zone 5)`) {
		t.Fatalf("a name two rooms share: %v", err)
	}

	// A device with no zone is not found by its name: a Pico drives none.
	for _, target := range []string{"Pico", "Garage", ""} {
		if _, err = caseta.FindZone(devices, target); !errors.Is(err, caseta.ErrNoZone) {
			t.Fatalf("%q: %v", target, err)
		}
	}
}
