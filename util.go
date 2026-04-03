package main

import (
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"
)

const maxEntries = 10000 // Rotate when current map reaches this size (~2.5MB)

var (
	current      atomic.Pointer[sync.Map]
	previous     atomic.Pointer[sync.Map]
	currentCount int64
	rotateMu     sync.Mutex
)

func init() {
	current.Store(&sync.Map{})
	previous.Store(&sync.Map{})
}

func rateLimitAllow(addr string) bool {
	ip := addr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		ip = host
	}

	if atomic.LoadInt64(&currentCount) >= maxEntries {
		rotate()
	}

	cur := current.Load()
	if val, ok := cur.Load(ip); ok {
		return val.(*rate.Limiter).Allow()
	}

	prev := previous.Load()
	if val, ok := prev.Load(ip); ok {
		cur.Store(ip, val)
		atomic.AddInt64(&currentCount, 1)
		return val.(*rate.Limiter).Allow()
	}

	limiter := rate.NewLimiter(100.0/60, 10)
	cur.Store(ip, limiter)
	atomic.AddInt64(&currentCount, 1)
	return limiter.Allow()
}

func rotate() {
	rotateMu.Lock()
	defer rotateMu.Unlock()
	// Double-check under lock
	if atomic.LoadInt64(&currentCount) < maxEntries {
		return
	}
	previous.Store(current.Load())
	current.Store(&sync.Map{})
	atomic.StoreInt64(&currentCount, 0)
}
