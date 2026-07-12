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
	"crypto/subtle"
	"encoding/base64"
	"fmt"
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

// --- the exchange itself ---

// scramConv holds one connection's SCRAM conversation between the two
// client messages; everything the final proof is computed over
// (AuthMessage) is carried here verbatim.
type scramConv struct {
	verifier scramVerifier
	mocked   bool

	gs2Header       string // e.g. "n,," — echoed back base64 in client-final's c=
	clientFirstBare string
	serverFirst     string
	nonce           string // client nonce + server nonce
}

// clientFirst parses the client-first-message and builds the
// server-first-message.
func (sc *scramConv) clientFirst(msg string) (string, error) {
	// GS2 header: channel-binding flag, optional authzid, comma. Only
	// "SCRAM-SHA-256" is advertised, so a client on TLS says "y" (I
	// could bind but you did not offer it) and one on plaintext says
	// "n"; "p=" would claim a binding this server never offered.
	rest, ok := strings.CutPrefix(msg, "n,")
	if !ok {
		if rest, ok = strings.CutPrefix(msg, "y,"); !ok {
			if strings.HasPrefix(msg, "p=") {
				return "", serr.New("channel binding is not supported")
			}
			return "", serr.New("malformed SCRAM client-first message")
		}
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
	// c= must echo the gs2 header from client-first: it is covered by
	// the proof, so a MITM downgrading the binding flag breaks it.
	if attrs[0][2:] != base64.StdEncoding.EncodeToString([]byte(sc.gs2Header)) {
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

	var adv wbuf
	adv.int32(authSASL)
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
	if mech != "SCRAM-SHA-256" {
		return fail(serr.New("unsupported SASL mechanism", "mechanism", mech))
	}

	verifier, mocked := c.srv.Auth.lookup(user)
	conv := &scramConv{verifier: verifier, mocked: mocked}
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
