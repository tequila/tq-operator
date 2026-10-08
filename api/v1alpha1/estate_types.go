package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The status types below are shared with the estate report (internal/report): a report's
// cluster, applied, running, externalSecrets, drift, preflight and health blocks ARE these
// types, marshalled the same way, so `kubectl get estate <env> -o yaml` shows what was sent.
// The markers below therefore serve two generators: controller-gen (the CRD) and
// internal/schemagen (schemas/report.estate.v1.json). Fields are never `omitempty` inside a
// shared type: the report carries every key, and an absent value is an explicit null.

// Capability is a rung of the operator's ladder: observe, verify, gate, act — each rung adds
// one capability to the one below it.
// +kubebuilder:validation:Enum=observe;verify;gate;act
type Capability string

const (
	// CapabilityObserve is the floor: read-only, writes Estate.status and reports.
	CapabilityObserve Capability = "observe"
)

// EstateSpec is the environment block of tequila.yaml, resolved by the tenant's render.
// It is written by Flux alone; the operator never writes it.
type EstateSpec struct {
	// Product is the product this environment belongs to, as <owner>/<slug>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?/[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=201
	Product string `json:"product"`

	// Environment is the environment's name in tequila.yaml.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Environment string `json:"environment"`

	// Source names the repository the render came from. The commit Flux applied is observed in
	// the cluster (status.applied), never written into the render.
	Source EstateSource `json:"source"`

	// Renderer is the renderer pins the render was made with.
	// +optional
	Renderer EstateRenderer `json:"renderer,omitempty"`

	// Platform is every other platform.<pin> of tequila.yaml, resolved (the platform estate,
	// tofu, …).
	// +optional
	Platform map[string]string `json:"platform,omitempty"`

	// Profile is the environment's tenant profile, as rendered.
	// +optional
	Profile EstateProfile `json:"profile,omitempty"`

	// Namespaces are the environment's two namespaces: the platform estate's and the services'.
	Namespaces EstateNamespaces `json:"namespaces"`

	// Services is every service registered for this environment, by name.
	// +optional
	Services map[string]EstateService `json:"services,omitempty"`

	// Operator configures the operator for this environment.
	// +optional
	Operator OperatorSpec `json:"operator,omitempty"`
}

// EstateSource names where the render came from.
type EstateSource struct {
	// Repository is the gitops repository, as <org>/<name>.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`
	// +kubebuilder:validation:MaxLength=201
	Repository string `json:"repository"`
}

// EstateRenderer is the renderer's versions, as pinned in tequila.yaml's platform: block.
type EstateRenderer struct {
	// +optional
	Klass8sCli string `json:"klass8s-cli,omitempty"`
	// +optional
	ManifestsCore string `json:"manifests-core,omitempty"`
	// +optional
	TqCli string `json:"tq-cli,omitempty"`
}

// EstateProfile is the environment's tenant profile — which estate components run in the
// cluster and which are managed elsewhere — keys snake_case as the render writes them. The
// toggles are always rendered, false included; an undeclared backup is omitted.
type EstateProfile struct {
	// +kubebuilder:validation:Enum=aws;generic
	// +optional
	Cloud string `json:"cloud,omitempty"`
	// +optional
	MongodbInCluster bool `json:"mongodb_in_cluster"`
	// +optional
	NatsInCluster bool `json:"nats_in_cluster"`
	// +optional
	ValkeyInCluster bool `json:"valkey_in_cluster"`
	// +optional
	MonitoringInCluster bool `json:"monitoring_in_cluster"`
	// +optional
	KedaInCluster bool `json:"keda_in_cluster"`
	// +optional
	Backup *EstateBackup `json:"backup,omitempty"`
}

// EstateBackup is the environment's backup promise: who backs up its data.
type EstateBackup struct {
	// +kubebuilder:validation:Enum=atlas;estate;external;none
	// +optional
	Provider string `json:"provider,omitempty"`
}

// EstateNamespaces are the environment's platform and services namespaces.
type EstateNamespaces struct {
	// Platform is the namespace the estate renders into (default operations).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Platform string `json:"platform"`
	// Services is the namespace the services render into.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Services string `json:"services"`
}

// EstateService is one registered service. A Tequila-made service carries its repository,
// pinned version and own: false; a tenant's own, push-rendered service is {own: true} alone.
type EstateService struct {
	// +optional
	Repo string `json:"repo,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Own bool `json:"own"`
}

// OperatorSpec configures the operator for the environment.
type OperatorSpec struct {
	// Capabilities is the ladder's enabled rungs; observe is the floor, and an empty list is
	// an environment whose operator is off.
	// +listType=set
	// +kubebuilder:validation:MaxItems=4
	// +optional
	Capabilities []Capability `json:"capabilities"`
	// +optional
	Report ReportSpec `json:"report,omitempty"`
}

// ReportSpec configures the report to console.tequila.dev.
type ReportSpec struct {
	// Enabled sends the report; false keeps status in-cluster only.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// OwnServices reports the tenant's own services (name, kind, replicas, image refs).
	// +kubebuilder:default=true
	// +optional
	OwnServices *bool `json:"ownServices,omitempty"`
	// Interval is the heartbeat: an unchanged report is re-sent this often. A Go duration
	// string, kept as the render wrote it ("15m" — a Duration type would re-marshal "15m0s").
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:MaxLength=32
	// +optional
	Interval string `json:"interval,omitempty"`
}

// EstateStatus is the operator's observation: applied and running state compared with spec.
// It is written by the operator alone, through the status subresource.
//
// An Estate is a report, never a workload, and it must never gate the Flux Kustomization that
// applies it (wait: true): Flux's health check holds a custom resource while a top-level
// status.observedGeneration differs from metadata.generation or while a condition of type Ready
// is False. So the status has neither: each condition records the generation it was set for in
// its own observedGeneration, and the operator's own state is the condition Observed.
type EstateStatus struct {
	// Conditions are Observed, Reported and Drifted — never Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Cluster *ClusterStatus `json:"cluster,omitempty"`
	// +optional
	Applied *AppliedStatus `json:"applied,omitempty"`
	// +optional
	Running *RunningStatus `json:"running,omitempty"`
	// +optional
	ExternalSecrets *ExternalSecretsStatus `json:"externalSecrets,omitempty"`
	// +kubebuilder:validation:MaxItems=200
	// +optional
	Drift []Drift `json:"drift,omitempty"`
	// Preflight is rung verify's; always empty at rung observe.
	// +kubebuilder:validation:MaxItems=0
	// +optional
	Preflight []PreflightResult `json:"preflight,omitempty"`
	// +optional
	Health *HealthStatus `json:"health,omitempty"`
	// +optional
	LastReport *LastReport `json:"lastReport,omitempty"`
}

// ClusterStatus is what the cluster says of itself.
type ClusterStatus struct {
	// Kubernetes is the API server's version.
	Kubernetes string `json:"kubernetes"`
	// +kubebuilder:validation:Enum=eks;k3s;doks;generic
	Platform string      `json:"platform"`
	Nodes    NodesStatus `json:"nodes"`
}

// NodesStatus counts the nodes; it never names one.
type NodesStatus struct {
	Count int32 `json:"count"`
	// +kubebuilder:validation:MaxItems=8
	Architectures []string `json:"architectures"`
	// +kubebuilder:validation:MaxItems=32
	KubeletVersions []string `json:"kubeletVersions"`
}

// AppliedStatus is what Flux applied.
type AppliedStatus struct {
	Source AppliedSource `json:"source"`
	// +kubebuilder:validation:MaxItems=200
	Kustomizations []KustomizationStatus `json:"kustomizations"`
}

// AppliedSource is the Flux source the Kustomizations apply from.
type AppliedSource struct {
	// +kubebuilder:validation:Enum=GitRepository;OCIRepository
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Revision is the source's artifact revision; null before the first artifact.
	// +nullable
	Revision *string `json:"revision"`
}

// KustomizationStatus is one Flux Kustomization.
type KustomizationStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// +nullable
	AppliedRevision *string `json:"appliedRevision"`
	// Reason is the Ready condition's reason — Flux's own word, never its message.
	Reason string `json:"reason"`
	// LastReconcile is the Ready condition's last transition.
	// +nullable
	LastReconcile *metav1.Time `json:"lastReconcile"`
}

// RunningStatus is what the kubelet runs.
type RunningStatus struct {
	Estate RunningEstate `json:"estate"`
	// +kubebuilder:validation:MaxItems=500
	Workloads []Workload `json:"workloads"`
	// Truncated is true when the estate had more workloads than the cap.
	Truncated bool `json:"truncated"`
}

// RunningEstate is the estate's own version, as stamped on its workloads.
type RunningEstate struct {
	// +nullable
	OperationsK8s *string `json:"operationsK8s"`
}

// Workload is one Deployment of the two estate namespaces.
type Workload struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +kubebuilder:validation:Enum=Deployment
	Kind string `json:"kind"`
	// Service is the registered service the workload belongs to; null for the estate's own
	// workloads and for a tenant's own services.
	// +nullable
	Service  *string  `json:"service"`
	Replicas Replicas `json:"replicas"`
	// +kubebuilder:validation:MaxItems=16
	Images []Image `json:"images"`
}

// Replicas is a workload's desired and ready replica counts.
type Replicas struct {
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
}

// Image is one container image of a workload.
type Image struct {
	Ref string `json:"ref"`
	// Digest is what the kubelet pulled (containerStatuses[].imageID); null until a Pod runs it.
	// +nullable
	Digest *string `json:"digest"`
	// Signature is unverified at rung observe.
	// +kubebuilder:validation:Enum=unverified;valid;invalid;unsigned
	Signature string `json:"signature"`
}

// ExternalSecretsStatus counts the ExternalSecrets of the two estate namespaces.
type ExternalSecretsStatus struct {
	Total int32 `json:"total"`
	Ready int32 `json:"ready"`
	// +kubebuilder:validation:MaxItems=100
	NotReady []NotReadyExternalSecret `json:"notReady"`
}

// NotReadyExternalSecret is an ExternalSecret whose Ready condition is not True.
type NotReadyExternalSecret struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// DriftKind is the class of a difference between declared and observed state.
// +kubebuilder:validation:Enum=RunningBehind;AppliedBehind;NotReady;SecretNotResolvable;Undeclared;NoEstate
type DriftKind string

const (
	// DriftRunningBehind: a service's declared version is not the tag of its running images.
	DriftRunningBehind DriftKind = "RunningBehind"
	// DriftAppliedBehind: a Kustomization's applied revision is not its source's artifact.
	DriftAppliedBehind DriftKind = "AppliedBehind"
	// DriftNotReady: a Kustomization, the FluxInstance or a workload is not ready.
	DriftNotReady DriftKind = "NotReady"
	// DriftSecretNotResolvable: an ExternalSecret is not ready.
	DriftSecretNotResolvable DriftKind = "SecretNotResolvable"
	// DriftUndeclared: a platform/* workload runs that spec does not list (not yet reported).
	DriftUndeclared DriftKind = "Undeclared"
	// DriftNoEstate: the operator runs and no Estate is declared.
	DriftNoEstate DriftKind = "NoEstate"
)

// Drift is one difference between declared and observed state.
type Drift struct {
	Kind DriftKind `json:"kind"`
	// Subject is <namespace>/<workload>, a Kustomization's name or an object's <namespace>/<name>.
	Subject string `json:"subject"`
	// +nullable
	Declared *string `json:"declared"`
	// +nullable
	Observed *string `json:"observed"`
}

// PreflightResult is rung verify's answer to "would this estate accept that release?".
// No entry exists at rung observe.
type PreflightResult struct {
	Release string `json:"release"`
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// HealthStatus is the Pods' health in the two estate namespaces.
type HealthStatus struct {
	Pods PodHealth `json:"pods"`
}

// PodHealth counts Pods by phase; crash-looping workloads are named <namespace>/<workload>.
type PodHealth struct {
	Running int32 `json:"running"`
	Pending int32 `json:"pending"`
	Failed  int32 `json:"failed"`
	// +kubebuilder:validation:MaxItems=100
	CrashLooping []string `json:"crashLooping"`
}

// ReportOutcome is how the last report attempt ended.
// +kubebuilder:validation:Enum=accepted;unreachable;unbound;refused;invalid
type ReportOutcome string

// LastReport is the last report attempt.
type LastReport struct {
	At      metav1.Time   `json:"at"`
	Outcome ReportOutcome `json:"outcome"`
	// Status is the HTTP status of the answer; absent when nothing answered.
	// +optional
	Status int32 `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=tequila
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Observed",type=string,JSONPath=`.status.conditions[?(@.type=="Observed")].status`
// +kubebuilder:printcolumn:name="Reported",type=string,JSONPath=`.status.conditions[?(@.type=="Reported")].status`
// +kubebuilder:printcolumn:name="Drifted",type=string,JSONPath=`.status.conditions[?(@.type=="Drifted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Estate is one environment's declared state (spec, from the tenant's render) and the
// operator's observation of it (status, the report the operator sends).
type Estate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EstateSpec `json:"spec"`
	// +optional
	Status EstateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EstateList is a list of Estates.
type EstateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Estate `json:"items"`
}
