// Package cli is the lutron command: finding a bridge, pairing with it,
// what it holds, its zones' levels, the commands a zone, a button and a
// scene take, its events, and call for the rest. The wiring of flags and
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
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/alecthomas/kong"
	"github.com/forkcloser/go-mdns/transport"
	"github.com/forkcloser/xos"

	"github.com/duncanite/go-lutron/caseta"
	"github.com/duncanite/go-lutron/internal/credentials"
	"github.com/duncanite/go-lutron/internal/terminal"
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
	errNotJSON  = errors.New("the body is not JSON")
	errIdentity = errors.New("--app-certificate, --app-key and --app-root go together")
	// errUsage is a command line the command cannot act on.
	errUsage = errors.New("cannot run")
	// errNotFound is a bridge that did not answer a search.
	errNotFound = errors.New("not found")
)

// exitStatus is how the parser's own exit, on --help, leaves Run: kong
// calls its exit function and goes on, so the function panics with the
// status and Run recovers it.
type exitStatus int

// Environment is what the command runs in.
type Environment struct {
	// Args is its arguments, without its name.
	Args []string
	// Out and Err are where it writes.
	Out io.Writer
	Err io.Writer
	// Network is where bridges are searched for; nil is the host's own.
	Network transport.Network
}

type rootCommand struct {
	Host        string        `env:"LUTRON_HOST"        help:"The bridge: its address, with a port when it is not the usual one, LEAP's, or the pairing port's for pair. With none, the bridge is searched for on this network."`
	Credentials string        `env:"LUTRON_CREDENTIALS" help:"The directory holding what pairing gave: key.pem, certificate.pem, root.pem. No default: the key is private, and where it lives is said. Every command but discover needs it."`
	Interface   string        `env:"LUTRON_INTERFACE"   help:"The network interface the bridge is on, when the host has several."`
	Wait        time.Duration `default:"3s"             env:"LUTRON_WAIT"                                                                                                                                                                    help:"How long a search hears bridges for, and how long each address found is given to answer."`

	Discover discoverCommand `cmd:"" help:"Print the bridges that announce themselves on this network. Needs no credentials."`
	Pair     pairCommand     `cmd:"" help:"Pair with the bridge: press its button when asked. Writes the credentials."`
	Ping     pingCommand     `cmd:"" help:"Ask the bridge whether it is there; print its LEAP version."`
	Devices  devicesCommand  `cmd:"" help:"Print every device the bridge knows, with its zone or its buttons."`
	Scenes   scenesCommand   `cmd:"" help:"Print the scenes."`
	Level    levelCommand    `cmd:"" help:"Print what a zone is at."`
	Set      setCommand      `cmd:"" help:"Send a zone to a level, 0 to 100."`
	On       onCommand       `cmd:"" help:"Send a zone to 100."`
	Off      offCommand      `cmd:"" help:"Send a zone to 0."`
	Raise    raiseCommand    `cmd:"" help:"Start raising a zone, until stop."`
	Lower    lowerCommand    `cmd:"" help:"Start lowering a zone, until stop."`
	Stop     stopCommand     `cmd:"" help:"Stop a zone being raised or lowered."`
	Press    pressCommand    `cmd:"" help:"Press and release a button, by its id."`
	Run      runCommand      `cmd:"" help:"Run a scene, by its id."`
	Watch    watchCommand    `cmd:"" help:"Print zone changes and button presses until interrupted."`
	Call     callCommand     `cmd:"" help:"Send any request and print the bridge's answer."`

	out     io.Writer
	network transport.Network
}

type zoneTarget struct {
	Zone string `arg:"" help:"A zone: its id, or the name of the device that drives it."`
}

type (
	discoverCommand struct{}
	pairCommand     struct {
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

// Run parses the arguments and runs the command, printing to Out and
// errors to Err, and returns the exit status.
func Run(ctx context.Context, env Environment) (status int) {
	stdout, stderr := env.Out, env.Err

	defer func() {
		if recovered := recover(); recovered != nil {
			exited, ok := recovered.(exitStatus)
			if !ok {
				panic(recovered)
			}

			status = int(exited)
		}
	}()

	root := rootCommand{out: stdout, network: env.Network}

	parser, err := kong.New(&root,
		kong.Name("lutron"),
		kong.Description("Speak to a Lutron Caséta bridge on this network."),
		kong.UsageOnError(),
		kong.Writers(stdout, stderr),
		kong.Exit(func(code int) { panic(exitStatus(code)) }),
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	if err != nil {
		terminal.Printf(stderr, "lutron: %s\n", err)

		return ExitError
	}

	kctx, err := parser.Parse(env.Args)
	if err != nil {
		parser.Errorf("%s", err)

		return ExitError
	}

	if err := kctx.Run(&root); err != nil {
		terminal.Printf(stderr, "lutron: %s\n", err)

		return ExitError
	}

	return ExitOK
}

// credentials reads what pairing wrote.
func (root *rootCommand) credentials() (*leap.Credentials, error) {
	if root.Credentials == "" {
		return nil, fmt.Errorf("%w: --credentials names the directory holding what pairing gave", errUsage)
	}

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

	bridge, err := root.open(ctx, creds)
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close() }()

	return visit(ctx, bridge)
}

// zone resolves a target to a zone's id through the bridge's devices.
func zone(ctx context.Context, bridge *caseta.Bridge, target string) (string, error) {
	devices, err := bridge.Devices(ctx)
	if err != nil {
		return "", err
	}

	found, err := caseta.FindZone(devices, target)
	if err != nil {
		return "", err
	}

	return found.ID, nil
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
	if root.Credentials == "" {
		return fmt.Errorf("%w: --credentials names the directory to write what pairing gives", errUsage)
	}

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

	host, err := root.pairing(ctx)
	if err != nil {
		return err
	}

	conn, err := leap.DialTLS(ctx, host, config)
	if err != nil {
		return fmt.Errorf("the pairing port: %w", err)
	}

	paired, err := leap.Pair(ctx, conn, cmd.Name, func() {
		terminal.Printf(root.out, "Press the button on the bridge.\n")
	})
	if err != nil {
		return err
	}

	if err = reserved.Save(paired); err != nil {
		return err
	}

	terminal.Printf(root.out, "Paired as \"%s\"; the credentials are in %s.\n", cmd.Name, root.Credentials)

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

		terminal.Printf(root.out, "LEAP %v\n", version)

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
				terminal.Printf(root.out, "%s\t%s\n", scene.ID(), scene.Name)
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
			terminal.Printf(root.out, "zone %s reports no level\n", zoneID)

			return nil
		}

		terminal.Printf(root.out, "zone %s is at %d\n", zoneID, *status.Level)

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

// Run prints what each zone is at, then every zone's change and every
// button's press and release, one line each, until the context ends or
// the bridge hangs up.
func (*watchCommand) Run(ctx context.Context, root *rootCommand) error {
	creds, err := root.credentials()
	if err != nil {
		return err
	}

	bridge, err := root.open(ctx, creds)
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close() }()

	err = bridge.Watch(ctx, func(event caseta.Event) {
		switch told := event.(type) {
		case caseta.ZoneStatus:
			if told.Level != nil {
				terminal.Printf(root.out, "zone %s is at %d\n", told.Zone.ID(), *told.Level)
			}
		case caseta.ButtonEvent:
			terminal.Printf(root.out, "button %s %s\n", told.Button.ID(), strings.ToLower(told.Type))
		case caseta.Settled:
			terminal.Printf(root.out, "watching\n")
		}
	})

	if ctx.Err() != nil {
		return nil
	}

	return err
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

		// The lines are this command's; what is on each is the bridge's,
		// written so that it stays JSON.
		for line := range strings.SplitSeq(drawn(pretty), "\n") {
			terminal.Printf(root.out, "%s\n", line)
		}

		return err
	})
}

// drawn is a JSON document with every character a terminal would not
// simply draw written as JSON itself writes one, \uXXXX, a pair of them
// for a character past the sixteen bits: the document reads back the same,
// and holds nothing a terminal obeys. Such a character is inside a string:
// outside one, a document Go marshalled holds spaces and line ends alone,
// and the line ends are kept.
//
// What terminal.Text would write for them is Go's, and two of its forms,
// \x7f and \U000e0067, are no JSON.
func drawn(document []byte) string {
	var written strings.Builder

	for _, char := range string(document) {
		if char == '\n' || unicode.IsPrint(char) {
			written.WriteRune(char)
		} else {
			written.WriteString(escaped(char))
		}
	}

	return written.String()
}

// escaped is one character as JSON escapes it: \uXXXX, and a pair of
// them, its two halves, for a character sixteen bits do not hold.
func escaped(char rune) string {
	if high, low := utf16.EncodeRune(char); high != unicode.ReplacementChar {
		return fmt.Sprintf(`\u%04x\u%04x`, high, low)
	}

	return fmt.Sprintf(`\u%04x`, char)
}
