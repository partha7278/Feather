package dest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/unifers/feather/internal/config"
)

// Registry holds one live client per configured destination alias. It is
// rebuilt whenever an operator adds or edits an account in the UI, so a new
// bucket is usable without restarting the process.
type Registry struct {
	mu      sync.RWMutex
	byAlias map[string]Destination
}

// NewRegistry builds clients for every destination given.
func NewRegistry(ctx context.Context, destinations map[string]config.Destination) (*Registry, error) {
	registry := &Registry{byAlias: map[string]Destination{}}
	if err := registry.Replace(ctx, destinations); err != nil {
		return nil, err
	}
	return registry, nil
}

// NewRegistryWith builds a registry over destinations that already exist.
//
// The normal path constructs real S3 and Drive clients from stored credentials,
// which leaves no way to exercise the runner without a cloud account. This is
// the seam: the safety property that nothing is deleted until every copy is
// stored and verified is worth a test, and a test needs a destination it can
// make fail on demand.
func NewRegistryWith(destinations map[string]Destination) *Registry {
	byAlias := make(map[string]Destination, len(destinations))
	for alias, destination := range destinations {
		byAlias[alias] = destination
	}
	return &Registry{byAlias: byAlias}
}

// Replace swaps the whole set of clients. Building every client first means a
// single bad account leaves the previous, working set in place.
func (r *Registry) Replace(ctx context.Context, destinations map[string]config.Destination) error {
	next := make(map[string]Destination, len(destinations))
	for alias, cfg := range destinations {
		d, err := New(ctx, cfg)
		if err != nil {
			return err
		}
		next[alias] = d
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAlias = next
	return nil
}

// Get resolves an alias to its client.
func (r *Registry) Get(alias string) (Destination, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	d, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("destination %q is not configured", alias)
	}
	return d, nil
}

// Resolve turns a job's alias list into clients, preserving the configured
// order so logs and catalogue rows read consistently.
func (r *Registry) Resolve(aliases []string) ([]Destination, error) {
	out := make([]Destination, 0, len(aliases))
	for _, alias := range aliases {
		d, err := r.Get(alias)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Aliases lists every configured destination, sorted.
func (r *Registry) Aliases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.byAlias))
	for alias := range r.byAlias {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// CheckAll reaches every destination concurrently, so credential and bucket
// problems surface before a scheduled run reads a single document.
func (r *Registry) CheckAll(ctx context.Context) error {
	// Every destination is checked even when one fails, so the operator sees all
	// the problems at once instead of fixing them one restart at a time.
	var (
		group    errgroup.Group
		mu       sync.Mutex
		failures []string
	)

	r.mu.RLock()
	targets := make([]Destination, 0, len(r.byAlias))
	for _, d := range r.byAlias {
		targets = append(targets, d)
	}
	r.mu.RUnlock()

	for _, d := range targets {
		group.Go(func() error {
			if err := d.Check(ctx); err != nil {
				mu.Lock()
				failures = append(failures, err.Error())
				mu.Unlock()
			}
			return nil
		})
	}
	_ = group.Wait()

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("destination check failed:\n  - %s", strings.Join(failures, "\n  - "))
	}
	return nil
}

// CheckEach reports the outcome per alias, which is what the UI shows next to
// each account rather than one combined error.
func (r *Registry) CheckEach(ctx context.Context) map[string]error {
	r.mu.RLock()
	targets := make(map[string]Destination, len(r.byAlias))
	for alias, d := range r.byAlias {
		targets[alias] = d
	}
	r.mu.RUnlock()

	var (
		mu      sync.Mutex
		group   errgroup.Group
		results = make(map[string]error, len(targets))
	)
	for alias, d := range targets {
		group.Go(func() error {
			err := d.Check(ctx)
			mu.Lock()
			results[alias] = err
			mu.Unlock()
			return nil
		})
	}
	_ = group.Wait()
	return results
}

// New builds the client one destination's provider calls for.
func New(ctx context.Context, cfg config.Destination) (Destination, error) {
	if cfg.Provider == config.ProviderGDrive {
		return NewGDriveDestination(ctx, cfg)
	}
	return NewS3Destination(ctx, cfg)
}

// Probe tests one destination without adding it to the registry — how the UI
// checks credentials an operator has typed but not yet saved.
func Probe(ctx context.Context, cfg config.Destination) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	d, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	return d.Check(ctx)
}
