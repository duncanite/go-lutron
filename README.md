# go-lutron

A Go library and command for a Lutron Caséta bridge over LEAP, the protocol the bridge
speaks to Lutron's own app on the LAN: what the bridge holds, switches, dimmers, Pico
remotes and scenes; what each zone is at; the commands they take; and the events the
bridge sends when a zone changes or a button is pressed.

Lutron does not publish LEAP. What is here follows the open implementations that
reverse-engineered it. It has been run against one Caséta Smart Bridge: pairing, the
listing, zone levels, zone and Pico events, and setting a level, at once or over a fade,
worked; the rest has not been tried.
[docs/protocol.md](docs/protocol.md#run-against-a-bridge) lists both.

The command, run from the bridge's network:

```bash
export LUTRON_HOST=192.0.2.20 LUTRON_CREDENTIALS=~/.config/lutron
just lutron pair                          # press the button on the bridge when asked
just lutron devices                       # every device: model, serial, firmware, zone or buttons
just lutron level "Hallway Lights"        # by name, or by zone id
just lutron set "Hallway Lights" 40 --fade 2s
just lutron off 1
just lutron run 1                         # a scene
just lutron watch                         # zone changes and button presses, until interrupted
just lutron call ReadRequest /device      # anything else, as the bridge answers it
```

`LUTRON_CREDENTIALS` has no default: it holds a private key, so where it lives is said.
`pair` asks for the button on the bridge to be pressed, and writes the credentials every
other command uses. A bridge's pairing port takes only a client presenting the identity of
Lutron's own app, which is the same for every Caséta bridge; it is carried here, in
[leap/identity](leap/identity/README.md), which says whose it is and where it is from.

[docs/protocol.md](docs/protocol.md) is the protocol and where each part of it comes
from; [docs/design.md](docs/design.md) is the shape of the module and its decisions.
