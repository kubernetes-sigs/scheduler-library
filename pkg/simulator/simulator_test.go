package simulator

import (
	"testing"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)


func init() {
	metrics.Register()
}

func TestNewSchedulingSimulator(t *testing.T) {
	cfg := &schedulerapi.KubeSchedulerConfiguration{}
	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	sim := NewSchedulingSimulator(cfg, informerFactory)
	if sim == nil {
		t.Fatal("Expected simulator to be non-nil")
	}
}


func TestNewClusterState(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "default-scheduler"},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
			sim := NewSchedulingSimulator(tc.cfg, informerFactory)
			ctx := t.Context()

			state, err := sim.NewClusterState(ctx)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterState err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && state == nil {
				t.Fatal("Expected state to be non-nil")
			}
		})
	}

}

func TestNewClusterSnapshot(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "default-scheduler"},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
			sim := NewSchedulingSimulator(tc.cfg, informerFactory)
			ctx := t.Context()

			snapshot, err := sim.NewClusterSnapshot(ctx, nil, nil)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterSnapshot err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && snapshot == nil {
				t.Fatal("Expected snapshot to be non-nil")
			}
		})
	}

}
