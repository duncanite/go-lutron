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
	"sync"
	"time"

	"github.com/duncanite/go-lutron/leap"
)

// The errors a Bridge hands back, to test with errors.Is.
var (
	// ErrCredentials: the credentials are no credentials, the bridge does
	// not take them, or they do not take the bridge: the certificate
	// pairing gave is not one this bridge signed, or the bridge's own
	// does not chain to the root pairing gave. Trying again with the same
	// credentials ends the same way; pairing again makes new ones.
	//
	// The bridge's refusal is known by its saying so. One that hangs up
	// on a client without a word is an ErrLost, and so is one whose word
	// a reset of the connection overtook, which Windows discards unread.
	ErrCredentials = leap.ErrCredentials
	// ErrNotFound: the bridge has no zone, button or scene of that id.
	ErrNotFound = errors.New("the bridge has no such object")
	// ErrRefused: the bridge answered a request with a status outside the
	// 200s; the error carries the status as the bridge wrote it.
	ErrRefused = leap.ErrStatus
	// ErrAnswer: the bridge's answer lacks what was asked of it.
	ErrAnswer = errors.New("the bridge's answer is not in the expected shape")
	// ErrArgument: a level outside 0 to 100, a fade that is negative, an
	// id that is empty or is more than an id; nothing was sent.
	ErrArgument = errors.New("not a valid argument")
	// ErrLost: the connection ended by itself; the error wraps what it
	// died of. The Bridge is over, and Open makes another.
	ErrLost = leap.ErrLost
	// ErrClosed: the Bridge was closed by its owner.
	ErrClosed = leap.ErrClosed
)

// ID is a bridge's identity: its serial number, as the bridge writes it
// in its own entry of its devices. It is the same for as long as the
// bridge is the same bridge, whatever its address and whoever is paired
// with it.
type ID string

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

// Bridge is a connection to a Caséta bridge. Its methods may be called
// from several goroutines.
type Bridge struct {
	conn *leap.Conn
	id   ID

	// hearing is held by the one Watch that subscribes to buttons.
	hearing chan struct{}

	mu       sync.Mutex
	watchers map[*watcher]struct{}
	// heard is the buttons whose subscription the bridge granted.
	heard map[string]struct{}
}

// Open connects to the bridge at address under the credentials pairing
// gave, and reads who it is; address may carry a port, and takes LEAP's
// when it does not. It returns when the bridge has let the client in.
func Open(ctx context.Context, address string, credentials *leap.Credentials) (*Bridge, error) {
	config, err := leap.TLSConfig(credentials)
	if err != nil {
		return nil, err
	}

	// No port: a name, an IPv4 address, or an IPv6 one, bare or bracketed.
	if _, _, err = net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(strings.Trim(address, "[]"), leap.Port)
	}

	conn, err := leap.Dial(ctx, address, config)
	if err != nil {
		return nil, mismatch(err)
	}

	bridge, err := New(ctx, conn)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return bridge, nil
}

// New is a bridge over a LEAP connection already made, which it reads
// who the bridge is from, and which Close closes.
func New(ctx context.Context, conn *leap.Conn) (*Bridge, error) {
	bridge := &Bridge{
		conn: conn, hearing: make(chan struct{}, 1), watchers: map[*watcher]struct{}{}, heard: map[string]struct{}{},
	}

	// A bridge lists itself first among its devices.
	devices, err := bridge.Devices(ctx)
	if err != nil {
		return nil, mismatch(err)
	}

	// Its own entry is the first, and is /device/1: one that is another's
	// would give the bridge that device's serial for a name.
	if len(devices) == 0 || devices[0].ID() != "1" || devices[0].SerialNumber == 0 {
		return nil, fmt.Errorf("%w: the bridge does not say who it is", ErrAnswer)
	}

	bridge.id = ID(strconv.FormatInt(devices[0].SerialNumber, 10))

	conn.Unsolicited(bridge.unsolicited)

	return bridge, nil
}

// mismatch is err as ErrCredentials, and as nothing else, when it is the
// bridge refusing the client's certificate. The client refusing the
// bridge's is one already.
func mismatch(err error) error {
	if errors.Is(err, ErrCredentials) {
		return err
	}

	// A peer's refusal is an alert, which crypto/tls hands back as an
	// error of a type it does not export: its text is all there is to
	// tell it by. Under TLS 1.3 it comes with the first read, not with
	// the handshake, as a connection lost; the cause is said and not
	// wrapped, since credentials refused are no loss, and the two call
	// for opposite things.
	_, alert, alerted := strings.Cut(err.Error(), "remote error: tls: ")
	if alerted && (strings.Contains(alert, "certificate") || strings.Contains(alert, "access denied")) {
		return fmt.Errorf("%w: the bridge answered: %s", ErrCredentials, alert)
	}

	return err
}

// ID is who the bridge is.
func (b *Bridge) ID() ID { return b.id }

// Conn is the LEAP connection, for what this package has no word for. Its
// handler for what the bridge sends unasked is the Bridge's: setting
// another ends the zones' changes for every Watch.
func (b *Bridge) Conn() *leap.Conn { return b.conn }

// Close ends the connection.
func (b *Bridge) Close() error {
	return b.conn.Close()
}

// Done is closed when the Bridge is over: closed, or lost.
func (b *Bridge) Done() <-chan struct{} { return b.conn.Done() }

// Err is nil while the Bridge lives, ErrClosed after Close, and an
// ErrLost with its cause when the connection ended by itself.
func (b *Bridge) Err() error {
	return b.conn.Err()
}

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

// Devices is every device the bridge knows, itself first. A bridge lists
// itself: an answer with no body is no answer, where for the other
// collections it is none.
func (b *Bridge) Devices(ctx context.Context) ([]Device, error) {
	return gather[Device](ctx, b, urlDevices, "Devices", false)
}

// Areas is every room.
func (b *Bridge) Areas(ctx context.Context) ([]Area, error) {
	return gather[Area](ctx, b, urlAreas, "Areas", true)
}

// Buttons is every button of every keypad and Pico.
func (b *Bridge) Buttons(ctx context.Context) ([]Button, error) {
	return gather[Button](ctx, b, urlButtons, "Buttons", true)
}

// Scenes is every scene slot; the ones a scene was made in have
// IsProgrammed.
func (b *Bridge) Scenes(ctx context.Context) ([]Scene, error) {
	return gather[Scene](ctx, b, urlScenes, "VirtualButtons", true)
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
		return fmt.Errorf("%w: a level of %d over %s", ErrArgument, level, fade)
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
		return absent(answer, err)
	}

	return answer.Decode(into)
}

// gather reads a collection whole, in however many pages the bridge
// answers it: the list each page holds under key follows the one before.
//
// A bridge may answer a collection it has none of with no body at all,
// under a status in the 200s: with mayBeNone that is none, and not an
// answer that cannot be read.
func gather[T any](ctx context.Context, b *Bridge, url, key string, mayBeNone bool) ([]T, error) {
	all := listed[T]{key: key}

	for answer, err := range b.conn.Pages(ctx, leap.Read, url, nil) {
		if err != nil {
			return nil, absent(answer, err)
		}

		if mayBeNone && len(answer.Body) == 0 {
			continue
		}

		if err = answer.Decode(&all); err != nil {
			return nil, err
		}
	}

	return all.items, nil
}

// listed is a collection as its pages come: each body decoded into it
// adds the list under key to what the pages before gave.
type listed[T any] struct {
	key   string
	items []T
}

func (l *listed[T]) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err //nolint:wrapcheck // Decode, which this is for, says whose body it was
	}

	var page []T
	if held, ok := members[l.key]; ok {
		if err := json.Unmarshal(held, &page); err != nil {
			return err //nolint:wrapcheck // Decode, which this is for, says whose body it was
		}
	}

	l.items = append(l.items, page...)

	return nil
}

// absent is err as ErrNotFound when it is the bridge answering that it
// has no such object.
func absent(answer *leap.Message, err error) error {
	const notFound = 404

	if answer != nil && errors.Is(err, leap.ErrStatus) {
		if code, _ := answer.Status(); code == notFound {
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}

	return err
}

// command sends a command to the processor of the object of this kind
// and id.
func (b *Bridge) command(ctx context.Context, kind, id string, body command) error {
	if err := checkID(id); err != nil {
		return err
	}

	answer, err := b.conn.Request(ctx, leap.Create, kind+id+"/commandprocessor", commandBody{Command: body})

	return absent(answer, err)
}

// checkID refuses an id that would change which URL a request goes to.
func checkID(id string) error {
	if id == "" || strings.ContainsAny(id, "/?") {
		return fmt.Errorf("%w: the id %s", ErrArgument, strconv.Quote(id))
	}

	return nil
}
