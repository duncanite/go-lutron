package caseta

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/duncanite/go-lutron/internal/terminal"
)

// WriteDevices prints the devices as a table, a person's listing: each
// with its number, its name with its area, its type, model, serial and
// firmware, the zones it drives, and its buttons as id=number. buttons
// is what Buttons returned; a device's are found through its button
// groups.
//
// What the bridge wrote is shown and not obeyed: a character a terminal
// would not simply draw, a line's end or a tab in a name among them, is
// written out as Go writes it in a string.
func WriteDevices(out io.Writer, devices []Device, buttons []Button) error {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	terminal.Printf(table, "DEVICE\tNAME\tTYPE\tMODEL\tSERIAL\tFIRMWARE\tZONE\tBUTTONS\n")

	for index := range devices {
		device := &devices[index]

		var zones []string

		for _, driven := range device.LocalZones {
			zones = append(zones, driven.ID())
		}

		terminal.Printf(table, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			device.ID(), strings.Join(device.FullyQualifiedName, " "), device.DeviceType,
			device.ModelNumber, device.SerialNumber, device.Firmware, strings.Join(zones, ","),
			strings.Join(buttonsOf(device, buttons), " "))
	}

	if err := table.Flush(); err != nil {
		return fmt.Errorf("writing the devices: %w", err)
	}

	return nil
}

// buttonsOf is the device's buttons, each as its id and its number on
// the device.
func buttonsOf(device *Device, buttons []Button) []string {
	var found []string

	for _, button := range buttons {
		for _, group := range device.ButtonGroups {
			if button.Parent.Href == group.Href {
				found = append(found, fmt.Sprintf("%s=%d", button.ID(), button.ButtonNumber))
			}
		}
	}

	return found
}
