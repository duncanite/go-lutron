# go-lutron: the design

A Go module and one command, `lutron`, for a Lutron Caséta bridge over LEAP
([protocol.md](protocol.md)). Run from the bridge's network. The protocol is the open
implementations', and the tests run against a bridge of this module's own making that
answers as their recorded answers read. The command has been run against one Caséta
Smart Bridge, and [protocol.md](protocol.md#run-against-a-bridge) lists what worked there
and what was not tried.

## Shape

- `leap`: the wire. `Conn` sends a request under a tag and waits for the answer that
  carries it, so one connection serves several goroutines; `Subscribe` keeps its tag for
  the events that follow; `Unsolicited` takes what no request asked for. `Pair` is the
  exchange on the pairing port, and `TLSConfig` turns what it gave into what `Dial`
  needs. Standard library only.
- `leap/leaptest`: a bridge for tests. Both TLS ports under authorities made for the
  test, the pairing behind a button, LEAP over a table of resources, with commands
  recorded and events emitted on demand.
- `caseta`: a Caséta bridge's objects over that wire. `Open` takes an address and the
  credentials and gives a `Bridge`, which has the bridge's `ID`; devices, with the
  firmware each runs, areas, buttons, scenes; `WriteDevices`, the listing a command
  prints; a zone's level; the commands a zone, a button and a scene take; `Watch`, how
  things stand and then what happens; `Close`, `Done` and `Err`. `Recognize` and `Find`
  tell a bridge on the network, through a `Browser` the caller lends.
- `cmd/lutron` and `internal/cli`: the command, kong under primordium's process setup.
- `internal/credentials`: what pairing gave, as three files in a directory, the key
  private: `Reserve` before the bridge's button, `Save` after, `Load` for every later
  run. It is the command's: the library reads and writes no file, and takes and gives
  credentials as bytes.

## Decisions

- **The transport knows no object.** `leap` moves messages and matches tags; what a zone
  is lives in `caseta`. A RadioRA 3 or HomeWorks processor speaks the same wire with
  another arrangement of objects, and would be a package beside `caseta`, not a change to
  `leap`.
- **The wire's handlers run on the reading goroutine.** In `leap`, an event is handed to
  its handler where it was read, in order, with no queue between: a queue has to be
  bounded, and a bound has to drop or block. The price is a rule: a handler returns
  promptly and makes no request of its own.
- **`caseta` pays that price once, for its callers.** `Watch` tells its handler on the
  goroutine that called it, where the handler may take its time and ask things of the
  bridge. Between the two is a queue that is bounded by what it holds: one status a
  zone, the last, since a level applied twice does no harm; and every button's press,
  up to a thousand, past which the watch ends with an error and the bridge still
  stands.
- **A zone's status takes its place by when the bridge sent it.** `Watch` reads each
  zone while the bridge may be sending that zone's changes; the answer comes back to
  the reader and the changes to their handler, on two goroutines, and which reaches the
  queue first says nothing. Every message carries its place among those the connection
  read (`leap.Message.Received`), and of two statuses of a zone the later one stays.
- **Lost is not closed, and neither is refused.** A connection its owner closed is
  `ErrClosed`; one that ended by itself is `ErrLost` with its cause; credentials the
  bridge does not take, or that do not take the bridge, are `ErrCredentials`, which
  trying again does not cure. Nothing dials again by itself. A bridge's refusal is
  known by the TLS alert that says it, which under TLS 1.3 comes after the client's
  handshake, at its first read; a bridge that hung up with no alert, or one whose alert
  a reset of the connection overtook, which Windows then discards unread, reads as
  lost: nothing sets it apart from a connection that failed.
- **A bridge is its serial.** `Bridge.ID` is the serial number the bridge gives in its
  own entry, the first of its devices, read when the bridge is opened: the same
  whatever its address. A bridge says it on the network too, in its host's name, and
  `Recognize` reads it there: the same value before a connection and after, so a bridge
  is adopted by who it is and found wherever it is.
- **An announcement is a claim, and the connection is the proof.** Anyone on the network
  can announce a host named for a bridge's serial, at an address of their choosing.
  `Found` is what a host says of itself. `Open` under the credentials pairing gave
  takes only what serves a certificate for a server's use that the pairing's root
  signs; on the one bridge seen that root is the bridge's own, and where a root is
  shared by a maker's bridges another bridge passes that check, so it is `Bridge.ID`,
  read once opened, that settles which bridge answered. So an `ErrCredentials` at an
  address that was found says that what answers there is not the paired bridge, or that
  the bridge takes the pairing no more: a consumer does not conclude from it alone that
  its pairing is void.
- **Finding a bridge needs a means this module does not have, and does not take.** A
  host has one Multicast DNS responder, and it belongs to the program, not to a
  library: a second one beside it is what must not happen. `Recognize` is the knowledge,
  with no socket: an announcement in, a bridge's identity and addresses out. `Find` is
  the search, through a `Browser` the caller lends, and keeps nothing between two of the
  browser's calls: a bridge is told again whenever the browser tells of it again, and
  one gone is told when it comes as it last stood. `Browser` and `Instance` are
  declared here, in this module's own terms, and name no Multicast DNS library: whoever
  has one satisfies them with a few lines. The same declarations are written again in
  each library of the family that is found this way; that duplication is the price of
  none of them depending on another.
- **The addresses found are LEAP's, not the announced one, and all of them.** A bridge
  announces port 22. `Found.Addresses` is every address the bridge's host has that can
  be dialled, in the order they came, or its host's name when there is none, each with
  LEAP's port. A host has several, and which one reaches the bridge depends on where
  the caller stands, which this module does not know: it does not pick.
- **A Caséta bridge is not known to say that its devices changed.** `Watch` tells the
  devices and the buttons as they were when it started; a device added, removed or
  renamed is seen by watching again.
- **How things stand ends with a mark.** `Watch` tells every device, every button and
  what every zone is at, then `Settled`, once, then what happens. A consumer that kept
  devices from before removes the ones it was not told of by then, and only then: no
  event of another kind says it, since a bridge with no zone and no button has none to
  send.
- **The mark is a promise, and what is left out has to keep it.** By `Settled` a
  consumer may forget the devices and the buttons it was not told of, so only what the
  bridge says it has none of is left out of them: no button, when it answers that it has
  none, a 404 or a list that is empty, with or without a body. Buttons refused any
  other way, the devices refused or listing none, and an answer that cannot be read end
  the `Watch`: watching again is the answer to a bridge that was busy. A zone's status
  is another matter, since the zone is known from its device and nothing is forgotten
  on its account: a zone whose status the bridge refuses, or answers nothing of, is told
  no status, where failing would fail every `Watch` the same way, for good. A button
  whose presses the bridge will not tell ends the `Watch`, since nothing would show
  them missing.
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
- **The Caséta app's identity is carried.** The open implementations present one
  certificate and key, Lutron's, to every Caséta bridge's pairing port, and publish them
  in their source. They are embedded, with
  a note beside them saying they are Lutron's, where they were copied from, and that they
  go from the repository at Lutron's request. The key is no secret but not nothing: its
  certificate also names a server's use, so its holder can stand in for a pairing port.
  The button on the bridge and the network guard a pairing, not the check of that port's
  certificate. `pair` takes another identity as three files, which is how a processor's
  would be given.
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
- **Watching reads each zone first.** The open implementations read every zone's status
  once before relying on the bridge to send its changes, and whether a bridge sends
  them to a connection that never asked is not known. `Watch` does the same, and the
  command's `watch` prints where each zone stands.
