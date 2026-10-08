// Package controller is the Estate controller at rung observe: reconcile → observe → compare →
// status → report.
package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/report"
)

// Condition types and reasons of Estate.status.conditions.
const (
	ConditionReady    = "Ready"
	ConditionReported = "Reported"
	ConditionDrifted  = "Drifted"

	ReasonReconciled             = "Reconciled"
	ReasonCacheNotSynced         = "CacheNotSynced"
	ReasonSelfVerificationFailed = "SelfVerificationFailed"
	ReasonDisabled               = "Disabled"
)

// Debounce is the shortest gap between two reconciles a burst of watched changes causes.
const Debounce = 30 * time.Second

// Config is the reconciler's fixed configuration, from the flags.
type Config struct {
	// Namespaces the operator watches; an Estate naming others is observed in these and says so.
	Namespaces observe.Namespaces
	// Interval is the default heartbeat (spec.operator.report.interval overrides it).
	Interval time.Duration
	// ReportOwnServices is the render's switch; spec.operator.report.ownServices is the
	// tenant's. A tenant's own services are reported only when both allow it.
	ReportOwnServices bool
	// Operator is the reporting operator's identity in the report.
	Operator report.Operator
	// SelfVerification is the result of the operator's check of its own image; a failure
	// is a Ready: False condition and continued operation — the operator never stops observing
	// because of its own supply chain.
	SelfVerification func() (ok bool, message string)
}

// Beater is told when a reconcile completed (the watchdog).
type Beater interface{ Beat() }

// EstateReconciler reconciles one Estate: it observes, compares, writes status and reports.
type EstateReconciler struct {
	client.Client
	Observer *observe.Observer
	Reporter *report.Reporter
	Recorder record.EventRecorder
	Watchdog Beater
	Config   Config
	Now      func() time.Time

	mu sync.Mutex
}

// Reconcile observes the cluster for one Estate and writes what it saw into the Estate's status.
// An observation that fails is a condition, never an error: the reconciler returns an error
// only when the status cannot be written, so the work queue retries that and nothing else.
func (r *EstateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	log := logf.FromContext(ctx)

	var est estatev1alpha1.Estate
	if err := r.Get(ctx, req.NamespacedName, &est); err != nil {
		if apierrors.IsNotFound(err) {
			r.Reporter.Forget(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	now := r.now()
	interval := r.interval(&est)
	scope := observe.Scope{Platform: r.Config.Namespaces.Platform, Services: r.Config.Namespaces.Services}

	obs := r.Observer.Observe(ctx, scope)
	ownServices := r.Config.ReportOwnServices && boolOr(est.Spec.Operator.Report.OwnServices, true)
	observed := Observed(&est.Spec, obs, scope, ownServices)

	next := est.Status.DeepCopy()
	next.ObservedGeneration = est.Generation
	next.Cluster, next.Applied, next.Running = observed.Cluster, observed.Applied, observed.Running
	next.ExternalSecrets, next.Drift, next.Preflight, next.Health = observed.ExternalSecrets, observed.Drift, nil, observed.Health

	r.setReady(next, &est, obs, now)
	reason, message := summarize(observed.Drift)
	setCondition(next, ConditionDrifted, len(observed.Drift) > 0, reason, message, est.Generation, now)

	nextDue := now.Add(interval)
	if boolOr(est.Spec.Operator.Report.Enabled, true) {
		payload := report.Assemble(&est, &observed, r.Config.Operator)
		attempt, due := r.Reporter.Report(ctx, req.String(), payload, est.Spec.Environment, interval)
		if attempt.Attempted {
			res := attempt.Result
			next.LastReport = &estatev1alpha1.LastReport{At: metav1.NewTime(attempt.At), Outcome: res.Outcome, Status: clampInt32(res.Status)}
			setCondition(next, ConditionReported, res.Outcome == report.OutcomeAccepted, res.Reason(), conditionMessage(res, due), est.Generation, now)
			log.Info("report", "outcome", res.Outcome, "status", res.Status, "code", res.Code, "next", due.UTC().Format(time.RFC3339))
		}
		if !due.IsZero() {
			nextDue = due
		}
	} else {
		r.Reporter.Forget(req.String())
		setCondition(next, ConditionReported, false, ReasonDisabled, "spec.operator.report.enabled is false: the status stays in this cluster", est.Generation, now)
	}

	r.emitTransitions(&est, est.Status, *next)
	if !equality.Semantic.DeepEqual(est.Status, *next) {
		if err := r.writeStatus(ctx, req.NamespacedName, *next); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Watchdog != nil {
		r.Watchdog.Beat()
	}
	return ctrl.Result{RequeueAfter: max(nextDue.Sub(r.now()), Debounce)}, nil
}

func (r *EstateReconciler) setReady(st *estatev1alpha1.EstateStatus, est *estatev1alpha1.Estate, obs *observe.Observation, now time.Time) {
	var problems []string
	declared := []string{est.Spec.Namespaces.Platform, est.Spec.Namespaces.Services}
	watched := []string{r.Config.Namespaces.Platform, r.Config.Namespaces.Services}
	if declared[0] != watched[0] || declared[1] != watched[1] {
		problems = append(problems, fmt.Sprintf("spec.namespaces are %s/%s, the operator watches %s/%s (its flags --platform-namespace/--services-namespace and its Roles come from the same render)",
			declared[0], declared[1], watched[0], watched[1]))
	}
	incomplete, absent := r.Observer.Kinds.Gaps()
	problems = append(problems, incomplete...)
	problems = append(problems, obs.Gaps...)
	if len(problems) > 0 {
		setCondition(st, ConditionReady, false, ReasonCacheNotSynced, "not observed: "+strings.Join(problems, "; "), est.Generation, now)
		return
	}
	if r.Config.SelfVerification != nil {
		if ok, msg := r.Config.SelfVerification(); !ok {
			setCondition(st, ConditionReady, false, ReasonSelfVerificationFailed, msg, est.Generation, now)
			return
		}
	}
	msg := fmt.Sprintf("observed %d workloads", len(obs.Workloads))
	if obs.Flux != nil {
		msg += fmt.Sprintf(", %d Flux Kustomizations", len(obs.Flux.Kustomizations))
	}
	if len(absent) > 0 {
		msg += "; not served in this cluster: " + strings.Join(absent, ", ")
	}
	setCondition(st, ConditionReady, true, ReasonReconciled, msg, est.Generation, now)
}

// writeStatus writes the status subresource; a conflict is a retry on the fresh object.
func (r *EstateReconciler) writeStatus(ctx context.Context, key types.NamespacedName, st estatev1alpha1.EstateStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh estatev1alpha1.Estate
		if err := r.Get(ctx, key, &fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		fresh.Status = st
		fresh.Status.ObservedGeneration = fresh.Generation
		return r.Status().Update(ctx, &fresh)
	})
}

// emitTransitions records an Event on the Estate for each drift that appeared or resolved and
// for each change of the Reported condition's reason — `kubectl describe estate` tells the
// story. At most maxEventsPerReconcile per reconcile; the status carries the rest.
func (r *EstateReconciler) emitTransitions(est *estatev1alpha1.Estate, before, after estatev1alpha1.EstateStatus) {
	if r.Recorder == nil {
		return
	}
	const maxEventsPerReconcile = 10
	emitted := 0
	emit := func(eventType, reason, message string) {
		if emitted < maxEventsPerReconcile {
			r.Recorder.Event(est, eventType, reason, message)
			emitted++
		}
	}
	key := func(d estatev1alpha1.Drift) string { return string(d.Kind) + " " + d.Subject }
	old := map[string]bool{}
	for _, d := range before.Drift {
		old[key(d)] = true
	}
	current := map[string]bool{}
	for _, d := range after.Drift {
		current[key(d)] = true
		if !old[key(d)] {
			emit("Warning", "DriftDetected", fmt.Sprintf("%s %s (declared %s, observed %s)", d.Kind, d.Subject, deref(d.Declared), deref(d.Observed)))
		}
	}
	for _, d := range before.Drift {
		if !current[key(d)] {
			emit("Normal", "DriftResolved", fmt.Sprintf("%s %s", d.Kind, d.Subject))
		}
	}
	was := meta.FindStatusCondition(before.Conditions, ConditionReported)
	is := meta.FindStatusCondition(after.Conditions, ConditionReported)
	if is != nil && (was == nil || was.Reason != is.Reason) {
		eventType := "Normal"
		if is.Status != metav1.ConditionTrue {
			eventType = "Warning"
		}
		emit(eventType, "Report"+is.Reason, is.Message)
	}
}

func (r *EstateReconciler) interval(est *estatev1alpha1.Estate) time.Duration {
	if d, err := time.ParseDuration(est.Spec.Operator.Report.Interval); err == nil && d >= time.Minute && d <= 24*time.Hour {
		return d
	}
	return r.Config.Interval
}

func (r *EstateReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func conditionMessage(res report.Result, next time.Time) string {
	msg := res.Message
	if res.Outcome != report.OutcomeAccepted && !next.IsZero() {
		msg += "; next attempt at " + next.UTC().Format(time.RFC3339)
	}
	return msg
}

func setCondition(st *estatev1alpha1.EstateStatus, conditionType string, ok bool, reason, message string, generation int64, now time.Time) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now),
	})
}

func clampInt32(n int) int32 {
	return int32(max(min(n, math.MaxInt32), math.MinInt32)) //nolint:gosec // clamped on this line
}

func boolOr(b *bool, fallback bool) bool {
	if b == nil {
		return fallback
	}
	return *b
}

// SetupWithManager registers the controller: the Estate itself (a new render — reconciled at
// once), and every observed kind in the watched namespaces (a change — reconciled at most once
// per Debounce, by enqueueing every Estate after the debounce rather than at once).
func (r *EstateReconciler) SetupWithManager(mgr ctrl.Manager, kinds observe.Availability) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("estate").
		For(&estatev1alpha1.Estate{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1})

	debounced := handler.TypedFuncs[client.Object, reconcile.Request]{
		CreateFunc: func(ctx context.Context, _ event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAll(ctx, q)
		},
		UpdateFunc: func(ctx context.Context, _ event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAll(ctx, q)
		},
		DeleteFunc: func(ctx context.Context, _ event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueAll(ctx, q)
		},
	}
	for _, gvk := range observe.AllKinds() {
		if gvk == observe.EstateGVK || !kinds.Observed(gvk) {
			continue
		}
		b = b.WatchesRawSource(source.Kind[client.Object](mgr.GetCache(), objectFor(gvk), debounced, changed()))
	}
	return b.Complete(r)
}

func (r *EstateReconciler) enqueueAll(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	var list estatev1alpha1.EstateList
	if err := r.List(ctx, &list, client.InNamespace(r.Config.Namespaces.Own)); err != nil {
		return
	}
	for _, e := range list.Items {
		q.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name}}, Debounce)
	}
}

// changed passes an update only when the cached object changed beyond its resource version —
// the transforms keep only what the readers use, so any other difference matters.
func changed() predicate.TypedPredicate[client.Object] {
	return predicate.TypedFuncs[client.Object]{
		UpdateFunc: func(e event.TypedUpdateEvent[client.Object]) bool {
			a, b := e.ObjectOld.DeepCopyObject().(client.Object), e.ObjectNew.DeepCopyObject().(client.Object)
			a.SetResourceVersion("")
			b.SetResourceVersion("")
			return !equality.Semantic.DeepEqual(a, b)
		},
	}
}

// objectFor returns an empty object of a kind: typed for the built-in kinds the scheme knows,
// unstructured for Flux's and ESO's.
func objectFor(gvk schema.GroupVersionKind) client.Object {
	switch gvk {
	case observe.EstateGVK:
		return &estatev1alpha1.Estate{}
	case observe.NodeGVK, observe.PodGVK, observe.DeploymentGVK:
		return typedObject(gvk)
	default:
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		return u
	}
}
