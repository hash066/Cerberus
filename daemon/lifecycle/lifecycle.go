package lifecycle

import (
	"context"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
)

type LifecycleEvent string

const (
	ThermalShed   LifecycleEvent = "THERMAL_SHED"
	SleepImminent LifecycleEvent = "SLEEP_IMMINENT"
	Wake          LifecycleEvent = "WAKE"
)

type Monitor struct {
	crdt        contract.CrdtEngine
	docID       []byte
	listeners   []func(LifecycleEvent)
	cancel      context.CancelFunc
	thermalWarn bool
	sleepWarn   bool
}

func NewMonitor(crdt contract.CrdtEngine, docID []byte) *Monitor {
	return &Monitor{
		crdt:  crdt,
		docID: docID,
	}
}

func (m *Monitor) OnTransition(fn func(LifecycleEvent)) {
	m.listeners = append(m.listeners, fn)
}

func (m *Monitor) State() contract.Power {
	info, _ := host.Info()
	// Stubs for power since gopsutil host doesn't expose battery directly on all OS in v3
	src := contract.PowerAC
	batteryPct := 100.0
	hint := contract.SleepAwake

	if info != nil && info.Uptime > 0 {
		// Mock logic: assume if it's running it's AC unless we hook specific sensors
	}
	
	if m.sleepWarn {
		hint = contract.SleepImminent
	}

	return contract.Power{
		Src:        src,
		BatteryPct: batteryPct,
		Lid:        contract.LidOpen,
		Hint:       hint,
	}
}

func (m *Monitor) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkSensors()
			}
		}
	}()
}

func (m *Monitor) checkSensors() {
	// Thermal check
	temps, err := host.SensorsTemperatures()
	if err == nil {
		maxTemp := 0.0
		for _, t := range temps {
			if t.Temperature > maxTemp {
				maxTemp = t.Temperature
			}
		}
		if maxTemp > 85.0 && !m.thermalWarn {
			m.thermalWarn = true
			m.emit(ThermalShed)
		} else if maxTemp < 70.0 && m.thermalWarn {
			m.thermalWarn = false
			m.emit(Wake)
		}
	} else {
		// Fallback check using CPU percentage if temps not available
		percs, err := cpu.Percent(0, false)
		if err == nil && len(percs) > 0 {
			if percs[0] > 90.0 && !m.thermalWarn {
				m.thermalWarn = true
				m.emit(ThermalShed)
			} else if percs[0] < 50.0 && m.thermalWarn {
				m.thermalWarn = false
				m.emit(Wake)
			}
		}
	}
}

func (m *Monitor) emit(ev LifecycleEvent) {
	for _, l := range m.listeners {
		l(ev)
	}
}

func (m *Monitor) PrepareSleep() error {
	m.sleepWarn = true
	m.emit(SleepImminent)
	if m.crdt != nil {
		_, err := m.crdt.Checkpoint(m.docID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Monitor) Resume() error {
	m.sleepWarn = false
	m.emit(Wake)
	return nil
}

func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}
