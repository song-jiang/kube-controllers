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
func (n *networkMigrator) Initialise() error {
	// Set calico image
	d := daemonset(n.config.CalicoDaemonsetName)
	image, err := d.GetContainerImage(n.k8sClientset, NAMESPACE_KUBE_SYSTEM, CALICO_NODE_CONTAINER_NAME)
	if err != nil {
		return err
	}
	n.calicoImage = image

	// Set calico CNI config file name
	cniConf, err := d.GetContainerEnv(n.k8sClientset, NAMESPACE_KUBE_SYSTEM, CALICO_CNI_CONTAINER_NAME, CALICO_CNI_CONFIG_ENV_NAME)
	if err != nil {
		return err
	}
	n.calicoCNIConfigName = cniConf

	return nil
}

// Delete all pods which is not host networked on node.
func (n *networkMigrator) DeleteNonHostNetworkedPodsOnNode(node *v1.Node) error {
	return deletePodsForNode(n.k8sClientset, node.Name, func(pod *v1.Pod) bool {
		return pod.Spec.HostNetwork == false
	})
}

// Remove Flannel network device/routes on node.
// Write a dummy calico cni config file in front of Flannel/Canal CNI config.
// This will prevent Flannel CNI from running and make sure new pod created will not get networked
// until Calico CNI been correctly installed.
func (n *networkMigrator) RemoveFlannelNetworkAndInstallDummyCalicoCNI(node *v1.Node) error {
	// Run a remove-flannel pod with specified nodeName, this will bypass kube-scheduler.
	// https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#nodename

	// Deleting a tunnel device will remove routes, APR and FDB entries related with the device.
	// It is possible tunnel device has been deleted already.
	cmd := fmt.Sprintf("ip link show flannel.%d || exit 0 && echo dummy-cni > /host/%s/%s ; ip link delete flannel.%d && exit 0 || exit 1",
		n.config.FlannelVNI, n.config.CNIConfigDir, n.calicoCNIConfigName, n.config.FlannelVNI)
	pod := k8spod("remove-flannel")
	return pod.RunPodOnNodeTillComplete(n.k8sClientset, NAMESPACE_KUBE_SYSTEM, n.calicoImage, node.Name, cmd, n.config.CNIConfigDir, true)
}

// Drain node, remove Flannel and setup Calico network for a node.
func (n *networkMigrator) SetupCalicoNetworkForNode(node *v1.Node) error {
	// Set node label so no Flannel pod can be scheduled on this node.
	// Flannel pod currently running on the node starts to be evicted as the side effect.
	err := addNodeLabels(n.k8sClientset, node, nodeNetworkUnknown)
	if err != nil {
		log.WithError(err).Errorf("Error adding node label to disable Flannel network for node %s.", node.Name)
		return err
	}

	// Cordon and Drain node. Make sure no pod (except daemonset pod or pod with nodeName selector) can run on this node.
	// TODO

	// Remove Flannel network from node.
	// Note Flannel vxlan tunnel device (flannel.1) is created by Flannel daemonset pod (not Flannel CNI)
	// Therefor if Flannel daemonset pod can not run on this node, the tunnel device will not be recreated
	// after we delete it.
	err = n.RemoveFlannelNetworkAndInstallDummyCalicoCNI(node)
	if err != nil {
		log.WithError(err).Errorf("failed to remove flannel network on node %s", node.Name)
	}

	// Delete all pods on the node which is not host networked.
	err = n.DeleteNonHostNetworkedPodsOnNode(node)
	if err != nil {
		log.WithError(err).Errorf("failed to delete non-host-networked pods on node %s", node.Name)
	}

	// Set node label so that Calico node pod start to run on this node.
	// This will install Calico CNI configuration file.
	// It will take the preference over Flannel CNI config or Canal CNI config.
	err = addNodeLabels(n.k8sClientset, node, nodeNetworkCalico)
	if err != nil {
		log.WithError(err).Errorf("Error adding node label to enable Calico network for node %s.", node.Name)
		return err
	}

	// Calico daemonset pod should be running now.
	// TODO

	// Uncordon node.
	//TODO


	return nil
}
