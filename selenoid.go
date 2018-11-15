package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/openshift/api/build/v1"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aerokube/util"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/lil-smile/selenoid/session"
	"github.com/lil-smile/selenoid/upload"
	appsV1 "github.com/openshift/api/apps/v1"
	//buildv1 "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	"golang.org/x/net/websocket"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/api/apps/v1beta1"
	v12 "github.com/openshift/api/template/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v14 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strconv"
)

const slash = "/"

var (
	httpClient = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	num     uint64
	numLock sync.RWMutex
)

type request struct {
	*http.Request
}

type sess struct {
	addr string
	id   string
}

// TODO There is simpler way to do this
func (r request) localaddr() string {
	addr := r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()
	_, port, _ := net.SplitHostPort(addr)
	return net.JoinHostPort("127.0.0.1", port)
}

func (r request) session(id string) *sess {
	return &sess{r.localaddr(), id}
}

func (s *sess) url() string {
	return fmt.Sprintf("http://%s/wd/hub/session/%s", s.addr, s.id)
}

func (s *sess) Delete(requestId uint64) {
	log.Printf("[%d] [SESSION_TIMED_OUT] [%s]", requestId, s.id)
	r, err := http.NewRequest(http.MethodDelete, s.url(), nil)
	if err != nil {
		log.Printf("[%d] [DELETE_FAILED] [%s] [%v]", requestId, s.id, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
	defer cancel()
	resp, err := httpClient.Do(r.WithContext(ctx))
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil && resp.StatusCode == http.StatusOK {
		return
	}
	if err != nil {
		log.Printf("[%d] [DELETE_FAILED] [%s] [%v]", requestId, s.id, err)
	} else {
		log.Printf("[%d] [DELETE_FAILED] [%s] [%s]", requestId, s.id, resp.Status)
	}
}

func serial() uint64 {
	numLock.Lock()
	defer numLock.Unlock()
	id := num
	num++
	return id
}

func getSerial() uint64 {
	numLock.RLock()
	defer numLock.RUnlock()
	return num
}

func create(w http.ResponseWriter, r *http.Request) {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	sessionStartTime := time.Now()
	requestId := serial()
	user, remote := util.RequestInfo(r)
	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		log.Printf("[%d] [ERROR_READING_REQUEST] [%v]", requestId, err)
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	var browser struct {
		Caps session.Caps `json:"desiredCapabilities"`
		W3CCaps struct {
			Caps session.Caps `json:"alwaysMatch"`
		} `json:"capabilities"`
	}
	err = json.Unmarshal(body, &browser)
	if err != nil {
		log.Printf("[%d] [BAD_JSON_FORMAT] [%v]", requestId, err)
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	if browser.W3CCaps.Caps.Name != "" && browser.Caps.Name == "" {
		browser.Caps = browser.W3CCaps.Caps
	}
	browser.Caps.ProcessExtensionCapabilities()
	sessionTimeout, err := getSessionTimeout(browser.Caps.SessionTimeout, maxTimeout, timeout)
	if err != nil {
		log.Printf("[%d] [BAD_SESSION_TIMEOUT] [%s]", requestId, browser.Caps.SessionTimeout)
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	resolution, err := getScreenResolution(browser.Caps.ScreenResolution)
	if err != nil {
		log.Printf("[%d] [BAD_SCREEN_RESOLUTION] [%s]", requestId, browser.Caps.ScreenResolution)
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	browser.Caps.ScreenResolution = resolution
	videoScreenSize, err := getVideoScreenSize(browser.Caps.VideoScreenSize, resolution)
	if err != nil {
		log.Printf("[%d] [BAD_VIDEO_SCREEN_SIZE] [%s]", requestId, browser.Caps.VideoScreenSize)
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	browser.Caps.VideoScreenSize = videoScreenSize
	finalVideoName := browser.Caps.VideoName
	if browser.Caps.Video && !disableDocker {
		browser.Caps.VideoName = getTemporaryFileName(videoOutputDir, videoFileExtension)
	}
	finalLogName := browser.Caps.LogName
	if logOutputDir != "" {
		browser.Caps.LogName = getTemporaryFileName(logOutputDir, logFileExtension)
	}
	starter, ok := manager.Find(browser.Caps, requestId)
	if !ok {
		log.Printf("[%d] [ENVIRONMENT_NOT_AVAILABLE] [%s] [%s]", requestId, browser.Caps.Name, browser.Caps.Version)
		util.JsonError(w, "Requested environment is not available", http.StatusBadRequest)
		queue.Drop()
		return
	}
	startedService, err := starter.StartWithCancel()
	if err != nil {
		log.Printf("[%d] [SERVICE_STARTUP_FAILED] [%v]", requestId, err)
		util.JsonError(w, err.Error(), http.StatusInternalServerError)
		queue.Drop()
		return
	}
	u := startedService.Url
	cancel := startedService.Cancel
	var resp *http.Response
	i := 1
	for ; ; i++ {
		r.URL.Host, r.URL.Path = "https", "/oapi/v1/builds" //path.Join(u.Path, r.URL.Path, "/oapi/v1/builds")
		req := createRequestDeploymentConfig()
		requestDump, _ := httputil.DumpRequest(req, true)
		log.Printf("request:%s", string(requestDump))
		ctx, done := context.WithTimeout(r.Context(), newSessionAttemptTimeout)
		defer done()
		log.Printf("[%d] [SESSION_ATTEMPTED] [%s] [%d]", requestId, u.String(), i)
		rsp, err := httpClient.Do(req.WithContext(ctx))
		select {
		case <-ctx.Done():
			if rsp != nil {
				rsp.Body.Close()
			}
			switch ctx.Err() {
			case context.DeadlineExceeded:
				log.Printf("[%d] [SESSION_ATTEMPT_TIMED_OUT] [%s]", requestId, newSessionAttemptTimeout)
				if i < retryCount {
					continue
				}
				err := fmt.Errorf("New session attempts retry count exceeded")
				log.Printf("[%d] [SESSION_FAILED] [%s] [%s]", requestId, u.String(), err)
				util.JsonError(w, err.Error(), http.StatusInternalServerError)
			case context.Canceled:
				log.Printf("[%d] [CLIENT_DISCONNECTED] [%s] [%s] [%.2fs]", requestId, user, remote, util.SecondsSince(sessionStartTime))
			}
			queue.Drop()
			cancel()
			return
		default:
		}
		if err != nil {
			if rsp != nil {
				log.Printf("response: %v", rsp)
				rsp.Body.Close()
			}
			log.Printf("[%d] [SESSION_FAILED] [%s] [%s]", requestId, u.String(), err)
			util.JsonError(w, err.Error(), http.StatusInternalServerError)
			queue.Drop()
			cancel()
			return
		}
		if rsp.StatusCode == http.StatusNotFound && u.Path == "" {
			u.Path = "/wd/hub"
			continue
		}
		resp = rsp
		break
	}
	defer resp.Body.Close()
	var s struct {
		Value struct {
			ID string `json:"sessionId"`
		}
		ID string `json:"sessionId"`
	}
	location := resp.Header.Get("Location")
	if location != "" {
		l, err := url.Parse(location)
		if err == nil {
			fragments := strings.Split(l.Path, slash)
			s.ID = fragments[len(fragments)-1]
			u := &url.URL{
				Scheme: "http",
				Host:   hostname,
				Path:   path.Join("/wd/hub/session", s.ID),
			}
			w.Header().Add("Location", u.String())
			w.WriteHeader(resp.StatusCode)
		}
	} else {
		tee := io.TeeReader(resp.Body, w)
		w.WriteHeader(resp.StatusCode)
		json.NewDecoder(tee).Decode(&s)
		if s.ID == "" {
			s.ID = s.Value.ID
		}
	}
	if s.ID == "" {
		log.Printf("[%d] [SESSION_FAILED] [%s] [%s]", requestId, u.String(), resp.Status)
		queue.Drop()
		cancel()
		return
	}
	sess := &session.Session{
		Quota:     user,
		Caps:      browser.Caps,
		URL:       u,
		Container: startedService.Container,
		HostPort:  startedService.HostPort,
		Timeout:   sessionTimeout,
		TimeoutCh: onTimeout(sessionTimeout, func() {
			request{r}.session(s.ID).Delete(requestId)
		})}
	cancelAndRenameFiles := func() {
		cancel()
		if browser.Caps.Video && !disableDocker {
			oldVideoName := filepath.Join(videoOutputDir, browser.Caps.VideoName)
			if finalVideoName == "" {
				finalVideoName = s.ID + videoFileExtension
			}
			newVideoName := filepath.Join(videoOutputDir, finalVideoName)
			err := os.Rename(oldVideoName, newVideoName)
			if err != nil {
				log.Printf("[%d] [VIDEO_ERROR] [%s]", requestId, fmt.Sprintf("Failed to rename %s to %s: %v", oldVideoName, newVideoName, err))
			} else {
				input := &upload.UploadRequest{
					RequestId: requestId,
					Filename:  newVideoName,
					SessionId: s.ID,
					Session:   sess,
					Type:      "video",
				}
				upload.Upload(input)
			}
		}
		if logOutputDir != "" {
			//The following logic will fail if -capture-driver-logs is enabled and a session is requested in driver mode.
			//Specifying both -log-output-dir and -capture-driver-logs in that case is considered a misconfiguration.
			oldLogName := filepath.Join(logOutputDir, browser.Caps.LogName)
			if finalLogName == "" {
				finalLogName = s.ID + logFileExtension
			}
			newLogName := filepath.Join(logOutputDir, finalLogName)
			err := os.Rename(oldLogName, newLogName)
			if err != nil {
				log.Printf("[%d] [LOG_ERROR] [%s]", requestId, fmt.Sprintf("Failed to rename %s to %s: %v", oldLogName, newLogName, err))
			} else {
				input := &upload.UploadRequest{
					RequestId: requestId,
					Filename:  newLogName,
					SessionId: s.ID,
					Session:   sess,
					Type:      "log",
				}
				upload.Upload(input)
			}
		}
	}
	sess.Cancel = cancelAndRenameFiles
	sessions.Put(s.ID, sess)
	queue.Create()
	log.Printf("[%d] [SESSION_CREATED] [%s] [%d] [%.2fs]", requestId, s.ID, i, util.SecondsSince(sessionStartTime))
}

const (
	videoFileExtension = ".mp4"
	logFileExtension   = ".log"
)

var (
	fullFormat  = regexp.MustCompile(`^([0-9]+x[0-9]+)x(8|16|24)$`)
	shortFormat = regexp.MustCompile(`^[0-9]+x[0-9]+$`)
)

func addHeaders(method string, req *http.Request) *http.Request {
	token := "" //get from UI-console
	req.Header.Set("Authorization", "Bearer "+token)
	//req.Header.Set("Accept", "application/json;as=Table;v=v1beta1;g=meta.k8s.io, application/json")
	//to create build
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Connection", "close")
	if http.MethodPost == method {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func creteBuildBody() v1.Build {
	image := "artifactorycn.netcracker.com:17028/atp/browsers/vnc-chrome:69.0"
	//image := "chrome"
	build := v1.Build{}
	build.Spec.TriggeredBy = append(build.Spec.TriggeredBy, v1.BuildTriggerCause{ImageChangeBuild: &v1.ImageChangeCause{ImageID: image}})
	build.Spec.Resources.Limits = make(k8sv1.ResourceList)
	build.Spec.Strategy = v1.BuildStrategy{Type: "Custom"}
	build.Spec.Strategy.CustomStrategy = &v1.CustomBuildStrategy{}
	build.Spec.Resources.Limits["cpu"] = resource.MustParse("500m")
	build.Spec.Resources.Limits["memory"] = resource.MustParse("128Mi")
	build.Kind = "Build"
	build.APIVersion = "build.openshift.io/v1"
	return build
}

func createRequestDeployment() *http.Request {
	body := createDeploymentBody()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "https://openshift.netcracker.cloud:8443/apis/extensions/v1beta1/namespaces/tadevelopment/deployments", bytes.NewReader(jsonBody))
	req.Close = true
	req = addHeaders(http.MethodPost, req)
	return req
}

func createRequestBuild() *http.Request {
	body := creteBuildBody()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "https://openshift.netcracker.cloud:8443/apis/build.openshift.io/v1/namespaces/tadevelopment/builds", bytes.NewReader(jsonBody))
	req.Close = true
	req = addHeaders(http.MethodPost, req)
	return req
}

func createRequestDeploymentConfig() *http.Request {
	body := createDeploymentConfigBody()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "https://openshift.netcracker.cloud:8443/oapi/v1/namespaces/tadevelopment/deploymentconfigs", bytes.NewReader(jsonBody))
	req.Close = true
	req = addHeaders(http.MethodPost, req)
	return req
}

func createRequestTemplate() *http.Request {
	body := createTemplateBody()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "https://openshift.netcracker.cloud:8443/apis/template.openshift.io/v1/namespaces/tadevelopment/processedtemplates", bytes.NewReader(jsonBody))
	req.Close = true
	req = addHeaders(http.MethodPost, req)
	return req
}

func createDeploymentBody() v1beta1.Deployment {
	config := v1beta1.Deployment{}
	config.Kind = "Deployment"
	config.APIVersion = "extensions/v1beta1"
	meta := v13.ObjectMeta{Name: "igor"}
	meta.Labels = make(map[string]string)
	meta.Labels["app"] = "igor"
	meta.Labels["name"] = "igor"
	config.ObjectMeta = meta
	tmpSelector := &v13.LabelSelector{}
	tmpSelector.MatchLabels = make(map[string]string)
	tmpSelector.MatchLabels["app"] = "igor"
	tmpSelector.MatchLabels["name"] = "igor"
	config.Spec.Selector = tmpSelector
	config.Spec.Template.Spec = k8sv1.PodSpec{}
	meta2 := v13.ObjectMeta{Name: "igor"}
	meta2.Labels = make(map[string]string)
	meta2.Labels["app"] = "igor"
	meta2.Labels["name"] = "igor"
	config.Spec.Template.ObjectMeta = meta2
	limits := make(k8sv1.ResourceList)
	limits["cpu"] = resource.MustParse("500m")
	limits["memory"] = resource.MustParse("128Mi")
	config.Spec.Template.Spec.Containers = []k8sv1.Container{{Name: "igor", Image: "artifactorycn.netcracker.com:17028/atp/browsers/vnc-chrome:69.0", Resources: k8sv1.ResourceRequirements{Limits: limits, Requests: limits}}}
	return config
}

func createServiceBody(placeholder string) k8sv1.Service {
	service := k8sv1.Service{}
	service.Kind = "Service"
	service.APIVersion = "v1"
	service.Spec = k8sv1.ServiceSpec{}
	service.Spec.Ports = []k8sv1.ServicePort{}
	service.Spec.Ports = append(service.Spec.Ports, k8sv1.ServicePort{Name: "web", Port: 8080, Protocol: "TCP"})
	service.Spec.Ports = append(service.Spec.Ports, k8sv1.ServicePort{Name: "vnc", Port: 5900, Protocol: "TCP"})
	service.Spec.Selector = make(map[string]string)
	service.Spec.Selector["name"] = placeholder
	service.Spec.Type = "NodePort"
	return service
}

func createRouteBody(placeholder string) v14.Route {
	route := v14.Route{}
	route.Kind = "Route"
	route.APIVersion = "v1"
	route.Spec = v14.RouteSpec{}
	route.Spec.Host=placeholder+".openshift.netcracker.cloud"
	route.Spec.Port=&v14.RoutePort{TargetPort:intstr.IntOrString{StrVal:"web"}}
	route.Spec.To=v14.RouteTargetReference{Kind:"Service", Name:placeholder}
	return route
}

func createDeploymentConfigBody() appsV1.DeploymentConfig {
	placeholder:="ingvar"
	timeout,_ := strconv.ParseInt("600", 10, 64)
	config := appsV1.DeploymentConfig{}
	config.Kind = "DeploymentConfig"
	config.APIVersion = "v1"
	meta := v13.ObjectMeta{Name: placeholder}
	meta.Labels = make(map[string]string)
	meta.Labels["app"] = placeholder
	meta.Labels["deploymentconfig"] = placeholder
	meta.Labels["name"] = placeholder
	config.ObjectMeta = meta
	config.Spec = appsV1.DeploymentConfigSpec{}
	config.Spec.Strategy = appsV1.DeploymentStrategy{Type:"Rolling", RecreateParams:&appsV1.RecreateDeploymentStrategyParams{TimeoutSeconds:&timeout}}
	config.Spec.Triggers = appsV1.DeploymentTriggerPolicies{}
	config.Spec.Triggers = append(config.Spec.Triggers, appsV1.DeploymentTriggerPolicy{Type:"ConfigChange"})
	config.Spec.Replicas = 1
	config.Spec.Selector = make(map[string]string)
	config.Spec.Selector["app"] = placeholder
	config.Spec.Selector["deploymentconfig"] = placeholder
	config.Spec.Selector["name"] = placeholder
	config.Spec.Template = &k8sv1.PodTemplateSpec{}
	meta2 := v13.ObjectMeta{Name: "igor"}
	meta2.Labels = make(map[string]string)
	meta2.Labels["app"] = placeholder
	meta2.Labels["deploymentconfig"] = placeholder
	meta2.Labels["name"] = placeholder
	config.Spec.Template.ObjectMeta = meta2
	config.Spec.Template.Spec = k8sv1.PodSpec{}
	limits := make(k8sv1.ResourceList)
	limits["cpu"] = resource.MustParse("100m")
	limits["memory"] = resource.MustParse("300Mi")
	container :=k8sv1.Container{Name: placeholder, Image: "artifactorycn.netcracker.com:17028/atp/browsers/vnc-chrome:69.0", ImagePullPolicy:"Always", Resources: k8sv1.ResourceRequirements{Limits: limits, Requests: limits}}
	container.Ports = []k8sv1.ContainerPort{{ContainerPort:8080, Name:"web", Protocol:"TCP"}}
	container.Ports = append(container.Ports, k8sv1.ContainerPort{ContainerPort:5900, Name:"vnc", Protocol:"TCP"})
	config.Spec.Template.Spec.Containers = []k8sv1.Container{container}
	return config
}

func createTemplateBody() v12.Template {
	placeholder:="ingvar"
	template := v12.Template{}
	template.APIVersion = "template.openshift.io/v1"
	template.Kind = "Template"
	template.Objects = []runtime.RawExtension{}
	config := createDeploymentConfigBody()
	configBytes, _ := json.Marshal(config)
	template.Objects = append(template.Objects, runtime.RawExtension{Raw: configBytes})
	service := createServiceBody(placeholder)
	serviceBytes, _ := json.Marshal(service)
	template.Objects = append(template.Objects, runtime.RawExtension{Raw: serviceBytes})
	route := createRouteBody(placeholder)
	routeBytes, _ := json.Marshal(route)
	template.Objects = append(template.Objects, runtime.RawExtension{Raw: routeBytes})
	return template
}

func getScreenResolution(input string) (string, error) {
	if input == "" {
		return "1920x1080x24", nil
	}
	if fullFormat.MatchString(input) {
		return input, nil
	}
	if shortFormat.MatchString(input) {
		return fmt.Sprintf("%sx24", input), nil
	}
	return "", fmt.Errorf(
		"Malformed screenResolution capability: %s. Correct format is WxH (1920x1080) or WxHxD (1920x1080x24).",
		input,
	)
}

func shortenScreenResolution(screenResolution string) string {
	return fullFormat.FindStringSubmatch(screenResolution)[1]
}

func getVideoScreenSize(videoScreenSize string, screenResolution string) (string, error) {
	if videoScreenSize != "" {
		if shortFormat.MatchString(videoScreenSize) {
			return videoScreenSize, nil
		}
		return "", fmt.Errorf(
			"Malformed videoScreenSize capability: %s. Correct format is WxH (1920x1080).",
			videoScreenSize,
		)
	}
	return shortenScreenResolution(screenResolution), nil
}

func getSessionTimeout(sessionTimeout string, maxTimeout time.Duration, defaultTimeout time.Duration) (time.Duration, error) {
	if sessionTimeout != "" {
		st, err := time.ParseDuration(sessionTimeout)
		if err != nil {
			return 0, fmt.Errorf("Invalid sessionTimeout capability: %v", err)
		}
		if st <= maxTimeout {
			return st, nil
		}
	}
	return defaultTimeout, nil
}

func getTemporaryFileName(dir string, extension string) string {
	filename := ""
	for {
		filename = generateRandomFileName(extension)
		_, err := os.Stat(filepath.Join(dir, filename))
		if err != nil {
			break
		}
	}
	return filename
}

func generateRandomFileName(extension string) string {
	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	return "selenoid" + hex.EncodeToString(randBytes) + extension
}

func proxy(w http.ResponseWriter, r *http.Request) {
	done := make(chan func())
	go func(w http.ResponseWriter, r *http.Request) {
		cancel := func() {}
		defer func() {
			done <- cancel
		}()
		(&httputil.ReverseProxy{
			Director: func(r *http.Request) {
				requestId := serial()
				fragments := strings.Split(r.URL.Path, slash)
				id := fragments[2]
				sess, ok := sessions.Get(id)
				if ok {
					sess.Lock.Lock()
					defer sess.Lock.Unlock()
					select {
					case <-sess.TimeoutCh:
					default:
						close(sess.TimeoutCh)
					}
					if r.Method == http.MethodDelete && len(fragments) == 3 {
						if enableFileUpload {
							os.RemoveAll(filepath.Join(os.TempDir(), id))
						}
						cancel = sess.Cancel
						sessions.Remove(id)
						queue.Release()
						log.Printf("[%d] [SESSION_DELETED] [%s]", requestId, id)
					} else {
						sess.TimeoutCh = onTimeout(sess.Timeout, func() {
							request{r}.session(id).Delete(requestId)
						})
						if len(fragments) == 4 && fragments[len(fragments)-1] == "file" && enableFileUpload {
							r.Header.Set("X-Selenoid-File", filepath.Join(os.TempDir(), id))
							r.URL.Path = "/file"
							return
						}
					}
					r.URL.Host, r.URL.Path = sess.URL.Host, path.Clean(sess.URL.Path+r.URL.Path)
					return
				}
				r.URL.Path = errorPath
			},
		}).ServeHTTP(w, r)
	}(w, r)
	go (<-done)()
}

func reverseProxy(hostFn func(sess *session.Session) string, status string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		requestId := serial()
		sid, remainingPath := splitRequestPath(r.URL.Path)
		sess, ok := sessions.Get(sid)
		if ok {
			(&httputil.ReverseProxy{
				Director: func(r *http.Request) {
					r.URL.Scheme = "http"
					r.URL.Host = hostFn(sess)
					r.URL.Path = remainingPath
					log.Printf("[%d] [%s] [%s] [%s]", requestId, status, sid, remainingPath)
				},
			}).ServeHTTP(w, r)
		} else {
			util.JsonError(w, fmt.Sprintf("Unknown session %s", sid), http.StatusNotFound)
			log.Printf("[%d] [SESSION_NOT_FOUND] [%s]", requestId, sid)
		}
	}
}

func splitRequestPath(p string) (string, string) {
	fragments := strings.Split(p, slash)
	return fragments[2], slash + strings.Join(fragments[3:], slash)
}

func fileUpload(w http.ResponseWriter, r *http.Request) {
	var jsonRequest struct {
		File []byte `json:"file"`
	}
	err := json.NewDecoder(r.Body).Decode(&jsonRequest)
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	z, err := zip.NewReader(bytes.NewReader(jsonRequest.File), int64(len(jsonRequest.File)))
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(z.File) != 1 {
		util.JsonError(w, fmt.Sprintf("Expected there to be only 1 file. There were: %d", len(z.File)), http.StatusBadRequest)
		return
	}
	file := z.File[0]
	src, err := file.Open()
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer src.Close()
	dir := r.Header.Get("X-Selenoid-File")
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fileName := filepath.Join(dir, file.Name)
	dst, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	if err != nil {
		util.JsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	reply := struct {
		V string `json:"value"`
	}{
		V: fileName,
	}
	json.NewEncoder(w).Encode(reply)
}

func vnc(wsconn *websocket.Conn) {
	defer wsconn.Close()
	requestId := serial()
	sid, _ := splitRequestPath(wsconn.Request().URL.Path)
	sess, ok := sessions.Get(sid)
	if ok {
		vncHostPort := sess.HostPort.VNC
		if vncHostPort != "" {
			log.Printf("[%d] [VNC_ENABLED] [%s]", requestId, sid)
			var d net.Dialer
			conn, err := d.DialContext(wsconn.Request().Context(), "tcp", vncHostPort)
			if err != nil {
				log.Printf("[%d] [VNC_ERROR] [%v]", requestId, err)
				return
			}
			defer conn.Close()
			wsconn.PayloadType = websocket.BinaryFrame
			go func() {
				io.Copy(wsconn, conn)
				wsconn.Close()
				log.Printf("[%d] [VNC_SESSION_CLOSED] [%s]", requestId, sid)
			}()
			io.Copy(conn, wsconn)
			log.Printf("[%d] [VNC_CLIENT_DISCONNECTED] [%s]", requestId, sid)
		} else {
			log.Printf("[%d] [VNC_NOT_ENABLED] [%s]", requestId, sid)
		}
	} else {
		log.Printf("[%d] [SESSION_NOT_FOUND] [%s]", requestId, sid)
	}
}

func logs(w http.ResponseWriter, r *http.Request) {
	fileNameOrSessionID := strings.TrimPrefix(r.URL.Path, logsPath)
	if logOutputDir != "" && (fileNameOrSessionID == "" || strings.HasSuffix(fileNameOrSessionID, logFileExtension)) {
		if r.Method == http.MethodDelete {
			deleteFileIfExists(w, r, logOutputDir, logsPath, "DELETED_LOG_FILE")
			return
		}
		fileServer := http.StripPrefix(logsPath, http.FileServer(http.Dir(logOutputDir)))
		fileServer.ServeHTTP(w, r)
		return
	}
	websocket.Handler(streamLogs).ServeHTTP(w, r)
}

func streamLogs(wsconn *websocket.Conn) {
	defer wsconn.Close()
	requestId := serial()
	sid, _ := splitRequestPath(wsconn.Request().URL.Path)
	sess, ok := sessions.Get(sid)
	if ok && sess.Container != nil {
		log.Printf("[%d] [CONTAINER_LOGS] [%s]", requestId, sess.Container.ID)
		r, err := cli.ContainerLogs(wsconn.Request().Context(), sess.Container.ID, types.ContainerLogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
		})
		if err != nil {
			log.Printf("[%d] [CONTAINER_LOGS_ERROR] [%v]", requestId, err)
			return
		}
		defer r.Close()
		wsconn.PayloadType = websocket.BinaryFrame
		stdcopy.StdCopy(wsconn, wsconn, r)
		log.Printf("[%d] [CONTAINER_LOGS_DISCONNECTED] [%s]", requestId, sid)
	} else {
		log.Printf("[%d] [SESSION_NOT_FOUND] [%s]", requestId, sid)
	}
}

func status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ready := limit > sessions.Len()
	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"value": map[string]interface{}{
				"message": fmt.Sprintf("Selenoid %s built at %s", gitRevision, buildStamp),
				"ready":   ready,
			},
		})
}

func onTimeout(t time.Duration, f func()) chan struct{} {
	cancel := make(chan struct{})
	go func(cancel chan struct{}) {
		select {
		case <-time.After(t):
			f()
		case <-cancel:
		}
	}(cancel)
	return cancel
}
