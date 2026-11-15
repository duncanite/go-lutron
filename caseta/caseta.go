// Package caseta is a Caséta bridge over LEAP: what it holds, devices,
// areas, buttons and scenes; what its zones are at; the commands a zone,
// a button and a scene take; and the events it sends when a zone
// changes or a button is pressed. A Smart Bridge and a Smart Bridge Pro
// answer this way; a RadioRA 3 or HomeWorks processor arranges the same
// things under areas and control stations and is not covered.
//
// The shapes are the open implementations' recorded answers. Part of
// this has been run against one Smart Bridge and part has not:
// docs/protocol.md lists which.
package caseta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/duncanite/go-lutron/leap"
)

// The errors a Bridge hands back, beside the transport's.
var (
	// ErrLevel: a level outside 0 to 100, or a fade that is negative.
	ErrLevel = errors.New("not a level")
	// ErrID: a zone's, a button's or a scene's id that is empty or is
	// more than an id.
	ErrID = errors.New("not an object's id")
	// ErrAnswer: the bridge's answer lacks what was asked of it.
	ErrAnswer = errors.New("the bridge's answer is not in the expected shape")
)

// The resources and the command types, as the bridge names them.
const (
	urlPing    = "/server/1/status/ping"
	urlDevices = "/device"
	urlAreas   = "/area"
	urlButtons = "/button"
	urlScenes  = "/virtualbutton"

	commandLevel   = "GoToLevel"
	commandFade    = "GoToDimmedLevel"
	commandRaise   = "Raise"
	commandLower   = "Lower"
	commandStop    = "Stop"
	commandPress   = "PressAndRelease"
	parameterLevel = "Level"

	// bodyZoneStatus is the body type of one zone's status, read or sent
	// unasked.
	bodyZoneStatus = "OneZoneStatus"

	maxLevel = 100
)

// Ref is a reference to another object: its URL on the bridge.
type Ref struct {
	Href string `json:"href"`
}

// ID is the last element of the reference's URL, "7" for "/zone/7".
func (r Ref) ID() string {
	return r.Href[strings.LastIndex(r.Href, "/")+1:]
}

// Device is one device the bridge knows: itself, a dimmer or switch, a
// fan control, a shade, a Pico, a sensor.
type Device struct {
	Ref

	Name string `json:"Name"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// FullyQualifiedName is the area, then the name.
	FullyQualifiedName []string `json:"FullyQualifiedName"` //nolint:tagliatelle // the bridge's keys are PascalCase
	SerialNumber       int64    `json:"SerialNumber"`       //nolint:tagliatelle // the bridge's keys are PascalCase
	ModelNumber        string   `json:"ModelNumber"`        //nolint:tagliatelle // the bridge's keys are PascalCase
	// DeviceType is the bridge's word for the kind: "WallDimmer",
	// "Pico3ButtonRaiseLower", "SmartBridge".
	DeviceType string `json:"DeviceType"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// LocalZones is the loads the device drives; a zone is what takes a
	// level. A Pico or a sensor has none.
	LocalZones []Ref `json:"LocalZones"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// ButtonGroups is the device's buttons' groups; a Button names its
	// group as its Parent.
	ButtonGroups   []Ref `json:"ButtonGroups"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	AssociatedArea *Ref  `json:"AssociatedArea"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// Firmware is the version of the firmware the device runs, as the
	// bridge words it; empty when the bridge's answer does not say. It is
	// read out of the answer's FirmwareImage, which holds the bridge's own
	// version in one place and every other device's in another, a list of
	// its parts: the bridge's own wins when both are there, and of
	// several parts the first is taken. No device seen has either.
	Firmware string `json:"-"`
}

// UnmarshalJSON reads a device and, out of its FirmwareImage, the version
// it runs.
func (d *Device) UnmarshalJSON(data []byte) error {
	// plain has Device's fields and none of its methods, this one first.
	type plain Device

	var raw struct {
		*plain

		FirmwareImage *firmwareImage `json:"FirmwareImage"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	raw.plain = (*plain)(d)

	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: a device: %w", ErrAnswer, err)
	}

	switch image := raw.FirmwareImage; {
	case image == nil:
	case image.Firmware != nil:
		d.Firmware = image.Firmware.DisplayName
	case len(image.Contents) > 0:
		d.Firmware = image.Contents[0].OS.Firmware.DisplayName
	}

	return nil
}

// A device's firmware as the bridge reports it: the bridge's own under
// Firmware, every other device's as a list of contents, one per part of
// the device that runs firmware ("CCA" on every device seen).
type (
	firmwareImage struct {
		Firmware *named            `json:"Firmware"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Contents []firmwareContent `json:"Contents"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	firmwareContent struct {
		OS firmwareOS `json:"OS"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	firmwareOS struct {
		Firmware named `json:"Firmware"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	named struct {
		DisplayName string `json:"DisplayName"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
)

// Area is a room.
type Area struct {
	Ref

	Name string `json:"Name"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// Button is one button of a keypad or a Pico.
type Button struct {
	Ref

	Name         string `json:"Name"`         //nolint:tagliatelle // the bridge's keys are PascalCase
	ButtonNumber int    `json:"ButtonNumber"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// Parent is the button's group, one of a Device's ButtonGroups.
	Parent Ref `json:"Parent"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// Scene is a scene, which the bridge calls a virtual button.
type Scene struct {
	Ref

	Name string `json:"Name"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// IsProgrammed is false for the slots no scene was made in.
	IsProgrammed bool `json:"IsProgrammed"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// ZoneStatus is what a zone is at.
type ZoneStatus struct {
	Zone Ref `json:"Zone"` //nolint:tagliatelle // the bridge's keys are PascalCase
	// Level is 0 to 100; nil for a zone that has none, a fan or a
	// tilting blind, which report otherwise.
	Level *int `json:"Level"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// ButtonEvent is a button pressed or released.
type ButtonEvent struct {
	Button Ref
	// Type is the bridge's word: "Press" or "Release".
	Type string
}

// Bridge is a connection to a Caséta bridge.
type Bridge struct {
	conn *leap.Conn
}

// Open connects to the bridge at host under the credentials pairing
// gave; host may carry a port, and takes LEAP's when it does not.
func Open(ctx context.Context, host string, credentials *leap.Credentials) (*Bridge, error) {
	config, err := leap.TLSConfig(credentials)
	if err != nil {
		return nil, err
	}

	// No port: a name, an IPv4 address, or an IPv6 one, bare or bracketed.
	if _, _, err = net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), leap.Port)
	}

	conn, err := leap.Dial(ctx, host, config)
	if err != nil {
		return nil, err
	}

	return New(conn), nil
}

// New is a bridge over a LEAP connection already made, which Close
// closes.
func New(conn *leap.Conn) *Bridge {
	return &Bridge{conn: conn}
}

// Conn is the LEAP connection, for what this package has no word for.
func (b *Bridge) Conn() *leap.Conn { return b.conn }

// Close ends the connection.
func (b *Bridge) Close() error { return b.conn.Close() }

// Ping asks the bridge whether it is there, and returns the LEAP version
// it answers with.
func (b *Bridge) Ping(ctx context.Context) (float64, error) {
	var body struct {
		PingResponse pingResponse `json:"PingResponse"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if err := b.read(ctx, urlPing, &body); err != nil {
		return 0, err
	}

	return body.PingResponse.LEAPVersion, nil
}

type pingResponse struct {
	LEAPVersion float64 `json:"LEAPVersion"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// Devices is every device the bridge knows, itself first.
func (b *Bridge) Devices(ctx context.Context) ([]Device, error) {
	var body struct {
		Devices []Device `json:"Devices"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if err := b.read(ctx, urlDevices, &body); err != nil {
		return nil, err
	}

	return body.Devices, nil
}

// Areas is every room.
func (b *Bridge) Areas(ctx context.Context) ([]Area, error) {
	var body struct {
		Areas []Area `json:"Areas"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if err := b.read(ctx, urlAreas, &body); err != nil {
		return nil, err
	}

	return body.Areas, nil
}

// Buttons is every button of every keypad and Pico.
func (b *Bridge) Buttons(ctx context.Context) ([]Button, error) {
	var body struct {
		Buttons []Button `json:"Buttons"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if err := b.read(ctx, urlButtons, &body); err != nil {
		return nil, err
	}

	return body.Buttons, nil
}

// Scenes is every scene slot; the ones a scene was made in have
// IsProgrammed.
func (b *Bridge) Scenes(ctx context.Context) ([]Scene, error) {
	var body struct {
		VirtualButtons []Scene `json:"VirtualButtons"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if err := b.read(ctx, urlScenes, &body); err != nil {
		return nil, err
	}

	return body.VirtualButtons, nil
}

// Zone reads what the zone with this id is at.
func (b *Bridge) Zone(ctx context.Context, zone string) (*ZoneStatus, error) {
	var body zoneBody

	if err := checkID(zone); err != nil {
		return nil, err
	}

	if err := b.read(ctx, "/zone/"+zone+"/status", &body); err != nil {
		return nil, err
	}

	if body.ZoneStatus == nil {
		return nil, fmt.Errorf("%w: no status for zone %s", ErrAnswer, zone)
	}

	return body.ZoneStatus, nil
}

type zoneBody struct {
	ZoneStatus *ZoneStatus `json:"ZoneStatus"` //nolint:tagliatelle // the bridge's keys are PascalCase
}

// SetLevel sends the zone to a level, 0 to 100, at once when fade is
// zero and over fade otherwise; the bridge takes a fade in whole
// seconds, and fade is rounded down to them.
func (b *Bridge) SetLevel(ctx context.Context, zone string, level int, fade time.Duration) error {
	if level < 0 || level > maxLevel || fade < 0 {
		return fmt.Errorf("%w: %d over %s", ErrLevel, level, fade)
	}

	if fade == 0 {
		return b.command(ctx, "/zone/", zone, command{
			CommandType: commandLevel,
			Parameter:   []parameter{{Type: parameterLevel, Value: level}},
		})
	}

	return b.command(ctx, "/zone/", zone, command{
		CommandType: commandFade,
		DimmedLevelParameters: &dimmedLevel{
			Level: level,
			FadeTime: fmt.Sprintf(
				"%02d:%02d:%02d",
				fade/time.Hour, fade%time.Hour/time.Minute, fade%time.Minute/time.Second,
			),
		},
	})
}

// Raise starts raising the zone, as a held button does, until Stop.
func (b *Bridge) Raise(ctx context.Context, zone string) error {
	return b.command(ctx, "/zone/", zone, command{CommandType: commandRaise})
}

// Lower starts lowering the zone until Stop.
func (b *Bridge) Lower(ctx context.Context, zone string) error {
	return b.command(ctx, "/zone/", zone, command{CommandType: commandLower})
}

// Stop stops a zone that is being raised or lowered.
func (b *Bridge) Stop(ctx context.Context, zone string) error {
	return b.command(ctx, "/zone/", zone, command{CommandType: commandStop})
}

// Press presses and releases the button with this id.
func (b *Bridge) Press(ctx context.Context, button string) error {
	return b.command(ctx, "/button/", button, command{CommandType: commandPress})
}

// Run runs the scene with this id.
func (b *Bridge) Run(ctx context.Context, scene string) error {
	return b.command(ctx, "/virtualbutton/", scene, command{CommandType: commandPress})
}

// WatchZones calls handler with every zone's status as it changes, from
// now until the connection ends. A Caséta bridge is not subscribed to
// for these: it sends them with no tag. So this takes the connection's
// unsolicited handler, the only one it has, and a second call replaces
// the first's handler. The open implementations read each zone once
// before relying on these, and whether a bridge sends a zone's changes
// to a connection that never read it is not known: read the zones with
// Zone first. handler runs under the rules
// leap.Conn.Subscribe gives: promptly, and without a request of its own.
func (b *Bridge) WatchZones(handler func(ZoneStatus)) {
	b.conn.Unsolicited(func(message leap.Message) {
		if message.Header.MessageBodyType != bodyZoneStatus {
			return
		}

		var body zoneBody
		if message.Decode(&body) == nil && body.ZoneStatus != nil {
			handler(*body.ZoneStatus)
		}
	})
}

// WatchButton calls handler with every press and release of the button
// with this id, under the same rules.
func (b *Bridge) WatchButton(ctx context.Context, button string, handler func(ButtonEvent)) error {
	if err := checkID(button); err != nil {
		return err
	}

	_, err := b.conn.Subscribe(ctx, "/button/"+button+"/status/event", func(message leap.Message) {
		var body struct {
			ButtonStatus buttonStatus `json:"ButtonStatus"` //nolint:tagliatelle // the bridge's keys are PascalCase
		}

		if message.Decode(&body) == nil && body.ButtonStatus.ButtonEvent.EventType != "" {
			handler(ButtonEvent{Button: body.ButtonStatus.Button, Type: body.ButtonStatus.ButtonEvent.EventType})
		}
	})

	return err
}

type (
	buttonStatus struct {
		Button      Ref         `json:"Button"`      //nolint:tagliatelle // the bridge's keys are PascalCase
		ButtonEvent buttonEvent `json:"ButtonEvent"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	buttonEvent struct {
		EventType string `json:"EventType"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	command struct {
		CommandType           string       `json:"CommandType"`                     //nolint:tagliatelle // the bridge's keys are PascalCase
		Parameter             []parameter  `json:"Parameter,omitempty"`             //nolint:tagliatelle // the bridge's keys are PascalCase
		DimmedLevelParameters *dimmedLevel `json:"DimmedLevelParameters,omitempty"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	parameter struct {
		Type  string `json:"Type"`  //nolint:tagliatelle // the bridge's keys are PascalCase
		Value int    `json:"Value"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	dimmedLevel struct {
		Level    int    `json:"Level"`    //nolint:tagliatelle // the bridge's keys are PascalCase
		FadeTime string `json:"FadeTime"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	commandBody struct {
		Command command `json:"Command"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
)

func (b *Bridge) read(ctx context.Context, url string, into any) error {
	answer, err := b.conn.Request(ctx, leap.Read, url, nil)
	if err != nil {
		return err
	}

	return answer.Decode(into)
}

// command sends a command to the processor of the object of this kind
// and id.
func (b *Bridge) command(ctx context.Context, kind, id string, body command) error {
	if err := checkID(id); err != nil {
		return err
	}

	_, err := b.conn.Request(ctx, leap.Create, kind+id+"/commandprocessor", commandBody{Command: body})

	return err
}

// checkID refuses an id that would change which URL a request goes to.
func checkID(id string) error {
	if id == "" || strings.ContainsAny(id, "/?") {
		return fmt.Errorf("%w: %s", ErrID, strconv.Quote(id))
	}

	return nil
}
