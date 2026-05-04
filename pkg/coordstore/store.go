// Package coordstore defines a small compare-and-swap (CAS) object store
// abstraction used by Harvester for distributed coordination — currently the
// per-VM ownership object that arbitrates which cluster is primary in the
// cross-cluster DR design.
//
// The interface deliberately stays narrow (Read / Create / Update / Delete /
// Probe) so backends with different on-the-wire mechanisms (S3 conditional
// writes, etcd transactions, NFSv4 byte-range locks, ...) can implement it
// uniformly. Consumers obtain a Store from a backend constructor (for example
// pkg/coordstore/s3.New) and then operate on it using opaque, driver-defined
// version identifiers.
package coordstore

import (
	"context"
	"errors"
)

// ErrPreconditionFailed is returned by Create or Update when the backend
// rejects the write because the caller-supplied precondition (object must not
// exist, or current version must match expected) is not satisfied.
//
// Callers should treat this as a "lost the race" signal — the operation did
// not apply because the object's observed state on the server did not match
// the caller's expectation — rather than as a generic transport error.
var ErrPreconditionFailed = errors.New("coordstore: conditional write precondition failed")

// ErrNotFound is returned by Read when the requested object does not exist.
// It is a typed signal so that callers bootstrapping a shared coordination
// object (read-then-create-if-missing) can distinguish "object absent" from
// a transport or backend error.
var ErrNotFound = errors.New("coordstore: object not found")

// Store is the compare-and-swap object store interface.
//
// Versions are opaque strings whose format is backend-defined (for example an
// S3 ETag). Callers must not interpret them; they should be passed back to
// Update unchanged.
type Store interface {
	// Read returns the object body and its current opaque version.
	// If the object does not exist, Read returns ErrNotFound.
	Read(ctx context.Context, key string) (data []byte, version string, err error)

	// Create writes an object only if it does not already exist. If the
	// object already exists, Create returns ErrPreconditionFailed. On
	// success it returns the version of the newly written object.
	Create(ctx context.Context, key string, data []byte) (version string, err error)

	// Update writes an object only if its current version matches
	// expectedVersion. If the version does not match — including the case
	// where the object has been deleted — Update returns
	// ErrPreconditionFailed. On success it returns the version of the
	// newly written object.
	Update(ctx context.Context, key string, data []byte, expectedVersion string) (newVersion string, err error)

	// Delete removes the object. Deleting a non-existent object is not an
	// error.
	Delete(ctx context.Context, key string) error

	// Probe verifies that the backend correctly enforces the
	// "create-if-not-exists" precondition by writing a sentinel object
	// under probePrefix twice. The first write must succeed; the second
	// must fail with ErrPreconditionFailed. The sentinel is removed
	// before the method returns, regardless of outcome.
	//
	// probePrefix is caller-controlled so the sentinel can live under a
	// path the caller already has permission to write to and is willing
	// to clutter briefly.
	//
	// Probe returns nil if the backend correctly enforces the
	// precondition, or a non-nil error otherwise — including the case
	// where the backend silently allows the second write to succeed,
	// which is the unsafe condition the probe exists to detect.
	Probe(ctx context.Context, probePrefix string) error
}
