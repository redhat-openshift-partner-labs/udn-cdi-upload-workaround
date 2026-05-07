package goldenimage

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	serverPodName = "mcs-image-server"
	serverSvcName = "mcs-image-server"
	serverPort    int32 = 80
)

// DataVolume phase constants (matching CDI)
const (
	DVPhaseSucceeded = "Succeeded"
	DVPhaseFailed    = "Failed"
)

// GVRs for dynamic client operations
var (
	dataVolumeGVR = schema.GroupVersionResource{
		Group:    "cdi.kubevirt.io",
		Version:  "v1beta1",
		Resource: "datavolumes",
	}
	cudnGVR = schema.GroupVersionResource{
		Group:    "k8s.ovn.org",
		Version:  "v1",
		Resource: "clusteruserdefinednetworks",
	}
	udnGVR = schema.GroupVersionResource{
		Group:    "k8s.ovn.org",
		Version:  "v1",
		Resource: "userdefinednetworks",
	}
)

// GoldenImageUploader handles golden image uploads to namespaces,
// automatically detecting and handling Primary UDN configurations.
type GoldenImageUploader struct {
	k8sClient     kubernetes.Interface
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
	namespace     string
	pvcName       string
	pvcSize       string
	storageClass  string
}

// NewGoldenImageUploader creates a new uploader instance with all required clients.
func NewGoldenImageUploader(
	restConfig *rest.Config,
	namespace string,
	pvcName string,
	pvcSize string,
	storageClass string,
) (*GoldenImageUploader, error) {
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return &GoldenImageUploader{
		k8sClient:     k8sClient,
		dynamicClient: dynamicClient,
		restConfig:    restConfig,
		namespace:     namespace,
		pvcName:       pvcName,
		pvcSize:       pvcSize,
		storageClass:  storageClass,
	}, nil
}

// Upload handles the complete golden image upload workflow.
// Automatically detects if namespace uses Primary UDN and selects appropriate method.
func (u *GoldenImageUploader) Upload(ctx context.Context, localImagePath string) error {
	// Validate local file exists
	if _, err := os.Stat(localImagePath); err != nil {
		return fmt.Errorf("local image not found: %w", err)
	}

	// Detect if namespace uses Primary UDN
	hasUDN, err := u.namespaceHasPrimaryUDN(ctx)
	if err != nil {
		return fmt.Errorf("detecting UDN: %w", err)
	}

	if hasUDN {
		fmt.Printf("Detected Primary UDN in namespace %s, using HTTP source workflow\n", u.namespace)
		return u.uploadViaHTTPSource(ctx, localImagePath)
	}

	fmt.Printf("No Primary UDN detected in namespace %s, using standard upload flow\n", u.namespace)
	return u.uploadViaProxy(ctx, localImagePath)
}

// namespaceHasPrimaryUDN checks if the target namespace uses a Primary User-Defined Network.
func (u *GoldenImageUploader) namespaceHasPrimaryUDN(ctx context.Context) (bool, error) {
	// Get namespace
	ns, err := u.k8sClient.CoreV1().Namespaces().Get(ctx, u.namespace, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("getting namespace: %w", err)
	}

	// Check for UDN label (required for UDN-enabled namespaces)
	nsLabels := ns.GetLabels()
	if _, exists := nsLabels["k8s.ovn.org/primary-user-defined-network"]; !exists {
		return false, nil
	}

	// Check ClusterUserDefinedNetworks
	cudnList, err := u.dynamicClient.Resource(cudnGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		// CRD may not exist - not an error, just no CUDNs
		if k8serrors.IsNotFound(err) {
			return u.checkNamespaceScopedUDN(ctx)
		}
		return false, fmt.Errorf("listing CUDNs: %w", err)
	}

	for _, cudn := range cudnList.Items {
		spec, ok := cudn.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}

		// Check if this CUDN has Primary role
		if !u.hasPrimaryRole(spec) {
			continue
		}

		// Check if namespaceSelector matches target namespace
		if u.selectorMatchesNamespace(spec, nsLabels) {
			return true, nil
		}
	}

	// Check namespace-scoped UDNs
	return u.checkNamespaceScopedUDN(ctx)
}

// checkNamespaceScopedUDN checks for UserDefinedNetwork resources in the namespace.
func (u *GoldenImageUploader) checkNamespaceScopedUDN(ctx context.Context) (bool, error) {
	udnList, err := u.dynamicClient.Resource(udnGVR).Namespace(u.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("listing UDNs: %w", err)
	}

	for _, udn := range udnList.Items {
		spec, ok := udn.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}

		if u.hasPrimaryRole(spec) {
			return true, nil
		}
	}

	return false, nil
}

// hasPrimaryRole checks if a UDN/CUDN spec has role: Primary in layer2 or layer3 config.
func (u *GoldenImageUploader) hasPrimaryRole(spec map[string]interface{}) bool {
	// Check network.layer2.role or network.layer3.role for CUDN
	if network, ok := spec["network"].(map[string]interface{}); ok {
		if l2, ok := network["layer2"].(map[string]interface{}); ok {
			if role, ok := l2["role"].(string); ok && role == "Primary" {
				return true
			}
		}
		if l3, ok := network["layer3"].(map[string]interface{}); ok {
			if role, ok := l3["role"].(string); ok && role == "Primary" {
				return true
			}
		}
	}

	// Check layer2.role or layer3.role directly for UDN
	if l2, ok := spec["layer2"].(map[string]interface{}); ok {
		if role, ok := l2["role"].(string); ok && role == "Primary" {
			return true
		}
	}
	if l3, ok := spec["layer3"].(map[string]interface{}); ok {
		if role, ok := l3["role"].(string); ok && role == "Primary" {
			return true
		}
	}

	return false
}

// selectorMatchesNamespace checks if a CUDN's namespaceSelector matches the target namespace.
func (u *GoldenImageUploader) selectorMatchesNamespace(spec map[string]interface{}, nsLabels map[string]string) bool {
	nsSelector, ok := spec["namespaceSelector"].(map[string]interface{})
	if !ok {
		return false
	}

	labelSelector := &metav1.LabelSelector{}

	// Parse matchLabels
	if matchLabels, ok := nsSelector["matchLabels"].(map[string]interface{}); ok {
		labelSelector.MatchLabels = make(map[string]string)
		for k, v := range matchLabels {
			if strVal, ok := v.(string); ok {
				labelSelector.MatchLabels[k] = strVal
			}
		}
	}

	// Parse matchExpressions
	if matchExpressions, ok := nsSelector["matchExpressions"].([]interface{}); ok {
		for _, expr := range matchExpressions {
			exprMap, ok := expr.(map[string]interface{})
			if !ok {
				continue
			}

			requirement := metav1.LabelSelectorRequirement{}
			if key, ok := exprMap["key"].(string); ok {
				requirement.Key = key
			}
			if operator, ok := exprMap["operator"].(string); ok {
				requirement.Operator = metav1.LabelSelectorOperator(operator)
			}
			if values, ok := exprMap["values"].([]interface{}); ok {
				for _, v := range values {
					if strVal, ok := v.(string); ok {
						requirement.Values = append(requirement.Values, strVal)
					}
				}
			}
			labelSelector.MatchExpressions = append(labelSelector.MatchExpressions, requirement)
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return false
	}

	return selector.Matches(labels.Set(nsLabels))
}

// uploadViaHTTPSource implements the HTTP source workflow for UDN namespaces.
func (u *GoldenImageUploader) uploadViaHTTPSource(ctx context.Context, localImagePath string) error {
	// Create ephemeral nginx pod
	fmt.Println("Creating ephemeral image server pod...")
	if err := u.createServerPod(ctx); err != nil {
		return fmt.Errorf("creating server pod: %w", err)
	}
	defer u.cleanup(ctx)

	// Create service
	fmt.Println("Creating image server service...")
	if err := u.createServerService(ctx); err != nil {
		return fmt.Errorf("creating server service: %w", err)
	}

	// Stream image to pod
	fmt.Printf("Streaming image %s to pod...\n", localImagePath)
	if err := u.streamImageToPod(ctx, localImagePath); err != nil {
		return fmt.Errorf("streaming image: %w", err)
	}

	// Create DataVolume with HTTP source
	fmt.Println("Creating DataVolume with HTTP source...")
	if err := u.createDataVolume(ctx); err != nil {
		return fmt.Errorf("creating DataVolume: %w", err)
	}

	// Wait for completion
	fmt.Println("Waiting for DataVolume to complete...")
	if err := u.waitForDataVolume(ctx); err != nil {
		return fmt.Errorf("waiting for DataVolume: %w", err)
	}

	fmt.Printf("Golden image %s created successfully\n", u.pvcName)
	return nil
}

// uploadViaProxy implements the standard CDI upload proxy workflow.
// This is a placeholder - integrate with existing virtctl-style upload logic.
func (u *GoldenImageUploader) uploadViaProxy(ctx context.Context, localImagePath string) error {
	return fmt.Errorf("namespace %s does not use a Primary UDN; this tool only handles UDN workaround uploads — use 'virtctl image-upload' instead", u.namespace)
}

// createServerPod creates an ephemeral nginx pod to serve the image.
func (u *GoldenImageUploader) createServerPod(ctx context.Context) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serverPodName,
			Namespace: u.namespace,
			Labels:    map[string]string{"app": serverPodName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "nginx",
				Image: "nginx:alpine",
				Ports: []corev1.ContainerPort{{
					ContainerPort: serverPort,
					Protocol:      corev1.ProtocolTCP,
				}},
				Command: []string{"sh", "-c", "mkdir -p /usr/share/nginx/html && nginx -g 'daemon off;'"},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt32(serverPort),
						},
					},
					InitialDelaySeconds: 2,
					PeriodSeconds:       2,
				},
			}},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	_, err := u.k8sClient.CoreV1().Pods(u.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}

	// Wait for pod ready
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true,
		func(ctx context.Context) (bool, error) {
			p, err := u.k8sClient.CoreV1().Pods(u.namespace).Get(ctx, serverPodName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		})
}

// createServerService creates a ClusterIP service for the image server pod.
func (u *GoldenImageUploader) createServerService(ctx context.Context) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serverSvcName,
			Namespace: u.namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": serverPodName},
			Ports: []corev1.ServicePort{{
				Port:       serverPort,
				TargetPort: intstr.FromInt32(serverPort),
				Protocol:   corev1.ProtocolTCP,
			}},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	_, err := u.k8sClient.CoreV1().Services(u.namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// streamImageToPod streams the local image file to the nginx pod via exec/tar.
func (u *GoldenImageUploader) streamImageToPod(ctx context.Context, localImagePath string) error {
	// Open local file
	file, err := os.Open(localImagePath)
	if err != nil {
		return fmt.Errorf("opening local file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat local file: %w", err)
	}

	fmt.Printf("Image size: %d bytes (%.2f GB)\n", fileInfo.Size(), float64(fileInfo.Size())/(1024*1024*1024))

	// Create tar stream (kubectl cp protocol)
	reader, writer := io.Pipe()

	// Write tar in goroutine
	errChan := make(chan error, 1)
	go func() {
		defer writer.Close()
		tw := tar.NewWriter(writer)
		defer tw.Close()

		header := &tar.Header{
			Name: "disk.qcow2",
			Mode: 0644,
			Size: fileInfo.Size(),
		}
		if err := tw.WriteHeader(header); err != nil {
			errChan <- fmt.Errorf("writing tar header: %w", err)
			return
		}

		written, err := io.Copy(tw, file)
		if err != nil {
			errChan <- fmt.Errorf("copying file to tar: %w", err)
			return
		}
		fmt.Printf("Wrote %d bytes to tar stream\n", written)
		errChan <- nil
	}()

	// Execute tar extract in pod
	req := u.k8sClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(serverPodName).
		Namespace(u.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "nginx",
			Command:   []string{"tar", "-xf", "-", "-C", "/usr/share/nginx/html"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(u.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  reader,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("streaming to pod: %w", err)
	}

	// Check for tar write errors
	if tarErr := <-errChan; tarErr != nil {
		return tarErr
	}

	return nil
}

// createDataVolume creates a DataVolume with HTTP source pointing to the ephemeral server.
// Uses dynamic client to avoid CDI typed client dependency issues.
func (u *GoldenImageUploader) createDataVolume(ctx context.Context) error {
	httpURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/disk.qcow2",
		serverSvcName, u.namespace, serverPort)

	// Build DataVolume as unstructured object
	dv := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]interface{}{
				"name":      u.pvcName,
				"namespace": u.namespace,
				"annotations": map[string]interface{}{
					"cdi.kubevirt.io/storage.bind.immediate.requested": "", // Force immediate binding
				},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"http": map[string]interface{}{
						"url": httpURL,
					},
				},
				"storage": map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": u.pvcSize,
						},
					},
				},
			},
		},
	}

	// Add storage class if specified
	if u.storageClass != "" {
		spec := dv.Object["spec"].(map[string]interface{})
		storage := spec["storage"].(map[string]interface{})
		storage["storageClassName"] = u.storageClass
	}

	_, err := u.dynamicClient.Resource(dataVolumeGVR).Namespace(u.namespace).Create(ctx, dv, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating DataVolume: %w", err)
	}

	return nil
}

// waitForDataVolume waits for the DataVolume to reach Succeeded phase.
// Uses dynamic client to avoid CDI typed client dependency issues.
func (u *GoldenImageUploader) waitForDataVolume(ctx context.Context) error {
	var lastPhase string

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 60*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			dv, err := u.dynamicClient.Resource(dataVolumeGVR).Namespace(u.namespace).Get(ctx, u.pvcName, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("getting DataVolume: %w", err)
			}

			// Extract phase from status
			status, ok := dv.Object["status"].(map[string]interface{})
			if !ok {
				return false, nil // Status not yet populated
			}

			phase, _ := status["phase"].(string)

			// Log phase changes
			if phase != lastPhase {
				fmt.Printf("DataVolume phase: %s\n", phase)
				lastPhase = phase
			}

			// Check for failure
			if phase == DVPhaseFailed {
				conditions, _ := status["conditions"].([]interface{})
				return false, fmt.Errorf("DataVolume failed: %v", conditions)
			}

			return phase == DVPhaseSucceeded, nil
		})
}

// cleanup removes the ephemeral pod and service.
func (u *GoldenImageUploader) cleanup(ctx context.Context) {
	fmt.Println("Cleaning up ephemeral resources...")

	// Delete service (ignore errors)
	if err := u.k8sClient.CoreV1().Services(u.namespace).Delete(ctx, serverSvcName, metav1.DeleteOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			fmt.Printf("Warning: failed to delete service: %v\n", err)
		}
	}

	// Delete pod (ignore errors)
	if err := u.k8sClient.CoreV1().Pods(u.namespace).Delete(ctx, serverPodName, metav1.DeleteOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			fmt.Printf("Warning: failed to delete pod: %v\n", err)
		}
	}
}

// GetPVCSize parses an image file and returns a recommended PVC size.
// Adds 20% overhead to account for qcow2 to raw conversion expansion.
func GetPVCSize(imagePath string) (string, error) {
	info, err := os.Stat(imagePath)
	if err != nil {
		return "", err
	}

	// Add 20% overhead and round up to nearest Gi
	sizeBytes := float64(info.Size()) * 1.2
	sizeGi := int64(sizeBytes/(1024*1024*1024)) + 1

	return fmt.Sprintf("%dGi", sizeGi), nil
}
