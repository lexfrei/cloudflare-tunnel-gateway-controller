package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// gatewayClassFor builds a GatewayClass with the given controllerName.
func gatewayClassForController(name, controllerName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(controllerName)},
	}
}

// gatewayInClass builds a Gateway whose Spec.GatewayClassName points at the
// supplied class. Reused by managed-by-controller tests.
func gatewayInClass(namespace, name, gatewayClass string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gatewayClass)},
	}
}

func TestGatewayManagedByController_EmptyControllerName_AcceptsAny(t *testing.T) {
	t.Parallel()

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	gateway := gatewayInClass("ns", "gw", "any-class")

	// Empty controllerName disables the gate — used by test fixtures that
	// don't construct a full GatewayClass chain.
	assert.True(t, gatewayManagedByController(context.Background(), cli, gateway, ""))
}

func TestGatewayManagedByController_ManagedClass_Allowed(t *testing.T) {
	t.Parallel()

	const ourController = "example.com/tunnel"

	scheme := newClientCertScheme(t)
	gc := gatewayClassForController("our-class", ourController)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc).Build()
	gateway := gatewayInClass("ns", "gw", "our-class")

	assert.True(t, gatewayManagedByController(context.Background(), cli, gateway, ourController))
}

func TestGatewayManagedByController_ForeignController_Denied(t *testing.T) {
	t.Parallel()

	const ourController = "example.com/tunnel"

	scheme := newClientCertScheme(t)
	gc := gatewayClassForController("foreign-class", "competitor.example.com/ingress")
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc).Build()
	gateway := gatewayInClass("ns", "gw", "foreign-class")

	assert.False(t, gatewayManagedByController(context.Background(), cli, gateway, ourController),
		"a Gateway managed by another controller must NOT be allowed to contribute its client cert to our proxy's mTLS handshake")
}

func TestGatewayManagedByController_MissingGatewayClass_Denied(t *testing.T) {
	t.Parallel()

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	gateway := gatewayInClass("ns", "gw", "nonexistent-class")

	// When the GatewayClass lookup fails (NotFound, transient error) we fail
	// closed: better to NOT present a cert that might belong to another
	// controller than to leak credentials on a doubtful match.
	assert.False(t, gatewayManagedByController(context.Background(), cli, gateway, "example.com/tunnel"))
}

func TestNewGatewayClientCertResolver_ForeignController_ReturnsNil(t *testing.T) {
	t.Parallel()

	const ourController = "example.com/tunnel"

	certPEM, keyPEM := generateClientKeypair(t)
	secret := clientCertSecret("ns", "client-cert", certPEM, keyPEM)
	gc := gatewayClassForController("foreign-class", "competitor.example.com/ingress")
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)
	gateway.Spec.GatewayClassName = "foreign-class"

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, gc, gateway).Build()

	resolver := newGatewayClientCertResolver(cli, ourController)
	result := resolver(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gw"})

	require.Nil(t, result, "resolver must refuse to load a cert from a Gateway managed by another controller")
}

func TestNewGatewayClientCertResolver_OurController_LoadsCert(t *testing.T) {
	t.Parallel()

	const ourController = "example.com/tunnel"

	certPEM, keyPEM := generateClientKeypair(t)
	secret := clientCertSecret("ns", "client-cert", certPEM, keyPEM)
	gc := gatewayClassForController("our-class", ourController)
	gateway := gatewayWithClientCertRef("ns", "gw", "client-cert", nil)
	gateway.Spec.GatewayClassName = "our-class"

	scheme := newClientCertScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, gc, gateway).Build()

	resolver := newGatewayClientCertResolver(cli, ourController)
	result := resolver(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gw"})

	require.NotNil(t, result, "Gateway managed by our controller must yield the cert")
	assert.Equal(t, certPEM, result.CertPEM)
	assert.Equal(t, keyPEM, result.KeyPEM)
}
