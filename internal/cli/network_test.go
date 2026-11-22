package cli_test

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mdns "github.com/forkcloser/go-mdns"
	"github.com/forkcloser/go-mdns/mdnstest"
	"github.com/forkcloser/go-mdns/transport"

	"github.com/duncanite/go-lutron/internal/cli"
	"github.com/duncanite/go-lutron/leap"
	"github.com/duncanite/go-lutron/leap/leaptest"
)

// wire is the one wire of the test networks.
const wire = "home"

// What the bridges of the test networks say they are: the test bridge's
// own serial in its host's name, and another's.
const (
	hostname = "Lutron-000003e9"
	itself   = "1001"
	another  = "Lutron-0123abcd"
	other    = "19114957"
)

// ground is this host's loopback addresses, where LEAP's own ports are
// one bridge's at a time: a test that stands a bridge there holds it.
//
//nolint:gochecknoglobals // the ports are the host's, one to all the tests of this package
var ground sync.Mutex

// held takes the loopback's LEAP ports for the test.
func held(t *testing.T) {
	t.Helper()

	ground.Lock()
	t.Cleanup(ground.Unlock)
}

// spare skips the test when LEAP's ports are not to be had at host: a
// host that lacks the address, or has another program on a port.
func spare(t *testing.T, host string) {
	t.Helper()

	for _, port := range []string{leap.Port, leap.PairingPort} {
		probe, err := new(net.ListenConfig).Listen(t.Context(), "tcp", net.JoinHostPort(host, port))
		if err != nil {
			t.Skipf("LEAP's port %s is not to be had at %s on this host: %v", port, host, err)
		}

		_ = probe.Close()
	}
}

// standing is a test bridge on LEAP's own ports at a loopback address,
// where the address a search finds leads. The test holds the ground.
func standing(t *testing.T, host string) *leaptest.Bridge {
	t.Helper()

	spare(t, host)

	bridge, err := leaptest.At(host)
	if err != nil {
		t.Fatalf("a bridge at %s: %v", host, err)
	}

	t.Cleanup(bridge.Close)

	return bridge
}

// announced puts something on the network that announces itself as a
// Lutron system does: under the name every bridge takes, on the port a
// bridge announces, which is not LEAP's, with its kind in its text and
// its host's name, at the addresses given.
func announced(t *testing.T, network *mdnstest.Network, name, system string, at ...string) {
	t.Helper()

	addrs := make([]netip.Addr, 0, len(at))
	for _, address := range at {
		addrs = append(addrs, netip.MustParseAddr(address))
	}

	peer, err := mdns.Open(t.Context(), mdns.Config{
		Network: network.Host(mdnstest.Port{Interface: "eth0", Wire: wire}),
	})
	if err != nil {
		t.Fatalf("opening the network of %s: %v", name, err)
	}

	t.Cleanup(func() { _ = peer.Close() })

	const announcedPort = 22

	if _, err = peer.Announce(t.Context(), mdns.Service{
		Name: "Lutron Status", Type: "_lutron._tcp", Port: announcedPort,
		Text: mdns.TXT{{Key: "SYSTYPE", Value: []byte(system)}},
		Host: name, Addrs: addrs,
	}); err != nil {
		t.Fatalf("announcing %s: %v", name, err)
	}
}

// here is the command's own host on the network: on the bridges' wire by
// en0, and by en1 on one where nothing is.
func here(network *mdnstest.Network) transport.Network {
	return network.Host(
		mdnstest.Port{Interface: "en0", Wire: wire},
		mdnstest.Port{Interface: "en1", Wire: "elsewhere"},
	)
}

// searching is the command with no host given, on a network; all it
// said, and its status.
func searching(t *testing.T, network transport.Network, args ...string) (string, int) {
	t.Helper()

	var out syncBuffer

	status := cli.Run(t.Context(), cli.Environment{Args: args, Out: &out, Err: &out, Network: network})

	return out.String(), status
}

// TestDiscover: the bridges that announce themselves are printed, each
// with who it says it is and where LEAP is spoken there, in the order of
// their ids, with no credentials asked for; a Lutron system that is no
// Caséta bridge is not one of them, and a network with none says so.
func TestDiscover(t *testing.T) {
	t.Parallel()

	var network mdnstest.Network

	announced(t, &network, hostname, "SmartBridge", "192.0.2.10", "192.0.2.11")
	announced(t, &network, another, "SmartBridge", "192.0.2.20")
	announced(t, &network, "Lutron-0000beef", "Processor", "192.0.2.30")

	out, status := searching(t, here(&network), "--wait", "2s", "discover")
	if want := strings.Join([]string{
		"BRIDGE AT",
		itself + " 192.0.2.10:8081, 192.0.2.11:8081",
		other + " 192.0.2.20:8081",
	}, "\n"); status != cli.ExitOK || rows(out) != want {
		t.Errorf("discover exits %d, saying\n%s\nwant\n%s", status, rows(out), want)
	}

	out, status = searching(t, here(&network), "--wait", "500ms", "--interface", "en1", "discover")
	if status != cli.ExitOK || out != "No bridge answered.\n" {
		t.Errorf("discover where nothing is exits %d, saying %q", status, out)
	}

	out, status = searching(t, here(&network), "--wait", "0s", "discover")
	if status != cli.ExitError || !strings.Contains(out, "--wait wants a time above zero") {
		t.Errorf("discover with no time to wait exits %d, saying %q", status, out)
	}
}

// rows is the output as lines of fields, the spacing of a table aside.
func rows(out string) string {
	var lines []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}

	return strings.Join(lines, "\n")
}

// TestSearch: with no host given, a command finds the bridge on the
// network and speaks to it under the credentials; where no bridge
// answers it says so, and names the flag that needs no search; and
// credentials the bridge found takes no more are said of that bridge.
func TestSearch(t *testing.T) {
	t.Parallel()
	held(t)

	bridge := standing(t, "127.0.0.1")
	dir := furnished(t, bridge)

	var network mdnstest.Network

	announced(t, &network, hostname, "SmartBridge", "127.0.0.1")

	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"ping"}, "LEAP 1.115"},
		{[]string{"scenes"}, "1\tMovie\n"},
		{[]string{"level", "lights"}, "zone 1 is at 75"},
	} {
		out, status := searching(t, here(&network), append([]string{"--credentials", dir}, step.args...)...)
		if status != cli.ExitOK || !strings.Contains(out, step.want) {
			t.Errorf("%v with no host exits %d, saying %q; want %q", step.args, status, out, step.want)
		}
	}

	out, status := searching(t, here(&network), "--credentials", dir, "--wait", "500ms", "--interface", "en1", "ping")
	if status != cli.ExitError || !strings.Contains(out, "no bridge answered the search within 500ms") ||
		!strings.Contains(out, "--host") {
		t.Errorf("ping where no bridge is exits %d, saying %q", status, out)
	}

	out, status = searching(t, here(&network), "--wait", "500ms", "ping")
	if status != cli.ExitError || !strings.Contains(out, "--credentials") {
		t.Errorf("ping with no credentials exits %d, saying %q", status, out)
	}

	bridge.Revoke()

	out, status = searching(t, here(&network), "--credentials", dir, "--wait", "1s", "ping")
	if status != cli.ExitError || !strings.Contains(out, "the bridge "+itself+", found on this network") {
		t.Errorf("ping under credentials the bridge found takes no more exits %d, saying %q", status, out)
	}
}

// TestSearchPassesOverWhatIsNoBridge: an announcement is a claim. What
// announces itself as a bridge, takes a connection and never answers is
// tried, and takes none of the time of the bridge that takes the
// credentials, which is tried beside it and spoken to; alone, it is named
// with what came of it.
func TestSearchPassesOverWhatIsNoBridge(t *testing.T) {
	t.Parallel()
	held(t)

	const elsewhere = "::1"

	spare(t, elsewhere)

	bridge := standing(t, "127.0.0.1")
	dir := furnished(t, bridge)

	pretender, err := new(net.ListenConfig).Listen(t.Context(), "tcp", net.JoinHostPort(elsewhere, leap.Port))
	if err != nil {
		t.Fatalf("a pretender at %s: %v", elsewhere, err)
	}

	t.Cleanup(func() { _ = pretender.Close() })

	tried := make(chan struct{}, 1)

	go func() {
		for {
			conn, err := pretender.Accept()
			if err != nil {
				return
			}

			// Held open, and never answered.
			t.Cleanup(func() { _ = conn.Close() })

			select {
			case tried <- struct{}{}:
			default:
			}
		}
	}()

	var network mdnstest.Network

	announced(t, &network, another, "SmartBridge", elsewhere)

	began := time.Now()

	out, status := searching(t, here(&network), "--credentials", dir, "--wait", "1s", "ping")
	if status != cli.ExitError || !strings.Contains(out, "the bridge "+other+", found on this network") {
		t.Fatalf("ping with a pretender alone exits %d, saying %q", status, out)
	}

	// What never answers is given the wait, of a second, and not all the
	// time a command has, of thirty.
	if took := time.Since(began); took > wait/3 {
		t.Errorf("ping with a pretender alone took %s under a wait of a second", took)
	}

	<-tried

	// The pretender is tried first, the bridge being announced only once
	// it was, and is still being waited for when the bridge is opened:
	// the wait given it is as long as a command has in all.
	var said syncBuffer

	done := make(chan int, 1)

	go func() {
		done <- cli.Run(t.Context(), cli.Environment{
			Args: []string{"--credentials", dir, "--wait", wait.String(), "ping"},
			Out:  &said, Err: &said, Network: here(&network),
		})
	}()

	select {
	case <-tried:
	case status = <-done:
		t.Fatalf("ping ended before it tried the pretender: %d, saying %q", status, said.String())
	}

	announced(t, &network, hostname, "SmartBridge", "127.0.0.1")

	if status = <-done; status != cli.ExitOK || !strings.Contains(said.String(), "LEAP 1.115") {
		t.Errorf("ping past a pretender exits %d, saying %q", status, said.String())
	}

	// A bridge that takes the credentials no more, announcing with its
	// own address one that never answers: what the bridge answered is
	// what is said, a silence saying nothing of it.
	bridge.Revoke()

	var stale mdnstest.Network

	announced(t, &stale, hostname, "SmartBridge", "127.0.0.1", elsewhere)

	out, status = searching(t, here(&stale), "--credentials", dir, "--wait", "1s", "ping")
	if status != cli.ExitError || !strings.Contains(out, "the bridge "+itself+", found on this network") ||
		!strings.Contains(out, "the credentials are not usable") {
		t.Errorf("ping under revoked credentials, beside an address that never answers, exits %d, saying %q",
			status, out)
	}
}

// TestPairSearches: with no host given, pair pairs with the one bridge on
// the network, at its pairing port; among several it chooses none, and
// names them.
func TestPairSearches(t *testing.T) {
	t.Parallel()
	held(t)

	bridge := standing(t, "127.0.0.1")

	app := t.TempDir()
	certificate, key, root := bridge.BootstrapPEM()
	write(t, app, &leap.Credentials{Key: key, Certificate: certificate, Root: root})

	pair := func(dir string) []string {
		return []string{
			"--credentials", dir, "--wait", "2s", "pair",
			"--app-certificate", filepath.Join(app, "certificate.pem"),
			"--app-key", filepath.Join(app, "key.pem"), "--app-root", filepath.Join(app, "root.pem"),
			"--name", "the test",
		}
	}

	var network mdnstest.Network

	out, status := searching(t, here(&network), pair(filepath.Join(t.TempDir(), "none"))...)
	if status != cli.ExitError || !strings.Contains(out, "no bridge answered the search within 2s") {
		t.Fatalf("pair where no bridge is exits %d, saying %q", status, out)
	}

	announced(t, &network, hostname, "SmartBridge", "127.0.0.1")

	dir := filepath.Join(t.TempDir(), "credentials")

	var said syncBuffer

	done := make(chan int, 1)

	go func() {
		done <- cli.Run(t.Context(), cli.Environment{Args: pair(dir), Out: &said, Err: &said, Network: here(&network)})
	}()

	await(t, &said, "Press the button on the bridge.")
	bridge.Press()

	if status = <-done; status != cli.ExitOK || !strings.Contains(said.String(), `Paired as "the test"`) {
		t.Fatalf("pair with no host: %d\n%s", status, said.String())
	}

	if paired := bridge.Paired(); len(paired) != 1 || paired[0] != "the test" {
		t.Errorf("the bridge paired with %q, want the test alone", paired)
	}

	if out, status = searching(t, here(&network), "--credentials", dir, "ping"); status != cli.ExitOK ||
		!strings.Contains(out, "LEAP 1.115") {
		t.Errorf("ping under what pair wrote exits %d, saying %q", status, out)
	}

	announced(t, &network, another, "SmartBridge", "192.0.2.20")

	out, status = searching(t, here(&network), pair(filepath.Join(t.TempDir(), "several"))...)
	if want := fmt.Sprintf("2 bridges answered the search, %s, %s", itself, other); status != cli.ExitError ||
		!strings.Contains(out, want) || strings.Contains(out, "Press the button") {
		t.Errorf("pair among two bridges exits %d, saying %q; want %q", status, out, want)
	}

	if paired := bridge.Paired(); len(paired) != 1 {
		t.Errorf("the bridge paired with %q after a pair that chose none", paired)
	}
}
