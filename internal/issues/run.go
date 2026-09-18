package issues

import (
	"context"
	"sync"
	"time"
)

// Run is the desk loop: one goroutine per enabled driver (healthcheck + replay
// timers) plus one quiet-close sweep goroutine. Total goroutines for the package
// is ≤ 3 in v0.1 (2 drivers maximum), inside the ≤ 8 budget of §2.2.
func (d *Desk) Run(ctx context.Context) {
	if !d.cfg.Enabled {
		// SPEC-09 §3.4a: a disabled desk starts no loop — no driver
		// healthcheck, no spool replay and no quiet-close sweep. It holds no
		// driver and no anchor, so there is nothing for a loop to serve.
		return
	}
	var wg sync.WaitGroup
	for _, name := range d.order {
		wg.Add(1)
		go func(driver string) {
			defer wg.Done()
			d.driverLoop(ctx, driver)
		}(name)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.sweepLoop(ctx)
	}()
	wg.Wait()
}

func (d *Desk) healthInterval(driver string) time.Duration {
	busy := d.spoolCount(driver) > 0
	if !busy {
		d.mu.Lock()
		for _, a := range d.anchors {
			if a.driver == driver && a.ref.State != "closed" && a.ref.ExternalID != "" {
				busy = true
				break
			}
		}
		d.mu.Unlock()
	}
	if busy {
		if iv := d.cfg.HealthcheckEvery.Std(); iv > 0 {
			return iv
		}
		return time.Minute
	}
	if iv := d.cfg.HealthcheckIdle.Std(); iv > 0 {
		return iv
	}
	return 5 * time.Minute
}

func (d *Desk) driverLoop(ctx context.Context, driver string) {
	health := time.NewTicker(d.healthInterval(driver))
	defer health.Stop()
	replay := time.NewTicker(d.replayInterval())
	defer replay.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-health.C:
			d.probe(ctx, driver)
			health.Reset(d.healthInterval(driver))
		case <-replay.C:
			if d.driverHealthy(driver) && d.ReplayDue(driver) {
				_, _ = d.Replay(ctx, d.cfg.ReplayBatch)
			}
		}
	}
}

func (d *Desk) sweepLoop(ctx context.Context) {
	iv := d.cfg.QuietCloseSweep.Std()
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = d.SweepQuietClose(ctx, d.now())
		}
	}
}

func (d *Desk) replayInterval() time.Duration {
	if iv := d.cfg.ReplayInterval.Std(); iv > 0 {
		return iv
	}
	return 5 * time.Second
}
