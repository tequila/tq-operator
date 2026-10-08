// Package options is the operator's command line: the flags an estate's render sets.
package options

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Options are the operator's flags.
type Options struct {
	Interval          time.Duration
	IAMURL            string
	ConsoleURL        string
	IAMTokenFile      string
	SigningKeys       string
	AllowAnyHost      bool
	ReportOwnServices bool
	Namespace         string
	PlatformNamespace string
	ServicesNamespace string
	FluxNamespace     string
	Image             string
}

// Defaults of the paths the unit mounts.
const (
	DefaultIAMTokenFile = "/var/run/tequila/iam/token" //nolint:gosec // G101: a path, not a credential
	DefaultSigningKeys  = "/etc/tq-operator/signing-keys.json"
)

// Bind registers the flags. The operator's own namespace and image default to POD_NAMESPACE and
// TQ_OPERATOR_IMAGE (the downward API cannot expose an image; the render sets both).
func (o *Options) Bind(fs *flag.FlagSet) {
	fs.DurationVar(&o.Interval, "interval", 15*time.Minute, "the report heartbeat and status resync (spec.operator.report.interval overrides the heartbeat)")
	fs.StringVar(&o.IAMURL, "iam-url", "", "IAM's issuer URL — where the projected token is exchanged (https://id.tequila.dev)")
	fs.StringVar(&o.ConsoleURL, "console-url", "", "the console's URL — where reports are put (https://console.tequila.dev)")
	fs.StringVar(&o.IAMTokenFile, "iam-token-file", DefaultIAMTokenFile, "the projected ServiceAccount token (audience = --iam-url)")
	fs.StringVar(&o.SigningKeys, "signing-keys", DefaultSigningKeys, "the rendered public signing keys (JSON)")
	fs.BoolVar(&o.AllowAnyHost, "allow-any-host", false, "accept IAM and console URLs outside tequila.dev (a dev VM pointing at *.<slug>.test)")
	fs.BoolVar(&o.ReportOwnServices, "report-own-services", true, "report the tenant's own services (spec.operator.report.ownServices must allow it too)")
	fs.StringVar(&o.Namespace, "namespace", envOr("POD_NAMESPACE", "tq-operator"), "the operator's own namespace, where its Estates live")
	fs.StringVar(&o.PlatformNamespace, "platform-namespace", "operations", "the environment's platform namespace")
	fs.StringVar(&o.ServicesNamespace, "services-namespace", "tequila", "the environment's services namespace")
	fs.StringVar(&o.FluxNamespace, "flux-namespace", "flux-system", "the namespace Flux's Kustomizations, sources and FluxInstance live in")
	fs.StringVar(&o.Image, "image", os.Getenv("TQ_OPERATOR_IMAGE"), "the operator's own image reference, as rendered (reported)")
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks the flags and returns the two endpoints. The operator refuses to start with
// an IAM or console URL that is not https:// under tequila.dev unless --allow-any-host is set:
// the NetworkPolicy cannot name a host, so the operator holds its two hosts itself.
func (o *Options) Validate() (iam, console *url.URL, err error) {
	var errs []error
	if iam, err = o.endpoint("--iam-url", o.IAMURL); err != nil {
		errs = append(errs, err)
	}
	if console, err = o.endpoint("--console-url", o.ConsoleURL); err != nil {
		errs = append(errs, err)
	}
	if o.Interval < time.Minute || o.Interval > 24*time.Hour {
		errs = append(errs, fmt.Errorf("--interval %s is outside 1m…24h", o.Interval))
	}
	for flagName, ns := range map[string]string{
		"--namespace": o.Namespace, "--platform-namespace": o.PlatformNamespace,
		"--services-namespace": o.ServicesNamespace, "--flux-namespace": o.FluxNamespace,
	} {
		if len(ns) > 63 || !dnsLabel.MatchString(ns) {
			errs = append(errs, fmt.Errorf("%s %q is not a namespace name", flagName, ns))
		}
	}
	if o.IAMTokenFile == "" {
		errs = append(errs, errors.New("--iam-token-file is empty"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	return iam, console, nil
}

func (o *Options) endpoint(name, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", name, raw, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%s %q is not an https:// URL", name, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s %q must be a bare origin (no credentials, path, query or fragment)", name, raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%s %q has no host", name, raw)
	}
	if !o.AllowAnyHost && host != "tequila.dev" && !strings.HasSuffix(host, ".tequila.dev") {
		return nil, fmt.Errorf("%s %q is not under tequila.dev (set --allow-any-host for a dev estate)", name, raw)
	}
	u.Path = ""
	return u, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
