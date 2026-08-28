package supervisor

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
)

// The sampler is the only place percentages are computed. platform hands out
// cumulative counters; a percentage describes an interval, so it needs two
// readings and a clock, and both live here.
//
// One tick measures everything: the machine, and every running service's whole
// process tree. Sampling per request instead would be worse in two ways — a
// single reading cannot produce a CPU percentage at all, and a GUI polling three
// views would pay for three walks of the machine's process table.

// usageSource is the seam tests replace so the percentage arithmetic can be
// checked against numbers chosen on purpose rather than whatever the test
// machine happened to be doing.
type usageSource interface {
	machine() (platform.MachineStat, error)
	trees(roots []int) map[int]platform.TreeStat
}

type hostSource struct{}

func (hostSource) machine() (platform.MachineStat, error) { return platform.SampleMachine() }

func (hostSource) trees(roots []int) map[int]platform.TreeStat {
	return platform.SampleTrees(roots)
}

// usageInterval is how often the counters are read. One second matches what the
// desktop displays and is slow enough that walking the process table costs
// nothing measurable.
const usageInterval = time.Second

type cpuMark struct {
	cpu time.Duration
	at  time.Time
}

type usageSampler struct {
	source usageSource
	now    func() time.Time

	mu        sync.Mutex
	services  map[string]dto.Usage
	machine   dto.MachineUsage
	prevProc  map[string]cpuMark
	prevBusy  time.Duration
	prevTotal time.Duration
	hasPrev   bool
}

func newUsageSampler(source usageSource) *usageSampler {
	return &usageSampler{
		source:   source,
		now:      time.Now,
		services: map[string]dto.Usage{},
		prevProc: map[string]cpuMark{},
	}
}

// run samples until the context is cancelled. It takes one sample immediately so
// memory figures are available before the first tick, even though CPU needs a
// second reading to mean anything.
func (u *usageSampler) run(ctx context.Context, roots func() map[string]int) {
	ticker := time.NewTicker(usageInterval)
	defer ticker.Stop()
	u.sample(roots())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.sample(roots())
		}
	}
}

// sample reads one set of counters and converts them into percentages.
//
// roots maps a service key to the pid of its tree. A service absent from roots
// is forgotten entirely: keeping its last reading would leave a stopped service
// showing the CPU it used just before it died.
func (u *usageSampler) sample(roots map[string]int) {
	at := u.now()
	stat, err := u.source.machine()
	cores := stat.Cores
	if err != nil || cores < 1 {
		cores = runtime.NumCPU()
	}

	pids := make([]int, 0, len(roots))
	for _, pid := range roots {
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	trees := u.source.trees(pids)

	u.mu.Lock()
	defer u.mu.Unlock()

	if err == nil {
		u.machine = dto.MachineUsage{
			Cores:            cores,
			MemoryUsedBytes:  stat.MemUsed,
			MemoryTotalBytes: stat.MemTotal,
			MemoryPercent:    ratio(float64(stat.MemUsed), float64(stat.MemTotal)),
			SampledAt:        at,
		}
		if u.hasPrev {
			// Both counters are cumulative, so the busy share of the elapsed
			// CPU time is the utilisation over exactly this interval.
			u.machine.CPUPercent = ratio(
				float64(stat.Busy-u.prevBusy),
				float64(stat.Total-u.prevTotal),
			)
		}
		u.prevBusy, u.prevTotal, u.hasPrev = stat.Busy, stat.Total, true
	}

	fresh := make(map[string]dto.Usage, len(roots))
	marks := make(map[string]cpuMark, len(roots))
	for key, pid := range roots {
		tree, measured := trees[pid]
		if !measured || tree.Procs == 0 {
			continue
		}
		usage := dto.Usage{
			MemoryBytes:   tree.RSS,
			MemoryPercent: ratio(float64(tree.RSS), float64(stat.MemTotal)),
			Procs:         tree.Procs,
			SampledAt:     at,
		}
		if previous, ok := u.prevProc[key]; ok {
			elapsed := at.Sub(previous.at)
			// A tree can lose a process between two ticks, which takes its CPU
			// time out of the sum. That is not negative work; it is an interval
			// this service cannot be billed for.
			if elapsed > 0 && tree.CPUTime > previous.cpu {
				spent := tree.CPUTime - previous.cpu
				capacity := time.Duration(cores) * elapsed
				usage.CPUPercent = ratio(float64(spent), float64(capacity))
			}
		}
		fresh[key] = usage
		marks[key] = cpuMark{cpu: tree.CPUTime, at: at}
	}
	u.services = fresh
	u.prevProc = marks
}

// forService returns the last reading for one service, or nil when there is
// none: a service with no host process tree, or one that has not been sampled
// since it started.
func (u *usageSampler) forService(key string) *dto.Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	usage, ok := u.services[key]
	if !ok {
		return nil
	}
	return &usage
}

// machineUsage returns the last host reading.
func (u *usageSampler) machineUsage() dto.MachineUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.machine
}

// ratio is a percentage that refuses to divide by zero or exceed the whole.
func ratio(part, whole float64) float64 {
	if whole <= 0 || part <= 0 {
		return 0
	}
	percent := part / whole * 100
	if percent > 100 {
		// Rounding between two counters read microseconds apart can put a fully
		// busy machine slightly over 100, which would look like a bug.
		return 100
	}
	return percent
}

// SumUsage totals the services that are being measured.
//
// It answers "what is this project costing me", so a service with no reading
// contributes nothing rather than making the total nil. The result is nil only
// when no service in the project could be measured at all.
func SumUsage(services []dto.Service) *dto.Usage {
	total := dto.Usage{}
	found := false
	for _, svc := range services {
		if svc.Usage == nil {
			continue
		}
		found = true
		total.CPUPercent += svc.Usage.CPUPercent
		total.MemoryBytes += svc.Usage.MemoryBytes
		total.MemoryPercent += svc.Usage.MemoryPercent
		total.Procs += svc.Usage.Procs
		if svc.Usage.SampledAt.After(total.SampledAt) {
			total.SampledAt = svc.Usage.SampledAt
		}
	}
	if !found {
		return nil
	}
	if total.CPUPercent > 100 {
		total.CPUPercent = 100
	}
	if total.MemoryPercent > 100 {
		total.MemoryPercent = 100
	}
	return &total
}
