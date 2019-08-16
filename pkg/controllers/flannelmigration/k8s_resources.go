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
	"fmt"
	"time"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// daemonset holds a collection of helper functions for Kubernetes daemonset.
type daemonset string

// Delete a daemonset and all dependents.
func (d daemonset) DeleteForeground(k8sClientset *kubernetes.Clientset, namespace string) error {
	policy := metav1.DeletePropagationForeground
	options := &metav1.DeleteOptions{
		PropagationPolicy: &policy,
	}
	return k8sClientset.AppsV1().DaemonSets(namespace).Delete(string(d), options)
}

// Check if daemonset exists or not.
// Return true if not exists.
func (d daemonset) CheckNotExists(k8sClientset *kubernetes.Clientset, namespace string) (bool, error) {
	_, err := k8sClientset.AppsV1().DaemonSets(namespace).Get(string(d), metav1.GetOptions{})
	if apierrs.IsNotFound(err) {
		return true, nil
	}

	return false, err
}

// Wait for daemonet to disappear.
func (d daemonset) WaitForDaemonsetNotFound(k8sClientset *kubernetes.Clientset, namespace string, interval, timeout time.Duration) error {
	return wait.PollImmediate(interval, timeout, func() (bool, error) {
		_, err := k8sClientset.AppsV1().DaemonSets(namespace).Get(string(d), metav1.GetOptions{})
		if apierrs.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		return false, nil
	})
}

// Add node selectors to daemonset.
// If node selectors has been set already, do nothing.
func (d daemonset) AddNodeSelector(k8sClientset *kubernetes.Clientset, namespace string, selector map[string]string) error {
	ds, err := k8sClientset.AppsV1().DaemonSets(namespace).Get(string(d), metav1.GetOptions{})
	if err != nil {
		return err
	}

	needUpdate := false
	for k, v := range selector {
		if currentVal, ok := ds.Spec.Template.Spec.NodeSelector[k]; ok && currentVal == v {
			continue
		}
		ds.Spec.Template.Spec.NodeSelector[k] = v
		needUpdate = true
	}

	if needUpdate {
		_, err = k8sClientset.AppsV1().DaemonSets(namespace).Update(ds)
		if err != nil {
			return err
		}
	}

	return nil
}

// Add node labels to node.
// If node labels has been set already, do nothing.
func addNodeLabels(k8sClientset *kubernetes.Clientset, node *v1.Node, labels map[string]string) error {
	needUpdate := false
	for k, v := range labels {
		if currentVal, ok := node.Labels[k]; ok && currentVal == v {
			continue
		}
		node.Labels[k] = v
		needUpdate = true
	}

	if needUpdate {
		_, err := k8sClientset.CoreV1().Nodes().Update(node)
		if err != nil {
			return err
		}
	}

	return nil
}

// Get value of a node label.
// If node does not have that label, return empty string and error.
func getNodeLabelValue(k8sClientset *kubernetes.Clientset, node *v1.Node, key string) (string, error) {
	currentVal, ok := node.Labels[key]
	if !ok {
		return "", fmt.Errorf("node label (%s) does not exists", key)
	}

	return currentVal, nil
}
