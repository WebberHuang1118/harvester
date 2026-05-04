// Package s3 implements coordstore.Store on top of an S3-compatible backend.
//
// The implementation uses aws-sdk-go v1 (already vendored by Harvester). The
// v1 SDK's PutObjectInput does not expose the IfMatch / IfNoneMatch fields
// that AWS S3 added in November 2024, so this package injects the conditional
// headers via the request handler hook before SigV4 signing.
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	awss3 "github.com/aws/aws-sdk-go/service/s3"

	"github.com/harvester/harvester/pkg/coordstore"
)

// Config carries the parameters needed to construct an S3-backed Store.
//
// Endpoint and UsePathStyle are needed for non-AWS S3-compatible backends
// such as MinIO. CustomCAPEM may be supplied for self-signed endpoints.
type Config struct {
	Bucket          string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	UsePathStyle    bool
	CustomCAPEM     []byte
}

// Store is the S3-backed coordstore implementation.
type Store struct {
	bucket string
	client *awss3.S3
}

// Compile-time check that *Store satisfies coordstore.Store.
var _ coordstore.Store = (*Store)(nil)

// New constructs an S3-backed Store from the given Config.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("coordstore/s3: Config.Bucket is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("coordstore/s3: AccessKeyID and SecretAccessKey are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	awsCfg := aws.NewConfig().
		WithRegion(cfg.Region).
		WithCredentials(credentials.NewStaticCredentials(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)).
		WithS3ForcePathStyle(cfg.UsePathStyle)

	if cfg.Endpoint != "" {
		awsCfg = awsCfg.WithEndpoint(cfg.Endpoint)
	}

	if len(cfg.CustomCAPEM) > 0 {
		hc, err := newHTTPClientWithCA(cfg.CustomCAPEM)
		if err != nil {
			return nil, fmt.Errorf("coordstore/s3: building HTTP client with custom CA: %w", err)
		}
		awsCfg = awsCfg.WithHTTPClient(hc)
	}

	sess, err := session.NewSession(awsCfg)
	if err != nil {
		return nil, fmt.Errorf("coordstore/s3: creating AWS session: %w", err)
	}

	return &Store{bucket: cfg.Bucket, client: awss3.New(sess)}, nil
}

// Read implements coordstore.Store.
func (s *Store) Read(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObjectWithContext(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, "", coordstore.ErrNotFound
		}
		return nil, "", fmt.Errorf("coordstore/s3: GetObject %q: %w", key, err)
	}
	defer func() {
		_ = out.Body.Close()
	}()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", fmt.Errorf("coordstore/s3: reading body for %q: %w", key, err)
	}
	return data, aws.StringValue(out.ETag), nil
}

// Create implements coordstore.Store using If-None-Match: *.
func (s *Store) Create(ctx context.Context, key string, data []byte) (string, error) {
	return s.putWithCondition(ctx, key, data, "If-None-Match", "*")
}

// Update implements coordstore.Store using If-Match: <expectedVersion>.
func (s *Store) Update(ctx context.Context, key string, data []byte, expectedVersion string) (string, error) {
	if expectedVersion == "" {
		return "", errors.New("coordstore/s3: Update requires non-empty expectedVersion")
	}
	return s.putWithCondition(ctx, key, data, "If-Match", expectedVersion)
}

// Delete implements coordstore.Store. Deleting a non-existent object is not
// reported as an error, matching the interface contract.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObjectWithContext(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("coordstore/s3: DeleteObject %q: %w", key, err)
	}
	return nil
}

// Probe implements coordstore.Store. It writes a uniquely-named sentinel
// object under probePrefix twice with If-None-Match: * and verifies the
// second write is rejected with coordstore.ErrPreconditionFailed.
func (s *Store) Probe(ctx context.Context, probePrefix string) error {
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("coordstore/s3: generating probe nonce: %w", err)
	}
	key := strings.TrimSuffix(probePrefix, "/") + "/cwprobe-" + nonce

	defer func() {
		if err := s.Delete(ctx, key); err != nil {
			// Cleanup is best-effort; a leftover probe object is
			// noisy but not unsafe.
		}
	}()

	body := []byte("coordstore conditional-write probe")

	if _, err := s.Create(ctx, key, body); err != nil {
		return fmt.Errorf("first probe write failed: %w", err)
	}

	_, err = s.Create(ctx, key, body)
	if err == nil {
		return errors.New("backend does not enforce If-None-Match: second write of probe object succeeded; conditional writes are unsafe on this backend")
	}
	if !errors.Is(err, coordstore.ErrPreconditionFailed) {
		return fmt.Errorf("second probe write returned unexpected error: %w", err)
	}
	return nil
}

// putWithCondition performs a PutObject with a conditional header that the
// v1 SDK does not expose on PutObjectInput. The header is injected on the
// raw HTTP request before SigV4 signing, so the precondition is included in
// the request signature.
func (s *Store) putWithCondition(ctx context.Context, key string, data []byte, header, value string) (string, error) {
	req, out := s.client.PutObjectRequest(&awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	req.SetContext(ctx)
	req.HTTPRequest.Header.Set(header, value)

	if err := req.Send(); err != nil {
		if isPreconditionFailed(err) {
			return "", coordstore.ErrPreconditionFailed
		}
		return "", fmt.Errorf("coordstore/s3: PutObject %q with %s=%s: %w", key, header, value, err)
	}
	return aws.StringValue(out.ETag), nil
}

func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newHTTPClientWithCA(caPEM []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if ok := pool.AppendCertsFromPEM(caPEM); !ok {
		return nil, errors.New("no PEM certificates parsed from CustomCAPEM")
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}

// isPreconditionFailed reports whether err represents a 412 Precondition
// Failed response — the signal that an If-Match or If-None-Match
// precondition was not satisfied.
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	var rf awserr.RequestFailure
	if errors.As(err, &rf) && rf.StatusCode() == http.StatusPreconditionFailed {
		return true
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		switch aerr.Code() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return true
		}
	}
	return false
}

// isNotFound reports whether err represents a missing-object response from
// the backend — either the typed NoSuchKey/NotFound error code or a 404
// status.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var rf awserr.RequestFailure
	if errors.As(err, &rf) && rf.StatusCode() == http.StatusNotFound {
		return true
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		switch aerr.Code() {
		case awss3.ErrCodeNoSuchKey, "NotFound":
			return true
		}
	}
	return false
}
