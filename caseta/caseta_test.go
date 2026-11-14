package caseta_test

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

//go:embed testdata/*.json
var recorded embed.FS

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

// TestWhatTheBridgeHolds: the recorded answers of a Caseta bridge, read
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
		if !errors.Is(refused, caseta.ErrLevel) {
			t.Fatalf("a level out of range: %v", refused)
		}
	}

	for _, refused := range []error{opened.Raise(ctx, ""), opened.Press(ctx, "1/status"), opened.Run(ctx, "1?x")} {
		if !errors.Is(refused, caseta.ErrID) {
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

	zone := func(id string, level any) map[string]any {
		status := map[string]any{"Zone": map[string]any{"href": "/zone/" + id}}
		if level != nil {
			status["Level"] = level
		}

		return map[string]any{"ZoneStatus": status}
	}

	fake.Set("/zone/1/status", "OneZoneStatus", zone("1", 75))
	fake.Set("/zone/2/status", "OneZoneStatus", zone("2", nil))
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

	if _, err = opened.Zone(ctx, "1/../2"); !errors.Is(err, caseta.ErrID) {
		t.Fatalf("a zone id that is a path: %v", err)
	}

	if err = opened.WatchButton(ctx, "", func(caseta.ButtonEvent) {}); !errors.Is(err, caseta.ErrID) {
		t.Fatalf("a button with no id: %v", err)
	}

	if _, err = opened.Zone(ctx, "9"); !errors.Is(err, leap.ErrStatus) {
		t.Fatalf("a zone the bridge lacks: %v", err)
	}

	zones := make(chan caseta.ZoneStatus, 4)
	presses := make(chan caseta.ButtonEvent, 4)

	opened.WatchZones(func(changed caseta.ZoneStatus) { zones <- changed })

	if err = opened.WatchButton(ctx, "101", func(event caseta.ButtonEvent) { presses <- event }); err != nil {
		t.Fatal(err)
	}

	// A zone's change comes unasked, with no tag; what else comes that
	// way is not a zone's.
	fake.Unsolicited("/device/1/status", "OneDeviceStatus", zone("7", 1))
	fake.Unsolicited("/zone/6/status", "OneZoneStatus", zone("6", 30))
	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", map[string]any{
		"ButtonStatus": map[string]any{
			"Button": map[string]any{"href": "/button/101"}, "ButtonEvent": map[string]any{"EventType": "Press"},
		},
	})
	// Not in the shape of an event: dropped, not handed on empty.
	fake.Emit("/button/101/status/event", "OneButtonStatusEvent", map[string]any{})
	fake.Unsolicited("/zone/6/status", "OneZoneStatus", map[string]any{})

	select {
	case changed := <-zones:
		if changed.Zone.ID() != "6" || changed.Level == nil || *changed.Level != 30 {
			t.Fatalf("zone event: %+v", changed)
		}
	case <-time.After(wait):
		t.Fatalf("no zone event within %s", wait)
	}

	select {
	case event := <-presses:
		if event.Button.ID() != "101" || event.Type != "Press" {
			t.Fatalf("button event: %+v", event)
		}
	case <-time.After(wait):
		t.Fatalf("no button event within %s", wait)
	}

	// A read after the events: the reading goroutine has handled the
	// two malformed ones by the time it answers.
	if _, err = opened.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	if len(zones) != 0 || len(presses) != 0 {
		t.Fatalf("a malformed event was handed on: %d zones, %d presses", len(zones), len(presses))
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
