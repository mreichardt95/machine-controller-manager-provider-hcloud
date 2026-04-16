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
	"encoding/json"
	"fmt"
)

// FlexString is a string that can be unmarshaled from both JSON strings and numbers.
type FlexString string

func (f *FlexString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err == nil {
		*f = FlexString(n.String())
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into FlexString", string(data))
}

// ProviderSpec is the spec to be used while parsing the calls.
type ProviderSpec struct {
	Cluster        string     `json:"cluster"`
	Zone           string     `json:"zone"`
	ServerType     string     `json:"serverType"`
	ImageName      FlexString `json:"imageName"`
	SSHFingerprint string     `json:"sshFingerprint"`

	PlacementGroupID string `json:"placementGroupID,omitempty"`
	FloatingPoolName string `json:"floatingPoolName,omitempty"`
	NetworkName      string `json:"networkName,omitempty"`
}
