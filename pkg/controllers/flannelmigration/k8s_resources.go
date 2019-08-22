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
	"os/exec"
	"time"

	"k8s.io/apimachinery/pkg/fields"

	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	log "github.com/sirupsen/logrus"
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

// Get spec of a container from a daemonset.
func (d daemonset) getContainerSpec(k8sClientset *kubernetes.Clientset, namespace, containerName string) (*v1.Container, error) {
	ds, err := k8sClientset.AppsV1().DaemonSets(namespace).Get(string(d), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	for _, c := range ds.Spec.Template.Spec.InitContainers {
		if c.Name == containerName {
			return &c, nil
		}
	}

	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("No container with name %s found in daemonset", containerName)
}

// Get container image from a container spec.
func (d daemonset) GetContainerImage(k8sClientset *kubernetes.Clientset, namespace, containerName string) (string, error) {
	container, err := d.getContainerSpec(k8sClientset, namespace, containerName)
	if err != nil {
		return "", err
	}

	return container.Image, nil
}

// Get container env value.
func (d daemonset) GetContainerEnv(k8sClientset *kubernetes.Clientset, namespace, containerName, envName string) (string, error) {
	container, err := d.getContainerSpec(k8sClientset, namespace, containerName)
	if err != nil {
		return "", err
	}

	env := container.Env
	for _, e := range env {
		if e.Name == envName {
			return e.Value, nil
		}
	}

	return "", fmt.Errorf("No Env with name %s found in container %s", envName, containerName)
}

// Wait for daemonset to disappear.
func (d daemonset) WaitForDaemonsetNotFound(k8sClientset *kubernetes.Clientset, namespace string, interval, timeout time.Duration) error {
	return wait.PollImmediate(interval, timeout, func() (bool, error) {
		_, err := k8sClientset.AppsV1().DaemonSets(namespace).Get(string(d), metav1.GetOptions{})
		if apierrs.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		// Daemoset still exists, retry.
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

// k8spod holds a collection of helper functions for Kubernetes pod.
type k8spod string

// Run a pod with a host path volume on specified node. Wait till it is completed.
// Return error with log if for any reason pod not completed successfully.
func (p k8spod) RunPodOnNodeTillComplete(k8sClientset *kubernetes.Clientset, namespace, imageName, nodeName, shellCmd, hostPath string, privileged, hostNetwork bool) (string, error) {
	podName := string(p)
	containerName := podName
	hostPathDirectory := v1.HostPathDirectory

	log.Infof("Create pod on node %s to run [ %s ].", nodeName, shellCmd)
	podSpec := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: podName + "-",
			Labels: map[string]string{
				"flannel-migration": nodeName,
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    containerName,
					Image:   imageName,
					Command: []string{"/bin/sh"},
					Args: []string{
						"-c",
						shellCmd,
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "host-dir",
							MountPath: fmt.Sprintf("/host/%s", hostPath),
						},
					},
					SecurityContext: &v1.SecurityContext{Privileged: &privileged},
				},
			},
			HostNetwork:   hostNetwork,
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "host-dir",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: hostPath,
							Type: &hostPathDirectory,
						},
					},
				},
			},
		},
	}

	logs := ""
	pod, err := k8sClientset.CoreV1().Pods(namespace).Create(podSpec)
	if err != nil {
		return logs, err
	}

	err = waitForPodSuccessTimeout(k8sClientset, pod.Name, pod.Namespace, 1*time.Second, 5*time.Minute)
	if err != nil {
		// Trying to get pod log on error.
		logs, _ = getPodContainerLog(k8sClientset, namespace, pod.Name, containerName)
	}

	// Delete pod if everything is fine.
	err = k8sClientset.CoreV1().Pods(namespace).Delete(pod.Name, &metav1.DeleteOptions{})
	if err != nil {
		return logs, err
	}

	return logs, nil
}

// Get Pod logs
func getPodContainerLog(k8sClientSet *kubernetes.Clientset, namespace, podName, containerName string) (string, error) {
	podLog, err := k8sClientSet.CoreV1().RESTClient().Get().
		Resource("pods").
		Namespace(namespace).
		Name(podName).SubResource("log").
		Param("container", containerName).
		Param("previous", "false").
		Do().
		Raw()
	if err != nil {
		log.Errorf("failed to get pod log with error %+v", err)
		return "", err
	}
	return string(podLog), err
}

// waitForPodSuccessTimeout returns nil if the pod reached state success, or an error if it reached failure or ran too long.
func waitForPodSuccessTimeout(k8sClientset *kubernetes.Clientset, podName, namespace string, interval, timeout time.Duration) error {
	return wait.PollImmediate(interval, timeout, func() (bool, error) {
		pod, err := k8sClientset.CoreV1().Pods(namespace).Get(podName, metav1.GetOptions{})
		if err != nil {
			// Cannot get pod yet, retry.
			return false, err
		}
		if pod.Status.Phase == v1.PodSucceeded {
			return true, nil
		}
		if pod.Status.Phase == v1.PodFailed {
			return true, fmt.Errorf("pod %s completed with failed status: %+v", podName, pod.Status)
		}
		// None of above, retry.
		return false, nil
	})
}

// k8snode holds a collection of helper functions for Kubernetes node.
type k8snode string

// Add node labels to node. Perform Get/Check/Update so that it always working on latest version.
// If node labels has been set already, do nothing.
func (n k8snode) addNodeLabels(k8sClientset *kubernetes.Clientset, labels map[string]string) error {
	nodeName := string(n)
	node, err := k8sClientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

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

// Start deletion process for pods on a node which satisfy a filter.
func (n k8snode) deletePodsForNode(k8sClientset *kubernetes.Clientset, filter func(pod *v1.Pod) bool) error {
	nodeName := string(n)
	podList, err := k8sClientset.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}).String()})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		if filter(&pod) {
			err = k8sClientset.CoreV1().Pods(pod.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
			if err != nil && !apierrs.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

// Start deletion process for pods on a node which satisfy a filter.
func (n k8snode) waitPodsDisappearForNode(k8sClientset *kubernetes.Clientset, interval, timeout time.Duration, filter func(pod *v1.Pod) bool) error {
	return wait.PollImmediate(interval, timeout, func() (bool, error) {
		nodeName := string(n)
		podList, err := k8sClientset.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{
			FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}).String()})
		if err != nil {
			// Something wrong, stop waiting
			return true, err
		}

		for _, pod := range podList.Items {
			if filter(&pod) {
				// Pod is there, retry.
				return false, nil
			}
		}
		// Pod gone, stop waiting
		return true, nil
	})

	return nil
}

func (n k8snode) Drain() error {
	nodeName := string(n)
	log.Infof("Start drain node %s", nodeName)
	out, err := exec.Command("/usr/bin/kubectl", "drain",
		"--ignore-daemonsets", "--delete-local-data", "--force", nodeName).CombinedOutput()
	if err != nil {
		log.Errorf("Drain node %s. \n ---Drain Node--- \n %s \n ------", nodeName, string(out))
		return err
	}

	log.Infof("Drain node %s completed successfully. \n ---Drain Node Logs--- \n %s \n ------", nodeName, string(out))
	return nil
}

func (n k8snode) Uncordon() error {
	nodeName := string(n)
	log.Infof("Start uncordon node %s", nodeName)
	out, err := exec.Command("/usr/bin/kubectl", "uncordon", nodeName).CombinedOutput()
	if err != nil {
		log.Errorf("Uncordon node %s. \n ---Uncordon Node Logs--- \n %s \n ------", nodeName, string(out))
		return err
	}

	log.Infof("Uncordon node %s completed successfully.", nodeName)
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
