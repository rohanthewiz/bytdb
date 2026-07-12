package pgwire

// proto.go: framing and body building for the PostgreSQL
// frontend/backend protocol, version 3.0.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/rohanthewiz/serr"
)

// maxMsgLen bounds one frame's body; anything larger is a corrupt or
// hostile stream.
const maxMsgLen = 1 << 26 // 64 MiB

// Startup request codes: the value a startup packet carries in its
// protocol-version field.
const (
	protoVersion3     = 196608 // 3.0
	codeSSLRequest    = 80877103
	codeGSSENCRequest = 80877104
	codeCancelRequest = 80877102
)

// Frontend message types.
const (
	msgQuery     = 'Q'
	msgParse     = 'P'
	msgBind      = 'B'
	msgDescribe  = 'D'
	msgExecute   = 'E'
	msgClose     = 'C'
	msgSync      = 'S'
	msgFlush     = 'H'
	msgTerminate = 'X'
	// 'p' is the frontend's one catch-all credential message; during
	// SCRAM it carries SASLInitialResponse and SASLResponse.
	msgSASLResponse = 'p'
)

// Authentication request codes: the int32 an 'R' (msgAuth) body opens
// with, telling the client what the server wants next.
const (
	authOK           = 0
	authSASL         = 10 // here are my SASL mechanisms; pick one
	authSASLContinue = 11 // mid-exchange challenge (server-first-message)
	authSASLFinal    = 12 // exchange done; server's proof (server-final-message)
)

// Backend message types.
const (
	msgAuth            = 'R'
	msgParameterStatus = 'S'
	msgBackendKeyData  = 'K'
	msgReadyForQuery   = 'Z'
	msgRowDescription  = 'T'
	msgDataRow         = 'D'
	msgCommandComplete = 'C'
	msgEmptyQuery      = 'I'
	msgErrorResponse   = 'E'
	msgParseComplete   = '1'
	msgBindComplete    = '2'
	msgCloseComplete   = '3'
	msgParamDesc       = 't'
	msgNoData          = 'n'
	msgNoticeResponse  = 'N'
)

// readMessage reads one typed frontend frame: type byte, int32 length
// (self-inclusive), body.
func readMessage(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	body, err := readBody(r)
	return typ, body, err
}

// readStartup reads one untyped startup frame (the client's first
// message has no type byte).
func readStartup(r *bufio.Reader) ([]byte, error) {
	return readBody(r)
}

func readBody(r *bufio.Reader) ([]byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(lb[:])) - 4
	if n < 0 || n > maxMsgLen {
		return nil, serr.New("bad message length", "length", fmt.Sprint(n))
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// rbuf consumes a message body; the first short read latches bad, so
// callers can parse a whole message and check once.
type rbuf struct {
	b   []byte
	bad bool
}

func (r *rbuf) byte() byte {
	if r.bad || len(r.b) < 1 {
		r.bad = true
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *rbuf) u16() int {
	if r.bad || len(r.b) < 2 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint16(r.b)
	r.b = r.b[2:]
	return int(v)
}

func (r *rbuf) u32() uint32 {
	if r.bad || len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *rbuf) cstr() string {
	if r.bad {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.bad = true
	return ""
}

// bytesN consumes an int32-length-prefixed value; -1 is SQL NULL,
// returned as ok=false.
func (r *rbuf) bytesN() (v []byte, ok bool) {
	n := int(int32(r.u32()))
	if r.bad || n < 0 {
		return nil, false
	}
	if len(r.b) < n {
		r.bad = true
		return nil, false
	}
	v = r.b[:n:n]
	r.b = r.b[n:]
	return v, true
}

// wbuf builds a message body.
type wbuf []byte

func (b *wbuf) byte(v byte)   { *b = append(*b, v) }
func (b *wbuf) int16(v int)   { *b = binary.BigEndian.AppendUint16(*b, uint16(v)) }
func (b *wbuf) int32(v int)   { *b = binary.BigEndian.AppendUint32(*b, uint32(v)) }
func (b *wbuf) cstr(s string) { *b = append(append(*b, s...), 0) }
func (b *wbuf) raw(v []byte)  { *b = append(*b, v...) }
