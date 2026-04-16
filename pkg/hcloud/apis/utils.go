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

// Package apis is the main package for provider specific APIs
package apis

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/hetznercloud/hcloud-go/hcloud/schema"
)

// Constant defaultMachineOperationInterval is the time to wait between retries
const defaultMachineOperationInterval = 5 * time.Second

// Constant defaultMachineOperationRetries is the maximum number of retries
const defaultMachineOperationRetries = 60

// GetRegionFromZone returns the region for a given zone string
//
// PARAMETERS
// zone string Datacenter zone
func GetRegionFromZone(zone string) string {
	zoneData := strings.SplitN(zone, "-", 2)
	return zoneData[0]
}

// waitForActionsOfRequest waits for all actions to complete.
//
// PARAMETERS
// ctx    context.Context Execution context
// client *hcloud.Client  HCloud client
// req    *http.Request   Actions endpoint request to perform
func waitForActionsOfRequest(ctx context.Context, client *hcloud.Client, req *http.Request) error {
	var body schema.ActionListResponse
	repeat := true
	tryCount := 0

	for repeat {
		_, err := client.Do(req, &body)
		if err != nil {
			return err
		}

		repeat = len(body.Actions) > 0
		tryCount += 1

		if repeat {
			if tryCount > defaultMachineOperationRetries {
				return errors.New("maximum number of retries exceeded waiting for actions")
			}

			time.Sleep(defaultMachineOperationInterval)
		}
	}

	return nil
}

// WaitForActionsAndGetFloatingIP waits for all actions to complete for the floating IP given and returns it afterwards.
//
// PARAMETERS
// ctx    context.Context    Execution context
// client *hcloud.Client     HCloud client
// ip     *hcloud.FloatingIP HCloud floating IP struct
func WaitForActionsAndGetFloatingIP(ctx context.Context, client *hcloud.Client, ip *hcloud.FloatingIP) (*hcloud.FloatingIP, error) {
	req, err := client.NewRequest(ctx, "GET", fmt.Sprintf("/floating_ips/%d/actions?status=running", ip.ID), nil)
	if err != nil {
		return nil, err
	}

	err = waitForActionsOfRequest(ctx, client, req)
	if nil != err {
		return nil, err
	}

	ip, _, err = client.FloatingIP.GetByID(ctx, ip.ID)
	if nil != err {
		return nil, err
	}

	return ip, nil
}

// WaitForActionsAndGetServer waits for all actions to complete for the server given and returns it afterwards.
//
// PARAMETERS
// ctx    context.Context Execution context
// client *hcloud.Client  HCloud client
// ip     *hcloud.Server  HCloud server struct
func WaitForActionsAndGetServer(ctx context.Context, client *hcloud.Client, server *hcloud.Server) (*hcloud.Server, error) {
	req, err := client.NewRequest(ctx, "GET", fmt.Sprintf("/servers/%d/actions?status=running", server.ID), nil)
	if err != nil {
		return nil, err
	}

	err = waitForActionsOfRequest(ctx, client, req)
	if nil != err {
		return nil, err
	}

	server, _, err = client.Server.GetByID(ctx, server.ID)
	if nil != err {
		return nil, err
	}

	return server, nil
}
