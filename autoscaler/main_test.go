package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Shrunk once, before any test goroutine starts: the leader election timings are
// production values, too slow to sit through in a unit test.
func TestMain(m *testing.M) {
	leaseDuration, renewDeadline, retryPeriod = 2*time.Second, time.Second, 100*time.Millisecond
	os.Exit(m.Run())
}

// Two replicas contend for one Lease: only the holder may reconcile. Without
// that, both compute the same next index from the same Proxmox VM list and race
// on the same VMID.
func TestRunAsLeader_OnlyOneReplicaReconciles(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leading := make(chan string, 2)
	for _, identity := range []string{"replica-a", "replica-b"} {
		go func(identity string) {
			runAsLeader(ctx, client, "autoscaler-system", identity, func(ctx context.Context) {
				leading <- identity
				<-ctx.Done()
			})
		}(identity)
	}

	var leader string
	select {
	case leader = <-leading:
	case <-time.After(retryPeriod * 20):
		t.Fatal("no replica acquired the lease")
	}

	// The standby keeps retrying but must never start reconciling.
	select {
	case second := <-leading:
		t.Fatalf("%q and %q both became leader, both replicas would reconcile", leader, second)
	case <-time.After(retryPeriod * 3):
	}

	lease, err := client.CoordinationV1().Leases("autoscaler-system").Get(ctx, leaseName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, leader, *lease.Spec.HolderIdentity)
}

// The leader must give the Lease up on shutdown so the standby takes over at
// once instead of waiting out the lease duration.
func TestRunAsLeader_ReleasesLeaseOnShutdown(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())

	leading := make(chan struct{})
	released := make(chan struct{})
	go func() {
		runAsLeader(ctx, client, "autoscaler-system", "replica-a", func(ctx context.Context) {
			close(leading)
			<-ctx.Done()
		})
		close(released)
	}()

	select {
	case <-leading:
	case <-time.After(retryPeriod * 20):
		t.Fatal("replica-a never acquired the lease")
	}

	cancel()
	select {
	case <-released:
	case <-time.After(retryPeriod * 20):
		t.Fatal("runAsLeader did not return after shutdown")
	}

	lease, err := client.CoordinationV1().Leases("autoscaler-system").Get(context.Background(), leaseName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, *lease.Spec.HolderIdentity)
}
