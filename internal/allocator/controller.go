// Copyright 2017 Google Inc.
// Copyright 2020 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package allocator

import (
	"net/url"

	v1 "k8s.io/api/core/v1"

	"purelb.io/internal/k8s"
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/go-kit/kit/log"
)

// Controller provides an event-handling interface for the k8s client
// to use.
type Controller interface {
	SetClient(*k8s.Client)
	SetConfig(*purelbv1.Config) k8s.SyncState
	SetBalancer(string, *v1.Service, *v1.Endpoints) k8s.SyncState
	DeleteBalancer(string) k8s.SyncState
	MarkSynced()
	Shutdown()
}

type controller struct {
	client   k8s.ServiceEvent
	synced   bool
	ips      *Allocator
	baseURL  *url.URL
	groupURL *string
	logger   log.Logger
}

// NewController configures a new controller. If error is non-nil then
// the controller object shouldn't be used.
func NewController(l log.Logger, ips *Allocator) (Controller, error) {
	con := &controller{
		logger: l,
		ips:    ips,
	}

	return con, nil
}

func (c *controller) SetClient(client *k8s.Client) {
	c.client = client
}

func (c *controller) DeleteBalancer(name string) k8s.SyncState {
	if c.ips.Unassign(name) {
		c.logger.Log("event", "serviceDeleted", "msg", "service deleted")
	}
	return k8s.SyncStateReprocessAll
}

func (c *controller) SetConfig(cfg *purelbv1.Config) k8s.SyncState {
	defer c.logger.Log("event", "configUpdated")

	if cfg == nil {
		c.logger.Log("op", "setConfig", "error", "no PureLB configuration in cluster", "msg", "configuration is missing, PureLB will not function")
		return k8s.SyncStateError
	}

	if err := c.ips.SetPools(cfg.Groups); err != nil {
		c.logger.Log("op", "setConfig", "error", err)
		return k8s.SyncStateError
	}

	// see if there's an EGW config. if so then we'll announce new
	// services to the EGW
	c.groupURL = nil
	c.baseURL = nil
	for _, group := range cfg.Groups {
		if group.Spec.EGW != nil {
			c.groupURL = &group.Spec.EGW.URL
			// Use the hostname from the service group, but reset the path.  EGW
			// and Netbox each have their own API URL schemes so we only need
			// the protocol, host, port, credentials, etc.
			url, err := url.Parse(*c.groupURL)
			if err != nil {
				c.logger.Log("op", "setConfig", "error", err)
				return k8s.SyncStateError
			}
			url.Path = ""
			c.baseURL = url
		}
	}

	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced() {
	c.synced = true
	c.logger.Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}

func (c *controller) Shutdown() {
	c.logger.Log("event", "shutdown")
}