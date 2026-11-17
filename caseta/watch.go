package caseta

import (
	"context"
	"errors"
	"fmt"

	"github.com/duncanite/go-lutron/leap"
)

// Event is something a Watch tells: a Device or a Button as the bridge
// describes it, a ZoneStatus, a ButtonEvent, Settled, and nothing else.
type Event interface{ event() }

// Settled is the mark between how things stood and what happens: a Watch
// tells it once, when every device, every button and what every zone is at
// has been told. What a consumer kept from before and was not told of by
// then, the bridge has no more.
type Settled struct{}

func (Device) event()      {}
func (Button) event()      {}
func (ZoneStatus) event()  {}
func (ButtonEvent) event() {}
func (Settled) event()     {}

// maxBehind is how many events a watcher may leave unread before Watch
// gives up on it.
const maxBehind = 1024

// errBehind is a watcher whose handler did not keep up.
var errBehind = errors.New("the watcher fell behind")

// Watch tells handle first how things stand: every device as the bridge
// describes it, every button, and what every zone is at; then Settled,
// once; and then everything that happens, a zone's change and a button's
// press and release, for as long as ctx lasts. Nothing falls between the
// two: what the bridge said while things were read is told before
// Settled, a zone's change in the place of what was read, and a button's
// press as it came.
// Events come one at a time, on the goroutine that called Watch; handle
// may take its time and may ask things of the Bridge. A button's presses
// are occurrences: every one is told, in the order they happened. Of a
// zone's changes that waited for handle, it is told the last, at the
// place the first of them had: a level applied twice does no harm, and a
// zone's last word may so come ahead of a press that happened before it.
// Several may watch at once, each told everything.
//
// A Caséta bridge is not known to say when a device is added, removed
// or renamed: Watch tells the devices as they were when it started, and
// calling it again reads them again.
//
// Watch returns ctx's error when ctx ends. Sooner, it returns ErrClosed
// when the Bridge was closed, an ErrLost when its connection ended by
// itself, and any other error when a read failed or handle fell more
// than a thousand events behind: the Bridge still stands, and may be
// watched again.
func (b *Bridge) Watch(ctx context.Context, handle func(Event)) error {
	watching := &watcher{latest: map[string]int{}, wake: make(chan struct{}, 1)}

	// Before anything is read, so that what the bridge says meanwhile is
	// kept and put in its place.
	b.join(watching)
	defer b.leave(watching)

	if err := b.standing(ctx, watching, handle); err != nil {
		return err
	}

	settled := false

	for {
		events, behind := watching.take()
		for _, event := range events {
			handle(event)
		}

		if behind {
			return fmt.Errorf("%w by more than %d events", errBehind, maxBehind)
		}

		// Every zone's status that was read is in what was just told.
		if !settled {
			settled = true

			handle(Settled{})
		}

		select {
		case <-watching.wake:
		case <-ctx.Done():
			return fmt.Errorf("watching the bridge: %w", context.Cause(ctx))
		case <-b.conn.Done():
			// What the bridge said before it went is told all the same.
			events, _ = watching.take()
			for _, event := range events {
				handle(event)
			}

			return fmt.Errorf("watching the bridge: %w", b.conn.Err())
		}
	}
}

// standing tells handle how things stand: the devices and the buttons as
// the bridge describes them, at once; and what each zone is at, through
// the watcher's queue, where it takes its place among what the bridge
// says meanwhile.
func (b *Bridge) standing(ctx context.Context, watching *watcher, handle func(Event)) error {
	devices, err := b.Devices(ctx)
	if err != nil {
		return err
	}

	buttons, err := b.Buttons(ctx)
	if err != nil {
		return err
	}

	if err = b.hear(ctx, buttons); err != nil {
		return err
	}

	for _, device := range devices {
		handle(device)
	}

	for _, button := range buttons {
		handle(button)
	}

	for _, device := range devices {
		for _, zone := range device.LocalZones {
			if err = b.readZone(ctx, watching, zone.ID()); err != nil {
				return err
			}
		}
	}

	return nil
}

// readZone reads what a zone is at and queues it for the watcher, at its
// place among what the bridge said meanwhile.
func (b *Bridge) readZone(ctx context.Context, watching *watcher, zone string) error {
	if err := checkID(zone); err != nil {
		return err
	}

	answer, err := b.conn.Request(ctx, leap.Read, "/zone/"+zone+"/status", nil)
	if err != nil {
		return absent(answer, err)
	}

	var body zoneBody
	if err = answer.Decode(&body); err != nil {
		return fmt.Errorf("%w: %w", ErrAnswer, err)
	}

	if body.ZoneStatus == nil {
		return fmt.Errorf("%w: no status for zone %s", ErrAnswer, zone)
	}

	watching.zone(*body.ZoneStatus, answer.Received())

	return nil
}

// hear subscribes to the events of the buttons not yet subscribed to. A
// subscription lasts as long as the connection: it is made once a
// button, whoever watches. One Watch at a time does this, and a button
// counts as heard only once the bridge granted it: a Watch that gives up
// while it waits leaves the button to the next.
func (b *Bridge) hear(ctx context.Context, buttons []Button) error {
	select {
	case b.hearing <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("watching the bridge: %w", context.Cause(ctx))
	case <-b.conn.Done():
		return fmt.Errorf("watching the bridge: %w", b.conn.Err())
	}

	defer func() { <-b.hearing }()

	for _, button := range buttons {
		number := button.ID()

		b.mu.Lock()
		_, heard := b.heard[number]
		b.mu.Unlock()

		if heard || checkID(number) != nil {
			continue
		}

		answer, err := b.conn.Subscribe(ctx, "/button/"+number+"/status/event", b.pressed)
		if err != nil {
			return absent(answer, err)
		}

		b.mu.Lock()
		b.heard[number] = struct{}{}
		b.mu.Unlock()
	}

	return nil
}

// pressed is a button's event, from its subscription: told to every
// watcher. It runs on the connection's reading goroutine.
func (b *Bridge) pressed(message leap.Message) {
	var body struct {
		ButtonStatus buttonStatus `json:"ButtonStatus"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}

	if message.Decode(&body) != nil || body.ButtonStatus.ButtonEvent.EventType == "" {
		return
	}

	event := ButtonEvent{Button: body.ButtonStatus.Button, Type: body.ButtonStatus.ButtonEvent.EventType}

	b.mu.Lock()
	defer b.mu.Unlock()

	for watching := range b.watchers {
		watching.occurred(event)
	}
}

// unsolicited is a message the bridge sent with no tag: a zone's change
// is one, which a Caséta bridge is not subscribed to for. It runs on the
// connection's reading goroutine.
func (b *Bridge) unsolicited(message leap.Message) {
	if message.Header.MessageBodyType != bodyZoneStatus {
		return
	}

	var body zoneBody
	if message.Decode(&body) != nil || body.ZoneStatus == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for watching := range b.watchers {
		watching.zone(*body.ZoneStatus, message.Received())
	}
}

func (b *Bridge) join(watching *watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.watchers[watching] = struct{}{}
}

func (b *Bridge) leave(watching *watcher) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.watchers, watching)
}
