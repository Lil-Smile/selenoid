package service

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/openshift/api/build/v1"
	buildv1 "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
)

type Openshift struct {
	Client *buildv1.BuildV1Client
}

func (op *Openshift) StartWithCancel() (*StartedService, error) {

	//s := StartedService{
	//	Url:&url.URL{Scheme: "http", Host: "", Path: ""},
	//	Container: &session.Container{
	//		ID:        "",
	//		IPAddress: getContainerIP("", nil),
	//	},
	//	HostPort: nil,
	//	Cancel: func() {
	//
	//	},
	//}
	return nil, nil
}

func CreateBuild(namespace, buildName string) (*v1.Build, error) {
	var kubeconfig *string
	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err
	}

	buildV1Client, err := buildv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	// get all builds
	builds, err := buildV1Client.Builds(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	fmt.Printf("There are %d builds in project %s\n", len(builds.Items), namespace)
	// List names of all builds
	for i, build := range builds.Items {
		fmt.Printf("index %d: Name of the build: %s", i, build.Name)
	}

	// get a specific build
	myBuild, err := buildV1Client.Builds(namespace).Get(buildName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	fmt.Printf("Found build %s in namespace %s\n", buildName, namespace)
	fmt.Printf("Raw printout of the build %+v\n", myBuild)
	// get details of the build
	fmt.Printf("name %s, start time %s, duration (in sec) %.0f, and phase %s\n",
		myBuild.Name, myBuild.Status.StartTimestamp.String(),
		myBuild.Status.Duration.Seconds(), myBuild.Status.Phase)

	// trigger a build
	buildConfig := "cakephp-ex"
	myBuildConfig, err := buildV1Client.BuildConfigs(namespace).Get(buildConfig, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	fmt.Printf("Found BuildConfig %s in namespace %s\n", myBuildConfig.Name, namespace)
	buildRequest := v1.BuildRequest{}
	buildRequest.Kind = "BuildRequest"
	buildRequest.APIVersion = "build.openshift.io/v1"
	objectMeta := metav1.ObjectMeta{}
	objectMeta.Name = "cakephp-ex"
	buildRequest.ObjectMeta = objectMeta
	buildTriggerCause := v1.BuildTriggerCause{}
	buildTriggerCause.Message = "Manually triggered"
	buildRequest.TriggeredBy = []v1.BuildTriggerCause{buildTriggerCause}
	myBuild, err = buildV1Client.BuildConfigs(namespace).Instantiate(buildConfig, &buildRequest)

	if err != nil {
		return nil, err
	}
	fmt.Printf("Name of the triggered build %s\n", myBuild.Name)
	return myBuild, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
