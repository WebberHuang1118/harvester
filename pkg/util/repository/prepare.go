// Package repository prepares third-party repositories before credentials are published.
package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	backuputil "github.com/harvester/harvester/pkg/util/backup"
	kopiautil "github.com/harvester/harvester/pkg/util/kopia"
	resticutil "github.com/harvester/harvester/pkg/util/restic"
)

const prepareTimeout = 2 * time.Minute

// Manager serializes local initializers. The leader context cancels and reaps
// each child before releasing the mutex. Independent clusters must coordinate
// their first initialization separately; this is not storage-level fencing.
type Manager struct {
	mu    sync.Mutex
	run   func(context.Context, command) error
	store func(*settings.BackupTarget, map[string][]byte) (objectStore, error)
}

type command struct {
	engine string
	action string
	args   []string
	env    []string
}

func (m *Manager) Prepare(
	ctx context.Context, target *settings.BackupTarget, data map[string][]byte, create bool,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "harvester-repository-")
	if err != nil {
		return fmt.Errorf("creating repository workspace: %w", err)
	}
	defer os.RemoveAll(dir)
	env, err := processEnv(data, dir)
	if err != nil {
		return err
	}
	store := m.store
	if store == nil {
		store = newStore
	}
	backend, err := store(target, data)
	if err != nil {
		return err
	}
	defer backend.close()
	if err := m.prepareRestic(ctx, target, env, backend, create); err != nil {
		return err
	}
	return m.prepareKopia(ctx, target, env, dir, backend, create)
}

func (m *Manager) execute(ctx context.Context, cmd command) error {
	if m.run != nil {
		return m.run(ctx, cmd)
	}
	return runCommand(ctx, cmd)
}

func (m *Manager) prepareRestic(
	ctx context.Context, target *settings.BackupTarget, env []string, store objectStore, create bool,
) error {
	repo, err := resticutil.Repository(target)
	if err != nil {
		return err
	}
	env = append(env, "RESTIC_REPOSITORY="+repo.URL)
	args := []string{"--no-cache", "-o", "s3.region=" + repo.Region}
	probe := command{engine: "restic", action: "open", args: append(args, "cat", "config"), env: env}
	err = m.execute(ctx, probe)
	if err == nil || !create || exitCode(err) != 10 {
		return err
	}
	if err := emptyPrefix(ctx, store, "restic/", "config", false); err != nil {
		return fmt.Errorf("preparing Restic: %w", err)
	}
	init := command{engine: "restic", action: "initialize", args: append(args, "init"), env: env}
	if err := m.execute(ctx, init); err != nil {
		return err
	}
	return m.execute(ctx, probe)
}

func (m *Manager) prepareKopia(
	ctx context.Context, target *settings.BackupTarget, env []string, dir string, store objectStore, create bool,
) error {
	repo, err := kopiautil.S3Repository(target)
	if err != nil {
		return err
	}
	probe := kopiaCommand(repo, "connect", dir, env)
	err = m.execute(ctx, probe)
	if err == nil || !create || exitCode(err) != 1 {
		return err
	}
	if err := emptyPrefix(ctx, store, "kopia/", "kopia.repository", true); err != nil {
		return fmt.Errorf("preparing Kopia: %w", err)
	}
	if err := m.execute(ctx, kopiaCommand(repo, "create", dir, env)); err != nil {
		return err
	}
	// Always verify through a fresh local connection after creation.
	if err := os.Remove(filepath.Join(dir, "kopia.config")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("resetting Kopia connection: %w", err)
	}
	return m.execute(ctx, probe)
}

func kopiaCommand(repo *kopiautil.Repository, action, dir string, env []string) command {
	args := []string{
		"--no-auto-maintenance", "--config-file=" + filepath.Join(dir, "kopia.config"),
		"repository", action, "s3", "--bucket=" + repo.Bucket, "--endpoint=" + repo.Endpoint,
		"--region=" + repo.Region, "--prefix=" + repo.Prefix,
		"--cache-directory=" + filepath.Join(dir, "cache"),
		"--content-cache-size-mb=0", "--metadata-cache-size-mb=0",
	}
	if repo.DisableTLS {
		args = append(args, "--disable-tls")
	}
	return command{engine: "kopia", action: action, args: args, env: env}
}

func credentialEnv(data map[string][]byte) []string {
	env := make([]string, 0, 4)
	for _, key := range []string{
		util.AWSAccessKey, util.AWSSecretKey, backuputil.ResticPasswordKey, backuputil.KopiaPasswordKey,
	} {
		env = append(env, key+"="+string(data[key]))
	}
	return env
}
