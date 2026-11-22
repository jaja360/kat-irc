package main

import (
	"context"
	"sync"
	"time"
)

// typingSession keeps +typing active below the IRCv3 six-second expiry. The
// returned stop function waits for renewal to finish before sending done.
func typingSession(ctx context.Context, interval time.Duration, emit func(string)) func() {
	emit("active")
	stop, stopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				emit("active")
			}
		}
	}()
	return func() { once.Do(func() { close(stop); <-stopped; emit("done") }) }
}
