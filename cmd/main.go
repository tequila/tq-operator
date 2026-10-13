// Command tq-operator is the Tequila estate operator at rung observe: it reads what the cluster
// runs and what Flux applied, compares it with the Estate the tenant's render declared, writes
// the comparison into Estate.status and reports the same payload to the console.
//
// It opens no socket: no metrics server, no health-probe server, no webhook server. Its one
// credential is the projected ServiceAccount token; it reaches IAM and the console and nothing
// else.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/controller"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/options"
	"github.com/tequila/tq-operator/internal/report"
	"github.com/tequila/tq-operator/internal/supplychain"
	"github.com/tequila/tq-operator/internal/version"
	"github.com/tequila/tq-operator/internal/watchdog"
)

// Exit codes: 0 a requested restart (a kind became readable) or a signal; 1 a stall or a
// failure; 2 a configuration the operator refuses to start with.
const (
	exitRestart = 0
	exitFailure = 1
	exitConfig  = 2
)

func main() {
	var opts options.Options
	opts.Bind(flag.CommandLine)
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	os.Exit(run(opts))
}

func run(opts options.Options) int {
	log := ctrl.Log.WithName("tq-operator")
	iamURL, consoleURL, err := opts.Validate()
	if err != nil {
		log.Error(err, "refusing to start")
		return exitConfig
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "no Kubernetes configuration")
		return exitConfig
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, estatev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			log.Error(err, "scheme")
			return exitFailure
		}
	}
	namespaces := observe.Namespaces{
		Own: opts.Namespace, Platform: opts.PlatformNamespace, Services: opts.ServicesNamespace, Flux: opts.FluxNamespace,
	}

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	stop := &stopper{cancel: cancel, code: exitRestart}

	// Probe before the manager exists: only what is served and permitted becomes an informer.
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "API client")
		return exitFailure
	}
	kinds := observe.Probe(ctx, direct, direct.RESTMapper(), namespaces)
	for _, gvk := range observe.AllKinds() {
		log.Info("probe", "kind", gvk.Kind, "access", kinds[gvk].Access, "detail", kinds[gvk].Detail)
	}

	syncPeriod := opts.Interval
	mgr, err := ctrl.NewManager(cfg, managerOptions(scheme, controller.CacheOptions(namespaces, kinds, &syncPeriod)))
	if err != nil {
		log.Error(err, "manager")
		return exitFailure
	}

	discoveryConfig := rest.CopyConfig(cfg)
	discoveryConfig.Timeout = 15 * time.Second
	disc, err := discovery.NewDiscoveryClientForConfig(discoveryConfig)
	if err != nil {
		log.Error(err, "discovery")
		return exitFailure
	}
	observer := &observe.Observer{
		Reader:        mgr.GetClient(),
		Kinds:         kinds,
		FluxNamespace: namespaces.Flux,
		ServerVersion: func(context.Context) (string, error) {
			v, err := disc.ServerVersion()
			if err != nil {
				return "", err
			}
			return v.GitVersion, nil
		},
	}
	started := time.Now()
	iamClient := &report.Client{
		IAM: iamURL, Console: consoleURL, TokenFile: opts.IAMTokenFile, HTTP: report.NewHTTPClient(),
	}
	reporter := &report.Reporter{Client: iamClient, Started: started}

	// The self-check: the rendered keys, the image's signature from its registry (the
	// projected token exchanged at IAM for the registry's audience is the registry's
	// password), once now and then once per interval. Every outcome is a status, never an exit.
	checker := &supplychain.Checker{
		Verify:   selfVerifier(opts, iamClient, log),
		Image:    opts.Image,
		Interval: opts.Interval,
		Log:      log.WithName("self-verification"),
	}
	checker.Run(ctx)
	cfgCommon := controller.Config{
		Namespaces:        namespaces,
		Interval:          opts.Interval,
		ReportOwnServices: opts.ReportOwnServices,
		Operator: report.Operator{
			Version:      version.Version,
			Image:        opts.Image,
			Capabilities: []estatev1alpha1.Capability{estatev1alpha1.CapabilityObserve},
		},
		SelfVerification: checker.Current,
	}

	wd := &watchdog.Watchdog{
		Limit:       3 * opts.Interval,
		SyncTimeout: 5 * time.Minute,
		WaitForSync: mgr.GetCache().WaitForCacheSync,
		Stop:        func(reason string) { stop.Stop(exitFailure, reason) },
	}
	if kinds.Observed(observe.EstateGVK) {
		r := &controller.EstateReconciler{
			Client:   mgr.GetClient(),
			Observer: observer,
			Reporter: reporter,
			// core/v1 Events: the operator's RBAC grants `events` in the core group only.
			Recorder: mgr.GetEventRecorderFor("tq-operator"), //nolint:staticcheck // see above
			Watchdog: wd,
			Config:   cfgCommon,
		}
		if err := r.SetupWithManager(mgr, kinds); err != nil {
			log.Error(err, "controller")
			return exitFailure
		}
	} else {
		log.Info("the Estate kind is not readable here; reporting without an Estate", "access", kinds[observe.EstateGVK].Access)
	}
	loop := &controller.NoEstateLoop{
		Reader:        mgr.GetClient(),
		Observer:      observer,
		Reporter:      reporter,
		Watchdog:      wd,
		Config:        cfgCommon,
		EstatesServed: kinds.Observed(observe.EstateGVK),
		Reprobe: func(ctx context.Context) {
			now := observe.Probe(ctx, direct, direct.RESTMapper(), namespaces)
			if !now.Equal(kinds) {
				stop.Stop(exitRestart, "what the operator may read changed (a CRD or a Role appeared or went); restarting to watch it")
			}
		},
	}
	for _, runnable := range []interface {
		Start(context.Context) error
	}{loop, wd, checker} {
		if err := mgr.Add(runnableFunc(runnable.Start)); err != nil {
			log.Error(err, "runnable")
			return exitFailure
		}
	}

	log.Info("starting", "version", version.Version, "namespaces", fmt.Sprintf("%+v", namespaces), "interval", opts.Interval.String())
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager stopped")
		return exitFailure
	}
	code, reason := stop.Result()
	if reason != "" {
		log.Info("stopped", "code", code, "reason", reason)
	}
	return code
}

// selfVerifier builds the check of the operator's own image: the signing keys the render
// mounted (a file that fails to parse is a result, not a refusal to start) and a registry
// client whose password is the projected token exchanged at IAM for the registry's audience.
// An exchange IAM refuses is `unverified` with reason registry-auth — once per interval, never
// a retry storm: the checker's rhythm is the only rhythm.
func selfVerifier(opts options.Options, iam *report.Client, log logr.Logger) func(context.Context) supplychain.Result {
	keys, keysErr := supplychain.LoadKeys(opts.SigningKeys)
	if keysErr != nil {
		log.Error(keysErr, "the signing-keys file is not usable; the operator's image stays unverified", "file", opts.SigningKeys)
	}
	verifier := &supplychain.Verifier{
		Keys:      keys,
		SignerKid: opts.SignerKid,
		// Where the image is a copy (by digest) of the hosted registry's, the signature is read
		// there, with the same token.
		SignatureRegistry: opts.SignatureRegistry,
		Transport:         report.NewTransport(),
		UserAgent:         "tq-operator/" + version.Version,
		Credential: func(ctx context.Context) (string, string, error) {
			_, token, failed := iam.Exchange(ctx, report.RegistryAudience)
			if failed != nil {
				return "", failed.Message, errors.New(failed.Code)
			}
			return token, "", nil
		},
	}
	return func(ctx context.Context) supplychain.Result {
		if keysErr != nil {
			r := supplychain.Result{Signature: supplychain.Unverified, CheckedAt: time.Now(), Reason: supplychain.ReasonKeysMalformed,
				Message: "the signing-keys file is not usable: " + keysErr.Error()}
			return r
		}
		return verifier.Verify(ctx, opts.Image)
	}
}

// managerOptions are the manager's options: every one a choice from the audit envelope
// (docs/audit-envelope.md). main_test.go holds them in place.
func managerOptions(scheme *runtime.Scheme, cacheOpts cache.Options) ctrl.Options {
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"}, // no metrics server
		HealthProbeBindAddress: "0",                                     // no probe server — the watchdog instead
		PprofBindAddress:       "",                                      // no pprof server
		LeaderElection:         false,                                   // one replica, Recreate; no Lease write
		WebhookServer:          noWebhooks{},                            // nothing listens on 9443, by construction
		Cache:                  cacheOpts,
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	}
}

// stopper ends the manager with an exit code and a reason (the watchdog, the re-probe).
type stopper struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	code   int
	reason string
}

func (s *stopper) Stop(code int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "" {
		s.code, s.reason = code, reason
	}
	s.cancel()
}

func (s *stopper) Result() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.reason
}

type runnableFunc func(context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }

// NeedLeaderElection: nothing in the operator elects.
func (runnableFunc) NeedLeaderElection() bool { return false }

// noWebhooks is the manager's webhook server: it refuses to start, so a webhook registered by
// mistake fails the operator loudly instead of opening port 9443.
type noWebhooks struct{}

var errNoWebhooks = errors.New("tq-operator serves no webhooks: no admission path exists, by construction")

func (noWebhooks) NeedLeaderElection() bool      { return false }
func (noWebhooks) Register(string, http.Handler) { panic(errNoWebhooks) }
func (noWebhooks) Start(context.Context) error   { return errNoWebhooks }
func (noWebhooks) StartedChecker() healthz.Checker {
	return func(*http.Request) error { return errNoWebhooks }
}
func (noWebhooks) WebhookMux() *http.ServeMux { panic(errNoWebhooks) }
