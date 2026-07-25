package autoscaler

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName    = "autoscaler.talos.dev"
	Version      = "v1alpha1"
	ResourceName = "machinedeployments"
)

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	NewScheme     = func() *runtime.Scheme {
		s := runtime.NewScheme()
		_ = SchemeBuilder.AddToScheme(s)
		return s
	}
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemaGroupVersion,
		&MachineDeployment{},
		&MachineDeploymentList{},
		&MachineClass{},
		&MachineClassList{},
	)
	metav1.AddToGroupVersion(scheme, SchemaGroupVersion)
	return nil
}

var SchemaGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type MachineDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineDeploymentSpec   `json:"spec,omitempty"`
	Status MachineDeploymentStatus `json:"status,omitempty"`
}

type MachineDeploymentSpec struct {
	Replicas         int32  `json:"replicas"`
	MachineClassName string `json:"machineClassName"`
	ClusterName      string `json:"clusterName"`
}

type MachineDeploymentStatus struct {
	Phase         string `json:"phase,omitempty"`
	ReadyReplicas int32  `json:"readyReplicas,omitempty"`
	Message       string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true

type MachineDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineDeployment `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="VCPU",type=integer,JSONPath=`.spec.vcpu`
// +kubebuilder:printcolumn:name="Memory",type=integer,JSONPath=`.spec.memoryGiB`
// +kubebuilder:printcolumn:name="Disk",type=integer,JSONPath=`.spec.diskGiB`

type MachineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MachineClassSpec `json:"spec,omitempty"`
}

type MachineClassSpec struct {
	VCPU          int32  `json:"vcpu"`
	MemoryGiB     int32  `json:"memoryGiB"`
	DiskGiB       int32  `json:"diskGiB"`
	NetworkBridge string `json:"networkBridge,omitempty"`
	StoragePool   string `json:"storagePool,omitempty"`
	MACAddress    string `json:"macAddress,omitempty"`
	Serial        string `json:"serial,omitempty"`
}

// +kubebuilder:object:root=true

type MachineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineClass `json:"items"`
}

// Unused but required for interface compliance
var _ runtime.Object = &MachineDeployment{}
var _ runtime.Object = &MachineDeploymentList{}
var _ runtime.Object = &MachineClass{}
var _ runtime.Object = &MachineClassList{}

func (md *MachineDeployment) GetConditions() []corev1.NodeCondition {
	return nil
}

func (in *MachineDeployment) DeepCopy() *MachineDeployment {
	if in == nil {
		return nil
	}
	out := new(MachineDeployment)
	in.DeepCopyInto(out)
	return out
}

func (in *MachineDeployment) DeepCopyInto(out *MachineDeployment) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

func (in *MachineDeployment) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MachineDeploymentList) DeepCopy() *MachineDeploymentList {
	if in == nil {
		return nil
	}
	out := new(MachineDeploymentList)
	in.DeepCopyInto(out)
	return out
}

func (in *MachineDeploymentList) DeepCopyInto(out *MachineDeploymentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]MachineDeployment, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *MachineDeploymentList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MachineClass) DeepCopy() *MachineClass {
	if in == nil {
		return nil
	}
	out := new(MachineClass)
	in.DeepCopyInto(out)
	return out
}

func (in *MachineClass) DeepCopyInto(out *MachineClass) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *MachineClass) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MachineClassList) DeepCopy() *MachineClassList {
	if in == nil {
		return nil
	}
	out := new(MachineClassList)
	in.DeepCopyInto(out)
	return out
}

func (in *MachineClassList) DeepCopyInto(out *MachineClassList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]MachineClass, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *MachineClassList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
