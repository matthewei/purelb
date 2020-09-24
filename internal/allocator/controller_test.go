// Copyright 2020 Acnodal Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"fmt"
	"testing"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func diffService(a, b *v1.Service) string {
	// v5 of the k8s client does not correctly compare nil
	// *metav1.Time objects, which svc.ObjectMeta contains. Add
	// some dummy non-nil values to all of in, want, got to work
	// around this until we migrate to v6.
	if a != nil {
		newA := new(v1.Service)
		*newA = *a
		newA.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		a = newA
	}
	if b != nil {
		newB := new(v1.Service)
		*newB = *b
		newB.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		b = newB
	}
	return cmp.Diff(a, b)
}

func statusAssigned(ip string) v1.ServiceStatus {
	return v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP: ip,
				},
			},
		},
	}
}

// testK8S implements service by recording what the controller wants
// to do to k8s.
type testK8S struct {
	loggedWarning bool
	t             *testing.T
}

func (s *testK8S) Infof(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func (s *testK8S) reset() {
	s.loggedWarning = false
}

func TestControllerConfig(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		logger: log.NewNopLogger(),
		ips:    New(),
		client: k,
	}

	// Create service that would need an IP allocation

	svc := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer("test", svc, nil), "SetBalancer should have failed")
	assert.False(t, k.loggedWarning, "SetBalancer with no configuration logged an error")

	// Set an empty config. Balancer should still not do anything to
	// our unallocated service, and return an error to force a
	// retry after sync is complete.
	wantSvc := svc.DeepCopy()
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(&purelbv1.Config{}), "SetConfig with empty config failed")
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer("test", svc, nil), "SetBalancer did not fail")

	assert.Empty(t, diffService(wantSvc, svc), "unsynced SetBalancer mutated service")
	assert.False(t, k.loggedWarning, "unsynced SetBalancer logged an error")

	// Set a config with some IPs. Still no allocation, not synced.
	cfg := &purelbv1.Config{
		Groups: []*purelbv1.ServiceGroup{
			&purelbv1.ServiceGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: purelbv1.ServiceGroupSpec{
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/24",
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg), "SetConfig failed")
	wantSvc = svc.DeepCopy()
	assert.Equal(t, k8s.SyncStateError, c.SetBalancer("test", svc, nil), "SetBalancer did not fail")

	assert.Empty(t, diffService(wantSvc, svc), "unsynced SetBalancer mutated service")
	assert.False(t, k.loggedWarning, "unsynced SetBalancer logged an error")

	// Mark synced. Finally, we can allocate.
	c.MarkSynced()

	wantSvc = svc.DeepCopy()
	wantSvc.Status = statusAssigned("1.2.3.0")
	wantSvc.ObjectMeta = metav1.ObjectMeta{
		Annotations: map[string]string{
			purelbv1.BrandAnnotation: purelbv1.Brand,
			purelbv1.PoolAnnotation:  "default",
		},
	}

	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer("test", svc, nil), "SetBalancer failed")

	assert.Empty(t, diffService(wantSvc, svc), "SetBalancer produced unexpected mutation")

	// Deleting the config also makes PureLB sad.
	assert.Equal(t, k8s.SyncStateError, c.SetConfig(nil), "SetConfig that deletes the config was accepted")
}

func TestDeleteRecyclesIP(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		logger: log.NewNopLogger(),
		ips:    New(),
		client: k,
	}

	cfg := &purelbv1.Config{
		Groups: []*purelbv1.ServiceGroup{
			&purelbv1.ServiceGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "default"},
				Spec: purelbv1.ServiceGroupSpec{
					Local: &purelbv1.ServiceGroupLocalSpec{
						Pool: "1.2.3.0/32",
					},
				},
			},
		},
	}
	assert.Equal(t, k8s.SyncStateReprocessAll, c.SetConfig(cfg), "SetConfig failed")
	c.MarkSynced()

	svc1 := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer("test", svc1, nil), "SetBalancer svc1 failed")
	assert.NotEmpty(t, svc1.Status.LoadBalancer.Ingress, "svc1 didn't get an IP")
	assert.Equal(t, "1.2.3.0", svc1.Status.LoadBalancer.Ingress[0].IP, "svc1 got the wrong IP")
	k.reset()

	// Second service should converge correctly, but not allocate an
	// IP because we have none left.
	svc2 := &v1.Service{
		Spec: v1.ServiceSpec{
			Type:      "LoadBalancer",
			ClusterIP: "1.2.3.4",
		},
	}
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer("test2", svc2, nil), "SetBalancer svc2 failed")
	assert.Empty(t, svc2.Status.LoadBalancer.Ingress, "svc2 didn't get an IP")
	k.reset()

	// Deleting the first LB should tell us to reprocess all services.
	assert.Equal(t, k8s.SyncStateReprocessAll, c.DeleteBalancer("test"), "DeleteBalancer didn't tell us to reprocess all balancers")

	// Setting svc2 should now allocate correctly.
	assert.Equal(t, k8s.SyncStateSuccess, c.SetBalancer("test2", svc2, nil), "SetBalancer svc2 failed")
	assert.NotEmpty(t, svc2.Status.LoadBalancer.Ingress, "svc2 didn't get an IP")
	assert.Equal(t, "1.2.3.0", svc2.Status.LoadBalancer.Ingress[0].IP, "svc2 got the wrong IP")
	}
	if len(svc2.Status.LoadBalancer.Ingress) == 0 || svc2.Status.LoadBalancer.Ingress[0].IP != "1.2.3.0" {
		t.Fatal("svc2 didn't get an IP")
	}
}
