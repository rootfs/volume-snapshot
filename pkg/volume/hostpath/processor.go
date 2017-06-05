/*
Copyright 2017 The Kubernetes Authors.

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

package hostpath

import (
	"fmt"
	"os"
	"os/exec"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/pkg/api/v1"

	tprv1 "github.com/rootfs/snapshot/pkg/apis/tpr/v1"
	"github.com/rootfs/snapshot/pkg/cloudprovider"
	"github.com/rootfs/snapshot/pkg/volume"
)

const (
	depot        = "/tmp/"
	restorePoint = "/restore/"
)

type hostPathPlugin struct {
}

var _ volume.VolumePlugin = &hostPathPlugin{}

func RegisterPlugin() volume.VolumePlugin {
	return &hostPathPlugin{}
}

func GetPluginName() string {
	return "hostPath"
}

func (h *hostPathPlugin) Init(_ cloudprovider.Interface) {
}

func (h *hostPathPlugin) SnapshotCreate(spec *v1.PersistentVolumeSpec) (*tprv1.VolumeSnapshotDataSource, error) {
	if spec == nil || spec.HostPath == nil {
		return nil, fmt.Errorf("invalid PV spec %v", spec)
	}
	path := spec.HostPath.Path
	file := depot + string(uuid.NewUUID()) + ".tgz"
	cmd := exec.Command("tar", "czvf", file, path)
	res := &tprv1.VolumeSnapshotDataSource{
		HostPath: &tprv1.HostPathVolumeSnapshotSource{
			Path: file,
		},
	}
	return res, cmd.Run()
}

func (h *hostPathPlugin) SnapshotDelete(src *tprv1.VolumeSnapshotDataSource, _ *v1.PersistentVolume) error {
	if src == nil || src.HostPath == nil {
		return fmt.Errorf("invalid VolumeSnapshotDataSource: %v", src)
	}
	path := src.HostPath.Path
	return os.Remove(path)
}

func (h *hostPathPlugin) SnapshotRestore(snapshotData *tprv1.VolumeSnapshotData, _ *v1.PersistentVolumeClaim, _ string, _ map[string]string) (*v1.PersistentVolumeSource, map[string]string, error) {
	// retrieve VolumeSnapshotDataSource
	if snapshotData == nil || snapshotData.Spec.HostPath == nil {
		return nil, nil, fmt.Errorf("failed to retrieve Snapshot spec")
	}
	// restore snapshot to a PV
	snapId := snapshotData.Spec.HostPath.Path
	dir := restorePoint + string(uuid.NewUUID())
	os.MkdirAll(dir, 0750)
	cmd := exec.Command("tar", "xzvf", snapId, "-C", dir)
	err := cmd.Run()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to restore %s to %s: %v", snapId, dir, err)
	}
	pv := &v1.PersistentVolumeSource{
		HostPath: &v1.HostPathVolumeSource{
			Path: dir,
		},
	}
	return pv, nil, nil
}
