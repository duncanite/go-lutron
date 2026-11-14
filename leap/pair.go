package leap

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed" // the Caséta app's identity
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// The errors pairing and the credentials hand back.
var (
	// ErrPairing: the bridge's side of the pairing did not go as the
	// exchange is known to go.
	ErrPairing = errors.New("pairing with the bridge failed")
	// ErrCredentials: the key, the certificate or the root is not PEM
	// of the kind expected, or the bridge's certificate does not chain
	// to the root.
	ErrCredentials = errors.New("the credentials are not usable")
)

// The identity the open implementations present to a Caséta bridge's
// pairing port: Lutron's, and published. identity/README.md says whose
// it is, where it was copied from, and what its holder can do.
var (
	//go:embed identity/caseta-app.pem
	casetaApp []byte
	//go:embed identity/caseta-app-key.pem
	casetaAppKey []byte
	//go:embed identity/caseta-pairing-root.pem
	casetaPairingRoot []byte
)

// CasetaApp is the identity to dial a Caséta bridge's pairing port with:
// a certificate and key issued to "Caseta Application" by Lutron, and
// the root that port's certificate chains to. Each call returns its own
// copy. The key is published, and its certificate names a server's use
// too: under TLSConfig of it, whoever holds the key passes for a pairing
// port. The button on the bridge and the network guard a pairing, not
// this check. A RadioRA 3 or HomeWorks processor's pairing port chains
// to another root, which is not here.
func CasetaApp() *Credentials {
	return &Credentials{
		Key:         append([]byte(nil), casetaAppKey...),
		Certificate: append([]byte(nil), casetaApp...),
		Root:        append([]byte(nil), casetaPairingRoot...),
	}
}

// Credentials is what pairing gives and every connection after it needs:
// this client's private key, the certificate the bridge signed for it,
// and the root the bridge's own certificate chains to. All three are
// PEM. The key is a secret; the bridge keeps the certificate's
// authorization until the client is removed in Lutron's app.
type Credentials struct {
	Key         []byte
	Certificate []byte
	Root        []byte
}

const (
	keyBits = 2048
	// pairTag marks the signing request, so its answer is told from
	// the status messages the bridge sends meanwhile.
	pairTag = "get-cert"
	// physicalAccess is the permission the bridge announces once its
	// button has been pressed.
	physicalAccess = "PhysicalAccess"
)

// The pairing port's messages are not the LEAP port's: a request names
// its type in the header, and the bridge announces the button press as a
// status. The keys are the bridge's.
type (
	pairRequest struct {
		Header pairRequestHeader `json:"Header"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Body   pairRequestBody   `json:"Body"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairRequestHeader struct {
		RequestType string `json:"RequestType"` //nolint:tagliatelle // the bridge's keys are PascalCase
		URL         string `json:"Url"`         //nolint:tagliatelle // the bridge's keys are PascalCase
		ClientTag   string `json:"ClientTag"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairRequestBody struct {
		CommandType string         `json:"CommandType"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Parameters  pairParameters `json:"Parameters"`  //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairParameters struct {
		CSR         string `json:"CSR"`         //nolint:tagliatelle // the bridge's keys are PascalCase
		DisplayName string `json:"DisplayName"` //nolint:tagliatelle // the bridge's keys are PascalCase
		DeviceUID   string `json:"DeviceUID"`   //nolint:tagliatelle // the bridge's keys are PascalCase
		Role        string `json:"Role"`        //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairAnswer struct {
		Header pairAnswerHeader `json:"Header"` //nolint:tagliatelle // the bridge's keys are PascalCase
		Body   pairAnswerBody   `json:"Body"`   //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairAnswerHeader struct {
		ContentType string `json:"ContentType"` //nolint:tagliatelle // the bridge's keys are PascalCase
		ClientTag   string `json:"ClientTag"`   //nolint:tagliatelle // the bridge's keys are PascalCase
		StatusCode  string `json:"StatusCode"`  //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairAnswerBody struct {
		Status        pairStatus  `json:"Status"`        //nolint:tagliatelle // the bridge's keys are PascalCase
		SigningResult pairSigning `json:"SigningResult"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairStatus struct {
		Permissions []string `json:"Permissions"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
	pairSigning struct {
		Certificate     string `json:"Certificate"`     //nolint:tagliatelle // the bridge's keys are PascalCase
		RootCertificate string `json:"RootCertificate"` //nolint:tagliatelle // the bridge's keys are PascalCase
	}
)

// Pair asks the bridge for credentials over conn, a connection to its
// pairing port made under the identity the bridge expects there, and
// closes it. The bridge says nothing until a person presses the button
// on it; ready, when not nil, is called once the connection is waiting
// for that. name is how the client is called on the bridge.
//
// The pairing port does not take any client: it wants the certificate
// Lutron's own app carries. CasetaApp is that identity for a Caséta
// bridge; the caller makes conn with it.
func Pair(ctx context.Context, conn net.Conn, name string, ready func()) (*Credentials, error) {
	defer func() { _ = conn.Close() }()

	// The context ends the wait for the button: closing the connection
	// is what unblocks the read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("%w: generating the key: %w", ErrPairing, err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("%w: making the signing request: %w", ErrPairing, err)
	}

	reader := bufio.NewReaderSize(conn, maxLine)

	if ready != nil {
		ready()
	}

	pressed := func(answer *pairAnswer) bool {
		return strings.HasPrefix(answer.Header.ContentType, "status;") &&
			slices.Contains(answer.Body.Status.Permissions, physicalAccess)
	}
	if _, err = awaitPairing(ctx, reader, pressed); err != nil {
		return nil, err
	}

	request := pairRequest{
		Header: pairRequestHeader{RequestType: "Execute", URL: "/pair", ClientTag: pairTag},
		Body: pairRequestBody{
			CommandType: "CSR",
			Parameters: pairParameters{
				CSR:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})),
				DisplayName: name,
				DeviceUID:   "000000000000",
				Role:        "Admin",
			},
		},
	}

	line, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPairing, err)
	}

	if _, err = conn.Write(append(line, '\r', '\n')); err != nil {
		return nil, fmt.Errorf("%w: sending the signing request: %w", ErrPairing, pairingCause(ctx, err))
	}

	signed, err := awaitPairing(ctx, reader, func(answer *pairAnswer) bool {
		return answer.Header.ClientTag == pairTag
	})
	if err != nil {
		return nil, err
	}

	result := signed.Body.SigningResult
	if result.Certificate == "" || result.RootCertificate == "" {
		return nil, fmt.Errorf("%w: the bridge signed nothing: %s", ErrPairing, signed.Header.StatusCode)
	}

	credentials := &Credentials{
		Key:         pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		Certificate: []byte(result.Certificate),
		Root:        []byte(result.RootCertificate),
	}

	if _, err = TLSConfig(credentials); err != nil {
		return nil, fmt.Errorf("%w: what the bridge signed: %w", ErrPairing, err)
	}

	return credentials, nil
}

// awaitPairing reads the pairing port's messages until one is wanted.
func awaitPairing(ctx context.Context, reader *bufio.Reader, wanted func(*pairAnswer) bool) (*pairAnswer, error) {
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: reading from the bridge: %w", ErrPairing, pairingCause(ctx, err))
		}

		var answer pairAnswer
		if err = json.Unmarshal(line, &answer); err != nil {
			return nil, fmt.Errorf("%w: %w: %w", ErrPairing, ErrMessage, err)
		}

		if wanted(&answer) {
			return &answer, nil
		}
	}
}

// pairingCause is the context's reason when the context is why the
// connection failed, the connection's own error otherwise.
func pairingCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("the wait ended: %w", cause)
	}

	return err
}

// TLSConfig is the TLS configuration for Dial from what pairing gave:
// the client's certificate, and the bridge's certificate verified against
// the root. The bridge's certificate names no host a client could check,
// so the verification is its chain to the root and its use: a server's,
// since the same root signs every paired client.
func TLSConfig(credentials *Credentials) (*tls.Config, error) {
	certificate, err := tls.X509KeyPair(credentials.Certificate, credentials.Key)
	if err != nil {
		return nil, fmt.Errorf("%w: the key and certificate: %w", ErrCredentials, err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(credentials.Root) {
		return nil, fmt.Errorf("%w: the root holds no certificate", ErrCredentials)
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		// The name cannot be checked; VerifyConnection checks the chain.
		InsecureSkipVerify: true, // #nosec G402 -- VerifyConnection below verifies the chain to the pairing's root
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyChain(state.PeerCertificates, roots)
		},
	}, nil
}

// verifyChain verifies the leaf against roots, with the rest of what the
// peer presented as intermediates, and no host name.
func verifyChain(presented []*x509.Certificate, roots *x509.CertPool) error {
	if len(presented) == 0 {
		return fmt.Errorf("%w: the bridge presented no certificate", ErrCredentials)
	}

	intermediates := x509.NewCertPool()
	for _, certificate := range presented[1:] {
		intermediates.AddCert(certificate)
	}

	// The root signs the clients too: the usage is what tells the bridge's
	// certificate from a paired client's.
	_, err := presented[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("%w: the bridge's certificate: %w", ErrCredentials, err)
	}

	return nil
}
