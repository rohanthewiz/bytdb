package pgwire

import "bufio"

// readBodyMax preserves the old readBody(r) shape for tests written
// against the post-auth (64 MiB) limit.
func readBodyMax(r *bufio.Reader) ([]byte, error) { return readBody(r, maxMsgLen) }
