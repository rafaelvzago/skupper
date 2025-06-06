package kube

import (
	"testing"

	"github.com/skupperproject/skupper/internal/cmd/skupper/common"
	"github.com/skupperproject/skupper/internal/cmd/skupper/common/testutils"
	"github.com/skupperproject/skupper/internal/cmd/skupper/common/utils"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCmdConnectorStatus_ValidateInput(t *testing.T) {
	type test struct {
		name           string
		args           []string
		flags          common.CommandConnectorStatusFlags
		k8sObjects     []runtime.Object
		skupperObjects []runtime.Object
		expectedError  string
		skupperError   string
	}

	testTable := []test{
		{
			name:          "missing CRD",
			args:          []string{"my-connector"},
			skupperError:  utils.CrdErr,
			expectedError: utils.CrdHelpErr,
		},
		{
			name:          "connector is not shown because connector does not exist in the namespace",
			args:          []string{"my-connector"},
			expectedError: "connector my-connector does not exist in namespace test",
		},
		{
			name:          "connector name is nil",
			args:          []string{""},
			expectedError: "connector name must not be empty",
		},
		{
			name:          "more than one argument is specified",
			args:          []string{"my", "connector"},
			expectedError: "only one argument is allowed for this command",
		},
		{
			name:          "connector name is not valid.",
			args:          []string{"my new connector"},
			expectedError: "connector name is not valid: value does not match this regular expression: ^[a-z0-9]([-a-z0-9]*[a-z0-9])*(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])*)*$",
		},
		{
			name:          "no args",
			expectedError: "",
		},
		{
			name:  "bad output status",
			args:  []string{"out-connector"},
			flags: common.CommandConnectorStatusFlags{Output: "not-supported"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "out-connector",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
			expectedError: "output type is not valid: value not-supported not allowed. It should be one of this options: [json yaml]",
		},
		{
			name:  "good output status",
			args:  []string{"out-connector"},
			flags: common.CommandConnectorStatusFlags{Output: "json"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "out-connector",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
			expectedError: "",
		},
	}

	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {

			command, err := newCmdConnectorStatusWithMocks("test", test.k8sObjects, test.skupperObjects, test.skupperError)
			assert.Assert(t, err)

			command.Flags = &test.flags

			testutils.CheckValidateInput(t, command, test.expectedError, test.args)
		})
	}
}

func TestCmdConnectorStatus_Run(t *testing.T) {
	type test struct {
		name                string
		connectorName       string
		flags               common.CommandConnectorStatusFlags
		k8sObjects          []runtime.Object
		skupperObjects      []runtime.Object
		skupperErrorMessage string
		errorMessage        string
	}

	testTable := []test{
		{
			name:         "run fails no connectors found",
			errorMessage: "NotFound",
		},
		{
			name:          "run fails connector doesn't exist",
			connectorName: "my-connector",
			errorMessage:  "connectors.skupper.io \"my-connector\" not found",
		},
		{
			name:          "runs ok, returns 1 connectors",
			connectorName: "my-connector",
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:          "runs ok, returns 1 connectors yaml",
			connectorName: "my-connector",
			flags:         common.CommandConnectorStatusFlags{Output: "yaml"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "runs ok, returns all connectors",
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector1",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector2",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:  "runs ok, returns all connectors json",
			flags: common.CommandConnectorStatusFlags{Output: "json"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector1",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector2",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:  "runs ok, returns all connectors output bad",
			flags: common.CommandConnectorStatusFlags{Output: "bad-value"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector1",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector2",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
			errorMessage: "format bad-value not supported",
		},
		{
			name:          "runs ok, returns 1 connectors bad output",
			connectorName: "my-connector",
			flags:         common.CommandConnectorStatusFlags{Output: "bad-value"},
			skupperObjects: []runtime.Object{
				&v2alpha1.ConnectorList{
					Items: []v2alpha1.Connector{
						{
							ObjectMeta: v1.ObjectMeta{
								Name:      "my-connector",
								Namespace: "test",
							},
							Status: v2alpha1.ConnectorStatus{
								Status: v2alpha1.Status{
									Conditions: []v1.Condition{
										{
											Type:   "Configured",
											Status: "True",
										},
									},
								},
							},
						},
					},
				},
			},
			errorMessage: "format bad-value not supported",
		},
	}

	for _, test := range testTable {
		cmd, err := newCmdConnectorStatusWithMocks("test", test.k8sObjects, test.skupperObjects, test.skupperErrorMessage)
		assert.Assert(t, err)

		cmd.name = test.connectorName
		cmd.Flags = &test.flags
		cmd.output = cmd.Flags.Output
		cmd.namespace = "test"

		t.Run(test.name, func(t *testing.T) {

			err := cmd.Run()
			if err != nil {
				assert.Check(t, test.errorMessage == err.Error())
			} else {
				assert.Check(t, err == nil)
			}
		})
	}
}

// --- helper methods

func newCmdConnectorStatusWithMocks(namespace string, k8sObjects []runtime.Object, skupperObjects []runtime.Object, fakeSkupperError string) (*CmdConnectorStatus, error) {

	client, err := fakeclient.NewFakeClient(namespace, k8sObjects, skupperObjects, fakeSkupperError)
	if err != nil {
		return nil, err
	}
	cmdConnectorStatus := &CmdConnectorStatus{
		client:    client.GetSkupperClient().SkupperV2alpha1(),
		namespace: namespace,
	}
	return cmdConnectorStatus, nil
}
