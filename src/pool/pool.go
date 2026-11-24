package pool

import (
	"context"
	"sync"
)

type Pool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func New(n int) *Pool {
	if n < 1 {
		n = 1
	}
	return &Pool{sem: make(chan struct{}, n)}
}
func (p *Pool) Go(ctx context.Context, fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-p.sem }()
		fn()
	}()
}
func (p *Pool) Wait() { p.wg.Wait() }
