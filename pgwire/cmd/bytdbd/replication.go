package main

// replication.go: replication and restore from the daemon (deep-look
// D-58), so shipping the log to an S3-compatible bucket no longer
// takes a Go program around the replicate package.
//
//	-restore-from s3://bucket/prefix   at startup, if the database file
//	                                   does not exist, assemble it from
//	                                   the replica first
//	-replicate-s3 s3://bucket/prefix   ship the log there while serving
//
// The bucket's endpoint and region come from -s3-endpoint / -s3-region
// (AWS's regional endpoint by default); credentials only from the
// environment (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, optionally
// AWS_SESSION_TOKEN), never argv, where ps and shell history would
// show them. The metrics listener reports replication progress and
// answers /healthz.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/replicate"
	"github.com/rohanthewiz/bytdb/replicate/s3"
	"github.com/rohanthewiz/serr"
)

// s3Location is a parsed s3://bucket/prefix flag value.
type s3Location struct {
	bucket, prefix string
}

// parseS3URL reads s3://bucket[/prefix]. The prefix namespaces one
// database's objects, so several can share a bucket.
func parseS3URL(raw string) (s3Location, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return s3Location{}, serr.New("replica location must look like s3://bucket/prefix", "value", raw)
	}
	return s3Location{bucket: u.Host, prefix: strings.TrimPrefix(u.Path, "/")}, nil
}

// s3Settings are the flags shared by -replicate-s3 and -restore-from.
type s3Settings struct {
	endpoint, region string
	virtualHost      bool
}

// client builds the S3 client for loc from the settings and the
// environment's credentials.
func (st s3Settings) client(loc s3Location) (*s3.Client, error) {
	region := st.region
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	endpoint := st.endpoint
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	access, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if access == "" || secret == "" {
		return nil, serr.New("replication needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the environment")
	}
	return s3.New(s3.Config{
		Endpoint: endpoint, Region: region, Bucket: loc.bucket,
		AccessKey: access, SecretKey: secret, SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
		VirtualHost: st.virtualHost,
	})
}

// restoreIfMissing assembles dbPath from the replica when it does not
// exist yet — the safe reading of -restore-from: a database already on
// disk is never overwritten (Restore would refuse a live one anyway).
// It reports whether a restore ran.
func restoreIfMissing(ctx context.Context, store replicate.Storage, prefix, dbPath string) (bool, *replicate.RestoreInfo, error) {
	if _, err := os.Stat(dbPath); err == nil {
		return false, nil, nil
	} else if !os.IsNotExist(err) {
		return false, nil, serr.Wrap(err, "op", "stat database before restore", "path", dbPath)
	}
	info, err := replicate.Restore(ctx, store, prefix, dbPath)
	if err != nil {
		return false, nil, err
	}
	return true, info, nil
}

// replication is the running replicator as the metrics listener sees
// it; the zero value (r nil) means replication is off.
type replication struct {
	r        *replicate.Replicator
	interval time.Duration
	started  time.Time
}

// status reports the replicator's Status; ok is false when off.
func (rp replication) status() (replicate.Status, bool) {
	if rp.r == nil {
		return replicate.Status{}, false
	}
	return rp.r.Status(), true
}

// failing reports a replica that has stopped keeping up: the last ship
// attempt failed and no ship has succeeded for three intervals (or, if
// none ever has, the replicator has run that long). One failed tick is
// ordinary — the loop retries — so it alone does not flip health.
func (rp replication) failing(st replicate.Status, now time.Time) bool {
	if st.LastError == nil {
		return false
	}
	since := rp.started
	if !st.LastShipTime.IsZero() {
		since = st.LastShipTime
	}
	return now.Sub(since) > 3*rp.interval
}

// writeMetrics appends the replication gauges to a /metrics response,
// in the engine's exposition format and prefix.
func (rp replication) writeMetrics(w interface{ Write([]byte) (int, error) }) {
	st, ok := rp.status()
	if !ok {
		return
	}
	gauge := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	b := func(x bool) int {
		if x {
			return 1
		}
		return 0
	}
	gauge("bytdb_replication_lag_bytes", "Log bytes not yet shipped as of the last attempt.", st.LagBytes)
	gauge("bytdb_replication_watermark_bytes", "Log bytes shipped in the current generation.", st.Watermark)
	gauge("bytdb_replication_sealed", "1 once the current generation is certified complete (restorable).", b(st.Sealed))
	last := int64(0)
	if !st.LastShipTime.IsZero() {
		last = st.LastShipTime.Unix()
	}
	gauge("bytdb_replication_last_ship_timestamp_seconds", "Unix time of the last successful ship (0 = none yet).", last)
	gauge("bytdb_replication_failing", "1 when the last ship attempt failed.", b(st.LastError != nil))
}

// healthHandler answers /healthz: 200 with {"status":"ok"} while the
// engine reads and replication (if on) is keeping up, 503 with
// {"status":"unhealthy"} and the reason otherwise. The replication
// figures ride along either way, for a dashboard's convenience.
func healthHandler(e *bytdb.Engine, rp replication) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		type replJSON struct {
			Generation   string `json:"generation"`
			Sealed       bool   `json:"sealed"`
			LagBytes     int64  `json:"lag_bytes"`
			LastShipTime string `json:"last_ship_time,omitempty"`
			LastError    string `json:"last_error,omitempty"`
		}
		out := struct {
			Status      string    `json:"status"`
			Reason      string    `json:"reason,omitempty"`
			Replication *replJSON `json:"replication,omitempty"`
		}{Status: "ok"}
		if _, _, err := e.LogState(); err != nil {
			out.Status, out.Reason = "unhealthy", "engine: "+err.Error()
		}
		if st, ok := rp.status(); ok {
			rj := &replJSON{Generation: st.Generation, Sealed: st.Sealed, LagBytes: st.LagBytes}
			if !st.LastShipTime.IsZero() {
				rj.LastShipTime = st.LastShipTime.UTC().Format(time.RFC3339)
			}
			if st.LastError != nil {
				rj.LastError = st.LastError.Error()
			}
			out.Replication = rj
			if out.Status == "ok" && rp.failing(st, time.Now()) {
				out.Status, out.Reason = "unhealthy", "replication has not shipped for over three intervals"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if out.Status != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(out)
	})
}
