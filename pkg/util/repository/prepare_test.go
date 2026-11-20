package repository

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	backuputil "github.com/harvester/harvester/pkg/util/backup"
)

type fakeStore struct {
	present bool
	objects []object
	more    bool
	err     error
	removed []string
	checks  int
}

func (s *fakeStore) close() {}

func (s *fakeStore) exists(context.Context, string) (bool, error) {
	s.checks++
	return s.present, s.err
}
func (s *fakeStore) list(context.Context, string) ([]object, bool, error) {
	return s.objects, s.more, s.err
}
func (s *fakeStore) remove(_ context.Context, key string) error {
	s.removed = append(s.removed, key)
	return s.err
}

func repoTarget() *settings.BackupTarget {
	return &settings.BackupTarget{Type: settings.S3BackupType, Endpoint: "http://minio.test:9000", BucketName: "test", BucketRegion: "us-east-1"}
}

func repoData() map[string][]byte {
	return map[string][]byte{
		util.AWSAccessKey: []byte("access"), util.AWSSecretKey: []byte("secret"),
		backuputil.ResticPasswordKey: []byte("password"), backuputil.KopiaPasswordKey: []byte("password"),
	}
}

func TestPrepareDecisions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		codes  []int
		create bool
		calls  string
		fail   bool
	}{
		{"existing", []int{0, 0}, true, "restic/open,kopia/connect", false},
		{"new", []int{10, 0, 0, 1, 0, 0}, true, "restic/open,restic/initialize,restic/open,kopia/connect,kopia/create,kopia/connect", false},
		{"wrong password", []int{12}, true, "restic/open", true},
		{"locked", []int{11}, true, "restic/open", true},
		{"network failure", []int{1}, true, "restic/open", true},
		{"unknown failure", []int{42}, true, "restic/open", true},
		{"unknown kopia failure", []int{0, 42}, true, "restic/open,kopia/connect", true},
		{"kopia launch failure", []int{0, -1}, true, "restic/open,kopia/connect", true},
		{"masked missing restic", []int{10}, false, "restic/open", true},
		{"masked missing kopia", []int{0, 1}, false, "restic/open,kopia/connect", true},
		{"restic creation failure", []int{10, 1}, true, "restic/open,restic/initialize", true},
		{"reopen failure", []int{10, 0, 12}, true, "restic/open,restic/initialize,restic/open", true},
		{"kopia creation failure", []int{0, 1, 1}, true, "restic/open,kopia/connect,kopia/create", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			var calls []string
			manager := Manager{store: func(*settings.BackupTarget, map[string][]byte) (objectStore, error) { return store, nil }}
			manager.run = func(_ context.Context, cmd command) error {
				calls = append(calls, cmd.engine+"/"+cmd.action)
				require.NotEmpty(t, tc.codes)
				code := tc.codes[0]
				tc.codes = tc.codes[1:]
				if code != 0 {
					return &commandError{engine: cmd.engine, action: cmd.action, code: code}
				}
				return nil
			}
			err := manager.Prepare(context.Background(), repoTarget(), repoData(), tc.create)
			require.Equal(t, tc.fail, err != nil, "%v", err)
			require.Equal(t, tc.calls, strings.Join(calls, ","))
		})
	}
}

func TestPrefixSafety(t *testing.T) {
	for _, tc := range []struct {
		name    string
		store   fakeStore
		marker  bool
		fail    bool
		removed bool
	}{
		{name: "empty"},
		{name: "existing configuration", store: fakeStore{present: true}, fail: true},
		{name: "inspection failed", store: fakeStore{err: errors.New("denied")}, fail: true},
		{name: "orphaned data", store: fakeStore{objects: []object{{key: "kopia/data"}}}, fail: true},
		{name: "truncated", store: fakeStore{more: true}, fail: true},
		{name: "kopia marker", store: fakeStore{objects: []object{{key: "kopia/"}}}, marker: true, removed: true},
		{name: "nonempty marker", store: fakeStore{objects: []object{{key: "kopia/", size: 1}}}, marker: true, fail: true},
		{name: "unknown marker size", store: fakeStore{objects: []object{{key: "kopia/", size: -1}}}, marker: true, fail: true},
		{name: "marker with data", store: fakeStore{objects: []object{{key: "kopia/"}, {key: "kopia/data"}}}, marker: true, fail: true},
		{name: "restic marker", store: fakeStore{objects: []object{{key: "kopia/"}}}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := emptyPrefix(context.Background(), &tc.store, "kopia/", "kopia.repository", tc.marker)
			require.Equal(t, tc.fail, err != nil)
			require.Equal(t, tc.removed, len(tc.store.removed) != 0)
		})
	}
}

func TestPrepareSerializesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	active, calls := 0, 0
	manager := Manager{store: func(*settings.BackupTarget, map[string][]byte) (objectStore, error) { return &fakeStore{}, nil }}
	manager.run = func(ctx context.Context, _ command) error {
		mu.Lock()
		active++
		calls++
		first := calls == 1
		if active != 1 {
			t.Error("overlapping repository processes")
		}
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if first {
			close(started)
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		select {
		case <-stopped:
			return nil
		default:
			return errors.New("previous process was not reaped")
		}
	}
	first, next := make(chan error, 1), make(chan error, 1)
	go func() { first <- manager.Prepare(ctx, repoTarget(), repoData(), true) }()
	<-started
	go func() { next <- manager.Prepare(context.Background(), repoTarget(), repoData(), true) }()
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	require.NoError(t, <-next)
	require.ErrorIs(t, manager.Prepare(ctx, repoTarget(), repoData(), true), context.Canceled)
	require.Equal(t, 3, calls)
}

func TestProcessCancellationAndPrivacy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := runCommand(ctx, command{engine: "sleep", action: "test", args: []string{"20"}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	err = runCommand(context.Background(), command{engine: "sh", action: "test", args: []string{"-c", "echo private-secret >&2; exit 12"}})
	require.Equal(t, 12, exitCode(err))
	require.NotContains(t, err.Error(), "private-secret")
	t.Setenv("AWS_SESSION_TOKEN", "ambient-token")
	t.Setenv("RESTIC_REPOSITORY", "ambient-repository")
	env, err := processEnv(repoData(), t.TempDir())
	require.NoError(t, err)
	require.NotContains(t, strings.Join(env, "\n"), "ambient-")
	require.Equal(t, "ambient-token", os.Getenv("AWS_SESSION_TOKEN"))
}
