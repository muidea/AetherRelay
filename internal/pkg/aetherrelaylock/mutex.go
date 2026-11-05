// Package aetherrelaylock provides cancellation-aware storage lock admission.
package aetherrelaylock

import (
	"context"
	"sync"
)

// Mutex is a zero-value-ready mutex. It must not be copied after first use.
type Mutex struct {
	once sync.Once
	busy chan struct{}
}

func (m *Mutex) Lock() { _ = m.LockContext(context.Background()) }

func (m *Mutex) LockContext(ctx context.Context) error {
	m.once.Do(func() { m.busy = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.busy <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Mutex) Unlock() { <-m.busy }
