package api

const Version = "kruise-agents-nfs-csi.zhida/v1alpha1"

type MountRequest struct {
	APIVersion    string `json:"api_version"`
	DriverName    string `json:"driver_name"`
	Namespace     string `json:"namespace"`
	PodName       string `json:"pod_name"`
	PodUID        string `json:"pod_uid"`
	PVName        string `json:"pv_name"`
	TargetPath    string `json:"target_path"`
	ContainerName string `json:"container_name,omitempty"`
}

type MountResult struct {
	Mounted       bool   `json:"mounted"`
	DriverName    string `json:"driver_name"`
	PVName        string `json:"pv_name"`
	TargetPath    string `json:"target_path"`
	ContainerName string `json:"container_name,omitempty"`
}

type Response struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
