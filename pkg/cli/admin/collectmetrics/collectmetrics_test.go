package collectmetrics

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/diff"

	//imagev1 "github.com/openshift/api/image/v1"
	imageclient "github.com/openshift/client-go/image/clientset/versioned/fake"
)

func TestImagesAndImageStreams(t *testing.T) {

	testCases := []struct {
		name          string
		image         string
		expectedImage string
		objects       []runtime.Object
	}{
		{
			name:          "DefaultNoCollectMetrics",
			expectedImage: "registry.redhat.io/openshift4/ose-collect-metrics:latest",
		},
		{
			name:          "ImagesTest",
			image:         "one",
			expectedImage: "one",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := CollectMetricOptions{
				IOStreams:   genericclioptions.NewTestIOStreamsDiscard(),
				Client:      fake.NewSimpleClientset(),
				ImageClient: imageclient.NewSimpleClientset(tc.objects...).ImageV1(),
				Image:       tc.image,
				LogOut:      genericclioptions.NewTestIOStreamsDiscard().Out,
			}
			err := options.completeImages()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(options.Image, tc.expectedImage) {
				t.Fatal(diff.ObjectDiff(options.Image, tc.expectedImage))
			}
		})
	}

}

func TestGetNamespace(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		Options          CollectMetricOptions
		ShouldBeRetained bool
		ShouldFail       bool
	}{
		"no namespace given": {
			Options: CollectMetricOptions{
				Client: fake.NewSimpleClientset(),
			},
			ShouldBeRetained: false,
		},
		"namespace given": {
			Options: CollectMetricOptions{
				Client: fake.NewSimpleClientset(
					&corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test-namespace",
						},
					},
				),
				RunNamespace: "test-namespace",
			},
			ShouldBeRetained: true,
		},
		"namespace given, but does not exist": {
			Options: CollectMetricOptions{
				Client:       fake.NewSimpleClientset(),
				RunNamespace: "test-namespace",
			},
			ShouldBeRetained: true,
			ShouldFail:       true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc.Options.PrinterCreated = printers.NewDiscardingPrinter()
			tc.Options.PrinterDeleted = printers.NewDiscardingPrinter()

			ns, cleanup, err := tc.Options.getNamespace()
			if err != nil {
				if tc.ShouldFail {
					return
				}

				t.Fatal(err)
			}

			if _, err = tc.Options.Client.CoreV1().Namespaces().Get(context.TODO(), ns.Name, metav1.GetOptions{}); err != nil {
				if !k8sapierrors.IsNotFound(err) {
					t.Fatal(err)
				}

				t.Error("namespace should exist")
			}

			cleanup()

			if _, err = tc.Options.Client.CoreV1().Namespaces().Get(context.TODO(), ns.Name, metav1.GetOptions{}); err != nil {
				if !k8sapierrors.IsNotFound(err) {
					t.Fatal(err)
				} else if tc.ShouldBeRetained {
					t.Error("namespace should still exist")
				}
			}
		})
	}
}
