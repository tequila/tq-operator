package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/report"
)

// NoEstateLoop reports when the operator runs and no Estate is declared — a render predating
// the operator, or `enabled: false` after one — with the cluster facts only, declared: null
// and one NoEstate drift. Without an Estate there is no status to write, so the loop's
// tick is also what tells the watchdog the operator is alive.
type NoEstateLoop struct {
	Reader   client.Reader
	Observer *observe.Observer
	Reporter *report.Reporter
	Watchdog Beater
	Config   Config
	// EstatesServed is false when the Estate CRD is not installed: every tick is a no-Estate tick.
	EstatesServed bool
	// Tick is how often the loop looks (default one minute); the reporter decides what is due.
	Tick time.Duration
	// Reprobe, when set, is called every ProbeEvery; it stops the operator for a restart
	// when a kind it could not read became readable (a CRD installed after start).
	Reprobe    func(ctx context.Context)
	ProbeEvery time.Duration
}

// NeedLeaderElection: the operator runs one replica and never elects.
func (l *NoEstateLoop) NeedLeaderElection() bool { return false }

// Start runs the loop until the context ends.
func (l *NoEstateLoop) Start(ctx context.Context) error {
	tick := l.Tick
	if tick == 0 {
		tick = time.Minute
	}
	probeEvery := l.ProbeEvery
	if probeEvery == 0 {
		probeEvery = 15 * time.Minute
	}
	lastProbe := time.Now()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		l.once(ctx)
		if l.Reprobe != nil && time.Since(lastProbe) >= probeEvery {
			l.Reprobe(ctx)
			lastProbe = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (l *NoEstateLoop) once(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("no-estate")
	if l.EstatesServed {
		var list estatev1alpha1.EstateList
		if err := l.Reader.List(ctx, &list, client.InNamespace(l.Config.Namespaces.Own)); err != nil {
			log.Info("listing Estates failed", "error", err.Error())
			return
		}
		if len(list.Items) > 0 {
			l.Reporter.Forget(report.NoEstateKey)
			return // the Estate reconciler reports, and beats the watchdog
		}
	}
	if l.Watchdog != nil {
		l.Watchdog.Beat()
	}
	if due := l.Reporter.NextDue(report.NoEstateKey, l.Config.Interval); !due.IsZero() && time.Now().Before(due) {
		return
	}
	scope := observe.Scope{Platform: l.Config.Namespaces.Platform, Services: l.Config.Namespaces.Services}
	obs := l.Observer.Observe(ctx, scope)
	st := Observed(nil, obs, scope, l.Config.ReportOwnServices)
	attempt, next := l.Reporter.Report(ctx, report.NoEstateKey, report.Assemble(nil, &st, l.Config.Operator), "", l.Config.Interval)
	if attempt.Attempted {
		log.Info("report without an Estate", "outcome", attempt.Result.Outcome, "status", attempt.Result.Status,
			"code", attempt.Result.Code, "message", attempt.Result.Message, "next", next.UTC().Format(time.RFC3339))
	}
}
