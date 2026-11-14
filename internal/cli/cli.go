// Package cli is the lutron command: pairing with a bridge, what it
// holds, its zones' levels, the commands a zone, a button and a scene
// take, its events, and call for the rest. The wiring of flags and
// output over the caseta and leap packages; nothing that speaks LEAP
// lives here.
package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/forkcloser/xos"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/credentials"
	"github.com/duncanite/go-lutron/leap"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitError = 1
)

const (
	// requestTimeout bounds one exchange with the bridge; pairing has
	// its own, the time a person has to reach the button.
	requestTimeout = 30 * time.Second
)

var (
	errNoZone   = errors.New("no zone by that id or name")
	errSeveral  = errors.New("several zones by that name; say the room too, or the zone's id")
	errNotJSON  = errors.New("the body is not JSON")
	errIdentity = errors.New("--app-certificate, --app-key and --app-root go together")
)

// exitStatus is how the parser's own exit, on --help, leaves Run: kong
// calls its exit function and goes on, so the function panics with the
// status and Run recovers it.
type exitStatus int

type rootCommand struct {
	Host        string `env:"LUTRON_HOST"        help:"The bridge: its address, with a port when it is not the usual one, LEAP's, or the pairing port's for pair."                               required:"true"`
	Credentials string `env:"LUTRON_CREDENTIALS" help:"The directory holding what pairing gave: key.pem, certificate.pem, root.pem. No default: the key is private, and where it lives is said." required:"true"`

	Pair    pairCommand    `cmd:"" help:"Pair with the bridge: press its button when asked. Writes the credentials."`
	Ping    pingCommand    `cmd:"" help:"Ask the bridge whether it is there; print its LEAP version."`
	Devices devicesCommand `cmd:"" help:"Print every device the bridge knows, with its zone or its buttons."`
	Scenes  scenesCommand  `cmd:"" help:"Print the scenes."`
	Level   levelCommand   `cmd:"" help:"Print what a zone is at."`
	Set     setCommand     `cmd:"" help:"Send a zone to a level, 0 to 100."`
	On      onCommand      `cmd:"" help:"Send a zone to 100."`
	Off     offCommand     `cmd:"" help:"Send a zone to 0."`
	Raise   raiseCommand   `cmd:"" help:"Start raising a zone, until stop."`
	Lower   lowerCommand   `cmd:"" help:"Start lowering a zone, until stop."`
	Stop    stopCommand    `cmd:"" help:"Stop a zone being raised or lowered."`
	Press   pressCommand   `cmd:"" help:"Press and release a button, by its id."`
	Run     runCommand     `cmd:"" help:"Run a scene, by its id."`
	Watch   watchCommand   `cmd:"" help:"Print zone changes and button presses until interrupted."`
	Call    callCommand    `cmd:"" help:"Send any request and print the bridge's answer."`

	out io.Writer
}

type zoneTarget struct {
	Zone string `arg:"" help:"A zone: its id, or the name of the device that drives it."`
}

type (
	pairCommand struct {
		AppCertificate string        `help:"Another certificate for the pairing port, PEM, in place of the Caséta app's; with --app-key and --app-root."  type:"existingfile"`
		AppKey         string        `help:"Its key, PEM."                                                                                                type:"existingfile"`
		AppRoot        string        `help:"The authority the pairing port's own certificate chains to, PEM."                                             type:"existingfile"`
		Name           string        `default:"go-lutron"                                                                                                 help:"What the bridge calls this client."`
		Timeout        time.Duration `default:"3m"                                                                                                        help:"How long to wait for the button."`
		Force          bool          `help:"Replace credentials already in the directory."`
	}
	pingCommand    struct{}
	devicesCommand struct{}
	scenesCommand  struct{}
	levelCommand   struct{ zoneTarget }
	setCommand     struct {
		zoneTarget

		Level int           `arg:""                                        help:"The level, 0 to 100."`
		Fade  time.Duration `help:"Fade over this long, in whole seconds."`
	}
	onCommand    struct{ zoneTarget }
	offCommand   struct{ zoneTarget }
	raiseCommand struct{ zoneTarget }
	lowerCommand struct{ zoneTarget }
	stopCommand  struct{ zoneTarget }
	pressCommand struct {
		Button string `arg:"" help:"The button's id, as devices prints it."`
	}
	runCommand struct {
		Scene string `arg:"" help:"The scene's id, as scenes prints it."`
	}
	watchCommand struct{}
	callCommand  struct {
		Communique string `arg:"" enum:"ReadRequest,CreateRequest,UpdateRequest,SubscribeRequest" help:"The request's type."`
		URL        string `arg:"" help:"The resource, as /device or /zone/1/status."`
		Body       string `arg:"" help:"The request's body, JSON."                                optional:""`
	}
)

// Run parses args and runs the command, printing to stdout and errors to
// stderr, and returns the exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) (status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exited, ok := recovered.(exitStatus)
			if !ok {
				panic(recovered)
			}

			status = int(exited)
		}
	}()

	root := rootCommand{out: stdout}

	parser, err := kong.New(&root,
		kong.Name("lutron"),
		kong.Description("Speak to a Lutron Caséta bridge on this network."),
		kong.UsageOnError(),
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) { panic(exitStatus(code)) }),
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	if err != nil {
		fmt.Fprintln(stderr, "lutron:", err)

		return ExitError
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		parser.Errorf("%s", err)

		return ExitError
	}

	if err := kctx.Run(&root); err != nil {
		fmt.Fprintln(stderr, "lutron:", err)

		return ExitError
	}

	return ExitOK
}

// credentials reads what pairing wrote.
func (root *rootCommand) credentials() (*leap.Credentials, error) {
	loaded, err := credentials.Load(root.Credentials)
	if err != nil {
		return nil, fmt.Errorf("%w; pair writes them", err)
	}

	return loaded, nil
}

// with runs visit against the bridge, under a deadline, and closes the
// connection after it.
func (root *rootCommand) with(ctx context.Context, visit func(context.Context, *caseta.Bridge) error) error {
	creds, err := root.credentials()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	bridge, err := caseta.Open(ctx, root.Host, creds)
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close() }()

	return visit(ctx, bridge)
}

// zone resolves a target to a zone's id. A zone's id wins; otherwise the
// target is a device's name, with or without its area, and has to fit
// one zone: a name two rooms share is refused with the zones it fits.
func zone(ctx context.Context, bridge *caseta.Bridge, target string) (string, error) {
	devices, err := bridge.Devices(ctx)
	if err != nil {
		return "", err
	}

	var ids, named []string

	for _, device := range devices {
		full := strings.Join(device.FullyQualifiedName, " ")

		for _, driven := range device.LocalZones {
			if driven.ID() == target {
				return target, nil
			}

			if strings.EqualFold(device.Name, target) || strings.EqualFold(full, target) {
				ids = append(ids, driven.ID())
				named = append(named, fmt.Sprintf("%q (zone %s)", full, driven.ID()))
			}
		}
	}

	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%w: %q", errNoZone, target)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%w: %q fits %s", errSeveral, target, strings.Join(named, ", "))
	}
}

// onZone resolves the target and runs act on the zone.
func (root *rootCommand) onZone(
	ctx context.Context,
	target string,
	act func(context.Context, *caseta.Bridge, string) error,
) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		id, err := zone(ctx, bridge, target)
		if err != nil {
			return err
		}

		return act(ctx, bridge, id)
	})
}

func (cmd *pairCommand) Run(ctx context.Context, root *rootCommand) error {
	if !cmd.Force {
		if err := credentials.Present(root.Credentials); err != nil {
			return fmt.Errorf("%w; --force replaces them", err)
		}
	}

	config, err := cmd.identity()
	if err != nil {
		return err
	}

	// Before the button: the directory and the key's file, empty and
	// private. A pairing the bridge granted with nowhere to write it
	// would be a pairing lost.
	reserved, err := credentials.Reserve(root.Credentials)
	if err != nil {
		return err
	}

	defer reserved.Discard()

	ctx, cancel := context.WithTimeout(ctx, cmd.Timeout)
	defer cancel()

	host := root.Host
	if _, _, err = net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), leap.PairingPort)
	}

	conn, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("connecting to the bridge's pairing port at %s: %w", host, err)
	}

	paired, err := leap.Pair(ctx, conn, cmd.Name, func() {
		fmt.Fprintln(root.out, "Press the button on the bridge.")
	})
	if err != nil {
		return err
	}

	if err = reserved.Save(paired); err != nil {
		return err
	}

	fmt.Fprintf(root.out, "Paired as %q; the credentials are in %s.\n", cmd.Name, root.Credentials)

	return nil
}

// identity is the TLS configuration the pairing port takes: the Caséta
// app's, or another from the three files, all of them or none. The
// port's certificate names no host, so its chain to the root is what is
// verified.
func (cmd *pairCommand) identity() (*tls.Config, error) {
	if cmd.AppCertificate == "" && cmd.AppKey == "" && cmd.AppRoot == "" {
		config, err := leap.TLSConfig(leap.CasetaApp())
		if err != nil {
			return nil, fmt.Errorf("the Caséta app's identity: %w", err)
		}

		return config, nil
	}

	if cmd.AppCertificate == "" || cmd.AppKey == "" || cmd.AppRoot == "" {
		return nil, errIdentity
	}

	certificate, err := xos.ReadFile(cmd.AppCertificate)
	if err != nil {
		return nil, fmt.Errorf("reading the app's certificate: %w", err)
	}

	key, err := xos.ReadFile(cmd.AppKey)
	if err != nil {
		return nil, fmt.Errorf("reading the app's key: %w", err)
	}

	root, err := xos.ReadFile(cmd.AppRoot)
	if err != nil {
		return nil, fmt.Errorf("reading the app's root: %w", err)
	}

	config, err := leap.TLSConfig(&leap.Credentials{Key: key, Certificate: certificate, Root: root})
	if err != nil {
		return nil, fmt.Errorf("the app's identity: %w", err)
	}

	return config, nil
}

func (*pingCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		version, err := bridge.Ping(ctx)
		if err != nil {
			return err
		}

		fmt.Fprintf(root.out, "LEAP %v\n", version)

		return nil
	})
}

func (*devicesCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		devices, err := bridge.Devices(ctx)
		if err != nil {
			return err
		}

		buttons, err := bridge.Buttons(ctx)
		if err != nil {
			return err
		}

		return caseta.WriteDevices(root.out, devices, buttons)
	})
}

func (*scenesCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		scenes, err := bridge.Scenes(ctx)
		if err != nil {
			return err
		}

		for _, scene := range scenes {
			if scene.IsProgrammed {
				fmt.Fprintf(root.out, "%s\t%s\n", scene.ID(), scene.Name)
			}
		}

		return nil
	})
}

func (cmd *levelCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		status, err := bridge.Zone(ctx, zoneID)
		if err != nil {
			return err
		}

		if status.Level == nil {
			fmt.Fprintf(root.out, "zone %s reports no level\n", zoneID)

			return nil
		}

		fmt.Fprintf(root.out, "zone %s is at %d\n", zoneID, *status.Level)

		return nil
	})
}

func (cmd *setCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.SetLevel(ctx, zoneID, cmd.Level, cmd.Fade)
	})
}

func (cmd *onCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.SetLevel(ctx, zoneID, 100, 0) //nolint:mnd // full
	})
}

func (cmd *offCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.SetLevel(ctx, zoneID, 0, 0)
	})
}

func (cmd *raiseCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.Raise(ctx, zoneID)
	})
}

func (cmd *lowerCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.Lower(ctx, zoneID)
	})
}

func (cmd *stopCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.onZone(ctx, cmd.Zone, func(ctx context.Context, bridge *caseta.Bridge, zoneID string) error {
		return bridge.Stop(ctx, zoneID)
	})
}

func (cmd *pressCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		return bridge.Press(ctx, cmd.Button)
	})
}

func (cmd *runCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		return bridge.Run(ctx, cmd.Scene)
	})
}

// Run takes the zones' changes, subscribes to every button, and prints until the
// context ends or the bridge hangs up. The events are printed from the
// connection's reading goroutine, one line each.
func (*watchCommand) Run(ctx context.Context, root *rootCommand) error {
	creds, err := root.credentials()
	if err != nil {
		return err
	}

	bridge, err := caseta.Open(ctx, root.Host, creds)
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close() }()

	buttons, err := bridge.Buttons(ctx)
	if err != nil {
		return err
	}

	printZone := func(status caseta.ZoneStatus) {
		if status.Level != nil {
			fmt.Fprintf(root.out, "zone %s is at %d\n", status.Zone.ID(), *status.Level)
		}
	}

	bridge.WatchZones(printZone)

	if err = readZones(ctx, bridge, printZone); err != nil {
		return err
	}

	for _, button := range buttons {
		err = bridge.WatchButton(ctx, button.ID(), func(event caseta.ButtonEvent) {
			fmt.Fprintf(root.out, "button %s %s\n", event.Button.ID(), strings.ToLower(event.Type))
		})
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(root.out, "watching the zones and %d buttons\n", len(buttons))

	select {
	case <-ctx.Done():
		return nil
	case <-bridge.Conn().Done():
		return bridge.Conn().Err()
	}
}

// readZones reads each zone once and hands its status on, as the open
// implementations do before they rely on the bridge sending changes; it
// also says where each stands.
func readZones(ctx context.Context, bridge *caseta.Bridge, each func(caseta.ZoneStatus)) error {
	devices, err := bridge.Devices(ctx)
	if err != nil {
		return err
	}

	for _, device := range devices {
		for _, driven := range device.LocalZones {
			status, err := bridge.Zone(ctx, driven.ID())
			if err != nil {
				return err
			}

			each(*status)
		}
	}

	return nil
}

func (cmd *callCommand) Run(ctx context.Context, root *rootCommand) error {
	var body any

	if cmd.Body != "" {
		if !json.Valid([]byte(cmd.Body)) {
			return errNotJSON
		}

		body = json.RawMessage(cmd.Body)
	}

	return root.with(ctx, func(ctx context.Context, bridge *caseta.Bridge) error {
		answer, err := bridge.Conn().Request(ctx, cmd.Communique, cmd.URL, body)
		if answer == nil {
			return err
		}

		// A refusal is an answer too: print it, then fail.
		pretty, marshalErr := json.MarshalIndent(answer, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("the bridge's answer: %w", marshalErr)
		}

		fmt.Fprintln(root.out, string(pretty))

		return err
	})
}
