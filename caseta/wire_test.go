package caseta_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/leap"
)

// peer is a bridge that decides the order of the wire, which the stand-in
// cannot: it answers over a pipe, line by line, as the test says. It
// holds itself, one dimmer on zone 1, and as many buttons as it is given.
type peer struct {
	conn    net.Conn
	buttons int

	mu sync.Mutex
	// changeFirst sends a zone's unasked change ahead of the answer to its
	// read; otherwise right behind it.
	changeFirst bool
	// hold keeps the answers to subscriptions back; asked is told of each
	// one held. read is told of every request, by its URL.
	hold  bool
	asked chan string
	read  chan string
	// tags is the tag each subscription came under, by its URL.
	tags map[string]string
}

func (p *peer) send(tag, url, kind, bodyType, body string) {
	header := fmt.Sprintf(`"MessageBodyType":%q,"StatusCode":"200 OK","Url":%q`, bodyType, url)
	if tag != "" {
		header += fmt.Sprintf(`,"ClientTag":%q`, tag)
	}

	_, _ = fmt.Fprintf(p.conn, `{"CommuniqueType":%q,"Header":{%s},"Body":%s}`+"\r\n", kind, header, body)
}

// level is zone 1's status at a level, as a body.
func level(zone string, at int) string {
	return fmt.Sprintf(`{"ZoneStatus":{"Level":%d,"Zone":{"href":"/zone/%s"}}}`, at, zone)
}

// pressed is a button's press, as a body.
func pressed(button int) string {
	return fmt.Sprintf(`{"ButtonStatus":{"Button":{"href":"/button/%d"},"ButtonEvent":{"EventType":"Press"}}}`, button)
}

// press sends a press of a button under its subscription's tag.
func (p *peer) press(button int) {
	url := fmt.Sprintf("/button/%d/status/event", button)

	p.mu.Lock()
	tag := p.tags[url]
	p.mu.Unlock()

	p.send(tag, url, "ReadResponse", "OneButtonStatusEvent", pressed(button))
}

func (p *peer) serve() {
	reader := bufio.NewReader(p.conn)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}

		var request leap.Message
		if json.Unmarshal(line, &request) != nil {
			return
		}

		p.answer(request.Header.ClientTag, request.Header.URL, request.CommuniqueType)
	}
}

func (p *peer) answer(tag, url, kind string) {
	select {
	case p.read <- url:
	default:
	}

	p.mu.Lock()
	changeFirst, hold := p.changeFirst, p.hold

	if kind == leap.Subscribe {
		p.tags[url] = tag
	}
	p.mu.Unlock()

	switch {
	case url == "/device":
		p.send(tag, url, "ReadResponse", "MultipleDeviceDefinition", `{"Devices":[`+
			`{"href":"/device/1","Name":"Smart Bridge","SerialNumber":1234,"DeviceType":"SmartBridge"},`+
			`{"href":"/device/2","Name":"Lamp","SerialNumber":5,"DeviceType":"WallDimmer","LocalZones":[{"href":"/zone/1"}]}]}`)
	case url == "/button":
		buttons := make([]string, 0, p.buttons)
		for index := range p.buttons {
			buttons = append(buttons, fmt.Sprintf(`{"href":"/button/%d","ButtonNumber":%d}`, 100+index, index))
		}

		p.send(tag, url, "ReadResponse", "MultipleButtonDefinition", `{"Buttons":[`+strings.Join(buttons, ",")+`]}`)
	case url == "/zone/1/status" && changeFirst:
		p.send("", url, "ReadResponse", "OneZoneStatus", level("1", 80))
		p.send(tag, url, "ReadResponse", "OneZoneStatus", level("1", 50))
		// Sent last: when it is told, everything before it was.
		p.send("", "/zone/9/status", "ReadResponse", "OneZoneStatus", level("9", 0))
	case url == "/zone/1/status":
		p.send(tag, url, "ReadResponse", "OneZoneStatus", level("1", 50))
		p.send("", url, "ReadResponse", "OneZoneStatus", level("1", 80))
		p.send("", "/zone/9/status", "ReadResponse", "OneZoneStatus", level("9", 0))
	case kind == leap.Subscribe && hold:
		p.asked <- url
	case kind == leap.Subscribe:
		p.send(tag, url, "SubscribeResponse", "OneButtonStatusEvent", `{}`)
	default:
		p.send(tag, url, "ReadResponse", "None", `{}`)
	}
}

// wired is a Bridge over a pipe to a peer.
func wired(t *testing.T, buttons int, changeFirst bool) (*caseta.Bridge, *peer) {
	t.Helper()

	ours, theirs := net.Pipe()
	bridge := &peer{
		conn: theirs, buttons: buttons, changeFirst: changeFirst, asked: make(chan string, 16),
		read: make(chan string, 4096), tags: map[string]string{},
	}

	go bridge.serve()

	opened, err := caseta.New(t.Context(), leap.New(ours))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = opened.Close() })

	return opened, bridge
}

// TestTheOrderOfTheWire: a zone's status read by Watch and that zone's
// change sent unasked reach the watcher from two goroutines; the last one
// told is the one the bridge sent last, whichever way round they came.
func TestTheOrderOfTheWire(t *testing.T) {
	t.Parallel()

	for _, changeFirst := range []bool{false, true} {
		want := 80
		if changeFirst {
			want = 50
		}

		for range 1000 {
			opened, _ := wired(t, 0, changeFirst)

			ctx, stop := context.WithCancel(t.Context())
			last := -1

			err := opened.Watch(ctx, func(event caseta.Event) {
				status, ok := event.(caseta.ZoneStatus)

				switch {
				case !ok || status.Level == nil:
				case status.Zone.ID() == "9":
					stop()
				default:
					last = *status.Level
				}
			})

			stop()

			if !errors.Is(err, context.Canceled) || last != want {
				t.Fatalf(
					"the change first %t: the zone's last word was %d, want %d (Watch: %v)",
					changeFirst,
					last,
					want,
					err,
				)
			}

			_ = opened.Close()
		}
	}
}

// TestAWatcherThatGivesUp: a Watch that gives up while its subscription
// to a button waits leaves the button to the next: a second watcher, who
// came to the buttons while the first still waited, asks for no
// subscription until the first is gone, then subscribes for itself, and
// is told the press.
func TestAWatcherThatGivesUp(t *testing.T) {
	t.Parallel()

	opened, bridge := wired(t, 1, false)

	bridge.mu.Lock()
	bridge.hold = true
	bridge.mu.Unlock()

	first, giveUp := context.WithCancel(t.Context())
	gone := make(chan error, 1)

	go func() { gone <- opened.Watch(first, func(caseta.Event) {}) }()

	// The first has asked for the button, and waits. Only that one is
	// held: what is asked from here on is answered.
	<-bridge.asked

	bridge.mu.Lock()
	bridge.hold = false
	bridge.mu.Unlock()

	for len(bridge.read) > 0 {
		<-bridge.read
	}

	presses := make(chan caseta.ButtonEvent, 4)

	go func() {
		_ = opened.Watch(t.Context(), func(event caseta.Event) {
			if press, ok := event.(caseta.ButtonEvent); ok {
				presses <- press
			}
		})
	}()

	// The second has read the buttons, its last step before it comes to
	// subscribe to them; a moment more, and it is there, with the first
	// still waiting. Nothing it does there can be seen from outside.
	for url := range bridge.read {
		if url == "/button" {
			break
		}
	}

	time.Sleep(100 * time.Millisecond)

	// One Watch at a time subscribes: with the first still waiting, the
	// second has asked the bridge for nothing of the button, which two
	// subscriptions on one connection would tell twice from then on.
	for len(bridge.read) > 0 {
		if url := <-bridge.read; strings.HasSuffix(url, "/status/event") {
			t.Fatalf("with the first watcher still waiting for the button, the second asked for %s", url)
		}
	}

	giveUp()

	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the watcher that gave up = %v", err)
	}

	deadline := time.Now().Add(wait)

	for {
		bridge.press(100)

		select {
		case press := <-presses:
			if press.Button.ID() != "100" {
				t.Fatalf("told %+v", press)
			}

			return
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("the second watcher was never told the button's press")
			}
		}
	}
}

// TestAWatcherFarBehind: a handler that leaves more than a thousand
// presses unread ends its Watch with an error that is neither the
// context's nor a loss, having been told the first thousand; the Bridge
// stands, and watching again tells how things stand.
func TestAWatcherFarBehind(t *testing.T) {
	t.Parallel()

	opened, bridge := wired(t, 1, false)

	stood, release, ended := make(chan struct{}, 4), make(chan struct{}), make(chan error, 1)
	told := 0

	go func() {
		ended <- opened.Watch(t.Context(), func(event caseta.Event) {
			switch status := event.(type) {
			case caseta.ZoneStatus:
				if status.Zone.ID() == "9" {
					stood <- struct{}{}

					<-release
				}
			case caseta.ButtonEvent:
				told++
			}
		})
	}()

	<-stood

	const sent = 1500

	for range sent {
		bridge.press(100)
	}

	// Answered once the presses before it were read.
	if _, err := opened.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}

	close(release)

	select {
	case err := <-ended:
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, caseta.ErrLost) ||
			!strings.Contains(err.Error(), "fell behind") {
			t.Fatalf("Watch, its handler %d presses behind = %v", sent, err)
		}
	case <-time.After(wait):
		t.Fatal("a Watch far behind went on")
	}

	if told != 1024 || opened.Err() != nil {
		t.Fatalf("told %d presses of %d, want the first 1024; the Bridge's Err: %v", told, sent, opened.Err())
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	err := opened.Watch(ctx, func(event caseta.Event) {
		if status, ok := event.(caseta.ZoneStatus); ok && status.Zone.ID() == "9" {
			stop()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watching again = %v", err)
	}
}

// TestAPressThenGone: a press read while the handler was busy is told
// even when the bridge hangs up right behind it.
func TestAPressThenGone(t *testing.T) {
	t.Parallel()

	for range 100 {
		opened, bridge := wired(t, 1, false)

		stood, release, ended := make(chan struct{}, 4), make(chan struct{}), make(chan error, 1)
		told := false

		go func() {
			ended <- opened.Watch(t.Context(), func(event caseta.Event) {
				switch status := event.(type) {
				case caseta.ZoneStatus:
					if status.Zone.ID() == "9" {
						stood <- struct{}{}

						<-release
					}
				case caseta.ButtonEvent:
					told = true
				}
			})
		}()

		<-stood

		bridge.press(100)

		_ = bridge.conn.Close()

		<-opened.Done()
		close(release)

		if err := <-ended; !errors.Is(err, caseta.ErrLost) || !told {
			t.Fatalf("a press, then the bridge gone: told %t, Watch = %v", told, err)
		}
	}
}
