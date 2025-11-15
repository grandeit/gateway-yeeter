package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
)

func testGatewayRemoval(t *testing.T, podName string, labels map[string]string, gateway string) {
	networks := []cnitypes.NetworkSelectionElement{
		{
			Namespace:      "default",
			Name:           "mtv-transfer",
			GatewayRequest: []net.IP{net.ParseIP(gateway)},
		},
	}
	networksJSON, _ := json.Marshal(networks)

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "test",
			Labels:    labels,
			Annotations: map[string]string{
				"k8s.v1.cni.cncf.io/networks": string(networksJSON),
			},
		},
	}

	rawPod, _ := json.Marshal(pod)
	review := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    "test",
			Object: runtime.RawExtension{Raw: rawPod},
		},
	}

	resp := reviewPod(review)

	if !resp.Allowed {
		t.Fatal("expected pod to be allowed")
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatal("expected JSONPatch response")
	}

	var patches []patch
	json.Unmarshal(resp.Patch, &patches)

	if len(patches) != 1 || patches[0].Path != "/metadata/annotations/k8s.v1.cni.cncf.io~1networks" {
		t.Fatal("expected network annotation patch")
	}

	var updated []cnitypes.NetworkSelectionElement
	json.Unmarshal([]byte(patches[0].Value.(string)), &updated)

	if len(updated[0].GatewayRequest) != 0 {
		t.Fatal("expected GatewayRequest to be removed")
	}
}

func TestVirtV2VPodGatewayRemoval(t *testing.T) {
	testGatewayRemoval(t, "virt-v2v-test", map[string]string{"forklift.app": "virt-v2v"}, "192.168.1.1")
}

func TestCDIPodGatewayRemoval(t *testing.T) {
	testGatewayRemoval(t, "importer-test", map[string]string{"app": "containerized-data-importer"}, "10.0.0.1")
}

func TestNonTargetPodPassthrough(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "random-pod",
			Namespace: "test",
			Labels:    map[string]string{"app": "other"},
		},
	}

	rawPod, _ := json.Marshal(pod)
	review := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    "test-skip",
			Object: runtime.RawExtension{Raw: rawPod},
		},
	}

	resp := reviewPod(review)

	if !resp.Allowed {
		t.Fatal("expected pod to be allowed")
	}
	if len(resp.Patch) != 0 {
		t.Fatal("expected no patches for non-target pod")
	}
}

func TestHandleMutateConfigMapPassthrough(t *testing.T) {
	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "test",
		},
		Data: map[string]string{
			"key": "value",
		},
	}
	rawConfigMap, _ := json.Marshal(configMap)

	admissionReview := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    "test-configmap",
			Kind:   metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			Object: runtime.RawExtension{Raw: rawConfigMap},
		},
	}

	body, _ := json.Marshal(admissionReview)
	req := httptest.NewRequest("POST", "/mutate", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handleMutate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var response admissionv1.AdmissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Response == nil {
		t.Fatal("expected response to be set")
	}

	if !response.Response.Allowed {
		t.Fatal("expected ConfigMap to be allowed")
	}

	if len(response.Response.Patch) != 0 {
		t.Fatal("expected no patches for ConfigMap")
	}
}
