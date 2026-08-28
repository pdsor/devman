package supervisor

import (
	"testing"

	"github.com/devman-project/devman/pkg/dto"
)

func declared(name string, desired dto.DesiredState, status dto.ProcessStatus, health dto.HealthStatus) dto.Service {
	return dto.Service{
		Name:         name,
		DesiredState: desired,
		Status:       status,
		Health:       dto.HealthResult{Status: health},
	}
}

func TestSummariseIgnoresServicesNobodyAskedToRun(t *testing.T) {
	cases := []struct {
		name     string
		services []dto.Service
		want     dto.ProjectStatus
		running  int
		total    int
	}{
		{
			name: "an optional worker left stopped does not degrade the project",
			services: []dto.Service{
				declared("web", dto.DesiredRunning, dto.StatusRunning, dto.HealthHealthy),
				declared("worker", dto.DesiredStopped, dto.StatusStopped, dto.HealthUnknown),
			},
			want:    dto.ProjectHealthy,
			running: 1,
			total:   2,
		},
		{
			name: "process-only health still counts as healthy",
			services: []dto.Service{
				declared("web", dto.DesiredRunning, dto.StatusRunning, dto.HealthNotApplicable),
				declared("worker", dto.DesiredStopped, dto.StatusStopped, dto.HealthUnknown),
			},
			want:    dto.ProjectHealthy,
			running: 1,
			total:   2,
		},
		{
			name: "a service that should be running and is not is still degraded",
			services: []dto.Service{
				declared("web", dto.DesiredRunning, dto.StatusRunning, dto.HealthHealthy),
				declared("worker", dto.DesiredRunning, dto.StatusCrashed, dto.HealthUnknown),
			},
			want:    dto.ProjectDegraded,
			running: 1,
			total:   2,
		},
		{
			name: "an unhealthy probe on a wanted service is still degraded",
			services: []dto.Service{
				declared("web", dto.DesiredRunning, dto.StatusRunning, dto.HealthUnhealthy),
				declared("worker", dto.DesiredStopped, dto.StatusStopped, dto.HealthUnknown),
			},
			want:    dto.ProjectDegraded,
			running: 1,
			total:   2,
		},
		{
			name: "everything deliberately stopped reads as stopped, not failed",
			services: []dto.Service{
				declared("web", dto.DesiredStopped, dto.StatusStopped, dto.HealthUnknown),
				declared("worker", dto.DesiredStopped, dto.StatusStopped, dto.HealthUnknown),
			},
			want:    dto.ProjectStopped,
			running: 0,
			total:   2,
		},
		{
			name: "a stopped service that crashed on the way out is not counted",
			services: []dto.Service{
				declared("web", dto.DesiredRunning, dto.StatusRunning, dto.HealthHealthy),
				declared("worker", dto.DesiredStopped, dto.StatusCrashed, dto.HealthUnknown),
			},
			want:    dto.ProjectHealthy,
			running: 1,
			total:   2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, status := Summarise(tc.services)
			if status != tc.want {
				t.Errorf("status is %s, want %s", status, tc.want)
			}
			if summary.Running != tc.running {
				t.Errorf("running is %d, want %d", summary.Running, tc.running)
			}
			// Total counts every declared service so a caller can still show that
			// something is idle.
			if summary.Total != tc.total {
				t.Errorf("total is %d, want %d", summary.Total, tc.total)
			}
		})
	}
}
