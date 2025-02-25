package openapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/controllers"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/kyverno/pkg/metrics"
	util "github.com/kyverno/kyverno/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeSchema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
)

const (
	// Workers is the number of workers for this controller
	Workers        = 1
	ControllerName = "openapi-controller"
)

type Controller interface {
	controllers.Controller
	CheckSync(context.Context)
}

type controller struct {
	client  dclient.Interface
	manager Manager
}

const (
	skipErrorMsg = "Got empty response for"
)

// NewController ...
func NewController(client dclient.Interface, mgr Manager) Controller {
	if mgr == nil {
		panic(fmt.Errorf("nil manager sent into crd sync"))
	}

	return &controller{
		manager: mgr,
		client:  client,
	}
}

func (c *controller) Run(ctx context.Context, workers int) {
	if err := c.updateInClusterKindToAPIVersions(); err != nil {
		logging.Error(err, "failed to update in-cluster api versions")
	}

	newDoc, err := c.client.Discovery().OpenAPISchema()
	if err != nil {
		logging.Error(err, "cannot get OpenAPI schema")
	}

	err = c.manager.UseOpenAPIDocument(newDoc)
	if err != nil {
		logging.Error(err, "Could not set custom OpenAPI document")
	}
	// Sync CRD before kyverno starts
	c.sync()
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.CheckSync, 15*time.Second)
	}
}

func (c *controller) sync() {
	c.client.Discovery().DiscoveryCache().Invalidate()
	crds, err := c.client.GetDynamicInterface().Resource(runtimeSchema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}).List(context.TODO(), metav1.ListOptions{})

	c.client.RecordClientQuery(metrics.ClientList, metrics.KubeDynamicClient, "CustomResourceDefinition", "")
	if err != nil {
		logging.Error(err, "could not fetch crd's from server")
		return
	}

	c.manager.DeleteCRDFromPreviousSync()

	for _, crd := range crds.Items {
		c.manager.ParseCRD(crd)
	}

	if err := c.updateInClusterKindToAPIVersions(); err != nil {
		logging.Error(err, "sync failed, unable to update in-cluster api versions")
	}

	newDoc, err := c.client.Discovery().OpenAPISchema()
	if err != nil {
		logging.Error(err, "cannot get OpenAPI schema")
	}

	err = c.manager.UseOpenAPIDocument(newDoc)
	if err != nil {
		logging.Error(err, "Could not set custom OpenAPI document")
	}
}

func (c *controller) updateInClusterKindToAPIVersions() error {
	util.OverrideRuntimeErrorHandler()
	_, apiResourceLists, err := discovery.ServerGroupsAndResources(c.client.Discovery().DiscoveryInterface())
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			err := err.(*discovery.ErrGroupDiscoveryFailed)
			for gv, err := range err.Groups {
				logger.Error(err, "failed to list api resources", "group", gv)
			}
		} else if !strings.Contains(err.Error(), skipErrorMsg) {
			return err
		}
	}
	preferredAPIResourcesLists, err := discovery.ServerPreferredResources(c.client.Discovery().DiscoveryInterface())
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			err := err.(*discovery.ErrGroupDiscoveryFailed)
			for gv, err := range err.Groups {
				logger.Error(err, "failed to list api resources", "group", gv)
			}
		} else if !strings.Contains(err.Error(), skipErrorMsg) {
			return err
		}
	}
	c.manager.UpdateKindToAPIVersions(apiResourceLists, preferredAPIResourcesLists)
	return nil
}

func (c *controller) CheckSync(ctx context.Context) {
	crds, err := c.client.GetDynamicInterface().Resource(runtimeSchema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		logging.Error(err, "could not fetch crd's from server")
		return
	}
	if len(c.manager.GetCrdList()) != len(crds.Items) {
		c.sync()
	}
}
