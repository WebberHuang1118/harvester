// cwprobe verifies that an S3-compatible target correctly implements the
// conditional-write semantics required by Harvester DR coordination
// (pkg/coordstore). It runs the standard If-None-Match probe and exercises
// the compare-and-swap semantics end-to-end (Create / Update / Read), then
// reports per-check PASS / FAIL.
//
// Use this tool before relying on a target as a DR coordination plane. A
// backend that fails any check cannot safely arbitrate ownership between
// cooperating clusters.
//
// Usage:
//
//	cwprobe -url <backup-target-url> [-prefix <path>] [-keep]
//
// Flags:
//
//	-url     Backup target URL, same format as Harvester's backup-target
//	         setting (e.g. s3://bucket@region/some/prefix). Required.
//	         Defaults to $BACKUPSTORE_URL.
//	-prefix  Path under the URL root for probe and CAS test objects
//	         (default: "cwprobe").
//	-keep    Do not clean up test objects on completion. Useful for
//	         post-mortem inspection on failure.
//
// Credentials and endpoint configuration are read from the environment in
// the same way as Harvester's backup-target setup:
//
//	AWS_ACCESS_KEY_ID
//	AWS_SECRET_ACCESS_KEY
//	AWS_SESSION_TOKEN  (optional)
//	AWS_ENDPOINTS      (optional, e.g. http://minio:9000 for non-AWS endpoints)
//	AWS_CERT           (optional, PEM-encoded custom CA for TLS endpoints)
//
// Exit codes:
//
//	0 — all checks passed; backend is safe for CAS-based coordination.
//	1 — at least one check failed; backend is NOT safe.
//	2 — setup error (bad URL, credentials, connectivity).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/harvester/harvester/pkg/coordstore"
	s3store "github.com/harvester/harvester/pkg/coordstore/s3"
)

const (
	exitOK         = 0
	exitCheckFail  = 1
	exitSetupError = 2

	defaultPrefix = "cwprobe"
	casObjectName = "cas-test"
)

func main() {
	rawURL := flag.String("url", os.Getenv("BACKUPSTORE_URL"),
		"backup target URL (e.g. s3://bucket@region/path). Defaults to $BACKUPSTORE_URL.")
	prefix := flag.String("prefix", defaultPrefix,
		"path under the URL root for probe and CAS test objects.")
	keep := flag.Bool("keep", false,
		"do not clean up test objects on completion (useful for post-mortem inspection).")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"cwprobe — verify an S3-compatible target supports conditional writes for DR coordination.\n\n"+
				"Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *rawURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required (or set BACKUPSTORE_URL).")
		flag.Usage()
		os.Exit(exitSetupError)
	}

	cfg, bucketPrefix, err := configFromURL(*rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(exitSetupError)
	}

	store, err := s3store.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to construct S3 store: %v\n", err)
		os.Exit(exitSetupError)
	}

	fullPrefix := path.Join(bucketPrefix, *prefix)
	fmt.Printf("Backend: s3 bucket=%s region=%s endpoint=%s\n",
		cfg.Bucket, cfg.Region, displayEndpoint(cfg.Endpoint))
	fmt.Printf("Prefix:  %s\n\n", fullPrefix)

	ctx := context.Background()
	results := runChecks(ctx, store, fullPrefix)

	if *keep {
		fmt.Printf("\n-keep set: leaving test objects under %q\n", fullPrefix)
	} else {
		if err := cleanup(ctx, store, fullPrefix); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup failed: %v\n", err)
		}
	}

	failed := 0
	for _, r := range results {
		if !r.passed {
			failed++
		}
	}
	fmt.Printf("\nResult: %d/%d checks passed\n", len(results)-failed, len(results))
	if failed > 0 {
		fmt.Println("Backend is NOT safe for CAS-based coordination.")
		os.Exit(exitCheckFail)
	}
	fmt.Println("Backend is safe for CAS-based coordination.")
	os.Exit(exitOK)
}

type checkResult struct {
	name   string
	passed bool
	detail string
}

// runChecks executes every check in sequence and returns the recorded
// results. The first failing check (probe or initial Create) short-circuits
// the rest because subsequent checks would just produce noise once the
// backend is known to be unsafe.
func runChecks(ctx context.Context, store coordstore.Store, prefix string) []checkResult {
	var results []checkResult
	record := func(name string, err error, detail string) {
		passed := err == nil
		mark := "PASS"
		if !passed {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %s", mark, name)
		if detail != "" {
			fmt.Printf(" — %s", detail)
		}
		if err != nil {
			fmt.Printf(" (err: %v)", err)
		}
		fmt.Println()
		results = append(results, checkResult{name: name, passed: passed, detail: detail})
	}

	// 1. Probe.
	probeErr := store.Probe(ctx, prefix)
	record("probe If-None-Match enforcement", probeErr,
		"second write of identical key must be rejected")
	if probeErr != nil {
		return results
	}

	// 2. Read of a missing object returns ErrNotFound.
	_, _, readMissingErr := store.Read(ctx, path.Join(prefix, "missing"))
	if errors.Is(readMissingErr, coordstore.ErrNotFound) {
		record("Read(missing) returns ErrNotFound", nil, "")
	} else {
		record("Read(missing) returns ErrNotFound",
			classifyUnexpected("ErrNotFound", readMissingErr), "")
	}

	casPath := path.Join(prefix, casObjectName)

	// 3. Create succeeds for a new key.
	v1, createErr := store.Create(ctx, casPath, []byte("v1"))
	record("Create(new key) succeeds", createErr,
		fmt.Sprintf("returned version=%q", v1))
	if createErr != nil {
		return results
	}

	// 4. Create on an existing key is rejected.
	_, recreateErr := store.Create(ctx, casPath, []byte("v1-dup"))
	if errors.Is(recreateErr, coordstore.ErrPreconditionFailed) {
		record("Create(existing key) rejected", nil, "")
	} else {
		record("Create(existing key) rejected",
			classifyUnexpected("ErrPreconditionFailed", recreateErr), "")
	}

	// 5. Update with matching version succeeds.
	v2, updateMatchErr := store.Update(ctx, casPath, []byte("v2"), v1)
	record("Update(matching version) succeeds", updateMatchErr,
		fmt.Sprintf("new version=%q", v2))
	if updateMatchErr != nil {
		return results
	}

	// 5b. Update returns a fresh version. A backend that returns the same
	// ETag on update would defeat the lease-renewal pattern — a secondary
	// would have no way to observe that the primary renewed.
	if v2 == v1 {
		record("Update returns fresh version",
			fmt.Errorf("backend returned same ETag (%q) on update; CAS would not detect concurrent writes", v1), "")
	} else {
		record("Update returns fresh version", nil, "")
	}

	// 6. Update with stale version is rejected.
	_, updateStaleErr := store.Update(ctx, casPath, []byte("v3"), v1)
	if errors.Is(updateStaleErr, coordstore.ErrPreconditionFailed) {
		record("Update(stale version) rejected", nil, "")
	} else {
		record("Update(stale version) rejected",
			classifyUnexpected("ErrPreconditionFailed", updateStaleErr), "")
	}

	// 7. Read reflects the latest write and returns the latest version.
	data, vRead, readErr := store.Read(ctx, casPath)
	switch {
	case readErr != nil:
		record("Read(after CAS) returns latest content+version", readErr, "")
	case string(data) != "v2":
		record("Read(after CAS) returns latest content+version",
			fmt.Errorf("body mismatch: got %q, want %q", string(data), "v2"), "")
	case vRead != v2:
		record("Read(after CAS) returns latest content+version",
			fmt.Errorf("version mismatch: got %q, want %q", vRead, v2), "")
	default:
		record("Read(after CAS) returns latest content+version", nil,
			fmt.Sprintf("body=%q version=%q", string(data), vRead))
	}

	return results
}

// classifyUnexpected turns a nil-or-wrong-type error into a descriptive
// error for the report. If err is nil, the operation unexpectedly
// succeeded; if err is non-nil but of the wrong type, the backend signals
// the wrong thing.
func classifyUnexpected(want string, got error) error {
	if got == nil {
		return fmt.Errorf("expected %s, but operation unexpectedly succeeded", want)
	}
	return fmt.Errorf("expected %s, got %v", want, got)
}

// cleanup removes objects we know we created. The probe sentinel is
// auto-removed by Store.Probe in its deferred cleanup, so only the CAS test
// object needs explicit deletion here.
func cleanup(ctx context.Context, store coordstore.Store, prefix string) error {
	return store.Delete(ctx, path.Join(prefix, casObjectName))
}

func configFromURL(rawURL string) (s3store.Config, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return s3store.Config{}, "", fmt.Errorf("parsing URL: %w", err)
	}
	if u.Scheme != "s3" {
		return s3store.Config{}, "", fmt.Errorf("URL scheme %q not supported (need s3://)", u.Scheme)
	}
	if u.User == nil || u.User.Username() == "" {
		return s3store.Config{}, "", errors.New("URL must encode bucket as user, e.g. s3://bucket@region/")
	}

	endpoint := os.Getenv("AWS_ENDPOINTS")
	cfg := s3store.Config{
		Bucket:          u.User.Username(),
		Region:          u.Host,
		Endpoint:        endpoint,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		UsePathStyle:    endpoint != "",
	}
	if cert := os.Getenv("AWS_CERT"); cert != "" {
		cfg.CustomCAPEM = []byte(cert)
	}

	bucketPrefix := strings.TrimPrefix(u.Path, "/")
	bucketPrefix = strings.TrimSuffix(bucketPrefix, "/")

	return cfg, bucketPrefix, nil
}

func displayEndpoint(endpoint string) string {
	if endpoint == "" {
		return "<aws-default>"
	}
	return endpoint
}
