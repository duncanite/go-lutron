package leap_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

// wait bounds every wait on the fake bridge; generous, for a loaded
// runner.
const wait = 30 * time.Second

func dial(t *testing.T, bridge *leaptest.Bridge, credentials *leap.Credentials) *leap.Conn {
	t.Helper()

	config, err := leap.TLSConfig(credentials)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := leap.Dial(t.Context(), bridge.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// TestPairing: nothing until the button, then credentials the LEAP port
// takes; a context that ends first ends the wait.
func TestPairing(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	pairing := func(ctx context.Context, ready func()) (*leap.Credentials, error) {
		conn, err := (&tls.Dialer{Config: bridge.Bootstrap()}).DialContext(ctx, "tcp", bridge.PairingAddr())
		if err != nil {
			t.Fatal(err)
		}

		return leap.Pair(ctx, conn, "the test", ready)
	}

	// No button: the context is what ends it.
	short, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	if _, err := pairing(short, nil); !errors.Is(err, leap.ErrPairing) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pairing with no button pressed: %v", err)
	}

	if len(bridge.Paired()) != 0 {
		t.Fatalf("the bridge signed without its button: %v", bridge.Paired())
	}

	garbling := leaptest.New()
	t.Cleanup(garbling.Close)
	garbling.Garble()

	raw, err := (&tls.Dialer{Config: garbling.Bootstrap()}).DialContext(t.Context(), "tcp", garbling.PairingAddr())
	if err != nil {
		t.Fatal(err)
	}

	if _, err = leap.Pair(t.Context(), raw, "the test", garbling.Press); !errors.Is(err, leap.ErrPairing) ||
		!errors.Is(err, leap.ErrCredentials) {
		t.Fatalf("a certificate that is not one: %v", err)
	}

	credentials, err := pairing(t.Context(), bridge.Press)
	if err != nil {
		t.Fatal(err)
	}

	if paired := bridge.Paired(); len(paired) != 1 || paired[0] != "the test" {
		t.Fatalf("paired: %v", paired)
	}

	conn := dial(t, bridge, credentials)

	answer, err := conn.Request(t.Context(), leap.Read, leaptest.PingURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	var ping map[string]map[string]float64

	if err = answer.Decode(&ping); err != nil || ping["PingResponse"]["LEAPVersion"] != leaptest.LEAPVersion {
		t.Fatalf("ping: %v, %+v", err, ping)
	}
}

// TestTheBridgeIsVerified: credentials for another bridge do not connect
// to this one, either way round.
func TestTheBridgeIsVerified(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	other := leaptest.New()
	t.Cleanup(other.Close)

	config, err := leap.TLSConfig(other.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	// The handshake may complete on our side before the bridge refuses
	// our certificate, so a request is what has to fail.
	conn, err := leap.Dial(t.Context(), bridge.Addr(), config)
	if err == nil {
		_, err = conn.Request(t.Context(), leap.Read, leaptest.PingURL, nil)
		_ = conn.Close()
	}

	if err == nil {
		t.Fatal("another bridge's credentials reached this one")
	}

	if !errors.Is(err, leap.ErrCredentials) {
		t.Fatalf("this bridge's certificate under another's root: %v", err)
	}

	mixed := bridge.Credentials()
	mixed.Root = []byte("not a certificate")

	if _, err = leap.TLSConfig(mixed); !errors.Is(err, leap.ErrCredentials) {
		t.Fatalf("a root that is not PEM: %v", err)
	}

	mixed = bridge.Credentials()
	mixed.Key = other.Credentials().Key

	if _, err = leap.TLSConfig(mixed); !errors.Is(err, leap.ErrCredentials) {
		t.Fatalf("a key that is not the certificate's: %v", err)
	}
}

// TestRequests: reads, a refusal, commands as the bridge gets them, and
// several requests at once each getting its own answer.
func TestRequests(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	bridge.Set("/zone/1/status", "OneZoneStatus", map[string]any{
		"ZoneStatus": map[string]any{"Level": 75, "Zone": map[string]any{"href": "/zone/1"}},
	})

	conn := dial(t, bridge, bridge.Credentials())

	answer, err := conn.Request(t.Context(), leap.Read, "/zone/1/status", nil)
	if err != nil {
		t.Fatal(err)
	}

	if code, text := answer.Status(); code != 200 || text != "OK" || answer.Header.MessageBodyType != "OneZoneStatus" ||
		answer.CommuniqueType != "ReadResponse" || answer.Header.URL != "/zone/1/status" {
		t.Fatalf("answer: %+v", answer)
	}

	refused, err := conn.Request(t.Context(), leap.Read, "/zone/9/status", nil)
	if !errors.Is(err, leap.ErrStatus) || refused == nil || refused.OK() {
		t.Fatalf("a resource the bridge lacks: %v, %+v", err, refused)
	}

	command := map[string]any{"Command": map[string]any{
		"CommandType": "GoToLevel", "Parameter": []map[string]any{{"Type": "Level", "Value": 50}},
	}}

	if _, err = conn.Request(t.Context(), leap.Create, "/zone/1/commandprocessor", command); err != nil {
		t.Fatal(err)
	}

	commands := bridge.Commands()
	if len(commands) != 1 || commands[0].CommuniqueType != leap.Create ||
		commands[0].URL != "/zone/1/commandprocessor" {
		t.Fatalf("commands: %+v", commands)
	}

	want := `{"Command":{"CommandType":"GoToLevel","Parameter":[{"Type":"Level","Value":50}]}}`
	if string(commands[0].Body) != want {
		t.Fatalf("the command's body: %s", commands[0].Body)
	}

	var group sync.WaitGroup

	failures := make(chan string, 20)

	for range 20 {
		group.Go(func() {
			one, err := conn.Request(t.Context(), leap.Read, "/zone/1/status", nil)
			if err != nil || one.Header.URL != "/zone/1/status" {
				failures <- "a concurrent read failed"
			}

			none, err := conn.Request(t.Context(), leap.Read, "/zone/9/status", nil)
			if !errors.Is(err, leap.ErrStatus) || none.Header.URL != "/zone/9/status" {
				failures <- "a concurrent refusal got another's answer"
			}
		})
	}

	group.Wait()
	close(failures)

	for failure := range failures {
		t.Fatal(failure)
	}
}

// TestEvents: a subscription's answer is returned, its events reach its
// handler and no other, an untagged message reaches the unsolicited
// handler, and a refused subscription gets no events.
func TestEvents(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	bridge.Set("/zone/status", "OneZoneStatus", map[string]any{"ZoneStatus": map[string]any{"Level": 0}})

	conn := dial(t, bridge, bridge.Credentials())

	zones := make(chan leap.Message, 4)
	buttons := make(chan leap.Message, 4)
	untagged := make(chan leap.Message, 4)

	conn.Unsolicited(func(message leap.Message) { untagged <- message })

	answer, err := conn.Subscribe(t.Context(), "/zone/status", func(message leap.Message) { zones <- message })
	if err != nil || answer.Header.MessageBodyType != "OneZoneStatus" {
		t.Fatalf("subscribing to the zones: %v, %+v", err, answer)
	}

	answer, err = conn.Subscribe(
		t.Context(),
		"/button/101/status/event",
		func(message leap.Message) { buttons <- message },
	)
	if err != nil {
		t.Fatalf("subscribing to a button: %v", err)
	}

	if code, _ := answer.Status(); code != 204 {
		t.Fatalf("subscribing to a button: %+v", answer)
	}

	bridge.Emit("/button/101/status/event", "OneButtonStatusEvent", map[string]any{
		"ButtonStatus": map[string]any{"ButtonEvent": map[string]any{"EventType": "Press"}},
	})
	bridge.Emit("/zone/status", "OneZoneStatus", map[string]any{"ZoneStatus": map[string]any{"Level": 100}})
	bridge.Unsolicited("/device/1/status", "OneDeviceStatus", map[string]any{})

	next := func(events chan leap.Message, what string) leap.Message {
		select {
		case message := <-events:
			return message
		case <-time.After(wait):
			t.Fatalf("no %s within %s", what, wait)

			return leap.Message{}
		}
	}

	if message := next(buttons, "button event"); message.Header.MessageBodyType != "OneButtonStatusEvent" {
		t.Fatalf("button event: %+v", message)
	}

	if message := next(zones, "zone event"); string(message.Body) != `{"ZoneStatus":{"Level":100}}` {
		t.Fatalf("zone event: %s", message.Body)
	}

	if message := next(untagged, "unsolicited message"); message.Header.URL != "/device/1/status" {
		t.Fatalf("unsolicited: %+v", message)
	}

	if len(zones) != 0 || len(buttons) != 0 || len(untagged) != 0 {
		t.Fatalf(
			"an event reached a handler twice: %d zones, %d buttons, %d untagged",
			len(zones),
			len(buttons),
			len(untagged),
		)
	}
}

// TestTheConnectionEnds: a bridge that hangs up, a context that ends,
// and Close each end what is waiting, and say which it was.
func TestTheConnectionEnds(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	conn := dial(t, bridge, bridge.Credentials())

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := conn.Request(ended, leap.Read, leaptest.PingURL, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a request under an ended context: %v", err)
	}

	// The connection survives a request that gave up.
	if _, err := conn.Request(t.Context(), leap.Read, leaptest.PingURL, nil); err != nil {
		t.Fatal(err)
	}

	bridge.Drop()

	select {
	case <-conn.Done():
	case <-time.After(wait):
		t.Fatalf("the connection did not end within %s of the bridge hanging up", wait)
	}

	if err := conn.Err(); !errors.Is(err, leap.ErrClosed) {
		t.Fatalf("after the bridge hung up: %v", err)
	}

	if _, err := conn.Request(t.Context(), leap.Read, leaptest.PingURL, nil); !errors.Is(err, leap.ErrClosed) {
		t.Fatalf("a request on an ended connection: %v", err)
	}

	again := dial(t, bridge, bridge.Credentials())

	if err := again.Close(); err != nil {
		t.Fatal(err)
	}

	if err := again.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}

	if err := again.Err(); !errors.Is(err, leap.ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

// piped is a Conn over a pipe, with the bridge's end in the test's
// hands: read reads one request, write sends lines as they are.
func piped(t *testing.T) (*leap.Conn, func() leap.Message, func(string)) {
	t.Helper()

	ours, theirs := net.Pipe()
	conn := leap.New(ours)
	reader := bufio.NewReader(theirs)

	t.Cleanup(func() {
		_ = conn.Close()
		_ = theirs.Close()
	})

	read := func() leap.Message {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Errorf("reading the request: %v", err)
		}

		var request leap.Message
		if err = json.Unmarshal(line, &request); err != nil {
			t.Errorf("the request: %v", err)
		}

		return request
	}

	// A write the client hangs up on fails, and that is the client's
	// answer: what a test checks is the client.
	write := func(lines string) { _, _ = theirs.Write([]byte(lines)) }

	return conn, read, write
}

func answerTo(request leap.Message, status string) string {
	return `{"CommuniqueType":"SubscribeResponse","Header":{"ClientTag":"` + request.Header.ClientTag +
		`","StatusCode":"` + status + `"}}` + "\r\n"
}

func eventFor(request leap.Message) string {
	return `{"CommuniqueType":"ReadResponse","Header":{"ClientTag":"` + request.Header.ClientTag +
		`","StatusCode":"200 OK"},"Body":{"n":1}}` + "\r\n"
}

// TestTheWire: the clauses the bridge's end has to be driven by hand
// for.
func TestTheWire(t *testing.T) {
	t.Parallel()

	t.Run("a request waiting when the connection ends", func(t *testing.T) {
		t.Parallel()

		conn, read, _ := piped(t)
		failed := make(chan error, 1)

		go func() {
			_, err := conn.Request(t.Context(), leap.Read, "/device", nil)
			failed <- err
		}()

		read()

		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}

		select {
		case err := <-failed:
			if !errors.Is(err, leap.ErrClosed) {
				t.Fatalf("the waiting request: %v", err)
			}
		case <-time.After(wait):
			t.Fatal("the waiting request did not return")
		}
	})

	t.Run("a refused subscription gets no events", func(t *testing.T) {
		t.Parallel()

		conn, read, write := piped(t)
		events := make(chan leap.Message, 2)

		go func() {
			request := read()
			write(answerTo(request, "400 BadRequest") + eventFor(request))
			// A read after them: answered once both are handled.
			write(answerTo(read(), "200 OK"))
		}()

		_, err := conn.Subscribe(t.Context(), "/zone/status", func(message leap.Message) { events <- message })
		if !errors.Is(err, leap.ErrStatus) {
			t.Fatalf("a refused subscription: %v", err)
		}

		if _, err = conn.Request(t.Context(), leap.Read, "/device", nil); err != nil {
			t.Fatal(err)
		}

		if len(events) != 0 {
			t.Fatal("a refused subscription's handler was called")
		}
	})

	t.Run("an event right behind the answer", func(t *testing.T) {
		t.Parallel()

		conn, read, write := piped(t)
		events := make(chan leap.Message, 2)

		go func() {
			request := read()
			write(answerTo(request, "200 OK") + eventFor(request))
		}()

		if _, err := conn.Subscribe(
			t.Context(),
			"/zone/status",
			func(message leap.Message) { events <- message },
		); err != nil {
			t.Fatal(err)
		}

		select {
		case <-events:
		case <-time.After(wait):
			t.Fatal("the event sent with the answer never reached the handler")
		}
	})

	t.Run("a line that is not a message", func(t *testing.T) {
		t.Parallel()

		conn, _, write := piped(t)

		go write("\r\n" + "not json\r\n")

		select {
		case <-conn.Done():
		case <-time.After(wait):
			t.Fatal("the connection outlived a line that is not JSON")
		}

		if err := conn.Err(); !errors.Is(err, leap.ErrMessage) || !errors.Is(err, leap.ErrClosed) {
			t.Fatalf("after a line that is not JSON: %v", err)
		}
	})

	t.Run("an empty line is skipped", func(t *testing.T) {
		t.Parallel()

		conn, read, write := piped(t)

		go func() {
			request := read()
			write("\r\n\n" + answerTo(request, "200 OK"))
		}()

		if _, err := conn.Request(t.Context(), leap.Read, "/device", nil); err != nil {
			t.Fatalf("a request answered after empty lines: %v", err)
		}
	})

	t.Run("a line longer than a message may be", func(t *testing.T) {
		t.Parallel()

		conn, _, write := piped(t)

		go write(strings.Repeat("x", 5<<20))

		select {
		case <-conn.Done():
		case <-time.After(wait):
			t.Fatal("the connection outlived an endless line")
		}

		if err := conn.Err(); !errors.Is(err, leap.ErrMessage) || !strings.Contains(err.Error(), "a line over") {
			t.Fatalf("after an endless line: %v", err)
		}
	})

	t.Run("a bridge that reads and never answers", func(t *testing.T) {
		t.Parallel()

		conn, read, _ := piped(t)

		go read()

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		if _, err := conn.Request(ctx, leap.Read, "/device", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a request never answered: %v", err)
		}

		// The request gave up; the connection did not end for it.
		if err := conn.Err(); err != nil {
			t.Fatalf("the connection after a request that gave up: %v", err)
		}
	})

	t.Run("a request under a context already ended", func(t *testing.T) {
		t.Parallel()

		ours, theirs := net.Pipe()
		conn := leap.New(ours)

		t.Cleanup(func() {
			_ = conn.Close()
			_ = theirs.Close()
		})

		ended, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := conn.Request(ended, leap.Read, "/device", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("a request under an ended context: %v", err)
		}

		// Nothing was written, so nothing is half on the wire and the
		// connection stands.
		if err := theirs.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		if read, _ := theirs.Read(make([]byte, 1)); read != 0 {
			t.Fatal("a request under an ended context reached the bridge")
		}

		if err := conn.Err(); err != nil {
			t.Fatalf("the connection after a request that was never sent: %v", err)
		}
	})

	t.Run("a bridge that stops reading", func(t *testing.T) {
		t.Parallel()

		conn, _, _ := piped(t)

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		failed := make(chan error, 1)

		go func() {
			_, err := conn.Request(ctx, leap.Read, "/device", nil)
			failed <- err
		}()

		select {
		case err := <-failed:
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, leap.ErrClosed) {
				t.Fatalf("a request the bridge never read: %v", err)
			}
		case <-time.After(wait):
			t.Fatal("a request the bridge never read outlived its context")
		}

		// Half a line may be on the wire: the connection is over.
		if _, err := conn.Request(t.Context(), leap.Read, "/device", nil); !errors.Is(err, leap.ErrClosed) {
			t.Fatalf("a request after a write cut short: %v", err)
		}
	})
}

// TestAClientIsNotTheBridge: the root signs the paired clients too, and
// a certificate issued to one, served, is refused.
func TestAClientIsNotTheBridge(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	impostor := bridge.Credentials()

	certificate, err := tls.X509KeyPair(impostor.Certificate, impostor.Key)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			// Drive the handshake, then hang up.
			_, _ = conn.Read(make([]byte, 1))
			_ = conn.Close()
		}
	}()

	config, err := leap.TLSConfig(bridge.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	conn, err := leap.Dial(t.Context(), listener.Addr().String(), config)
	if err == nil {
		_ = conn.Close()

		t.Fatal("a paired client's certificate passed for the bridge")
	}

	if !errors.Is(err, leap.ErrCredentials) {
		t.Fatalf("a paired client's certificate, served: %v", err)
	}
}
