package keypool

import (
	"context"

	"github.com/RC-CHN/command-code-reverse/proxy/internal/commandcode"
	"github.com/RC-CHN/command-code-reverse/proxy/internal/session"
)

type SystemOneClient interface {
	SystemOne(context.Context, commandcode.Credentials, *commandcode.SystemOneRequest, int64) (*commandcode.SystemOneResponse, error)
}

// SystemOnePool owns independent leases, cooldowns and half-open probes.
// It shares the chat pool's algorithm and configured keys, never its state.
type SystemOnePool struct {
	pool             *Pool
	client           SystemOneClient
	maxResponseBytes int64
}

func NewSystemOne(client SystemOneClient, sessions *session.Store, keys []string, policy BreakerPolicy, maxResponseBytes int64) *SystemOnePool {
	pool := New(nil, sessions, keys, policy)
	pool.systemOne = true
	return &SystemOnePool{pool: pool, client: client, maxResponseBytes: maxResponseBytes}
}

func (p *SystemOnePool) SystemOne(ctx context.Context, hint string, meta commandcode.CallMeta, req *commandcode.SystemOneRequest) (*commandcode.SystemOneResponse, error) {
	return withKey(ctx, p.pool, hint, func(key string) (*commandcode.SystemOneResponse, error) {
		return p.client.SystemOne(ctx, p.pool.creds(key, meta), req, p.maxResponseBytes)
	})
}
