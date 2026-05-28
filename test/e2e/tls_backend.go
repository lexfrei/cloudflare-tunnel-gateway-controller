//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// tlsBackendNginxImage is the image used as the TLS-terminating server.
	tlsBackendNginxImage = "nginx:1.27-alpine"
	// tlsBackendPort is the HTTPS port the nginx container exposes.
	tlsBackendPort int32 = 8443
	// tlsBackendKeyBits is the RSA key size used for both CA and leaf
	// certificates. 2048 bits is the smallest size still considered safe
	// for short-lived test certs; rotation is per-test-run so we trade no
	// safety for fast key generation.
	tlsBackendKeyBits = 2048

	appProtocolHTTPS    = "https"
	tlsBackendCASuffix  = "-ca"
	tlsBackendCfgSuffix = "-nginx-config"

	volumeNameTLS    = "tls"
	volumeNameConfig = "config"

	pemTypeCertificate   = "CERTIFICATE"
	pemTypeRSAPrivateKey = "RSA PRIVATE KEY"
)

// nginxConfTemplate renders a TLS-only listener that returns a plain-text
// body, sufficient for the rotation / mirror / gRPC-TLS e2e tests in
// later phases to confirm the request actually reached the backend over
// TLS rather than plaintext.
const nginxConfTemplate = `server {
    listen 8443 ssl;
    listen [::]:8443 ssl;
    http2 on;
    ssl_certificate /etc/nginx/tls/tls.crt;
    ssl_certificate_key /etc/nginx/tls/tls.key;
    ssl_protocols TLSv1.2 TLSv1.3;

    location / {
        add_header Content-Type text/plain;
        return 200 'tls-backend-ok\n';
    }
}
`

// TLSBackendManifests holds the manifest set produced by
// BuildTLSBackendManifests. The CA in the ConfigMap signs the leaf in the
// Secret, and the Deployment serves only that leaf over TLS on port
// 8443. A second ConfigMap carries the nginx server block.
type TLSBackendManifests struct {
	// Secret is a kubernetes.io/tls Secret with tls.crt / tls.key for the leaf.
	Secret *corev1.Secret
	// ConfigMap carries the CA bundle (key "ca.crt") for a BackendTLSPolicy
	// to reference via caCertificateRefs.
	ConfigMap *corev1.ConfigMap
	// NginxConfig carries the nginx server block (key "default.conf"),
	// mounted at /etc/nginx/conf.d/ inside the Deployment.
	NginxConfig *corev1.ConfigMap
	// Deployment runs an nginx image and mounts both the Secret and the
	// nginx server block.
	Deployment *appsv1.Deployment
	// Service exposes 8443 with appProtocol https.
	Service *corev1.Service
}

// BuildTLSBackendManifests assembles a TLS-only backend whose certificate
// chain is rooted at an in-memory CA. Re-invoked builds produce a fresh
// CA + leaf pair — callers in different tests must not share manifests
// instances if isolation across tests is required.
func BuildTLSBackendManifests(namespace, name string) (TLSBackendManifests, error) {
	caCertPEM, caKey, err := newCA()
	if err != nil {
		return TLSBackendManifests{}, errors.Wrap(err, "generate CA")
	}

	dnsNames := []string{
		name,
		fmt.Sprintf("%s.%s", name, namespace),
		fmt.Sprintf("%s.%s.svc", name, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace),
	}

	leafCertPEM, leafKeyPEM, err := newLeaf(caCertPEM, caKey, name, dnsNames)
	if err != nil {
		return TLSBackendManifests{}, errors.Wrap(err, "generate leaf")
	}

	labels := map[string]string{"app": name}

	return TLSBackendManifests{
		Secret:      newTLSBackendSecret(namespace, name, labels, leafCertPEM, leafKeyPEM),
		ConfigMap:   newTLSBackendCAConfigMap(namespace, name, labels, caCertPEM),
		NginxConfig: newTLSBackendNginxConfigMap(namespace, name, labels),
		Deployment:  newTLSBackendDeployment(namespace, name, labels),
		Service:     newTLSBackendService(namespace, name, labels),
	}, nil
}

func newTLSBackendSecret(namespace, name string, labels map[string]string, leafCertPEM, leafKeyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       leafCertPEM,
			corev1.TLSPrivateKeyKey: leafKeyPEM,
		},
	}
}

func newTLSBackendCAConfigMap(namespace, name string, labels map[string]string, caCertPEM []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + tlsBackendCASuffix,
			Namespace: namespace,
			Labels:    labels,
		},
		Data: map[string]string{"ca.crt": string(caCertPEM)},
	}
}

func newTLSBackendNginxConfigMap(namespace, name string, labels map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + tlsBackendCfgSuffix,
			Namespace: namespace,
			Labels:    labels,
		},
		Data: map[string]string{"default.conf": nginxConfTemplate},
	}
}

func newTLSBackendDeployment(namespace, name string, labels map[string]string) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: tlsBackendNginxImage,
						Ports: []corev1.ContainerPort{{
							Name:          appProtocolHTTPS,
							ContainerPort: tlsBackendPort,
							Protocol:      corev1.ProtocolTCP,
						}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: volumeNameTLS, MountPath: "/etc/nginx/tls", ReadOnly: true},
							{Name: volumeNameConfig, MountPath: "/etc/nginx/conf.d", ReadOnly: true},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name:         volumeNameTLS,
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}},
						},
						{
							Name: volumeNameConfig,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: name + tlsBackendCfgSuffix},
								},
							},
						},
					},
				},
			},
		},
	}
}

func newTLSBackendService(namespace, name string, labels map[string]string) *corev1.Service {
	httpsProtocol := appProtocolHTTPS

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:        appProtocolHTTPS,
				Port:        tlsBackendPort,
				TargetPort:  intstr.FromInt32(tlsBackendPort),
				Protocol:    corev1.ProtocolTCP,
				AppProtocol: &httpsProtocol,
			}},
		},
	}
}

// newCA generates a self-signed CA with a 1-year validity window.
func newCA() ([]byte, *rsa.PrivateKey, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, tlsBackendKeyBits)
	if err != nil {
		return nil, nil, errors.Wrap(err, "generate CA key")
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tls-backend test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "sign CA certificate")
	}

	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der}), caKey, nil
}

// newLeaf generates a leaf certificate signed by the supplied CA, valid
// for all dnsNames.
func newLeaf(caCertPEM []byte, caKey *rsa.PrivateKey, commonName string, dnsNames []string) ([]byte, []byte, error) {
	caBlock, _ := pem.Decode(caCertPEM)
	if caBlock == nil {
		return nil, nil, errors.New("decode CA PEM")
	}

	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, nil, errors.Wrap(err, "parse CA certificate")
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, tlsBackendKeyBits)
	if err != nil {
		return nil, nil, errors.Wrap(err, "generate leaf key")
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "sign leaf certificate")
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeRSAPrivateKey, Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})

	return certPEM, keyPEM, nil
}

// pemDecodeFirst returns the first PEM block in input, plus the
// remaining bytes. Wraps pem.Decode for readability at call sites that
// don't care about subsequent blocks.
func pemDecodeFirst(input []byte) (*pem.Block, []byte) {
	return pem.Decode(input)
}
