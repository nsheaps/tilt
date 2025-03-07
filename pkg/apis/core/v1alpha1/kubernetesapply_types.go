/*
Copyright 2020 The Tilt Dev Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
)

const KubernetesApplyTimeoutDefault = 30 * time.Second

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// KubernetesApply specifies a blob of YAML to apply, and a set of ImageMaps
// that the YAML depends on.
//
// The KubernetesApply controller will resolve the ImageMaps into immutable image
// references. The controller will process the spec YAML, then apply it to the cluster.
// Those processing steps might include:
//
// - Injecting the resolved image references.
// - Adding custom labels so that Tilt can track the progress of the apply.
// - Modifying image pull rules to ensure the image is pulled correctly.
//
// The controller won't apply anything until all ImageMaps resolve to real images.
//
// The controller will watch all the image maps, and redeploy the entire YAML if
// any of the maps resolve to a new image.
//
// The status field will contain both the raw applied object, and derived fields
// to help other controllers figure out how to watch the apply progress.
//
// +k8s:openapi-gen=true
// +tilt:starlark-gen=true
type KubernetesApply struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Spec   KubernetesApplySpec   `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`
	Status KubernetesApplyStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// KubernetesApplyList
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type KubernetesApplyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []KubernetesApply `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// KubernetesApplySpec defines the desired state of KubernetesApply
type KubernetesApplySpec struct {
	// YAML to apply to the cluster.
	//
	// Exactly one of YAML OR ApplyCmd MUST be provided.
	//
	// +optional
	YAML string `json:"yaml,omitempty" protobuf:"bytes,1,opt,name=yaml"`

	// Names of image maps that this applier depends on.
	//
	// The controller will watch all the image maps, and redeploy the entire YAML
	// if any of the maps resolve to a new image.
	//
	// +optional
	ImageMaps []string `json:"imageMaps,omitempty" protobuf:"bytes,2,rep,name=imageMaps"`

	// Descriptors of how to find images in the YAML.
	//
	// Needed when injecting images into CRDs.
	//
	// +optional
	ImageLocators []KubernetesImageLocator `json:"imageLocators,omitempty" protobuf:"bytes,3,rep,name=imageLocators"`

	// The timeout on the apply operation.
	//
	// We've had problems with both:
	// 1) CRD apiservers that take an arbitrarily long time to apply, and
	// 2) Infinite loops in the apimachinery
	// So we offer the ability to set a timeout on Kubernetes apply operations.
	//
	// The default timeout is 30s.
	//
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty" protobuf:"bytes,4,opt,name=timeout"`

	// KubernetesDiscoveryTemplateSpec describes how we discover pods
	// for resources created by this Apply.
	//
	// If not specified, the KubernetesDiscovery controller will listen to all pods,
	// and follow owner references to find the pods owned by these resources.
	//
	// +optional
	KubernetesDiscoveryTemplateSpec *KubernetesDiscoveryTemplateSpec `json:"kubernetesDiscoveryTemplateSpec,omitempty" protobuf:"bytes,5,opt,name=kubernetesDiscoveryTemplateSpec"`

	// PortForwardTemplateSpec describes the data model for port forwards
	// that KubernetesApply should set up.
	//
	// Underneath the hood, we'll create a KubernetesDiscovery object that finds
	// the pods and sets up the port-forwarding. Only one PortForward will be
	// active at a time.
	//
	// +optional
	PortForwardTemplateSpec *PortForwardTemplateSpec `json:"portForwardTemplateSpec,omitempty" protobuf:"bytes,6,opt,name=portForwardTemplateSpec"`

	// PodLogStreamTemplateSpec describes the data model for PodLogStreams
	// that KubernetesApply should set up.
	//
	// Underneath the hood, we'll create a KubernetesDiscovery object that finds
	// the pods and sets up the pod log streams.
	//
	// If no template is specified, the controller will stream all
	// pod logs available from the apiserver.
	//
	// +optional
	PodLogStreamTemplateSpec *PodLogStreamTemplateSpec `json:"podLogStreamTemplateSpec,omitempty" protobuf:"bytes,7,opt,name=podLogStreamTemplateSpec"`

	// DiscoveryStrategy describes how we set up pod watches for the applied
	// resources. This affects all systems that attach to pods, including
	// PortForwards, PodLogStreams, resource readiness, and live-updates.
	//
	// +optional
	DiscoveryStrategy KubernetesDiscoveryStrategy `json:"discoveryStrategy,omitempty" protobuf:"bytes,8,opt,name=discoveryStrategy,casttype=KubernetesDiscoveryStrategy"`

	// Specifies how to disable this.
	//
	// +optional
	DisableSource *DisableSource `json:"disableSource,omitempty" protobuf:"bytes,9,opt,name=disableSource"`

	// ApplyCmd is a custom command to execute to deploy entities to the Kubernetes cluster.
	//
	// The command must be idempotent, e.g. it must not fail if some or all entities already exist.
	//
	// The ApplyCmd MUST return valid Kubernetes YAML for the entities it applied to the cluster.
	//
	// Exactly one of YAML OR ApplyCmd MUST be provided.
	//
	// +optional
	ApplyCmd *KubernetesApplyCmd `json:"applyCmd,omitempty" protobuf:"bytes,10,opt,name=applyCmd"`

	// RestartOn determines external triggers that will result in an apply.
	//
	// +optional
	RestartOn *RestartOnSpec `json:"restartOn,omitempty" protobuf:"bytes,11,opt,name=restartOn"`

	// DeleteCmd is a custom command to execute to delete entities created by ApplyCmd and clean up any
	// additional state.
	//
	// +optional
	DeleteCmd *KubernetesApplyCmd `json:"deleteCmd,omitempty" protobuf:"bytes,12,opt,name=deleteCmd"`

	// Cluster name to determine the Kubernetes cluster.
	//
	// If not provided, "default" will be used.
	//
	// +optional
	Cluster string `json:"cluster" protobuf:"bytes,13,opt,name=cluster"`
}

var _ resource.Object = &KubernetesApply{}
var _ resourcestrategy.Defaulter = &KubernetesApply{}
var _ resourcestrategy.Validater = &KubernetesApply{}
var _ resourcerest.ShortNamesProvider = &KubernetesApply{}

func (in *KubernetesApply) Default() {
	if in.Spec.Cluster == "" {
		in.Spec.Cluster = ClusterNameDefault
	}
}

func (in *KubernetesApply) GetSpec() interface{} {
	return in.Spec
}

func (in *KubernetesApply) GetObjectMeta() *metav1.ObjectMeta {
	return &in.ObjectMeta
}

func (in *KubernetesApply) NamespaceScoped() bool {
	return false
}

func (in *KubernetesApply) ShortNames() []string {
	return []string{"ka", "kapp"}
}

func (in *KubernetesApply) New() runtime.Object {
	return &KubernetesApply{}
}

func (in *KubernetesApply) NewList() runtime.Object {
	return &KubernetesApplyList{}
}

func (in *KubernetesApply) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "tilt.dev",
		Version:  "v1alpha1",
		Resource: "kubernetesapplys",
	}
}

func (in *KubernetesApply) IsStorageVersion() bool {
	return true
}

func (in *KubernetesApply) Validate(ctx context.Context) field.ErrorList {
	var fieldErrors field.ErrorList

	kdStrategy := in.Spec.DiscoveryStrategy
	if !(kdStrategy == "" ||
		kdStrategy == KubernetesDiscoveryStrategyDefault ||
		kdStrategy == KubernetesDiscoveryStrategySelectorsOnly) {
		fieldErrors = append(fieldErrors, field.NotSupported(
			field.NewPath("spec.discoveryStrategy"),
			kdStrategy,
			[]string{
				string(KubernetesDiscoveryStrategyDefault),
				string(KubernetesDiscoveryStrategySelectorsOnly),
			}))
	}

	if in.Spec.YAML != "" {
		if in.Spec.ApplyCmd != nil {
			fieldErrors = append(fieldErrors, field.Invalid(
				field.NewPath("spec.applyCmd"),
				in.Spec.ApplyCmd,
				"must specify exactly ONE of .spec.yaml or .spec.applyCmd"))
		}
	} else if in.Spec.ApplyCmd != nil {
		fieldErrors = append(fieldErrors, in.Spec.ApplyCmd.validateAsSubfield(ctx, field.NewPath("spec.applyCmd"))...)
	} else {
		fieldErrors = append(fieldErrors, field.Required(
			field.NewPath("spec.yaml"),
			"must specify exactly ONE of .spec.yaml or .spec.applyCmd"))
	}

	return fieldErrors
}

var _ resource.ObjectList = &KubernetesApplyList{}

func (in *KubernetesApplyList) GetListMeta() *metav1.ListMeta {
	return &in.ListMeta
}

// KubernetesApplyStatus defines the observed state of KubernetesApply
type KubernetesApplyStatus struct {
	// The result of applying the YAML to the cluster. This should contain
	// UIDs for the applied resources.
	//
	// +optional
	ResultYAML string `json:"resultYAML,omitempty" protobuf:"bytes,1,opt,name=resultYAML"`

	// An error applying the YAML.
	//
	// If there was an error, than ResultYAML should be empty (and vice versa).
	//
	// +optional
	Error string `json:"error,omitempty" protobuf:"bytes,2,opt,name=error"`

	// Timestamp of we last finished applying this YAML to the cluster.
	//
	// When populated, must be equal or after the LastApplyStartTime field.
	//
	// TODO(nick): In v1, we may rename this to LastApplyFinishTime, which
	// is more consistent with how we name this in other API objects.
	//
	// +optional
	LastApplyTime metav1.MicroTime `json:"lastApplyTime,omitempty" protobuf:"bytes,3,opt,name=lastApplyTime"`

	// Timestamp of when we last started applying this YAML to the cluster.
	//
	// +optional
	LastApplyStartTime metav1.MicroTime `json:"lastApplyStartTime,omitempty" protobuf:"bytes,6,opt,name=lastApplyStartTime"`

	// A base64-encoded hash of all the inputs to the apply.
	//
	// We added this so that more procedural code can determine whether
	// their updates have been applied yet or not by the reconciler. But any code
	// using it this way should note that the reconciler may "skip" an update
	// (e.g., if two images get updated in quick succession before the reconciler
	// injects them into the YAML), so a particular ApplieInputHash might never appear.
	//
	// +optional
	AppliedInputHash string `json:"appliedInputHash,omitempty" protobuf:"bytes,4,opt,name=appliedInputHash"`

	// Details about whether/why this is disabled.
	// +optional
	DisableStatus *DisableStatus `json:"disableStatus,omitempty" protobuf:"bytes,5,opt,name=disableStatus"`

	// Conditions based on the result of the apply.
	//
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" protobuf:"bytes,7,rep,name=conditions"`

	// TODO(nick): We should also add some sort of status field to this
	// status (like waiting, active, done).
}

const (
	// ApplyConditionJobComplete means the apply was for a batch/v1.Job that has already
	// run to successful completion.
	//
	// Tilt primarily monitors Pods for resource runtime status, but no Pod
	// will be created for the Job in this scenario since it already ran in the
	// past, and it's possible that the Pod has been GC'd (e.g. due to cluster
	// settings or due to a Node being recycled). This condition allows Tilt to
	// bypass Pod monitoring for this resource.
	ApplyConditionJobComplete string = "JobComplete"
)

// KubernetesApply implements ObjectWithStatusSubResource interface.
var _ resource.ObjectWithStatusSubResource = &KubernetesApply{}

func (in *KubernetesApply) GetStatus() resource.StatusSubResource {
	return in.Status
}

// KubernetesApplyStatus{} implements StatusSubResource interface.
var _ resource.StatusSubResource = &KubernetesApplyStatus{}

func (in KubernetesApplyStatus) CopyTo(parent resource.ObjectWithStatusSubResource) {
	parent.(*KubernetesApply).Status = in
}

// Finds image references in Kubernetes YAML.
type KubernetesImageLocator struct {
	// Selects which objects to look in.
	ObjectSelector ObjectSelector `json:"objectSelector" protobuf:"bytes,1,opt,name=objectSelector"`

	// A JSON path to the image reference field.
	//
	// If Object is empty, the field should be a string.
	//
	// If Object is non-empty, the field should be an object with subfields.
	Path string `json:"path" protobuf:"bytes,2,opt,name=path"`

	// A descriptor of the path and structure of an object that describes an image
	// reference. This is a common way to describe images in CRDs, breaking
	// them down into an object rather than an image reference string.
	//
	// +optional
	Object *KubernetesImageObjectDescriptor `json:"object,omitempty" protobuf:"bytes,3,opt,name=object"`
}

type KubernetesImageObjectDescriptor struct {
	// The name of the field that contains the image repository.
	RepoField string `json:"repoField" protobuf:"bytes,1,opt,name=repoField"`

	// The name of the field that contains the image tag.
	TagField string `json:"tagField" protobuf:"bytes,2,opt,name=tagField"`
}

type KubernetesDiscoveryTemplateSpec struct {
	// ExtraSelectors are label selectors that will force discovery of a Pod even
	// if it does not match the AncestorUID.
	//
	// This should only be necessary in the event that a CRD creates Pods but does
	// not set an owner reference to itself.
	ExtraSelectors []metav1.LabelSelector `json:"extraSelectors,omitempty" protobuf:"bytes,1,rep,name=extraSelectors"`
}

type KubernetesDiscoveryStrategy string

var (
	// In the default strategy, we traverse owner references of every pod,
	// and follow pods that belong to an applied resource. If extra selectors
	// are specified, we use them too.
	KubernetesDiscoveryStrategyDefault KubernetesDiscoveryStrategy = "default"

	// In the selectors-only strategy, we only traverse label selectors, and ignore
	// owner references.
	//
	// For example, you might have a CRD that does some of its work in pods, then
	// creates a new Deployment. In that case, the child pods of the CRD aren't
	// the ones we want to track for readiness or live-update. You want the ones
	// from the deployment.
	KubernetesDiscoveryStrategySelectorsOnly KubernetesDiscoveryStrategy = "selectors-only"
)

type KubernetesApplyCmd struct {
	// Args are the command-line arguments for the apply command. Must have length >= 1.
	Args []string `json:"args" protobuf:"bytes,1,rep,name=args"`

	// Process working directory.
	//
	// If not specified, will default to Tilt working directory.
	//
	// +optional
	// +tilt:local-path=true
	Dir string `json:"dir" protobuf:"bytes,2,opt,name=dir"`

	// Env are additional variables for the process environment.
	//
	// Environment variables are layered on top of the environment variables
	// that Tilt runs with.
	//
	// +optional
	Env []string `json:"env" protobuf:"bytes,3,rep,name=env"`
}

func (c *KubernetesApplyCmd) Validate(ctx context.Context) field.ErrorList {
	return c.validateAsSubfield(ctx, nil)
}

// validateAsSubfield performs validation prepending the rootField (if non-nil) to paths in returned errors.
func (c *KubernetesApplyCmd) validateAsSubfield(_ context.Context, rootField *field.Path) field.ErrorList {
	var fieldErrors field.ErrorList
	if len(c.Args) == 0 {
		fieldErrors = append(fieldErrors, field.Required(rootField.Child("args"), "args cannot be empty"))
	}
	return fieldErrors
}
