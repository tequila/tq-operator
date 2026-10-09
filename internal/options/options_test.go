package options

import (
	"flag"
	"strings"
	"testing"
)

func parse(t *testing.T, args ...string) *Options {
	t.Helper()
	var o Options
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o.Bind(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return &o
}

func TestTheRenderedFlagsValidate(t *testing.T) {
	o := parse(t, "--interval=15m", "--iam-url=https://id.tequila.dev", "--console-url=https://console.tequila.dev",
		"--iam-token-file=/var/run/tequila/iam/token", "--signing-keys=/etc/tq-operator/signing-keys.json")
	iam, console, err := o.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if iam.String() != "https://id.tequila.dev" || console.String() != "https://console.tequila.dev" {
		t.Errorf("%s %s", iam, console)
	}
	if o.Namespace != "tq-operator" || o.PlatformNamespace != "operations" || o.ServicesNamespace != "tequila" || o.FluxNamespace != "flux-system" || !o.ReportOwnServices {
		t.Errorf("defaults: %+v", o)
	}
}

func TestOnlyTequilaHostsUnlessAllowed(t *testing.T) {
	cases := map[string]string{
		"http://id.tequila.dev":               "not an https",
		"https://id.tequila.dev.evil.example": "not under tequila.dev",
		"https://eviltequila.dev":             "not under tequila.dev",
		"https://user:pw@id.tequila.dev":      "bare origin",
		"https://id.tequila.dev/oauth":        "bare origin",
		"https://id.tequila.dev?x=1":          "bare origin",
		"":                                    "required",
	}
	for raw, want := range cases {
		o := parse(t, "--iam-url="+raw, "--console-url=https://console.tequila.dev")
		if _, _, err := o.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", raw, err, want)
		}
	}
	o := parse(t, "--iam-url=https://id.acme.test", "--console-url=https://console.acme.test", "--allow-any-host")
	if _, _, err := o.Validate(); err != nil {
		t.Errorf("--allow-any-host: %v", err)
	}
	o = parse(t, "--iam-url=http://id.acme.test", "--console-url=https://console.acme.test", "--allow-any-host")
	if _, _, err := o.Validate(); err == nil {
		t.Error("--allow-any-host must still require https")
	}
}

func TestBoundsAndNames(t *testing.T) {
	o := parse(t, "--iam-url=https://id.tequila.dev", "--console-url=https://console.tequila.dev", "--interval=10s", "--flux-namespace=Flux_System")
	_, _, err := o.Validate()
	if err == nil || !strings.Contains(err.Error(), "--interval") || !strings.Contains(err.Error(), "--flux-namespace") {
		t.Errorf("got %v", err)
	}
}
