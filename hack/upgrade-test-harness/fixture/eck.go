// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package fixture

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

const (
	operatorNamespace      = "elastic-system"
	operatorSTS            = "elastic-operator"
	wantAPMServerNodes     = 1
	wantElasticsearchNodes = 3
	wantHealth             = "green"
	wantKibanaNodes        = 1
)

// TestParam holds parameters for a test.
type TestParam struct {
	Name            string `json:"name"`
	OperatorVersion string `json:"operatorVersion"`
	StackVersion    string `json:"stackVersion"`
	CRDVersion      string `json:"crdVersion"`
}

// Path returns the full path to the given filename from the test data files.
func (tp TestParam) Path(fileName string) string {
	return filepath.Join("testdata", tp.Name, fileName)
}

// Suffixed adds a suffix describing the test to the given name.
func (tp TestParam) Suffixed(name string) string {
	return fmt.Sprintf("%s[%s]", name, tp.Name)
}

// GVR returns the GroupVersionResource for the given kind.
func (tp TestParam) GVR(kind string) schema.GroupVersionResource {
	switch kind {
	case "elasticsearch":
		return schema.GroupVersionResource{Group: "elasticsearch.k8s.elastic.co", Version: tp.CRDVersion, Resource: "elasticsearches"}
	case "kibana":
		return schema.GroupVersionResource{Group: "kibana.k8s.elastic.co", Version: tp.CRDVersion, Resource: "kibanas"}
	case "apmserver":
		return schema.GroupVersionResource{Group: "apm.k8s.elastic.co", Version: tp.CRDVersion, Resource: "apmservers"}
	default:
		panic(fmt.Errorf("unknown kind: %s", kind))
	}
}

// TestInstallOperator is the fixture for installing an operator.
func TestInstallOperator(param TestParam) *Fixture {
	return &Fixture{
		Name: param.Suffixed("TestInstallOperator"),
		Steps: []*TestStep{
			noRetry(param.Suffixed("InstallOperator"), applyManifests(param.Path("install.yaml"))),
			pause(20 * time.Second),
			retryRetriable("CheckOperatorIsReady", checkOperatorIsReady),
		},
	}
}

func checkOperatorIsReady(ctx *TestContext) error {
	ctx.Debug("Getting status of operator")
	resources := ctx.GetResources(operatorNamespace, "statefulset", operatorSTS)

	runtimeObj, err := resources.Object()
	if err != nil {
		return err
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(runtimeObj)
	if err != nil {
		return err
	}

	ready, _, err := unstructured.NestedInt64(obj, "status", "readyReplicas")
	if err != nil {
		return fmt.Errorf("failed to get ready pods from status: %w", err)
	}

	if ready != 1 {
		return fmt.Errorf("operator is not ready: %w", ErrRetry)
	}

	return nil
}

// TestDeployResources is the fixture for deploying a set of resources.
func TestDeployResources(param TestParam) *Fixture {
	return &Fixture{
		Name: param.Suffixed("TestDeployResources"),
		Steps: []*TestStep{
			noRetry(param.Suffixed("DeployResources"), applyManifests(param.Path("stack.yaml"))),
		},
	}
}

// TestStatusOfResources is the fixture for checking the status of a set of resources.
func TestStatusOfResources(param TestParam) *Fixture {
	return &Fixture{
		Name: param.Suffixed("TestStatusOfResources"),
		Steps: []*TestStep{
			retryRetriable(param.Suffixed("CheckElasticsearchStatus"),
				checkStatus("elasticsearch", param.Name, status{health: wantHealth, nodes: wantElasticsearchNodes, version: param.StackVersion})),
			retryRetriable(param.Suffixed("CheckKibana"),
				checkStatus("kibana", param.Name, status{health: wantHealth, nodes: wantKibanaNodes, version: param.StackVersion})),
			retryRetriable(param.Suffixed("CheckAPMServer"),
				checkStatus("apmserver", param.Name, status{health: wantHealth, nodes: wantAPMServerNodes, version: param.StackVersion})),
		},
	}
}

type status struct {
	health  string
	nodes   int64
	version string
}

func checkStatus(kind, name string, want status) func(*TestContext) error {
	return func(ctx *TestContext) error {
		ctx.Debugw("Getting status", "kind", kind, "name", name)

		have, err := getStatus(ctx, kind, name)
		if err != nil {
			return err
		}

		if have != want {
			return fmt.Errorf("status mismatch: want=%+v have=%+v: %w", want, have, ErrRetry)
		}

		return nil
	}
}

func getStatus(ctx *TestContext, kind, name string) (status, error) {
	s := status{}
	resources := ctx.GetResources(ctx.Namespace(), kind, name)

	runtimeObj, err := resources.Object()
	if err != nil {
		return s, err
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(runtimeObj)
	if err != nil {
		return s, err
	}

	s.health, _, err = unstructured.NestedString(obj, "status", "health")
	if err != nil {
		return s, fmt.Errorf("failed to get health from status: %w", err)
	}

	s.nodes, _, err = unstructured.NestedInt64(obj, "status", "availableNodes")
	if err != nil {
		return s, fmt.Errorf("failed to get nodes from status: %w", err)
	}

	s.version, _, err = unstructured.NestedString(obj, "spec", "version")
	if err != nil {
		return s, fmt.Errorf("failed to get version from status: %w", err)
	}

	return s, nil
}

// TestRemoveResources is the fixture for removing a set of resources.
func TestRemoveResources(param TestParam) *Fixture {
	return &Fixture{
		Name: param.Suffixed("TestRemoveResources"),
		Steps: []*TestStep{
			retryRetriable(param.Suffixed("RemoveResources"), deleteManifests(param.Path("stack.yaml"))),
		},
	}
}

// TestScaleElasticsearch is the fixture for scaling an Elasticsearch resource.
func TestScaleElasticsearch(param TestParam, count int) *Fixture {
	return &Fixture{
		Name: param.Suffixed("TestScaleElasticsearch"),
		Steps: []*TestStep{
			retryRetriable(param.Suffixed("ScaleElasticsearch"), scaleElasticsearch(param, int64(count))),
			pause(30 * time.Second),
			retryRetriable(param.Suffixed("CheckElasticsearchStatus"),
				checkStatus("elasticsearch", param.Name, status{health: wantHealth, nodes: int64(count), version: param.StackVersion})),
		},
	}
}

func scaleElasticsearch(param TestParam, count int64) func(*TestContext) error {
	return func(ctx *TestContext) error {
		resources := ctx.GetResources(ctx.Namespace(), "elasticsearch", param.Name)

		dynamicClient, err := ctx.DynamicClient()
		if err != nil {
			return err
		}

		return resources.Visit(func(info *resource.Info, err error) error {
			if err != nil {
				return err
			}

			runtimeObj, err := resources.Object()
			if err != nil {
				return err
			}

			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(runtimeObj)
			if err != nil {
				return err
			}

			nodeSets, found, err := unstructured.NestedSlice(obj, "spec", "nodeSets")
			if err != nil {
				return err
			}

			if !found {
				return errors.New("unable to find nodeSets in the object")
			}

			if len(nodeSets) > 0 {
				firstNodeSet, ok := nodeSets[0].(map[string]interface{})
				if !ok {
					return errors.New("unexpected format for nodeSets slice")
				}

				if err := unstructured.SetNestedField(firstNodeSet, count, "count"); err != nil {
					return fmt.Errorf("failed to set nodeSet.count: %w", err)
				}
			}

			if err := unstructured.SetNestedSlice(obj, nodeSets, "spec", "nodeSets"); err != nil {
				return fmt.Errorf("failed to set nodeSets: %w", err)
			}

			u := &unstructured.Unstructured{Object: obj}
			_, err = dynamicClient.Resource(param.GVR("elasticsearch")).Namespace(ctx.Namespace()).Update(context.TODO(), u, metav1.UpdateOptions{})

			return err
		})
	}
}
