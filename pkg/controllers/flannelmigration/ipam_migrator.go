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
	"encoding/json"
	"fmt"

	"github.com/projectcalico/libcalico-go/lib/ipam"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	FLANNEL_NODE_ANNOTATION_KEY_BACKEND_DATA = "backend-data"
	FLANNEL_NODE_ANNOTATION_KEY_BACKEND_TYPE = "backend-type"
	DEFAULT_IPV4_POOL_NAME                   = "default-ipv4-ippool"
	DEFAULT_FELIX_CONFIGURATION_NAME         = "default"
)

// IPAMMigrator responsible for migrating host-local IPAM to Calico IPAM.
// It also converts Flannel vxlan setup for each hosts to Calico vxlan setup.
// IPAM migration process should be idempotent. It can be restarted and still be able to
// complete the process.
type ipamMigrator struct {
	ctx          context.Context
	calicoClient client.Interface
	k8sClientset *kubernetes.Clientset
	config       *Config
}

func NewIPAMMigrator(ctx context.Context, k8sClientset *kubernetes.Clientset, calicoClient client.Interface, config *Config) ipamMigrator {
	return ipamMigrator{
		ctx:          ctx,
		calicoClient: calicoClient,
		k8sClientset: k8sClientset,
		config:       config,
	}
}

// Create and initialise default Calico IPPool if not exists.
// Update default FelixConfiguration with Flannel VNI and vxlan port.
func (m ipamMigrator) InitialiseIPPoolAndFelixConfig() error {
	// Validate config and get pod CIDR.
	_, cidr, err := cnet.ParseCIDR(m.config.FlannelNetwork)
	if err != nil {
		return fmt.Errorf("Failed to parse the CIDR '%s'", m.config.FlannelNetwork)
	}

	// Creating default ippool with vxlan enabled also creating a global felix configuration.
	err = createDefaultVxlanIPPool(m.ctx, m.calicoClient, cidr, m.config.FlannelIPMasq)
	if err != nil {
		return fmt.Errorf("Failed to create default ippool")
	}

	// Update default Felix configuration with Flannel VNI and vxlan port.
	err = updateDefaultFelixConfigurtion(m.ctx, m.calicoClient, m.config.FlannelVNI, m.config.FlannelPort, m.config.FlannelMTU)
	if err != nil {
		return fmt.Errorf("Failed to create default ippool")
	}

	return nil
}

// Create Calico IPAM blocks for a Kubernetes node.
func (m ipamMigrator) SetupCalicoIPAMForNode(node *v1.Node) error {
	log.Infof("Start setting up Calico IPAM for node %s.", node.Name)

	if node == nil {
		return fmt.Errorf("nil pointer for node")
	}

	// Get podCIDR for node.
	if node.Spec.PodCIDR == "" {
		return fmt.Errorf("node %s pod cidr not assigned", node.Name)
	}

	// Get first IP address which is used by Flannel as vtep IP.
	vtepIP, cidr, err := cnet.ParseCIDR(node.Spec.PodCIDR)
	if err != nil {
		return err
	}

	// Get Flannel vxlan setup from node annotations. An example is
	// flannel.alpha.coreos.com/backend-data: '{"VtepMAC":"56:1d:8d:30:79:97"}'
	// flannel.alpha.coreos.com/backend-type: vxlan
	backendType, ok := node.Annotations[m.config.FlannelAnnotationPreifx+"/"+FLANNEL_NODE_ANNOTATION_KEY_BACKEND_TYPE]
	if !ok {
		return fmt.Errorf("node %s missing annotation for Flannel backend type", node.Name)
	}
	if backendType != "vxlan" {
		return fmt.Errorf("node %s got wrong Flannel backend type %s", node.Name, backendType)
	}

	backendData, ok := node.Annotations[m.config.FlannelAnnotationPreifx+"/"+FLANNEL_NODE_ANNOTATION_KEY_BACKEND_DATA]
	if !ok {
		return fmt.Errorf("node %s missing annotation for Flannel backend data", node.Name)
	}

	type flannelVtepMac struct {
		VtepMAC string
	}
	var fvm flannelVtepMac
	err = json.Unmarshal([]byte(backendData), &fvm)
	if err != nil {
		return fmt.Errorf("node %s got wrong Flannel backend data %s", node.Name, backendData)
	}

	vtepMac := fvm.VtepMAC
	log.Infof("node %s has vxlan setup from Flannel (vtepMac: '%s', vtepIP: '%s').", node.Name, vtepMac, vtepIP.String())

	// Allocate Calico IPAM blocks for node.
	claimed, failed, err := m.calicoClient.IPAM().ClaimAffinity(m.ctx, *cidr, node.Name)
	if err != nil {
		log.WithError(err).Errorf("Failed to claim IPAM blocks for node %s, claimed %d, failed %d", node.Name, len(claimed), len(failed))
		return err
	}
	log.Infof("%d IPAM blocks claimed for node %s.", len(claimed), node.Name)

	// Update Calico node with Flannel vtep IP/Mac.
	err = setupCalicoNodeVxlan(m.ctx, m.calicoClient, node.Name, *vtepIP, vtepMac)
	if err != nil {
		return err
	}

	log.Infof("Setting up Calico IPAM for node %s completed successfully.", node.Name)
	return nil
}

// MigrateNodes setup Calico IPAM for array of nodes.
func (m ipamMigrator) MigrateNodes(nodes []*v1.Node) error {
	log.Infof("Start IPAM migration process for %d nodes.", len(nodes))
	for _, n := range nodes {
		err := m.SetupCalicoIPAMForNode(n)
		if err != nil {
			return err
		}
	}
	log.Infof("%d nodes completed IPAM migration process.", len(nodes))

	return nil
}

// setupCalicoNodeVxlan assigns specified IP/Mac address as vtep IP/Mac address for Calico node.
func setupCalicoNodeVxlan(ctx context.Context, c client.Interface, nodeName string, ip cnet.IP, mac string) error {
	log.Infof("Updating Calico Node %s with vtep IP %s, Mac %s.", nodeName, ip.String(), mac)

	// Assign vtep IP.
	// Check current status of vtep IP. It could be assigned already.
	assign := true
	attr, err := c.IPAM().GetAssignmentAttributes(ctx, ip)
	if err == nil {
		if attr[ipam.AttributeType] == ipam.AttributeTypeVXLAN && attr[ipam.AttributeNode] == nodeName {
			// The tunnel address is still valid, do nothing.
			log.Infof("Calico Node %s vtep IP been assigned already.", nodeName)
			assign = false
		} else {
			// The tunnel address has been allocated to something else, return error.
			return fmt.Errorf("vtep IP %s has been occupied", ip.String())
		}
	} else if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
		// The tunnel address is not assigned, assign it.
		log.WithField("vtepIP", ip.String()).Info("assign a new vtep IP")
	} else {
		// Failed to get assignment attributes, datastore connection issues possible.
		log.WithError(err).Errorf("Failed to get assignment attributes for vtep IP '%s'", ip.String())
		return fmt.Errorf("Failed to get vtep IP %s attribute", ip.String())
	}

	if assign {
		// Build attributes and handle for this allocation.
		attrs := map[string]string{ipam.AttributeNode: nodeName}
		attrs[ipam.AttributeType] = ipam.AttributeTypeVXLAN
		handle := fmt.Sprintf("vxlan-tunnel-addr-%s", nodeName)

		err := c.IPAM().AssignIP(ctx, ipam.AssignIPArgs{
			IP:       ip,
			Hostname: nodeName,
			HandleID: &handle,
			Attrs:    attrs,
		})
		if err != nil {
			return fmt.Errorf("Failed to assign vtep IP %s", ip.String())
		}
		log.Infof("Calico Node %s vtep IP assigned.", nodeName)
	}

	// Update Calico node with vtep IP/Mac
	node, err := c.Nodes().Get(ctx, nodeName, options.GetOptions{})
	if err != nil {
		return err
	}

	// If node has correct vxlan setup, do nothing.
	if node.Spec.IPv4VXLANTunnelAddr == ip.String() && node.Spec.VXLANTunnelMACAddr == mac {
		return nil
	}

	node.Spec.IPv4VXLANTunnelAddr = ip.String()
	node.Spec.VXLANTunnelMACAddr = mac
	_, err = c.Nodes().Update(ctx, node, options.SetOptions{})
	if err != nil {
		return err
	}

	log.Infof("Calico Node %s vtep IP/Mac updated.", nodeName)
	return nil
}

// createIPPool creates an IP pool using the specified CIDR.
// If the pool already exists, normally this indicates migration controller restarted, check if it is a still valid pool we can use.
func createDefaultVxlanIPPool(ctx context.Context, client client.Interface, cidr *cnet.IPNet, isNATOutgoingEnabled bool) error {
	// TODO: need to set the size for IPAM block based on nodePodCIDR. default /26
	pool := &api.IPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: DEFAULT_IPV4_POOL_NAME,
		},
		Spec: api.IPPoolSpec{
			CIDR:        cidr.String(),
			NATOutgoing: isNATOutgoingEnabled,
			IPIPMode:    api.IPIPModeNever,
			VXLANMode:   api.VXLANModeAlways,
		},
	}

	log.Infof("Ensure default IPv4 pool is created with VXLAN.")

	// Create the pool.
	// Validate if pool already exists.
	if _, err := client.IPPools().Create(ctx, pool, options.SetOptions{}); err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); !ok {
			log.WithError(err).Errorf("Failed to create default IPv4 pool (%s)", cidr.String())
			return err
		} else {
			defaultPool, err := client.IPPools().Get(ctx, DEFAULT_IPV4_POOL_NAME, options.GetOptions{})
			if err != nil {
				log.WithError(err).Errorf("Failed to get existing default IPv4 IP pool")
				return err
			}

			// Check if existing ip pool is valid.
			if defaultPool.Spec.CIDR != cidr.String() {
				log.Errorf("Failed to validate existing default IPv4 IP pool (old CIDR: %s, new CIDR %s)", defaultPool.Spec.CIDR, cidr.String())
				return cerrors.ErrorValidation{
					ErroredFields: []cerrors.ErroredField{{
						Name:   "Spec.CIDR",
						Reason: "Found existing default ippool with different pod CIDR",
					}},
				}
			}
			if defaultPool.Spec.NATOutgoing != isNATOutgoingEnabled {
				log.Errorf("Failed to validate existing default IPv4 IP pool (old NAT outgoing: %s, new NAT outgoing %s)", defaultPool.Spec.NATOutgoing, isNATOutgoingEnabled)
				return cerrors.ErrorValidation{
					ErroredFields: []cerrors.ErroredField{{
						Name:   "Spec.NATOutgoing",
						Reason: "Found existing default ippool with different NATOutgoing",
					}},
				}
			}
			log.Infof("Use existing default IPv4 pool (%s) with VXLAN and NAT outgoing %t.", cidr, isNATOutgoingEnabled)
		}
	} else {
		log.Infof("Created default IPv4 pool (%s) with VXLAN and NAT outgoing %t.", cidr, isNATOutgoingEnabled)
	}

	return nil
}

// Update default FelixConfiguration with specified VNI, port and MTU.
// Do nothing if correct values already been set.
// Return error if default FelixConfiguration not exists or vxlan is not enabled.
func updateDefaultFelixConfigurtion(ctx context.Context, client client.Interface, vni, port, mtu int) error {
	// Get default Felix configuration. Return error if not exists.
	defaultConfig, err := client.FelixConfigurations().Get(ctx, DEFAULT_FELIX_CONFIGURATION_NAME, options.GetOptions{})
	if err != nil {
		log.WithError(err).Errorf("Error getting default FelixConfiguration resource")
		return err
	}

	// Check if vxlan is enabled. Return error is not.
	vxlanEnabled := false
	if defaultConfig.Spec.VXLANEnabled != nil {
		vxlanEnabled = *defaultConfig.Spec.VXLANEnabled
	}
	if !vxlanEnabled {
		log.WithError(err).Errorf("vxlan is not enabled by default Felix configration")
		return err
	}

	// Get current value for VNI , Port and MTU.
	currentVNI := 0
	if defaultConfig.Spec.VXLANVNI != nil {
		currentVNI = *defaultConfig.Spec.VXLANVNI
	}
	currentPort := 0
	if defaultConfig.Spec.VXLANPort != nil {
		currentPort = *defaultConfig.Spec.VXLANPort
	}
	currentMTU := 0
	if defaultConfig.Spec.VXLANMTU != nil {
		currentMTU = *defaultConfig.Spec.VXLANMTU
	}

	// Do nothing if the correct value has been set.
	if currentVNI == vni && currentPort == port && currentMTU == mtu {
		log.Infof("Default Felix configration got correct VNI(%d), port(%d), mtu(%d).", currentVNI, currentPort, currentMTU)
		return nil
	}

	defaultConfig.Spec.VXLANVNI = &vni
	defaultConfig.Spec.VXLANPort = &port
	defaultConfig.Spec.VXLANMTU = &mtu
	_, err = client.FelixConfigurations().Update(ctx, defaultConfig, options.SetOptions{})
	if err != nil {
		// Migration controller should be the single source of updating FelixConfiguration.
		// If an update conflict occur, it means there is a race between two migration controllers or
		// between migration controller and a calico node. In both cases, something is wrong.
		if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
			log.Errorf("default FelixConfiguration update conflict - something wrong.")
			return err
		}
		log.WithError(err).Errorf("Failed to update default FelixConfiguration.")
		return err
	}

	log.Info("default FelixConfiguration updated successfully")
	return nil
}
