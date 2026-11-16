package caseta

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/duncanite/go-lutron/leap"
)

// Service is the service type a Lutron bridge announces itself under on
// the local network, over Multicast DNS.
const Service = "_lutron._tcp"

// Instance is one instance of a service as it was found on the network:
// what this package reads of an announcement, and nothing of the means
// that found it.
type Instance struct {
	// Name is the instance's name. A bridge's is "Lutron Status".
	Name string
	// Host is the name of the host it is on, as announced: with or
	// without ".local", with or without a last dot.
	Host string
	// Addrs is where that host is, when that is known: every address it
	// has, an IPv6 link-local one with its zone.
	Addrs []netip.Addr
	// Text is the entries of its TXT record, by their keys, in whatever
	// case those came.
	Text map[string]string
}

// Found is what a host on the network says of itself: that it is a
// Caséta bridge, which one, and where.
//
// An announcement is anyone's to make, and proves nothing. Open under
// the credentials pairing gave takes only a bridge the pairing's root
// vouches for, and Bridge.ID, read once it is opened, says which.
// An ErrCredentials at an address that was found says that what answers
// there is not the bridge that was paired, or that this one takes the
// pairing no more: by itself it does not say the pairing is void.
type Found struct {
	// ID is who it says it is: the value Bridge.ID gives once that bridge
	// is opened.
	ID ID
	// Addresses is where it says it is, each as Open takes it, on LEAP's
	// port and not the one a bridge announces: every address its host
	// has that can be dialled, in the order they came; and its host's
	// name alone when there is none. Which of several reaches the bridge
	// from here is the caller's to know, or to try.
	Addresses []string
}

// host is a bridge's host name: Lutron, then its serial number in eight
// hexadecimal digits.
var host = regexp.MustCompile(`(?i)^lutron-([0-9a-f]{8})(\.local)?\.?$`)

// Recognize says whether an instance of Service is a Caséta bridge, and
// which. A bridge says what it is in its TXT record, SYSTYPE beginning
// with SmartBridge; other Lutron systems announce the same service under
// other types, and are none of this package's. It says who it is in its
// host's name, which carries its serial.
func Recognize(instance Instance) (Found, bool) {
	const system = "smartbridge"

	if !strings.HasPrefix(strings.ToLower(text(instance, "SYSTYPE")), system) {
		return Found{}, false
	}

	named := host.FindStringSubmatch(instance.Host)
	if named == nil {
		return Found{}, false
	}

	const hexadecimal, bits = 16, 32

	serial, err := strconv.ParseUint(named[1], hexadecimal, bits)
	if err != nil || serial == 0 {
		return Found{}, false
	}

	found := Found{ID: ID(strconv.FormatUint(serial, 10))}

	for _, addr := range instance.Addrs {
		// A link-local address with no zone names no interface to leave by.
		if !addr.IsValid() || addr.IsUnspecified() || addr.Is6() && addr.IsLinkLocalUnicast() && addr.Zone() == "" {
			continue
		}

		found.Addresses = append(found.Addresses, net.JoinHostPort(addr.String(), leap.Port))
	}

	if len(found.Addresses) == 0 {
		found.Addresses = []string{net.JoinHostPort(strings.TrimSuffix(instance.Host, "."), leap.Port)}
	}

	return found, true
}

// text is an entry of an instance's TXT record. The keys of a TXT record
// are matched whatever their case (RFC 6763, section 6.4), and a browser
// may hand them as announced or folded; the key as written wins, and of
// two that differ from it by case alone, either.
func text(instance Instance, key string) string {
	if value, ok := instance.Text[key]; ok {
		return value
	}

	for name, value := range instance.Text {
		if strings.EqualFold(name, key) {
			return value
		}
	}

	return ""
}

// Browser is the means of finding services on the local network, which
// this package does not have and is lent: a host has one Multicast DNS
// responder, and it is the caller's. Whatever browses satisfies it with
// a few lines.
type Browser interface {
	// Browse tells handle of the instances of a service type: each one it
	// knows of or finds, and each change to one, whole as it then stands,
	// with lost false; and each one that is gone, as it last stood, with
	// lost true. Calls come one at a time and in order, for as long as
	// ctx lasts. It returns ctx's error when ctx ends, and its own when
	// it can browse no more.
	//
	// One that is gone has to come with its host and its TXT record: Find
	// knows a bridge by them, and is told nothing of an instance that
	// comes with its name alone.
	Browse(ctx context.Context, service string, handle func(instance Instance, lost bool)) error
}

// Find tells handle of the Caséta bridges on the local network, through
// the browser it is lent: each one found, and again each time the
// browser tells of it again, whether or not what Found holds changed,
// with lost false; each one gone, with lost true. It is the browser's
// Browse for Service, narrowed to what Recognize takes, with nothing
// kept between two calls, and lasts and ends as that does.
func Find(ctx context.Context, browser Browser, handle func(found Found, lost bool)) error {
	err := browser.Browse(ctx, Service, func(instance Instance, lost bool) {
		if found, ours := Recognize(instance); ours {
			handle(found, lost)
		}
	})
	if err != nil {
		return fmt.Errorf("finding bridges: %w", err)
	}

	return nil
}
