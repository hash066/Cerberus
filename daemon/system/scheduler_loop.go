package system

import (
	"context"
	"fmt"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/telemetry"
)

// schedulerLoopConfig wires peer telemetry consumption into the placement brain.
type schedulerLoopConfig struct {
	Sched    *scheduler.Scheduler
	Fabric   *mesh.Fabric
	Site     string
	TopicCap contract.CapHandle
	Sample   func() contract.NodeTelemetry
}

// schedulerLoop subscribes to cerberus/<site>/telemetry/** and feeds decoded
// NodeTelemetry into the scheduler. It also refreshes the local node's sample on
// a ticker so CPU core counts stay current even without inbound telemetry.
func schedulerLoop(ctx context.Context, cfg schedulerLoopConfig) error {
	if cfg.Sched == nil {
		return fmt.Errorf("scheduler loop: nil scheduler")
	}
	key := fmt.Sprintf("cerberus/%s/telemetry/**", cfg.Site)
	var sub <-chan contract.Sample
	if cfg.Fabric != nil && cfg.TopicCap != 0 {
		ch, err := cfg.Fabric.Subscribe(ctx, key, cfg.TopicCap)
		if err != nil {
			return fmt.Errorf("scheduler telemetry subscribe: %w", err)
		}
		sub = ch
	}

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if sub == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				if cfg.Sample != nil {
					cfg.Sched.UpdateNode(cfg.Sample())
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sample, open := <-sub:
			if !open {
				sub = nil
				continue
			}
			tel, err := telemetry.Decode(sample.Payload)
			if err != nil {
				continue
			}
			cfg.Sched.UpdateNode(tel)
		case <-t.C:
			if cfg.Sample != nil {
				cfg.Sched.UpdateNode(cfg.Sample())
			}
		}
	}
}
