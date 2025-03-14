// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// +build !windows

package gce

import (
	"context"
	"fmt"
	"os"
	"time"

	log "github.com/golang/glog"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
)

// EnvGCENetworkProjectID is an environment variable to set the network project
// When set, network routes will be created within a network project instead of the project running the instances
const EnvGCENetworkProjectID = "GCE_NETWORK_PROJECT_ID"

type gceAPI struct {
	project        string
	useIPNextHop   bool
	computeService *compute.Service
	gceNetwork     *compute.Network
	gceInstance    *compute.Instance
}

// limit auth scope to just the required GCP API's
// https://developers.google.com/identity/protocols/oauth2/scopes
func gceScopes() []string {
	return []string{"https://www.googleapis.com/auth/compute"}
}

func newAPI() (*gceAPI, error) {
	client, err := google.DefaultClient(context.TODO(), gceScopes()...)
	if err != nil {
		return nil, fmt.Errorf("error creating client: %v", err)
	}

	cs, err := compute.New(client)
	if err != nil {
		return nil, fmt.Errorf("error creating compute service: %v", err)
	}

	networkName, err := networkFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting network metadata: %v", err)
	}

	prj, err := projectFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting project: %v", err)
	}

	instanceName, err := instanceNameFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting instance name: %v", err)
	}

	instanceZone, err := instanceZoneFromMetadata()
	if err != nil {
		return nil, fmt.Errorf("error getting instance zone: %v", err)
	}

	// netPrj refers to the project which owns the network being used
	// defaults to what is read by the metadata
	netPrj := prj
	// has the network project been provided?
	if v := os.Getenv(EnvGCENetworkProjectID); v != "" {
		netPrj = v
	}

	gn, err := cs.Networks.Get(netPrj, networkName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting network from compute service: %v", err)
	}

	gi, err := cs.Instances.Get(prj, instanceZone, instanceName).Do()
	if err != nil {
		return nil, fmt.Errorf("error getting instance from compute service: %v", err)
	}

	// if the instance project is different from the network project
	// we need to use the ip as the next hop when creating routes
	// cross project referencing is not allowed for instances
	useIPNextHop := prj != netPrj

	return &gceAPI{
		project:        netPrj,
		useIPNextHop:   useIPNextHop,
		computeService: cs,
		gceNetwork:     gn,
		gceInstance:    gi,
	}, nil
}

func (api *gceAPI) getRoute(subnet string) (*compute.Route, error) {
	routeName := formatRouteName(subnet)
	return api.computeService.Routes.Get(api.project, routeName).Do()
}

func (api *gceAPI) deleteRoute(subnet string) (*compute.Operation, error) {
	routeName := formatRouteName(subnet)
	return api.computeService.Routes.Delete(api.project, routeName).Do()
}

func (api *gceAPI) insertRoute(subnet string) (*compute.Operation, error) {
	log.Infof("Inserting route for subnet: %v", subnet)
	route := &compute.Route{
		Name:      formatRouteName(subnet),
		DestRange: subnet,
		Network:   api.gceNetwork.SelfLink,
		Priority:  1000,
		Tags:      []string{},
	}

	if api.useIPNextHop {
		if len(api.gceInstance.NetworkInterfaces) == 0 {
			return nil, fmt.Errorf("error expected instance=%v to have network interfaces",
				api.gceInstance.SelfLink)
		}

		route.NextHopIp = api.gceInstance.NetworkInterfaces[0].NetworkIP
	} else {
		route.NextHopInstance = api.gceInstance.SelfLink
	}

	return api.computeService.Routes.Insert(api.project, route).Do()
}

func (api *gceAPI) pollOperationStatus(operationName string) error {
	for i := 0; i < 100; i++ {
		operation, err := api.computeService.GlobalOperations.Get(api.project, operationName).Do()
		if err != nil {
			return fmt.Errorf("error fetching operation status: %v", err)
		}

		if operation.Error != nil {
			return fmt.Errorf("error running operation: %v", operation.Error)
		}

		if i%5 == 0 {
			log.Infof("%v operation status: %v waiting for completion...", operation.OperationType, operation.Status)
		}

		if operation.Status == "DONE" {
			return nil
		}
		time.Sleep(time.Second)
	}

	return fmt.Errorf("timeout waiting for operation to finish")
}

func formatRouteName(subnet string) string {
	return fmt.Sprintf("flannel-%s", replacer.Replace(subnet))
}
