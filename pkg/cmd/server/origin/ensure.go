package origin

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/golang/glog"

	kapierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/rbac"
	"k8s.io/kubernetes/pkg/client/retry"
	kbootstrappolicy "k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac/bootstrappolicy"

	"github.com/openshift/origin/pkg/oc/admin/policy"

	authorizationapi "github.com/openshift/origin/pkg/authorization/apis/authorization"
	clusterpolicyregistry "github.com/openshift/origin/pkg/authorization/registry/clusterpolicy"
	clusterpolicystorage "github.com/openshift/origin/pkg/authorization/registry/clusterpolicy/etcd"
	"github.com/openshift/origin/pkg/cmd/server/admin"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
	"github.com/openshift/origin/pkg/security/legacyclient"
)

// ensureOpenShiftSharedResourcesNamespace is called as part of global policy initialization to ensure shared namespace exists
func (c *MasterConfig) ensureOpenShiftSharedResourcesNamespace() {
	if _, err := c.KubeClientsetInternal().Core().Namespaces().Get(c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace, metav1.GetOptions{}); kapierror.IsNotFound(err) {
		namespace, createErr := c.KubeClientsetInternal().Core().Namespaces().Create(&kapi.Namespace{ObjectMeta: metav1.ObjectMeta{Name: c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace}})
		if createErr != nil {
			glog.Errorf("Error creating namespace: %v due to %v\n", c.Options.PolicyConfig.OpenShiftSharedResourcesNamespace, createErr)
			return
		}

		c.ensureNamespaceServiceAccountRoleBindings(namespace)
	}
}

// ensureOpenShiftInfraNamespace is called as part of global policy initialization to ensure infra namespace exists
func (c *MasterConfig) ensureOpenShiftInfraNamespace() {
	ns := c.Options.PolicyConfig.OpenShiftInfrastructureNamespace

	// Ensure namespace exists
	namespace, err := c.KubeClientsetInternal().Core().Namespaces().Create(&kapi.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if kapierror.IsAlreadyExists(err) {
		// Get the persisted namespace
		namespace, err = c.KubeClientsetInternal().Core().Namespaces().Get(ns, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Error getting namespace %s: %v", ns, err)
			return
		}
	} else if err != nil {
		glog.Errorf("Error creating namespace %s: %v", ns, err)
		return
	}

	for _, role := range bootstrappolicy.ControllerRoles() {
		reconcileRole := &policy.ReconcileClusterRolesOptions{
			RolesToReconcile: []string{role.Name},
			Confirmed:        true,
			Union:            true,
			Out:              ioutil.Discard,
			RoleClient:       c.PrivilegedLoopbackOpenShiftClient.ClusterRoles(),
		}
		if err := reconcileRole.RunReconcileClusterRoles(nil, nil); err != nil {
			glog.Errorf("Could not reconcile %v: %v\n", role.Name, err)
		}
	}
	for _, roleBinding := range bootstrappolicy.ControllerRoleBindings() {
		reconcileRoleBinding := &policy.ReconcileClusterRoleBindingsOptions{
			RolesToReconcile:  []string{roleBinding.RoleRef.Name},
			Confirmed:         true,
			Union:             true,
			Out:               ioutil.Discard,
			RoleBindingClient: c.PrivilegedLoopbackOpenShiftClient.ClusterRoleBindings(),
		}
		if err := reconcileRoleBinding.RunReconcileClusterRoleBindings(nil, nil); err != nil {
			glog.Errorf("Could not reconcile %v: %v\n", roleBinding.Name, err)
		}
	}

	c.ensureNamespaceServiceAccountRoleBindings(namespace)
}

// ensureDefaultNamespaceServiceAccountRoles initializes roles for service accounts in the default namespace
func (c *MasterConfig) ensureDefaultNamespaceServiceAccountRoles() {
	// Wait for the default namespace
	var namespace *kapi.Namespace
	for i := 0; i < 30; i++ {
		ns, err := c.KubeClientsetInternal().Core().Namespaces().Get(metav1.NamespaceDefault, metav1.GetOptions{})
		if err == nil {
			namespace = ns
			break
		}
		if kapierror.IsNotFound(err) {
			time.Sleep(time.Second)
			continue
		}
		glog.Errorf("Error adding service account roles to %q namespace: %v", metav1.NamespaceDefault, err)
		return
	}
	if namespace == nil {
		glog.Errorf("Namespace %q not found, could not initialize the %q namespace", metav1.NamespaceDefault, metav1.NamespaceDefault)
		return
	}

	c.ensureNamespaceServiceAccountRoleBindings(namespace)
}

// ensureNamespaceServiceAccountRoleBindings initializes roles for service accounts in the namespace
func (c *MasterConfig) ensureNamespaceServiceAccountRoleBindings(namespace *kapi.Namespace) {
	const ServiceAccountRolesInitializedAnnotation = "openshift.io/sa.initialized-roles"

	// Short-circuit if we're already initialized
	if namespace.Annotations[ServiceAccountRolesInitializedAnnotation] == "true" {
		return
	}

	hasErrors := false
	for _, binding := range bootstrappolicy.GetBootstrapServiceAccountProjectRoleBindings(namespace.Name) {
		addRole := &policy.RoleModificationOptions{
			RoleName:            binding.RoleRef.Name,
			RoleNamespace:       binding.RoleRef.Namespace,
			RoleBindingAccessor: policy.NewLocalRoleBindingAccessor(namespace.Name, c.ServiceAccountRoleBindingClient()),
			Subjects:            binding.Subjects,
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error { return addRole.AddRole() }); err != nil {
			glog.Errorf("Could not add service accounts to the %v role in the %q namespace: %v\n", binding.RoleRef.Name, namespace.Name, err)
			hasErrors = true
		}
	}

	// If we had errors, don't register initialization so we can try again
	if hasErrors {
		return
	}

	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	namespace.Annotations[ServiceAccountRolesInitializedAnnotation] = "true"
	// Log any error other than a conflict (the update will be retried and recorded again on next startup in that case)
	if _, err := c.KubeClientsetInternal().Core().Namespaces().Update(namespace); err != nil && !kapierror.IsConflict(err) {
		glog.Errorf("Error recording adding service account roles to %q namespace: %v", namespace.Name, err)
	}
}

func (c *MasterConfig) ensureDefaultSecurityContextConstraints() {
	ns := c.Options.PolicyConfig.OpenShiftInfrastructureNamespace
	bootstrapSCCGroups, bootstrapSCCUsers := bootstrappolicy.GetBoostrapSCCAccess(ns)

	for _, scc := range bootstrappolicy.GetBootstrapSecurityContextConstraints(bootstrapSCCGroups, bootstrapSCCUsers) {
		_, err := legacyclient.NewFromClient(c.KubeClientsetInternal().Core().RESTClient()).Create(&scc)
		if kapierror.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			glog.Errorf("Unable to create default security context constraint %s.  Got error: %v", scc.Name, err)
			continue
		}
		glog.Infof("Created default security context constraint %s", scc.Name)
	}
}

// ensureComponentAuthorizationRules initializes the cluster policies
func (c *MasterConfig) ensureComponentAuthorizationRules() {
	clusterPolicyStorage, err := clusterpolicystorage.NewREST(c.RESTOptionsGetter)
	if err != nil {
		glog.Errorf("Error creating policy storage: %v", err)
		return
	}
	clusterPolicyRegistry := clusterpolicyregistry.NewRegistry(clusterPolicyStorage)
	ctx := apirequest.WithNamespace(apirequest.NewContext(), "")

	if _, err := clusterPolicyRegistry.GetClusterPolicy(ctx, authorizationapi.PolicyName, &metav1.GetOptions{}); kapierror.IsNotFound(err) {
		glog.Infof("No cluster policy found.  Creating bootstrap policy based on: %v", c.Options.PolicyConfig.BootstrapPolicyFile)

		if err := admin.OverwriteBootstrapPolicy(c.RESTOptionsGetter, c.Options.PolicyConfig.BootstrapPolicyFile, admin.CreateBootstrapPolicyFileFullCommand, true, ioutil.Discard); err != nil {
			glog.Errorf("Error creating bootstrap policy: %v", err)
		}

		// these are namespaced, so we can't reconcile them.  Just try to put them in until we work against rbac
		// This only had to hold us until the transition is complete
		// TODO remove this block and use a post-starthook
		// ensure bootstrap namespaced roles are created or reconciled
		for namespace, roles := range kbootstrappolicy.NamespaceRoles() {
			for _, rbacRole := range roles {
				role := &authorizationapi.Role{}
				if err := authorizationapi.Convert_rbac_Role_To_authorization_Role(&rbacRole, role, nil); err != nil {
					utilruntime.HandleError(fmt.Errorf("unable to convert role.%s/%s in %v: %v", rbac.GroupName, rbacRole.Name, namespace, err))
					continue
				}
				if _, err := c.PrivilegedLoopbackOpenShiftClient.Roles(namespace).Create(role); err != nil {
					// don't fail on failures, try to create as many as you can
					utilruntime.HandleError(fmt.Errorf("unable to reconcile role.%s/%s in %v: %v", rbac.GroupName, role.Name, namespace, err))
				}
			}
		}

		// ensure bootstrap namespaced rolebindings are created or reconciled
		for namespace, roleBindings := range kbootstrappolicy.NamespaceRoleBindings() {
			for _, rbacRoleBinding := range roleBindings {
				roleBinding := &authorizationapi.RoleBinding{}
				if err := authorizationapi.Convert_rbac_RoleBinding_To_authorization_RoleBinding(&rbacRoleBinding, roleBinding, nil); err != nil {
					utilruntime.HandleError(fmt.Errorf("unable to convert rolebinding.%s/%s in %v: %v", rbac.GroupName, rbacRoleBinding.Name, namespace, err))
					continue
				}
				if _, err := c.PrivilegedLoopbackOpenShiftClient.RoleBindings(namespace).Create(roleBinding); err != nil {
					// don't fail on failures, try to create as many as you can
					utilruntime.HandleError(fmt.Errorf("unable to reconcile rolebinding.%s/%s in %v: %v", rbac.GroupName, roleBinding.Name, namespace, err))
				}
			}
		}

	} else {
		glog.V(2).Infof("Ignoring bootstrap policy file because cluster policy found")
	}

	// Reconcile roles that must exist for the cluster to function
	// Be very judicious about what is placed in this list, since it will be enforced on every server start
	reconcileRoles := &policy.ReconcileClusterRolesOptions{
		RolesToReconcile: []string{bootstrappolicy.DiscoveryRoleName},
		Confirmed:        true,
		Union:            true,
		Out:              ioutil.Discard,
		RoleClient:       c.PrivilegedLoopbackOpenShiftClient.ClusterRoles(),
	}
	if err := reconcileRoles.RunReconcileClusterRoles(nil, nil); err != nil {
		glog.Errorf("Could not auto reconcile roles: %v\n", err)
	}

	// Reconcile rolebindings that must exist for the cluster to function
	// Be very judicious about what is placed in this list, since it will be enforced on every server start
	reconcileRoleBindings := &policy.ReconcileClusterRoleBindingsOptions{
		RolesToReconcile:  []string{bootstrappolicy.DiscoveryRoleName},
		Confirmed:         true,
		Union:             true,
		Out:               ioutil.Discard,
		RoleBindingClient: c.PrivilegedLoopbackOpenShiftClient.ClusterRoleBindings(),
	}
	if err := reconcileRoleBindings.RunReconcileClusterRoleBindings(nil, nil); err != nil {
		glog.Errorf("Could not auto reconcile role bindings: %v\n", err)
	}
}
