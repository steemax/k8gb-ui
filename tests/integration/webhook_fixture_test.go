//go:build integration

package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"k8gb-ui/internal/traefik"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var webhookConfigs = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}

func webhookConfig(ns string) traefik.Config {
	return traefik.Config{Domain: "apps.example.test", LocalDomain: "local.example.test", IngressClass: "traefik", MTLSEntrypoint: "mtls", MTLSTLSOption: ns + "/client-auth", CNPlugin: "cncheck", AllowedGeoTags: []string{"east", "west"}, AllowedNamespaces: []string{ns}}
}

func (f *fixture) installWebhook(t *testing.T, image string) traefik.Config {
	t.Helper()
	cfg := webhookConfig(f.ns)
	ca, cert, key := servingCertificate(t, "admission."+f.ns+".svc")
	secret := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "admission-tls", "namespace": f.ns}, "type": "kubernetes.io/tls",
		"data": map[string]interface{}{"tls.crt": base64.StdEncoding.EncodeToString(cert), "tls.key": base64.StdEncoding.EncodeToString(key)},
	}}
	_, err := f.api(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Create(f.ctx, secret, metav1.CreateOptions{})
	check(t, err)
	policy := object("traefik.io/v1alpha1", "TLSOption", f.ns, "client-auth", map[string]interface{}{"clientAuth": map[string]interface{}{"clientAuthType": "RequireAndVerifyClientCert", "secretNames": []interface{}{"client-ca"}}})
	_, err = f.api(schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "tlsoptions"}).Create(f.ctx, policy, metav1.CreateOptions{})
	check(t, err)
	account := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]interface{}{"name": "admission", "namespace": f.ns}}}
	_, err = f.api(schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}).Create(f.ctx, account, metav1.CreateOptions{})
	check(t, err)
	role := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": map[string]interface{}{"name": "admission-tls", "namespace": f.ns}, "rules": []interface{}{map[string]interface{}{"apiGroups": []interface{}{"traefik.io"}, "resources": []interface{}{"tlsoptions"}, "verbs": []interface{}{"get"}}}}}
	_, err = f.api(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}).Create(f.ctx, role, metav1.CreateOptions{})
	check(t, err)
	binding := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": map[string]interface{}{"name": "admission-tls", "namespace": f.ns}, "roleRef": map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "admission-tls"}, "subjects": []interface{}{map[string]interface{}{"kind": "ServiceAccount", "name": "admission", "namespace": f.ns}}}}
	_, err = f.api(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}).Create(f.ctx, binding, metav1.CreateOptions{})
	check(t, err)
	service := object("v1", "Service", f.ns, "admission", map[string]interface{}{
		"selector": map[string]interface{}{"app": "admission"}, "ports": []interface{}{map[string]interface{}{"name": "https", "port": int64(443), "targetPort": int64(8443)}},
	})
	_, err = f.api(schema.GroupVersionResource{Version: "v1", Resource: "services"}).Create(f.ctx, service, metav1.CreateOptions{})
	check(t, err)
	args := []interface{}{"--listen=:8443", "--tls-cert-file=/tls/tls.crt", "--tls-private-key-file=/tls/tls.key", "--allowed-namespaces=" + f.ns, "--allowed-geo-tags=east,west", "--domain=" + cfg.Domain, "--local-domain=" + cfg.LocalDomain, "--ingress-class=traefik", "--mtls-tls-option=" + cfg.MTLSTLSOption, "--cn-plugin=" + cfg.CNPlugin}
	pod := object("v1", "Pod", f.ns, "admission", map[string]interface{}{
		"automountServiceAccountToken": true, "serviceAccountName": "admission", "restartPolicy": "Never", "terminationGracePeriodSeconds": int64(1),
		"securityContext": map[string]interface{}{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532)},
		"containers": []interface{}{map[string]interface{}{
			"name": "admission", "image": image, "imagePullPolicy": "Never", "args": args,
			"ports":           []interface{}{map[string]interface{}{"name": "https", "containerPort": int64(8443)}},
			"securityContext": map[string]interface{}{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]interface{}{"drop": []interface{}{"ALL"}}},
			"volumeMounts":    []interface{}{map[string]interface{}{"name": "tls", "mountPath": "/tls", "readOnly": true}},
			"readinessProbe":  map[string]interface{}{"httpGet": map[string]interface{}{"path": "/readyz", "port": int64(8443), "scheme": "HTTPS"}, "periodSeconds": int64(1)},
		}},
		"volumes": []interface{}{map[string]interface{}{"name": "tls", "secret": map[string]interface{}{"secretName": "admission-tls", "defaultMode": int64(0444)}}},
	})
	pod.SetLabels(map[string]string{"app": "admission"})
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	_, err = f.api(pods).Create(f.ctx, pod, metav1.CreateOptions{})
	check(t, err)
	deadline := time.Now().Add(90 * time.Second)
	ready := false
	for time.Now().Before(deadline) && f.ctx.Err() == nil {
		pod = f.get(t, pods, "admission")
		conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
		for _, condition := range conditions {
			c, _ := condition.(map[string]interface{})
			if c["type"] == "Ready" && c["status"] == "True" {
				ready = true
			}
		}
		if ready {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		status, _, _ := unstructured.NestedMap(pod.Object, "status")
		t.Fatalf("webhook Pod not ready (image must be preloaded into kind): %#v", status)
	}

	name := f.ns + "-admission"
	registration := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingWebhookConfiguration", "metadata": map[string]interface{}{"name": name, "labels": map[string]interface{}{"k8gb-ui.io/e2e-namespace": f.ns}},
		"webhooks": []interface{}{map[string]interface{}{
			"name": "ingressroute.integration.k8gb-ui.io", "admissionReviewVersions": []interface{}{"v1"}, "sideEffects": "None", "failurePolicy": "Fail", "timeoutSeconds": int64(5), "matchPolicy": "Exact", "reinvocationPolicy": "Never",
			"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": f.ns}},
			"clientConfig":      map[string]interface{}{"caBundle": base64.StdEncoding.EncodeToString(ca), "service": map[string]interface{}{"namespace": f.ns, "name": "admission", "port": int64(443), "path": "/mutate"}},
			"rules":             []interface{}{map[string]interface{}{"apiGroups": []interface{}{"traefik.io"}, "apiVersions": []interface{}{"v1alpha1"}, "resources": []interface{}{"ingressroutes"}, "operations": []interface{}{"CREATE", "UPDATE"}, "scope": "Namespaced"}},
		}},
	}}
	registration, err = f.client.Resource(webhookConfigs).Create(f.ctx, registration, metav1.CreateOptions{})
	check(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		uid := registration.GetUID()
		err := f.client.Resource(webhookConfigs).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete own webhook configuration %s: %v", name, err)
		}
	})
	cfg.WebhookConfig = name

	// Wait for the API server's registration cache, not merely Pod readiness.
	probe := f.route("registration-probe", map[string]string{"gslb": "true", "gslb_mtls": "true", "gslb_hostname": "probe.apps.example.test"})
	var lastErr error
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		admitted, err := f.api(routes).Create(f.ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		lastErr = err
		if err == nil && admitted.GetAnnotations()[traefik.OriginalEntrypoints] != "" {
			t.Logf("real TLS admission active: image=%s MWC=%s", image, name)
			return cfg
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("real TLS webhook did not admit/mutate dry-run probe: %v", lastErr)
	return cfg
}

func servingCertificate(t *testing.T, dnsName string) (ca, cert, key []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, err)
	now := time.Now()
	issuer := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "k8gb-ui isolated e2e CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, issuer, issuer, &caKey.PublicKey, caKey)
	check(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, err)
	server := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName, dnsName + ".cluster.local"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(2 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, issuer, &serverKey.PublicKey, caKey)
	check(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	check(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func requireWebhookImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("K8GB_UI_E2E") != "1" {
		t.Skip("real kind tests require K8GB_UI_E2E=1")
	}
	image := os.Getenv("K8GB_UI_E2E_WEBHOOK_IMAGE")
	if image == "" {
		t.Skip("real TLS admission additionally requires K8GB_UI_E2E_WEBHOOK_IMAGE preloaded in kind")
	}
	return image
}

func (f *fixture) route(name string, annotations map[string]string) *unstructured.Unstructured {
	u := object("traefik.io/v1alpha1", "IngressRoute", f.ns, name, map[string]interface{}{
		"entryPoints": []interface{}{"websecure", "grpc"}, "routes": []interface{}{map[string]interface{}{"kind": "Rule", "match": fmt.Sprintf("Host(`%s.apps.example.test`)", name), "services": []interface{}{map[string]interface{}{"name": "backend", "port": int64(8080)}}}},
	})
	u.SetAnnotations(annotations)
	return u
}
