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
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	log "github.com/sirupsen/logrus"
)

const (
	CALICO_NODE_CONTAINER_NAME = "calico-node"
	CALICO_CNI_CONTAINER_NAME  = "install-cni"
	CALICO_CNI_CONFIG_ENV_NAME = "CNI_CONF_NAME"
)

// networkMigrator responsible for migrating Flannel vxlan data plane to Calico vxlan data plane.
type networkMigrator struct {
	ctx          context.Context
	calicoClient client.Interface
	k8sClientset *kubernetes.Clientset
	config       *Config

	// Calico node image used by network migrator.
	calicoImage string

	// Calico CNI config file name
	calicoCNIConfigName string
}

func NewNetworkMigrator(ctx context.Context, k8sClientset *kubernetes.Clientset, calicoClient client.Interface, config *Config) *networkMigrator {
	return &networkMigrator{
		ctx:          ctx,
		calicoClient: calicoClient,
		k8sClientset: k8sClientset,
		config:       config,
	}
}

// Initialise network migrator.
func (m *networkMigrator) Initialise() error {
	// Set calico image
	d := daemonset(m.config.CalicoDaemonsetName)
	image, err := d.GetContainerImage(m.k8sClientset, NAMESPACE_KUBE_SYSTEM, CALICO_NODE_CONTAINER_NAME)
	if err != nil {
		return err
	}
	m.calicoImage = image

	// Set calico CNI config file name
	cniConf, err := d.GetContainerEnv(m.k8sClientset, NAMESPACE_KUBE_SYSTEM, CALICO_CNI_CONTAINER_NAME, CALICO_CNI_CONFIG_ENV_NAME)
	if err != nil {
		return err
	}
	m.calicoCNIConfigName = cniConf

	return nil
}

// Delete all pods which is not host networked on node.
func (m *networkMigrator) deleteNonHostNetworkedPodsOnNode(node *v1.Node) error {
	return deletePodsForNode(m.k8sClientset, node.Name, func(pod *v1.Pod) bool {
		return pod.Spec.HostNetwork == false
	})
}

// Remove Flannel network device/routes on node.
// Write a dummy calico cni config file in front of Flannel/Canal CNI config.
// This will prevent Flannel CNI from running and make sure new pod created will not get networked
// until Calico CNI been correctly installed.
func (m *networkMigrator) removeFlannelNetworkAndInstallDummyCalicoCNI(node *v1.Node) error {
	// Run a remove-flannel pod with specified nodeName, this will bypass kube-scheduler.
	// https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#nodename

	// Deleting a tunnel device will remove routes, APR and FDB entries related with the device.
	// It is possible tunnel device has been deleted already.
	cmd := fmt.Sprintf("ip link show flannel.%d || exit 0 && echo dummy-cni > /host/%s/%s ; ip link delete flannel.%d && exit 0 || exit 1",
		m.config.FlannelVNI, m.config.CNIConfigDir, m.calicoCNIConfigName, m.config.FlannelVNI)
	pod := k8spod("remove-flannel")
	return pod.RunPodOnNodeTillComplete(m.k8sClientset, NAMESPACE_KUBE_SYSTEM, m.calicoImage, node.Name, cmd, m.config.CNIConfigDir, true)
}

// Drain node, remove Flannel and setup Calico network for a node.
func (m *networkMigrator) setupCalicoNetworkForNode(node *v1.Node) error {
	log.Infof("Setting node label to disable Flannel daemonset pod on %d.", node.Name)
	// Set node label so no Flannel pod can be scheduled on this node.
	// Flannel pod currently running on the node starts to be evicted as the side effect.
	err := addNodeLabels(m.k8sClientset, node, nodeNetworkUnknown)
	if err != nil {
		log.WithError(err).Errorf("Error adding node label to disable Flannel network for node %s.", node.Name)
		return err
	}

	// Cordon and Drain node. Make sure no pod (except daemonset pod or pod with nodeName selector) can run on this node.
	// TODO

	log.Infof("Removing flannel tunnel device/routes on %s.", node.Name)
	// Remove Flannel network from node.
	// Note Flannel vxlan tunnel device (flannel.1) is created by Flannel daemonset pod (not Flannel CNI)
	// Therefor if Flannel daemonset pod can not run on this node, the tunnel device will not be recreated
	// after we delete it.
	err = m.removeFlannelNetworkAndInstallDummyCalicoCNI(node)
	if err != nil {
		log.WithError(err).Errorf("failed to remove flannel network on node %s", node.Name)
		return err
	}

	log.Infof("Deleting non-host-networked pods on %s.", node.Name)
	// Delete all pods on the node which is not host networked.
	err = m.deleteNonHostNetworkedPodsOnNode(node)
	if err != nil {
		log.WithError(err).Errorf("failed to delete non-host-networked pods on node %s", node.Name)
		return err
	}

	// Set node label so that Calico daemonset pod start to run on this node.
	// This will install Calico CNI configuration file.
	// It will take the preference over Flannel CNI config or Canal CNI config.
	log.Infof("Setting node lable to enable Calico daemonset pod on %s.", node.Name)
	err = addNodeLabels(m.k8sClientset, node, nodeNetworkCalico)
	if err != nil {
		log.WithError(err).Errorf("Error adding node label to enable Calico network for node %s.", node.Name)
		return err
	}

	// Calico daemonset pod should start running now.
	// TODO do we need to wait for calico node pod ready?

	// Uncordon node.
	//TODO

	log.Infof("Calico networking is running on %s.", node.Name)
	return nil
}

// MigrateNodes setup Calico network for array of nodes.
func (m *networkMigrator) MigrateNodes(nodes []*v1.Node) error {
	log.Infof("Start network migration process for %d nodes.", len(nodes))
	for _, node := range nodes {
		err := m.setupCalicoNetworkForNode(node)
		if err != nil {
			return err
		}
	}
	log.Infof("%d nodes completed network migration process.", len(nodes))

	return nil
}
