/*
Copyright 2019 The Kubernetes Authors.

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

package wcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"

	"github.com/container-storage-interface/spec/lib/go/csi"
	cnstypes "github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/units"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cnsvolume "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/vsphere"
	"sigs.k8s.io/vsphere-csi-driver/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	csitypes "sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
)

var (
	// controllerCaps represents the capability of controller service
	controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	}
)

var getSharedDatastores = getSharedDatastoresInPodVMK8SCluster

type controller struct {
	manager *common.Manager
}

// New creates a CNS controller
func New() csitypes.CnsController {
	return &controller{}
}

// Init is initializing controller struct
func (c *controller) Init(config *config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)

	log.Infof("Initializing WCP CSI controller")
	var err error
	// Get VirtualCenterManager instance and validate version
	vcenterconfig, err := cnsvsphere.GetVirtualCenterConfig(config)
	if err != nil {
		log.Errorf("failed to get VirtualCenterConfig. err=%v", err)
		return err
	}
	vcManager := cnsvsphere.GetVirtualCenterManager(ctx)
	vcenter, err := vcManager.RegisterVirtualCenter(ctx, vcenterconfig)
	if err != nil {
		log.Errorf("failed to register VC with virtualCenterManager. err=%v", err)
		return err
	}
	c.manager = &common.Manager{
		VcenterConfig:  vcenterconfig,
		CnsConfig:      config,
		VolumeManager:  cnsvolume.GetManager(ctx, vcenter),
		VcenterManager: cnsvsphere.GetVirtualCenterManager(ctx),
	}

	vc, err := common.GetVCenter(ctx, c.manager)
	if err != nil {
		log.Errorf("failed to get vcenter. err=%v", err)
		return err
	}
	// Check vCenter API Version
	if err = common.CheckAPI(vc.Client.ServiceContent.About.ApiVersion); err != nil {
		log.Errorf("checkAPI failed for vcenter API version: %s, err=%v", vc.Client.ServiceContent.About.ApiVersion, err)
		return err
	}
	go cnsvolume.ClearTaskInfoObjects()
	cfgPath := common.GetConfigPath(ctx)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Errorf("failed to create fsnotify watcher. err=%v", err)
		return err
	}
	go func() {
		for {
			log.Debugf("Waiting for event on fsnotify watcher")
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				log.Debugf("fsnotify event: %q", event.String())
				if event.Op&fsnotify.Remove == fsnotify.Remove {
					c.ReloadConfiguration()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					log.Errorf("fsnotify error: %+v", err)
					return
				}
			}
			log.Debugf("fsnotify event processed")
		}
	}()
	cfgDirPath := filepath.Dir(cfgPath)
	log.Infof("Adding watch on path: %q", cfgDirPath)
	err = watcher.Add(cfgDirPath)
	if err != nil {
		log.Errorf("failed to watch on path: %q. err=%v", cfgDirPath, err)
		return err
	}
	return nil
}

// ReloadConfiguration reloads configuration from the secret, and update controller's config cache
// and VolumeManager's VC Config cache.
func (c *controller) ReloadConfiguration() {
	ctx, log := logger.GetNewContextWithLogger()
	log.Info("Reloading Configuration")
	cfg, err := common.GetConfig(ctx)
	if err != nil {
		log.Errorf("failed to read config. Error: %+v", err)
		return
	}
	newVCConfig, err := cnsvsphere.GetVirtualCenterConfig(cfg)
	if err != nil {
		log.Errorf("failed to get VirtualCenterConfig. err=%v", err)
		return
	}
	if newVCConfig != nil {
		var vcenter *cnsvsphere.VirtualCenter
		if c.manager.VcenterConfig.Host != newVCConfig.Host ||
			c.manager.VcenterConfig.Username != newVCConfig.Username ||
			c.manager.VcenterConfig.Password != newVCConfig.Password {
			log.Debugf("Unregistering virtual center: %q from virtualCenterManager", c.manager.VcenterConfig.Host)
			err = c.manager.VcenterManager.UnregisterAllVirtualCenters(ctx)
			if err != nil {
				log.Errorf("failed to unregister vcenter with virtualCenterManager.")
				return
			}
			log.Debugf("Registering virtual center: %q with virtualCenterManager", newVCConfig.Host)
			vcenter, err = c.manager.VcenterManager.RegisterVirtualCenter(ctx, newVCConfig)
			if err != nil {
				log.Errorf("failed to register VC with virtualCenterManager. err=%v", err)
				return
			}
			c.manager.VcenterManager = cnsvsphere.GetVirtualCenterManager(ctx)
		} else {
			vcenter, err = c.manager.VcenterManager.GetVirtualCenter(ctx, newVCConfig.Host)
			if err != nil {
				log.Errorf("failed to get VirtualCenter. err=%v", err)
				return
			}
		}
		c.manager.VolumeManager.ResetManager(ctx, vcenter)
		c.manager.VolumeManager = cnsvolume.GetManager(ctx, vcenter)
		c.manager.VcenterConfig = newVCConfig
	}
	if cfg != nil {
		log.Debugf("updating manager.CnsConfig")
		c.manager.CnsConfig = cfg
	}
	log.Info("Successfully reloaded configuration")
}

// CreateVolume is creating CNS Volume using volume request specified
// in CreateVolumeRequest
func (c *controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("CreateVolume: called with args %+v", *req)
	err := validateWCPCreateVolumeRequest(ctx, req)
	if err != nil {
		msg := fmt.Sprintf("Validation for CreateVolume Request: %+v has failed. Error: %+v", *req, err)
		log.Error(msg)
		return nil, err
	}

	// Volume Size - Default is 10 GiB
	volSizeBytes := int64(common.DefaultGbDiskSize * common.GbInBytes)
	if req.GetCapacityRange() != nil && req.GetCapacityRange().RequiredBytes != 0 {
		volSizeBytes = int64(req.GetCapacityRange().GetRequiredBytes())
	}
	volSizeMB := int64(common.RoundUpSize(volSizeBytes, common.MbInBytes))

	var storagePolicyID string

	var affineToHost string
	// Support case insensitive parameters
	for paramName := range req.Parameters {
		param := strings.ToLower(paramName)
		if param == common.AttributeStoragePolicyID {
			storagePolicyID = req.Parameters[paramName]
		} else if param == common.AttributeAffineToHost {
			affineToHost = req.Parameters[common.AttributeAffineToHost]
		}
	}

	var createVolumeSpec = common.CreateVolumeSpec{
		CapacityMB:      volSizeMB,
		Name:            req.Name,
		StoragePolicyID: storagePolicyID,
		ScParams:        &common.StorageClassParams{},
		AffineToHost:    affineToHost,
		VolumeType:      common.BlockVolumeType,
	}
	// Get shared datastores for the Kubernetes cluster
	sharedDatastores, err := getSharedDatastores(ctx, c)
	if err != nil {
		msg := fmt.Sprintf("failed to obtain shared datastores. Error: %+v", err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}
	volumeID, err := common.CreateBlockVolumeUtil(ctx, cnstypes.CnsClusterFlavorWorkload, c.manager, &createVolumeSpec, sharedDatastores)
	if err != nil {
		msg := fmt.Sprintf("failed to create volume. Error: %+v", err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}
	attributes := make(map[string]string)
	attributes[common.AttributeDiskType] = common.DiskTypeBlockVolume
	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: int64(units.FileSize(volSizeMB * common.MbInBytes)),
			VolumeContext: attributes,
		},
	}
	return resp, nil
}

// DeleteVolume is deleting CNS Volume specified in DeleteVolumeRequest
func (c *controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DeleteVolume: called with args: %+v", *req)
	var err error
	err = validateWCPDeleteVolumeRequest(ctx, req)
	if err != nil {
		msg := fmt.Sprintf("Validation for DeleteVolume Request: %+v has failed. Error: %+v", *req, err)
		log.Error(msg)
		return nil, err
	}
	err = common.DeleteVolumeUtil(ctx, c.manager, req.VolumeId, true)
	if err != nil {
		msg := fmt.Sprintf("failed to delete volume: %q. Error: %+v", req.VolumeId, err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches a volume to the Node VM.
// volume id and node name is retrieved from ControllerPublishVolumeRequest
func (c *controller) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerPublishVolume: called with args %+v", *req)
	err := validateWCPControllerPublishVolumeRequest(ctx, req)
	if err != nil {
		msg := fmt.Sprintf("Validation for PublishVolume Request: %+v has failed. Error: %v", *req, err)
		log.Errorf(msg)
		return nil, err
	}

	vmuuid, err := getVMUUIDFromPodListenerService(ctx, req.VolumeId, req.NodeId)
	if err != nil {
		msg := fmt.Sprintf("failed to get the pod vmuuid annotation from the pod listener service when processing attach for volumeID: %s on node: %s. Error: %+v", req.VolumeId, req.NodeId, err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	vcdcMap, err := getDatacenterFromConfig(c.manager.CnsConfig)
	if err != nil {
		msg := fmt.Sprintf("failed to get datacenter from config with error: %+v", err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}
	var vCenterHost, dcMorefValue string
	for key, value := range vcdcMap {
		vCenterHost = key
		dcMorefValue = value
	}
	vc, err := c.manager.VcenterManager.GetVirtualCenter(ctx, vCenterHost)
	if err != nil {
		msg := fmt.Sprintf("Cannot get virtual center %s from virtualcentermanager while attaching disk with error %+v",
			vc.Config.Host, err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	// Connect to VC
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = vc.Connect(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to connect to Virtual Center: %s", vc.Config.Host)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	podVM, err := getVMByInstanceUUIDInDatacenter(ctx, vc, dcMorefValue, vmuuid)
	if err != nil {
		msg := fmt.Sprintf("failed to the PodVM Moref from the PodVM UUID: %s in datacenter: %s with err: %+v", vmuuid, dcMorefValue, err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	// Attach the volume to the node
	diskUUID, err := common.AttachVolumeUtil(ctx, c.manager, podVM, req.VolumeId)
	if err != nil {
		msg := fmt.Sprintf("failed to attach volume with volumeID: %s. Error: %+v", req.VolumeId, err)
		log.Error(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	publishInfo := make(map[string]string)
	publishInfo[common.AttributeDiskType] = common.DiskTypeBlockVolume
	publishInfo[common.AttributeFirstClassDiskUUID] = common.FormatDiskUUID(diskUUID)
	resp := &csi.ControllerPublishVolumeResponse{
		PublishContext: publishInfo,
	}

	return resp, nil
}

// ControllerUnpublishVolume detaches a volume from the Node VM.
// volume id and node name is retrieved from ControllerUnpublishVolumeRequest
func (c *controller) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerUnpublishVolume: called with args %+v", *req)
	err := validateWCPControllerUnpublishVolumeRequest(ctx, req)
	if err != nil {
		msg := fmt.Sprintf("Validation for UnpublishVolume Request: %+v has failed. Error: %v", *req, err)
		log.Error(msg)
		return nil, err
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities returns the capabilities of the volume.
func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerGetCapabilities: called with args %+v", *req)
	volCaps := req.GetVolumeCapabilities()
	var confirmed *csi.ValidateVolumeCapabilitiesResponse_Confirmed
	if common.IsValidVolumeCapabilities(ctx, volCaps) {
		confirmed = &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: volCaps}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: confirmed,
	}, nil
}

func (c *controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ListVolumes: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (c *controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("GetCapacity: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (c *controller) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {

	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerGetCapabilities: called with args %+v", *req)
	var caps []*csi.ControllerServiceCapability
	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (c *controller) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (
	*csi.CreateSnapshotResponse, error) {

	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("CreateSnapshot: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (c *controller) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (
	*csi.DeleteSnapshotResponse, error) {

	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("DeleteSnapshot: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (c *controller) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (
	*csi.ListSnapshotsResponse, error) {

	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ListSnapshots: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume expands a volume.
func (c *controller) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (
	*csi.ControllerExpandVolumeResponse, error) {
	ctx = logger.NewContextWithLogger(ctx)
	log := logger.GetLogger(ctx)
	log.Infof("ControllerExpandVolume: called with args %+v", *req)
	return nil, status.Error(codes.Unimplemented, "")
}

// GetSharedDatastoresInPodVMK8SCluster gets the shared datastores for WCP PodVM cluster
func getSharedDatastoresInPodVMK8SCluster(ctx context.Context, c *controller) ([]*cnsvsphere.DatastoreInfo, error) {
	log := logger.GetLogger(ctx)
	vc, err := common.GetVCenter(ctx, c.manager)
	if err != nil {
		log.Errorf("failed to get vCenter from Manager, err=%+v", err)
		return nil, err
	}
	hosts, err := vc.GetHostsByCluster(ctx, c.manager.CnsConfig.Global.ClusterID)
	if err != nil {
		log.Errorf("failed to get hosts from VC with err %+v", err)
		return nil, err
	}
	if len(hosts) == 0 {
		errMsg := "Empty List of hosts returned from VC"
		log.Errorf(errMsg)
		return make([]*cnsvsphere.DatastoreInfo, 0), fmt.Errorf(errMsg)
	}
	var sharedDatastores []*cnsvsphere.DatastoreInfo
	for _, host := range hosts {
		log.Debugf("Getting accessible datastores for node %s", host.InventoryPath)
		accessibleDatastores, err := host.GetAllAccessibleDatastores(ctx)
		if err != nil {
			return nil, err
		}
		if len(sharedDatastores) == 0 {
			sharedDatastores = accessibleDatastores
		} else {
			var sharedAccessibleDatastores []*cnsvsphere.DatastoreInfo
			// Check if sharedDatastores is found in accessibleDatastores
			for _, sharedDs := range sharedDatastores {
				for _, accessibleDs := range accessibleDatastores {
					// Intersection is performed based on the datastoreUrl as this uniquely identifies the datastore.
					if sharedDs.Info.Url == accessibleDs.Info.Url {
						sharedAccessibleDatastores = append(sharedAccessibleDatastores, sharedDs)
						break
					}
				}
			}
			sharedDatastores = sharedAccessibleDatastores
		}
		if len(sharedDatastores) == 0 {
			return nil, fmt.Errorf("No shared datastores found in the Kubernetes cluster for host: %+v", host)
		}
	}
	log.Debugf("The list of shared datastores: %+v", sharedDatastores)
	return sharedDatastores, nil
}
