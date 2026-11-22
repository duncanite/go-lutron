package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"

	mdns "github.com/forkcloser/go-mdns"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/internal/terminal"
	"github.com/duncanite/go-lutron/leap"
)

// lent is the host's Multicast DNS as the library asks for it.
type lent struct {
	peer *mdns.Peer
}

func (l lent) Browse(ctx context.Context, service string, handle func(caseta.Instance, bool)) error {
	//nolint:wrapcheck // the Peer's own word, which the library hands on as it is
	return l.peer.Browse(ctx, mdns.Query{Type: service}, func(change mdns.Change, heard mdns.Instance) {
		text := make(map[string]string, len(heard.Text))
		for _, entry := range heard.Text {
			text[entry.Key] = string(entry.Value)
		}

		handle(
			caseta.Instance{Name: heard.Name, Host: heard.Host, Addrs: heard.Addrs, Text: text},
			change == mdns.Lost,
		)
	})
}

// search hears the bridges that announce themselves, for the wait or
// until ctx ends, and gives them in the order of their ids. Each is put
// to heard as it is heard, and again each time more is heard of it.
func (root *rootCommand) search(ctx context.Context, heard func(caseta.Found)) ([]caseta.Found, error) {
	if root.Wait <= 0 {
		return nil, fmt.Errorf("%w: --wait wants a time above zero", errUsage)
	}

	cfg := mdns.Config{Network: root.network}

	if root.Interface != "" {
		cfg.Interfaces = mdns.On(root.Interface)
	}

	peer, err := mdns.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the network: %w", err)
	}

	defer func() { _ = peer.Close() }()

	searching, cancel := context.WithTimeout(ctx, root.Wait)
	defer cancel()

	found := make(map[caseta.ID]caseta.Found)

	err = caseta.Find(searching, lent{peer: peer}, func(bridge caseta.Found, lost bool) {
		if lost {
			delete(found, bridge.ID)

			return
		}

		found[bridge.ID] = bridge

		if heard != nil {
			heard(bridge)
		}
	})

	if ctx.Err() != nil {
		return nil, fmt.Errorf("searching for bridges: %w", context.Cause(ctx))
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("searching for bridges: %w", err)
	}

	bridges := make([]caseta.Found, 0, len(found))
	for _, id := range slices.Sorted(maps.Keys(found)) {
		bridges = append(bridges, found[id])
	}

	return bridges, nil
}

// tries is the places tried for a bridge that takes the credentials, side
// by side: the first that opens, and what one that did not answered.
type tries struct {
	running sync.WaitGroup

	mu      sync.Mutex
	opened  *caseta.Bridge
	refused error
}

// settle takes the end of one try, at a place whoever claimed that id
// gave, and says whether it is the one kept. A bridge opened after
// another was is closed: one is spoken to.
//
// A try that ended unanswered, its time run out, says nothing of the
// bridge: it does not replace what one answered, which is what a person
// with credentials a bridge takes no more has to read.
func (t *tries) settle(claimed caseta.ID, candidate *caseta.Bridge, err error, unanswered bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err != nil {
		if t.refused == nil || !unanswered {
			t.refused = fmt.Errorf("the bridge %s, found on this network: %w", claimed, err)
		}

		return false
	}

	if t.opened != nil {
		_ = candidate.Close()

		return false
	}

	t.opened = candidate

	return true
}

// open is the bridge at --host, or with none the bridge on this network
// that takes the credentials: each one that announces itself is tried, at
// each place it says it is, as soon as it is heard. An announcement is a
// claim; the credentials are the proof, and a bridge that is not the one
// paired with refuses them.
//
// The places are tried side by side, each given the wait to answer: one
// that does not answer takes none of another's time, and a command with
// nothing to open ends within twice the wait, however many announce
// themselves.
func (root *rootCommand) open(ctx context.Context, creds *leap.Credentials) (*caseta.Bridge, error) {
	if root.Host != "" {
		return caseta.Open(ctx, root.Host, creds)
	}

	// Ended by the first bridge opened: nothing more is heard, and the
	// tries still running are given up.
	looking, found := context.WithCancel(ctx)
	defer found()

	var attempts tries

	tried := map[string]bool{}

	_, err := root.search(looking, func(bridge caseta.Found) {
		for _, address := range bridge.Addresses {
			if tried[address] {
				continue
			}

			tried[address] = true

			attempts.running.Go(func() {
				trying, cancel := context.WithTimeout(looking, root.Wait)
				defer cancel()

				candidate, err := caseta.Open(trying, address, creds)
				if attempts.settle(bridge.ID, candidate, err, trying.Err() != nil) {
					found()
				}
			})
		}
	})

	attempts.running.Wait()

	switch {
	case attempts.opened != nil:
		return attempts.opened, nil
	case err != nil:
		return nil, err
	case attempts.refused != nil:
		return nil, attempts.refused
	default:
		return nil, fmt.Errorf("%w: no bridge answered the search within %s; name one with --host",
			errNotFound, root.Wait)
	}
}

// pairing is where to pair: the pairing port of the bridge at --host, or
// with none of the one bridge on this network. Several are not chosen
// among: the button pressed says which bridge a person means, and the
// command cannot see it.
func (root *rootCommand) pairing(ctx context.Context) (string, error) {
	if root.Host != "" {
		if _, _, err := net.SplitHostPort(root.Host); err == nil {
			return root.Host, nil
		}

		return net.JoinHostPort(strings.Trim(root.Host, "[]"), leap.PairingPort), nil
	}

	bridges, err := root.search(ctx, nil)
	if err != nil {
		return "", err
	}

	switch len(bridges) {
	case 0:
		return "", fmt.Errorf("%w: no bridge answered the search within %s; name one with --host",
			errNotFound, root.Wait)
	case 1:
		host, _, err := net.SplitHostPort(bridges[0].Addresses[0])
		if err != nil {
			return "", fmt.Errorf("the bridge %s: %w", bridges[0].ID, err)
		}

		return net.JoinHostPort(host, leap.PairingPort), nil
	default:
		ids := make([]string, 0, len(bridges))
		for _, bridge := range bridges {
			ids = append(ids, string(bridge.ID))
		}

		return "", fmt.Errorf("%w: %d bridges answered the search, %s; name the one to pair with by --host",
			errUsage, len(bridges), strings.Join(ids, ", "))
	}
}

// Run finds the bridges that announce themselves on this network and
// prints each: who it says it is, and where.
func (*discoverCommand) Run(ctx context.Context, root *rootCommand) error {
	bridges, err := root.search(ctx, nil)
	if err != nil {
		return err
	}

	if len(bridges) == 0 {
		terminal.Printf(root.out, "No bridge answered.\n")

		return nil
	}

	table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

	terminal.Printf(table, "BRIDGE\tAT\n")

	for _, bridge := range bridges {
		terminal.Printf(table, "%s\t%s\n", bridge.ID, strings.Join(bridge.Addresses, ", "))
	}

	if err = table.Flush(); err != nil {
		return fmt.Errorf("writing the bridges: %w", err)
	}

	return nil
}
