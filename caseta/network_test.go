package caseta_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/duncanite/go-lutron/caseta"
)

// announced is a Smart Bridge's announcement, as one was seen: its
// instance, its host named after its serial in hexadecimal, and its TXT
// record. The serial and the MACADDR are made up; the rest says a kind
// of bridge, a firmware and a state, which are no one bridge's.
func announced() caseta.Instance {
	return caseta.Instance{
		Name: "Lutron Status",
		Host: "Lutron-000004d2.local.",
		Text: map[string]string{
			"MACADDR": "00:00:5e:00:53:01", "CODEVER": "08.28.13f000", "DEVCLASS": "08040100",
			"FW_STATUS": "1:NoUpdate", "NW_STATUS": "InternetWorking", "ST_STATUS": "good",
			"SYSTYPE": "SmartBridge",
		},
	}
}

// TestRecognize: a Smart Bridge's announcement gives its identity, the
// serial its host's name carries in hexadecimal, which is the one its
// own entry gives once opened; and where to open it, on LEAP's port and
// not the one announced. What is not a Caséta bridge is not recognized.
func TestRecognize(t *testing.T) {
	t.Parallel()

	// The recorded bridge's serial is 1234, which is 4d2.
	_, opened := bridge(t)

	found, ours := caseta.Recognize(announced())
	if !ours || found.ID != opened.ID() || !slices.Equal(found.Addresses, []string{"Lutron-000004d2.local:8081"}) {
		t.Fatalf("Recognize = %+v, %t; want the ID %q", found, ours, opened.ID())
	}

	parse := netip.MustParseAddr

	for name, where := range map[string]struct {
		host  string
		addrs []netip.Addr
		want  []string
	}{
		"an address":        {"lutron-000004d2.local", []netip.Addr{parse("192.0.2.7")}, []string{"192.0.2.7:8081"}},
		"an IPv6 address":   {"LUTRON-000004D2", []netip.Addr{parse("2001:db8::7")}, []string{"[2001:db8::7]:8081"}},
		"a zoned address":   {"Lutron-000004d2.", []netip.Addr{parse("fe80::1%en0")}, []string{"[fe80::1%en0]:8081"}},
		"a name and no dot": {"Lutron-000004d2", nil, []string{"Lutron-000004d2:8081"}},
		"several addresses, in their order": {
			"Lutron-000004d2.local.",
			[]netip.Addr{parse("2001:db8::7"), parse("192.0.2.7"), parse("169.254.3.4")},
			[]string{"[2001:db8::7]:8081", "192.0.2.7:8081", "169.254.3.4:8081"},
		},
		"addresses nothing can dial, ahead of one": {
			"Lutron-000004d2.local.",
			[]netip.Addr{parse("fe80::1"), {}, parse("::"), parse("0.0.0.0"), parse("192.0.2.7")},
			[]string{"192.0.2.7:8081"},
		},
		"no address anything can dial": {
			"Lutron-000004d2.local.", []netip.Addr{parse("fe80::1"), {}}, []string{"Lutron-000004d2.local:8081"},
		},
	} {
		instance := announced()
		instance.Host, instance.Addrs = where.host, where.addrs

		found, ours = caseta.Recognize(instance)
		if !ours || found.ID != "1234" || !slices.Equal(found.Addresses, where.want) {
			t.Errorf("Recognize with %s = %+v, %t; want %s", name, found, ours, where.want)
		}
	}

	pro := announced()
	pro.Text["SYSTYPE"] = "smartbridge pro"

	if _, ours = caseta.Recognize(pro); !ours {
		t.Error("a system type that begins with SmartBridge, whatever its case, is not recognized")
	}

	// A TXT record's keys are matched whatever their case, and the key as
	// written is the one read when a record holds both.
	for _, key := range []string{"systype", "Systype", "sYSTYPE"} {
		folded := announced()
		delete(folded.Text, "SYSTYPE")
		folded.Text[key] = "SmartBridge"

		if _, ours = caseta.Recognize(folded); !ours {
			t.Errorf("a bridge whose system type is under the key %q is not recognized", key)
		}

		folded.Text["SYSTYPE"] = "radiora3"

		if _, ours = caseta.Recognize(folded); ours {
			t.Errorf("a system type under %q was read ahead of the one under SYSTYPE", key)
		}
	}

	for name, change := range map[string]func(*caseta.Instance){
		"another Lutron system":       func(instance *caseta.Instance) { instance.Text["SYSTYPE"] = "radiora3" },
		"no system type":              func(instance *caseta.Instance) { delete(instance.Text, "SYSTYPE") },
		"no TXT record":               func(instance *caseta.Instance) { instance.Text = nil },
		"a host that is no bridge's":  func(instance *caseta.Instance) { instance.Host = "printer.local." },
		"a host with seven digits":    func(instance *caseta.Instance) { instance.Host = "Lutron-00004d2.local." },
		"a host with no hexadecimal":  func(instance *caseta.Instance) { instance.Host = "Lutron-0000zzzz.local." },
		"a host with more behind it":  func(instance *caseta.Instance) { instance.Host = "Lutron-000004d2.local.evil" },
		"a serial of zero":            func(instance *caseta.Instance) { instance.Host = "Lutron-00000000.local." },
		"a host with something ahead": func(instance *caseta.Instance) { instance.Host = "x-Lutron-000004d2.local." },
		"another system, key folded": func(instance *caseta.Instance) {
			delete(instance.Text, "SYSTYPE")
			instance.Text["systype"] = "radiora3"
		},
	} {
		instance := announced()
		change(&instance)

		if found, ours = caseta.Recognize(instance); ours {
			t.Errorf("%s was recognized as %+v", name, found)
		}
	}
}

// errBrowser is what the browser of TestFind ends with.
var errBrowser = errors.New("the network is down")

// browser tells of what a test gives it, then ends as told.
type browser struct {
	asked []string
	tell  func(handle func(caseta.Instance, bool))
	ends  error
}

func (b *browser) Browse(_ context.Context, service string, handle func(caseta.Instance, bool)) error {
	b.asked = append(b.asked, service)
	b.tell(handle)

	return b.ends
}

// TestFind: Find browses for the bridges' service through the browser it
// is lent, and tells of the bridges alone: found, moved, told again as
// the browser tells again, and gone; one gone by its name alone is not
// told. It ends as the browser does.
func TestFind(t *testing.T) {
	t.Parallel()

	moved := announced()
	moved.Addrs = []netip.Addr{netip.MustParseAddr("192.0.2.9")}

	other := announced()
	other.Text["SYSTYPE"] = "hwqs"

	// The same bridge, with something Found does not hold changed.
	updating := moved
	updating.Text = announced().Text
	updating.Text["FW_STATUS"] = "1:Updating"

	lent := &browser{ends: errBrowser, tell: func(handle func(caseta.Instance, bool)) {
		handle(other, false)
		handle(announced(), false)
		handle(moved, false)
		handle(updating, false)
		handle(other, true)
		handle(caseta.Instance{Name: moved.Name}, true)
		handle(moved, true)
	}}

	var told []string

	err := caseta.Find(t.Context(), lent, func(found caseta.Found, lost bool) {
		told = append(told,
			string(found.ID)+" at "+strings.Join(found.Addresses, ",")+map[bool]string{true: ", gone", false: ""}[lost])
	})

	want := []string{
		"1234 at Lutron-000004d2.local:8081", "1234 at 192.0.2.9:8081", "1234 at 192.0.2.9:8081",
		"1234 at 192.0.2.9:8081, gone",
	}
	if !errors.Is(err, errBrowser) || !slices.Equal(told, want) || !slices.Equal(lent.asked, []string{"_lutron._tcp"}) {
		t.Fatalf("Find = %v, told %v, having asked for %v\nwant %v", err, told, lent.asked, want)
	}

	lent.ends = nil

	if err = caseta.Find(t.Context(), lent, func(caseta.Found, bool) {}); err != nil {
		t.Fatalf("Find through a browser that ends with no error = %v", err)
	}
}
