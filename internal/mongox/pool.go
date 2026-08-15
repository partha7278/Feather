package mongox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/unifers/feather/internal/config"
)

// Pool holds one live connection per configured MongoDB account. Connections
// are opened on first use rather than at startup, so a cluster that is down
// only breaks the jobs that read from it.
type Pool struct {
	mu      sync.Mutex
	sources map[string]config.MongoSource
	clients map[string]*Client
}

// NewPool builds a pool over the given accounts, keyed by alias.
func NewPool(sources map[string]config.MongoSource) *Pool {
	return &Pool{sources: copySources(sources), clients: map[string]*Client{}}
}

// Get returns the client for an alias, connecting if this is its first use.
func (p *Pool) Get(ctx context.Context, alias string) (*Client, error) {
	p.mu.Lock()
	source, known := p.sources[alias]
	client, connected := p.clients[alias]
	p.mu.Unlock()

	if !known {
		return nil, fmt.Errorf("MongoDB account %q is not configured", alias)
	}
	if connected {
		return client, nil
	}

	fresh, err := Connect(ctx, source.URI, source.AuthSource, source.AppName)
	if err != nil {
		return nil, fmt.Errorf("MongoDB account %q: %w", alias, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another goroutine may have connected the same alias while this one was
	// dialling; keep theirs and drop this one rather than leaking a client.
	if existing, ok := p.clients[alias]; ok {
		go func() { _ = fresh.Close(context.WithoutCancel(ctx)) }()
		return existing, nil
	}
	p.clients[alias] = fresh
	return fresh, nil
}

// Replace swaps in a new set of accounts, disconnecting any whose settings
// changed or which no longer exist. Editing a connection string in the UI takes
// effect on the next run without restarting the process.
func (p *Pool) Replace(ctx context.Context, sources map[string]config.MongoSource) {
	p.mu.Lock()
	stale := make([]*Client, 0, len(p.clients))
	for alias, client := range p.clients {
		if next, ok := sources[alias]; !ok || next != p.sources[alias] {
			stale = append(stale, client)
			delete(p.clients, alias)
		}
	}
	p.sources = copySources(sources)
	p.mu.Unlock()

	for _, client := range stale {
		closeQuietly(ctx, client)
	}
}

// Aliases lists the configured accounts.
func (p *Pool) Aliases() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.sources))
	for alias := range p.sources {
		out = append(out, alias)
	}
	return out
}

// Close disconnects everything.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	clients := make([]*Client, 0, len(p.clients))
	for alias, client := range p.clients {
		clients = append(clients, client)
		delete(p.clients, alias)
	}
	p.mu.Unlock()

	for _, client := range clients {
		closeQuietly(ctx, client)
	}
}

// Ping opens a throwaway connection and closes it, which is how the UI tests an
// account the operator has just typed in but not yet saved.
func Ping(ctx context.Context, source config.MongoSource) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, err := Connect(ctx, source.URI, source.AuthSource, source.AppName)
	if err != nil {
		return err
	}
	return client.Close(context.WithoutCancel(ctx))
}

func closeQuietly(ctx context.Context, client *Client) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = client.Close(closeCtx)
}

func copySources(sources map[string]config.MongoSource) map[string]config.MongoSource {
	out := make(map[string]config.MongoSource, len(sources))
	for alias, source := range sources {
		out[alias] = source
	}
	return out
}
