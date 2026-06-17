package kube

type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []OwnerReference  `json:"ownerReferences"`
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type ObjectReference struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type TokenReviewRequest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Spec       TokenReviewSpec `json:"spec"`
}

type TokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type TokenReviewResponse struct {
	Status TokenReviewStatus `json:"status"`
}

type TokenReviewStatus struct {
	Authenticated bool              `json:"authenticated"`
	Audiences     []string          `json:"audiences"`
	User          TokenReviewUser   `json:"user"`
	Error         string            `json:"error"`
	Extra         map[string]string `json:"extra"`
}

type TokenReviewUser struct {
	Username string   `json:"username"`
	UID      string   `json:"uid"`
	Groups   []string `json:"groups"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}

type PodSpec struct {
	ServiceAccountName string      `json:"serviceAccountName"`
	Volumes            []Volume    `json:"volumes"`
	Containers         []Container `json:"containers"`
}

type Container struct {
	Name string   `json:"name"`
	Env  []EnvVar `json:"env,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type Volume struct {
	Name                  string                 `json:"name"`
	PersistentVolumeClaim *PersistentVolumeClaim `json:"persistentVolumeClaim,omitempty"`
}

type PersistentVolumeClaim struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type PodStatus struct {
	Phase             string            `json:"phase"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses"`
}

type ContainerStatus struct {
	Name        string `json:"name"`
	ContainerID string `json:"containerID"`
	Ready       bool   `json:"ready"`
}

type PersistentVolume struct {
	Metadata ObjectMeta           `json:"metadata"`
	Spec     PersistentVolumeSpec `json:"spec"`
}

type PersistentVolumeClaimResource struct {
	Metadata ObjectMeta `json:"metadata"`
}

type PersistentVolumeSpec struct {
	ClaimRef     *ObjectReference           `json:"claimRef,omitempty"`
	CSI          *CSIPersistentVolumeSource `json:"csi,omitempty"`
	MountOptions []string                   `json:"mountOptions,omitempty"`
}

type CSIPersistentVolumeSource struct {
	Driver           string            `json:"driver"`
	VolumeHandle     string            `json:"volumeHandle"`
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
}
