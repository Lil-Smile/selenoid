package service

import (
	//"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"github.com/lil-smile/selenoid/session"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/openshift/api/build/v1"
	buildv1 "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	"github.com/docker/go-connections/nat"
	"github.com/lil-smile/selenoid/config"
	"log"
	"net/url"
)

type Openshift struct {
	Client *buildv1.BuildV1Client
	ServiceBase
	Environment
	session.Caps
}

func (op *Openshift) StartWithCancel() (*StartedService, error) {
	fmt.Printf("stating openshift")
	portConfig, err := getPortConfigOs(op.Service, op.Caps, op.Environment)
	if err != nil {
		return nil, fmt.Errorf("configuring ports: %v", err)
	}
	selenium := portConfig.SeleniumPort
	fileserver := portConfig.FileserverPort
	clipboard := portConfig.ClipboardPort
	vnc := portConfig.VNCPort
	requestId := op.RequestId
	image := op.Service.Image
	//ctx := context.Background()
	log.Printf("[%d] [CREATING_CONTAINER] [%s]", requestId, image)
	buildRequest := v1.BuildRequest{}

	u := &url.URL{Scheme: "http", Host: selenium.Port(), Path: op.Service.Path}

	s := StartedService{
		Url:u,
		Container: &session.Container{
			ID:        string(buildRequest.UID),
			IPAddress: "127.0.0.1",
		},
		HostPort: session.HostPort{
			Selenium:selenium.Port(),
			Fileserver:fileserver.Port(),
			Clipboard:clipboard.Port(),
			VNC:vnc.Port(),
		},
		Cancel: func() {

		},
	}
	return &s, nil
}

func GetClient() (*buildv1.BuildV1Client, error) {
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

	return buildV1Client, nil
}

func CreateBuild(namespace, buildName, buildConfig string) (*v1.Build, error) {
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
	myBuildConfig, err := buildV1Client.BuildConfigs(namespace).Get(buildConfig, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	fmt.Printf("Found BuildConfig %s in namespace %s\n", myBuildConfig.Name, namespace)
	buildRequest := v1.BuildRequest{}
	buildRequest.Kind = "BuildRequest"
	buildRequest.APIVersion = "build.openshift.io/v1"
	objectMeta := metav1.ObjectMeta{}
	objectMeta.Name = buildName
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
func getPortConfigOs(service *config.Browser, caps session.Caps, env Environment) (*portConfig, error) {
	selenium, err := nat.NewPort("tcp", service.Port)
	if err != nil {
		return nil, fmt.Errorf("new selenium port: %v", err)
	}
	fileserver, err := nat.NewPort("tcp", fileserverPort)
	if err != nil {
		return nil, fmt.Errorf("new fileserver port: %v", err)
	}
	clipboard, err := nat.NewPort("tcp", clipboardPort)
	if err != nil {
		return nil, fmt.Errorf("new clipboard port: %v", err)
	}
	exposedPorts := map[nat.Port]struct{}{selenium: {}, fileserver: {}}
	var vnc nat.Port
	if caps.VNC {
		vnc, err = nat.NewPort("tcp", vncPort)
		if err != nil {
			return nil, fmt.Errorf("new vnc port: %v", err)
		}
		exposedPorts[vnc] = struct{}{}
	}
	portBindings := nat.PortMap{}
	if env.IP != "" || !env.InDocker {
		portBindings[selenium] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		portBindings[fileserver] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		portBindings[clipboard] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		if caps.VNC {
			portBindings[vnc] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		}
	}
	return &portConfig{
		SeleniumPort:   selenium,
		FileserverPort: fileserver,
		ClipboardPort:  clipboard,
		VNCPort:        vnc,
		PortBindings:   portBindings,
		ExposedPorts:   exposedPorts}, nil
}
