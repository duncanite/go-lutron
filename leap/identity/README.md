# The identity a Caséta bridge's pairing port expects

These three files are Lutron's, not this project's, and nothing here licenses them:

- `caseta-app.pem` and `caseta-app-key.pem`: the certificate and private key Lutron's own
  app presents to a Caséta bridge's pairing port. Subject `CN=Caseta Application`,
  `O=Lutron Electronics Co., Inc.`.
- `caseta-pairing-root.pem`: the authority that issued it, and that the pairing port's
  own certificate chains to. `CN=Caseta Local Access Protocol Cert Authority`.

The open implementations present this identity to every Caséta bridge's pairing port, and
carry it in their source; that a bridge refuses a client without it has not been tested
here. The key is published, so it is no secret, but it is not nothing. Its certificate
names a server's use as well as a client's and chains to the root a pairing client
checks, so whoever holds the key can reach a pairing port and can also stand in for one.
What guards a pairing is therefore the button on the bridge and the network the pairing
runs on, not the check of the pairing port's certificate. That is Lutron's design, and
true of every client of the other implementations.

Copied unchanged from [pylutron-caseta](https://github.com/gurumitts/pylutron-caseta)'s
`src/pylutron_caseta/assets.py` at commit `1c25f55bef14adf0bac81c2bba43ea4f7e6826b1`,
where they are `LAP_CERT_PEM`, `LAP_KEY_PEM` and `LAP_CA_PEM`.

They will be removed from this repository at Lutron's request. A public module's
versions, once fetched through the Go module proxy, stay served there whatever the
repository later removes; that is beyond what a removal here can undo.
