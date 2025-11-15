package main

import (
	"encoding/json"
	"io"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	cnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
)

type patch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func reviewPod(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	var pod corev1.Pod

	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		klog.Errorf("Could not unmarshal pod: %v", err)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	podName := pod.Name
	if pod.Name == "" {
		podName = pod.GenerateName + "<generated>"
	}

	podType := ""
	if pod.Labels["forklift.app"] == "virt-v2v" {
		podType = "virt-v2v"
	} else if pod.Labels["app"] == "containerized-data-importer" {
		podType = "cdi"
	} else {
		klog.Warningf("Reviewing non virt-v2v or non cdi pod: %s/%s - This should not happen, skipping the pod.", pod.Namespace, podName)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	uid := string(ar.Request.UID)
	klog.Infof("Reviewing %s pod: %s/%s (uid=%s)", podType, pod.Namespace, podName, uid)

	var patches []patch
	if networksAnnotation, exists := pod.Annotations["k8s.v1.cni.cncf.io/networks"]; exists {
		klog.Infof("Found networks annotation on %s pod %s/%s (uid=%s): %s", podType, pod.Namespace, podName, uid, networksAnnotation)

		var networks []cnitypes.NetworkSelectionElement
		if err := json.Unmarshal([]byte(networksAnnotation), &networks); err != nil {
			klog.Warningf("Cannot parse k8s.v1.cni.cncf.io/networks on %s pod %s/%s (uid=%s): %v", podType, pod.Namespace, podName, uid, err)
			return &admissionv1.AdmissionResponse{
				Allowed: true,
			}
		}

		yeeted := false
		for i := range networks {
			if len(networks[i].GatewayRequest) > 0 {
				klog.Infof("YEETING default-route %v from network %s/%s on %s pod %s/%s (uid=%s)!", networks[i].GatewayRequest, networks[i].Namespace, networks[i].Name, podType, pod.Namespace, podName, uid)
				networks[i].GatewayRequest = nil
				yeeted = true
			}
		}

		if yeeted {
			modifiedNetworks, err := json.Marshal(networks)
			if err != nil {
				klog.Errorf("Could not marshal modified networks: %v", err)
				return &admissionv1.AdmissionResponse{
					Result: &metav1.Status{
						Message: err.Error(),
					},
				}
			}

			klog.Infof("New networks annotation for %s pod %s/%s (uid=%s): %s", podType, pod.Namespace, podName, uid, modifiedNetworks)
			patches = append(patches, patch{
				Op:    "replace",
				Path:  "/metadata/annotations/k8s.v1.cni.cncf.io~1networks",
				Value: string(modifiedNetworks),
			})
		}
	}

	if len(patches) == 0 {
		klog.Infof("No networks annotation or no default-route(s) found on %s pod %s/%s (uid=%s)", podType, pod.Namespace, podName, uid)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Could not marshal patches: %v", err)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	klog.Infof("Patching %s pod %s/%s (uid=%s): %s", podType, pod.Namespace, podName, uid, string(patchBytes))

	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &pt,
	}
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		klog.Errorf("Could not read request body: %v", err)
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		klog.Errorf("Could not unmarshal admission review: %v", err)
		http.Error(w, "could not unmarshal admission review", http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil {
		klog.Errorf("Missing admission request")
		http.Error(w, "missing admission request", http.StatusBadRequest)
		return
	}

	if admissionReview.Request.Kind.Group != "" || admissionReview.Request.Kind.Kind != "Pod" {
		klog.Warningf("Unsupported GVK %s - This should not happen, skipping.", admissionReview.Request.Kind.String())
		admissionReview.Response = &admissionv1.AdmissionResponse{
			Allowed: true,
		}
		if err := writeAdmissionReviewResponse(w, &admissionReview); err != nil {
			http.Error(w, "could not marshal response", http.StatusInternalServerError)
		}
		return
	}

	admissionReview.Response = reviewPod(&admissionReview)
	if admissionReview.Response != nil {
		admissionReview.Response.UID = admissionReview.Request.UID
	}

	if err := writeAdmissionReviewResponse(w, &admissionReview); err != nil {
		http.Error(w, "could not marshal response", http.StatusInternalServerError)
	}
}

func writeAdmissionReviewResponse(w http.ResponseWriter, review *admissionv1.AdmissionReview) error {
	resp, err := json.Marshal(review)
	if err != nil {
		klog.Errorf("Could not marshal response: %v", err)
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(resp); err != nil {
		klog.Errorf("Could not write response: %v", err)
		return err
	}
	return nil
}

func main() {
	klog.Info("Starting Gateway Yeeter on :8443")

	http.HandleFunc("/mutate", handleMutate)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	if err := http.ListenAndServeTLS(":8443", "/etc/server/certs/tls.crt", "/etc/server/certs/tls.key", nil); err != nil {
		klog.Fatalf("Failed to start server: %v", err)
	}

}
