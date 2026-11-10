package main

// reload.go: SIGHUP re-reads the credentials file and the TLS key pair
// without a restart (deep-look D-58) — a password rotation or a
// certificate renewal no longer drops every connection. Only what was
// configured at startup reloads: a server started without -auth-file or
// -tls-cert keeps trust authentication or plaintext, since turning
// either on mid-flight would change the rules for clients already
// connected under the old ones. A file that fails to load leaves the
// running settings in place and says so.

import (
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/serr"
)

// reloadConfig names what SIGHUP re-reads.
type reloadConfig struct {
	authFile        string
	tlsCert, tlsKey string
	creds           *pgwire.Credentials // the live registry (nil: trust auth)
	srv             *pgwire.Server
}

// reload re-reads the configured files and installs what loads,
// returning one line per item for the log.
func (rc reloadConfig) reload() []string {
	var out []string
	if rc.authFile != "" && rc.creds != nil {
		if fresh, err := loadAuthFile(rc.authFile); err != nil {
			out = append(out, "auth file not reloaded, keeping the current users: "+serr.StringFromErr(err))
		} else {
			rc.creds.ReplaceFrom(fresh)
			out = append(out, "auth file reloaded")
		}
	}
	if rc.tlsCert != "" {
		if cfg, err := loadTLS(rc.tlsCert, rc.tlsKey); err != nil {
			out = append(out, "TLS key pair not reloaded, keeping the current certificate: "+serr.StringFromErr(err))
		} else {
			rc.srv.SetTLSConfig(cfg)
			out = append(out, "TLS key pair reloaded")
		}
	}
	if len(out) == 0 {
		out = append(out, "nothing to reload (no -auth-file or -tls-cert)")
	}
	return out
}
