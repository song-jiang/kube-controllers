// Copyright (c) 2019 Tigera, Inc. All rights reserved.
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

package flannelmigration

import (
	"context"

	"k8s.io/client-go/kubernetes"

	client "github.com/projectcalico/libcalico-go/lib/clientv3"
)

// networkMigrator responsible for migrating Flannel vxlan data plane to Calico vxlan data plane.
type networkMigrator struct {
	ctx          context.Context
	calicoClient client.Interface
	k8sClientset *kubernetes.Clientset
	config       *Config
}

func NewNetworkMigrator(ctx context.Context, k8sClientset *kubernetes.Clientset, calicoClient client.Interface, config *Config) networkMigrator {
	return networkMigrator{
		ctx:          ctx,
		calicoClient: calicoClient,
		k8sClientset: k8sClientset,
		config:       config,
	}
}
