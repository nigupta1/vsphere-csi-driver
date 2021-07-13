/*
Copyright 2020 The Kubernetes Authors.

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

package storagepool

import (
	"context"
	"fmt"
	"strings"

	vmoperatortypes "github.com/vmware-tanzu/vm-operator-api/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	storagepoolv1alpha1 "sigs.k8s.io/vsphere-csi-driver/pkg/apis/storagepool/cns/v1alpha1"
	commonconfig "sigs.k8s.io/vsphere-csi-driver/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	k8s "sigs.k8s.io/vsphere-csi-driver/pkg/kubernetes"
)

type GuestStoragePoolController struct {
	k8sDynamicSupervisorClient dynamic.Interface
	k8sDynamicGuestClient      dynamic.Interface
	vmOperatorClient           client.Client
	spResource                 *schema.GroupVersionResource
	spWatch                    watch.Interface
	clientset                  *kubernetes.Clientset
	supervisorNamespace        string
}

func initStoragePoolServiceOnGC(ctx context.Context, info *commonconfig.ConfigurationInfo) error {
	log := logger.GetLogger(ctx)
	// Get a config to talk to the apiserver
	restConfig, err := config.GetConfig()
	if err != nil {
		log.Errorf("failed to get Kubernetes config. Err: %+v", err)
		return err
	}
	spOperatorClient, err := k8s.NewClientForGroup(ctx, restConfig, storagepoolv1alpha1.SchemeGroupVersion.Group+"/"+storagepoolv1alpha1.SchemeGroupVersion.Version)
	if err != nil {
		log.Errorf("Failed to create StoragePoolOperator client. Err: %+v", err, spOperatorClient)
		return err
	}
	log.Info("created storagePoolOperator")

	restClientConfig := k8s.GetRestClientConfigForSupervisor(ctx, info.Cfg.GC.Endpoint, info.Cfg.GC.Port)
	log.Info("nitish" + info.Cfg.GC.Endpoint + info.Cfg.GC.Port)

	k8sDynamicSupervisorClient, _, err := getSPClientWithConfig(ctx, restClientConfig) // dynamic.NewForConfig(restClientConfig)
	if err != nil {
		log.Errorf("Failed to create StoragePool client using config. Err: %+v", err)
		return err
	}

	k8sDynamicGuestClient, spResource, err := getSPClient(ctx)
	if err != nil {
		log.Errorf("Failed to create local StoragePool client. Err: %+v", err)
		return err
	}

	cfg, err := config.GetConfig()
	if err != nil {
		log.Errorf("Failed to get Kubernetes config. Err: %+v", err)
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Errorf("Failed to create Kubernetes client. Err: %+v", err)
		return err
	}
	guestStoragePoolController := &GuestStoragePoolController{}
	guestStoragePoolController.k8sDynamicSupervisorClient = k8sDynamicSupervisorClient
	guestStoragePoolController.k8sDynamicGuestClient = k8sDynamicGuestClient
	guestStoragePoolController.vmOperatorClient, err = k8s.NewClientForGroup(ctx, restClientConfig, vmoperatortypes.GroupName)
	if err != nil {
		log.Error(err)
	}

	guestStoragePoolController.spResource = spResource
	guestStoragePoolController.supervisorNamespace, err = commonconfig.GetSupervisorNamespace(ctx)
	if err != nil {
		log.Error(err)
	}
	err = guestStoragePoolController.renewStoragePoolWatch(ctx)
	if err != nil {
		log.Error(err)
	}
	guestStoragePoolController.clientset = client
	go guestStoragePoolController.watchStoragePool(ctx)

	//spResource := schema.GroupVersion{Group: apis.GroupName, Version: apis.Version}.WithResource("storagepools")

	// TODO Enable label on each storage pool and use label as filter storage
	// pool list.
	spList, err := k8sDynamicSupervisorClient.Resource(*spResource).List(ctx, metav1.ListOptions{
		LabelSelector: spTypeLabelKey,
	})
	if err != nil {
		log.Errorf("Failed to get StoragePool list. Error: %+v", err)
		return err
	}
	for _, sp := range spList.Items {
		log.Info("Storage pool: " + sp.GetName())
	}
	return nil
}

// As our watch can and will expire, we need a helper to renew it. Note that after we re-new it,
// we will get a bunch of already processed events.
func (w *GuestStoragePoolController) renewStoragePoolWatch(ctx context.Context) error {
	log := logger.GetLogger(ctx)
	// This means every 24h our watch may expire and require to be re-created.
	// When that happens, we may need to do a full remediation, hence we change
	// from 30m (default) to 24h.
	timeout := int64(60 * 60 * 24) // 24h
	spWatch, err := w.k8sDynamicSupervisorClient.Resource(*w.spResource).Watch(ctx, metav1.ListOptions{
		TimeoutSeconds: &timeout,
	})
	if err != nil {
		log.Errorf("Failed to start StoragePool watch. Error: %v", err)
		return err
	}
	w.spWatch = spWatch
	return nil
}

// watchStoragePool looks for event putting a SP under disk decommission. It does so by storing the current drain label
// value for each StoragePool. Once it gets an event which updates the drain label (established by comparing stored drain
// label value with new one) of a SP to ensureAccessibilityMM/fullDataEvacuationMM/noMigrationMM it invokes the func to process disk decommossion
// of that storage pool.
func (w *GuestStoragePoolController) watchStoragePool(ctx context.Context) {
	log := logger.GetLogger(ctx)
	done := false
	for !done {
		select {
		case <-ctx.Done():
			log.Info("StoragePool watch shutdown", "ctxErr", ctx.Err())
			done = true
		case e, ok := <-w.spWatch.ResultChan():
			if !ok {
				log.Info("StoragePool watch not ok")
				err := w.renewStoragePoolWatch(ctx)
				for err != nil {
					err = w.renewStoragePoolWatch(ctx)
				}
				continue
			}

			spU, ok := e.Object.(*unstructured.Unstructured)
			if !ok {
				log.Warnf("Object in StoragePool watch event is not of type *unstructured.Unstructured, but of type %T", e.Object)
				continue
			}
			spName := spU.GetName()
			log.Info(spName)
			log.Info(e.Type)
			var sp storagepoolv1alpha1.StoragePool
			err := runtime.DefaultUnstructuredConverter.
				FromUnstructured(spU.UnstructuredContent(), &sp)
			if err != nil {
				log.Error(err)
			} else {
				w.addStoragePool(ctx, &sp)
			}
			//e.Object.(*w.spResource)
		}
	}
	log.Info("watchStoragePool ends")
}

func (w *GuestStoragePoolController) addStoragePool(ctx context.Context, sp *storagepoolv1alpha1.StoragePool) {
	spTypePrefix := "cns.vmware.com/"
	//first get all the storage classes in the
	log := logger.GetLogger(ctx)
	scList, err := w.clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Info("could not get storage classes")
		//return err
	}

	var intesectedSc []string
	for _, sc := range scList.Items {
		name := sc.Name
		for _, sc1 := range sp.Status.CompatibleStorageClasses {
			if name == sc1 {
				intesectedSc = append(intesectedSc, name)
			}
		}
	}
	log.Infof("Intersected sc %v", intesectedSc)

	//find intersected hosts
	//get all nodes in the cluster
	//get the supervisor node for the guest cluster node. For first level it is IP
	//how to check if vm is relocated(watch on vms on cluster on supervisor?)
	var svVmsForGC []vmoperatortypes.VirtualMachine
	nodeList, err := w.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "!node-role.kubernetes.io/master"})
	if err != nil {
		log.Errorf("Cannot list nodes %+v", err)
		//return err
	}
	for _, node := range nodeList.Items {
		virtualMachine := &vmoperatortypes.VirtualMachine{}
		vmKey := types.NamespacedName{
			Namespace: w.supervisorNamespace,
			Name:      node.Name,
		}
		if err = w.vmOperatorClient.Get(ctx, vmKey, virtualMachine); err != nil {
			msg := fmt.Sprintf("failed to get VirtualMachines for the node: %q. Error: %+v", node.Name, err)
			log.Error(msg)
			//return nil, status.Errorf(codes.Internal, msg)
		}
		log.Infof("Got Vm %s for node %s", virtualMachine.Name, node.Name)
		svVmsForGC = append(svVmsForGC, *virtualMachine)
	}
	//log.Info(svVmsForGC)
	log.Info(sp.Status.AccessibleNodes)
	var intesectedNodes []string
	for _, node := range svVmsForGC {
		for _, svNode := range sp.Status.AccessibleNodes {
			//change hypens to . as accessible is node but vmoperator has hosts with ip
			svNode = strings.ReplaceAll(svNode, "-", ".")
			log.Info(node.Status.Host)
			if node.Status.Host == svNode {
				intesectedNodes = append(intesectedNodes, node.Name)
			}
		}
	}

	log.Info(intesectedNodes)
	accesible := false
	if intesectedNodes != nil && len(intesectedNodes) > 0 {
		accesible = true
	}
	state := &intendedState{
		spName:           sp.Name,
		dsType:           spTypePrefix + sp.ObjectMeta.Labels["cns.vmware.com/StoragePoolType"],
		url:              sp.Spec.Parameters["datastoreUrl"],
		nodes:            intesectedNodes,
		compatSC:         intesectedSc,
		capacity:         sp.Status.Capacity.Total,
		freeSpace:        sp.Status.Capacity.FreeSpace,
		allocatableSpace: sp.Status.Capacity.AllocatableSpace,
		accessible:       accesible,
		//todo add state errors
	}
	err = w.applyIntendedState(ctx, state)
	if err != nil {
		log.Error(err)
	}
	/*struct {
		// Datastore moid in VC
		dsMoid string
		// Datastore type in VC
		dsType string
		// StoragePool name derived from datastore's name
		spName string
		// From Datastore.summary.capacity in VC
		capacity *resource.Quantity
		// From Datastore.summary.freeSpace in VC
		freeSpace *resource.Quantity
		// obtained after subtracting overhead from free space. for vSAN-SNA its 4% less of free space, for vSAN-Direct its 4 MiB less of free space.
		// for vSAN default overhead depends on type of policy used, currently its also 4% less of free space.
		allocatableSpace *resource.Quantity
		// from Datastore.summary.Url
		url string
		// from Datastore.summary.Accessible
		accessible bool
		// from Datastore.summary.maintenanceMode
		datastoreInMM bool
		// true only when all hosts this Datastore is mounted on is in MM
		allHostsInMM bool
		// accessible list of k8s nodes on which this datastore is mounted in VC cluster
		nodes []string
		// compatible list of StorageClass names computed from SPBM
		compatSC []string
		// is a remote vSAN Datastore mounted into this cluster - HCI Mesh feature
		isRemoteVsan bool
	}*/

	//w.k8sDynamicGuestClient.Resource(*spResource).Create(ctx, sp, metav1.CreateOptions{})
}
