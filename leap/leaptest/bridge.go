// Package leaptest is a Lutron bridge for tests: the two TLS ports, the
// pairing exchange behind a button, and LEAP over a table of resources
// the test fills in. It answers the way the open implementations'
// recorded answers read; where one of its answers was not in those
// records, the code says so.
package leaptest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/duncanite/go-lutron/leap"
)

// PingURL is the resource every bridge answers; a new Bridge has it,
// answering LEAPVersion.
const (
	PingURL     = "/server/1/status/ping"
	LEAPVersion = 1.115
)

// DevicesURL is the bridge's devices; a new Bridge holds one there,
// itself.
const DevicesURL = "/device"

// Serial is the serial number a new Bridge gives itself.
const Serial = 1001

// lingering is how long the bridge waits for a client it refused to hang
// up first.
const lingering = 5 * time.Second

// Itself is the bridge's own entry among its devices, which a bridge
// lists first: a test that sets the devices puts it there.
func Itself() map[string]any {
	return map[string]any{
		"href": "/device/1", "Name": "Smart Bridge", "FullyQualifiedName": []string{"Smart Bridge"},
		"DeviceType": "SmartBridge", "ModelNumber": "L-BDG2-WH", "SerialNumber": Serial,
	}
}

// What the bridge writes in its answers.
const (
	readResponse = "ReadResponse"
	statusOK     = "200 OK"
)

// pairingRequest is what a client sends the pairing port, as far as the
// bridge reads it.
type (
	pairingRequest struct {
		Header pairingHeader `json:"Header"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Body   pairingBody   `json:"Body"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairingHeader struct {
		RequestType string `json:"RequestType"` //nolint:tagliatelle // the bridge's keys are PascalCase
		ClientTag   string `json:"ClientTag"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairingBody struct {
		CommandType string            `json:"CommandType"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Parameters  pairingParameters `json:"Parameters"`  //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairingParameters struct {
		CSR         string `json:"CSR"`         //nolint:tagliatelle // the bridge's keys are PascalCase
		DisplayName string `json:"DisplayName"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
)

// Command is one request that changes something, as the bridge got it.
type Command struct {
	CommuniqueType string
	URL            string
	Body           json.RawMessage
}

type resource struct {
	bodyType string
	body     json.RawMessage
}

type subscription struct {
	client *client
	tag    string
	url    string
}

// client is one LEAP connection; write serializes lines onto it.
type client struct {
	conn  net.Conn
	write sync.Mutex
}

func (c *client) send(message *leap.Message) {
	line, err := json.Marshal(message)
	if err != nil {
		return
	}

	c.write.Lock()
	defer c.write.Unlock()

	_, _ = c.conn.Write(append(line, '\r', '\n'))
}

// Bridge is the fake, listening until Close.
type Bridge struct {
	authority *authority
	bootstrap *authority
	app       tls.Certificate
	appPEM    [2][]byte
	// address is the LEAP port's, kept so that a bridge that was away
	// comes back where it was.
	address string
	secured *tls.Config
	pairing net.Listener
	pressed chan struct{}
	press   sync.Once

	mu sync.Mutex
	// leap is the LEAP port's listener; nil while the bridge is away.
	leap net.Listener
	// signer signs the clients' certificates, and is whom the LEAP port
	// takes them from: Revoke replaces it.
	signer        *authority
	refused       map[string]string
	resources     map[string]resource
	commands      []Command
	subscriptions []subscription
	clients       []*client
	paired        []string
	garbled       bool
}

// New starts a bridge on two loopback ports.
func New() *Bridge {
	bridge := &Bridge{
		authority: newAuthority("Lutron test bridge root"),
		bootstrap: newAuthority("Lutron test pairing root"),
		pressed:   make(chan struct{}),
		refused:   map[string]string{},
		resources: map[string]resource{},
	}
	bridge.signer = bridge.authority
	bridge.app, bridge.appPEM = bridge.bootstrap.issuePEM("the app", x509.ExtKeyUsageClientAuth)

	served := bridge.authority.issue("bridge", x509.ExtKeyUsageServerAuth)

	// Read at each handshake: whom the port takes a client from changes
	// when the clients are revoked.
	bridge.secured = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			bridge.mu.Lock()
			defer bridge.mu.Unlock()

			return &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{served},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    bridge.signer.pool(),
			}, nil
		},
	}
	bridge.leap = listen(bridge.secured, "127.0.0.1:0")
	bridge.address = bridge.leap.Addr().String()
	bridge.pairing = listen(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{bridge.bootstrap.issue("bridge", x509.ExtKeyUsageServerAuth)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    bridge.bootstrap.pool(),
	}, "127.0.0.1:0")

	bridge.Set(PingURL, "OnePingResponse", map[string]any{"PingResponse": map[string]any{"LEAPVersion": LEAPVersion}})
	bridge.Set(DevicesURL, "MultipleDeviceDefinition", map[string]any{"Devices": []map[string]any{Itself()}})

	go accept(bridge.leap, bridge.serve)
	go accept(bridge.pairing, bridge.pair)

	return bridge
}

// Addr is the LEAP port's address, host and port.
func (b *Bridge) Addr() string { return b.address }

// PairingAddr is the pairing port's address, host and port.
func (b *Bridge) PairingAddr() string { return b.pairing.Addr().String() }

// Bootstrap is the TLS configuration the pairing port takes: the
// identity a real bridge expects of Lutron's app, here the test's own.
func (b *Bridge) Bootstrap() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{b.app},
		RootCAs:      b.bootstrap.pool(),
		ServerName:   "bridge",
	}
}

// BootstrapPEM is the identity Bootstrap carries, as the three PEM blocks
// a client reading it from files needs: the app's certificate, its key,
// and the root the pairing port's certificate chains to.
func (b *Bridge) BootstrapPEM() (certificate, key, root []byte) {
	return b.appPEM[0], b.appPEM[1], b.bootstrap.pem()
}

// Credentials issues credentials the LEAP port takes, without a pairing.
func (b *Bridge) Credentials() *leap.Credentials {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}

	b.mu.Lock()
	signer := b.signer
	b.mu.Unlock()

	return &leap.Credentials{
		Key:         pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
		Certificate: signer.sign("a test client", &key.PublicKey, x509.ExtKeyUsageClientAuth),
		Root:        b.authority.pem(),
	}
}

// Revoke makes the bridge forget every client: the credentials it issued
// or paired so far are refused at the LEAP port, as a bridge's are after
// it is reset, and those it issues from now on are taken. Connections
// already made stay.
func (b *Bridge) Revoke() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.signer = newAuthority("Lutron test bridge clients, anew")
}

// Refuse makes the bridge answer every request for url, of any kind, with
// status, "500 InternalServerError" or any other outside the 200s; an
// empty status makes it answer as before.
func (b *Bridge) Refuse(url, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if status == "" {
		delete(b.refused, url)

		return
	}

	b.refused[url] = status
}

// Away takes the bridge off the network: it hangs up on every LEAP
// connection and refuses new ones, until Back.
func (b *Bridge) Away() {
	b.mu.Lock()
	listener := b.leap
	b.leap = nil
	b.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}

	b.Drop()
}

// Back puts a bridge that was away on the network again, at the address
// it had.
func (b *Bridge) Back() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.leap != nil {
		return
	}

	b.leap = listen(b.secured, b.address)

	go accept(b.leap, b.serve)
}

// Press presses the bridge's button: pairing connections waiting for it
// go on. A real bridge forgets the press after a while; this one does
// not.
func (b *Bridge) Press() {
	b.press.Do(func() { close(b.pressed) })
}

// Garble makes the bridge answer a signing request with a certificate
// that is not one.
func (b *Bridge) Garble() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.garbled = true
}

// Paired is the display name of every client the bridge has signed for.
func (b *Bridge) Paired() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.paired...)
}

// Set makes url a resource: a read of it answers 200 with body under
// bodyType, and a subscription to it answers the same.
func (b *Bridge) Set(url, bodyType string, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.resources[url] = resource{bodyType: bodyType, body: raw}
}

// Commands is every create and update request received, in order.
func (b *Bridge) Commands() []Command {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]Command(nil), b.commands...)
}

// Emit sends an event to every subscription to url, each under its own
// tag: the message a bridge sends when what a subscription watches
// changes.
func (b *Bridge) Emit(url, bodyType string, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}

	b.mu.Lock()
	subscriptions := append([]subscription(nil), b.subscriptions...)
	b.mu.Unlock()

	for _, sub := range subscriptions {
		if sub.url != url {
			continue
		}

		sub.client.send(&leap.Message{
			CommuniqueType: readResponse,
			Header:         leap.Header{ClientTag: sub.tag, URL: url, StatusCode: statusOK, MessageBodyType: bodyType},
			Body:           raw,
		})
	}
}

// Unsolicited sends a message with no tag to every connection.
func (b *Bridge) Unsolicited(url, bodyType string, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}

	b.mu.Lock()
	clients := append([]*client(nil), b.clients...)
	b.mu.Unlock()

	for _, connected := range clients {
		connected.send(&leap.Message{
			CommuniqueType: readResponse,
			Header:         leap.Header{URL: url, StatusCode: statusOK, MessageBodyType: bodyType},
			Body:           raw,
		})
	}
}

// Connected is how many LEAP connections the bridge holds now.
func (b *Bridge) Connected() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.clients)
}

// Drop hangs up on every LEAP connection, as a bridge that restarts.
func (b *Bridge) Drop() {
	b.mu.Lock()
	clients := b.clients
	b.clients = nil
	b.subscriptions = nil
	b.mu.Unlock()

	for _, connected := range clients {
		_ = connected.conn.Close()
	}
}

// Close stops both ports and hangs up.
func (b *Bridge) Close() {
	b.Away()

	_ = b.pairing.Close()
	b.Press()
	b.Drop()
}

// serve is one LEAP connection.
func (b *Bridge) serve(conn net.Conn) {
	connected := &client{conn: conn}

	b.mu.Lock()
	b.clients = append(b.clients, connected)
	b.mu.Unlock()

	defer func() {
		_ = conn.Close()

		b.mu.Lock()
		defer b.mu.Unlock()

		for index, held := range b.clients {
			if held == connected {
				b.clients = append(b.clients[:index], b.clients[index+1:]...)

				return
			}
		}
	}()

	if secured, ok := conn.(*tls.Conn); ok {
		if err := secured.Handshake(); err != nil { //nolint:noctx // the listener's own connection, ended by Close
			refuse(secured.NetConn())

			return
		}
	}

	reader := bufio.NewReader(conn)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}

		var request leap.Message
		if json.Unmarshal(line, &request) != nil {
			return
		}

		connected.send(b.answer(connected, &request))
	}
}

func (b *Bridge) answer(from *client, request *leap.Message) *leap.Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	answer := &leap.Message{Header: leap.Header{ClientTag: request.Header.ClientTag, URL: request.Header.URL}}
	found, known := b.resources[request.Header.URL]

	if status, refused := b.refused[request.Header.URL]; refused {
		answer.CommuniqueType = "ExceptionResponse"
		answer.Header.StatusCode = status

		return answer
	}

	switch request.CommuniqueType {
	case leap.Read:
		answer.CommuniqueType = readResponse

		if !known {
			// What a bridge answers for a resource it lacks was not in
			// the records; a status outside the 200s is all that is
			// relied on.
			answer.Header.StatusCode = "404 NotFound"

			return answer
		}

		answer.Header.StatusCode = statusOK
		answer.Header.MessageBodyType = found.bodyType
		answer.Body = found.body
	case leap.Subscribe:
		answer.CommuniqueType = "SubscribeResponse"

		b.subscriptions = append(b.subscriptions, subscription{
			client: from, tag: request.Header.ClientTag, url: request.Header.URL,
		})

		if !known {
			// Seen for a button's events: accepted, with nothing to say yet.
			answer.Header.StatusCode = "204 NoContent"

			return answer
		}

		answer.Header.StatusCode = statusOK
		answer.Header.MessageBodyType = found.bodyType
		answer.Body = found.body
	case leap.Create, leap.Update:
		b.commands = append(b.commands, Command{
			CommuniqueType: request.CommuniqueType, URL: request.Header.URL, Body: request.Body,
		})

		// The statuses a bridge gives a command were not in the records.
		answer.CommuniqueType = "CreateResponse"
		answer.Header.StatusCode = "201 Created"

		if request.CommuniqueType == leap.Update {
			answer.CommuniqueType = "UpdateResponse"
			answer.Header.StatusCode = statusOK
		}
	default:
		answer.CommuniqueType = "ExceptionResponse"
		answer.Header.StatusCode = "400 BadRequest"
	}

	return answer
}

// refuse ends a connection whose handshake the bridge failed, so that
// the alert saying why is read. Under TLS 1.3 a client's certificate is
// refused after the client's own handshake is over, with its first
// request on the way: closing with that request unread resets the
// connection, and on Windows a reset discards what the client had not
// read yet, the alert with it. So the bridge sends no more, and reads
// until the client hangs up.
func refuse(conn net.Conn) {
	if sending, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = sending.CloseWrite()
	}

	_ = conn.SetReadDeadline(time.Now().Add(lingering))
	_, _ = io.Copy(io.Discard, conn)
}

// pair is one pairing connection: nothing until the button, then the
// permission, then a certificate for the request that comes.
func (b *Bridge) pair(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// The handshake is not waited on the button: a server's happens at
	// its first read or write, and this one's first write waits.
	if secured, ok := conn.(*tls.Conn); ok {
		if err := secured.Handshake(); err != nil { //nolint:noctx // the listener's own connection, ended by Close
			return
		}
	}

	// Before the button, a status that grants nothing, and after the
	// signing request a message that is not its answer: neither was in
	// the records, and a client that took either for what it waits for
	// would be wrong.
	if _, err := conn.Write([]byte(
		`{"Header":{"StatusCode":"200 OK","ContentType":"status;plurality=single"},` +
			`"Body":{"Status":{"Permissions":["Public"]}}}` + "\r\n",
	)); err != nil {
		return
	}

	<-b.pressed

	// The content types are as the open implementations match them: the
	// status by its prefix, the signing result by its tag alone.
	status := `{"Header":{"StatusCode":"200 OK","ContentType":"status;plurality=single"},` +
		`"Body":{"Status":{"Permissions":["Public","PhysicalAccess"]}}}` + "\r\n"
	if _, err := conn.Write([]byte(status)); err != nil {
		return
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}

	var request pairingRequest

	if json.Unmarshal(line, &request) != nil {
		return
	}

	block, _ := pem.Decode([]byte(request.Body.Parameters.CSR))
	if block == nil || request.Header.RequestType != "Execute" || request.Body.CommandType != "CSR" {
		return
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return
	}

	b.mu.Lock()
	b.paired = append(b.paired, request.Body.Parameters.DisplayName)
	garbled := b.garbled
	b.mu.Unlock()

	b.mu.Lock()
	signer := b.signer
	b.mu.Unlock()

	signed := string(signer.sign(csr.Subject.CommonName, csr.PublicKey, x509.ExtKeyUsageClientAuth))
	if garbled {
		signed = "not a certificate"
	}

	if _, err = conn.Write(
		[]byte(`{"Header":{"ContentType":"status;plurality=single"},"Body":{}}` + "\r\n"),
	); err != nil {
		return
	}

	answer, err := json.Marshal(map[string]any{
		"Header": map[string]any{
			"StatusCode": statusOK, "ClientTag": request.Header.ClientTag,
			"ContentType": "signing-result;plurality=single",
		},
		"Body": map[string]any{"SigningResult": map[string]any{
			"Certificate":     signed,
			"RootCertificate": string(b.authority.pem()),
		}},
	})
	if err != nil {
		return
	}

	_, _ = conn.Write(append(answer, '\r', '\n'))
}

func listen(config *tls.Config, address string) net.Listener {
	listener, err := tls.Listen("tcp", address, config)
	if err != nil {
		panic(err)
	}

	return listener
}

func accept(listener net.Listener, serve func(net.Conn)) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		go serve(conn)
	}
}

// authority is a certificate authority made for one test.
type authority struct {
	key         *ecdsa.PrivateKey
	certificate *x509.Certificate
	serial      int64
	mu          sync.Mutex
}

func newAuthority(name string) *authority {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}

	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}

	return &authority{key: key, certificate: certificate, serial: 1}
}

func (a *authority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.certificate)

	return pool
}

func (a *authority) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.certificate.Raw})
}

// sign issues a certificate for public under name, as PEM.
func (a *authority) sign(name string, public any, usage x509.ExtKeyUsage) []byte {
	a.mu.Lock()
	a.serial++
	serial := a.serial
	a.mu.Unlock()

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, public, a.key)
	if err != nil {
		panic(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// issue makes a key and a certificate for it, ready for a tls.Config.
func (a *authority) issue(name string, usage x509.ExtKeyUsage) tls.Certificate {
	certificate, _ := a.issuePEM(name, usage)

	return certificate
}

// issuePEM is issue, with the certificate and the key as PEM beside it.
func (a *authority) issuePEM(name string, usage x509.ExtKeyUsage) (tls.Certificate, [2][]byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}

	blocks := [2][]byte{
		a.sign(name, &key.PublicKey, usage),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
	}

	certificate, err := tls.X509KeyPair(blocks[0], blocks[1])
	if err != nil {
		panic(err)
	}

	return certificate, blocks
}
