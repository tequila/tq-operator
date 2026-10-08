// Package report is the estate report `tequila.dev/report/estate/v1` and the client that sends
// it: the IAM exchange with the projected ServiceAccount token, the PUT to the console's report
// door, the bounds and the backoff.
//
// The types in this file ARE the schema: schemas/report.estate.v1.json is generated from them
// (internal/schemagen), every key is required, every object closed (additionalProperties:
// false), every string at most 512 characters. A field that is not declared here cannot reach
// the wire — the allow-list is the type.
package report

import (
	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// SchemaID is the report's schema identifier, carried in every report.
const SchemaID = "tequila.dev/report/estate/v1"

// MaxStringLength caps every string the report carries.
const MaxStringLength = 512

// MaxBodyBytes is the console's limit for a report body (413 above it).
const MaxBodyBytes = 256 * 1024

// Report is one estate report. Its cluster, applied, running, externalSecrets, drift,
// preflight and health blocks are the Estate's status blocks, byte for byte.
type Report struct {
	// +schema:const=tequila.dev/report/estate/v1
	Schema string `json:"schema"`
	// Estate is who reports: the identity's account and environment, and the declared product.
	Estate EstateRef `json:"estate"`
	// ReportedAt is when the report was assembled (RFC 3339, UTC).
	// +kubebuilder:validation:Format=date-time
	ReportedAt string `json:"reportedAt"`
	// Operator is the reporting operator.
	Operator Operator `json:"operator"`
	// Declared echoes Estate.spec; null when no Estate is declared.
	// +nullable
	Declared *Declared `json:"declared"`
	// Cluster is what the cluster says of itself.
	Cluster estatev1alpha1.ClusterStatus `json:"cluster"`
	// Applied is what Flux applied; null where no Flux source is observed.
	// +nullable
	Applied *estatev1alpha1.AppliedStatus `json:"applied"`
	// Running is what the kubelet runs.
	Running estatev1alpha1.RunningStatus `json:"running"`
	// ExternalSecrets counts the estate namespaces' ExternalSecrets.
	ExternalSecrets estatev1alpha1.ExternalSecretsStatus `json:"externalSecrets"`
	// Drift is every difference between declared and observed state.
	// +kubebuilder:validation:MaxItems=200
	Drift []estatev1alpha1.Drift `json:"drift"`
	// Preflight is rung verify's; always empty at rung observe.
	// +kubebuilder:validation:MaxItems=0
	Preflight []estatev1alpha1.PreflightResult `json:"preflight"`
	// Health is the Pods' health in the estate namespaces.
	Health estatev1alpha1.HealthStatus `json:"health"`
}

// EstateRef names the reporting estate.
type EstateRef struct {
	// Account is the organisation's slug, from the estate identity (sub estate:<account>/<env>).
	Account string `json:"account"`
	// Environment is the environment, from the estate identity.
	Environment string `json:"environment"`
	// Product is Estate.spec.product as <owner>/<slug>; null when no Estate is declared.
	// +nullable
	Product *string `json:"product"`
}

// Operator describes the reporting operator.
type Operator struct {
	// Version is the operator's release.
	Version string `json:"version"`
	// Image is the operator's image reference, as the render set it.
	Image string `json:"image"`
	// Capabilities is the rungs this operator runs.
	// +kubebuilder:validation:MaxItems=4
	Capabilities []estatev1alpha1.Capability `json:"capabilities"`
}

// Declared echoes the Estate's spec — the declared state the observation was compared with.
type Declared struct {
	// Generation is the Estate's metadata.generation.
	Generation int64 `json:"generation"`
	// Source names the render's repository.
	Source DeclaredSource `json:"source"`
	// Renderer is the renderer pins; null where tequila.yaml pins none.
	Renderer DeclaredRenderer `json:"renderer"`
	// Platform is every other platform pin.
	Platform map[string]string `json:"platform"`
	// Services is every registered service of the environment.
	Services map[string]DeclaredService `json:"services"`
}

// DeclaredSource is spec.source.
type DeclaredSource struct {
	Repository string `json:"repository"`
}

// DeclaredRenderer is spec.renderer.
type DeclaredRenderer struct {
	// +nullable
	Klass8sCli *string `json:"klass8s-cli"`
	// +nullable
	ManifestsCore *string `json:"manifests-core"`
	// +nullable
	TqCli *string `json:"tq-cli"`
}

// DeclaredService is one entry of spec.services.
type DeclaredService struct {
	// +nullable
	Repo *string `json:"repo"`
	// +nullable
	Version *string `json:"version"`
	Own     bool    `json:"own"`
}
