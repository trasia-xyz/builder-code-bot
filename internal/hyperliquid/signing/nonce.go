package signing

import (
	"sync"
	"time"
)

type NonceGenerator struct {
	mu   sync.Mutex
	last uint64
	now  func() time.Time
}

func NewNonceGenerator() *NonceGenerator {
	return newNonceGenerator(time.Now)
}

func newNonceGenerator(now func() time.Time) *NonceGenerator {
	return &NonceGenerator{now: now}
}

func (g *NonceGenerator) Next() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	current := uint64(g.now().UnixMilli())
	if current <= g.last {
		current = g.last + 1
	}
	g.last = current
	return current
}
