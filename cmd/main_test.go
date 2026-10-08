package main

import (
	"context"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// No listening socket (the audit envelope, line 4): no metrics server, no health-probe server,
// no pprof server, no webhook server, no leader election.
func TestTheManagerOpensNoSocket(t *testing.T) {
	o := managerOptions(runtime.NewScheme(), cache.Options{})
	if o.Metrics.BindAddress != "0" {
		t.Errorf("metrics bind address %q — \"0\" disables the server", o.Metrics.BindAddress)
	}
	if o.HealthProbeBindAddress != "0" {
		t.Errorf("health probe bind address %q", o.HealthProbeBindAddress)
	}
	if o.PprofBindAddress != "" {
		t.Errorf("pprof bind address %q", o.PprofBindAddress)
	}
	if o.LeaderElection {
		t.Error("leader election writes a Lease; the operator runs one replica")
	}
	if _, ok := o.WebhookServer.(noWebhooks); !ok {
		t.Errorf("webhook server %T", o.WebhookServer)
	}
}

func TestTheWebhookServerRefusesToServe(t *testing.T) {
	var s noWebhooks
	if err := s.Start(context.Background()); err == nil {
		t.Error("the webhook server started")
	}
	if err := s.StartedChecker()(&http.Request{}); err == nil {
		t.Error("the webhook server claims to have started")
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a webhook did not fail loudly")
		}
	}()
	s.Register("/validate", http.NotFoundHandler())
}
