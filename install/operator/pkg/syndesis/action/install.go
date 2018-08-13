package action

import (
	"errors"
	"github.com/openshift/api/route/v1"
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/sirupsen/logrus"
	"github.com/syndesisio/syndesis/install/operator/pkg/apis/syndesis/v1alpha1"
	"github.com/syndesisio/syndesis/install/operator/pkg/openshift/serviceaccount"
	"github.com/syndesisio/syndesis/install/operator/pkg/syndesis/operation"
	syndesistemplate "github.com/syndesisio/syndesis/install/operator/pkg/syndesis/template"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	SyndesisRouteName = "syndesis"
)

// Install syndesis into the namespace, taking resources from the bundled template.
type Install struct{}

func (a *Install) CanExecute(syndesis *v1alpha1.Syndesis) bool {
	return syndesisPhaseIs(syndesis, v1alpha1.SyndesisPhaseInstalling)
}

func (a *Install) Execute(syndesis *v1alpha1.Syndesis) error {

	logrus.Info("Installing Syndesis resource ", syndesis.Name)

	token, err := installServiceAccount(syndesis)
	if err != nil {
		return err
	}

	// Let's use a copy of the original parameter from now on, so that it can be changed inside this action
	originalSyndesis := syndesis
	syndesis = syndesis.DeepCopy()

	// Detect if the route should be auto-generated
	autoGenerateRoute := syndesis.Spec.RouteHostName == ""
	if autoGenerateRoute {
		syndesis.Spec.RouteHostName = "dummy"
	}

	list, err := syndesistemplate.GetInstallResourcesAsRuntimeObjects(syndesis, syndesistemplate.InstallParams{
		OAuthClientSecret: token,
	})
	if err != nil {
		return err
	}

	syndesisRoute, err := installSyndesisRoute(syndesis, list, autoGenerateRoute)
	if err != nil {
		return err
	}

	if autoGenerateRoute {
		// Set the right hostname after generating the route
		syndesis.Spec.RouteHostName = syndesisRoute.Spec.Host

		// Hack to remove the auto-generated annotation
		// In Openshift 3.9, the route gets low priority for being displayed as main route for the app if the openshift.io/host.generated=true annotation is present
		err = removeAutoGeneratedAnnotation(syndesisRoute)
		if err != nil {
			return err
		}

		// Recreate the list of resources to inject the route hostname
		list, err = syndesistemplate.GetInstallResourcesAsRuntimeObjects(syndesis, syndesistemplate.InstallParams{
			OAuthClientSecret: token,
		})
	}

	for _, res := range list {
		if _, isSyndesisRoute := isSyndesisRoute(res); isSyndesisRoute {
			// Syndesis route already installed
			continue
		}

		operation.SetNamespaceAndOwnerReference(res, syndesis)

		err = createOrReplace(res)
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return err
		}
	}

	// Installation completed, set the next state
	target := originalSyndesis.DeepCopy()
	target.Status.Phase = v1alpha1.SyndesisPhaseStarting
	target.Status.Reason = v1alpha1.SyndesisStatusReasonMissing
	target.Status.Description = ""
	addRouteAnnotation(target, syndesisRoute)
	logrus.Info("Syndesis resource ", target.Name, " installed")

	return sdk.Update(target)
}

func installServiceAccount(syndesis *v1alpha1.Syndesis) (string, error) {
	sa := newSyndesisServiceAccount()
	operation.SetNamespaceAndOwnerReference(sa, syndesis)
	// We don't replace the service account if already present, to let Kubernetes generate its tokens
	err := sdk.Create(sa)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return "", err
	}

	return serviceaccount.GetServiceAccountToken(sa.Name, syndesis.Namespace)
}

func newSyndesisServiceAccount() *corev1.ServiceAccount {
	sa := corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "syndesis-oauth-client",
			Labels: map[string]string{
				"app": "syndesis",
			},
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirecturi.local":       "https://localhost:4200",
				"serviceaccounts.openshift.io/oauth-redirecturi.route":       "https://",
				"serviceaccounts.openshift.io/oauth-redirectreference.route": `{"kind": "OAuthRedirectReference", "apiVersion": "v1", "reference": {"kind": "Route","name": "syndesis"}}`,
			},
		},
	}

	return &sa
}

func addRouteAnnotation(syndesis *v1alpha1.Syndesis, route *v1.Route) {
	annotations := syndesis.ObjectMeta.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
		syndesis.ObjectMeta.Annotations = annotations
	}
	annotations["syndesis.io/applicationUrl"] = extractApplicationUrl(route)
}
func extractApplicationUrl(route *v1.Route) string {
	scheme := "http"
	if route.Spec.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + route.Spec.Host
}

func installSyndesisRoute(syndesis *v1alpha1.Syndesis, objects []runtime.Object, autoGenerate bool) (*v1.Route, error) {
	route, err := findSyndesisRoute(objects)
	if err != nil {
		return nil, err
	}

	operation.SetNamespaceAndOwnerReference(route, syndesis)

	if autoGenerate {
		route.Spec.Host = ""
	}

	// We don't replace the route if already present, to let Openshift generate its host
	err = sdk.Create(route)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, err
	}

	if route.Spec.Host != "" {
		return route, nil
	}

	// Let's try to get the route from Openshift to check the host field
	err = sdk.Get(route)
	if err != nil {
		return nil, err
	}

	if route.Spec.Host == "" {
		return nil, errors.New("hostname still not present on syndesis route")
	}

	return route, nil
}

func findSyndesisRoute(resources []runtime.Object) (*v1.Route, error) {
	for _, res := range resources {
		if route, ok := isSyndesisRoute(res); ok {
			return route, nil
		}
	}
	return nil, errors.New("syndesis route not found")
}

func isSyndesisRoute(resource runtime.Object) (*v1.Route, bool) {
	if route, ok := resource.(*v1.Route); ok {
		if route.Name == SyndesisRouteName {
			return route, true
		}
	}
	return nil, false
}

func removeAutoGeneratedAnnotation(route *v1.Route) error {
	return updateOnLatestRevision(route, func(obj runtime.Object) {
		if r, ok := obj.(*v1.Route); ok {
			delete(r.Annotations, "openshift.io/host.generated")
		}
	})
}