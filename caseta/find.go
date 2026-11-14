package caseta

import (
	"errors"
	"fmt"
	"strings"
)

// The errors FindZone hands back.
var (
	// ErrNoZone: no zone has that id, and no device that drives one has
	// that name.
	ErrNoZone = errors.New("no zone by that id or name")
	// ErrSeveralZones: the name fits more than one zone; the error lists
	// them.
	ErrSeveralZones = errors.New("several zones by that name; say the room too, or the zone's id")
)

// Zone is a zone found by FindZone: its id, and the full name, area then
// device, of the device that drives it.
type Zone struct {
	ID   string
	Name string
}

// FindZone finds, among devices, the zone a person means by target. A
// zone's id wins; otherwise target is a device's name, with or without
// its area and whatever its case, and has to fit one zone: a name two
// rooms share is refused with the zones it fits, never settled by which
// the bridge lists first.
func FindZone(devices []Device, target string) (Zone, error) {
	var found []Zone

	for index := range devices {
		device := &devices[index]
		full := strings.Join(device.FullyQualifiedName, " ")

		for _, driven := range device.LocalZones {
			if driven.ID() == target {
				return Zone{ID: target, Name: full}, nil
			}

			if strings.EqualFold(device.Name, target) || strings.EqualFold(full, target) {
				found = append(found, Zone{ID: driven.ID(), Name: full})
			}
		}
	}

	switch len(found) {
	case 0:
		return Zone{}, fmt.Errorf("%w: %q", ErrNoZone, target)
	case 1:
		return found[0], nil
	default:
		named := make([]string, 0, len(found))
		for _, zone := range found {
			named = append(named, fmt.Sprintf("%q (zone %s)", zone.Name, zone.ID))
		}

		return Zone{}, fmt.Errorf("%w: %q fits %s", ErrSeveralZones, target, strings.Join(named, ", "))
	}
}
