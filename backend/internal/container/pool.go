package container

import (
	"context"
	"log"
	"time"
)

type Pool struct {
	mgr   Manager
	image string
	size  int
	ready chan string
	stop  chan struct{}
}

func NewPool(mgr Manager, image string, size int) *Pool {
	p := &Pool{
		mgr:   mgr,
		image: image,
		size:  size,
		ready: make(chan string, size),
		stop:  make(chan struct{}),
	}
	go p.maintain()
	return p
}

func (p *Pool) maintain() {
	p.fill()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.fill()
		}
	}
}

func (p *Pool) fill() {
	for len(p.ready) < p.size {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		id, err := p.mgr.Create(ctx, CreateOpts{
			Image: p.image,
			Labels: map[string]string{
				"k8s-quiz":      "true",
				"k8s-quiz.pool": "warm",
			},
			CPULimit:    1_000_000_000,
			MemoryLimit: 1073741824,
			Privileged:  true,
		})
		if err != nil {
			cancel()
			log.Printf("pool: create warm container failed: %v", err)
			return
		}

		err = p.mgr.WaitReady(ctx, id, func() bool {
			result, execErr := p.mgr.Exec(ctx, id, []string{"kubectl", "get", "nodes"})
			return execErr == nil && result.ExitCode == 0
		}, 90*time.Second)
		cancel()
		if err != nil {
			p.mgr.Remove(context.Background(), id)
			log.Printf("pool: warm container not ready: %v", err)
			return
		}

		select {
		case p.ready <- id:
		default:
			p.mgr.Remove(context.Background(), id)
			return
		}
	}
}

func (p *Pool) Acquire() (string, bool) {
	select {
	case id := <-p.ready:
		return id, true
	default:
		return "", false
	}
}

func (p *Pool) Len() int {
	return len(p.ready)
}

func (p *Pool) Drain(ctx context.Context) {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	for {
		select {
		case id := <-p.ready:
			p.mgr.Remove(ctx, id)
		default:
			return
		}
	}
}
