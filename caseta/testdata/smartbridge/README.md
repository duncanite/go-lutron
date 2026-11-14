The five JSON files here are answers of a Caséta Smart Bridge, model `L-BDG2-WH`, LEAP
1.124, captured on 2026-11-14 with this module's own `lutron call ReadRequest <url>`: the
whole message as the bridge sent it, reindented. Four kinds of value were replaced and
nothing else: every area's name (`Home`, `Area 1` and on), every area's category, which
named the kind of room, all set to `Hallway`, one of the bridge's own words for it; every
device's name and the names in its `FullyQualifiedName` (`Dimmer 1`, `Pico 2` and on);
and every serial number (20000000 plus the device's number). Structure, keys,
references, model numbers, product ids and firmware names are the bridge's, the
bridge's firmware install time with its UTC offset among them.

The bridge held fourteen wall dimmers, two wall switches, an outdoor plug-in switch and
three five-button Pico remotes, in eighteen areas, and no programmed scene: its hundred
scene slots carry Lutron's default names.
