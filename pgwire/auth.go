package pgwire

// auth.go: server-side SCRAM-SHA-256 authentication (RFC 5802 as
// profiled by RFC 7677 and the Postgres SASL docs).
//
// The exchange, as it crosses the wire:
//
//	client                                server
//	  StartupMessage (user=...)  ────────►
//	  ◄──────── AuthenticationSASL ("SCRAM-SHA-256")
//	  SASLInitialResponse ──────────────►   n,,n=*,r=<cnonce>
//	  ◄──────── AuthenticationSASLContinue  r=<cnonce+snonce>,s=<salt>,i=<iters>
//	  SASLResponse ─────────────────────►   c=biws,r=<nonce>,p=<proof>
//	  ◄──────── AuthenticationSASLFinal     v=<server signature>
//	  ◄──────── AuthenticationOk
//
// The server never sees the password: it stores only the derived
// StoredKey/ServerKey pair (Postgres's rolpassword verifier format),
// the client proves knowledge of the password with a one-time proof
// bound to both nonces, and the v= signature proves to the client
// that the server really held the verifier — mutual authentication.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/serr"
	"golang.org/x/text/secure/precis"
)

const (
	// scramIterations is the PBKDF2 iteration count for newly set
	// passwords — Postgres's own default. Higher is slower for an
	// offline attacker but also for every login.
	scramIterations = 4096
	scramSaltLen    = 16
	scramKeyLen     = sha256.Size
	verifierPrefix  = "SCRAM-SHA-256$"
)

// Credentials is the server's user registry: user name → SCRAM
// verifier. Safe for concurrent use; connections read it on every
// login, and the owner may add or change users while serving.
type Credentials struct {
	mu    sync.RWMutex
	users map[string]scramVerifier

	// mockKey feeds the fake verifiers handed out for unknown users,
	// so a probe cannot distinguish "no such user" (instant refusal
	// would leak it) from "wrong password" — the exchange runs to
	// completion either way, with a salt that is stable per user (a
	// fresh random salt on every attempt would also be a tell).
	mockKey []byte
}

// scramVerifier is what the server stores instead of a password —
// exactly the fields of a Postgres SCRAM-SHA-256 rolpassword entry.
type scramVerifier struct {
	iterations int
	salt       []byte
	storedKey  []byte // H(ClientKey): checks the client's proof
	serverKey  []byte // HMAC(SaltedPassword, "Server Key"): signs server-final
}

// NewCredentials returns an empty registry. A Server with a non-nil
// Auth refuses every connection until users are added.
func NewCredentials() *Credentials {
	key := make([]byte, 32)
	if _, err := crand.Read(key); err != nil {
		// Same stance as newCancelSecret: a broken randomness source is
		// not something to limp past in authentication code.
		panic("pgwire: reading random mock-verifier key: " + err.Error())
	}
	return &Credentials{users: map[string]scramVerifier{}, mockKey: key}
}

// SetPassword registers (or re-keys) a user from a plaintext
// password. Only the derived verifier is retained.
func (cr *Credentials) SetPassword(user, password string) {
	v := deriveVerifier(password, newSalt(), scramIterations)
	cr.mu.Lock()
	cr.users[user] = v
	cr.mu.Unlock()
}

// SetVerifier registers a user from a Postgres-format SCRAM verifier
// ("SCRAM-SHA-256$<iter>:<salt>$<StoredKey>:<ServerKey>", base64
// fields) — e.g. one copied from pg_authid or built by MakeVerifier —
// so credential files never need to hold plaintext.
func (cr *Credentials) SetVerifier(user, verifier string) error {
	v, err := parseVerifier(verifier)
	if err != nil {
		return serr.Wrap(err, "user", user)
	}
	cr.mu.Lock()
	cr.users[user] = v
	cr.mu.Unlock()
	return nil
}

// MakeVerifier derives a Postgres-format SCRAM-SHA-256 verifier from
// a password, for building credential files out of band.
func MakeVerifier(password string) string {
	v := deriveVerifier(password, newSalt(), scramIterations)
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		v.iterations, b64(v.salt), b64(v.storedKey), b64(v.serverKey))
}

// lookup returns the user's verifier, or — for an unknown user — a
// deterministic fake one (mocked=true) so the exchange proceeds
// normally and fails only at proof verification, indistinguishable
// from a wrong password.
func (cr *Credentials) lookup(user string) (v scramVerifier, mocked bool) {
	cr.mu.RLock()
	v, ok := cr.users[user]
	cr.mu.RUnlock()
	if ok {
		return v, false
	}
	mock := func(label string) []byte {
		m := hmac.New(sha256.New, cr.mockKey)
		m.Write([]byte(label + ":" + user))
		return m.Sum(nil)
	}
	return scramVerifier{
		iterations: scramIterations,
		salt:       mock("salt")[:scramSaltLen],
		storedKey:  mock("stored"),
		serverKey:  mock("server"),
	}, true
}

func parseVerifier(s string) (scramVerifier, error) {
	bad := func() (scramVerifier, error) {
		// Deliberately no echo of s: a malformed verifier is still a
		// secret and must not land in logs.
		return scramVerifier{}, serr.New("malformed SCRAM-SHA-256 verifier")
	}
	rest, ok := strings.CutPrefix(s, verifierPrefix)
	if !ok {
		return bad()
	}
	iterSalt, keys, ok := strings.Cut(rest, "$")
	if !ok {
		return bad()
	}
	iterStr, saltB64, ok := strings.Cut(iterSalt, ":")
	if !ok {
		return bad()
	}
	storedB64, serverB64, ok := strings.Cut(keys, ":")
	if !ok {
		return bad()
	}
	iters, err := strconv.Atoi(iterStr)
	if err != nil || iters < 1 {
		return bad()
	}
	salt, err1 := base64.StdEncoding.DecodeString(saltB64)
	stored, err2 := base64.StdEncoding.DecodeString(storedB64)
	server, err3 := base64.StdEncoding.DecodeString(serverB64)
	if err1 != nil || err2 != nil || err3 != nil ||
		len(salt) == 0 || len(stored) != scramKeyLen || len(server) != scramKeyLen {
		return bad()
	}
	return scramVerifier{iterations: iters, salt: salt, storedKey: stored, serverKey: server}, nil
}

// deriveVerifier runs the RFC 5802 key schedule:
//
//	SaltedPassword = Hi(Normalize(password), salt, i)   (PBKDF2-HMAC-SHA-256)
//	StoredKey      = H(HMAC(SaltedPassword, "Client Key"))
//	ServerKey      = HMAC(SaltedPassword, "Server Key")
func deriveVerifier(password string, salt []byte, iters int) scramVerifier {
	sp, err := pbkdf2.Key(sha256.New, saslPrep(password), salt, iters, scramKeyLen)
	if err != nil {
		// Only reachable with invalid parameters, and ours are fixed
		// constants — treat like any other impossible internal state.
		panic("pgwire: pbkdf2: " + err.Error())
	}
	clientKey := hmacSHA256(sp, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	return scramVerifier{
		iterations: iters,
		salt:       salt,
		storedKey:  storedKey[:],
		serverKey:  hmacSHA256(sp, "Server Key"),
	}
}

// saslPrep normalizes a password the way clients will before hashing
// it. precis.OpaqueString is the same profile pgx applies client-side,
// so both ends derive identical keys for non-ASCII passwords; like
// Postgres's pg_saslprep, an unnormalizable string falls back to its
// raw bytes rather than being rejected.
func saslPrep(password string) string {
	if s, err := precis.OpaqueString.String(password); err == nil {
		return s
	}
	return password
}

func newSalt() []byte {
	salt := make([]byte, scramSaltLen)
	if _, err := crand.Read(salt); err != nil {
		panic("pgwire: reading random salt: " + err.Error())
	}
	return salt
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// --- channel binding (SCRAM-SHA-256-PLUS, RFC 5929) ---

// channelBinding returns the tls-server-end-point binding data — a
// hash of the server's DER certificate — or nil when the PLUS
// mechanism cannot be offered. Binding data must equal what the client
// computes from the certificate it received, so it is only derivable
// when the served certificate is unambiguous: exactly one static
// certificate and no dynamic selection callbacks (with those, which
// certificate a handshake served depends on SNI and is not observable
// after the fact from the server side).
func (s *Server) channelBinding() []byte {
	s.bindOnce.Do(func() {
		cfg := s.TLSConfig
		if cfg == nil || cfg.GetCertificate != nil || cfg.GetConfigForClient != nil ||
			len(cfg.Certificates) != 1 || len(cfg.Certificates[0].Certificate) == 0 {
			return
		}
		leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
		if err != nil {
			return
		}
		h := bindingHash(leaf.SignatureAlgorithm)
		if h == nil {
			return
		}
		h.Write(leaf.Raw)
		s.bindData = h.Sum(nil)
	})
	return s.bindData
}

// bindingHash picks the certificate hash per RFC 5929 §4.1: the hash
// of the certificate's signature algorithm, except MD5 and SHA-1
// (too weak) are replaced by SHA-256. The mapping mirrors pgx's
// client-side getTLSCertificateHash exactly — both ends must hash the
// same way or the proof never verifies. Unlisted algorithms (Ed25519,
// DSA) get nil: the binding is undefined for them, so PLUS is simply
// not advertised and clients fall back to plain SCRAM-SHA-256.
func bindingHash(alg x509.SignatureAlgorithm) hash.Hash {
	switch alg {
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1,
		x509.SHA256WithRSA, x509.SHA256WithRSAPSS, x509.ECDSAWithSHA256:
		return sha256.New()
	case x509.SHA384WithRSA, x509.SHA384WithRSAPSS, x509.ECDSAWithSHA384:
		return sha512.New384()
	case x509.SHA512WithRSA, x509.SHA512WithRSAPSS, x509.ECDSAWithSHA512:
		return sha512.New()
	}
	return nil
}

// --- the exchange itself ---

// scramConv holds one connection's SCRAM conversation between the two
// client messages; everything the final proof is computed over
// (AuthMessage) is carried here verbatim.
type scramConv struct {
	verifier scramVerifier
	mocked   bool

	// binding is the tls-server-end-point data when the client chose
	// SCRAM-SHA-256-PLUS (nil for plain SCRAM-SHA-256); plusOffered
	// records whether PLUS was advertised, because a "y" gs2 flag from
	// a client that was offered binding is a downgrade in progress.
	binding     []byte
	plusOffered bool

	gs2Header       string // e.g. "n,," — echoed back base64 in client-final's c=
	clientFirstBare string
	serverFirst     string
	nonce           string // client nonce + server nonce
}

// clientFirst parses the client-first-message and builds the
// server-first-message.
func (sc *scramConv) clientFirst(msg string) (string, error) {
	// GS2 header: channel-binding flag, optional authzid, comma. The
	// flag must agree with the mechanism the client selected — "p=" if
	// and only if it chose SCRAM-SHA-256-PLUS — and RFC 5802 §6 makes
	// "y" a tripwire: it means "I can bind but you did not offer it",
	// so if PLUS *was* advertised, someone between us stripped it from
	// the mechanism list and authentication must fail.
	var rest string
	switch {
	case strings.HasPrefix(msg, "n,"):
		if sc.binding != nil {
			return "", serr.New("SCRAM-SHA-256-PLUS requires the p= gs2 flag")
		}
		rest = msg[2:]
	case strings.HasPrefix(msg, "y,"):
		if sc.binding != nil {
			return "", serr.New("SCRAM-SHA-256-PLUS requires the p= gs2 flag")
		}
		if sc.plusOffered {
			return "", serr.New("channel binding downgrade detected")
		}
		rest = msg[2:]
	case strings.HasPrefix(msg, "p="):
		cbName, r, ok := strings.Cut(msg[2:], ",")
		if !ok {
			return "", serr.New("malformed SCRAM gs2 header")
		}
		if sc.binding == nil {
			// p= under plain SCRAM-SHA-256 (or PLUS never advertised).
			return "", serr.New("channel binding not offered")
		}
		if cbName != "tls-server-end-point" {
			return "", serr.New("unsupported channel binding type", "type", cbName)
		}
		rest = r
	default:
		return "", serr.New("malformed SCRAM client-first message")
	}
	authzid, bare, ok := strings.Cut(rest, ",")
	if !ok || (authzid != "" && !strings.HasPrefix(authzid, "a=")) {
		return "", serr.New("malformed SCRAM gs2 header")
	}
	sc.gs2Header = msg[:len(msg)-len(bare)]
	sc.clientFirstBare = bare

	// client-first-message-bare: n=<user>,r=<nonce>[,...]. The n= user
	// is ignored — Postgres authenticates the startup-message user and
	// so does this server. m= would demand an extension we lack.
	attrs := strings.Split(bare, ",")
	if len(attrs) < 2 || !strings.HasPrefix(attrs[0], "n=") || !strings.HasPrefix(attrs[1], "r=") {
		if len(attrs) > 0 && strings.HasPrefix(attrs[0], "m=") {
			return "", serr.New("unsupported mandatory SCRAM extension")
		}
		return "", serr.New("malformed SCRAM client-first message")
	}
	clientNonce := attrs[1][2:]
	if clientNonce == "" || !validNonce(clientNonce) {
		return "", serr.New("bad SCRAM client nonce")
	}

	// The server appends its own entropy to the client's nonce; the
	// proof later covers both, so neither side can replay the other.
	var raw [18]byte
	if _, err := crand.Read(raw[:]); err != nil {
		panic("pgwire: reading random server nonce: " + err.Error())
	}
	sc.nonce = clientNonce + base64.StdEncoding.EncodeToString(raw[:])
	sc.serverFirst = fmt.Sprintf("r=%s,s=%s,i=%d",
		sc.nonce, base64.StdEncoding.EncodeToString(sc.verifier.salt), sc.verifier.iterations)
	return sc.serverFirst, nil
}

// clientFinal verifies the client's proof and returns the
// server-final-message (v=<ServerSignature>), or an error on any
// mismatch — including the mocked-verifier case, decided only after
// the full computation so unknown users cost the same time as wrong
// passwords.
func (sc *scramConv) clientFinal(msg string) (string, error) {
	// p= is defined to be last; everything before it is what the
	// AuthMessage (and thus the proof) was computed over.
	cut := strings.LastIndex(msg, ",p=")
	if cut < 0 {
		return "", serr.New("malformed SCRAM client-final message")
	}
	withoutProof, proofB64 := msg[:cut], msg[cut+3:]
	attrs := strings.Split(withoutProof, ",")
	if len(attrs) < 2 || !strings.HasPrefix(attrs[0], "c=") || !strings.HasPrefix(attrs[1], "r=") {
		return "", serr.New("malformed SCRAM client-final message")
	}
	// c= must echo the gs2 header from client-first — plus, under
	// SCRAM-SHA-256-PLUS, the channel-binding data itself (the server
	// certificate's hash). Both are covered by the proof, so a MITM
	// downgrading the binding flag breaks the exchange, and under PLUS
	// even a relayed handshake fails: the attacker's own certificate
	// hash is what the client signed, not ours.
	cbindInput := append([]byte(sc.gs2Header), sc.binding...)
	if attrs[0][2:] != base64.StdEncoding.EncodeToString(cbindInput) {
		return "", serr.New("SCRAM channel-binding mismatch")
	}
	if attrs[1][2:] != sc.nonce {
		return "", serr.New("SCRAM nonce mismatch")
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil || len(proof) != scramKeyLen {
		return "", serr.New("malformed SCRAM proof")
	}

	// ClientProof = ClientKey XOR HMAC(StoredKey, AuthMessage), and
	// StoredKey = H(ClientKey): recover the ClientKey and check its
	// hash. Holding only StoredKey, the server can verify the proof
	// but never construct one — a stolen verifier alone cannot log in.
	authMsg := sc.clientFirstBare + "," + sc.serverFirst + "," + withoutProof
	m := hmac.New(sha256.New, sc.verifier.storedKey)
	m.Write([]byte(authMsg))
	clientSig := m.Sum(nil)
	clientKey := make([]byte, scramKeyLen)
	for i := range clientKey {
		clientKey[i] = proof[i] ^ clientSig[i]
	}
	sum := sha256.Sum256(clientKey)
	// Constant-time compare, and the mocked flag folded in only after
	// the arithmetic: neither timing nor error text may separate
	// "unknown user" from "wrong password".
	if subtle.ConstantTimeCompare(sum[:], sc.verifier.storedKey) != 1 || sc.mocked {
		return "", serr.New("SCRAM proof verification failed")
	}

	m = hmac.New(sha256.New, sc.verifier.serverKey)
	m.Write([]byte(authMsg))
	return "v=" + base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
}

// validNonce reports whether a client nonce is made of the printable
// characters RFC 5802 allows (no comma — it is the attribute
// separator the nonce gets spliced between).
func validNonce(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' || s[i] == ',' {
			return false
		}
	}
	return true
}

// authSCRAM runs the server side of the exchange over the connection.
// A nil return means the client proved the password; the caller then
// sends AuthenticationOk. Any failure has already been reported to
// the client (uniformly, as 28P01) before the error returns.
func (c *conn) authSCRAM(user string) error {
	// One shape for every authentication failure. Postgres's exact
	// message and SQLSTATE, and never a hint of which step failed:
	// clients pattern-match the former, attackers the latter.
	fail := func(err error) error {
		c.send(msgErrorResponse, fatalBody(
			fmt.Sprintf("password authentication failed for user %q", user), "28P01"))
		c.w.Flush()
		return serr.Wrap(err, "scram authentication failed", "user", user)
	}

	// On TLS, offer SCRAM-SHA-256-PLUS (channel binding) ahead of the
	// base mechanism when the certificate's binding data is derivable;
	// clients with channel_binding=require refuse servers that don't.
	// Postgres advertises the same pair in the same order.
	var binding []byte
	if c.tlsOn {
		binding = c.srv.channelBinding()
	}
	var adv wbuf
	adv.int32(authSASL)
	if binding != nil {
		adv.cstr("SCRAM-SHA-256-PLUS")
	}
	adv.cstr("SCRAM-SHA-256")
	adv.byte(0) // end of mechanism list
	c.send(msgAuth, adv)
	if err := c.w.Flush(); err != nil {
		return err
	}

	// SASLInitialResponse: mechanism name, then the length-prefixed
	// client-first-message.
	typ, body, err := readMessage(c.r)
	if err != nil {
		return err
	}
	if typ != msgSASLResponse {
		return fail(serr.New("expected SASLInitialResponse", "got", string(typ)))
	}
	r := &rbuf{b: body}
	mech := r.cstr()
	first, ok := r.bytesN()
	if r.bad || !ok {
		return fail(serr.New("malformed SASLInitialResponse"))
	}
	switch mech {
	case "SCRAM-SHA-256":
	case "SCRAM-SHA-256-PLUS":
		if binding == nil {
			return fail(serr.New("SCRAM-SHA-256-PLUS was not offered"))
		}
	default:
		return fail(serr.New("unsupported SASL mechanism", "mechanism", mech))
	}

	verifier, mocked := c.srv.Auth.lookup(user)
	conv := &scramConv{verifier: verifier, mocked: mocked, plusOffered: binding != nil}
	if mech == "SCRAM-SHA-256-PLUS" {
		conv.binding = binding
	}
	serverFirst, err := conv.clientFirst(string(first))
	if err != nil {
		return fail(err)
	}
	var cont wbuf
	cont.int32(authSASLContinue)
	cont.raw([]byte(serverFirst))
	c.send(msgAuth, cont)
	if err := c.w.Flush(); err != nil {
		return err
	}

	// SASLResponse: the body is the client-final-message, unadorned.
	typ, body, err = readMessage(c.r)
	if err != nil {
		return err
	}
	if typ != msgSASLResponse {
		return fail(serr.New("expected SASLResponse", "got", string(typ)))
	}
	serverFinal, err := conv.clientFinal(string(body))
	if err != nil {
		return fail(err)
	}
	var fin wbuf
	fin.int32(authSASLFinal)
	fin.raw([]byte(serverFinal))
	c.send(msgAuth, fin)
	return nil
}
