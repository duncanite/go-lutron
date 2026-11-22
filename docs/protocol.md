# LEAP, as the open implementations speak it

LEAP is the protocol a Lutron bridge speaks to Lutron's own app on the LAN. Lutron names
it ([Lutron's LEAP API integration
protocol](https://support.lutron.com/us/en/product/radiora3/article/integration/Lutron-s-LEAP-API-Integration-Protocol))
and gives its documentation to approved partners only. Everything below is read from the
open implementations that reverse-engineered it, first among them
[pylutron-caseta](https://github.com/gurumitts/pylutron-caseta), the library Home
Assistant uses, with the answers its tests replay, under `tests/responses` (called its
recorded answers below, though it does not say how they were made: most of their
identifiers are placeholders, one serial reads as real), and
[lutron-leap-js](https://github.com/thenewwazoo/lutron-leap-js). Part of it has since been
run against one bridge, as [Run against a bridge](#run-against-a-bridge) lists; the rest
has not. Where a statement is this module's assumption and not something those sources or
that run show, it says so.

## Which bridges

| system | bridge | LEAP | the older Telnet protocol (LIP) |
| --- | --- | --- | --- |
| Caséta | Smart Bridge (L-BDG2) | yes | no |
| Caséta | Smart Bridge Pro (L-BDGPRO2) | yes | yes, when switched on in the app |
| RA2 Select | main repeater | yes | yes |
| RadioRA 3 | processor | yes | no |
| HomeWorks QSX | processor | yes | no |

LIP is plain text over Telnet on port 23, documented by Lutron, and covers less: it has
no occupancy groups and cannot list what the bridge holds. LEAP is the one protocol every
current bridge speaks, so it is the one this module implements.

The Caséta family and the RadioRA 3 and HomeWorks family speak the same LEAP with
different object models: a Caséta bridge lists every device and button flat, the
processors arrange them under areas and control stations. The recorded answers cover
both; only the Caséta shape is described here so far.

## The wire

TLS 1.2 or later on port **8081**, with a client certificate. Inside it, one JSON object
per line, each way, ended by `\r\n` from the client.

A message has three parts:

```json
{"CommuniqueType": "ReadRequest", "Header": {"ClientTag": "t1", "Url": "/device"}}
```

```json
{"CommuniqueType": "ReadResponse",
 "Header": {"ClientTag": "t1", "Url": "/device", "StatusCode": "200 OK", "MessageBodyType": "MultipleDeviceDefinition"},
 "Body": {"Devices": [...]}}
```

- `CommuniqueType` is one of `ReadRequest`, `CreateRequest`, `UpdateRequest`,
  `SubscribeRequest`; the answer's is the same with `Response`.
- `Header.ClientTag` is the client's, any string; the bridge echoes it on the answer, so
  several requests can be in flight on one connection.
- `Header.StatusCode` is a number and a word, `200 OK`, `204 NoContent`; the 200s are
  success.
- `Header.MessageBodyType` names the body's shape, and the body has one key matching it.

A subscription is a `SubscribeRequest` for a URL. The bridge answers it once, and from
then on sends a message with the same `ClientTag` each time what the URL names changes.
The bridge also sends messages with no tag, which no request asked for.

An answer may be one page of several. Its header then carries a `Paging`, and the same
request with that `Paging` in its header is answered with what follows, until an answer
carries none:

```json
{"CommuniqueType": "ReadResponse",
 "Header": {"MessageBodyType": "MultipleAreaDefinition", "StatusCode": "200 OK", "Url": "/area",
            "Paging": {"ContinuationToken": "..."}},
 "Body": {"Areas": [...]}}
```

That header was reported from a HomeWorks QSX processor that holds more than 99 areas
([the report](https://github.com/gurumitts/pylutron-caseta/issues/152#issuecomment-1928150976),
and [what sending it back
gave](https://github.com/gurumitts/pylutron-caseta/issues/152#issuecomment-1928654717)).
pylutron-caseta has since repeated every request for as long as an answer carries a
`Paging`, and joined the lists the bodies hold
([`_request`](https://github.com/gurumitts/pylutron-caseta/blob/1c25f55bef14adf0bac81c2bba43ea4f7e6826b1/src/pylutron_caseta/smartbridge.py#L284-L319)).
This module does the same for a collection, up to 256 pages. Whether a Caséta bridge
pages anything, and what a bridge answers a `Paging` it did not give, is not known: the
answers [captured from one](../caseta/testdata/smartbridge/README.md), 21 devices, 18
areas, 15 buttons and 100 scene slots, each came whole, with no `Paging`. Whether the
answer to a subscription pages is not known either; pylutron-caseta reads it as one
message, and so does this module.

What a bridge answers for a URL it does not have was not in the recorded answers.

## Pairing

A new client gets its certificate on port **8083**:

1. The client connects over TLS with a fixed client certificate and key, the identity of
   Lutron's app, and verifies the bridge against a fixed authority that goes with it.
   The open implementations carry that certificate, key and authority in their source;
   RadioRA 3 processors are verified against a different Lutron root.
2. The bridge sends nothing until a person presses the button on it. Then it sends a
   status whose `Header.ContentType` starts with `status;` and whose
   `Body.Status.Permissions` holds `PhysicalAccess`.
3. The client makes a key, here RSA 2048 as the others do, and sends a signing request:

   ```json
   {"Header": {"RequestType": "Execute", "Url": "/pair", "ClientTag": "get-cert"},
    "Body": {"CommandType": "CSR",
             "Parameters": {"CSR": "<PEM>", "DisplayName": "<name>", "DeviceUID": "000000000000", "Role": "Admin"}}}
   ```

4. The bridge answers under the same tag with `Body.SigningResult.Certificate`, the
   client's certificate, and `Body.SigningResult.RootCertificate`, the root the bridge's
   own certificate on port 8081 chains to.

The three things kept are the client's key, its certificate and that root. The bridge's
certificate names no host a client could check, so the open implementations verify its
chain to the root without a name and, through the OpenSSL they sit on, hold it to a
server's use when it names its uses. This module does both: the same root signs every
paired client, and a client's certificate must not pass for the bridge.

That is a Caséta bridge. For a RadioRA 3 processor, whose pairing port verifies against
Lutron's root and not the app's authority, pylutron-caseta discards the root the
processor answers with and keeps Lutron's root for port 8081. This module's `Pair` keeps
what is answered, always, and has not been written for a processor.

This module's `Pair` takes a connection already made to port 8083, and `CasetaApp` is
the identity to make it with for a Caséta bridge: Lutron's, copied from pylutron-caseta,
as [leap/identity](../leap/identity/README.md) says.

## What a Caséta bridge holds

Read with `ReadRequest`. Every object has an `href`, its own URL, and refers to others by
theirs.

| URL | body | what |
| --- | --- | --- |
| `/server/1/status/ping` | `PingResponse.LEAPVersion` | the bridge is there, and its protocol version |
| `/project` | `Project` | the project's name and product type |
| `/device` | `Devices[]` | every device: `Name`, `FullyQualifiedName[]` (area, then name), `SerialNumber`, `ModelNumber`, `DeviceType`, `LocalZones[]`, `AssociatedArea`, `ButtonGroups[]`, `FirmwareImage` (the bridge's own version under `Firmware.DisplayName`, another device's under `Contents[].OS.Firmware.DisplayName`; seen on a bridge, absent from pylutron-caseta's fixtures) |
| `/area` | `Areas[]` | rooms: `Name` |
| `/button` | `Buttons[]` | every button of every keypad and Pico: `ButtonNumber`, `Name`, `Parent` its button group |
| `/virtualbutton` | `VirtualButtons[]` | scenes: `Name`, `IsProgrammed` |
| `/occupancygroup` | `OccupancyGroups[]` | groups of occupancy sensors, by area |
| `/zone/<id>/status` | `ZoneStatus` | a zone's `Level` 0 to 100, or `FanSpeed`, `Tilt`, `CCOLevel` by its kind |
| `/system/away/1/status` | `AwayStatus.EnabledState` | Smart Away |

A device with a load has a zone, and the zone is what is commanded and what reports a
level. A Pico has no zone: it has buttons, and its presses are events.

## Commands

A command is a `CreateRequest` to a command processor:

| URL | `Body.Command` | what |
| --- | --- | --- |
| `/zone/<id>/commandprocessor` | `{"CommandType": "GoToLevel", "Parameter": [{"Type": "Level", "Value": 50}]}` | a level, at once |
| same | `{"CommandType": "GoToDimmedLevel", "DimmedLevelParameters": {"Level": 50, "FadeTime": "00:00:02"}}` | a level, fading |
| same | `{"CommandType": "Raise"}`, `"Lower"`, `"Stop"` | as a held button does |
| same | `{"CommandType": "GoToFanSpeed", ...}`, `"GoToTilt"`, `"GoToCCOLevel"` | fans, blinds, contact closures |
| `/button/<id>/commandprocessor` | `{"CommandType": "PressAndRelease"}` | press a button |
| `/virtualbutton/<id>/commandprocessor` | `{"CommandType": "PressAndRelease"}` | run a scene |

Smart Away is an `UpdateRequest` to `/system/away/1/status` with
`{"AwayStatus": {"EnabledState": "Enabled"}}`.

The statuses a bridge gives a command were not in the recorded answers.

## Events

A Caséta bridge is not subscribed to for its zones' changes: it sends them with no tag,
a `ReadResponse` whose `MessageBodyType` is `OneZoneStatus` and whose body is
`ZoneStatus`. pylutron-caseta reads each zone's status once at the start and takes the
rest from those. Whether that first read is what makes the bridge send a zone's changes
to a connection, or whether every connection gets them, is not shown. `/zone/status` is a subscription of the RadioRA 3 and
HomeWorks processors, and sends `ZoneStatuses`, a list.

The subscriptions a Caséta bridge does take:

| subscription | what arrives |
| --- | --- |
| `/button/<id>/status/event` | `ButtonStatus.ButtonEvent.EventType`, `Press` or `Release`; the subscription itself answers `204 NoContent` |
| `/occupancygroup/status` | occupancy groups' states |
| `/system/away/1/status` | Smart Away |

## Run against a bridge

On 2026-11-14 the command of this module was run against one bridge: a Caséta Smart
Bridge, model `L-BDG2-WH`, answering LEAP version 1.124, holding seventeen zones (wall
dimmers, wall switches, an outdoor plug-in switch) and three five-button Pico remotes.

What worked, each as this document describes it:

- The pairing port presents a certificate with subject `CN=Caseta Smart Bridge`, issued
  by `CN=Caseta Local Access Protocol Cert Authority`, naming a server's use and a
  client's. Pairing under the identity in [leap/identity](../leap/identity/README.md),
  with the button pressed, gave credentials that port 8081 took.
- `/server/1/status/ping`, `/device` and `/button` read into this module's types: every
  device with its type, model and serial, every load with its zone, each Pico with five
  buttons numbered 0 to 4. Device and zone numbers are not contiguous.
- `/zone/<id>/status` for each of the seventeen zones.
- A zone's change arrived with no tag, on a connection that had read that zone first.
  Whether it arrives on one that had not was not tried.
- A subscription to `/button/<id>/status/event`, for each of the fifteen buttons: a
  press arrives as `Press`, then `Release`. On the Pico tried, button number 0 turned
  its light on and number 2 turned it off.
- `GoToLevel` to a dimmer's zone, to 30 and to 0: the light followed.
- `GoToDimmedLevel` to a dimmer's zone, with a `FadeTime` of two seconds and of ten, up and
  down: the light took that long to arrive, by eye, in both directions. A short fade
  upwards looks quicker than the same one downwards; over ten seconds both are plainly
  ten.
- `/device` carried each device's `FirmwareImage`, the bridge's own version and every
  other device's, the Picos' as `000.000.000r000`.

What a bridge with no Pico answers for `/button` was not seen: this one has three.
pylutron-caseta reads that answer as a body in which `Buttons` is absent, by the comment
in its `_load_buttons`, and reads other collections a bridge has none of, occupancy groups
among them, as an answer with no body at all. This module takes both for none.

What was not tried: `Raise`, `Lower` and `Stop`, pressing a button or running a scene by
command, Smart Away, occupancy groups, a request for a URL the bridge lacks, pairing
without the identity, a signing request sent before the button, and credentials the
bridge no longer takes: which TLS version it speaks, and whether it refuses a client's
certificate with an alert or by hanging up, is not recorded. That bridge has no
programmed scene: `/virtualbutton` answers a hundred slots under Lutron's default names,
each with `IsProgrammed` false.

On 2026-11-15, the same bridge, reached by its mDNS name in place of its address:

- Port 8081 presented a self-signed certificate, subject and issuer
  `CN=lutron-4294967295`, which the root pairing gave does not sign: this module refused
  it, and so did OpenSSL under the same root.
- Reached by its address, so that no server name was sent, it presented its own
  certificate and the command listed its devices.
- The root pairing gave for that bridge is self-signed, and its subject is `SmartBridge`
  followed by twelve hexadecimal digits, which are the Ethernet address the bridge
  announces as `MACADDR`: a root of that bridge's own. Whether every Caséta bridge has
  one of its own was not seen, and a RadioRA 3 processor's is Lutron's, shared.

- Reached by that same mDNS name by this module once it sent no server name, with the
  same credentials, it presented its own certificate again and the command listed its
  devices.

So a bridge chooses its certificate by the server name it is asked for, and a client
sends none: `leap.DialTLS` makes the connection that way whatever the host is written as.
Whether the pairing port does the same was not tried; it is dialled the same way.

## On the network

A bridge announces itself over Multicast DNS. On 2026-11-15 the same bridge announced:

- one instance, `Lutron Status`, of the service `_lutron._tcp`;
- on a host named `Lutron-`, then eight hexadecimal digits, `.local`;
- at port 22, which is not LEAP's: LEAP is on 8081 whatever is announced;
- with a TXT record of seven entries: `MACADDR`, its Ethernet address; `CODEVER`, the
  firmware version `/device` gives for the bridge; `DEVCLASS`; `FW_STATUS`, `NW_STATUS`
  and `ST_STATUS`, its own view of its updates, its network and itself; and `SYSTYPE`,
  `SmartBridge`.

The eight digits are the bridge's serial number: read as hexadecimal they are the number
`/device` gives in the bridge's own entry, checked on that bridge. So a bridge says who it
is before any connection, and `caseta.Recognize` reads it there. It is the host's word,
which any host can give: [design.md](design.md#decisions) says what proves it.

Home Assistant's integration reads the same two things
([its manifest and its configuration flow](https://github.com/home-assistant/core/tree/dbd98fabb3b9699714e4bdd5431d1316696f22e6/homeassistant/components/lutron_caseta)):
it takes a host named `lutron-` and keeps what follows as the bridge's identity, and it
tells Lutron's systems apart by `SYSTYPE`, a Caséta bridge's beginning with
`smartbridge` and the others' with `hwqs`, `radiora3`, `ra2select` and `athena`. Only the
first was seen here.

Its answers for `/device`, `/area`, `/button`, `/virtualbutton` and one zone's status are
kept as fixtures, names and serials replaced, under
[caseta/testdata/smartbridge](../caseta/testdata/smartbridge/README.md). A real answer
carries many keys the tables above do not list: a device's firmware, links, load
controllers and rules, an area's category and settings.
