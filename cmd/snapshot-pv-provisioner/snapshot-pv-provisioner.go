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

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/golang/glog"
	"github.com/kubernetes-incubator/external-storage/lib/controller"
	crdv1 "github.com/rootfs/snapshot/pkg/apis/crd/v1"
	crdclient "github.com/rootfs/snapshot/pkg/client"
	"github.com/rootfs/snapshot/pkg/cloudprovider"
	"github.com/rootfs/snapshot/pkg/cloudprovider/providers/aws"
	"github.com/rootfs/snapshot/pkg/cloudprovider/providers/gce"
	"github.com/rootfs/snapshot/pkg/cloudprovider/providers/openstack"
	"github.com/rootfs/snapshot/pkg/volume"
	"github.com/rootfs/snapshot/pkg/volume/aws_ebs"
	"github.com/rootfs/snapshot/pkg/volume/cinder"
	"github.com/rootfs/snapshot/pkg/volume/gce_pd"
	"github.com/rootfs/snapshot/pkg/volume/hostpath"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	provisionerName  = "volumesnapshot.external-storage.k8s.io/snapshot-promoter"
	provisionerIDAnn = "snapshotProvisionerIdentity"
)

type snapshotProvisioner struct {
	// Kubernetes Client.
	client kubernetes.Interface
	// CRD client
	crdclient *rest.RESTClient
	// Identity of this snapshotProvisioner, generated. Used to identify "this"
	// provisioner's PVs.
	identity string
}

func newSnapshotProvisioner(client kubernetes.Interface, crdclient *rest.RESTClient, id string) controller.Provisioner {
	return &snapshotProvisioner{
		client:    client,
		crdclient: crdclient,
		identity:  id,
	}
}

var _ controller.Provisioner = &snapshotProvisioner{}

func (p *snapshotProvisioner) getPVFromVolumeSnapshotDataSpec(snapshotDataSpec *crdv1.VolumeSnapshotDataSpec) (*v1.PersistentVolume, error) {
	if snapshotDataSpec.PersistentVolumeRef == nil {
		return nil, fmt.Errorf("VolumeSnapshotDataSpec is not bound to any PV")
	}
	pvName := snapshotDataSpec.PersistentVolumeRef.Name
	if pvName == "" {
		return nil, fmt.Errorf("The PV name is not specified in snapshotdata %#v", *snapshotDataSpec)
	}
	pv, err := p.client.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve PV %s from the API server: %q", pvName, err)
	}
	return pv, nil
}

func (p *snapshotProvisioner) snapshotRestore(snapshotName string, snapshotData crdv1.VolumeSnapshotData, options controller.VolumeOptions) (*v1.PersistentVolumeSource, map[string]string, error) {
	// validate the PV supports snapshot and restore
	spec := &snapshotData.Spec
	pv, err := p.getPVFromVolumeSnapshotDataSpec(spec)
	if err != nil {
		return nil, nil, err
	}
	volumeType := crdv1.GetSupportedVolumeFromPVSpec(&pv.Spec)
	if len(volumeType) == 0 {
		return nil, nil, fmt.Errorf("unsupported volume type found in PV %#v", *spec)
	}
	plugin, ok := volumePlugins[volumeType]
	if !ok {
		return nil, nil, fmt.Errorf("%s is not supported volume for %#v", volumeType, *spec)
	}

	// restore snapshot
	pvSrc, labels, err := plugin.SnapshotRestore(&snapshotData, options.PVC, options.PVName, options.Parameters)
	if err != nil && pv == nil {
		glog.Warningf("failed to snapshot %#v, err: %v", *spec, err)
	} else {
		glog.Infof("snapshot %#v to snap %#v", *spec, *pvSrc)
		return pvSrc, labels, nil
	}

	return nil, nil, nil
}

// Provision creates a storage asset and returns a PV object representing it.
func (p *snapshotProvisioner) Provision(options controller.VolumeOptions) (*v1.PersistentVolume, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, fmt.Errorf("claim Selector is not supported")
	}
	snapshotName, ok := options.PVC.Annotations[crdclient.SnapshotPVCAnnotation]
	if !ok {
		return nil, fmt.Errorf("snapshot annotation not found on PV")
	}

	var snapshot crdv1.VolumeSnapshot
	err := p.crdclient.Get().
		Resource(crdv1.VolumeSnapshotResourcePlural).
		Namespace(options.PVC.Namespace).
		Name(snapshotName).
		Do().Into(&snapshot)

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve VolumeSnapshot %s: %v", snapshotName, err)
	}
	// FIXME: should also check if any VolumeSnapshotData points to this VolumeSnapshot
	if len(snapshot.Spec.SnapshotDataName) == 0 {
		return nil, fmt.Errorf("VolumeSnapshot %s is not bound to any VolumeSnapshotData", snapshotName, err)
	}
	var snapshotData crdv1.VolumeSnapshotData
	err = p.crdclient.Get().
		Resource(crdv1.VolumeSnapshotDataResourcePlural).
		Namespace(v1.NamespaceDefault).
		Name(snapshot.Spec.SnapshotDataName).
		Do().Into(&snapshotData)

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve VolumeSnapshotData %s: %v", snapshot.Spec.SnapshotDataName, err)
	}
	glog.V(3).Infof("restore from VolumeSnapshotData %s", snapshot.Spec.SnapshotDataName)

	pvSrc, labels, err := p.snapshotRestore(snapshot.Spec.SnapshotDataName, snapshotData, options)
	if err != nil || pvSrc == nil {
		return nil, fmt.Errorf("failed to create a PV from snapshot %s: %v", snapshotName, err)
	}
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
			Annotations: map[string]string{
				provisionerIDAnn: p.identity,
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: options.PersistentVolumeReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)],
			},
			PersistentVolumeSource: *pvSrc,
		},
	}

	if len(labels) != 0 {
		if pv.Labels == nil {
			pv.Labels = make(map[string]string)
		}
		for k, v := range labels {
			pv.Labels[k] = v
		}
	}

	glog.Infof("successfully created Snapshot share %#v", pv)

	return pv, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *snapshotProvisioner) Delete(volume *v1.PersistentVolume) error {
	ann, ok := volume.Annotations[provisionerIDAnn]
	if !ok {
		return errors.New("identity annotation not found on PV")
	}
	if ann != p.identity {
		return &controller.IgnoredError{"identity annotation on PV does not match ours"}
	}

	volumeType := crdv1.GetSupportedVolumeFromPVSpec(&volume.Spec)
	if len(volumeType) == 0 {
		return fmt.Errorf("unsupported volume type found in PV %#v", *volume)
	}
	plugin, ok := volumePlugins[volumeType]
	if !ok {
		return fmt.Errorf("%s is not supported volume for %#v", volumeType, *volume)
	}

	// delete PV
	return plugin.VolumeDelete(volume)
}

var (
	master          = flag.String("master", "", "Master URL")
	kubeconfig      = flag.String("kubeconfig", "", "Absolute path to the kubeconfig")
	id              = flag.String("id", "", "Unique provisioner identity")
	cloudProvider   = flag.String("cloudprovider", "", "aws|gce|openstack")
	cloudConfigFile = flag.String("cloudconfig", "", "Path to a Cloud config. Only required if cloudprovider is set.")
	volumePlugins   = make(map[string]volume.VolumePlugin)
)

func main() {
	flag.Parse()
	flag.Set("logtostderr", "true")

	var config *rest.Config
	var err error
	if *master != "" || *kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	prId := string(uuid.NewUUID())
	if *id != "" {
		prId = *id
	}
	if err != nil {
		glog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		glog.Fatalf("Error getting server version: %v", err)
	}

	// build volume plugins map
	buildVolumePlugins()

	// make a crd client to list VolumeSnapshot
	snapshotClient, _, err := crdclient.NewClient(config)
	if err != nil || snapshotClient == nil {
		glog.Fatalf("Failed to make CRD client: %v", err)
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	snapshotProvisioner := newSnapshotProvisioner(clientset, snapshotClient, prId)

	// Start the provision controller which will dynamically provision snapshot
	// PVs
	pc := controller.NewProvisionController(
		clientset,
		provisionerName,
		snapshotProvisioner,
		serverVersion.GitVersion,
	)
	glog.Infof("starting PV provisioner %s", provisionerName)
	pc.Run(wait.NeverStop)
}

func buildVolumePlugins() {
	if len(*cloudProvider) != 0 {
		cloud, err := cloudprovider.InitCloudProvider(*cloudProvider, *cloudConfigFile)
		if err == nil && cloud != nil {
			if *cloudProvider == aws.ProviderName {
				awsPlugin := aws_ebs.RegisterPlugin()
				awsPlugin.Init(cloud)
				volumePlugins[aws_ebs.GetPluginName()] = awsPlugin
			}
			if *cloudProvider == gce.ProviderName {
				gcePlugin := gce_pd.RegisterPlugin()
				gcePlugin.Init(cloud)
				volumePlugins[gce_pd.GetPluginName()] = gcePlugin
				glog.Info("Register cloudprovider %s", gce_pd.GetPluginName())
			}
			if *cloudProvider == openstack.ProviderName {
				cinderPlugin := cinder.RegisterPlugin()
				cinderPlugin.Init(cloud)
				volumePlugins[cinder.GetPluginName()] = cinderPlugin
				glog.Info("Register cloudprovider %s", cinder.GetPluginName())
			}
		} else {
			glog.Warningf("failed to initialize aws cloudprovider: %v, supported cloudproviders are %#v", err, cloudprovider.CloudProviders())
		}
	}

	volumePlugins[hostpath.GetPluginName()] = hostpath.RegisterPlugin()

}
