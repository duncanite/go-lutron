package leaptest_test

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

// TestARefusalIsRead: a client the bridge forgot is told so, and its
// connection is not reset under it, whatever it sent meanwhile: a reset
// is what makes Windows discard the alert before the client reads it.
func TestARefusalIsRead(t *testing.T) {
	t.Parallel()

	bridge := leaptest.New()
	t.Cleanup(bridge.Close)

	config, err := leap.TLSConfig(bridge.Credentials())
	if err != nil {
		t.Fatal(err)
	}

	bridge.Revoke()

	// Under TLS 1.3 the client's handshake is over before the bridge has
	// judged its certificate.
	conn, err := (&tls.Dialer{Config: config}).DialContext(t.Context(), "tcp", bridge.Addr())
	if err != nil {
		t.Fatalf("the handshake of a client the bridge forgot: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	secured, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("the dialer gave a %T", conn)
	}

	if _, err = conn.Write([]byte(`{"CommuniqueType":"ReadRequest","Header":{"Url":"/device"}}` + "\r\n")); err != nil {
		t.Fatalf("the first request of a client the bridge forgot: %v", err)
	}

	_, err = conn.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "remote error: tls:") {
		t.Fatalf("what a client the bridge forgot reads = %v, want the bridge's alert", err)
	}

	// A peer that had closed answers what arrives with a reset, and a
	// write after the reset fails: this one is still reading.
	raw := secured.NetConn()

	for range 10 {
		if _, err = raw.Write([]byte("\r\n")); err != nil {
			t.Fatalf("writing to a bridge that refused: %v, want it to read until the client hangs up", err)
		}

		time.Sleep(10 * time.Millisecond)
	}
}
