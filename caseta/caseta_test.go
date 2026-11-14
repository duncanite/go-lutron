package caseta_test

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
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
		{"href": "/device/1", "FirmwareImage": map[string]any{"Contents": []any{part("first"), part("second")}}},
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
