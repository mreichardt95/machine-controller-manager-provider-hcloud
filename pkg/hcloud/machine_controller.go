/*
Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved.

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

// Package hcloud contains the HCloud provider specific implementations to manage machines
package hcloud

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/gardener/machine-controller-manager/pkg/util/provider/driver"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machinecodes/codes"
	"github.com/gardener/machine-controller-manager/pkg/util/provider/machinecodes/status"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"k8s.io/klog/v2"

	"github.com/23technologies/machine-controller-manager-provider-hcloud/pkg/hcloud/apis"
	"github.com/23technologies/machine-controller-manager-provider-hcloud/pkg/hcloud/apis/transcoder"
)

// CreateMachine handles a machine creation request
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The create request for VM creation
func (p *MachineProvider) CreateMachine(ctx context.Context, req *driver.CreateMachineRequest) (*driver.CreateMachineResponse, error) {
	extendedCtx := context.WithValue(ctx, CtxWrapDataKey("MethodData"), &CreateMachineMethodData{})

	resp, err := p.createMachine(extendedCtx, req)

	if err != nil {
		p.createMachineOnErrorCleanup(extendedCtx, req)
	}

	return resp, err
}

// createMachine handles the actual machine creation request without cleanup
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The create request for VM creation
func (p *MachineProvider) createMachine(ctx context.Context, req *driver.CreateMachineRequest) (*driver.CreateMachineResponse, error) {
	var (
		machine      = req.Machine
		machineClass = req.MachineClass
		secret       = req.Secret
		resultData   = ctx.Value(CtxWrapDataKey("MethodData")).(*CreateMachineMethodData)
	)

	// Log messages to track request
	klog.V(2).Infof("Machine creation request has been received for %q", machine.Name)
	defer klog.V(2).Infof("Machine creation request has been processed for %q", machine.Name)

	if machine.Spec.ProviderID != "" {
		return nil, status.Error(codes.InvalidArgument, "Machine creation with existing provider ID is not supported")
	}

	providerSpec, err := transcoder.DecodeProviderSpecFromMachineClass(machineClass, secret)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	userData, ok := secret.Data["userData"]
	if !ok {
		return nil, status.Error(codes.Internal, "userData doesn't exist")
	}

	client := apis.GetClientForToken(string(secret.Data["token"]))

	server, _, _ := client.Server.GetByName(ctx, machine.Name)

	if nil != server {
		var errorCode codes.Code
		isCleanupAvailable := p.createMachineValidateDeploymentAndCleanup(ctx, req, server)

		if isCleanupAvailable {
			errorCode = codes.Aborted
		} else {
			errorCode = codes.AlreadyExists
		}

		return nil, status.Error(errorCode, "Server already exists")
	}

	imageName := string(providerSpec.ImageName)
	userDataStr := string(userData)

	var image *hcloud.Image
	if imageID, parseErr := strconv.Atoi(imageName); parseErr == nil {
		image, _, err = client.Image.GetByID(ctx, imageID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		image, _, err = client.Image.GetByName(ctx, imageName)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	if image == nil {
		images, err := client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{Name: imageName, IncludeDeprecated: true})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		if len(images) == 0 {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Image %s not found", imageName))
		} else {
			image = images[0]
		}
	}

	region := apis.GetRegionFromZone(providerSpec.Zone)
	startAfterCreate := false
	zone := providerSpec.Zone

	opts := hcloud.ServerCreateOpts{
		Name:       machine.Name,
		ServerType: &hcloud.ServerType{Name: providerSpec.ServerType},
		Image:      image,
		Labels: map[string]string{
			"mcm.gardener.cloud/cluster":    providerSpec.Cluster,
			"mcm.gardener.cloud/role":       "node",
			"topology.kubernetes.io/region": region,
			"topology.kubernetes.io/zone":   zone,
		},
		Datacenter:       &hcloud.Datacenter{Name: zone},
		UserData:         userDataStr,
		StartAfterCreate: &startAfterCreate,
	}

	sshKey, _, err := client.SSHKey.GetByFingerprint(ctx, providerSpec.SSHFingerprint)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	} else if sshKey == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("SSH key with fingerprint %s not found", providerSpec.SSHFingerprint))
	}

	opts.SSHKeys = append(opts.SSHKeys, sshKey)

	if providerSpec.NetworkName != "" {
		network, _, err := client.Network.GetByName(ctx, providerSpec.NetworkName)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		} else if network == nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Network %s not found", providerSpec.NetworkName))
		}

		opts.Networks = append(opts.Networks, network)
	}

	serverResult, _, err := client.Server.Create(ctx, opts)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	resultData.ServerID = serverResult.Server.ID

	server, err = apis.WaitForActionsAndGetServer(ctx, client, serverResult.Server)
	if err != nil {
		return nil, status.Error(codes.Unknown, err.Error())
	}

	if providerSpec.FloatingPoolName != "" {
		placementGroupID, err := strconv.Atoi(providerSpec.PlacementGroupID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		placementGroup, _, err := client.PlacementGroup.GetByID(ctx, placementGroupID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		_, _, err = client.Server.AddToPlacementGroup(ctx, server, placementGroup)
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}

		server, err = apis.WaitForActionsAndGetServer(ctx, client, serverResult.Server)
		if err != nil {
			return nil, status.Error(codes.Unknown, err.Error())
		}
	}

	if providerSpec.FloatingPoolName != "" {
		name := fmt.Sprintf("%s-%s-ipv4", providerSpec.FloatingPoolName, machine.Name)

		floatingIP, _, err := client.FloatingIP.GetByName(ctx, name)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		} else if floatingIP == nil {
			opts := hcloud.FloatingIPCreateOpts{
				Name:   &name,
				Type:   hcloud.FloatingIPTypeIPv4,
				Server: server,
				Labels: map[string]string{
					"mcm.gardener.cloud/cluster":                         providerSpec.Cluster,
					"networking.hcloud.mcm.gardener.cloud/floating-pool": providerSpec.FloatingPoolName,
				},
			}

			ipResult, _, err := client.FloatingIP.Create(ctx, opts)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}

			resultData.FloatingIPID = ipResult.FloatingIP.ID

			_, err = apis.WaitForActionsAndGetFloatingIP(ctx, client, ipResult.FloatingIP)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
	}

	if hcloud.ServerStatusStarting != server.Status && hcloud.ServerStatusRunning != server.Status {
		_, _, err = client.Server.Poweron(ctx, server)
		if err != nil {
			return nil, status.Error(codes.Aborted, err.Error())
		}
	}

	server, err = apis.WaitForActionsAndGetServer(ctx, client, serverResult.Server)
	if err != nil {
		return nil, status.Error(codes.Unknown, err.Error())
	}

	// For some reason our machine was not started, return an error in this case
	// This behavior was observed from time to time. Therefore, this check is meaningful
	if server.Status != hcloud.ServerStatusRunning {
		return nil, status.Error(codes.Unknown, "Server was not started for some reason.")
	}

	// Compare running results with expectation
	unexpectedState := ""

	if providerSpec.FloatingPoolName != "" && len(server.PublicNet.FloatingIPs) != 1 {
		unexpectedState = "Floating IP set-up failed"
	}

	if providerSpec.NetworkName != "" && len(server.PrivateNet) != 1 {
		unexpectedState = "Private network set-up failed"
	}

	if unexpectedState != "" {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Server state does not match expectation: %s", unexpectedState))
	}

	response := &driver.CreateMachineResponse{
		ProviderID: transcoder.EncodeProviderID(providerSpec.Zone, server.ID),
		NodeName:   server.Name,
	}

	return response, nil
}

// createMachineOnErrorCleanup cleans up a failed machine creation request
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The create request for VM creation
// err error                        Error encountered
func (p *MachineProvider) createMachineValidateDeploymentAndCleanup(ctx context.Context, req *driver.CreateMachineRequest, server *hcloud.Server) bool {
	var (
		machine      = req.Machine
		machineClass = req.MachineClass
		secret       = req.Secret
	)

	providerSpec, err := transcoder.DecodeProviderSpecFromMachineClass(machineClass, secret)
	if err != nil {
		return false
	}

	isCleanupAvailable := true

	if clusterLabel, ok := server.Labels["mcm.gardener.cloud/cluster"]; !ok || providerSpec.Cluster != clusterLabel {
		isCleanupAvailable = false
	}

	if roleLabel, ok := server.Labels["mcm.gardener.cloud/role"]; !ok || roleLabel != "node" {
		isCleanupAvailable = false
	}

	region := apis.GetRegionFromZone(providerSpec.Zone)

	if regionLabel, ok := server.Labels["topology.kubernetes.io/region"]; !ok || region != regionLabel {
		isCleanupAvailable = false
	}

	if zoneLabel, ok := server.Labels["topology.kubernetes.io/zone"]; !ok || providerSpec.Zone != zoneLabel {
		isCleanupAvailable = false
	}

	if isCleanupAvailable {
		client := apis.GetClientForToken(string(req.Secret.Data["token"]))
		resultData := ctx.Value(CtxWrapDataKey("MethodData")).(*CreateMachineMethodData)

		resultData.ServerID = server.ID

		if providerSpec.FloatingPoolName != "" {
			name := fmt.Sprintf("%s-%s-ipv4", providerSpec.FloatingPoolName, machine.Name)

			floatingIP, _, _ := client.FloatingIP.GetByName(ctx, name)
			if floatingIP != nil {
				resultData.FloatingIPID = floatingIP.ID
			}
		}
	}

	return isCleanupAvailable
}

// createMachineOnErrorCleanup cleans up a failed machine creation request
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The create request for VM creation
// err error                        Error encountered
func (p *MachineProvider) createMachineOnErrorCleanup(ctx context.Context, req *driver.CreateMachineRequest) {
	client := apis.GetClientForToken(string(req.Secret.Data["token"]))
	resultData := ctx.Value(CtxWrapDataKey("MethodData")).(*CreateMachineMethodData)

	var server *hcloud.Server
	if resultData.ServerID != 0 {
		server, _, _ = client.Server.GetByID(ctx, resultData.ServerID)
	} else {
		server, _, _ = client.Server.GetByName(ctx, req.Machine.Name)
	}
	if nil != server {
		_, _ = client.Server.Delete(ctx, server)
	}

	if resultData.FloatingIPID != 0 {
		floatingIP, _, _ := client.FloatingIP.GetByID(ctx, resultData.FloatingIPID)
		if nil != floatingIP {
			_, _ = client.FloatingIP.Delete(ctx, floatingIP)
		}
	}
}

// DeleteMachine handles a machine deletion request
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The delete request for VM deletion
func (p *MachineProvider) DeleteMachine(ctx context.Context, req *driver.DeleteMachineRequest) (*driver.DeleteMachineResponse, error) {
	var (
		machine      = req.Machine
		machineClass = req.MachineClass
		secret       = req.Secret
	)

	// Log messages to track delete request
	klog.V(2).Infof("Machine deletion request has been received for %q", machine.Name)
	defer klog.V(2).Infof("Machine deletion request has been processed for %q", machine.Name)

	client := apis.GetClientForToken(string(secret.Data["token"]))

	providerSpec, err := transcoder.DecodeProviderSpecFromMachineClass(machineClass, secret)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	var server *hcloud.Server
	if machine.Spec.ProviderID != "" {
		serverID, err := transcoder.DecodeServerIDFromProviderID(machine.Spec.ProviderID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		server, _, err = client.Server.GetByID(ctx, serverID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		server, _, err = client.Server.GetByName(ctx, machine.Name)
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	} else if server == nil {
		klog.V(3).Infof("VM %s does not exist", machine.Name)
		return &driver.DeleteMachineResponse{}, nil
	}

	_, err = client.Server.Delete(ctx, server)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	if providerSpec.FloatingPoolName != "" {
		name := fmt.Sprintf("%s-%s-ipv4", providerSpec.FloatingPoolName, machine.Name)

		floatingIP, _, err := client.FloatingIP.GetByName(ctx, name)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		} else if nil != floatingIP {
			_, err = client.FloatingIP.Delete(ctx, floatingIP)
			if err != nil {
				return nil, status.Error(codes.Unavailable, err.Error())
			}
		}
	}

	return &driver.DeleteMachineResponse{}, nil
}

// GetMachineStatus handles a machine get status request
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The get request for VM info
func (p *MachineProvider) GetMachineStatus(ctx context.Context, req *driver.GetMachineStatusRequest) (*driver.GetMachineStatusResponse, error) {
	var (
		err          error
		machine      = req.Machine
		secret       = req.Secret
		machineClass = req.MachineClass
		server       *hcloud.Server
		serverID     int
	)

	// Log messages to track start and end of request
	klog.V(2).Infof("Get request has been received for %q", machine.Name)
	defer klog.V(2).Infof("Machine get request has been processed successfully for %q", machine.Name)

	providerSpec, err := transcoder.DecodeProviderSpecFromMachineClass(machineClass, secret)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	client := apis.GetClientForToken(string(secret.Data["token"]))

	// Handle case where machine lookup occurs with empty provider ID
	if machine.Spec.ProviderID == "" {
		server, _, err = client.Server.GetByName(ctx, machine.Name)

		if server == nil {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("Provider ID for machine %q is not defined (%q)", machine.Name, err))
		}

		serverID = server.ID
	} else {
		serverID, err := transcoder.DecodeServerIDFromProviderID(machine.Spec.ProviderID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		server, _, err = client.Server.GetByID(ctx, serverID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	} else if server == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("VM %s (%d) does not exist", machine.Name, serverID))
	}

	providerID := transcoder.EncodeProviderID(providerSpec.Zone, serverID)
	return &driver.GetMachineStatusResponse{ProviderID: providerID, NodeName: server.Name}, nil
}

// ListMachines lists all the machines possibilly created by a providerSpec
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The request object to get a list of VMs belonging to a machineClass
func (p *MachineProvider) ListMachines(ctx context.Context, req *driver.ListMachinesRequest) (*driver.ListMachinesResponse, error) {
	var (
		machineClass = req.MachineClass
		secret       = req.Secret
	)

	providerSpec, err := transcoder.DecodeProviderSpecFromMachineClass(machineClass, secret)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Log messages to track start and end of request
	klog.V(2).Infof("List machines request has been received for %q", machineClass.Name)
	defer klog.V(2).Infof("List machines request has been processed for %q", machineClass.Name)

	client := apis.GetClientForToken(string(secret.Data["token"]))
	zone := providerSpec.Zone

	listopts := hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf(
				"mcm.gardener.cloud/cluster=%s,mcm.gardener.cloud/role=node,topology.kubernetes.io/zone=%s",
				url.QueryEscape(providerSpec.Cluster),
				url.QueryEscape(zone),
			),
			PerPage: 50,
		},
	}

	servers, err := client.Server.AllWithOpts(ctx, listopts)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	listOfVMs := make(map[string]string)

	for _, server := range servers {
		listOfVMs[transcoder.EncodeProviderID(zone, server.ID)] = server.Name
	}

	return &driver.ListMachinesResponse{MachineList: listOfVMs}, nil
}

// GetVolumeIDs returns a list of Volume IDs for all PV Specs for whom an provider volume was found
//
// PARAMETERS
// ctx context.Context              Execution context
// req *driver.CreateMachineRequest The request object to get a list of VolumeIDs for a PVSpec
func (p *MachineProvider) GetVolumeIDs(_ context.Context, req *driver.GetVolumeIDsRequest) (*driver.GetVolumeIDsResponse, error) {
	// Log messages to track start and end of request
	klog.V(2).Infof("GetVolumeIDs request has been received for %q", req.PVSpecs)
	defer klog.V(2).Infof("GetVolumeIDs request has been processed successfully for %q", req.PVSpecs)

	return &driver.GetVolumeIDsResponse{}, status.Error(codes.Unimplemented, "")
}

// InitializeMachine handles VM initialization for hcloud VM's. Currently, un-implemented.
func (p *MachineProvider) InitializeMachine(_ context.Context, _ *driver.InitializeMachineRequest) (*driver.InitializeMachineResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Hcloud Provider does not yet implement InitializeMachine")
}
