// Package leap is the transport of LEAP, the protocol a Lutron bridge
// speaks to its own app on the LAN: JSON messages, one per line, over
// TLS with a client certificate the bridge signed at pairing. A message
// is a communique type, a header and a body; a request carries a tag the
// bridge echoes on its answer, and a subscription keeps its tag for every
// event the bridge sends afterwards. Nothing here knows what a zone or a
// button is: this is the wire, for the package above to give meaning to.
//
// Lutron publishes no description of LEAP. What is here is what the open
// implementations send and what their recorded answers hold. Pairing,
// requests, subscriptions and unsolicited messages have been run against
// one Smart Bridge; docs/protocol.md lists what was and was not.
package leap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The ports a bridge listens on.
const (
	// Port is LEAP itself, behind the certificate pairing gave.
	Port = "8081"
	// PairingPort is where a new client asks for that certificate.
	PairingPort = "8083"
)

// The communique types of the requests; an answer's is the same word
// with Response in place of Request.
const (
	Read      = "ReadRequest"
	Create    = "CreateRequest"
	Update    = "UpdateRequest"
	Subscribe = "SubscribeRequest"
)

// maxLine bounds one message; a whole project's devices is the largest
// the bridge sends, well under this.
const maxLine = 4 << 20

// maxPages bounds one answer: a bridge that always says more follows
// would otherwise be asked forever.
const maxPages = 256

// The errors a Conn hands back, to test with errors.Is.
var (
	// ErrClosed: the connection was closed by its owner, with Close.
	ErrClosed = errors.New("the connection to the bridge is closed")
	// ErrLost: the connection ended by itself: the bridge hung up, the
	// network went, or a line could not be written or read. The error
	// wraps what it died of. The Conn is over; Dial makes another.
	ErrLost = errors.New("the connection to the bridge is lost")
	// ErrStatus: the bridge answered a request with a status outside
	// the 200s; the error carries the status as the bridge wrote it.
	ErrStatus = errors.New("the bridge refused the request")
	// ErrMessage: a line from the bridge is not a LEAP message, or is
	// longer than a message may be, or an answer comes in more pages than
	// one may.
	ErrMessage = errors.New("not a LEAP message")
)

// Header is a message's header. A request sets ClientTag and URL; an
// answer adds StatusCode and names its body's type. An answer that is not
// the whole of what was asked for carries Paging, and the same request
// with that Paging in its header asks for what follows: Pages does.
type Header struct {
	ClientTag       string          `json:"ClientTag,omitempty"`       //nolint:tagliatelle // the bridge's keys are PascalCase
	URL             string          `json:"Url,omitempty"`             //nolint:tagliatelle // the bridge's keys are PascalCase
	StatusCode      string          `json:"StatusCode,omitempty"`      //nolint:tagliatelle // the bridge's keys are PascalCase
	MessageBodyType string          `json:"MessageBodyType,omitempty"` //nolint:tagliatelle // the bridge's keys are PascalCase
	Paging          json.RawMessage `json:"Paging,omitempty"`          //nolint:tagliatelle // the bridge's keys are PascalCase
}

// Message is one LEAP message, either way.
type Message struct {
	CommuniqueType string          `json:"CommuniqueType,omitempty"` //nolint:tagliatelle // the bridge's keys are PascalCase
	Header         Header          `json:"Header"`                   //nolint:tagliatelle // the bridge's keys are PascalCase
	Body           json.RawMessage `json:"Body,omitempty"`           //nolint:tagliatelle // the bridge's keys are PascalCase

	// received is the message's place among those its connection read.
	received uint64
}

// Received is the message's place among those its Conn read from the
// bridge, counted from one: of two messages, the one the bridge sent
// later has the greater. An answer comes back to its requester while the
// messages nobody asked for go to their handler on another goroutine, so
// their order of arrival in a program says nothing: this does. It is zero
// for a message that was not read from a bridge.
func (m *Message) Received() uint64 { return m.received }

// Status splits the header's status, "200 OK", into its code and text;
// the code is zero when the header has none or it does not start with a
// number.
func (m *Message) Status() (code int, text string) {
	number, rest, _ := strings.Cut(m.Header.StatusCode, " ")

	code, err := strconv.Atoi(number)
	if err != nil {
		return 0, m.Header.StatusCode
	}

	return code, rest
}

// OK reports whether the status is in the 200s.
func (m *Message) OK() bool {
	code, _ := m.Status()

	return code >= 200 && code < 300
}

// Decode unmarshals the body into into.
func (m *Message) Decode(into any) error {
	if err := json.Unmarshal(m.Body, into); err != nil {
		return fmt.Errorf("%w: the body of %s: %w", ErrMessage, m.Header.URL, err)
	}

	return nil
}

// Conn is one LEAP connection. Requests may be made from any goroutine,
// several at once; each waits for the answer carrying its tag.
type Conn struct {
	conn net.Conn
	next atomic.Uint64
	// count is how many messages were read; the reading goroutine's own.
	count uint64

	// writing serializes whole lines onto the connection.
	writing sync.Mutex

	mu          sync.Mutex
	pending     map[string]chan Message
	subscribed  map[string]func(Message)
	unsolicited func(Message)
	err         error
	done        chan struct{}
}

// Dial connects to the bridge at addr, host and port, under config, and
// returns the connection. TLSConfig makes the config from what pairing
// gave.
func Dial(ctx context.Context, addr string, config *tls.Config) (*Conn, error) {
	conn, err := DialTLS(ctx, addr, config)
	if err != nil {
		return nil, err
	}

	return New(conn), nil
}

// DialTLS makes a TLS connection to one of a bridge's ports, LEAP's or
// the pairing port, under config, and returns it once the handshake is
// done.
//
// No server name is sent, whatever the host in addr and whatever config
// names: a bridge's certificate names no host, and a bridge asked for one
// by name answers with another certificate, which its root does not sign.
// So config has to verify the bridge by itself, as TLSConfig's does: one
// that leaves the check of a name to crypto/tls is refused by it.
func DialTLS(ctx context.Context, addr string, config *tls.Config) (net.Conn, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: no TLS configuration for %s", ErrCredentials, addr)
	}

	var dialer net.Dialer

	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the bridge at %s: %w", addr, err)
	}

	// tls.Dialer would fill the name in from addr: the handshake is done
	// here, over a connection made without it.
	unnamed := config.Clone()
	unnamed.ServerName = ""

	secured := tls.Client(raw, unnamed)
	if err = secured.HandshakeContext(ctx); err != nil {
		_ = raw.Close()

		return nil, fmt.Errorf("connecting to the bridge at %s: %w", addr, err)
	}

	return secured, nil
}

// New speaks LEAP over a connection already made, and owns it from here:
// Close closes it.
func New(conn net.Conn) *Conn {
	leap := &Conn{
		conn:       conn,
		pending:    map[string]chan Message{},
		subscribed: map[string]func(Message){},
		done:       make(chan struct{}),
	}

	go leap.read()

	return leap
}

// Request sends a request of the communique type given for url, with body
// marshalled as its body when it is not nil, and returns the bridge's
// answer. An answer outside the 200s is returned with ErrStatus. The
// context bounds the wait for the answer and the write before it; a
// write it cuts short leaves half a line on the wire, so it ends the
// connection.
//
// The answer is one message. A bridge may answer a collection in pages,
// and this is then the first: Pages reads them all.
func (c *Conn) Request(ctx context.Context, communique, url string, body any) (*Message, error) {
	return c.exchange(ctx, communique, url, body, nil, nil)
}

// Pages is Request for an answer that may come in pages: it sends the
// request, and sends it again with the Paging of each answer for as long
// as an answer carries one, handing over every answer in the order the
// bridge gave them. An answer whose Paging is null or an empty object is
// the last. Each page is a Request, with what Request says of the context
// and of the connection.
//
// An error ends the pages, and is handed over with the answer it came
// with when it is ErrStatus, and with none otherwise. An answer in more
// than 256 pages is ErrMessage. Nothing more is asked of the bridge once
// the caller stops ranging.
func (c *Conn) Pages(ctx context.Context, communique, url string, body any) iter.Seq2[*Message, error] {
	return func(yield func(*Message, error) bool) {
		var paging json.RawMessage

		for range maxPages {
			answer, err := c.exchange(ctx, communique, url, body, paging, nil)
			if !yield(answer, err) || err != nil {
				return
			}

			paging = answer.Header.Paging
			if !follows(paging) {
				return
			}
		}

		yield(nil, fmt.Errorf("%w: %s %s: an answer in more than %d pages", ErrMessage, communique, url, maxPages))
	}
}

// follows reports whether an answer's Paging says that more follows.
func follows(paging json.RawMessage) bool {
	var written bytes.Buffer

	// What was not there at all does not compact.
	if json.Compact(&written, paging) != nil {
		return false
	}

	return written.String() != "null" && written.String() != "{}"
}

// Subscribe sends a subscription for url and, when the bridge accepts
// it, calls handler with every message the bridge sends for it
// afterwards. The answer itself, which for some URLs already holds the
// current state, is returned and not handed to handler.
//
// handler runs on the connection's reading goroutine: it must return
// promptly and must not call Request or Subscribe, which wait on that
// goroutine. A handler that needs either hands the message to a
// goroutine of its own.
func (c *Conn) Subscribe(ctx context.Context, url string, handler func(Message)) (*Message, error) {
	return c.exchange(ctx, Subscribe, url, nil, nil, handler)
}

// Unsolicited sets the handler for the messages the bridge sends with no
// tag, which no request asked for. It runs under the rules Subscribe
// gives for a handler.
func (c *Conn) Unsolicited(handler func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.unsolicited = handler
}

// Done is closed when the connection has ended; Err then says why.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Err is why the connection ended: ErrClosed after Close, an ErrLost with
// its cause when it ended by itself, nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// Close ends the connection. Requests still waiting return ErrClosed. A
// connection already lost stays lost.
func (c *Conn) Close() error {
	c.fail(ErrClosed)

	if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("closing the connection to the bridge: %w", err)
	}

	return nil
}

func (c *Conn) exchange(
	ctx context.Context,
	communique, url string,
	body any,
	paging json.RawMessage,
	handler func(Message),
) (*Message, error) {
	message := Message{CommuniqueType: communique}
	message.Header.URL = url
	message.Header.Paging = paging
	message.Header.ClientTag = "go-" + strconv.FormatUint(c.next.Add(1), 10)

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: the body for %s: %w", ErrMessage, url, err)
		}

		message.Body = raw
	}

	tag := message.Header.ClientTag
	answer := make(chan Message, 1)

	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()

		return nil, c.err
	}

	c.pending[tag] = answer

	// Before the request goes out, so no event can arrive ahead of its
	// handler. This module's caution: the open implementations register
	// after the answer, and no record shows an event that close behind.
	if handler != nil {
		c.subscribed[tag] = handler
	}
	c.mu.Unlock()

	if err := c.write(ctx, &message); err != nil {
		c.forget(tag)

		return nil, err
	}

	select {
	case got := <-answer:
		if !got.OK() {
			c.forget(tag)

			return &got, fmt.Errorf("%w: %s %s: %s", ErrStatus, communique, url, got.Header.StatusCode)
		}

		return &got, nil
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		c.forget(tag)

		return nil, fmt.Errorf("%s %s: %w", communique, url, context.Cause(ctx))
	}
}

// forget drops a tag: its answer is no longer waited for and its
// subscription, when it had one, is not kept.
func (c *Conn) forget(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.pending, tag)
	delete(c.subscribed, tag)
}

func (c *Conn) write(ctx context.Context, message *Message) error {
	line, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMessage, err)
	}

	c.writing.Lock()
	defer c.writing.Unlock()

	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("writing to the bridge: %w", cause)
	}

	// A bridge that stops reading would hold the write, and the lock
	// with it, for good: the context's end is a deadline already past.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.conn.SetWriteDeadline(time.Now())

		close(fired)
	})

	_, err = c.conn.Write(append(line, '\r', '\n'))

	// The deadline is this write's alone: when it was set, it is taken
	// off again before the next writer has the connection.
	if !stop() {
		<-fired

		_ = c.conn.SetWriteDeadline(time.Time{})
	}

	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}

		// Part of a line may be on the wire: nothing after it would parse.
		c.fail(fmt.Errorf("%w: writing to the bridge: %w", ErrLost, err))

		_ = c.conn.Close()

		// What ended the connection, which this write may only have found
		// ended: the bridge's last word, read a moment before, says more
		// than a write to what is closed.
		return c.Err()
	}

	return nil
}

// read is the reading goroutine: every line is a message, handed to the
// request waiting on its tag, to its subscription's handler, or to the
// unsolicited handler.
func (c *Conn) read() {
	reader := bufio.NewReaderSize(c.conn, maxLine)

	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				err = fmt.Errorf("%w: a line over %d bytes", ErrMessage, maxLine)
			}

			c.fail(fmt.Errorf("%w: reading from the bridge: %w", ErrLost, err))

			_ = c.conn.Close()

			return
		}

		// An empty line is no message and no fault.
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var message Message
		if err := json.Unmarshal(line, &message); err != nil {
			c.fail(fmt.Errorf("%w: %w: %w", ErrLost, ErrMessage, err))

			_ = c.conn.Close()

			return
		}

		// The reading goroutine alone counts, in the order of the wire.
		c.count++
		message.received = c.count

		c.dispatch(message)
	}
}

func (c *Conn) dispatch(message Message) {
	tag := message.Header.ClientTag

	c.mu.Lock()
	answer, waiting := c.pending[tag]
	delete(c.pending, tag)

	// A refused subscription is forgotten here, on the reading goroutine,
	// so a message under its tag right behind the refusal finds no
	// handler; the requester would forget it too late.
	if waiting && !message.OK() {
		delete(c.subscribed, tag)
	}

	handler := c.subscribed[tag]
	if tag == "" {
		handler = c.unsolicited
	}
	c.mu.Unlock()

	switch {
	case waiting:
		answer <- message
	case handler != nil:
		handler(message)
	}
}

// fail ends the connection for every waiter, once.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return
	}

	c.err = err
	close(c.done)
}
