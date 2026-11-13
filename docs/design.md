# go-lutron: the design

A Go module and one command, `lutron`, for a Lutron Caséta bridge over LEAP
([protocol.md](protocol.md)). Run from the bridge's network. **Nothing here has been run
against a bridge yet**: the protocol is the open implementations', and the tests run
against a bridge of this module's own making that answers as their recorded answers read.

## Shape

- `leap`: the wire. `Conn` sends a request under a tag and waits for the answer that
  carries it, so one connection serves several goroutines; `Subscribe` keeps its tag for
  the events that follow; `Unsolicited` takes what no request asked for. `Pair` is the
  exchange on the pairing port, and `TLSConfig` turns what it gave into what `Dial`
  needs. Standard library only.
- `leap/leaptest`: a bridge for tests. Both TLS ports under authorities made for the
  test, the pairing behind a button, LEAP over a table of resources, with commands
  recorded and events emitted on demand.
- `caseta`: a Caséta bridge's objects over that wire: devices, areas, buttons, scenes; a
  zone's level; the commands a zone, a button and a scene take; a button's events by
  subscription, and the zones' changes, which a Caséta bridge sends unasked.
- `cmd/lutron` and `internal/cli`: the command, kong under primordium's process setup.

## Decisions

- **The transport knows no object.** `leap` moves messages and matches tags; what a zone
  is lives in `caseta`. A RadioRA 3 or HomeWorks processor speaks the same wire with
  another arrangement of objects, and would be a package beside `caseta`, not a change to
  `leap`.
- **Handlers run on the reading goroutine.** An event is handed to its handler where it
  was read, in order, with no queue between: a queue has to be bounded, and a bound has
  to drop or block. The price is a rule: a handler returns promptly and makes no request
  of its own.
- **A subscription is registered before its request leaves,** so no event can arrive
  ahead of its handler, and a refused one is forgotten on the reading goroutine, so a
  message right behind the refusal finds none. The open implementations register after
  the answer; no record shows an event that close behind, and this is caution.
- **The bridge is verified by its chain and its use, not its name.** Its certificate
  names no host a client could check. The root pairing gave is the trust, and since that
  root signs every paired client too, the certificate must also be a server's: a
  client's, served, is refused.
- **The context bounds the write.** A bridge that stops reading would otherwise hold the
  connection's one writer for good. A write the context cuts short ends the connection,
  since half a line may be on the wire.
- **The app's identity is not in this repository.** The pairing port takes only a client
  carrying the certificate of Lutron's app. Other projects publish that certificate and
  its key in their source. `Pair` takes a connection made with it, and the command takes
  it as three files; whether to carry it here is a decision left open.
- **Ids are checked before they become URLs.** A zone, a button or a scene is addressed
  by an id spliced into a URL; one that is empty or holds a `/` or a `?` is refused
  before any request.
- **A target is an id or a name, and a name has to fit one zone.** The command takes a
  zone by its id, which wins, or by the name of the device that drives it, with or
  without its area, read from the bridge each run. A name two rooms share is refused
  with the zones it fits, never settled by which the bridge lists first.
- **`call` is the generic path.** Any request, any URL, the answer printed as the bridge
  sent it; the typed commands are conveniences over it, and what they do not cover is
  still reachable.
- **Credentials are files, the key private, the directory named.** `pair` writes the key,
  the certificate and the root into a directory, the key readable by its owner alone
  (its mode, and on Windows, where a mode does not say who may read a file, an access
  list of the current user alone),
  and refuses to overwrite a pairing unless told to. The directory has no default, so a
  private key never lands in whatever directory the command was run from, and `pair`
  makes it before asking for the button: a pairing granted and not written is lost.
- **watch reads each zone first.** The open implementations read every zone's status
  once before relying on the bridge to send its changes, and whether a bridge sends
  them to a connection that never asked is not known. `watch` does the same, and prints
  where each zone stands.
