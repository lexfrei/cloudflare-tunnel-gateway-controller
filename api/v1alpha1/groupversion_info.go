package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

//nolint:gochecknoglobals // kubebuilder-standard scheme registration
var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cf.k8s.lex.la", Version: "v1alpha1"}

	// SchemeBuilder collects functions that register the API types with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&GatewayClassConfig{}, &GatewayClassConfigList{},
		&ExternalBackend{}, &ExternalBackendList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)

	return nil
}
