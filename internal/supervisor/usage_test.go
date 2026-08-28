package supervisor

import (
	"testing"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/dto"
)

// fakeSource lets the percentage arithmetic be checked against counters chosen
// on purpose. Sampling the real machine could only assert "some number came
// back", which would not catch a wrong denominator.
type fakeSource struct {
	stat platform.MachineStat
	tree map[int]platform.TreeStat
}

func (f *fakeSource) machine() (platform.MachineStat, error) { return f.stat, nil }

func (f *fakeSource) trees([]int) map[int]platform.TreeStat { return f.tree }

const gib = 1 << 30

func newTestSampler(source *fakeSource, clock *time.Time) *usageSampler {
	sampler := newUsageSampler(source)
	sampler.now = func() time.Time { return *clock }
	return sampler
}

func TestUsageNeedsTwoSamplesForCPU(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	source := &fakeSource{
		stat: platform.MachineStat{
			Cores: 4, MemTotal: 16 * gib, MemUsed: 8 * gib,
			Busy: 10 * time.Second, Total: 100 * time.Second,
		},
		tree: map[int]platform.TreeStat{100: {CPUTime: 2 * time.Second, RSS: 512 * 1024 * 1024, Procs: 3}},
	}
	sampler := newTestSampler(source, &now)
	roots := map[string]int{"p/web": 100}

	sampler.sample(roots)
	first := sampler.forService("p/web")
	if first == nil {
		t.Fatal("no reading after the first sample")
	}
	if first.CPUPercent != 0 {
		t.Errorf("cpu is %.2f after one sample; a percentage needs an interval", first.CPUPercent)
	}
	// Memory is an instantaneous quantity, so it must be usable immediately.
	if first.MemoryBytes != 512*1024*1024 {
		t.Errorf("memory is %d bytes", first.MemoryBytes)
	}
	if got := first.MemoryPercent; got < 3.1 || got > 3.2 {
		t.Errorf("memory is %.2f%% of 16GiB, want about 3.125", got)
	}
	if first.Procs != 3 {
		t.Errorf("procs is %d", first.Procs)
	}

	// One second later the tree has used half a second of CPU. With four cores
	// that is an eighth of the machine.
	now = now.Add(time.Second)
	source.tree[100] = platform.TreeStat{CPUTime: 2500 * time.Millisecond, RSS: 512 * 1024 * 1024, Procs: 3}
	source.stat.Busy += 800 * time.Millisecond
	source.stat.Total += 4 * time.Second
	sampler.sample(roots)

	second := sampler.forService("p/web")
	if second == nil {
		t.Fatal("no reading after the second sample")
	}
	if got := second.CPUPercent; got < 12.4 || got > 12.6 {
		t.Errorf("cpu is %.2f%%, want 12.5 (0.5s of a 4-core second)", got)
	}

	machine := sampler.machineUsage()
	if got := machine.CPUPercent; got < 19.9 || got > 20.1 {
		t.Errorf("machine cpu is %.2f%%, want 20 (0.8s busy of 4s elapsed cpu time)", got)
	}
	if machine.Cores != 4 {
		t.Errorf("cores is %d", machine.Cores)
	}
	if got := machine.MemoryPercent; got < 49.9 || got > 50.1 {
		t.Errorf("machine memory is %.2f%%, want 50", got)
	}
}

func TestUsageForgetsAServiceThatStopped(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	source := &fakeSource{
		stat: platform.MachineStat{Cores: 2, MemTotal: 8 * gib, MemUsed: gib},
		tree: map[int]platform.TreeStat{100: {CPUTime: time.Second, RSS: 1024, Procs: 1}},
	}
	sampler := newTestSampler(source, &now)
	sampler.sample(map[string]int{"p/web": 100})
	if sampler.forService("p/web") == nil {
		t.Fatal("no reading for a running service")
	}

	// The service stopped, so it is no longer a root. Keeping the old reading
	// would leave a stopped service showing the CPU it used before it died.
	now = now.Add(time.Second)
	sampler.sample(map[string]int{})
	if usage := sampler.forService("p/web"); usage != nil {
		t.Errorf("a stopped service still reports %+v", *usage)
	}
}

func TestUsageIgnoresAnUnmeasurableService(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	source := &fakeSource{
		stat: platform.MachineStat{Cores: 2, MemTotal: 8 * gib, MemUsed: gib},
		tree: map[int]platform.TreeStat{},
	}
	sampler := newTestSampler(source, &now)
	// A compose or external service has no host pid; asking about it must not
	// invent a zero reading, which would read as "using nothing".
	sampler.sample(map[string]int{"p/db": 0})
	if usage := sampler.forService("p/db"); usage != nil {
		t.Errorf("a service with no pid reports %+v", *usage)
	}
}

func TestUsageWillNotBillNegativeCPU(t *testing.T) {
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	source := &fakeSource{
		stat: platform.MachineStat{Cores: 2, MemTotal: 8 * gib, MemUsed: gib},
		tree: map[int]platform.TreeStat{100: {CPUTime: 10 * time.Second, RSS: 2048, Procs: 4}},
	}
	sampler := newTestSampler(source, &now)
	roots := map[string]int{"p/web": 100}
	sampler.sample(roots)

	// A worker process exited, so its CPU time left the tree's sum. That is not
	// negative work.
	now = now.Add(time.Second)
	source.tree[100] = platform.TreeStat{CPUTime: 6 * time.Second, RSS: 1024, Procs: 3}
	sampler.sample(roots)

	usage := sampler.forService("p/web")
	if usage == nil {
		t.Fatal("no reading")
	}
	if usage.CPUPercent != 0 {
		t.Errorf("cpu is %.2f%% after the counter went backwards", usage.CPUPercent)
	}
}

func TestSumUsage(t *testing.T) {
	sampled := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	later := sampled.Add(time.Second)

	if total := SumUsage([]dto.Service{{Name: "db"}, {Name: "web"}}); total != nil {
		t.Errorf("a project with nothing measurable totals %+v, want no total at all", *total)
	}

	total := SumUsage([]dto.Service{
		{Name: "db"},
		{Name: "web", Usage: &dto.Usage{CPUPercent: 12, MemoryBytes: 100, MemoryPercent: 1, Procs: 2, SampledAt: sampled}},
		{Name: "api", Usage: &dto.Usage{CPUPercent: 8, MemoryBytes: 200, MemoryPercent: 2, Procs: 1, SampledAt: later}},
	})
	if total == nil {
		t.Fatal("no total for a project with two measured services")
	}
	if total.CPUPercent != 20 {
		t.Errorf("cpu total is %.2f, want 20", total.CPUPercent)
	}
	if total.MemoryBytes != 300 {
		t.Errorf("memory total is %d, want 300", total.MemoryBytes)
	}
	if total.Procs != 3 {
		t.Errorf("procs total is %d, want 3", total.Procs)
	}
	if !total.SampledAt.Equal(later) {
		t.Errorf("sampled at %v, want the most recent reading %v", total.SampledAt, later)
	}
}
