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
	"time"

	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	calicoNodeContainerName = "calico-node"
	calicoCniContainerName  = "install-cni"
	calicoCniConfigEnvName  = "CNI_CONF_NAME"

	// Sync period between kubelet and CNI config file change.
	// see https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/dockershim/network/cni/cni.go#L48
	defaultSyncConfigPeriod = time.Second * 5
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
	image, err := d.GetContainerImage(m.k8sClientset, namespaceKubeSystem, calicoNodeContainerName)
	if err != nil {
		return err
	}
	m.calicoImage = image

	// Set calico CNI config file name
	cniConf, err := d.GetContainerEnv(m.k8sClientset, namespaceKubeSystem, calicoCniContainerName, calicoCniConfigEnvName)
	if err != nil {
		return err
	}
	m.calicoCNIConfigName = cniConf

	log.Infof("Network migrator initialised, container image %s, CNI config %s/%s.", m.calicoImage, m.config.CNIConfigDir, m.calicoCNIConfigName)
	return nil
}

// Remove Flannel network device/routes on node.
// Write a dummy calico cni config file in front of Flannel/Canal CNI config.
// This will prevent Flannel CNI from running and make sure new pod created will not get networked
// until Calico CNI been correctly installed.
func (m *networkMigrator) removeFlannelNetworkAndInstallDummyCalicoCNI(node *v1.Node) error {
	// Deleting a tunnel device will remove routes, ARP and FDB entries related with the device.
	// Deleting cni0 device to remove routes to local pods.
	// It is possible tunnel device or cni0 has been deleted already.
	dummyCNI := `{ "name": "dummy", "plugins": [{ "type": "flannel-migration-in-progress" }]}`

	cmd := fmt.Sprintf("{ ip link show cni0; ip link show flannel.%d; } || exit 0 && { echo '%s' > /host/%s/%s ; ip link delete cni0 type bridge; ip link delete flannel.%d; } && exit 0 || exit 1",
		m.config.FlannelVNI, dummyCNI, m.config.CNIConfigDir, m.calicoCNIConfigName, m.config.FlannelVNI)

	// Run a remove-flannel pod with specified nodeName, this will bypass kube-scheduler.
	// https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#nodename
	pod := k8spod("remove-flannel")
	podLog, err := pod.RunPodOnNodeTillComplete(m.k8sClientset, namespaceKubeSystem, m.calicoImage, node.Name, cmd, m.config.CNIConfigDir, true, true)
	if podLog != "" {
		log.Infof("remove-flannel pod logs: %s.", podLog)
	}

	// Wait twice as long as default sync period so kubelet has picked up dummy CNI config.
	// We probably need something better here but currently no API available for us to know
	// if kubelet sees new config.
	time.Sleep(2 * defaultSyncConfigPeriod)
	return err
}

// Drain node, remove Flannel and setup Calico network for a node.
func (m *networkMigrator) setupCalicoNetworkForNode(node *v1.Node) error {
	log.Infof("Setting node label to disable Flannel daemonset pod on %s.", node.Name)
	// Set node label so no Flannel pod can be scheduled on this node.
	// Flannel pod currently running on the node starts to be evicted as the side effect.
	n := k8snode(node.Name)
	err := n.addNodeLabels(m.k8sClientset, nodeNetworkNone)
	if err != nil {
		log.WithError(err).Errorf("Error adding node label to disable Flannel network for node %s.", node.Name)
		return err
	}

	// Cordon and Drain node. Make sure no pod (except daemonset pod or pod with nodeName selector) can run on this node.
	// TODO

	// Wait for flannel pod to disappear before we proceed, otherwise it may reinstall Flannel network.
	//n.waitPodsDisappearForNode(m.k8sClientset, 1*time.Second, 2*time.Minute, func(pod *v1.Pod) bool {
	//	return true
	//})

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
	err = n.deletePodsForNode(m.k8sClientset, func(pod *v1.Pod) bool {
		return pod.Spec.HostNetwork == false
	})
	if err != nil {
		log.WithError(err).Errorf("failed to delete non-host-networked pods on node %s", node.Name)
		return err
	}

	// Set node label so that Calico daemonset pod start to run on this node.
	// This will install Calico CNI configuration file.
	// It will take the preference over Flannel CNI config or Canal CNI config.
	log.Infof("Setting node lable to enable Calico daemonset pod on %s.", node.Name)
	err = n.addNodeLabels(m.k8sClientset, nodeNetworkCalico)
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

		// TODO for debug purpose, migrate just one slave node.
		break
	}
	log.Infof("%d nodes completed network migration process.", len(nodes))

	return nil
}
